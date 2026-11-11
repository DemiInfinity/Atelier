package web

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
)

// pagesSiteCookie returns the Set-Cookie line for the site session, if any.
func pagesSiteSetCookie(h http.Header) string {
	for _, c := range h.Values("Set-Cookie") {
		if strings.HasPrefix(c, pagesSiteCookie+"=") || strings.HasPrefix(c, pagesSiteCookieTLS+"=") {
			return c
		}
	}
	return ""
}

func TestPagesPrivateSiteOnCustomDomain(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	noFileVerify(t)
	pullsCommit(t, f.work, "index.html", "<h1>Secret home</h1>", "home")
	pullsCommit(t, f.work, "guide/index.html", "<h1>Secret guide</h1>", "guide")
	f.push(t, "main")
	if err := a.db.UpdateRepo(ctx, f.demo.ID, "", true); err != nil {
		t.Fatal(err)
	}
	// Private without a domain: can't publish (as before).
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusBadRequest)
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String(), "private page", "verified custom domain", "Add domain")

	// With a verified domain it can, and it's served only there.
	f.verifyDomain(t, "demo", "docs.example.com")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)
	if f.site(t).LiveID == 0 {
		t.Fatal("not deployed")
	}
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String(), "private site page", "http://docs.example.com/", "to people who can read the repository")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 404, "", "There isn't a site here")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", f.ck), 404, "", "")

	// No session: sent to the app to sign in. The app cookie means nothing here.
	rec := pagesHostGet(t, a, "docs.example.com", "/guide/?x=1", map[string]string{"Cookie": f.ck["Cookie"]})
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "http://git.test/-/pages/signin?host=docs.example.com&return=%2Fguide%2F%3Fx%3D1" {
		t.Fatalf("anonymous private = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if strings.Contains(rec.Body.String(), "Secret") || len(rec.Header().Values("Set-Cookie")) > 0 {
		t.Error("private content or cookies before sign-in")
	}

	// The app hands out a single-use token to readers only.
	handoff := func(ck map[string]string, host, ret string) *httpResult {
		rec := do(t, a, "GET", "/-/pages/signin?host="+url.QueryEscape(host)+"&return="+url.QueryEscape(ret), nil, ck)
		loc := rec.Header().Get("Location")
		if rec.Code == 200 { // a refresh page, not a redirect (CSP form-action)
			b := rec.Body.String()
			if i := strings.Index(b, `content="0;url=`); i >= 0 {
				b = b[i+len(`content="0;url=`):]
				loc = html.UnescapeString(b[:strings.IndexByte(b, '"')])
			}
		}
		return &httpResult{rec.Code, loc, rec.Header()}
	}
	if r := handoff(nil, "docs.example.com", "/"); r.code != http.StatusSeeOther || !strings.HasPrefix(r.loc, "/signin?next=") {
		t.Errorf("anonymous handoff = %d %q", r.code, r.loc)
	}
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	samCk := cookieFor(t, a, sam.ID)
	if r := handoff(samCk, "docs.example.com", "/"); r.code != http.StatusNotFound {
		t.Errorf("non-reader handoff = %d %q", r.code, r.loc)
	}
	if r := handoff(f.ck, "unknown.example.com", "/"); r.code != http.StatusNotFound {
		t.Errorf("unknown host handoff = %d", r.code)
	}
	r := handoff(f.ck, "DOCS.example.com.", "//evil.test/x")
	const prefix = "http://docs.example.com/.well-known/atelier-pages/_signin?token="
	if r.code != http.StatusOK || !strings.HasPrefix(r.loc, prefix) || !strings.HasSuffix(r.loc, "&return=%2F") {
		t.Fatalf("handoff = %d %q", r.code, r.loc)
	}
	if r.hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("handoff cache control = %q", r.hdr.Get("Cache-Control"))
	}
	r = handoff(f.ck, "docs.example.com", "/guide/")
	signin := strings.TrimPrefix(r.loc, "http://docs.example.com")

	// The token works on another host? No.
	other := profRepo(t, a, f.zoe, "other", true, "index.html", "<h1>Other secret</h1>")
	_ = a.db.SavePagesSite(ctx, store.PagesSite{RepoID: other.ID, Enabled: true, Source: store.PagesFromBranch, Branch: "main"})
	f.verifyDomain(t, "other", "other.example.com")
	ops, _ := a.db.PagesSite(ctx, other.ID)
	if _, err := a.pagesDeployBranch(ctx, other, ops, "", "test"); err != nil {
		t.Fatal(err)
	}
	if rec := pagesHostGet(t, a, "other.example.com", signin, nil); rec.Code != http.StatusForbidden || pagesSiteSetCookie(rec.Header()) != "" {
		t.Errorf("token on another host = %d", rec.Code)
	}
	// On its own host: a host-only, HttpOnly cookie, then back to the page.
	rec = pagesHostGet(t, a, "docs.example.com", signin, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/guide/" {
		t.Fatalf("signin = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	set := pagesSiteSetCookie(rec.Header())
	if set == "" || len(rec.Header().Values("Set-Cookie")) != 1 {
		t.Fatalf("cookies = %v", rec.Header().Values("Set-Cookie"))
	}
	for _, want := range []string{"Path=/", "HttpOnly", "SameSite=Lax"} {
		if !strings.Contains(set, want) {
			t.Errorf("cookie %q lacks %s", set, want)
		}
	}
	if strings.Contains(strings.ToLower(set), "domain=") || strings.Contains(set, sessionCookie) {
		t.Errorf("cookie not host-only: %q", set)
	}
	site := map[string]string{"Cookie": strings.SplitN(set, ";", 2)[0]}
	// Single use.
	if rec := pagesHostGet(t, a, "docs.example.com", signin, nil); rec.Code != http.StatusForbidden || pagesSiteSetCookie(rec.Header()) != "" {
		t.Errorf("token reused = %d", rec.Code)
	}

	// Signed in: the site, privately cached, not readable cross-origin.
	rec = pagesHostGet(t, a, "docs.example.com", "/guide/", site)
	pagesWant(t, rec, 200, "", "Secret guide")
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || !strings.HasPrefix(rec.Header().Get("Cache-Control"), "private") {
		t.Errorf("private headers = %v", rec.Header())
	}
	pagesWant(t, pagesHostGet(t, a, "docs.example.com", "/nope", site), 404, "", "")
	// The cookie is for this host and site only.
	if rec := pagesHostGet(t, a, "other.example.com", "/", site); rec.Code != http.StatusFound {
		t.Errorf("cookie accepted on another host: %d", rec.Code)
	}
	// It never opens the app.
	if rec := pagesHostGet(t, a, "git.test", "/zoe/demo", site); rec.Code == 200 {
		t.Error("site cookie opened the app")
	}

	// Expired tokens don't work.
	savedTTL := pagesHandoffTTLFor
	t.Cleanup(func() { pagesHandoffTTLFor = savedTTL })
	pagesHandoffTTLFor = func() time.Duration { return -time.Minute }
	stale := strings.TrimPrefix(handoff(f.ck, "docs.example.com", "/").loc, "http://docs.example.com")
	pagesHandoffTTLFor = savedTTL
	if rec := pagesHostGet(t, a, "docs.example.com", stale, nil); rec.Code != http.StatusForbidden {
		t.Errorf("expired token = %d", rec.Code)
	}

	// Read access is checked on every request: a session for someone who
	// can't read the repository gets nothing.
	if err := a.db.CreatePagesSiteSession(ctx, store.Hash("sams-session"), f.demo.ID, sam.ID, "docs.example.com", time.Hour); err != nil {
		t.Fatal(err)
	}
	if rec := pagesHostGet(t, a, "docs.example.com", "/", map[string]string{"Cookie": pagesSiteCookie + "=sams-session"}); rec.Code != http.StatusFound {
		t.Errorf("non-reader session = %d", rec.Code)
	}

	// Signing out ends the session.
	rec = pagesHostGet(t, a, "docs.example.com", "/.well-known/atelier-pages/_signout", site)
	if rec.Code != 200 || !strings.Contains(pagesSiteSetCookie(rec.Header()), "Max-Age=0") {
		t.Errorf("signout = %d %v", rec.Code, rec.Header().Values("Set-Cookie"))
	}
	if rec := pagesHostGet(t, a, "docs.example.com", "/", site); rec.Code != http.StatusFound {
		t.Errorf("after signout = %d", rec.Code)
	}

	// Removing the domain takes the private site offline everywhere.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/domain/remove", url.Values{}, f.ck), http.StatusSeeOther, "remove")
	if rec := pagesHostGet(t, a, "docs.example.com", "/", site); strings.Contains(rec.Body.String(), "Secret") {
		t.Error("served after the domain was removed")
	}
	before, _ := a.db.PagesDeploys(ctx, f.demo.ID, 100)
	pullsCommit(t, f.work, "index.html", "<h1>v2</h1>", "v2")
	f.push(t, "main")
	if after, _ := a.db.PagesDeploys(ctx, f.demo.ID, 100); len(after) != len(before) {
		t.Error("a private repository without a domain deployed")
	}
}

type httpResult struct {
	code int
	loc  string
	hdr  http.Header
}
