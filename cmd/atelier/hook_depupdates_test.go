package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

func TestLockfileJobPushRules(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	o, _ := db.CreateOwner(ctx, "zoe", "", "user")
	repo, _ := db.CreateRepo(ctx, o.ID, "marbl", "", true)
	other, _ := db.CreateRepo(ctx, o.ID, "tray", "", true)
	gs, err := gitrepo.New(filepath.Join(dir, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	if err := gs.Init(ctx, "zoe", "marbl"); err != nil {
		t.Fatal(err)
	}
	g, _ := gs.Open("zoe", "marbl")

	// A work tree with the bot's update commit, a lockfile commit and a
	// commit touching source, fetched into the bare repository (fetching
	// runs no hooks).
	work := filepath.Join(dir, "work")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(file, content, msg string) string {
		t.Helper()
		p := filepath.Join(work, filepath.FromSlash(file))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", file)
		git("commit", "-q", "-m", msg)
		return git("rev-parse", "HEAD")
	}
	os.MkdirAll(work, 0o755)
	git("init", "-q", "--initial-branch=main", ".")
	commit("web/package.json", `{"dependencies":{"svelte":"^5.57.0"}}`, "init")
	commit("web/package-lock.json", `{"lockfileVersion":3}`, "lock")
	const branch = "atelier-updates/npm@web/svelte/5.57.3"
	git("checkout", "-q", "-b", branch)
	bump := commit("web/package.json", `{"dependencies":{"svelte":"^5.57.3"}}`, "Bump svelte")
	lock := commit("web/package-lock.json", `{"lockfileVersion":3,"x":1}`, "Update lockfiles")
	src := commit("web/src/app.js", "evil()", "sneaky")
	git("checkout", "-q", "main")
	commit("web/package-lock.json", `{"lockfileVersion":3,"main":1}`, "main moves on")
	git("checkout", "-q", "-b", "atelier-updates/merge", lock)
	git("merge", "-q", "--no-ff", "-X", "ours", "-m", "merge", "main")
	merged := git("rev-parse", "HEAD")
	if out, err := exec.Command("git", "--git-dir="+gs.Path("zoe", "marbl"), "fetch", "-q", work, "+refs/heads/*:refs/heads/*").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}

	pr, err := db.CreatePull(ctx, store.NewPull{RepoID: repo.ID, Title: "Bump svelte", Head: branch, Base: "main", HeadSHA: bump,
		AuthorKind: "bot", AuthorName: "atelier-updates"})
	if err != nil {
		t.Fatal(err)
	}
	newJob := func(rp store.Repo, ref, status string) int64 {
		t.Helper()
		_, jobs, err := db.CreateActionsRun(ctx, store.ActionsRun{RepoID: rp.ID, WorkflowPath: ".atelier/workflows/lockfiles.yml", WorkflowName: "lockfiles",
			Event: "push", Ref: ref, SHA: bump, Status: store.RunQueued, ActorKind: "owner", ActorName: "zoe"},
			[]store.ActionsJob{{JobKey: "lockfiles", Name: "lockfiles", Status: status}})
		if err != nil {
			t.Fatal(err)
		}
		return jobs[0].ID
	}
	job := newJob(repo, "refs/heads/"+branch, "running")
	mainJob := newJob(repo, "refs/heads/main", "running")
	doneJob := newJob(repo, "refs/heads/"+branch, "success")
	otherJob := newJob(other, "refs/heads/"+branch, "running")
	ref := "refs/heads/" + branch
	check := func(user string, ups ...refUpdate) []string {
		t.Helper()
		msgs, err := lockfileJobViolations(ctx, pushContext{DB: db, Repo: repo, Git: g, Updates: ups}, user)
		if err != nil {
			t.Fatal(err)
		}
		return msgs
	}
	user := fmt.Sprintf("job:actions-job-%d", job)

	// Off by default.
	if m := check(user, refUpdate{bump, lock, ref}); len(m) != 1 || !strings.Contains(m[0], "off") {
		t.Errorf("push with lockfiles off: %v", m)
	}
	if err := db.SetDepUpdateSettings(ctx, repo.ID, store.DepUpdateSettings{Lockfiles: true, ScheduleDay: -1}); err != nil {
		t.Fatal(err)
	}
	if m := check(user, refUpdate{bump, lock, ref}); len(m) != 0 {
		t.Errorf("lockfile push refused: %v", m)
	}
	zero := strings.Repeat("0", 40)
	for name, c := range map[string]struct {
		user string
		ups  []refUpdate
	}{
		"source file":    {user, []refUpdate{{bump, src, ref}}},
		"force push":     {user, []refUpdate{{lock, bump, ref}}},
		"merge commit":   {user, []refUpdate{{bump, merged, ref}}},
		"other branch":   {user, []refUpdate{{bump, lock, "refs/heads/main"}}},
		"second ref":     {user, []refUpdate{{bump, lock, ref}, {zero, lock, "refs/heads/x"}}},
		"delete":         {user, []refUpdate{{bump, zero, ref}}},
		"tag":            {user, []refUpdate{{zero, lock, "refs/tags/v1"}}},
		"run on main":    {fmt.Sprintf("job:actions-job-%d", mainJob), []refUpdate{{bump, lock, ref}}},
		"finished job":   {fmt.Sprintf("job:actions-job-%d", doneJob), []refUpdate{{bump, lock, ref}}},
		"other repo job": {fmt.Sprintf("job:actions-job-%d", otherJob), []refUpdate{{bump, lock, ref}}},
		"garbage job id": {"job:actions-job-x", []refUpdate{{bump, lock, ref}}},
		"unknown job id": {"job:actions-job-99999", nil},
	} {
		msgs, err := lockfileJobViolations(ctx, pushContext{DB: db, Repo: repo, Git: g, Updates: c.ups}, c.user)
		if err == nil && len(msgs) == 0 {
			t.Errorf("%s: push allowed", name)
		}
	}
	// A branch whose pull request was closed is off limits.
	if err := db.ClosePull(ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	if m := check(user, refUpdate{bump, lock, ref}); len(m) != 1 {
		t.Errorf("push after the PR closed: %v", m)
	}
	// Other pushers aren't this check's business.
	if m := check("owner:zoe", refUpdate{bump, src, ref}); len(m) != 0 {
		t.Errorf("owner push affected: %v", m)
	}
}
