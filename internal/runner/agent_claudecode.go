package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"atelier/internal/actions"
	"atelier/internal/workflow"
)

// The claude-code agent engine: Claude Code runs headless (claude -p) on this
// device, signed in to the owner's Claude subscription, so agent runs count
// against the subscription instead of an API key. Atelier's MCP server is its
// only tool source: the job token goes in a temporary MCP config file (mode
// 0600, deleted afterwards), never in argv or the environment, and Claude
// Code's own tools (shell, file edits, web) are switched off.
//
// Every flag below was checked against `claude --help` of Claude Code 2.1.305
// and by letting its argument parser accept them with empty input (which
// stops before any model request). --max-turns isn't listed by --help but is
// accepted by the parser; an unknown flag is refused ("unknown option").
//
//	-p                          print mode; the prompt comes on stdin
//	--output-format stream-json one JSON event per line (needs --verbose with -p)
//	--verbose
//	--max-turns N
//	--permission-mode dontAsk   anything not pre-allowed is denied, never asked
//	--permission-prompts none   nobody answers prompts: they're denied
//	--tools ""                  no built-in tools (Bash, Edit, Write, WebFetch, …)
//	--strict-mcp-config         only the MCP servers from --mcp-config
//	--mcp-config FILE           Atelier's /mcp with the job token
//	--allowed-tools a,b         mcp__atelier__<tool> for each allowed tool
//	--setting-sources project   no user settings (hooks, allow rules); the
//	                            working directory is empty, so no project ones
//	--no-session-persistence    nothing saved to ~/.claude for resuming
//	--disable-slash-commands    no skills or commands
//	--append-system-prompt      the same agent rules as the API engine
//	--model M, --effort E       only when the workflow sets them

const (
	claudeCodeBinary    = "claude"
	claudeCodeMCPServer = "atelier" // the server name in the MCP config; tools are mcp__atelier__<name>
	claudeCodeCheckWait = 30 * time.Second
)

// claudeCodeForbiddenTools are Claude Code's own tools that would reach the
// shell, files or the web. If Claude Code ever reports one as available the
// run is stopped (they're switched off with --tools "").
var claudeCodeForbiddenTools = []string{"Bash", "BashOutput", "KillShell", "PowerShell", "Edit", "MultiEdit", "Write",
	"NotebookEdit", "WebFetch", "WebSearch"}

// claudeCodeToolName is the name Claude Code gives an Atelier MCP tool:
// mcp__<server>__<tool>, with characters other than letters, digits, _ and -
// replaced by _ (pulls.diff → mcp__atelier__pulls_diff).
func claudeCodeToolName(mcpName string) string {
	var b strings.Builder
	for _, r := range mcpName {
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return "mcp__" + claudeCodeMCPServer + "__" + b.String()
}

// claudeCodeAllowedTools maps the workflow's tools to Claude Code tool names.
// Tools agents may never use are dropped again here.
func claudeCodeAllowedTools(tools []string) (names []string, byName map[string]string) {
	byName = map[string]string{}
	for _, t := range tools {
		if !workflow.AgentToolAllowed(t) {
			continue
		}
		n := claudeCodeToolName(t)
		if _, dup := byName[n]; dup {
			continue
		}
		byName[n] = t
		names = append(names, n)
	}
	slices.Sort(names)
	return names, byName
}

// claudeCodeEnv is the environment Claude Code runs with: the runner's own,
// without API keys, so it always uses the subscription it's signed in with.
// Nothing from the job (secrets, the job token) is added.
func claudeCodeEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// findClaudeCode resolves the claude binary: path if set, else claude on PATH.
func findClaudeCode(path string) (string, error) {
	if path == "" {
		p, err := exec.LookPath(claudeCodeBinary)
		if err != nil {
			return "", errors.New("Claude Code (claude) isn't on PATH; install it or pass --path")
		}
		path = p
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s isn't an executable Claude Code binary", abs)
	}
	return abs, nil
}

// ---------- the enable check ----------

// ClaudeCodeCheck is what `atelier-runner claude-code enable` found.
type ClaudeCodeCheck struct {
	Path         string
	Version      string
	LoggedIn     bool   // false with a warning when sign-in couldn't be checked
	AuthMethod   string // e.g. claude.ai (a subscription)
	Subscription string // e.g. max, pro
	Warnings     []string
}

// CheckClaudeCode checks that Claude Code is installed and signed in, without
// using the model: `claude --version` and `claude auth status --json`.
func CheckClaudeCode(ctx context.Context, path string) (ClaudeCodeCheck, error) {
	var c ClaudeCodeCheck
	p, err := findClaudeCode(path)
	if err != nil {
		return c, err
	}
	c.Path = p
	run := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, claudeCodeCheckWait)
		defer cancel()
		cmd := exec.CommandContext(ctx, p, args...)
		cmd.Env = claudeCodeEnv()
		cmd.Dir = os.TempDir()
		cmd.Stdin = nil
		cmd.WaitDelay = 5 * time.Second
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = io.Discard
		err := cmd.Run()
		return out.Bytes(), err
	}
	out, err := run("--version")
	ver := strings.TrimSpace(string(out))
	if err != nil || !strings.Contains(ver, "Claude Code") {
		return c, fmt.Errorf("%s --version didn't report Claude Code (%q, %v)", p, clip(ver, 100), err)
	}
	c.Version = ver
	out, err = run("auth", "status", "--json")
	var st struct {
		LoggedIn         *bool  `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if jerr := json.Unmarshal(bytes.TrimSpace(out), &st); jerr != nil || st.LoggedIn == nil {
		reason := "its output wasn't recognised"
		if err != nil {
			reason = err.Error()
		}
		c.Warnings = append(c.Warnings, fmt.Sprintf("Couldn't confirm that Claude Code is signed in (`claude auth status --json`: %s). Make sure `claude` works for this user before running agents.", reason))
		return c, nil
	}
	if !*st.LoggedIn {
		return c, errors.New("Claude Code isn't signed in for this user: run `claude auth login` (or start `claude` and use /login), then try again")
	}
	c.LoggedIn, c.AuthMethod, c.Subscription = true, st.AuthMethod, st.SubscriptionType
	if st.AuthMethod != "" && st.AuthMethod != "claude.ai" {
		c.Warnings = append(c.Warnings, fmt.Sprintf("Claude Code is signed in with %q, not a Claude subscription (claude.ai): agent usage may be billed to that account.", st.AuthMethod))
	}
	return c, nil
}

// ---------- a run ----------

// claudeCodeSession is one agent run on Claude Code.
type claudeCodeSession struct {
	Step       actions.AgentStep
	JobToken   string
	RunContext string
	Binary     string                    // "" = claude on PATH
	Log        func(stream, text string) // masked by the caller
}

func (s *claudeCodeSession) logf(stream, format string, a ...any) {
	if s.Log != nil {
		s.Log(stream, fmt.Sprintf(format, a...))
	}
}

// claudeCodeEvent is one line of --output-format stream-json.
type claudeCodeEvent struct {
	Type       string   `json:"type"`
	Subtype    string   `json:"subtype"`
	Model      string   `json:"model"`
	Version    string   `json:"claude_code_version"`
	Tools      []string `json:"tools"`
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	Message *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *claudeCodeUse  `json:"usage"`
	} `json:"message"`
	IsError           bool           `json:"is_error"`
	NumTurns          int            `json:"num_turns"`
	Result            string         `json:"result"`
	TotalCostUSD      *float64       `json:"total_cost_usd"`
	Usage             *claudeCodeUse `json:"usage"`
	PermissionDenials []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
}

type claudeCodeUse struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

func (u *claudeCodeUse) usage() workflow.AgentUsage {
	if u == nil {
		return workflow.AgentUsage{}
	}
	return workflow.AgentUsage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
}

type claudeCodeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// claudeCodeText flattens a tool result's content (a string or text blocks).
func claudeCodeText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []claudeCodeBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// writeMCPConfig writes the MCP config (job token included) into dir, mode 0600.
func writeMCPConfig(dir, mcpURL, token string) (string, error) {
	cfg := map[string]any{"mcpServers": map[string]any{claudeCodeMCPServer: map[string]any{
		"type": "http", "url": mcpURL, "headers": map[string]string{"Authorization": "Bearer " + token},
	}}}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "mcp.json")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return "", err
	}
	return p, f.Close()
}

// args builds Claude Code's argv. It never contains the job token.
func (s *claudeCodeSession) args(mcpConfig string, allowed []string) []string {
	st := s.Step
	maxTurns := min(max(st.MaxTurns, 1), workflow.AgentMaxTurns)
	a := []string{"-p",
		"--output-format", "stream-json", "--verbose",
		"--max-turns", strconv.Itoa(maxTurns),
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
		"--tools", "",
		"--strict-mcp-config",
	}
	if mcpConfig != "" {
		a = append(a, "--mcp-config", mcpConfig)
	}
	if len(allowed) > 0 {
		a = append(a, "--allowed-tools", strings.Join(allowed, ","))
	}
	a = append(a, "--setting-sources", "project",
		"--no-session-persistence",
		"--disable-slash-commands",
		"--append-system-prompt", agentSystemPrompt)
	if st.Model != "" {
		a = append(a, "--model", st.Model)
	}
	if st.Effort != "" {
		a = append(a, "--effort", st.Effort)
	}
	return a
}

// run runs Claude Code and turns its stream into a transcript and a result.
func (s *claudeCodeSession) run(ctx context.Context) (res agentResult, err error) {
	st := s.Step
	res.Model, res.Subscription = st.Model, true
	defer func() {
		if res.Model == "" {
			res.Model = "claude-code-default"
		}
		var stop *agentStop
		switch {
		case err == nil:
		case errors.As(err, &stop):
			res.Stop = stop.reason
		case ctx.Err() != nil:
			res.Stop = "cancelled"
		default:
			res.Stop = "error"
		}
	}()
	if st.Engine != workflow.AgentEngineClaudeCode {
		return res, fmt.Errorf("unknown agent engine %q", st.Engine)
	}
	bin, err := findClaudeCode(s.Binary)
	if err != nil {
		return res, &agentStop{"error", err.Error() + ". Run `atelier-runner claude-code enable` on this runner"}
	}

	// A private temp dir: an empty working directory for Claude Code, and the
	// MCP config next to it (not inside it). Removed when the run ends.
	root, err := os.MkdirTemp("", "atelier-claude-code-*")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(root)
	work, conf := filepath.Join(root, "work"), filepath.Join(root, "mcp")
	for _, d := range []string{work, conf} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return res, err
		}
	}
	allowed, byName := claudeCodeAllowedTools(st.Tools)
	mcpConfig := ""
	if len(allowed) > 0 {
		if mcpConfig, err = writeMCPConfig(conf, st.MCPURL, s.JobToken); err != nil {
			return res, fmt.Errorf("writing the MCP config: %w", err)
		}
		defer os.Remove(mcpConfig)
		s.logf("system", "Tools offered to Claude Code: %s", strings.Join(allowed, ", "))
	} else {
		s.logf("system", "The agent has no tools: it can only reply with a summary.")
	}
	model := st.Model
	if model == "" {
		model = "Claude Code's default model"
	}
	maxTurns := min(max(st.MaxTurns, 1), workflow.AgentMaxTurns)
	s.logf("system", "Agent %q on Claude Code (subscription): %s, up to %d turns.", st.WorkflowName, model, maxTurns)

	timeout := time.Duration(max(st.TimeoutMinutes, 1)) * time.Minute
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stopCtx, stopRun := context.WithCancelCause(runCtx)
	defer stopRun(nil)

	first := st.Task
	if s.RunContext != "" {
		first = "<task>\n" + st.Task + "\n</task>\n\n<run>\n" + s.RunContext + "\n</run>"
	}
	cmd := exec.CommandContext(stopCtx, bin, s.args(mcpConfig, allowed)...)
	cmd.Dir = work
	cmd.Env = claudeCodeEnv()
	cmd.Stdin = strings.NewReader(first)
	cmd.WaitDelay = 10 * time.Second
	setProcGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("starting Claude Code: %w", err)
	}

	var wg sync.WaitGroup
	var lastStderr string
	var mu sync.Mutex
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			s.logf("stderr", "%s", line)
			if strings.TrimSpace(line) != "" {
				mu.Lock()
				lastStderr = line
				mu.Unlock()
			}
		}
	}()

	var result *claudeCodeEvent
	perMsg := map[string]workflow.AgentUsage{} // usage per assistant message (a fallback without a result)
	var msgOrder []string
	assistantTurns := 0
	toolNames := map[string]string{} // tool_use id → Atelier tool name
	br := bufio.NewReaderSize(stdout, 256<<10)
	for {
		line, rerr := br.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			var ev claudeCodeEvent
			if json.Unmarshal(t, &ev) != nil {
				s.logf("stdout", "%s", clip(string(t), 2000))
			} else {
				switch ev.Type {
				case "system":
					if ev.Subtype != "init" {
						break
					}
					if ev.Model != "" {
						res.Model = ev.Model
					}
					for _, m := range ev.MCPServers {
						if m.Name == claudeCodeMCPServer && m.Status != "connected" {
							s.logf("system", "Atelier's MCP server is %s for Claude Code: the agent may have no tools.", m.Status)
						}
					}
					var bad []string
					for _, t := range ev.Tools {
						if slices.Contains(claudeCodeForbiddenTools, t) {
							bad = append(bad, t)
						}
					}
					if len(bad) > 0 {
						stopRun(&agentStop{"error", "Claude Code offered its own tools (" + strings.Join(bad, ", ") + "), which agents must not have; the run was stopped"})
					}
					v := ""
					if ev.Version != "" {
						v = " " + ev.Version
					}
					s.logf("system", "Claude Code%s started: model %s.", v, res.Model)
				case "assistant":
					if ev.Message == nil {
						break
					}
					if ev.Message.Model != "" && res.Model == st.Model {
						res.Model = ev.Message.Model
					}
					if id := ev.Message.ID; id != "" {
						if _, seen := perMsg[id]; !seen {
							msgOrder = append(msgOrder, id)
							assistantTurns++
						}
						perMsg[id] = ev.Message.Usage.usage()
					}
					var blocks []claudeCodeBlock
					_ = json.Unmarshal(ev.Message.Content, &blocks)
					for _, b := range blocks {
						switch b.Type {
						case "text":
							if t := strings.TrimSpace(b.Text); t != "" {
								res.Final = t
								for _, l := range strings.Split(t, "\n") {
									s.logf("stdout", "%s", l)
								}
							}
						case "tool_use":
							name := b.Name
							if n, ok := byName[b.Name]; ok {
								name = n
							}
							toolNames[b.ID] = name
							args := strings.TrimSpace(string(b.Input))
							if args == "" {
								args = "{}"
							}
							s.logf("system", "→ %s %s", name, clip(args, agentLogArgs))
						}
					}
				case "user":
					if ev.Message == nil {
						break
					}
					var blocks []claudeCodeBlock
					_ = json.Unmarshal(ev.Message.Content, &blocks)
					for _, b := range blocks {
						if b.Type != "tool_result" {
							continue
						}
						text := claudeCodeText(b.Content)
						name := toolNames[b.ToolUseID]
						if name == "" {
							name = "tool"
						}
						mark := "←"
						if b.IsError {
							mark = "← error:"
						}
						s.logf("system", "%s %s (%s bytes) %s", mark, name, thousands(int64(len(text))), clip(text, agentLogResult))
					}
				case "result":
					e := ev
					result = &e
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	wg.Wait()
	waitErr := cmd.Wait()

	// Usage and turns: Claude Code's totals, or what the transcript showed.
	if result != nil && result.Usage != nil {
		res.Usage = result.Usage.usage()
	} else {
		for _, id := range msgOrder {
			res.Usage.Add(perMsg[id])
		}
	}
	res.Turns = assistantTurns
	if result != nil && result.NumTurns > 0 {
		res.Turns = result.NumTurns
	}
	if result != nil && result.TotalCostUSD != nil && *result.TotalCostUSD >= 0 {
		res.Cost, res.HasCost = *result.TotalCostUSD, true
	}
	if result != nil {
		if f := strings.TrimSpace(result.Result); f != "" {
			res.Final = f
		}
		for _, d := range result.PermissionDenials {
			s.logf("system", "✗ Refused tool call %s: it isn't one of this workflow's tools.", clip(d.ToolName, 80))
		}
	}
	extra := ""
	if res.HasCost {
		extra = fmt.Sprintf(" · ≈ $%.4f at API prices (subscription usage, not billed)", res.Cost)
	}
	s.logf("system", "Claude Code: %d turns · %s in, %s out, %s cache read, %s cache write%s", res.Turns,
		thousands(res.Usage.Input), thousands(res.Usage.Output), thousands(res.Usage.CacheRead), thousands(res.Usage.CacheWrite), extra)

	// Why it ended.
	if cause := context.Cause(stopCtx); cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return res, cause
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if runCtx.Err() != nil {
		return res, &agentStop{"timeout", fmt.Sprintf("Claude Code didn't finish within %d minutes (timeout-minutes) and was stopped", max(st.TimeoutMinutes, 1))}
	}
	if result != nil {
		switch {
		case result.Subtype == "error_max_turns":
			return res, &agentStop{"max_turns", fmt.Sprintf("the agent stopped after %d turns (max-turns) before finishing", maxTurns)}
		case result.Subtype == "success" && !result.IsError:
			res.Stop = "end_turn"
			if waitErr != nil {
				s.logf("system", "Claude Code exited with %v after finishing.", waitErr)
			}
			return res, nil
		default:
			msg := strings.TrimSpace(result.Result)
			if msg == "" {
				msg = result.Subtype
			}
			return res, &agentStop{"error", "Claude Code stopped with an error: " + clip(msg, 500) + claudeCodeHint(msg)}
		}
	}
	mu.Lock()
	last := lastStderr
	mu.Unlock()
	if waitErr == nil {
		return res, &agentStop{"error", "Claude Code exited without a result"}
	}
	return res, &agentStop{"error", fmt.Sprintf("Claude Code failed (%v): %s%s", waitErr, clip(last, 500), claudeCodeHint(last))}
}

// claudeCodeHint adds advice for a sign-in problem.
func claudeCodeHint(msg string) string {
	m := strings.ToLower(msg)
	if strings.Contains(m, "/login") || strings.Contains(m, "not logged in") || strings.Contains(m, "invalid api key") ||
		strings.Contains(m, "log in") || strings.Contains(m, "oauth") {
		return ". Claude Code on this runner doesn't seem to be signed in for the runner's user: run `claude auth login` as that user, then `atelier-runner claude-code enable` again"
	}
	return ""
}
