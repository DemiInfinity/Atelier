package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"atelier/internal/actions"
	"atelier/internal/runner"
)

func TestWorkspacesEnableDisable(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := &runner.Config{ServerURL: "https://git.example", RunnerToken: "atr_x", Name: "MacBook", Capabilities: []string{actions.CapJobs}}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	root := filepath.Join(dir, "ws")
	err := run([]string{"workspaces", "enable", "--config", cfgPath, "--root", root, "--remote", "ssh", "--preset", "mark=touch {path}/x"}, &out, &out)
	if err != nil {
		t.Fatalf("enable: %v\n%s", err, out.String())
	}
	got, _ := runner.LoadConfig(cfgPath)
	if !slices.Contains(got.Capabilities, actions.CapWorkspaces) || !slices.Contains(got.Capabilities, actions.CapJobs) {
		t.Fatalf("capabilities = %v", got.Capabilities)
	}
	wc, err := runner.LoadWorkspaceConfig(filepath.Join(dir, "workspaces.json"))
	if err != nil || wc.Root != root || wc.Remote != "ssh" || wc.Presets["mark"] != "touch {path}/x" {
		t.Fatalf("workspaces.json = %+v, %v", wc, err)
	}
	if !strings.Contains(out.String(), "Workspaces are on") || !strings.Contains(out.String(), "mark") {
		t.Errorf("output = %s", out.String())
	}
	if err := run([]string{"workspaces", "enable", "--config", cfgPath, "--preset", "Bad Name=x"}, &out, &out); err == nil {
		t.Error("bad preset name accepted")
	}
	if err := run([]string{"workspaces", "disable", "--config", cfgPath}, &out, &out); err != nil {
		t.Fatal(err)
	}
	got, _ = runner.LoadConfig(cfgPath)
	if slices.Contains(got.Capabilities, actions.CapWorkspaces) {
		t.Errorf("capabilities after disable = %v", got.Capabilities)
	}
}
