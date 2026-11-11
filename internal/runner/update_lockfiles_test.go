package runner

import (
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"atelier/internal/actions"
	"atelier/internal/workflow"
)

func TestUpdateLockfilesBranchRestriction(t *testing.T) {
	if !isUpdateLockfiles("atelier/update-lockfiles@v1") || !isUpdateLockfiles(" Atelier/Update-Lockfiles@main ") || isUpdateLockfiles("atelier/update-lockfiles-x@v1") {
		t.Error("isUpdateLockfiles")
	}
	for ref, ok := range map[string]bool{
		"refs/heads/atelier-updates/npm/left-pad/2.0.0":       true,
		"refs/heads/atelier-updates/gradle@android/_group/ab": true,
		"refs/heads/main":                      false,
		"refs/heads/feature/atelier-updates/x": false,
		"refs/tags/atelier-updates/x":          false,
		"refs/pull/1/head":                     false,
		"":                                     false,
		"refs/heads/atelier-updates/":          false,
	} {
		if branch, _ := lockfileBranch(ref); (branch != "") != ok {
			t.Errorf("lockfileBranch(%q) = %q", ref, branch)
		}
	}
	r, err := workflow.ParseUses("atelier/update-lockfiles@v1")
	if err != nil || !r.IsBuiltin() {
		t.Errorf("update-lockfiles isn't a built-in action: %+v %v", r, err)
	}
}

// TestUpdateLockfilesEndToEnd runs a job with a fake npm on PATH: on an
// update branch the step commits the regenerated lockfile (and nothing
// else) and pushes it; on main it does nothing.
func TestUpdateLockfilesEndToEnd(t *testing.T) {
	skipNoUnixShell(t)
	const token = "atl_job_token_lockfiles_123456"
	const branch = "atelier-updates/npm/left-pad/2.0.0"
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "repos", "demi", "app.git")
	os.MkdirAll(work, 0o755)
	gitCmd(t, root, "init", "-q", "--bare", "-b", "main", bare)
	gitCmd(t, bare, "config", "http.receivepack", "true")
	gitCmd(t, root, "init", "-q", "-b", "main", work)
	os.WriteFile(filepath.Join(work, "package.json"), []byte(`{"dependencies":{"left-pad":"^1.2.0"}}`), 0o644)
	os.WriteFile(filepath.Join(work, "package-lock.json"), []byte(`{"lockfileVersion":3,"left-pad":"1.2.0"}`), 0o644)
	gitCmd(t, work, "add", ".")
	gitCmd(t, work, "commit", "-q", "-m", "init")
	mainSHA := gitCmd(t, work, "rev-parse", "HEAD")
	gitCmd(t, work, "checkout", "-q", "-b", branch)
	os.WriteFile(filepath.Join(work, "package.json"), []byte(`{"dependencies":{"left-pad":"^2.0.0"}}`), 0o644)
	gitCmd(t, work, "commit", "-q", "-am", "Bump left-pad")
	bumpSHA := gitCmd(t, work, "rev-parse", "HEAD")
	gitCmd(t, work, "push", "-q", bare, "main", branch)

	execPath := gitCmd(t, root, "--exec-path")
	backend := &cgi.Handler{Path: filepath.Join(execPath, "git-http-backend"), Root: "/git",
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "repos"), "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "atelier" || p != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	// A fake npm that rewrites the lockfile and also scribbles on a source
	// file, which must not be committed.
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\necho \"npm $*\"\nprintf '{\"lockfileVersion\":3,\"left-pad\":\"2.0.0\"}' > package-lock.json\necho junk > scribble.js\n"), 0o755)

	job := func(id int64, ref, sha string) *JobSpec {
		j := baseJob(id, nil)
		j.JobToken, j.CloneURL, j.Ref, j.SHA = token, srv.URL+"/git/demi/app.git", ref, sha
		j.Steps = []actions.Step{
			{Index: 0, Uses: "actions/checkout@" + fakeSHA},
			{Index: 1, Run: `echo "` + bin + `" >> "$GITHUB_PATH"`},
			{Index: 2, ID: "lock", Uses: "atelier/update-lockfiles@v1"},
			{Index: 3, Run: `echo "pushed=${{ steps.lock.outputs.pushed }}"`},
		}
		return j
	}
	f := newFakeServer(t)
	r := f.testRunner(t)
	f.runJobs(t, r, job(1, "refs/heads/"+branch, bumpSHA), job(2, "refs/heads/main", mainSHA))

	logs := f.logText(1)
	if fin := f.finish(1); fin.Status != actions.StatusSuccess {
		t.Fatalf("update-branch job = %q\n%s", fin.Status, logs)
	}
	for _, want := range []string{"npm install --package-lock-only --ignore-scripts", "Pushed updated lockfiles to " + branch, "pushed=true"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, token) {
		t.Error("the job token leaked into the logs")
	}
	tip := gitCmd(t, bare, "rev-parse", "refs/heads/"+branch)
	if parent := gitCmd(t, bare, "rev-parse", tip+"^"); parent != bumpSHA {
		t.Errorf("lockfile commit's parent = %s, want the bump %s", parent, bumpSHA)
	}
	if files := gitCmd(t, bare, "diff-tree", "--no-commit-id", "--name-only", "-r", tip); files != "package-lock.json" {
		t.Errorf("lockfile commit changed %q", files)
	}
	if who := gitCmd(t, bare, "log", "-1", "--format=%an <%ae> %s", tip); who != "atelier-updates <atelier-updates@atelier.invalid> Update lockfiles" {
		t.Errorf("lockfile commit = %q", who)
	}
	if lock := gitCmd(t, bare, "show", tip+":package-lock.json"); !strings.Contains(lock, `"2.0.0"`) {
		t.Errorf("pushed lockfile = %s", lock)
	}

	logs2 := f.logText(2)
	if fin := f.finish(2); fin.Status != actions.StatusSuccess || !strings.Contains(logs2, "isn't an atelier-updates/* branch") ||
		!strings.Contains(logs2, "pushed=false") || strings.Contains(logs2, "npm install") {
		t.Errorf("main job = %q\n%s", fin.Status, logs2)
	}
	if got := gitCmd(t, bare, "rev-parse", "refs/heads/main"); got != mainSHA {
		t.Error("main moved")
	}
}
