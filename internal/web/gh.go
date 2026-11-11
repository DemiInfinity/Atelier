package web

// GitHub as native: browse any GitHub profile or repository inside Atelier,
// read-only, through Tether (with the signed-in person's own GitHub token).
//
//	gh.go           routes under /gh/, who may use them, the per-request client
//	gh_profile.go   /gh/{login}: a person or organisation, with the Atelier badge
//	gh_repo.go      /gh/{owner}/{repo}: code, files, commits and releases
//	gh_issues.go    issues and pull requests (read-only)
//	gh_markdown.go  GitHub Markdown rendered with Atelier's renderer, links and images rewritten
//	gh_img.go       /gh/-/img: the image proxy for README and comment images
//
// Rules: everything from GitHub is untrusted (templates escape it; Markdown is
// rendered by goldmark with raw HTML left out; GitHub's body_html is never
// used). Only signed-in people with their own Tether connection can browse,
// and always with their own token. GET responses are cached per person
// (tether_cache): fresh for a short while so browsing is fast, and served
// with an "as of" notice when GitHub is rate limited or unreachable.
//
// The routes live on their own ServeMux behind a path handler: patterns under
// /gh/ would conflict with the top-level /{owner}/{repo}/… ones. "gh" is a
// reserved account name, so no Atelier account can be shadowed.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"atelier/internal/store"
	"atelier/internal/tether"
)

func init() {
	registerFeature(feature{
		name:   "gh",
		routes: func(a *App, _ *http.ServeMux) { ghMuxes.Store(a, a.ghRoutes()) },
		start: func(ctx context.Context, a *App) {
			if o, err := a.db.OwnerByName(ctx, "gh"); err == nil {
				log.Printf("gh: an account named %q exists; /gh/ now shows GitHub, so rename it to reach its pages", o.Name)
			}
		},
	})
	registerPathHandler(ghPathHandler)
}

// ghFreshFor is how long a GitHub answer is reused without asking again.
const ghFreshFor = 90 * time.Second

var ghMuxes sync.Map // *App → *http.ServeMux

// ghState is the feature's per-App state (tests change it).
type ghState struct {
	fresh time.Duration
	img   *ghImgState
}

var ghStates sync.Map // *App → *ghState

func (a *App) gh() *ghState {
	if st, ok := ghStates.Load(a); ok {
		return st.(*ghState)
	}
	st, _ := ghStates.LoadOrStore(a, &ghState{fresh: ghFreshFor, img: newGHImgState()})
	return st.(*ghState)
}

func (a *App) ghRoutes() *http.ServeMux {
	m := http.NewServeMux()
	g := func(h ghHandler) http.HandlerFunc { return a.authed(a.ghWith(h)) }
	m.HandleFunc("GET /gh/{$}", a.authed(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/search?src=github", http.StatusSeeOther)
	}))
	m.HandleFunc("GET /gh/-/img", g(a.ghImage))
	m.HandleFunc("GET /gh/{login}", g(a.ghProfile))
	m.HandleFunc("GET /gh/{owner}/{repo}", g(a.ghCode))
	m.HandleFunc("GET /gh/{owner}/{repo}/tree/{rest...}", g(a.ghTree))
	m.HandleFunc("GET /gh/{owner}/{repo}/blob/{rest...}", g(a.ghBlob))
	m.HandleFunc("GET /gh/{owner}/{repo}/commits", g(a.ghCommits))
	m.HandleFunc("GET /gh/{owner}/{repo}/commits/{rest...}", g(a.ghCommits))
	m.HandleFunc("GET /gh/{owner}/{repo}/commit/{sha}", g(a.ghCommit))
	m.HandleFunc("GET /gh/{owner}/{repo}/issues", g(a.ghIssues))
	m.HandleFunc("GET /gh/{owner}/{repo}/issues/{n}", g(a.ghIssuePage))
	m.HandleFunc("GET /gh/{owner}/{repo}/pulls", g(a.ghPulls))
	m.HandleFunc("GET /gh/{owner}/{repo}/pull/{n}", g(a.ghPullPage))
	m.HandleFunc("GET /gh/{owner}/{repo}/pull/{n}/files", g(a.ghPullPage))
	m.HandleFunc("GET /gh/{owner}/{repo}/releases", g(a.ghReleases))
	return m
}

// ghPathHandler answers everything under /gh/.
func ghPathHandler(a *App, w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if p != "/gh" && !strings.HasPrefix(p, "/gh/") {
		return false
	}
	v, ok := ghMuxes.Load(a)
	if !ok {
		return false
	}
	setUIHeaders(w.Header())
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	if p == "/gh" {
		http.Redirect(w, r, "/gh/", http.StatusSeeOther)
		return true
	}
	m := v.(*http.ServeMux)
	if _, pattern := m.Handler(r); pattern == "" {
		a.notFound(w, r)
		return true
	}
	m.ServeHTTP(w, r)
	return true
}

// ---------- the request ----------

// ghReq is one /gh/ request: the person, their GitHub client and whether any
// answer came from the cache because GitHub couldn't be asked.
type ghReq struct {
	who Actor
	c   *tether.Client
	acc store.TetherAccount

	flash string // an error to show (from ?err=, e.g. after Mirror to studio)

	mu         sync.Mutex
	staleAt    time.Time
	staleCause error
}

type ghHandler func(w http.ResponseWriter, r *http.Request, g *ghReq)

func (g *ghReq) noteStale(at time.Time, cause error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.staleAt.IsZero() || at.Before(g.staleAt) {
		g.staleAt = at
	}
	if g.staleCause == nil {
		g.staleCause = cause
	}
}

// ghBase is on every /gh/ page.
type ghBase struct {
	AsOf   time.Time // set when something shown is older than it should be
	Notice string    // why
	Login  string    // the person's GitHub login
	Flash  string
}

func (g *ghReq) base() ghBase {
	g.mu.Lock()
	defer g.mu.Unlock()
	b := ghBase{Login: g.acc.Login, Flash: g.flash}
	if !g.staleAt.IsZero() {
		b.AsOf = g.staleAt
		var rl *tether.RateLimitError
		switch {
		case errors.As(g.staleCause, &rl):
			b.Notice = "GitHub's rate limit ran out, so this is a saved copy. Fresh data after " + rl.Reset.UTC().Format("15:04") + " UTC."
		default:
			b.Notice = "GitHub can't be reached right now, so this is a saved copy."
		}
	}
	return b
}

// ghWith checks the person has Tether and builds their client. People
// without a connection get a friendly page; nobody's token is ever used for
// anyone else (tetherClient reads the signed-in person's own).
func (a *App) ghWith(h ghHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who := a.actor(r)
		c, acc, err := a.tetherClient(r.Context(), who.PersonID)
		if err != nil {
			if !errors.Is(err, errTetherNotConnected) {
				log.Printf("gh: %s", tetherErrText(err))
			}
			msg := ""
			if !errors.Is(err, errTetherNotConnected) {
				msg = tetherErrText(err)
			}
			a.render(w, r, "gh_connect", page{Title: "Connect GitHub", Data: map[string]string{"Error": msg}})
			return
		}
		g := &ghReq{who: who, c: c, acc: acc, flash: eventExcerpt(r.URL.Query().Get("err"), 300)}
		c.MaxAge = a.gh().fresh
		c.StaleOK = true
		c.OnStale = g.noteStale
		h(w, r, g)
	}
}

// ghFail shows a GitHub error as a page.
func (a *App) ghFail(w http.ResponseWriter, r *http.Request, g *ghReq, err error, ghURL string) {
	code := http.StatusBadGateway
	var rl *tether.RateLimitError
	switch {
	case tether.IsStatus(err, http.StatusNotFound):
		code = http.StatusNotFound
	case errors.As(err, &rl):
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	a.render(w, r, "gh_error", page{Title: "GitHub", Data: map[string]any{
		"Base": g.base(), "Message": tetherErrText(err), "GH": ghURL, "NotFound": code == http.StatusNotFound,
	}})
}

// ---------- links ----------

// ghLocalURL is a /gh/ link for a GitHub path ("login", "owner/repo/…").
// Callers pass validated names; each segment is escaped.
func ghLocalURL(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/gh/" + strings.Join(segs, "/")
}

// ghSafeBack accepts a local /gh/ path to return to (no query, no tricks).
func ghSafeBack(s string) bool {
	return strings.HasPrefix(s, "/gh/") && len(s) < 600 && !strings.ContainsAny(s, "?#\\\r\n\t ") && !strings.Contains(s, "//") && !strings.Contains(s, "/../")
}

var ghNumberRe = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

// ghBadges looks up the badges of the logins shown on a page (from the
// cache; unknown ones are checked in the background with the person's token).
func (a *App) ghBadges(ctx context.Context, g *ghReq, logins []string) map[string]store.TetherBadge {
	return a.tetherBadges(ctx, g.who.PersonID, logins)
}

// ghPerson is a GitHub user from an API object.
func (a *App) ghPerson(u tether.User, badges map[string]store.TetherBadge) tetherPerson {
	if !tether.ValidLogin(u.Login) {
		return tetherPerson{Login: "ghost"}
	}
	return a.tetherPersonView(u.Login, u.AvatarURL, badges)
}

// ghWebURL is a github.com link for a validated path.
func (a *App) ghWebURL(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return a.tetherWebURL(strings.Join(segs, "/"))
}

// ghPages is a "newer / older" pager.
type ghPager struct {
	Page       int
	Prev, Next string
}

func ghPageNum(r *http.Request) int {
	n := 1
	if s := r.URL.Query().Get("page"); s != "" {
		fmt.Sscanf(s, "%d", &n)
	}
	if n < 1 || n > 1000 {
		n = 1
	}
	return n
}

// pagerFor builds prev/next links keeping the other query parameters.
func pagerFor(r *http.Request, page int, more bool) ghPager {
	p := ghPager{Page: page}
	q := r.URL.Query()
	link := func(n int) string {
		q.Set("page", fmt.Sprint(n))
		if n == 1 {
			q.Del("page")
		}
		if s := q.Encode(); s != "" {
			return r.URL.EscapedPath() + "?" + s
		}
		return r.URL.EscapedPath()
	}
	if page > 1 {
		p.Prev = link(page - 1)
	}
	if more {
		p.Next = link(page + 1)
	}
	return p
}
