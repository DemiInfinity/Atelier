package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prRepo makes a bare repo with main (README + app.txt) pushed, and a work
// clone to make branches in.
func prRepo(t *testing.T) (*Store, *Repo, string) {
	t.Helper()
	gitEnv(t)
	ctx := context.Background()
	s, err := New(filepath.Join(t.TempDir(), "repos"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx, "zoe", "demo"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Open("zoe", "demo")
	work := t.TempDir()
	git(t, work, "init", "--initial-branch=main", ".")
	write(t, filepath.Join(work, "README.md"), "# Demo\n")
	write(t, filepath.Join(work, "app.txt"), "one\ntwo\nthree\n")
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "Initial")
	git(t, work, "remote", "add", "origin", s.Path("zoe", "demo"))
	git(t, work, "push", "origin", "main")
	return s, r, work
}

func commitFile(t *testing.T, work, branch, file, content, msg string) string {
	t.Helper()
	git(t, work, "checkout", "-q", branch)
	write(t, filepath.Join(work, file), content)
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", msg)
	git(t, work, "push", "-q", "-f", "origin", branch)
	return git(t, work, "rev-parse", "HEAD")
}

var prOwnerSig = Signature{Name: "Zoë", Email: "zoe@atelier.local"}

func TestCompareAndCleanMerge(t *testing.T) {
	_, r, work := prRepo(t)
	ctx := context.Background()
	git(t, work, "checkout", "-q", "-b", "feature/greeting")
	commitFile(t, work, "feature/greeting", "hello.txt", "hi\n", "Add hello")
	head := commitFile(t, work, "feature/greeting", "app.txt", "one\nTWO\nthree\nfour\n", "Shout two")
	// main moves on independently (no conflict).
	baseMoved := commitFile(t, work, "main", "README.md", "# Demo\n\nMore.\n", "Docs")

	base, _ := r.BranchSHA(ctx, "main")
	if base != baseMoved {
		t.Fatalf("BranchSHA main = %s, want %s", base, baseMoved)
	}
	if _, err := r.BranchSHA(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("BranchSHA missing = %v", err)
	}
	c, err := r.Compare(ctx, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if c.Ahead != 2 || c.Behind != 1 || len(c.Commits) != 2 {
		t.Fatalf("ahead/behind/commits = %d/%d/%d", c.Ahead, c.Behind, len(c.Commits))
	}
	if c.Commits[0].Subject != "Add hello" || c.Commits[1].Subject != "Shout two" {
		t.Errorf("commits not oldest-first: %q, %q", c.Commits[0].Subject, c.Commits[1].Subject)
	}
	d := c.Diff
	if len(d.Files) != 2 || d.Additions != 3 || d.Deletions != 1 {
		t.Fatalf("diff files=%d +%d -%d: %+v", len(d.Files), d.Additions, d.Deletions, d.Files)
	}
	byPath := map[string]DiffFile{}
	for _, f := range d.Files {
		byPath[f.Path] = f
	}
	if f := byPath["hello.txt"]; f.Status != "added" || f.Additions != 1 {
		t.Errorf("hello.txt = %+v", f)
	}
	app := byPath["app.txt"]
	if app.Status != "modified" || app.Additions != 2 || app.Deletions != 1 {
		t.Errorf("app.txt = %+v", app)
	}
	// Line numbers: "two" deleted at old 2, "TWO" added at new 2, "four" at new 4.
	var sawDel, sawAdd, sawFour bool
	for _, l := range app.Lines {
		switch {
		case l.Kind == "del" && l.Text == "two" && l.Old == 2:
			sawDel = true
		case l.Kind == "add" && l.Text == "TWO" && l.New == 2:
			sawAdd = true
		case l.Kind == "add" && l.Text == "four" && l.New == 4:
			sawFour = true
		}
	}
	if !sawDel || !sawAdd || !sawFour {
		t.Errorf("line numbers wrong: %+v", app.Lines)
	}

	mc, err := r.CheckMerge(ctx, base, head)
	if err != nil || !mc.Clean || len(mc.Conflicts) != 0 || mc.Tree == "" {
		t.Fatalf("CheckMerge = %+v, %v", mc, err)
	}

	// Merge commit.
	res, err := r.MergeBranches(ctx, MergeRequest{Base: "main", Head: "feature/greeting", ExpectedHead: head,
		Method: "merge", Message: "Merge pull request #1 from feature/greeting\n\nGreeting", Committer: prOwnerSig})
	if err != nil {
		t.Fatal(err)
	}
	if res.BaseBefore != base || res.Head != head {
		t.Errorf("result = %+v", res)
	}
	got, _ := r.BranchSHA(ctx, "main")
	if got != res.SHA {
		t.Fatalf("main = %s, want merge %s", got, res.SHA)
	}
	bare := r.dir
	if parents := strings.Fields(git(t, bare, "log", "-1", "--format=%P", "main")); len(parents) != 2 || parents[0] != base || parents[1] != head {
		t.Errorf("merge parents = %v", parents)
	}
	if who := git(t, bare, "log", "-1", "--format=%cn <%ce>|%an", "main"); who != "Zoë <zoe@atelier.local>|Zoë" {
		t.Errorf("committer = %q", who)
	}
	if got := git(t, bare, "show", "main:app.txt"); got != "one\nTWO\nthree\nfour" {
		t.Errorf("merged app.txt = %q", got)
	}
	if got := git(t, bare, "show", "main:README.md"); !strings.Contains(got, "More.") {
		t.Errorf("merge lost base change: %q", got)
	}
	// Nothing left to merge.
	if _, err := r.MergeBranches(ctx, MergeRequest{Base: "main", Head: "feature/greeting", Method: "merge", Message: "x", Committer: prOwnerSig}); !errors.Is(err, ErrNothingToMerge) {
		t.Errorf("second merge = %v, want ErrNothingToMerge", err)
	}

	// Delete the head branch with CAS.
	if err := r.DeleteBranch(ctx, "feature/greeting", "0123456789012345678901234567890123456789", prOwnerSig); !errors.Is(err, ErrRefMoved) {
		t.Errorf("DeleteBranch wrong sha = %v", err)
	}
	if err := r.DeleteBranch(ctx, "feature/greeting", head, prOwnerSig); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BranchSHA(ctx, "feature/greeting"); !errors.Is(err, ErrNotFound) {
		t.Errorf("branch still there: %v", err)
	}
}

func TestSquashMerge(t *testing.T) {
	_, r, work := prRepo(t)
	ctx := context.Background()
	git(t, work, "checkout", "-q", "-b", "topic")
	commitFile(t, work, "topic", "a.txt", "a\n", "A")
	commitFile(t, work, "topic", "b.txt", "b\n", "B")
	base, _ := r.BranchSHA(ctx, "main")

	author := Signature{Name: "Agent", Email: "agent@atelier.local"}
	res, err := r.MergeBranches(ctx, MergeRequest{Base: "main", Head: "topic", Method: "squash",
		Message: "Topic (#2)\n\n* A\n* B", Author: author, Committer: prOwnerSig})
	if err != nil {
		t.Fatal(err)
	}
	bare := r.dir
	if parents := strings.Fields(git(t, bare, "log", "-1", "--format=%P", "main")); len(parents) != 1 || parents[0] != base {
		t.Errorf("squash parents = %v, want [%s]", parents, base)
	}
	if got := git(t, bare, "log", "-1", "--format=%s|%an|%cn", res.SHA); got != "Topic (#2)|Agent|Zoë" {
		t.Errorf("squash commit = %q", got)
	}
	if files := git(t, bare, "ls-tree", "--name-only", "main"); files != "README.md\na.txt\napp.txt\nb.txt" {
		t.Errorf("squash tree = %q", files)
	}
	if n := git(t, bare, "rev-list", "--count", "main"); n != "2" {
		t.Errorf("main has %s commits, want 2", n)
	}
}

func TestMergeConflicts(t *testing.T) {
	_, r, work := prRepo(t)
	ctx := context.Background()
	git(t, work, "checkout", "-q", "-b", "clash")
	commitFile(t, work, "clash", "app.txt", "one\nfeature\nthree\n", "Feature edit")
	commitFile(t, work, "clash", "README.md", "# Feature\n", "Feature readme")
	commitFile(t, work, "main", "app.txt", "one\nmain\nthree\n", "Main edit")
	commitFile(t, work, "main", "README.md", "# Main\n", "Main readme")

	base, _ := r.BranchSHA(ctx, "main")
	head, _ := r.BranchSHA(ctx, "clash")
	mc, err := r.CheckMerge(ctx, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if mc.Clean || len(mc.Conflicts) != 2 || mc.Conflicts[0] != "README.md" || mc.Conflicts[1] != "app.txt" {
		t.Fatalf("CheckMerge = %+v", mc)
	}
	_, err = r.MergeBranches(ctx, MergeRequest{Base: "main", Head: "clash", Method: "squash", Message: "x", Committer: prOwnerSig})
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("merge with conflicts = %v", err)
	}
	if got, _ := r.BranchSHA(ctx, "main"); got != base {
		t.Error("a failed merge moved main")
	}
}

func TestMergeFastForwardAndConflictFiles(t *testing.T) {
	_, r, work := prRepo(t)
	ctx := context.Background()
	git(t, work, "checkout", "-q", "-b", "stale")
	git(t, work, "push", "-q", "origin", "stale")
	old, _ := r.BranchSHA(ctx, "stale")
	tip := commitFile(t, work, "main", "app.txt", "one\nmain\nthree\n", "Main edit")
	// stale has nothing of its own: it moves straight to main, no new commit.
	res, err := r.MergeBranches(ctx, MergeRequest{Base: "stale", Head: "main", Method: "merge", Message: "x", Committer: prOwnerSig, FastForward: true})
	if err != nil || res.SHA != tip || res.BaseBefore != old {
		t.Fatalf("fast-forward = %+v, %v", res, err)
	}
	if got, _ := r.BranchSHA(ctx, "stale"); got != tip {
		t.Error("stale didn't move to main")
	}
	commitFile(t, work, "stale", "app.txt", "one\nstale\nthree\n", "Stale edit")
	commitFile(t, work, "main", "app.txt", "one\nmain again\nthree\n", "Main again")
	_, err = r.MergeBranches(ctx, MergeRequest{Base: "stale", Head: "main", Method: "merge", Message: "x", Committer: prOwnerSig, FastForward: true})
	var ce *MergeConflictError
	if !errors.As(err, &ce) || !errors.Is(err, ErrMergeConflict) || len(ce.Files) != 1 || ce.Files[0] != "app.txt" {
		t.Fatalf("conflict = %v", err)
	}
}

func TestMergeCASFailsWhenBranchMoved(t *testing.T) {
	_, r, work := prRepo(t)
	ctx := context.Background()
	git(t, work, "checkout", "-q", "-b", "topic")
	head := commitFile(t, work, "topic", "a.txt", "a\n", "A")

	// The reviewed head is stale: a newer push landed on topic.
	newer := commitFile(t, work, "topic", "a.txt", "aa\n", "A again")
	_, err := r.MergeBranches(ctx, MergeRequest{Base: "main", Head: "topic", ExpectedHead: head, Method: "merge", Message: "x", Committer: prOwnerSig})
	if !errors.Is(err, ErrRefMoved) {
		t.Fatalf("stale head merge = %v, want ErrRefMoved", err)
	}

	// Base moves between computing the merge and update-ref: casRef refuses.
	base, _ := r.BranchSHA(ctx, "main")
	pushed := commitFile(t, work, "main", "c.txt", "c\n", "Concurrent push")
	err = r.casRef(ctx, "refs/heads/main", newer, base, "test", prOwnerSig)
	if !errors.Is(err, ErrRefMoved) {
		t.Fatalf("casRef with stale old = %v, want ErrRefMoved", err)
	}
	if got, _ := r.BranchSHA(ctx, "main"); got != pushed {
		t.Errorf("main = %s, want the concurrent push %s kept", got, pushed)
	}
}

func TestUnrelatedAndHelpers(t *testing.T) {
	_, r, work := prRepo(t)
	ctx := context.Background()
	git(t, work, "checkout", "-q", "--orphan", "other")
	git(t, work, "rm", "-rqf", ".")
	write(t, filepath.Join(work, "x.txt"), "x\n")
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "Orphan")
	git(t, work, "push", "-q", "origin", "other")
	base, _ := r.BranchSHA(ctx, "main")
	head, _ := r.BranchSHA(ctx, "other")
	c, err := r.Compare(ctx, base, head)
	if err != nil || !c.Unrelated {
		t.Errorf("Compare unrelated = %+v, %v", c, err)
	}
	if _, err := r.CheckMerge(ctx, base, head); !errors.Is(err, ErrUnrelated) {
		t.Errorf("CheckMerge unrelated = %v", err)
	}

	for name, ok := range map[string]bool{
		"main": true, "feature/x-1": true, "v1.2": true, "-x": false, "a..b": false, "a b": false,
		"a:b": false, "x.lock": false, "": false, "a//b": false, "/a": false, "a/": false, "a@{1}": false,
	} {
		if ValidBranchName(name) != ok {
			t.Errorf("ValidBranchName(%q) = %v", name, !ok)
		}
	}

	// Merge commits detection for linear history.
	git(t, work, "checkout", "-q", "main")
	git(t, work, "checkout", "-q", "-b", "m1")
	commitFile(t, work, "m1", "m1.txt", "1\n", "m1")
	git(t, work, "checkout", "-q", "main")
	git(t, work, "merge", "-q", "--no-ff", "-m", "Merge m1", "m1")
	merged := git(t, work, "rev-parse", "HEAD")
	git(t, work, "push", "-q", "origin", "main")
	if has, err := r.HasMergeCommits(ctx, base, merged); err != nil || !has {
		t.Errorf("HasMergeCommits = %v, %v", has, err)
	}
	m1, _ := r.BranchSHA(ctx, "m1")
	if has, _ := r.HasMergeCommits(ctx, base, m1); has {
		t.Error("linear range reported merge commits")
	}
}

func TestPreReceiveHookInstall(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	s, err := New(filepath.Join(t.TempDir(), "repos"))
	if err != nil {
		t.Fatal(err)
	}
	// In test binaries hooks are skipped unless HookExecutable is set.
	if err := s.Init(ctx, "zoe", "nohook"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Path("zoe", "nohook"), "hooks", "pre-receive")); err == nil {
		t.Error("hook installed from a test binary without HookExecutable")
	}

	HookExecutable = "/opt/atelier/bin/atelier"
	t.Cleanup(func() { HookExecutable = "" })
	if err := s.Init(ctx, "zoe", "demo"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.Path("zoe", "demo"), "hooks", "pre-receive")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := "ATELIER_DATA='" + filepath.Dir(s.Root) + "'"
	if !strings.Contains(string(b), want) || !strings.Contains(string(b), "exec '/opt/atelier/bin/atelier' hook pre-receive 'zoe' 'demo'") {
		t.Errorf("hook script:\n%s", b)
	}
	if fi, _ := os.Stat(p); fi.Mode()&0o111 == 0 {
		t.Errorf("hook not executable: %v", fi.Mode())
	}
	// A foreign hook is left alone; ours is refreshed.
	os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	HookExecutable = "/new/atelier"
	if err := s.InstallPreReceiveHook("zoe", "demo"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "#!/bin/sh\nexit 0\n" {
		t.Errorf("foreign hook overwritten:\n%s", b)
	}
	os.Remove(p)
	if err := s.InstallPreReceiveHook("zoe", "demo"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); !strings.Contains(string(b), "'/new/atelier'") {
		t.Errorf("hook not refreshed:\n%s", b)
	}
}
