package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"atelier/internal/actions"
	"atelier/internal/store"
)

// ---------- fixture ----------

type mcpFixture struct {
	a      *App
	srv    *httptest.Server
	repo   store.Repo // zoe/demo, private, with commits and a pull request
	other  store.Repo // zoe/other, private, no commits
	head   string     // head commit of the pull request
	pull   store.Pull
	writeT string // mcp read+write token
	readT  string // mcp read-only token
}

func newMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	g := newGitFixture(t) // owner zoe, private zoe/demo
	a := g.a
	ctx := context.Background()
	f := &mcpFixture{a: a}
	f.repo, _ = a.db.RepoByName(ctx, "zoe", "demo")

	work := pullsWork(t, a)
	pullsCommit(t, work, "README.md", "# Demo\n\nHello from the demo repo.\n", "Initial")
	pullsCommit(t, work, "app/main.go", "package app\n\nfunc A() {}\n\n// FindMe is a marker.\nfunc FindMe() {}\n", "Add app")
	runGit(t, work, "push", "-q", "origin", "main")
	runGit(t, work, "checkout", "-q", "-b", "feature/hello")
	f.head = pullsCommit(t, work, "app/main.go", "package app\n\nfunc A() {}\n\n// FindMe is a marker.\nfunc FindMe() {}\n\nfunc Hello() string { return \"hi\" }\n", "Add Hello")
	runGit(t, work, "push", "-q", "origin", "feature/hello")
	var err error
	f.pull, err = a.db.CreatePull(ctx, store.NewPull{RepoID: f.repo.ID, Title: "Add Hello", Body: "Says hi.", Head: "feature/hello",
		Base: "main", HeadSHA: f.head, AuthorKind: ActorOwner, AuthorName: "zoe"})
	if err != nil {
		t.Fatal(err)
	}
	me, _ := a.db.PersonalOwner(ctx)
	if err := a.git.Init(ctx, "zoe", "other"); err != nil {
		t.Fatal(err)
	}
	if f.other, err = a.db.CreateRepo(ctx, me.ID, "other", "", true); err != nil {
		t.Fatal(err)
	}
	if err := a.db.EnsureDefaultLabels(ctx, f.repo.ID); err != nil {
		t.Fatal(err)
	}
	f.writeT, _ = a.db.CreateToken(ctx, "claude", tokenScopes["mcp"], time.Hour)
	f.readT, _ = a.db.CreateToken(ctx, "claude-ro", tokenScopes["mcp-read"], time.Hour)
	f.srv = httptest.NewServer(a)
	t.Cleanup(f.srv.Close)
	return f
}

type bearerRT struct{ tok string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func (f *mcpFixture) connect(t *testing.T, tok string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "atelier-test", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: f.srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRT{tok}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// call runs a tool and decodes its JSON text; it fails the test on a tool error.
func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	out, errText := callRaw(t, s, name, args)
	if errText != "" {
		t.Fatalf("%s: tool error: %s", name, errText)
	}
	return out
}

// callRaw returns the decoded result, or the error text for a tool error.
func callRaw(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, "protocol: " + err.Error()
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		if text == "" {
			text = "(empty error)"
		}
		return nil, text
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: result isn't JSON: %v\n%.500s", name, err, text)
	}
	return out, ""
}

func toolNames(t *testing.T, s *mcp.ClientSession) map[string]bool {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range res.Tools {
		names[tl.Name] = true
	}
	return names
}

func mustErr(t *testing.T, s *mcp.ClientSession, name string, args map[string]any, want string) {
	t.Helper()
	_, errText := callRaw(t, s, name, args)
	if errText == "" {
		t.Fatalf("%s: expected an error containing %q, got success", name, want)
	}
	if !strings.Contains(errText, want) {
		t.Fatalf("%s: error %q doesn't contain %q", name, errText, want)
	}
}

// ---------- tests ----------

func TestMCPToolsListAndRead(t *testing.T) {
	f := newMCPFixture(t)
	s := f.connect(t, f.writeT)

	names := toolNames(t, s)
	for _, tl := range mcpTools() {
		if !names[tl.Name] {
			t.Errorf("tools/list is missing %s", tl.Name)
		}
	}
	if len(names) != 33 {
		t.Errorf("got %d tools, want 33", len(names))
	}

	// Input schemas are strict.
	res, _ := s.ListTools(context.Background(), nil)
	for _, tl := range res.Tools {
		b, _ := json.Marshal(tl.InputSchema)
		if !strings.Contains(string(b), `"additionalProperties"`) {
			t.Errorf("%s: schema allows extra properties: %s", tl.Name, b)
		}
		if tl.Description == "" {
			t.Errorf("%s has no description", tl.Name)
		}
	}
	mustErr(t, s, "repo.info", map[string]any{"repo": "zoe/demo", "bogus": 1}, "")
	mustErr(t, s, "repo.info", map[string]any{"repo": "not a repo"}, "")

	list := call(t, s, "repo.list", nil)
	if n := len(list["repositories"].([]any)); n != 2 {
		t.Errorf("repo.list = %d repos, want 2", n)
	}
	info := call(t, s, "repo.info", map[string]any{"repo": "zoe/demo"})
	if info["default_branch"] != "main" || info["open_pulls"].(float64) != 1 {
		t.Errorf("repo.info = %v", info)
	}

	file := call(t, s, "code.read", map[string]any{"repo": "zoe/demo", "path": "README.md"})
	if !strings.Contains(file["content"].(string), "Hello from the demo repo.") {
		t.Errorf("code.read = %v", file)
	}
	part := call(t, s, "code.read", map[string]any{"repo": "zoe/demo", "path": "app/main.go", "ref": "feature/hello", "start_line": 8, "end_line": 8})
	if part["content"] != "func Hello() string { return \"hi\" }\n" {
		t.Errorf("code.read lines = %q", part["content"])
	}
	mustErr(t, s, "code.read", map[string]any{"repo": "zoe/demo", "path": "../etc/passwd"}, "..")
	mustErr(t, s, "code.read", map[string]any{"repo": "zoe/demo", "path": "app"}, "directory")

	tree := call(t, s, "code.list", map[string]any{"repo": "zoe/demo"})
	if len(tree["entries"].([]any)) != 2 {
		t.Errorf("code.list = %v", tree)
	}
	rec := call(t, s, "code.list", map[string]any{"repo": "zoe/demo", "recursive": true})
	if rec["total"].(float64) != 2 || !strings.Contains(fmt.Sprint(rec["entries"]), "app/main.go") {
		t.Errorf("code.list recursive = %v", rec)
	}

	found := call(t, s, "code.search", map[string]any{"repo": "zoe/demo", "query": "FindMe"})
	m := found["matches"].([]any)
	if len(m) != 2 || m[0].(map[string]any)["path"] != "app/main.go" {
		t.Errorf("code.search = %v", found)
	}
	none := call(t, s, "code.search", map[string]any{"repo": "zoe/demo", "query": "nothing-like-this"})
	if len(none["matches"].([]any)) != 0 {
		t.Errorf("code.search with no match = %v", none)
	}
	// Option-looking queries are searched for, not run.
	call(t, s, "code.search", map[string]any{"repo": "zoe/demo", "query": "--output=/tmp/x"})

	commits := call(t, s, "commits.list", map[string]any{"repo": "zoe/demo", "per_page": 1})
	if len(commits["commits"].([]any)) != 1 || commits["has_more"] != true {
		t.Errorf("commits.list = %v", commits)
	}
	cm := call(t, s, "commit.get", map[string]any{"repo": "zoe/demo", "sha": f.head[:10]})
	if !strings.Contains(cm["patch"].(string), "+func Hello()") {
		t.Errorf("commit.get = %v", cm)
	}
	mustErr(t, s, "repo.info", map[string]any{"repo": "zoe/missing"}, "not found")
}

func TestMCPIssuesAndPulls(t *testing.T) {
	f := newMCPFixture(t)
	s := f.connect(t, f.writeT)
	ctx := context.Background()
	seen := &map[string]bool{}
	savedHandlers := eventHandlers
	t.Cleanup(func() { eventHandlers = savedHandlers })
	registerEventHandler(func(_ context.Context, _ *App, ev Event) { (*seen)[ev.Kind] = true })

	created := call(t, s, "issues.create", map[string]any{"repo": "zoe/demo", "title": "Crash on start",
		"body": "Steps:\n1. run it", "labels": []any{"bug"}})
	is := created["issue"].(map[string]any)
	n := int64(is["number"].(float64))
	if is["author"] != "token:claude" || fmt.Sprint(is["labels"]) != "[bug]" {
		t.Errorf("issues.create = %v", is)
	}
	mustErr(t, s, "issues.create", map[string]any{"repo": "zoe/demo", "title": "x", "labels": []any{"nope"}}, "no label named nope")

	call(t, s, "issues.comment", map[string]any{"repo": "zoe/demo", "number": n, "body": "Looking into it."})
	lab := call(t, s, "issues.label", map[string]any{"repo": "zoe/demo", "number": n, "add": []any{"enhancement"}, "remove": []any{"bug"}})
	if fmt.Sprint(lab["labels"]) != "[enhancement]" {
		t.Errorf("issues.label = %v", lab)
	}
	got := call(t, s, "issues.get", map[string]any{"repo": "zoe/demo", "number": n})
	if len(got["comments"].([]any)) != 1 || got["issue"].(map[string]any)["body"] != "Steps:\n1. run it" {
		t.Errorf("issues.get = %v", got)
	}
	call(t, s, "issues.close", map[string]any{"repo": "zoe/demo", "number": n, "reason": "not_planned", "comment": "Won't fix."})
	mustErr(t, s, "issues.close", map[string]any{"repo": "zoe/demo", "number": n}, "already closed")
	closed := call(t, s, "issues.list", map[string]any{"repo": "zoe/demo", "state": "closed"})
	if len(closed["issues"].([]any)) != 1 {
		t.Errorf("issues.list closed = %v", closed)
	}
	open := call(t, s, "issues.list", map[string]any{"repo": "zoe/demo"})
	if len(open["issues"].([]any)) != 0 {
		t.Errorf("issues.list open = %v", open)
	}
	mustErr(t, s, "issues.get", map[string]any{"repo": "zoe/demo", "number": f.pull.Number}, "pull request")

	// Pull requests.
	pl := call(t, s, "pulls.list", map[string]any{"repo": "zoe/demo"})
	if len(pl["pulls"].([]any)) != 1 {
		t.Errorf("pulls.list = %v", pl)
	}
	pg := call(t, s, "pulls.get", map[string]any{"repo": "zoe/demo", "number": f.pull.Number})
	if pg["head_sha"] != f.head || len(pg["files"].([]any)) != 1 {
		t.Errorf("pulls.get = %v", pg)
	}
	diff := call(t, s, "pulls.diff", map[string]any{"repo": "zoe/demo", "number": f.pull.Number})
	files := diff["files"].([]any)
	if len(files) != 1 || !strings.Contains(files[0].(map[string]any)["patch"].(string), "+func Hello() string") {
		t.Errorf("pulls.diff = %v", diff)
	}
	call(t, s, "pulls.comment", map[string]any{"repo": "zoe/demo", "number": f.pull.Number, "body": "Looks good."})
	note := call(t, s, "pulls.note", map[string]any{"repo": "zoe/demo", "number": f.pull.Number, "path": "app/main.go",
		"line": 8, "body": "Maybe say hello?"})
	if note["commit"] != f.head {
		t.Errorf("pulls.note = %v", note)
	}
	mustErr(t, s, "pulls.note", map[string]any{"repo": "zoe/demo", "number": f.pull.Number, "path": "app/main.go",
		"line": 400, "body": "x"}, "isn't part of the diff")
	if up := call(t, s, "pulls.update_branch", map[string]any{"repo": "zoe/demo", "number": f.pull.Number}); up["updated"] != false || up["head_sha"] != f.head {
		t.Errorf("pulls.update_branch = %v", up)
	}
	mustErr(t, s, "pulls.note", map[string]any{"repo": "zoe/demo", "number": f.pull.Number, "path": "README.md",
		"line": 1, "body": "x"}, "isn't changed")
	notes, _ := f.a.db.PullNotes(ctx, f.pull.ID)
	if len(notes) != 1 || notes[0].Line != 8 || notes[0].Side != "new" || notes[0].AuthorName != "claude" {
		t.Errorf("stored notes = %+v", notes)
	}

	// Every write emitted its event.
	for _, k := range []string{EventIssueOpened, EventIssueCommented, "issue.labeled", EventIssueClosed, EventPullCommented, EventPullNote} {
		if !(*seen)[k] {
			t.Errorf("no %s event (got %v)", k, *seen)
		}
	}
	// …and the owner was notified (tokens acting for them aren't "themselves").
	me, _ := f.a.db.PersonalOwner(ctx)
	ns, _ := f.a.db.Notifications(ctx, store.NotificationQuery{PersonID: me.ID})
	if len(ns) != 2 || ns[0].ActorName != "claude" {
		t.Errorf("notifications = %+v", ns)
	}
	nl := call(t, s, "notifications.list", map[string]any{"per_page": 1})
	if len(nl["notifications"].([]any)) != 1 || nl["has_more"] != true {
		t.Errorf("notifications.list = %v", nl)
	}

	// Releases: drafts only.
	d := call(t, s, "releases.draft", map[string]any{"repo": "zoe/demo", "tag": "v0.1.0", "body": "First release."})
	rel := d["release"].(map[string]any)
	if rel["draft"] != true {
		t.Errorf("releases.draft = %v", rel)
	}
	if g, err := f.a.git.Open("zoe", "demo"); err == nil {
		if _, ok := g.TagCommit(ctx, "v0.1.0"); ok {
			t.Error("drafting a release must not create its tag")
		}
	}
	rl := call(t, s, "releases.list", map[string]any{"repo": "zoe/demo"})
	if len(rl["releases"].([]any)) != 1 {
		t.Errorf("releases.list = %v", rl)
	}
	mustErr(t, s, "releases.draft", map[string]any{"repo": "zoe/demo", "tag": "v0.1.0", "body": "again"}, "already a release")
}

func TestMCPAuthAndScopes(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()

	// No token / bad token / token without the mcp scope.
	for _, c := range []struct {
		name, auth string
		want       int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"junk token", "Bearer atl_nope", http.StatusUnauthorized},
	} {
		req, _ := http.NewRequest("POST", f.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, res.StatusCode, c.want)
		}
	}
	gitTok, _ := f.a.db.CreateToken(ctx, "git", tokenScopes["write"], time.Hour)
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	if _, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: f.srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRT{gitTok}}, MaxRetries: -1}, nil); err == nil {
		t.Error("a token without the mcp scope connected")
	}

	// A read-only MCP token sees and runs only read tools.
	ro := f.connect(t, f.readT)
	names := toolNames(t, ro)
	if names["issues.create"] || names["releases.draft"] || !names["code.read"] || !names["notifications.list"] {
		t.Errorf("read-only token tools = %v", names)
	}
	call(t, ro, "code.read", map[string]any{"repo": "zoe/demo", "path": "README.md"})
	mustErr(t, ro, "issues.create", map[string]any{"repo": "zoe/demo", "title": "nope"}, "")

	// Sessions are bound to their token.
	rw := f.connect(t, f.writeT)
	sid := rw.ID()
	req, _ := http.NewRequest("POST", f.srv.URL+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"issues.create","arguments":{"repo":"zoe/demo","title":"hijack"}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+f.readT)
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Error("another token could use a session")
	}
	if is, _, _ := f.a.db.Issues(ctx, f.repo.ID, store.IssueQuery{}, 0, 10); len(is) != 0 {
		t.Errorf("hijacked session created an issue: %+v", is)
	}

	// A second person can't see the owner's private repositories.
	friend, err := f.a.db.CreateOwner(ctx, "friend", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	ft, _ := f.a.db.CreateToken(ctx, "friend-claude", tokenScopes["mcp"], time.Hour)
	if err := f.a.db.AssignTokenPerson(ctx, ft, friend.ID); err != nil {
		t.Fatal(err)
	}
	fs := f.connect(t, ft)
	if n := len(call(t, fs, "repo.list", nil)["repositories"].([]any)); n != 0 {
		t.Errorf("second person sees %d repositories", n)
	}
	mustErr(t, fs, "code.read", map[string]any{"repo": "zoe/demo", "path": "README.md"}, "not found")
	mustErr(t, fs, "issues.create", map[string]any{"repo": "zoe/demo", "title": "sneaky"}, "not found")
	nl := call(t, fs, "notifications.list", nil)
	if len(nl["notifications"].([]any)) != 0 {
		t.Errorf("second person's notifications = %v", nl)
	}
}

// jobToken fakes an agentic job's token: a real run and job (so the actor is
// named after the workflow) and a resolver like the Actions one.
func (f *mcpFixture) jobToken(t *testing.T, tools ...string) string {
	t.Helper()
	ctx := context.Background()
	me, _ := f.a.db.PersonalOwner(ctx)
	run, jobs, err := f.a.db.CreateActionsRun(ctx, store.ActionsRun{RepoID: f.repo.ID, WorkflowPath: ".atelier/workflows/review-helper.md",
		WorkflowName: "Review helper", Event: "pull_request", Ref: "refs/heads/feature/hello", SHA: f.head,
		Status: actions.StatusRunning, PersonID: me.ID}, []store.ActionsJob{{JobKey: "agent", Name: "agent", Status: actions.StatusRunning}})
	if err != nil {
		t.Fatal(err)
	}
	plain := "atj_test" + store.RandomToken(8)
	scopes := []string{"repo:read", fmt.Sprintf("actions:repo:%d", f.repo.ID), fmt.Sprintf("actions:job:%d", jobs[0].ID), "packages:read"}
	for _, tl := range tools {
		scopes = append(scopes, "mcp:"+tl)
	}
	_ = run
	saved := tokenResolvers
	t.Cleanup(func() { tokenResolvers = saved })
	registerTokenResolver(func(_ context.Context, _ *App, s string) (Actor, bool) {
		if s != plain {
			return Actor{}, false
		}
		return Actor{Kind: ActorJob, Name: fmt.Sprintf("actions-job-%d", jobs[0].ID), PersonID: me.ID, Scopes: scopes}, true
	})
	return plain
}

func TestMCPJobActor(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()
	tok := f.jobToken(t, "code.read", "issues.comment", "issues.create", "pulls.note", "actions.dispatch", "notifications.list")
	s := f.connect(t, tok)

	names := toolNames(t, s)
	want := map[string]bool{"code.read": true, "issues.comment": true, "issues.create": true, "pulls.note": true}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("job tools = %v, want %v (never actions.dispatch or notifications.list)", names, want)
	}
	call(t, s, "code.read", map[string]any{"repo": "zoe/demo", "path": "README.md"})
	// Only its own repository.
	mustErr(t, s, "code.read", map[string]any{"repo": "zoe/other", "path": "README.md"}, "not found")
	// Only listed tools.
	mustErr(t, s, "issues.close", map[string]any{"repo": "zoe/demo", "number": 1}, "")
	mustErr(t, s, "actions.dispatch", map[string]any{"repo": "zoe/demo", "workflow": ".atelier/workflows/ci.yml"}, "")
	mustErr(t, s, "notifications.list", nil, "")

	// Writes are recorded as the agent, named after its workflow.
	created := call(t, s, "issues.create", map[string]any{"repo": "zoe/demo", "title": "Found a bug"})
	is := created["issue"].(map[string]any)
	if is["author"] != "agent:review-helper" {
		t.Errorf("agent issue author = %v", is["author"])
	}
	call(t, s, "pulls.note", map[string]any{"repo": "zoe/demo", "number": f.pull.Number, "path": "app/main.go", "line": 8, "body": "nit"})
	notes, _ := f.a.db.PullNotes(ctx, f.pull.ID)
	if len(notes) != 1 || notes[0].AuthorKind != "agent" || notes[0].AuthorName != "review-helper" {
		t.Errorf("agent note = %+v", notes)
	}
	// The owner hears about agent activity.
	me, _ := f.a.db.PersonalOwner(ctx)
	ns, _ := f.a.db.Notifications(ctx, store.NotificationQuery{PersonID: me.ID})
	if len(ns) == 0 || ns[0].ActorKind != "agent" {
		t.Errorf("notifications after agent writes = %+v", ns)
	}

	// The call log names the agent.
	calls, _ := f.a.db.MCPCalls(ctx, me.ID, 50)
	sawAgent := false
	for _, c := range calls {
		sawAgent = sawAgent || (c.ActorKind == "agent" && c.ActorName == "review-helper" && c.Tool == "issues.create" && c.OK)
	}
	if !sawAgent {
		t.Errorf("call log = %+v", calls)
	}

	// A job without MCP tools can't connect at all.
	bare := f.jobToken(t)
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	if _, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: f.srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRT{bare}}, MaxRetries: -1}, nil); err == nil {
		t.Error("a job without MCP tools connected")
	}
}

func TestMCPOutputCapsAndLog(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()
	work := pullsWork(t, f.a)
	runGit(t, work, "fetch", "-q", "origin", "main")
	runGit(t, work, "checkout", "-q", "-B", "main", "FETCH_HEAD")
	big := strings.Repeat("0123456789abcdef\n", 10000) // 170 KB
	pullsCommit(t, work, "big.txt", big, "Big file")
	runGit(t, work, "push", "-q", "origin", "main")

	s := f.connect(t, f.writeT)
	out := call(t, s, "code.read", map[string]any{"repo": "zoe/demo", "path": "big.txt"})
	content := out["content"].(string)
	if out["truncated"] != true || len(content) > mcpMaxText+100 || !strings.Contains(content, "[truncated:") {
		t.Errorf("code.read big: truncated=%v len=%d", out["truncated"], len(content))
	}
	if out["total_lines"].(float64) != 10000 {
		t.Errorf("total_lines = %v", out["total_lines"])
	}

	// Long comments are cut when read back.
	is, _ := f.a.db.CreateIssue(ctx, f.repo.ID, "Long", strings.Repeat("x", 100<<10), ActorOwner, "zoe")
	got := call(t, s, "issues.get", map[string]any{"repo": "zoe/demo", "number": is.Number})
	if b := got["issue"].(map[string]any)["body"].(string); len(b) > mcpMaxText+100 || !strings.Contains(b, "[truncated:") {
		t.Errorf("issue body len %d", len(b))
	}
	// Bodies over the limit are refused by the schema.
	mustErr(t, s, "issues.comment", map[string]any{"repo": "zoe/demo", "number": is.Number, "body": strings.Repeat("y", 70000)}, "")

	// Pagination.
	for i := 0; i < 3; i++ {
		f.a.db.CreateIssue(ctx, f.repo.ID, fmt.Sprintf("Issue %d", i), "", ActorOwner, "zoe")
	}
	p1 := call(t, s, "issues.list", map[string]any{"repo": "zoe/demo", "per_page": 3})
	p2 := call(t, s, "issues.list", map[string]any{"repo": "zoe/demo", "per_page": 3, "page": 2})
	if len(p1["issues"].([]any)) != 3 || p1["has_more"] != true || len(p2["issues"].([]any)) != 1 || p2["has_more"] != false {
		t.Errorf("pagination: p1=%v p2=%v", p1, p2)
	}
	mustErr(t, s, "issues.list", map[string]any{"repo": "zoe/demo", "per_page": 1000}, "")

	// Actions logs keep their tail.
	run, jobs, err := f.a.db.CreateActionsRun(ctx, store.ActionsRun{RepoID: f.repo.ID, WorkflowPath: ".atelier/workflows/ci.yml",
		WorkflowName: "CI", Event: "push", Ref: "refs/heads/main", Status: actions.StatusRunning},
		[]store.ActionsJob{{JobKey: "build", Name: "build", Status: actions.StatusRunning}})
	if err != nil {
		t.Fatal(err)
	}
	var lines []actions.LogLine
	for i := 0; i < 5000; i++ {
		lines = append(lines, actions.LogLine{Step: 0, Stream: "stdout", Text: fmt.Sprintf("line %05d %s", i, strings.Repeat(".", 20))})
	}
	b, _ := json.Marshal(lines)
	if _, err := f.a.db.AppendActionsLog(ctx, jobs[0].ID, string(b), int64(len(b)), 1<<30); err != nil {
		t.Fatal(err)
	}
	runs := call(t, s, "actions.runs", map[string]any{"repo": "zoe/demo"})
	if len(runs["runs"].([]any)) != 1 {
		t.Errorf("actions.runs = %v", runs)
	}
	logs := call(t, s, "actions.logs", map[string]any{"repo": "zoe/demo", "run": run.Seq})
	j := logs["jobs"].([]any)[0].(map[string]any)
	if j["truncated"] != true || !strings.Contains(j["log"].(string), "line 04999") || strings.Contains(j["log"].(string), "line 00000") {
		t.Errorf("actions.logs: truncated=%v", j["truncated"])
	}

	// The call log: tool, actor, repo and ids, no arguments.
	me, _ := f.a.db.PersonalOwner(ctx)
	calls, err := f.a.db.MCPCalls(ctx, me.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) < 5 {
		t.Fatalf("only %d calls logged", len(calls))
	}
	var sawErr, sawRead bool
	for _, c := range calls {
		if c.ActorKind != ActorToken || c.ActorName != "claude" {
			t.Errorf("call actor = %s:%s", c.ActorKind, c.ActorName)
		}
		if c.Tool == "code.read" && c.Repo == "zoe/demo" && c.Target == "big.txt" && c.OK {
			sawRead = true
		}
		if !c.OK && c.Error != "" {
			sawErr = true
		}
		if strings.Contains(c.Target+c.Error, "yyyy") {
			t.Error("arguments leaked into the call log")
		}
	}
	if !sawRead || !sawErr {
		t.Errorf("call log missing entries (read %v, error %v): %+v", sawRead, sawErr, calls[:5])
	}
}

func TestMCPSettingsPage(t *testing.T) {
	f := newMCPFixture(t)
	ck := pullsCookie(t, f.a)
	s := f.connect(t, f.writeT)
	call(t, s, "repo.info", map[string]any{"repo": "zoe/demo"})

	body := expectStatus(t, do(t, f.a, "GET", "/settings/mcp", nil, ck), http.StatusOK, "settings page")
	mustContain(t, body, "settings", "/mcp", "claude mcp add --transport http atelier", "&#34;type&#34;: &#34;http&#34;",
		"repo.info", "Recent calls", "Create MCP token", `aria-current="page">MCP`)
	if strings.Contains(body, f.writeT) {
		t.Error("the page shows an existing token")
	}

	rec := do(t, f.a, "POST", "/settings/mcp/token", url.Values{"name": {"Claude Desktop"}, "access": {"mcp-read"}, "expires": {"30"}}, ck)
	body = expectStatus(t, rec, http.StatusOK, "create token")
	if !strings.Contains(body, "Your new MCP token") || !strings.Contains(body, "Bearer atl_") {
		t.Errorf("new token not shown in snippets")
	}
	toks, _ := f.a.db.Tokens(context.Background())
	var made store.Token
	for _, tk := range toks {
		if tk.Name == "Claude Desktop" {
			made = tk
		}
	}
	if !made.Has("mcp") || made.Has("repo:write") {
		t.Errorf("created token scopes = %v", made.Scopes)
	}
	// Only MCP access levels can be created here.
	rec = do(t, f.a, "POST", "/settings/mcp/token", url.Values{"name": {"x"}, "access": {"write"}, "expires": {"30"}}, ck)
	expectStatus(t, rec, http.StatusUnprocessableEntity, "non-MCP token")

	// The tokens page offers the MCP access option.
	body = expectStatus(t, do(t, f.a, "GET", "/settings/tokens", nil, ck), http.StatusOK, "tokens page")
	mustContain(t, body, "tokens", "MCP (for Claude and other AI clients)", `value="mcp"`)
}
