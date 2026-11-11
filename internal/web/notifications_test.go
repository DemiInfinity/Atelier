package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
)

func notifCount(t *testing.T, f issuesFixture) (int, string) {
	t.Helper()
	body := f.get(t, "/notifications/count", 200)
	var d struct {
		Unread int    `json:"unread"`
		Label  string `json:"label"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("count JSON: %v: %s", err, body)
	}
	return d.Unread, d.Label
}

func zoe(t *testing.T, f issuesFixture) store.Owner {
	t.Helper()
	me, err := f.a.db.PersonalOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return me
}

func TestNotificationsNoSelfButTokensAndBots(t *testing.T) {
	f := newIssuesFixture(t)
	ctx := context.Background()
	me := zoe(t, f)

	// The owner's own clicks don't notify them.
	loc := f.post(t, "/zoe/demo/issues/new", url.Values{"title": {"Crash on start"}, "body": {"boom"}}, 303)
	f.post(t, loc+"/comment", url.Values{"body": {"more info"}}, 303)
	f.post(t, loc+"/state", url.Values{"action": {"close"}}, 303)
	if n, label := notifCount(t, f); n != 0 || label != "" {
		t.Fatalf("own actions notified: %d %q", n, label)
	}

	// The same person acting through an access token (e.g. Claude over MCP) does.
	is, err := f.a.db.IssueByNumber(ctx, f.repo.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	v := &repoView{Repo: f.repo, Base: "/zoe/demo"}
	tok := Actor{Kind: ActorToken, Name: "claude-mcp", PersonID: me.ID, Scopes: []string{"repo:write"}}
	f.a.emit(ctx, issueEvent(EventIssueCommented, v, is, tok, "I found the bug", "#comment-9"))
	if n, label := notifCount(t, f); n != 1 || label != "1" {
		t.Fatalf("token comment: unread %d %q, want 1", n, label)
	}
	// …and a bot on the same issue coalesces into the same row.
	f.a.emit(ctx, issueEvent(EventIssueReopened, v, is, Actor{Kind: "bot", Name: "triage-bot"}, ""))
	list, _ := f.a.db.Notifications(ctx, store.NotificationQuery{PersonID: me.ID})
	if len(list) != 1 || list[0].Kind != EventIssueReopened || list[0].ActorName != "triage-bot" {
		t.Fatalf("after bot: %+v", list)
	}
	if list[0].URL != "/zoe/demo/issues/1" || list[0].Reason != ReasonSubscribed {
		t.Errorf("url/reason = %q %q", list[0].URL, list[0].Reason)
	}

	// Pull request events go through the same path.
	p := store.Pull{Number: 2, Title: "Fix crash", Base: "main"}
	f.a.emit(ctx, pullEvent(EventPullMerged, v, p, tok, "into main"))
	f.a.emit(ctx, pullEvent(EventPullNote, v, p, Actor{Kind: ActorOwner, Name: "zoe", PersonID: me.ID}, "self note", "/files#note-1"))
	if n, _ := notifCount(t, f); n != 2 {
		t.Fatalf("unread after pull events = %d, want 2", n)
	}
	got, _ := f.a.db.Notifications(ctx, store.NotificationQuery{PersonID: me.ID, SubjectTypes: []string{"pull"}})
	if len(got) != 1 || got[0].Kind != EventPullMerged {
		t.Errorf("own note should not have replaced the token's merge notification: %+v", got)
	}

	// Unsafe links are dropped.
	f.a.emit(ctx, Event{Kind: "issue.commented", Repo: f.repo, Number: 77, Title: "x", URL: "//evil.example/", Actor: tok})
	got, _ = f.a.db.Notifications(ctx, store.NotificationQuery{PersonID: me.ID, SubjectTypes: []string{"issue"}, Limit: 1})
	if got[0].URL != "" {
		t.Errorf("unsafe URL kept: %q", got[0].URL)
	}
}

func TestNotificationsRespectRepoAccess(t *testing.T) {
	f := newIssuesFixture(t)
	ctx := context.Background()
	me := zoe(t, f)
	visitor, err := f.a.db.CreateOwner(ctx, "visitor", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	bot := Actor{Kind: "bot", Name: "ci-bot"}
	ev := Event{Kind: EventIssueOpened, RepoID: f.repo.ID, Number: 5, Title: "Secret plans", URL: "/zoe/demo/issues/5", Actor: bot}
	f.a.emit(ctx, ev)
	ev.Recipients = []int64{visitor.ID} // even when named explicitly
	ev.Number = 6
	f.a.emit(ctx, ev)

	if n, _ := f.a.db.UnreadNotificationCount(ctx, visitor.ID); n != 0 {
		t.Errorf("visitor got %d notifications about a private repo they can't read", n)
	}
	if n, _ := f.a.db.UnreadNotificationCount(ctx, me.ID); n != 1 {
		t.Errorf("owner unread = %d, want 1", n)
	}

	// Instance-wide (repo-less) events go to the owner only.
	f.a.emit(ctx, Event{Kind: EventBackupFailed, Title: "Backup failed", URL: "/settings/backups", Actor: Actor{Kind: ActorSystem, Name: "backups"}})
	if n, _ := f.a.db.UnreadNotificationCount(ctx, visitor.ID); n != 0 {
		t.Errorf("visitor got a system notification")
	}

	// The visitor's inbox page shows nothing of the owner's, and they can't
	// open or complete the owner's notifications.
	sess, _ := f.a.db.CreateSessionFor(ctx, visitor.ID, time.Hour)
	vf := issuesFixture{a: f.a, repo: f.repo, cookie: map[string]string{"Cookie": sessionCookie + "=" + sess}}
	body := vf.get(t, "/notifications", 200)
	if strings.Contains(body, "Secret plans") {
		t.Error("visitor inbox shows the owner's private notification")
	}
	mine, _ := f.a.db.Notifications(ctx, store.NotificationQuery{PersonID: me.ID})
	vf.post(t, fmt.Sprintf("/notifications/%d/done", mine[0].ID), url.Values{}, 303)
	if n, _ := f.a.db.UnreadNotificationCount(ctx, me.ID); n != 2 {
		t.Errorf("visitor marked the owner's notification done (owner unread %d)", n)
	}
	if loc := vf.get303(t, fmt.Sprintf("/notifications/%d", mine[0].ID)); loc != "/notifications?msg=missing" {
		t.Errorf("visitor opening owner's notification went to %q", loc)
	}
}

func (f issuesFixture) get303(t *testing.T, path string) string {
	t.Helper()
	rec := do(t, f.a, "GET", path, nil, f.cookie)
	if rec.Code != 303 {
		t.Fatalf("GET %s = %d, want 303", path, rec.Code)
	}
	return rec.Header().Get("Location")
}

func TestNotificationsPageFiltersAndDone(t *testing.T) {
	f := newIssuesFixture(t)
	ctx := context.Background()
	v := &repoView{Repo: f.repo, Base: "/zoe/demo"}
	agent := Actor{Kind: "agent", Name: "review-helper"}
	f.a.emit(ctx, issueEvent(EventIssueOpened, v, store.Issue{Number: 1, Title: "Agent issue"}, agent, ""))
	f.a.emit(ctx, pullEvent(EventPullCommented, v, store.Pull{Number: 2, Title: "Bot pull"}, Actor{Kind: "bot", Name: "coderabbit"}, "nit"))
	f.a.emit(ctx, Event{Kind: EventBackupFailed, Title: "Backup failed", Detail: "disk full", URL: "/settings/backups",
		Actor: Actor{Kind: ActorSystem, Name: "backups"}})

	all := f.get(t, "/notifications", 200)
	for _, want := range []string{"Agent issue", "Bot pull", "Backup failed", "disk full", "zoe/demo · #1", `data-nf-bell`, "Mark all as done", "nf-agent"} {
		if !strings.Contains(all, want) {
			t.Errorf("inbox missing %q", want)
		}
	}
	check := func(q string, want, notWant []string) {
		t.Helper()
		body := f.get(t, "/notifications"+q, 200)
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q", q, w)
			}
		}
		for _, w := range notWant {
			if strings.Contains(body, w) {
				t.Errorf("%s: should not show %q", q, w)
			}
		}
	}
	check("?filter=issues", []string{"Agent issue"}, []string{"Bot pull", "Backup failed"})
	check("?filter=pulls", []string{"Bot pull"}, []string{"Agent issue", "Backup failed"})
	check("?filter=agents", []string{"Agent issue", "Bot pull"}, []string{"Backup failed"})
	check("?filter=system", []string{"Backup failed"}, []string{"Agent issue", "Bot pull"})
	check("?filter=actions", nil, []string{"Agent issue", "arrive with Phase 3"})
	check("?filter=tether", nil, []string{"Agent issue", "arrive with Phase 4"})
	check(fmt.Sprintf("?repo=%d", f.repo.ID), []string{"Agent issue", "Bot pull"}, []string{"Backup failed"})

	// Opening one marks it read and goes to the subject.
	list, _ := f.a.db.Notifications(ctx, store.NotificationQuery{PersonID: zoe(t, f).ID, SubjectTypes: []string{"issue"}})
	if loc := f.get303(t, fmt.Sprintf("/notifications/%d", list[0].ID)); loc != "/zoe/demo/issues/1" {
		t.Errorf("open went to %q", loc)
	}
	check("?unread=1", []string{"Bot pull", "Backup failed"}, []string{"Agent issue"})
	if n, _ := notifCount(t, f); n != 2 {
		t.Errorf("unread after open = %d, want 2", n)
	}

	// Per-row done returns to the same view; mark all done follows the filter.
	if loc := f.post(t, fmt.Sprintf("/notifications/%d/done", list[0].ID), url.Values{"back": {"/notifications?filter=issues"}}, 303); loc != "/notifications?filter=issues" {
		t.Errorf("done redirect = %q", loc)
	}
	if loc := f.post(t, fmt.Sprintf("/notifications/%d/done", list[0].ID), url.Values{"back": {"https://evil.example/"}}, 303); loc != "/notifications" {
		t.Errorf("done redirect with foreign back = %q", loc)
	}
	check("?view=done", []string{"Agent issue"}, []string{"Bot pull"})
	f.post(t, "/notifications/done", url.Values{"filter": {"system"}}, 303)
	check("", []string{"Bot pull"}, []string{"Backup failed", "Agent issue"})
	f.post(t, "/notifications/done", url.Values{}, 303)
	check("", []string{"All caught up"}, []string{"Bot pull"})
	if n, label := notifCount(t, f); n != 0 || label != "" {
		t.Errorf("bell after mark all done = %d %q", n, label)
	}
}

func TestNotificationsPaginationAndBellCap(t *testing.T) {
	f := newIssuesFixture(t)
	ctx := context.Background()
	me := zoe(t, f)
	for i := 1; i <= 120; i++ {
		if _, err := f.a.db.Notify(ctx, store.NewNotification{PersonID: me.ID, Kind: "issue.opened", RepoID: f.repo.ID,
			SubjectType: "issue", Number: int64(i), Title: fmt.Sprintf("Issue number %d", i), ActorKind: "bot", ActorName: "b"}); err != nil {
			t.Fatal(err)
		}
	}
	if n, label := notifCount(t, f); n != 120 || label != "99+" {
		t.Errorf("bell = %d %q, want 120 99+", n, label)
	}
	page1 := f.get(t, "/notifications", 200)
	if strings.Count(page1, `class="list-row nf-row`) != notificationsPerPage || !strings.Contains(page1, `href="/notifications?page=2"`) {
		t.Errorf("page 1 should have %d rows and a next link", notificationsPerPage)
	}
	page5 := f.get(t, "/notifications?page=5", 200)
	if strings.Count(page5, `class="list-row nf-row`) != 20 || strings.Contains(page5, `rel="next"`) {
		t.Errorf("page 5 should have the last 20 rows and no next link")
	}
	for n, want := range map[int]string{0: "", 1: "1", 99: "99", 100: "99+"} {
		if got := bellLabel(n); got != want {
			t.Errorf("bellLabel(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestNotificationsForBackupsAndImports(t *testing.T) {
	f := newIssuesFixture(t)
	ctx := context.Background()
	me := zoe(t, f)
	// History from before the feature existed isn't announced.
	f.a.db.AddBackupRun(ctx, store.BackupRun{StartedAt: time.Now(), OK: false, Error: "old failure", Trigger: "schedule"})
	f.a.pollSystemSources(ctx, true)
	if n, _ := f.a.db.UnreadNotificationCount(ctx, me.ID); n != 0 {
		t.Fatalf("seeding announced %d", n)
	}

	f.a.db.AddBackupRun(ctx, store.BackupRun{StartedAt: time.Now(), OK: true, Trigger: "schedule"}) // quiet
	f.a.db.AddBackupRun(ctx, store.BackupRun{StartedAt: time.Now(), OK: false, Error: "bucket unreachable", Trigger: "schedule"})
	f.a.db.AddBackupRun(ctx, store.BackupRun{StartedAt: time.Now(), OK: true, Size: 2048, Destinations: "local", Trigger: "manual", ActorKind: "owner", ActorName: "zoe"})
	if _, err := f.a.db.CreateRepo(ctx, me.ID, "imported", "", true); err != nil {
		t.Fatal(err)
	}
	ok, _ := f.a.db.CreateImportJob(ctx, store.ImportJob{Source: "git", SourceURL: "https://x/y.git", Owner: "zoe", Name: "imported"})
	f.a.db.SetImportJobStatus(ctx, ok, "done", "Imported 3 branches")
	bad, _ := f.a.db.CreateImportJob(ctx, store.ImportJob{Source: "git", SourceURL: "https://x/z.git", Owner: "zoe", Name: "broken"})
	f.a.db.SetImportJobStatus(ctx, bad, "failed", "authentication failed")
	running, _ := f.a.db.CreateImportJob(ctx, store.ImportJob{Source: "git", SourceURL: "https://x/w.git", Owner: "zoe", Name: "later"})
	f.a.db.SetImportJobStatus(ctx, running, "running", "Cloning…")

	f.a.pollSystemSources(ctx, false)
	f.a.pollSystemSources(ctx, false) // idempotent
	list, _ := f.a.db.Notifications(ctx, store.NotificationQuery{PersonID: me.ID})
	kinds := map[string]store.Notification{}
	for _, n := range list {
		kinds[n.Kind] = n
	}
	// The two backup notifications coalesce into one "backups" row (latest wins).
	if len(list) != 3 {
		t.Fatalf("got %d notifications, want 3: %+v", len(list), list)
	}
	if b, ok := kinds[EventBackupSucceeded]; !ok || !strings.Contains(b.Detail, "started by zoe") {
		t.Errorf("backup row: %+v", kinds)
	}
	if d, ok := kinds[EventImportDone]; !ok || d.URL != "/zoe/imported" || d.RepoID == 0 {
		t.Errorf("import done row: %+v", d)
	}
	if d, ok := kinds[EventImportFailed]; !ok || d.Detail != "authentication failed" || d.RepoID != 0 {
		t.Errorf("import failed row: %+v", d)
	}
	body := f.get(t, "/notifications?filter=system", 200)
	if !strings.Contains(body, "Import of zoe/broken failed") || !strings.Contains(body, "nf-failed") {
		t.Error("system filter should show the failed import, in red")
	}
}
