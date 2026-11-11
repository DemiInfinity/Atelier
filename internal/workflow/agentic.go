package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Agentic workflows
//
// A Markdown file in .atelier/workflows/ is an agentic workflow: YAML front
// matter between --- lines says when it runs and what the agent may do, and
// the Markdown body is the task:
//
//	---
//	on:
//	  pull_request:
//	engine: claude
//	model: claude-opus-5-5
//	effort: medium
//	tools: [pulls.get, pulls.diff, pulls.comment]
//	---
//	Read the pull request and post one summary comment.
//
// ParseAgentic compiles it to an ordinary Workflow with one job ("agent")
// whose one step ("agent") runs the agent; the Actions engine runs it like
// any other job and adds the agent settings when a runner takes it.

// Agent engines, limits and defaults.
//
// engine: claude calls the Claude API from the runner with an API key secret
// (billed to that key). engine: claude-code runs Claude Code headless on a
// runner where the owner is signed in (the runner capability claude-code), so
// usage counts against their Claude subscription; max-tokens and secret don't
// apply to it, and model and effort default to Claude Code's own.
const (
	AgentEngine           = "claude"
	AgentEngineClaudeCode = "claude-code"
	// AgentWithEngine is the step's with: key naming a claude-code engine, so
	// the scheduler can see it in the stored job without parsing the workflow.
	AgentWithEngine      = "engine"
	AgentDefaultModel    = "claude-opus-5-5"
	AgentDefaultEffort   = "medium"
	AgentDefaultTurns    = 30
	AgentMaxTurns        = 100
	AgentDefaultTokens   = 400_000
	AgentMaxTokens       = 2_000_000
	AgentMinTokens       = 20_000
	AgentDefaultTimeout  = 20
	AgentMaxTimeout      = 360
	AgentDefaultSecret   = "ANTHROPIC_API_KEY"
	AgentMaxTaskBytes    = 64 << 10
	AgentJobID           = "agent"
	AgentStepID          = "agent"
	AgentLockSuffix      = ".lock.yml"
	agentMaxTools        = 64
	agentMaxIssueLabels  = 50
	agentFrontMatterLine = "---"
)

// AgentTools are the MCP tools an agent may be given, in display order.
var AgentTools = []string{
	"repo.info", "repo.list", "code.read", "code.list", "code.search", "commits.list", "commit.get",
	"issues.list", "issues.get", "issues.create", "issues.comment", "issues.close", "issues.label",
	"pulls.list", "pulls.get", "pulls.diff", "pulls.comment", "pulls.note",
	"actions.runs", "actions.logs", "actions.dispatch", "releases.list", "releases.draft", "notifications.list",
	"projects.list", "projects.get", "projects.items", "projects.add_item", "projects.add_draft", "projects.set_field",
	"projects.move_item", "projects.remove_item",
}

// agentWriteTools are the AgentTools that change something. Each tool list is
// approved by the owner as a whole (ToolsKey), so adding any of these to a
// workflow makes its next run wait; AgentToolWrites lets the approval prompt
// and the MCP server agree on which tools write. The projects tools only
// reach projects that link the job's repository.
var agentWriteTools = map[string]bool{
	"issues.create": true, "issues.comment": true, "issues.close": true, "issues.label": true,
	"pulls.comment": true, "pulls.note": true, "actions.dispatch": true, "releases.draft": true,
	"projects.add_item": true, "projects.add_draft": true, "projects.set_field": true,
	"projects.move_item": true, "projects.remove_item": true,
}

// AgentToolWrites reports whether an agent tool changes something.
func AgentToolWrites(name string) bool { return agentWriteTools[name] }

// agentForbiddenTools exist on the MCP server but are never given to agents.
var agentForbiddenTools = map[string]string{
	"actions.dispatch":   "agents can't start workflows",
	"notifications.list": "agents can't read your notifications",
}

// AgentToolAllowed reports whether name is a tool agents may be given.
func AgentToolAllowed(name string) bool {
	return slices.Contains(AgentTools, name) && agentForbiddenTools[name] == ""
}

var (
	agentModelRe = regexp.MustCompile(`^claude-[a-z0-9][a-z0-9.-]{0,62}$`)
	// Claude Code also takes model aliases.
	agentClaudeCodeAliases = []string{"opus", "sonnet", "haiku", "fable"}
	agentSecretRe          = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)
	agentEfforts           = []string{"low", "medium", "high", "xhigh", "max"}
)

// AgentSpec is what the front matter and body say about the agent.
type AgentSpec struct {
	Engine         string
	Model          string
	Effort         string
	Tools          []string // sorted, unique, all allowed
	RunsOn         []string
	MaxTurns       int
	MaxTokens      int
	TimeoutMinutes int
	Secret         string
	Task           string
	Issues         *IssuesTrigger // on: issues (agentic workflows only)
	SourceSHA      string         // sha256 of the whole file
	on             *yaml.Node
}

// IssuesTrigger: on.issues. Types are opened and/or labeled; Labels limits
// labeled to these label names (any label when empty).
type IssuesTrigger struct {
	Types  []string
	Labels []string
}

// Matches reports whether an issue event (opened, or labeled with label) runs the workflow.
func (t *IssuesTrigger) Matches(action, label string) bool {
	if t == nil || !slices.Contains(t.Types, action) {
		return false
	}
	if action == "labeled" && len(t.Labels) > 0 {
		return slices.ContainsFunc(t.Labels, func(l string) bool { return strings.EqualFold(l, label) })
	}
	return true
}

// ClaudeCode reports whether the agent runs on Claude Code (subscription) rather than the Claude API.
func (s *AgentSpec) ClaudeCode() bool { return s != nil && s.Engine == AgentEngineClaudeCode }

// EngineLabel names the engine and who pays, for the workflow page.
func (s *AgentSpec) EngineLabel() string {
	if s.ClaudeCode() {
		return "Claude Code · subscription"
	}
	return "Claude API · API key"
}

// ModelLabel is the model, or Claude Code's default when none is set.
func (s *AgentSpec) ModelLabel() string {
	if s.Model == "" {
		return "Claude Code's default"
	}
	return s.Model
}

// EffortLabel is the effort, or Claude Code's default when none is set.
func (s *AgentSpec) EffortLabel() string {
	if s.Effort == "" {
		return "Claude Code's default"
	}
	return s.Effort
}

// ToolsKey identifies the tool list for approvals ("" for no tools).
func (s *AgentSpec) ToolsKey() string { return strings.Join(s.Tools, ",") }

// splitFrontMatter returns the YAML between the opening and closing --- lines and the body.
func splitFrontMatter(src []byte) (front, body []byte, ok bool) {
	src = bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.HasPrefix(s, agentFrontMatterLine+"\n") {
		return nil, nil, false
	}
	rest := s[len(agentFrontMatterLine)+1:]
	for i := 0; i <= len(rest); {
		j := strings.IndexByte(rest[i:], '\n')
		line := rest[i:]
		if j >= 0 {
			line = rest[i : i+j]
		}
		if strings.TrimRight(line, " \t") == agentFrontMatterLine {
			end := len(rest)
			if j >= 0 {
				end = i + j + 1
			}
			return []byte(rest[:i]), []byte(rest[end:]), true
		}
		if j < 0 {
			break
		}
		i += j + 1
	}
	return nil, nil, false
}

// ParseAgentic reads a Markdown workflow. Like Parse it never fails: problems
// become Errors (the workflow can't run) or Warnings. The spec is nil only
// when the file has no usable front matter.
func ParseAgentic(filePath string, src []byte) (*Workflow, *AgentSpec) {
	base := path.Base(filePath)
	w := &Workflow{Path: filePath, Name: strings.TrimSuffix(base, path.Ext(base)), Agentic: true, Source: src,
		Permissions: Permissions{Contents: "read"}}
	if len(src) > MaxFileSize {
		w.Errors = append(w.Errors, fmt.Sprintf("The workflow file is larger than %d KB.", MaxFileSize/1024))
		return w, nil
	}
	front, body, ok := splitFrontMatter(src)
	if !ok {
		w.Errors = append(w.Errors, "An agentic workflow starts with YAML front matter between two --- lines (on:, engine:, tools: …), followed by the task in Markdown.")
		return w, nil
	}
	sum := sha256.Sum256(src)
	spec := &AgentSpec{Engine: AgentEngine, Model: AgentDefaultModel, Effort: AgentDefaultEffort, RunsOn: []string{"self-hosted"},
		MaxTurns: AgentDefaultTurns, MaxTokens: AgentDefaultTokens, TimeoutMinutes: AgentDefaultTimeout,
		Secret: AgentDefaultSecret, Task: strings.TrimSpace(string(body)), SourceSHA: hex.EncodeToString(sum[:])}

	var doc yaml.Node
	if err := yaml.Unmarshal(front, &doc); err != nil {
		w.Errors = append(w.Errors, "Invalid YAML front matter: "+err.Error())
		return w, spec
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		w.Errors = append(w.Errors, "The front matter must be a YAML mapping (on:, engine:, tools: …).")
		return w, spec
	}
	p := &parser{w: w}
	p.agentRoot(doc.Content[0], spec)
	if spec.Task == "" {
		p.fail("The task is empty: write what the agent should do below the front matter.")
	} else if len(spec.Task) > AgentMaxTaskBytes {
		p.fail("The task is longer than %d KB.", AgentMaxTaskBytes/1024)
	}
	w.Jobs = []*Job{spec.compileJob(w.Name)}
	return w, spec
}

// AgentSpecOf re-reads the agent settings of an agentic workflow (nil if wf isn't one).
func AgentSpecOf(wf *Workflow) *AgentSpec {
	if wf == nil || !wf.Agentic {
		return nil
	}
	_, spec := ParseAgentic(wf.Path, wf.Source)
	return spec
}

func (p *parser) agentRoot(n *yaml.Node, s *AgentSpec) {
	sawOn := false
	seen := map[string]bool{}
	var model, effort *yaml.Node
	for _, kv := range pairs(n) {
		key, v := kv[0].Value, kv[1]
		where := key
		seen[key] = true
		switch key {
		case "name":
			if str, ok := scalar(v); ok && strings.TrimSpace(str) != "" {
				p.w.Name = strings.TrimSpace(str)
			}
		case "on", "true":
			sawOn = true
			s.on = v
			p.agentTriggers(v, s)
		case "engine":
			str, _ := scalar(v)
			switch str = strings.TrimSpace(str); str {
			case AgentEngine, AgentEngineClaudeCode:
				s.Engine = str
			default:
				p.fail("engine: %q isn't supported; use %q (the Claude API, with an API key secret) or %q (Claude Code on a runner signed in to your Claude subscription).",
					str, AgentEngine, AgentEngineClaudeCode)
			}
		case "model":
			model = v
		case "effort":
			effort = v
		case "tools":
			s.Tools = p.agentTools(v)
		case "runs-on":
			if l := p.stringList(v, "runs-on"); len(l) > 0 {
				s.RunsOn = l
			}
		case "max-turns":
			s.MaxTurns = p.agentInt(v, where, AgentDefaultTurns, 1, AgentMaxTurns)
		case "max-tokens":
			s.MaxTokens = p.agentInt(v, where, AgentDefaultTokens, AgentMinTokens, AgentMaxTokens)
		case "timeout-minutes":
			s.TimeoutMinutes = p.agentInt(v, where, AgentDefaultTimeout, 1, AgentMaxTimeout)
		case "secret":
			str, _ := scalar(v)
			if !agentSecretRe.MatchString(str) || strings.HasPrefix(strings.ToUpper(str), "GITHUB_") || strings.EqualFold(str, "ATELIER_TOKEN") {
				p.fail("secret must be the name of a repository or account secret, such as %s.", AgentDefaultSecret)
				continue
			}
			s.Secret = str
		default:
			p.warn("Unsupported key %q in the front matter (ignored).", key)
		}
	}
	if !sawOn {
		p.fail("The workflow has no `on:` triggers.")
	}
	// Engine-dependent settings, checked once the engine is known (keys can come in any order).
	cc := s.Engine == AgentEngineClaudeCode
	if cc {
		s.Model, s.Effort, s.MaxTokens, s.Secret = "", "", 0, ""
		if seen["max-tokens"] {
			p.fail("max-tokens doesn't apply to engine: %s: Claude Code doesn't take a token budget. Remove it and limit the run with max-turns and timeout-minutes.", AgentEngineClaudeCode)
		}
		if seen["secret"] {
			p.fail("secret doesn't apply to engine: %s: it uses the Claude subscription Claude Code is signed in with on the runner, not an API key. Remove it.", AgentEngineClaudeCode)
		}
	}
	if model != nil {
		str, _ := scalar(model)
		str = strings.TrimSpace(str)
		switch {
		case cc && slices.Contains(agentClaudeCodeAliases, str):
			s.Model = str
		case !agentModelRe.MatchString(str):
			if cc {
				p.fail("model: %q isn't a Claude model id or Claude Code alias (%s); leave it out to use Claude Code's default.", str, strings.Join(agentClaudeCodeAliases, ", "))
			} else {
				p.fail("model: %q isn't a Claude model id (for example %s).", str, AgentDefaultModel)
			}
		default:
			s.Model = str
			if _, ok := AgentPrice(str); !ok && !cc {
				p.warn("model: %s has no price in Atelier's table, so runs show no cost estimate.", str)
			}
		}
	}
	if effort != nil {
		str, _ := scalar(effort)
		if !slices.Contains(agentEfforts, str) {
			p.fail("effort must be one of %s.", strings.Join(agentEfforts, ", "))
		} else {
			s.Effort = str
		}
	}
}

func (p *parser) agentInt(v *yaml.Node, what string, def, lo, hi int) int {
	str, ok := scalar(v)
	if !ok {
		p.fail("%s must be a number.", what)
		return def
	}
	str = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(str), "_", ""), ",", "")
	mult := 1
	if k, ok := strings.CutSuffix(strings.ToLower(str), "k"); ok {
		str, mult = k, 1000
	} else if m, ok := strings.CutSuffix(strings.ToLower(str), "m"); ok {
		str, mult = m, 1_000_000
	}
	n, err := strconv.Atoi(str)
	if err != nil {
		p.fail("%s must be a whole number.", what)
		return def
	}
	n *= mult
	if n < lo || n > hi {
		p.fail("%s must be between %d and %d.", what, lo, hi)
		return def
	}
	return n
}

func (p *parser) agentTools(v *yaml.Node) []string {
	list := p.stringList(v, "tools")
	if len(list) > agentMaxTools {
		p.fail("tools lists more than %d tools.", agentMaxTools)
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range list {
		t = strings.TrimSpace(t)
		switch {
		case agentForbiddenTools[t] != "":
			p.fail("tools: %s isn't allowed (%s).", t, agentForbiddenTools[t])
		case !slices.Contains(AgentTools, t):
			p.fail("tools: unknown tool %q.", t)
		case !seen[t]:
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

func (p *parser) agentTriggers(n *yaml.Node, s *AgentSpec) {
	n = resolveAlias(n)
	one := func(event string, cfg *yaml.Node) {
		if event == "issues" {
			s.Issues = p.issuesTrigger(cfg)
			return
		}
		p.trigger(event, cfg)
	}
	switch {
	case n == nil:
		p.fail("`on:` must be an event name, a list of events or a mapping.")
	case n.Kind == yaml.ScalarNode:
		one(n.Value, nil)
	case n.Kind == yaml.SequenceNode:
		for _, c := range n.Content {
			if str, ok := scalar(c); ok {
				one(str, nil)
			}
		}
	case n.Kind == yaml.MappingNode:
		for _, kv := range pairs(n) {
			one(kv[0].Value, kv[1])
		}
	default:
		p.fail("`on:` must be an event name, a list of events or a mapping.")
	}
}

func (p *parser) issuesTrigger(cfg *yaml.Node) *IssuesTrigger {
	t := &IssuesTrigger{}
	for _, kv := range pairs(cfg) {
		switch kv[0].Value {
		case "types":
			for _, ty := range p.stringList(kv[1], "on.issues.types") {
				switch ty {
				case "opened", "labeled", "labelled":
					if ty == "labelled" {
						ty = "labeled"
					}
					if !slices.Contains(t.Types, ty) {
						t.Types = append(t.Types, ty)
					}
				default:
					p.warn("issues type %q isn't supported; agents run on opened and labeled issues.", ty)
				}
			}
		case "labels":
			t.Labels = p.stringList(kv[1], "on.issues.labels")
			if len(t.Labels) > agentMaxIssueLabels {
				t.Labels = t.Labels[:agentMaxIssueLabels]
			}
		default:
			p.warn("on.issues.%s isn't supported (ignored).", kv[0].Value)
		}
	}
	if len(t.Types) == 0 {
		t.Types = []string{"opened"}
	}
	return t
}

// compileJob is the one job an agentic workflow runs.
func (s *AgentSpec) compileJob(name string) *Job {
	step := Step{
		ID: AgentStepID, Name: fmt.Sprintf("Run Claude agent (%s, %s effort)", s.Model, s.Effort),
		// The key is referenced here so the engine resolves (and the runner
		// masks) the secret; the runner reads it from with.api-key.
		With:           map[string]string{"api-key": "${{ secrets." + s.Secret + " }}"},
		TimeoutMinutes: s.TimeoutMinutes,
	}
	if s.ClaudeCode() {
		// No secret: Claude Code uses the subscription it's signed in with on
		// the runner. with.engine lets the scheduler pick a runner that has it.
		step.Name = fmt.Sprintf("Run Claude Code agent (%s, subscription)", s.ModelLabel())
		step.With = map[string]string{AgentWithEngine: AgentEngineClaudeCode}
	}
	return &Job{
		ID: AgentJobID, Name: "Agent: " + name, RunsOn: append([]string(nil), s.RunsOn...),
		TimeoutMinutes: s.TimeoutMinutes, Permissions: Permissions{Contents: "read"},
		Outputs: map[string]string{
			"model": "${{ steps.agent.outputs.model }}", "turns": "${{ steps.agent.outputs.turns }}",
			"input_tokens": "${{ steps.agent.outputs.input_tokens }}", "output_tokens": "${{ steps.agent.outputs.output_tokens }}",
			"cache_read_tokens":  "${{ steps.agent.outputs.cache_read_tokens }}",
			"cache_write_tokens": "${{ steps.agent.outputs.cache_write_tokens }}",
			"stop":               "${{ steps.agent.outputs.stop }}",
			"cost_usd":           "${{ steps.agent.outputs.cost_usd }}",
			"engine":             "${{ steps.agent.outputs.engine }}",
		},
		Steps: []Step{step},
	}
}

// LockPath is where the compiled view of an agentic workflow would live.
func LockPath(p string) string { return strings.TrimSuffix(p, path.Ext(p)) + AgentLockSuffix }

// LockYAML renders the compiled workflow for transparency: exactly what the
// agent runs with. It is shown on the workflow page and doesn't need committing.
func (s *AgentSpec) LockYAML(wf *Workflow) []byte {
	taskSum := sha256.Sum256([]byte(s.Task))
	q := func(v string) string { b, _ := yaml.Marshal(v); return strings.TrimSpace(string(b)) }
	var b strings.Builder
	fmt.Fprintf(&b, "# %s - compiled by Atelier from %s. Don't edit; change the Markdown file.\n", path.Base(LockPath(wf.Path)), path.Base(wf.Path))
	fmt.Fprintf(&b, "# source-sha256: %s\n", s.SourceSHA)
	fmt.Fprintf(&b, "name: %s\n", q(wf.Name))
	if s.on != nil {
		onb, err := yaml.Marshal(map[string]*yaml.Node{"on": s.on})
		if err == nil {
			b.Write(onb)
		}
	}
	b.WriteString("permissions:\n  contents: read\n")
	fmt.Fprintf(&b, "jobs:\n  %s:\n    name: %s\n", AgentJobID, q("Agent: "+wf.Name))
	fmt.Fprintf(&b, "    runs-on: [%s]\n", strings.Join(quoteAll(s.RunsOn, q), ", "))
	fmt.Fprintf(&b, "    timeout-minutes: %d\n", s.TimeoutMinutes)
	b.WriteString("    steps:\n")
	fmt.Fprintf(&b, "      - id: %s\n        name: %s\n        uses: atelier/agent # built into atelier-runner\n        with:\n", AgentStepID, q(wf.Jobs[0].Steps[0].Name))
	if s.ClaudeCode() {
		fmt.Fprintf(&b, "          engine: %s # Claude Code on a runner with the claude-code capability; usage counts against your Claude subscription\n", s.Engine)
		if s.Model == "" {
			b.WriteString("          model: default # Claude Code's default model\n")
		} else {
			fmt.Fprintf(&b, "          model: %s\n", s.Model)
		}
		if s.Effort == "" {
			b.WriteString("          effort: default # Claude Code's default effort\n")
		} else {
			fmt.Fprintf(&b, "          effort: %s\n", s.Effort)
		}
		fmt.Fprintf(&b, "          max-turns: %d\n", s.MaxTurns)
	} else {
		fmt.Fprintf(&b, "          engine: %s\n          model: %s\n          effort: %s\n", s.Engine, s.Model, s.Effort)
		fmt.Fprintf(&b, "          max-turns: %d\n          max-tokens: %d\n", s.MaxTurns, s.MaxTokens)
	}
	if len(s.Tools) == 0 {
		b.WriteString("          tools: [] # no tools: the agent can only write its summary\n")
	} else {
		b.WriteString("          tools:\n")
		for _, t := range s.Tools {
			fmt.Fprintf(&b, "            - %s\n", t)
		}
	}
	b.WriteString("          mcp-url: ${{ atelier.server_url }}/mcp # job token limited to this repository and these tools\n")
	if s.ClaudeCode() {
		b.WriteString("          # no API key: Claude Code's own shell, file and web tools are off; only the tools above\n")
	} else {
		fmt.Fprintf(&b, "          api-key: ${{ secrets.%s }}\n", s.Secret)
	}
	fmt.Fprintf(&b, "          task-sha256: %s # the Markdown body (%d bytes)\n", hex.EncodeToString(taskSum[:]), len(s.Task))
	if s.Issues != nil {
		b.WriteString("# on.issues: " + strings.Join(s.Issues.Types, ", "))
		if len(s.Issues.Labels) > 0 {
			b.WriteString(" (labels: " + strings.Join(s.Issues.Labels, ", ") + ")")
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func quoteAll(in []string, q func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = q(s)
	}
	return out
}

// AgentModelPrice is a model's list price in US dollars per million tokens.
type AgentModelPrice struct {
	Input, Output, CacheRead, CacheWrite float64
	// Long applies above LongAbove prompt tokens (0 = never).
	LongAbove int64
	Long      *AgentModelPrice
}

// agentPrices are first-party Claude API list prices (cached 2026-11-03).
// Costs computed from them are estimates: fallbacks, long-context tiers and
// price changes aren't tracked.
var agentPrices = map[string]AgentModelPrice{
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.20, CacheWrite: 5},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.10, CacheWrite: 2.5},
	"claude-haiku-5-5": {Input: 0.10, Output: 0.50, CacheRead: 0.01, CacheWrite: 0.125, LongAbove: 100_000,
		Long: &AgentModelPrice{Input: 0.50, Output: 2.50, CacheRead: 0.05, CacheWrite: 0.625}},
	"claude-fable-5-2":  {Input: 8, Output: 40, CacheRead: 0.20, CacheWrite: 10},
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-opus-5":     {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.20, CacheWrite: 2.5},
	"claude-opus-4-8":   {Input: 5, Output: 25, CacheRead: 0.50, CacheWrite: 6.25},
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheRead: 0.30, CacheWrite: 3.75},
	"claude-haiku-4-5":  {Input: 1, Output: 5, CacheRead: 0.10, CacheWrite: 1.25},
}

// AgentPrice returns the list price of a model.
func AgentPrice(model string) (AgentModelPrice, bool) {
	p, ok := agentPrices[model]
	return p, ok
}

// AgentUsage is token usage of one agent run (or one turn).
type AgentUsage struct {
	Input, Output, CacheRead, CacheWrite int64
}

// Add accumulates u2 into u.
func (u *AgentUsage) Add(u2 AgentUsage) {
	u.Input += u2.Input
	u.Output += u2.Output
	u.CacheRead += u2.CacheRead
	u.CacheWrite += u2.CacheWrite
}

// Counted is what the max-tokens budget counts: output plus every input token
// not served from the cache (cache reads are reported but not counted).
func (u AgentUsage) Counted() int64 { return u.Input + u.Output + u.CacheWrite }

// EstimateCost estimates the cost in US dollars, per turn when turns are known
// (long-context tiers apply per request). ok is false for unknown models.
func EstimateCost(model string, turns ...AgentUsage) (float64, bool) {
	p, ok := AgentPrice(model)
	if !ok {
		return 0, false
	}
	total := 0.0
	for _, u := range turns {
		pp := p
		if p.Long != nil && p.LongAbove > 0 && u.Input+u.CacheRead+u.CacheWrite > p.LongAbove {
			pp = *p.Long
		}
		total += (float64(u.Input)*pp.Input + float64(u.Output)*pp.Output + float64(u.CacheRead)*pp.CacheRead +
			float64(u.CacheWrite)*pp.CacheWrite) / 1e6
	}
	return total, true
}
