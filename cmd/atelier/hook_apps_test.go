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

func TestBotPushRules(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	o, _ := db.CreateOwner(ctx, "zoe", "", "user")
	repo, _ := db.CreateRepo(ctx, o.ID, "demo", "", true)
	other, _ := db.CreateRepo(ctx, o.ID, "other", "", true)
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
	if _, err := db.CreateBranchRule(ctx, store.BranchRule{RepoID: repo.ID, Pattern: "release/*", BlockForcePush: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveBot(ctx, store.Bot{PersonID: o.ID, Name: "fixer", CanPush: true, RepoIDs: []int64{repo.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveBot(ctx, store.Bot{PersonID: o.ID, Name: "talker", CanComment: true, RepoIDs: []int64{repo.ID, other.ID}}); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	zero := strings.Repeat("0", 40)
	check := func(user string, ups ...refUpdate) []string {
		msgs, err := botPushViolations(ctx, pushContext{DB: db, Repo: repo, Git: g, Updates: ups}, user)
		if err != nil {
			t.Fatal(err)
		}
		return msgs
	}
	if m := check("bot:fixer", refUpdate{zero, sha, "refs/heads/bot/fix-1"}); len(m) != 0 {
		t.Errorf("bot push to a new unprotected branch refused: %v", m)
	}
	for name, u := range map[string]refUpdate{
		"default":   {sha, sha, "refs/heads/main"},
		"protected": {sha, sha, "refs/heads/release/1.0"},
		"tag":       {zero, sha, "refs/tags/v1"},
		"delete":    {sha, zero, "refs/heads/bot/fix-1"},
	} {
		if m := check("bot:fixer", u); len(m) != 1 {
			t.Errorf("%s: bot push allowed (%v)", name, m)
		}
	}
	if m := check("bot:talker", refUpdate{zero, sha, "refs/heads/x"}); len(m) != 1 {
		t.Errorf("bot without push permission allowed: %v", m)
	}
	if m := check("bot:ghost", refUpdate{zero, sha, "refs/heads/x"}); len(m) != 1 {
		t.Errorf("unknown bot allowed: %v", m)
	}
	if m := check("owner:zoe", refUpdate{sha, sha, "refs/heads/main"}); len(m) != 0 {
		t.Errorf("owner push affected: %v", m)
	}
}
