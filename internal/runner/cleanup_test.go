package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"atelier/internal/actions"
)

// deadPID is the id of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("go", "version")
	if err := cmd.Run(); err != nil {
		t.Skip("can't start a process:", err)
	}
	return cmd.Process.Pid
}

func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	return !pidAlive(pid)
}

// A program a step starts in the background lives on for the next steps,
// doesn't hold its step open, and is stopped when the job ends.
func TestBackgroundProgramsStoppedAtJobEnd(t *testing.T) {
	skipNoUnixShell(t)
	dir := t.TempDir()
	f := newFakeServer(t)
	job := baseJob(41, nil)
	job.Steps = []actions.Step{
		// holds the step's output open
		{Index: 0, Run: `sleep 300 & echo $! > ` + filepath.Join(dir, "a") + `; echo started-a`},
		// output redirected, like a server started with nohup
		{Index: 1, Run: `nohup sleep 300 >/dev/null 2>&1 & echo $! > ` + filepath.Join(dir, "b")},
		{Index: 2, Run: `kill -0 "$(cat ` + filepath.Join(dir, "a") + `)" && echo still-running`},
	}
	start := time.Now()
	f.runJobs(t, f.testRunner(t), job)
	logs := f.logText(41)
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("job took %v: a background program held its step open\n%s", d, logs)
	}
	if f.finish(41).Status != actions.StatusSuccess || !hasLine(logs, "still-running") {
		t.Fatalf("status %s\n%s", f.finish(41).Status, logs)
	}
	if !strings.Contains(logs, "Stopped 2 programs that steps left running") {
		t.Errorf("no stop message:\n%s", logs)
	}
	for _, name := range []string{"a", "b"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid == 0 || !waitGone(pid, 3*time.Second) {
			t.Errorf("background program %s (pid %d) still running after the job", name, pid)
		}
	}
}

// A step's output written just before it exits is never lost.
func TestStepOutputComplete(t *testing.T) {
	skipNoUnixShell(t)
	f := newFakeServer(t)
	job := baseJob(42, nil)
	job.Steps = []actions.Step{{Index: 0, Run: `i=0; while [ $i -lt 3000 ]; do echo "line-$i"; i=$((i+1)); done; printf 'no-newline-at-end'`}}
	f.runJobs(t, f.testRunner(t), job)
	logs := f.logText(42)
	if !hasLine(logs, "line-2999") || !strings.Contains(logs, "no-newline-at-end") {
		t.Errorf("output cut short (%d bytes)", len(logs))
	}
}

func TestSweepJobDirs(t *testing.T) {
	work := t.TempDir()
	dead := deadPID(t)
	mk := func(name, owner string, age time.Duration) string {
		d := filepath.Join(work, name)
		os.MkdirAll(filepath.Join(d, "w"), 0o755)
		if owner != "" {
			os.WriteFile(filepath.Join(d, jobOwnerFile), []byte(owner+"\n"), 0o644)
		}
		old := time.Now().Add(-age)
		os.Chtimes(d, old, old)
		return d
	}
	crashed := mk("job-1-aaaa", strconv.Itoa(dead), 0)
	mine := mk("job-2-bbbb", strconv.Itoa(os.Getpid()), 0)
	oldUnowned := mk("job-3-cccc", "", 48*time.Hour)
	newUnowned := mk("job-4-dddd", "", time.Hour)
	other := mk("_tool", "", 48*time.Hour)
	if n := sweepJobDirs(work, time.Now()); n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	for d, want := range map[string]bool{crashed: false, mine: true, oldUnowned: false, newUnowned: true, other: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(d), err == nil, want)
		}
	}
}

func TestStaleContainers(t *testing.T) {
	out := "abc123\t4242\trunning\t2026-11-13 09:00:00 +0000 UTC\n" +
		"def456\t\texited\t2026-11-13 09:00:00 +0000 UTC\n" +
		"aaa111\t\trunning\t2026-11-10 09:00:00 +0000 UTC\n" +
		"bbb222\t\trunning\t2026-11-13 09:00:00 +0000 UTC\n" +
		"ccc333\t7\trunning\t2026-11-13 09:00:00 +0000 UTC\n" +
		"ddd444\t99\texited\t2026-11-13 09:00:00 +0000 UTC\n"
	cs := parseContainerList(out)
	if len(cs) != 6 || cs[0].pid != 4242 || cs[1].pid != 0 || cs[0].created.IsZero() {
		t.Fatalf("parsed %+v", cs)
	}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	alive := func(pid int) bool { return pid == 4242 }
	var stale []string
	for _, c := range cs {
		if staleContainer(c, 99, alive, now) {
			stale = append(stale, c.id)
		}
	}
	// 4242 is alive and 99 is us: kept. No owner: stopped, or older than a
	// day, goes. 7 is gone: goes.
	if got := strings.Join(stale, ","); got != "def456,aaa111,ccc333" {
		t.Errorf("stale = %s", got)
	}
}

func TestPruneActionCache(t *testing.T) {
	root := filepath.Join(t.TempDir(), "actions")
	now := time.Now()
	mk := func(rel string, age time.Duration, marker bool) string {
		d := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "action.yml"), []byte("name: x\n"), 0o644)
		when := now.Add(-age)
		if marker {
			os.WriteFile(filepath.Join(d, actionMarker), []byte("sha\n"), 0o644)
			os.Chtimes(filepath.Join(d, actionMarker), when, when)
		}
		os.Chtimes(d, when, when)
		return d
	}
	old := mk("actions/checkout/aaa", 40*24*time.Hour, true)
	fresh := mk("actions/checkout/bbb", 2*24*time.Hour, true)
	oldAtelier := mk("atelier/demi/tool/ccc", 31*24*time.Hour, true)
	halfDone := mk("actions/setup-node/.dl-123", 2*24*time.Hour, false)
	inFlight := mk("actions/setup-node/.dl-456", time.Minute, false)
	if n := pruneActionCache(root, now); n != 2 {
		t.Errorf("pruned %d, want 2", n)
	}
	for d, want := range map[string]bool{old: false, fresh: true, oldAtelier: false, halfDone: false, inFlight: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", d, err == nil, want)
		}
	}
	// Using an action counts as fresh again.
	touchAction(fresh)
	later := now.Add(actionCacheTTL - time.Hour)
	if n := pruneActionCache(root, later); n != 0 {
		t.Errorf("pruned a recently used action")
	}
	if pruneActionCache(filepath.Join(t.TempDir(), "missing"), now) != 0 {
		t.Error("missing cache")
	}
}

// Cancelling a docker step whose program ignores the stop signal still
// removes its container.
func TestCancelRemovesStepContainer(t *testing.T) {
	if os.Getenv("ATELIER_TEST_DOCKER") != "1" {
		t.Skip("set ATELIER_TEST_DOCKER=1 to run docker tests")
	}
	f := newFakeServer(t)
	job := baseJob(43, nil)
	job.Steps = []actions.Step{{Index: 0, Uses: "docker://alpine:3",
		With: map[string]string{"args": `sh -c "trap '' TERM INT; echo ready; sleep 300 & wait"`}}}
	f.cancelWhen[43] = "ready"
	f.runJobs(t, f.testRunner(t, actions.CapJobs, actions.CapDocker), job)
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label="+containerLabelJobID+"=43").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("container left after cancel: %s", out)
		exec.Command("docker", "rm", "-f", strings.TrimSpace(string(out))).Run()
	}
}
