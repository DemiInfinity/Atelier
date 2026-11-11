package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// botComments returns the Pages bot's comments on a pull request.
func botComments(t *testing.T, a *App, repoID, number int64) []store.PullComment {
	t.Helper()
	ctx := context.Background()
	pr, err := a.db.PullByNumber(ctx, repoID, number)
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := a.db.PullComments(ctx, pr.ID)
	var out []store.PullComment
	for _, c := range cs {
		if c.AuthorKind == ActorBot && c.AuthorName == pagesPreviewBot {
			out = append(out, c)
		}
	}
	return out
}

func (f *pagesFix) openPull(t *testing.T, head, title string) {
	t.Helper()
	expectStatus(t, do(t, f.a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {head}, "title": {title}}, f.ck), http.StatusSeeOther, "open "+head)
	pagesBG.Wait()
}

func (f *pagesFix) previewSite(t *testing.T) {
	t.Helper()
	pullsCommit(t, f.work, "index.html", "<h1>Main site</h1>", "home")
	pullsCommit(t, f.work, "~preview/1/index.html", "<h1>A real folder</h1>", "a folder that looks like a preview")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)
}

func (f *pagesFix) branch(t *testing.T, name, content string) {
	t.Helper()
	runGit(t, f.work, "checkout", "-q", "-B", name, "main")
	pullsCommit(t, f.work, "index.html", content, name)
	f.push(t, name)
	runGit(t, f.work, "checkout", "-q", "main")
}

func TestPagesPreviewFromBranch(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	f.previewSite(t)
	f.branch(t, "feature", "<h1>Feature v1</h1>")
	f.openPull(t, "feature", "Feature")

	// Published at <pages>/~preview/<owner>/<repo>/<n>/ and linked in one comment.
	purl := pagesTestBase + "/~preview/zoe/demo/1/"
	rec := pagesGet(t, a, "/~preview/zoe/demo/1/", nil)
	pagesWant(t, rec, 200, "text/html; charset=utf-8", "Feature v1")
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") || rec.Header().Get("X-Robots-Tag") != "noindex" {
		t.Errorf("preview headers = %v", rec.Header())
	}
	if rec := pagesGet(t, a, "/~preview/zoe/demo/1", nil); rec.Code != 301 || rec.Header().Get("Location") != "/~preview/zoe/demo/1/" {
		t.Errorf("no slash = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cs := botComments(t, a, f.demo.ID, 1)
	if len(cs) != 1 || !strings.Contains(cs[0].Body, purl) || !strings.Contains(cs[0].Body, "is ready") {
		t.Fatalf("comments = %+v", cs)
	}
	// The live site and a real folder named like the preview path are untouched.
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Main site")
	pagesWant(t, pagesGet(t, a, "/zoe/demo/~preview/1/", nil), 200, "", "A real folder")
	if gitrepo.ValidName(pagesPreviewSeg) {
		t.Error("~preview is a valid owner name, so previews could collide with a site")
	}
	// Previews are never the site's deploys: not listed, not rollback targets.
	site, _ := a.db.PagesDeploys(ctx, f.demo.ID, 100)
	pds, _ := a.db.PagesPreviewDeploys(ctx, f.demo.ID)
	if len(site) != 1 || len(pds) != 1 {
		t.Fatalf("site deploys %d, preview deploys %d", len(site), len(pds))
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/rollback", url.Values{"deploy": {fmt.Sprint(pds[0].ID)}}, f.ck), 404, "rollback to a preview")
	if err := a.db.SetPagesLive(ctx, f.demo.ID, pds[0].ID); err == nil {
		t.Error("a preview became the live site")
	}
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String(), "previews card", "Pull request previews", purl, "Feature")

	// A push updates the preview and the same comment; the old snapshot goes.
	runGit(t, f.work, "checkout", "-q", "feature")
	pullsCommit(t, f.work, "index.html", "<h1>Feature v2</h1>", "v2")
	sha := f.push(t, "feature")
	runGit(t, f.work, "checkout", "-q", "main")
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 200, "", "Feature v2")
	cs2 := botComments(t, a, f.demo.ID, 1)
	if len(cs2) != 1 || cs2[0].ID != cs[0].ID || !strings.Contains(cs2[0].Body, sha[:7]) {
		t.Errorf("comments after push = %+v", cs2)
	}
	if pds, _ := a.db.PagesPreviewDeploys(ctx, f.demo.ID); len(pds) != 1 {
		t.Errorf("%d preview deploys after a push", len(pds))
	}
	if n := len(a.pagesFiles().Snapshots(f.demo.ID)); n != 2 {
		t.Errorf("%d snapshots, want the site's and one preview", n)
	}
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Main site")
	// A restart's cleanup keeps preview snapshots.
	a.pagesCleanup(ctx)
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 200, "", "Feature v2")

	// Closing removes it; the comment says so.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/state", url.Values{"action": {"close"}}, f.ck), 303, "close")
	pagesBG.Wait()
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 404, "", "")
	if cs := botComments(t, a, f.demo.ID, 1); len(cs) != 1 || !strings.Contains(cs[0].Body, "removed: the pull request was closed") {
		t.Errorf("comment after close = %+v", cs)
	}
	if n := len(a.pagesFiles().Snapshots(f.demo.ID)); n != 1 {
		t.Errorf("%d snapshots after close", n)
	}
	// Reopening rebuilds it (editing the same comment).
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/state", url.Values{"action": {"reopen"}}, f.ck), 303, "reopen")
	pagesBG.Wait()
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 200, "", "Feature v2")

	// Merging removes it too.
	f.branch(t, "topic", "<h1>Topic</h1>")
	f.openPull(t, "topic", "Topic")
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/2/", nil), 200, "", "Topic")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/2/merge", url.Values{"method": {"merge"}}, f.ck), 303, "merge")
	pagesBG.Wait()
	a.actions().bg.Wait()
	pagesBG.Wait()
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/2/", nil), 404, "", "")
	if cs := botComments(t, a, f.demo.ID, 2); len(cs) != 1 || !strings.Contains(cs[0].Body, "removed: the pull request was merged") {
		t.Errorf("comment after merge = %+v", cs)
	}

	// Turning previews off removes them and stops new ones.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/previews", url.Values{}, f.ck), 303, "previews off")
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 404, "", "")
	f.branch(t, "third", "<h1>Third</h1>")
	f.openPull(t, "third", "Third")
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/3/", nil), 404, "", "")
	if cs := botComments(t, a, f.demo.ID, 3); len(cs) != 0 {
		t.Errorf("comments with previews off = %+v", cs)
	}
	// Private repositories get no previews.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/previews", url.Values{"previews": {"on"}}, f.ck), 303, "previews on")
	_ = a.db.UpdateRepo(ctx, f.demo.ID, "", true)
	f.branch(t, "fourth", "<h1>Fourth</h1>")
	f.openPull(t, "fourth", "Fourth")
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/4/", nil), 404, "", "")
}

func TestPagesPreviewApproval(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	f.previewSite(t)
	f.branch(t, "outside", "<h1>From outside</h1>")
	g, _ := a.git.Open("zoe", "demo")
	head, _ := g.BranchSHA(ctx, "outside")
	pr, err := a.db.CreatePull(ctx, store.NewPull{RepoID: f.demo.ID, Title: "Outside", Head: "outside", Base: "main", HeadSHA: head,
		AuthorKind: "import", AuthorName: "github:stranger"})
	if err != nil {
		t.Fatal(err)
	}
	a.emit(ctx, Event{Kind: EventPullOpened, Repo: f.demo, Number: pr.Number, Actor: Actor{Kind: "import", Name: "github:stranger"}})
	pagesBG.Wait()

	// Waits: nothing published, the comment explains, settings offer approval.
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 404, "", "")
	cs := botComments(t, a, f.demo.ID, 1)
	if len(cs) != 1 || !strings.Contains(cs[0].Body, "waiting for approval") {
		t.Fatalf("comments = %+v", cs)
	}
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String(), "settings", "Waiting for approval", "Approve and publish")
	sam, _ := a.db.CreateOwner(ctx, "sam", "", "user")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/previews/approve", url.Values{"number": {"1"}}, cookieFor(t, a, sam.ID)), 403, "sam approves")
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 404, "", "")
	// A stale approval (the head moved since the page was shown) is refused.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/previews/approve", url.Values{"number": {"1"}, "sha": {strings.Repeat("a", 40)}}, f.ck), 409, "stale approve")

	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/previews/approve", url.Values{"number": {"1"}, "sha": {head}}, f.ck), 303, "approve")
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 200, "", "From outside")
	if cs := botComments(t, a, f.demo.ID, 1); len(cs) != 1 || !strings.Contains(cs[0].Body, "is ready") {
		t.Errorf("comment after approval = %+v", cs)
	}
	// A new push from outside waits again; the approved preview stays as it was.
	runGit(t, f.work, "checkout", "-q", "outside")
	pullsCommit(t, f.work, "index.html", "<h1>Sneaky</h1>", "sneaky")
	f.push(t, "outside")
	runGit(t, f.work, "checkout", "-q", "main")
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 200, "", "From outside")
	if cs := botComments(t, a, f.demo.ID, 1); len(cs) != 1 || !strings.Contains(cs[0].Body, "waiting for approval") {
		t.Errorf("comment after outside push = %+v", cs)
	}
}

func TestPagesPreviewLimits(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	f.previewSite(t)
	a.cfg.PagesKeep = 2
	for i, b := range []string{"one", "two", "three"} {
		f.branch(t, b, "<h1>"+b+"</h1>")
		f.openPull(t, b, b)
		pagesWant(t, pagesGet(t, a, fmt.Sprintf("/~preview/zoe/demo/%d/", i+1), nil), 200, "", b)
	}
	// The least recently updated preview went; the site's deploys are separate.
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 404, "", "")
	if cs := botComments(t, a, f.demo.ID, 1); len(cs) != 1 || !strings.Contains(cs[0].Body, "only the 2 most recently updated previews are kept") {
		t.Errorf("expired comment = %+v", cs)
	}
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/2/", nil), 200, "", "two")
	if pds, _ := a.db.PagesPreviewDeploys(ctx, f.demo.ID); len(pds) != 2 {
		t.Errorf("%d preview deploys, want 2", len(pds))
	}
	for i := 0; i < 3; i++ { // site deploys keep their own budget
		pullsCommit(t, f.work, "index.html", fmt.Sprintf("<h1>Main %d</h1>", i), "main")
		f.push(t, "main")
	}
	if ds, _ := a.db.PagesDeploys(ctx, f.demo.ID, 100); len(ds) != 2 {
		t.Errorf("%d site deploys, want 2", len(ds))
	}
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/3/", nil), 200, "", "three")

	// Size limits apply: a too-big preview fails, says why and keeps the old one.
	a.cfg.PagesMaxFiles = 1
	runGit(t, f.work, "checkout", "-q", "three")
	pullsCommit(t, f.work, "extra.html", "x", "extra")
	f.push(t, "three")
	runGit(t, f.work, "checkout", "-q", "main")
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/3/", nil), 200, "", "three")
	if cs := botComments(t, a, f.demo.ID, 3); len(cs) != 1 || !strings.Contains(cs[0].Body, "failed") || !strings.Contains(cs[0].Body, "more than 1 files") {
		t.Errorf("failed comment = %+v", cs)
	}
}

func TestPagesPreviewFromActions(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	_ = a.db.SavePagesSite(ctx, store.PagesSite{RepoID: f.demo.ID, Enabled: true, Source: store.PagesFromActions, Branch: "main"})
	pullsCommit(t, f.work, "index.html", "x", "x")
	f.push(t, "main")
	f.branch(t, "feature", "y")
	f.openPull(t, "feature", "Feature")
	if cs := botComments(t, a, f.demo.ID, 1); len(cs) != 0 {
		t.Fatalf("Actions source commented before an upload: %+v", cs)
	}

	savedR, savedRun := tokenResolvers, pagesJobRun
	t.Cleanup(func() { tokenResolvers, pagesJobRun = savedR, savedRun })
	registerTokenResolver(func(_ context.Context, _ *App, s string) (Actor, bool) {
		if s == "atljob_pr" {
			return Actor{Kind: ActorJob, Name: "actions-job-7", Scopes: []string{"repo:read", fmt.Sprintf("actions:repo:%d", f.demo.ID), "actions:job:7"}}, true
		}
		return Actor{}, false
	})
	info := pagesJobInfo{Event: "pull_request", Ref: "refs/heads/feature", Pull: 1, Waiting: true}
	pagesJobRun = func(context.Context, *App, int64) (pagesJobInfo, error) { return info, nil }
	site := pagesTarGz(t, "index.html", "<h1>Built preview</h1>")

	// A job still waiting for approval can't upload.
	expectStatus(t, pagesUploadReq(t, a, "", "atljob_pr", site), http.StatusForbidden, "waiting job")
	info.Waiting = false
	rec := pagesUploadReq(t, a, "", "atljob_pr", site)
	expectStatus(t, rec, http.StatusCreated, "preview upload")
	mustContain(t, rec.Body.String(), "reply", pagesTestBase+"/~preview/zoe/demo/1/", `"preview":true`)
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 200, "", "Built preview")
	if ps := f.site(t); ps.LiveID != 0 {
		t.Error("a preview upload became the site")
	}
	if cs := botComments(t, a, f.demo.ID, 1); len(cs) != 1 || !strings.Contains(cs[0].Body, "is ready") {
		t.Errorf("comments = %+v", cs)
	}
	// Same size limits.
	a.cfg.PagesMaxBytes = 4
	expectStatus(t, pagesUploadReq(t, a, "", "atljob_pr", pagesTarGz(t, "index.html", strings.Repeat("x", 100))), http.StatusRequestEntityTooLarge, "too big")
	a.cfg.PagesMaxBytes = 0
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 200, "", "Built preview")
	// Unknown or closed pull requests and previews turned off are refused.
	info.Pull = 9
	expectStatus(t, pagesUploadReq(t, a, "", "atljob_pr", site), http.StatusConflict, "unknown pull")
	info.Pull = 1
	_ = a.db.SetPagesPreviews(ctx, f.demo.ID, false)
	expectStatus(t, pagesUploadReq(t, a, "", "atljob_pr", site), http.StatusConflict, "previews off")
	_ = a.db.SetPagesPreviews(ctx, f.demo.ID, true)
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/1/state", url.Values{"action": {"close"}}, f.ck), 303, "close")
	pagesBG.Wait()
	pagesWant(t, pagesGet(t, a, "/~preview/zoe/demo/1/", nil), 404, "", "")
	expectStatus(t, pagesUploadReq(t, a, "", "atljob_pr", site), http.StatusConflict, "closed pull")
}
