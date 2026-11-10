package store

import (
	"context"
	"errors"
	"testing"
)

func pullsRepo(t *testing.T, s *Store) Repo {
	t.Helper()
	ctx := context.Background()
	o, err := s.CreateOwner(ctx, "zoe", "Zoë", "user")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRepo(ctx, o.ID, "demo", "", true)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPullsCRUD(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := pullsRepo(t, s)

	// Numbers share the repo's #sequence (an issue would take #1 here).
	if n, _ := s.NextNumber(ctx, r.ID); n != 1 {
		t.Fatalf("NextNumber = %d", n)
	}
	p, err := s.CreatePull(ctx, NewPull{RepoID: r.ID, Title: "Add greeting", Body: "Hello", Head: "feature/hi", Base: "main",
		HeadSHA: "abc", AuthorKind: "owner", AuthorName: "zoe"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Number != 2 || p.State != "open" || !p.IsOpen() || p.AuthorName != "zoe" || p.HeadSHA != "abc" {
		t.Fatalf("created = %+v", p)
	}
	p2, _ := s.CreatePull(ctx, NewPull{RepoID: r.ID, Title: "Other", Head: "x", Base: "main"})
	if p2.Number != 3 {
		t.Errorf("second number = %d", p2.Number)
	}

	got, err := s.PullByNumber(ctx, r.ID, 2)
	if err != nil || got.Title != "Add greeting" {
		t.Fatalf("PullByNumber = %+v, %v", got, err)
	}
	if _, err := s.PullByNumber(ctx, r.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing pull = %v", err)
	}
	if op, err := s.OpenPullFor(ctx, r.ID, "main", "feature/hi"); err != nil || op.ID != p.ID {
		t.Errorf("OpenPullFor = %+v, %v", op, err)
	}

	if err := s.UpdatePullText(ctx, p.ID, "Add a greeting", "Body 2"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPullHeadSHA(ctx, p.ID, "def"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.PullByNumber(ctx, r.ID, 2)
	if got.Title != "Add a greeting" || got.Body != "Body 2" || got.HeadSHA != "def" {
		t.Errorf("after update = %+v", got)
	}

	// Close / reopen / merge state machine.
	if err := s.ClosePull(ctx, p2.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ClosePull(ctx, p2.ID); !errors.Is(err, ErrPullStateChanged) {
		t.Errorf("closing twice = %v", err)
	}
	c, _ := s.PullByNumber(ctx, r.ID, 3)
	if !c.IsClosed() || c.ClosedAt == nil {
		t.Errorf("closed = %+v", c)
	}
	if err := s.ReopenPull(ctx, p2.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPullMerged(ctx, p.ID, "squash", "m3rg3", "base0", "head1", "zoe"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPullMerged(ctx, p.ID, "squash", "again", "", "", "zoe"); !errors.Is(err, ErrPullStateChanged) {
		t.Errorf("merging twice = %v", err)
	}
	if err := s.ClosePull(ctx, p.ID); !errors.Is(err, ErrPullStateChanged) {
		t.Errorf("closing a merged pull = %v", err)
	}
	m, _ := s.PullByNumber(ctx, r.ID, 2)
	if !m.IsMerged() || m.MergedSHA != "m3rg3" || m.MergeMethod != "squash" || m.BaseSHA != "base0" || m.HeadSHA != "head1" || m.MergedAt == nil || m.MergedBy != "zoe" {
		t.Errorf("merged = %+v", m)
	}
	if _, err := s.OpenPullFor(ctx, r.ID, "main", "feature/hi"); !errors.Is(err, ErrNotFound) {
		t.Errorf("merged pull still found as open: %v", err)
	}

	counts, _ := s.PullCounts(ctx, r.ID)
	if counts["open"] != 1 || counts["merged"] != 1 || counts["closed"] != 0 {
		t.Errorf("counts = %v", counts)
	}
	if open, _ := s.Pulls(ctx, r.ID, "open"); len(open) != 1 || open[0].Number != 3 {
		t.Errorf("open list = %+v", open)
	}
	if all, _ := s.Pulls(ctx, r.ID, ""); len(all) != 2 || all[0].Number != 3 {
		t.Errorf("all list = %+v", all)
	}

	// Comments, events.
	if _, err := s.AddPullComment(ctx, p.ID, "owner", "zoe", "Looks good"); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.PullComments(ctx, p.ID)
	if len(cs) != 1 || cs[0].Body != "Looks good" || cs[0].AuthorKind != "owner" {
		t.Errorf("comments = %+v", cs)
	}
	if err := s.AddPullEvent(ctx, p.ID, "merged", "owner", "zoe", "squash m3rg3"); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.PullEvents(ctx, p.ID)
	if len(evs) != 1 || evs[0].Kind != "merged" || evs[0].Data != "squash m3rg3" {
		t.Errorf("events = %+v", evs)
	}

	// Notes to self.
	n1, err := s.AddPullNote(ctx, NewPullNote{PullID: p2.ID, CommitSHA: "c1", Path: "a.go", Line: 4, Side: "new", Body: "rename this", AuthorKind: "owner", AuthorName: "zoe"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPullNote(ctx, NewPullNote{PullID: p2.ID, CommitSHA: "c1", Path: "a.go", Line: 2, Side: "old", Body: "why gone?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddPullNote(ctx, NewPullNote{PullID: p2.ID, Side: "middle"}); err == nil {
		t.Error("bad side accepted")
	}
	if n, _ := s.UnresolvedPullNotes(ctx, p2.ID); n != 2 {
		t.Errorf("unresolved = %d", n)
	}
	if err := s.SetPullNoteResolved(ctx, p.ID, n1.ID, true, "zoe"); !errors.Is(err, ErrNotFound) {
		t.Errorf("resolving a note through another pull = %v", err)
	}
	if err := s.SetPullNoteResolved(ctx, p2.ID, n1.ID, true, "zoe"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.UnresolvedPullNotes(ctx, p2.ID); n != 1 {
		t.Errorf("unresolved after resolve = %d", n)
	}
	notes, _ := s.PullNotes(ctx, p2.ID)
	if len(notes) != 2 || notes[0].Line != 2 || notes[1].ID != n1.ID || !notes[1].Resolved || notes[1].ResolvedBy != "zoe" {
		t.Errorf("notes = %+v", notes)
	}
	_ = s.SetPullNoteResolved(ctx, p2.ID, n1.ID, false, "zoe")
	if n, _ := s.UnresolvedPullNotes(ctx, p2.ID); n != 2 {
		t.Errorf("unresolved after unresolve = %d", n)
	}

	// Deleting the repo cascades.
	if err := s.DeleteRepo(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	s.DB().QueryRow(`SELECT (SELECT COUNT(*) FROM pulls) + (SELECT COUNT(*) FROM pull_notes) + (SELECT COUNT(*) FROM pull_comments)`).Scan(&left)
	if left != 0 {
		t.Errorf("%d rows left after deleting the repo", left)
	}
}

func TestBranchRules(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	r := pullsRepo(t, s)

	main, err := s.CreateBranchRule(ctx, BranchRule{RepoID: r.ID, Pattern: "main", RequirePR: true, BlockForcePush: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBranchRule(ctx, BranchRule{RepoID: r.ID, Pattern: "main"}); !errors.Is(err, ErrBranchRuleExists) {
		t.Errorf("duplicate pattern = %v", err)
	}
	rel, _ := s.CreateBranchRule(ctx, BranchRule{RepoID: r.ID, Pattern: "release/*", LinearHistory: true})
	rules, _ := s.BranchRules(ctx, r.ID)
	if len(rules) != 2 || rules[0].Pattern != "main" || !rules[0].RequirePR || !rules[1].LinearHistory {
		t.Fatalf("rules = %+v", rules)
	}

	rel.RequireResolvedNotes = true
	rel.Pattern = "release/**"
	if err := s.UpdateBranchRule(ctx, rel); err != nil {
		t.Fatal(err)
	}
	rel.Pattern = "main"
	if err := s.UpdateBranchRule(ctx, rel); !errors.Is(err, ErrBranchRuleExists) {
		t.Errorf("update to duplicate = %v", err)
	}
	got, _ := s.BranchRuleByID(ctx, r.ID, rel.ID)
	if got.Pattern != "release/**" || !got.RequireResolvedNotes {
		t.Errorf("updated = %+v", got)
	}

	p, _ := s.ProtectionFor(ctx, r.ID, "main")
	if !p.Protected() || !p.RequirePR || !p.BlockForcePush || p.LinearHistory || p.Patterns() != `"main"` {
		t.Errorf("main protection = %+v", p)
	}
	p, _ = s.ProtectionFor(ctx, r.ID, "release/1/hotfix")
	if !p.LinearHistory || !p.RequireResolvedNotes || p.RequirePR {
		t.Errorf("release protection = %+v", p)
	}
	if p, _ := s.ProtectionFor(ctx, r.ID, "feature/x"); p.Protected() {
		t.Errorf("feature/x protected: %+v", p)
	}

	if err := s.DeleteBranchRule(ctx, r.ID, main.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BranchRuleByID(ctx, r.ID, main.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted rule = %v", err)
	}
}

func TestMatchBranch(t *testing.T) {
	for _, c := range []struct {
		pattern, branch string
		want            bool
	}{
		{"main", "main", true},
		{"main", "main2", false},
		{"release/*", "release/1.0", true},
		{"release/*", "release/1/x", false},
		{"release/**", "release/1/x", true},
		{"*", "main", true},
		{"*", "feature/x", false},
		{"**", "feature/x", true},
		{"v1.*", "v1.2", true},
		{"v1.*", "v1x2", false},
		{"feat-*-fix", "feat-a-fix", true},
	} {
		if got := MatchBranch(c.pattern, c.branch); got != c.want {
			t.Errorf("MatchBranch(%q, %q) = %v", c.pattern, c.branch, got)
		}
	}
	for p, ok := range map[string]bool{
		"main": true, "release/*": true, "**": true, "": false, "a b": false, "/main": false, "main/": false,
		"a//b": false, "a..b": false, "a***": false, "(x)": false,
	} {
		if ValidBranchPattern(p) != ok {
			t.Errorf("ValidBranchPattern(%q) = %v", p, !ok)
		}
	}
}
