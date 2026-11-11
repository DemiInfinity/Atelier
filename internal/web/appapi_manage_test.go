package web

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
)

// allowManage switches "Can rename and delete repositories" for a device,
// as its person does in Settings → Devices.
func allowManage(t *testing.T, a *App, ck map[string]string, deviceID int64, on bool) {
	t.Helper()
	v := "0"
	if on {
		v = "1"
	}
	rec := do(t, a, "POST", "/settings/devices/admin", url.Values{"id": {itoa(deviceID)}, "on": {v}}, ck)
	if rec.Code != 303 || !strings.HasPrefix(rec.Header().Get("Location"), "/settings/devices?done=admin-") {
		t.Fatalf("switching rename and delete for device %d = %d → %q", deviceID, rec.Code, rec.Header().Get("Location"))
	}
}

func TestAppRepoRenameAndDelete(t *testing.T) {
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	api(t, a, "POST", "/repos", f.tok, map[string]any{"owner": "zoe", "name": "Ingle"}, 201)
	mallory, err := a.db.CreateOwner(ctx, "mallory", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	other, otherDev := appTestDevice(t, a, mallory.ID, "Elsewhere")

	// Off by default: the device is told so, and a stranger still sees nothing.
	if d := obj(api(t, a, "GET", "/me", f.tok, nil, 200)["device"]); d["can_admin"] != false {
		t.Errorf("a new device's can_admin = %v", d["can_admin"])
	}
	wantErr(t, "rename while off", api(t, a, "PATCH", "/repos/zoe/demo", f.tok, map[string]any{"name": "Demo2"}, 403), "device_not_allowed")
	wantErr(t, "delete while off", api(t, a, "DELETE", "/repos/zoe/demo", f.tok, map[string]any{"confirm": "zoe/demo"}, 403), "device_not_allowed")
	wantErr(t, "rename by a stranger", api(t, a, "PATCH", "/repos/zoe/demo", other, map[string]any{"name": "Demo2"}, 404), "not_found")
	wantErr(t, "delete by a stranger", api(t, a, "DELETE", "/repos/zoe/demo", other, map[string]any{"confirm": "zoe/demo"}, 404), "not_found")
	if rp, err := a.db.RepoByName(ctx, "zoe", "demo"); err != nil || rp.Name != "demo" {
		t.Fatalf("the repository changed: %+v, %v", rp, err)
	}

	// A device can't switch it on itself: not through the app API, not by
	// posting the settings form with its token.
	bearer := map[string]string{"Authorization": "Bearer " + f.tok}
	form := url.Values{"id": {itoa(f.dev.ID)}, "on": {"1"}}
	if rec := do(t, a, "POST", "/settings/devices/admin", form, bearer); rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "/signin") {
		t.Errorf("the settings form with a device token = %d, want a redirect to sign-in", rec.Code)
	}
	api(t, a, "POST", "/settings/devices/admin", f.tok, map[string]any{"id": f.dev.ID, "on": "1"}, 404)
	api(t, a, "POST", "/device/admin", f.tok, map[string]any{"can_admin": true}, 404)
	api(t, a, "PATCH", "/me", f.tok, map[string]any{"device": map[string]any{"can_admin": true}}, 404)
	api(t, a, "POST", "/device/ssh-key", f.tok, map[string]any{"key": "nonsense", "can_admin": true}, 422)
	// Nor can someone else switch it for the owner's device, or with a cross-site post.
	malloryCK := sessionFor(t, a, mallory.ID)
	if rec := do(t, a, "POST", "/settings/devices/admin", form, malloryCK); rec.Code == 303 {
		t.Errorf("someone else switched the owner's device: %d", rec.Code)
	}
	ownerCK := sessionFor(t, a, f.owner.ID)
	cross := map[string]string{"Cookie": ownerCK["Cookie"], "Sec-Fetch-Site": "cross-site"}
	if rec := do(t, a, "POST", "/settings/devices/admin", form, cross); rec.Code != 403 {
		t.Errorf("a cross-site post = %d, want 403", rec.Code)
	}
	if d, _ := a.db.AppDevice(ctx, f.owner.ID, f.dev.ID); d.CanAdmin {
		t.Fatal("the device was allowed without its person switching it on")
	}
	wantErr(t, "still off", api(t, a, "PATCH", "/repos/zoe/demo", f.tok, map[string]any{"name": "Demo2"}, 403), "device_not_allowed")

	// The owner switches it on in Settings → Devices, for this device only.
	page := expectStatus(t, do(t, a, "GET", "/settings/devices", nil, ownerCK), 200, "devices page")
	mustContain(t, page, "devices page", "Can rename and delete repositories", `action="/settings/devices/admin"`, `name="on" value="1"`)
	second, secondDev := appTestDevice(t, a, f.owner.ID, "Studio")
	allowManage(t, a, ownerCK, f.dev.ID, true)
	if d := obj(api(t, a, "GET", "/me", f.tok, nil, 200)["device"]); d["can_admin"] != true {
		t.Errorf("can_admin after switching on = %v", d["can_admin"])
	}
	if d := obj(api(t, a, "GET", "/me", second, nil, 200)["device"]); d["can_admin"] != false {
		t.Errorf("another device's can_admin = %v", d["can_admin"])
	}
	wantErr(t, "another device", api(t, a, "DELETE", "/repos/zoe/demo", second, map[string]any{"confirm": "zoe/demo"}, 403), "device_not_allowed")
	_ = secondDev

	// It grants nothing else: the token still isn't an admin anywhere, and
	// the admin pages still want a signed-in person.
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	who, _ := a.resolveCredential(ctx, f.tok)
	if a.can(ctx, who, demo, PermAdmin) || who.hasScope("repo:admin") || len(who.Scopes) != 3 {
		t.Errorf("the allowed device became an admin: scopes %v", who.Scopes)
	}
	for path, form := range map[string]url.Values{
		"/zoe/demo/settings":          {"description": {"hacked"}, "visibility": {"public"}},
		"/zoe/demo/settings/secrets":  {"name": {"HACKED"}, "value": {"x"}},
		"/zoe/demo/settings/branches": {"pattern": {"main"}},
		"/zoe/demo/transfer":          {"owner": {"zoe"}, "name": {"hacked"}, "confirm": {"zoe/demo"}},
		"/zoe/demo/delete":            {"confirm": {"zoe/demo"}},
		"/settings/tokens":            {"name": {"x"}, "access": {"write"}, "expires": {"30"}},
	} {
		if rec := do(t, a, "POST", path, form, bearer); rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "/signin") {
			t.Errorf("POST %s with an allowed device token = %d, want a redirect to sign-in", path, rec.Code)
		}
	}
	if rp, err := a.db.RepoByName(ctx, "zoe", "demo"); err != nil || rp.Name != "demo" || !rp.Private || rp.Description == "hacked" {
		t.Fatalf("an admin page changed the repository: %+v, %v", rp, err)
	}
	if names, _ := a.db.Secrets(ctx, store.RepoScope(f.rid)); len(names) != 1 {
		t.Errorf("secrets = %d, want the fixture's one", len(names))
	}

	// An ordinary token with the app scope has no switch; a stranger's
	// allowed device still can't see the owner's repository.
	script, err := a.db.CreateToken(ctx, "script", tokenScopes["app"], time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wantErr(t, "an access token", api(t, a, "PATCH", "/repos/zoe/demo", script, map[string]any{"name": "Demo2"}, 403), "forbidden")
	wantErr(t, "an access token", api(t, a, "DELETE", "/repos/zoe/demo", script, map[string]any{"confirm": "zoe/demo"}, 403), "forbidden")
	allowManage(t, a, malloryCK, otherDev.ID, true)
	wantErr(t, "rename by an allowed stranger", api(t, a, "PATCH", "/repos/zoe/demo", other, map[string]any{"name": "Demo2"}, 404), "not_found")
	wantErr(t, "delete by an allowed stranger", api(t, a, "DELETE", "/repos/zoe/demo", other, map[string]any{"confirm": "zoe/demo"}, 404), "not_found")
	// A public repository they can read is still not theirs to manage.
	api(t, a, "POST", "/repos", f.tok, map[string]any{"owner": "zoe", "name": "Open", "private": false}, 201)
	if rec, _ := appReq(t, a, "PATCH", appAPIPrefix+"/repos/zoe/Open", other, map[string]any{"name": "Mine"}, nil); rec.Code != 403 && rec.Code != 404 {
		t.Errorf("a stranger renaming a public repository = %d", rec.Code)
	}
	if rec, _ := appReq(t, a, "DELETE", appAPIPrefix+"/repos/zoe/Open", other, map[string]any{"confirm": "zoe/Open"}, nil); rec.Code != 403 && rec.Code != 404 {
		t.Errorf("a stranger deleting a public repository = %d", rec.Code)
	}
	if _, err := a.db.RepoByName(ctx, "zoe", "Open"); err != nil {
		t.Fatalf("the public repository is gone: %v", err)
	}
	if err := a.deleteRepo(ctx, appMustRepo(t, a, "zoe", "Open")); err != nil {
		t.Fatal(err)
	}

	// Off again: refused at once.
	allowManage(t, a, ownerCK, f.dev.ID, false)
	wantErr(t, "switched off", api(t, a, "PATCH", "/repos/zoe/demo", f.tok, map[string]any{"name": "Demo2"}, 403), "device_not_allowed")
	allowManage(t, a, ownerCK, f.dev.ID, true)

	// --- rename ---
	wantErr(t, "taken", api(t, a, "PATCH", "/repos/zoe/demo", f.tok, map[string]any{"name": "Ingle"}, 409), "name_taken")
	for _, bad := range []string{"", "  ", "bad name", "x.git", "settings"} {
		wantErr(t, fmt.Sprintf("invalid %q", bad), api(t, a, "PATCH", "/repos/zoe/demo", f.tok, map[string]any{"name": bad}, 422), "invalid_name")
	}
	api(t, a, "PATCH", "/repos/zoe/nope", f.tok, map[string]any{"name": "x"}, 404)
	if same := api(t, a, "PATCH", "/repos/zoe/demo", f.tok, map[string]any{"name": "demo"}, 200); same["full_name"] != "zoe/demo" {
		t.Errorf("rename to the same name = %v", same)
	}
	out := api(t, a, "PATCH", "/repos/zoe/demo", f.tok, map[string]any{"name": "Demo2"}, 200)
	wantKeys(t, "renamed", out, "full_name", "owner", "owner_kind", "name", "private", "description", "default_branch", "language",
		"language_color", "clone_https", "clone_ssh", "github", "open_pulls", "updated_at")
	if out["full_name"] != "zoe/Demo2" || out["name"] != "Demo2" || out["owner"] != "zoe" || out["default_branch"] != "main" {
		t.Errorf("renamed = %v", out)
	}
	rp, err := a.db.RepoByName(ctx, "zoe", "Demo2")
	if err != nil || rp.ID != f.rid {
		t.Fatalf("after rename: %+v, %v", rp, err)
	}
	if g, err := a.git.Open("zoe", "Demo2"); err != nil || g.IsEmpty(ctx) {
		t.Errorf("the bare repository didn't move: %v", err)
	}
	if _, err := a.git.Open("zoe", "demo"); err == nil {
		t.Error("the old folder is still there")
	}
	list1 := api(t, a, "GET", "/repos", f.tok, nil, 200)["repos"]
	if find(list1, "full_name", "zoe/Demo2") == nil || find(list1, "full_name", "zoe/demo") != nil {
		t.Errorf("repos after rename = %v", list1)
	}
	// The old name keeps leading there.
	api(t, a, "GET", "/repos/zoe/demo/pulls", f.tok, nil, 200)

	// --- delete ---
	for _, confirm := range []any{"", "Demo2", "zoe/demo", "zoe/Ingle", nil} {
		wantErr(t, fmt.Sprintf("confirm %v", confirm), api(t, a, "DELETE", "/repos/zoe/Demo2", f.tok, map[string]any{"confirm": confirm}, 422), "confirm_mismatch")
	}
	wantErr(t, "no body", api(t, a, "DELETE", "/repos/zoe/Demo2", f.tok, nil, 422), "confirm_mismatch")
	if _, err := a.db.RepoByID(ctx, f.rid); err != nil {
		t.Fatalf("deleted without the right confirm: %v", err)
	}
	rec, _ := appReq(t, a, "DELETE", appAPIPrefix+"/repos/zoe/Demo2", f.tok, map[string]any{"confirm": "ZOE/demo2"}, nil)
	if rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d %q", rec.Code, rec.Body)
	}
	if _, err := a.db.RepoByID(ctx, f.rid); err == nil {
		t.Error("the repository is still in the database")
	}
	if _, err := a.git.Open("zoe", "Demo2"); err == nil {
		t.Error("the repository is still on disk")
	}
	api(t, a, "DELETE", "/repos/zoe/Demo2", f.tok, map[string]any{"confirm": "zoe/Demo2"}, 404)
	if left := list(api(t, a, "GET", "/repos", f.tok, nil, 200)["repos"]); len(left) != 1 || obj(left[0])["full_name"] != "zoe/Ingle" {
		t.Errorf("repos after delete = %v", left)
	}
}

func TestAppPullChecksSummaryAndRepoFilter(t *testing.T) {
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	checks := func(path string, n int64) map[string]any {
		t.Helper()
		sum := find(api(t, a, "GET", path, f.tok, nil, 200)["pulls"], "number", n)
		c := obj(sum["checks"])
		wantKeys(t, "checks of "+path, c, "total", "passed", "failed", "pending", "state")
		return c
	}
	want := func(what string, c map[string]any, total, passed, failed, pending int, state string) {
		t.Helper()
		if c["total"] != float64(total) || c["passed"] != float64(passed) || c["failed"] != float64(failed) ||
			c["pending"] != float64(pending) || c["state"] != state {
			t.Errorf("%s: checks = %v, want total %d passed %d failed %d pending %d state %s", what, c, total, passed, failed, pending, state)
		}
	}

	// No workflow yet: no checks.
	none := f.openPull(t, "feature/none", "Nothing runs")
	want("none", checks("/pulls", none.Number), 0, 0, 0, 0, "none")

	runGit(t, f.work, "checkout", "-q", "main")
	pullsCommit(t, f.work, ".atelier/workflows/pr.yml", appPRYML, "PR workflow")
	f.push(t, "main")
	rn := f.registerRunner(t, "Box", []string{"linux"}, []string{"jobs"})

	pass := f.openPull(t, "feature/pass", "Will pass")
	for _, path := range []string{"/pulls", "/pulls?repo=zoe/demo", "/repos/zoe/demo/pulls"} {
		want("pending "+path, checks(path, pass.Number), 1, 0, 0, 1, "pending")
	}
	rn.runJob(rn.poll(), actions.StatusSuccess, nil)
	f.settle()
	want("passing", checks("/pulls?repo=zoe/demo", pass.Number), 1, 1, 0, 0, "passing")

	fail := f.openPull(t, "feature/fail", "Will fail")
	rn.runJob(rn.poll(), actions.StatusFailure, nil)
	f.settle()
	want("failing", checks("/pulls?repo=zoe/demo", fail.Number), 1, 0, 1, 0, "failing")
	want("still none", checks("/pulls", none.Number), 0, 0, 0, 0, "none")

	// The summary's states, including the ones a single job can't reach.
	for _, tc := range []struct {
		n     appCheckCounts
		total int
		state string
	}{
		{appCheckCounts{}, 0, "none"},
		{appCheckCounts{Passed: 2}, 2, "passing"},
		{appCheckCounts{Passed: 2, Pending: 1}, 3, "pending"},
		{appCheckCounts{Passed: 1, Waiting: 1}, 2, "pending"},
		{appCheckCounts{Passed: 2, Failed: 1, Pending: 1, Waiting: 1}, 5, "failing"},
	} {
		if got := appChecksSummary(tc.n); got["total"] != tc.total || got["state"] != tc.state {
			t.Errorf("appChecksSummary(%+v) = %v, want total %d state %s", tc.n, got, tc.total, tc.state)
		}
	}

	// --- ?repo= keeps the list to one repository ---
	api(t, a, "POST", "/repos", f.tok, map[string]any{"owner": "zoe", "name": "Other"}, 201)
	otherRepo, err := a.db.RepoByName(ctx, "zoe", "Other")
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, a.git.Path("zoe", "Other"), "fetch", "-q", f.work, "+refs/heads/*:refs/heads/*")
	head := runGit(t, f.work, "rev-parse", "feature/none")
	elsewhere, err := a.db.CreatePull(ctx, store.NewPull{RepoID: otherRepo.ID, Title: "Elsewhere", Head: "feature/none", Base: "main",
		HeadSHA: trimNL(head), AuthorKind: ActorOwner, AuthorName: "zoe"})
	if err != nil {
		t.Fatal(err)
	}
	repoOf := func(path string) map[string]int {
		t.Helper()
		seen := map[string]int{}
		for _, p := range list(api(t, a, "GET", path, f.tok, nil, 200)["pulls"]) {
			seen[fmt.Sprint(obj(p)["repo"])]++
		}
		return seen
	}
	if got := repoOf("/pulls"); got["zoe/demo"] != 3 || got["zoe/Other"] != 1 || len(got) != 2 {
		t.Errorf("/pulls = %v", got)
	}
	if got := repoOf("/pulls?repo=zoe/demo"); got["zoe/demo"] != 3 || len(got) != 1 {
		t.Errorf("?repo=zoe/demo = %v", got)
	}
	if got := repoOf("/pulls?repo=zoe/Other&state=all"); got["zoe/Other"] != 1 || len(got) != 1 {
		t.Errorf("?repo=zoe/Other = %v", got)
	}
	want("other repo", checks("/pulls?repo=zoe/Other", elsewhere.Number), 0, 0, 0, 0, "none")
	if got := repoOf("/pulls?repo=zoe/Other&state=merged"); len(got) != 0 {
		t.Errorf("?repo=zoe/Other&state=merged = %v", got)
	}
	wantErr(t, "no such repo", api(t, a, "GET", "/pulls?repo=zoe/nope", f.tok, nil, 404), "not_found")
	wantErr(t, "not a repo name", api(t, a, "GET", "/pulls?repo=demo", f.tok, nil, 404), "not_found")
	mallory, err := a.db.CreateOwner(ctx, "mallory", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := appTestDevice(t, a, mallory.ID, "Elsewhere")
	wantErr(t, "someone else's repo", api(t, a, "GET", "/pulls?repo=zoe/demo", tok, nil, 404), "not_found")
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func TestAppIssues(t *testing.T) {
	var mu sync.Mutex
	var kinds []string
	saved := eventHandlers
	t.Cleanup(func() { eventHandlers = saved })
	registerEventHandler(func(_ context.Context, _ *App, ev Event) {
		if strings.HasPrefix(ev.Kind, "issue.") {
			mu.Lock()
			kinds = append(kinds, fmt.Sprintf("%s #%d by %s", ev.Kind, ev.Number, ev.Actor.Name))
			mu.Unlock()
		}
	})
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	a.cfg.PublicURL = "https://studio.example"
	base := "/repos/zoe/demo/issues"

	if got := api(t, a, "GET", "/issues", f.tok, nil, 200); len(list(got["issues"])) != 0 || got["has_more"] != false {
		t.Errorf("no issues yet = %v", got)
	}

	// --- open ---
	made := api(t, a, "POST", base, f.tok, map[string]any{"title": "  Tray icon flickers ", "body": "On **macOS**.\r\n"}, 201)
	wantKeys(t, "issue", made, "repo", "number", "title", "state", "author", "labels", "comments", "created_at", "updated_at", "web_url", "web_path", "body")
	n1 := int64(made["number"].(float64))
	if made["repo"] != "zoe/demo" || made["title"] != "Tray icon flickers" || made["state"] != "open" || made["author"] != "token:device:MacBook" ||
		made["body"] != "On **macOS**." || len(list(made["comments"])) != 0 || len(list(made["labels"])) != 0 ||
		made["web_path"] != "/zoe/demo/issues/"+itoa(n1) || made["web_url"] != "https://studio.example/zoe/demo/issues/"+itoa(n1) {
		t.Errorf("created = %v", made)
	}
	for _, bad := range []any{"", "   ", "two\nlines", strings.Repeat("x", 257), nil} {
		wantErr(t, fmt.Sprintf("title %.20v", bad), api(t, a, "POST", base, f.tok, map[string]any{"title": bad}, 422), "invalid_title")
	}
	second := api(t, a, "POST", base, f.tok, map[string]any{"title": "Second"}, 201)
	n2 := int64(second["number"].(float64))

	// A label set on the web shows with its colour.
	is1, err := a.db.IssueByNumber(ctx, f.rid, n1)
	if err != nil {
		t.Fatal(err)
	}
	labels, _ := a.db.Labels(ctx, f.rid)
	if len(labels) == 0 {
		t.Fatal("opening an issue didn't seed the default labels")
	}
	if err := a.db.SetIssueLabels(ctx, is1.ID, []int64{labels[0].ID}, ActorOwner, "zoe"); err != nil {
		t.Fatal(err)
	}

	// --- comment ---
	cm := api(t, a, "POST", base+"/"+itoa(n1)+"/comments", f.tok, map[string]any{"body": "Seen it too."}, 201)
	wantKeys(t, "comment", cm, "id", "author", "body", "created_at")
	if cm["author"] != "device:MacBook" || cm["body"] != "Seen it too." {
		t.Errorf("comment = %v", cm)
	}
	wantErr(t, "empty comment", api(t, a, "POST", base+"/"+itoa(n1)+"/comments", f.tok, map[string]any{"body": " \n"}, 422), "empty_body")
	api(t, a, "POST", base+"/999/comments", f.tok, map[string]any{"body": "x"}, 404)

	// --- read ---
	d := api(t, a, "GET", base+"/"+itoa(n1), f.tok, nil, 200)
	lb := obj(list(d["labels"])[0])
	if len(list(d["comments"])) != 1 || obj(list(d["comments"])[0])["body"] != "Seen it too." || d["body"] != "On **macOS**." ||
		lb["name"] != labels[0].Name || lb["color"] != "#"+labels[0].Color {
		t.Errorf("issue = %v", d)
	}
	api(t, a, "GET", base+"/999", f.tok, nil, 404)
	p := f.openPull(t, "feature/a", "A pull request")
	api(t, a, "GET", base+"/"+itoa(p.Number), f.tok, nil, 404) // a pull request isn't an issue

	// --- list ---
	for _, path := range []string{"/issues", "/issues?state=open", "/issues?repo=zoe/demo"} {
		got := api(t, a, "GET", path, f.tok, nil, 200)
		l := list(got["issues"])
		if len(l) != 2 || obj(l[0])["number"] != float64(n2) || obj(l[1])["number"] != float64(n1) || got["has_more"] != false {
			t.Fatalf("%s = %v", path, got)
		}
		sum := obj(l[1])
		wantKeys(t, "summary "+path, sum, "repo", "number", "title", "state", "author", "labels", "comments", "created_at", "updated_at", "web_url")
		if _, has := sum["body"]; has || sum["comments"] != float64(1) || len(list(sum["labels"])) != 1 {
			t.Errorf("%s: summary = %v", path, sum)
		}
	}
	for _, path := range []string{"/issues?per_page=1", "/issues?repo=zoe/demo&per_page=1"} {
		p1 := api(t, a, "GET", path, f.tok, nil, 200)
		p2 := api(t, a, "GET", path+"&page=2", f.tok, nil, 200)
		p3 := api(t, a, "GET", path+"&page=3", f.tok, nil, 200)
		if len(list(p1["issues"])) != 1 || p1["has_more"] != true || obj(list(p1["issues"])[0])["number"] != float64(n2) ||
			len(list(p2["issues"])) != 1 || p2["has_more"] != false || obj(list(p2["issues"])[0])["number"] != float64(n1) ||
			len(list(p3["issues"])) != 0 || p3["has_more"] != false {
			t.Errorf("%s pages = %v / %v / %v", path, p1, p2, p3)
		}
	}
	wantErr(t, "bad state", api(t, a, "GET", "/issues?state=merged", f.tok, nil, 422), "invalid_state")
	wantErr(t, "no such repo", api(t, a, "GET", "/issues?repo=zoe/nope", f.tok, nil, 404), "not_found")

	// --- close and reopen ---
	wantErr(t, "bad state", api(t, a, "PATCH", base+"/"+itoa(n1), f.tok, map[string]any{"state": "merged"}, 422), "invalid_state")
	if c := api(t, a, "PATCH", base+"/"+itoa(n1), f.tok, map[string]any{"state": "closed"}, 200); c["state"] != "closed" || c["number"] != float64(n1) {
		t.Errorf("closed = %v", c)
	}
	api(t, a, "PATCH", base+"/"+itoa(n1), f.tok, map[string]any{"state": "closed"}, 200) // again: nothing happens
	if got := list(api(t, a, "GET", "/issues?repo=zoe/demo", f.tok, nil, 200)["issues"]); len(got) != 1 || obj(got[0])["number"] != float64(n2) {
		t.Errorf("open after closing = %v", got)
	}
	if got := list(api(t, a, "GET", "/issues?state=closed", f.tok, nil, 200)["issues"]); len(got) != 1 || obj(got[0])["number"] != float64(n1) {
		t.Errorf("closed = %v", got)
	}
	if got := list(api(t, a, "GET", "/issues?state=all", f.tok, nil, 200)["issues"]); len(got) != 2 {
		t.Errorf("all = %v", got)
	}
	if c := api(t, a, "PATCH", base+"/"+itoa(n1), f.tok, map[string]any{"state": "open"}, 200); c["state"] != "open" {
		t.Errorf("reopened = %v", c)
	}
	evs, _ := a.db.IssueEvents(ctx, is1.ID)
	var timeline []string
	for _, e := range evs {
		if e.Kind == store.EventClosed || e.Kind == store.EventReopened {
			timeline = append(timeline, e.Kind+" by "+e.ActorName)
		}
	}
	if fmt.Sprint(timeline) != "[closed by device:MacBook reopened by device:MacBook]" {
		t.Errorf("timeline = %v", timeline)
	}

	// The same events as the web pages fire, once each.
	f.settle()
	mu.Lock()
	got := fmt.Sprint(kinds)
	mu.Unlock()
	wantEvents := fmt.Sprintf("[issue.opened #%d by device:MacBook issue.opened #%d by device:MacBook issue.commented #%d by device:MacBook "+
		"issue.closed #%d by device:MacBook issue.reopened #%d by device:MacBook]", n1, n2, n1, n1, n1)
	if got != wantEvents {
		t.Errorf("events = %s\nwant     %s", got, wantEvents)
	}

	// --- someone else sees and changes nothing; a read-only token can't write ---
	mallory, err := a.db.CreateOwner(ctx, "mallory", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := appTestDevice(t, a, mallory.ID, "Elsewhere")
	if got := list(api(t, a, "GET", "/issues?state=all", other, nil, 200)["issues"]); len(got) != 0 {
		t.Errorf("a stranger's issues = %v", got)
	}
	api(t, a, "GET", "/issues?repo=zoe/demo", other, nil, 404)
	api(t, a, "GET", base+"/"+itoa(n1), other, nil, 404)
	api(t, a, "POST", base, other, map[string]any{"title": "Hello"}, 404)
	api(t, a, "POST", base+"/"+itoa(n1)+"/comments", other, map[string]any{"body": "Hello"}, 404)
	api(t, a, "PATCH", base+"/"+itoa(n1), other, map[string]any{"state": "closed"}, 404)
	saved2 := accessPolicies
	t.Cleanup(func() { accessPolicies = saved2 })
	accessPolicies = append([]AccessPolicy{func(_ context.Context, _ *App, who Actor, _ store.Repo, need Perm) (bool, bool) {
		return false, who.Kind == ActorToken && need != PermRead // read-only from here on
	}}, saved2...)
	api(t, a, "GET", base+"/"+itoa(n1), f.tok, nil, 200)
	wantErr(t, "read-only create", api(t, a, "POST", base, f.tok, map[string]any{"title": "Hello"}, 403), "forbidden")
	wantErr(t, "read-only comment", api(t, a, "POST", base+"/"+itoa(n1)+"/comments", f.tok, map[string]any{"body": "Hello"}, 403), "forbidden")
	wantErr(t, "read-only close", api(t, a, "PATCH", base+"/"+itoa(n1), f.tok, map[string]any{"state": "closed"}, 403), "forbidden")
	if is, _ := a.db.IssueByNumber(ctx, f.rid, n1); !is.IsOpen() || is.Comments != 1 {
		t.Errorf("the issue changed: %+v", is)
	}
}

func appMustRepo(t *testing.T, a *App, owner, name string) store.Repo {
	t.Helper()
	rp, err := a.db.RepoByName(context.Background(), owner, name)
	if err != nil {
		t.Fatal(err)
	}
	return rp
}
