package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"atelier/internal/actions"
)

// The local control channel (docs/app-api.md, section 10): while
// `atelier-runner run` is active it listens on a Unix socket next to its
// config (a named pipe on Windows) so a desktop app on this device can show
// and steer the runner. One JSON object per line, request then response.
//
// It only reports and changes the runner's own state: it never runs a
// command and never returns the runner token, a job token or a secret.

const (
	controlSocketName = "control.sock"
	controlMaxLine    = 8 << 10 // a request is one short JSON object
	controlMaxConns   = 16
	controlTailKeep   = 500  // log lines kept per running job
	controlTailLine   = 2000 // bytes kept of one log line
	controlMaxParall  = 64
)

var (
	// controlIdle is how long a connection may sit without a full request.
	controlIdle = 60 * time.Second
	// controlBusyWait is how long a client waits for a free connection slot
	// before it is told "too many connections".
	controlBusyWait = 2 * time.Second
	// controlWrite bounds writing one response.
	controlWrite = 10 * time.Second
)

// ErrNotRunning: nothing answers on the control channel.
var ErrNotRunning = errors.New("not running")

// errControlInUse: another runner already serves this control channel.
var errControlInUse = errors.New("another atelier-runner is already running with this config")

// controlListener is the platform's listener (control_unix.go, control_windows.go).
type controlListener interface {
	Accept() (io.ReadWriteCloser, error)
	Close() error
}

// ---------- state kept for the channel ----------

type controlState struct {
	once  sync.Once
	wakeC chan struct{}

	running    atomic.Int32 // jobs started and not yet finished
	jobsOnline atomic.Bool  // the last job poll succeeded
	wsOnline   atomic.Bool  // the last workspace poll succeeded

	mu              sync.Mutex
	live            map[int64]*liveJob
	pollCancel      context.CancelFunc
	pollInterrupted bool
	parallel        int // set over the channel; 0: Runner.Parallel
	battAt          time.Time
	battOn          bool

	setMu sync.Mutex // one `set` at a time
}

func (c *controlState) wakeCh() chan struct{} {
	c.once.Do(func() { c.wakeC = make(chan struct{}, 1) })
	return c.wakeC
}

// wake makes the poll loop look at its state again.
func (c *controlState) wake() {
	select {
	case c.wakeCh() <- struct{}{}:
	default:
	}
}

// liveJob is what the channel shows of a running job.
type liveJob struct {
	id       int64
	repo     string
	workflow string
	kind     string // "job" or "agent"
	started  time.Time
	mask     *Masker
	cancel   func()

	mu     sync.Mutex
	detail string
	ring   []string
	next   int
}

func (c *controlState) addJob(spec *JobSpec, m *Masker, started time.Time, cancel func()) *liveJob {
	lj := &liveJob{id: spec.ID, repo: spec.Repo.Owner + "/" + spec.Repo.Name, kind: "job", started: started,
		mask: m, cancel: cancel, detail: "setting up"}
	lj.workflow = strings.TrimSuffix(path.Base(filepath.ToSlash(spec.Workflow)), path.Ext(spec.Workflow))
	for _, s := range spec.Steps {
		if s.Agent != nil {
			lj.kind = "agent"
			if s.Agent.WorkflowName != "" {
				lj.workflow = s.Agent.WorkflowName
			}
			break
		}
	}
	c.mu.Lock()
	if c.live == nil {
		c.live = map[int64]*liveJob{}
	}
	c.live[spec.ID] = lj
	c.mu.Unlock()
	return lj
}

func (c *controlState) removeJob(id int64) {
	c.mu.Lock()
	delete(c.live, id)
	c.mu.Unlock()
}

func (c *controlState) job(id int64) *liveJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live[id]
}

func (c *controlState) jobs() []*liveJob {
	c.mu.Lock()
	out := make([]*liveJob, 0, len(c.live))
	for _, lj := range c.live {
		out = append(out, lj)
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b *liveJob) int {
		if d := a.started.Compare(b.started); d != 0 {
			return d
		}
		return int(a.id - b.id)
	})
	return out
}

// setDetail records what the job is doing now (a step name, an agent turn).
func (l *liveJob) setDetail(s string) {
	if l == nil {
		return
	}
	s = clipLine(l.mask.Mask(s), 200)
	l.mu.Lock()
	l.detail = s
	l.mu.Unlock()
}

// addLine keeps an already masked log line for `tail`.
func (l *liveJob) addLine(text string) {
	if l == nil {
		return
	}
	text = clipLine(text, controlTailLine)
	l.mu.Lock()
	if len(l.ring) < controlTailKeep {
		l.ring = append(l.ring, text)
	} else {
		l.ring[l.next] = text
		l.next = (l.next + 1) % controlTailKeep
	}
	l.mu.Unlock()
}

// tail returns the last n lines, oldest first.
func (l *liveJob) tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	all := make([]string, 0, len(l.ring))
	all = append(all, l.ring[l.next:]...)
	all = append(all, l.ring[:l.next]...)
	if n < len(all) {
		all = all[len(all)-n:]
	}
	return all
}

func clipLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && s[len(s)-1]&0xC0 == 0x80 { // don't cut a rune in half
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] >= 0xC0 {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// ---------- what the poll loop asks ----------

func (r *Runner) maxParallel() int {
	r.ctl.mu.Lock()
	p := r.ctl.parallel
	r.ctl.mu.Unlock()
	if p > 0 {
		return p
	}
	return max(r.Parallel, 1)
}

func (r *Runner) slotsFull() bool { return int(r.ctl.running.Load()) >= r.maxParallel() }

// busy: no new job should be taken now.
func (r *Runner) busy() bool { return r.slotsFull() || r.pauseReason() != "" }

// waitSlot waits for a free slot (or for shutdown).
func (r *Runner) waitSlot(ctx context.Context) {
	for r.slotsFull() {
		select {
		case <-ctx.Done():
			return
		case <-r.ctl.wakeCh():
		case <-time.After(time.Second):
		}
	}
}

// pauseReason is "" while the runner takes jobs, else "user" (paused over
// the channel or in the config) or "battery" (the pause_on_battery rule).
func (r *Runner) pauseReason() string {
	r.Config.mu.RLock()
	paused, onBatt := r.Config.Paused, r.Config.PauseOnBattery
	r.Config.mu.RUnlock()
	switch {
	case paused:
		return "user"
	case onBatt && r.onBattery():
		return "battery"
	}
	return ""
}

// batteryProbe reports whether this device runs on battery (battery_*.go).
var batteryProbe = deviceOnBattery

// batteryEvery is how long a battery reading is reused.
var batteryEvery = 15 * time.Second

func (r *Runner) onBattery() bool {
	c := &r.ctl
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.battAt.IsZero() || time.Since(c.battAt) > batteryEvery {
		c.battOn, c.battAt = batteryProbe(), time.Now()
	}
	return c.battOn
}

// pollContext is the context of one job poll. A control change cancels it,
// so a pause takes effect at once instead of after the long poll; the
// returned func ends the poll and reports whether that happened.
func (r *Runner) pollContext(ctx context.Context, busy bool) (context.Context, func() bool) {
	c := &r.ctl
	pctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.pollCancel, c.pollInterrupted = cancel, false
	c.mu.Unlock()
	if !busy && r.busy() { // changed between the check and here
		r.interruptPoll()
	}
	return pctx, func() bool {
		c.mu.Lock()
		was := c.pollInterrupted
		c.pollCancel = nil
		c.mu.Unlock()
		cancel()
		return was
	}
}

func (r *Runner) interruptPoll() {
	c := &r.ctl
	c.mu.Lock()
	if c.pollCancel != nil {
		c.pollInterrupted = true
		c.pollCancel()
	}
	c.mu.Unlock()
	c.wake()
}

// ---------- requests ----------

type controlRules struct {
	PauseOnBattery *bool `json:"pause_on_battery"`
	MaxParallel    *int  `json:"max_parallel"`
	ContainerJobs  *bool `json:"container_jobs"`
}

type controlRequest struct {
	Op         string        `json:"op"`
	TakingJobs *bool         `json:"taking_jobs"`
	Rules      *controlRules `json:"rules"`
	JobID      int64         `json:"job_id"`
	Lines      *int          `json:"lines"`
}

// ControlRulesStatus are the rules as `status` reports them.
type ControlRulesStatus struct {
	PauseOnBattery bool `json:"pause_on_battery"`
	MaxParallel    int  `json:"max_parallel"`
	ContainerJobs  bool `json:"container_jobs"`
}

// ControlJob is a running job in `status`.
type ControlJob struct {
	JobID     int64     `json:"job_id"`
	Repo      string    `json:"repo"`
	Workflow  string    `json:"workflow"`
	Kind      string    `json:"kind"`
	StartedAt time.Time `json:"started_at"`
	Detail    string    `json:"detail"`
}

// ControlStatus is the reply to `status` and `set`.
type ControlStatus struct {
	OK           bool               `json:"ok"`
	Error        string             `json:"error,omitempty"`
	Name         string             `json:"name"`
	Version      string             `json:"version"`
	Server       string             `json:"server"`
	Online       bool               `json:"online"`
	TakingJobs   bool               `json:"taking_jobs"`
	PausedReason *string            `json:"paused_reason"`
	Capabilities []string           `json:"capabilities"`
	Rules        ControlRulesStatus `json:"rules"`
	Current      []ControlJob       `json:"current"`
}

type controlError struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func controlErr(format string, a ...any) controlError {
	return controlError{Error: fmt.Sprintf(format, a...)}
}

type controlWorkspace struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Dirty  bool   `json:"dirty"`
}

// redact hides the runner token should it ever show up in a reply.
func (r *Runner) redact(s string) string {
	if t := r.Config.RunnerToken; t != "" && strings.Contains(s, t) {
		return strings.ReplaceAll(s, t, "***")
	}
	return s
}

func (r *Runner) controlStatus() ControlStatus {
	cfg := r.Config
	jobsOn := cfg.HasCap(actions.CapJobs)
	st := ControlStatus{OK: true, Name: cfg.Name, Version: r.Version, Server: cfg.ServerURL,
		Capabilities: cfg.AdvertisedCaps(), Current: []ControlJob{}}
	if st.Capabilities == nil {
		st.Capabilities = []string{}
	}
	if jobsOn {
		st.Online = r.ctl.jobsOnline.Load()
	} else {
		st.Online = r.ctl.wsOnline.Load()
	}
	reason := r.pauseReason()
	if reason == "" && !jobsOn {
		reason = "jobs_off"
	}
	st.TakingJobs = reason == ""
	if reason != "" {
		st.PausedReason = &reason
	}
	cfg.mu.RLock()
	st.Rules.PauseOnBattery = cfg.PauseOnBattery
	cfg.mu.RUnlock()
	st.Rules.MaxParallel = r.maxParallel()
	st.Rules.ContainerJobs = cfg.HasCap(actions.CapDocker)
	for _, lj := range r.ctl.jobs() {
		lj.mu.Lock()
		d := lj.detail
		lj.mu.Unlock()
		st.Current = append(st.Current, ControlJob{JobID: lj.id, Repo: lj.repo, Workflow: lj.workflow, Kind: lj.kind,
			StartedAt: lj.started, Detail: r.redact(d)})
	}
	return st
}

// changeConfig applies a change to the config file (re-read first, so
// changes made by other commands while the runner runs are kept) and then to
// the running config.
func (r *Runner) changeConfig(apply func(*Config)) error {
	if r.ConfigPath != "" {
		disk, err := LoadConfig(r.ConfigPath)
		if err != nil {
			return err
		}
		apply(disk)
		if err := disk.Save(r.ConfigPath); err != nil {
			return err
		}
	}
	r.Config.mu.Lock()
	apply(r.Config)
	r.Config.mu.Unlock()
	return nil
}

func (r *Runner) controlSet(req *controlRequest) any {
	rules := req.Rules
	if rules == nil {
		rules = &controlRules{}
	}
	if req.TakingJobs == nil && rules.PauseOnBattery == nil && rules.MaxParallel == nil && rules.ContainerJobs == nil {
		return controlErr("set: nothing to change (taking_jobs, rules.pause_on_battery, rules.max_parallel, rules.container_jobs)")
	}
	if p := rules.MaxParallel; p != nil && (*p < 1 || *p > controlMaxParall) {
		return controlErr("set: max_parallel must be between 1 and %d", controlMaxParall)
	}
	r.ctl.setMu.Lock()
	defer r.ctl.setMu.Unlock()
	err := r.changeConfig(func(c *Config) {
		if req.TakingJobs != nil {
			c.Paused = !*req.TakingJobs
		}
		if rules.PauseOnBattery != nil {
			c.PauseOnBattery = *rules.PauseOnBattery
		}
		if rules.MaxParallel != nil {
			c.Parallel = *rules.MaxParallel
		}
		if rules.ContainerJobs != nil {
			has := slices.Contains(c.Capabilities, actions.CapDocker)
			switch {
			case *rules.ContainerJobs && !has:
				c.Capabilities = append(slices.Clone(c.Capabilities), actions.CapDocker)
			case !*rules.ContainerJobs && has:
				c.Capabilities = slices.DeleteFunc(slices.Clone(c.Capabilities), func(s string) bool { return s == actions.CapDocker })
			}
		}
	})
	if err != nil {
		return controlErr("set: couldn't save the runner config: %s", r.redact(err.Error()))
	}
	r.ctl.mu.Lock()
	if rules.MaxParallel != nil {
		r.ctl.parallel = *rules.MaxParallel
	}
	r.ctl.battAt = time.Time{} // read the battery again
	r.ctl.mu.Unlock()
	r.logger().Info("settings changed over the control channel", "taking_jobs", !r.Config.pausedNow(),
		"max_parallel", r.maxParallel(), "container_jobs", r.Config.HasCap(actions.CapDocker))
	r.interruptPoll()
	return r.controlStatus()
}

func (c *Config) pausedNow() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Paused
}

func (r *Runner) controlWorkspaces(ctx context.Context) any {
	p := WorkspaceConfigPath
	if p == "" && r.ConfigPath != "" {
		p = filepath.Join(filepath.Dir(r.ConfigPath), "workspaces.json")
	}
	if p == "" {
		var err error
		if p, err = DefaultWorkspaceConfigPath(); err != nil {
			return controlErr("workspaces: no config folder")
		}
	}
	wc, err := LoadWorkspaceConfig(p)
	if err != nil {
		return controlErr("workspaces: %s", err.Error())
	}
	root, err := wc.RootDir()
	if err != nil {
		return controlErr("workspaces: %s", err.Error())
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real // the spelling the workspace loop reports
	}
	presets := wc.PresetNames()
	if presets == nil {
		presets = []string{}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	items := []controlWorkspace{}
	for _, w := range ListWorkspaces(ctx, root) {
		items = append(items, controlWorkspace{Repo: w.Owner + "/" + w.Repo, Path: w.Path, Branch: w.Branch, Dirty: w.Dirty})
	}
	return struct {
		OK      bool               `json:"ok"`
		Root    string             `json:"root"`
		Presets []string           `json:"presets"`
		Items   []controlWorkspace `json:"items"`
	}{true, root, presets, items}
}

// handleControl answers one request line.
func (r *Runner) handleControl(ctx context.Context, line []byte) (resp any) {
	defer func() {
		if p := recover(); p != nil {
			r.logger().Error("control request failed", "panic", fmt.Sprint(p))
			resp = controlErr("internal error")
		}
	}()
	var req controlRequest
	if err := json.Unmarshal(line, &req); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return controlErr("bad request: %s has the wrong type", te.Field)
		}
		return controlErr("bad request: send one JSON object per line")
	}
	switch req.Op {
	case "status":
		return r.controlStatus()
	case "set":
		return r.controlSet(&req)
	case "tail":
		lj := r.ctl.job(req.JobID)
		if lj == nil {
			return controlErr("tail: job %d isn't running on this runner", req.JobID)
		}
		n := 50
		if req.Lines != nil {
			n = *req.Lines
		}
		n = min(max(n, 0), controlTailKeep)
		lines := lj.tail(n)
		for i, l := range lines {
			lines[i] = r.redact(l)
		}
		return struct {
			OK    bool     `json:"ok"`
			Lines []string `json:"lines"`
		}{true, lines}
	case "cancel":
		lj := r.ctl.job(req.JobID)
		if lj == nil {
			return controlErr("cancel: job %d isn't running on this runner", req.JobID)
		}
		r.logger().Info("job cancelled over the control channel", "job", req.JobID)
		lj.cancel()
		return struct {
			OK bool `json:"ok"`
		}{true}
	case "workspaces":
		return r.controlWorkspaces(ctx)
	case "":
		return controlErr("bad request: op is missing")
	}
	return controlErr("unknown op %q (status, set, tail, cancel, workspaces)", clipLine(req.Op, 40))
}

// ---------- the server ----------

type controlServer struct {
	r      *Runner
	l      controlListener
	ctx    context.Context
	cancel context.CancelFunc
	slots  chan struct{} // connections being served
	// pending bounds the connections accepted in all, served or waiting for a slot
	pending  chan struct{}
	idle     time.Duration
	busyWait time.Duration
	wg       sync.WaitGroup

	mu    sync.Mutex
	conns map[io.Closer]struct{}
}

// StartControl starts the control channel for the config at r.ConfigPath.
// The returned func stops it and removes the socket.
func (r *Runner) StartControl() (stop func(), err error) {
	if r.ConfigPath == "" {
		return nil, errors.New("control channel: no config path")
	}
	l, err := listenControl(r.ConfigPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &controlServer{r: r, l: l, ctx: ctx, cancel: cancel, slots: make(chan struct{}, controlMaxConns),
		pending: make(chan struct{}, 4*controlMaxConns), idle: controlIdle, busyWait: controlBusyWait, conns: map[io.Closer]struct{}{}}
	s.wg.Add(1)
	go s.acceptLoop()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			_ = l.Close()
			s.mu.Lock()
			for c := range s.conns {
				_ = c.Close()
			}
			s.mu.Unlock()
			s.wg.Wait()
		})
	}, nil
}

func (s *controlServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.l.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			s.r.logger().Warn("control channel: accept failed", "err", err)
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		select {
		case s.pending <- struct{}{}:
		default:
			_ = conn.Close() // a flood: not even worth a reply
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { <-s.pending }()
			defer conn.Close()
			// a burst of short-lived clients may have to wait a moment for a slot
			t := time.NewTimer(s.busyWait)
			defer t.Stop()
			select {
			case s.slots <- struct{}{}:
			case <-s.ctx.Done():
				return
			case <-t.C:
				stop := closeAfter(conn, controlWrite)
				_ = writeControl(conn, controlErr("too many connections"))
				stop()
				return
			}
			defer func() { <-s.slots }()
			s.mu.Lock()
			s.conns[conn] = struct{}{}
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				delete(s.conns, conn)
				s.mu.Unlock()
			}()
			if s.ctx.Err() == nil {
				s.serve(conn)
			}
		}()
	}
}

// closeAfter closes c after d unless the returned func is called first: a
// timeout that works on sockets and pipes alike.
func closeAfter(c io.Closer, d time.Duration) (stop func()) {
	t := time.AfterFunc(d, func() { _ = c.Close() })
	return func() { t.Stop() }
}

func writeControl(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"ok":false,"error":"internal error"}`)
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func (s *controlServer) serve(conn io.ReadWriteCloser) {
	br := bufio.NewReaderSize(conn, controlMaxLine)
	reply := func(v any) bool {
		stop := closeAfter(conn, controlWrite)
		defer stop()
		return writeControl(conn, v) == nil
	}
	for s.ctx.Err() == nil {
		stop := closeAfter(conn, s.idle)
		line, err := br.ReadSlice('\n')
		stop()
		if errors.Is(err, bufio.ErrBufferFull) {
			reply(controlErr("request too large (limit %d bytes)", controlMaxLine))
			// read a little more so the client sees the reply, not a reset
			stop := closeAfter(conn, time.Second)
			_, _ = io.CopyN(io.Discard, br, 1<<20)
			stop()
			return
		}
		last := err != nil // EOF or timeout: answer a final unterminated line, then stop
		if t := strings.TrimSpace(string(line)); t != "" {
			if !reply(s.r.handleControl(s.ctx, []byte(t))) {
				return
			}
		}
		if last {
			return
		}
	}
}

// ---------- the client ----------

// ControlAddress is where the control channel of a config lives: the socket
// path, or the pipe name on Windows.
func ControlAddress(cfgPath string) string { return controlAddress(cfgPath) }

// ControlRequest sends one request to the runner using cfgPath and returns
// its reply line. The error is ErrNotRunning when nothing answers.
func ControlRequest(cfgPath string, req any, timeout time.Duration) (json.RawMessage, error) {
	conn, err := dialControl(cfgPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := closeAfter(conn, timeout)
	defer stop()
	if err := writeControl(conn, req); err != nil {
		return nil, fmt.Errorf("control channel: %w", err)
	}
	line, err := bufio.NewReaderSize(conn, 64<<10).ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return nil, fmt.Errorf("control channel: no reply: %w", err)
	}
	line = []byte(strings.TrimSpace(string(line)))
	if !json.Valid(line) {
		return nil, errors.New("control channel: the reply wasn't JSON")
	}
	return line, nil
}
