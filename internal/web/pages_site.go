package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/pagesdeploy"
	"atelier/internal/store"
)

// Pages: static sites published from a repository.
//
//	pages_site.go          feature, configuration, deploys (branch and upload), rollback, pruning
//	pages_site_serve.go    the Pages host: host dispatch and serving files
//	pages_site_settings.go /{owner}/{repo}/settings/pages
//	pages_site_api.go      POST /api/pages/deploy, used by atelier/deploy-pages with a job token
//	pages_site_domain.go   custom domains: claims, verification, serving at "/"
//	pages_site_private.go  private sites on a custom domain: sign-in handoff, site sessions
//	pages_site_preview.go  pull request previews
//
// Sites are served only on the host of ATELIER_PAGES_URL, never on the app's
// host, so a site's HTML can't reach Atelier's session cookie. Addresses are
// path-based: <pages>/<owner>/<repo>/, and the repository named
// <owner>.pages is the owner's site at <pages>/<owner>/.
//
// On the shared Pages origin only public repositories are served. A private
// site would need a sign-in there, and since every site shares that origin,
// any published site's script could read a private site's pages with the
// visitor's credentials. A verified custom domain is the site's own origin,
// so a private repository may publish once it has one, and is then served
// only there, to people who can read the repository (pages_site_private.go).

func init() {
	registerFeature(feature{
		name: "pages",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /{owner}/{repo}/settings/pages", a.authed(a.pagesSettings))
			m.HandleFunc("POST /{owner}/{repo}/settings/pages", a.authed(a.pagesSettingsSave))
			m.HandleFunc("POST /{owner}/{repo}/settings/pages/deploy", a.authed(a.pagesDeployNow))
			m.HandleFunc("POST /{owner}/{repo}/settings/pages/rollback", a.authed(a.pagesRollback))
			m.HandleFunc("POST /{owner}/{repo}/settings/pages/unpublish", a.authed(a.pagesUnpublish))
			m.HandleFunc("POST /api/pages/deploy", a.pagesUpload)
		},
		start: func(ctx context.Context, a *App) {
			if a.pagesHost() == "" && a.cfg.PagesURL != "" {
				log.Printf("pages: ATELIER_PAGES_URL %q is not usable (it needs its own host, different from the app's); Pages is off", a.cfg.PagesURL)
			}
			_ = a.db.FailStalePagesDeploys(ctx)
			a.pagesCleanup(ctx)
		},
	})
	registerEventHandler(func(ctx context.Context, a *App, ev Event) {
		if ev.Kind != EventRepoPushed || ev.Push == nil {
			return
		}
		rp := ev.Repo
		if rp.ID == 0 {
			return
		}
		pagesBG.Add(1)
		go func() {
			defer pagesBG.Done()
			a.pagesOnPush(ctx, rp, ev.Push.Ref, ev.Push.New, ev.Push.Pusher)
			a.pagesPreviewOnPush(ctx, rp, ev.Push.Ref, ev.Push.New)
		}()
	})
}

// pagesUserSiteSuffix names an owner's own site: the repository
// <owner>.pages is served at <pages>/<owner>/.
const pagesUserSiteSuffix = ".pages"

func pagesUserSiteName(owner string) string { return owner + pagesUserSiteSuffix }

// pagesBG tracks deploys started by pushes (tests wait on it).
var pagesBG sync.WaitGroup

// pagesLocks serialises deploys per repository.
var pagesLocks sync.Map // repoID → *sync.Mutex

func pagesLock(repoID int64) func() {
	m, _ := pagesLocks.LoadOrStore(repoID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ---------- configuration ----------

// pagesBase is ATELIER_PAGES_URL as scheme://host[:port], or "" when Pages
// is off (unset, unparsable, or on the app's own host).
func (a *App) pagesBase() string {
	raw := a.cfg.PagesURL
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return ""
	}
	if a.cfg.PublicURL != "" {
		if pu, err := url.Parse(a.cfg.PublicURL); err == nil && pagesSameHost(pu.Host, pu.Scheme, u.Host, u.Scheme) {
			return "" // never serve sites on the app's origin
		}
	}
	return u.Scheme + "://" + strings.ToLower(u.Host)
}

// pagesHost is the host[:port] Pages answers on ("" = off).
func (a *App) pagesHost() string {
	b := a.pagesBase()
	if b == "" {
		return ""
	}
	_, h, _ := strings.Cut(b, "://")
	return h
}

func (a *App) pagesEnabled() bool { return a.pagesBase() != "" }

// pagesSameHost compares two host[:port] values, treating a missing port as
// the scheme's default.
func pagesSameHost(h1, s1, h2, s2 string) bool {
	n1, p1 := pagesSplitHost(h1, s1)
	n2, p2 := pagesSplitHost(h2, s2)
	return n1 != "" && n1 == n2 && p1 == p2
}

func pagesSplitHost(h, scheme string) (string, string) {
	h = strings.ToLower(strings.TrimSpace(h))
	name, port, err := net.SplitHostPort(h)
	if err != nil {
		name, port = strings.Trim(h, "[]"), ""
	}
	name = strings.TrimSuffix(name, ".")
	if port == "" {
		port = "80"
		if scheme == "https" {
			port = "443"
		}
	}
	return name, port
}

func (a *App) pagesFiles() *pagesdeploy.Store {
	lim := pagesdeploy.Limits{MaxBytes: a.cfg.PagesMaxBytes, MaxFiles: a.cfg.PagesMaxFiles}
	if lim.MaxBytes <= 0 {
		lim.MaxBytes = 1 << 30
	}
	if lim.MaxFiles <= 0 {
		lim.MaxFiles = 50000
	}
	return &pagesdeploy.Store{Root: filepath.Join(a.cfg.DataDir, "pages"), Limits: lim}
}

func (a *App) pagesKeep() int {
	if a.cfg.PagesKeep > 0 {
		return a.cfg.PagesKeep
	}
	return 5
}

// pagesSiteURL is the live address of a repository's site ("" when Pages is off).
func (a *App) pagesSiteURL(rp store.Repo) string {
	b := a.pagesBase()
	if b == "" {
		return ""
	}
	if strings.EqualFold(rp.Name, pagesUserSiteName(rp.Owner)) {
		return b + "/" + url.PathEscape(rp.Owner) + "/"
	}
	return b + "/" + url.PathEscape(rp.Owner) + "/" + url.PathEscape(rp.Name) + "/"
}

// ---------- deploys ----------

var errPagesPrivate = errors.New("private repositories can't publish Pages without a verified custom domain")

// pagesMayPublish reports whether the repository's site may be deployed and
// served: public repositories always, private ones only on a verified
// custom domain (their own origin).
func (a *App) pagesMayPublish(ctx context.Context, rp store.Repo) bool {
	if !rp.Private {
		return true
	}
	d, err := a.db.PagesDomain(ctx, rp.ID)
	return err == nil && d.Verified()
}

// pagesOnPush publishes the site when a push moves its branch. Call it for
// every ref update; it ignores the ones that don't concern Pages.
func (a *App) pagesOnPush(ctx context.Context, rp store.Repo, ref, newSHA, pusher string) {
	if actZeroSHA(newSHA) || !a.pagesMayPublish(ctx, rp) {
		return
	}
	ps, err := a.db.PagesSite(ctx, rp.ID)
	if err != nil || !ps.Enabled || ps.Source != store.PagesFromBranch || ref != "refs/heads/"+ps.Branch {
		return
	}
	if _, err := a.pagesDeployBranch(ctx, rp, ps, newSHA, pusher); err != nil {
		log.Printf("pages: %s: %v", rp.FullName(), err)
	}
}

// pagesDeployBranch publishes ps.Folder of ps.Branch (at sha, or the branch
// head when sha is "") as a new deploy and makes it live.
func (a *App) pagesDeployBranch(ctx context.Context, rp store.Repo, ps store.PagesSite, sha, actor string) (store.PagesDeploy, error) {
	if !a.pagesMayPublish(ctx, rp) {
		return store.PagesDeploy{}, errPagesPrivate
	}
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return store.PagesDeploy{}, err
	}
	d := store.PagesDeploy{RepoID: rp.ID, Source: store.PagesFromBranch, Ref: "refs/heads/" + ps.Branch, Actor: actor}
	if sha == "" {
		if sha, err = g.BranchSHA(ctx, ps.Branch); err != nil {
			return a.pagesFail(ctx, rp, d, fmt.Errorf("branch %s doesn't exist", ps.Branch))
		}
	}
	d.SHA = sha
	feed, err := pagesFolderFeed(ctx, g, sha, ps.Folder, ps.Branch)
	if err != nil {
		return a.pagesFail(ctx, rp, d, err)
	}
	return a.pagesPublish(ctx, rp, d, feed)
}

// pagesFolderFeed streams folder of commit sha as a tar archive. where names
// the commit in error messages (a branch name).
func pagesFolderFeed(ctx context.Context, g *gitrepo.Repo, sha, folder, where string) (func(extract func(io.Reader) error) error, error) {
	folder, ok := pagesdeploy.CleanFolder(folder)
	if !ok {
		return nil, fmt.Errorf("invalid folder %q", folder)
	}
	treeish := sha
	if folder != "" {
		if g.ObjectType(ctx, sha, folder) != "tree" {
			return nil, fmt.Errorf("folder /%s doesn't exist on %s", folder, where)
		}
		treeish = sha + ":" + folder
	}
	return func(extract func(io.Reader) error) error {
		return g.StreamGit(ctx, 10*time.Minute, extract, "archive", "--format=tar", treeish)
	}, nil
}

// pagesFail records a deploy that couldn't start.
func (a *App) pagesFail(ctx context.Context, rp store.Repo, d store.PagesDeploy, cause error) (store.PagesDeploy, error) {
	d, err := a.db.CreatePagesDeploy(ctx, d)
	if err != nil {
		return d, err
	}
	_ = a.db.FinishPagesDeploy(ctx, d.ID, 0, 0, cause.Error())
	d.Status, d.Error = "failed", cause.Error()
	return d, cause
}

// pagesPublish records a deploy, fills its snapshot from the tar stream that
// feed hands to extract, switches the site to it and prunes old deploys.
func (a *App) pagesPublish(ctx context.Context, rp store.Repo, d store.PagesDeploy, feed func(extract func(io.Reader) error) error) (store.PagesDeploy, error) {
	unlock := pagesLock(rp.ID)
	defer unlock()
	ctx = context.WithoutCancel(ctx)
	d, err := a.pagesBuild(ctx, rp, d, feed)
	if err != nil {
		return d, err
	}
	if err := a.db.SetPagesLive(ctx, rp.ID, d.ID); err != nil {
		return d, err
	}
	a.pagesPrune(ctx, rp.ID)
	return d, nil
}

// pagesBuild records a deploy and fills its snapshot, leaving it ready (or
// failed, with the snapshot removed). The caller holds pagesLock.
func (a *App) pagesBuild(ctx context.Context, rp store.Repo, d store.PagesDeploy, feed func(extract func(io.Reader) error) error) (store.PagesDeploy, error) {
	d, err := a.db.CreatePagesDeploy(ctx, d)
	if err != nil {
		return d, err
	}
	files := a.pagesFiles()
	var res pagesdeploy.Result
	extracted := false
	err = feed(func(r io.Reader) error {
		var err error
		res, err = files.Extract(rp.ID, d.ID, r, "")
		extracted = err == nil
		return err
	})
	if err == nil && !extracted {
		err = errors.New("nothing was uploaded")
	}
	if err != nil {
		_ = files.Remove(rp.ID, d.ID)
		_ = a.db.FinishPagesDeploy(ctx, d.ID, res.Files, res.Bytes, pagesErrText(err))
		d.Status, d.Error = "failed", pagesErrText(err)
		return d, err
	}
	if err := a.db.FinishPagesDeploy(ctx, d.ID, res.Files, res.Bytes, ""); err != nil {
		return d, err
	}
	d.Status, d.Files, d.Bytes = "ready", res.Files, res.Bytes
	return d, nil
}

func pagesErrText(err error) string {
	s := err.Error()
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// pagesPrune keeps the newest pagesKeep ready deploys (and the live one);
// older snapshots and their history rows are removed.
func (a *App) pagesPrune(ctx context.Context, repoID int64) {
	ps, err := a.db.PagesSite(ctx, repoID)
	if err != nil {
		return
	}
	ds, err := a.db.PagesDeploys(ctx, repoID, 10000)
	if err != nil {
		return
	}
	files := a.pagesFiles()
	ready := 0
	for _, d := range ds { // newest first
		if d.ID == ps.LiveID || d.Status == "building" {
			if d.Status == "ready" {
				ready++
			}
			continue
		}
		if d.Status == "ready" && ready < a.pagesKeep() {
			ready++
			continue
		}
		if d.Status == "failed" && ready < a.pagesKeep() {
			continue // recent failures stay in the history
		}
		_ = files.Remove(repoID, d.ID)
		_ = a.db.DeletePagesDeploy(ctx, repoID, d.ID)
	}
}

// pagesCleanup removes snapshot directories nothing refers to (deleted
// repositories, unpublished sites, interrupted deploys).
func (a *App) pagesCleanup(ctx context.Context) {
	ids, err := a.db.PagesSiteIDs(ctx)
	if err != nil {
		return
	}
	files := a.pagesFiles()
	for _, repoID := range files.Sites() {
		if !ids[repoID] {
			_ = files.RemoveSite(repoID)
			continue
		}
		ds, err := a.db.PagesDeploys(ctx, repoID, 10000)
		if err != nil {
			continue
		}
		pds, err := a.db.PagesPreviewDeploys(ctx, repoID)
		if err != nil {
			continue
		}
		keep := map[int64]bool{}
		for _, d := range append(ds, pds...) {
			if d.Status == "ready" {
				keep[d.ID] = true
			}
		}
		for _, id := range files.Snapshots(repoID) {
			if !keep[id] {
				_ = files.Remove(repoID, id)
			}
		}
		matches, _ := filepath.Glob(filepath.Join(files.Root, "sites", fmt.Sprint(repoID), ".tmp-*"))
		for _, m := range matches {
			_ = os.RemoveAll(m)
		}
	}
}

// pagesRollbackTo makes an earlier ready deploy live again.
func (a *App) pagesRollbackTo(ctx context.Context, rp store.Repo, deployID int64) error {
	if !a.pagesMayPublish(ctx, rp) {
		return errPagesPrivate
	}
	unlock := pagesLock(rp.ID)
	defer unlock()
	d, err := a.db.PagesDeploy(ctx, rp.ID, deployID)
	if err != nil {
		return err
	}
	if d.Source == store.PagesFromPreview {
		return store.ErrNotFound // previews never become the site
	}
	if d.Status != "ready" {
		return errors.New("that deploy can't be served")
	}
	if _, err := os.Stat(a.pagesFiles().Dir(rp.ID, d.ID)); err != nil {
		return errors.New("that deploy's files are gone")
	}
	return a.db.SetPagesLive(ctx, rp.ID, d.ID)
}

// pagesUnpublishRepo takes the site down and deletes every deploy.
func (a *App) pagesUnpublishRepo(ctx context.Context, rp store.Repo) error {
	unlock := pagesLock(rp.ID)
	defer unlock()
	if err := a.db.DeletePagesSite(ctx, rp.ID); err != nil {
		return err
	}
	return a.pagesFiles().RemoveSite(rp.ID)
}

// pagesBranchExists reports whether branch exists in the repository.
func pagesBranchExists(ctx context.Context, g *gitrepo.Repo, branch string) bool {
	_, err := g.BranchSHA(ctx, branch)
	return err == nil
}
