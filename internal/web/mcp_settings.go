package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"atelier/internal/store"
)

// Settings → MCP: how to connect Claude and other clients, a shortcut to
// create an MCP token, and the log of recent tool calls.

func init() {
	registerFeature(feature{
		name: "mcp",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/mcp", a.authed(a.mcpSettingsPage))
			m.HandleFunc("POST /settings/mcp/token", a.authed(a.mcpSettingsToken))
		},
	})
}

type mcpToolView struct {
	Name, Title, Desc string
	Write, NoJobs     bool
}

type mcpSnippet struct {
	ID, Label, Hint, Text string
}

type mcpSettingsData struct {
	Endpoint  string
	Created   string
	Snippets  []mcpSnippet
	Tools     []mcpToolView
	Calls     []store.MCPCall
	Tokens    []store.Token
	TokenName string
}

func (a *App) mcpSettingsPage(w http.ResponseWriter, r *http.Request) {
	a.renderMCPSettings(w, r, "", "", http.StatusOK)
}

func (a *App) renderMCPSettings(w http.ResponseWriter, r *http.Request, created, errMsg string, code int) {
	ctx := r.Context()
	who := a.actor(r)
	d := mcpSettingsData{Endpoint: strings.TrimRight(a.baseURL(r), "/") + mcpPath, Created: created, TokenName: "Claude"}
	token := created
	if token == "" {
		token = "<your-mcp-token>"
	}
	d.Snippets = mcpSnippets(d.Endpoint, token)
	for _, t := range mcpTools() {
		d.Tools = append(d.Tools, mcpToolView{Name: t.Name, Title: t.Title, Desc: t.Desc, Write: t.Write, NoJobs: t.NoJobs})
	}
	calls, err := a.db.MCPCalls(ctx, who.PersonID, 100)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d.Calls = calls
	toks, err := a.db.Tokens(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for _, t := range toks {
		if t.Has(mcpScope) {
			d.Tokens = append(d.Tokens, t)
		}
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "mcp_settings", page{Title: "MCP", Error: errMsg, Data: d})
}

// mcpSnippets are the connection snippets for the common clients.
func mcpSnippets(endpoint, token string) []mcpSnippet {
	auth := "Bearer " + token
	project, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{"atelier": map[string]any{
		"type": "http", "url": endpoint, "headers": map[string]string{"Authorization": auth}}}}, "", "  ")
	desktop, _ := json.MarshalIndent(map[string]any{"mcpServers": map[string]any{"atelier": map[string]any{
		"command": "npx", "args": []string{"-y", "mcp-remote", endpoint, "--header", "Authorization:${ATELIER_AUTH}"},
		"env": map[string]string{"ATELIER_AUTH": auth}}}}, "", "  ")
	return []mcpSnippet{
		{ID: "mcp-snip-cli", Label: "Claude Code (terminal)",
			Hint: "Run once. Add --scope user to use Atelier in every project.",
			Text: "claude mcp add --transport http atelier " + endpoint + " --header \"Authorization: " + auth + "\""},
		{ID: "mcp-snip-json", Label: "Claude Code (.mcp.json) and other HTTP clients",
			Hint: "Put this in .mcp.json at a project's root, or your client's MCP config. Keep the token out of shared repositories.",
			Text: string(project)},
		{ID: "mcp-snip-desktop", Label: "Claude Desktop",
			Hint: "Add to claude_desktop_config.json (Settings → Developer → Edit config), then restart Claude. It uses the mcp-remote bridge, which needs Node.js.",
			Text: string(desktop)},
	}
}

func (a *App) mcpSettingsToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	name := strings.TrimSpace(r.FormValue("name"))
	scopes, ok1 := tokenScopes[r.FormValue("access")]
	ttl, ok2 := tokenExpiry[r.FormValue("expires")]
	if name == "" || len(name) > 80 || !ok1 || !ok2 || !(store.Token{Scopes: scopes}).Has(mcpScope) {
		a.renderMCPSettings(w, r, "", "Give the token a name, access level and expiry.", http.StatusUnprocessableEntity)
		return
	}
	ctx := r.Context()
	plain, err := a.db.CreateToken(ctx, name, scopes, ttl)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	// The token belongs to whoever created it.
	if who := a.actor(r); who.PersonID != 0 {
		if err := a.db.AssignTokenPerson(ctx, plain, who.PersonID); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	a.renderMCPSettings(w, r, plain, "", http.StatusOK)
}
