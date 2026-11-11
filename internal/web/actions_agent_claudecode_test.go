package web

import (
	"encoding/json"
	"strings"
	"testing"

	"atelier/internal/actions"
	"atelier/internal/store"
	"atelier/internal/workflow"
)

const agentClaudeCodeMD = `---
name: Review helper
on:
  push:
    branches: [main]
engine: claude-code
tools: [pulls.get, pulls.comment]
runs-on: linux
max-turns: 12
---
Read the pull request and post one comment.
`

// TestAgentClaudeCodeEngine: claude-code agent jobs only go to runners that
// understand agent steps and have Claude Code switched on; they carry no API
// key; the pages say the usage is the subscription's.
func TestAgentClaudeCodeEngine(t *testing.T) {
	f, _ := agentFixture(t)
	a := f.a
	pullsCommit(t, f.work, ".atelier/workflows/review.md", agentClaudeCodeMD, "Add review agent")
	f.push(t, "main")
	run := onlyRun(t, f, 0)
	jb := f.jobs(t, run.ID)["Agent: Review helper"]
	if jb.Status != actions.StatusWaiting || !strings.Contains(jb.Approval, "first run") {
		t.Fatalf("the approval gate applies to claude-code too: %+v", jb)
	}
	if len(jb.SecretNames) != 0 {
		t.Errorf("secrets referenced: %v", jb.SecretNames)
	}
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/actions?workflow=.atelier/workflows/review.md", nil, f.ck), 200, "workflow page")
	mustContain(t, body, "workflow page", "Claude Code · subscription", "Claude Code&#39;s default", "counts against your Claude subscription",
		"engine: claude-code", "12 turns")
	if strings.Contains(body, "ANTHROPIC_API_KEY") {
		t.Error("the workflow page asks for an API key")
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/runs/"+itoa(run.Seq)+"/approve", nil, f.ck), 303, "approve")

	// Older runners (no "agents") and runners without Claude Code never get it.
	old := f.registerRunner(t, "Old", []string{"linux"}, []string{"jobs"})
	if j := old.poll(); j != nil {
		t.Fatalf("an older runner got the agent job: %+v", j.Steps)
	}
	api := f.registerRunner(t, "Box", []string{"linux"}, []string{"jobs", "agents"})
	if j := api.poll(); j != nil {
		t.Fatal("a runner without claude-code got a claude-code job")
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions/runs/"+itoa(run.Seq), nil, f.ck), 200, "queued run")
	mustContain(t, body, "queued run", "atelier-runner claude-code enable")

	cc := f.registerRunner(t, "Mac", []string{"linux"}, []string{"jobs", "agents", "claude-code"})
	j := cc.poll()
	if j == nil || len(j.Steps) != 1 || j.Steps[0].Agent == nil {
		t.Fatalf("job = %+v", j)
	}
	ag := j.Steps[0].Agent
	if ag.Engine != workflow.AgentEngineClaudeCode || ag.Model != "" || ag.Effort != "" || ag.MaxTokens != 0 || ag.MaxTurns != 12 ||
		strings.Join(ag.Tools, ",") != "pulls.comment,pulls.get" || ag.MCPURL != f.srv.URL+"/mcp" {
		t.Errorf("agent step = %+v", ag)
	}
	if j.Steps[0].With["api-key"] != "" || len(j.Secrets) != 0 {
		t.Errorf("an API key was sent: with=%v secrets=%d", j.Steps[0].With, len(j.Secrets))
	}
	who, ok := a.jobActor(t.Context(), j.JobToken)
	if !ok || !who.hasScope("mcp:pulls.get") || who.hasScope("mcp:actions.dispatch") {
		t.Errorf("actor = %+v", who)
	}
	cc.runJob(j, actions.StatusSuccess, map[string]string{"engine": "claude-code", "model": "claude-opus-5-5", "turns": "2",
		"input_tokens": "1500", "output_tokens": "120", "cache_read_tokens": "5000", "cache_write_tokens": "5000", "stop": "end_turn",
		"cost_usd": "0.042100"})
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions/runs/"+itoa(run.Seq), nil, f.ck), 200, "finished run")
	mustContain(t, body, "finished run", "Claude Code · subscription", "subscription usage (≈ $0.04 at API prices, not billed)", "2 turns")
	if strings.Contains(body, "(estimate)") {
		t.Error("subscription usage shown as an estimated cost")
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions?workflow=.atelier/workflows/review.md", nil, f.ck), 200, "page after")
	mustContain(t, body, "page after", "subscription usage", "nothing is billed")
	if strings.Contains(body, "in all (estimate)") {
		t.Error("subscription runs counted in the estimated total")
	}
}

// TestAgentJobCapsGate: every agent job (either engine) needs "agents";
// ordinary jobs need nothing new.
func TestAgentJobCapsGate(t *testing.T) {
	mk := func(wf string, with map[string]string) store.ActionsJob {
		pj := actions.Job{Workflow: wf, Steps: []actions.Step{{Index: 0, ID: workflow.AgentStepID, With: with}}}
		return store.ActionsJob{JobKey: workflow.AgentJobID, RunsOn: []string{"self-hosted"}, Payload: agentTestJSON(t, pj)}
	}
	apiJob := mk(".atelier/workflows/a.md", map[string]string{"api-key": "${{ secrets.K }}"})
	ccJob := mk(".atelier/workflows/b.md", map[string]string{"engine": "claude-code"})
	yamlJob := mk(".atelier/workflows/ci.yml", nil) // a YAML job that happens to be called "agent"
	runner := func(caps ...string) store.ActionsRunner {
		return store.ActionsRunner{Enabled: true, Capabilities: caps}
	}
	for _, c := range []struct {
		rn   store.ActionsRunner
		j    store.ActionsJob
		want bool
	}{
		{runner("jobs"), apiJob, false},
		{runner("jobs", "agents"), apiJob, true},
		{runner("jobs", "claude-code"), ccJob, false},
		{runner("jobs", "agents"), ccJob, false},
		{runner("jobs", "agents", "claude-code"), ccJob, true},
		{runner("agents", "claude-code"), ccJob, false},
		{runner("jobs"), yamlJob, true},
		{runner("jobs"), store.ActionsJob{RunsOn: []string{"self-hosted"}}, true},
	} {
		if got := runnerCanRun(c.rn, c.j); got != c.want {
			t.Errorf("runnerCanRun(%v, %s) = %v, want %v", c.rn.Capabilities, c.j.Payload, got, c.want)
		}
	}
}

func agentTestJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
