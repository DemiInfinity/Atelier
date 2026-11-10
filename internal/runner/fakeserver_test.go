package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/actions"
)

// fakeServer implements the runner side of the contract and records everything.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	regToken   string
	runnerTok  string
	queue      []*JobSpec
	logs       map[int64][]actions.LogLine
	steps      map[int64]map[int]StepReport
	finished   map[int64]actions.FinishRequest
	cancelWhen map[int64]string // cancel a job once a log line contains this
	polls      []actions.PollRequest
	failPolls  int
	discovery  []actions.DiscoveryRequest
	registered []actions.RegisterRequest
	badAuth    int
	finishCh   chan int64
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, regToken: "reg-once", runnerTok: "rnr_secret_token",
		logs: map[int64][]actions.LogLine{}, steps: map[int64]map[int]StepReport{},
		finished: map[int64]actions.FinishRequest{}, cancelWhen: map[int64]string{}, finishCh: make(chan int64, 16)}
	mux := http.NewServeMux()
	p := actions.APIPrefix
	mux.HandleFunc("POST "+p+"/register", f.register)
	mux.HandleFunc("POST "+p+"/poll", f.auth(f.poll))
	mux.HandleFunc("POST "+p+"/jobs/{id}/logs", f.auth(f.logsH))
	mux.HandleFunc("POST "+p+"/jobs/{id}/steps/{index}", f.auth(f.stepH))
	mux.HandleFunc("POST "+p+"/jobs/{id}/finish", f.auth(f.finishH))
	mux.HandleFunc("POST "+p+"/discovery", f.auth(f.discoveryH))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) decode(r *http.Request, v any) {
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, v); err != nil {
		f.t.Errorf("bad request body for %s: %v", r.URL.Path, err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeServer) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.runnerTok {
			f.mu.Lock()
			f.badAuth++
			f.mu.Unlock()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (f *fakeServer) register(w http.ResponseWriter, r *http.Request) {
	var req actions.RegisterRequest
	f.decode(r, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, req)
	if req.Token != f.regToken {
		http.Error(w, "bad token", http.StatusForbidden)
		return
	}
	f.regToken = "" // one-time
	writeJSON(w, actions.RegisterResponse{RunnerID: 7, RunnerToken: f.runnerTok, ServerURL: f.srv.URL})
}

func (f *fakeServer) poll(w http.ResponseWriter, r *http.Request) {
	var req actions.PollRequest
	f.decode(r, &req)
	f.mu.Lock()
	f.polls = append(f.polls, req)
	if f.failPolls > 0 {
		f.failPolls--
		f.mu.Unlock()
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	f.mu.Unlock()
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		f.mu.Lock()
		if !req.Busy && len(f.queue) > 0 {
			j := f.queue[0]
			f.queue = f.queue[1:]
			f.mu.Unlock()
			writeJSON(w, map[string]any{"job": j})
			return
		}
		f.mu.Unlock()
		if time.Now().After(deadline) || r.Context().Err() != nil {
			writeJSON(w, map[string]any{})
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pathID(r *http.Request, k string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(k), 10, 64)
	return n
}

func (f *fakeServer) logsH(w http.ResponseWriter, r *http.Request) {
	var req actions.LogsRequest
	f.decode(r, &req)
	id := pathID(r, "id")
	f.mu.Lock()
	f.logs[id] = append(f.logs[id], req.Lines...)
	cancel := false
	if want := f.cancelWhen[id]; want != "" {
		for _, l := range f.logs[id] {
			if strings.Contains(l.Text, want) {
				cancel = true
			}
		}
	}
	f.mu.Unlock()
	writeJSON(w, actions.LogsResponse{Cancel: cancel})
}

func (f *fakeServer) stepH(w http.ResponseWriter, r *http.Request) {
	var req StepReport
	f.decode(r, &req)
	id, idx := pathID(r, "id"), int(pathID(r, "index"))
	f.mu.Lock()
	if f.steps[id] == nil {
		f.steps[id] = map[int]StepReport{}
	}
	f.steps[id][idx] = req
	f.mu.Unlock()
	writeJSON(w, map[string]any{})
}

func (f *fakeServer) finishH(w http.ResponseWriter, r *http.Request) {
	var req actions.FinishRequest
	f.decode(r, &req)
	id := pathID(r, "id")
	f.mu.Lock()
	f.finished[id] = req
	f.mu.Unlock()
	writeJSON(w, map[string]any{})
	f.finishCh <- id
}

func (f *fakeServer) discoveryH(w http.ResponseWriter, r *http.Request) {
	var req actions.DiscoveryRequest
	f.decode(r, &req)
	f.mu.Lock()
	f.discovery = append(f.discovery, req)
	f.mu.Unlock()
	writeJSON(w, map[string]any{})
}

func (f *fakeServer) enqueue(j *JobSpec) {
	f.mu.Lock()
	f.queue = append(f.queue, j)
	f.mu.Unlock()
}

// logText is every log line of a job joined by newlines.
func (f *fakeServer) logText(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, l := range f.logs[id] {
		b.WriteString(l.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func (f *fakeServer) step(id int64, idx int) StepReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps[id][idx]
}

func (f *fakeServer) finish(id int64) actions.FinishRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished[id]
}

// testRunner registers against the fake server and returns a runner.
func (f *fakeServer) testRunner(t *testing.T, caps ...string) *Runner {
	t.Helper()
	if len(caps) == 0 {
		caps = []string{actions.CapJobs}
	}
	f.mu.Lock()
	f.regToken = "reg-once"
	f.mu.Unlock()
	cfg, err := Register(context.Background(), f.srv.URL, "reg-once", "test-runner", []string{"Test"}, caps, "test")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg.WorkDir = dir + "/work"
	cfg.CacheDir = dir + "/cache"
	return &Runner{
		Config:         cfg,
		Client:         &Client{BaseURL: cfg.ServerURL, Token: cfg.RunnerToken},
		Version:        "test",
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		HeartbeatEvery: 100 * time.Millisecond,
		MaxBackoff:     200 * time.Millisecond,
	}
}

// runJobs runs the runner until every given job has finished.
func (f *fakeServer) runJobs(t *testing.T, r *Runner, jobs ...*JobSpec) {
	t.Helper()
	for _, j := range jobs {
		f.enqueue(j)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	want := map[int64]bool{}
	for _, j := range jobs {
		want[j.ID] = true
	}
	timeout := time.After(90 * time.Second)
	for len(want) > 0 {
		select {
		case id := <-f.finishCh:
			delete(want, id)
		case <-timeout:
			cancel()
			t.Fatalf("jobs did not finish: %v", want)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("runner did not stop")
	}
}
