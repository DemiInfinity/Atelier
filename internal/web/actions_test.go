package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
	"atelier/internal/workflow"
)

// ---------- fixtures ----------

const actCIWorkflow = `name: CI
on:
  push:
    branches: [main, 'feature/**']
    paths-ignore: ['docs/**']
  pull_request:
  workflow_dispatch:
    inputs:
      level:
        type: choice
        options: [info, debug]
        default: info
jobs:
  build:
    runs-on: [linux]
    outputs:
      version: ${{ steps.v.outputs.version }}
    steps:
      - uses: acme/tool@v1
      - id: v
        run: echo "version=1.2.3" >> $GITHUB_OUTPUT
  test:
    needs: build
    runs-on: ${{ matrix.os }}
    strategy:
      matrix:
        os: [linux]
        go: ['1.22', '1.23']
    steps:
      - run: echo ${{ matrix.go }} ${{ needs.build.outputs.version }}${{ inputs.level }}
        env:
          TOKEN: ${{ secrets.GITHUB_TOKEN }}
      - if: matrix.go == '1.23' && success()
        run: echo last
  deploy:
    needs: test
    if: github.ref == 'refs/heads/main'
    runs-on: linux
    container: alpine:3
    steps:
      - run: ./deploy
`

type actFixture struct {
	gitFixture
	srv    *httptest.Server
	ck     map[string]string
	work   string
	tagSHA string
	remote string // base of the fake GitHub
}

func newActFixture(t *testing.T) *actFixture {
	t.Helper()
	f := &actFixture{gitFixture: newGitFixture(t)}
	a := f.a
	st := a.actions()
	st.pollWait = 300 * time.Millisecond
	f.remote = t.TempDir()
	f.tagSHA = actFakeRemote(t, f.remote, "acme", "tool")
	ls := &workflow.LsRemote{Base: "file://" + f.remote}
	st.resolveRemote = ls.Resolve
	f.srv = httptest.NewServer(a)
	t.Cleanup(f.srv.Close)
	f.ck = pullsCookie(t, a)
	f.work = pullsWork(t, a)
	return f
}

// actFakeRemote makes base/owner/repo with an annotated tag v1.
func actFakeRemote(t *testing.T, base, owner, repo string) string {
	t.Helper()
	bare := filepath.Join(base, owner, repo)
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, bare, "init", "-q", "--bare", ".")
	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", ".")
	os.WriteFile(filepath.Join(work, "action.yml"), []byte("name: tool\n"), 0o644)
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-q", "-m", "tool")
	sha := runGit(t, work, "rev-parse", "HEAD")
	runGit(t, work, "tag", "-a", "v1", "-m", "v1")
	runGit(t, work, "push", "-q", bare, "main", "v1")
	return sha
}

// push pushes a branch and hands the update to Actions as the hook would.
func (f *actFixture) push(t *testing.T, branch string) string {
	t.Helper()
	g, _ := f.a.git.Open("zoe", "demo")
	old, err := g.BranchSHA(context.Background(), branch)
	if err != nil {
		old = strings.Repeat("0", 40)
	}
	runGit(t, f.work, "push", "-q", "origin", branch)
	nw := runGit(t, f.work, "rev-parse", branch)
	if err := f.a.db.QueueActionsPush(context.Background(), store.ActionsPush{Owner: "zoe", Repo: "demo", Old: old, New: nw,
		Ref: "refs/heads/" + branch, Pusher: "owner:zoe"}); err != nil {
		t.Fatal(err)
	}
	f.a.drainActionsPushes(context.Background())
	return nw
}

func (f *actFixture) runs(t *testing.T) []store.ActionsRun {
	t.Helper()
	runs, err := f.a.db.ActionsRuns(context.Background(), store.ActionsRunFilter{RepoID: f.repoID(t)})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func (f *actFixture) run(t *testing.T, id int64) store.ActionsRun {
	t.Helper()
	r, err := f.a.db.ActionsRunByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *actFixture) jobs(t *testing.T, runID int64) map[string]store.ActionsJob {
	t.Helper()
	js, err := f.a.db.ActionsJobs(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.ActionsJob{}
	for _, j := range js {
		out[j.Name] = j
	}
	return out
}

// registerRunner gets a one-time command from the settings page and registers.
func (f *actFixture) registerRunner(t *testing.T, name string, labels, caps []string) *fakeRunner {
	t.Helper()
	body := expectStatus(t, do(t, f.a, "POST", "/settings/runners/token", url.Values{}, f.ck), 200, "generate token")
	m := regexp.MustCompile(`--token (atreg_[0-9a-f]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no registration command:\n%.1500s", body)
	}
	fr := &fakeRunner{t: t, base: f.srv.URL}
	osName, arch := "linux", "amd64"
	if name == "Mac" {
		osName, arch = "darwin", "arm64"
	}
	var resp actions.RegisterResponse
	code := fr.call("/register", actions.RegisterRequest{Token: m[1], Name: name, OS: osName, Arch: arch, Labels: labels,
		Capabilities: caps, Version: "test", Protocol: actions.ProtocolVersion}, &resp)
	if code != 200 || resp.RunnerToken == "" || resp.ServerURL == "" {
		t.Fatalf("register = %d %+v", code, resp)
	}
	// The token works once.
	if code := fr.call("/register", actions.RegisterRequest{Token: m[1], Name: "again"}, nil); code != http.StatusUnauthorized {
		t.Errorf("second registration = %d, want 401", code)
	}
	fr.token, fr.labels, fr.caps = resp.RunnerToken, labels, caps
	return fr
}

type fakeRunner struct {
	t            *testing.T
	base, token  string
	labels, caps []string
}

func (fr *fakeRunner) call(path string, in, out any) int {
	fr.t.Helper()
	b, _ := json.Marshal(in)
	req, _ := http.NewRequest("POST", fr.base+actions.APIPrefix+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if fr.token != "" {
		req.Header.Set("Authorization", "Bearer "+fr.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fr.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode == 200 {
		if err := json.Unmarshal(data, out); err != nil {
			fr.t.Fatalf("%s: %v\n%s", path, err, data)
		}
	}
	return resp.StatusCode
}

func (fr *fakeRunner) poll() *actions.Job {
	fr.t.Helper()
	var resp actions.PollResponse
	if code := fr.call("/poll", actions.PollRequest{Labels: fr.labels, Capabilities: fr.caps, Version: "test"}, &resp); code != 200 {
		fr.t.Fatalf("poll = %d", code)
	}
	return resp.Job
}

func (fr *fakeRunner) logs(id int64, step int, texts ...string) actions.LogsResponse {
	fr.t.Helper()
	var lines []actions.LogLine
	for _, s := range texts {
		lines = append(lines, actions.LogLine{Time: time.Now(), Step: step, Stream: "stdout", Text: s})
	}
	var resp actions.LogsResponse
	if code := fr.call("/jobs/"+itoa(id)+"/logs", actions.LogsRequest{Lines: lines}, &resp); code != 200 {
		fr.t.Fatalf("logs = %d", code)
	}
	return resp
}

func (fr *fakeRunner) step(id int64, idx int, status string) {
	fr.t.Helper()
	now := time.Now()
	if code := fr.call("/jobs/"+itoa(id)+"/steps/"+itoa(int64(idx)), actions.StepResult{Status: status, StartedAt: now.Add(-time.Second),
		FinishedAt: now, Summary: "### Step summary"}, nil); code != 200 {
		fr.t.Fatalf("step = %d", code)
	}
}

func (fr *fakeRunner) finish(id int64, status string, outputs map[string]string) {
	fr.t.Helper()
	if code := fr.call("/jobs/"+itoa(id)+"/finish", actions.FinishRequest{Status: status, Outputs: outputs,
		StartedAt: time.Now(), FinishedAt: time.Now()}, nil); code != 200 {
		fr.t.Fatalf("finish = %d", code)
	}
}

// runJob plays a whole job: logs, every step, finish.
func (fr *fakeRunner) runJob(j *actions.Job, status string, outputs map[string]string) {
	fr.t.Helper()
	fr.logs(j.ID, -1, "Setting up")
	for _, s := range j.Steps {
		fr.logs(j.ID, s.Index, "running "+s.Name)
		fr.step(j.ID, s.Index, status)
	}
	fr.finish(j.ID, status, outputs)
}

// ---------- tests ----------

func TestActionsEndToEnd(t *testing.T) {
	f := newActFixture(t)
	a, ctx := f.a, context.Background()
	pullsCommit(t, f.work, ".atelier/workflows/ci.yml", actCIWorkflow, "Add CI")
	sha := f.push(t, "main")

	runs := f.runs(t)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	run := runs[0]
	if run.Event != "push" || run.SHA != sha || run.WorkflowName != "CI" || run.RunNumber != 1 || run.Seq != 1 {
		t.Fatalf("run = %+v", run)
	}
	if run.LockResolved["acme/tool@v1"] != f.tagSHA || !strings.Contains(run.LockGenerated, f.tagSHA) {
		t.Fatalf("lockfile resolution = %v / %q", run.LockResolved, run.LockGenerated)
	}
	jobs := f.jobs(t, run.ID)
	if len(jobs) != 4 || jobs["build"].Status != actions.StatusQueued || jobs["test (linux, 1.22)"].Status != store.JobPending {
		t.Fatalf("jobs = %+v", jobs)
	}

	// A runner without the label gets nothing; one with it gets build.
	mac := f.registerRunner(t, "Mac", []string{"mac"}, []string{"jobs"})
	if j := mac.poll(); j != nil {
		t.Fatalf("mac got %s", j.Name)
	}
	lx := f.registerRunner(t, "Linux box", []string{"linux"}, []string{"jobs"})
	build := lx.poll()
	if build == nil || build.Name != "build" {
		t.Fatalf("build = %+v", build)
	}
	if build.Steps[0].Uses != "acme/tool@"+f.tagSHA || build.Steps[0].UsesTag != "v1" {
		t.Errorf("pinned uses = %q (%q)", build.Steps[0].Uses, build.Steps[0].UsesTag)
	}
	if build.Outputs["version"] != "${{ steps.v.outputs.version }}" || build.JobToken == "" ||
		build.CloneURL != f.srv.URL+"/zoe/demo.git" || build.Context["sha"] != sha || build.Context["run_number"] != "1" {
		t.Errorf("job = outputs %v token %q clone %q ctx %v", build.Outputs, build.JobToken, build.CloneURL, build.Context)
	}

	// The job token clones the repository over HTTPS, and nothing more.
	u, _ := url.Parse(f.srv.URL)
	clone := t.TempDir()
	runGit(t, clone, "clone", "-q", "http://atelier:"+build.JobToken+"@"+u.Host+"/zoe/demo.git", "src")
	if _, err := os.Stat(filepath.Join(clone, "src", ".atelier", "workflows", "ci.yml")); err != nil {
		t.Fatalf("clone with job token: %v", err)
	}
	pullsCommit(t, filepath.Join(clone, "src"), "x.txt", "x", "x")
	if out, err := gitTry(filepath.Join(clone, "src"), "push", "origin", "HEAD:main"); err == nil {
		t.Fatalf("job token pushed!\n%s", out)
	}
	o, _ := a.db.OwnerByName(ctx, "zoe")
	if err := a.git.Init(ctx, "zoe", "secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.CreateRepo(ctx, o.ID, "secret", "", true); err != nil {
		t.Fatal(err)
	}
	who, ok := a.jobActor(ctx, build.JobToken)
	if !ok || who.Kind != ActorJob || jobRepoID(who) != f.repoID(t) {
		t.Fatalf("jobActor = %+v %v", who, ok)
	}
	secret, _ := a.db.RepoByName(ctx, "zoe", "secret")
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	if a.can(ctx, who, secret, PermRead) || !a.can(ctx, who, demo, PermRead) || a.can(ctx, who, demo, PermWrite) {
		t.Error("job token access is wrong")
	}
	// Packages of the repository's owner: read yes, write only when granted.
	pkg := store.Repo{OwnerID: o.ID, Private: true}
	if !a.can(ctx, who, pkg, PermRead) || a.can(ctx, who, pkg, PermWrite) {
		t.Error("job token package access is wrong")
	}

	lx.runJob(build, actions.StatusSuccess, map[string]string{"version": "1.2.3"})

	// The matrix jobs are queued with needs outputs filled in.
	var tests []*actions.Job
	for i := 0; i < 2; i++ {
		j := lx.poll()
		if j == nil {
			t.Fatalf("test job %d not assigned", i)
		}
		tests = append(tests, j)
	}
	for _, j := range tests {
		go122 := strings.Contains(j.Name, "1.22")
		want := "echo 1.23 1.2.3"
		if go122 {
			want = "echo 1.22 1.2.3"
		}
		if j.Steps[0].Run != want {
			t.Errorf("%s run = %q, want %q", j.Name, j.Steps[0].Run, want)
		}
		if j.Steps[0].Env["TOKEN"] != "${{ secrets.GITHUB_TOKEN }}" || j.Secrets["GITHUB_TOKEN"] != j.JobToken {
			t.Errorf("%s secrets = %v env = %v", j.Name, j.Secrets, j.Steps[0].Env)
		}
		wantIf := "success()"
		if go122 {
			wantIf = "false"
		}
		if j.Steps[1].If != wantIf {
			t.Errorf("%s step if = %q, want %q", j.Name, j.Steps[1].If, wantIf)
		}
		if m, _ := j.Context["matrix"].(map[string]any); m["os"] != "linux" {
			t.Errorf("%s context matrix = %v", j.Name, j.Context["matrix"])
		}
		lx.runJob(j, actions.StatusSuccess, nil)
	}

	// deploy needs Docker: the plain runner can't take it.
	if j := lx.poll(); j != nil {
		t.Fatalf("non-docker runner got %s", j.Name)
	}
	dk := f.registerRunner(t, "Docker box", []string{"linux"}, []string{"jobs", "docker"})
	deploy := dk.poll()
	if deploy == nil || deploy.Name != "deploy" || deploy.Container == nil || deploy.Container.Image != "alpine:3" {
		t.Fatalf("deploy = %+v", deploy)
	}
	dk.runJob(deploy, actions.StatusSuccess, nil)

	run = f.run(t, run.ID)
	if run.Status != store.RunCompleted || run.Conclusion != actions.StatusSuccess {
		t.Fatalf("run = %s/%s", run.Status, run.Conclusion)
	}
	if _, ok := a.jobActor(ctx, build.JobToken); ok {
		t.Error("job token still works after the job finished")
	}

	// Pages.
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/actions", nil, f.ck), 200, "actions index")
	mustContain(t, body, "index", "All workflows", "CI", "YAML", "Linux box", "Lockfile", "Add CI", "/zoe/demo/actions/runs/1")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions?workflow=.atelier/workflows/ci.yml", nil, f.ck), 200, "workflow")
	mustContain(t, body, "workflow", "Run workflow", "paths-ignore", "input.level")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions/runs/1", nil, f.ck), 200, "run page")
	mustContain(t, body, "run page", "resolved without a lockfile", "Download actions.lock", "test (linux, 1.22)",
		"running Run echo 1.22 1.2.3", "Re-run all jobs", "act-graph", "<h3>Step summary</h3>")
	rec := do(t, a, "GET", "/zoe/demo/actions/runs/1/lockfile", nil, f.ck)
	if l, err := workflow.ParseLockfile(rec.Body.Bytes()); err != nil || l["acme/tool@v1"] != f.tagSHA {
		t.Errorf("lockfile download = %v %v", l, err)
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions/runs/1/jobs/"+itoa(build.ID)+"/logs", nil, f.ck), 200, "raw log")
	mustContain(t, body, "raw log", "Setting up", "running Run acme/tool@v1", "Job assigned to runner Linux box")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions/runs/1/logs/stream", nil, f.ck), 200, "stream")
	mustContain(t, body, "stream", "event: log", "event: status", "event: done", "Setting up")
	body = expectStatus(t, do(t, a, "GET", "/settings/runners", nil, f.ck), 200, "runners page")
	mustContain(t, body, "runners", "Linux box", "Docker box", "Online", "self-hosted", "x64", "docker", "last job")

	// Paths: a docs-only push doesn't run CI.
	pullsCommit(t, f.work, "docs/readme.md", "docs", "Docs")
	f.push(t, "main")
	if n := len(f.runs(t)); n != 1 {
		t.Errorf("docs-only push started a run (%d runs)", n)
	}

	// Re-run failed jobs needs a failure; re-run all starts everything again.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/runs/1/rerun", url.Values{"mode": {"failed"}}, f.ck), 409, "rerun failed")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/runs/1/rerun", url.Values{"mode": {"all"}}, f.ck), 303, "rerun all")
	run = f.run(t, run.ID)
	if run.Attempt != 2 || run.Done() || f.jobs(t, run.ID)["build"].Status != actions.StatusQueued {
		t.Fatalf("after rerun: %+v", run)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/runs/1/cancel", url.Values{}, f.ck), 303, "cancel")
	if run = f.run(t, run.ID); run.Conclusion != actions.StatusCancelled || !run.Done() {
		t.Fatalf("cancelled run = %s/%s", run.Status, run.Conclusion)
	}

	// workflow_dispatch with inputs.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/dispatch", url.Values{"workflow": {".atelier/workflows/ci.yml"},
		"ref": {"main"}, "input.level": {"loud"}}, f.ck), 422, "bad choice")
	rec = do(t, a, "POST", "/zoe/demo/actions/dispatch", url.Values{"workflow": {".atelier/workflows/ci.yml"},
		"ref": {"main"}, "input.level": {"debug"}}, f.ck)
	if rec.Code != 303 {
		t.Fatalf("dispatch = %d\n%.1000s", rec.Code, rec.Body)
	}
	disp := f.runs(t)[0]
	if disp.Event != "workflow_dispatch" || disp.Inputs["level"] != "debug" || disp.RunNumber != 2 {
		t.Errorf("dispatched run = %+v", disp)
	}

	// Requiring a lockfile makes unpinned runs fail.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/lockfile", url.Values{"require": {"1"}}, f.ck), 303, "require lockfile")
	pullsCommit(t, f.work, "main.go", "package main\n", "Code")
	f.push(t, "main")
	failed := f.runs(t)[0]
	if failed.Conclusion != actions.StatusFailure || len(failed.Errors) == 0 || !strings.Contains(failed.Errors[0], "requires a lockfile") {
		t.Errorf("unlocked run = %s %v", failed.Conclusion, failed.Errors)
	}
	// Committing the generated lockfile fixes it.
	pullsCommit(t, f.work, ".atelier/actions.lock", run.LockGenerated, "Lock actions")
	f.push(t, "main")
	if r := f.runs(t)[0]; len(r.Errors) != 0 || len(r.LockResolved) != 0 {
		t.Errorf("locked run errors = %v resolved = %v", r.Errors, r.LockResolved)
	}
}

func TestActionsCancelAndTimeout(t *testing.T) {
	f := newActFixture(t)
	a, ctx := f.a, context.Background()
	pullsCommit(t, f.work, ".atelier/workflows/long.yml", `on: push
jobs:
  long:
    runs-on: linux
    timeout-minutes: 1
    steps:
      - run: sleep 1000
  after:
    needs: long
    runs-on: linux
    steps:
      - run: echo after
`, "Long")
	f.push(t, "main")
	rn := f.registerRunner(t, "Box", []string{"linux"}, []string{"jobs"})
	j := rn.poll()
	if j == nil {
		t.Fatal("no job")
	}
	if rn.logs(j.ID, 0, "working").Cancel {
		t.Fatal("cancel before anyone asked")
	}
	run := f.runs(t)[0]
	if run.Status != store.RunRunning {
		t.Errorf("run status = %s", run.Status)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/runs/1/cancel", url.Values{}, f.ck), 303, "cancel")
	if !rn.logs(j.ID, 0, "still working").Cancel {
		t.Fatal("the logs reply didn't ask the runner to cancel")
	}
	rn.finish(j.ID, actions.StatusCancelled, nil)
	run = f.run(t, run.ID)
	jobs := f.jobs(t, run.ID)
	if run.Conclusion != actions.StatusCancelled || jobs["after"].Status != actions.StatusCancelled {
		t.Fatalf("after cancel: run %s, after %s", run.Conclusion, jobs["after"].Status)
	}

	// Timeouts: the runner is asked to stop, then the server gives up.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/runs/1/rerun", url.Values{"mode": {"all"}}, f.ck), 303, "rerun")
	j = rn.poll()
	if j == nil {
		t.Fatal("no job after rerun")
	}
	sj, _ := a.db.ActionsJob(ctx, j.ID)
	sj.StartedAt = time.Now().Add(-90 * time.Second)
	_ = a.db.SaveActionsJob(ctx, sj)
	a.actionsMaintenance(ctx)
	if !rn.logs(j.ID, 0, "ignoring the clock").Cancel {
		t.Fatal("timed-out job wasn't cancelled")
	}
	sj, _ = a.db.ActionsJob(ctx, j.ID)
	sj.StartedAt = time.Now().Add(-5 * time.Minute)
	_ = a.db.SaveActionsJob(ctx, sj)
	a.actionsMaintenance(ctx)
	sj, _ = a.db.ActionsJob(ctx, j.ID)
	if sj.Status != actions.StatusCancelled && sj.Status != actions.StatusFailure || !strings.Contains(sj.Error, "timeout") {
		t.Fatalf("timed-out job = %s %q", sj.Status, sj.Error)
	}
	// A late finish from the runner changes nothing.
	rn.finish(j.ID, actions.StatusSuccess, nil)
	if sj2, _ := a.db.ActionsJob(ctx, j.ID); sj2.Status != sj.Status {
		t.Error("late finish overwrote the timed-out job")
	}
	// Unknown runner tokens are refused.
	bad := &fakeRunner{t: t, base: f.srv.URL, token: "atr_nope"}
	if code := bad.call("/poll", actions.PollRequest{}, nil); code != http.StatusUnauthorized {
		t.Errorf("bad token poll = %d", code)
	}
}

func TestActionsPullRequestApprovalAndChecks(t *testing.T) {
	f := newActFixture(t)
	a, ctx := f.a, context.Background()
	pullsCommit(t, f.work, "README.md", "# demo\n", "Initial")
	f.push(t, "main")
	runGit(t, f.work, "checkout", "-q", "-b", "feature/x")
	pullsCommit(t, f.work, ".atelier/workflows/pr.yml", `on: pull_request
jobs:
  build:
    runs-on: linux
    steps:
      - run: make
`, "PR workflow")
	f.push(t, "feature/x") // no push trigger in pr.yml: nothing runs
	if n := len(f.runs(t)); n != 0 {
		t.Fatalf("push started %d runs", n)
	}
	rid := f.repoID(t)
	rule, err := a.db.CreateBranchRule(ctx, store.BranchRule{RepoID: rid, Pattern: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetBranchRuleChecks(ctx, rule.ID, []string{"build"}); err != nil {
		t.Fatal(err)
	}
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/settings/branches", nil, f.ck), 200, "rules")
	mustContain(t, body, "rules", "Check: build", "required_checks")

	// Opening the pull request starts its workflow.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"feature/x"}, "title": {"X"}}, f.ck), 303, "open PR")
	a.actions().bg.Wait()
	runs := f.runs(t)
	if len(runs) != 1 || runs[0].Event != "pull_request" || runs[0].PullNumber != 1 {
		t.Fatalf("PR runs = %+v", runs)
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/1", nil, f.ck), 200, "PR")
	mustContain(t, body, "PR pending", "Not ready to merge", "Waiting for required check", "build")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/merge", url.Values{"method": {"squash"}}, f.ck), 409, "merge blocked")

	rn := f.registerRunner(t, "Box", []string{"linux"}, []string{"jobs"})
	j := rn.poll()
	if j == nil || j.Event != "pull_request" || j.Context["head_ref"] != "feature/x" || j.Context["base_ref"] != "main" {
		t.Fatalf("PR job = %+v", j)
	}
	rn.runJob(j, actions.StatusFailure, nil)
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/1", nil, f.ck), 200, "PR failing")
	mustContain(t, body, "PR failing", "Checks failing · 1 of 1", "Required check failed: build")

	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/runs/1/rerun", url.Values{"mode": {"failed"}}, f.ck), 303, "rerun failed")
	j = rn.poll()
	rn.runJob(j, actions.StatusSuccess, nil)
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/1", nil, f.ck), 200, "PR passing")
	mustContain(t, body, "PR passing", "Checks passed · 1 of 1", "Ready to merge")

	// A pull request from outside (imported) waits for approval.
	g, _ := a.git.Open("zoe", "demo")
	head, _ := g.BranchSHA(ctx, "feature/x")
	p, err := a.db.CreatePull(ctx, store.NewPull{RepoID: rid, Title: "Outside", Head: "feature/x", Base: "main", HeadSHA: head,
		AuthorKind: "import", AuthorName: "stranger"})
	if err != nil {
		t.Fatal(err)
	}
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	a.emit(ctx, Event{Kind: EventPullOpened, Repo: demo, Number: p.Number, Title: p.Title, Actor: Actor{Kind: "import", Name: "stranger"}})
	a.actions().bg.Wait()
	out := f.runs(t)[0]
	jobs := f.jobs(t, out.ID)
	if out.Status != store.RunWaiting || jobs["build"].Status != actions.StatusWaiting || !strings.Contains(jobs["build"].Approval, "stranger") {
		t.Fatalf("outside run = %s, job %+v", out.Status, jobs["build"])
	}
	if j := rn.poll(); j != nil {
		t.Fatal("a waiting job was handed out")
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions/runs/"+itoa(out.Seq), nil, f.ck), 200, "waiting run")
	mustContain(t, body, "waiting run", "waiting for approval", "Approve and run")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/runs/"+itoa(out.Seq)+"/approve", url.Values{}, f.ck), 303, "approve")
	j = rn.poll()
	if j == nil || j.RunID != out.ID {
		t.Fatalf("approved job not assigned: %+v", j)
	}
	// The newest run of a check counts: while it runs, the merge waits.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/merge", url.Values{"method": {"squash"}}, f.ck), 409, "merge while pending")
	rn.runJob(j, actions.StatusSuccess, nil)

	// The merge goes through, and counts as a push to main.
	rec := do(t, a, "POST", "/zoe/demo/pulls/1/merge", url.Values{"method": {"squash"}}, f.ck)
	if rec.Code != 303 {
		t.Fatalf("merge = %d\n%.2000s", rec.Code, rec.Body)
	}
	a.actions().bg.Wait()
}

func TestActionsQueueMatching(t *testing.T) {
	rn := store.ActionsRunner{Enabled: true, OS: "darwin", Arch: "arm64", Labels: []string{"Mac"}, Capabilities: []string{"jobs"}}
	for _, c := range []struct {
		labels []string
		docker bool
		want   bool
	}{
		{[]string{"self-hosted"}, false, true},
		{[]string{"mac", "macos", "ARM64"}, false, true},
		{[]string{"macos", "x64"}, false, false},
		{[]string{"linux"}, false, false},
		{[]string{"mac"}, true, false},
	} {
		if got := runnerCanRun(rn, store.ActionsJob{RunsOn: c.labels, NeedsDocker: c.docker}); got != c.want {
			t.Errorf("runnerCanRun(%v, docker=%v) = %v", c.labels, c.docker, got)
		}
	}
	rn.Capabilities = append(rn.Capabilities, "docker")
	if !runnerCanRun(rn, store.ActionsJob{RunsOn: []string{"mac"}, NeedsDocker: true}) {
		t.Error("docker runner should take container jobs")
	}
	rn.Enabled = false
	if runnerCanRun(rn, store.ActionsJob{RunsOn: []string{"mac"}}) {
		t.Error("disabled runner took a job")
	}
	if runnerCanRun(store.ActionsRunner{Enabled: true, Labels: []string{"x"}}, store.ActionsJob{RunsOn: []string{"x"}}) {
		t.Error("a runner without the jobs capability took a job")
	}
	names := parseCheckNames("build, CI / lint\nbuild,,  ")
	if strings.Join(names, "|") != "build|CI / lint" {
		t.Errorf("parseCheckNames = %v", names)
	}
}
