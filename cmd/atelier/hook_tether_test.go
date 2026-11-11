package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

func TestTetherHomeGitHubRefusesDefaultBranchPush(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	o, _ := db.CreateOwner(ctx, "zoe", "", "user")
	repo, _ := db.CreateRepo(ctx, o.ID, "demo", "", true)
	gs, err := gitrepo.New(filepath.Join(dir, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.Init(ctx, "zoe", "demo"); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "--git-dir="+gs.Path("zoe", "demo"), "symbolic-ref", "HEAD", "refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	g, _ := gs.Open("zoe", "demo")
	sha := strings.Repeat("a", 40)
	check := func() []string {
		msgs, err := checkTetherHome(ctx, pushContext{DB: db, Repo: repo, Git: g, Updates: []refUpdate{
			{sha, sha, "refs/heads/main"}, {sha, sha, "refs/heads/feature"}}})
		if err != nil {
			t.Fatal(err)
		}
		return msgs
	}
	if m := check(); len(m) != 0 {
		t.Fatalf("no Tether link, push refused: %v", m)
	}
	if err := db.SaveTetherLink(ctx, store.TetherLink{RepoID: repo.ID, PersonID: o.ID, GHRepo: "zoe/demo", Home: "atelier"}); err != nil {
		t.Fatal(err)
	}
	if m := check(); len(m) != 0 {
		t.Fatalf("home Atelier, push refused: %v", m)
	}
	if err := db.SaveTetherLink(ctx, store.TetherLink{RepoID: repo.ID, PersonID: o.ID, GHRepo: "zoe/demo", Home: "github"}); err != nil {
		t.Fatal(err)
	}
	m := check()
	if len(m) != 1 || !strings.Contains(m[0], "refs/heads/main") || !strings.Contains(m[0], "home is GitHub") {
		t.Fatalf("home GitHub: want only main refused, got %v", m)
	}
}
