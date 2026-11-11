package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestProjectIterationField(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := issueRepo(t, s)
	p, err := s.CreateProject(ctx, NewProject{OwnerID: r.OwnerID, Title: "Sprints", ActorKind: "owner", ActorName: "zoe"})
	if err != nil {
		t.Fatal(err)
	}
	today := projDay(time.Now())
	start := today.AddDate(0, 0, -7) // the second iteration is running today

	if _, err := s.CreateIterationField(ctx, p.ID, NewIterationField{Name: "Sprint", Days: 400}); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("400-day iterations = %v", err)
	}
	if _, err := s.CreateIterationField(ctx, p.ID, NewIterationField{Name: "Sprint", Lines: []string{"A", "a"}}); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("duplicate iteration names = %v", err)
	}
	f, err := s.CreateIterationField(ctx, p.ID, NewIterationField{Name: "Sprint", Days: 7, Start: start,
		Lines: []string{"Sprint 1", "Sprint 2", "Sprint 3", "Later " + today.AddDate(0, 3, 0).Format(projDate)}})
	if err != nil {
		t.Fatal(err)
	}
	if !f.IsIteration() || f.KindLabel() != "Iteration" || f.Duration != 7 || len(f.Iterations) != 4 {
		t.Fatalf("field = %+v", f)
	}
	if got := f.Iterations[1]; got.Name != "Sprint 2" || !got.Start.Equal(start.AddDate(0, 0, 7)) || got.Days != 7 || !got.Current() {
		t.Fatalf("Sprint 2 = %+v (today %s)", got, today)
	}
	if got := f.Iterations[3]; got.Name != "Later" || !got.Start.Equal(today.AddDate(0, 3, 0)) {
		t.Fatalf("dated line = %+v", got)
	}
	for ref, want := range map[string]string{"@current": "Sprint 2", "@next": "Sprint 3", "@previous": "Sprint 1", "@CURRENT": "Sprint 2"} {
		if it, ok := f.IterationAt(ref, time.Now()); !ok || it.Name != want {
			t.Errorf("%s = %+v %v, want %s", ref, it, ok, want)
		}
	}

	// CreateField with the iteration kind makes three default iterations.
	g, err := s.CreateField(ctx, p.ID, "Cycle", ProjFieldIteration, nil)
	if err != nil || !g.IsIteration() || len(g.Iterations) != 3 || g.Duration != 14 || g.Iterations[2].Name != "Iteration 3" {
		t.Fatalf("CreateField iteration = %+v %v", g, err)
	}

	// Values: by name (any case), by id, @current; unknown names are refused.
	it, _ := s.AddDraft(ctx, p.ID, "Card", "", "owner", "zoe")
	if err := s.SetItemValue(ctx, p.ID, it.ID, f.ID, "sprint 3"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ProjectItem(ctx, p.ID, it.ID)
	if got.Values[f.ID] != strconv.FormatInt(f.Iterations[2].ID, 10) {
		t.Fatalf("value by name = %q", got.Values[f.ID])
	}
	if err := s.SetItemValue(ctx, p.ID, it.ID, f.ID, "@current"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ProjectItem(ctx, p.ID, it.ID)
	if got.Values[f.ID] != strconv.FormatInt(f.Iterations[1].ID, 10) {
		t.Fatalf("value @current = %q", got.Values[f.ID])
	}
	if err := s.SetItemValue(ctx, p.ID, it.ID, f.ID, "Sprint 9"); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("unknown iteration = %v", err)
	}

	// Edit: rename, re-date, set a length, add one after the last, delete one
	// (its values are cleared).
	edits := []IterationEdit{
		{ID: f.Iterations[0].ID, Name: "Kickoff", Start: start.AddDate(0, 0, -1).Format(projDate), Days: 8},
		{ID: f.Iterations[1].ID, Delete: true},
		{ID: f.Iterations[2].ID},
		{ID: f.Iterations[3].ID},
		{Name: ""},
	}
	if err := s.EditIterations(ctx, p.ID, f.ID, 10, edits); err != nil {
		t.Fatal(err)
	}
	f, _ = s.ProjectField(ctx, p.ID, f.ID)
	if f.Duration != 10 || len(f.Iterations) != 4 {
		t.Fatalf("after edit = %+v", f)
	}
	if k := f.Iterations[0]; k.Name != "Kickoff" || k.Days != 8 || !k.Start.Equal(start.AddDate(0, 0, -1)) {
		t.Fatalf("edited = %+v", k)
	}
	added := f.Iterations[3]
	if added.Name != "Iteration 4" || added.Days != 10 || !added.Start.Equal(today.AddDate(0, 3, 7)) {
		t.Fatalf("added = %+v", added)
	}
	got, _ = s.ProjectItem(ctx, p.ID, it.ID)
	if v := got.Values[f.ID]; v != "" {
		t.Fatalf("value of a deleted iteration = %q", v)
	}
	if err := s.EditIterations(ctx, p.ID, f.ID, 0, []IterationEdit{{ID: f.Iterations[0].ID, Name: "Sprint 3"}}); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("rename onto another iteration = %v", err)
	}
	status, _ := s.StatusField(ctx, p.ID)
	if err := s.EditIterations(ctx, p.ID, status.ID, 0, nil); !errors.Is(err, ErrProjInvalid) {
		t.Fatalf("EditIterations on Status = %v", err)
	}

	// Deleting the field takes its iterations with it.
	if err := s.DeleteField(ctx, p.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM proj_iterations WHERE field_id=?`, f.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d iterations left after deleting the field", n)
	}
}

func TestParseIterationLine(t *testing.T) {
	for in, want := range map[string]string{"Sprint 1": "", "Sprint 1 2026-11-02": "2026-11-02", "  Q4   2026-10-01 ": "2026-10-01", "2026-11-02": ""} {
		name, st, ok := ParseIterationLine(in)
		if (want != "") != ok || (ok && st.Format(projDate) != want) || name == "" {
			t.Errorf("%q = %q %v %v", in, name, st, ok)
		}
	}
}
