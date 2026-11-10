package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func newWorkRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	runGit(t, work, "init", "--initial-branch=main", ".")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# New thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "First commit")
	return work
}

func tryGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestPushToCreateHTTPS(t *testing.T) {
	f := newGitFixture(t) // owner zoe, tokens read/write
	ctx := context.Background()
	srv := httptest.NewServer(f.a)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	at := func(tok, path string) string { return "http://zoe:" + tok + "@" + u.Host + path }
	work := newWorkRepo(t)

	// A write token pushing to a missing repo creates it (private) and the push lands.
	runGit(t, work, "push", at(f.write, "/zoe/new-thing.git"), "main")
	r, err := f.a.db.RepoByName(ctx, "zoe", "new-thing")
	if err != nil {
		t.Fatalf("repo not created: %v", err)
	}
	if !r.Private {
		t.Error("push-created repos must be private")
	}
	if g, err := f.a.git.Open("zoe", "new-thing"); err != nil || g.IsEmpty(ctx) {
		t.Fatalf("pushed commits missing (err %v)", err)
	}

	// Things that must never create a repository.
	for name, try := range map[string]func() (string, error){
		"read token push": func() (string, error) { return tryGit(work, "push", at(f.read, "/zoe/read-made.git"), "main") },
		"anonymous push": func() (string, error) {
			return tryGit(work, "-c", "credential.helper=", "push", "http://"+u.Host+"/zoe/anon-made.git", "main")
		},
		"clone of a missing repo": func() (string, error) {
			return tryGit(t.TempDir(), "clone", at(f.write, "/zoe/clone-made.git"), "x")
		},
		"reserved name":          func() (string, error) { return tryGit(work, "push", at(f.write, "/zoe/settings.git"), "main") },
		"someone else's account": func() (string, error) { return tryGit(work, "push", at(f.write, "/nobody/x.git"), "main") },
	} {
		if out, err := try(); err == nil {
			t.Errorf("%s succeeded:\n%s", name, out)
		}
	}
	for _, n := range []string{"read-made", "anon-made", "clone-made", "settings", "x"} {
		if _, err := f.a.db.RepoByName(ctx, "zoe", n); err == nil {
			t.Errorf("%s was created but shouldn't have been", n)
		}
	}

	// Switched off: a push to a missing repo is refused.
	if err := f.a.db.SetSetting(ctx, settingPushToCreate, "off"); err != nil {
		t.Fatal(err)
	}
	if out, err := tryGit(work, "push", at(f.write, "/zoe/off-made.git"), "main"); err == nil {
		t.Errorf("push succeeded with push-to-create off:\n%s", out)
	}
	if _, err := f.a.db.RepoByName(ctx, "zoe", "off-made"); err == nil {
		t.Error("repo created while push-to-create was off")
	}
}

func TestPushToCreateRules(t *testing.T) {
	f := newGitFixture(t)
	ctx := context.Background()
	me, _ := f.a.db.PersonalOwner(ctx)
	co, _ := f.a.db.CreateOwner(ctx, "studio", "Studio", "company")
	visitor, _ := f.a.db.CreateOwner(ctx, "visitor", "", "user")
	owner := Actor{Kind: ActorOwner, Name: me.Name, PersonID: me.ID}

	// The owner can create under their company account too.
	if r, err := f.a.pushCreate(ctx, owner, co.Name, "site"); err != nil || r.Owner != "studio" {
		t.Errorf("owner creating under company: %v %v", r, err)
	}
	// Another person can't create under the owner's accounts, nor under their own in core.
	other := Actor{Kind: ActorOwner, Name: "visitor", PersonID: visitor.ID}
	if _, err := f.a.pushCreate(ctx, other, "zoe", "sneaky"); err == nil {
		t.Error("another person created a repo in zoe's account")
	}
	// Invalid names are refused.
	if _, err := f.a.pushCreate(ctx, owner, "zoe", "../evil"); err == nil {
		t.Error("invalid name accepted")
	}
	// SSH path: a key acts as its person.
	key, err := f.a.db.AddSSHKey(ctx, "laptop", "ssh-ed25519", "SHA256:k", "ssh-ed25519 K", "zoe", ActorOwner)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := f.a.sshPushCreate(ctx, key.ID, "zoe", "via-ssh"); err != nil || !r.Private {
		t.Errorf("ssh push-create: %v %v", r, err)
	}
	vkey, _ := f.a.db.AddSSHKey(ctx, "v", "ssh-ed25519", "SHA256:v", "ssh-ed25519 V", "visitor", ActorOwner)
	if _, err := f.a.sshPushCreate(ctx, vkey.ID, "zoe", "via-visitor"); err == nil {
		t.Error("visitor's key created a repo in zoe's account")
	}
	_ = time.Second
}
