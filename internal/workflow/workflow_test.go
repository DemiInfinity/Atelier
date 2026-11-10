package workflow

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atelier/internal/workflow/expr"
)

const ciYAML = `
name: CI
on:
  push:
    branches: [main, 'release/**', '!release/old']
    paths-ignore: ['docs/**', '*.md']
  pull_request:
    branches: [main]
  workflow_dispatch:
    inputs:
      level:
        description: Log level
        type: choice
        options: [info, debug]
        default: info
      dry:
        type: boolean
        required: true
  schedule:
    - cron: '30 4 * * 1-5'
env:
  GOFLAGS: -mod=readonly
permissions:
  contents: read
  packages: write
concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true
defaults:
  run:
    shell: bash
jobs:
  lint:
    runs-on: [self-hosted, linux]
    steps:
      - uses: actions/checkout@v4
      - run: golangci-lint run
  test:
    name: Test (${{ matrix.os }}, Go ${{ matrix.go }})
    needs: lint
    runs-on: ${{ matrix.os }}
    timeout-minutes: 20
    strategy:
      fail-fast: false
      max-parallel: 2
      matrix:
        os: [linux, macos]
        go: ['1.22', '1.23']
        exclude:
          - os: macos
            go: '1.22'
        include:
          - os: linux
            race: true
          - os: windows
            go: '1.23'
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: ${{ matrix.go }}
      - id: t
        name: Test
        run: go test ${{ matrix.race && '-race' || '' }} ./...
        env:
          TOKEN: ${{ secrets.NPM_TOKEN }}
        continue-on-error: true
  deploy:
    needs: [lint, test]
    if: github.ref == 'refs/heads/main' && success()
    runs-on: linux
    environment: production
    container:
      image: alpine:3.20
      env:
        A: b
    outputs:
      url: ${{ steps.d.outputs.url }}
    steps:
      - id: d
        run: ./deploy.sh
        working-directory: ops
      - uses: ./.atelier/actions/notify
      - uses: docker://alpine:3.20
`

func TestParseCI(t *testing.T) {
	w := Parse(".atelier/workflows/ci.yml", []byte(ciYAML))
	if !w.Valid() {
		t.Fatalf("errors: %v", w.Errors)
	}
	if len(w.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", w.Warnings)
	}
	if w.Name != "CI" || len(w.Jobs) != 3 || w.Env["GOFLAGS"] != "-mod=readonly" {
		t.Fatalf("workflow = %+v", w)
	}
	if !w.Permissions.PackagesWrite() || w.Concurrency == nil || !w.Concurrency.CancelInProgress {
		t.Errorf("permissions/concurrency = %+v %+v", w.Permissions, w.Concurrency)
	}
	if got := strings.Join(w.On.Events(), ","); got != "push,pull_request,workflow_dispatch,schedule,release" &&
		got != "push,pull_request,workflow_dispatch,schedule" {
		t.Errorf("events = %s", got)
	}
	if d := w.On.Dispatch; d == nil || len(d.Inputs) != 2 || d.Inputs[0].Type != "choice" || !d.Inputs[1].Required {
		t.Errorf("dispatch = %+v", d)
	}
	if len(w.On.Schedule) != 1 {
		t.Fatalf("schedule = %+v", w.On.Schedule)
	}
	test := w.Job("test")
	if test.TimeoutMinutes != 20 || test.Needs[0] != "lint" || test.Strategy.FailFast || test.Strategy.MaxParallel != 2 {
		t.Errorf("test job = %+v", test)
	}
	if test.Steps[2].Shell != "bash" || !test.Steps[2].ContinueOnError || test.Steps[2].ID != "t" {
		t.Errorf("step = %+v", test.Steps[2])
	}
	dep := w.Job("deploy")
	if dep.Environment != "production" || dep.Container.Image != "alpine:3.20" || !dep.NeedsDocker() || dep.Outputs["url"] == "" {
		t.Errorf("deploy = %+v", dep)
	}
	if dep.Steps[0].WorkingDirectory != "ops" {
		t.Errorf("working dir = %q", dep.Steps[0].WorkingDirectory)
	}
	uses := strings.Join(w.Uses(), " ")
	if uses != "./.atelier/actions/notify actions/checkout@v4 actions/setup-go@v5 docker://alpine:3.20" {
		t.Errorf("uses = %s", uses)
	}
	if st := Stages(w.Jobs); len(st) != 3 || st[0][0] != "lint" || st[2][0] != "deploy" {
		t.Errorf("stages = %v", st)
	}

	// Matrix: 4 combos − 1 excluded, race added to linux ones, windows appended.
	combos, err := test.Strategy.Matrix.Expand(nil)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, c := range combos {
		labels = append(labels, c.Label())
	}
	want := "linux, 1.22, true|linux, 1.23, true|macos, 1.23|windows, 1.23"
	if got := strings.Join(labels, "|"); got != want {
		t.Errorf("matrix = %s, want %s", got, want)
	}
}

func TestParseWarningsAndErrors(t *testing.T) {
	w := Parse("x.yml", []byte(`
on:
  push:
  issues:
  pull_request_target:
jobs:
  a:
    runs-on: linux
    services:
      db: {image: postgres}
    steps:
      - run: echo hi
        frobnicate: 1
  b:
    uses: org/repo/.github/workflows/x.yml@v1
weird: 1
`))
	if !w.Valid() {
		t.Fatalf("errors: %v", w.Errors)
	}
	joined := strings.Join(w.Warnings, "\n")
	for _, s := range []string{`"issues" trigger`, `"pull_request_target"`, "services", "frobnicate", "Reusable workflows", `"weird"`} {
		if !strings.Contains(joined, s) {
			t.Errorf("warnings missing %q:\n%s", s, joined)
		}
	}
	if w.Job("b").Unsupported == "" {
		t.Error("reusable workflow job should be unsupported")
	}

	for name, src := range map[string]string{
		"yaml":     "on: [push\njobs: {",
		"no on":    "jobs:\n  a:\n    runs-on: x\n    steps: [{run: x}]\n",
		"no jobs":  "on: push\n",
		"cycle":    "on: push\njobs:\n  a: {runs-on: x, needs: b, steps: [{run: x}]}\n  b: {runs-on: x, needs: a, steps: [{run: x}]}\n",
		"missing":  "on: push\njobs:\n  a: {runs-on: x, needs: zz, steps: [{run: x}]}\n",
		"run+uses": "on: push\njobs:\n  a: {runs-on: x, steps: [{run: x, uses: a/b@v1}]}\n",
		"nouses":   "on: push\njobs:\n  a: {runs-on: x, steps: [{uses: justname}]}\n",
		"badcron":  "on: {schedule: [{cron: '99 * * * *'}]}\njobs:\n  a: {runs-on: x, steps: [{run: x}]}\n",
		"badif":    "on: push\njobs:\n  a: {runs-on: x, if: 'a ==', steps: [{run: x}]}\n",
		"no steps": "on: push\njobs:\n  a: {runs-on: x}\n",
		"unclosed": "on: push\njobs:\n  a: {runs-on: x, steps: [{run: 'echo ${{ x'}]}\n",
	} {
		if w := Parse("x.yml", []byte(src)); w.Valid() {
			t.Errorf("%s: expected an error", name)
		}
	}
	if w := Parse("agent.md", []byte("# hi")); !w.Agentic || w.Name != "agent" {
		t.Errorf("agentic = %+v", w)
	}
}

func TestPushAndPullFilters(t *testing.T) {
	w := Parse("ci.yml", []byte(ciYAML))
	push := w.On.Push
	cases := []struct {
		ref     string
		changed []string
		want    bool
	}{
		{"refs/heads/main", []string{"main.go"}, true},
		{"refs/heads/main", []string{"docs/a/b.txt", "README.md"}, false}, // all ignored
		{"refs/heads/main", []string{"docs/x.txt", "cmd/main.go"}, true},
		{"refs/heads/main", nil, true}, // unknown change set
		{"refs/heads/release/1.0", []string{"a.go"}, true},
		{"refs/heads/release/old", []string{"a.go"}, false}, // negated
		{"refs/heads/feature", []string{"a.go"}, false},
		{"refs/tags/v1.0", nil, false}, // only branches asked for
	}
	for _, c := range cases {
		if got := push.MatchPush(c.ref, c.changed); got != c.want {
			t.Errorf("MatchPush(%s, %v) = %v, want %v", c.ref, c.changed, got, c.want)
		}
	}
	tags := &PushTrigger{Tags: []string{"v*", "!v0.*"}}
	if !tags.MatchPush("refs/tags/v1.2", nil) || tags.MatchPush("refs/tags/v0.9", nil) || tags.MatchPush("refs/heads/main", nil) {
		t.Error("tag filters")
	}
	if all := (&PushTrigger{}); !all.MatchPush("refs/heads/x", nil) || !all.MatchPush("refs/tags/y", nil) {
		t.Error("no filters should match everything")
	}
	paths := &PushTrigger{Paths: []string{"src/**", "!src/gen/**"}}
	if !paths.MatchPush("refs/heads/x", []string{"src/a/b.go"}) || paths.MatchPush("refs/heads/x", []string{"src/gen/z.go"}) {
		t.Error("path filters")
	}
	if paths.MatchPush("refs/tags/v1", nil) {
		t.Error("path filters never match tags")
	}
	pr := w.On.PullRequest
	if !pr.MatchPullRequest("opened", "main", nil) || pr.MatchPullRequest("opened", "dev", nil) ||
		!pr.MatchPullRequest("synchronize", "main", nil) {
		t.Error("pull request filters")
	}
	if (&PullRequestTrigger{Types: []string{"opened"}}).MatchPullRequest("synchronize", "main", nil) {
		t.Error("types filter")
	}
	for pat, cases := range map[string]map[string]bool{
		"feature/*":    {"feature/a": true, "feature/a/b": false},
		"feature/**":   {"feature/a/b": true},
		"**/*.go":      {"a.go": true, "x/y/a.go": true, "a.js": false},
		"v[12].[0-9]+": {"v1.0": true, "v2.10": true, "v3.0": false},
		"docs?":        {"doc": true, "docs": true, "docss": false},
	} {
		for s, want := range cases {
			if MatchGlob(pat, s) != want {
				t.Errorf("MatchGlob(%q, %q) != %v", pat, s, want)
			}
		}
	}
}

func TestCron(t *testing.T) {
	loc := time.Local
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	cases := []struct {
		spec, when string
		want       bool
	}{
		{"30 4 * * 1-5", "2026-11-09 04:30", true}, // Monday
		{"30 4 * * 1-5", "2026-11-08 04:30", false},
		{"*/15 * * * *", "2026-11-08 10:45", true},
		{"*/15 * * * *", "2026-11-08 10:46", false},
		{"0 0 1 jan *", "2026-01-01 00:00", true},
		{"0 12 1 * mon", "2026-11-09 12:00", true}, // dom OR dow
		{"0 12 1 * mon", "2026-11-01 12:00", true},
		{"0 12 1 * mon", "2026-11-03 12:00", false},
		{"0 0 * * 7", "2026-11-08 00:00", true}, // 7 = Sunday
		{"5,10-12/2 * * * *", "2026-11-08 01:11", false},
		{"5,10-12/2 * * * *", "2026-11-08 01:12", true},
	}
	for _, c := range cases {
		cr, err := ParseCron(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := cr.Match(at(c.when)); got != c.want {
			t.Errorf("%s at %s = %v, want %v", c.spec, c.when, got, c.want)
		}
	}
	for _, bad := range []string{"* * * *", "60 * * * *", "* * 0 * *", "*/0 * * * *", "a * * * *", "5-1 * * * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("ParseCron(%q) should fail", bad)
		}
	}
}

func TestDynamicMatrix(t *testing.T) {
	m := &Matrix{Expr: "${{ fromJSON(needs.plan.outputs.matrix) }}"}
	e := &expr.Evaluator{Ctx: expr.Context{"needs": map[string]any{"plan": map[string]any{"outputs": map[string]any{
		"matrix": `{"target":["a","b"],"include":[{"target":"c","extra":1}]}`}}}}}
	combos, err := m.Expand(e)
	if err != nil || len(combos) != 3 || combos[2].Values["extra"] != float64(1) {
		t.Fatalf("combos = %+v, %v", combos, err)
	}
	m2 := &Matrix{Axes: []Axis{{Name: "v", Expr: "${{ fromJSON('[1,2]') }}"}}}
	if c, err := m2.Expand(e); err != nil || len(c) != 2 {
		t.Fatalf("axis expr = %+v %v", c, err)
	}
	if _, err := m.Expand(nil); err == nil {
		t.Error("dynamic matrix without evaluator should fail")
	}
	big := &Matrix{Axes: []Axis{{Name: "a", Values: make([]any, 20)}, {Name: "b", Values: make([]any, 20)}}}
	if _, err := big.Expand(nil); err == nil {
		t.Error("400 combos should be rejected")
	}
}

func TestUsesAndLockfile(t *testing.T) {
	for in, want := range map[string]string{
		"actions/checkout@v4":                      "remote actions/checkout@v4",
		"org/repo/sub/dir@v1.2.3":                  "remote org/repo@v1.2.3",
		"atelier:zoe/tools@main":                   "atelier atelier:zoe/tools@main",
		"./local/action":                           "local ",
		"docker://alpine:3":                        "docker ",
		"actions/cache@" + strings.Repeat("a", 40): "remote actions/cache@" + strings.Repeat("a", 40),
	} {
		r, err := ParseUses(in)
		if err != nil {
			t.Errorf("ParseUses(%q): %v", in, err)
			continue
		}
		got := r.Kind + " "
		if r.Kind == UsesRemote || r.Kind == UsesAtelier {
			got += r.LockKey()
		}
		if got != want {
			t.Errorf("ParseUses(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"actions/checkout", "x@v1", "a/b@", "a/../b@v1", "a/b@${{ x }}", "a/b@-x", "docker://"} {
		if _, err := ParseUses(bad); err == nil {
			t.Errorf("ParseUses(%q) should fail", bad)
		}
	}
	if _, err := ParseLockfile([]byte(`{"a/b@v1": "short"}`)); err == nil {
		t.Error("short sha should be rejected")
	}
	l := Lockfile{"b/b@v1": strings.Repeat("b", 40), "a/a@v2": strings.Repeat("a", 40)}
	back, err := ParseLockfile(l.Marshal())
	if err != nil || len(back) != 2 || !strings.HasPrefix(string(l.Marshal()), "{\n  \"a/a@v2\"") {
		t.Errorf("round trip = %v %v\n%s", back, err, l.Marshal())
	}
}

// fakeRemote makes a bare repository under base/owner/repo with a tag and a branch.
func fakeRemote(t *testing.T, base, owner, repo string) (tagSHA, branchSHA string) {
	t.Helper()
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	bare := filepath.Join(base, owner, repo)
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	git(bare, "init", "-q", "--bare", ".")
	work := t.TempDir()
	git(work, "init", "-q", "--initial-branch=main", ".")
	os.WriteFile(filepath.Join(work, "action.yml"), []byte("name: x\n"), 0o644)
	git(work, "add", ".")
	git(work, "commit", "-q", "-m", "one")
	tagSHA = git(work, "rev-parse", "HEAD")
	git(work, "tag", "-a", "v1", "-m", "v1") // annotated: ls-remote must use the peeled commit
	os.WriteFile(filepath.Join(work, "action.yml"), []byte("name: y\n"), 0o644)
	git(work, "commit", "-q", "-am", "two")
	branchSHA = git(work, "rev-parse", "HEAD")
	git(work, "push", "-q", bare, "main", "v1")
	return tagSHA, branchSHA
}

func TestPinWithLsRemote(t *testing.T) {
	base := t.TempDir()
	tagSHA, branchSHA := fakeRemote(t, base, "acme", "tool")
	ls := &LsRemote{Base: "file://" + base}
	locked := strings.Repeat("c", 40)
	lock := Lockfile{"actions/checkout@v4": locked}
	uses := []string{"actions/checkout@v4", "acme/tool@v1", "acme/tool/sub@main", "./local", "docker://x"}
	res := Pin(context.Background(), uses, lock, ls.Resolve, false)
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
	if res.Pins["actions/checkout@v4"] != "actions/checkout@"+locked || res.Tags["actions/checkout@v4"] != "v4" {
		t.Errorf("locked pin = %q", res.Pins["actions/checkout@v4"])
	}
	if res.Pins["acme/tool@v1"] != "acme/tool@"+tagSHA {
		t.Errorf("tag pin = %q, want peeled %s", res.Pins["acme/tool@v1"], tagSHA)
	}
	if res.Pins["acme/tool/sub@main"] != "acme/tool/sub@"+branchSHA {
		t.Errorf("branch pin = %q", res.Pins["acme/tool/sub@main"])
	}
	if res.Pins["./local"] != "./local" || res.Pins["docker://x"] != "docker://x" {
		t.Error("local and docker pass through")
	}
	if len(res.Resolved) != 2 || len(res.Generated) != 3 {
		t.Errorf("resolved = %v generated = %v", res.Resolved, res.Generated)
	}
	// Requiring the lockfile turns missing entries into errors.
	res = Pin(context.Background(), uses, lock, ls.Resolve, true)
	if len(res.Errors) != 2 {
		t.Errorf("require lock errors = %v", res.Errors)
	}
	// Unknown refs fail clearly.
	res = Pin(context.Background(), []string{"acme/tool@v9"}, nil, ls.Resolve, false)
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "v9") {
		t.Errorf("missing ref errors = %v", res.Errors)
	}
}
