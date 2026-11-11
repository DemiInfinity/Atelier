package web

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
	"atelier/internal/workflow"
)

// projectFor makes a project in zoe's account with a Priority (single
// select) and an Estimate (number) field.
func (f *mcpFixture) projectFor(t *testing.T, title string, private bool) store.Project {
	t.Helper()
	ctx := context.Background()
	me, _ := f.a.db.PersonalOwner(ctx)
	p, err := f.a.db.CreateProject(ctx, store.NewProject{OwnerID: me.ID, Title: title, Private: private,
		ActorKind: ActorOwner, ActorName: me.Name, PersonID: me.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.db.CreateField(ctx, p.ID, "Priority", store.ProjFieldSelect, []string{"High", "Low"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.db.CreateField(ctx, p.ID, "Estimate", store.ProjFieldNumber, nil); err != nil {
		t.Fatal(err)
	}
	return p
}

func itemsOf(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	var xs []map[string]any
	for _, x := range out["items"].([]any) {
		xs = append(xs, x.(map[string]any))
	}
	return xs
}

func TestMCPProjectsFlow(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()
	p := f.projectFor(t, "Atelier", true)
	is, _ := f.a.db.CreateIssue(ctx, f.repo.ID, "Add projects tools", "", ActorOwner, "zoe")
	other, _ := f.a.db.CreateIssue(ctx, f.repo.ID, "Another one", "", ActorOwner, "zoe")
	s := f.connect(t, f.writeT)
	pid := p.ID

	list := call(t, s, "projects.list", nil)
	if ps := list["projects"].([]any); len(ps) != 1 || ps[0].(map[string]any)["title"] != "Atelier" {
		t.Fatalf("projects.list = %v", list)
	}
	if n := len(call(t, s, "projects.list", map[string]any{"state": "closed"})["projects"].([]any)); n != 0 {
		t.Errorf("closed projects = %d", n)
	}

	// Add an issue with Status Todo; it's recorded as the token.
	ref := fmt.Sprintf("zoe/demo#%d", is.Number)
	added := call(t, s, "projects.add_item", map[string]any{"project": pid, "ref": ref, "status": "todo"})
	item := added["item"].(map[string]any)
	if added["added"] != true || item["status"] != "Todo" || item["title"] != "Add projects tools" || item["repo"] != "zoe/demo" ||
		item["added_by"] != "token:claude" || item["state"] != "open" {
		t.Fatalf("add_item = %v", added)
	}
	id := int64(item["id"].(float64))
	stored, _ := f.a.db.ProjectItem(ctx, pid, id)
	if stored.AddedKind != ActorToken || stored.AddedBy != "claude" {
		t.Errorf("stored added_by = %s:%s", stored.AddedKind, stored.AddedBy)
	}
	// Adding again returns the same item.
	again := call(t, s, "projects.add_item", map[string]any{"project": pid, "ref": "/zoe/demo/issues/" + itoa(is.Number)})
	if again["added"] != false || int64(again["item"].(map[string]any)["id"].(float64)) != id {
		t.Errorf("add again = %v", again)
	}
	mustErr(t, s, "projects.add_item", map[string]any{"project": pid, "ref": ref, "status": "Someday"}, "options are: Todo, In progress, Done")
	mustErr(t, s, "projects.add_item", map[string]any{"project": pid, "ref": "zoe/demo#999"}, "couldn't find")
	mustErr(t, s, "projects.add_item", map[string]any{"project": pid, "ref": "zoe/nope#1"}, "couldn't find")
	second := call(t, s, "projects.add_item", map[string]any{"project": pid, "ref": fmt.Sprintf("zoe/demo#%d", other.Number)})
	id2 := int64(second["item"].(map[string]any)["id"].(float64))

	// Move it to Done.
	moved := call(t, s, "projects.move_item", map[string]any{"project": pid, "item": id, "status": "Done"})
	if moved["item"].(map[string]any)["status"] != "Done" {
		t.Errorf("move to Done = %v", moved)
	}
	// Custom fields by name, validated.
	set := call(t, s, "projects.set_field", map[string]any{"project": pid, "item": id, "field": "priority", "value": "High"})
	if set["item"].(map[string]any)["fields"].(map[string]any)["Priority"] != "High" {
		t.Errorf("set Priority = %v", set)
	}
	call(t, s, "projects.set_field", map[string]any{"project": pid, "item": id, "field": "Estimate", "value": "3"})
	mustErr(t, s, "projects.set_field", map[string]any{"project": pid, "item": id, "field": "Estimate", "value": "lots"}, "number")
	mustErr(t, s, "projects.set_field", map[string]any{"project": pid, "item": id, "field": "Priority", "value": "Urgent"}, "High, Low")
	mustErr(t, s, "projects.set_field", map[string]any{"project": pid, "item": id, "field": "Colour", "value": "x"}, "Estimate, Priority, Status")
	mustErr(t, s, "projects.set_field", map[string]any{"project": pid, "item": 99999, "field": "Status", "value": "Done"}, "no item")
	call(t, s, "projects.set_field", map[string]any{"project": pid, "item": id2, "field": "Status", "value": "In progress"})

	// A draft.
	draft := call(t, s, "projects.add_draft", map[string]any{"project": pid, "title": "Write the docs", "body": "Settings → MCP", "status": "Todo"})
	di := draft["item"].(map[string]any)
	if di["kind"] != "draft" || di["status"] != "Todo" || di["state"] != "draft" || di["added_by"] != "token:claude" {
		t.Errorf("add_draft = %v", draft)
	}
	did := int64(di["id"].(float64))

	// Details and counts per Status.
	got := call(t, s, "projects.get", map[string]any{"project": pid})
	if got["can_write"] != true || !strings.Contains(fmt.Sprint(got["fields"]), "Priority") || len(got["views"].([]any)) != 1 {
		t.Errorf("projects.get = %v", got)
	}
	if c := fmt.Sprint(got["status_counts"]); c != "[map[items:1 status:Todo] map[items:1 status:In progress] map[items:1 status:Done]]" {
		t.Errorf("status_counts = %s", c)
	}

	// Filters use the board's syntax, and lists are paginated.
	for q, want := range map[string]int{
		`status:Done`: 1, `status:"In progress",Todo`: 2, `is:draft`: 1, `is:issue is:open`: 2, `priority:High`: 1,
		`estimate:>2`: 1, `-status:Done`: 2, `no:priority`: 2, `repo:demo`: 2, `docs`: 1, ``: 3,
	} {
		out := call(t, s, "projects.items", map[string]any{"project": pid, "filter": q})
		if n := len(itemsOf(t, out)); n != want {
			t.Errorf("filter %q = %d items, want %d", q, n, want)
		}
	}
	pg := call(t, s, "projects.items", map[string]any{"project": pid, "per_page": 2})
	pg2 := call(t, s, "projects.items", map[string]any{"project": pid, "per_page": 2, "page": 2})
	if len(itemsOf(t, pg)) != 2 || pg["has_more"] != true || len(itemsOf(t, pg2)) != 1 || pg2["has_more"] != false || pg["total"].(float64) != 3 {
		t.Errorf("pagination: %v / %v", pg, pg2)
	}

	// Reorder (moving id to Done without an anchor put it at the end:
	// id2, id, draft): the draft to the top, then after id.
	call(t, s, "projects.move_item", map[string]any{"project": pid, "item": did, "before": id2})
	if first := itemsOf(t, call(t, s, "projects.items", map[string]any{"project": pid}))[0]; int64(first["id"].(float64)) != did {
		t.Errorf("first item = %v", first)
	}
	call(t, s, "projects.move_item", map[string]any{"project": pid, "item": did, "after": id})
	order := itemsOf(t, call(t, s, "projects.items", map[string]any{"project": pid}))
	if int64(order[2]["id"].(float64)) != did {
		t.Errorf("order after move = %v", order)
	}
	mustErr(t, s, "projects.move_item", map[string]any{"project": pid, "item": did, "before": id, "after": id2}, "not both")
	mustErr(t, s, "projects.move_item", map[string]any{"project": pid, "item": did, "before": 424242}, "no item")

	// Remove.
	call(t, s, "projects.remove_item", map[string]any{"project": pid, "item": did})
	if items, _ := f.a.db.ProjectItems(ctx, pid); len(items) != 2 {
		t.Errorf("after remove: %d items", len(items))
	}
	mustErr(t, s, "projects.get", map[string]any{"project": 98765}, "not found")

	// The call log has ids only.
	me, _ := f.a.db.PersonalOwner(ctx)
	calls, _ := f.a.db.MCPCalls(ctx, me.ID, 200)
	var sawSet, sawErr bool
	for _, c := range calls {
		if strings.Contains(c.Target+c.Error, "Urgent") || strings.Contains(c.Target+c.Error, "lots") || strings.Contains(c.Target+c.Error, "Write the docs") {
			t.Errorf("arguments in the call log: %+v", c)
		}
		if c.Tool == "projects.set_field" && c.OK && c.Target == fmt.Sprintf("project:%d item:%d", pid, id) && c.ActorName == "claude" {
			sawSet = true
		}
		if c.Tool == "projects.set_field" && !c.OK && c.Error != "" {
			sawErr = true
		}
	}
	if !sawSet || !sawErr {
		t.Errorf("call log missing project calls (set %v, error %v)", sawSet, sawErr)
	}
}

func TestMCPProjectsPermissions(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()
	p := f.projectFor(t, "Board", true)
	is, _ := f.a.db.CreateIssue(ctx, f.repo.ID, "Item", "", ActorOwner, "zoe")
	it, _, err := f.a.db.AddItem(ctx, p.ID, "issue", f.repo.ID, is.Number, ActorOwner, "zoe")
	if err != nil {
		t.Fatal(err)
	}

	// A read-only token reads but has no write tools, and can't call them.
	ro := f.connect(t, f.readT)
	names := toolNames(t, ro)
	for _, n := range []string{"projects.list", "projects.get", "projects.items"} {
		if !names[n] {
			t.Errorf("read-only token is missing %s", n)
		}
	}
	for _, n := range []string{"projects.add_item", "projects.add_draft", "projects.set_field", "projects.move_item", "projects.remove_item"} {
		if names[n] {
			t.Errorf("read-only token has %s", n)
		}
		mustErr(t, ro, n, map[string]any{"project": p.ID, "item": it.ID, "ref": "zoe/demo#1", "title": "x", "field": "Status", "value": "Done"}, "")
	}
	if n := len(itemsOf(t, call(t, ro, "projects.items", map[string]any{"project": p.ID}))); n != 1 {
		t.Errorf("read-only items = %d", n)
	}
	if items, _ := f.a.db.ProjectItems(ctx, p.ID); len(items) != 1 || len(items[0].Values) != 0 {
		t.Errorf("read-only token changed the project: %+v", items)
	}

	// A second person sees no projects, even public ones in zoe's account.
	pub := f.projectFor(t, "Public board", false)
	friend, err := f.a.db.CreateOwner(ctx, "friend", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	ft, _ := f.a.db.CreateToken(ctx, "friend-claude", tokenScopes["mcp"], time.Hour)
	if err := f.a.db.AssignTokenPerson(ctx, ft, friend.ID); err != nil {
		t.Fatal(err)
	}
	fs := f.connect(t, ft)
	if n := len(call(t, fs, "projects.list", map[string]any{"state": "all"})["projects"].([]any)); n != 0 {
		t.Errorf("second person sees %d projects", n)
	}
	for _, id := range []int64{p.ID, pub.ID} {
		mustErr(t, fs, "projects.get", map[string]any{"project": id}, "not found")
		mustErr(t, fs, "projects.items", map[string]any{"project": id}, "not found")
		mustErr(t, fs, "projects.add_draft", map[string]any{"project": id, "title": "sneaky"}, "not found")
		mustErr(t, fs, "projects.remove_item", map[string]any{"project": id, "item": it.ID}, "not found")
	}
	if items, _ := f.a.db.ProjectItems(ctx, p.ID); len(items) != 1 {
		t.Errorf("second person changed the project: %d items", len(items))
	}

	// The web rule is unchanged: plain (non-MCP) tokens never manage projects.
	if f.a.canProject(ctx, Actor{Kind: ActorToken, PersonID: 1, Scopes: tokenScopes["mcp"]}, p, PermRead) {
		t.Error("canProject now lets tokens in")
	}
}

func TestMCPProjectsJobScoping(t *testing.T) {
	f := newMCPFixture(t)
	ctx := context.Background()
	linked := f.projectFor(t, "Linked", true)
	unlinked := f.projectFor(t, "Unlinked", true)
	if err := f.a.db.LinkProjectRepo(ctx, linked.ID, f.repo.ID); err != nil {
		t.Fatal(err)
	}
	mine, _ := f.a.db.CreateIssue(ctx, f.repo.ID, "Job repo issue", "", ActorOwner, "zoe")
	secret, _ := f.a.db.CreateIssue(ctx, f.other.ID, "Secret plan", "", ActorOwner, "zoe")
	lb, _ := f.a.db.CreateLabel(ctx, f.other.ID, "confidential", "d73a4a", "")
	_ = f.a.db.SetIssueLabels(ctx, secret.ID, []int64{lb.ID}, ActorOwner, "zoe")
	hidden, _, _ := f.a.db.AddItem(ctx, linked.ID, "issue", f.other.ID, secret.Number, ActorOwner, "zoe")
	status, _ := f.a.db.StatusField(ctx, linked.ID)
	_ = f.a.db.SetItemValue(ctx, linked.ID, hidden.ID, status.ID, "In progress")
	if _, err := f.a.db.AddDraft(ctx, unlinked.ID, "Elsewhere", "", ActorOwner, "zoe"); err != nil {
		t.Fatal(err)
	}

	tok := f.jobToken(t, "projects.list", "projects.items", "projects.add_item", "projects.set_field")
	s := f.connect(t, tok)
	names := toolNames(t, s)
	want := map[string]bool{"projects.list": true, "projects.items": true, "projects.add_item": true, "projects.set_field": true}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("job tools = %v, want %v", names, want)
	}

	// Only projects that link the job's repository.
	ps := call(t, s, "projects.list", nil)["projects"].([]any)
	if len(ps) != 1 || ps[0].(map[string]any)["title"] != "Linked" {
		t.Errorf("job projects.list = %v", ps)
	}
	mustErr(t, s, "projects.items", map[string]any{"project": unlinked.ID}, "not found")
	mustErr(t, s, "projects.add_item", map[string]any{"project": unlinked.ID, "ref": fmt.Sprintf("zoe/demo#%d", mine.Number)}, "not found")

	// Items from repositories the job can't read are private.
	items := itemsOf(t, call(t, s, "projects.items", map[string]any{"project": linked.ID}))
	if len(items) != 1 {
		t.Fatalf("job items = %v", items)
	}
	h := items[0]
	if h["private"] != true || h["status"] != "In progress" {
		t.Errorf("hidden item = %v", h)
	}
	for _, k := range []string{"title", "repo", "labels", "number", "kind", "state", "web_path", "added_by"} {
		if _, ok := h[k]; ok {
			t.Errorf("hidden item shows %s: %v", k, h)
		}
	}
	// …and content filters can't probe them; the project's own fields can.
	for _, q := range []string{"Secret", "repo:other", "label:confidential", "is:open", "is:issue"} {
		if n := len(itemsOf(t, call(t, s, "projects.items", map[string]any{"project": linked.ID, "filter": q}))); n != 0 {
			t.Errorf("filter %q matched the hidden item", q)
		}
	}
	if n := len(itemsOf(t, call(t, s, "projects.items", map[string]any{"project": linked.ID, "filter": `status:"In progress"`}))); n != 1 {
		t.Errorf("status filter on hidden item = %d", n)
	}
	mustErr(t, s, "projects.set_field", map[string]any{"project": linked.ID, "item": hidden.ID, "field": "Status", "value": "Done"}, "no item")
	mustErr(t, s, "projects.add_item", map[string]any{"project": linked.ID, "ref": fmt.Sprintf("zoe/other#%d", secret.Number)}, "couldn't find")

	// Writes are allowed by the workflow's grant and recorded as the agent.
	added := call(t, s, "projects.add_item", map[string]any{"project": linked.ID, "ref": fmt.Sprintf("zoe/demo#%d", mine.Number), "status": "Todo"})
	ai := added["item"].(map[string]any)
	if ai["added_by"] != "agent:review-helper" || ai["status"] != "Todo" {
		t.Errorf("agent add_item = %v", ai)
	}
	call(t, s, "projects.set_field", map[string]any{"project": linked.ID, "item": ai["id"], "field": "Status", "value": "Done"})
	it, _ := f.a.db.ProjectItem(ctx, linked.ID, int64(ai["id"].(float64)))
	if it.AddedKind != "agent" || it.AddedBy != "review-helper" {
		t.Errorf("stored agent actor = %s:%s", it.AddedKind, it.AddedBy)
	}
	if o, _ := status.Option(it.Values[status.ID]); o.Name != "Done" {
		t.Errorf("agent status = %q", it.Values[status.ID])
	}

	// Unlisted tools are neither shown nor callable.
	mustErr(t, s, "projects.remove_item", map[string]any{"project": linked.ID, "item": it.ID}, "")
	mustErr(t, s, "projects.get", map[string]any{"project": linked.ID}, "")
	if items, _ := f.a.db.ProjectItems(ctx, linked.ID); len(items) != 2 {
		t.Errorf("linked project has %d items", len(items))
	}

	// The call log names the agent.
	me, _ := f.a.db.PersonalOwner(ctx)
	calls, _ := f.a.db.MCPCalls(ctx, me.ID, 100)
	saw := false
	for _, c := range calls {
		saw = saw || (c.Tool == "projects.add_item" && c.OK && c.ActorKind == "agent" && c.ActorName == "review-helper")
	}
	if !saw {
		t.Errorf("call log = %+v", calls)
	}
}

// The agentic workflow tool list and the MCP server agree on names and on
// which tools write.
func TestMCPAgentToolsMatchServer(t *testing.T) {
	byName := map[string]*mcpTool{}
	for _, tl := range mcpTools() {
		byName[tl.Name] = tl
	}
	for _, name := range workflow.AgentTools {
		tl := byName[name]
		if tl == nil {
			t.Errorf("agent tool %s isn't an MCP tool", name)
			continue
		}
		if tl.Write != workflow.AgentToolWrites(name) {
			t.Errorf("%s: MCP write = %v, workflow write = %v", name, tl.Write, workflow.AgentToolWrites(name))
		}
	}
	for name := range byName {
		if !strings.HasPrefix(name, "projects.") {
			continue
		}
		if !workflow.AgentToolAllowed(name) {
			t.Errorf("agents can't be given %s", name)
		}
	}
}
