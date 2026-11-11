package web

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
	"atelier/internal/tether"
)

const tetherPRWorkflow = `name: CI
on: pull_request
jobs:
  build:
    runs-on: linux
    steps:
      - run: make
`

// newTetherActFixture is a Tether fixture with the Actions runner API up.
func newTetherActFixture(t *testing.T) (*tetherFixture, *actFixture) {
	t.Helper()
	af := newActFixture(t)
	f := &tetherFixture{gitFixture: af.gitFixture, gh: newFakeGH(t), ck: af.ck, work: af.work}
	f.gh.use(f.a)
	t.Cleanup(func() { f.wait() })
	return f, af
}

// twinCommit makes a commit on top of the twin's main (in a clone) and
// returns its sha; push is the refspec to push it to ("" = don't push).
func twinCommit(t *testing.T, f *tetherFixture, full, file, content, push string) string {
	t.Helper()
	w := t.TempDir()
	runGit(t, w, "clone", "-q", f.gh.bare(full), ".")
	sha := pullsCommit(t, w, file, content, "Change "+file)
	if push != "" {
		runGit(t, w, "push", "-q", "-f", "origin", "HEAD:"+push)
	}
	return sha
}

func (f *fakeGH) setPull(full string, pr tether.Pull) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pulls[full] == nil {
		f.pulls[full] = map[int64]*tether.Pull{}
	}
	cp := pr
	f.pulls[full][pr.Number] = &cp
}

func (f *fakeGH) statusFor(sha, ctxName string) (tether.Status, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out tether.Status
	ok := false
	for _, s := range f.statuses {
		if s.SHA == sha && s.Context == ctxName {
			out, ok = s.Status, true
		}
	}
	return out, ok
}

func TestTetherGitHubPullChecksAndStatuses(t *testing.T) {
	f, af := newTetherActFixture(t)
	a, ctx := f.a, context.Background()
	a.cfg.PublicURL = "https://atelier.example"
	f.connect(t)
	pullsCommit(t, f.work, "README.md", "# demo\n", "Initial")
	af.push(t, "main")
	twin := f.gh.addRepo("zoe-gh/demo", true)
	runGit(t, f.work, "push", "-q", f.gh.bare("zoe-gh/demo"), "main")
	f.link(t, "zoe-gh/demo", "github", false)

	// A fork's pull request (#5): only refs/pull/5/head exists on the twin.
	forkSHA := twinCommit(t, f, "zoe-gh/demo", ".atelier/workflows/pr.yml", tetherPRWorkflow, "refs/pull/5/head")
	fork := &tether.Repo{FullName: "mallory/demo"}
	f.gh.setPull("zoe-gh/demo", tether.Pull{Number: 5, Title: "Fork change", State: "open", User: tether.User{Login: "mallory"},
		Head: tether.Branch{Ref: "patch-1", SHA: forkSHA, Repo: fork}, Base: tether.Branch{Ref: "main", Repo: twin}, CreatedAt: time.Now()})
	// Your own pull request (#6) from a branch of the twin.
	ownSHA := twinCommit(t, f, "zoe-gh/demo", ".atelier/workflows/pr.yml", tetherPRWorkflow+"# own\n", "refs/heads/mine")
	runGit(t, f.gh.bare("zoe-gh/demo"), "update-ref", "refs/pull/6/head", ownSHA)
	f.gh.setPull("zoe-gh/demo", tether.Pull{Number: 6, Title: "My change", State: "open", User: tether.User{Login: "zoe-gh"},
		Head: tether.Branch{Ref: "mine", SHA: ownSHA, Repo: twin}, Base: tether.Branch{Ref: "main", Repo: twin}, CreatedAt: time.Now()})

	f.sync(t)
	rid := f.repoID(t)
	p5, err := a.db.PullByNumber(ctx, rid, 5)
	if err != nil || p5.Head != "tether/pr-5" || p5.AuthorKind != "import" || p5.AuthorName != "github:mallory" || p5.HeadSHA != forkSHA {
		t.Fatalf("fork PR = %+v, %v", p5, err)
	}
	p6, err := a.db.PullByNumber(ctx, rid, 6)
	if err != nil || p6.Head != "mine" || p6.AuthorKind != ActorOwner {
		t.Fatalf("own PR = %+v, %v", p6, err)
	}
	g, _ := a.git.Open("zoe", "demo")
	if sha, _ := a.tetherGit(ctx, a.git.Path("zoe", "demo"), "", "", "rev-parse", "refs/pull/5/head"); strings.TrimSpace(sha) != forkSHA {
		t.Errorf("refs/pull/5/head = %q", sha)
	}
	// The fork's run waits for approval; yours runs.
	runs := af.runs(t)
	var forkRun, ownRun store.ActionsRun
	for _, r := range runs {
		switch r.PullNumber {
		case 5:
			forkRun = r
		case 6:
			ownRun = r
		}
	}
	if forkRun.ID == 0 || forkRun.Status != store.RunWaiting || ownRun.ID == 0 || ownRun.Status == store.RunWaiting {
		t.Fatalf("runs = %+v", runs)
	}
	// Push pending statuses for both on the next sync.
	f.sync(t)
	cx := "atelier/CI/build"
	if s, ok := f.gh.statusFor(forkSHA, cx); !ok || s.State != "pending" || !strings.Contains(s.Description, "approval") ||
		!strings.HasPrefix(s.TargetURL, "https://atelier.example/zoe/demo/actions/runs/") {
		t.Fatalf("fork status = %+v %v (all %+v)", s, ok, f.gh.statuses)
	}
	// Your job runs and passes: success goes to GitHub.
	rn := af.registerRunner(t, "Box", []string{"linux"}, []string{"jobs"})
	j := rn.poll()
	if j == nil || j.RunID != ownRun.ID {
		t.Fatalf("job = %+v", j)
	}
	rn.runJob(j, actions.StatusSuccess, nil)
	f.wait()
	if s, ok := f.gh.statusFor(ownSHA, cx); !ok || s.State != "success" {
		t.Fatalf("own status = %+v %v", s, ok)
	}
	n := len(f.gh.statuses)
	f.sync(t)
	if len(f.gh.statuses) != n {
		t.Error("unchanged statuses were sent again")
	}

	// The fork pushes again: Atelier follows the head and runs (still gated).
	newFork := twinCommit(t, f, "zoe-gh/demo", ".atelier/workflows/pr.yml", tetherPRWorkflow+"# v2\n", "refs/pull/5/head")
	f.gh.mu.Lock()
	f.gh.pulls["zoe-gh/demo"][5].Head.SHA = newFork
	f.gh.mu.Unlock()
	f.sync(t)
	if sha, _ := g.BranchSHA(ctx, "tether/pr-5"); sha != newFork {
		t.Errorf("tether/pr-5 = %s, want %s", sha, newFork)
	}
	waiting := 0
	for _, r := range af.runs(t) {
		if r.PullNumber == 5 && r.SHA == newFork && r.Status == store.RunWaiting && r.EventPayload["action"] == "synchronize" {
			waiting++
		}
	}
	if waiting != 1 {
		t.Errorf("synchronize runs waiting for the fork's new head = %d", waiting)
	}
	// No push workflows ran for the fork's branch.
	for _, r := range af.runs(t) {
		if r.Event == "push" && strings.Contains(r.Ref, "tether/") {
			t.Errorf("a push run started on fork code: %+v", r)
		}
	}

	// Closed on GitHub → closed here.
	f.gh.mu.Lock()
	f.gh.pulls["zoe-gh/demo"][5].State = "closed"
	f.gh.mu.Unlock()
	f.sync(t)
	if p5, _ = a.db.PullByNumber(ctx, rid, 5); p5.State != "closed" {
		t.Errorf("fork PR state = %s", p5.State)
	}

	// Merging in Atelier isn't possible: the button merges on GitHub.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/6/merge", url.Values{"method": {"merge"}}, f.ck), 303, "merge on GitHub")
	if p6, _ = a.db.PullByNumber(ctx, rid, 6); p6.State != "merged" {
		t.Fatalf("own PR after merge = %+v", p6)
	}
	if main, _ := g.BranchSHA(ctx, "main"); main != ownSHA || ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/main") != ownSHA {
		t.Errorf("main after GitHub merge = %s (twin %s), want %s", main, ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/main"), ownSHA)
	}
	// Cross-site posts are refused before anything happens.
	rec := do(t, a, "POST", "/zoe/demo/pulls/6/merge", url.Values{}, map[string]string{"Cookie": f.ck["Cookie"], "Sec-Fetch-Site": "cross-site"})
	if rec.Code != 403 {
		t.Errorf("cross-site merge = %d", rec.Code)
	}
}

func TestTetherMergeOnGitHubMirrorsAtelierPull(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	f.connect(t)
	pullsCommit(t, f.work, "README.md", "# demo\n", "Initial")
	runGit(t, f.work, "push", "-q", "origin", "main")
	f.gh.addRepo("zoe-gh/demo", true)
	runGit(t, f.work, "push", "-q", f.gh.bare("zoe-gh/demo"), "main")
	f.link(t, "zoe-gh/demo", "github", false)
	runGit(t, f.work, "checkout", "-q", "-b", "topic")
	head := pullsCommit(t, f.work, "t.txt", "t\n", "Topic")
	runGit(t, f.work, "push", "-q", "origin", "topic")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"topic"}, "title": {"Topic"}}, f.ck), 303, "open PR")
	f.wait()
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/merge", url.Values{"method": {"squash"}}, f.ck), 303, "merge")
	p, _ := a.db.PullByNumber(ctx, f.repoID(t), 1)
	it, err := a.db.TetherItem(ctx, f.repoID(t), 1)
	if p.State != "merged" || err != nil || it.Origin != "atelier" || it.GHState != "merged" {
		t.Fatalf("pull %+v, item %+v %v", p, it, err)
	}
	g, _ := a.git.Open("zoe", "demo")
	if main, _ := g.BranchSHA(ctx, "main"); main != head {
		t.Errorf("main = %s, want %s", main, head)
	}
}

func TestTetherBridgeMirrorAndClose(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	f.connect(t)
	pullsCommit(t, f.work, "README.md", "# demo\n", "Initial")
	runGit(t, f.work, "push", "-q", "origin", "main")
	rid := f.repoID(t)

	b := tether.Get()
	if _, ok, err := b.Linked(ctx, rid); ok || err != nil {
		t.Fatalf("unlinked repo reported linked (%v)", err)
	}
	if _, _, err := b.MirrorPull(ctx, rid, 1); !errors.Is(err, tether.ErrNotLinked) {
		t.Fatalf("MirrorPull unlinked = %v", err)
	}
	f.link(t, "zoe-gh/demo", "atelier", false)
	if gh, ok, err := b.Linked(ctx, rid); !ok || err != nil || gh != "zoe-gh/demo" {
		t.Fatalf("Linked = %q %v %v", gh, ok, err)
	}
	runGit(t, f.work, "checkout", "-q", "-b", "feat")
	head := pullsCommit(t, f.work, "a.go", "package a\n\nfunc A() {}\n", "Add A")
	runGit(t, f.work, "push", "-q", "origin", "feat")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"feat"}, "title": {"Add A"}}, f.ck), 303, "open PR")
	f.wait()

	n, u, err := b.MirrorPull(ctx, rid, 1)
	if err != nil || n == 0 || !strings.Contains(u, fmt.Sprintf("/pull/%d", n)) {
		t.Fatalf("MirrorPull = %d %q %v", n, u, err)
	}
	if ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/feat") != head {
		t.Error("head branch not pushed to the twin")
	}
	f.gh.mu.Lock()
	gp := *f.gh.pulls["zoe-gh/demo"][int64(n)]
	f.gh.mu.Unlock()
	if gp.Head.Ref != "feat" || gp.Base.Ref != "main" || !strings.Contains(gp.Body, "Mirrored from Atelier") {
		t.Fatalf("GitHub PR = %+v", gp)
	}
	// Again: the same GitHub PR is updated, not a new one.
	p, _ := a.db.PullByNumber(ctx, rid, 1)
	_ = a.db.UpdatePullText(ctx, p.ID, "Add A (v2)", "")
	if n2, _, err := b.MirrorPull(ctx, rid, 1); err != nil || n2 != n {
		t.Fatalf("second MirrorPull = %d %v", n2, err)
	}
	f.gh.mu.Lock()
	title := f.gh.pulls["zoe-gh/demo"][int64(n)].Title
	f.gh.mu.Unlock()
	if title != "Add A (v2)" {
		t.Errorf("title on GitHub = %q", title)
	}
	// The mirrored PR doesn't come back as a new Atelier PR.
	f.sync(t)
	if ps, _ := a.db.Pulls(ctx, rid, "open"); len(ps) != 1 {
		t.Errorf("open pulls after sync = %d", len(ps))
	}

	// A GitHub-only bot reviews it there; its comments come back.
	f.gh.mu.Lock()
	f.gh.reviews["zoe-gh/demo"] = []tether.ReviewComment{{Comment: tether.Comment{ID: 900, Body: "Consider a doc comment.",
		User: tether.User{Login: "coderabbitai[bot]"}, CreatedAt: time.Now(), UpdatedAt: time.Now()}, Path: "a.go", Line: 3,
		PullRequestURL: fmt.Sprintf("%s/repos/zoe-gh/demo/pulls/%d", f.gh.srv.URL, n)}}
	f.gh.comments["zoe-gh/demo"] = append(f.gh.comments["zoe-gh/demo"], tether.Comment{ID: 901, Body: "Summary: looks fine.",
		User: tether.User{Login: "cursor[bot]"}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		IssueURL: fmt.Sprintf("%s/repos/zoe-gh/demo/issues/%d", f.gh.srv.URL, n)})
	f.gh.mu.Unlock()
	f.sync(t)
	f.sync(t)
	cs, _ := a.db.PullComments(ctx, p.ID)
	var bodies []string
	for _, c := range cs {
		bodies = append(bodies, c.AuthorName+": "+c.Body)
	}
	all := strings.Join(bodies, "\n")
	if len(cs) != 2 || !strings.Contains(all, "github:coderabbitai[bot]: On `a.go` line 3:") || !strings.Contains(all, "github:cursor[bot]: Summary") {
		t.Fatalf("bot comments = %q", all)
	}
	ns, _ := a.db.Notifications(ctx, store.NotificationQuery{PersonID: f.me(t).ID, SubjectTypes: []string{"tether"}})
	if len(ns) == 0 || ns[0].Kind != EventTetherComment || !strings.HasPrefix(ns[0].ActorName, "github:") {
		t.Errorf("tether notifications = %+v", ns)
	}

	// Merged in Atelier: the bridge closes the GitHub side.
	if err := b.ClosePull(ctx, rid, 1, true, head); err != nil {
		t.Fatal(err)
	}
	f.gh.mu.Lock()
	st := f.gh.pulls["zoe-gh/demo"][int64(n)].State
	last := f.gh.comments["zoe-gh/demo"][len(f.gh.comments["zoe-gh/demo"])-1].Body
	f.gh.mu.Unlock()
	if st != "closed" || !strings.Contains(last, "Merged in Atelier as "+head) {
		t.Errorf("after ClosePull: state %s, last comment %q", st, last)
	}
	// Our own "merged" comment isn't synced back.
	f.sync(t)
	if cs, _ := a.db.PullComments(ctx, p.ID); len(cs) != 2 {
		t.Errorf("comments after close = %d", len(cs))
	}
	if err := b.ClosePull(ctx, rid, 1, true, head); err != nil {
		t.Errorf("second ClosePull = %v", err)
	}
}
