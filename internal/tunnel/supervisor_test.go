//go:build !windows

package tunnel

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeCloudflared writes a shell script that behaves like `cloudflared tunnel
// --url …`: it records its arguments and environment, prints a quick-tunnel
// banner and a "Registered" line, starts a child (to prove the whole process
// group dies) and then waits. With a "crash" file in dir it exits instead, to test
// restarts.
func fakeCloudflared(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "fake-cloudflared")
	script := `#!/bin/sh
echo "$@" > "` + dir + `/args"
env > "` + dir + `/env"
n=$(cat "` + dir + `/runs" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "` + dir + `/runs"
echo "2026-11-11T09:00:00Z INF Requesting new quick Tunnel on trycloudflare.com..." >&2
echo "2026-11-11T09:00:00Z INF +--------------------------------------------------------------------------------------------+" >&2
echo "2026-11-11T09:00:00Z INF |  https://quiet-fox-run$n.trycloudflare.com                                                |" >&2
echo "2026-11-11T09:00:01Z INF Registered tunnel connection connIndex=0 connection=abc event=0 ip=198.41.200.13 location=lhr01 protocol=quic" >&2
sleep 600 &
echo $! > "` + dir + `/child"
if [ -f "` + dir + `/crash" ]; then exit 3; fi
wait
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func readInt(t *testing.T, p string) int {
	t.Helper()
	b, _ := os.ReadFile(p)
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func TestQuickTunnelStartParseStop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ATELIER_SECRET_KEY", "must-not-leak")
	s := &Supervisor{Bin: fakeCloudflared(t, dir), Provider: Cloudflare{}, PIDFile: filepath.Join(dir, "tunnel.pid")}
	if st := s.Status(); st.Phase != PhaseOff {
		t.Fatalf("new supervisor phase = %q", st.Phase)
	}
	if err := s.Start(Settings{Mode: ModeQuick, Origin: "http://127.0.0.1:3001"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "connected", func() bool { st := s.Status(); return st.Phase == PhaseConnected && st.Address != "" })
	if got := s.Status().Address; got != "https://quiet-fox-run1.trycloudflare.com" {
		t.Errorf("address = %q", got)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if strings.TrimSpace(string(args)) != "tunnel --no-autoupdate --url http://127.0.0.1:3001" {
		t.Errorf("args = %q", args)
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if strings.Contains(string(env), "must-not-leak") {
		t.Error("Atelier's environment leaked into the helper")
	}
	waitFor(t, "child pid", func() bool { return readInt(t, filepath.Join(dir, "child")) > 0 })
	child := readInt(t, filepath.Join(dir, "child"))
	pid := s.Status().PID
	if readInt(t, s.PIDFile) != pid || !alive(pid) || !alive(child) {
		t.Fatalf("pid file / processes: file=%d pid=%d child=%d", readInt(t, s.PIDFile), pid, child)
	}

	start := time.Now()
	s.Stop()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Stop took %s; it must be immediate", d)
	}
	waitFor(t, "helper and child to die", func() bool { return !alive(pid) && !alive(child) })
	if st := s.Status(); st.Phase != PhaseOff || st.Address != "" {
		t.Errorf("after stop: %+v", st)
	}
	if _, err := os.Stat(s.PIDFile); !os.IsNotExist(err) {
		t.Error("pid file left behind")
	}
}

func TestRestartsWithBackoffAndNewAddress(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "crash"), nil, 0o600)
	var waits []int
	s := &Supervisor{Bin: fakeCloudflared(t, dir), Provider: Cloudflare{},
		Backoff: func(n int) time.Duration { waits = append(waits, n); return 20 * time.Millisecond }}
	if err := s.Start(Settings{Mode: ModeQuick, Origin: "http://127.0.0.1:3001"}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "three runs", func() bool { return readInt(t, filepath.Join(dir, "runs")) >= 3 })
	st := s.Status()
	if st.Restarts < 2 || !strings.Contains(st.LastError, "exit status 3") {
		t.Errorf("status after crashes: %+v", st)
	}
	s.Stop()
	if len(waits) < 2 || waits[0] != 1 || waits[1] != 2 {
		t.Errorf("backoff steps = %v, want 1, 2, …", waits)
	}
}

func TestNamedTunnelTokenStaysOutOfArgsAndLogs(t *testing.T) {
	dir := t.TempDir()
	s := &Supervisor{Bin: fakeCloudflared(t, dir), Provider: Cloudflare{}}
	tok := "eyJhIjoic2VjcmV0LXRva2VuIn0"
	if err := s.Start(Settings{Mode: ModeNamed, Origin: "http://127.0.0.1:3001", Token: tok, Hostname: "https://Git.Example.com/"}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "connected", func() bool { return s.Status().Phase == PhaseConnected })
	if a := s.Status().Address; a != "https://git.example.com" {
		t.Errorf("named address = %q (the configured hostname wins over anything printed)", a)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	if strings.Contains(string(args), tok) || strings.TrimSpace(string(args)) != "tunnel --no-autoupdate run" {
		t.Errorf("args = %q", args)
	}
	if !strings.Contains(string(env), "TUNNEL_TOKEN="+tok) {
		t.Error("token not passed in TUNNEL_TOKEN")
	}
}

func TestMissingBinaryKeepsRetrying(t *testing.T) {
	s := &Supervisor{Bin: filepath.Join(t.TempDir(), "nope"), Provider: Cloudflare{},
		Backoff: func(int) time.Duration { return 10 * time.Millisecond }}
	if err := s.Start(Settings{Mode: ModeQuick, Origin: "http://127.0.0.1:3001"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "error", func() bool { return strings.Contains(s.Status().LastError, "not installed") })
	s.Stop()
	if s.Status().Phase != PhaseOff {
		t.Error("not off after Stop")
	}
}

func TestKillFromPIDFile(t *testing.T) {
	dir := t.TempDir()
	s := &Supervisor{Bin: fakeCloudflared(t, dir), Provider: Cloudflare{}, PIDFile: filepath.Join(dir, "tunnel.pid"),
		Backoff: func(int) time.Duration { return time.Hour }}
	if err := s.Start(Settings{Mode: ModeQuick, Origin: "http://127.0.0.1:3001"}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "pid file", func() bool { return readInt(t, s.PIDFile) > 0 })
	pid := readInt(t, s.PIDFile)
	// A pid file naming some other program is ignored (and removed).
	other := filepath.Join(dir, "other.pid")
	os.WriteFile(other, []byte(strconv.Itoa(os.Getpid())), 0o600)
	if killed, _ := KillFromPIDFile(other, "cloudflared"); killed {
		t.Fatal("killed a process that isn't cloudflared")
	}
	killed, err := KillFromPIDFile(s.PIDFile, "cloudflared")
	if err != nil || !killed {
		t.Fatalf("KillFromPIDFile = %v, %v", killed, err)
	}
	waitFor(t, "helper to die", func() bool { return !alive(pid) })
}

func TestProviderParsing(t *testing.T) {
	cf := Cloudflare{}
	for line, want := range map[string]string{
		"INF |  https://quiet-fox.trycloudflare.com   |":                "https://quiet-fox.trycloudflare.com",
		"failed to request quick Tunnel: https://api.trycloudflare.com": "",
		"https://evil.trycloudflare.com.attacker.net":                   "",
		"nothing here": "",
	} {
		got, _ := cf.Address(line)
		if got != want {
			t.Errorf("Address(%q) = %q, want %q", line, got, want)
		}
	}
	for in, want := range map[string]string{
		"git.example.com": "git.example.com", "https://Git.Example.com/": "git.example.com",
		"localhost": "", "git.example.com:443": "", "a b.com": "", "": "",
	} {
		got, _ := NormalizeHostname(in)
		if got != want {
			t.Errorf("NormalizeHostname(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := cf.Command(Settings{Mode: ModeNamed, Origin: "http://x", Hostname: "git.example.com"}); err == nil {
		t.Error("named tunnel without a token accepted")
	}
}

func TestNoRestartWhenNotAllowed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "crash"), nil, 0o600)
	allow := false
	var mu sync.Mutex
	s := &Supervisor{Bin: fakeCloudflared(t, dir), Provider: Cloudflare{},
		Backoff: func(int) time.Duration { return 10 * time.Millisecond },
		Allow:   func() bool { mu.Lock(); defer mu.Unlock(); return allow }}
	if err := s.Start(Settings{Mode: ModeQuick, Origin: "http://127.0.0.1:3001"}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	waitFor(t, "first run", func() bool { return readInt(t, filepath.Join(dir, "runs")) == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := readInt(t, filepath.Join(dir, "runs")); n != 1 {
		t.Fatalf("restarted %d times while not allowed", n-1)
	}
	mu.Lock()
	allow = true
	mu.Unlock()
	waitFor(t, "restart once allowed", func() bool { return readInt(t, filepath.Join(dir, "runs")) >= 2 })
}
