package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
)

func TestProjectsCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := issueRepo(t, s)
	is1, _ := s.CreateIssue(ctx, r.ID, "First", "", "owner", "zoe")
	pl, err := s.CreatePull(ctx, NewPull{RepoID: r.ID, Title: "A change", Head: "feat", Base: "main", AuthorKind: "owner", AuthorName: "zoe"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateProject(ctx, NewProject{OwnerID: r.OwnerID, Title: "   "}); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("blank title = %v", err)
	}
	p, err := s.CreateProject(ctx, NewProject{OwnerID: r.OwnerID, Title: " Roadmap ", Private: true, ActorKind: "owner", ActorName: "zoe"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Roadmap" || !p.Private || p.Owner != "zoe" || !p.AutoDone || !p.AutoReopen || p.AutoAdd {
		t.Fatalf("project = %+v", p)
	}
	fields, _ := s.ProjectFields(ctx, p.ID)
	if len(fields) != 1 || !fields[0].IsStatus() || len(fields[0].Options) != 3 || fields[0].Options[1].Name != "In progress" {
		t.Fatalf("default fields = %+v", fields)
	}
	views, _ := s.ProjectViews(ctx, p.ID)
	if len(views) != 1 || views[0].Layout != "board" {
		t.Fatalf("default views = %+v", views)
	}
	status := fields[0]

	// Items: an issue, a pull request (added twice) and a draft.
	a, added, err := s.AddItem(ctx, p.ID, "issue", r.ID, is1.Number, "owner", "zoe")
	if err != nil || !added || a.Title != "First" || a.State != "open" || a.RepoFullName() != "zoe/demo" {
		t.Fatalf("AddItem issue = %+v %v %v", a, added, err)
	}
	b, _, _ := s.AddItem(ctx, p.ID, "pull", r.ID, pl.Number, "owner", "zoe")
	if again, added, _ := s.AddItem(ctx, p.ID, "pull", r.ID, pl.Number, "owner", "zoe"); added || again.ID != b.ID {
		t.Fatalf("second AddItem = %+v added=%v", again, added)
	}
	if _, _, err := s.AddItem(ctx, p.ID, "draft", r.ID, 1, "", ""); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("AddItem draft kind = %v", err)
	}
	c, err := s.AddDraft(ctx, p.ID, "Sketch idea", "body", "owner", "zoe")
	if err != nil || !c.IsDraft() || c.State != "draft" {
		t.Fatalf("AddDraft = %+v %v", c, err)
	}
	if p2, _ := s.ProjectByID(ctx, p.ID); p2.Items != 3 {
		t.Fatalf("Items = %d", p2.Items)
	}

	// Values are checked against their field.
	todo := strconv.FormatInt(status.Options[0].ID, 10)
	if err := s.SetItemValue(ctx, p.ID, a.ID, status.ID, todo); err != nil {
		t.Fatal(err)
	}
	if err := s.SetItemValue(ctx, p.ID, a.ID, status.ID, "999999"); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("bad option = %v", err)
	}
	if err := s.SetItemValue(ctx, p.ID, a.ID, status.ID, "done"); err != nil { // by name
		t.Fatal(err)
	}
	est, err := s.CreateField(ctx, p.ID, "Estimate", ProjFieldNumber, nil)
	if err != nil {
		t.Fatal(err)
	}
	due, _ := s.CreateField(ctx, p.ID, "Target date", ProjFieldDate, nil)
	phase, _ := s.CreateField(ctx, p.ID, "Phase", ProjFieldSelect, []string{"One", "Two", "one"})
	if len(phase.Options) != 2 {
		t.Fatalf("phase options = %+v", phase.Options)
	}
	for _, bad := range []string{"Title", "Status", "Estimate", "a:b", ""} {
		if _, err := s.CreateField(ctx, p.ID, bad, ProjFieldText, nil); !errors.Is(err, ErrProjInvalid) {
			t.Errorf("CreateField(%q) = %v", bad, err)
		}
	}
	if err := s.SetItemValue(ctx, p.ID, a.ID, est.ID, "3.50"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetItemValue(ctx, p.ID, a.ID, est.ID, "lots"); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("number = %v", err)
	}
	if err := s.SetItemValue(ctx, p.ID, a.ID, due.ID, "2026-13-01"); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("date = %v", err)
	}
	_ = s.SetItemValue(ctx, p.ID, a.ID, due.ID, "2026-12-01")
	got, _ := s.ProjectItem(ctx, p.ID, a.ID)
	if got.Values[est.ID] != "3.5" || got.Values[due.ID] != "2026-12-01" || got.Values[status.ID] != strconv.FormatInt(status.Options[2].ID, 10) {
		t.Fatalf("values = %+v", got.Values)
	}
	// Clearing.
	_ = s.SetItemValue(ctx, p.ID, a.ID, est.ID, "")
	if got, _ := s.ProjectItem(ctx, p.ID, a.ID); got.Values[est.ID] != "" {
		t.Fatalf("cleared value = %q", got.Values[est.ID])
	}
	// An item of another project can't be changed through this one.
	other, _ := s.CreateProject(ctx, NewProject{OwnerID: r.OwnerID, Title: "Other"})
	if err := s.SetItemValue(ctx, other.ID, a.ID, status.ID, todo); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project SetItemValue = %v", err)
	}
	if err := s.RemoveItem(ctx, other.ID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project RemoveItem = %v", err)
	}

	// Options: rename, recolour, delete (clears values), add.
	done := status.Options[2]
	err = s.EditOptions(ctx, p.ID, status.ID, []OptionEdit{
		{ID: status.Options[0].ID, Name: "Backlog", Color: "blue"},
		{ID: status.Options[1].ID, Name: "Doing", Color: "amber"},
		{ID: done.ID, Delete: true},
		{Name: "Shipped", Color: "green"},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _ = s.StatusField(ctx, p.ID)
	if len(status.Options) != 3 || status.Options[0].Name != "Backlog" || status.Options[0].Color != "blue" || status.Options[2].Name != "Shipped" {
		t.Fatalf("options after edit = %+v", status.Options)
	}
	if got, _ := s.ProjectItem(ctx, p.ID, a.ID); got.Values[status.ID] != "" {
		t.Fatalf("value of a deleted option kept: %q", got.Values[status.ID])
	}
	if err := s.EditOptions(ctx, p.ID, status.ID, []OptionEdit{{ID: status.Options[0].ID, Name: "Doing"}, {ID: status.Options[1].ID, Name: "Doing"}}); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("duplicate option names = %v", err)
	}
	if err := s.DeleteField(ctx, p.ID, status.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting Status = %v", err)
	}
	if err := s.RenameField(ctx, p.ID, est.ID, "Points"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteField(ctx, p.ID, phase.ID); err != nil {
		t.Fatal(err)
	}

	// Drafts convert to issues and keep their place.
	is2, _ := s.CreateIssue(ctx, r.ID, "Sketch idea", "body", "owner", "zoe")
	if err := s.ConvertDraft(ctx, p.ID, c.ID, r.ID, is2.Number); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ProjectItem(ctx, p.ID, c.ID); got.Kind != "issue" || got.Number != is2.Number || got.Title != "Sketch idea" {
		t.Fatalf("converted = %+v", got)
	}
	if refs, _ := s.ItemsFor(ctx, "issue", r.ID, is2.Number); len(refs) != 1 || refs[0].ItemID != c.ID {
		t.Fatalf("ItemsFor = %+v", refs)
	}

	// Views and links.
	v, err := s.SaveView(ctx, ProjView{ProjectID: p.ID, Name: "Bugs", Layout: "table", Filter: "label:bug"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveView(ctx, ProjView{ProjectID: p.ID, Name: "Bad", Layout: "gantt"}); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("bad layout = %v", err)
	}
	v.Filter = "label:bug is:open"
	if _, err := s.SaveView(ctx, v); err != nil {
		t.Fatal(err)
	}
	views, _ = s.ProjectViews(ctx, p.ID)
	if len(views) != 2 || views[1].Filter != "label:bug is:open" {
		t.Fatalf("views = %+v", views)
	}
	_ = s.LinkProjectRepo(ctx, p.ID, r.ID)
	_ = s.LinkProjectRepo(ctx, p.ID, r.ID)
	if ids, _ := s.ProjectsLinkedTo(ctx, r.ID); len(ids) != 1 || ids[0] != p.ID {
		t.Fatalf("linked = %v", ids)
	}

	// Deleting the repository takes its items and links with it.
	if err := s.DeleteRepo(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	items, _ := s.ProjectItems(ctx, p.ID)
	if len(items) != 0 {
		t.Fatalf("items after repo delete = %+v", items)
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProjectByID(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted project = %v", err)
	}
}

func TestProjectMoveItem(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := issueRepo(t, s)
	p, _ := s.CreateProject(ctx, NewProject{OwnerID: r.OwnerID, Title: "Board"})
	status, _ := s.StatusField(ctx, p.ID)
	var ids []int64
	for _, title := range []string{"a", "b", "c", "d"} {
		it, err := s.AddDraft(ctx, p.ID, title, "", "owner", "zoe")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, it.ID)
	}
	order := func() string {
		items, _ := s.ProjectItems(ctx, p.ID)
		out := ""
		for _, it := range items {
			out += it.Title
		}
		return out
	}
	if err := s.MoveItem(ctx, p.ID, ids[3], ids[0], 0, ""); err != nil {
		t.Fatal(err)
	}
	if got := order(); got != "dabc" {
		t.Fatalf("move d before a = %s", got)
	}
	inProgress := strconv.FormatInt(status.Options[1].ID, 10)
	if err := s.MoveItem(ctx, p.ID, ids[3], 0, status.ID, inProgress); err != nil {
		t.Fatal(err)
	}
	if got := order(); got != "abcd" {
		t.Fatalf("move d to end = %s", got)
	}
	if it, _ := s.ProjectItem(ctx, p.ID, ids[3]); it.Values[status.ID] != inProgress {
		t.Fatalf("status after move = %q", it.Values[status.ID])
	}
	if err := s.MoveItem(ctx, p.ID, ids[0], 0, status.ID, "nope"); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("bad status move = %v", err)
	}
	if err := s.MoveItem(ctx, p.ID, 9999, 0, 0, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing item move = %v", err)
	}
	if got := order(); got != "abcd" {
		t.Fatalf("failed moves changed order: %s", got)
	}
}
