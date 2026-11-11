package web

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"atelier/internal/store"
)

const pagesTestBase = "http://pages.test"

type pagesFix struct {
	a    *App
	zoe  store.Owner
	demo store.Repo
	ck   map[string]string
	work string
}

// newPagesFix: zoe's public demo repository, Pages at pages.test and the app
// at git.test.
func newPagesFix(t *testing.T) *pagesFix {
	t.Helper()
	g := newGitFixture(t)
	a := g.a
	a.cfg.PagesURL = pagesTestBase
	a.cfg.PublicURL = "http://git.test"
	ctx := context.Background()
	zoe, _ := a.db.OwnerByName(ctx, "zoe")
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	if err := a.db.UpdateRepo(ctx, demo.ID, "", false); err != nil {
		t.Fatal(err)
	}
	demo.Private = false
	return &pagesFix{a: a, zoe: zoe, demo: demo, ck: cookieFor(t, a, zoe.ID), work: pullsWork(t, a)}
}

// push pushes branch and runs the push pipeline (repo.pushed → Pages).
func (f *pagesFix) push(t *testing.T, branch string) string {
	t.Helper()
	g, _ := f.a.git.Open("zoe", "demo")
	old, err := g.BranchSHA(context.Background(), branch)
	if err != nil {
		old = strings.Repeat("0", 40)
	}
	runGit(t, f.work, "push", "-q", "origin", branch)
	nw := runGit(t, f.work, "rev-parse", branch)
	if err := f.a.db.QueueActionsPush(context.Background(), store.ActionsPush{Owner: "zoe", Repo: "demo", Old: old, New: nw,
		Ref: "refs/heads/" + branch, Pusher: "owner:zoe"}); err != nil {
		t.Fatal(err)
	}
	f.a.drainActionsPushes(context.Background())
	pagesBG.Wait()
	return nw
}

func (f *pagesFix) save(t *testing.T, form url.Values, want int) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(t, f.a, "POST", "/zoe/demo/settings/pages", form, f.ck)
	if rec.Code != want {
		t.Fatalf("save %v = %d, want %d\n%.1500s", form, rec.Code, want, rec.Body)
	}
	return rec
}

func (f *pagesFix) site(t *testing.T) store.PagesSite {
	t.Helper()
	ps, err := f.a.db.PagesSite(context.Background(), f.demo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

func pagesGet(t *testing.T, a *App, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "pages.test"
	req.URL.Path = path
	req.RequestURI = path
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	if c := rec.Header().Values("Set-Cookie"); len(c) > 0 {
		t.Errorf("pages host set cookies on %s: %v", path, c)
	}
	return rec
}

func pagesWant(t *testing.T, rec *httptest.ResponseRecorder, code int, ctype, body string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d\n%.500s", rec.Code, code, rec.Body)
	}
	if ctype != "" && rec.Header().Get("Content-Type") != ctype {
		t.Errorf("content type = %q, want %q", rec.Header().Get("Content-Type"), ctype)
	}
	if body != "" && !strings.Contains(rec.Body.String(), body) {
		t.Errorf("body lacks %q:\n%.500s", body, rec.Body)
	}
}

func TestPagesFromBranch(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	pullsCommit(t, f.work, "index.html", "<h1>Home v1</h1>", "home")
	pullsCommit(t, f.work, "style.css", "body{color:red}", "css")
	pullsCommit(t, f.work, "app.js", "console.log(1)", "js")
	pullsCommit(t, f.work, "data.json", `{"a":1}`, "json")
	pullsCommit(t, f.work, "logo.svg", "<svg/>", "svg")
	pullsCommit(t, f.work, "blob.zzq", "???", "unknown")
	pullsCommit(t, f.work, "guide/index.html", "<h1>Guide</h1>", "guide")
	pullsCommit(t, f.work, "about.html", "<h1>About</h1>", "about")
	pullsCommit(t, f.work, "404.html", "<h1>Lost here</h1>", "404")
	f.push(t, "main")

	// Not published yet.
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 404, "", "no site")

	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}, "folder": {"/"}}, http.StatusSeeOther)
	ps := f.site(t)
	if !ps.Enabled || ps.LiveID == 0 {
		t.Fatalf("site = %+v", ps)
	}

	rec := pagesGet(t, a, "/zoe/demo/", nil)
	pagesWant(t, rec, 200, "text/html; charset=utf-8", "Home v1")
	h := rec.Header()
	if !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("security headers = %v", h)
	}
	etag, lm := h.Get("ETag"), h.Get("Last-Modified")
	if etag == "" || lm == "" {
		t.Fatalf("no ETag / Last-Modified: %v", h)
	}
	if rec := pagesGet(t, a, "/zoe/demo/", map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match = %d", rec.Code)
	}
	if rec := pagesGet(t, a, "/zoe/demo/", map[string]string{"If-Modified-Since": lm}); rec.Code != http.StatusNotModified {
		t.Errorf("If-Modified-Since = %d", rec.Code)
	}
	for path, ct := range map[string]string{
		"/zoe/demo/style.css": "text/css; charset=utf-8", "/zoe/demo/app.js": "text/javascript; charset=utf-8",
		"/zoe/demo/data.json": "application/json", "/zoe/demo/logo.svg": "image/svg+xml",
		"/zoe/demo/blob.zzq": "application/octet-stream", "/zoe/demo/index.html": "text/html; charset=utf-8",
	} {
		pagesWant(t, pagesGet(t, a, path, nil), 200, ct, "")
	}
	pagesWant(t, pagesGet(t, a, "/zoe/demo/guide/", nil), 200, "", "Guide")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/about", nil), 200, "", "About")
	for _, p := range []string{"/zoe/demo", "/zoe/demo/guide"} {
		rec := pagesGet(t, a, p, nil)
		if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != p+"/" {
			t.Errorf("%s = %d %q", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	// Missing files get the site's 404.html with a 404 status.
	pagesWant(t, pagesGet(t, a, "/zoe/demo/nope.html", nil), 404, "text/html; charset=utf-8", "Lost here")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/style.css/", nil), 404, "", "Lost here")

	// A push to main republishes; other branches don't.
	pullsCommit(t, f.work, "index.html", "<h1>Home v2</h1>", "v2")
	f.push(t, "main")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Home v2")
	if rec := pagesGet(t, a, "/zoe/demo/", map[string]string{"If-None-Match": etag}); rec.Code != 200 {
		t.Errorf("stale ETag after a deploy = %d", rec.Code)
	}
	runGit(t, f.work, "checkout", "-q", "-b", "feature")
	pullsCommit(t, f.work, "index.html", "<h1>Feature</h1>", "feature")
	f.push(t, "feature")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Home v2")

	// The settings page shows the live site and the history.
	body := do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String()
	mustContain(t, body, "settings page", "Your site is live", pagesTestBase+"/zoe/demo/", "Roll back to this deploy")
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings", nil, f.ck).Body.String(), "repo settings", "/zoe/demo/settings/pages")
}

func TestPagesFromFolder(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	pullsCommit(t, f.work, "secret.txt", "not for the site", "root")
	pullsCommit(t, f.work, "docs/index.html", "<h1>Docs</h1>", "docs")
	pullsCommit(t, f.work, "docs/img/a.png", "png", "img")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}, "folder": {"/docs"}}, http.StatusSeeOther)
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Docs")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/img/a.png", nil), 200, "image/png", "png")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/secret.txt", nil), 404, "", "")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/docs/index.html", nil), 404, "", "")

	// A folder that doesn't exist fails the deploy and keeps the old one live.
	live := f.site(t).LiveID
	rec := f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}, "folder": {"/site"}}, http.StatusSeeOther)
	if !strings.Contains(rec.Header().Get("Location"), "failed=1") {
		t.Errorf("redirect = %q", rec.Header().Get("Location"))
	}
	if f.site(t).LiveID != live {
		t.Error("a failed deploy replaced the live one")
	}
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings/pages?failed=1", nil, f.ck).Body.String(), "page", "doesn&#39;t exist")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}, "folder": {"../x"}}, http.StatusBadRequest)
}

func TestPagesHostIsolation(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	pullsCommit(t, f.work, "index.html", "<h1>Site</h1>", "home")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)

	// The pages host serves no app routes, git, static UI files or cookies.
	for _, p := range []string{"/", "/signin", "/setup", "/settings/tokens", "/static/atelier.css", "/zoe/demo.git/info/refs",
		"/api/v3/user", "/healthz", "/zoe"} {
		rec := pagesGet(t, a, p, map[string]string{"Cookie": f.ck["Cookie"]})
		if rec.Code != 404 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("pages host %s = %d %q", p, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	req := httptest.NewRequest("POST", "/signin", strings.NewReader("password=x"))
	req.Host = "pages.test"
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || len(rec.Header().Values("Set-Cookie")) > 0 {
		t.Errorf("POST on pages host = %d %v", rec.Code, rec.Header())
	}
	// Host matching ignores case and the default port.
	for _, host := range []string{"PAGES.test", "pages.test:80"} {
		req := httptest.NewRequest("GET", "/zoe/demo/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		pagesWant(t, rec, 200, "", "Site")
	}

	// The app host never serves the site.
	app := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "git.test"
		for k, v := range f.ck {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		return rec
	}
	if rec := app("/zoe/demo/"); strings.Contains(rec.Body.String(), "<h1>Site</h1>") {
		t.Error("app host served the site")
	}
	if rec := app("/zoe/demo/index.html"); rec.Code == 200 && strings.Contains(rec.Body.String(), "<h1>Site</h1>") {
		t.Error("app host served a site file")
	}
	if rec := app("/zoe/demo"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "demo") {
		t.Errorf("app repo page = %d", rec.Code)
	}

	// Path traversal never leaves the deploy.
	secret := filepath.Join(a.cfg.DataDir, "atelier.db")
	if _, err := os.Stat(secret); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/zoe/demo/../../../atelier.db", "/zoe/demo/..%2f..%2fatelier.db", "/zoe/demo/./index.html",
		"/zoe/demo/%2e%2e/%2e%2e/atelier.db", "/zoe/demo/..\\..\\atelier.db", "/zoe/demo//etc/passwd", "/zoe/demo/\x00"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = "pages.test"
		if u, err := url.Parse(p); err == nil {
			req.URL = u
		} else {
			req.URL.Path = p
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code == 200 {
			t.Errorf("%q = 200: %.200s", p, rec.Body)
		}
	}
	// Symlinks committed to the repository are never published.
	if err := os.Symlink("/etc/passwd", filepath.Join(f.work, "passwd")); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.work, "add", ".")
	runGit(t, f.work, "commit", "-q", "-m", "link")
	f.push(t, "main")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/passwd", nil), 404, "", "")

	// Pages off: requests to that host are ordinary app requests.
	a.cfg.PagesURL = ""
	if rec := pagesGet(t, a, "/zoe/demo/", nil); strings.Contains(rec.Body.String(), "<h1>Site</h1>") {
		t.Error("Pages served with ATELIER_PAGES_URL unset")
	}
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String(), "off page", "Pages is off on this server", "ATELIER_PAGES_URL")
	// Pages on the app's own host is refused.
	a.cfg.PagesURL = "http://git.test"
	if a.pagesEnabled() {
		t.Error("Pages enabled on the app host")
	}
}

func TestPagesUserSite(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	us := profRepo(t, a, f.zoe, "zoe.pages", false, "index.html", "<h1>Zoe home</h1>", "blog.html", "<h1>Blog</h1>")
	if err := a.db.SavePagesSite(ctx, store.PagesSite{RepoID: us.ID, Enabled: true, Source: store.PagesFromBranch, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	ps, _ := a.db.PagesSite(ctx, us.ID)
	if _, err := a.pagesDeployBranch(ctx, us, ps, "", "test"); err != nil {
		t.Fatal(err)
	}
	pullsCommit(t, f.work, "index.html", "<h1>Demo</h1>", "demo")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)

	pagesWant(t, pagesGet(t, a, "/zoe/", nil), 200, "", "Zoe home")
	pagesWant(t, pagesGet(t, a, "/zoe/blog", nil), 200, "", "Blog")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Demo")
	if rec := pagesGet(t, a, "/zoe", nil); rec.Code != 301 || rec.Header().Get("Location") != "/zoe/" {
		t.Errorf("/zoe = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := a.pagesSiteURL(us); got != pagesTestBase+"/zoe/" {
		t.Errorf("user site URL = %q", got)
	}
}

func TestPagesPrivateRepos(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	pullsCommit(t, f.work, "index.html", "<h1>Was public</h1>", "home")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Was public")

	// Turning the repository private takes the site down at once…
	if err := a.db.UpdateRepo(ctx, f.demo.ID, "", true); err != nil {
		t.Fatal(err)
	}
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 404, "text/plain; charset=utf-8", "")
	// …even for the owner, and nothing new can be published.
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", f.ck), 404, "", "")
	before, _ := a.db.PagesDeploys(ctx, f.demo.ID, 100)
	pullsCommit(t, f.work, "index.html", "<h1>Private</h1>", "private")
	f.push(t, "main")
	if after, _ := a.db.PagesDeploys(ctx, f.demo.ID, 100); len(after) != len(before) {
		t.Errorf("a push to a private repository deployed (%d → %d)", len(before), len(after))
	}
	rec := f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusBadRequest)
	mustContain(t, rec.Body.String(), "save", "Private repositories can")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/deploy", url.Values{}, f.ck), http.StatusBadRequest, "deploy now")
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String(), "page", "Private repositories can")
}

func TestPagesRollbackAndPruning(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	pullsCommit(t, f.work, "index.html", "<h1>v1</h1>", "v1")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)
	first := f.site(t).LiveID
	for i := 2; i <= 7; i++ {
		pullsCommit(t, f.work, "index.html", fmt.Sprintf("<h1>v%d</h1>", i), "next")
		f.push(t, "main")
	}
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "v7")
	ds, _ := a.db.PagesDeploys(ctx, f.demo.ID, 100)
	if len(ds) != 5 {
		t.Fatalf("kept %d deploys, want 5", len(ds))
	}
	if snaps := a.pagesFiles().Snapshots(f.demo.ID); len(snaps) != 5 {
		t.Errorf("%d snapshots on disk, want 5", len(snaps))
	}
	// The oldest are gone; rolling back to them is refused.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/rollback", url.Values{"deploy": {fmt.Sprint(first)}}, f.ck), 404, "rollback pruned")

	// Roll back to v4 (the fourth newest).
	v4 := ds[3]
	rec := do(t, a, "POST", "/zoe/demo/settings/pages/rollback", url.Values{"deploy": {fmt.Sprint(v4.ID)}}, f.ck)
	expectStatus(t, rec, http.StatusSeeOther, "rollback")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "v4")
	if f.site(t).LiveID != v4.ID {
		t.Error("live deploy not switched")
	}
	body := do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String()
	if strings.Count(body, "Roll back to this deploy") != 4 {
		t.Errorf("rollback buttons = %d, want 4", strings.Count(body, "Roll back to this deploy"))
	}
	// The next push goes live again and pruning still keeps five.
	pullsCommit(t, f.work, "index.html", "<h1>v8</h1>", "next")
	f.push(t, "main")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "v8")
	if ds, _ := a.db.PagesDeploys(ctx, f.demo.ID, 100); len(ds) != 5 || ds[0].ID != f.site(t).LiveID {
		t.Errorf("after rollback and push: %d deploys", len(ds))
	}

	// Another repository's deploy id can't be used.
	other := profRepo(t, a, f.zoe, "other", false, "index.html", "x")
	_ = a.db.SavePagesSite(ctx, store.PagesSite{RepoID: other.ID, Enabled: true, Source: store.PagesFromBranch, Branch: "main"})
	ops, _ := a.db.PagesSite(ctx, other.ID)
	od, err := a.pagesDeployBranch(ctx, other, ops, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/rollback", url.Values{"deploy": {fmt.Sprint(od.ID)}}, f.ck), 404, "foreign rollback")

	// Unpublish removes everything.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/unpublish", url.Values{}, f.ck), http.StatusSeeOther, "unpublish")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 404, "", "")
	if snaps := a.pagesFiles().Snapshots(f.demo.ID); len(snaps) != 0 {
		t.Errorf("unpublish left %d snapshots", len(snaps))
	}
	pagesWant(t, pagesGet(t, a, "/zoe/other/", nil), 200, "", "x")
}

func TestPagesSizeLimits(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	pullsCommit(t, f.work, "index.html", "<h1>ok</h1>", "v1")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)
	live := f.site(t).LiveID

	a.cfg.PagesMaxFiles = 2
	pullsCommit(t, f.work, "b.html", "b", "b")
	pullsCommit(t, f.work, "c.html", "c", "c")
	f.push(t, "main")
	ds, _ := a.db.PagesDeploys(context.Background(), f.demo.ID, 10)
	if ds[0].Status != "failed" || !strings.Contains(ds[0].Error, "more than 2 files") {
		t.Errorf("newest deploy = %+v", ds[0])
	}
	if f.site(t).LiveID != live {
		t.Error("an oversized deploy went live")
	}
	a.cfg.PagesMaxFiles = 0
	a.cfg.PagesMaxBytes = 4
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/deploy", url.Values{}, f.ck), http.StatusSeeOther, "deploy now")
	ds, _ = a.db.PagesDeploys(context.Background(), f.demo.ID, 10)
	if ds[0].Status != "failed" || !strings.Contains(ds[0].Error, "too large") {
		t.Errorf("newest deploy = %+v", ds[0])
	}
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "ok")
	if m, _ := filepath.Glob(filepath.Join(a.cfg.DataDir, "pages", "sites", "*", ".tmp-*")); len(m) != 0 {
		t.Errorf("temp dirs left: %v", m)
	}
}

func TestPagesSettingsNeedAdmin(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	pullsCommit(t, f.work, "index.html", "<h1>ok</h1>", "v1")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)
	live := f.site(t).LiveID
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sc := cookieFor(t, a, sam.ID)
	for _, c := range []struct{ method, path string }{
		{"GET", "/zoe/demo/settings/pages"},
		{"POST", "/zoe/demo/settings/pages"},
		{"POST", "/zoe/demo/settings/pages/deploy"},
		{"POST", "/zoe/demo/settings/pages/rollback"},
		{"POST", "/zoe/demo/settings/pages/unpublish"},
	} {
		var form url.Values
		if c.method == "POST" {
			form = url.Values{"source": {"actions"}, "branch": {"main"}, "deploy": {fmt.Sprint(live)}}
		}
		if rec := do(t, a, c.method, c.path, form, sc); rec.Code != http.StatusForbidden {
			t.Errorf("sam %s %s = %d", c.method, c.path, rec.Code)
		}
		if rec := do(t, a, c.method, c.path, form, nil); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin") {
			t.Errorf("anonymous %s %s = %d", c.method, c.path, rec.Code)
		}
	}
	if ps := f.site(t); !ps.Enabled || ps.Source != store.PagesFromBranch || ps.LiveID != live {
		t.Errorf("settings changed by sam: %+v", ps)
	}
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "ok")
}

// ---------- uploads from Actions ----------

func pagesTarGz(t *testing.T, files ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for i := 0; i+1 < len(files); i += 2 {
		if err := tw.WriteHeader(&tar.Header{Name: files[i], Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[i+1]))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(files[i+1]))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func pagesUploadReq(t *testing.T, a *App, query, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	hdr := map[string]string{}
	if token != "" {
		hdr["Authorization"] = "Bearer " + token
	}
	return doRaw(t, a, "POST", "/api/pages/deploy"+query, bytes.NewReader(body), hdr)
}

const pagesWorkflow = `name: Pages
on: push
jobs:
  deploy:
    runs-on: linux
    steps:
      - uses: atelier/deploy-pages@v1
        with:
          path: dist
`

func TestPagesUploadWithJobToken(t *testing.T) {
	f := newActFixture(t)
	a, ctx := f.a, context.Background()
	a.cfg.PagesURL = pagesTestBase
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	if err := a.db.UpdateRepo(ctx, demo.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SavePagesSite(ctx, store.PagesSite{RepoID: demo.ID, Enabled: true, Source: store.PagesFromActions, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	pullsCommit(t, f.work, ".atelier/workflows/pages.yml", pagesWorkflow, "pages workflow")
	f.push(t, "main")
	runs := f.runs(t)
	if len(runs) != 1 || len(runs[0].Errors) != 0 {
		t.Fatalf("runs = %+v", runs)
	}
	if runs[0].Pins["atelier/deploy-pages@v1"] != "atelier/deploy-pages@v1" {
		t.Errorf("built-in action pinned as %q", runs[0].Pins["atelier/deploy-pages@v1"])
	}
	lx := f.registerRunner(t, "Linux", []string{"linux"}, []string{"jobs"})
	job := lx.poll()
	if job == nil || job.JobToken == "" || job.Steps[0].Uses != "atelier/deploy-pages@v1" {
		t.Fatalf("job = %+v", job)
	}
	tok := job.JobToken
	site := pagesTarGz(t, "index.html", "<h1>Built</h1>", "assets/app.js", "go()")

	// No token, a junk token, and a personal access token are refused.
	expectStatus(t, pagesUploadReq(t, a, "", "", site), http.StatusUnauthorized, "no token")
	expectStatus(t, pagesUploadReq(t, a, "", "atljob_nope", site), http.StatusUnauthorized, "junk token")
	pat, _ := a.db.CreateToken(ctx, "w", tokenScopes["write"], 0)
	expectStatus(t, pagesUploadReq(t, a, "?repo=zoe/demo", pat, site), http.StatusForbidden, "personal token")

	// Another repository, even public with Pages from Actions, is refused.
	other := profRepo(t, a, mustOwner(t, a, "zoe"), "other", false, "index.html", "x")
	_ = a.db.SavePagesSite(ctx, store.PagesSite{RepoID: other.ID, Enabled: true, Source: store.PagesFromActions, Branch: "main"})
	expectStatus(t, pagesUploadReq(t, a, "?repo=zoe/other", tok, site), http.StatusForbidden, "other repo")
	expectStatus(t, pagesUploadReq(t, a, "?repo=zoe/nope", tok, site), http.StatusForbidden, "missing repo")

	// The job's own repository works, and the site goes live.
	rec := pagesUploadReq(t, a, "?repo=zoe/demo&sha="+job.SHA, tok, site)
	expectStatus(t, rec, http.StatusCreated, "upload")
	mustContain(t, rec.Body.String(), "upload reply", pagesTestBase+"/zoe/demo/", `"files":2`)
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Built")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/assets/app.js", nil), 200, "text/javascript; charset=utf-8", "go()")
	ds, _ := a.db.PagesDeploys(ctx, demo.ID, 5)
	if len(ds) != 1 || ds[0].Source != "actions" || ds[0].SHA != job.SHA || !strings.HasPrefix(ds[0].Actor, "actions-job-") {
		t.Errorf("deploys = %+v", ds)
	}
	// Without ?repo the job's repository is used.
	expectStatus(t, pagesUploadReq(t, a, "", tok, pagesTarGz(t, "index.html", "<h1>Again</h1>")), http.StatusCreated, "upload 2")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Again")

	// Too large → 413 and the live site stays.
	a.cfg.PagesMaxBytes = 8
	expectStatus(t, pagesUploadReq(t, a, "", tok, pagesTarGz(t, "index.html", strings.Repeat("x", 100))), http.StatusRequestEntityTooLarge, "too big")
	a.cfg.PagesMaxBytes = 0
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Again")

	// Pull requests and other branches can't deploy.
	saved := pagesJobRun
	t.Cleanup(func() { pagesJobRun = saved })
	pagesJobRun = func(context.Context, *App, int64) (string, string, error) {
		return "pull_request", "refs/heads/main", nil
	}
	expectStatus(t, pagesUploadReq(t, a, "", tok, site), http.StatusForbidden, "pull request")
	pagesJobRun = func(context.Context, *App, int64) (string, string, error) { return "push", "refs/heads/feature", nil }
	expectStatus(t, pagesUploadReq(t, a, "", tok, site), http.StatusForbidden, "other branch")
	pagesJobRun = saved

	// A site set to publish from a branch doesn't take uploads.
	_ = a.db.SavePagesSite(ctx, store.PagesSite{RepoID: demo.ID, Enabled: true, Source: store.PagesFromBranch, Branch: "main"})
	expectStatus(t, pagesUploadReq(t, a, "", tok, site), http.StatusConflict, "branch source")
	// A private repository can't publish.
	_ = a.db.SavePagesSite(ctx, store.PagesSite{RepoID: demo.ID, Enabled: true, Source: store.PagesFromActions, Branch: "main"})
	_ = a.db.UpdateRepo(ctx, demo.ID, "", true)
	expectStatus(t, pagesUploadReq(t, a, "", tok, site), http.StatusForbidden, "private")
	_ = a.db.UpdateRepo(ctx, demo.ID, "", false)

	// Once the job finishes, its token is dead.
	lx.finish(job.ID, "success", nil)
	expectStatus(t, pagesUploadReq(t, a, "", tok, site), http.StatusUnauthorized, "finished job")
}

// A job token from a workflow that uses an atelier: action from another
// repository carries that repository's actions:repo scope too; it still
// can't deploy there.
func TestPagesUploadScopeIsOwnRepoOnly(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	other := profRepo(t, a, f.zoe, "other", false, "index.html", "x")
	for _, id := range []int64{f.demo.ID, other.ID} {
		_ = a.db.SavePagesSite(ctx, store.PagesSite{RepoID: id, Enabled: true, Source: store.PagesFromActions, Branch: "main"})
	}
	savedR, savedRun := tokenResolvers, pagesJobRun
	t.Cleanup(func() { tokenResolvers, pagesJobRun = savedR, savedRun })
	pagesJobRun = func(context.Context, *App, int64) (string, string, error) { return "push", "refs/heads/main", nil }
	registerTokenResolver(func(_ context.Context, _ *App, s string) (Actor, bool) {
		switch s {
		case "atljob_demo":
			return Actor{Kind: ActorJob, Name: "actions-job-9", Scopes: []string{"repo:read", fmt.Sprintf("actions:repo:%d", f.demo.ID),
				"actions:job:9", "packages:read", fmt.Sprintf("actions:repo:%d", other.ID)}}, true
		case "atljob_agent":
			return Actor{Kind: ActorJob, Name: "agent:triage", Scopes: []string{"repo:read", fmt.Sprintf("actions:repo:%d", f.demo.ID), "actions:job:10"}}, true
		}
		return Actor{}, false
	})
	site := pagesTarGz(t, "index.html", "<h1>Mine</h1>")
	expectStatus(t, pagesUploadReq(t, a, "?repo=zoe/other", "atljob_demo", site), http.StatusForbidden, "pinned action repo")
	expectStatus(t, pagesUploadReq(t, a, "?repo=zoe/demo", "atljob_agent", site), http.StatusForbidden, "agent job")
	expectStatus(t, pagesUploadReq(t, a, "?repo=zoe/demo", "atljob_demo", site), http.StatusCreated, "own repo")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Mine")
	pagesWant(t, pagesGet(t, a, "/zoe/other/", nil), 404, "", "")
	// Basic auth (as git uses) works too.
	req := httptest.NewRequest("POST", "/api/pages/deploy", bytes.NewReader(site))
	req.SetBasicAuth("atelier", "atljob_demo")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	expectStatus(t, rec, http.StatusCreated, "basic auth")
}
