package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"atelier/internal/store"
)

// "Use my GitHub avatar": fetched once from (a fake) avatars.githubusercontent.com,
// stored locally like an upload, refreshable; a company uses its linked
// organisation's avatar. The profile links to GitHub, and /gh/<you> says so.
func TestGitHubAvatarAndLinks(t *testing.T) {
	f := newGHFixture(t)
	a := f.a
	ctx := context.Background()
	me := f.me(t)
	pngBytes := testPNG(t, 120, 120)
	f.img.mu.Lock()
	f.img.answers["/u/9"] = func(w http.ResponseWriter, r *http.Request) { // the fake /user's avatar_url
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes)
	}
	f.img.answers["/u/77"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes)
	}
	f.img.answers["/u/66"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/x.png", http.StatusFound)
	}
	f.img.answers["/u/55"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	}
	f.img.mu.Unlock()
	f.gh.mu.Lock()
	f.w.profiles["zoe-gh"] = map[string]any{"login": "zoe-gh", "name": "Zoe", "type": "User", "avatar_url": "https://avatars.githubusercontent.com/u/9"}
	f.w.profiles["acme"]["avatar_url"] = "https://avatars.githubusercontent.com/u/77?v=4"
	f.w.profiles["evilorg"] = map[string]any{"login": "evilorg", "type": "Organization", "avatar_url": "https://evil.example/u/1"}
	f.w.profiles["hopper"] = map[string]any{"login": "hopper", "type": "Organization", "avatar_url": "https://avatars.githubusercontent.com/u/66"}
	f.w.profiles["svgorg"] = map[string]any{"login": "svgorg", "type": "Organization", "avatar_url": "https://avatars.githubusercontent.com/u/55"}
	f.gh.mu.Unlock()

	// The edit page offers it once Tether is connected.
	body := expectStatus(t, do(t, a, "GET", "/settings/profile", nil, f.ck), 200, "edit")
	mustContain(t, body, "edit", "Use my GitHub avatar", "Connected as", `href="/gh/zoe-gh"`)

	rec := do(t, a, "POST", "/settings/profile/picture", url.Values{"account": {me.Name}, "action": {"github"}}, f.ck)
	expectStatus(t, rec, http.StatusSeeOther, "use GitHub avatar")
	av, err := a.db.AccountAvatar(ctx, me.ID)
	if err != nil || av.Source != store.AvatarGitHub || av.SourceLogin != "zoe-gh" {
		t.Fatalf("avatar = %+v %v", av, err)
	}
	var hit *http.Request
	for _, r := range f.img.seen() {
		if r.URL.Path == "/u/9" {
			hit = r
		}
	}
	if hit == nil || hit.URL.Query().Get("s") != "460" || hit.Header.Get("Authorization") != "" || !strings.EqualFold(hit.Host, "avatars.githubusercontent.com") {
		t.Fatalf("avatar fetch = %+v", hit)
	}
	if rec := do(t, a, "GET", "/-/avatar/"+me.Name+"/460.png", nil, f.ck); rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("serving the GitHub avatar = %d", rec.Code)
	}
	body = expectStatus(t, do(t, a, "GET", "/settings/profile", nil, f.ck), 200, "edit after")
	mustContain(t, body, "edit after", "Refresh from GitHub", "From GitHub (zoe-gh)")
	before := len(f.img.seen())
	expectStatus(t, do(t, a, "POST", "/settings/profile/picture", url.Values{"account": {me.Name}, "action": {"github"}}, f.ck), http.StatusSeeOther, "refresh")
	if len(f.img.seen()) != before+1 {
		t.Error("refresh should fetch the avatar again")
	}

	// The profile shows the GitHub link; /gh/<you> says it's you.
	body = expectStatus(t, do(t, a, "GET", "/"+me.Name, nil, f.ck), 200, "profile")
	mustContain(t, body, "profile", "On GitHub:", `href="https://`, "zoe-gh", `href="/gh/zoe-gh"`)
	body = expectStatus(t, do(t, a, "GET", "/gh/zoe-gh", nil, f.ck), 200, "gh self")
	mustContain(t, body, "gh self", "This is you.", `href="/`+me.Name+`">Your Atelier profile`)
	body = expectStatus(t, do(t, a, "GET", "/gh/dave", nil, f.ck), 200, "gh dave")
	mustNotContain(t, body, "gh dave", "This is you.")

	// A company linked to its organisation uses the organisation's avatar,
	// and /<org> finds the company.
	co, _ := a.db.CreateOwner(ctx, "acmeco", "Acme Company", "company")
	body = expectStatus(t, do(t, a, "GET", "/settings/profile?account=acmeco", nil, f.ck), 200, "company edit")
	mustContain(t, body, "company edit", "GitHub organisation")
	mustNotContain(t, body, "company edit", "GitHub avatar</button>")
	rec = do(t, a, "POST", "/settings/profile/links", url.Values{"account": {"acmeco"}, "github": {"not a login!"}}, f.ck)
	expectStatus(t, rec, http.StatusUnprocessableEntity, "bad org")
	expectStatus(t, do(t, a, "POST", "/settings/profile/links", url.Values{"account": {"acmeco"}, "github": {"acme"}}, f.ck), http.StatusSeeOther, "link org")
	body = expectStatus(t, do(t, a, "GET", "/settings/profile?account=acmeco", nil, f.ck), 200, "company edit linked")
	mustContain(t, body, "company edit linked", "Use acme's GitHub avatar")
	expectStatus(t, do(t, a, "POST", "/settings/profile/picture", url.Values{"account": {"acmeco"}, "action": {"github"}}, f.ck), http.StatusSeeOther, "org avatar")
	if av, err := a.db.AccountAvatar(ctx, co.ID); err != nil || av.SourceLogin != "acme" {
		t.Fatalf("company avatar = %+v %v", av, err)
	}
	if rec := do(t, a, "GET", "/acme", nil, f.ck); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/acmeco" {
		t.Errorf("/acme = %d %q, want → /acmeco", rec.Code, rec.Header().Get("Location"))
	}
	body = expectStatus(t, do(t, a, "GET", "/acmeco", nil, f.ck), 200, "company profile")
	mustContain(t, body, "company profile", "On GitHub:", `href="/gh/acme"`)

	// Avatars from anywhere but GitHub's avatar host, redirects away from it,
	// and non-images are refused; the old picture stays.
	for org, msg := range map[string]string{"evilorg": "GitHub has no avatar", "hopper": "couldn&#39;t be reached", "svgorg": "Use a PNG, JPEG or WebP"} {
		expectStatus(t, do(t, a, "POST", "/settings/profile/links", url.Values{"account": {"acmeco"}, "github": {org}}, f.ck), http.StatusSeeOther, "link "+org)
		rec := do(t, a, "POST", "/settings/profile/picture", url.Values{"account": {"acmeco"}, "action": {"github"}}, f.ck)
		mustContain(t, expectStatus(t, rec, http.StatusUnprocessableEntity, org), org, msg)
		if av, _ := a.db.AccountAvatar(ctx, co.ID); av.SourceLogin != "acme" {
			t.Errorf("%s replaced the picture: %+v", org, av)
		}
	}
	for _, r := range f.img.seen() {
		if !strings.EqualFold(r.Host, "avatars.githubusercontent.com") && !strings.Contains(r.Host, "githubusercontent.com") {
			t.Errorf("fetched from %s", r.Host)
		}
	}

	// A second person sees neither the guessed links nor zoe's tools.
	sam, _ := a.db.CreateOwner(ctx, "sam", "", "user")
	sc := cookieFor(t, a, sam.ID)
	if rec := do(t, a, "POST", "/settings/profile/picture", url.Values{"account": {"acmeco"}, "action": {"github"}}, sc); rec.Code != http.StatusForbidden {
		t.Errorf("sam using acmeco's GitHub avatar = %d", rec.Code)
	}
	if rec := do(t, a, "POST", "/settings/profile/links", url.Values{"account": {"acmeco"}, "github": {"x"}}, sc); rec.Code != http.StatusForbidden {
		t.Errorf("sam linking acmeco = %d", rec.Code)
	}
}
