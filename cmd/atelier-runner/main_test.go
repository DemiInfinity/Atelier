package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"atelier/internal/actions"
)

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, &out, &out); err != nil || !strings.Contains(out.String(), "atelier-runner dev") {
		t.Errorf("version: %v %q", err, out.String())
	}
	if err := run([]string{"bogus"}, &out, &out); err == nil {
		t.Error("unknown command accepted")
	}
}

func TestRegisterStatusAndDryRunInstall(t *testing.T) {
	var got actions.RegisterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != actions.APIPrefix+"/register" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(actions.RegisterResponse{RunnerID: 9, RunnerToken: "rnr_cli_token"})
	}))
	defer srv.Close()
	cfg := filepath.Join(t.TempDir(), "cfg", "config.json")
	var out bytes.Buffer
	err := run([]string{"register", "--config", cfg, "--url", srv.URL + "/", "--token", "reg", "--name", "box",
		"--labels", "gpu", "--capabilities", "jobs,discovery"}, &out, &out)
	if err != nil {
		t.Fatalf("register: %v\n%s", err, out.String())
	}
	if got.Token != "reg" || got.Name != "box" || len(got.Capabilities) != 2 || !strings.Contains(strings.Join(got.Labels, ","), "gpu") {
		t.Errorf("request = %+v", got)
	}
	b, _ := os.ReadFile(cfg)
	if !strings.Contains(string(b), "rnr_cli_token") || !strings.Contains(string(b), srv.URL) {
		t.Errorf("config = %s", b)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(cfg); fi.Mode().Perm() != 0o600 {
			t.Errorf("config mode %v", fi.Mode().Perm())
		}
	}
	out.Reset()
	if err := run([]string{"status", "--config", cfg}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Runner: box (id 9)") || strings.Contains(out.String(), "rnr_cli_token") {
		t.Errorf("status = %s", out.String())
	}
	if err := run([]string{"register", "--config", cfg, "--url", srv.URL, "--token", "x", "--capabilities", "nope"}, &out, &out); err == nil {
		t.Error("bad capability accepted")
	}
	switch runtime.GOOS {
	case "darwin", "linux":
		if os.Geteuid() == 0 {
			t.Skip("running as root")
		}
		out.Reset()
		if err := run([]string{"install", "--dry-run", "--config", cfg}, &out, &out); err != nil {
			t.Fatalf("install dry-run: %v", err)
		}
		if !strings.Contains(out.String(), cfg) || !strings.Contains(out.String(), "run") {
			t.Errorf("dry-run output:\n%s", out.String())
		}
	}
}
