package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"atelier/internal/importer"
	"atelier/internal/store"
	"atelier/internal/tether"
)

// /search: one box for your studio first, then GitHub ("From GitHub via
// Tether"). GitHub results can be mirrored into the studio in one click.

func tetherSearchRoutes(a *App, m *http.ServeMux) {
	m.HandleFunc("GET /search", a.authed(a.tetherSearch))
	m.HandleFunc("POST /search/mirror", a.authed(a.tetherMirrorToStudio))
}

const (
	searchMaxQuery     = 200
	searchCodeRepos    = 30
	searchCodeResults  = 30
	searchCodePerRepo  = 3
	searchCodeDeadline = 4 * time.Second
)

type searchRepoHit struct {
	Repo store.Repo
}

type searchItemHit struct {
	Repo store.Repo
	Item store.LocalItem
	URL  string
}

type searchCodeHit struct {
	Repo store.Repo
	Path string
	Line string
	Text string
	URL  string
}

type searchGHRepo struct {
	tether.Repo
	Person   tetherPerson
	URL      string // /gh/<owner>/<repo>
	GHURL    string // on github.com
	Desc     string
	InStudio string // "owner/name" in Atelier when already mirrored
}

type searchGHItem struct {
	Number int64
	Title  string
	Repo   string
	State  string
	IsPull bool
	URL    string // /gh/…
	GHURL  string // on github.com
	Person tetherPerson
}

type searchData struct {
	Q, Src    string
	Sources   []tetherLangChip
	Repos     []searchRepoHit
	Items     []searchItemHit
	Code      []searchCodeHit
	CodeCut   bool
	Connected bool
	GHRepos   []searchGHRepo
	GHItems   []searchGHItem
	GHTotal   int
	GHError   string
	GHMore    string
}

func (a *App) tetherSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > searchMaxQuery {
		q = q[:searchMaxQuery]
	}
	src := r.URL.Query().Get("src")
	if src != "studio" && src != "github" {
		src = "all"
	}
	d := &searchData{Q: q, Src: src}
	for _, s := range []struct{ key, label string }{{"all", "Everywhere"}, {"studio", "My studio"}, {"github", "GitHub"}} {
		d.Sources = append(d.Sources, tetherLangChip{Label: s.label, Active: s.key == src,
			URL: "/search?" + url.Values{"q": {q}, "src": {s.key}}.Encode()})
	}
	_, err := a.db.TetherAccount(ctx, who.PersonID)
	d.Connected = err == nil
	if q != "" {
		if src != "github" {
			if err := a.searchStudio(ctx, who, q, d); err != nil {
				a.fail(w, r, err)
				return
			}
		}
		if src != "studio" && d.Connected {
			a.searchGitHub(ctx, who, q, d)
		}
	}
	a.render(w, r, "tether_search", page{Title: "Search", Nav: "search", Error: r.URL.Query().Get("err"), Data: d})
}

func (a *App) searchStudio(ctx context.Context, who Actor, q string, d *searchData) error {
	repos, err := a.db.Repos(ctx)
	if err != nil {
		return err
	}
	var readable []store.Repo
	var ids []int64
	byID := map[int64]store.Repo{}
	lq := strings.ToLower(q)
	for _, rp := range repos {
		if !a.can(ctx, who, rp, PermRead) {
			continue
		}
		readable = append(readable, rp)
		ids = append(ids, rp.ID)
		byID[rp.ID] = rp
		if strings.Contains(strings.ToLower(rp.FullName()), lq) || strings.Contains(strings.ToLower(rp.Description), lq) {
			if len(d.Repos) < 20 {
				d.Repos = append(d.Repos, searchRepoHit{Repo: rp})
			}
		}
	}
	items, err := a.db.SearchLocalItems(ctx, ids, q, 20)
	if err != nil {
		return err
	}
	for _, it := range items {
		rp := byID[it.RepoID]
		kind := "issues"
		if it.Kind == "pull" {
			kind = "pulls"
		}
		d.Items = append(d.Items, searchItemHit{Repo: rp, Item: it, URL: fmt.Sprintf("/%s/%s/%d", rp.FullName(), kind, it.Number)})
	}
	d.Code, d.CodeCut = a.searchCode(ctx, readable, q)
	return nil
}

// searchCode runs git grep on the default branch of readable repositories.
func (a *App) searchCode(ctx context.Context, repos []store.Repo, q string) ([]searchCodeHit, bool) {
	if len(q) < 2 || strings.ContainsAny(q, "\x00\n") {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, searchCodeDeadline)
	defer cancel()
	var hits []searchCodeHit
	cut := false
	for i, rp := range repos {
		if i >= searchCodeRepos {
			cut = true
			break
		}
		if ctx.Err() != nil || len(hits) >= searchCodeResults {
			cut = true
			break
		}
		g, err := a.git.Open(rp.Owner, rp.Name)
		if err != nil || g.IsEmpty(ctx) {
			continue
		}
		branch := g.DefaultBranch(ctx)
		out, _ := a.tetherGit(ctx, a.git.Path(rp.Owner, rp.Name), "", "", "grep", "-I", "-n", "-i", "-F",
			fmt.Sprintf("--max-count=%d", searchCodePerRepo), "-e", q, "refs/heads/"+branch, "--")
		n := 0
		for _, line := range strings.Split(out, "\n") {
			// refs/heads/main:path:12:text
			rest, ok := strings.CutPrefix(line, "refs/heads/"+branch+":")
			if !ok {
				continue
			}
			parts := strings.SplitN(rest, ":", 3)
			if len(parts) < 3 {
				continue
			}
			text := strings.TrimSpace(parts[2])
			if len(text) > 200 {
				text = text[:200] + "…"
			}
			hits = append(hits, searchCodeHit{Repo: rp, Path: parts[0], Line: parts[1], Text: text,
				URL: fmt.Sprintf("/%s/blob/%s/%s#L%s", rp.FullName(), branch, parts[0], parts[1])})
			n++
			if n >= searchCodePerRepo*3 || len(hits) >= searchCodeResults {
				break
			}
		}
	}
	return hits, cut
}

func (a *App) searchGitHub(ctx context.Context, who Actor, q string, d *searchData) {
	c, _, err := a.tetherClient(ctx, who.PersonID)
	if err != nil {
		d.GHError = tetherErrText(err)
		return
	}
	repos, total, err := c.SearchRepos(ctx, q, "", 10)
	if err != nil {
		d.GHError = tetherErrText(err)
		return
	}
	d.GHTotal = total
	d.GHMore = a.tetherWebURL("search") + "?" + url.Values{"q": {q}}.Encode()
	var logins []string
	for _, rp := range repos {
		logins = append(logins, rp.Owner.Login)
	}
	issues, _, ierr := c.SearchIssues(ctx, q, 8)
	if ierr == nil {
		for _, is := range issues {
			logins = append(logins, is.User.Login)
		}
	}
	badges := a.tetherBadges(ctx, who.PersonID, logins)
	mine, _ := a.db.PersonalOwner(ctx)
	for _, rp := range repos {
		if !tether.ValidRepo(rp.FullName) {
			continue
		}
		g := searchGHRepo{Repo: rp, Person: a.tetherPersonView(rp.Owner.Login, rp.Owner.AvatarURL, badges),
			URL: ghLocalURL(rp.FullName), GHURL: a.tetherWebURL(rp.FullName), Desc: eventExcerpt(rp.Description, 200)}
		if _, err := a.db.RepoByName(ctx, mine.Name, rp.Name); err == nil {
			g.InStudio = mine.Name + "/" + rp.Name
		}
		d.GHRepos = append(d.GHRepos, g)
	}
	for _, is := range issues {
		repo := is.Repo()
		if !tether.ValidRepo(repo) {
			continue
		}
		kind := "issues"
		if is.IsPull() {
			kind = "pull"
		}
		d.GHItems = append(d.GHItems, searchGHItem{Number: is.Number, Title: eventExcerpt(is.Title, 200), Repo: repo,
			State: is.State, IsPull: is.IsPull(), Person: a.tetherPersonView(is.User.Login, is.User.AvatarURL, badges),
			URL:   ghLocalURL(fmt.Sprintf("%s/%s/%d", repo, kind, is.Number)),
			GHURL: a.tetherWebURL(fmt.Sprintf("%s/%s/%d", repo, kind, is.Number))})
	}
}

// tetherMirrorToStudio imports a GitHub repository into the personal account.
// Public repositories become ordinary import mirrors (no credentials kept).
// Private ones are cloned once with the token and then kept up to date by
// a Tether link (Home: GitHub), so the token stays sealed.
func (a *App) tetherMirrorToStudio(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	full := strings.TrimSpace(r.FormValue("repo"))
	back := "/search?" + url.Values{"q": {r.FormValue("q")}, "src": {r.FormValue("src")}}.Encode()
	sep := "&"
	if b := r.FormValue("back"); ghSafeBack(b) {
		back, sep = b, "?" // a /gh/… page (gh.go): errors go back there
	}
	fail := func(msg string) { http.Redirect(w, r, back+sep+"err="+url.QueryEscape(msg), http.StatusSeeOther) }
	if !tether.ValidRepo(full) {
		fail("That isn't a GitHub repository.")
		return
	}
	c, _, err := a.tetherLinkClient(ctx, who.PersonID, full)
	if err != nil {
		fail(tetherErrText(err))
		return
	}
	gh, err := c.Repo(ctx, full)
	if err != nil {
		fail("Couldn't look up " + full + ": " + tetherErrText(err))
		return
	}
	me, err := a.db.Person(ctx, who.PersonID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	name := gh.Name
	req := importer.Request{Source: "github", URL: a.tetherRemote(gh.FullName), Owner: me.Name, Name: name,
		Description: eventExcerpt(gh.Description, 300), Private: true}
	if _, err := importer.Check(ctx, a.db, req); err != nil {
		fail(err.Error())
		return
	}
	if !gh.Private {
		req.Mirror, req.MirrorInterval = true, a.mirrorInterval()
		if err := a.enqueueImports(ctx, []importer.Request{req}, who); err != nil {
			a.fail(w, r, err)
			return
		}
		http.Redirect(w, r, "/settings/import", http.StatusSeeOther)
		return
	}
	tok, err := a.tetherLinkToken(ctx, who.PersonID, full)
	if err != nil {
		fail(tetherErrText(err))
		return
	}
	req.Creds = importer.Creds{Token: tok}
	id, err := a.db.CreateImportJob(ctx, store.ImportJob{Source: "github", SourceURL: req.URL, Owner: req.Owner, Name: req.Name,
		Description: req.Description, Private: true, Mirror: true, ActorKind: who.Kind, ActorName: who.Name})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	_ = a.db.SetImportJobStatus(ctx, id, "running", "Cloning through Tether…")
	bctx := context.WithoutCancel(ctx)
	a.tether().goBG("", func() {
		rp, err := importer.Import(bctx, a.db, a.git, req) // one-off: credentials aren't stored
		if err != nil {
			_ = a.db.SetImportJobStatus(bctx, id, "failed", tetherScrub(err.Error(), tok))
			return
		}
		l := store.TetherLink{RepoID: rp.ID, PersonID: who.PersonID, GHRepo: gh.FullName, Home: store.TetherHomeGitHub}
		if err := a.db.SaveTetherLink(bctx, l); err != nil {
			log.Printf("tether: linking %s: %v", rp.FullName(), err)
		}
		_ = a.db.SetImportJobStatus(bctx, id, "done", "Imported through Tether. It stays up to date with GitHub (Home: GitHub).")
	})
	http.Redirect(w, r, "/settings/import", http.StatusSeeOther)
}
