package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
)

// profRepo creates a repository for owner, optionally with files committed
// (one commit per file) on main.
func profRepo(t *testing.T, a *App, owner store.Owner, name string, private bool, files ...string) store.Repo {
	t.Helper()
	ctx := context.Background()
	if err := a.git.Init(ctx, owner.Name, name); err != nil {
		t.Fatal(err)
	}
	rp, err := a.db.CreateRepo(ctx, owner.ID, name, "about "+name, private)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		return rp
	}
	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", ".")
	for i := 0; i+1 < len(files); i += 2 {
		p := filepath.Join(work, files[i])
		if err := os.WriteFile(p, []byte(files[i+1]), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, work, "add", ".")
		runGit(t, work, "commit", "-q", "-m", "add "+files[i])
	}
	runGit(t, work, "push", "-q", a.git.Path(owner.Name, name), "main")
	return rp
}

func cookieFor(t *testing.T, a *App, personID int64) map[string]string {
	t.Helper()
	tok, err := a.db.CreateSessionFor(context.Background(), personID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Cookie": sessionCookie + "=" + tok}
}

type profFixture struct {
	a         *App
	zoe, sam  store.Owner
	acme      store.Owner
	zc, sc    map[string]string // cookies
	demo, pub store.Repo
}

// newProfFixture: zoe (personal owner) with private demo (3 commits) and
// public open; a second person sam; and a company acme.
func newProfFixture(t *testing.T) profFixture {
	t.Helper()
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	zoe, _ := a.db.OwnerByName(ctx, "zoe")
	sam, err := a.db.CreateOwner(ctx, "sam", "Sam", "user")
	if err != nil {
		t.Fatal(err)
	}
	acme, err := a.db.CreateOwner(ctx, "acme", "Acme Ltd", "company")
	if err != nil {
		t.Fatal(err)
	}
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", ".")
	for i := 0; i < 3; i++ {
		runGit(t, work, "commit", "-q", "--allow-empty", "-m", "secret work")
	}
	runGit(t, work, "push", "-q", a.git.Path("zoe", "demo"), "main")
	pub := profRepo(t, a, zoe, "open", false, "main.go", "package main\n")
	return profFixture{a: a, zoe: zoe, sam: sam, acme: acme, zc: cookieFor(t, a, zoe.ID), sc: cookieFor(t, a, sam.ID), demo: demo, pub: pub}
}

func (f profFixture) get(t *testing.T, path string, cookie map[string]string, want int) string {
	t.Helper()
	return expectStatus(t, do(t, f.a, "GET", path, nil, cookie), want, "GET "+path)
}

func TestProfilePageAndPrivacy(t *testing.T) {
	f := newProfFixture(t)
	ctx := context.Background()
	if err := f.a.db.SetPinnedRepos(ctx, f.zoe.ID, []int64{f.demo.ID, f.pub.ID}); err != nil {
		t.Fatal(err)
	}

	own := f.get(t, "/zoe", f.zc, http.StatusOK)
	mustContain(t, own, "own profile", "Edit profile", `href="/zoe/demo"`, `href="/zoe/open"`, "Pinned",
		"4 commits", "2 repositories", "Add a profile README", "prof-avatar")
	if strings.Contains(own, "is-company") {
		t.Error("a person's avatar shouldn't use the company style")
	}

	// A second signed-in person sees what anonymous visitors see: the public
	// repository only, never the private one.
	other := f.get(t, "/zoe", f.sc, http.StatusOK)
	mustContain(t, other, "someone else's view", "1 repository", "1 commit", `href="/zoe/open"`)
	all, _ := f.a.db.ReposByOwner(ctx, f.zoe.ID)
	if vis := f.a.readableRepos(ctx, Actor{Kind: ActorAnonymous}, all); len(vis) != 1 || vis[0].Name != "open" {
		t.Errorf("anonymous visitor sees %v, want only open", vis)
	}
	for _, leak := range []string{"zoe/demo", "about demo", "Edit profile", "Add a profile README", "/settings/profile"} {
		if strings.Contains(other, leak) {
			t.Errorf("sam's view of /zoe leaks %q", leak)
		}
	}

	// Search and sort.
	body := f.get(t, "/zoe?q=OPE&sort=name", f.zc, http.StatusOK)
	list := body[strings.Index(body, `id="repositories"`):]
	if !strings.Contains(list, `href="/zoe/open"`) || strings.Contains(list, `href="/zoe/demo"`) {
		t.Error("search for 'ope' should list only open")
	}
	mustContain(t, body, "search", "1 result")

	// Company profile: violet, editable by the personal owner.
	co := f.get(t, "/acme", f.zc, http.StatusOK)
	mustContain(t, co, "company profile", "is-company", "prof-company", "Acme Ltd", "Edit profile")
	if strings.Contains(f.get(t, "/acme", f.sc, http.StatusOK), "Edit profile") {
		t.Error("sam shouldn't be able to edit acme")
	}
	f.get(t, "/nobody", f.zc, http.StatusNotFound)
}

func TestProfilePinsFallback(t *testing.T) {
	f := newProfFixture(t)
	ctx := context.Background()
	readme := profRepo(t, f.a, f.zoe, "zoe", false, "README.md", "# Hi\n")
	_ = f.a.db.TouchRepo(ctx, readme.ID)
	who := Actor{Kind: ActorOwner, Name: "zoe", PersonID: f.zoe.ID}
	all, _ := f.a.db.ReposByOwner(ctx, f.zoe.ID)
	readable := f.a.readableRepos(ctx, who, all)
	pins, fallback, err := f.a.profilePins(ctx, who, f.zoe, readable, &readme)
	if err != nil || !fallback || len(pins) != 2 {
		t.Fatalf("fallback pins = %v, %v, %v", pins, fallback, err)
	}
	for _, p := range pins {
		if p.ID == readme.ID {
			t.Error("the profile README repository shouldn't be in the fallback")
		}
	}
	// Only private pins and a stranger: they fall back to what's visible.
	_ = f.a.db.SetPinnedRepos(ctx, f.zoe.ID, []int64{f.demo.ID})
	sam := Actor{Kind: ActorOwner, Name: "sam", PersonID: f.sam.ID}
	pins, fallback, _ = f.a.profilePins(ctx, sam, f.zoe, f.a.readableRepos(ctx, sam, all), nil)
	if !fallback {
		t.Error("a stranger seeing no readable pins should get the fallback")
	}
	for _, p := range pins {
		if p.Private {
			t.Errorf("stranger sees private %s", p.Name)
		}
	}
}

func TestProfileReadme(t *testing.T) {
	f := newProfFixture(t)
	ctx := context.Background()

	// GitHub convention: zoe/zoe.
	profRepo(t, f.a, f.zoe, "zoe", false, "README.md", "# Hello from zoe\n\nI make **things**.\n")
	body := f.get(t, "/zoe", f.zc, http.StatusOK)
	mustContain(t, body, "zoe/zoe README", "Hello from zoe", "<strong>things</strong>", "zoe/zoe")
	if strings.Contains(body, "Add a profile README") {
		t.Error("empty-state hint shown alongside a README")
	}

	// Gitea / Forgejo convention: acme/.profile (lower-case readme.md too).
	profRepo(t, f.a, f.acme, ".profile", false, "readme.md", "# Acme builds\n")
	mustContain(t, f.get(t, "/acme", f.zc, http.StatusOK), "acme/.profile README", "Acme builds")

	// A private profile README is the owner's alone.
	// (zoe is the personal owner, so core policy lets her read everything;
	// check with an anonymous visitor.)
	profRepo(t, f.a, f.sam, "sam", true, "README.md", "# Sam's secret plan\n")
	anon := Actor{Kind: ActorAnonymous, Name: "anonymous"}
	if rp, b := f.a.profileReadme(ctx, anon, f.sam); rp != nil || b != nil {
		t.Errorf("anonymous visitor found private profile README %v", rp)
	}
	if rp, b := f.a.profileReadme(ctx, anon, f.zoe); rp == nil || rp.Name != "zoe" || !strings.Contains(string(b), "Hello from zoe") {
		t.Errorf("visitors should see zoe's public profile README, got %v", rp)
	}

	// A profile repository without a README: hint points at it.
	profRepo(t, f.a, f.sam, ".profile", false)
	if rp, b := f.a.profileReadme(ctx, anon, f.sam); rp == nil || rp.Name != ".profile" || b != nil {
		t.Errorf("empty .profile lookup = %v, %q", rp, b)
	}
}

func TestSanitizeWebsite(t *testing.T) {
	ok := map[string]string{
		"":                             "",
		"  https://demi.example  ":     "https://demi.example",
		"demi.example":                 "https://demi.example",
		"HTTP://Demi.Example/a?b=1#c":  "http://demi.example/a?b=1#c",
		"demi.example:8080/blog":       "https://demi.example:8080/blog",
		"https://example.com/~me/path": "https://example.com/~me/path",
	}
	for in, want := range ok {
		got, err := sanitizeWebsite(in)
		if err != nil || got != want {
			t.Errorf("sanitizeWebsite(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"javascript:alert(1)", "JavaScript://%0aalert(1)", "data:text/html,hi", "mailto:me@example.com",
		"ftp://example.com", "https://user:pass@example.com", "https://", "//example.com/x\"onmouseover=1",
		"https://exa mple.com", "https://example.com/<script>", "vbscript:x", "https://" + strings.Repeat("a", 300) + ".com",
	} {
		if got, err := sanitizeWebsite(in); err == nil {
			t.Errorf("sanitizeWebsite(%q) = %q, want an error", in, got)
		}
	}
}

func TestCanEditAccount(t *testing.T) {
	f := newProfFixture(t)
	ctx := context.Background()
	zoe := Actor{Kind: ActorOwner, Name: "zoe", PersonID: f.zoe.ID}
	sam := Actor{Kind: ActorOwner, Name: "sam", PersonID: f.sam.ID}
	tok := Actor{Kind: ActorToken, Name: "t", PersonID: f.zoe.ID, Scopes: []string{"repo:write"}}
	anon := Actor{Kind: ActorAnonymous}
	cases := []struct {
		name string
		who  Actor
		acct store.Owner
		want bool
	}{
		{"owner edits own", zoe, f.zoe, true},
		{"owner edits company", zoe, f.acme, true},
		{"person edits own", sam, f.sam, true},
		{"person can't edit another person", sam, f.zoe, false},
		{"person can't edit company in core", sam, f.acme, false},
		{"owner can't edit another person", zoe, f.sam, false},
		{"tokens never edit", tok, f.zoe, false},
		{"anonymous never edits", anon, f.acme, false},
	}
	for _, c := range cases {
		if got := f.a.canEditAccount(ctx, c.who, c.acct); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// A Company addon could let sam administer acme.
	saved := accountEditPolicies
	t.Cleanup(func() { accountEditPolicies = saved })
	registerAccountEditPolicy(func(ctx context.Context, a *App, who Actor, acct store.Owner) (bool, bool) {
		if who.PersonID == f.sam.ID && acct.ID == f.acme.ID {
			return true, true
		}
		return false, false
	})
	if !f.a.canEditAccount(ctx, sam, f.acme) || f.a.canEditAccount(ctx, sam, f.zoe) {
		t.Error("registered policy not applied")
	}
}

func TestProfileEditFlow(t *testing.T) {
	f := newProfFixture(t)
	ctx := context.Background()
	a := f.a

	// Permissions on the editor.
	mustContain(t, f.get(t, "/settings/profile", f.zc, http.StatusOK), "own editor", "Your profile", "Pinned repositories", "Acme Ltd")
	mustContain(t, f.get(t, "/settings/profile?account=acme", f.zc, http.StatusOK), "company editor", "Acme Ltd profile", "prof-company")
	mustContain(t, f.get(t, "/settings/profile", f.sc, http.StatusOK), "sam's own editor", "Your profile")
	f.get(t, "/settings/profile?account=zoe", f.sc, http.StatusForbidden)
	f.get(t, "/settings/profile?account=acme", f.sc, http.StatusForbidden)
	f.get(t, "/settings/profile?account=nobody", f.zc, http.StatusNotFound)
	if rec := do(t, a, "POST", "/settings/profile", url.Values{"account": {"zoe"}, "bio": {"hacked"}}, f.sc); rec.Code != http.StatusForbidden {
		t.Errorf("sam editing zoe = %d", rec.Code)
	}
	if rec := do(t, a, "POST", "/settings/profile/pins", url.Values{"account": {"acme"}}, f.sc); rec.Code != http.StatusForbidden {
		t.Errorf("sam pinning for acme = %d", rec.Code)
	}

	// Bad website and avatar are refused.
	bad := url.Values{"account": {"zoe"}, "website": {"javascript:alert(1)"}, "avatar": {""}}
	mustContain(t, expectStatus(t, do(t, a, "POST", "/settings/profile", bad, f.zc), http.StatusUnprocessableEntity, "bad website"),
		"bad website", "http:// or https://")
	bad = url.Values{"account": {"zoe"}, "avatar": {"<x>"}}
	expectStatus(t, do(t, a, "POST", "/settings/profile", bad, f.zc), http.StatusUnprocessableEntity, "bad avatar")
	bad = url.Values{"account": {"zoe"}, "bio": {strings.Repeat("é", maxBio+1)}}
	expectStatus(t, do(t, a, "POST", "/settings/profile", bad, f.zc), http.StatusUnprocessableEntity, "long bio")

	good := url.Values{"account": {"zoe"}, "display_name": {"Zoë Quinn"}, "bio": {"Makes <b>things</b>."},
		"location": {"Cork"}, "website": {"zoe.example/about"}, "pronouns": {"she/her"}, "avatar": {"initials"}}
	rec := do(t, a, "POST", "/settings/profile", good, f.zc)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "saved=1") {
		t.Fatalf("save = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	p, _ := a.db.Profile(ctx, f.zoe.ID)
	if p.Website != "https://zoe.example/about" || p.UpdatedBy != "zoe" || p.UpdatedByKind != ActorOwner || p.UpdatedByPerson != f.zoe.ID {
		t.Errorf("stored profile = %+v", p)
	}
	page := f.get(t, "/zoe", f.sc, http.StatusOK)
	mustContain(t, page, "saved profile", "Zoë Quinn", "she/her", "Cork", `href="https://zoe.example/about" rel="me noopener"`,
		"zoe.example/about", "Makes &lt;b&gt;things&lt;/b&gt;.", ">ZQ<")

	// Pins: tick both, then move "open" up without JavaScript.
	pins := url.Values{"account": {"zoe"}, "pin": {itoa(f.demo.ID), itoa(f.pub.ID)}}
	if rec := do(t, a, "POST", "/settings/profile/pins", pins, f.zc); rec.Code != http.StatusSeeOther {
		t.Fatalf("save pins = %d", rec.Code)
	}
	pins.Set("move", "up:"+itoa(f.pub.ID))
	do(t, a, "POST", "/settings/profile/pins", pins, f.zc)
	got, _ := a.db.PinnedRepos(ctx, f.zoe.ID)
	if len(got) != 2 || got[0].Name != "open" || got[1].Name != "demo" {
		t.Errorf("pins after move up = %v", got)
	}
	// Up/down buttons render, with the first "up" disabled.
	ed := f.get(t, "/settings/profile", f.zc, http.StatusOK)
	mustContain(t, ed, "pin picker", `value="up:`+itoa(f.pub.ID)+`" aria-label="Move open up" disabled`,
		`value="down:`+itoa(f.demo.ID)+`" aria-label="Move demo down" disabled`, "2 of 6 pinned")

	// More than six is refused with a message, and nothing changes.
	var many []string
	for _, n := range []string{"r1", "r2", "r3", "r4", "r5", "r6"} {
		many = append(many, itoa(profRepo(t, a, f.zoe, n, false).ID))
	}
	many = append(many, itoa(f.pub.ID))
	rec = do(t, a, "POST", "/settings/profile/pins", url.Values{"account": {"zoe"}, "pin": many}, f.zc)
	mustContain(t, expectStatus(t, rec, http.StatusUnprocessableEntity, "seven pins"), "seven pins", "You can pin up to 6")
	if got, _ := a.db.PinnedRepos(ctx, f.zoe.ID); len(got) != 2 {
		t.Errorf("refused save changed pins: %v", got)
	}
	// Another account's repositories are ignored.
	other := profRepo(t, a, f.acme, "tools", false)
	do(t, a, "POST", "/settings/profile/pins", url.Values{"account": {"zoe"}, "pin": {itoa(other.ID), itoa(f.pub.ID)}}, f.zc)
	if got, _ := a.db.PinnedRepos(ctx, f.zoe.ID); len(got) != 1 || got[0].Name != "open" {
		t.Errorf("foreign pin accepted: %v", got)
	}

	// Company profile edits are kept separate.
	co := url.Values{"account": {"acme"}, "display_name": {"Acme"}, "bio": {"We build."}, "avatar": {"spark"}}
	if rec := do(t, a, "POST", "/settings/profile", co, f.zc); rec.Code != http.StatusSeeOther {
		t.Fatalf("company save = %d", rec.Code)
	}
	mustContain(t, f.get(t, "/acme", f.sc, http.StatusOK), "company page", "We build.", "<svg viewBox=\"0 0 24 24\"")
	if zp, _ := a.db.Profile(ctx, f.zoe.ID); zp.Bio != "Makes <b>things</b>." {
		t.Errorf("company save touched zoe's profile: %q", zp.Bio)
	}
}

func TestProfileEmbers(t *testing.T) {
	f := newProfFixture(t)
	ctx := context.Background()
	zoe := Actor{Kind: ActorOwner, Name: "zoe", PersonID: f.zoe.ID}
	sam := Actor{Kind: ActorOwner, Name: "sam", PersonID: f.sam.ID}
	all, _ := f.a.db.ReposByOwner(ctx, f.zoe.ID)

	cells, total := f.a.embers(ctx, f.a.readableRepos(ctx, zoe, all))
	if total != 4 { // 3 in demo, 1 in open
		t.Errorf("zoe's embers total = %d, want 4", total)
	}
	if n := len(cells); n < 52*7+1 || n > 53*7 {
		t.Errorf("embers has %d cells", n)
	}
	last := cells[len(cells)-1]
	if last.Level != 2 || !strings.HasPrefix(last.Title, "4 commits on ") {
		t.Errorf("today's cell = %+v, want level 2 with 4 commits", last)
	}
	anon := Actor{Kind: ActorAnonymous}
	if _, total := f.a.embers(ctx, f.a.readableRepos(ctx, anon, all)); total != 1 {
		t.Errorf("visitor sees %d commits, want 1 (private commits hidden)", total)
	}
	if _, total := f.a.embers(ctx, f.a.readableRepos(ctx, sam, all)); total != 1 {
		t.Errorf("sam sees %d commits, want 1 (public repo only)", total)
	}
	mustContain(t, f.get(t, "/zoe", f.sc, http.StatusOK), "sam's embers", "1 commit")
	mustContain(t, f.get(t, "/zoe", f.zc, http.StatusOK), "zoe's embers", "4 commits")
}

func TestProfileHelpers(t *testing.T) {
	for in, want := range map[string]string{"Zoë Quinn": "ZQ", "acme": "A", "elyndra-studios": "ES", "": "?", "  ": "?"} {
		if got := initialsOf(in); got != want {
			t.Errorf("initialsOf(%q) = %q, want %q", in, got, want)
		}
	}
	ids := applyPinMove([]int64{1, 2, 3}, "down:1")
	ids = applyPinMove(ids, "up:3")
	ids = applyPinMove(ids, "up:3")
	ids = applyPinMove(ids, "sideways:2")
	if want := []int64{3, 2, 1}; len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Errorf("applyPinMove = %v", ids)
	}
	if countLabel(1, "repository", "repositories") != "1 repository" || countLabel(0, "commit", "commits") != "0 commits" {
		t.Error("countLabel")
	}
}
