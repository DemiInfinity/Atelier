package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The owner can open a company account's profile, with and without repos.
func TestCompanyProfileOpens(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	co, _ := f.a.db.CreateOwner(ctx, "elyndrastudios", "Elyndra Studios LTD", "company")
	sess, _ := f.a.db.CreateSession(ctx, time.Hour)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
		rec := httptest.NewRecorder()
		f.a.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/elyndrastudios"); rec.Code != 200 {
		t.Fatalf("empty company profile = %d\n%.600s", rec.Code, rec.Body)
	}
	if _, err := f.a.db.CreateRepo(ctx, co.ID, "RebelVerify", "Zero knowledge", true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/elyndrastudios", "/elyndrastudios?tab=repositories", "/projects?account=elyndrastudios"} {
		if rec := get(p); rec.Code != 200 {
			t.Errorf("%s = %d\n%.600s", p, rec.Code, rec.Body)
		}
	}
}

// #16: the company account is one click from anywhere: the top bar's account
// menu lists it, and profiles have account tabs. Nobody else sees it there.
func TestCompanyReachableFromNav(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	zoe, _ := f.a.db.OwnerByName(ctx, "zoe")
	co, _ := f.a.db.CreateOwner(ctx, "elyndrastudios", "Elyndra Studios LTD", "company")
	if _, err := f.a.db.CreateRepo(ctx, co.ID, "RebelVerify", "Zero knowledge", true); err != nil {
		t.Fatal(err)
	}
	zc := cookieFor(t, f.a, zoe.ID)

	// Any page: the account menu in the top bar.
	for _, p := range []string{"/", "/zoe", "/projects", "/settings/tokens"} {
		body := expectStatus(t, do(t, f.a, "GET", p, nil, zc), 200, p)
		menu := body[strings.Index(body, "data-acct-menu"):]
		menu = menu[:strings.Index(menu, "</details>")]
		mustContain(t, menu, p+" account menu", `href="/zoe"`, `href="/elyndrastudios"`, "Elyndra Studios LTD", "Company", "Personal")
	}
	// The Repositories link goes to the repositories tab.
	home := expectStatus(t, do(t, f.a, "GET", "/", nil, zc), 200, "home")
	mustContain(t, home, "top bar", `href="/zoe?tab=repositories"`)

	// Profile account tabs: Personal · Elyndra Studios.
	prof := expectStatus(t, do(t, f.a, "GET", "/zoe?tab=repositories", nil, zc), 200, "zoe repos")
	mustContain(t, prof, "account tabs", "Repositories:", `href="/elyndrastudios?tab=repositories"`, `href="/zoe?tab=repositories"`)
	cp := expectStatus(t, do(t, f.a, "GET", "/elyndrastudios?tab=repositories", nil, zc), 200, "company repos")
	mustContain(t, cp, "company repositories tab", `href="/elyndrastudios/RebelVerify"`, `href="/projects?account=elyndrastudios"`)
	if !strings.Contains(cp, `class="acct-tab is-active" href="/elyndrastudios?tab=repositories" aria-current="page"`) {
		t.Error("the company tab should be the current one")
	}
	if !strings.Contains(cp, `href="/elyndrastudios?tab=repositories" aria-current="page">Repositories`) {
		t.Error("the Repositories tab should be current on ?tab=repositories")
	}
	// The top bar highlights Repositories on the company profile too.
	if !strings.Contains(cp, `href="/zoe?tab=repositories" aria-current="page">Repositories`) {
		t.Error("top bar Repositories should be current on a company you own")
	}

	// A second person: only their own account in the menu, no tabs.
	sam, _ := f.a.db.CreateOwner(ctx, "sam", "", "user")
	sc := cookieFor(t, f.a, sam.ID)
	for _, p := range []string{"/", "/sam", "/zoe"} {
		body := expectStatus(t, do(t, f.a, "GET", p, nil, sc), 200, "sam "+p)
		mustNotContain(t, body, "sam "+p, "elyndrastudios", "Elyndra", "Repositories:")
		mustContain(t, body, "sam "+p, `href="/sam?tab=repositories"`)
	}
	// Anonymous visitors are sent to sign in.
	rec := do(t, f.a, "GET", "/elyndrastudios", nil, nil)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin") {
		t.Errorf("anonymous /elyndrastudios = %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

// #16: near-miss addresses redirect to the account they mean, or show
// "Did you mean"; never revealing accounts the viewer can't know about.
func TestForgivingAccountAddresses(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	zoe, _ := f.a.db.OwnerByName(ctx, "zoe")
	co, _ := f.a.db.CreateOwner(ctx, "elyndrastudios", "Elyndra Studios LTD", "company")
	priv, err := f.a.db.CreateRepo(ctx, co.ID, "RebelVerify", "Zero knowledge", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.a.db.SetAccountGitHub(ctx, co.ID, "Elyndra-GH"); err != nil {
		t.Fatal(err)
	}
	zc := cookieFor(t, f.a, zoe.ID)
	redirects := map[string]string{
		"/Elyndra-Studios":       "/elyndrastudios",
		"/ElyndraStudios":        "/elyndrastudios",
		"/elyndra.studios":       "/elyndrastudios",
		"/Elyndra_Studios_LTD":   "/elyndrastudios",
		"/elyndra-gh":            "/elyndrastudios", // the GitHub organisation it's linked to
		"/Elyndra-Studios?tab=x": "/elyndrastudios?tab=x",
		"/ZOE":                   "/zoe",
	}
	for from, to := range redirects {
		rec := do(t, f.a, "GET", from, nil, zc)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != to {
			t.Errorf("GET %s = %d → %q, want 302 → %q", from, rec.Code, rec.Header().Get("Location"), to)
		}
	}
	// Not close enough to be sure: a helpful 404.
	body := expectStatus(t, do(t, f.a, "GET", "/elyndra", nil, zc), http.StatusNotFound, "/elyndra")
	mustContain(t, body, "did you mean", "Did you mean", `href="/elyndrastudios"`, "Look on GitHub:", `href="/gh/elyndra"`)
	body = expectStatus(t, do(t, f.a, "GET", "/nothing-like-it", nil, zc), http.StatusNotFound, "far miss")
	mustContain(t, body, "far miss", "Nothing here", `href="/gh/nothing-like-it"`)
	mustNotContain(t, body, "far miss", "Did you mean")

	// A second person can't learn the company exists (its only repository is
	// private), neither by its name nor by its GitHub organisation.
	sam, _ := f.a.db.CreateOwner(ctx, "sam", "", "user")
	sc := cookieFor(t, f.a, sam.ID)
	for _, p := range []string{"/Elyndra-Studios", "/elyndra-gh", "/elyndra", "/elyndra.studios"} {
		body := expectStatus(t, do(t, f.a, "GET", p, nil, sc), http.StatusNotFound, "sam "+p)
		mustNotContain(t, body, "sam "+p, "elyndrastudios", "Elyndra Studios", "Did you mean")
	}
	// Nor zoe's personal account (nothing of hers is readable to sam).
	body = expectStatus(t, do(t, f.a, "GET", "/zoey", nil, sc), http.StatusNotFound, "sam /zoey")
	mustNotContain(t, body, "sam /zoey", `href="/zoe"`, "Did you mean")
	if body := expectStatus(t, do(t, f.a, "GET", "/zoey", nil, zc), http.StatusNotFound, "zoe /zoey"); !strings.Contains(body, `href="/zoe"`) {
		t.Error("zoe should be offered her own account for /zoey")
	}

	// Once it has a public repository the display name works for sam, but
	// the GitHub link (zoe's own setting) still doesn't.
	if err := f.a.db.UpdateRepo(ctx, priv.ID, "Zero knowledge", false); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, f.a, "GET", "/Elyndra-Studios", nil, sc); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/elyndrastudios" {
		t.Errorf("sam /Elyndra-Studios after going public = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	body = expectStatus(t, do(t, f.a, "GET", "/elyndra-gh", nil, sc), http.StatusNotFound, "sam /elyndra-gh")
	mustNotContain(t, body, "sam GitHub org", "/elyndrastudios")

	// Anonymous visitors never get a redirect or suggestion: sign in first.
	for _, p := range []string{"/Elyndra-Studios", "/elyndra"} {
		rec := do(t, f.a, "GET", p, nil, nil)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin") || strings.Contains(rec.Body.String(), "elyndrastudios") {
			t.Errorf("anonymous %s = %d %s", p, rec.Code, rec.Header().Get("Location"))
		}
	}
}
