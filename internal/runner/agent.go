package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"atelier/internal/actions"
	"atelier/internal/workflow"
)

// Agent steps (agentic Markdown workflows). The server sends the job's one
// step with Agent set and with.api-key = ${{ secrets.<name> }}; the runner
// runs the agent loop in agent_loop.go against Atelier's MCP server
// (agent_mcp.go) and reports usage as step outputs and a step summary.

// agentStep returns the agent settings of a top-level step, or nil.
func (j *jobRun) agentStep(st stepDef) *actions.AgentStep {
	if st.Run != "" || st.Uses != "" {
		return nil
	}
	for _, s := range j.job.Steps {
		if s.Index == st.Index && s.Agent != nil {
			return s.Agent
		}
	}
	return nil
}

// runAgent runs an agent step. A non-zero code fails the step.
func (j *jobRun) runAgent(ctx context.Context, sc *scope, st stepDef, stepEnv envMap, ag *actions.AgentStep, fc *fileCmds, cs *cmdState) (int, error) {
	with, err := j.evalWith(sc, st, stepEnv)
	if err != nil {
		return 1, err
	}
	if len(ag.Tools) > 0 {
		if err := checkMCPURL(ag.MCPURL, j.r.Config.ServerURL); err != nil {
			return 1, err
		}
	}
	logf := func(stream, text string) { j.log.Line(sc.logStep, stream, text) }
	var res agentResult
	var runErr error
	engine := ag.Engine
	if engine == workflow.AgentEngineClaudeCode {
		// Claude Code on this device, signed in to the owner's subscription (agent_claudecode.go).
		if !j.r.Config.HasCap(actions.CapClaudeCode) {
			return 1, errors.New("agent: this runner hasn't switched on Claude Code: run `atelier-runner claude-code enable` on it")
		}
		s := &claudeCodeSession{Step: *ag, JobToken: j.job.JobToken, RunContext: j.agentRunContext(),
			Binary: j.r.Config.ClaudeCodePath, Log: logf}
		res, runErr = s.run(ctx)
	} else {
		engine = workflow.AgentEngine
		key := strings.TrimSpace(with["api-key"])
		j.mask.Add(key)
		s := &agentSession{Step: *ag, APIKey: key, JobToken: j.job.JobToken, RunContext: j.agentRunContext(),
			APIBaseURL: agentAPIBaseURL(), HTTPClient: agentHTTPClient, Log: logf}
		res, runErr = s.run(ctx)
	}

	cs.setOutput("engine", engine)
	cs.setOutput("model", res.Model)
	cs.setOutput("turns", strconv.Itoa(res.Turns))
	cs.setOutput("input_tokens", strconv.FormatInt(res.Usage.Input, 10))
	cs.setOutput("output_tokens", strconv.FormatInt(res.Usage.Output, 10))
	cs.setOutput("cache_read_tokens", strconv.FormatInt(res.Usage.CacheRead, 10))
	cs.setOutput("cache_write_tokens", strconv.FormatInt(res.Usage.CacheWrite, 10))
	cs.setOutput("stop", res.Stop)
	if res.HasCost {
		cs.setOutput("cost_usd", fmt.Sprintf("%.6f", res.Cost))
	}
	if err := appendSummary(fc.summary, agentSummary(*ag, res, runErr)); err != nil {
		j.log.System(sc.logStep, "Warning: couldn't write the step summary: "+err.Error())
	}
	if runErr != nil {
		if ctx.Err() != nil { // cancelled or timed out: runOneStep reports why
			return -1, nil
		}
		return 1, fmt.Errorf("agent: %s", runErr.Error())
	}
	j.log.System(sc.logStep, fmt.Sprintf("Agent finished after %d turns.", res.Turns))
	return 0, nil
}

func appendSummary(path, md string) error {
	if path == "" {
		return errors.New("no summary file")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(md)
	return err
}

// agentSummary is the Markdown shown on the run page.
func agentSummary(ag actions.AgentStep, res agentResult, runErr error) string {
	var b strings.Builder
	b.WriteString("### Agent: " + mdEscape(ag.WorkflowName) + "\n\n")
	if runErr != nil {
		b.WriteString("**Stopped:** " + mdEscape(runErr.Error()) + "\n\n")
	}
	if f := strings.TrimSpace(res.Final); f != "" {
		if len(f) > 20000 {
			f = f[:20000] + "\n\n… (summary truncated)"
		}
		b.WriteString(f + "\n\n")
	}
	cost := "no estimate for this model"
	switch {
	case res.Subscription && res.HasCost:
		cost = fmt.Sprintf("subscription usage (≈ $%.4f at API prices, as reported by Claude Code; not billed)", res.Cost)
	case res.Subscription:
		cost = "subscription usage"
	case res.HasCost:
		cost = fmt.Sprintf("≈ $%.4f (estimate from list prices)", res.Cost)
	}
	fmt.Fprintf(&b, "| Model | Turns | Input | Output | Cache read | Cache write | Cost |\n|---|---|---|---|---|---|---|\n| `%s` | %d | %s | %s | %s | %s | %s |\n",
		res.Model, res.Turns, thousands(res.Usage.Input), thousands(res.Usage.Output), thousands(res.Usage.CacheRead),
		thousands(res.Usage.CacheWrite), cost)
	return b.String()
}

func mdEscape(s string) string {
	return strings.NewReplacer("\n", " ", "|", "\\|", "<", "&lt;", ">", "&gt;").Replace(s)
}

// agentRunContext describes the run to the agent: only numbers, names and
// refs, never titles or bodies (it reads those through tools, as data).
func (j *jobRun) agentRunContext() string {
	lines := []string{
		"repository: " + j.repoFull(),
		"event: " + j.job.Event,
		"ref: " + j.job.Ref,
		"commit: " + j.job.SHA,
	}
	ev, _ := j.job.Context["event"].(map[string]any)
	if pr, ok := ev["pull_request"].(map[string]any); ok {
		lines = append(lines, "pull request: #"+numString(pr["number"]))
		if b, ok := pr["base"].(map[string]any); ok {
			lines = append(lines, fmt.Sprintf("base branch: %v", b["ref"]))
		}
		if h, ok := pr["head"].(map[string]any); ok {
			lines = append(lines, fmt.Sprintf("head branch: %v", h["ref"]))
		}
	}
	if is, ok := ev["issue"].(map[string]any); ok {
		lines = append(lines, "issue: #"+numString(is["number"]))
	}
	if a, ok := ev["action"].(string); ok && a != "" {
		lines = append(lines, "action: "+a)
	}
	if l, ok := ev["label"].(map[string]any); ok {
		lines = append(lines, fmt.Sprintf("label: %v", l["name"]))
	}
	if in, ok := j.job.Context["inputs"].(map[string]any); ok && len(in) > 0 {
		keys := make([]string, 0, len(in))
		for k := range in {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := fmt.Sprint(in[k])
			if len(v) > 500 {
				v = v[:500] + "…"
			}
			lines = append(lines, fmt.Sprintf("input %s: %s", k, v))
		}
	}
	return strings.Join(lines, "\n")
}

func numString(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case int:
		return strconv.Itoa(n)
	}
	return fmt.Sprint(v)
}

// agentAPIBaseURL lets the runner's own environment point the Claude API at
// a proxy (ANTHROPIC_BASE_URL); jobs can't change it.
var agentAPIBaseURL = func() string { return os.Getenv("ANTHROPIC_BASE_URL") }

// agentHTTPClient is used for the Claude API and MCP (nil = defaults; tests swap it).
var agentHTTPClient *http.Client
