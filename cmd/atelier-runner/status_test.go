//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atelier/internal/actions"
	"atelier/internal/runner"
)

// TestStatusAsksTheRunningRunner: `status` and `status --json` ask the
// control socket of a runner started with `run`, and say "not running"
// (exit code 3) once it has stopped.
func TestStatusAsksTheRunningRunner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case actions.APIPrefix + "/register":
			json.NewEncoder(w).Encode(actions.RegisterResponse{RunnerID: 4, RunnerToken: "rnr_status_token"})
		case actions.APIPrefix + "/poll":
			if r.Header.Get("Authorization") != "Bearer rnr_status_token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			select {
			case <-r.Context().Done():
			case <-time.After(100 * time.Millisecond):
			}
			io.WriteString(w, "{}")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	// a short folder: Unix socket paths are limited to about 100 bytes
	dir, err := os.MkdirTemp("", "atl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "config.json")
	var out bytes.Buffer
	if err := run([]string{"register", "--config", cfg, "--url", srv.URL, "--token", "reg", "--name", "MacBook"}, &out, &out); err != nil {
		t.Fatalf("register: %v\n%s", err, out.String())
	}

	notRunning := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		err := run(append([]string{"status", "--config", cfg}, args...), &out, &out)
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 3 || ee.Error() != "not running" {
			t.Fatalf("status %v of a stopped runner: err = %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	if got := notRunning("--json"); strings.TrimSpace(got) != `{"ok":false,"error":"not running"}` {
		t.Errorf("status --json, stopped = %q", got)
	}
	if got := notRunning(); !strings.Contains(got, "Process: not running") {
		t.Errorf("status, stopped = %q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runCmd(ctx, []string{"--config", cfg}, io.Discard) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("run did not stop")
		}
	}
	defer stop()

	// --json: the runner's own status object, once it has polled
	var st map[string]any
	var raw string
	deadline := time.Now().Add(30 * time.Second)
	for {
		var out bytes.Buffer
		err := run([]string{"status", "--json", "--config", cfg}, &out, &out)
		raw = out.String()
		st = nil
		if err == nil {
			if jerr := json.Unmarshal(out.Bytes(), &st); jerr != nil {
				t.Fatalf("status --json printed %q: %v", raw, jerr)
			}
			if st["online"] == true {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no online status: %v %q", err, raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st["ok"] != true || st["name"] != "MacBook" || st["version"] != version || st["server"] != srv.URL ||
		st["taking_jobs"] != true || st["paused_reason"] != nil {
		t.Errorf("status --json = %s", raw)
	}
	for _, k := range []string{"capabilities", "rules", "current"} {
		if _, ok := st[k]; !ok {
			t.Errorf("status --json lacks %s: %s", k, raw)
		}
	}
	if strings.Count(strings.TrimSpace(raw), "\n") != 0 || strings.Contains(raw, "rnr_status_token") {
		t.Errorf("status --json should be one line without the token: %q", raw)
	}

	// the friendly form
	out.Reset()
	if err := run([]string{"status", "--config", cfg}, &out, &out); err != nil {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Runner: MacBook (id 4)", "Process: running (version " + version + ")", "Connection: online",
		"Taking jobs: yes", "Rules: pause on battery off, 1 at once, container jobs off", "Running now: nothing"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "rnr_status_token") {
		t.Errorf("status prints the token:\n%s", out.String())
	}

	// a pause set over the socket shows up
	if _, err := runner.ControlRequest(cfg, map[string]any{"op": "set", "taking_jobs": false}, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"status", "--config", cfg}, &out, &out); err != nil || !strings.Contains(out.String(), "Taking jobs: no (paused)") {
		t.Errorf("paused status: %v\n%s", err, out.String())
	}
	if fi, err := os.Stat(cfg); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode after set: %v %v", fi.Mode().Perm(), err)
	}

	// stopped: the socket is gone and status says so
	stop()
	if _, err := os.Lstat(runner.ControlAddress(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left after run: %v", err)
	}
	if got := notRunning("--json"); strings.TrimSpace(got) != `{"ok":false,"error":"not running"}` {
		t.Errorf("status --json after stop = %q", got)
	}
	notRunning()
}

func TestPrintLiveStatus(t *testing.T) {
	reason := "battery"
	var out bytes.Buffer
	printLiveStatus(&out, &runner.ControlStatus{OK: true, Version: "7f82d31", Server: "https://studio.example", PausedReason: &reason,
		Rules: runner.ControlRulesStatus{PauseOnBattery: true, MaxParallel: 2, ContainerJobs: true},
		Current: []runner.ControlJob{{JobID: 412, Repo: "DemiInfinity/Atelier", Workflow: "review-helper", Kind: "agent",
			StartedAt: time.Now().Add(-90 * time.Second), Detail: "turn 3 of 20"}}})
	for _, want := range []string{"Connection: offline", "Taking jobs: no (on battery)", "pause on battery on, 2 at once, container jobs on",
		"job 412  DemiInfinity/Atelier  review-helper (agent), 1m30s: turn 3 of 20"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("lacks %q:\n%s", want, out.String())
		}
	}
}
