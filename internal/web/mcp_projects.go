package web

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"

	"atelier/internal/store"
)

// MCP tools for Projects (planning boards).
//
// They follow the web Projects feature: a project is judged by a.can on its
// account stand-in (projRepo) — read for projects.list / get / items, write
// for item changes — and each issue or pull request on it is shown only to
// callers who can read its repository; everyone else gets {"private": true}
// with the item's id and the project's own field values, like the board's
// "Private item". Project settings, field definitions, links, views and
// deletion stay in the browser.
//
// Agentic jobs can't use a.can on projects (job tokens only read their own
// repository), so a job reaches a project only when the project links the
// job's repository and the workflow listed the tool; that per-tool grant is
// what lets it change items, as with the repository write tools.

func mcpProjectTools() []*mcpTool {
	return []*mcpTool{
		mcpDef(mcpTool{Name: "projects.list", Title: "List projects",
			Desc: "List the projects (planning boards) you can see, open ones first, optionally only one account's or only open/closed ones. Use the id it returns as the project argument of the other projects tools."},
			mcpProjectsList),
		mcpDef(mcpTool{Name: "projects.get", Title: "Project details",
			Desc: "Get a project's fields (with the options of single-select fields such as Status), saved views and how many items are in each Status."},
			mcpProjectsGet),
		mcpDef(mcpTool{Name: "projects.items", Title: "List project items",
			Desc: `List a project's items in board order with their field values. filter uses the board's syntax: status:"In progress" repo:name label:bug is:open|closed|merged|draft|issue|pr no:status estimate:>3, -qualifier to negate, other words match the title. Items from repositories you can't read come back as {"private": true} with only their id and field values.`},
			mcpProjectsItems),
		mcpDef(mcpTool{Name: "projects.add_item", Title: "Add to a project", Write: true,
			Desc: "Add an issue or pull request to a project by reference (owner/repo#12 or its link), optionally setting its Status by option name. Adding something already on the project returns the existing item."},
			mcpProjectsAddItem),
		mcpDef(mcpTool{Name: "projects.add_draft", Title: "Add a draft card", Write: true,
			Desc: "Add a draft card (a title and an optional Markdown body) to a project, optionally setting its Status by option name."},
			mcpProjectsAddDraft),
		mcpDef(mcpTool{Name: "projects.set_field", Title: "Set a project field", Write: true,
			Desc: "Set one field of a project item: Status or any custom field, by field name. Single-select values are option names; numbers are numbers; dates are YYYY-MM-DD; an empty value clears the field. projects.get lists the fields and options."},
			mcpProjectsSetField),
		mcpDef(mcpTool{Name: "projects.move_item", Title: "Move a project item", Write: true,
			Desc: "Reorder an item: put it before or after another item (neither = the end). Pass status (an option name, or empty for No status) to move it to another board column at the same time."},
			mcpProjectsMoveItem),
		mcpDef(mcpTool{Name: "projects.remove_item", Title: "Remove from a project", Write: true,
			Desc: "Take an item off a project. The issue or pull request itself is not changed; a draft card is gone."},
			mcpProjectsRemoveItem),
	}
}

// ---------- access ----------

// mcpCanProject decides whether who may use a project with need.
func (a *App) mcpCanProject(ctx context.Context, who Actor, p store.Project, need Perm) bool {
	switch who.Kind {
	case ActorToken:
		return a.can(ctx, who, projRepo(p), need)
	case ActorJob:
		jr := jobRepoID(who)
		if jr == 0 {
			return false
		}
		rp, err := a.db.RepoByID(ctx, jr)
		if err != nil || !a.can(ctx, who, rp, PermRead) {
			return false
		}
		links, err := a.db.ProjectLinks(ctx, p.ID)
		return err == nil && containsID(links, jr)
	}
	return false
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// project loads a project for the call: not found unless the caller can read
// it, and an error unless it can change it when the tool writes.
func (c *mcpCall) project(ctx context.Context, id int64) (store.Project, error) {
	c.target = "project:" + strconv.FormatInt(id, 10)
	p, err := c.a.db.ProjectByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Project{}, errMCPNotFound
	}
	if err != nil {
		return store.Project{}, err
	}
	if !c.a.mcpCanProject(ctx, c.who, p, PermRead) {
		return store.Project{}, errMCPNotFound
	}
	if c.tool != nil && c.tool.Write && !c.a.mcpCanProject(ctx, c.who, p, PermWrite) {
		return store.Project{}, userErr("you can see project %d but not change it", p.ID)
	}
	return p, nil
}

// item loads one item of p that the caller can see (hidden items are not found).
func (c *mcpCall) item(ctx context.Context, p store.Project, id int64) (store.ProjItem, error) {
	c.target = "project:" + strconv.FormatInt(p.ID, 10) + " item:" + strconv.FormatInt(id, 10)
	it, err := c.a.db.ProjectItem(ctx, p.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.ProjItem{}, userErr("project %d has no item %d you can see", p.ID, id)
	}
	if err != nil {
		return store.ProjItem{}, err
	}
	if !c.a.projItemVisible(ctx, c.who, it) {
		return store.ProjItem{}, userErr("project %d has no item %d you can see", p.ID, id)
	}
	return it, nil
}

// ---------- output ----------

type mcpProjectOut struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Account     string `json:"account"`
	Private     bool   `json:"private"`
	Closed      bool   `json:"closed"`
	Items       int    `json:"items"`
	UpdatedAt   string `json:"updated_at"`
	URL         string `json:"web_path"`
}

func projectOut(p store.Project) mcpProjectOut {
	return mcpProjectOut{ID: p.ID, Title: p.Title, Description: p.Description, Account: p.Owner, Private: p.Private,
		Closed: p.Closed, Items: p.Items, UpdatedAt: timeStr(p.UpdatedAt), URL: projURL(p)}
}

type mcpProjItemOut struct {
	ID      int64             `json:"id"`
	Private bool              `json:"private,omitempty"`
	Kind    string            `json:"kind,omitempty"` // issue, pull, draft
	Title   string            `json:"title,omitempty"`
	Repo    string            `json:"repo,omitempty"`
	Number  int64             `json:"number,omitempty"`
	State   string            `json:"state,omitempty"`
	Labels  []string          `json:"labels,omitempty"`
	Status  string            `json:"status,omitempty"`
	Fields  map[string]string `json:"fields,omitempty"` // field name → value (option names for single selects)
	AddedBy string            `json:"added_by,omitempty"`
	URL     string            `json:"web_path,omitempty"`
}

// projItemOut renders an item like the board does: hidden items show only
// their id and the project's own field values.
func projItemOut(p store.Project, it store.ProjItem, visible bool, fields []store.ProjField) mcpProjItemOut {
	c := projCardFor(p, it, visible, fields)
	out := mcpProjItemOut{ID: it.ID, Status: c.Status.Display}
	for _, cell := range c.Cells {
		if cell.Value == "" {
			continue
		}
		if out.Fields == nil {
			out.Fields = map[string]string{}
		}
		v := cell.Display
		if cell.Field.Kind == store.ProjFieldDate {
			v = cell.Value // YYYY-MM-DD, as set_field takes it
		}
		out.Fields[cell.Field.Name] = v
	}
	if c.Hidden {
		out.Private = true
		return out
	}
	out.Kind, out.Title, out.Repo, out.Number, out.URL = it.Kind, c.Title, c.Repo, c.Number, c.URL
	out.State = strings.ToLower(c.StateLabel())
	for _, l := range c.Labels {
		out.Labels = append(out.Labels, l.Name)
	}
	out.AddedBy = authorStr(it.AddedKind, it.AddedBy)
	return out
}

// ---------- projects.list ----------

type mcpProjectsListIn struct {
	Account string `json:"account,omitempty" jsonschema:"only projects of this account (a username or company account name)"`
	State   string `json:"state,omitempty" jsonschema:"open (default), closed or all"`
	mcpPage
}

func mcpProjectsList(ctx context.Context, c *mcpCall, in mcpProjectsListIn) (any, error) {
	state := in.State
	switch state {
	case "":
		state = "open"
	case "open", "closed", "all":
	default:
		return nil, userErr("state must be open, closed or all")
	}
	all, err := c.a.db.Projects(ctx)
	if err != nil {
		return nil, err
	}
	var linked []int64
	if c.who.Kind == ActorJob {
		if linked, err = c.a.db.ProjectsLinkedTo(ctx, jobRepoID(c.who)); err != nil {
			return nil, err
		}
	}
	var ok []mcpProjectOut
	for _, p := range all {
		if in.Account != "" && !strings.EqualFold(p.Owner, in.Account) {
			continue
		}
		if (state == "open" && p.Closed) || (state == "closed" && !p.Closed) {
			continue
		}
		if c.who.Kind == ActorJob && !containsID(linked, p.ID) {
			continue
		}
		if c.a.mcpCanProject(ctx, c.who, p, PermRead) {
			ok = append(ok, projectOut(p))
		}
	}
	items, page, more := pageOf(ok, in.mcpPage)
	return map[string]any{"projects": items, "page": page, "has_more": more}, nil
}

// ---------- projects.get ----------

type mcpProjectIn struct {
	Project int64 `json:"project" jsonschema:"the project id (projects.list shows it)"`
}

func mcpProjectsGet(ctx context.Context, c *mcpCall, in mcpProjectIn) (any, error) {
	p, err := c.project(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	fields, err := c.a.db.ProjectFields(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	views, err := c.a.db.ProjectViews(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	items, err := c.a.db.ProjectItems(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	type optOut struct {
		Name  string `json:"name"`
		Color string `json:"color,omitempty"`
	}
	type fieldOut struct {
		Name    string   `json:"name"`
		Kind    string   `json:"kind"` // single_select, text, number, date
		Builtin bool     `json:"builtin,omitempty"`
		Options []optOut `json:"options,omitempty"`
	}
	type viewOut struct {
		Name   string `json:"name"`
		Layout string `json:"layout"`
		Filter string `json:"filter,omitempty"`
		Sort   string `json:"sort,omitempty"`
	}
	type countOut struct {
		Status string `json:"status"` // "" = No status
		Items  int    `json:"items"`
	}
	var status store.ProjField
	fo := []fieldOut{}
	for _, f := range fields {
		if f.IsStatus() {
			status = f
		}
		o := fieldOut{Name: f.Name, Kind: f.Kind, Builtin: f.IsStatus()}
		for _, opt := range f.Options {
			o.Options = append(o.Options, optOut{Name: opt.Name, Color: opt.Color})
		}
		fo = append(fo, o)
	}
	vo := []viewOut{}
	for _, v := range views {
		vo = append(vo, viewOut{Name: v.Name, Layout: v.Layout, Filter: v.Filter, Sort: v.Sort})
	}
	// Counts per Status, in option order, like the board's columns (hidden
	// items count: the board shows them as "Private item" cards).
	per := map[string]int{}
	for _, it := range items {
		v := it.Values[status.ID]
		if _, ok := status.Option(v); !ok {
			v = ""
		}
		per[v]++
	}
	counts := []countOut{}
	if per[""] > 0 {
		counts = append(counts, countOut{Status: "", Items: per[""]})
	}
	for _, o := range status.Options {
		counts = append(counts, countOut{Status: o.Name, Items: per[strconv.FormatInt(o.ID, 10)]})
	}
	p.Items = len(items)
	return map[string]any{"project": projectOut(p), "fields": fo, "views": vo, "status_counts": counts,
		"can_write": c.a.mcpCanProject(ctx, c.who, p, PermWrite)}, nil
}

// ---------- projects.items ----------

type mcpProjectsItemsIn struct {
	Project int64  `json:"project" jsonschema:"the project id"`
	Filter  string `json:"filter,omitempty" jsonschema:"the board's filter, e.g. status:\"In progress\" repo:website label:bug is:open"`
	mcpPage
}

func mcpProjectsItems(ctx context.Context, c *mcpCall, in mcpProjectsItemsIn) (any, error) {
	p, err := c.project(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	fields, err := c.a.db.ProjectFields(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	items, err := c.a.db.ProjectItems(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	filter := parseProjFilter(strings.TrimSpace(in.Filter))
	out := []mcpProjItemOut{}
	for _, it := range items {
		visible := c.a.projItemVisible(ctx, c.who, it)
		if !filter.match(it, visible, fields) {
			continue
		}
		out = append(out, projItemOut(p, it, visible, fields))
	}
	pageItems, page, more := pageOf(out, in.mcpPage)
	return map[string]any{"items": pageItems, "total": len(out), "page": page, "has_more": more}, nil
}

// ---------- changes ----------

// statusValue checks an optional Status option name against the project.
func (c *mcpCall) statusValue(ctx context.Context, p store.Project, name string) (store.ProjField, string, error) {
	sf, err := c.a.db.StatusField(ctx, p.ID)
	if err != nil {
		return store.ProjField{}, "", err
	}
	v, err := store.NormalizeProjValue(sf, name)
	if err != nil {
		return store.ProjField{}, "", projValueErr(sf)
	}
	return sf, v, nil
}

// projValueErr explains why a value doesn't fit a field. It never repeats the
// value: errors go into the call log, which keeps no arguments.
func projValueErr(f store.ProjField) error {
	switch f.Kind {
	case store.ProjFieldSelect:
		var names []string
		for _, o := range f.Options {
			names = append(names, o.Name)
		}
		return userErr("that isn't an option of %s; its options are: %s", f.Name, strings.Join(names, ", "))
	case store.ProjFieldNumber:
		return userErr("%s is a number field", f.Name)
	case store.ProjFieldDate:
		return userErr("%s is a date field: use YYYY-MM-DD", f.Name)
	}
	return userErr("that value doesn't fit %s (text is up to 1000 characters)", f.Name)
}

func (c *mcpCall) itemResult(ctx context.Context, p store.Project, id int64) (mcpProjItemOut, error) {
	fields, err := c.a.db.ProjectFields(ctx, p.ID)
	if err != nil {
		return mcpProjItemOut{}, err
	}
	it, err := c.a.db.ProjectItem(ctx, p.ID, id)
	if err != nil {
		return mcpProjItemOut{}, err
	}
	return projItemOut(p, it, c.a.projItemVisible(ctx, c.who, it), fields), nil
}

type mcpProjectsAddItemIn struct {
	Project int64  `json:"project" jsonschema:"the project id"`
	Ref     string `json:"ref" jsonschema:"the issue or pull request as owner/repo#12, or its link"`
	Status  string `json:"status,omitempty" jsonschema:"optional Status option name, e.g. Todo"`
}

func mcpProjectsAddItem(ctx context.Context, c *mcpCall, in mcpProjectsAddItemIn) (any, error) {
	p, err := c.project(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	var sf store.ProjField
	var sv string
	if in.Status != "" {
		if sf, sv, err = c.statusValue(ctx, p, in.Status); err != nil {
			return nil, err
		}
	}
	rp, kind, num, err := c.a.projResolveRefAs(ctx, c.who, in.Ref)
	if err != nil {
		return nil, userErr("couldn't find that issue or pull request: use owner/repo#12 or its link")
	}
	who := c.recordAs(ctx)
	it, added, err := c.a.db.AddItem(ctx, p.ID, kind, rp.ID, num, who.Kind, who.Name)
	if err != nil {
		return nil, err
	}
	c.target = "project:" + strconv.FormatInt(p.ID, 10) + " item:" + strconv.FormatInt(it.ID, 10)
	if sv != "" {
		if err := c.a.db.SetItemValue(ctx, p.ID, it.ID, sf.ID, sv); err != nil {
			return nil, err
		}
	}
	out, err := c.itemResult(ctx, p, it.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"item": out, "added": added}, nil
}

type mcpProjectsAddDraftIn struct {
	Project int64  `json:"project" jsonschema:"the project id"`
	Title   string `json:"title" jsonschema:"the card's title (up to 256 characters)"`
	Body    string `json:"body,omitempty" jsonschema:"optional notes, in Markdown"`
	Status  string `json:"status,omitempty" jsonschema:"optional Status option name, e.g. Todo"`
}

func mcpProjectsAddDraft(ctx context.Context, c *mcpCall, in mcpProjectsAddDraftIn) (any, error) {
	p, err := c.project(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(in.Title)
	if !store.ValidProjTitle(title, 256) || strings.ContainsAny(title, "\n\r") {
		return nil, userErr("title must be one line of 1-256 characters")
	}
	body, err := checkBody(in.Body, false)
	if err != nil {
		return nil, err
	}
	var sf store.ProjField
	var sv string
	if in.Status != "" {
		if sf, sv, err = c.statusValue(ctx, p, in.Status); err != nil {
			return nil, err
		}
	}
	who := c.recordAs(ctx)
	it, err := c.a.db.AddDraft(ctx, p.ID, title, body, who.Kind, who.Name)
	if errors.Is(err, store.ErrProjInvalid) {
		return nil, userErr("title must be one line of 1-256 characters")
	}
	if err != nil {
		return nil, err
	}
	c.target = "project:" + strconv.FormatInt(p.ID, 10) + " item:" + strconv.FormatInt(it.ID, 10)
	if sv != "" {
		if err := c.a.db.SetItemValue(ctx, p.ID, it.ID, sf.ID, sv); err != nil {
			return nil, err
		}
	}
	out, err := c.itemResult(ctx, p, it.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"item": out}, nil
}

type mcpProjectsSetFieldIn struct {
	Project int64  `json:"project" jsonschema:"the project id"`
	Item    int64  `json:"item" jsonschema:"the item id (projects.items shows it)"`
	Field   string `json:"field" jsonschema:"the field name, e.g. Status or Priority"`
	Value   string `json:"value" jsonschema:"the new value: an option name, a number, a date as YYYY-MM-DD or text; empty clears the field"`
}

func mcpProjectsSetField(ctx context.Context, c *mcpCall, in mcpProjectsSetFieldIn) (any, error) {
	p, err := c.project(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	it, err := c.item(ctx, p, in.Item)
	if err != nil {
		return nil, err
	}
	fields, err := c.a.db.ProjectFields(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	f, ok := projFindField(fields, projFieldKey(in.Field))
	if !ok {
		var names []string
		for _, f := range fields {
			names = append(names, f.Name)
		}
		sort.Strings(names)
		return nil, userErr("project %d has no field by that name; its fields are: %s", p.ID, strings.Join(names, ", "))
	}
	if _, err := store.NormalizeProjValue(f, in.Value); err != nil {
		return nil, projValueErr(f)
	}
	if err := c.a.db.SetItemValue(ctx, p.ID, it.ID, f.ID, in.Value); err != nil {
		if errors.Is(err, store.ErrProjInvalid) {
			return nil, projValueErr(f)
		}
		return nil, err
	}
	out, err := c.itemResult(ctx, p, it.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"item": out}, nil
}

type mcpProjectsMoveItemIn struct {
	Project int64   `json:"project" jsonschema:"the project id"`
	Item    int64   `json:"item" jsonschema:"the item to move"`
	Before  int64   `json:"before,omitempty" jsonschema:"put it just before this item"`
	After   int64   `json:"after,omitempty" jsonschema:"put it just after this item"`
	Status  *string `json:"status,omitempty" jsonschema:"also move it to this Status column (an option name; empty = No status)"`
}

func mcpProjectsMoveItem(ctx context.Context, c *mcpCall, in mcpProjectsMoveItemIn) (any, error) {
	if in.Before != 0 && in.After != 0 {
		return nil, userErr("give before or after, not both")
	}
	p, err := c.project(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	it, err := c.item(ctx, p, in.Item)
	if err != nil {
		return nil, err
	}
	var statusField int64
	status := ""
	if in.Status != nil {
		sf, sv, err := c.statusValue(ctx, p, *in.Status)
		if err != nil {
			return nil, err
		}
		statusField, status = sf.ID, sv
	}
	items, err := c.a.db.ProjectItems(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	anchor := in.Before
	if anchor == 0 {
		anchor = in.After
	}
	if anchor != 0 {
		if anchor == it.ID {
			return nil, userErr("before / after must be another item")
		}
		if !slicesContainsItem(items, anchor) {
			return nil, userErr("project %d has no item %d", p.ID, anchor)
		}
	}
	before := in.Before
	if in.After != 0 {
		before = projBeforeFromAfter(items, it.ID, in.After)
	}
	err = c.a.db.MoveItem(ctx, p.ID, it.ID, before, statusField, status)
	if errors.Is(err, store.ErrProjInvalid) {
		return nil, userErr("unknown status")
	}
	if err != nil {
		return nil, err
	}
	out, err := c.itemResult(ctx, p, it.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"item": out}, nil
}

func slicesContainsItem(items []store.ProjItem, id int64) bool {
	for _, x := range items {
		if x.ID == id {
			return true
		}
	}
	return false
}

type mcpProjectsRemoveItemIn struct {
	Project int64 `json:"project" jsonschema:"the project id"`
	Item    int64 `json:"item" jsonschema:"the item to take off the project"`
}

func mcpProjectsRemoveItem(ctx context.Context, c *mcpCall, in mcpProjectsRemoveItemIn) (any, error) {
	p, err := c.project(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	// Like the board: only items you can see, so a card you can't read isn't
	// dropped by mistake.
	it, err := c.item(ctx, p, in.Item)
	if err != nil {
		return nil, err
	}
	if err := c.a.db.RemoveItem(ctx, p.ID, it.ID); err != nil {
		return nil, err
	}
	return map[string]any{"removed": it.ID}, nil
}
