package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// pullsWork clones nothing: it makes a work tree whose origin is the bare repo
// on disk (hooks aren't installed in tests unless HookExecutable is set).
func pullsWork(t *testing.T, a *App) string {
	t.Helper()
	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", ".")
	runGit(t, work, "remote", "add", "origin", a.git.Path("zoe", "demo"))
	return work
}

func pullsCommit(t *testing.T, work, file, content, msg string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(work, file)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-q", "-m", msg)
	return runGit(t, work, "rev-parse", "HEAD")
}

func pullsCookie(t *testing.T, a *App) map[string]string {
	t.Helper()
	sess, err := a.db.CreateSession(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Cookie": sessionCookie + "=" + sess}
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, code int, what string) string {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("%s = %d, want %d\n%.2000s", what, rec.Code, code, rec.Body)
	}
	return rec.Body.String()
}

func mustContain(t *testing.T, body, what string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("%s: missing %q", what, s)
		}
	}
}

func TestPullRequestFlow(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	ck := pullsCookie(t, a)
	work := pullsWork(t, a)
	pullsCommit(t, work, "app/main.go", "package app\n\nfunc A() {}\n", "Initial")
	runGit(t, work, "push", "-q", "origin", "main")
	runGit(t, work, "checkout", "-q", "-b", "feature/add-greeting")
	head := pullsCommit(t, work, "app/main.go", "package app\n\nfunc A() {}\n\nfunc Hello() string { return \"hi\" }\n", "Add Hello")
	runGit(t, work, "push", "-q", "origin", "feature/add-greeting")

	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls", nil, ck), 200, "GET pulls")
	mustContain(t, body, "pulls list", "No open pull requests", "/static/pulls.css")

	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/compare", nil, ck), 200, "GET compare (no head)")
	mustContain(t, body, "compare picker", "Recently pushed branches", "feature/add-greeting")

	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/new?base=main&head=feature/add-greeting", nil, ck), 200, "GET compare")
	mustContain(t, body, "compare", "Open a pull request", `value="Add Hello"`, "Able to merge", "Hello()", "1 commit")

	// Create it.
	rec := do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"feature/add-greeting"},
		"title": {"Add a greeting"}, "body": {"Says **hi**."}}, ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/zoe/demo/pulls/1" {
		t.Fatalf("create = %d %q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	p, err := a.db.PullByNumber(ctx, f.repoID(t), 1)
	if err != nil || p.HeadSHA != head || p.AuthorName != "zoe" || p.AuthorKind != "owner" {
		t.Fatalf("stored pull = %+v, %v", p, err)
	}
	// A second create for the same branches goes to the existing one.
	rec = do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"feature/add-greeting"}, "title": {"dup"}}, ck)
	if rec.Header().Get("Location") != "/zoe/demo/pulls/1" {
		t.Errorf("duplicate create = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Validation.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"main"}, "title": {"x"}}, ck), 422, "same branches")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"nope"}, "title": {"x"}}, ck), 422, "missing head")

	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/1", nil, ck), 200, "GET pull")
	mustContain(t, body, "overview", "Add a greeting", "<strong>hi</strong>", "Ready to merge", "No conflicts",
		"No checks", "Squash and merge", "Create a merge commit", "Next steps", "Add Hello", `name="expected_head" value="`+head+`"`)

	// Files changed, with the no-JS note form on a line.
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/1/files?path=app/main.go&side=new&line=5", nil, ck), 200, "GET files")
	mustContain(t, body, "files", "app/main.go", "func Hello()", `id="f0-n5"`, `id="note-form"`, `name="line" value="5"`, "pr-note-template", "pr-note-add")

	rec = do(t, a, "POST", "/zoe/demo/pulls/1/notes", url.Values{"commit": {head}, "path": {"app/main.go"}, "side": {"new"},
		"line": {"5"}, "body": {"Name this better"}}, ck)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "/zoe/demo/pulls/1/files#note-") {
		t.Fatalf("add note = %d %q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/notes", url.Values{"commit": {"x"}, "path": {"a"}, "side": {"new"}, "line": {"1"}, "body": {"b"}}, ck), 422, "bad note")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/1/files", nil, ck), 200, "GET files with note")
	mustContain(t, body, "files with note", "Name this better", "pr-dot", "Resolve")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/1", nil, ck), 200, "GET pull with note")
	mustContain(t, body, "notes pill", "1 note to self unresolved")

	// A branch rule requiring resolved notes blocks the merge.
	expectStatus(t, do(t, a, "GET", "/zoe/demo/settings/branches", nil, ck), 200, "GET branch rules")
	rec = do(t, a, "POST", "/zoe/demo/settings/branches", url.Values{"pattern": {"main"}, "require_pr": {"1"}, "require_resolved_notes": {"1"}}, ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create rule = %d\n%s", rec.Code, rec.Body)
	}
	body = expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/merge", url.Values{"method": {"squash"}, "expected_head": {head}}, ck), 409, "blocked merge")
	mustContain(t, body, "blocked merge", "Resolve every note to self first")

	notes, _ := a.db.PullNotes(ctx, p.ID)
	rec = do(t, a, "POST", "/zoe/demo/pulls/1/notes/"+itoa(notes[0].ID)+"/resolve", url.Values{"resolved": {"1"}, "back": {"files"}}, ck)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/zoe/demo/pulls/1/files#note-") {
		t.Fatalf("resolve = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Comment and rename.
	rec = do(t, a, "POST", "/zoe/demo/pulls/1/comments", url.Values{"body": {"Ship it"}}, ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("comment = %d", rec.Code)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/edit", url.Values{"title": {"Add a friendly greeting"}, "body": {"Says hi."}}, ck), 303, "edit")

	// A stale expected head (someone pushed since the page loaded) is refused.
	body = expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/merge", url.Values{"method": {"squash"}, "expected_head": {strings.Repeat("a", 40)}, "delete_branch": {"1"}}, ck), 409, "stale merge")
	mustContain(t, body, "stale merge", "changed while merging")

	// Squash and merge, deleting the branch.
	baseBefore, _ := gitBranch(t, a, "main")
	rec = do(t, a, "POST", "/zoe/demo/pulls/1/merge", url.Values{"method": {"squash"}, "expected_head": {head}, "delete_branch": {"1"}}, ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("merge = %d\n%.3000s", rec.Code, rec.Body)
	}
	p, _ = a.db.PullByNumber(ctx, f.repoID(t), 1)
	mainSHA, _ := gitBranch(t, a, "main")
	if !p.IsMerged() || p.MergeMethod != "squash" || p.MergedSHA != mainSHA || p.BaseSHA != baseBefore || p.HeadSHA != head {
		t.Errorf("after merge = %+v (main %s)", p, mainSHA)
	}
	bare := a.git.Path("zoe", "demo")
	if got := runGit(t, bare, "log", "-1", "--format=%P|%s|%cn <%ce>", "main"); got != baseBefore+"|Add a friendly greeting (#1)|zoe <zoe@atelier.local>" {
		t.Errorf("squash commit = %q", got)
	}
	if _, err := gitBranch(t, a, "feature/add-greeting"); err == nil {
		t.Error("head branch not deleted after merge")
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/1", nil, ck), 200, "GET merged pull")
	mustContain(t, body, "merged pull", "Merged", "as one squashed commit", "Ship it", "changed the title", "deleted the", "Add Hello")
	expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/1/files", nil, ck), 200, "GET merged files")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls?state=merged", nil, ck), 200, "merged list")
	mustContain(t, body, "merged list", "Add a friendly greeting", "1 Merged")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/merge", url.Values{"method": {"squash"}}, ck), 409, "merge twice")

	// Second PR: linear history allows squash only; close and reopen.
	runGit(t, work, "checkout", "-q", "main")
	runGit(t, work, "pull", "-q", "origin", "main")
	runGit(t, work, "checkout", "-q", "-b", "topic")
	pullsCommit(t, work, "b.txt", "b\n", "B one")
	pullsCommit(t, work, "c.txt", "c\n", "B two")
	runGit(t, work, "push", "-q", "origin", "topic")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/compare?base=main&head=topic", nil, ck), 200, "compare topic")
	mustContain(t, body, "default title from branch", `value="Topic"`, "2 commits")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"topic"}, "title": {"Topic"}}, ck), 303, "create #2")
	rules, _ := a.db.BranchRules(ctx, f.repoID(t))
	rl := rules[0]
	rl.LinearHistory = true
	if err := a.db.UpdateBranchRule(ctx, rl); err != nil {
		t.Fatal(err)
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/2", nil, ck), 200, "GET #2")
	mustContain(t, body, "#2", "Linear history: squash only", "main is protected")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/2/merge", url.Values{"method": {"merge"}}, ck), 422, "merge commit under linear history")

	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/2/state", url.Values{"action": {"close"}}, ck), 303, "close")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/2", nil, ck), 200, "closed #2")
	mustContain(t, body, "closed", "closed without merging", "Reopen", "Delete branch topic")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/2/merge", url.Values{"method": {"squash"}}, ck), 409, "merge closed")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/2/state", url.Values{"action": {"reopen"}}, ck), 303, "reopen")

	// Merge commit once linear history is off again.
	rl.LinearHistory = false
	_ = a.db.UpdateBranchRule(ctx, rl)
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/2/merge", url.Values{"method": {"merge"}}, ck), 303, "merge commit")
	if parents := strings.Fields(runGit(t, bare, "log", "-1", "--format=%P", "main")); len(parents) != 2 {
		t.Errorf("merge commit parents = %v", parents)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/2/delete-branch", nil, ck), 303, "delete branch after merge")
	if _, err := gitBranch(t, a, "topic"); err == nil {
		t.Error("topic not deleted")
	}

	// Conflicts are reported and block merging.
	runGit(t, work, "checkout", "-q", "main")
	runGit(t, work, "pull", "-q", "origin", "main")
	runGit(t, work, "checkout", "-q", "-b", "clash")
	pullsCommit(t, work, "b.txt", "clash\n", "Clash")
	runGit(t, work, "push", "-q", "origin", "clash")
	runGit(t, work, "checkout", "-q", "main")
	pullsCommit(t, work, "b.txt", "main\n", "Main moves")
	runGit(t, work, "push", "-q", "origin", "main")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"clash"}, "title": {"Clash"}}, ck), 303, "create #3")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/3", nil, ck), 200, "GET #3")
	mustContain(t, body, "#3", "Conflicts in 1 file", "Conflicting files", "b.txt", "Not ready to merge")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/3/merge", url.Values{"method": {"squash"}}, ck), 409, "merge with conflicts")

	// Rules page: edit and delete.
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/settings/branches?edit="+itoa(rl.ID), nil, ck), 200, "edit rule page")
	mustContain(t, body, "edit rule", "Edit rule", "/settings/branches/"+itoa(rl.ID))
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/branches", url.Values{"pattern": {"bad pattern"}}, ck), 422, "bad pattern")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/branches", url.Values{"pattern": {"main"}}, ck), 409, "duplicate pattern")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/branches/"+itoa(rl.ID), url.Values{"pattern": {"release/*"}, "block_force_push": {"1"}}, ck), 303, "update rule")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/branches/identity", url.Values{"email": {"me@example.com"}}, ck), 303, "identity")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/branches/identity", url.Values{"email": {"not an email"}}, ck), 422, "bad identity")
	if sig := a.pullIdentity(ctx); sig.Email != "me@example.com" {
		t.Errorf("identity = %+v", sig)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/branches/"+itoa(rl.ID)+"/delete", nil, ck), 303, "delete rule")
	if rules, _ := a.db.BranchRules(ctx, f.repoID(t)); len(rules) != 0 {
		t.Errorf("rules left: %+v", rules)
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/settings", nil, ck), 200, "repo settings")
	mustContain(t, body, "settings link", "/zoe/demo/settings/branches")
	expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/99", nil, ck), 404, "missing pull")
	expectStatus(t, do(t, a, "GET", "/static/pulls.js", nil, nil), 200, "pulls.js")
}

func TestDefaultPullText(t *testing.T) {
	one := []gitrepo.Commit{{Subject: "Fix the thing", Body: "Because."}}
	if ti, b := defaultPullText(one, "x"); ti != "Fix the thing" || b != "Because." {
		t.Errorf("single commit = %q %q", ti, b)
	}
	two := append(one, gitrepo.Commit{Subject: "More"})
	for head, want := range map[string]string{"feature/add-login_page": "Add login page", "wip": "Wip", "a/": "A/"} {
		if ti, _ := defaultPullText(two, head); ti != want {
			t.Errorf("defaultPullText(%q) = %q, want %q", head, ti, want)
		}
	}
}

func TestPullTree(t *testing.T) {
	files := []pullFileView{
		{DiffFile: &gitrepo.DiffFile{Path: "z.txt"}},
		{DiffFile: &gitrepo.DiffFile{Path: "a/b/c.go"}},
		{DiffFile: &gitrepo.DiffFile{Path: "a/d.go"}},
		{DiffFile: &gitrepo.DiffFile{Path: "a/b/e.go"}},
	}
	var got []string
	for _, r := range pullTree(files) {
		s := strings.Repeat(" ", r.Depth) + r.Name
		if r.Dir {
			s += "/"
		}
		got = append(got, s)
	}
	want := "a/| b/|  c.go|  e.go| d.go|z.txt"
	if strings.Join(got, "|") != want {
		t.Errorf("tree = %q, want %q", strings.Join(got, "|"), want)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (f gitFixture) repoID(t *testing.T) int64 {
	t.Helper()
	rp, err := f.a.db.RepoByName(context.Background(), "zoe", "demo")
	if err != nil {
		t.Fatal(err)
	}
	return rp.ID
}

func gitBranch(t *testing.T, a *App, branch string) (string, error) {
	t.Helper()
	g, err := a.git.Open("zoe", "demo")
	if err != nil {
		t.Fatal(err)
	}
	return g.BranchSHA(context.Background(), branch)
}

// buildAtelier compiles the real binary so the pre-receive hook can run it.
// Call it before isolateGit: it needs the user's Go caches.
func buildAtelier(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "atelier")
	cmd := exec.Command(goBin, "build", "-o", bin, "atelier/cmd/atelier")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build atelier: %v\n%s", err, out)
	}
	return bin
}

func gitTry(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestPreReceiveHookOverHTTP(t *testing.T) {
	bin := buildAtelier(t)
	gitrepo.HookExecutable = bin
	t.Cleanup(func() { gitrepo.HookExecutable = "" })

	f := newGitFixture(t) // creates zoe/demo, which installs the hook
	a := f.a
	ctx := context.Background()
	hook := filepath.Join(a.git.Path("zoe", "demo"), "hooks", "pre-receive")
	if b, err := os.ReadFile(hook); err != nil || !strings.Contains(string(b), bin) {
		t.Fatalf("hook not installed: %v\n%s", err, b)
	}
	srv := httptest.NewServer(a)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	remote := "http://zoe:" + f.write + "@" + u.Host + "/zoe/demo.git"

	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", ".")
	runGit(t, work, "remote", "add", "origin", remote)
	pullsCommit(t, work, "README.md", "# Demo\n", "First")
	runGit(t, work, "push", "-q", "origin", "main") // no rules yet

	rid := f.repoID(t)
	if _, err := a.db.CreateBranchRule(ctx, store.BranchRule{RepoID: rid, Pattern: "main", RequirePR: true, BlockForcePush: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.CreateBranchRule(ctx, store.BranchRule{RepoID: rid, Pattern: "release/*", BlockForcePush: true}); err != nil {
		t.Fatal(err)
	}
	mainBefore, _ := gitBranch(t, a, "main")

	// Direct push to main is rejected.
	pullsCommit(t, work, "README.md", "# Demo\n\nDirect.\n", "Direct")
	out, err := gitTry(work, "push", "origin", "main")
	if err == nil || !strings.Contains(out, "changes must go through a pull request") || !strings.Contains(out, "pre-receive hook declined") {
		t.Fatalf("direct push to main: err=%v\n%s", err, out)
	}
	if got, _ := gitBranch(t, a, "main"); got != mainBefore {
		t.Fatal("rejected push moved main")
	}
	// Force push to main is rejected (with the force-push message).
	runGit(t, work, "reset", "-q", "--hard", "HEAD~1")
	runGit(t, work, "commit", "-q", "--amend", "-m", "Rewritten")
	out, err = gitTry(work, "push", "-f", "origin", "main")
	if err == nil || !strings.Contains(out, "force pushes aren't allowed") {
		t.Fatalf("force push to main: err=%v\n%s", err, out)
	}
	// Deleting main is rejected.
	out, err = gitTry(work, "push", "origin", ":main")
	if err == nil || !strings.Contains(out, "can't be deleted") {
		t.Fatalf("delete main: err=%v\n%s", err, out)
	}

	// release/* blocks force pushes but allows fast-forwards.
	runGit(t, work, "checkout", "-q", "-b", "release/1", mainBefore)
	pullsCommit(t, work, "VERSION", "1\n", "v1")
	runGit(t, work, "push", "-q", "origin", "release/1")
	pullsCommit(t, work, "VERSION", "1.1\n", "v1.1")
	if out, err := gitTry(work, "push", "origin", "release/1"); err != nil {
		t.Fatalf("fast-forward push to release/1 rejected: %v\n%s", err, out)
	}
	runGit(t, work, "commit", "-q", "--amend", "-m", "v1.1 rewritten")
	out, err = gitTry(work, "push", "-f", "origin", "release/1")
	if err == nil || !strings.Contains(out, "release/1 is protected") || !strings.Contains(out, "force pushes aren't allowed") {
		t.Fatalf("force push to release/1: err=%v\n%s", err, out)
	}
	// Unprotected branches take anything.
	runGit(t, work, "checkout", "-q", "-b", "feature", mainBefore)
	pullsCommit(t, work, "f.txt", "f\n", "Feature")
	runGit(t, work, "push", "-q", "origin", "feature")
	runGit(t, work, "commit", "-q", "--amend", "-m", "Feature again")
	runGit(t, work, "push", "-q", "-f", "origin", "feature")

	// Atelier's own merge into the protected branch is not blocked by the hook.
	ck := pullsCookie(t, a)
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"feature"}, "title": {"Feature"}}, ck), 303, "create")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/merge", url.Values{"method": {"squash"}, "delete_branch": {"1"}}, ck), 303, "merge into protected main")
	if got, _ := gitBranch(t, a, "main"); got == mainBefore {
		t.Error("merge didn't move main")
	}

	// The startup hook refreshes hooks on existing repos.
	os.Remove(hook)
	a.ensurePullHooks(ctx)
	if _, err := os.Stat(hook); err != nil {
		t.Errorf("ensurePullHooks didn't reinstall the hook: %v", err)
	}
}
