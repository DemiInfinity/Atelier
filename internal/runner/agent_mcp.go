package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"atelier/internal/workflow"
)

// The agent's side of Atelier's MCP server: it connects with the job token,
// lists the server's tools and keeps only the ones the workflow allows. The
// model is offered those (as client tools, since the Claude API can't reach a
// server on a home network) and nothing else is ever called.

const (
	agentToolCallTimeout = 2 * time.Minute
	agentMaxToolResult   = 100 << 10 // bytes of tool output sent back to the model
	agentMaxToolDesc     = 2000
)

// agentTool is one MCP tool the agent may call.
type agentTool struct {
	MCPName     string         // e.g. "pulls.diff"
	APIName     string         // the name Claude sees, e.g. "pulls_diff"
	Description string         // from the MCP server
	Schema      map[string]any // JSON Schema of the arguments
}

var agentAPINameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// agentAPIName maps an MCP tool name to one the Claude API accepts.
func agentAPIName(mcpName string) string { return strings.ReplaceAll(mcpName, ".", "_") }

// bearerTransport adds the job token to requests to the MCP server only.
type bearerTransport struct {
	token string
	host  string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.EqualFold(r.URL.Host, t.host) {
		return nil, fmt.Errorf("refusing to send the job token to %s", r.URL.Host)
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}

// checkMCPURL makes sure the MCP endpoint is on the runner's own server, so
// the job token never leaves it.
func checkMCPURL(mcpURL, serverURL string) error {
	m, err := url.Parse(mcpURL)
	if err != nil || m.Host == "" || (m.Scheme != "https" && m.Scheme != "http") {
		return fmt.Errorf("the job's MCP URL isn't a valid http(s) URL")
	}
	s, err := url.Parse(serverURL)
	if err != nil || s.Host == "" {
		return fmt.Errorf("the runner's server URL is invalid")
	}
	if !strings.EqualFold(m.Hostname(), s.Hostname()) || (s.Scheme == "https" && m.Scheme != "https") {
		return fmt.Errorf("the job's MCP URL (%s) isn't on this runner's server (%s)", m.Host, s.Host)
	}
	return nil
}

// agentMCP is a connected MCP session plus the allowed tools.
type agentMCP struct {
	session *mcp.ClientSession
	tools   map[string]agentTool // by APIName
}

// connectAgentMCP connects to the MCP server and keeps the allowed tools.
// allowed must already be the workflow's list; tools agents may never use are
// dropped again here.
func connectAgentMCP(ctx context.Context, mcpURL, jobToken string, allowed []string, hc *http.Client, logf func(string)) (*agentMCP, error) {
	u, err := url.Parse(mcpURL)
	if err != nil {
		return nil, err
	}
	base := http.DefaultTransport
	if hc != nil && hc.Transport != nil {
		base = hc.Transport
	}
	client := &http.Client{Transport: &bearerTransport{token: jobToken, host: u.Host, base: base}}
	c := mcp.NewClient(&mcp.Implementation{Name: "atelier-runner-agent", Version: "1"}, nil)
	session, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: mcpURL, HTTPClient: client, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to Atelier's MCP server: %w", err)
	}
	want := map[string]bool{}
	for _, t := range allowed {
		if workflow.AgentToolAllowed(t) {
			want[t] = true
		}
	}
	m := &agentMCP{session: session, tools: map[string]agentTool{}}
	seen := map[string]bool{}
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			session.Close()
			return nil, fmt.Errorf("listing MCP tools: %w", err)
		}
		if t == nil || !want[t.Name] || seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		name := agentAPIName(t.Name)
		if !agentAPINameRe.MatchString(name) {
			logf(fmt.Sprintf("Skipping MCP tool %q: its name can't be offered to Claude.", t.Name))
			continue
		}
		desc := t.Description
		if len(desc) > agentMaxToolDesc {
			desc = desc[:agentMaxToolDesc] + "…"
		}
		m.tools[name] = agentTool{MCPName: t.Name, APIName: name, Description: desc, Schema: agentSchema(t.InputSchema)}
	}
	var missing []string
	for t := range want {
		if !seen[t] {
			missing = append(missing, t)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		logf("Not offered by the MCP server for this job, so unavailable: " + strings.Join(missing, ", "))
	}
	return m, nil
}

// agentSchema turns an MCP input schema into a JSON object schema.
func agentSchema(s any) map[string]any {
	out := map[string]any{}
	if s != nil {
		if b, err := json.Marshal(s); err == nil {
			_ = json.Unmarshal(b, &out)
		}
	}
	if out == nil {
		out = map[string]any{}
	}
	out["type"] = "object"
	if _, ok := out["properties"]; !ok {
		out["properties"] = map[string]any{}
	}
	delete(out, "$schema")
	return out
}

// names lists the allowed tools by API name, sorted (a stable tool list keeps the prompt cache warm).
func (m *agentMCP) names() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.tools))
	for k := range m.tools {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *agentMCP) close() {
	if m != nil && m.session != nil {
		_ = m.session.Close()
	}
}

// errToolNotAllowed: the model asked for a tool this workflow doesn't allow.
var errToolNotAllowed = errors.New("tool not available")

// call runs one allowed tool. The returned text is what the model sees; an
// error result (isError) is still returned as text so the model can react.
func (m *agentMCP) call(ctx context.Context, apiName string, args map[string]any) (text string, isError bool, err error) {
	t, ok := m.lookup(apiName)
	if !ok {
		return "", true, errToolNotAllowed
	}
	ctx, cancel := context.WithTimeout(ctx, agentToolCallTimeout)
	defer cancel()
	res, err := m.session.CallTool(ctx, &mcp.CallToolParams{Name: t.MCPName, Arguments: args})
	if err != nil {
		return "", true, err
	}
	var b strings.Builder
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			b.WriteString(v.Text)
		default:
			if raw, err := json.Marshal(v); err == nil {
				b.Write(raw)
			}
		}
		b.WriteString("\n")
	}
	text = strings.TrimSpace(b.String())
	if text == "" && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			text = string(raw)
		}
	}
	return text, res.IsError, nil
}

func (m *agentMCP) lookup(apiName string) (agentTool, bool) {
	if m == nil {
		return agentTool{}, false
	}
	t, ok := m.tools[apiName]
	return t, ok
}
