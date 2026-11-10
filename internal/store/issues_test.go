package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func issueRepo(t *testing.T, s *Store) Repo {
	t.Helper()
	ctx := context.Background()
	o, err := s.CreateOwner(ctx, "zoe", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRepo(ctx, o.ID, "demo", "", true)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestIssueCRUDAndNumbering(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := issueRepo(t, s)

	a, err := s.CreateIssue(ctx, r.ID, "  First  ", "body one", "owner", "zoe")
	if err != nil {
		t.Fatal(err)
	}
	if a.Number != 1 || a.Title != "First" || !a.IsOpen() || a.AuthorName != "zoe" {
		t.Fatalf("unexpected issue %+v", a)
	}
	// A pull request takes #2 from the shared sequence.
	if n, _ := s.NextNumber(ctx, r.ID); n != 2 {
		t.Fatalf("NextNumber = %d, want 2", n)
	}
	b, err := s.CreateIssue(ctx, r.ID, "Second", "", "agent", "helper")
	if err != nil || b.Number != 3 {
		t.Fatalf("second issue = %+v, %v; want #3", b, err)
	}
	if _, err := s.IssueByNumber(ctx, r.ID, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("IssueByNumber(PR #2) = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateIssue(ctx, r.ID, "   ", "", "owner", "zoe"); !errors.Is(err, ErrInvalidIssue) {
		t.Errorf("empty title: %v", err)
	}

	if err := s.UpdateIssue(ctx, a.ID, "First, renamed", "new body", "owner", "zoe"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.IssueByNumber(ctx, r.ID, 1)
	if got.Title != "First, renamed" || got.Body != "new body" {
		t.Errorf("after update %+v", got)
	}

	// Close, close again (no-op), reopen, close not planned via number.
	if err := s.CloseIssue(ctx, a.ID, "owner", "zoe", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseIssue(ctx, a.ID, "owner", "zoe", ReasonNotPlanned); err != nil {
		t.Fatal(err)
	}
	got, _ = s.IssueByNumber(ctx, r.ID, 1)
	if got.State != "closed" || got.StateReason != ReasonCompleted || got.ClosedAt == nil {
		t.Errorf("after close %+v", got)
	}
	if err := s.ReopenIssue(ctx, a.ID, "owner", "zoe"); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseIssueByNumber(ctx, r.ID, 1, "bot", "pr-merge", ReasonNotPlanned); err != nil {
		t.Fatal(err)
	}
	got, _ = s.IssueByNumber(ctx, r.ID, 1)
	if got.State != "closed" || got.StateReason != ReasonNotPlanned {
		t.Errorf("after CloseIssueByNumber %+v", got)
	}
	if err := s.CloseIssueByNumber(ctx, r.ID, 2, "bot", "x", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("CloseIssueByNumber(PR) = %v", err)
	}

	evs, err := s.IssueEvents(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := []string{EventRenamed, EventClosed, EventReopened, EventClosed}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("events = %v, want %v", kinds, want)
	}
	if evs[0].OldTitle != "First" || evs[3].ActorName != "pr-merge" || evs[3].Reason != ReasonNotPlanned {
		t.Errorf("event details wrong: %+v", evs)
	}

	// Comments.
	c, err := s.AddIssueComment(ctx, a.ID, "hello", "owner", "zoe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddIssueComment(ctx, a.ID, "  ", "owner", "zoe"); err == nil {
		t.Error("empty comment accepted")
	}
	if err := s.UpdateIssueComment(ctx, c.ID, "hello again"); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.IssueComments(ctx, a.ID)
	if len(cs) != 1 || cs[0].Body != "hello again" {
		t.Errorf("comments %+v", cs)
	}
	got, _ = s.IssueByNumber(ctx, r.ID, 1)
	if got.Comments != 1 {
		t.Errorf("comment count = %d", got.Comments)
	}
	if err := s.DeleteIssueComment(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssueCommentByID(ctx, c.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted comment still there: %v", err)
	}

	nums, _ := s.IssueNumbers(ctx, r.ID)
	if !nums[1] || nums[2] || !nums[3] {
		t.Errorf("IssueNumbers = %v", nums)
	}

	// Deleting the repo removes its issues.
	if err := s.DeleteRepo(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IssueByNumber(ctx, r.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("issue survived repo deletion: %v", err)
	}
}

func TestLabels(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := issueRepo(t, s)

	if err := s.EnsureDefaultLabels(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	ls, _ := s.Labels(ctx, r.ID)
	if len(ls) != len(DefaultLabels) {
		t.Fatalf("seeded %d labels, want %d", len(ls), len(DefaultLabels))
	}
	// Deleting a default label sticks: seeding runs once.
	if err := s.DeleteLabel(ctx, r.ID, ls[0].ID); err != nil {
		t.Fatal(err)
	}
	s.EnsureDefaultLabels(ctx, r.ID)
	if ls2, _ := s.Labels(ctx, r.ID); len(ls2) != len(DefaultLabels)-1 {
		t.Errorf("reseeded: %d labels", len(ls2))
	}

	l, err := s.CreateLabel(ctx, r.ID, "good first issue", "#7FD16B", "Easy")
	if err != nil {
		t.Fatal(err)
	}
	if l.Color != "7fd16b" {
		t.Errorf("colour = %q", l.Color)
	}
	if _, err := s.CreateLabel(ctx, r.ID, "GOOD first issue", "abc", ""); err == nil {
		t.Error("duplicate (case-insensitive) label accepted")
	}
	for _, bad := range [][2]string{{"", "ffffff"}, {"x", "zzzzzz"}, {"x", "12345"}, {`a"b`, "ffffff"}} {
		if _, err := s.CreateLabel(ctx, r.ID, bad[0], bad[1], ""); err == nil {
			t.Errorf("invalid label %v accepted", bad)
		}
	}
	if err := s.UpdateLabel(ctx, r.ID, l.ID, "starter", "abc", "Short"); err != nil {
		t.Fatal(err)
	}
	if l2, _ := s.LabelByID(ctx, r.ID, l.ID); l2.Name != "starter" || l2.Color != "aabbcc" {
		t.Errorf("after update %+v", l2)
	}

	is, _ := s.CreateIssue(ctx, r.ID, "Crash", "", "owner", "zoe")
	ls, _ = s.Labels(ctx, r.ID)
	ids := []int64{ls[0].ID, ls[1].ID, 999999}
	if err := s.SetIssueLabels(ctx, is.ID, ids, "owner", "zoe"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.IssueByNumber(ctx, r.ID, is.Number)
	if len(got.Labels) != 2 {
		t.Fatalf("labels = %+v", got.Labels)
	}
	if err := s.SetIssueLabels(ctx, is.ID, []int64{ls[1].ID}, "owner", "zoe"); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.IssueEvents(ctx, is.ID)
	if len(evs) != 3 || evs[2].Kind != EventUnlabeled || evs[2].LabelName != ls[0].Name {
		t.Errorf("label events %+v", evs)
	}
	if ls3, _ := s.Labels(ctx, r.ID); ls3[1].Issues != 1 {
		t.Errorf("open issue count on label = %d", ls3[1].Issues)
	}
	// Deleting a label detaches it; the timeline keeps its name.
	s.DeleteLabel(ctx, r.ID, ls[1].ID)
	got, _ = s.IssueByNumber(ctx, r.ID, is.Number)
	if len(got.Labels) != 0 {
		t.Errorf("deleted label still attached")
	}
}

func TestMilestones(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := issueRepo(t, s)

	due := time.Date(2030, 1, 15, 0, 0, 0, 0, time.UTC)
	m, err := s.CreateMilestone(ctx, r.ID, "v1.0", "First release", &due)
	if err != nil {
		t.Fatal(err)
	}
	if m.DueOn == nil || !m.DueOn.Equal(due) || m.State != "open" {
		t.Errorf("milestone %+v", m)
	}
	if _, err := s.CreateMilestone(ctx, r.ID, "V1.0", "", nil); err == nil {
		t.Error("duplicate milestone accepted")
	}
	a, _ := s.CreateIssue(ctx, r.ID, "A", "", "owner", "zoe")
	b, _ := s.CreateIssue(ctx, r.ID, "B", "", "owner", "zoe")
	s.SetIssueMilestone(ctx, a.ID, m.ID, "owner", "zoe")
	s.SetIssueMilestone(ctx, b.ID, m.ID, "owner", "zoe")
	s.CloseIssue(ctx, a.ID, "owner", "zoe", "")
	m, _ = s.MilestoneByID(ctx, r.ID, m.ID)
	if m.Open != 1 || m.Closed != 1 || m.Percent() != 50 {
		t.Errorf("progress %+v %d%%", m, m.Percent())
	}
	got, _ := s.IssueByNumber(ctx, r.ID, a.Number)
	if got.Milestone != "v1.0" {
		t.Errorf("issue milestone = %q", got.Milestone)
	}
	if err := s.SetIssueMilestone(ctx, b.ID, 0, "owner", "zoe"); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.IssueEvents(ctx, b.ID)
	if len(evs) != 2 || evs[0].Kind != EventMilestoned || evs[1].Kind != EventDemilestoned || evs[1].Milestone != "v1.0" {
		t.Errorf("milestone events %+v", evs)
	}

	if err := s.UpdateMilestone(ctx, r.ID, m.ID, "v1", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMilestoneState(ctx, r.ID, m.ID, false); err != nil {
		t.Fatal(err)
	}
	open, closed, _ := s.MilestoneCounts(ctx, r.ID)
	if open != 0 || closed != 1 {
		t.Errorf("counts %d/%d", open, closed)
	}
	ms, _ := s.Milestones(ctx, r.ID, "closed")
	if len(ms) != 1 || ms[0].Title != "v1" || ms[0].DueOn != nil || ms[0].ClosedAt == nil {
		t.Errorf("closed milestones %+v", ms)
	}
	// Deleting a milestone clears it from issues.
	s.DeleteMilestone(ctx, r.ID, m.ID)
	got, _ = s.IssueByNumber(ctx, r.ID, a.Number)
	if got.MilestoneID != 0 {
		t.Errorf("issue still has deleted milestone")
	}
}

func TestParseIssueQuery(t *testing.T) {
	q := ParseIssueQuery(`is:closed label:bug label:"good first issue" milestone:"v1 beta" crash "on start" no:milestone sort:created-asc is:issue author:zoe`)
	want := IssueQuery{State: "closed", Labels: []string{"bug", "good first issue"}, Milestone: "v1 beta", NoMilestone: true,
		Author: "zoe", Text: []string{"crash", "on start"}, Sort: "oldest"}
	if !reflect.DeepEqual(q, want) {
		t.Errorf("parsed %+v\nwant   %+v", q, want)
	}
	if s := q.String(); s != `is:closed label:bug label:"good first issue" milestone:"v1 beta" no:milestone author:zoe crash "on start"` {
		t.Errorf("String() = %s", s)
	}
	if q := ParseIssueQuery("is:weird foo: :bar"); !reflect.DeepEqual(q.Text, []string{"is:weird", "foo:", ":bar"}) || q.State != "" {
		t.Errorf("unknown qualifiers %+v", q)
	}
	if !ParseIssueQuery("label:Bug").HasLabel("bug") {
		t.Error("HasLabel should ignore case")
	}
}

func TestIssueSearch(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	r := issueRepo(t, s)
	s.EnsureDefaultLabels(ctx, r.ID)
	ls, _ := s.Labels(ctx, r.ID)
	bug := ls[0] // "bug"
	m, _ := s.CreateMilestone(ctx, r.ID, "v1 beta", "", nil)

	a, _ := s.CreateIssue(ctx, r.ID, "Crash on start", "segfault 100%", "owner", "zoe")
	b, _ := s.CreateIssue(ctx, r.ID, "Add dark mode", "users want it", "owner", "zoe")
	c, _ := s.CreateIssue(ctx, r.ID, "Docs typo", "crash course link broken", "agent", "helper")
	s.SetIssueLabels(ctx, a.ID, []int64{bug.ID}, "owner", "zoe")
	s.SetIssueMilestone(ctx, a.ID, m.ID, "owner", "zoe")
	s.CloseIssue(ctx, c.ID, "owner", "zoe", "")
	_ = b

	cases := []struct {
		q    string
		want []int64
	}{
		{"is:open", []int64{b.Number, a.Number}},
		{"is:closed", []int64{c.Number}},
		{"crash", []int64{c.Number, a.Number}},
		{"crash is:open", []int64{a.Number}},
		{"label:BUG", []int64{a.Number}},
		{"no:label is:open", []int64{b.Number}},
		{`milestone:"v1 beta"`, []int64{a.Number}},
		{"no:milestone", []int64{c.Number, b.Number}},
		{"100%", []int64{a.Number}},
		{"10_%", nil},
		{"author:helper", []int64{c.Number}},
		{"#2", []int64{b.Number}},
		{"sort:created-asc", []int64{a.Number, b.Number, c.Number}},
	}
	for _, tc := range cases {
		got, _, err := s.Issues(ctx, r.ID, ParseIssueQuery(tc.q), 0, 50)
		if err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		var nums []int64
		for _, i := range got {
			nums = append(nums, i.Number)
		}
		if !reflect.DeepEqual(nums, tc.want) {
			t.Errorf("%q = %v, want %v", tc.q, nums, tc.want)
		}
	}

	open, closed, _ := s.IssueCounts(ctx, r.ID, ParseIssueQuery("is:open crash"))
	if open != 1 || closed != 1 {
		t.Errorf("counts for crash = %d/%d", open, closed)
	}

	// Pagination.
	page1, more, _ := s.Issues(ctx, r.ID, IssueQuery{}, 0, 2)
	page2, more2, _ := s.Issues(ctx, r.ID, IssueQuery{}, 2, 2)
	if len(page1) != 2 || !more || len(page2) != 1 || more2 {
		t.Errorf("pagination: %d %v / %d %v", len(page1), more, len(page2), more2)
	}
	if len(page2[0].Labels) != 1 || page2[0].Milestone != "v1 beta" {
		t.Errorf("list row missing label/milestone: %+v", page2[0])
	}
}

func TestNormalizeColor(t *testing.T) {
	for in, want := range map[string]string{"#ABCDEF": "abcdef", "abc": "aabbcc", " #0f0 ": "00ff00"} {
		if got, ok := NormalizeColor(in); !ok || got != want {
			t.Errorf("NormalizeColor(%q) = %q %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "#12", "ggg", "1234567", "#12345;"} {
		if _, ok := NormalizeColor(bad); ok {
			t.Errorf("NormalizeColor(%q) accepted", bad)
		}
	}
}
