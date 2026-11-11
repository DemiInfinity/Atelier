package web

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/store"
)

// mainCommit adds one commit to main and pushes it, leaving the work tree on main.
func (f *appFix) mainCommit(t *testing.T, file, content string) {
	t.Helper()
	runGit(t, f.work, "checkout", "-q", "main")
	pullsCommit(t, f.work, file, content, "Main: "+file)
	f.push(t, "main")
}

func (f *appFix) branchSHA(t *testing.T, branch string) string {
	t.Helper()
	sha, err := gitBranch(t, f.a, branch)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

func TestPullUpdateBranch(t *testing.T) {
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	pullsCommit(t, f.work, ".atelier/workflows/pr.yml", appPRYML, "PR workflow")
	pullsCommit(t, f.work, "shared.txt", "one\ntwo\nthree\n", "Shared file")
	f.push(t, "main")
	base := "/repos/zoe/demo/pulls/"
	bare := a.git.Path("zoe", "demo")

	var mu sync.Mutex
	var pushed []Event
	registerEventHandler(func(ctx context.Context, app *App, ev Event) {
		if app == a && ev.Kind == EventRepoPushed {
			mu.Lock()
			pushed = append(pushed, ev)
			mu.Unlock()
		}
	})

	// --- up to date: nothing to do, no button ---
	p1 := f.openPull(t, "feature/u", "Update me")
	n1 := itoa(p1.Number)
	head0 := f.branchSHA(t, "feature/u")
	d := api(t, a, "GET", base+n1, f.tok, nil, 200)
	wantKeys(t, "pull", d, "ahead", "behind", "conflicts", "conflict_files", "can_update")
	if d["ahead"] != float64(1) || d["behind"] != float64(0) || d["conflicts"] != false || d["can_update"] != false || len(list(d["conflict_files"])) != 0 {
		t.Errorf("fresh pull = %v", d)
	}
	page := expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/"+n1, nil, f.ck), 200, "PR page")
	if strings.Contains(page, "Update branch") {
		t.Error("an up-to-date pull request offers Update branch")
	}
	out := api(t, a, "POST", base+n1+"/update", f.tok, nil, 200)
	if out["updated"] != false || out["head_sha"] != head0 {
		t.Errorf("update when current = %v", out)
	}

	// --- behind: the button shows, and the update merges main in ---
	f.mainCommit(t, "main-only.txt", "main\n")
	mainSHA := f.branchSHA(t, "main")
	d = api(t, a, "GET", base+n1, f.tok, nil, 200)
	if d["ahead"] != float64(1) || d["behind"] != float64(1) || d["conflicts"] != false || d["can_update"] != true {
		t.Errorf("behind pull = %v", d)
	}
	sum := find(api(t, a, "GET", "/repos/zoe/demo/pulls", f.tok, nil, 200)["pulls"], "number", p1.Number)
	wantKeys(t, "summary", sum, "ahead", "behind", "conflicts", "can_update")
	if _, has := sum["conflict_files"]; has || sum["behind"] != float64(1) {
		t.Errorf("summary = %v", sum)
	}
	page = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/"+n1, nil, f.ck), 200, "PR page")
	mustContain(t, page, "behind PR page", "Update branch", `action="/zoe/demo/pulls/`+n1+`/update"`, "1 commit behind")
	if strings.Contains(page, "pr-update-reason") {
		t.Error("a clean update shows a reason / disabled button")
	}

	// No permission: a read-only app token.
	if _, err := a.db.DB().Exec(`INSERT INTO tokens (name, token_hash, prefix, scopes, created_at, person_id) VALUES ('ro', ?, 'atl_ro', 'app repo:read', ?, ?)`,
		store.Hash("atl_readonlyapptoken"), time.Now().Unix(), f.owner.ID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "read-only update", api(t, a, "POST", base+n1+"/update", "atl_readonlyapptoken", nil, 403), "forbidden")
	api(t, a, "POST", base+"999/update", f.tok, nil, 404)
	// The core operation checks it too, whoever calls.
	g, _ := a.git.Open("zoe", "demo")
	rp, _ := a.db.RepoByName(ctx, "zoe", "demo")
	if _, err := a.updatePullBranch(ctx, rp, g, f.pull(t, p1.Number), Actor{Kind: ActorAnonymous, Name: "anonymous"}); err == nil ||
		err.(*pullUpdateError).Kind != "forbidden" {
		t.Errorf("anonymous update = %v", err)
	}

	// A protected head branch refuses, with the rule named.
	rule, err := a.db.CreateBranchRule(ctx, store.BranchRule{RepoID: f.rid, Pattern: "feature/u", RequirePR: true})
	if err != nil {
		t.Fatal(err)
	}
	prot := api(t, a, "POST", base+n1+"/update", f.tok, nil, 403)
	wantErr(t, "protected head", prot, "branch_protected")
	if !strings.Contains(prot["message"].(string), "feature/u is protected by branch rule") {
		t.Errorf("protected message = %v", prot["message"])
	}
	if d := api(t, a, "GET", base+n1, f.tok, nil, 200); d["can_update"] != false {
		t.Errorf("protected head can_update = %v", d["can_update"])
	}
	page = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/"+n1, nil, f.ck), 200, "PR page")
	mustContain(t, page, "protected PR page", "Update branch", "disabled", "pr-update-reason", "is protected by branch rule")
	rule.RequirePR, rule.LinearHistory = false, true
	if err := a.db.UpdateBranchRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "linear head", api(t, a, "POST", base+n1+"/update", f.tok, nil, 403), "branch_protected")
	if err := a.db.DeleteBranchRule(ctx, f.rid, rule.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.branchSHA(t, "feature/u"); got != head0 {
		t.Fatal("a refused update moved the branch")
	}
	a.drainActionsPushes(ctx)
	f.settle()
	mu.Lock()
	pushed = nil
	mu.Unlock()
	runsBefore := len(f.runs(t))

	out = api(t, a, "POST", base+n1+"/update", f.tok, nil, 200)
	sha, _ := out["head_sha"].(string)
	if out["updated"] != true || sha == "" || sha == head0 || sha != f.branchSHA(t, "feature/u") {
		t.Fatalf("update = %v", out)
	}
	if got := runGit(t, bare, "log", "-1", "--format=%P|%s|%an|%cn", sha); got != head0+" "+mainSHA+"|Merge branch 'main' into feature/u|zoe|zoe" {
		t.Errorf("merge commit = %q", got)
	}
	if got := f.branchSHA(t, "main"); got != mainSHA {
		t.Error("the update moved main")
	}
	if p := f.pull(t, p1.Number); p.HeadSHA != sha || !p.IsOpen() {
		t.Errorf("pull after update = %+v", p)
	}
	// It is a push like any other: repo.pushed fires and the checks run again.
	a.drainActionsPushes(ctx)
	f.settle()
	mu.Lock()
	if len(pushed) != 1 || pushed[0].Push == nil || pushed[0].Push.Ref != "refs/heads/feature/u" || pushed[0].Push.Old != head0 ||
		pushed[0].Push.New != sha || pushed[0].Push.Pusher != "token:device:MacBook" {
		t.Errorf("repo.pushed = %+v", pushed)
	}
	mu.Unlock()
	var onNew int
	for _, r := range f.runs(t) {
		if r.SHA == sha {
			onNew++
		}
	}
	if onNew == 0 || len(f.runs(t)) <= runsBefore {
		t.Errorf("no runs on the updated head (%d runs before, %d now)", runsBefore, len(f.runs(t)))
	}
	d = api(t, a, "GET", base+n1, f.tok, nil, 200)
	if d["behind"] != float64(0) || d["ahead"] != float64(2) || d["can_update"] != false {
		t.Errorf("after update = %v", d)
	}
	if again := api(t, a, "POST", base+n1+"/update", f.tok, nil, 200); again["updated"] != false || again["head_sha"] != sha {
		t.Errorf("second update = %v", again)
	}

	// --- the web button ---
	f.mainCommit(t, "main-two.txt", "two\n")
	rec := do(t, a, "POST", "/zoe/demo/pulls/"+n1+"/update", url.Values{}, f.ck)
	expectStatus(t, rec, 303, "web update")
	if loc := rec.Header().Get("Location"); loc != "/zoe/demo/pulls/"+n1+"?branch=updated" {
		t.Errorf("web update redirects to %q", loc)
	}
	webSHA := f.branchSHA(t, "feature/u")
	if webSHA == sha || f.pull(t, p1.Number).HeadSHA != webSHA {
		t.Error("the web button didn't update the branch")
	}
	page = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/"+n1+"?branch=updated", nil, f.ck), 200, "PR page")
	mustContain(t, page, "updated PR page", "Branch updated: main was merged into feature/u.")
	if strings.Contains(page, "Update branch") {
		t.Error("Update branch still offered after updating")
	}
	rec = do(t, a, "POST", "/zoe/demo/pulls/"+n1+"/update", url.Values{}, f.ck)
	if loc := rec.Header().Get("Location"); rec.Code != 303 || !strings.HasSuffix(loc, "?branch=current") {
		t.Errorf("web update when current = %d %q", rec.Code, loc)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/"+n1+"/update", url.Values{},
		map[string]string{"Cookie": f.ck["Cookie"], "Sec-Fetch-Site": "cross-site"}), 403, "cross-site update")
	a.drainActionsPushes(ctx)
	f.settle()

	// --- conflicts: nothing is written, the files are named ---
	runGit(t, f.work, "checkout", "-q", "main")
	runGit(t, f.work, "checkout", "-q", "-b", "feature/c")
	pullsCommit(t, f.work, "shared.txt", "one\nfeature\nthree\n", "Feature edit")
	f.push(t, "feature/c")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"feature/c"}, "title": {"Clash"}}, f.ck), 303, "open PR")
	f.settle()
	p2, err := a.db.OpenPullFor(ctx, f.rid, "main", "feature/c")
	if err != nil {
		t.Fatal(err)
	}
	n2 := itoa(p2.Number)
	f.mainCommit(t, "shared.txt", "one\nmain\nthree\n")
	headC := f.branchSHA(t, "feature/c")
	d = api(t, a, "GET", base+n2, f.tok, nil, 200)
	if d["conflicts"] != true || fmt.Sprint(d["conflict_files"]) != "[shared.txt]" || d["can_update"] != false || d["behind"] != float64(1) ||
		obj(d["merge"])["can_merge"] != false {
		t.Errorf("conflicting pull = %v", d)
	}
	for _, path := range []string{"/pulls", "/repos/zoe/demo/pulls?state=open"} {
		pulls := api(t, a, "GET", path, f.tok, nil, 200)["pulls"]
		if s := find(pulls, "number", p2.Number); s["conflicts"] != true {
			t.Errorf("%s: summary conflicts = %v", path, s["conflicts"])
		}
		if s := find(pulls, "number", p1.Number); s["conflicts"] != false {
			t.Errorf("%s: clean summary conflicts = %v", path, s["conflicts"])
		}
	}
	cf := api(t, a, "POST", base+n2+"/update", f.tok, nil, 409)
	wantErr(t, "conflicts", cf, "conflicts")
	if fmt.Sprint(cf["files"]) != "[shared.txt]" || !strings.Contains(cf["message"].(string), "shared.txt") {
		t.Errorf("409 = %v", cf)
	}
	if got := f.branchSHA(t, "feature/c"); got != headC || f.pull(t, p2.Number).HeadSHA != headC {
		t.Error("a conflicting update wrote something")
	}
	page = expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/"+n2, nil, f.ck), 200, "conflict PR page")
	mustContain(t, page, "conflict PR page", "Update branch", "disabled", "Resolve the conflicts with main locally first.",
		"Conflicting files", "shared.txt", "git merge origin/main")
	body := expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/"+n2+"/update", url.Values{}, f.ck), 409, "web update with conflicts")
	mustContain(t, body, "web conflict message", "flash-error", "shared.txt", "git checkout feature/c", "git merge origin/main")
	listPage := expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls", nil, f.ck), 200, "PR list")
	if strings.Count(listPage, ">Conflicts</span>") != 1 {
		t.Errorf("PR list shows %d Conflicts pills, want 1", strings.Count(listPage, ">Conflicts</span>"))
	}

	// --- closed: not open ---
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/"+n2+"/state", url.Values{"action": {"close"}}, f.ck), 303, "close")
	wantErr(t, "closed", api(t, a, "POST", base+n2+"/update", f.tok, nil, 422), "not_open")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/"+n2+"/update", url.Values{}, f.ck), 422, "web update of a closed PR")
	if d := api(t, a, "GET", base+n2, f.tok, nil, 200); d["conflicts"] != false || d["behind"] != float64(0) || d["can_update"] != false {
		t.Errorf("closed pull = %v", d)
	}
	if got := f.branchSHA(t, "feature/c"); got != headC {
		t.Error("updating a closed pull request moved its branch")
	}
	f.settle()
}
