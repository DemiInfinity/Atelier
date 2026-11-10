package web

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"atelier/internal/store"
)

func TestCorePolicy(t *testing.T) {
	f := newGitFixture(t) // owner "zoe" with private repo zoe/demo
	ctx := context.Background()
	a := f.a
	me, _ := a.db.PersonalOwner(ctx)
	private, _ := a.db.RepoByName(ctx, "zoe", "demo")
	public := private
	public.Private = false

	owner := Actor{Kind: ActorOwner, Name: me.Name, PersonID: me.ID}
	readTok := Actor{Kind: ActorToken, Name: "r", PersonID: me.ID, Scopes: []string{"repo:read"}}
	writeTok := Actor{Kind: ActorToken, Name: "w", PersonID: me.ID, Scopes: []string{"repo:read", "repo:write"}}
	anon := Actor{Kind: ActorAnonymous}
	stranger := Actor{Kind: ActorOwner, Name: "someone", PersonID: me.ID + 999}
	strangerTok := Actor{Kind: ActorToken, PersonID: me.ID + 999, Scopes: []string{"repo:write"}}
	bot := Actor{Kind: "bot", Name: "review-helper", PersonID: me.ID}

	cases := []struct {
		name string
		who  Actor
		repo store.Repo
		need Perm
		want bool
	}{
		{"owner reads", owner, private, PermRead, true},
		{"owner writes", owner, private, PermWrite, true},
		{"owner administers", owner, private, PermAdmin, true},
		{"read token reads", readTok, private, PermRead, true},
		{"read token can't push", readTok, private, PermWrite, false},
		{"write token pushes", writeTok, private, PermWrite, true},
		{"tokens never administer", writeTok, private, PermAdmin, false},
		{"anonymous can't see private", anon, private, PermRead, false},
		{"anonymous reads public", anon, public, PermRead, true},
		{"anonymous can't push public", anon, public, PermWrite, false},
		{"another person is locked out", stranger, private, PermRead, false},
		{"another person's token is locked out", strangerTok, private, PermWrite, false},
		{"bots get nothing in core", bot, private, PermRead, false},
	}
	for _, c := range cases {
		if got := a.can(ctx, c.who, c.repo, c.need); got != c.want {
			t.Errorf("%s: can(%s) = %v, want %v", c.name, c.need, got, c.want)
		}
	}
}

func TestAccessPolicyOverride(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	repo, _ := f.a.db.RepoByName(ctx, "zoe", "demo")
	friend := Actor{Kind: ActorOwner, Name: "friend", PersonID: 4242}
	if f.a.can(ctx, friend, repo, PermRead) {
		t.Fatal("friend shouldn't have access before a policy grants it")
	}
	// What a Duo addon would do: give one extra person read access.
	saved := accessPolicies
	t.Cleanup(func() { accessPolicies = saved })
	registerAccessPolicy(func(ctx context.Context, a *App, who Actor, r store.Repo, need Perm) (bool, bool) {
		if who.PersonID == 4242 {
			return need == PermRead, true
		}
		return false, false
	})
	if !f.a.can(ctx, friend, repo, PermRead) || f.a.can(ctx, friend, repo, PermWrite) {
		t.Error("policy should allow read and refuse write for the friend")
	}
	me, _ := f.a.db.PersonalOwner(ctx)
	if !f.a.can(ctx, Actor{Kind: ActorOwner, PersonID: me.ID}, repo, PermAdmin) {
		t.Error("undecided requests should still fall through to the core policy")
	}
}

// A second person (as a future addon would create) who isn't granted anything
// must be locked out of the web UI and of git over HTTPS.
func TestSecondPersonIsLockedOut(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	other, err := f.a.db.CreateOwner(ctx, "visitor", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := f.a.db.CreateSessionFor(ctx, other.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := f.a.db.SessionPerson(ctx, sess); !ok || id != other.ID {
		t.Fatalf("session belongs to %d (ok=%v), want %d", id, ok, other.ID)
	}
	get := func(path string) int {
		req, _ := http.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
		return doReq(f.a, req).Code
	}
	if code := get("/zoe/demo"); code != http.StatusNotFound {
		t.Errorf("visitor viewing private repo: %d, want 404", code)
	}
	if code := get("/zoe/demo/settings"); code != http.StatusNotFound {
		t.Errorf("visitor viewing settings: %d, want 404", code)
	}

	// The visitor's own write token must not open the owner's repo either.
	tok, _ := f.a.db.CreateToken(ctx, "visitor", tokenScopes["write"], time.Hour)
	if err := f.a.db.AssignTokenPerson(ctx, tok, other.ID); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "/zoe/demo.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("visitor:"+tok)))
	if code := doReq(f.a, req).Code; code != http.StatusUnauthorized {
		t.Errorf("visitor token clone: %d, want 401", code)
	}

	// SSH goes through the same check.
	key, err := f.a.db.AddSSHKey(ctx, "visitor laptop", "ssh-ed25519", "SHA256:visitor", "ssh-ed25519 AAAA", "visitor", ActorOwner)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := f.a.db.RepoByName(ctx, "zoe", "demo")
	if f.a.sshAllow(ctx, key.ID, repo, false) {
		t.Error("visitor's SSH key could read the owner's private repo")
	}
	mine, err := f.a.db.AddSSHKey(ctx, "zoe laptop", "ssh-ed25519", "SHA256:zoe", "ssh-ed25519 BBBB", "zoe", ActorOwner)
	if err != nil {
		t.Fatal(err)
	}
	if !f.a.sshAllow(ctx, mine.ID, repo, true) {
		t.Error("owner's SSH key should be able to push")
	}
}

func TestOwnerStillHasFullAccess(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	sess, err := f.a.db.CreateSession(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/zoe/demo", "/zoe/demo/settings"} {
		req, _ := http.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
		if code := doReq(f.a, req).Code; code != http.StatusOK {
			t.Errorf("owner GET %s: %d, want 200", path, code)
		}
	}
	req, _ := http.NewRequest("POST", "/zoe/demo/settings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Body = http.NoBody
	req.Form = url.Values{"description": {"x"}}
	if code := doReq(f.a, req).Code; code != http.StatusSeeOther {
		t.Errorf("owner saving settings: %d, want 303", code)
	}
}

func doReq(a *App, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}
