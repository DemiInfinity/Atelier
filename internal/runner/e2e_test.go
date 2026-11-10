package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"atelier/internal/actions"
)

func skipNoUnixShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("end-to-end scripts are written for sh/bash")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitServer serves bare repos over smart HTTP (git http-backend), requiring
// Basic auth "atelier:<token>". It counts requests that lacked the token.
type gitServer struct {
	url        string
	sha        string
	unauthed   atomic.Int32
	authorized atomic.Int32
}

func newGitServer(t *testing.T, token string) *gitServer {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "repos", "demi", "app.git")
	os.MkdirAll(work, 0o755)
	gitCmd(t, root, "init", "-q", "--bare", "-b", "main", bare)
	gitCmd(t, root, "init", "-q", "-b", "main", work)
	os.WriteFile(filepath.Join(work, "README.md"), []byte("# app\n"), 0o644)
	os.MkdirAll(filepath.Join(work, "src"), 0o755)
	os.WriteFile(filepath.Join(work, "src", "lock.txt"), []byte("v1\n"), 0o644)
	gitCmd(t, work, "add", ".")
	gitCmd(t, work, "commit", "-q", "-m", "first")
	os.WriteFile(filepath.Join(work, "second.txt"), []byte("2\n"), 0o644)
	gitCmd(t, work, "add", ".")
	gitCmd(t, work, "commit", "-q", "-m", "second")
	gitCmd(t, work, "push", "-q", bare, "main")
	gs := &gitServer{sha: gitCmd(t, work, "rev-parse", "HEAD")}
	execPath := gitCmd(t, root, "--exec-path")
	backend := &cgi.Handler{Path: filepath.Join(execPath, "git-http-backend"), Root: "/git",
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "repos"), "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "atelier" || p != token {
			gs.unauthed.Add(1)
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		gs.authorized.Add(1)
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	gs.url = srv.URL + "/git/demi/app.git"
	return gs
}

const fakeSHA = "0123456789abcdef0123456789abcdef01234567"

func baseJob(id int64, gs *gitServer) *JobSpec {
	j := &JobSpec{Job: actions.Job{
		ID: id, RunID: 100 + id, Name: "build", Workflow: ".atelier/workflows/ci.yml",
		Repo: actions.RepoRef{Owner: "demi", Name: "app"}, Event: "push", Ref: "refs/heads/main",
		JobToken: "atl_job_token_abcdef123456", RunsOn: []string{"self-hosted"},
		Context: map[string]any{"actor": "demi", "event": map[string]any{"ref": "refs/heads/main"}},
	}}
	if gs != nil {
		j.CloneURL = gs.url
		j.SHA = gs.sha
	}
	return j
}

func TestEndToEndJob(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	job := baseJob(1, nil)
	gs := newGitServer(t, job.JobToken)
	job.CloneURL, job.SHA = gs.url, gs.sha
	secret := "s3cr3t-Value-42!"
	job.Secrets = map[string]string{"API_KEY": secret}
	job.Env = map[string]string{"JOB_LEVEL": "jl-${{ github.event_name }}"}
	job.Steps = []actions.Step{
		{Index: 0, Name: "Checkout", Uses: "actions/checkout@" + fakeSHA},
		{Index: 1, ID: "first", Run: `test -f README.md && test -f second.txt
echo "greeting=hello" >> "$GITHUB_OUTPUT"
echo "MYVAR=fromenv" >> "$GITHUB_ENV"
{ echo 'MULTI<<EOF'; echo 'line one'; echo 'line two'; echo 'EOF'; } >> "$GITHUB_ENV"
mkdir -p "$RUNNER_TEMP/bin"
printf '#!/bin/sh\necho tool-ran\n' > "$RUNNER_TEMP/bin/mytool"
chmod +x "$RUNNER_TEMP/bin/mytool"
echo "$RUNNER_TEMP/bin" >> "$GITHUB_PATH"
echo "## Summary" >> "$GITHUB_STEP_SUMMARY"
echo "depth=$(git rev-list --count HEAD)"`},
		{Index: 2, Run: `echo "got ${{ steps.first.outputs.greeting }} $MYVAR $JOB_LEVEL"
mytool
echo "multi=[$MULTI]"
echo "ci=$CI sha=$GITHUB_SHA ref=$GITHUB_REF repo=$GITHUB_REPOSITORY event=$GITHUB_EVENT_NAME run=$GITHUB_RUN_ID"
echo "atelier=$ATELIER_REPOSITORY os=$RUNNER_OS ws_ok=$([ "$GITHUB_WORKSPACE" = "$(pwd)" ] && echo yes)"
echo "runner=${{ runner.os }} hash=${{ hashFiles('**/lock.txt') }}"
test -f "$GITHUB_EVENT_PATH" && echo event-file-ok`},
		{Index: 3, Name: "secrets", Env: map[string]string{"KEY": "${{ secrets.API_KEY }}"}, Run: `echo "secret is ${{ secrets.API_KEY }}"
echo "env $KEY"
echo "b64 $(printf %s "$KEY" | base64)"
echo "basic $(printf 'x:%s' "$KEY" | base64)"
echo "::add-mask::dynamic-value-123"
echo "later dynamic-value-123"
echo "token ${{ github.token }}"`},
		{Index: 4, ID: "bad", Run: `echo "::set-output name=legacy::old"; exit 3`, ContinueOnError: true},
		{Index: 5, ID: "fail", Run: `echo failing; exit 1`},
		{Index: 6, Run: `echo should-not-run`},
		{Index: 7, If: "failure()", Run: `echo "handler outcome=${{ steps.bad.outcome }} conclusion=${{ steps.bad.conclusion }} legacy=${{ steps.bad.outputs.legacy }} fail=${{ steps.fail.outcome }}"`},
		{Index: 8, If: "${{ always() }}", Shell: "sh", Run: `echo "status ${{ job.status }}"`},
		{Index: 9, If: "steps.first.outputs.greeting == 'hello'", Run: `echo implicit-success-skipped`},
	}
	job.Outputs = map[string]string{"greeting": "${{ steps.first.outputs.greeting }}", "leak": "${{ secrets.API_KEY }}"}

	r := f.testRunner(t)
	f.runJobs(t, r, job)

	logs := f.logText(1)
	fin := f.finish(1)
	if fin.Status != actions.StatusFailure {
		t.Errorf("job status = %q, want failure\n%s", fin.Status, logs)
	}
	for _, want := range []string{
		"got hello fromenv jl-push", "tool-ran", "multi=[line one\nline two]", "depth=1",
		"ci=true sha=" + gs.sha + " ref=refs/heads/main repo=demi/app event=push run=101",
		"atelier=demi/app os=" + runnerOS() + " ws_ok=yes", "runner=" + runnerOS() + " hash=",
		"event-file-ok", "secret is ***", "env ***", "b64 ***", "::add-mask::***", "later ***", "token ***",
		"handler outcome=failure conclusion=success legacy=old fail=failure", "status failure",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q", want)
		}
	}
	for _, bad := range []string{secret, base64.StdEncoding.EncodeToString([]byte(secret)), "dynamic-value-123", job.JobToken} {
		if strings.Contains(logs, bad) {
			t.Errorf("logs contain %q", bad)
		}
	}
	for _, bad := range []string{"should-not-run", "implicit-success-skipped"} {
		if hasLine(logs, bad) {
			t.Errorf("step output %q present", bad)
		}
	}
	if strings.Contains(logs, "basic "+base64.StdEncoding.EncodeToString([]byte("x:"+secret))) {
		t.Error("embedded base64 of the secret was not masked")
	}
	if strings.Contains(logs, "hash=\n") || strings.Contains(logs, "hash= ") {
		t.Error("hashFiles returned empty")
	}
	wantStatus := map[int]string{0: "success", 1: "success", 2: "success", 3: "success", 4: "failure", 5: "failure", 6: "skipped", 7: "success", 8: "success", 9: "skipped"}
	for idx, st := range wantStatus {
		if got := f.step(1, idx).Status; got != st {
			t.Errorf("step %d status = %q, want %q", idx, got, st)
		}
	}
	if c := f.step(1, 4).Conclusion; c != "success" {
		t.Errorf("continue-on-error conclusion = %q", c)
	}
	if f.step(1, 4).ExitCode != 3 {
		t.Errorf("exit code = %d", f.step(1, 4).ExitCode)
	}
	if o := f.step(1, 1).Outputs["greeting"]; o != "hello" {
		t.Errorf("step output = %q", o)
	}
	if !strings.Contains(f.step(1, 1).Summary, "## Summary") {
		t.Errorf("summary = %q", f.step(1, 1).Summary)
	}
	if fin.Outputs["greeting"] != "hello" || fin.Outputs["leak"] != "***" {
		t.Errorf("job outputs = %v", fin.Outputs)
	}
	if gs.authorized.Load() == 0 {
		t.Error("git server never saw the job token")
	}
	// workspace cleaned up
	work, _ := r.Config.Dirs()
	entries, _ := os.ReadDir(work)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "job-") {
			t.Errorf("workspace %s left behind", e.Name())
		}
	}
	// lines carry step indexes and streams
	f.mu.Lock()
	sawStderr := false
	for _, l := range f.logs[1] {
		if l.Stream == "stderr" {
			sawStderr = true
		}
		if l.Time.IsZero() {
			t.Error("log line without time")
			break
		}
	}
	f.mu.Unlock()
	if !sawStderr {
		t.Error("no stderr lines recorded (git progress goes to stderr)")
	}
}

func TestCheckoutOptions(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	job := baseJob(2, nil)
	gs := newGitServer(t, job.JobToken)
	job.CloneURL, job.SHA = gs.url, gs.sha
	job.Steps = []actions.Step{
		{Index: 0, Uses: "actions/checkout@" + fakeSHA, With: map[string]string{"path": "sub/dir", "fetch-depth": "0", "ref": "main"}},
		{Index: 1, WorkingDirectory: "sub/dir", Run: `echo "count=$(git rev-list --count HEAD) branch=$(git rev-parse --abbrev-ref HEAD)"
git config --get include.path
! grep -r "atl_job_token" .git/config
git ls-remote origin main >/dev/null && echo persisted-ok`},
		{Index: 2, Uses: "actions/checkout@" + fakeSHA, With: map[string]string{"path": "nocreds", "persist-credentials": "false"}},
		{Index: 3, WorkingDirectory: "nocreds", Run: `if git config --get include.path; then echo has-include; else echo no-include; fi`},
	}
	f.runJobs(t, f.testRunner(t), job)
	logs := f.logText(2)
	if fin := f.finish(2); fin.Status != actions.StatusSuccess {
		t.Fatalf("status %s\n%s", fin.Status, logs)
	}
	for _, want := range []string{"count=2 branch=main", "atelier-credentials.config", "persisted-ok", "no-include"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q\n%s", want, logs)
		}
	}
	if strings.Contains(logs, job.JobToken) {
		t.Error("token leaked")
	}
}

func TestCancelMidStep(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	job := baseJob(3, nil)
	job.Steps = []actions.Step{
		{Index: 0, Run: `echo started-sleep; sleep 30; echo woke-up`},
		{Index: 1, Run: `echo next-step`},
		{Index: 2, If: "cancelled()", Run: `echo cleanup-ran status=${{ job.status }}`},
	}
	f.cancelWhen[3] = "started-sleep"
	start := time.Now()
	f.runJobs(t, f.testRunner(t), job)
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("cancel took %v", d)
	}
	logs := f.logText(3)
	if st := f.finish(3).Status; st != actions.StatusCancelled {
		t.Errorf("status = %s\n%s", st, logs)
	}
	if hasLine(logs, "woke-up") || hasLine(logs, "next-step") {
		t.Errorf("job kept running after cancel:\n%s", logs)
	}
	if !strings.Contains(logs, "cleanup-ran status=cancelled") {
		t.Errorf("cancelled() step did not run:\n%s", logs)
	}
	if s := f.step(3, 0).Status; s != actions.StatusCancelled {
		t.Errorf("step 0 = %s", s)
	}
	if s := f.step(3, 1).Status; s != actions.StatusSkipped {
		t.Errorf("step 1 = %s", s)
	}
}

func TestTimeouts(t *testing.T) {
	skipNoUnixShell(t)
	old := timeUnit
	timeUnit = 300 * time.Millisecond
	defer func() { timeUnit = old }()
	f := newFakeServer(t)
	stepJob := baseJob(4, nil)
	stepJob.Steps = []actions.Step{
		{Index: 0, TimeoutMinutes: 1, Run: `sleep 20; echo not-reached`},
		{Index: 1, If: "always()", Run: `echo after-timeout`},
	}
	jobJob := baseJob(5, nil)
	jobJob.TimeoutMinutes = 2
	jobJob.Steps = []actions.Step{{Index: 0, Run: `sleep 20`}, {Index: 1, If: "always()", Run: `echo always-after-job-timeout`}}
	f.runJobs(t, f.testRunner(t), stepJob, jobJob)

	if st := f.finish(4).Status; st != actions.StatusFailure {
		t.Errorf("step-timeout job = %s", st)
	}
	logs := f.logText(4)
	if !strings.Contains(logs, "timed out") || hasLine(logs, "not-reached") || !strings.Contains(logs, "after-timeout") {
		t.Errorf("step timeout logs:\n%s", logs)
	}
	if st := f.finish(5).Status; st != actions.StatusFailure {
		t.Errorf("job-timeout job = %s", st)
	}
	if !strings.Contains(f.logText(5), "always-after-job-timeout") {
		t.Errorf("always() step after job timeout did not run:\n%s", f.logText(5))
	}
}

// ---------- actions from a fake codeload ----------

type tarFile struct {
	name, body string
	mode       int64
}

func actionTarball(t *testing.T, repo, sha string, files []tarFile, pax bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if pax {
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": sha}})
	}
	top := repo + "-" + sha + "/"
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: top, Mode: 0o755})
	for _, f := range files {
		m := f.mode
		if m == 0 {
			m = 0o644
		}
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: top + f.name, Mode: m, Size: int64(len(f.body))})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func codeload(t *testing.T, tarballs map[string][]byte) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := tarballs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const (
	compSHA  = "1111111111111111111111111111111111111111"
	jsSHA    = "2222222222222222222222222222222222222222"
	evilSHA  = "3333333333333333333333333333333333333333"
	otherSHA = "4444444444444444444444444444444444444444"
)

func TestCompositeAndJSActions(t *testing.T) {
	skipNoUnixShell(t)
	_, nodeErr := exec.LookPath("node")
	composite := actionTarball(t, "greet", compSHA, []tarFile{
		{name: "action.yml", body: `name: greet
inputs:
  who:
    description: who
    default: world
  loud:
    default: "no"
outputs:
  greeting:
    value: ${{ steps.say.outputs.msg }}
runs:
  using: composite
  steps:
    - id: say
      shell: bash
      run: echo "msg=hello ${{ inputs.who }}" >> "$GITHUB_OUTPUT"
    - shell: bash
      run: |
        echo "in composite: $INPUT_WHO_UNSET ${{ inputs.loud }} parent=$PARENT_ENV"
        test -f "$GITHUB_ACTION_PATH/action.yml" && echo action-path-ok
        echo "COMPOSITE_SET=yes" >> "$GITHUB_ENV"
    - uses: ./nested
      with:
        x: ${{ inputs.who }}
`},
		{name: "nested/action.yml", body: `runs:
  using: composite
  steps:
    - run: echo "nested got ${{ inputs.x }}"
      shell: sh
inputs:
  x: {}
`},
	}, true)
	js := actionTarball(t, "jsact", jsSHA, []tarFile{
		{name: "action.yml", body: `name: js
inputs:
  name:
    default: anon
runs:
  using: node20
  main: dist/index.js
  post: dist/post.js
`},
		{name: "dist/index.js", body: `const fs = require('fs');
const n = process.env.INPUT_NAME;
fs.appendFileSync(process.env.GITHUB_OUTPUT, 'out=js-' + n + '\n');
fs.appendFileSync(process.env.GITHUB_STATE, 'k=state-' + n + '\n');
console.log('js main ran for ' + n);
`},
		{name: "dist/post.js", body: `console.log('post saw ' + process.env.STATE_k);`},
	}, false)
	// wrong commit inside: must be refused
	evil := actionTarball(t, "evil", otherSHA, []tarFile{{name: "action.yml", body: "runs:\n  using: composite\n  steps: []\n"}}, true)
	srv, hits := codeload(t, map[string][]byte{
		"/demi/greet/tar.gz/" + compSHA: composite,
		"/demi/jsact/tar.gz/" + jsSHA:   js,
		"/demi/evil/tar.gz/" + evilSHA:  evil,
	})

	f := newFakeServer(t)
	job := baseJob(6, nil)
	job.Steps = []actions.Step{
		// relative nested uses: ./nested resolves against the workspace, so
		// copy the action there first.
		{Index: 0, Run: `mkdir -p nested && printf 'runs:\n  using: composite\n  steps:\n    - run: echo "nested got ${{ '${{' }} inputs.x }}"\n      shell: sh\ninputs:\n  x: {}\n' > nested/action.yml`},
		{Index: 1, ID: "g", Uses: "demi/greet@" + compSHA, With: map[string]string{"who": "Demi"}, Env: map[string]string{"PARENT_ENV": "pe"}},
		{Index: 2, Run: `echo "composite output: ${{ steps.g.outputs.greeting }} set=$COMPOSITE_SET"`},
		{Index: 3, ID: "g2", Uses: "demi/greet@" + compSHA},
		{Index: 4, Run: `echo "default: ${{ steps.g2.outputs.greeting }}"`},
	}
	if nodeErr == nil {
		job.Steps = append(job.Steps,
			actions.Step{Index: 5, ID: "js", Uses: "demi/jsact@" + jsSHA, With: map[string]string{"name": "demi"}},
			actions.Step{Index: 6, Run: `echo "js output: ${{ steps.js.outputs.out }}"`})
	}
	r := f.testRunner(t)
	r.Config.CodeloadURL = srv.URL
	f.runJobs(t, r, job)
	logs := f.logText(6)
	if st := f.finish(6).Status; st != actions.StatusSuccess {
		t.Fatalf("status = %s\n%s", st, logs)
	}
	for _, want := range []string{"in composite:  no parent=pe", "action-path-ok", "nested got Demi",
		"composite output: hello Demi set=yes", "default: hello world"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q\n%s", want, logs)
		}
	}
	if nodeErr == nil {
		for _, want := range []string{"js main ran for demi", "js output: js-demi", "post saw state-demi"} {
			if !strings.Contains(logs, want) {
				t.Errorf("logs missing %q\n%s", want, logs)
			}
		}
	} else {
		t.Log("node not installed: JavaScript action part skipped")
	}
	if g := f.step(6, 1).Outputs["greeting"]; g != "hello Demi" {
		t.Errorf("composite step output = %q", g)
	}
	// downloaded once per sha (cached), even though greet is used twice
	if n := hits.Load(); nodeErr == nil && n != 2 || nodeErr != nil && n != 1 {
		t.Errorf("codeload hits = %d", n)
	}
	_, cache := r.Config.Dirs()
	if _, err := os.Stat(filepath.Join(cache, "actions", "demi", "greet", compSHA, "action.yml")); err != nil {
		t.Errorf("action not cached: %v", err)
	}

	// a second job reuses the cache
	job2 := baseJob(7, nil)
	job2.Steps = []actions.Step{{Index: 0, Uses: "demi/greet@" + compSHA}}
	before := hits.Load()
	f.runJobs(t, r, job2)
	if hits.Load() != before {
		t.Error("cached action downloaded again")
	}

	// sha mismatch and unpinned refs are refused
	job3 := baseJob(8, nil)
	job3.Steps = []actions.Step{{Index: 0, Uses: "demi/evil@" + evilSHA}}
	job4 := baseJob(9, nil)
	job4.Steps = []actions.Step{{Index: 0, Uses: "demi/greet@v1"}}
	f.runJobs(t, r, job3, job4)
	if fin := f.finish(8); fin.Status != actions.StatusFailure || !strings.Contains(fin.Error, "expected "+evilSHA) {
		t.Errorf("evil action: %+v", fin)
	}
	if fin := f.finish(9); fin.Status != actions.StatusFailure || !strings.Contains(fin.Error, "full commit SHA") {
		t.Errorf("unpinned action: %+v", fin)
	}
}

func TestJSActionNeedsNode(t *testing.T) {
	skipNoUnixShell(t)
	js := actionTarball(t, "jsact", jsSHA, []tarFile{{name: "action.yml", body: "runs:\n  using: node20\n  main: index.js\n"}, {name: "index.js", body: "1"}}, true)
	srv, _ := codeload(t, map[string][]byte{"/demi/jsact/tar.gz/" + jsSHA: js})
	f := newFakeServer(t)
	job := baseJob(10, nil)
	job.Env = map[string]string{"PATH": "/nonexistent"}
	job.Steps = []actions.Step{{Index: 0, Uses: "demi/jsact@" + jsSHA}}
	r := f.testRunner(t)
	r.Config.CodeloadURL = srv.URL
	f.runJobs(t, r, job)
	if !strings.Contains(f.logText(10), "needs Node.js") {
		t.Errorf("logs:\n%s", f.logText(10))
	}
}

func TestDockerCapabilityRequired(t *testing.T) {
	f := newFakeServer(t)
	job := baseJob(11, nil)
	job.Container = &actions.Container{Image: "alpine:3"}
	job.Steps = []actions.Step{{Index: 0, Run: "true"}}
	job2 := baseJob(12, nil)
	job2.Steps = []actions.Step{{Index: 0, Uses: "docker://alpine:3"}}
	f.runJobs(t, f.testRunner(t), job, job2)
	if fin := f.finish(11); fin.Status != actions.StatusFailure || !strings.Contains(fin.Error, "docker capability") {
		t.Errorf("container job: %+v", fin)
	}
	if !strings.Contains(f.logText(12), "docker capability") {
		t.Errorf("docker:// step: %s", f.logText(12))
	}
}

// TestDockerJobs runs real containers; opt in with ATELIER_TEST_DOCKER=1.
func TestDockerJobs(t *testing.T) {
	if os.Getenv("ATELIER_TEST_DOCKER") != "1" {
		t.Skip("set ATELIER_TEST_DOCKER=1 to run docker tests")
	}
	f := newFakeServer(t)
	job := baseJob(13, nil)
	job.Secrets = map[string]string{"TOK": "container-secret-1"}
	job.Container = &actions.Container{Image: "alpine:3", Env: map[string]string{"FROM_CTR": "${{ secrets.TOK }}"}}
	job.Steps = []actions.Step{
		{Index: 0, Run: `echo "in container $(cat /etc/alpine-release) $FROM_CTR"; echo "x=1" >> "$GITHUB_OUTPUT"; pwd`, ID: "a"},
		{Index: 1, Uses: "docker://alpine:3", With: map[string]string{"args": `sh -c "echo docker-step $GITHUB_WORKSPACE"`}},
	}
	f.runJobs(t, f.testRunner(t, actions.CapJobs, actions.CapDocker), job)
	logs := f.logText(13)
	if f.finish(13).Status != actions.StatusSuccess || !strings.Contains(logs, "in container 3.") || !strings.Contains(logs, "docker-step /github/workspace") {
		t.Errorf("logs:\n%s", logs)
	}
	if strings.Contains(logs, "container-secret-1") || f.step(13, 0).Outputs["x"] != "1" {
		t.Errorf("secret leaked or output missing:\n%s", logs)
	}
}

func TestPollBackoffHeartbeatAndParallel(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	f.failPolls = 3
	r := f.testRunner(t)
	r.Parallel = 2
	a := baseJob(20, nil)
	a.Steps = []actions.Step{{Index: 0, Run: "sleep 1; echo a-done"}}
	b := baseJob(21, nil)
	b.Steps = []actions.Step{{Index: 0, Run: "sleep 1; echo b-done"}}
	start := time.Now()
	f.runJobs(t, r, a, b)
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("parallel jobs took %v", d)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.polls) < 4 {
		t.Errorf("polls = %d, want retries after failures", len(f.polls))
	}
	for _, p := range f.polls {
		if p.Version != "test" || len(p.Labels) < 3 || p.Labels[0] != "self-hosted" {
			t.Errorf("poll request %+v", p)
			break
		}
	}
	if f.badAuth != 0 {
		t.Errorf("bad auth calls: %d", f.badAuth)
	}
}

func TestBusyHeartbeat(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	r := f.testRunner(t)
	j := baseJob(22, nil)
	j.Steps = []actions.Step{{Index: 0, Run: "sleep 1"}}
	f.runJobs(t, r, j)
	f.mu.Lock()
	defer f.mu.Unlock()
	busy := 0
	for _, p := range f.polls {
		if p.Busy {
			busy++
		}
	}
	if busy == 0 {
		t.Error("no busy heartbeat while the job ran")
	}
}

func TestShutdownCancelsJob(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	r := f.testRunner(t)
	j := baseJob(23, nil)
	j.Steps = []actions.Step{{Index: 0, Run: "echo long; sleep 30"}, {Index: 1, If: "always()", Run: "echo should-skip-on-shutdown"}}
	f.enqueue(j)
	ctx, cancel := contextWithCancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for !hasLine(f.logText(23), "long") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("runner did not shut down")
	}
	if st := f.finish(23).Status; st != actions.StatusCancelled {
		t.Errorf("status = %q", st)
	}
	if hasLine(f.logText(23), "should-skip-on-shutdown") {
		t.Error("steps ran during shutdown")
	}
}

func TestDiscoveryReported(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "a", "b", "proj")
	gitCmd(t, root, "init", "-q", repo)
	gitCmd(t, repo, "remote", "add", "origin", "https://user:ghp_secret@github.com/demi/proj.git")
	f := newFakeServer(t)
	r := f.testRunner(t, actions.CapDiscovery)
	r.Config.DiscoveryPaths = []string{root}
	ctx, cancel := contextWithCancel()
	go r.Run(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.discovery)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.discovery) == 0 || len(f.discovery[0].Repos) != 1 {
		t.Fatalf("discovery = %+v", f.discovery)
	}
	got := f.discovery[0].Repos[0]
	if got.Remotes["origin"] != "https://github.com/demi/proj.git" || !strings.HasSuffix(got.Path, filepath.Join("a", "b", "proj")) {
		t.Errorf("repo = %+v", got)
	}
	if strings.Contains(fmt.Sprint(f.discovery), "ghp_secret") {
		t.Error("credentials sent")
	}
}

// hasLine reports whether some log line is exactly s.
func hasLine(logs, s string) bool {
	for _, l := range strings.Split(logs, "\n") {
		if l == s {
			return true
		}
	}
	return false
}
