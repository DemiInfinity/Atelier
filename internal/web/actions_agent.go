package web

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
	"atelier/internal/workflow"
)

// Agentic workflows (Markdown files in .atelier/workflows/).
//
// internal/workflow/agentic.go compiles one into an ordinary workflow with a
// single "agent" job, so the engine triggers, queues, approves and logs it
// like any other. This file adds what's special about agents:
//
//   - approvals: the first run of an agentic workflow in a repository, every
//     change to its tools: list, and runs for pull requests or issues opened
//     by anyone but the owner wait for the owner (the Actions approval gate);
//   - when a runner takes the job, the agent settings (task, model, limits,
//     tools, MCP URL) are added to the step, and the job is recorded so its
//     job token resolves to Actor{Kind: "job", Name: "agent:<workflow>"} with
//     an mcp:<tool> scope per allowed tool;
//   - issues triggers (opened, labeled), which YAML workflows don't have;
//   - the agent panel on the workflow page (compiled lock view, approvals,
//     token usage and estimated cost) and the "Add an agent" templates.
//
// Repository content, issue text and model output never reach any of these
// decisions: the tools come only from the committed workflow file at the
// run's commit, and approvals only from the owner.

// EventIssueLabeled is emitted when a label is added to an issue: Number is
// the issue, Detail the label name. Agentic workflows with
// `on: issues: types: [labeled]` listen for it.
const EventIssueLabeled = "issue.labeled"

//go:embed agent_templates/*.md
var agentTemplateFS embed.FS

func init() {
	registerFeature(feature{name: "agents", routes: func(a *App, m *http.ServeMux) {
		m.HandleFunc("GET /{owner}/{repo}/actions/agents/new", a.authed(a.agentTemplatesPage))
	}})
	registerEventHandler(agentIssueEventHandler)
}

// ---------- approvals ----------

// agentApprovalReason says why a new run of an agentic workflow must wait for
// the owner ("" if it may run).
func (a *App) agentApprovalReason(ctx context.Context, rp store.Repo, wf *workflow.Workflow, spec *workflow.AgentSpec, t actTrigger) string {
	var reasons []string
	if _, err := a.db.AgentToolsApproval(ctx, rp.ID, wf.Path, spec.ToolsKey()); err != nil {
		tools := "no tools"
		if len(spec.Tools) > 0 {
			tools = strings.Join(spec.Tools, ", ")
			var writes []string
			for _, t := range spec.Tools {
				if workflow.AgentToolWrites(t) {
					writes = append(writes, t)
				}
			}
			if len(writes) > 0 {
				tools += " (these change things: " + strings.Join(writes, ", ") + ")"
			}
		}
		if prev, err := a.db.LatestAgentApproval(ctx, rp.ID, wf.Path); err == nil {
			was := "no tools"
			if prev.ToolsKey != "" {
				was = strings.ReplaceAll(prev.ToolsKey, ",", ", ")
			}
			reasons = append(reasons, fmt.Sprintf("The agent's tools changed (was: %s; now: %s), so it waits for your approval.", was, tools))
		} else {
			reasons = append(reasons, fmt.Sprintf("This is the first run of the agentic workflow %s in this repository, so it waits for your approval. It may use: %s.", wf.Name, tools))
		}
	}
	if t.Event == "pull_request" {
		kind, login := agentPullAuthor(t.Payload)
		if kind != ActorOwner {
			reasons = append(reasons, fmt.Sprintf("Pull request #%d was opened by %s (%s), not you, so the agent waits for your approval.", t.PullNumber, login, kind))
		}
	}
	return strings.Join(reasons, " ")
}

func agentPullAuthor(payload map[string]any) (kind, login string) {
	pr, _ := payload["pull_request"].(map[string]any)
	user, _ := pr["user"].(map[string]any)
	kind, _ = user["type"].(string)
	login, _ = user["login"].(string)
	if kind == "" {
		kind = "unknown"
	}
	return kind, login
}

// agentGateJobs adds the approval reason to the agent job of a new run.
func (a *App) agentGateJobs(ctx context.Context, rp store.Repo, wf *workflow.Workflow, t actTrigger, jobs []store.ActionsJob) {
	spec := workflow.AgentSpecOf(wf)
	if spec == nil {
		return
	}
	reason := a.agentApprovalReason(ctx, rp, wf, spec, t)
	if reason == "" {
		return
	}
	for i := range jobs {
		j := &jobs[i]
		if j.JobKey != workflow.AgentJobID || j.Status != store.JobPending {
			continue
		}
		switch {
		case j.Approval == "":
			j.Approval = reason
		case !strings.Contains(j.Approval, reason):
			j.Approval += " " + reason
		}
	}
}

// ---------- runner hand-off and job tokens ----------

// agentJobCaps lists the runner capabilities an agent job needs: every agent
// job needs a runner that understands agent steps (older runners don't send
// "agents" and fail them), and engine: claude-code also needs Claude Code
// signed in on that device. It reads the stored job, whose agent step
// carries with.engine for claude-code. nil for ordinary jobs.
func agentJobCaps(j store.ActionsJob) []string {
	if j.JobKey != workflow.AgentJobID || j.Payload == "" {
		return nil
	}
	pj, err := actDecodeJob(j.Payload)
	if err != nil || !workflow.IsAgenticFile(pj.Workflow) {
		return nil
	}
	caps := []string{actions.CapAgents}
	for _, st := range pj.Steps {
		if st.ID == workflow.AgentStepID && st.With[workflow.AgentWithEngine] == workflow.AgentEngineClaudeCode {
			caps = append(caps, actions.CapClaudeCode)
		}
	}
	return caps
}

// prepareAgentJob fills in the agent step when a runner takes an agent job,
// records the job for its token scopes, and remembers the owner's approval of
// the tool list. It returns an error message for the job, or "".
func (a *App) prepareAgentJob(ctx context.Context, pj *actions.Job, j store.ActionsJob, run store.ActionsRun, rp store.Repo, base string) string {
	wf := a.runWorkflow(ctx, rp, run)
	spec := workflow.AgentSpecOf(wf)
	if wf == nil || spec == nil || !wf.Valid() {
		return "The agentic workflow couldn't be loaded from this commit."
	}
	// Defence in depth: an agent never runs with a tool list the owner hasn't approved.
	if _, err := a.db.AgentToolsApproval(ctx, rp.ID, wf.Path, spec.ToolsKey()); err != nil {
		if j.ApprovedBy == "" {
			return "The agent's tools haven't been approved for this repository. Re-run the workflow to approve them."
		}
		if err := a.db.ApproveAgentTools(ctx, rp.ID, wf.Path, spec.ToolsKey(), j.ApprovedBy); err != nil {
			log.Printf("actions: recording agent approval for %s in %s: %v", wf.Path, rp.FullName(), err)
			return "The agent's approval couldn't be saved."
		}
	}
	tools := make([]string, 0, len(spec.Tools))
	for _, t := range spec.Tools {
		if workflow.AgentToolAllowed(t) {
			tools = append(tools, t)
		}
	}
	found := false
	for i := range pj.Steps {
		if pj.Steps[i].ID != workflow.AgentStepID {
			continue
		}
		found = true
		pj.Steps[i].Agent = &actions.AgentStep{
			Engine: spec.Engine, Model: spec.Model, Effort: spec.Effort, Task: spec.Task, Tools: tools,
			MaxTurns: spec.MaxTurns, MaxTokens: spec.MaxTokens, TimeoutMinutes: spec.TimeoutMinutes,
			MCPURL: strings.TrimRight(base, "/") + "/mcp", WorkflowName: wf.Name,
		}
	}
	if !found {
		return "The agent job has no agent step."
	}
	if err := a.db.SetAgentJob(ctx, store.AgentJob{JobID: j.ID, RepoID: rp.ID, WorkflowPath: wf.Path, WorkflowName: wf.Name, Tools: tools}); err != nil {
		log.Printf("actions: recording agent job %d: %v", j.ID, err)
		return "The agent job couldn't be prepared."
	}
	return ""
}

// agentJobActor turns the job token of an agent job into its Actor: the
// usual repository scopes (no packages) plus mcp:<tool> for each allowed
// tool. ok is false for ordinary jobs.
func (a *App) agentJobActor(ctx context.Context, t store.ActionsJobToken, base []string) (Actor, bool) {
	aj, err := a.db.AgentJobByID(ctx, t.JobID)
	if err != nil {
		return Actor{}, false
	}
	scopes := []string{}
	for _, s := range base {
		if !strings.HasPrefix(s, "packages:") {
			scopes = append(scopes, s) // keeps actions:repo:<id> second (jobRepoID)
		}
	}
	if aj.RepoID != t.RepoID {
		return Actor{Kind: ActorJob, Name: "agent:" + aj.WorkflowName, PersonID: t.PersonID, Scopes: scopes}, true
	}
	for _, tool := range aj.Tools {
		if workflow.AgentToolAllowed(tool) {
			scopes = append(scopes, "mcp:"+tool)
		}
	}
	return Actor{Kind: ActorJob, Name: "agent:" + aj.WorkflowName, PersonID: t.PersonID, Scopes: scopes}, true
}

// ---------- issues triggers ----------

// agentIssueEventHandler starts agentic workflows on issues opened or labeled.
// Issues opened by agents and jobs never trigger agents (no loops).
func agentIssueEventHandler(ctx context.Context, a *App, ev Event) {
	var action, label string
	switch ev.Kind {
	case EventIssueOpened:
		action = "opened"
	case EventIssueLabeled:
		action, label = "labeled", strings.TrimSpace(ev.Detail)
	default:
		return
	}
	if ev.Actor.Kind == ActorJob || ev.Actor.Kind == "agent" || strings.HasPrefix(ev.Actor.Name, "agent:") {
		return
	}
	rp := ev.Repo
	if rp.ID == 0 {
		found, err := a.repoByIDForActions(ctx, ev.RepoID)
		if err != nil {
			return
		}
		rp = found
	}
	a.actions().go_(func() {
		ctx := context.WithoutCancel(ctx)
		a.triggerAgentIssue(ctx, rp, ev, action, label)
	})
}

func (a *App) triggerAgentIssue(ctx context.Context, rp store.Repo, ev Event, action, label string) {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil || g.IsEmpty(ctx) {
		return
	}
	def := g.DefaultBranch(ctx)
	sha, err := g.BranchSHA(ctx, def)
	if err != nil {
		return
	}
	for _, wf := range a.loadWorkflows(ctx, rp.ID, g, sha) {
		if !wf.Agentic || !wf.Valid() {
			continue
		}
		spec := workflow.AgentSpecOf(wf)
		if spec == nil || !spec.Issues.Matches(action, label) {
			continue
		}
		payload := map[string]any{
			"action":     action,
			"issue":      map[string]any{"number": ev.Number, "title": ev.Title, "user": map[string]any{"login": ev.Actor.Name, "type": ev.Actor.Kind}},
			"repository": map[string]any{"full_name": rp.FullName(), "name": rp.Name, "owner": map[string]any{"login": rp.Owner}},
		}
		if label != "" {
			payload["label"] = map[string]any{"name": label}
		}
		t := actTrigger{Event: "issues", Action: action, Ref: "refs/heads/" + def, SHA: sha, Actor: ev.Actor, Payload: payload,
			Workflow: wf.Path, Title: ev.Title}
		if ev.Actor.Kind != ActorOwner {
			t.Approval = fmt.Sprintf("Issue #%d was opened or labelled by %s (%s), not you, so the agent waits for your approval.", ev.Number, ev.Actor.Name, ev.Actor.Kind)
		}
		if _, err := a.startRun(ctx, rp, g, wf, t); err != nil {
			log.Printf("actions: starting %s in %s for issue #%d: %v", wf.Path, rp.FullName(), ev.Number, err)
		}
	}
}

// ---------- usage and cost ----------

// agentRunUsage is one run's token usage, read from the agent job's outputs.
type agentRunUsage struct {
	Model                                string
	Subscription                         bool // engine: claude-code: usage counts against the Claude subscription, nothing is billed
	Turns                                int64
	Input, Output, CacheRead, CacheWrite int64
	Cost                                 float64
	HasCost                              bool
	Stop                                 string
}

func agentOutputInt(m map[string]string, k string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(m[k]), 10, 64)
	if n < 0 {
		return 0
	}
	return n
}

// agentUsageFromJob reads usage from an agent job's outputs (ok=false if none).
func agentUsageFromJob(j store.ActionsJob) (agentRunUsage, bool) {
	o := j.Outputs
	if j.JobKey != workflow.AgentJobID || len(o) == 0 || o["model"] == "" {
		return agentRunUsage{}, false
	}
	u := agentRunUsage{Model: o["model"], Turns: agentOutputInt(o, "turns"), Input: agentOutputInt(o, "input_tokens"),
		Output: agentOutputInt(o, "output_tokens"), CacheRead: agentOutputInt(o, "cache_read_tokens"),
		CacheWrite: agentOutputInt(o, "cache_write_tokens"), Stop: o["stop"], Subscription: o["engine"] == workflow.AgentEngineClaudeCode}
	if u.Subscription {
		// Claude Code's own figure, if it reported one, is what the run would
		// have cost at API prices; nothing is billed, so no estimate of ours.
		if c, err := strconv.ParseFloat(strings.TrimSpace(o["cost_usd"]), 64); err == nil && c >= 0 && c < 1e6 {
			u.Cost, u.HasCost = c, true
		}
		return u, true
	}
	if c, err := strconv.ParseFloat(strings.TrimSpace(o["cost_usd"]), 64); err == nil && c >= 0 && c < 1e6 {
		u.Cost, u.HasCost = c, true
	} else {
		u.Cost, u.HasCost = workflow.EstimateCost(u.Model, workflow.AgentUsage{Input: u.Input, Output: u.Output,
			CacheRead: u.CacheRead, CacheWrite: u.CacheWrite})
	}
	return u, true
}

// Tokens is the total shown for a run.
func (u agentRunUsage) Tokens() string {
	return agentTokens(u.Input + u.Output + u.CacheRead + u.CacheWrite)
}

// CostText is the estimated cost, labelled as an estimate.
func (u agentRunUsage) CostText() string {
	if u.Subscription {
		if u.HasCost {
			return "subscription usage (≈ " + agentDollars(u.Cost) + " at API prices, not billed)"
		}
		return "subscription usage"
	}
	if !u.HasCost {
		return "no estimate for this model"
	}
	return "≈ " + agentDollars(u.Cost) + " (estimate)"
}

// Detail breaks the tokens down.
func (u agentRunUsage) Detail() string {
	return fmt.Sprintf("%s in · %s out · %s cache read · %s cache write · %d turns", agentTokens(u.Input), agentTokens(u.Output),
		agentTokens(u.CacheRead), agentTokens(u.CacheWrite), u.Turns)
}

func agentTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM tokens", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%dk tokens", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fk tokens", float64(n)/1000)
	}
	return fmt.Sprintf("%d tokens", n)
}

func agentDollars(c float64) string {
	if c > 0 && c < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", c)
}

// agentRunInfo is shown on the run page of an agentic workflow.
type agentRunInfo struct {
	Usage *agentRunUsage
}

// agentRunPanel returns the run page's agent info (nil for YAML workflows).
func (a *App) agentRunPanel(run store.ActionsRun, jobs []store.ActionsJob) *agentRunInfo {
	if !workflow.IsAgenticFile(run.WorkflowPath) {
		return nil
	}
	info := &agentRunInfo{}
	for _, j := range jobs {
		if u, ok := agentUsageFromJob(j); ok {
			info.Usage = &u
		}
	}
	return info
}

// ---------- workflow page panel ----------

type agentRunRow struct {
	URL       string
	RunNumber int64
	Outcome   string
	When      time.Time
	Usage     *agentRunUsage
}

type agentPanelData struct {
	Spec       *workflow.AgentSpec
	Valid      bool
	Tools      []string
	LockPath   string
	Lock       []codeLine
	Approved   bool
	ApprovedBy string
	ApprovedAt time.Time
	Changed    bool // an earlier tool list was approved, this one isn't
	Runs       []agentRunRow
	TotalCost  string
	NewURL     string
}

// agentPanel builds the agent panel of an agentic workflow's page.
func (a *App) agentPanel(ctx context.Context, v *repoView, wf *workflow.Workflow) *agentPanelData {
	spec := workflow.AgentSpecOf(wf)
	d := &agentPanelData{Spec: spec, Valid: wf.Valid() && spec != nil, LockPath: workflow.LockPath(wf.Path),
		NewURL: v.Base + "/actions/agents/new"}
	if spec == nil {
		return d
	}
	d.Tools = spec.Tools
	if d.Valid {
		d.Lock = codeLines(spec.LockYAML(wf))
	}
	if ap, err := a.db.AgentToolsApproval(ctx, v.Repo.ID, wf.Path, spec.ToolsKey()); err == nil {
		d.Approved, d.ApprovedBy, d.ApprovedAt = true, ap.ApprovedBy, ap.ApprovedAt
	} else if _, err := a.db.LatestAgentApproval(ctx, v.Repo.ID, wf.Path); err == nil {
		d.Changed = true
	}
	runs, err := a.db.ActionsRuns(ctx, store.ActionsRunFilter{RepoID: v.Repo.ID, Workflow: wf.Path, Limit: 10})
	if err != nil {
		return d
	}
	total, priced := 0.0, false
	for _, run := range runs {
		row := agentRunRow{URL: actionsRunURL(v.Repo, run), RunNumber: run.RunNumber, Outcome: run.Outcome(), When: run.CreatedAt}
		if jobs, err := a.db.ActionsJobs(ctx, run.ID); err == nil {
			for _, j := range jobs {
				if u, ok := agentUsageFromJob(j); ok {
					row.Usage = &u
					if u.HasCost && !u.Subscription {
						total, priced = total+u.Cost, true
					}
				}
			}
		}
		d.Runs = append(d.Runs, row)
	}
	if priced {
		d.TotalCost = "≈ " + agentDollars(total)
	}
	return d
}

// ---------- "Add an agent" ----------

type agentTemplate struct {
	File, Title, Summary, Text, ID string
}

var agentTemplateSummaries = map[string]string{
	"review-helper.md":             "On every pull request: reads the diff and the linked issue, posts one summary comment and up to five inline notes. It can't merge.",
	"weekly-tidy.md":               "Every Monday: closes issues fixed by merged pull requests (with a comment) and labels stale ones.",
	"release-notes.md":             "When you run it: drafts release notes since the last release as a draft release for you to publish.",
	"review-helper-claude-code.md": "The review helper on Claude Code: runs on a runner where you're signed in to Claude Code, so it uses your Claude subscription instead of an API key.",
}

func agentTemplates() []agentTemplate {
	entries, _ := fs.ReadDir(agentTemplateFS, "agent_templates")
	var out []agentTemplate
	for _, e := range entries {
		b, err := agentTemplateFS.ReadFile(path.Join("agent_templates", e.Name()))
		if err != nil {
			continue
		}
		wf, _ := workflow.ParseAgentic(e.Name(), b)
		out = append(out, agentTemplate{File: e.Name(), Title: wf.Name, Summary: agentTemplateSummaries[e.Name()], Text: string(b),
			ID: "agent-tpl-" + strings.TrimSuffix(e.Name(), ".md")})
	}
	order := []string{"review-helper.md", "review-helper-claude-code.md", "weekly-tidy.md", "release-notes.md"}
	sort.SliceStable(out, func(i, j int) bool {
		return slices.Index(order, out[i].File) < slices.Index(order, out[j].File)
	})
	return out
}

type agentTemplatesData struct {
	V         *repoView
	Templates []agentTemplate
	Tools     []string
	Secret    string
	Model     string
}

// agentTemplatesPage shows ready-to-copy agentic workflows. It commits nothing.
func (a *App) agentTemplatesPage(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "actions")
	if !ok {
		return
	}
	var tools []string
	for _, t := range workflow.AgentTools {
		if workflow.AgentToolAllowed(t) {
			tools = append(tools, t)
		}
	}
	a.render(w, r, "agents_new", page{Title: "Add an agent · " + v.Repo.FullName(), Nav: "repos", Data: &agentTemplatesData{
		V: v, Templates: agentTemplates(), Tools: tools, Secret: workflow.AgentDefaultSecret, Model: workflow.AgentDefaultModel}})
}
