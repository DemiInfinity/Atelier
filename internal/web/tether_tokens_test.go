package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
)

const fakeOrgToken = "github_pat_ORGTOKEN_never_show"

// orgToken lets the fake GitHub accept a token that covers Elyndra-Studios only,
// and hides that organisation from the main token.
func (f *fakeGH) orgToken(owners ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokens == nil {
		f.tokens = map[string]fakeToken{}
	}
	if f.mainDenied == nil {
		f.mainDenied = map[string]bool{}
	}
	f.tokens[fakeOrgToken] = fakeToken{login: "zoe-gh", owners: owners}
	for _, o := range owners {
		f.mainDenied[strings.ToLower(o)] = true
	}
}

func (f *fakeGH) tokenHits(token, method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byToken[token+" "+method+" "+path]
}

func (f *fakeGH) resetTokenHits() {
	f.mu.Lock()
	f.byToken = map[string]int{}
	f.mu.Unlock()
}

func TestTetherMoreTokensSettings(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	f.gh.orgToken("Elyndra-Studios")
	f.gh.addRepo("Elyndra-Studios/sparky-ventures", true)

	// Needs the main token first.
	expectStatus(t, do(t, a, "POST", "/settings/tether/tokens", url.Values{"token": {fakeOrgToken}}, f.ck), 422, "no main")
	f.connect(t)
	body := expectStatus(t, do(t, a, "GET", "/settings/tether", nil, f.ck), 200, "settings")
	mustContain(t, body, "settings", "Main token", "More tokens", "Add another token")

	// Checked with GitHub before saving.
	expectStatus(t, do(t, a, "POST", "/settings/tether/tokens", url.Values{"token": {"github_pat_wrong"}}, f.ck), 422, "bad token")
	// The main token again is refused.
	expectStatus(t, do(t, a, "POST", "/settings/tether/tokens", url.Values{"token": {fakeGHToken}}, f.ck), 422, "duplicate")
	// No owners typed: the owners the token can see are suggested.
	expectStatus(t, do(t, a, "POST", "/settings/tether/tokens", url.Values{"token": {fakeOrgToken}, "label": {"Studio org"}}, f.ck), 303, "add")
	all, err := a.db.TetherExtraTokens(ctx, f.me(t).ID)
	if err != nil || len(all) != 1 {
		t.Fatalf("tokens = %v, %v", all, err)
	}
	tk := all[0]
	if strings.Join(tk.Owners, ",") != "Elyndra-Studios" || tk.Login != "zoe-gh" || tk.Scopes != "fine-grained" || tk.RateLimit != 5000 {
		t.Errorf("saved token = %+v", tk)
	}
	body = expectStatus(t, do(t, a, "GET", "/settings/tether", nil, f.ck), 200, "listed")
	mustContain(t, body, "listed", "Studio org", "zoe-gh", "Elyndra-Studios", "fine-grained", "4990 of 5000",
		"/settings/tether/tokens/"+strconv.FormatInt(tk.ID, 10)+"/delete")
	if strings.Contains(body, fakeOrgToken) || strings.Contains(body, fakeGHToken) {
		t.Fatal("a token was rendered")
	}
	expectStatus(t, do(t, a, "POST", "/settings/tether/tokens", url.Values{"token": {fakeOrgToken}}, f.ck), 422, "same token twice")

	// Owners can be edited; bad names are refused.
	id := strconv.FormatInt(tk.ID, 10)
	expectStatus(t, do(t, a, "POST", "/settings/tether/tokens/"+id+"/owners", url.Values{"owners": {"<script>"}}, f.ck), 422, "bad owner")
	expectStatus(t, do(t, a, "POST", "/settings/tether/tokens/"+id+"/owners", url.Values{"owners": {"Elyndra-Studios, elyndra-labs"}}, f.ck), 303, "owners")
	if got, _ := a.db.TetherExtraToken(ctx, f.me(t).ID, tk.ID); strings.Join(got.Owners, ",") != "elyndra-labs,Elyndra-Studios" {
		t.Errorf("owners = %v", got.Owners)
	}

	// A second person can't see, use, change or remove it.
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := a.db.CreateSessionFor(ctx, sam.ID, time.Hour)
	samCk := map[string]string{"Cookie": sessionCookie + "=" + sess}
	body = expectStatus(t, do(t, a, "GET", "/settings/tether", nil, samCk), 200, "sam settings")
	if strings.Contains(body, "Studio org") || strings.Contains(body, "Elyndra-Studios") {
		t.Error("sam saw zoe's extra token")
	}
	if _, _, err := a.tetherTokenFor(ctx, sam.ID, "Elyndra-Studios"); !errors.Is(err, errTetherNotConnected) {
		t.Errorf("sam's pick = %v", err)
	}
	expectStatus(t, do(t, a, "POST", "/settings/tether/tokens/"+id+"/owners", url.Values{"owners": {"x"}}, samCk), 404, "sam edits")
	expectStatus(t, do(t, a, "POST", "/settings/tether/tokens/"+id+"/delete", nil, samCk), 404, "sam deletes")
	if body := do(t, a, "GET", "/gh/Elyndra-Studios/sparky-ventures", nil, samCk).Body.String(); strings.Contains(body, "sparky-ventures</") {
		t.Error("sam browsed the org repository through zoe's token")
	}
	if n := f.gh.tokenHits(fakeOrgToken, "GET", "/repos/Elyndra-Studios/sparky-ventures"); n != 0 {
		t.Errorf("zoe's org token was used %d times for sam", n)
	}

	// Disconnect removes just that token.
	expectStatus(t, do(t, a, "POST", "/settings/tether/tokens/"+id+"/delete", nil, f.ck), 303, "delete")
	if all, _ := a.db.TetherExtraTokens(ctx, f.me(t).ID); len(all) != 0 {
		t.Error("token not removed")
	}
	if _, err := a.db.TetherAccount(ctx, f.me(t).ID); err != nil {
		t.Error("removing an extra token disconnected the main one")
	}
}

func TestTetherTokenSelection(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	f.gh.orgToken("Elyndra-Studios")
	f.connect(t)
	me := f.me(t).ID

	// Only the main token: it serves every owner (existing single-token data).
	for _, owner := range []string{"", "zoe-gh", "Elyndra-Studios"} {
		pick, _, err := a.tetherTokenFor(ctx, me, owner)
		if err != nil || pick.Token != fakeGHToken || pick.Extra != nil {
			t.Fatalf("before: %q → %+v, %v", owner, pick.Extra, err)
		}
	}
	if _, err := a.tetherAddToken(ctx, me, "", fakeOrgToken, []string{"Elyndra-Studios"}); err != nil {
		t.Fatal(err)
	}
	for owner, want := range map[string]string{"Elyndra-Studios": fakeOrgToken, "elyndra-STUDIOS": fakeOrgToken,
		"zoe-gh": fakeGHToken, "someone-else": fakeGHToken, "": fakeGHToken} {
		pick, _, err := a.tetherTokenFor(ctx, me, owner)
		if err != nil || pick.Token != want {
			t.Errorf("%q → wrong token (%v)", owner, err)
		}
	}
	// git uses the same pick.
	if tok, err := a.tetherLinkToken(ctx, me, "Elyndra-Studios/sparky-ventures"); err != nil || tok != fakeOrgToken {
		t.Errorf("git token for the org: %v", err)
	}
	if tok, err := a.tetherLinkToken(ctx, me, "zoe-gh/demo"); err != nil || tok != fakeGHToken {
		t.Errorf("git token for personal: %v", err)
	}
	// Search, feed and badges use the main token.
	if c, _, err := a.tetherClient(ctx, me); err != nil || c.Token != fakeGHToken {
		t.Errorf("main client: %v", err)
	}
	// Rate limits are recorded against the token used, and blocking is per token.
	c, _, _ := a.tetherClientFor(ctx, me, "Elyndra-Studios")
	if _, err := c.Repo(ctx, "Elyndra-Studios/missing"); err == nil {
		t.Fatal("expected a 404")
	}
	main, _, _ := a.tetherClient(ctx, me)
	f.gh.mu.Lock()
	f.gh.limited = true
	f.gh.mu.Unlock()
	_, _ = c.Repo(ctx, "Elyndra-Studios/x")
	f.gh.mu.Lock()
	f.gh.limited = false
	f.gh.mu.Unlock()
	if _, blocked := c.BlockedUntil(); !blocked {
		t.Error("the org token isn't blocked")
	}
	if _, blocked := main.BlockedUntil(); blocked {
		t.Error("the main token was blocked by the org token's limit")
	}
	c.Unblock()
}

func TestTetherLinkOrgRepoUsesOrgToken(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	f.gh.orgToken("Elyndra-Studios")
	f.gh.addRepo("Elyndra-Studios/sparky-ventures", true)
	f.connect(t)

	// No token covers the organisation: a clear message.
	form := url.Values{"gh_repo": {"Elyndra-Studios/sparky-ventures"}, "home": {"github"}}
	body := expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/tether/link", form, f.ck), 422, "no org token")
	mustContain(t, body, "no org token", "No token covers Elyndra-Studios. Add one in Settings → Tether.")
	if _, err := a.db.TetherLink(ctx, f.repoID(t)); err == nil {
		t.Fatal("linked without a token")
	}

	if _, err := a.tetherAddToken(ctx, f.me(t).ID, "Studio", fakeOrgToken, []string{"elyndra-studios"}); err != nil {
		t.Fatal(err)
	}
	f.gh.resetTokenHits()
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/tether/link", form, f.ck), 303, "link")
	f.wait()
	l, err := a.db.TetherLink(ctx, f.repoID(t))
	if err != nil || l.GHRepo != "Elyndra-Studios/sparky-ventures" || l.Home != store.TetherHomeGitHub {
		t.Fatalf("link = %+v, %v", l, err)
	}
	if f.gh.tokenHits(fakeOrgToken, "GET", "/repos/Elyndra-Studios/sparky-ventures") == 0 {
		t.Error("the link wasn't checked with the org token")
	}
	f.sync(t)
	if f.gh.tokenHits(fakeOrgToken, "GET", "/repos/Elyndra-Studios/sparky-ventures/pulls") == 0 {
		t.Error("sync didn't use the org token")
	}
	f.gh.mu.Lock()
	for k := range f.gh.byToken {
		if strings.HasPrefix(k, fakeGHToken+" ") && strings.Contains(k, "Elyndra-Studios") {
			t.Errorf("the main token was used for the org: %s", k)
		}
	}
	f.gh.mu.Unlock()
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/settings/tether", nil, f.ck), 200, "repo page")
	mustContain(t, body, "repo page", "Studio")

	// Personal repositories keep using the main token.
	f.gh.addRepo("zoe-gh/demo", false)
	if c, _, err := a.tetherLinkClient(ctx, f.me(t).ID, "zoe-gh/demo"); err != nil || c.Token != fakeGHToken {
		t.Errorf("personal client: %v", err)
	}
}

func TestGHRepoPageUsesOrgToken(t *testing.T) {
	f := newGHFixture(t)
	f.wait()
	f.gh.orgToken("dave")
	if _, err := f.a.tetherAddToken(context.Background(), f.me(t).ID, "", fakeOrgToken, []string{"Dave"}); err != nil {
		t.Fatal(err)
	}
	f.gh.resetTokenHits()
	mustContain(t, f.get(t, "/gh/dave/rocket", 200), "org repo page", "Fast rockets for everyone")
	if f.gh.tokenHits(fakeOrgToken, "GET", "/repos/dave/rocket") == 0 {
		t.Error("the repository page didn't use the org token")
	}
	if n := f.gh.tokenHits(fakeGHToken, "GET", "/repos/dave/rocket"); n != 0 {
		t.Errorf("the main token was used %d times", n)
	}
	// The owner's profile uses the main token.
	f.gh.resetTokenHits()
	f.get(t, "/gh/dave", 200)
	if f.gh.tokenHits(fakeGHToken, "GET", "/users/dave") == 0 {
		t.Error("the profile didn't use the main token")
	}
}

func TestTetherBadgeWithoutPublicAddress(t *testing.T) {
	f := newTetherFixture(t)
	a := f.a
	f.connect(t)
	f.gh.addRepo("zoe-gh/zoe-gh", false)
	for _, pub := range []string{"", "https://atelier.infinityhomelab.lan", "http://192.168.1.20:3000", "http://atelier"} {
		a.cfg.PublicURL = pub
		expectStatus(t, do(t, a, "POST", "/settings/tether/badge", url.Values{"on": {"1"}}, f.ck), 303, "badge on "+pub)
		f.gh.mu.Lock()
		file := f.gh.files["zoe-gh/zoe-gh/.atelier"]
		f.gh.mu.Unlock()
		var got tetherBadgeFileJSON
		if json.Unmarshal(file.data, &got) != nil || !got.Atelier || got.Studio != "" {
			t.Errorf("%q: badge file = %q", pub, file.data)
		}
		body := expectStatus(t, do(t, a, "GET", "/settings/tether", nil, f.ck), 200, "settings "+pub)
		mustContain(t, body, "settings "+pub, "Badge on: no public address yet, so there&#39;s no &#39;Visit their studio&#39; link")
		if strings.Contains(body, "infinityhomelab") || strings.Contains(body, "192.168") {
			t.Errorf("%q: the LAN address was shown", pub)
		}
	}
	// A public address arrives: the page offers to update the badge.
	a.cfg.PublicURL = "https://atelier.example.com"
	body := expectStatus(t, do(t, a, "GET", "/settings/tether", nil, f.ck), 200, "public")
	mustContain(t, body, "public", "Badge on, linking to https://atelier.example.com/zoe", "Update the badge")
}

func TestTetherPublicAddress(t *testing.T) {
	for in, want := range map[string]bool{
		"https://git.example.com": true, "https://atelier.example": true, "http://203.0.113.9": true,
		"": false, "https://atelier.infinityhomelab.lan": false, "http://localhost:3000": false, "http://10.0.0.2": false,
		"http://192.168.1.2": false, "http://100.101.102.103": false, "http://atelier": false, "https://box.local": false,
		"https://nas.home.arpa": false, "http://[::1]:80": false, "ftp://git.example.com": false,
	} {
		if got := tetherPublicAddress(in); got != want {
			t.Errorf("tetherPublicAddress(%q) = %v", in, got)
		}
	}
}
