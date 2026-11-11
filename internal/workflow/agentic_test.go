package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const reviewMD = `---
name: Review helper
on:
  pull_request:
    types: [opened]
  issues:
    types: [opened, labelled]
    labels: [needs-review]
engine: claude
model: claude-sonnet-5-5
effort: high
tools: [pulls.get, pulls.diff, pulls.get, issues.get]
runs-on: [self-hosted, macos]
max-turns: 50
max-tokens: 1.5m
timeout-minutes: 30
secret: CLAUDE_KEY
---
# Review

Read the diff and post one comment.
`

func TestParseAgentic(t *testing.T) {
	w, _ := ParseAgentic(".atelier/workflows/review.md", []byte(reviewMD))
	if !strings.Contains(strings.Join(w.Errors, " "), "max-tokens must be a whole number") {
		t.Fatalf("1.5m: errors = %v", w.Errors)
	}
	src := strings.Replace(reviewMD, "max-tokens: 1.5m", "max-tokens: 1500k", 1)
	w, spec := ParseAgentic(".atelier/workflows/review.md", []byte(src))
	if len(w.Errors) > 0 || spec == nil || !w.Agentic || w.Name != "Review helper" {
		t.Fatalf("errors: %v", w.Errors)
	}
	if spec.Model != "claude-sonnet-5-5" || spec.Effort != "high" || spec.MaxTurns != 50 || spec.MaxTokens != 1_500_000 ||
		spec.TimeoutMinutes != 30 || spec.Secret != "CLAUDE_KEY" || strings.Join(spec.RunsOn, ",") != "self-hosted,macos" {
		t.Errorf("spec = %+v", spec)
	}
	if got := strings.Join(spec.Tools, ","); got != "issues.get,pulls.diff,pulls.get" {
		t.Errorf("tools = %s (want sorted, unique)", got)
	}
	if spec.ToolsKey() != "issues.get,pulls.diff,pulls.get" {
		t.Errorf("tools key = %s", spec.ToolsKey())
	}
	if !strings.HasPrefix(spec.Task, "# Review") || !strings.HasSuffix(spec.Task, "post one comment.") {
		t.Errorf("task = %q", spec.Task)
	}
	if w.On.PullRequest == nil || w.On.Push != nil || spec.Issues == nil {
		t.Errorf("triggers = %+v issues=%+v", w.On, spec.Issues)
	}
	if !spec.Issues.Matches("opened", "") || !spec.Issues.Matches("labeled", "Needs-Review") || spec.Issues.Matches("labeled", "bug") {
		t.Errorf("issues matching = %+v", spec.Issues)
	}

	// Compiled job.
	if len(w.Jobs) != 1 {
		t.Fatalf("jobs = %d", len(w.Jobs))
	}
	j := w.Jobs[0]
	if j.ID != AgentJobID || j.TimeoutMinutes != 30 || strings.Join(j.RunsOn, ",") != "self-hosted,macos" || len(j.Steps) != 1 {
		t.Errorf("job = %+v", j)
	}
	st := j.Steps[0]
	if st.ID != AgentStepID || st.Run != "" || st.Uses != "" || st.With["api-key"] != "${{ secrets.CLAUDE_KEY }}" {
		t.Errorf("step = %+v", st)
	}
	if j.Outputs["cost_usd"] == "" || j.Outputs["input_tokens"] == "" {
		t.Errorf("outputs = %v", j.Outputs)
	}
	if len(w.Uses()) != 0 {
		t.Errorf("an agent job pins no actions: %v", w.Uses())
	}

	// Lock view.
	lock := string(spec.LockYAML(w))
	for _, want := range []string{"review.lock.yml", "source-sha256: " + spec.SourceSHA, "model: claude-sonnet-5-5", "- pulls.diff",
		"api-key: ${{ secrets.CLAUDE_KEY }}", "pull_request:", "task-sha256:", "max-tokens: 1500000"} {
		if !strings.Contains(lock, want) {
			t.Errorf("lock lacks %q:\n%s", want, lock)
		}
	}
	if LockPath(".atelier/workflows/review.md") != ".atelier/workflows/review.lock.yml" {
		t.Error(LockPath(".atelier/workflows/review.md"))
	}
	if again := AgentSpecOf(w); again == nil || again.ToolsKey() != spec.ToolsKey() {
		t.Errorf("AgentSpecOf = %+v", again)
	}
}

func TestParseAgenticDefaultsAndErrors(t *testing.T) {
	w, spec := ParseAgentic("a.md", []byte("---\non: workflow_dispatch\n---\nSay hello.\n"))
	if len(w.Errors) > 0 || spec.Model != AgentDefaultModel || spec.Effort != "medium" || spec.MaxTurns != 30 ||
		spec.MaxTokens != 400_000 || spec.TimeoutMinutes != 20 || spec.Secret != "ANTHROPIC_API_KEY" ||
		strings.Join(spec.RunsOn, ",") != "self-hosted" || len(spec.Tools) != 0 || w.On.Dispatch == nil {
		t.Errorf("defaults: errors=%v spec=%+v", w.Errors, spec)
	}

	for _, c := range []struct{ src, want string }{
		{"no front matter", "front matter"},
		{"---\non: push\n", "front matter"},
		{"---\non: push\n---\n", "task is empty"},
		{"---\nengine: claude\n---\ntask", "no `on:`"},
		{"---\non: push\nengine: gpt\n---\ntask", "isn't supported"},
		{"---\non: push\ntools: [repo.info, actions.dispatch]\n---\ntask", "can't start workflows"},
		{"---\non: push\ntools: [notifications.list]\n---\ntask", "notifications"},
		{"---\non: push\ntools: [repo.delete]\n---\ntask", "unknown tool"},
		{"---\non: push\nmax-turns: 101\n---\ntask", "between 1 and 100"},
		{"---\non: push\nmax-tokens: 3m\n---\ntask", "between 20000 and 2000000"},
		{"---\non: push\nmax-tokens: 1000\n---\ntask", "between 20000"},
		{"---\non: push\neffort: extreme\n---\ntask", "effort must be"},
		{"---\non: push\nmodel: gpt-5\n---\ntask", "isn't a Claude model"},
		{"---\non: push\nsecret: GITHUB_TOKEN\n---\ntask", "secret must be"},
		{"---\non: push\nsecret: 'a b'\n---\ntask", "secret must be"},
		{"---\non: [push\n---\ntask", "Invalid YAML"},
		{"---\n- a\n---\ntask", "must be a YAML mapping"},
		{"---\non: push\n---\n" + strings.Repeat("x", AgentMaxTaskBytes+1), "longer than"},
	} {
		w, _ := ParseAgentic("x.md", []byte(c.src))
		if !strings.Contains(strings.Join(w.Errors, " | "), c.want) {
			t.Errorf("%q: errors = %v, want %q", c.src, w.Errors, c.want)
		}
	}

	w, _ = ParseAgentic("x.md", []byte("---\non: push\nmodel: claude-future-9\nfoo: 1\n---\ntask"))
	if len(w.Errors) > 0 || !strings.Contains(strings.Join(w.Warnings, " "), "no price") || !strings.Contains(strings.Join(w.Warnings, " "), `"foo"`) {
		t.Errorf("warnings = %v errors = %v", w.Warnings, w.Errors)
	}
	// CRLF files and a BOM work.
	w, spec = ParseAgentic("x.md", []byte("\xef\xbb\xbf---\r\non: push\r\n---\r\nDo it.\r\n"))
	if len(w.Errors) > 0 || spec.Task != "Do it." || w.On.Push == nil {
		t.Errorf("crlf: %v %+v", w.Errors, spec)
	}
}

func TestAgentToolAllowed(t *testing.T) {
	for _, tool := range AgentTools {
		want := tool != "actions.dispatch" && tool != "notifications.list"
		if AgentToolAllowed(tool) != want {
			t.Errorf("%s allowed = %v", tool, !want)
		}
	}
	if AgentToolAllowed("repo.delete") || AgentToolAllowed("") {
		t.Error("unknown tools must not be allowed")
	}
}

func TestEstimateCost(t *testing.T) {
	c, ok := EstimateCost("claude-opus-5-5", AgentUsage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000})
	if !ok || c < 29.19 || c > 29.21 { // 4 + 20 + 0.20 + 5
		t.Errorf("opus cost = %v %v", c, ok)
	}
	// Haiku's long-context tier applies per request.
	short, _ := EstimateCost("claude-haiku-5-5", AgentUsage{Input: 50_000})
	long, _ := EstimateCost("claude-haiku-5-5", AgentUsage{Input: 200_000})
	if short != 0.005 || long != 0.1 {
		t.Errorf("haiku = %v %v", short, long)
	}
	if _, ok := EstimateCost("claude-unknown", AgentUsage{Input: 1}); ok {
		t.Error("unknown model priced")
	}
	u := AgentUsage{Input: 10, Output: 5, CacheRead: 100, CacheWrite: 7}
	if u.Counted() != 22 {
		t.Errorf("counted = %d", u.Counted())
	}
}

// The templates in docs/agents must parse cleanly.
func TestAgentTemplatesParse(t *testing.T) {
	files, _ := filepath.Glob("../../docs/agents/*.md")
	if len(files) != 4 {
		t.Fatalf("templates = %v", files)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		w, spec := ParseAgentic(".atelier/workflows/"+filepath.Base(f), b)
		if len(w.Errors) > 0 || len(w.Warnings) > 0 || spec == nil || len(spec.Tools) == 0 {
			t.Errorf("%s: errors=%v warnings=%v", f, w.Errors, w.Warnings)
		}
		for _, tool := range spec.Tools {
			if !AgentToolAllowed(tool) {
				t.Errorf("%s uses %s", f, tool)
			}
		}
	}
}

func TestAgentProjectsTools(t *testing.T) {
	src := "---\non: push\ntools: [projects.items, projects.set_field, projects.add_item, projects.move_item, projects.list, projects.get, projects.add_draft, projects.remove_item]\n---\nTidy the board."
	w, spec := ParseAgentic("board.md", []byte(src))
	if len(w.Errors) > 0 || spec == nil || len(spec.Tools) != 8 {
		t.Fatalf("errors = %v spec = %+v", w.Errors, spec)
	}
	for _, tool := range spec.Tools {
		if !AgentToolAllowed(tool) {
			t.Errorf("%s not allowed", tool)
		}
	}
	for tool, want := range map[string]bool{"projects.list": false, "projects.get": false, "projects.items": false,
		"projects.add_item": true, "projects.add_draft": true, "projects.set_field": true, "projects.move_item": true,
		"projects.remove_item": true, "issues.get": false, "issues.create": true} {
		if AgentToolWrites(tool) != want {
			t.Errorf("AgentToolWrites(%s) = %v", tool, !want)
		}
	}
	// Project settings, fields, links and deletion are never tools.
	w, _ = ParseAgentic("x.md", []byte("---\non: push\ntools: [projects.delete]\n---\nx"))
	if len(w.Errors) == 0 {
		t.Error("projects.delete accepted")
	}
}
