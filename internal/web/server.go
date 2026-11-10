// Package web serves Atelier's UI and git-over-HTTPS endpoints.
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"atelier/internal/config"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const (
	sessionCookie = "atelier_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// Reserved names can't be used for accounts because they are top-level routes.
var reserved = map[string]bool{
	"setup": true, "signin": true, "signout": true, "auth": true, "new": true, "accounts": true,
	"settings": true, "static": true, "api": true, "explore": true, "notifications": true,
	"search": true, "mcp": true, "npm": true, "healthz": true, "v2": true, "go": true,
	"packages": true, "runners": true, "actions": true, "lfs": true,
}

type App struct {
	cfg    config.Config
	db     *store.Store
	git    *gitrepo.Store
	pages  map[string]*template.Template
	mux    *http.ServeMux
	static http.Handler
	lim    *limiter
}

func New(cfg config.Config, db *store.Store, git *gitrepo.Store) (*App, error) {
	a := &App{cfg: cfg, db: db, git: git, mux: http.NewServeMux(), lim: newLimiter(8, time.Minute)}
	if err := a.loadTemplates(); err != nil {
		return nil, err
	}
	a.routes()
	for _, f := range features {
		if f.routes != nil {
			f.routes(a, a.mux)
		}
	}
	return a, nil
}

func (a *App) routes() {
	m := a.mux
	// /static/ is dispatched in ServeHTTP rather than registered here: as a mux
	// pattern it conflicts with "GET /{owner}/{repo}" and ServeMux panics.
	static, _ := fs.Sub(staticFS, "static")
	a.static = http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static)))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	m.HandleFunc("GET /setup", a.setupPage)
	m.HandleFunc("POST /setup", a.setupSubmit)
	m.HandleFunc("GET /signin", a.signinPage)
	m.HandleFunc("POST /signin", a.signinSubmit)
	m.HandleFunc("POST /signout", a.signout)
	m.HandleFunc("GET /auth/github", a.githubStart)
	m.HandleFunc("GET /auth/github/callback", a.githubCallback)

	m.HandleFunc("GET /{$}", a.authed(a.home))
	m.HandleFunc("GET /new", a.authed(a.newRepoPage))
	m.HandleFunc("POST /new", a.authed(a.newRepoSubmit))
	m.HandleFunc("GET /accounts/new", a.authed(a.newAccountPage))
	m.HandleFunc("POST /accounts/new", a.authed(a.newAccountSubmit))
	m.HandleFunc("GET /settings/tokens", a.authed(a.tokensPage))
	m.HandleFunc("POST /settings/tokens", a.authed(a.tokensCreate))
	m.HandleFunc("POST /settings/tokens/{id}/revoke", a.authed(a.tokensRevoke))

	m.HandleFunc("GET /{owner}", a.authed(a.ownerPage))
	m.HandleFunc("GET /{owner}/{repo}", a.authed(a.repoCode))
	m.HandleFunc("GET /{owner}/{repo}/tree/{rest...}", a.authed(a.repoTree))
	m.HandleFunc("GET /{owner}/{repo}/blob/{rest...}", a.authed(a.repoBlob))
	m.HandleFunc("GET /{owner}/{repo}/raw/{rest...}", a.authed(a.repoRaw))
	m.HandleFunc("GET /{owner}/{repo}/commits/{rest...}", a.authed(a.repoCommits))
	m.HandleFunc("GET /{owner}/{repo}/commit/{sha}", a.authed(a.repoCommit))
	m.HandleFunc("GET /{owner}/{repo}/settings", a.authed(a.repoSettings))
	m.HandleFunc("POST /{owner}/{repo}/settings", a.authed(a.repoSettingsSubmit))
	m.HandleFunc("POST /{owner}/{repo}/delete", a.authed(a.repoDelete))
}

// pathHandlers get a first look at every request, before git and the UI
// router, for URLs the mux can't express (e.g. Git LFS under
// /{owner}/{repo}.git/info/lfs/). A handler returns true if it answered.
// Register from a feature's init() with registerPathHandler.
var pathHandlers []func(a *App, w http.ResponseWriter, r *http.Request) bool

func registerPathHandler(h func(a *App, w http.ResponseWriter, r *http.Request) bool) {
	pathHandlers = append(pathHandlers, h)
}

// ServeHTTP adds security headers, sends git traffic to the git handler and
// everything else to the UI router.
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")

	for _, ph := range pathHandlers {
		if ph(a, w, r) {
			return
		}
	}

	if owner, repo, rest, ok := gitrepo.IsGitPath(r.URL.Path); ok {
		a.serveGit(w, r, owner, repo, rest)
		return
	}

	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' https://fonts.googleapis.com; "+
		"font-src https://fonts.gstatic.com; img-src 'self' data: https://avatars.githubusercontent.com; "+
		"script-src 'self'; form-action 'self' https://github.com; frame-ancestors 'none'; base-uri 'none'")

	if strings.HasPrefix(r.URL.Path, "/static/") {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		a.static.ServeHTTP(w, r)
		return
	}

	if r.Method == http.MethodPost && !a.sameOrigin(r) {
		http.Error(w, "cross-site request blocked", http.StatusForbidden)
		return
	}
	a.mux.ServeHTTP(w, r)
}

// ---------- request helpers ----------

// scheme and host as the browser sees them, honouring nginx when trusted.
func (a *App) external(r *http.Request) (scheme, host string) {
	scheme, host = "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	if a.cfg.TrustProxy {
		if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
			scheme = p
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = strings.TrimSpace(strings.Split(h, ",")[0])
		}
	}
	return
}

func (a *App) baseURL(r *http.Request) string {
	if a.cfg.PublicURL != "" {
		return a.cfg.PublicURL
	}
	s, h := a.external(r)
	return s + "://" + h
}

func (a *App) isHTTPS(r *http.Request) bool {
	if strings.HasPrefix(a.cfg.PublicURL, "https://") {
		return true
	}
	s, _ := a.external(r)
	return s == "https"
}

func (a *App) clientIP(r *http.Request) string {
	if a.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return xr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sameOrigin blocks cross-site form posts (CSRF) using Fetch metadata and Origin.
func (a *App) sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return origin == "" // non-browser clients (no Origin) are allowed; opaque origins are not
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	_, host := a.external(r)
	return strings.EqualFold(u.Host, host)
}

// ---------- sessions ----------

func (a *App) signedIn(r *http.Request) bool {
	_, ok := a.sessionActor(r)
	return ok
}

// sessionActor returns the signed-in person, if the session is valid and the
// person still exists.
func (a *App) sessionActor(r *http.Request) (Actor, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Actor{}, false
	}
	id, ok := a.db.SessionPerson(r.Context(), c.Value)
	if !ok {
		return Actor{}, false
	}
	p, err := a.db.Person(r.Context(), id)
	if err != nil {
		return Actor{}, false
	}
	return Actor{Kind: ActorOwner, Name: p.Name, PersonID: p.ID}, true
}

// startSession signs in the personal owner (password and GitHub sign-in are
// the owner's). Use startSessionFor when the person is known another way.
func (a *App) startSession(w http.ResponseWriter, r *http.Request) error {
	me, err := a.db.PersonalOwner(r.Context())
	if err != nil {
		return err
	}
	return a.startSessionFor(w, r, me.ID)
}

func (a *App) startSessionFor(w http.ResponseWriter, r *http.Request, personID int64) error {
	tok, err := a.db.CreateSessionFor(r.Context(), personID, sessionTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: a.isHTTPS(r),
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds()),
	})
	return nil
}

func (a *App) needsSetup(ctx context.Context) bool {
	_, err := a.db.PersonalOwner(ctx)
	return errors.Is(err, store.ErrNotFound)
}

// authed requires the owner to be signed in (or sends them to setup/sign-in).
func (a *App) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.needsSetup(r.Context()) {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		who, ok := a.sessionActor(r)
		if !ok {
			http.Redirect(w, r, "/signin?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		h(w, withActor(r, who))
	}
}

// ---------- git over HTTPS ----------

func (a *App) serveGit(w http.ResponseWriter, r *http.Request, owner, name, rest string) {
	ctx := r.Context()
	repo, err := a.db.RepoByName(ctx, owner, name)
	if err != nil {
		// Push to create: only for pushes, never for clones or fetches.
		if !errors.Is(err, store.ErrNotFound) || !gitrepo.IsWrite(r, rest) {
			http.Error(w, "repository not found", http.StatusNotFound)
			return
		}
		created, ok := a.gitPushCreate(w, r, owner, name)
		if !ok {
			return
		}
		repo = created
	}
	write := gitrepo.IsWrite(r, rest)
	need := PermRead
	if write {
		need = PermWrite
	}

	who := Actor{Kind: ActorAnonymous, Name: "anonymous"}
	if !a.can(ctx, who, repo, need) {
		if !a.lim.allow(a.clientIP(r)) {
			http.Error(w, "too many attempts, try again in a minute", http.StatusTooManyRequests)
			return
		}
		if u, p, ok := r.BasicAuth(); ok {
			for _, cand := range []string{p, u} { // some clients put the token in the username
				if w, ok := a.resolveCredential(ctx, cand); ok {
					who = w
					break
				}
			}
		}
		if who.Kind == ActorAnonymous || !a.can(ctx, who, repo, need) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Atelier", charset="UTF-8"`)
			http.Error(w, "authentication required: use an Atelier access token as the password", http.StatusUnauthorized)
			return
		}
		a.lim.reset(a.clientIP(r))
	}
	user := who.Kind + ":" + who.Name

	a.git.ServeHTTP(w, r, repo.Owner, repo.Name, rest, user)
	if write && rest == "git-receive-pack" {
		_ = a.db.TouchRepo(context.WithoutCancel(ctx), repo.ID)
	}
}

func (a *App) basicToken(r *http.Request) (store.Token, bool) {
	u, p, ok := r.BasicAuth()
	if !ok {
		return store.Token{}, false
	}
	for _, cand := range []string{p, u} { // some clients put the token in the username
		if t, err := a.db.CheckToken(r.Context(), cand); err == nil {
			return t, true
		}
	}
	return store.Token{}, false
}

// ---------- small utilities ----------

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		h.ServeHTTP(w, r)
	})
}

// limiter is a tiny per-IP failure counter for sign-in and git auth.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-l.window)
	hs := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			hs = append(hs, t)
		}
	}
	if len(hs) >= l.max {
		l.hits[key] = hs
		return false
	}
	l.hits[key] = append(hs, time.Now())
	return true
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.hits, key)
	l.mu.Unlock()
}

func logErr(r *http.Request, err error) {
	log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
}
