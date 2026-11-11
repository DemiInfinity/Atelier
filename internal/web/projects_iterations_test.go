package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
)

func iterField(t *testing.T, a *App, p store.Project, name string) store.ProjField {
	t.Helper()
	fields, err := a.db.ProjectFields(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if f.Name == name && f.IsIteration() {
			return f
		}
	}
	t.Fatalf("no iteration field %q in %+v", name, fields)
	return store.ProjField{}
}

func TestProjectsIterationField(t *testing.T) {
	f := newProjFix(t)
	a := f.a
	ctx := context.Background()
	p := f.newProject(t, "Sprints", false)
	base := "/projects/" + itoa(p.ID)
	start := time.Now().AddDate(0, 0, -7).Format("2006-01-02")

	// Settings offer the kind; bad lengths are refused.
	page := expectStatus(t, do(t, a, "GET", base+"/settings", nil, f.zc), 200, "settings")
	mustContain(t, page, "settings", `value="iteration"`, "Iteration length")
	expectStatus(t, do(t, a, "POST", base+"/fields", url.Values{"name": {"Sprint"}, "kind": {"iteration"}, "duration": {"60"}, "duration_unit": {"weeks"}}, f.zc), 422, "61-week iterations")
	rec := do(t, a, "POST", base+"/fields", url.Values{"name": {"Sprint"}, "kind": {"iteration"}, "duration": {"1"}, "duration_unit": {"weeks"},
		"start": {start}, "options": {"Sprint 1\nSprint 2\nSprint 3"}}, f.zc)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create iteration field = %d\n%s", rec.Code, rec.Body)
	}
	sf := iterField(t, a, p, "Sprint")
	if sf.Duration != 7 || len(sf.Iterations) != 3 || !sf.Iterations[1].Current() {
		t.Fatalf("iteration field = %+v", sf)
	}
	page = expectStatus(t, do(t, a, "GET", base+"/settings", nil, f.zc), 200, "settings with iterations")
	mustContain(t, page, "settings with iterations", "Sprint 2", `name="it_start_`, "Current")

	// Three drafts: one in the current sprint, one in the next, one unset.
	for _, title := range []string{"Now", "Soon", "Someday"} {
		if rec := do(t, a, "POST", base+"/items", url.Values{"title": {title}}, f.zc); rec.Code != http.StatusSeeOther {
			t.Fatalf("add %s = %d", title, rec.Code)
		}
	}
	setIt := func(title, value string) {
		t.Helper()
		it := f.itemFor(t, p, title)
		rec := do(t, a, "POST", base+"/items/"+itoa(it.ID)+"/field", url.Values{"field": {itoa(sf.ID)}, "value": {value}}, f.zc)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("set %s = %d\n%s", title, rec.Code, rec.Body)
		}
	}
	setIt("Now", strconv.FormatInt(sf.Iterations[1].ID, 10))
	setIt("Soon", strconv.FormatInt(sf.Iterations[2].ID, 10))

	// Board cards and table cells show the iteration.
	board := expectStatus(t, do(t, a, "GET", base, nil, f.zc), 200, "board")
	mustContain(t, board, "board", "proj-iter is-current", ">Sprint 2", "Sprint 3")
	table := expectStatus(t, do(t, a, "GET", base+"?layout=table", nil, f.zc), 200, "table")
	mustContain(t, table, "table", ">Sprint<", "Sprint 2 (", " · current")

	// Filters: @current, @next, a name, no:, and the generic iteration: key
	// works whatever the field is called.
	for q, want := range map[string][]string{
		"iteration:@current":  {"Now"},
		"sprint:@current":     {"Now"},
		"iteration:@next":     {"Soon"},
		`sprint:"Sprint 3"`:   {"Soon"},
		"-iteration:@current": {"Soon", "Someday"},
		"no:sprint":           {"Someday"},
		"iteration:@previous": {},
	} {
		body := expectStatus(t, do(t, a, "GET", base+"?layout=table&q="+url.QueryEscape(q), nil, f.zc), 200, q)
		for _, title := range []string{"Now", "Soon", "Someday"} {
			has := strings.Contains(body, ">"+title+"</a>")
			wanted := false
			for _, w := range want {
				wanted = wanted || w == title
			}
			if has != wanted {
				t.Errorf("filter %s: %s shown = %v, want %v", q, title, has, wanted)
			}
		}
	}

	// Sorting the table by the iteration orders by start date.
	sorted := expectStatus(t, do(t, a, "GET", base+"?layout=table&sort=f"+itoa(sf.ID)+":desc", nil, f.zc), 200, "sorted")
	if i, j := strings.Index(sorted, ">Soon</a>"), strings.Index(sorted, ">Now</a>"); i < 0 || j < 0 || i > j {
		t.Errorf("desc sort by iteration: Soon at %d, Now at %d", i, j)
	}

	// Editing: rename, add one after the last, remove one (clearing values).
	edit := url.Values{"duration": {"10"}, "duration_unit": {"days"}, "new_iter": {"Cooldown"}}
	for _, it := range sf.Iterations {
		k := itoa(it.ID)
		edit.Set("it_"+k, it.Name)
		edit.Set("it_start_"+k, it.StartDate())
		edit.Set("it_days_"+k, strconv.Itoa(it.Days))
	}
	edit.Set("it_"+itoa(sf.Iterations[0].ID), "Kickoff")
	edit.Set("it_del_"+itoa(sf.Iterations[2].ID), "1")
	if rec := do(t, a, "POST", base+"/fields/"+itoa(sf.ID)+"/edit", edit, f.zc); rec.Code != http.StatusSeeOther {
		t.Fatalf("edit iterations = %d\n%s", rec.Code, rec.Body)
	}
	sf2 := iterField(t, a, p, "Sprint")
	var names []string
	for _, it := range sf2.Iterations {
		names = append(names, it.Name)
	}
	if sf2.Duration != 10 || strings.Join(names, ",") != "Kickoff,Sprint 2,Cooldown" {
		t.Fatalf("after edit: %d days, %v", sf2.Duration, names)
	}
	if v := f.itemFor(t, p, "Soon").Values[sf.ID]; v != "" {
		t.Errorf("Soon still points at a removed iteration: %q", v)
	}
	bad := url.Values{"it_" + itoa(sf2.Iterations[0].ID): {"Sprint 2"}}
	expectStatus(t, do(t, a, "POST", base+"/fields/"+itoa(sf.ID)+"/edit", bad, f.zc), 422, "duplicate iteration name")

	// A reader who can't write the project can't edit iterations.
	if rec := do(t, a, "POST", base+"/fields/"+itoa(sf.ID)+"/edit", edit, f.sc); rec.Code == http.StatusSeeOther {
		t.Errorf("sam edited zoe's iterations: %d", rec.Code)
	}
	_ = ctx
}
