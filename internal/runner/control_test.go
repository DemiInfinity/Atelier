//go:build !windows

package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/actions"
)

// ctlDir is a short temp folder: a Unix socket path is limited to about 100
// bytes, which t.TempDir() can exceed on macOS.
func ctlDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "atl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// ctlRunner is a registered runner whose config is saved in a short folder,
// with its control channel started.
func ctlRunner(t *testing.T, f *fakeServer, caps ...string) (*Runner, string) {
	t.Helper()
	r := f.testRunner(t, caps...)
	cfgPath := filepath.Join(ctlDir(t), "config.json")
	if err := r.Config.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	r.ConfigPath = cfgPath
	stop, err := r.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return r, cfgPath
}

// startRun runs the runner in the background until the test ends.
func startRun(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("runner did not stop")
		}
	})
}

type ctlReply map[string]any

func (m ctlReply) ok() bool { b, _ := m["ok"].(bool); return b }
func (m ctlReply) errText() string {
	s, _ := m["error"].(string)
	return s
}

// ctlRaw sends one request line and returns the raw reply line.
func ctlRaw(t *testing.T, cfgPath, req string) string {
	t.Helper()
	raw, err := ControlRequest(cfgPath, json.RawMessage(req), 20*time.Second)
	if err != nil {
		t.Fatalf("control %s: %v", req, err)
	}
	return string(raw)
}

func ctl(t *testing.T, cfgPath, req string) ctlReply {
	t.Helper()
	var m ctlReply
	raw := ctlRaw(t, cfgPath, req)
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("control %s: reply %q: %v", req, raw, err)
	}
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func current(m ctlReply) []map[string]any {
	var out []map[string]any
	list, _ := m["current"].([]any)
	for _, v := range list {
		if j, ok := v.(map[string]any); ok {
			out = append(out, j)
		}
	}
	return out
}

// waitFinish waits until the server has the job's result.
func waitFinish(t *testing.T, f *fakeServer, id int64) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		f.mu.Lock()
		_, ok := f.finished[id]
		f.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d did not finish", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readCfg(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", fi.Mode().Perm())
	}
	return m
}

// waitFileJob is a job that prints a line and then waits for a file.
func waitFileJob(id int64, gate string) *JobSpec {
	j := baseJob(id, nil)
	j.Steps = []actions.Step{{Index: 0, Name: "Wait for the gate",
		Run: `echo started-waiting; n=0; while [ ! -f "` + gate + `" ] && [ $n -lt 600 ]; do sleep 0.05; n=$((n+1)); done; echo gate-open`}}
	return j
}

func TestControlStatus(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	r, cfg := ctlRunner(t, f)

	// before the first poll: not online, nothing running
	st := ctl(t, cfg, `{"op":"status"}`)
	if !st.ok() || st["online"] != false || st["name"] != "test-runner" || st["version"] != "test" || st["server"] != f.srv.URL {
		t.Errorf("status before run = %v", st)
	}
	startRun(t, r)
	waitFor(t, "online", func() bool { return ctl(t, cfg, `{"op":"status"}`)["online"] == true })

	gate := filepath.Join(ctlDir(t), "gate")
	f.enqueue(waitFileJob(41, gate))
	waitFor(t, "the job to show its step", func() bool {
		c := current(ctl(t, cfg, `{"op":"status"}`))
		return len(c) == 1 && c[0]["detail"] == "Wait for the gate"
	})
	raw := ctlRaw(t, cfg, `{"op":"status"}`)
	for _, secret := range []string{f.runnerTok, "atl_job_token_abcdef123456"} {
		if strings.Contains(raw, secret) {
			t.Errorf("status leaks %q: %s", secret, raw)
		}
	}
	var got ControlStatus
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || !got.TakingJobs || got.PausedReason != nil || !got.Online {
		t.Errorf("status = %s", raw)
	}
	if !strings.Contains(raw, `"paused_reason":null`) {
		t.Errorf("paused_reason should be null: %s", raw)
	}
	if !slices.Equal(got.Capabilities, []string{"jobs", "agents"}) {
		t.Errorf("capabilities = %v", got.Capabilities)
	}
	if got.Rules != (ControlRulesStatus{PauseOnBattery: false, MaxParallel: 1, ContainerJobs: false}) {
		t.Errorf("rules = %+v", got.Rules)
	}
	j := got.Current[0]
	if j.JobID != 41 || j.Repo != "demi/app" || j.Workflow != "ci" || j.Kind != "job" || j.StartedAt.IsZero() || time.Since(j.StartedAt) > time.Minute {
		t.Errorf("current = %+v", j)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFinish(t, f, 41)
	waitFor(t, "the job to leave status", func() bool { return len(current(ctl(t, cfg, `{"op":"status"}`))) == 0 })
	if raw := ctlRaw(t, cfg, `{"op":"status"}`); !strings.Contains(raw, `"current":[]`) {
		t.Errorf("idle status = %s", raw)
	}
}

func TestControlPauseKeepsRunningJob(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	r, cfg := ctlRunner(t, f)
	startRun(t, r)
	gate := filepath.Join(ctlDir(t), "gate")
	f.enqueue(waitFileJob(50, gate))
	waitFor(t, "job 50 to run", func() bool { return len(current(ctl(t, cfg, `{"op":"status"}`))) == 1 })

	st := ctl(t, cfg, `{"op":"set","taking_jobs":false}`)
	if !st.ok() || st["taking_jobs"] != false || st["paused_reason"] != "user" || len(current(st)) != 1 {
		t.Fatalf("set reply = %v", st)
	}
	if c := readCfg(t, cfg); c["paused"] != true || c["runner_token"] != f.runnerTok {
		t.Errorf("config after pause = %v", c)
	}

	// the running job finishes although the runner is paused
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFinish(t, f, 50)
	if s := f.finish(50).Status; s != actions.StatusSuccess {
		t.Errorf("job 50 = %s\n%s", s, f.logText(50))
	}

	// a new job is not picked up while paused
	next := baseJob(51, nil)
	next.Steps = []actions.Step{{Index: 0, Run: "echo ran-51"}}
	f.mu.Lock()
	pollsBefore := len(f.polls)
	f.mu.Unlock()
	f.enqueue(next)
	waitFor(t, "a few paused heartbeats", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.polls) >= pollsBefore+3
	})
	f.mu.Lock()
	queued := len(f.queue)
	for _, p := range f.polls[pollsBefore:] {
		if !p.Busy {
			t.Error("a paused runner asked for a job")
		}
	}
	f.mu.Unlock()
	if queued != 1 || f.logText(51) != "" {
		t.Fatalf("job 51 was picked up while paused (queue %d)", queued)
	}
	if st := ctl(t, cfg, `{"op":"status"}`); st["taking_jobs"] != false || len(current(st)) != 0 {
		t.Errorf("paused status = %v", st)
	}

	// resume: the job runs
	st = ctl(t, cfg, `{"op":"set","taking_jobs":true}`)
	if st["taking_jobs"] != true || st["paused_reason"] != nil {
		t.Errorf("resume reply = %v", st)
	}
	waitFinish(t, f, 51)
	if !strings.Contains(f.logText(51), "ran-51") {
		t.Errorf("job 51 logs:\n%s", f.logText(51))
	}
	if c := readCfg(t, cfg); c["paused"] != nil {
		t.Errorf("config after resume = %v", c)
	}
}

// A pause set in the config file holds from the start.
func TestControlPausedFromConfig(t *testing.T) {
	f := newFakeServer(t)
	r := f.testRunner(t)
	r.Config.Paused = true
	startRun(t, r)
	j := baseJob(55, nil)
	j.Steps = []actions.Step{{Index: 0, Run: "echo hi"}}
	f.enqueue(j)
	waitFor(t, "heartbeats", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.polls) >= 3
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) != 1 {
		t.Error("a runner paused in its config took a job")
	}
}

func TestControlRulesPersisted(t *testing.T) {
	f := newFakeServer(t)
	r, cfg := ctlRunner(t, f)
	onBatt := false
	var mu sync.Mutex
	old := batteryProbe
	batteryProbe = func() bool { mu.Lock(); defer mu.Unlock(); return onBatt }
	defer func() { batteryProbe = old }()

	// another command changes the file while the runner runs: it must survive
	disk, err := LoadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	disk.ClaudeCodePath = "/opt/claude"
	disk.Capabilities = append(disk.Capabilities, actions.CapWorkspaces)
	if err := disk.Save(cfg); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(cfg, 0o644) // and a sloppy mode is tightened

	st := ctl(t, cfg, `{"op":"set","rules":{"max_parallel":3,"container_jobs":true,"pause_on_battery":true}}`)
	rules, _ := st["rules"].(map[string]any)
	if !st.ok() || rules["max_parallel"] != float64(3) || rules["container_jobs"] != true || rules["pause_on_battery"] != true {
		t.Fatalf("set reply = %v", st)
	}
	if st["taking_jobs"] != true || st["paused_reason"] != nil {
		t.Errorf("on mains the rule shouldn't pause: %v", st)
	}
	caps, _ := st["capabilities"].([]any)
	if !slices.Contains(caps, any("docker")) {
		t.Errorf("capabilities = %v", caps)
	}
	c := readCfg(t, cfg)
	diskCaps, _ := c["capabilities"].([]any)
	if c["parallel"] != float64(3) || c["pause_on_battery"] != true || c["runner_token"] != f.runnerTok ||
		c["claude_code_path"] != "/opt/claude" || c["server_url"] != f.srv.URL || c["name"] != "test-runner" ||
		!slices.Contains(diskCaps, any("docker")) || !slices.Contains(diskCaps, any("workspaces")) || !slices.Contains(diskCaps, any("jobs")) {
		t.Errorf("config = %v", c)
	}
	if r.maxParallel() != 3 || !r.Config.HasCap(actions.CapDocker) {
		t.Errorf("running config: parallel %d, docker %v", r.maxParallel(), r.Config.HasCap(actions.CapDocker))
	}

	// on battery with the rule on: paused, reason "battery"
	mu.Lock()
	onBatt = true
	mu.Unlock()
	r.ctl.mu.Lock()
	r.ctl.battAt = time.Time{}
	r.ctl.mu.Unlock()
	if st := ctl(t, cfg, `{"op":"status"}`); st["taking_jobs"] != false || st["paused_reason"] != "battery" {
		t.Errorf("on battery = %v", st)
	}
	if !r.busy() {
		t.Error("the poll loop would still take jobs on battery")
	}
	// a pause by the user wins as the reason
	if st := ctl(t, cfg, `{"op":"set","taking_jobs":false}`); st["paused_reason"] != "user" {
		t.Errorf("paused by user on battery = %v", st)
	}
	ctl(t, cfg, `{"op":"set","taking_jobs":true}`)
	// rule off: taking jobs again
	st = ctl(t, cfg, `{"op":"set","rules":{"pause_on_battery":false,"container_jobs":false}}`)
	if st["taking_jobs"] != true || st["paused_reason"] != nil {
		t.Errorf("rule off = %v", st)
	}
	c = readCfg(t, cfg)
	diskCaps, _ = c["capabilities"].([]any)
	if c["pause_on_battery"] != nil || slices.Contains(diskCaps, any("docker")) || c["parallel"] != float64(3) || !slices.Contains(diskCaps, any("workspaces")) {
		t.Errorf("config after rule off = %v", c)
	}

	// bad values change nothing
	for _, req := range []string{
		`{"op":"set"}`,
		`{"op":"set","rules":{}}`,
		`{"op":"set","rules":{"max_parallel":0}}`,
		`{"op":"set","rules":{"max_parallel":1000}}`,
		`{"op":"set","rules":{"max_parallel":"two"}}`,
		`{"op":"set","taking_jobs":"no"}`,
	} {
		if st := ctl(t, cfg, req); st.ok() || st.errText() == "" {
			t.Errorf("%s accepted: %v", req, st)
		}
	}
	if c := readCfg(t, cfg); c["parallel"] != float64(3) || c["paused"] != nil {
		t.Errorf("config after bad requests = %v", c)
	}
}

// The docker capability switched on over the channel is advertised on the next poll.
func TestControlContainerJobsAdvertised(t *testing.T) {
	f := newFakeServer(t)
	r, cfg := ctlRunner(t, f)
	startRun(t, r)
	waitFor(t, "a poll", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.polls) > 0 })
	ctl(t, cfg, `{"op":"set","rules":{"container_jobs":true}}`)
	waitFor(t, "docker in a poll", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.polls[len(f.polls)-1].Capabilities, actions.CapDocker)
	})
}

func TestControlMaxParallel(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	r, cfg := ctlRunner(t, f)
	startRun(t, r)
	gate := filepath.Join(ctlDir(t), "gate")
	f.enqueue(waitFileJob(60, gate))
	f.enqueue(waitFileJob(61, gate))
	waitFor(t, "one job", func() bool { return len(current(ctl(t, cfg, `{"op":"status"}`))) == 1 })
	time.Sleep(500 * time.Millisecond)
	if n := len(current(ctl(t, cfg, `{"op":"status"}`))); n != 1 {
		t.Fatalf("%d jobs at once with max_parallel 1", n)
	}
	ctl(t, cfg, `{"op":"set","rules":{"max_parallel":2}}`)
	waitFor(t, "two jobs at once", func() bool { return len(current(ctl(t, cfg, `{"op":"status"}`))) == 2 })
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFinish(t, f, 60)
	waitFinish(t, f, 61)
}

func TestControlTailMasked(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	r, cfg := ctlRunner(t, f)
	startRun(t, r)
	gate := filepath.Join(ctlDir(t), "gate")
	const secret = "hunter2-very-secret-value"
	j := baseJob(70, nil)
	j.Secrets = map[string]string{"DEPLOY_KEY": secret}
	j.Steps = []actions.Step{{Index: 0, Name: "Leak", Env: map[string]string{"K": "${{ secrets.DEPLOY_KEY }}", "T": "${{ secrets.GITHUB_TOKEN }}"},
		Run: `echo "key=$K"; echo "token=$T"; echo "runner=` + f.runnerTok + `"; for i in 1 2 3 4 5; do echo "line $i"; done; while [ ! -f "` + gate + `" ]; do sleep 0.05; done`}}
	f.enqueue(j)
	var lines []string
	waitFor(t, "the log lines", func() bool {
		m := ctl(t, cfg, `{"op":"tail","job_id":70,"lines":200}`)
		lines = nil
		got, _ := m["lines"].([]any)
		for _, l := range got {
			lines = append(lines, l.(string))
		}
		return slices.Contains(lines, "line 5")
	})
	all := strings.Join(lines, "\n")
	for _, s := range []string{secret, j.JobToken, f.runnerTok} {
		if strings.Contains(all, s) {
			t.Errorf("tail leaks %q:\n%s", s, all)
		}
	}
	for _, want := range []string{"key=***", "token=***", "runner=***", "line 1"} {
		if !slices.Contains(lines, want) {
			t.Errorf("tail lacks %q:\n%s", want, all)
		}
	}
	// the last N, oldest first; 50 by default
	m := ctl(t, cfg, `{"op":"tail","job_id":70,"lines":2}`)
	if got := m["lines"].([]any); len(got) != 2 || got[0] != "line 4" || got[1] != "line 5" {
		t.Errorf("tail 2 = %v", got)
	}
	if m := ctl(t, cfg, `{"op":"tail","job_id":70}`); !m.ok() || len(m["lines"].([]any)) != len(lines) {
		t.Errorf("default tail = %v", m)
	}
	if raw := ctlRaw(t, cfg, `{"op":"tail","job_id":70,"lines":0}`); raw != `{"ok":true,"lines":[]}` {
		t.Errorf("tail 0 = %s", raw)
	}
	if m := ctl(t, cfg, `{"op":"tail","job_id":999}`); m.ok() || !strings.Contains(m.errText(), "999") {
		t.Errorf("tail of an unknown job = %v", m)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFinish(t, f, 70)
	waitFor(t, "the job to go", func() bool { return !ctl(t, cfg, `{"op":"tail","job_id":70}`).ok() })
}

func TestControlTailRing(t *testing.T) {
	l := &liveJob{mask: NewMasker()}
	for i := range controlTailKeep + 25 {
		l.addLine("n" + strings.Repeat("x", i%3) + "-" + string(rune('a'+i%26)))
	}
	l.addLine(strings.Repeat("é", controlTailLine)) // clipped on a rune boundary
	all := l.tail(controlTailKeep * 2)
	if len(all) != controlTailKeep {
		t.Fatalf("kept %d lines", len(all))
	}
	last := all[len(all)-1]
	if len(last) > controlTailLine+3 || !strings.HasSuffix(last, "…") || strings.ContainsRune(last, '\uFFFD') {
		t.Errorf("long line kept as %d bytes", len(last))
	}
	if got := l.tail(1); len(got) != 1 || got[0] != last {
		t.Errorf("tail(1) = %v", got)
	}
	var nilJob *liveJob
	nilJob.addLine("x") // jobs run without the channel in other tests
	nilJob.setDetail("x")
}

func TestControlCancel(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	r, cfg := ctlRunner(t, f)
	startRun(t, r)
	j := baseJob(80, nil)
	j.Steps = []actions.Step{
		{Index: 0, Run: `echo started-sleep; sleep 30; echo woke-up`},
		{Index: 1, Run: `echo next-step`},
		{Index: 2, If: "cancelled()", Run: `echo cleanup-ran status=${{ job.status }}`},
	}
	f.enqueue(j)
	waitFor(t, "the step to start", func() bool { return strings.Contains(f.logText(80), "started-sleep") })
	if m := ctl(t, cfg, `{"op":"cancel","job_id":81}`); m.ok() {
		t.Errorf("cancel of an unknown job = %v", m)
	}
	start := time.Now()
	if raw := ctlRaw(t, cfg, `{"op":"cancel","job_id":80}`); raw != `{"ok":true}` {
		t.Fatalf("cancel = %s", raw)
	}
	waitFinish(t, f, 80)
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("cancel took %v", d)
	}
	logs := f.logText(80)
	if st := f.finish(80).Status; st != actions.StatusCancelled {
		t.Errorf("status = %s\n%s", st, logs)
	}
	if hasLine(logs, "woke-up") || hasLine(logs, "next-step") || !strings.Contains(logs, "cleanup-ran status=cancelled") {
		t.Errorf("not cancelled like a server-side cancel:\n%s", logs)
	}
	// the runner goes on to the next job
	n := baseJob(82, nil)
	n.Steps = []actions.Step{{Index: 0, Run: "echo after"}}
	f.enqueue(n)
	waitFinish(t, f, 82)
}

// An agent job shows kind "agent", its workflow name and its turn; cancel stops it.
func TestControlAgentJob(t *testing.T) {
	f := newFakeServer(t)
	m := newFakeMCP(t, testJobToken, "")
	claude := newFakeClaude(t, fakeTurn{Hang: true})
	oldBase := agentAPIBaseURL
	agentAPIBaseURL = func() string { return claude.srv.URL }
	defer func() { agentAPIBaseURL = oldBase }()

	r, cfg := ctlRunner(t, f)
	startRun(t, r)
	job := baseJob(90, nil)
	job.JobToken = testJobToken
	job.Workflow = ".atelier/workflows/review.md"
	job.Secrets = map[string]string{"ANTHROPIC_API_KEY": testAPIKey}
	step := testStep(m.srv.URL, "pulls.get")
	job.Steps = []actions.Step{{Index: 0, ID: "agent", Name: "Run Claude agent", With: map[string]string{"api-key": "${{ secrets.ANTHROPIC_API_KEY }}"}, Agent: &step}}
	f.enqueue(job)
	var c []map[string]any
	waitFor(t, "the agent's first turn", func() bool {
		c = current(ctl(t, cfg, `{"op":"status"}`))
		return len(c) == 1 && c[0]["detail"] == "turn 1 of 10"
	})
	if c[0]["kind"] != "agent" || c[0]["workflow"] != "Review helper" || c[0]["job_id"] != float64(90) {
		t.Errorf("current = %v", c[0])
	}
	raw := ctlRaw(t, cfg, `{"op":"status"}`) + ctlRaw(t, cfg, `{"op":"tail","job_id":90,"lines":500}`)
	for _, s := range []string{testAPIKey, testJobToken, f.runnerTok} {
		if strings.Contains(raw, s) {
			t.Errorf("agent job leaks %q: %s", s, raw)
		}
	}
	if !ctl(t, cfg, `{"op":"cancel","job_id":90}`).ok() {
		t.Fatal("cancel failed")
	}
	waitFinish(t, f, 90)
	if st := f.finish(90).Status; st != actions.StatusCancelled {
		t.Errorf("status = %s\n%s", st, f.logText(90))
	}
}

func TestControlWorkspaces(t *testing.T) {
	skipNoUnixShell(t)
	wsGitEnv(t)
	f := newFakeServer(t)
	_, cfg := ctlRunner(t, f)
	old := WorkspaceConfigPath
	WorkspaceConfigPath = ""
	defer func() { WorkspaceConfigPath = old }()

	root := filepath.Join(t.TempDir(), "ws")
	repo := filepath.Join(root, "DemiInfinity", "Atelier")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	wsRun(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsRun(t, repo, "add", ".")
	wsRun(t, repo, "commit", "-q", "-m", "one")
	_ = os.MkdirAll(filepath.Join(root, "DemiInfinity", "not-a-repo"), 0o755)
	wc := &WorkspaceConfig{Root: root, Presets: map[string]string{"vscode": "code {path}", "fork": "open -a Fork {path}"}}
	if err := wc.Save(filepath.Join(filepath.Dir(cfg), "workspaces.json")); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(root)

	raw := ctlRaw(t, cfg, `{"op":"workspaces"}`)
	var got struct {
		OK      bool
		Root    string
		Presets []string
		Items   []map[string]any
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Root != real || !slices.Equal(got.Presets, []string{"fork", "vscode"}) || len(got.Items) != 1 {
		t.Fatalf("workspaces = %s", raw)
	}
	it := got.Items[0]
	if it["repo"] != "DemiInfinity/Atelier" || it["path"] != filepath.Join(real, "DemiInfinity", "Atelier") || it["branch"] != "main" || it["dirty"] != false {
		t.Errorf("item = %v", it)
	}
	if strings.Contains(raw, "code {path}") || strings.Contains(raw, "open -a") {
		t.Errorf("workspaces returns preset commands: %s", raw)
	}
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if raw := ctlRaw(t, cfg, `{"op":"workspaces"}`); !strings.Contains(raw, `"dirty":true`) {
		t.Errorf("dirty workspace = %s", raw)
	}

	// nothing configured: defaults, empty lists
	if err := os.Remove(filepath.Join(filepath.Dir(cfg), "workspaces.json")); err != nil {
		t.Fatal(err)
	}
	raw = ctlRaw(t, cfg, `{"op":"workspaces"}`)
	if !strings.Contains(raw, `"presets":[]`) || !strings.Contains(raw, `"items":[]`) || !strings.Contains(raw, `"ok":true`) {
		t.Errorf("default workspaces = %s", raw)
	}
}

func TestControlSocketPermissionsAndCleanup(t *testing.T) {
	f := newFakeServer(t)
	r := f.testRunner(t)
	cfg := filepath.Join(ctlDir(t), "config.json")
	if err := r.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := ControlRequest(cfg, map[string]string{"op": "status"}, time.Second); !errors.Is(err, ErrNotRunning) {
		t.Errorf("before start: %v", err)
	}
	if _, err := r.StartControl(); err == nil {
		t.Fatal("started without a config path")
	}
	r.ConfigPath = cfg
	stop, err := r.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(filepath.Dir(cfg), "control.sock")
	if ControlAddress(cfg) != sock {
		t.Errorf("address = %s", ControlAddress(cfg))
	}
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %v, want a socket with 0600", fi.Mode())
	}
	if !ctl(t, cfg, `{"op":"status"}`).ok() {
		t.Error("no status")
	}

	// a second runner on the same config is refused and doesn't disturb the first
	r2 := &Runner{Config: r.Config, Client: r.Client, ConfigPath: cfg, Log: r.Log}
	if _, err := r2.StartControl(); !errors.Is(err, errControlInUse) {
		t.Errorf("second start: %v", err)
	}
	if !ctl(t, cfg, `{"op":"status"}`).ok() {
		t.Error("the first channel stopped answering")
	}

	// an idle client doesn't hold up the stop
	idle, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	done := make(chan struct{})
	go func() { stop(); stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop hung on an idle client")
	}
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
	if _, err := ControlRequest(cfg, map[string]string{"op": "status"}, time.Second); !errors.Is(err, ErrNotRunning) {
		t.Errorf("after stop: %v", err)
	}
}

func TestControlStaleSocket(t *testing.T) {
	f := newFakeServer(t)
	r := f.testRunner(t)
	cfg := filepath.Join(ctlDir(t), "config.json")
	if err := r.Config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	r.ConfigPath = cfg
	sock := ControlAddress(cfg)

	// a socket file left by a runner that died: nothing answers on it
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal("no stale socket to test with")
	}
	if _, err := ControlRequest(cfg, map[string]string{"op": "status"}, time.Second); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stale socket: %v", err)
	}
	stop, err := r.StartControl()
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	if !ctl(t, cfg, `{"op":"status"}`).ok() {
		t.Error("no status after replacing a stale socket")
	}
	stop()

	// something else at that path is not removed
	if err := os.WriteFile(sock, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.StartControl(); err == nil {
		t.Fatal("started over a regular file")
	}
	if b, _ := os.ReadFile(sock); string(b) != "mine" {
		t.Error("a regular file at the socket path was changed")
	}
}

func TestControlTwoClients(t *testing.T) {
	f := newFakeServer(t)
	_, cfg := ctlRunner(t, f)
	sock := ControlAddress(cfg)
	dial := func() (net.Conn, *bufio.Reader) {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		return c, bufio.NewReader(c)
	}
	ask := func(c net.Conn, br *bufio.Reader, req string) ctlReply {
		if _, err := c.Write([]byte(req + "\n")); err != nil {
			t.Fatal(err)
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("%s: %v", req, err)
		}
		var m ctlReply
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("%s: %q", req, line)
		}
		return m
	}
	// two connections open at once, several requests on each, interleaved
	a, ar := dial()
	b, br := dial()
	if !ask(a, ar, `{"op":"status"}`).ok() || !ask(b, br, `{"op":"status"}`).ok() {
		t.Fatal("no status")
	}
	if m := ask(b, br, `{"op":"set","taking_jobs":false}`); m["taking_jobs"] != false {
		t.Errorf("set on b = %v", m)
	}
	if m := ask(a, ar, `{"op":"status"}`); m["taking_jobs"] != false || m["paused_reason"] != "user" {
		t.Errorf("a doesn't see b's change: %v", m)
	}
	// two requests written at once are answered in order
	if _, err := a.Write([]byte("{\"op\":\"status\"}\n{\"op\":\"tail\",\"job_id\":1}\n")); err != nil {
		t.Fatal(err)
	}
	l1, _ := ar.ReadString('\n')
	l2, _ := ar.ReadString('\n')
	if !strings.Contains(l1, `"name"`) || !strings.Contains(l2, `"ok":false`) {
		t.Errorf("pipelined replies: %q %q", l1, l2)
	}

	// many clients in parallel, some changing settings
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 5 {
				req := map[string]any{"op": "status"}
				if i%3 == 0 {
					req = map[string]any{"op": "set", "taking_jobs": i%2 == 0, "rules": map[string]any{"max_parallel": i%4 + 1}}
				}
				raw, err := ControlRequest(cfg, req, 20*time.Second)
				if err != nil || !strings.Contains(string(raw), `"ok":true`) {
					errs <- string(raw) + " " + errString(err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	c := readCfg(t, cfg)
	if c["runner_token"] != f.runnerTok || c["parallel"] == nil {
		t.Errorf("config after concurrent sets = %v", c)
	}
}

func TestControlConnectionCap(t *testing.T) {
	old := controlBusyWait
	controlBusyWait = 200 * time.Millisecond
	defer func() { controlBusyWait = old }()
	f := newFakeServer(t)
	_, cfg := ctlRunner(t, f)
	sock := ControlAddress(cfg)
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for range controlMaxConns {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		// a request on each, so the server has accepted it for sure
		_, _ = c.Write([]byte("{\"op\":\"status\"}\n"))
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
	m := ctl(t, cfg, `{"op":"status"}`)
	if m.ok() || !strings.Contains(m.errText(), "too many") {
		t.Errorf("connection %d = %v", controlMaxConns+1, m)
	}
	held[0].Close()
	waitFor(t, "a freed connection slot", func() bool {
		raw, err := ControlRequest(cfg, map[string]string{"op": "status"}, 5*time.Second)
		return err == nil && strings.Contains(string(raw), `"ok":true`)
	})
}

func TestControlGarbageAndOversized(t *testing.T) {
	f := newFakeServer(t)
	_, cfg := ctlRunner(t, f)
	sock := ControlAddress(cfg)
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReader(c)
	for _, req := range []string{
		"hello there",
		"\x00\x01\xff\xfe",
		"[1,2,3]",
		`"status"`,
		`{"op":"status"`,
		`{"op":"shell","cmd":"id"}`,
		`{"op":"exec","args":["rm","-rf","/"]}`,
		`{"op":42}`,
		`{}`,
		`{"op":"tail","job_id":"x"}`,
		`{"op":"tail","job_id":1,"lines":-5}`,
		`{"op":"cancel"}`,
	} {
		if _, err := c.Write([]byte(req + "\n")); err != nil {
			t.Fatal(err)
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("%q: %v", req, err)
		}
		var m ctlReply
		if json.Unmarshal([]byte(line), &m) != nil || m.ok() || m.errText() == "" {
			t.Errorf("%q → %q", req, line)
		}
	}
	// blank lines are skipped and the connection still works
	if _, err := c.Write([]byte("\n  \r\n{\"op\":\"status\"}\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := br.ReadString('\n'); err != nil || !strings.Contains(line, `"ok":true`) {
		t.Errorf("after garbage: %q %v", line, err)
	}

	// an oversized request gets an error and the connection is closed
	big, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer big.Close()
	_ = big.SetDeadline(time.Now().Add(20 * time.Second))
	go func() {
		_, _ = big.Write([]byte(`{"op":"status","pad":"` + strings.Repeat("A", 200<<10) + `"}` + "\n"))
	}()
	bbr := bufio.NewReader(big)
	line, err := bbr.ReadString('\n')
	if err != nil || !strings.Contains(line, "too large") || !strings.Contains(line, `"ok":false`) {
		t.Errorf("oversized: %q %v", line, err)
	}
	if _, err := bbr.ReadString('\n'); err == nil {
		t.Error("the connection stayed open after an oversized request")
	}
	// a request without a final newline is answered when the client stops writing
	half, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer half.Close()
	_ = half.SetDeadline(time.Now().Add(20 * time.Second))
	_, _ = half.Write([]byte(`{"op":"status"}`))
	_ = half.(*net.UnixConn).CloseWrite()
	if line, err := bufio.NewReader(half).ReadString('\n'); err != nil || !strings.Contains(line, `"ok":true`) {
		t.Errorf("unterminated request: %q %v", line, err)
	}
	// the server is still fine
	if !ctl(t, cfg, `{"op":"status"}`).ok() {
		t.Error("the channel died")
	}
}

func TestControlIdleTimeout(t *testing.T) {
	old := controlIdle
	controlIdle = 200 * time.Millisecond
	defer func() { controlIdle = old }()
	f := newFakeServer(t)
	_, cfg := ctlRunner(t, f)
	c, err := net.Dial("unix", ControlAddress(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte(`{"op":"sta`)) // never finished
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	start := time.Now()
	buf := make([]byte, 256)
	for {
		if _, err := c.Read(buf); err != nil {
			break
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("a silent client was kept for %v", d)
	}
}

func TestBatteryParsers(t *testing.T) {
	if !pmsetOnBattery("Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1234)\t80%; discharging; 4:10 remaining present: true\n") {
		t.Error("pmset battery not seen")
	}
	if pmsetOnBattery("Now drawing from 'AC Power'\n -InternalBattery-0 (id=1234)\t100%; charged; 0:00 remaining present: true\n") ||
		pmsetOnBattery("Now drawing from 'AC Power'\n") || pmsetOnBattery("") {
		t.Error("pmset AC read as battery")
	}
	mk := func(files map[string]string) string {
		dir := t.TempDir()
		for name, content := range files {
			p := filepath.Join(dir, name)
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"laptop on battery", map[string]string{"AC/type": "Mains", "AC/online": "0", "BAT0/type": "Battery", "BAT0/status": "Discharging"}, true},
		{"laptop plugged in", map[string]string{"AC/type": "Mains", "AC/online": "1", "BAT0/type": "Battery", "BAT0/status": "Charging"}, false},
		{"plugged in, battery sagging", map[string]string{"AC/type": "Mains", "AC/online": "1", "BAT0/type": "Battery", "BAT0/status": "Discharging"}, false},
		{"usb-c power", map[string]string{"ucsi/type": "USB", "ucsi/online": "1", "BAT0/type": "Battery", "BAT0/status": "Discharging"}, false},
		{"full, unplugged reading", map[string]string{"AC/type": "Mains", "AC/online": "0", "BAT0/type": "Battery", "BAT0/status": "Full"}, false},
		{"desktop with a wireless mouse", map[string]string{"hidpp/type": "Battery", "hidpp/status": "Discharging", "hidpp/scope": "Device"}, false},
		{"desktop", map[string]string{}, false},
	}
	for _, c := range cases {
		if got := sysfsOnBattery(mk(c.files)); got != c.want {
			t.Errorf("%s: on battery = %v, want %v", c.name, got, c.want)
		}
	}
	if sysfsOnBattery(filepath.Join(t.TempDir(), "missing")) {
		t.Error("a missing folder read as battery")
	}
	_ = deviceOnBattery() // must not panic or hang on this machine
}
