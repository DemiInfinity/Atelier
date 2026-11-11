package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Phases of the helper process.
const (
	PhaseOff        = "off"
	PhaseStarting   = "starting"   // running, not yet connected to the edge
	PhaseConnected  = "connected"  // the edge has registered a connection
	PhaseRestarting = "restarting" // it exited; waiting to start it again
)

// Status is a snapshot of the supervisor.
type Status struct {
	Phase     string
	Address   string // https://… once known
	Since     time.Time
	LastError string
	Restarts  int
	PID       int
	Log       []string // the helper's most recent output lines
}

// Supervisor runs one helper process at a time, restarts it with backoff
// when it exits, and kills its whole process group on Stop.
type Supervisor struct {
	Bin      string // the helper binary, e.g. "cloudflared"
	Provider Provider
	// PIDFile, when set, holds the running helper's pid so that
	// `atelier tunnel off` (a separate process) can kill it too.
	PIDFile string
	Logf    func(format string, args ...any)
	// Backoff says how long to wait before restart n (1-based).
	Backoff func(n int) time.Duration
	// Allow, when set, is asked before every restart; false means wait
	// instead (e.g. the setting was turned off from the shell, which also
	// killed the helper, and the server hasn't caught up yet).
	Allow func() bool

	mu     sync.Mutex
	st     Status
	cancel context.CancelFunc
	done   chan struct{}
	pid    int
	// fixedAddr: the address comes from the settings (named tunnels), not the log.
	fixedAddr bool
}

const (
	keepLines   = 30
	maxLineLen  = 300
	stableAfter = time.Minute // a run this long resets the backoff
)

// DefaultBackoff is 1s, 2s, 4s … capped at a minute.
func DefaultBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > 7 {
		return time.Minute
	}
	return time.Second << (n - 1)
}

func (s *Supervisor) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Status returns a copy of the current state.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	if st.Phase == "" {
		st.Phase = PhaseOff
	}
	st.Log = append([]string(nil), s.st.Log...)
	return st
}

// Running reports whether the supervisor is active (in any phase but off).
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel != nil
}

// Start (re)starts the helper with these settings. A running helper is
// stopped first.
func (s *Supervisor) Start(set Settings) error {
	if s.Provider == nil {
		return errors.New("tunnel: no provider")
	}
	cmd, err := s.Provider.Command(set)
	if err != nil {
		return err
	}
	s.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.mu.Lock()
	s.cancel, s.done = cancel, done
	s.fixedAddr = cmd.Address != ""
	s.st = Status{Phase: PhaseStarting, Address: cmd.Address, Since: time.Now()}
	s.mu.Unlock()
	go func() {
		defer close(done)
		s.loop(ctx, cmd, set.Token)
	}()
	return nil
}

// Stop kills the helper's process group at once (SIGKILL: cloudflared's
// graceful shutdown would keep serving for up to 30 seconds) and waits for
// the supervisor loop to finish.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	cancel, done, pid := s.cancel, s.done, s.pid
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if pid > 0 {
		killGroup(pid)
	}
	<-done
	s.mu.Lock()
	s.st = Status{Phase: PhaseOff, Since: time.Now()}
	s.mu.Unlock()
}

func (s *Supervisor) loop(ctx context.Context, c Command, secret string) {
	backoff := s.Backoff
	if backoff == nil {
		backoff = DefaultBackoff
	}
	failures := 0
	for {
		started := time.Now()
		err := s.runOnce(ctx, c, secret)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= stableAfter {
			failures = 0
		}
		failures++
		wait := backoff(failures)
		msg := "the tunnel helper stopped"
		if err != nil {
			msg = err.Error()
		}
		s.logf("tunnel: %s; restarting in %s", msg, wait)
		s.mu.Lock()
		s.st.Phase = PhaseRestarting
		s.st.LastError = msg
		s.st.Restarts++
		s.st.PID = 0
		if c.Address == "" {
			s.st.Address = "" // a quick tunnel gets a new address each start
		}
		s.mu.Unlock()
		for {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			if s.Allow == nil || s.Allow() {
				break
			}
			wait = time.Second
		}
	}
}

// runOnce runs the helper until it exits or ctx is cancelled.
func (s *Supervisor) runOnce(ctx context.Context, c Command, secret string) error {
	cmd := exec.Command(s.Bin, c.Args...)
	cmd.Env = append(baseEnv(), c.Env...)
	cmd.Dir = os.TempDir()
	setGroup(cmd)
	// A real pipe (not an io.Writer) so Wait returns when the helper exits,
	// even if something it started still holds the write end.
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pr.Close()
	cmd.Stdout, cmd.Stderr = pw, pw
	err = cmd.Start()
	pw.Close()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s is not installed (looked for %q)", s.Provider.Name()+" helper", s.Bin)
		}
		return err
	}
	pid := cmd.Process.Pid
	s.mu.Lock()
	s.pid = pid
	s.st.PID = pid
	s.st.Phase = PhaseStarting
	s.st.Since = time.Now()
	s.mu.Unlock()
	s.writePID(pid)
	// Stop may have been called between Start and here: it saw no pid.
	if ctx.Err() != nil {
		killGroup(pid)
	}

	read := make(chan struct{})
	go func() {
		defer close(read)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			s.line(sc.Text(), secret)
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	err = cmd.Wait()
	killGroup(pid) // anything the helper left behind in its group
	<-read
	s.mu.Lock()
	if s.pid == pid {
		s.pid = 0
	}
	s.mu.Unlock()
	s.removePID(pid)
	if err != nil {
		return fmt.Errorf("the tunnel helper exited: %v", err)
	}
	return errors.New("the tunnel helper exited")
}

func (s *Supervisor) line(l, secret string) {
	if secret != "" {
		l = strings.ReplaceAll(l, secret, "[token]")
	}
	if len(l) > maxLineLen {
		l = l[:maxLineLen] + "…"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Log = append(s.st.Log, l)
	fixed := s.fixedAddr
	if len(s.st.Log) > keepLines {
		s.st.Log = s.st.Log[len(s.st.Log)-keepLines:]
	}
	if addr, ok := s.Provider.Address(l); ok && !fixed && s.st.Address != addr {
		s.st.Address = addr
		s.logf("tunnel: address %s", addr)
	}
	if s.Provider.Ready(l) && s.st.Phase != PhaseConnected {
		s.st.Phase = PhaseConnected
		s.st.Since = time.Now()
	}
}

// baseEnv is the helper's environment: never Atelier's own, which holds
// secrets (ATELIER_SECRET_KEY, OAuth and S3 keys) the helper has no use for.
func baseEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "HOME", "TZ", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (s *Supervisor) writePID(pid int) {
	if s.PIDFile == "" {
		return
	}
	tmp := s.PIDFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)+"\n"), 0o600); err == nil {
		_ = os.Rename(tmp, s.PIDFile)
	}
}

func (s *Supervisor) removePID(pid int) {
	if s.PIDFile == "" {
		return
	}
	if b, err := os.ReadFile(s.PIDFile); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(pid) {
		_ = os.Remove(s.PIDFile)
	}
}

// KillFromPIDFile kills the helper recorded in a PID file, for
// `atelier tunnel off` run beside the server (docker exec). It only kills a
// process whose command line mentions want (e.g. "cloudflared"), so a stale
// file can't take down an unrelated process that reused the pid. It returns
// whether something was killed.
func KillFromPIDFile(path, want string) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		_ = os.Remove(path)
		return false, nil
	}
	cl, ok := commandLine(pid)
	if !ok || !strings.Contains(cl, filepath.Base(want)) {
		_ = os.Remove(path)
		return false, nil
	}
	killGroup(pid)
	_ = os.Remove(path)
	return true, nil
}
