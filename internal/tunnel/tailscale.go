package tunnel

// Tailscale as a provider: the studio becomes a node on the owner's tailnet
// and `tailscale serve` (private) or `tailscale funnel` (public) forwards
// https://<hostname>.<tailnet>.ts.net to the remote listener.
//
// Two programs run, both under a Supervisor (own process group, backoff,
// never restarted once the setting is off):
//
//   - tailscaled --tun=userspace-networking, with its state (the node's
//     identity) in <data>/tailscale and its socket beside it. Userspace
//     networking needs no TUN device, no NET_ADMIN and no root.
//   - tailscale serve|funnel in the foreground. A foreground session's
//     config is dropped by tailscaled when the CLI goes away, so killing it
//     stops the forwarding even while the daemon lives.
//
// Between the two, a controller signs the node in when it has to (`tailscale
// up` with the auth key in a 0600 file, never on the command line), reads
// the node's DNS name from `tailscale status --json`, and checks that the
// tailnet allows what was asked (HTTPS certificates, and the funnel node
// attribute for public mode) so the owner gets a sentence instead of the
// CLI's interactive prompt.
//
// Off stops everything, tailscaled included: no process, no connection to
// Tailscale, the node shows as offline. Starting again takes a few seconds
// because the node's identity is kept on disk.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Providers.
const (
	ProviderCloudflare = "cloudflare"
	ProviderTailscale  = "tailscale"
)

// Tailscale modes.
const (
	ModeTailscalePrivate = "tailscale-private" // tailscale serve: the owner's own devices only
	ModeTailscaleFunnel  = "tailscale-funnel"  // tailscale funnel: the internet
)

// PID files (in the data directory) for `atelier tunnel off`.
const (
	TailscaledPIDFile     = "tailscaled.pid"
	TailscaleServePIDFile = "tailscale-serve.pid"
)

// DefaultTailscaleHostname is the node's name on the tailnet unless changed.
const DefaultTailscaleHostname = "atelier"

// What the owner is told when Tailscale can't do what was asked.
const (
	MsgTailscaleNeedsKey = "This server isn't signed in to your tailnet. Paste a Tailscale auth key below, save, and it will sign in by itself. (After a restore, or when the node's key expired or the machine was removed in the admin console, it needs a new key.)"
	MsgTailscaleBadKey   = "Tailscale didn't accept the auth key: it is expired, revoked, already used or mistyped. Create a new one in the Tailscale admin console (Settings → Keys) and paste it below."
	MsgTailscaleApproval = "Your tailnet has device approval on. Approve this machine in the Tailscale admin console (Machines) and remote access will finish starting by itself."
	MsgTailscaleHTTPS    = "HTTPS certificates aren't enabled for your tailnet. In the Tailscale admin console open DNS, turn on MagicDNS, then press Enable HTTPS under HTTPS Certificates. Remote access will finish starting by itself."
	MsgTailscaleFunnel   = "Funnel isn't allowed for this machine. In the Tailscale admin console open Access controls and give it the funnel node attribute, for example \"nodeAttrs\": [{\"target\": [\"autogroup:member\"], \"attr\": [\"funnel\"]}] (use the tag as the target if the auth key was tagged). Remote access will finish starting by itself; or switch to Private."
	MsgTailscalePort     = "Your tailnet's policy doesn't allow Funnel on port 443. In the Tailscale admin console, open Access controls and allow port 443 for the funnel node attribute."
)

// TailscaleProblem turns the tailscale CLI's output into a sentence for the
// owner, or "" when it recognises nothing.
func TailscaleProblem(output string) string {
	o := strings.ToLower(output)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(o, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("not allowed for funnel"):
		return MsgTailscalePort
	case has(`"funnel" node attribute not set`, "/f/funnel", "funnel is not enabled", "s/no-funnel"):
		return MsgTailscaleFunnel
	case has("https must be enabled", "/f/serve", "serve is not enabled", "https cert support is not enabled", "s/https"):
		return MsgTailscaleHTTPS
	case has("invalid key", "key not valid", "authkey expired", "invalid authkey", "key is not valid"):
		return MsgTailscaleBadKey
	case has("logged out", "needslogin"):
		return MsgTailscaleNeedsKey
	}
	return ""
}

var tsLabelRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeTailscaleHostname accepts one DNS label ("" means the default).
func NormalizeTailscaleHostname(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return DefaultTailscaleHostname, true
	}
	return s, tsLabelRe.MatchString(s)
}

// TailscaleSocket is where tailscaled's socket goes for a state directory:
// beside the state, or under the temp directory when that path would be too
// long for a unix socket.
func TailscaleSocket(stateDir string) string {
	p := filepath.Join(stateDir, "tailscaled.sock")
	if len(p) <= 100 {
		return p
	}
	sum := 0
	for _, c := range []byte(stateDir) {
		sum = sum*31 + int(c)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("atelier-ts-%08x", uint32(sum)), "tailscaled.sock")
}

// staticProvider runs a fixed command under a Supervisor.
type staticProvider struct {
	name  string
	cmd   Command
	ready func(line string) bool
}

func (p staticProvider) Name() string                      { return p.name }
func (p staticProvider) Command(Settings) (Command, error) { return p.cmd, nil }
func (p staticProvider) Address(string) (string, bool)     { return "", false }
func (p staticProvider) ClientIPHeader() string            { return "X-Forwarded-For" }
func (p staticProvider) Ready(line string) bool            { return p.ready != nil && p.ready(line) }

// Tailscale runs tailscaled and tailscale serve/funnel. Its Start, Stop,
// Status and Running match Supervisor's.
type Tailscale struct {
	Bin       string // the tailscale CLI
	DaemonBin string // tailscaled
	StateDir  string // <data>/tailscale: the node's identity, mode 0700, never backed up
	Socket    string // "" = TailscaleSocket(StateDir)
	PIDDir    string // where the two PID files go ("" = none)
	Logf      func(format string, args ...any)
	Backoff   func(n int) time.Duration
	Allow     func() bool
	// OnNode is called from the controller when the node is signed in, with
	// its DNS name. The auth key isn't needed after that.
	OnNode func(dnsName string)
	// Poll is how often the controller looks at the daemon while starting
	// (default 500ms; five seconds once serving).
	Poll time.Duration

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	daemon  *Supervisor
	serve   *Supervisor
	node    string // the DNS name, once signed in
	problem string
	since   time.Time
}

func (t *Tailscale) logf(format string, args ...any) {
	if t.Logf != nil {
		t.Logf(format, args...)
	}
}

func (t *Tailscale) socket() string {
	if t.Socket != "" {
		return t.Socket
	}
	return TailscaleSocket(t.StateDir)
}

func (t *Tailscale) pidFile(name string) string {
	if t.PIDDir == "" {
		return ""
	}
	return filepath.Join(t.PIDDir, name)
}

// prepare makes the state directory (0700) and the socket's directory.
func (t *Tailscale) prepare() error {
	if t.StateDir == "" {
		return errors.New("tailscale: no state directory")
	}
	for _, d := range []string{t.StateDir, filepath.Dir(t.socket())} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// daemonCommand is tailscaled's command line. --no-logs-no-support keeps it
// from uploading its logs to Tailscale; TS_LOGS_DIR keeps its log config
// inside the state directory.
func (t *Tailscale) daemonCommand() Command {
	return Command{
		Args: []string{"--tun=userspace-networking", "--statedir=" + t.StateDir, "--socket=" + t.socket(), "--no-logs-no-support"},
		Env:  []string{"TS_LOGS_DIR=" + t.StateDir},
	}
}

// Running reports whether it was started (in any phase but off).
func (t *Tailscale) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancel != nil
}

// Start (re)starts tailscaled and the controller. set.Mode is one of the
// Tailscale modes, set.Token the auth key ("" once the node is signed in)
// and set.Hostname the node's name.
func (t *Tailscale) Start(set Settings) error {
	if set.Origin == "" {
		return errNoOrigin
	}
	if set.Mode != ModeTailscalePrivate && set.Mode != ModeTailscaleFunnel {
		return errMode
	}
	host, ok := NormalizeTailscaleHostname(set.Hostname)
	if !ok {
		return errors.New("the Tailscale hostname should be one word, like atelier")
	}
	set.Hostname = host
	set.Token = strings.TrimSpace(set.Token)
	t.Stop()
	if err := t.prepare(); err != nil {
		return fmt.Errorf("the Tailscale state directory couldn't be prepared: %w", err)
	}
	daemon := &Supervisor{Bin: t.DaemonBin, PIDFile: t.pidFile(TailscaledPIDFile), Logf: t.Logf, Backoff: t.Backoff, Allow: t.Allow,
		Provider: staticProvider{name: "tailscaled", cmd: t.daemonCommand()}}
	if err := daemon.Start(Settings{Token: set.Token}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.mu.Lock()
	t.cancel, t.done, t.daemon, t.serve = cancel, done, daemon, nil
	t.node, t.problem, t.since = "", "", time.Now()
	t.mu.Unlock()
	go func() {
		defer close(done)
		t.control(ctx, set)
	}()
	return nil
}

// Stop kills serve/funnel and tailscaled at once and waits for the controller.
func (t *Tailscale) Stop() {
	t.mu.Lock()
	cancel, done, daemon, serve := t.cancel, t.done, t.daemon, t.serve
	t.cancel, t.done = nil, nil
	t.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if serve != nil {
		serve.Stop()
	}
	daemon.Stop()
	<-done
	t.mu.Lock()
	// The controller may have started serve between the read above and its exit.
	serve = t.serve
	t.daemon, t.serve, t.node, t.problem, t.since = nil, nil, "", "", time.Now()
	t.mu.Unlock()
	if serve != nil {
		serve.Stop()
	}
	t.removeTempSocket()
}

// removeTempSocket removes the socket's directory when it had to go under
// the temp directory (TailscaleSocket); beside the state it is left alone.
func (t *Tailscale) removeTempSocket() {
	if sock := t.socket(); t.Socket == "" && !strings.HasPrefix(sock, t.StateDir+string(filepath.Separator)) {
		_ = os.RemoveAll(filepath.Dir(sock))
	}
}

// Status is the two helpers seen as one: connected once serve/funnel says
// the address is available.
func (t *Tailscale) Status() Status {
	t.mu.Lock()
	running, daemon, serve := t.cancel != nil, t.daemon, t.serve
	node, problem, since := t.node, t.problem, t.since
	t.mu.Unlock()
	if !running || daemon == nil {
		return Status{Phase: PhaseOff, Since: since}
	}
	d := daemon.Status()
	st := Status{Phase: PhaseStarting, Since: since, PID: d.PID, Restarts: d.Restarts, Log: d.Log, LastError: problem}
	if node != "" {
		st.Address = "https://" + node
	}
	if d.Phase == PhaseRestarting {
		st.Phase = PhaseRestarting
		if st.LastError == "" {
			st.LastError = d.LastError
		}
	}
	if serve != nil {
		s := serve.Status()
		st.Log = append(st.Log, s.Log...)
		st.Restarts += s.Restarts
		if st.LastError == "" {
			st.LastError = TailscaleProblem(strings.Join(s.Log, "\n"))
		}
		if s.Phase == PhaseConnected && st.Phase != PhaseRestarting && st.LastError == "" {
			st.Phase, st.Since = PhaseConnected, s.Since
		}
	}
	if len(st.Log) > keepLines {
		st.Log = st.Log[len(st.Log)-keepLines:]
	}
	return st
}

// tsStatus is the part of `tailscale status --json` the controller reads.
type tsStatus struct {
	BackendState string
	Self         *struct {
		HostName     string
		DNSName      string
		Capabilities []string
		CapMap       map[string]json.RawMessage
	}
	CertDomains []string
}

func (s tsStatus) dnsName() string {
	if s.Self == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s.Self.DNSName), "."))
}

func (s tsStatus) hasCap(c string) bool {
	if s.Self == nil {
		return false
	}
	if _, ok := s.Self.CapMap[c]; ok {
		return true
	}
	for _, have := range s.Self.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

// The node attributes the tailscale CLI itself checks before serving
// (tailcfg/nodecap: HTTPS and Funnel).
func (s tsStatus) httpsEnabled() bool  { return s.hasCap("https") || len(s.CertDomains) > 0 }
func (s tsStatus) funnelEnabled() bool { return s.hasCap("funnel") }

// cli runs the tailscale CLI against this daemon's socket.
func (t *Tailscale) cli(ctx context.Context, timeout time.Duration, args ...string) (stdout, all string, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.Bin, append([]string{"--socket=" + t.socket()}, args...)...)
	cmd.Env = baseEnv()
	cmd.Dir = os.TempDir()
	setGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			killGroup(cmd.Process.Pid)
		}
		return nil
	}
	cmd.WaitDelay = time.Second
	var mu sync.Mutex
	var out, both bytes.Buffer
	cmd.Stdout = &capped{mu: &mu, to: []*bytes.Buffer{&out, &both}}
	cmd.Stderr = &capped{mu: &mu, to: []*bytes.Buffer{&both}}
	err = cmd.Run()
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		err = fmt.Errorf("tailscale is not installed (looked for %q)", t.Bin)
	}
	mu.Lock()
	defer mu.Unlock()
	return out.String(), both.String(), err
}

// capped writes to several buffers, each capped at 256 KB.
type capped struct {
	mu *sync.Mutex
	to []*bytes.Buffer
}

func (w *capped) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range w.to {
		if room := 256<<10 - b.Len(); room > 0 {
			b.Write(p[:min(len(p), room)])
		}
	}
	return len(p), nil
}

// query asks the daemon where it stands. `tailscale status` exits 1 while
// signed out but still prints the JSON.
func (t *Tailscale) query(ctx context.Context) (tsStatus, error) {
	out, all, err := t.cli(ctx, 5*time.Second, "status", "--json", "--peers=false")
	var st tsStatus
	if jerr := json.Unmarshal([]byte(out), &st); jerr != nil || st.BackendState == "" {
		if err == nil {
			err = errors.New("unexpected output from tailscale status")
		}
		return tsStatus{}, fmt.Errorf("%w: %s", err, lastLine(all))
	}
	return st, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 200 {
		l = l[:200] + "…"
	}
	return l
}

func (t *Tailscale) setProblem(p string) {
	t.mu.Lock()
	changed := t.problem != p
	t.problem = p
	t.mu.Unlock()
	if changed && p != "" {
		t.logf("tunnel: tailscale: %s", p)
	}
}

// login signs the node in with the auth key, which reaches the CLI in a
// 0600 file inside the state directory (deleted straight after), never in
// its arguments or environment. It returns what to tell the owner, or "".
func (t *Tailscale) login(ctx context.Context, set Settings) string {
	keyFile := filepath.Join(t.StateDir, "authkey")
	if err := os.WriteFile(keyFile, []byte(set.Token+"\n"), 0o600); err != nil {
		return "The auth key couldn't be handed to tailscale: " + err.Error()
	}
	defer os.Remove(keyFile)
	_, all, err := t.cli(ctx, 75*time.Second, "up", "--auth-key=file:"+keyFile, "--hostname="+set.Hostname, "--reset", "--timeout=60s")
	if err == nil {
		t.logf("tunnel: tailscale: signed in to the tailnet")
		return ""
	}
	if ctx.Err() != nil {
		return ""
	}
	all = strings.ReplaceAll(all, set.Token, "[auth key]")
	if p := TailscaleProblem(all); p != "" && p != MsgTailscaleNeedsKey {
		return p
	}
	if strings.Contains(strings.ToLower(all), "timeout") || errors.Is(err, context.DeadlineExceeded) {
		return "Tailscale didn't answer in time while signing in. Check that this server can reach the internet, then turn remote access off and on."
	}
	return "Signing in to your tailnet failed: " + lastLine(all)
}

// control brings the node up and keeps serve/funnel matched to its state
// until ctx ends.
func (t *Tailscale) control(ctx context.Context, set Settings) {
	poll := t.Poll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	var (
		started     = time.Now()
		signedOut   int  // consecutive polls that said NeedsLogin
		triedKey    bool // one attempt per start: auth keys are often single-use
		triedUp     bool
		setHostname bool
		announced   string
		served      string // the DNS name serve was started for
	)
	stopServe := func() {
		t.mu.Lock()
		s := t.serve
		t.serve = nil
		t.mu.Unlock()
		if s != nil {
			s.Stop()
		}
		served = ""
	}
	defer stopServe()
	for {
		st, err := t.query(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := poll
		switch {
		case err != nil:
			stopServe()
			if time.Since(started) > 20*time.Second {
				t.setProblem("tailscaled isn't answering: " + err.Error())
			}
		case st.BackendState == "NeedsLogin":
			stopServe()
			t.mu.Lock()
			t.node = ""
			t.mu.Unlock()
			if signedOut++; signedOut < 2 {
				break // it can say so for a moment while starting
			}
			switch {
			case triedKey:
			case set.Token == "":
				t.setProblem(MsgTailscaleNeedsKey)
			default:
				triedKey = true
				t.setProblem(t.login(ctx, set))
				signedOut = 0
			}
		case st.BackendState == "NeedsMachineAuth":
			stopServe()
			t.setProblem(MsgTailscaleApproval)
		case st.BackendState == "Stopped":
			stopServe()
			if !triedUp {
				triedUp = true
				_, _, _ = t.cli(ctx, 45*time.Second, "up", "--hostname="+set.Hostname, "--reset", "--timeout=30s")
			}
		case st.BackendState == "Running":
			signedOut = 0
			dns := st.dnsName()
			if dns == "" {
				break
			}
			if !setHostname {
				setHostname = true
				if st.Self != nil && !strings.EqualFold(st.Self.HostName, set.Hostname) {
					_, _, _ = t.cli(ctx, 20*time.Second, "set", "--hostname="+set.Hostname)
					break // read the new name on the next poll
				}
			}
			t.mu.Lock()
			t.node = dns
			t.mu.Unlock()
			if announced != dns {
				announced = dns
				t.logf("tunnel: tailscale node %s", dns)
				if t.OnNode != nil {
					t.OnNode(dns)
				}
			}
			if !st.httpsEnabled() {
				stopServe()
				t.setProblem(MsgTailscaleHTTPS)
				break
			}
			if set.Mode == ModeTailscaleFunnel && !st.funnelEnabled() {
				stopServe()
				t.setProblem(MsgTailscaleFunnel)
				break
			}
			t.setProblem("")
			if served != "" && served != dns {
				stopServe() // the node was renamed
			}
			if served == "" {
				// Anything left in the stored serve config would outlive this run.
				_, _, _ = t.cli(ctx, 10*time.Second, "serve", "reset")
				if ctx.Err() != nil {
					return
				}
				verb, ready := "serve", "Available within your tailnet"
				if set.Mode == ModeTailscaleFunnel {
					verb, ready = "funnel", "Available on the internet"
				}
				s := &Supervisor{Bin: t.Bin, PIDFile: t.pidFile(TailscaleServePIDFile), Logf: t.Logf, Backoff: t.Backoff, Allow: t.Allow,
					Provider: staticProvider{name: "tailscale " + verb,
						cmd:   Command{Args: []string{"--socket=" + t.socket(), verb, "--https=443", set.Origin}, Address: "https://" + dns},
						ready: func(line string) bool { return strings.Contains(line, ready) }}}
				if err := s.Start(Settings{Token: set.Token}); err != nil {
					t.setProblem(err.Error())
					break
				}
				t.mu.Lock()
				t.serve = s
				t.mu.Unlock()
				served = dns
			}
			wait = 10 * poll
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Forget signs the node out of the tailnet and deletes its state, so the
// next start needs a new auth key. loggedOut is false when Tailscale
// couldn't be told (offline, or the binaries are missing): the machine then
// has to be removed in the admin console by hand. The state is deleted
// either way.
func (t *Tailscale) Forget(ctx context.Context) (loggedOut bool, err error) {
	t.Stop()
	if _, serr := os.Stat(filepath.Join(t.StateDir, "tailscaled.state")); serr == nil {
		loggedOut = t.logout(ctx)
	} else {
		loggedOut = true // never signed in from here
	}
	if t.StateDir != "" {
		err = os.RemoveAll(t.StateDir)
	}
	t.removeTempSocket()
	return loggedOut, err
}

// logout runs tailscaled just long enough for `tailscale logout`.
func (t *Tailscale) logout(ctx context.Context) bool {
	if err := t.prepare(); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c := t.daemonCommand()
	cmd := exec.Command(t.DaemonBin, c.Args...)
	cmd.Env = append(baseEnv(), c.Env...)
	cmd.Dir = os.TempDir()
	setGroup(cmd)
	if err := cmd.Start(); err != nil {
		return false
	}
	defer func() {
		killGroup(cmd.Process.Pid)
		_ = cmd.Wait()
	}()
	poll := t.Poll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	for ctx.Err() == nil {
		st, err := t.query(ctx)
		switch {
		case err != nil, st.BackendState == "NoState", st.BackendState == "Starting":
		case st.BackendState == "NeedsLogin":
			return true // nothing to sign out
		default:
			_, _, err := t.cli(ctx, 20*time.Second, "logout")
			return err == nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
	return false
}
