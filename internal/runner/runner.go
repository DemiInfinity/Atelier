package runner

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"time"

	"atelier/internal/actions"
)

// Runner polls the server and runs jobs.
type Runner struct {
	Config  *Config
	Client  *Client
	Version string
	Log     *slog.Logger
	// Parallel is how many jobs run at once (default 1).
	Parallel int
	// Grace is how long running jobs may keep going after shutdown starts
	// before they are cancelled (default 0: cancel at once).
	Grace time.Duration
	// HeartbeatEvery is the busy heartbeat interval (default 15s).
	HeartbeatEvery time.Duration
	// DiscoveryInterval overrides the 10-minute discovery interval (tests).
	DiscoveryInterval time.Duration
	// MaxBackoff caps the error backoff (default 60s).
	MaxBackoff time.Duration

	// ConfigPath is the config file; changes made over the control channel
	// (control.go) are persisted there. Empty: they only last until exit.
	ConfigPath string

	fetchMu sync.Mutex
	jobsMu  sync.Mutex
	jobs    map[int64]context.CancelCauseFunc
	ctl     controlState // local control channel state (control.go)
}

func (r *Runner) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Runner) trackJob(id int64, cancel context.CancelCauseFunc) {
	r.jobsMu.Lock()
	defer r.jobsMu.Unlock()
	if r.jobs == nil {
		r.jobs = map[int64]context.CancelCauseFunc{}
	}
	r.jobs[id] = cancel
}

func (r *Runner) untrackJob(id int64) {
	r.jobsMu.Lock()
	defer r.jobsMu.Unlock()
	delete(r.jobs, id)
}

func (r *Runner) cancelAll(cause error) {
	r.jobsMu.Lock()
	defer r.jobsMu.Unlock()
	for _, c := range r.jobs {
		c(cause)
	}
}

// backoff returns a full-jitter exponential delay for attempt n (n ≥ 1).
func backoff(n int, max time.Duration) time.Duration {
	d := time.Second << min(n-1, 10)
	if d > max || d <= 0 {
		d = max
	}
	return time.Duration(rand.Int64N(int64(d)/2)) + d/2
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Run polls for jobs until ctx is cancelled, then shuts down gracefully:
// running jobs get Grace to finish and are then cancelled and reported.
func (r *Runner) Run(ctx context.Context) error {
	maxBackoff := r.MaxBackoff
	if maxBackoff == 0 {
		maxBackoff = 60 * time.Second
	}
	hb := r.HeartbeatEvery
	if hb == 0 {
		hb = 15 * time.Second
	}
	if r.Config.HasCap(actions.CapDiscovery) {
		go r.discoveryLoop(ctx)
	}
	if r.Config.HasCap(actions.CapWorkspaces) {
		go r.workspaceLoop(ctx) // device workspaces (workspace.go)
	}
	if !r.Config.HasCap(actions.CapJobs) {
		r.logger().Info("jobs capability is off; only discovery runs")
		<-ctx.Done()
		return nil
	}
	labels := AllLabels(r.Config.Labels)
	r.logger().Info("runner started", "name", r.Config.Name, "server", r.Config.ServerURL, "labels", labels,
		"capabilities", r.Config.Caps(), "parallel", r.maxParallel(), "os", runtime.GOOS, "arch", runtime.GOARCH)

	// jobs run on a context that is not cancelled with ctx: shutdown cancels
	// them explicitly after the grace period, with errShutdown as the cause.
	jobCtx := context.WithoutCancel(ctx)
	var wg sync.WaitGroup
	failures := 0
	for ctx.Err() == nil {
		// busy: every slot is taken, or the runner is paused (control.go).
		// Either way it only heartbeats and the server sends it no job.
		busy := r.busy()
		if busy {
			// heartbeat until a slot frees up or a setting changes
			select {
			case <-ctx.Done():
				continue
			case <-r.ctl.wakeCh():
				continue
			case <-time.After(hb):
			}
		}
		started := time.Now()
		pollCtx, pollDone := r.pollContext(ctx, busy)
		job, err := r.Client.Poll(pollCtx, actions.PollRequest{Labels: labels, Capabilities: r.Config.AdvertisedCaps(), Busy: busy, Version: r.Version})
		interrupted := pollDone()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if interrupted {
				continue // a control change (pause, rules) stopped this poll
			}
			r.ctl.jobsOnline.Store(false)
			failures++
			d := backoff(failures, maxBackoff)
			var he *HTTPError
			if errors.As(err, &he) && (he.Status == 401 || he.Status == 403) {
				r.logger().Error("the server rejected this runner's token; re-register it", "status", he.Status)
				d = maxBackoff
			} else {
				r.logger().Warn("poll failed", "err", err, "retry_in", d.Round(time.Millisecond))
			}
			sleepCtx(ctx, d)
			continue
		}
		failures = 0
		r.ctl.jobsOnline.Store(true)
		if job == nil {
			// a server that answers at once (no long-poll) must not make us spin
			if time.Since(started) < time.Second {
				sleepCtx(ctx, time.Second+time.Duration(rand.Int64N(int64(time.Second))))
			}
			continue
		}
		if r.slotsFull() {
			r.logger().Warn("server sent a job to a busy runner; it will run when a slot frees", "job", job.ID)
			r.waitSlot(ctx)
		}
		r.logger().Info("job received", "job", job.ID, "name", job.Name, "repo", job.Repo.Owner+"/"+job.Repo.Name)
		r.ctl.running.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { r.ctl.running.Add(-1); r.ctl.wake() }()
			r.runJob(jobCtx, job)
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	if r.Grace > 0 {
		select {
		case <-done:
			return nil
		case <-time.After(r.Grace):
		}
	}
	r.cancelAll(errShutdown)
	<-done
	return nil
}

// Register exchanges a one-time registration token for runner credentials.
func Register(ctx context.Context, serverURL, token, name string, labels, caps []string, version string) (*Config, error) {
	c := &Client{BaseURL: serverURL}
	resp, err := c.Register(ctx, actions.RegisterRequest{
		Token: token, Name: name, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Labels: AllLabels(labels), Capabilities: caps, Version: version, Protocol: actions.ProtocolVersion,
	})
	if err != nil {
		return nil, err
	}
	if resp.RunnerToken == "" {
		return nil, errors.New("the server did not return a runner token")
	}
	srv := resp.ServerURL
	if srv == "" {
		srv = serverURL
	}
	return &Config{ServerURL: srv, RunnerID: resp.RunnerID, RunnerToken: resp.RunnerToken, Name: name, Labels: labels, Capabilities: caps}, nil
}
