package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"atelier/internal/store"
)

type projFix struct {
	a        *App
	zoe, sam store.Owner
	zc, sc   map[string]string
	demo     store.Repo // private
	pub      store.Repo // public
}

// newProjFix: zoe (the owner) with private demo and public open, and a
// second person sam.
func newProjFix(t *testing.T) projFix {
	t.Helper()
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	zoe, _ := a.db.OwnerByName(ctx, "zoe")
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	pub := profRepo(t, a, zoe, "open", false)
	return projFix{a: a, zoe: zoe, sam: sam, zc: cookieFor(t, a, zoe.ID), sc: cookieFor(t, a, sam.ID), demo: demo, pub: pub}
}

func (f projFix) issue(t *testing.T, rp store.Repo, title string, labels ...string) store.Issue {
	t.Helper()
	ctx := context.Background()
	is, err := f.a.db.CreateIssue(ctx, rp.ID, title, "", "owner", "zoe")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, l := range labels {
		lb, err := f.a.db.CreateLabel(ctx, rp.ID, l, "d73a4a", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, lb.ID)
	}
	if len(ids) > 0 {
		if err := f.a.db.SetIssueLabels(ctx, is.ID, ids, "owner", "zoe"); err != nil {
			t.Fatal(err)
		}
	}
	return is
}

// newProject creates a project through the web and returns it.
func (f projFix) newProject(t *testing.T, title string, public bool) store.Project {
	t.Helper()
	vis := "private"
	if public {
		vis = "public"
	}
	rec := do(t, f.a, "POST", "/projects/new", url.Values{"account": {"zoe"}, "title": {title}, "visibility": {vis}}, f.zc)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/projects/") {
		t.Fatalf("create project = %d %q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	id, _ := strconv.ParseInt(strings.TrimPrefix(rec.Header().Get("Location"), "/projects/"), 10, 64)
	p, err := f.a.db.ProjectByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f projFix) items(t *testing.T, p store.Project) []store.ProjItem {
	t.Helper()
	items, err := f.a.db.ProjectItems(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func (f projFix) itemFor(t *testing.T, p store.Project, title string) store.ProjItem {
	t.Helper()
	for _, it := range f.items(t, p) {
		if it.Title == title {
			return it
		}
	}
	t.Fatalf("no item %q on project %d", title, p.ID)
	return store.ProjItem{}
}

func optID(f store.ProjField, name string) string {
	o, _ := f.OptionNamed(name)
	return strconv.FormatInt(o.ID, 10)
}

func TestProjectsBoardFlow(t *testing.T) {
	f := newProjFix(t)
	ctx := context.Background()
	a := f.a
	bug := f.issue(t, f.demo, "Crash on save", "bug")
	f.issue(t, f.demo, "Quiet issue")
	pl, err := a.db.CreatePull(ctx, store.NewPull{RepoID: f.demo.ID, Title: "Fix the crash", Head: "fix", Base: "main", AuthorKind: "owner", AuthorName: "zoe"})
	if err != nil {
		t.Fatal(err)
	}

	// Bad input re-renders the form.
	expectStatus(t, do(t, a, "POST", "/projects/new", url.Values{"account": {"zoe"}, "title": {""}}, f.zc), 422, "blank title")
	expectStatus(t, do(t, a, "POST", "/projects/new", url.Values{"account": {"nobody"}, "title": {"x"}}, f.zc), 422, "unknown account")
	expectStatus(t, do(t, a, "GET", "/projects/new?account=zoe", nil, f.zc), 200, "new page")

	p := f.newProject(t, "Atelier roadmap", false)
	base := "/projects/" + itoa(p.ID)
	page := expectStatus(t, do(t, a, "GET", base, nil, f.zc), 200, "empty board")
	mustContain(t, page, "empty board", "Atelier roadmap", "Board", "+ Add item", "/static/projects.js")

	// Add by reference, by link, and a draft into a column.
	status, _ := a.db.StatusField(ctx, p.ID)
	for _, form := range []url.Values{
		{"ref": {"zoe/demo#" + itoa(bug.Number)}},
		{"ref": {"https://git.example.com/zoe/demo/pulls/" + itoa(pl.Number)}},
		{"ref": {"zoe/demo#2"}, "status": {optID(status, "In progress")}},
		{"title": {"Write the docs"}, "status": {optID(status, "Todo")}, "return": {base + "?layout=table"}},
	} {
		rec := do(t, a, "POST", base+"/items", form, f.zc)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("add %v = %d\n%s", form, rec.Code, rec.Body)
		}
	}
	if loc := do(t, a, "POST", base+"/items", url.Values{"title": {"x"}, "return": {"//evil.com"}}, f.zc).Header().Get("Location"); loc != base {
		t.Errorf("evil return = %q", loc)
	}
	expectStatus(t, do(t, a, "POST", base+"/items", url.Values{"ref": {"zoe/nope#1"}}, f.zc), 422, "missing repo")
	expectStatus(t, do(t, a, "POST", base+"/items", url.Values{"ref": {"zoe/demo#99"}}, f.zc), 422, "missing number")
	if n := len(f.items(t, p)); n != 5 {
		t.Fatalf("items = %d, want 5", n)
	}
	quiet := f.itemFor(t, p, "Quiet issue")
	if quiet.Values[status.ID] != optID(status, "In progress") {
		t.Errorf("status on add = %q", quiet.Values[status.ID])
	}
	if it := f.itemFor(t, p, "Fix the crash"); it.Kind != "pull" {
		t.Errorf("pull added as %q", it.Kind)
	}

	page = expectStatus(t, do(t, a, "GET", base, nil, f.zc), 200, "board")
	mustContain(t, page, "board", "Crash on save", "Fix the crash", "Write the docs", "No status", "In progress", `draggable="true"`, "/zoe/demo/labels.css")

	// Board moves: into a column (as the script does), then up and down.
	crash := f.itemFor(t, p, "Crash on save")
	itemURL := base + "/items/" + itoa(crash.ID)
	rec := do(t, a, "POST", itemURL+"/move", url.Values{"status": {optID(status, "In progress")}, "before": {itoa(quiet.ID)}}, map[string]string{"Cookie": f.zc["Cookie"], "Accept": "application/json"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("json move = %d %s", rec.Code, rec.Body)
	}
	crash = f.itemFor(t, p, "Crash on save")
	if crash.Values[status.ID] != optID(status, "In progress") || crash.Position >= f.itemFor(t, p, "Quiet issue").Position {
		t.Fatalf("after move: %+v", crash)
	}
	expectStatus(t, do(t, a, "POST", itemURL+"/move", url.Values{"after": {itoa(quiet.ID)}}, f.zc), 303, "move down")
	if f.itemFor(t, p, "Crash on save").Position <= f.itemFor(t, p, "Quiet issue").Position {
		t.Error("move down didn't put the card after its neighbour")
	}
	if f.itemFor(t, p, "Crash on save").Values[status.ID] != optID(status, "In progress") {
		t.Error("a move without status changed the status")
	}
	expectStatus(t, do(t, a, "POST", itemURL+"/move", url.Values{"before": {itoa(quiet.ID)}}, f.zc), 303, "move up")
	expectStatus(t, do(t, a, "POST", itemURL+"/move", url.Values{"status": {""}}, f.zc), 303, "to no status")
	if v := f.itemFor(t, p, "Crash on save").Values[status.ID]; v != "" {
		t.Errorf("no status = %q", v)
	}
	expectStatus(t, do(t, a, "POST", itemURL+"/move", url.Values{"status": {"424242"}}, f.zc), 422, "unknown status")

	// Fields: create, edit inline, reject bad values.
	expectStatus(t, do(t, a, "POST", base+"/fields", url.Values{"name": {"Estimate"}, "kind": {"number"}}, f.zc), 303, "new field")
	expectStatus(t, do(t, a, "POST", base+"/fields", url.Values{"name": {"Phase"}, "kind": {"single_select"}, "options": {"Phase 1\nPhase 2"}}, f.zc), 303, "new select")
	expectStatus(t, do(t, a, "POST", base+"/fields", url.Values{"name": {"Start date"}, "kind": {"date"}}, f.zc), 303, "start")
	expectStatus(t, do(t, a, "POST", base+"/fields", url.Values{"name": {"Target date"}, "kind": {"date"}}, f.zc), 303, "target")
	expectStatus(t, do(t, a, "POST", base+"/fields", url.Values{"name": {"Repository"}, "kind": {"text"}}, f.zc), 422, "reserved name")
	fields, _ := a.db.ProjectFields(ctx, p.ID)
	byName := map[string]store.ProjField{}
	for _, fd := range fields {
		byName[fd.Name] = fd
	}
	set := func(it store.ProjItem, field, value string, want int) {
		t.Helper()
		expectStatus(t, do(t, a, "POST", base+"/items/"+itoa(it.ID)+"/field",
			url.Values{"field": {itoa(byName[field].ID)}, "value": {value}, "return": {base + "?layout=table"}}, f.zc), want, "set "+field+"="+value)
	}
	set(crash, "Estimate", "5", 303)
	set(quiet, "Estimate", "2", 303)
	set(crash, "Estimate", "five", 422)
	set(crash, "Phase", optID(byName["Phase"], "Phase 2"), 303)
	set(crash, "Start date", "2026-11-02", 303)
	set(crash, "Target date", "2026-11-20", 303)
	set(crash, "Target date", "next week", 422)
	if v := f.itemFor(t, p, "Crash on save").Values[byName["Estimate"].ID]; v != "5" {
		t.Errorf("estimate = %q", v)
	}

	// Table: sortable, with editable cells.
	page = expectStatus(t, do(t, a, "GET", base+"?layout=table&sort=f"+itoa(byName["Estimate"].ID)+":desc", nil, f.zc), 200, "table")
	mustContain(t, page, "table", "<table", "Estimate", `aria-sort="descending"`, `name="value"`)
	if i, j := strings.Index(page, "Crash on save"), strings.Index(page, "Quiet issue"); i < 0 || j < 0 || i > j {
		t.Errorf("table not sorted by estimate desc (crash at %d, quiet at %d)", i, j)
	}
	// Roadmap: the dated item gets a bar; the others are listed without dates.
	page = expectStatus(t, do(t, a, "GET", base+"?layout=roadmap", nil, f.zc), 200, "roadmap")
	mustContain(t, page, "roadmap", "<rect", "2 Nov 2026 – 20 Nov 2026", "No dates")

	// Filters in the URL.
	page = expectStatus(t, do(t, a, "GET", base+"?q="+url.QueryEscape(`status:"In progress"`), nil, f.zc), 200, "status filter")
	if !strings.Contains(page, "Quiet issue") || strings.Contains(page, "Write the docs") || strings.Contains(page, "Crash on save") {
		t.Errorf("status filter shows the wrong cards")
	}
	page = expectStatus(t, do(t, a, "GET", base+"?layout=table&q="+url.QueryEscape("label:bug is:open repo:demo"), nil, f.zc), 200, "label filter")
	if !strings.Contains(page, "Crash on save") || strings.Contains(page, "Quiet issue") || strings.Contains(page, "Fix the crash") {
		t.Errorf("label filter shows the wrong rows")
	}
	page = expectStatus(t, do(t, a, "GET", base+"?q="+url.QueryEscape("estimate:>3 is:issue"), nil, f.zc), 200, "number filter")
	if !strings.Contains(page, "Crash on save") || strings.Contains(page, "Quiet issue") {
		t.Errorf("number filter shows the wrong cards")
	}

	// Saved views.
	rec = do(t, a, "POST", base+"/views", url.Values{"name": {"Bugs"}, "layout": {"table"}, "q": {"label:bug"}}, f.zc)
	if rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "?view=") {
		t.Fatalf("save view = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	page = expectStatus(t, do(t, a, "GET", rec.Header().Get("Location"), nil, f.zc), 200, "saved view")
	mustContain(t, page, "saved view", ">Bugs</a>", "<table", `value="label:bug"`)
	if strings.Contains(page, "Quiet issue") {
		t.Error("saved view filter not applied")
	}
	expectStatus(t, do(t, a, "POST", base+"/views", url.Values{"name": {""}, "layout": {"table"}}, f.zc), 422, "unnamed view")

	// Drafts: edit, then convert to an issue in demo.
	docs := f.itemFor(t, p, "Write the docs")
	expectStatus(t, do(t, a, "GET", base+"/items/"+itoa(docs.ID), nil, f.zc), 200, "draft page")
	expectStatus(t, do(t, a, "POST", base+"/items/"+itoa(docs.ID)+"/draft", url.Values{"title": {"Write the guide"}, "body": {"All of it"}}, f.zc), 303, "edit draft")
	expectStatus(t, do(t, a, "POST", base+"/items/"+itoa(docs.ID)+"/convert", url.Values{"repo": {itoa(f.pub.ID)}}, f.zc), 303, "convert")
	conv, _ := a.db.ProjectItem(ctx, p.ID, docs.ID)
	if conv.Kind != "issue" || conv.RepoID != f.pub.ID || conv.Title != "Write the guide" || conv.Values[status.ID] != optID(status, "Todo") {
		t.Fatalf("converted item = %+v", conv)
	}
	if is, err := a.db.IssueByNumber(ctx, f.pub.ID, conv.Number); err != nil || is.Body != "All of it" {
		t.Fatalf("converted issue = %+v %v", is, err)
	}

	// Settings, option edits and removal.
	expectStatus(t, do(t, a, "GET", base+"/settings", nil, f.zc), 200, "settings")
	expectStatus(t, do(t, a, "POST", base+"/fields/"+itoa(status.ID)+"/edit",
		url.Values{"opt_" + optID(status, "Todo"): {"Backlog"}, "new": {"Blocked"}, "new_color": {"red"}}, f.zc), 303, "edit status options")
	status, _ = a.db.StatusField(ctx, p.ID)
	if _, ok := status.OptionNamed("Backlog"); !ok || len(status.Options) != 4 {
		t.Errorf("status options = %+v", status.Options)
	}
	expectStatus(t, do(t, a, "POST", base+"/fields/"+itoa(status.ID)+"/delete", url.Values{}, f.zc), 422, "delete Status")
	expectStatus(t, do(t, a, "POST", base+"/items/"+itoa(quiet.ID)+"/remove", url.Values{}, f.zc), 303, "remove")
	if len(f.items(t, p)) != 4 {
		t.Error("item not removed")
	}
	if _, err := a.db.IssueByNumber(ctx, f.demo.ID, quiet.Number); err != nil {
		t.Error("removing the item deleted the issue")
	}

	// The list, the profile tab, the issue sidebar panel.
	mustContain(t, expectStatus(t, do(t, a, "GET", "/projects", nil, f.zc), 200, "list"), "list", "Atelier roadmap", "New project")
	mustContain(t, expectStatus(t, do(t, a, "GET", "/projects?account=zoe", nil, f.zc), 200, "account list"), "account list", "Atelier roadmap", `aria-current="page">Projects`)
	mustContain(t, expectStatus(t, do(t, a, "GET", "/zoe", nil, f.zc), 200, "profile"), "profile", "/projects?account=zoe")
	mustContain(t, expectStatus(t, do(t, a, "GET", "/zoe/demo/issues/"+itoa(bug.Number), nil, f.zc), 200, "issue page"), "issue page",
		"data-proj-panel", "/projects/panel?kind=issue&amp;repo="+itoa(f.demo.ID)+"&amp;number="+itoa(bug.Number))
	panelURL := "/projects/panel?kind=issue&repo=" + itoa(f.demo.ID) + "&number=" + itoa(bug.Number)
	frag := expectStatus(t, do(t, a, "GET", panelURL+"&fragment=1", nil, f.zc), 200, "panel fragment")
	mustContain(t, frag, "panel fragment", "Atelier roadmap", `name="value"`)
	if strings.Contains(frag, "<html") {
		t.Error("fragment has a whole page")
	}
	expectStatus(t, do(t, a, "GET", panelURL, nil, f.zc), 200, "panel page")
	// Add the quiet issue back from its sidebar.
	expectStatus(t, do(t, a, "POST", "/projects/panel", url.Values{"project": {itoa(p.ID)}, "kind": {"issue"},
		"repo": {itoa(f.demo.ID)}, "number": {itoa(quiet.Number)}, "return": {"/zoe/demo/issues/" + itoa(quiet.Number)}}, f.zc), 303, "panel add")
	if len(f.items(t, p)) != 5 {
		t.Error("panel add didn't add the item")
	}

	// Delete needs the name.
	expectStatus(t, do(t, a, "POST", base+"/delete", url.Values{"confirm": {"nope"}}, f.zc), 422, "delete without name")
	expectStatus(t, do(t, a, "POST", base+"/delete", url.Values{"confirm": {"Atelier roadmap"}}, f.zc), 303, "delete")
	expectStatus(t, do(t, a, "GET", base, nil, f.zc), 404, "deleted project")
}

func TestProjectsAutomation(t *testing.T) {
	f := newProjFix(t)
	ctx := context.Background()
	a := f.a
	p := f.newProject(t, "Inbox", false)
	other := f.newProject(t, "Manual", false)
	base := "/projects/" + itoa(p.ID)
	expectStatus(t, do(t, a, "POST", base+"/settings", url.Values{"title": {"Inbox"}, "visibility": {"private"},
		"auto_add": {"1"}, "auto_done": {"1"}, "auto_reopen": {"1"}}, f.zc), 303, "settings")
	expectStatus(t, do(t, a, "POST", base+"/links", url.Values{"repo": {itoa(f.demo.ID)}}, f.zc), 303, "links")
	status, _ := a.db.StatusField(ctx, p.ID)

	// Opening an issue in the linked repository adds it as Todo.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/issues/new", url.Values{"title": {"Auto one"}}, f.zc), 303, "open issue")
	it := f.itemFor(t, p, "Auto one")
	if it.Values[status.ID] != optID(status, "Todo") || it.AddedBy != projAutoActor {
		t.Fatalf("auto-added item = %+v", it)
	}
	if len(f.items(t, other)) != 0 {
		t.Error("an unlinked project got the issue")
	}
	// Not in an unlinked repository.
	expectStatus(t, do(t, a, "POST", "/zoe/open/issues/new", url.Values{"title": {"Elsewhere"}}, f.zc), 303, "open elsewhere")
	if len(f.items(t, p)) != 1 {
		t.Error("an unlinked repository's issue was added")
	}

	// Closed → Done, reopened → Todo.
	issueURL := "/zoe/demo/issues/" + itoa(it.Number)
	expectStatus(t, do(t, a, "POST", issueURL+"/state", url.Values{"action": {"close"}}, f.zc), 303, "close")
	if v := f.itemFor(t, p, "Auto one").Values[status.ID]; v != optID(status, "Done") {
		t.Errorf("after close status = %q", v)
	}
	expectStatus(t, do(t, a, "POST", issueURL+"/state", url.Values{"action": {"reopen"}}, f.zc), 303, "reopen")
	if v := f.itemFor(t, p, "Auto one").Values[status.ID]; v != optID(status, "Todo") {
		t.Errorf("after reopen status = %q", v)
	}

	// A merged pull request is Done, and so is the issue it closes.
	pl, _ := a.db.CreatePull(ctx, store.NewPull{RepoID: f.demo.ID, Title: "Fix it", Body: "Closes #" + itoa(it.Number), Head: "fix", Base: "main", AuthorKind: "owner", AuthorName: "zoe"})
	a.emit(ctx, Event{Kind: EventPullOpened, RepoID: f.demo.ID, Number: pl.Number, Title: pl.Title, Actor: Actor{Kind: ActorOwner, Name: "zoe"}})
	if v := f.itemFor(t, p, "Fix it").Values[status.ID]; v != optID(status, "Todo") {
		t.Errorf("auto-added PR status = %q", v)
	}
	a.emit(ctx, Event{Kind: EventPullMerged, RepoID: f.demo.ID, Number: pl.Number, Title: pl.Title, Actor: Actor{Kind: ActorOwner, Name: "zoe"}})
	if v := f.itemFor(t, p, "Fix it").Values[status.ID]; v != optID(status, "Done") {
		t.Errorf("merged PR status = %q", v)
	}
	if v := f.itemFor(t, p, "Auto one").Values[status.ID]; v != optID(status, "Done") {
		t.Errorf("issue closed by the PR: status = %q", v)
	}

	// Turned off: nothing happens.
	expectStatus(t, do(t, a, "POST", base+"/settings", url.Values{"title": {"Inbox"}, "visibility": {"private"}}, f.zc), 303, "automation off")
	expectStatus(t, do(t, a, "POST", issueURL+"/state", url.Values{"action": {"reopen"}}, f.zc), 303, "reopen again")
	if v := f.itemFor(t, p, "Auto one").Values[status.ID]; v != optID(status, "Done") {
		t.Errorf("automation off but status changed to %q", v)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/issues/new", url.Values{"title": {"Auto two"}}, f.zc), 303, "open issue 2")
	for _, it := range f.items(t, p) {
		if it.Title == "Auto two" {
			t.Error("auto-add is off but the issue was added")
		}
	}

	// A converted draft isn't added twice by auto-add.
	expectStatus(t, do(t, a, "POST", base+"/settings", url.Values{"title": {"Inbox"}, "visibility": {"private"}, "auto_add": {"1"}}, f.zc), 303, "auto-add on")
	d, _ := a.db.AddDraft(ctx, p.ID, "From a draft", "", "owner", "zoe")
	before := len(f.items(t, p))
	expectStatus(t, do(t, a, "POST", base+"/items/"+itoa(d.ID)+"/convert", url.Values{"repo": {itoa(f.demo.ID)}}, f.zc), 303, "convert")
	if after := len(f.items(t, p)); after != before {
		t.Errorf("convert with auto-add: %d items, want %d", after, before)
	}
}

func TestProjectsPermissions(t *testing.T) {
	f := newProjFix(t)
	ctx := context.Background()
	a := f.a
	secret := f.issue(t, f.demo, "Secret plan", "confidential")
	open := f.issue(t, f.pub, "Public bug", "bug")

	priv := f.newProject(t, "Private board", false)
	pub := f.newProject(t, "Shared board", true)
	for _, p := range []store.Project{priv, pub} {
		for _, ref := range []string{"zoe/demo#" + itoa(secret.Number), "zoe/open#" + itoa(open.Number)} {
			expectStatus(t, do(t, a, "POST", "/projects/"+itoa(p.ID)+"/items", url.Values{"ref": {ref}}, f.zc), 303, "add "+ref)
		}
	}
	secretItem := f.itemFor(t, pub, "Secret plan")
	openItem := f.itemFor(t, pub, "Public bug")
	status, _ := a.db.StatusField(ctx, pub.ID)
	_ = a.db.SetItemValue(ctx, pub.ID, secretItem.ID, status.ID, optID(status, "In progress"))

	// sam can't see the private project at all.
	pb := "/projects/" + itoa(priv.ID)
	for _, path := range []string{pb, pb + "?layout=table", pb + "/settings", pb + "/items/" + itoa(f.itemFor(t, priv, "Public bug").ID)} {
		expectStatus(t, do(t, a, "GET", path, nil, f.sc), 404, "sam GET "+path)
	}
	for _, path := range []string{pb + "/items", pb + "/settings", pb + "/delete", pb + "/fields", pb + "/views",
		pb + "/items/" + itoa(f.itemFor(t, priv, "Public bug").ID) + "/move"} {
		expectStatus(t, do(t, a, "POST", path, url.Values{"title": {"x"}, "confirm": {"Private board"}, "name": {"x"}, "kind": {"text"}}, f.sc), 404, "sam POST "+path)
	}
	list := expectStatus(t, do(t, a, "GET", "/projects", nil, f.sc), 200, "sam list")
	if strings.Contains(list, "Private board") || strings.Contains(list, "New project") {
		t.Error("sam's list shows the private project or a New button")
	}
	// …and can't create projects in zoe's account (or anywhere, in core).
	expectStatus(t, do(t, a, "GET", "/projects/new", nil, f.sc), 403, "sam new page")
	expectStatus(t, do(t, a, "POST", "/projects/new", url.Values{"account": {"zoe"}, "title": {"Mine now"}}, f.sc), 403, "sam create")
	expectStatus(t, do(t, a, "POST", "/projects/new", url.Values{"account": {"sam"}, "title": {"Mine"}}, f.sc), 403, "sam create own")

	// The public project: sam sees it, but items from repositories sam can't
	// read are "Private item", with nothing of their title, repo or labels.
	ub := "/projects/" + itoa(pub.ID)
	for _, path := range []string{ub, ub + "?layout=table", ub + "?layout=roadmap"} {
		page := expectStatus(t, do(t, a, "GET", path, nil, f.sc), 200, "sam "+path)
		mustContain(t, page, path, "Shared board", "Public bug", "Private item")
		for _, leak := range []string{"Secret plan", "zoe/demo", "confidential", "/zoe/demo/labels.css", `draggable="true"`, "/move"} {
			if strings.Contains(page, leak) {
				t.Errorf("sam %s shows %q", path, leak)
			}
		}
	}
	// Filters can't probe hidden items: content filters never match them.
	for _, q := range []string{"Secret", "repo:demo", "label:confidential", "is:open", "-label:nothing", "plan"} {
		page := expectStatus(t, do(t, a, "GET", ub+"?q="+url.QueryEscape(q), nil, f.sc), 200, "sam filter "+q)
		if strings.Contains(page, "Private item") || strings.Contains(page, "Secret plan") {
			t.Errorf("filter %q matched the hidden item", q)
		}
	}
	// Project fields are the project's, so they still filter.
	page := expectStatus(t, do(t, a, "GET", ub+"?q="+url.QueryEscape(`status:"In progress"`), nil, f.sc), 200, "sam status filter")
	if !strings.Contains(page, "Private item") || strings.Contains(page, "Public bug") {
		t.Error("status filter on the public project is wrong for sam")
	}
	// Reading isn't writing: sam can't change the public project.
	for _, path := range []string{ub + "/items", ub + "/items/" + itoa(openItem.ID) + "/move", ub + "/items/" + itoa(openItem.ID) + "/field",
		ub + "/items/" + itoa(openItem.ID) + "/remove", ub + "/views", ub + "/settings", ub + "/fields", ub + "/delete"} {
		expectStatus(t, do(t, a, "POST", path, url.Values{"status": {""}, "field": {itoa(status.ID)}, "value": {""}, "ref": {"zoe/open#1"},
			"name": {"x"}, "layout": {"board"}, "title": {"x"}, "confirm": {"Shared board"}}, f.sc), 403, "sam POST "+path)
	}
	expectStatus(t, do(t, a, "GET", ub+"/settings", nil, f.sc), 403, "sam settings")
	expectStatus(t, do(t, a, "GET", ub+"/items/"+itoa(secretItem.ID), nil, f.sc), 404, "sam hidden item page")
	expectStatus(t, do(t, a, "GET", ub+"/items/"+itoa(openItem.ID), nil, f.sc), 200, "sam visible item page")
	if n := len(f.items(t, pub)); n != 2 {
		t.Errorf("sam changed the project: %d items", n)
	}

	// The sidebar panel: hidden repositories are 404; on a public issue sam sees
	// only projects sam can read, and gets no add or status controls.
	expectStatus(t, do(t, a, "GET", "/projects/panel?kind=issue&repo="+itoa(f.demo.ID)+"&number="+itoa(secret.Number), nil, f.sc), 404, "sam panel private repo")
	frag := expectStatus(t, do(t, a, "GET", "/projects/panel?fragment=1&kind=issue&repo="+itoa(f.pub.ID)+"&number="+itoa(open.Number), nil, f.sc), 200, "sam panel")
	if !strings.Contains(frag, "Shared board") || strings.Contains(frag, "Private board") || strings.Contains(frag, "<select") {
		t.Errorf("sam's panel = %s", frag)
	}
	expectStatus(t, do(t, a, "POST", "/projects/panel", url.Values{"project": {itoa(priv.ID)}, "kind": {"issue"},
		"repo": {itoa(f.pub.ID)}, "number": {itoa(open.Number)}}, f.sc), 404, "sam panel add to private")

	// References to repositories that don't exist (or you can't read) are refused.
	expectStatus(t, do(t, a, "POST", "/projects/"+itoa(pub.ID)+"/items", url.Values{"ref": {"sam/none#1"}}, f.zc), 422, "unknown repo")

	// Tokens never manage projects.
	if a.canProject(ctx, Actor{Kind: ActorToken, PersonID: f.zoe.ID, Scopes: tokenScopes["write"]}, pub, PermRead) {
		t.Error("a token can read projects")
	}
}

func TestProjectFilterParse(t *testing.T) {
	fields := []store.ProjField{
		{ID: 1, Name: "Status", Kind: store.ProjFieldSelect, Builtin: "status", Options: []store.ProjOption{{ID: 10, Name: "Todo"}, {ID: 11, Name: "In progress"}}},
		{ID: 2, Name: "Target date", Kind: store.ProjFieldDate},
		{ID: 3, Name: "Notes", Kind: store.ProjFieldText},
	}
	it := store.ProjItem{Kind: "issue", RepoName: "Marbl", RepoOwner: "zoe", Title: "Fix the crash", State: "open",
		Labels: []store.Label{{Name: "bug"}}, Values: map[int64]string{1: "11", 2: "2026-12-01", 3: "needs design"}}
	cases := map[string]bool{
		``:                             true,
		`status:"In progress"`:         true,
		`status:Todo`:                  false,
		`status:Todo,"In progress"`:    true,
		`-status:Todo`:                 true,
		`repo:Marbl is:open label:bug`: true,
		`repo:zoe/marbl`:               true,
		`repo:other`:                   false,
		`is:closed`:                    false,
		`is:pr`:                        false,
		`is:issue`:                     true,
		`label:BUG`:                    true,
		`-label:bug`:                   false,
		`no:label`:                     false,
		`no:status`:                    false,
		`target-date:>2026-11-30`:      true,
		`target-date:<2026-11-30`:      false,
		`"target date":2026-12-01`:     true,
		`notes:design`:                 true,
		`crash`:                        true,
		`fix crash`:                    true,
		`crash missing`:                false,
		`unknown:thing`:                false,
	}
	for q, want := range cases {
		if got := parseProjFilter(q).match(it, true, fields); got != want {
			t.Errorf("%q: match = %v, want %v", q, got, want)
		}
	}
	// A hidden item only matches project-field filters.
	for q, want := range map[string]bool{`status:"In progress"`: true, `no:status`: false, `crash`: false, `-label:x`: false, `is:open`: false, ``: true} {
		if got := parseProjFilter(q).match(it, false, fields); got != want {
			t.Errorf("hidden %q: match = %v, want %v", q, got, want)
		}
	}
}
