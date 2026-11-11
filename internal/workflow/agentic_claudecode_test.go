package workflow

import (
	"strings"
	"testing"
)

func TestParseAgenticClaudeCode(t *testing.T) {
	src := "---\non:\n  pull_request:\nengine: claude-code\ntools: [pulls.diff, pulls.get, pulls.comment]\nmax-turns: 12\ntimeout-minutes: 15\n---\nReview it.\n"
	w, spec := ParseAgentic(".atelier/workflows/review.md", []byte(src))
	if len(w.Errors) > 0 || len(w.Warnings) > 0 {
		t.Fatalf("errors = %v warnings = %v", w.Errors, w.Warnings)
	}
	if !spec.ClaudeCode() || spec.Model != "" || spec.Effort != "" || spec.MaxTokens != 0 || spec.Secret != "" ||
		spec.MaxTurns != 12 || spec.TimeoutMinutes != 15 || strings.Join(spec.Tools, ",") != "pulls.comment,pulls.diff,pulls.get" {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.EngineLabel() != "Claude Code · subscription" || spec.ModelLabel() != "Claude Code's default" {
		t.Errorf("labels = %q %q", spec.EngineLabel(), spec.ModelLabel())
	}
	st := w.Jobs[0].Steps[0]
	if st.With[AgentWithEngine] != AgentEngineClaudeCode || st.With["api-key"] != "" || len(st.With) != 1 {
		t.Errorf("with = %v (no secret may be referenced)", st.With)
	}
	if !strings.Contains(st.Name, "Claude Code") || w.Jobs[0].Outputs["engine"] == "" {
		t.Errorf("step = %+v outputs = %v", st, w.Jobs[0].Outputs)
	}
	lock := string(spec.LockYAML(w))
	for _, want := range []string{"engine: claude-code", "model: default", "max-turns: 12", "subscription"} {
		if !strings.Contains(lock, want) {
			t.Errorf("lock lacks %q:\n%s", want, lock)
		}
	}
	for _, bad := range []string{"api-key", "max-tokens", "secrets."} {
		if strings.Contains(lock, bad) {
			t.Errorf("lock has %q:\n%s", bad, lock)
		}
	}

	// The API engine is unchanged.
	w, spec = ParseAgentic("x.md", []byte("---\non: push\nengine: claude\n---\ntask"))
	if len(w.Errors) > 0 || spec.ClaudeCode() || spec.Model != AgentDefaultModel || w.Jobs[0].Steps[0].With["api-key"] != "${{ secrets.ANTHROPIC_API_KEY }}" ||
		w.Jobs[0].Steps[0].With[AgentWithEngine] != "" || spec.EngineLabel() != "Claude API · API key" {
		t.Errorf("api engine: %v %+v %v", w.Errors, spec, w.Jobs[0].Steps[0].With)
	}

	// Model aliases and ids; effort is passed through.
	w, spec = ParseAgentic("x.md", []byte("---\non: push\nmodel: sonnet\neffort: high\nengine: claude-code\n---\ntask"))
	if len(w.Errors) > 0 || spec.Model != "sonnet" || spec.Effort != "high" {
		t.Errorf("alias: %v %+v", w.Errors, spec)
	}
	w, spec = ParseAgentic("x.md", []byte("---\non: push\nengine: claude-code\nmodel: claude-future-9\n---\ntask"))
	if len(w.Errors) > 0 || len(w.Warnings) > 0 || spec.Model != "claude-future-9" {
		t.Errorf("unpriced id with claude-code shouldn't warn about prices: %v %v", w.Errors, w.Warnings)
	}

	for _, c := range []struct{ src, want string }{
		// max-tokens and secret are refused, before or after engine:.
		{"---\non: push\nengine: claude-code\nmax-tokens: 100k\n---\ntask", "max-tokens doesn't apply to engine: claude-code"},
		{"---\non: push\nmax-tokens: 100k\nengine: claude-code\n---\ntask", "max-tokens doesn't apply"},
		{"---\non: push\nsecret: MY_KEY\nengine: claude-code\n---\ntask", "secret doesn't apply to engine: claude-code"},
		{"---\non: push\nengine: claude-code\nmodel: gpt-5\n---\ntask", "Claude Code alias"},
		{"---\non: push\nengine: claude\nmodel: sonnet\n---\ntask", "isn't a Claude model id"},
		{"---\non: push\nengine: claude-code\neffort: extreme\n---\ntask", "effort must be"},
		{"---\non: push\nengine: claude-code\ntools: [actions.dispatch]\n---\ntask", "can't start workflows"},
		{"---\non: push\nengine: claude-code\ntools: [shell.run]\n---\ntask", "unknown tool"},
		{"---\non: push\nengine: claude-code\nmax-turns: 0\n---\ntask", "between 1 and 100"},
		{"---\non: push\nengine: claude-cli\n---\ntask", `use "claude"`},
	} {
		w, _ := ParseAgentic("x.md", []byte(c.src))
		if !strings.Contains(strings.Join(w.Errors, " | "), c.want) {
			t.Errorf("%q: errors = %v, want %q", c.src, w.Errors, c.want)
		}
	}

	// Unknown keys warn (ignored), as for the API engine.
	w, _ = ParseAgentic("x.md", []byte("---\non: push\nengine: claude-code\npermission-mode: bypassPermissions\n---\ntask"))
	if len(w.Errors) > 0 || !strings.Contains(strings.Join(w.Warnings, " "), `"permission-mode"`) {
		t.Errorf("unknown key: errors=%v warnings=%v", w.Errors, w.Warnings)
	}
}
