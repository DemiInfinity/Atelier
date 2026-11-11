package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"atelier/internal/actions"
	"atelier/internal/runner"
)

// fakeClaude writes a fake `claude` that answers --version and
// `auth status --json` and logs every invocation; anything else (such as -p,
// which would use the model) fails the test through the log.
func fakeClaude(t *testing.T, version, authOut string, authExit int) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$@" >> "` + calls + `"
case "$1" in
  --version) echo '` + version + `' ;;
  auth) if [ "$2 $3" = "status --json" ]; then echo '` + authOut + `'; exit ` + itoa(authExit) + `; fi; echo "error: unknown command" >&2; exit 1 ;;
  *) echo "unexpected call" >&2; exit 9 ;;
esac
`
	if authOut == "" {
		script = strings.Replace(script, `if [ "$2 $3" = "status --json" ]; then echo ''; exit `+itoa(authExit)+`; fi; `, "", 1)
	}
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func itoa(n int) string { return string(rune('0' + n)) }

func newRunnerConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	cfg := &runner.Config{ServerURL: "https://git.example", RunnerToken: "atr_x", Name: "MacBook", Capabilities: []string{actions.CapJobs}}
	if err := cfg.Save(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeCodeEnable(t *testing.T) {
	const ver = "2.1.305 (Claude Code)"
	signedIn := `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}`

	// Signed in with a subscription, found on PATH.
	cfgPath := newRunnerConfig(t)
	bin, calls := fakeClaude(t, ver, signedIn, 0)
	t.Setenv("PATH", filepath.Dir(bin))
	var out bytes.Buffer
	if err := run([]string{"claude-code", "enable", "--config", cfgPath}, &out, &out); err != nil {
		t.Fatalf("enable: %v\n%s", err, out.String())
	}
	cfg, _ := runner.LoadConfig(cfgPath)
	if !slices.Contains(cfg.Capabilities, actions.CapClaudeCode) || !slices.Contains(cfg.Capabilities, actions.CapJobs) || cfg.ClaudeCodePath != bin {
		t.Fatalf("config = %+v", cfg)
	}
	if s := out.String(); !strings.Contains(s, "2.1.305") || !strings.Contains(s, "Signed in with claude.ai (max)") || strings.Contains(s, "Warning") {
		t.Errorf("output = %s", s)
	}
	log, _ := os.ReadFile(calls)
	if string(log) != "--version\nauth status --json\n" {
		t.Errorf("claude was called with:\n%s", log)
	}
	out.Reset()
	_ = run([]string{"claude-code", "show", "--config", cfgPath}, &out, &out)
	if !strings.Contains(out.String(), "Claude Code: on") || !strings.Contains(out.String(), bin) {
		t.Errorf("show = %s", out.String())
	}
	if err := run([]string{"claude-code", "disable", "--config", cfgPath}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := runner.LoadConfig(cfgPath); slices.Contains(cfg.Capabilities, actions.CapClaudeCode) || cfg.ClaudeCodePath != "" {
		t.Errorf("after disable = %+v", cfg)
	}

	// Signed out: refused, nothing changes.
	cfgPath = newRunnerConfig(t)
	bin, _ = fakeClaude(t, ver, `{"loggedIn":false,"authMethod":"none"}`, 1)
	err := run([]string{"claude-code", "enable", "--config", cfgPath, "--path", bin}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "isn't signed in") {
		t.Errorf("signed out: %v", err)
	}
	if cfg, _ := runner.LoadConfig(cfgPath); slices.Contains(cfg.Capabilities, actions.CapClaudeCode) {
		t.Error("enabled while signed out")
	}

	// Not Claude Code.
	bin, _ = fakeClaude(t, "1.0.0 (something else)", signedIn, 0)
	if err := run([]string{"claude-code", "enable", "--config", cfgPath, "--path", bin}, &out, &out); err == nil || !strings.Contains(err.Error(), "didn't report Claude Code") {
		t.Errorf("not claude code: %v", err)
	}

	// Not installed.
	t.Setenv("PATH", t.TempDir())
	if err := run([]string{"claude-code", "enable", "--config", cfgPath}, &out, &out); err == nil || !strings.Contains(err.Error(), "isn't on PATH") {
		t.Errorf("missing: %v", err)
	}

	// No usable status command (older Claude Code): accepted with a warning.
	bin, _ = fakeClaude(t, ver, "", 1)
	out.Reset()
	if err := run([]string{"claude-code", "enable", "--config", cfgPath, "--path", bin}, &out, &out); err != nil {
		t.Fatalf("no status: %v", err)
	}
	if !strings.Contains(out.String(), "Warning: Couldn't confirm that Claude Code is signed in") {
		t.Errorf("output = %s", out.String())
	}

	// Signed in with an API key rather than a subscription: a warning.
	cfgPath = newRunnerConfig(t)
	bin, _ = fakeClaude(t, ver, `{"loggedIn":true,"authMethod":"api_key"}`, 0)
	out.Reset()
	if err := run([]string{"claude-code", "enable", "--config", cfgPath, "--path", bin}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `signed in with "api_key", not a Claude subscription`) {
		t.Errorf("output = %s", out.String())
	}
}
