package web

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"atelier/internal/actions"
	"atelier/internal/store"
	"atelier/internal/workflow"
)

const agentReviewMD = `---
name: Review helper
on:
  push:
    branches: [main]
  pull_request:
  workflow_dispatch:
engine: claude
effort: low
tools: [pulls.get, pulls.comment]
runs-on: linux
max-turns: 12
---
Read the pull request and post one comment.
`

func agentFixture(t *testing.T) (*actFixture, int64) {
	t.Helper()
	f := newActFixture(t)
	pullsCommit(t, f.work, "README.md", "# demo\n", "Initial")
	f.push(t, "main")
	rid := f.repoID(t)
	by := store.SecretsBy{Name: "zoe", Kind: ActorOwner}
	if err := f.a.db.SetSecret(context.Background(), f.a.secretSealer(), store.RepoScope(rid), "ANTHROPIC_API_KEY", "sk-ant-secret-value", by); err != nil {
		t.Fatal(err)
	}
	return f, rid
}

func onlyRun(t *testing.T, f *actFixture, before int) store.ActionsRun {
	t.Helper()
	runs := f.runs(t)
	if len(runs) != before+1 {
		t.Fatalf("runs = %d, want %d", len(runs), before+1)
	}
	return runs[0]
}

func TestAgenticWorkflowEndToEnd(t *testing.T) {
	f, rid := agentFixture(t)
	a, ctx := f.a, context.Background()
	pullsCommit(t, f.work, ".atelier/workflows/review.md", agentReviewMD, "Add review agent")
	f.push(t, "main")

	// First run in this repository: the agent job waits for the owner.
	run := onlyRun(t, f, 0)
	if run.WorkflowName != "Review helper" || len(run.Errors) > 0 {
		t.Fatalf("run = %+v", run)
	}
	jobs := f.jobs(t, run.ID)
	jb := jobs["Agent: Review helper"]
	if jb.Status != actions.StatusWaiting || !strings.Contains(jb.Approval, "first run") || !strings.Contains(jb.Approval, "pulls.comment, pulls.get") {
		t.Fatalf("agent job = %+v", jb)
	}
	rn := f.registerRunner(t, "Box", []string{"linux"}, []string{"jobs", "agents"})
	if j := rn.poll(); j != nil {
		t.Fatal("an unapproved agent job was handed out")
	}

	// The workflow page shows the agent panel and the compiled lock view.
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/actions?workflow=.atelier/workflows/review.md", nil, f.ck), 200, "workflow page")
	mustContain(t, body, "workflow page", "Claude agent", "First run needs your approval", "review.lock.yml", "pulls.comment",
		"claude-opus-5-5", "/static/agents.css", "Add an agent")
	if strings.Contains(body, "Phase 4") {
		t.Error("the Phase 4 placeholder is still there")
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions/runs/"+itoa(run.Seq), nil, f.ck), 200, "run page")
	mustContain(t, body, "run page", "act-chip is-agent", "first run of the agentic workflow", "Approve and run",
		"Token usage and an estimated cost appear here")

	// Approving hands the job to a runner with the agent settings.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/runs/"+itoa(run.Seq)+"/approve", nil, f.ck), 303, "approve")
	j := rn.poll()
	if j == nil || len(j.Steps) != 1 {
		t.Fatalf("job = %+v", j)
	}
	st := j.Steps[0]
	if st.Agent == nil || st.Agent.Model != workflow.AgentDefaultModel || st.Agent.Effort != "low" || st.Agent.MaxTurns != 12 ||
		strings.Join(st.Agent.Tools, ",") != "pulls.comment,pulls.get" || st.Agent.MCPURL != f.srv.URL+"/mcp" ||
		!strings.Contains(st.Agent.Task, "post one comment") || st.Agent.WorkflowName != "Review helper" {
		t.Fatalf("agent step = %+v", st.Agent)
	}
	if st.With["api-key"] != "${{ secrets.ANTHROPIC_API_KEY }}" || j.Secrets["ANTHROPIC_API_KEY"] != "sk-ant-secret-value" {
		t.Errorf("api key: with=%v secrets=%v", st.With, slices.Collect(func(yield func(string) bool) {
			for k := range j.Secrets {
				yield(k)
			}
		}))
	}
	if strings.Contains(jobPayload(t, f, j.ID), "sk-ant-secret-value") {
		t.Error("the stored payload contains the secret")
	}

	// The job token is an agent: MCP scopes for exactly the allowed tools.
	who, ok := a.jobActor(ctx, j.JobToken)
	if !ok || who.Kind != ActorJob || who.Name != "agent:Review helper" || jobRepoID(who) != rid {
		t.Fatalf("actor = %+v", who)
	}
	if !who.hasScope("mcp:pulls.get") || !who.hasScope("mcp:pulls.comment") || who.hasScope("mcp:actions.dispatch") ||
		who.hasScope("packages:read") || who.hasScope("packages:write") {
		t.Errorf("scopes = %v", who.Scopes)
	}
	n := 0
	for _, s := range who.Scopes {
		if strings.HasPrefix(s, "mcp:") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("mcp scopes = %v", who.Scopes)
	}
	rn.runJob(j, actions.StatusSuccess, map[string]string{"model": "claude-opus-5-5", "turns": "4", "input_tokens": "12000",
		"output_tokens": "3000", "cache_read_tokens": "40000", "cache_write_tokens": "9000", "stop": "end_turn", "cost_usd": "0.161000"})
	if _, ok := a.jobActor(ctx, j.JobToken); ok {
		t.Error("the job token outlived the job")
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions/runs/"+itoa(run.Seq), nil, f.ck), 200, "finished run")
	mustContain(t, body, "finished run", "≈ $0.16 (estimate)", "4 turns", "stopped: end_turn")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions?workflow=.atelier/workflows/review.md", nil, f.ck), 200, "page after")
	mustContain(t, body, "page after", "Tools approved", "≈ $0.16 (estimate)", "64k tokens")

	// Later runs with the same tools don't wait.
	pullsCommit(t, f.work, "notes.txt", "x\n", "More")
	f.push(t, "main")
	run = onlyRun(t, f, 1)
	if jb := f.jobs(t, run.ID)["Agent: Review helper"]; jb.Status != actions.StatusQueued || jb.Approval != "" {
		t.Fatalf("second run job = %+v", jb)
	}
	j = rn.poll()
	if j == nil || j.Steps[0].Agent == nil {
		t.Fatal("approved agent job not handed out")
	}
	rn.runJob(j, actions.StatusSuccess, nil)

	// Changing the tools asks again.
	pullsCommit(t, f.work, ".atelier/workflows/review.md", strings.Replace(agentReviewMD, "tools: [pulls.get, pulls.comment]",
		"tools: [pulls.get, pulls.comment, issues.close]", 1), "More tools")
	f.push(t, "main")
	run = onlyRun(t, f, 2)
	jb = f.jobs(t, run.ID)["Agent: Review helper"]
	if jb.Status != actions.StatusWaiting || !strings.Contains(jb.Approval, "tools changed") || !strings.Contains(jb.Approval, "issues.close") {
		t.Fatalf("changed tools job = %+v", jb)
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/actions?workflow=.atelier/workflows/review.md", nil, f.ck), 200, "changed")
	mustContain(t, body, "changed", "Tools changed: next run needs approval")
	if j := rn.poll(); j != nil {
		t.Fatal("a job with changed tools was handed out")
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/actions/runs/"+itoa(run.Seq)+"/approve", nil, f.ck), 303, "approve changed")
	j = rn.poll()
	who, _ = a.jobActor(ctx, j.JobToken)
	if !who.hasScope("mcp:issues.close") {
		t.Errorf("scopes after approval = %v", who.Scopes)
	}
	rn.runJob(j, actions.StatusSuccess, nil)

	// Manual runs work for agentic workflows.
	g, _ := a.git.Open("zoe", "demo")
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	if _, err := a.dispatchWorkflow(ctx, demo, g, ".atelier/workflows/review.md", "main", nil, Actor{Kind: ActorOwner, Name: "zoe"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if j := rn.poll(); j == nil || j.Event != "workflow_dispatch" {
		t.Fatalf("dispatched job = %+v", j)
	}
}

func jobPayload(t *testing.T, f *actFixture, id int64) string {
	t.Helper()
	j, err := f.a.db.ActionsJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j.Payload
}

func TestAgentPullRequestsAndIssues(t *testing.T) {
	f, rid := agentFixture(t)
	a, ctx := f.a, context.Background()
	md := strings.Replace(agentReviewMD, "  workflow_dispatch:\n", "  workflow_dispatch:\n  issues:\n    types: [opened, labeled]\n    labels: [agent]\n", 1)
	pullsCommit(t, f.work, ".atelier/workflows/review.md", md, "Add agent")
	runGit(t, f.work, "push", "-q", "origin", "main") // no Actions push: no run yet
	if err := a.db.ApproveAgentTools(ctx, rid, ".atelier/workflows/review.md", "pulls.comment,pulls.get", "zoe"); err != nil {
		t.Fatal(err)
	}
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	g, _ := a.git.Open("zoe", "demo")

	// A pull request opened with a token (not the owner in person) waits.
	runGit(t, f.work, "checkout", "-q", "-b", "feature/y")
	pullsCommit(t, f.work, "y.txt", "y\n", "Y")
	runGit(t, f.work, "push", "-q", "origin", "feature/y")
	head, _ := g.BranchSHA(ctx, "feature/y")
	p, err := a.db.CreatePull(ctx, store.NewPull{RepoID: rid, Title: "Y", Head: "feature/y", Base: "main", HeadSHA: head,
		AuthorKind: ActorToken, AuthorName: "ci-token"})
	if err != nil {
		t.Fatal(err)
	}
	a.emit(ctx, Event{Kind: EventPullOpened, Repo: demo, Number: p.Number, Title: p.Title, Actor: Actor{Kind: ActorToken, Name: "ci-token"}})
	a.actions().bg.Wait()
	run := onlyRun(t, f, 0)
	jb := f.jobs(t, run.ID)["Agent: Review helper"]
	if jb.Status != actions.StatusWaiting || !strings.Contains(jb.Approval, "ci-token") || strings.Contains(jb.Approval, "first run") {
		t.Fatalf("token PR job = %+v", jb)
	}

	// Issues: the owner's issue runs straight away (tools approved) …
	a.emit(ctx, Event{Kind: EventIssueOpened, Repo: demo, Number: 5, Title: "Bug", Actor: Actor{Kind: ActorOwner, Name: "zoe"}})
	a.actions().bg.Wait()
	run = onlyRun(t, f, 1)
	jb = f.jobs(t, run.ID)["Agent: Review helper"]
	if run.Event != "issues" || jb.Status != actions.StatusQueued {
		t.Fatalf("issue run = %+v job = %+v", run, jb)
	}
	if ev, _ := run.EventPayload["issue"].(map[string]any); ev["number"] != float64(5) && ev["number"] != int64(5) {
		t.Errorf("payload = %v", run.EventPayload)
	}
	// … someone else's waits, an agent's own issue never triggers, and labels filter.
	a.emit(ctx, Event{Kind: EventIssueOpened, Repo: demo, Number: 6, Title: "Spam", Actor: Actor{Kind: "import", Name: "stranger"}})
	a.emit(ctx, Event{Kind: EventIssueOpened, Repo: demo, Number: 7, Title: "Loop", Actor: Actor{Kind: ActorJob, Name: "agent:Review helper"}})
	a.emit(ctx, Event{Kind: EventIssueLabeled, Repo: demo, Number: 5, Detail: "bug", Actor: Actor{Kind: ActorOwner, Name: "zoe"}})
	a.actions().bg.Wait()
	run = onlyRun(t, f, 2)
	jb = f.jobs(t, run.ID)["Agent: Review helper"]
	if jb.Status != actions.StatusWaiting || !strings.Contains(jb.Approval, "stranger") {
		t.Fatalf("outside issue job = %+v", jb)
	}
	a.emit(ctx, Event{Kind: EventIssueLabeled, Repo: demo, Number: 5, Detail: "Agent", Actor: Actor{Kind: ActorOwner, Name: "zoe"}})
	a.actions().bg.Wait()
	if run := onlyRun(t, f, 3); run.EventPayload["action"] != "labeled" {
		t.Errorf("labeled run = %+v", run)
	}

	// YAML jobs never get MCP scopes.
	pullsCommit(t, f.work, ".atelier/workflows/ci.yml", "on: push\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: make\n", "CI")
	f.push(t, "feature/y")
	rn := f.registerRunner(t, "Box", []string{"linux"}, []string{"jobs", "agents"})
	for i := 0; i < 5; i++ {
		j := rn.poll()
		if j == nil {
			break
		}
		who, ok := a.jobActor(ctx, j.JobToken)
		if !ok {
			t.Fatal("job token not resolved")
		}
		isAgent := j.Steps[0].Agent != nil
		hasMCP := slices.ContainsFunc(who.Scopes, func(s string) bool { return strings.HasPrefix(s, "mcp:") })
		if isAgent != hasMCP || (isAgent != strings.HasPrefix(who.Name, "agent:")) {
			t.Errorf("job %d (agent=%v) actor %+v", j.ID, isAgent, who)
		}
		rn.runJob(j, actions.StatusSuccess, nil)
	}
}

func TestAgentTemplatesPage(t *testing.T) {
	f := newActFixture(t)
	pullsCommit(t, f.work, "README.md", "# demo\n", "Initial")
	f.push(t, "main")
	body := expectStatus(t, do(t, f.a, "GET", "/zoe/demo/actions/agents/new", nil, f.ck), 200, "templates")
	mustContain(t, body, "templates", "Add an agent", "Review helper", "Weekly tidy", "Release notes", "review-helper.md",
		"weekly-tidy.md", "release-notes.md", "data-copy=\"#agent-tpl-review-helper\"", "ANTHROPIC_API_KEY")
	if strings.Contains(body, "actions.dispatch") || strings.Contains(body, "notifications.list") {
		t.Error("forbidden tools listed")
	}
	// Nothing was committed.
	g, _ := f.a.git.Open("zoe", "demo")
	if sha, _ := g.BranchSHA(context.Background(), "main"); len(f.a.loadWorkflows(context.Background(), f.repoID(t), g, sha)) != 0 {
		t.Error("the templates page committed a workflow")
	}
	// The embedded copies match docs/agents.
	for _, tpl := range agentTemplates() {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", "agents", tpl.File))
		if err != nil || string(b) != tpl.Text {
			t.Errorf("%s differs from docs/agents (%v)", tpl.File, err)
		}
		if w, _ := workflow.ParseAgentic(tpl.File, []byte(tpl.Text)); !w.Valid() {
			t.Errorf("%s: %v", tpl.File, w.Errors)
		}
	}
	if len(agentTemplates()) != 4 {
		t.Errorf("templates = %d", len(agentTemplates()))
	}
}
