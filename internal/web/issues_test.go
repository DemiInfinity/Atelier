package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
)

type issuesFixture struct {
	a      *App
	repo   store.Repo
	cookie map[string]string
}

func newIssuesFixture(t *testing.T) issuesFixture {
	t.Helper()
	a := newTestApp(t)
	ctx := context.Background()
	o, err := a.db.CreateOwner(ctx, "zoe", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.git.Init(ctx, "zoe", "demo"); err != nil {
		t.Fatal(err)
	}
	r, err := a.db.CreateRepo(ctx, o.ID, "demo", "", true)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := a.db.CreateSession(ctx, time.Hour)
	return issuesFixture{a: a, repo: r, cookie: map[string]string{"Cookie": sessionCookie + "=" + sess}}
}

func (f issuesFixture) get(t *testing.T, path string, want int) string {
	t.Helper()
	rec := do(t, f.a, "GET", path, nil, f.cookie)
	if rec.Code != want {
		t.Fatalf("GET %s = %d, want %d\n%.800s", path, rec.Code, want, rec.Body)
	}
	return rec.Body.String()
}

func (f issuesFixture) post(t *testing.T, path string, form url.Values, want int) string {
	t.Helper()
	rec := do(t, f.a, "POST", path, form, f.cookie)
	if rec.Code != want {
		t.Fatalf("POST %s = %d, want %d\n%.800s", path, rec.Code, want, rec.Body)
	}
	return rec.Header().Get("Location")
}

func TestIssuesHandlers(t *testing.T) {
	f := newIssuesFixture(t)
	ctx := context.Background()

	// Signed out: sent to sign in.
	if rec := do(t, f.a, "GET", "/zoe/demo/issues", nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("signed-out issues = %d", rec.Code)
	}

	// First visit seeds the default labels and shows the empty state.
	body := f.get(t, "/zoe/demo/issues", 200)
	if !strings.Contains(body, "No open issues") || !strings.Contains(body, `href="/zoe/demo/labels.css"`) {
		t.Errorf("empty list page wrong:\n%.2000s", body)
	}
	labels, _ := f.a.db.Labels(ctx, f.repo.ID)
	if len(labels) != len(store.DefaultLabels) {
		t.Fatalf("seeded %d labels", len(labels))
	}
	bug := labels[0]
	css := f.get(t, "/zoe/demo/labels.css", 200)
	if !strings.Contains(css, fmt.Sprintf(".lbl-%d{background:", bug.ID)) {
		t.Errorf("labels.css missing bug rule:\n%s", css)
	}

	f.get(t, "/zoe/demo/issues/new", 200)
	// Missing title re-renders with an error.
	f.post(t, "/zoe/demo/issues/new", url.Values{"title": {" "}, "body": {"x"}}, http.StatusUnprocessableEntity)

	ms, _ := f.a.db.CreateMilestone(ctx, f.repo.ID, "v1.0", "", nil)
	loc := f.post(t, "/zoe/demo/issues/new", url.Values{
		"title": {"Crash on start"}, "body": {"It **crashes**. See #2."},
		"label": {fmt.Sprint(bug.ID)}, "milestone": {fmt.Sprint(ms.ID)},
	}, http.StatusSeeOther)
	if loc != "/zoe/demo/issues/1" {
		t.Fatalf("created issue redirect = %q", loc)
	}
	// #2 is taken by a "pull request" (shared numbering) and must 404 as an issue.
	if n, _ := f.a.db.NextNumber(ctx, f.repo.ID); n != 2 {
		t.Fatalf("NextNumber = %d", n)
	}
	f.get(t, "/zoe/demo/issues/2", 404)
	f.get(t, "/zoe/demo/issues/abc", 404)
	f.post(t, "/zoe/demo/issues/new", url.Values{"title": {"Second issue"}}, http.StatusSeeOther) // #3

	page := f.get(t, "/zoe/demo/issues/1", 200)
	for _, want := range []string{"Crash on start", "<strong>crashes</strong>", fmt.Sprintf("lbl lbl-%d", bug.ID), "v1.0", "added this to the"} {
		if !strings.Contains(page, want) {
			t.Errorf("issue page missing %q", want)
		}
	}
	if strings.Contains(page, `href="/zoe/demo/issues/2"`) {
		t.Error("#2 (a pull request) should not be linked as an issue")
	}

	// Comment, then comment and close as not planned in one go.
	loc = f.post(t, "/zoe/demo/issues/1/comment", url.Values{"body": {"Same here, and #3 too"}, "action": {"comment"}}, http.StatusSeeOther)
	if !strings.HasPrefix(loc, "/zoe/demo/issues/1#comment-") {
		t.Errorf("comment redirect = %q", loc)
	}
	f.post(t, "/zoe/demo/issues/1/comment", url.Values{"body": {""}, "action": {"comment"}}, http.StatusUnprocessableEntity)
	f.post(t, "/zoe/demo/issues/1/comment", url.Values{"body": {"Won't fix"}, "action": {"close_not_planned"}}, http.StatusSeeOther)
	is, _ := f.a.db.IssueByNumber(ctx, f.repo.ID, 1)
	if is.State != "closed" || is.StateReason != store.ReasonNotPlanned || is.Comments != 2 {
		t.Fatalf("after close: %+v", is)
	}
	page = f.get(t, "/zoe/demo/issues/1", 200)
	if !strings.Contains(page, `<a href="/zoe/demo/issues/3" class="issue-ref">#3</a>`) {
		t.Errorf("#3 not linked in comment:\n%.3000s", page)
	}
	if !strings.Contains(page, "closed this as not planned") || !strings.Contains(page, "Reopen issue") {
		t.Error("timeline/close state missing")
	}

	// Reopen via the state endpoint, then close as completed with an empty comment.
	f.post(t, "/zoe/demo/issues/1/state", url.Values{"action": {"reopen"}}, http.StatusSeeOther)
	f.post(t, "/zoe/demo/issues/1/comment", url.Values{"action": {"close_completed"}}, http.StatusSeeOther)
	is, _ = f.a.db.IssueByNumber(ctx, f.repo.ID, 1)
	if is.State != "closed" || is.StateReason != store.ReasonCompleted || is.Comments != 2 {
		t.Fatalf("after reopen+close: %+v", is)
	}
	f.post(t, "/zoe/demo/issues/1/state", url.Values{"action": {"reopen"}}, http.StatusSeeOther)

	// Edit title and body separately.
	f.post(t, "/zoe/demo/issues/1/edit", url.Values{"title": {"Crash on launch"}}, http.StatusSeeOther)
	f.post(t, "/zoe/demo/issues/1/edit", url.Values{"body": {"new body"}}, http.StatusSeeOther)
	is, _ = f.a.db.IssueByNumber(ctx, f.repo.ID, 1)
	if is.Title != "Crash on launch" || is.Body != "new body" {
		t.Errorf("after edit: %q / %q", is.Title, is.Body)
	}
	f.post(t, "/zoe/demo/issues/1/edit", url.Values{"title": {""}}, http.StatusUnprocessableEntity)
	if page := f.get(t, "/zoe/demo/issues/1", 200); !strings.Contains(page, "changed the title") {
		t.Error("rename event missing")
	}

	// Edit and delete own comments; others' comments are protected.
	cs, _ := f.a.db.IssueComments(ctx, is.ID)
	f.post(t, fmt.Sprintf("/zoe/demo/issues/1/comments/%d/edit", cs[0].ID), url.Values{"body": {"edited text"}}, http.StatusSeeOther)
	if c, _ := f.a.db.IssueCommentByID(ctx, cs[0].ID); c.Body != "edited text" {
		t.Errorf("comment not edited: %q", c.Body)
	}
	f.post(t, fmt.Sprintf("/zoe/demo/issues/1/comments/%d/delete", cs[1].ID), nil, http.StatusSeeOther)
	botc, _ := f.a.db.AddIssueComment(ctx, is.ID, "beep", "bot", "helper")
	f.post(t, fmt.Sprintf("/zoe/demo/issues/1/comments/%d/delete", botc.ID), nil, http.StatusForbidden)
	f.post(t, fmt.Sprintf("/zoe/demo/issues/3/comments/%d/edit", cs[0].ID), url.Values{"body": {"x"}}, http.StatusNotFound)

	// Sidebar pickers.
	f.post(t, "/zoe/demo/issues/1/labels", url.Values{"label": {fmt.Sprint(labels[1].ID), fmt.Sprint(labels[2].ID)}}, http.StatusSeeOther)
	is, _ = f.a.db.IssueByNumber(ctx, f.repo.ID, 1)
	if len(is.Labels) != 2 {
		t.Errorf("labels after picker: %+v", is.Labels)
	}
	f.post(t, "/zoe/demo/issues/1/milestone", url.Values{"milestone": {"0"}}, http.StatusSeeOther)
	if is, _ = f.a.db.IssueByNumber(ctx, f.repo.ID, 1); is.MilestoneID != 0 {
		t.Error("milestone not cleared")
	}

	// Close #3 so the filters have something to tell apart.
	f.post(t, "/zoe/demo/issues/3/state", url.Values{"action": {"close"}}, http.StatusSeeOther)

	list := f.get(t, "/zoe/demo/issues", 200)
	if !strings.Contains(list, "1 Open") || !strings.Contains(list, "1 Closed") || !strings.Contains(list, "Crash on launch") || strings.Contains(list, "Second issue") {
		t.Errorf("open list wrong:\n%.4000s", list)
	}
	list = f.get(t, "/zoe/demo/issues?q=is%3Aclosed", 200)
	if !strings.Contains(list, "Second issue") || strings.Contains(list, "Crash on launch") {
		t.Error("closed filter wrong")
	}
	list = f.get(t, "/zoe/demo/issues?q="+url.QueryEscape("label:"+labels[1].Name), 200)
	if !strings.Contains(list, "Crash on launch") {
		t.Error("label filter wrong")
	}
	list = f.get(t, "/zoe/demo/issues?q="+url.QueryEscape("label:bug"), 200)
	if !strings.Contains(list, "No issues match this search") {
		t.Error("label:bug should match nothing now")
	}
	list = f.get(t, "/zoe/demo/issues?q=&sort=oldest", 200)
	if i, j := strings.Index(list, "Crash on launch"), strings.Index(list, "Second issue"); i < 0 || j < 0 || i > j {
		t.Error("oldest sort wrong")
	}
	list = f.get(t, "/zoe/demo/issues?q=launch", 200)
	if !strings.Contains(list, "Crash on launch") || strings.Contains(list, "Second issue") {
		t.Error("text search wrong")
	}
}

func TestIssuesPagination(t *testing.T) {
	f := newIssuesFixture(t)
	ctx := context.Background()
	for i := 1; i <= issuesPerPage+2; i++ {
		f.a.db.CreateIssue(ctx, f.repo.ID, fmt.Sprintf("Issue number %03d", i), "", "owner", "zoe")
	}
	p1 := f.get(t, "/zoe/demo/issues?sort=oldest", 200)
	if !strings.Contains(p1, "Issue number 001") || strings.Contains(p1, "Issue number 026") || !strings.Contains(p1, `rel="next"`) {
		t.Error("page 1 wrong")
	}
	p2 := f.get(t, "/zoe/demo/issues?q=is%3Aopen&sort=oldest&page=2", 200)
	if !strings.Contains(p2, "Issue number 026") || !strings.Contains(p2, "Issue number 027") || strings.Contains(p2, `rel="next"`) {
		t.Error("page 2 wrong")
	}
}

func TestLabelsAndMilestonesPages(t *testing.T) {
	f := newIssuesFixture(t)
	ctx := context.Background()
	f.get(t, "/zoe/demo/labels", 200)

	f.post(t, "/zoe/demo/labels", url.Values{"name": {"perf"}, "color": {"#00FF88"}, "description": {"Speed"}}, http.StatusSeeOther)
	f.post(t, "/zoe/demo/labels", url.Values{"name": {"perf"}, "color": {"#00FF88"}}, http.StatusUnprocessableEntity)
	f.post(t, "/zoe/demo/labels", url.Values{"name": {"bad"}, "color": {"red; } body {"}}, http.StatusUnprocessableEntity)
	ls, _ := f.a.db.Labels(ctx, f.repo.ID)
	var perf store.Label
	for _, l := range ls {
		if l.Name == "perf" {
			perf = l
		}
	}
	if perf.Color != "00ff88" {
		t.Fatalf("perf label = %+v", perf)
	}
	f.post(t, fmt.Sprintf("/zoe/demo/labels/%d/edit", perf.ID), url.Values{"name": {"performance"}, "color": {"#112233"}}, http.StatusSeeOther)
	f.post(t, "/zoe/demo/labels/99999/edit", url.Values{"name": {"x"}, "color": {"#112233"}}, http.StatusNotFound)
	if page := f.get(t, "/zoe/demo/labels", 200); !strings.Contains(page, "performance") {
		t.Error("renamed label missing")
	}
	f.post(t, fmt.Sprintf("/zoe/demo/labels/%d/delete", perf.ID), nil, http.StatusSeeOther)
	if _, err := f.a.db.LabelByID(ctx, f.repo.ID, perf.ID); err == nil {
		t.Error("label not deleted")
	}

	f.get(t, "/zoe/demo/milestones", 200)
	f.get(t, "/zoe/demo/milestones/new", 200)
	f.post(t, "/zoe/demo/milestones/new", url.Values{"title": {"v1"}, "due": {"31/12/2026"}}, http.StatusUnprocessableEntity)
	f.post(t, "/zoe/demo/milestones/new", url.Values{"title": {"v1"}, "due": {"2026-12-31"}, "description": {"First **release**"}}, http.StatusSeeOther)
	ms, _ := f.a.db.Milestones(ctx, f.repo.ID, "open")
	if len(ms) != 1 || ms[0].DueOn == nil || ms[0].DueOn.Format("2006-01-02") != "2026-12-31" {
		t.Fatalf("milestones %+v", ms)
	}
	id := ms[0].ID
	a, _ := f.a.db.CreateIssue(ctx, f.repo.ID, "A", "", "owner", "zoe")
	b, _ := f.a.db.CreateIssue(ctx, f.repo.ID, "B", "", "owner", "zoe")
	f.a.db.SetIssueMilestone(ctx, a.ID, id, "owner", "zoe")
	f.a.db.SetIssueMilestone(ctx, b.ID, id, "owner", "zoe")
	f.a.db.CloseIssue(ctx, a.ID, "owner", "zoe", "")
	page := f.get(t, "/zoe/demo/milestones", 200)
	if !strings.Contains(page, `value="50"`) || !strings.Contains(page, "<strong>release</strong>") || !strings.Contains(page, "31 December 2026") {
		t.Errorf("milestones page wrong:\n%.3000s", page)
	}
	f.get(t, fmt.Sprintf("/zoe/demo/milestones/%d/edit", id), 200)
	f.post(t, fmt.Sprintf("/zoe/demo/milestones/%d/edit", id), url.Values{"title": {"v1.0"}, "due": {""}}, http.StatusSeeOther)
	f.post(t, fmt.Sprintf("/zoe/demo/milestones/%d/state", id), url.Values{"action": {"close"}}, http.StatusSeeOther)
	if page := f.get(t, "/zoe/demo/milestones?state=closed", 200); !strings.Contains(page, "v1.0") || !strings.Contains(page, "No due date") {
		t.Error("closed milestone missing")
	}
	f.post(t, fmt.Sprintf("/zoe/demo/milestones/%d/delete", id), nil, http.StatusSeeOther)
	f.get(t, fmt.Sprintf("/zoe/demo/milestones/%d/edit", id), 404)
	f.get(t, "/zoe/nope/milestones", 404)
}

func TestIssueMarkdownLinks(t *testing.T) {
	exists := func(n int64) bool { return n == 1 || n == 12 || n == 123 }
	cases := []struct{ in, want, notWant string }{
		{"Fixes #123.", `Fixes <a href="/zoe/demo/issues/123" class="issue-ref">#123</a>.`, ""},
		{"#1 and #12", `<a href="/zoe/demo/issues/1" class="issue-ref">#1</a> and <a href="/zoe/demo/issues/12" class="issue-ref">#12</a>`, ""},
		{"see (#12)", `(<a href="/zoe/demo/issues/12" class="issue-ref">#12</a>)`, ""},
		{"#99 is missing", "#99 is missing", "issue-ref"},
		{"`#123` in code", "<code>#123</code>", "issue-ref"},
		{"```\n#123\n```", "#123", "issue-ref"},
		{"[link #123](https://x.test)", `<a href="https://x.test">link #123</a>`, "issue-ref"},
		{"abc#123 and #123abc", "abc#123 and #123abc", "issue-ref"},
		{"**#1**", `<strong><a href="/zoe/demo/issues/1" class="issue-ref">#1</a></strong>`, ""},
		{"<script>alert(1)</script> #1", "raw HTML omitted", "<script>"},
		{"<b>x</b> #1 <i>", `<a href="/zoe/demo/issues/1" class="issue-ref">#1</a>`, "<b>"},
		{"line one #1\nline two", "<a href=\"/zoe/demo/issues/1\" class=\"issue-ref\">#1</a>\nline two", ""},
		{"# Heading #12", `<h1>Heading <a href="/zoe/demo/issues/12" class="issue-ref">#12</a></h1>`, ""},
	}
	for _, c := range cases {
		got := string(renderIssueMarkdown(c.in, "/zoe/demo", exists))
		if !strings.Contains(got, c.want) {
			t.Errorf("%q:\n got  %s\n want %s", c.in, got, c.want)
		}
		if c.notWant != "" && strings.Contains(got, c.notWant) {
			t.Errorf("%q: should not contain %q: %s", c.in, c.notWant, got)
		}
	}
}

func TestLabelColorsContrast(t *testing.T) {
	for _, hex := range []string{"000000", "ffffff", "d73a4a", "a2eeef", "0075ca", "f2b85c", "5319e7", "333333"} {
		bg, _, fg := labelColors(hex)
		if c := contrast(parseHex(fg), parseHex(bg)); c < 4.5 {
			t.Errorf("%s: text %s on %s has contrast %.2f", hex, fg, bg, c)
		}
	}
	if rule := labelRule(".lbl-1", "not a colour"); !strings.HasPrefix(rule, ".lbl-1{background:#") {
		t.Errorf("bad colour rule: %s", rule)
	}
}
