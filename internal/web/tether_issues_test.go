package web

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
	"atelier/internal/tether"
)

// ---------- the fake GitHub's issue writes, labels, milestones and reviews ----------

// serveIssueSync answers the issue sync's calls (f.mu held).
func (f *fakeGH) serveIssueSync(w http.ResponseWriter, r *http.Request, full string, rest []string, body map[string]any) bool {
	str := func(k string) string { s, _ := body[k].(string); return s }
	num := func(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
	switch {
	case len(rest) == 1 && rest[0] == "issues" && r.Method == http.MethodPost:
		n := f.nextNumberLocked(full)
		now := time.Now()
		is := tether.Issue{Number: n, Title: str("title"), Body: str("body"), State: "open", User: tether.User{Login: f.login},
			HTMLURL: fmt.Sprintf("%s/%s/issues/%d", f.srv.URL, full, n), CreatedAt: now, UpdatedAt: now}
		f.applyIssueLocked(full, &is, body)
		f.issues[full] = append(f.issues[full], is)
		f.write(w, r, 201, is)
	case len(rest) == 2 && rest[0] == "issues" && r.Method == http.MethodPatch:
		n := num(rest[1])
		for i := range f.issues[full] {
			is := &f.issues[full][i]
			if is.Number != n || is.IsPull() {
				continue
			}
			if v, ok := body["title"].(string); ok {
				is.Title = v
			}
			if v, ok := body["body"].(string); ok {
				is.Body = v
			}
			if v, ok := body["state"].(string); ok {
				is.State, is.StateReason = v, str("state_reason")
				if v == "open" {
					is.StateReason = "reopened"
				}
			}
			f.applyIssueLocked(full, is, body)
			is.UpdatedAt = time.Now()
			f.write(w, r, 200, *is)
			return true
		}
		f.write(w, r, 404, map[string]string{"message": "Not Found"})
	case len(rest) == 1 && rest[0] == "labels" && r.Method == http.MethodGet:
		f.write(w, r, 200, append([]tether.Label{}, f.labels[full]...))
	case len(rest) == 1 && rest[0] == "labels" && r.Method == http.MethodPost:
		for _, l := range f.labels[full] {
			if strings.EqualFold(l.Name, str("name")) {
				f.write(w, r, 422, map[string]string{"message": "Validation Failed"})
				return true
			}
		}
		l := tether.Label{Name: str("name"), Color: str("color"), Description: str("description")}
		f.labels[full] = append(f.labels[full], l)
		f.write(w, r, 201, l)
	case len(rest) == 1 && rest[0] == "milestones" && r.Method == http.MethodGet:
		out := []tether.Milestone{}
		for _, m := range f.milestones[full] {
			out = append(out, *m)
		}
		f.write(w, r, 200, out)
	case len(rest) == 1 && rest[0] == "milestones" && r.Method == http.MethodPost:
		for _, m := range f.milestones[full] {
			if strings.EqualFold(m.Title, str("title")) {
				f.write(w, r, 422, map[string]string{"message": "Validation Failed"})
				return true
			}
		}
		m := &tether.Milestone{Number: int64(len(f.milestones[full]) + 1), Title: str("title"), Description: str("description"),
			State: str("state"), UpdatedAt: time.Now()}
		if m.State == "" {
			m.State = "open"
		}
		if d, err := time.Parse(time.RFC3339, str("due_on")); err == nil {
			d = d.Add(8 * time.Hour) // GitHub moves due dates into the day
			m.DueOn = &d
		}
		f.milestones[full] = append(f.milestones[full], m)
		f.write(w, r, 201, *m)
	case len(rest) == 2 && rest[0] == "milestones" && r.Method == http.MethodPatch:
		for _, m := range f.milestones[full] {
			if m.Number != num(rest[1]) {
				continue
			}
			if s := str("state"); s != "" {
				m.State = s
			}
			if v, ok := body["due_on"]; ok {
				m.DueOn = nil
				if d, err := time.Parse(time.RFC3339, fmt.Sprint(v)); err == nil {
					m.DueOn = &d
				}
			}
			m.UpdatedAt = time.Now()
			f.write(w, r, 200, *m)
			return true
		}
		f.write(w, r, 404, map[string]string{"message": "Not Found"})
	case len(rest) == 3 && rest[0] == "pulls" && rest[2] == "reviews" && r.Method == http.MethodGet:
		f.write(w, r, 200, append([]tether.Review{}, f.prReviews[full][num(rest[1])]...))
	default:
		return false
	}
	return true
}

func (f *fakeGH) nextNumberLocked(full string) int64 {
	var n int64
	for _, is := range f.issues[full] {
		if is.Number > n {
			n = is.Number
		}
	}
	for k := range f.pulls[full] {
		if k > n {
			n = k
		}
	}
	return n + 1
}

// applyIssueLocked sets labels (by name; unknown ones are made, like GitHub
// does) and the milestone (by number) from a request body.
func (f *fakeGH) applyIssueLocked(full string, is *tether.Issue, body map[string]any) {
	if v, ok := body["labels"]; ok {
		is.Labels = nil
		list, _ := v.([]any)
		for _, x := range list {
			name, _ := x.(string)
			var found *tether.Label
			for i := range f.labels[full] {
				if strings.EqualFold(f.labels[full][i].Name, name) {
					found = &f.labels[full][i]
				}
			}
			if found == nil {
				f.labels[full] = append(f.labels[full], tether.Label{Name: name, Color: "ededed"})
				found = &f.labels[full][len(f.labels[full])-1]
			}
			is.Labels = append(is.Labels, *found)
		}
	}
	if v, ok := body["milestone"]; ok {
		is.Milestone = nil
		if n, ok := v.(float64); ok {
			for _, m := range f.milestones[full] {
				if m.Number == int64(n) {
					cp := *m
					is.Milestone = &cp
				}
			}
		}
	}
}

// ghIssue returns a copy of a GitHub issue.
func (f *fakeGH) ghIssue(t *testing.T, full string, n int64) tether.Issue {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, is := range f.issues[full] {
		if is.Number == n && !is.IsPull() {
			return is
		}
	}
	t.Fatalf("no GitHub issue %s#%d", full, n)
	return tether.Issue{}
}

// editIssue changes a GitHub issue as someone on GitHub would, at time at.
func (f *fakeGH) editIssue(full string, n int64, at time.Time, fn func(is *tether.Issue)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.issues[full] {
		if f.issues[full][i].Number == n {
			fn(&f.issues[full][i])
			f.issues[full][i].UpdatedAt = at
		}
	}
}

func (f *fakeGH) milestone(full, title string) *tether.Milestone {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.milestones[full] {
		if m.Title == title {
			cp := *m
			return &cp
		}
	}
	return nil
}

// writes counts every request that changes something on GitHub.
func (f *fakeGH) writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k, v := range f.hits {
		if !strings.HasPrefix(k, "GET ") {
			n += v
		}
	}
	return n
}

func fakeIssueLabels(is tether.Issue) []string {
	var out []string
	for _, l := range is.Labels {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
}

func localLabelNames(is store.Issue) []string {
	var out []string
	for _, l := range is.Labels {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
}

const tetherTwin = "zoe-gh/demo"

// newIssueSyncFixture: connected, main pushed, an empty twin on GitHub.
func newIssueSyncFixture(t *testing.T) *tetherFixture {
	t.Helper()
	f := newTetherFixture(t)
	f.connect(t)
	pullsCommit(t, f.work, "README.md", "# demo\n", "Initial")
	runGit(t, f.work, "push", "-q", "origin", "main")
	f.gh.addRepo(tetherTwin, true)
	runGit(t, f.work, "push", "-q", f.gh.bare(tetherTwin), "main")
	return f
}

func (f *tetherFixture) issue(t *testing.T, n int64) store.Issue {
	t.Helper()
	is, err := f.a.db.IssueByNumber(context.Background(), f.repoID(t), n)
	if err != nil {
		t.Fatalf("issue #%d: %v", n, err)
	}
	return is
}

// ---------- tests ----------

func TestTetherIssueEditsBothWaysWithoutPingPong(t *testing.T) {
	f := newIssueSyncFixture(t)
	a, ctx := f.a, context.Background()
	t0 := time.Now().Add(-time.Hour)
	f.gh.mu.Lock()
	f.gh.issues[tetherTwin] = []tether.Issue{{Number: 3, Title: "Crash on start", Body: "It crashes\r\n<script>alert(1)</script>", State: "open",
		User: tether.User{Login: "alice"}, CreatedAt: t0, UpdatedAt: t0, HTMLURL: f.gh.srv.URL + "/zoe-gh/demo/issues/3"}}
	f.gh.mu.Unlock()
	f.link(t, tetherTwin, "atelier", true)
	if is := f.issue(t, 3); is.Title != "Crash on start" {
		t.Fatalf("synced issue = %+v", is)
	}
	// Untrusted GitHub text is stored as text and rendered only through Markdown.
	page := expectStatus(t, do(t, a, "GET", "/zoe/demo/issues/3", nil, f.ck), 200, "issue page")
	if strings.Contains(page, "<script>alert(1)") {
		t.Fatal("GitHub's issue body reached the page unescaped")
	}
	mustContain(t, page, "issue page", "GitHub #3")
	writes := f.gh.writes()

	// Edited on GitHub → Atelier follows, nothing is written back.
	f.gh.editIssue(tetherTwin, 3, time.Now(), func(is *tether.Issue) { is.Title, is.Body = "Crash on startup", "Steps: open it" })
	f.sync(t)
	if is := f.issue(t, 3); is.Title != "Crash on startup" || is.Body != "Steps: open it" {
		t.Fatalf("after GitHub edit: %q / %q", is.Title, is.Body)
	}
	f.sync(t)
	if w := f.gh.writes(); w != writes {
		t.Fatalf("GitHub's edit bounced back: %d writes", w-writes)
	}

	// Edited in Atelier → GitHub follows once; GitHub's echo changes nothing here.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/issues/3/edit", url.Values{"title": {"Crash when starting"}, "body": {"Steps: open it twice"}}, f.ck), 303, "edit")
	f.sync(t)
	gi := f.gh.ghIssue(t, tetherTwin, 3)
	if gi.Title != "Crash when starting" || gi.Body != "Steps: open it twice" {
		t.Fatalf("GitHub after Atelier edit: %q / %q", gi.Title, gi.Body)
	}
	if n := f.gh.hit("PATCH /repos/zoe-gh/demo/issues/3"); n != 1 {
		t.Fatalf("PATCH issue hits = %d, want 1", n)
	}
	before := f.issue(t, 3)
	f.sync(t)
	f.sync(t)
	after := f.issue(t, 3)
	if n := f.gh.hit("PATCH /repos/zoe-gh/demo/issues/3"); n != 1 || after.Title != before.Title || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("ping-pong: %d PATCHes, title %q→%q, updated %v→%v", n, before.Title, after.Title, before.UpdatedAt, after.UpdatedAt)
	}
	evs, _ := a.db.IssueEvents(ctx, after.ID)
	renames := 0
	for _, e := range evs {
		if e.Kind == "renamed" {
			renames++
		}
	}
	if renames != 2 { // GitHub's rename and ours: no echo of either
		t.Errorf("renamed events = %d, want 2", renames)
	}

	// Both sides edited: the later edit wins.
	_ = a.db.UpdateIssue(ctx, after.ID, "Atelier's title", "a", ActorOwner, "zoe")
	f.gh.editIssue(tetherTwin, 3, time.Now().Add(time.Hour), func(is *tether.Issue) { is.Title = "GitHub's title" })
	f.sync(t)
	if is := f.issue(t, 3); is.Title != "GitHub's title" {
		t.Errorf("GitHub's later edit lost: %q", is.Title)
	}
	f.gh.editIssue(tetherTwin, 3, time.Now().Add(-time.Minute), func(is *tether.Issue) { is.Title = "Older GitHub title" })
	_ = a.db.UpdateIssue(ctx, after.ID, "Newer Atelier title", "a", ActorOwner, "zoe")
	f.sync(t)
	if gi := f.gh.ghIssue(t, tetherTwin, 3); gi.Title != "Newer Atelier title" || f.issue(t, 3).Title != "Newer Atelier title" {
		t.Errorf("Atelier's later edit lost: GitHub %q, Atelier %q", gi.Title, f.issue(t, 3).Title)
	}
	w := f.gh.writes()
	f.sync(t)
	f.sync(t)
	if f.gh.writes() != w {
		t.Error("settled issue still writes to GitHub")
	}
}

func TestTetherLabelsAndMilestonesBothWays(t *testing.T) {
	f := newIssueSyncFixture(t)
	a, ctx := f.a, context.Background()
	rid := f.repoID(t)
	t0 := time.Now().Add(-time.Hour)
	due := time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC)
	v1 := &tether.Milestone{Number: 1, Title: "v1", Description: "First", State: "open", DueOn: &due, UpdatedAt: t0}
	f.gh.mu.Lock()
	f.gh.labels[tetherTwin] = []tether.Label{{Name: "Triage", Color: "FBCA04", Description: "Needs a look"}, {Name: "Docs", Color: "0075ca"}}
	f.gh.milestones[tetherTwin] = []*tether.Milestone{v1}
	f.gh.issues[tetherTwin] = []tether.Issue{{Number: 3, Title: "Labelled", State: "open", User: tether.User{Login: "alice"},
		Labels: []tether.Label{f.gh.labels[tetherTwin][0]}, Milestone: &tether.Milestone{Number: 1, Title: "v1", State: "open", DueOn: &due},
		CreatedAt: t0, UpdatedAt: t0}}
	f.gh.mu.Unlock()
	if err := a.db.EnsureDefaultLabels(ctx, rid); err != nil { // bug, enhancement, docs…
		t.Fatal(err)
	}
	f.link(t, tetherTwin, "atelier", true)

	// In: the missing label is made with GitHub's colour and description; the milestone with its due date.
	is := f.issue(t, 3)
	if got := localLabelNames(is); len(got) != 1 || got[0] != "Triage" || is.Labels[0].Color != "fbca04" || is.Labels[0].Description != "Needs a look" {
		t.Fatalf("labels in = %+v", is.Labels)
	}
	ms, _ := a.db.Milestones(ctx, rid, "")
	if is.Milestone != "v1" || len(ms) != 1 || ms[0].DueOn == nil || ms[0].DueOn.Format("2006-01-02") != "2026-12-01" || ms[0].State != "open" {
		t.Fatalf("milestone in = %q, %+v", is.Milestone, ms)
	}

	// Out: Atelier adds "docs" (GitHub has "Docs": mapped, not made again) and a new "Urgent", drops "Triage".
	labels, _ := a.db.Labels(ctx, rid)
	byName := map[string]store.Label{}
	for _, l := range labels {
		byName[l.Name] = l
	}
	urgent, err := a.db.CreateLabel(ctx, rid, "Urgent", "#ff0000", "Do it now")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetIssueLabels(ctx, is.ID, []int64{byName["docs"].ID, urgent.ID}, ActorOwner, "zoe"); err != nil {
		t.Fatal(err)
	}
	f.sync(t)
	gi := f.gh.ghIssue(t, tetherTwin, 3)
	if got := strings.Join(fakeIssueLabels(gi), ","); got != "Docs,Urgent" {
		t.Fatalf("GitHub labels = %s", got)
	}
	if n := f.gh.hit("POST /repos/zoe-gh/demo/labels"); n != 1 {
		t.Errorf("labels made on GitHub = %d, want 1 (Urgent)", n)
	}
	f.gh.mu.Lock()
	for _, l := range f.gh.labels[tetherTwin] {
		if l.Name == "Urgent" && (l.Color != "ff0000" || l.Description != "Do it now") {
			t.Errorf("Urgent on GitHub = %+v", l)
		}
	}
	f.gh.mu.Unlock()

	// In again: GitHub removes Docs and adds "enhancement" (Atelier's default, matched case-insensitively).
	f.gh.editIssue(tetherTwin, 3, time.Now(), func(is *tether.Issue) {
		is.Labels = []tether.Label{{Name: "Urgent", Color: "ff0000"}, {Name: "Enhancement", Color: "a2eeef"}}
	})
	f.sync(t)
	if got := strings.Join(localLabelNames(f.issue(t, 3)), ","); got != "Urgent,enhancement" {
		t.Fatalf("Atelier labels after GitHub change = %s", got)
	}
	if ls, _ := a.db.Labels(ctx, rid); len(ls) != len(labels)+1 {
		t.Errorf("labels were duplicated: %d, want %d", len(ls), len(labels)+1)
	}

	// Milestones out: a new one with a due date, created on GitHub and set on the issue.
	d2 := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	v2, err := a.db.CreateMilestone(ctx, rid, "v2", "Second", &d2)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetIssueMilestone(ctx, is.ID, v2.ID, ActorOwner, "zoe"); err != nil {
		t.Fatal(err)
	}
	f.sync(t)
	gm := f.gh.milestone(tetherTwin, "v2")
	gi = f.gh.ghIssue(t, tetherTwin, 3)
	if gm == nil || gm.DueOn == nil || gm.DueOn.Format("2006-01-02") != "2027-01-15" || gm.State != "open" || gm.Description != "Second" {
		t.Fatalf("v2 on GitHub = %+v", gm)
	}
	if gi.Milestone == nil || gi.Milestone.Title != "v2" {
		t.Fatalf("GitHub issue milestone = %+v", gi.Milestone)
	}
	// Closed on GitHub (later): closed here. Reopened here (later): reopened there.
	f.gh.mu.Lock()
	for _, m := range f.gh.milestones[tetherTwin] {
		if m.Title == "v2" {
			m.State, m.UpdatedAt = "closed", time.Now().Add(time.Hour)
		}
	}
	f.gh.mu.Unlock()
	f.sync(t)
	if m, _ := a.db.MilestoneByID(ctx, rid, v2.ID); m.State != "closed" {
		t.Fatalf("v2 here = %s", m.State)
	}
	f.gh.mu.Lock()
	for _, m := range f.gh.milestones[tetherTwin] {
		if m.Title == "v2" {
			m.UpdatedAt = time.Now().Add(-time.Hour)
		}
	}
	f.gh.mu.Unlock()
	_ = a.db.SetMilestoneState(ctx, rid, v2.ID, true)
	nd := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	_ = a.db.UpdateMilestone(ctx, rid, v2.ID, "v2", "Second", &nd)
	f.sync(t)
	if gm := f.gh.milestone(tetherTwin, "v2"); gm.State != "open" || gm.DueOn == nil || gm.DueOn.Format("2006-01-02") != "2027-02-01" {
		t.Fatalf("v2 on GitHub after reopening here = %+v", gm)
	}
	// Milestone cleared on GitHub: cleared here.
	f.gh.editIssue(tetherTwin, 3, time.Now(), func(is *tether.Issue) { is.Milestone = nil })
	f.sync(t)
	if is := f.issue(t, 3); is.Milestone != "" {
		t.Errorf("milestone after GitHub cleared it = %q", is.Milestone)
	}

	// Settled: no more writes either way.
	w := f.gh.writes()
	snap := f.issue(t, 3)
	f.sync(t)
	f.sync(t)
	if f.gh.writes() != w || !f.issue(t, 3).UpdatedAt.Equal(snap.UpdatedAt) {
		t.Errorf("labels/milestones ping-pong: %d writes", f.gh.writes()-w)
	}
}

func TestTetherAtelierIssuePushedToGitHub(t *testing.T) {
	f := newIssueSyncFixture(t)
	a, ctx := f.a, context.Background()
	rid := f.repoID(t)
	t0 := time.Now().Add(-time.Hour)
	// GitHub already has #1 and #2 that Atelier doesn't: the numbers will differ.
	f.gh.mu.Lock()
	f.gh.issues[tetherTwin] = []tether.Issue{
		{Number: 1, Title: "Old one", State: "closed", User: tether.User{Login: "alice"}, CreatedAt: t0, UpdatedAt: t0},
		{Number: 2, Title: "Old two", State: "open", User: tether.User{Login: "bob"}, CreatedAt: t0, UpdatedAt: t0}}
	f.gh.mu.Unlock()
	// An issue written before the link (and before pushing started) stays in Atelier.
	if _, err := a.db.CreateIssue(ctx, rid, "Before the link", "", ActorOwner, "zoe"); err != nil {
		t.Fatal(err)
	}
	f.link(t, tetherTwin, "atelier", true)
	if it, err := a.db.TetherItemByGH(ctx, rid, 1); err != nil || it.Number == 1 {
		t.Fatalf("GitHub #1 = %+v %v (Atelier #1 was taken)", it, err)
	}
	if it, err := a.db.TetherItem(ctx, rid, 1); err == nil && it.Origin == "atelier" {
		t.Fatal("an issue from before the link was pushed") // Atelier #1 is "Before the link"
	}

	// Opened in Atelier (with a label): pushed right away, the mapping recorded.
	labels, _ := a.db.Labels(ctx, rid)
	if len(labels) == 0 {
		_ = a.db.EnsureDefaultLabels(ctx, rid)
		labels, _ = a.db.Labels(ctx, rid)
	}
	rec := do(t, a, "POST", "/zoe/demo/issues/new", url.Values{"title": {"Made in Atelier"}, "body": {"Hello GitHub"},
		"label": {strconv.FormatInt(labels[0].ID, 10)}}, f.ck)
	expectStatus(t, rec, 303, "new issue")
	loc := rec.Header().Get("Location")
	num, _ := strconv.ParseInt(loc[strings.LastIndex(loc, "/")+1:], 10, 64)
	f.wait()
	it, err := a.db.TetherItem(ctx, rid, num)
	if err != nil || it.Origin != "atelier" || it.Kind != "issue" {
		t.Fatalf("item for Atelier #%d = %+v %v", num, it, err)
	}
	if it.GHNumber == num {
		t.Fatalf("numbers should differ here: Atelier #%d, GitHub #%d", num, it.GHNumber)
	}
	gi := f.gh.ghIssue(t, tetherTwin, it.GHNumber)
	if gi.Title != "Made in Atelier" || gi.Body != "Hello GitHub" || len(gi.Labels) != 1 || !strings.EqualFold(gi.Labels[0].Name, labels[0].Name) {
		t.Fatalf("GitHub issue = %+v", gi)
	}
	page := expectStatus(t, do(t, a, "GET", fmt.Sprintf("/zoe/demo/issues/%d", num), nil, f.ck), 200, "issue page")
	mustContain(t, page, "issue page", fmt.Sprintf("GitHub #%d", it.GHNumber), fmt.Sprintf("/zoe-gh/demo/issues/%d", it.GHNumber))
	// The sync doesn't push it again, and doesn't bring it back as a new issue.
	f.sync(t)
	f.sync(t)
	if n := f.gh.hit("POST /repos/zoe-gh/demo/issues"); n != 1 {
		t.Fatalf("issue created on GitHub %d times", n)
	}
	if all, _, _ := a.db.Issues(ctx, rid, store.IssueQuery{}, 0, 100); len(all) != 4 {
		t.Fatalf("Atelier has %d issues, want 4", len(all))
	}

	// Comments: written here go to GitHub #N; written there come to Atelier #num.
	expectStatus(t, do(t, a, "POST", fmt.Sprintf("/zoe/demo/issues/%d/comment", num), url.Values{"body": {"From Atelier"}}, f.ck), 303, "comment")
	f.wait()
	f.gh.mu.Lock()
	var mine tether.Comment
	for _, c := range f.gh.comments[tetherTwin] {
		if c.Body == "From Atelier" {
			mine = c
		}
	}
	f.gh.comments[tetherTwin] = append(f.gh.comments[tetherTwin], tether.Comment{ID: 7001, Body: "From GitHub", User: tether.User{Login: "carol"},
		CreatedAt: time.Now(), UpdatedAt: time.Now(), IssueURL: fmt.Sprintf("%s/repos/%s/issues/%d", f.gh.srv.URL, tetherTwin, it.GHNumber)})
	f.gh.mu.Unlock()
	if mine.IssueNumber() != it.GHNumber {
		t.Fatalf("Atelier's comment went to GitHub #%d, want #%d", mine.IssueNumber(), it.GHNumber)
	}
	f.sync(t)
	f.sync(t)
	local := f.issue(t, num)
	cs, _ := a.db.IssueComments(ctx, local.ID)
	if len(cs) != 2 || cs[1].Body != "From GitHub" || cs[1].AuthorName != "github:carol" {
		t.Fatalf("comments on Atelier #%d = %+v", num, cs)
	}

	// Closed in Atelier → closed on GitHub; reopened on GitHub → reopened here.
	expectStatus(t, do(t, a, "POST", fmt.Sprintf("/zoe/demo/issues/%d/state", num), url.Values{"action": {"close"}}, f.ck), 303, "close")
	f.sync(t)
	if gi := f.gh.ghIssue(t, tetherTwin, it.GHNumber); gi.State != "closed" || gi.StateReason != "completed" {
		t.Fatalf("GitHub after close here = %s/%s", gi.State, gi.StateReason)
	}
	f.gh.editIssue(tetherTwin, it.GHNumber, time.Now().Add(time.Minute), func(is *tether.Issue) { is.State = "open" })
	f.sync(t)
	if is := f.issue(t, num); is.State != "open" {
		t.Fatalf("Atelier after reopen on GitHub = %s", is.State)
	}

	// Opened here while GitHub opens its own #N at the same time: inbound
	// first, so both land and the mapping keeps them apart.
	other, _ := a.db.CreateIssue(ctx, rid, "Local twin race", "", ActorOwner, "zoe")
	f.gh.mu.Lock()
	ghN := f.gh.nextNumberLocked(tetherTwin)
	f.gh.issues[tetherTwin] = append(f.gh.issues[tetherTwin], tether.Issue{Number: ghN, Title: "GitHub race", State: "open",
		User: tether.User{Login: "dave"}, CreatedAt: time.Now(), UpdatedAt: time.Now().Add(2 * time.Minute)})
	f.gh.mu.Unlock()
	f.sync(t)
	a1, _ := a.db.TetherItem(ctx, rid, other.Number)
	g1, _ := a.db.TetherItemByGH(ctx, rid, ghN)
	if a1.GHNumber == 0 || g1.Number == 0 || a1.GHNumber == ghN || g1.Number == other.Number {
		t.Fatalf("race mapping: Atelier #%d→GitHub #%d, GitHub #%d→Atelier #%d", other.Number, a1.GHNumber, ghN, g1.Number)
	}
	if f.gh.ghIssue(t, tetherTwin, a1.GHNumber).Title != "Local twin race" || f.issue(t, g1.Number).Title != "GitHub race" {
		t.Error("race: titles crossed")
	}
}

func TestTetherReviewsShownOnTimeline(t *testing.T) {
	f := newIssueSyncFixture(t)
	a, ctx := f.a, context.Background()
	rid := f.repoID(t)
	f.link(t, tetherTwin, "atelier", false)
	runGit(t, f.work, "checkout", "-q", "-b", "feat")
	pullsCommit(t, f.work, "a.go", "package a\n", "Add A")
	runGit(t, f.work, "push", "-q", "origin", "feat")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"feat"}, "title": {"Add A"}}, f.ck), 303, "open PR")
	f.wait()
	n, _, err := tether.Get().MirrorPull(ctx, rid, 1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	f.gh.mu.Lock()
	f.gh.prReviews[tetherTwin] = map[int64][]tether.Review{int64(n): {
		{ID: 1, User: tether.User{Login: "alice"}, State: "APPROVED", SubmittedAt: now},
		{ID: 2, User: tether.User{Login: "coderabbitai[bot]"}, State: "CHANGES_REQUESTED", Body: "Please add **tests**.", SubmittedAt: now},
		{ID: 3, User: tether.User{Login: "bob"}, State: "COMMENTED", SubmittedAt: now}, // line comments only: no body
		{ID: 4, User: tether.User{Login: "bob"}, State: "PENDING", Body: "draft", SubmittedAt: now},
		{ID: 5, User: tether.User{Login: "carol"}, State: "COMMENTED", Body: "<img src=x onerror=alert(1)> Looks fine", SubmittedAt: now},
	}}
	f.gh.pulls[tetherTwin][int64(n)].UpdatedAt = now
	f.gh.mu.Unlock()
	f.sync(t)
	p, _ := a.db.PullByNumber(ctx, rid, 1)
	evs, _ := a.db.PullEvents(ctx, p.ID)
	var got []string
	for _, e := range evs {
		if e.Kind == "tether_review" {
			got = append(got, e.ActorName+" "+e.Data)
		}
	}
	if strings.Join(got, "; ") != "github:alice approved; github:coderabbitai[bot] changes_requested; github:carol commented" {
		t.Fatalf("review events = %q", got)
	}
	cs, _ := a.db.PullComments(ctx, p.ID)
	if len(cs) != 2 || cs[0].Body != "Please add **tests**." || cs[0].AuthorName != "github:coderabbitai[bot]" {
		t.Fatalf("review bodies = %+v", cs)
	}
	page := expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/1", nil, f.ck), 200, "pull page")
	mustContain(t, page, "pull page", "approved these changes", "requested changes", "github:alice", "via Tether", "<strong>tests</strong>")
	if strings.Contains(page, "<img src=x") {
		t.Fatal("a review body reached the page unescaped")
	}
	// Unchanged on GitHub: reviews aren't asked for again, nothing doubles.
	hits := f.gh.hit(fmt.Sprintf("GET /repos/zoe-gh/demo/pulls/%d/reviews", n))
	f.sync(t)
	if f.gh.hit(fmt.Sprintf("GET /repos/zoe-gh/demo/pulls/%d/reviews", n)) != hits {
		t.Error("reviews fetched for an unchanged pull request")
	}
	f.gh.mu.Lock()
	f.gh.pulls[tetherTwin][int64(n)].UpdatedAt = time.Now().Add(time.Minute)
	f.gh.mu.Unlock()
	f.sync(t)
	if evs2, _ := a.db.PullEvents(ctx, p.ID); len(evs2) != len(evs) {
		t.Errorf("reviews shown twice: %d events, was %d", len(evs2), len(evs))
	}
}

func TestTetherCloseOnMergeIsOneCall(t *testing.T) {
	f := newIssueSyncFixture(t)
	a, ctx := f.a, context.Background()
	rid := f.repoID(t)
	f.link(t, tetherTwin, "atelier", false)
	if err := a.db.SetTetherReview(ctx, rid, true); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.work, "checkout", "-q", "-b", "feat")
	head := pullsCommit(t, f.work, "b.txt", "b\n", "B")
	runGit(t, f.work, "push", "-q", "origin", "feat")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"feat"}, "title": {"B"}}, f.ck), 303, "open PR")
	f.wait()
	f.a.apps().bg.Wait()
	it, err := a.db.TetherItem(ctx, rid, 1)
	if err != nil || it.Origin != "atelier" {
		t.Fatalf("not mirrored by Review on GitHub: %+v %v", it, err)
	}
	patch := fmt.Sprintf("PATCH /repos/zoe-gh/demo/pulls/%d", it.GHNumber)
	comment := fmt.Sprintf("POST /repos/zoe-gh/demo/issues/%d/comments", it.GHNumber)
	before, beforeC := f.gh.hit(patch), f.gh.hit(comment)
	rec := do(t, a, "POST", "/zoe/demo/pulls/1/merge", url.Values{"method": {"merge"}, "expected_head": {head}}, f.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("merge = %d", rec.Code)
	}
	f.wait()
	f.a.apps().bg.Wait()
	f.wait()
	if n := f.gh.hit(patch) - before; n != 1 {
		t.Errorf("GitHub pull request closed with %d calls, want 1", n)
	}
	if n := f.gh.hit(comment) - beforeC; n != 1 {
		t.Errorf("\"Merged in Atelier\" posted %d times, want 1", n)
	}
	f.gh.mu.Lock()
	st := f.gh.pulls[tetherTwin][it.GHNumber].State
	f.gh.mu.Unlock()
	p, _ := a.db.PullByNumber(ctx, rid, 1)
	if m, _ := a.db.TetherMirror(ctx, p.ID); st != "closed" || !m.Closed {
		t.Errorf("GitHub state %s, mirror closed %v", st, m.Closed)
	}
}

func TestTetherIssueSyncUsesOrgToken(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	const org = "Elyndra-Studios/sparky"
	f.gh.orgToken("Elyndra-Studios")
	f.gh.addRepo(org, true)
	f.connect(t)
	pullsCommit(t, f.work, "README.md", "# demo\n", "Initial")
	runGit(t, f.work, "push", "-q", "origin", "main")
	if _, err := a.tetherAddToken(ctx, f.me(t).ID, "Studio", fakeOrgToken, []string{"elyndra-studios"}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-time.Hour)
	f.gh.mu.Lock()
	f.gh.issues[org] = []tether.Issue{{Number: 1, Title: "Org issue", State: "open", User: tether.User{Login: "alice"}, CreatedAt: t0, UpdatedAt: t0,
		Labels: []tether.Label{{Name: "studio", Color: "123456"}}}}
	f.gh.mu.Unlock()
	f.link(t, org, "atelier", true)
	f.gh.resetTokenHits()
	is, _ := a.db.CreateIssue(ctx, f.repoID(t), "From Atelier", "", ActorOwner, "zoe")
	_ = a.db.UpdateIssue(ctx, f.issue(t, 1).ID, "Org issue (edited)", "", ActorOwner, "zoe")
	f.sync(t)
	if f.gh.tokenHits(fakeOrgToken, "POST", "/repos/Elyndra-Studios/sparky/issues") != 1 ||
		f.gh.tokenHits(fakeOrgToken, "PATCH", "/repos/Elyndra-Studios/sparky/issues/1") != 1 {
		t.Fatalf("org writes not made with the org token: %v", f.gh.byToken)
	}
	if it, err := a.db.TetherItem(ctx, f.repoID(t), is.Number); err != nil || it.Origin != "atelier" {
		t.Fatalf("org push = %+v %v", it, err)
	}
	f.gh.mu.Lock()
	for k := range f.gh.byToken {
		if strings.HasPrefix(k, fakeGHToken+" ") && strings.Contains(k, "Elyndra-Studios") {
			t.Errorf("the main token was used for the org: %s", k)
		}
	}
	f.gh.mu.Unlock()
}

func TestTetherIssueSyncPrivacy(t *testing.T) {
	f := newIssueSyncFixture(t)
	a, ctx := f.a, context.Background()
	rid := f.repoID(t)
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// A second person, without GitHub connected, owns the link: nothing
	// uses the owner's tokens, whatever happens in the repository.
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.db.SaveTetherLink(ctx, store.TetherLink{RepoID: rid, PersonID: sam.ID, GHRepo: tetherTwin, Home: store.TetherHomeAtelier, SyncIssues: true}); err != nil {
		t.Fatal(err)
	}
	f.gh.resetTokenHits()
	expectStatus(t, do(t, a, "POST", "/zoe/demo/issues/new", url.Values{"title": {"Secret plans"}}, f.ck), 303, "new issue")
	f.wait()
	l := f.sync(t)
	f.gh.mu.Lock()
	used := len(f.gh.byToken)
	f.gh.mu.Unlock()
	if used != 0 {
		t.Fatalf("GitHub was called for sam's link: %v", f.gh.byToken)
	}
	if !strings.Contains(l.SyncError, "connect GitHub") {
		t.Errorf("sync error = %q", l.SyncError)
	}
	if _, err := a.db.TetherItem(ctx, rid, 1); err == nil {
		t.Error("the issue reached GitHub through sam's link")
	}

	// Back with the owner, and GitHub refusing: errors are logged, the token never is.
	if err := a.db.SaveTetherLink(ctx, store.TetherLink{RepoID: rid, PersonID: f.me(t).ID, GHRepo: "zoe-gh/gone", Home: store.TetherHomeAtelier, SyncIssues: true}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/issues/new", url.Values{"title": {"Another"}}, f.ck), 303, "new issue 2")
	f.wait()
	l = f.sync(t)
	if strings.Contains(logs.String(), fakeGHToken) || strings.Contains(l.SyncError, fakeGHToken) {
		t.Fatal("the token was logged")
	}
	if !strings.Contains(logs.String(), "pushing issue") {
		t.Errorf("expected a logged push failure, got %q", logs.String())
	}
}
