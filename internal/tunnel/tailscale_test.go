//go:build !windows

package tunnel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/tunnel/tunneltest"
)

const (
	tsTestKey  = "tskey-auth-kTESTONLY11CNTRL-notARealKeyJustForTests0000"
	tsTestNode = "atelier." + tunneltest.Tailnet
)

type tsFix struct {
	t    *testing.T
	dir  string // the fakes' scratch directory
	ts   *Tailscale
	mu   sync.Mutex
	logs []string
	node []string
}

func newTSFix(t *testing.T) *tsFix {
	t.Helper()
	dir := t.TempDir()
	bin, daemon, err := tunneltest.FakeTailscale(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	f := &tsFix{t: t, dir: dir}
	f.ts = &Tailscale{Bin: bin, DaemonBin: daemon, StateDir: filepath.Join(data, "tailscale"), PIDDir: data,
		Poll: 20 * time.Millisecond, Backoff: func(int) time.Duration { return 20 * time.Millisecond },
		Logf: func(format string, args ...any) {
			f.mu.Lock()
			f.logs = append(f.logs, fmt.Sprintf(format, args...))
			f.mu.Unlock()
		},
		OnNode: func(dns string) {
			f.mu.Lock()
			f.node = append(f.node, dns)
			f.mu.Unlock()
		}}
	t.Cleanup(f.ts.Stop)
	f.write("goodkey", tsTestKey)
	return f
}

func (f *tsFix) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *tsFix) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.dir, name))
	return strings.TrimSpace(string(b))
}

func (f *tsFix) pid(name string) int {
	// The helper script writes its pid files just after it starts; wait for them.
	for i := 0; i < 100; i++ {
		if n, _ := strconv.Atoi(f.read(name)); n > 0 {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0
}

func (f *tsFix) start(mode, key string) {
	f.t.Helper()
	if err := f.ts.Start(Settings{Mode: mode, Origin: "http://127.0.0.1:3001", Token: key, Hostname: "atelier"}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *tsFix) connected() bool {
	st := f.ts.Status()
	return st.Phase == PhaseConnected && st.Address == "https://"+tsTestNode
}

func TestTailscalePrivateSignsInServesAndStops(t *testing.T) {
	f := newTSFix(t)
	f.write("caps", `"https":null`)
	t.Setenv("ATELIER_SECRET_KEY", "must-not-reach-the-helpers")
	if f.ts.Running() || f.ts.Status().Phase != PhaseOff {
		t.Fatal("running before Start")
	}
	f.start(ModeTailscalePrivate, tsTestKey)
	waitFor(t, "serve to be available", f.connected)

	// tailscaled: userspace networking, state and socket where they belong.
	dargs := f.read("daemon.args")
	sock := TailscaleSocket(f.ts.StateDir)
	for _, want := range []string{"--tun=userspace-networking", "--statedir=" + f.ts.StateDir, "--socket=" + sock, "--no-logs-no-support"} {
		if !strings.Contains(dargs, want) {
			t.Errorf("tailscaled args %q lack %q", dargs, want)
		}
	}
	if fi, err := os.Stat(f.ts.StateDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("state dir = %v, %v; want mode 0700", fi, err)
	}
	if got := f.read("socket"); got != "--socket="+sock {
		t.Errorf("CLI socket = %q", got)
	}

	// The auth key: in a 0600 file that is gone again, never in argv or env.
	if got := f.read("up.keyseen"); got != tsTestKey {
		t.Errorf("tailscale up read key %q", got)
	}
	if mode := f.read("up.keymode"); mode != "-rw-------" {
		t.Errorf("key file mode = %q", mode)
	}
	keyFile := f.read("up.keyfile")
	if !strings.HasPrefix(keyFile, f.ts.StateDir) {
		t.Errorf("key file %q outside the state dir", keyFile)
	}
	if _, err := os.Stat(keyFile); err == nil {
		t.Error("the key file was left behind")
	}
	if up := f.read("up.args"); !strings.Contains(up, "--auth-key=file:") || !strings.Contains(up, "--hostname=atelier") {
		t.Errorf("up args = %q", up)
	}
	for _, name := range []string{"up.args", "up.env", "daemon.args", "daemon.env", "serve.args", "serve.env"} {
		got := f.read(name)
		if got == "" {
			t.Errorf("%s not recorded", name)
		}
		if strings.Contains(got, tsTestKey) || strings.Contains(got, "kTESTONLY") {
			t.Errorf("%s contains the auth key", name)
		}
		if strings.Contains(got, "ATELIER_SECRET_KEY") {
			t.Errorf("%s got Atelier's environment", name)
		}
	}
	f.mu.Lock()
	logs, nodes := strings.Join(f.logs, "\n"), append([]string(nil), f.node...)
	f.mu.Unlock()
	if strings.Contains(logs, "kTESTONLY") || strings.Contains(strings.Join(f.ts.Status().Log, "\n"), "kTESTONLY") {
		t.Error("the auth key was logged")
	}
	if len(nodes) != 1 || nodes[0] != tsTestNode {
		t.Errorf("OnNode calls = %v", nodes)
	}

	// serve, in the foreground, at the remote listener; stale config reset first.
	if got := f.read("serve.args"); got != "serve --https=443 http://127.0.0.1:3001" {
		t.Errorf("serve args = %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "reset")); err != nil {
		t.Error("serve reset wasn't run before serving")
	}
	for _, name := range []string{TailscaledPIDFile, TailscaleServePIDFile} {
		if _, err := os.Stat(filepath.Join(f.ts.PIDDir, name)); err != nil {
			t.Errorf("no %s", name)
		}
	}

	pids := []int{f.pid("daemon.pid"), f.pid("daemon.child"), f.pid("serve.child"), f.ts.Status().PID}
	for _, p := range pids {
		if !alive(p) {
			t.Fatalf("helper pid %d not running (%v)", p, pids)
		}
	}
	start := time.Now()
	f.ts.Stop()
	if d := time.Since(start); d > time.Second {
		t.Errorf("Stop took %s", d)
	}
	waitFor(t, "every helper process to die", func() bool {
		for _, p := range pids {
			if alive(p) {
				return false
			}
		}
		return true
	})
	if st := f.ts.Status(); st.Phase != PhaseOff || st.Address != "" || f.ts.Running() {
		t.Errorf("status after Stop = %+v", st)
	}
	for _, name := range []string{TailscaledPIDFile, TailscaleServePIDFile} {
		if _, err := os.Stat(filepath.Join(f.ts.PIDDir, name)); err == nil {
			t.Errorf("%s left behind", name)
		}
	}
	runs := f.read("serve.runs")
	time.Sleep(100 * time.Millisecond)
	if f.read("serve.runs") != runs || alive(f.pid("daemon.pid")) {
		t.Error("something came back after Stop")
	}
}

func TestTailscaleFunnelAndSignedInNode(t *testing.T) {
	f := newTSFix(t)
	// Already signed in under another name: no key needed, the name is fixed.
	f.write("state", "Running")
	f.write("hostname", "old-name")
	f.write("caps", `"https":null,"funnel":null,"https://tailscale.com/cap/funnel-ports?ports=443,8443,10000":null`)
	f.start(ModeTailscaleFunnel, "")
	waitFor(t, "funnel to be available", f.connected)
	if got := f.read("serve.args"); got != "funnel --https=443 http://127.0.0.1:3001" {
		t.Errorf("funnel args = %q", got)
	}
	if got := f.read("set.args"); got != "--hostname=atelier" {
		t.Errorf("set args = %q", got)
	}
	if f.read("up.runs") != "" {
		t.Error("tailscale up was run for a node that is already signed in")
	}
}

func TestTailscaleFunnelNotEnabledThenEnabled(t *testing.T) {
	f := newTSFix(t)
	f.write("caps", `"https":null`)
	f.start(ModeTailscaleFunnel, tsTestKey)
	waitFor(t, "the funnel message", func() bool { return f.ts.Status().LastError == MsgTailscaleFunnel })
	st := f.ts.Status()
	if st.Phase == PhaseConnected || f.read("serve.args") != "" {
		t.Errorf("funnel started without the node attribute: %+v", st)
	}
	if st.Address != "https://"+tsTestNode {
		t.Errorf("address = %q (the node is signed in, so it is known)", st.Address)
	}
	if !strings.Contains(MsgTailscaleFunnel, "Access controls") || !strings.Contains(MsgTailscaleFunnel, "funnel") {
		t.Error("the funnel message doesn't say what to switch on")
	}
	// Enabled in the admin console: it starts by itself.
	f.write("caps", `"https":null,"funnel":null`)
	waitFor(t, "funnel after enabling", f.connected)
	if f.ts.Status().LastError != "" {
		t.Errorf("message still shown: %q", f.ts.Status().LastError)
	}
}

func TestTailscaleHTTPSNotEnabled(t *testing.T) {
	f := newTSFix(t)
	f.write("caps", ``)
	f.start(ModeTailscalePrivate, tsTestKey)
	waitFor(t, "the HTTPS message", func() bool { return f.ts.Status().LastError == MsgTailscaleHTTPS })
	if f.read("serve.args") != "" || f.ts.Status().Phase == PhaseConnected {
		t.Error("serve started without HTTPS certificates")
	}
	if !strings.Contains(MsgTailscaleHTTPS, "Enable HTTPS") || !strings.Contains(MsgTailscaleHTTPS, "MagicDNS") {
		t.Error("the HTTPS message doesn't say what to switch on")
	}
	f.write("caps", `"https":null`)
	waitFor(t, "serve after enabling HTTPS", f.connected)
}

// The CLI's own refusals (seen when the status JSON didn't predict them) are
// turned into the same sentences.
func TestTailscaleProblemFromCLIOutput(t *testing.T) {
	for out, want := range map[string]string{
		`Funnel not available; "funnel" node attribute not set. See https://tailscale.com/s/no-funnel.`:                        MsgTailscaleFunnel,
		"Funnel is not enabled on your tailnet.\nTo enable, visit:\n\n         https://login.tailscale.com/f/funnel?node=nABC": MsgTailscaleFunnel,
		"Funnel not available; HTTPS must be enabled. See https://tailscale.com/s/https.":                                      MsgTailscaleHTTPS,
		"Serve is not enabled on your tailnet.\nTo enable, visit:\n\n         https://login.tailscale.com/f/serve?node=nABC":   MsgTailscaleHTTPS,
		"port 443 is not allowed for funnel; allowed ports are: 8443":                                                          MsgTailscalePort,
		"backend error: invalid key: API key does not exist":                                                                   MsgTailscaleBadKey,
		"Logged out.":                             MsgTailscaleNeedsKey,
		"Available within your tailnet:":          "",
		"https://atelier.tail1234.ts.net/":        "",
		"2026/11/11 magicsock: disco key = d:abc": "",
	} {
		if got := TailscaleProblem(out); got != want {
			t.Errorf("TailscaleProblem(%q) = %q, want %q", out, got, want)
		}
	}

	f := newTSFix(t)
	f.write("state", "Running")
	f.write("hostname", "atelier")
	f.write("caps", `"https":null,"funnel":null`)
	f.write("serve.fail", `Funnel not available; "funnel" node attribute not set. See https://tailscale.com/s/no-funnel.`)
	f.start(ModeTailscaleFunnel, "")
	waitFor(t, "the CLI's refusal to be explained", func() bool { return f.ts.Status().LastError == MsgTailscaleFunnel })
	if f.ts.Status().Phase == PhaseConnected {
		t.Error("connected although funnel refused")
	}
}

func TestTailscaleBadAndMissingKey(t *testing.T) {
	f := newTSFix(t)
	f.write("caps", `"https":null`)
	f.start(ModeTailscalePrivate, "tskey-auth-kWRONG11CNTRL-thisOneIsNotAccepted000000")
	waitFor(t, "the bad key message", func() bool { return f.ts.Status().LastError == MsgTailscaleBadKey })
	time.Sleep(150 * time.Millisecond)
	if n := f.read("up.runs"); n != "1" {
		t.Errorf("tailscale up ran %s times; a refused key must not be retried", n)
	}
	if st := f.ts.Status(); st.Address != "" || st.Phase == PhaseConnected {
		t.Errorf("status with a bad key = %+v", st)
	}
	f.mu.Lock()
	logs := strings.Join(f.logs, "\n")
	f.mu.Unlock()
	if strings.Contains(logs, "kWRONG") {
		t.Error("the refused key was logged")
	}

	// No key at all, on a node that isn't signed in (e.g. after a restore).
	f.start(ModeTailscalePrivate, "")
	waitFor(t, "the needs-a-key message", func() bool { return f.ts.Status().LastError == MsgTailscaleNeedsKey })
	if f.read("up.runs") != "1" {
		t.Error("tailscale up was run without a key")
	}
}

func TestTailscaleNeverRestartsWhenOff(t *testing.T) {
	f := newTSFix(t)
	f.write("state", "Running")
	f.write("hostname", "atelier")
	f.write("caps", `"https":null`)
	var mu sync.Mutex
	allowed := true
	f.ts.Allow = func() bool { mu.Lock(); defer mu.Unlock(); return allowed }
	f.start(ModeTailscalePrivate, "")
	waitFor(t, "serve", f.connected)
	// The setting goes off and the helpers are killed from outside, as
	// `atelier tunnel off` does.
	mu.Lock()
	allowed = false
	mu.Unlock()
	for _, name := range []string{TailscaleServePIDFile, TailscaledPIDFile} {
		want := map[string]string{TailscaleServePIDFile: "tailscale", TailscaledPIDFile: "tailscaled"}[name]
		if killed, err := KillFromPIDFile(filepath.Join(f.ts.PIDDir, name), want); err != nil || !killed {
			t.Fatalf("kill from %s = %v, %v", name, killed, err)
		}
	}
	old := f.pid("daemon.pid")
	waitFor(t, "tailscaled to die", func() bool { return !alive(old) })
	time.Sleep(200 * time.Millisecond)
	if p := f.pid("daemon.pid"); p != old && alive(p) {
		t.Error("tailscaled was restarted although the setting is off")
	}
	if f.ts.Status().Phase == PhaseConnected {
		t.Error("still reported as connected")
	}
}

func TestTailscaleForget(t *testing.T) {
	f := newTSFix(t)
	f.write("caps", `"https":null`)
	f.start(ModeTailscalePrivate, tsTestKey)
	waitFor(t, "serve", f.connected)
	// What tailscaled would have written.
	if err := os.WriteFile(filepath.Join(f.ts.StateDir, "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	served := f.pid("daemon.pid")
	loggedOut, err := f.ts.Forget(context.Background())
	if err != nil || !loggedOut {
		t.Fatalf("Forget = %v, %v", loggedOut, err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "loggedout")); err != nil {
		t.Error("tailscale logout wasn't run")
	}
	if _, err := os.Stat(f.ts.StateDir); err == nil {
		t.Error("the state directory survived")
	}
	if f.ts.Running() || alive(served) {
		t.Error("still running after Forget")
	}
	waitFor(t, "the logout daemon to die", func() bool { return !alive(f.pid("daemon.pid")) })
	// Signed out: the next start needs a key again.
	f.start(ModeTailscalePrivate, "")
	waitFor(t, "the needs-a-key message", func() bool { return f.ts.Status().LastError == MsgTailscaleNeedsKey })
}

func TestTailscaleSettingsChecked(t *testing.T) {
	f := newTSFix(t)
	for _, set := range []Settings{
		{Mode: ModeTailscalePrivate},                       // no origin
		{Mode: ModeQuick, Origin: "http://127.0.0.1:3001"}, // not a Tailscale mode
		{Mode: ModeTailscaleFunnel, Origin: "http://127.0.0.1:3001", Hostname: "not a host"},
	} {
		if err := f.ts.Start(set); err == nil {
			t.Errorf("Start(%+v) accepted", set)
			f.ts.Stop()
		}
	}
	for in, want := range map[string]string{"": "atelier", " Atelier ": "atelier", "my-studio": "my-studio"} {
		if got, ok := NormalizeTailscaleHostname(in); !ok || got != want {
			t.Errorf("NormalizeTailscaleHostname(%q) = %q, %v", in, got, ok)
		}
	}
	for _, bad := range []string{"a.b", "-x", "x-", "has space", strings.Repeat("a", 64), "x;rm"} {
		if _, ok := NormalizeTailscaleHostname(bad); ok {
			t.Errorf("hostname %q accepted", bad)
		}
	}
	if p := TailscaleSocket("/data/tailscale"); p != "/data/tailscale/tailscaled.sock" {
		t.Errorf("socket = %q", p)
	}
	if p := TailscaleSocket("/" + strings.Repeat("x", 120)); len(p) > 100 || !strings.HasPrefix(p, os.TempDir()) {
		t.Errorf("long state dir gives socket %q", p)
	}
}
