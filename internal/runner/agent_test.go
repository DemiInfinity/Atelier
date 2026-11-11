package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"atelier/internal/actions"
)

// ---------- a fake Claude Messages API (streaming) ----------

type fakeBlock struct {
	Text  string
	Tool  string // tool_use name
	ID    string
	Input string // raw JSON
}

type fakeTurn struct {
	Status     int    // non-200: an error response
	RetryAfter string // with Status 429
	Blocks     []fakeBlock
	Stop       string
	Category   string // refusal category
	In, Out    int64
	CacheRead  int64
	CacheWrite int64
	Hang       bool // never answer (until the client gives up)
}

type fakeClaude struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	turns  []fakeTurn
	reqs   []map[string]any
	heads  []http.Header
	served int
}

func newFakeClaude(t *testing.T, turns ...fakeTurn) *fakeClaude {
	f := &fakeClaude{t: t, turns: turns}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeClaude) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.reqs...)
}

func (f *fakeClaude) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		f.t.Errorf("bad request JSON: %v", err)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, body)
	f.heads = append(f.heads, r.Header.Clone())
	if f.served >= len(f.turns) {
		f.mu.Unlock()
		f.t.Errorf("unexpected request %d", len(f.reqs))
		http.Error(w, `{"type":"error","error":{"type":"api_error","message":"no more scripted turns"}}`, 500)
		return
	}
	turn := f.turns[f.served]
	f.served++
	f.mu.Unlock()

	if turn.Hang {
		<-r.Context().Done()
		return
	}
	if turn.Status != 0 && turn.Status != 200 {
		if turn.RetryAfter != "" {
			w.Header().Set("Retry-After", turn.RetryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(turn.Status)
		typ := map[int]string{401: "authentication_error", 429: "rate_limit_error", 529: "overloaded_error", 400: "invalid_request_error"}[turn.Status]
		if typ == "" {
			typ = "api_error"
		}
		fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":"scripted %d"}}`, typ, turn.Status)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(event string, v any) {
		d, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, d)
	}
	model, _ := body["model"].(string)
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + fmt.Sprint(f.served), "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": turn.In, "output_tokens": 1, "cache_read_input_tokens": turn.CacheRead,
			"cache_creation_input_tokens": turn.CacheWrite}}})
	for i, bl := range turn.Blocks {
		if bl.Tool != "" {
			send("content_block_start", map[string]any{"type": "content_block_start", "index": i,
				"content_block": map[string]any{"type": "tool_use", "id": bl.ID, "name": bl.Tool, "input": map[string]any{}}})
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": bl.Input}})
		} else {
			send("content_block_start", map[string]any{"type": "content_block_start", "index": i,
				"content_block": map[string]any{"type": "text", "text": ""}})
			send("content_block_delta", map[string]any{"type": "content_block_delta", "index": i,
				"delta": map[string]any{"type": "text_delta", "text": bl.Text}})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	delta := map[string]any{"stop_reason": turn.Stop, "stop_sequence": nil, "stop_details": nil}
	if turn.Stop == "refusal" {
		delta["stop_details"] = map[string]any{"type": "refusal", "category": turn.Category, "explanation": "scripted"}
	}
	send("message_delta", map[string]any{"type": "message_delta", "delta": delta, "usage": map[string]any{"output_tokens": turn.Out}})
	send("message_stop", map[string]any{"type": "message_stop"})
}

// ---------- a fake Atelier MCP server ----------

type fakeMCP struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string
	auth  []string
}

func newFakeMCP(t *testing.T, token, secretInResult string) *fakeMCP {
	f := &fakeMCP{}
	s := mcp.NewServer(&mcp.Implementation{Name: "fake-atelier", Version: "1"}, nil)
	schema := json.RawMessage(`{"type":"object","properties":{"number":{"type":"integer"},"body":{"type":"string"}},"required":["number"]}`)
	for _, name := range []string{"pulls.get", "pulls.comment", "issues.close", "actions.dispatch", "notifications.list"} {
		s.AddTool(&mcp.Tool{Name: name, Description: "fake " + name, InputSchema: schema}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			f.mu.Lock()
			f.calls = append(f.calls, req.Params.Name+" "+string(req.Params.Arguments))
			f.mu.Unlock()
			text := "result of " + req.Params.Name
			if secretInResult != "" {
				text += " (leaked " + secretInResult + ")"
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
		})
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeMCP) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// ---------- helpers ----------

const testAPIKey = "sk-ant-test-KEY-0123456789abcdef"
const testJobToken = "atj_agent_job_token_9876543210"

func testStep(mcpURL string, tools ...string) actions.AgentStep {
	return actions.AgentStep{Engine: "claude", Model: "claude-opus-5-5", Effort: "medium", Task: "Review the pull request.",
		Tools: tools, MaxTurns: 10, MaxTokens: 400_000, TimeoutMinutes: 5, MCPURL: mcpURL + "/mcp", WorkflowName: "Review helper"}
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) log(stream, text string) {
	l.mu.Lock()
	l.lines = append(l.lines, stream+": "+text)
	l.mu.Unlock()
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func runSession(t *testing.T, claude *fakeClaude, step actions.AgentStep) (agentResult, error, *logSink) {
	t.Helper()
	sink := &logSink{}
	s := &agentSession{Step: step, APIKey: testAPIKey, JobToken: testJobToken, RunContext: "event: pull_request\npull request: #1",
		Log: sink.log, APIBaseURL: claude.srv.URL, retryBase: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.run(ctx)
	return res, err, sink
}

func toolNames(req map[string]any) []string {
	var out []string
	tools, _ := req["tools"].([]any)
	for _, t := range tools {
		m, _ := t.(map[string]any)
		out = append(out, fmt.Sprint(m["name"]))
	}
	return out
}

// ---------- tests ----------

func TestAgentToolRoundTripAndDisallowedTools(t *testing.T) {
	m := newFakeMCP(t, testJobToken, "")
	claude := newFakeClaude(t,
		fakeTurn{Blocks: []fakeBlock{{Text: "Reading the pull request."}, {Tool: "pulls_get", ID: "toolu_1", Input: `{"number": 1}`}},
			Stop: "tool_use", In: 1000, Out: 50, CacheWrite: 3000},
		// The model asks for tools the workflow doesn't allow (and one that doesn't exist).
		fakeTurn{Blocks: []fakeBlock{
			{Tool: "actions_dispatch", ID: "toolu_2", Input: `{"number": 1}`},
			{Tool: "issues_close", ID: "toolu_3", Input: `{"number": 1}`},
			{Tool: "pulls_comment", ID: "toolu_4", Input: `{"number": 1, "body": "Looks good."}`},
		}, Stop: "tool_use", In: 200, Out: 80, CacheRead: 4000},
		fakeTurn{Blocks: []fakeBlock{{Text: "Posted one summary comment."}}, Stop: "end_turn", In: 300, Out: 20, CacheRead: 4200},
	)
	// actions.dispatch is listed on purpose: the runner must drop it even if a job asks.
	res, err, logs := runSession(t, claude, testStep(m.srv.URL, "pulls.comment", "pulls.get", "actions.dispatch"))
	if err != nil {
		t.Fatalf("run: %v\n%s", err, logs)
	}
	if res.Stop != "end_turn" || res.Turns != 3 || res.Final != "Posted one summary comment." {
		t.Errorf("result = %+v", res)
	}
	if res.Usage.Input != 1500 || res.Usage.Output != 150 || res.Usage.CacheRead != 8200 || res.Usage.CacheWrite != 3000 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if !res.HasCost || res.Cost <= 0 {
		t.Errorf("no cost estimate: %+v", res)
	}

	calls := m.called()
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "pulls.get ") || !strings.HasPrefix(calls[1], "pulls.comment ") {
		t.Errorf("MCP calls = %q, want only pulls.get then pulls.comment", calls)
	}
	reqs := claude.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d", len(reqs))
	}
	for i, r := range reqs {
		if got := strings.Join(toolNames(r), ","); got != "pulls_comment,pulls_get" {
			t.Errorf("request %d offered tools %q", i, got)
		}
	}
	r0 := reqs[0]
	if r0["model"] != "claude-opus-5-5" || r0["stream"] != true {
		t.Errorf("model/stream = %v %v", r0["model"], r0["stream"])
	}
	if _, ok := r0["tool_choice"]; ok {
		t.Error("tool_choice must not be forced")
	}
	if th, _ := r0["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Errorf("thinking = %v", r0["thinking"])
	}
	oc, _ := r0["output_config"].(map[string]any)
	tb, _ := oc["task_budget"].(map[string]any)
	if oc["effort"] != "medium" || tb["total"] != float64(400000) || tb["type"] != "tokens" {
		t.Errorf("output_config = %v", oc)
	}
	if r0["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v", r0["fallbacks"])
	}
	if mt, _ := r0["max_tokens"].(float64); mt != agentMaxOutputPerTurn {
		t.Errorf("max_tokens = %v", r0["max_tokens"])
	}
	sys, _ := r0["system"].([]any)
	if len(sys) != 1 || sys[0].(map[string]any)["cache_control"] == nil {
		t.Errorf("system isn't cached: %v", r0["system"])
	}
	if r0["cache_control"] == nil {
		t.Error("no automatic cache_control for the conversation")
	}
	tool0 := r0["tools"].([]any)[0].(map[string]any)
	if tool0["eager_input_streaming"] != true {
		t.Errorf("tool = %v", tool0)
	}
	beta := claude.heads[0].Get("anthropic-beta")
	if !strings.Contains(beta, "task-budgets-2026-03-13") || !strings.Contains(beta, "server-side-fallback-2026-07-01") {
		t.Errorf("anthropic-beta = %q", beta)
	}
	if h := claude.heads[0]; h.Get("X-Api-Key") != testAPIKey && h.Get("Authorization") != "Bearer "+testAPIKey {
		t.Error("API key not sent")
	}
	// The third request carries the tool results of turn 2, all in one message.
	msgs := reqs[2]["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	content := last["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("tool results in one message = %d", len(content))
	}
	b, _ := json.Marshal(content)
	if !strings.Contains(string(b), "not available to you") || !strings.Contains(string(b), "result of pulls.comment") {
		t.Errorf("tool results = %s", b)
	}
	// The run details are in the first message, after the task.
	first, _ := json.Marshal(reqs[0]["messages"])
	if !strings.Contains(string(first), "Review the pull request.") || !strings.Contains(string(first), "pull request: #1") {
		t.Errorf("first message = %s", first)
	}
	out := logs.String()
	for _, want := range []string{"Reading the pull request.", "→ pulls.get", "Refused tool call actions_dispatch", "Refused tool call issues_close", "Turn 1: 1,000 in"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, a := range m.auth {
		if a != "Bearer "+testJobToken {
			t.Errorf("MCP auth header = %q", a)
		}
	}
}

func TestAgentRefusalAndMaxTokens(t *testing.T) {
	claude := newFakeClaude(t, fakeTurn{Stop: "refusal", Category: "cyber", In: 10, Out: 0})
	res, err, _ := runSession(t, claude, testStep("http://127.0.0.1:1"))
	if err == nil || res.Stop != "refusal" || !strings.Contains(err.Error(), "cyber") {
		t.Errorf("refusal: stop=%q err=%v", res.Stop, err)
	}

	claude = newFakeClaude(t, fakeTurn{Blocks: []fakeBlock{{Text: "a very long"}}, Stop: "max_tokens", In: 10, Out: 64000})
	res, err, _ = runSession(t, claude, testStep("http://127.0.0.1:1"))
	if err == nil || res.Stop != "max_tokens" {
		t.Errorf("max_tokens: stop=%q err=%v", res.Stop, err)
	}
}

func TestAgentRetries(t *testing.T) {
	claude := newFakeClaude(t,
		fakeTurn{Status: 429, RetryAfter: "0"},
		fakeTurn{Status: 529},
		fakeTurn{Status: 500},
		fakeTurn{Blocks: []fakeBlock{{Text: "done"}}, Stop: "end_turn", In: 5, Out: 5},
	)
	res, err, logs := runSession(t, claude, testStep("http://127.0.0.1:1"))
	if err != nil || res.Stop != "end_turn" || len(claude.requests()) != 4 {
		t.Fatalf("err=%v stop=%q requests=%d\n%s", err, res.Stop, len(claude.requests()), logs)
	}
	if !strings.Contains(logs.String(), "Rate limited") || !strings.Contains(logs.String(), "Retrying") {
		t.Errorf("log:\n%s", logs)
	}

	claude = newFakeClaude(t, fakeTurn{Status: 401})
	res, err, _ = runSession(t, claude, testStep("http://127.0.0.1:1"))
	if err == nil || !strings.Contains(err.Error(), "401") || len(claude.requests()) != 1 || res.Stop != "error" {
		t.Errorf("401: err=%v requests=%d", err, len(claude.requests()))
	}

	turns := make([]fakeTurn, agentMaxRetries+1)
	for i := range turns {
		turns[i] = fakeTurn{Status: 503}
	}
	claude = newFakeClaude(t, turns...)
	_, err, _ = runSession(t, claude, testStep("http://127.0.0.1:1"))
	if err == nil || !strings.Contains(err.Error(), "kept failing") {
		t.Errorf("5xx: err=%v", err)
	}
}

func TestAgentLimits(t *testing.T) {
	m := newFakeMCP(t, testJobToken, "")
	call := fakeTurn{Blocks: []fakeBlock{{Tool: "pulls_get", ID: "t", Input: `{"number":1}`}}, Stop: "tool_use", In: 100, Out: 10}

	// Turns.
	claude := newFakeClaude(t, call, call, call)
	step := testStep(m.srv.URL, "pulls.get")
	step.MaxTurns = 2
	res, err, _ := runSession(t, claude, step)
	if err == nil || res.Stop != "max_turns" || len(claude.requests()) != 2 {
		t.Errorf("turns: stop=%q err=%v requests=%d", res.Stop, err, len(claude.requests()))
	}

	// Tokens: the first turn uses the whole budget, so no second request.
	big := call
	big.In, big.Out = 15000, 5000
	claude = newFakeClaude(t, big, call)
	step = testStep(m.srv.URL, "pulls.get")
	step.MaxTokens = 20000
	res, err, _ = runSession(t, claude, step)
	if err == nil || res.Stop != "budget" || len(claude.requests()) != 1 {
		t.Errorf("budget: stop=%q err=%v requests=%d", res.Stop, err, len(claude.requests()))
	}
	if mt := claude.requests()[0]["max_tokens"].(float64); mt != 20000 {
		t.Errorf("max_tokens capped by budget = %v", mt)
	}

	// Timeout: the API never answers; the run stops when the context ends.
	claude = newFakeClaude(t, fakeTurn{Hang: true})
	s := &agentSession{Step: testStep(m.srv.URL), APIKey: testAPIKey, JobToken: testJobToken, APIBaseURL: claude.srv.URL, retryBase: time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err = s.run(ctx)
	if err == nil || res.Stop != "cancelled" || time.Since(start) > 10*time.Second {
		t.Errorf("timeout: stop=%q err=%v after %s", res.Stop, err, time.Since(start))
	}
}

func TestAgentInvalidToolInput(t *testing.T) {
	m := newFakeMCP(t, testJobToken, "")
	claude := newFakeClaude(t,
		fakeTurn{Blocks: []fakeBlock{{Tool: "pulls_get", ID: "t1", Input: `{"number": `}}, Stop: "tool_use", In: 10, Out: 10},
		fakeTurn{Blocks: []fakeBlock{{Tool: "pulls_get", ID: "t2", Input: `{}`}}, Stop: "tool_use", In: 10, Out: 10},
		fakeTurn{Blocks: []fakeBlock{{Text: "ok"}}, Stop: "end_turn", In: 10, Out: 10},
	)
	_, err, logs := runSession(t, claude, testStep(m.srv.URL, "pulls.get"))
	if err != nil {
		t.Fatalf("%v\n%s", err, logs)
	}
	if calls := m.called(); len(calls) != 0 {
		t.Errorf("invalid calls reached MCP: %q", calls)
	}
	// Truncated JSON either fails to parse or is read as {} by the SDK: both
	// come back to the model as an error, and nothing runs.
	b, _ := json.Marshal(claude.requests()[1]["messages"])
	if !strings.Contains(string(b), "INVALID_JSON") && !strings.Contains(string(b), `missing required argument`) {
		t.Errorf("no error result for invalid input: %s", b)
	}
	b, _ = json.Marshal(claude.requests()[2]["messages"])
	if !strings.Contains(string(b), `missing required argument \"number\"`) {
		t.Errorf("no missing-argument result: %s", b)
	}
}

func TestCheckMCPURL(t *testing.T) {
	for _, c := range []struct {
		mcp, server string
		ok          bool
	}{
		{"https://atelier.example/mcp", "https://atelier.example", true},
		{"https://atelier.example:8443/mcp", "https://atelier.example", true},
		{"https://evil.example/mcp", "https://atelier.example", false},
		{"http://atelier.example/mcp", "https://atelier.example", false},
		{"ftp://atelier.example/mcp", "https://atelier.example", false},
	} {
		if err := checkMCPURL(c.mcp, c.server); (err == nil) != c.ok {
			t.Errorf("checkMCPURL(%s, %s) = %v", c.mcp, c.server, err)
		}
	}
}

// TestAgentJobMasksSecrets runs an agent step through the runner: the API
// key and job token never show up in logs, outputs or the summary, even when
// the model or a tool echoes them.
func TestAgentJobMasksSecrets(t *testing.T) {
	f := newFakeServer(t)
	m := newFakeMCP(t, testJobToken, testJobToken)
	claude := newFakeClaude(t,
		fakeTurn{Blocks: []fakeBlock{{Text: "My key is " + testAPIKey}, {Tool: "pulls_get", ID: "t1", Input: `{"number":1}`}}, Stop: "tool_use", In: 100, Out: 10},
		fakeTurn{Blocks: []fakeBlock{{Text: "Done; token was " + testJobToken}}, Stop: "end_turn", In: 100, Out: 10},
	)
	oldBase := agentAPIBaseURL
	agentAPIBaseURL = func() string { return claude.srv.URL }
	defer func() { agentAPIBaseURL = oldBase }()

	job := baseJob(7, nil)
	job.JobToken = testJobToken
	job.Workflow = ".atelier/workflows/review.md"
	job.Event = "pull_request"
	job.Secrets = map[string]string{"ANTHROPIC_API_KEY": testAPIKey}
	step := testStep(m.srv.URL, "pulls.get")
	job.Steps = []actions.Step{{Index: 0, ID: "agent", Name: "Run Claude agent", With: map[string]string{"api-key": "${{ secrets.ANTHROPIC_API_KEY }}"}, Agent: &step}}
	job.Outputs = map[string]string{"model": "${{ steps.agent.outputs.model }}", "turns": "${{ steps.agent.outputs.turns }}",
		"cost_usd": "${{ steps.agent.outputs.cost_usd }}"}

	r := f.testRunner(t)
	f.runJobs(t, r, job)
	logs := f.logText(7)
	fin := f.finish(7)
	if fin.Status != actions.StatusSuccess {
		t.Fatalf("status = %s\n%s", fin.Status, logs)
	}
	sum := f.step(7, 0).Summary
	all := logs + sum + fmt.Sprint(fin.Outputs)
	for _, secret := range []string{testAPIKey, testJobToken} {
		if strings.Contains(all, secret) {
			t.Errorf("secret %q leaked:\n%s", secret, all)
		}
	}
	if !strings.Contains(logs, "My key is ***") || !strings.Contains(logs, "(leaked ***)") {
		t.Errorf("expected masked lines:\n%s", logs)
	}
	if fin.Outputs["model"] != "claude-opus-5-5" || fin.Outputs["turns"] != "2" || fin.Outputs["cost_usd"] == "" {
		t.Errorf("outputs = %v", fin.Outputs)
	}
	if !strings.Contains(sum, "| `claude-opus-5-5` | 2 |") || !strings.Contains(sum, "estimate") {
		t.Errorf("summary = %s", sum)
	}
}
