package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"atelier/internal/store"
)

// Pull request previews.
//
// When a pull request's head builds a site, the result is published at
//
//	<pages>/~preview/<owner>/<repo>/<number>/
//
// "~preview" sits where an owner name goes, and owner names must start with
// a letter or digit (gitrepo.ValidName), so no site, user site or file in a
// site can ever have that address. (A path under the site, like
// <pages>/<owner>/<repo>/_preview/, could clash with a folder the site has.)
//
//   - Branch source: the configured folder is published from the head commit
//     when the pull request opens or reopens and on every push to its head.
//   - Actions source: atelier/deploy-pages in a pull_request run uploads the
//     preview (pages_site_api.go).
//   - Pull requests from anyone but the owner wait for approval, like their
//     Actions runs: branch-source previews publish once an admin approves
//     that head commit in Settings → Pages; Actions jobs only get a token
//     after approval, and an upload from a job still waiting is refused.
//   - One comment on the pull request carries the link and is edited in
//     place on every push; the preview is removed when the pull request is
//     closed or merged.
//   - Previews are snapshots like deploys (same size limits, atomic switch)
//     and never become the live site. Each pull request keeps only its
//     current preview, and at most ATELIER_PAGES_KEEP previews are kept per
//     repository (separately from the site's deploys): the least recently
//     updated one goes first.
//   - Only public repositories get previews (they're on the shared host).

const (
	pagesPreviewSeg     = "~preview"
	pagesPreviewBot     = "atelier-pages"
	pagesPreviewWaiting = "waiting"
	pagesPreviewLive    = "live"
	pagesPreviewFailed  = "failed"
)

func init() {
	registerFeature(feature{
		name: "pages-previews",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("POST /{owner}/{repo}/settings/pages/previews", a.authed(a.pagesPreviewsToggle))
			m.HandleFunc("POST /{owner}/{repo}/settings/pages/previews/approve", a.authed(a.pagesPreviewApprove))
		},
	})
	registerEventHandler(func(ctx context.Context, a *App, ev Event) {
		switch ev.Kind {
		case EventPullOpened, EventPullReopened, EventPullClosed, EventPullMerged:
		default:
			return
		}
		rp := ev.Repo
		if rp.ID == 0 {
			found, err := a.db.RepoByID(ctx, ev.RepoID)
			if err != nil {
				return
			}
			rp = found
		}
		pagesBG.Add(1)
		go func() {
			defer pagesBG.Done()
			ctx := context.WithoutCancel(ctx)
			pr, err := a.db.PullByNumber(ctx, rp.ID, ev.Number)
			if err != nil {
				return
			}
			switch ev.Kind {
			case EventPullOpened, EventPullReopened:
				a.pagesPreviewPull(ctx, rp, pr)
			case EventPullClosed:
				a.pagesRemovePreview(ctx, rp, pr, "the pull request was closed")
			case EventPullMerged:
				a.pagesRemovePreview(ctx, rp, pr, "the pull request was merged")
			}
		}()
	})
}

// pagesPreviewLocks serialises preview work per repository (the push and
// the pull request event for the same commit can arrive together). Take it
// before pagesLock, never after.
var pagesPreviewLocks sync.Map // repoID → *sync.Mutex

func pagesPreviewLock(repoID int64) func() {
	m, _ := pagesPreviewLocks.LoadOrStore(repoID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// pagesPreviewURL is the address of a pull request's preview.
func (a *App) pagesPreviewURL(rp store.Repo, number int64) string {
	b := a.pagesBase()
	if b == "" {
		return ""
	}
	return b + "/" + pagesPreviewSeg + "/" + url.PathEscape(rp.Owner) + "/" + url.PathEscape(rp.Name) + "/" + strconv.FormatInt(number, 10) + "/"
}

func pagesPreviewRef(number int64) string { return fmt.Sprintf("refs/pull/%d/head", number) }

// pagesPullNeedsApproval mirrors the Actions gate (triggerPull): pull
// requests opened by anyone but the owner or the owner's tokens wait.
func pagesPullNeedsApproval(pr store.Pull) bool {
	return pr.AuthorKind != ActorOwner && pr.AuthorKind != ActorToken
}

// pagesPreviewsWanted reports whether a repository makes previews now.
func (a *App) pagesPreviewsWanted(ctx context.Context, rp store.Repo) (store.PagesSite, bool) {
	if !a.pagesEnabled() || rp.Private {
		return store.PagesSite{}, false
	}
	ps, err := a.db.PagesSite(ctx, rp.ID)
	if err != nil || !ps.Enabled {
		return ps, false
	}
	on, err := a.db.PagesPreviewsOn(ctx, rp.ID)
	return ps, err == nil && on
}

// ---------- serving ----------

// servePreview serves <pages>/~preview/<owner>/<repo>/<n>/…; segs starts at <owner>.
func (a *App) servePreview(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) < 3 {
		pagesPlain404(w, r)
		return
	}
	n, err := strconv.ParseInt(segs[2], 10, 64)
	if err != nil || n <= 0 || segs[2] != strconv.FormatInt(n, 10) {
		pagesPlain404(w, r)
		return
	}
	ctx := r.Context()
	rp, err := a.db.RepoByName(ctx, segs[0], segs[1])
	if err != nil {
		pagesPlain404(w, r)
		return
	}
	if _, ok := a.pagesPreviewsWanted(ctx, rp); !ok {
		pagesPlain404(w, r)
		return
	}
	pv, err := a.db.PagesPreview(ctx, rp.ID, n)
	if err != nil || pv.LiveID == 0 {
		pagesPlain404(w, r)
		return
	}
	if len(segs) == 3 {
		pagesRedirectSlash(w, r)
		return
	}
	w.Header().Set("X-Robots-Tag", "noindex")
	a.pagesServeFile(w, r, rp.ID, pv.LiveID, strings.Join(segs[3:], "/"), false)
}

// ---------- publishing ----------

// pagesPreviewOnPush refreshes the previews of open pull requests whose
// head branch a push moved (branch source).
func (a *App) pagesPreviewOnPush(ctx context.Context, rp store.Repo, ref, newSHA string) {
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || actZeroSHA(newSHA) {
		return
	}
	if _, ok := a.pagesPreviewsWanted(ctx, rp); !ok {
		return
	}
	pulls, err := a.db.Pulls(ctx, rp.ID, "open")
	if err != nil {
		return
	}
	for _, pr := range pulls {
		if pr.Head == branch {
			a.pagesPreviewPull(ctx, rp, pr)
		}
	}
}

// pagesPreviewPull publishes (or holds for approval) the preview of an open
// pull request whose site publishes from a branch.
func (a *App) pagesPreviewPull(ctx context.Context, rp store.Repo, pr store.Pull) {
	ps, ok := a.pagesPreviewsWanted(ctx, rp)
	if !ok || ps.Source != store.PagesFromBranch || !pr.IsOpen() {
		return
	}
	defer pagesPreviewLock(rp.ID)()
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return
	}
	sha, err := g.BranchSHA(ctx, pr.Head)
	if err != nil {
		return
	}
	pv, err := a.db.PagesPreview(ctx, rp.ID, pr.Number)
	if err != nil {
		pv = store.PagesPreview{RepoID: rp.ID, Number: pr.Number}
	}
	if pv.SHA == sha && pv.Status != pagesPreviewFailed && (pv.Status == pagesPreviewLive || pv.ApprovedSHA != sha) {
		return // nothing new (the push event and the pull request event can both arrive)
	}
	if pagesPullNeedsApproval(pr) && pv.ApprovedSHA != sha {
		pv.Status, pv.SHA, pv.Error = pagesPreviewWaiting, sha, ""
		a.pagesPreviewComment(ctx, rp, pr, &pv)
		_ = a.db.SavePagesPreview(ctx, pv)
		return
	}
	feed, err := pagesFolderFeed(ctx, g, sha, ps.Folder, pr.Head)
	if err != nil {
		pv.Status, pv.SHA, pv.Error = pagesPreviewFailed, sha, pagesErrText(err)
		a.pagesPreviewComment(ctx, rp, pr, &pv)
		_ = a.db.SavePagesPreview(ctx, pv)
		return
	}
	if _, err := a.pagesPublishPreview(ctx, rp, pr, sha, pagesPreviewBot, feed); err != nil {
		log.Printf("pages: preview of %s#%d: %v", rp.FullName(), pr.Number, err)
	}
}

// pagesPublishPreview builds a preview snapshot from feed and makes it the
// pull request's preview, replacing the previous one, then updates the
// comment and enforces the preview limit. The caller holds pagesPreviewLock.
func (a *App) pagesPublishPreview(ctx context.Context, rp store.Repo, pr store.Pull, sha, actor string, feed func(extract func(io.Reader) error) error) (store.PagesDeploy, error) {
	ctx = context.WithoutCancel(ctx)
	unlock := pagesLock(rp.ID)
	d, err := a.pagesBuild(ctx, rp, store.PagesDeploy{RepoID: rp.ID, Source: store.PagesFromPreview, Ref: pagesPreviewRef(pr.Number), SHA: sha, Actor: actor}, feed)
	pv, perr := a.db.PagesPreview(ctx, rp.ID, pr.Number)
	if perr != nil {
		pv = store.PagesPreview{RepoID: rp.ID, Number: pr.Number}
	}
	files := a.pagesFiles()
	if err != nil {
		if d.ID != 0 {
			_ = a.db.DeletePagesDeploy(ctx, rp.ID, d.ID) // the reason lives in the preview row
		}
		pv.Status, pv.SHA, pv.Error = pagesPreviewFailed, sha, pagesErrText(err)
		a.pagesPreviewComment(ctx, rp, pr, &pv)
		_ = a.db.SavePagesPreview(ctx, pv)
		unlock()
		return d, err
	}
	old := pv.LiveID
	pv.Status, pv.LiveID, pv.SHA, pv.Error = pagesPreviewLive, d.ID, sha, ""
	a.pagesPreviewComment(ctx, rp, pr, &pv)
	if err := a.db.SavePagesPreview(ctx, pv); err != nil {
		unlock()
		return d, err
	}
	if old != 0 && old != d.ID {
		_ = files.Remove(rp.ID, old)
		_ = a.db.DeletePagesDeploy(ctx, rp.ID, old)
	}
	unlock()
	a.pagesPrunePreviews(ctx, rp, pr.Number)
	return d, nil
}

// pagesPrunePreviews keeps at most pagesKeep live previews, removing the
// least recently updated ones (never keep, the one just published).
func (a *App) pagesPrunePreviews(ctx context.Context, rp store.Repo, keep int64) {
	pvs, err := a.db.PagesPreviews(ctx, rp.ID)
	if err != nil {
		return
	}
	n := 0
	for _, pv := range pvs { // most recently updated first
		if pv.LiveID == 0 {
			continue
		}
		n++
		if n <= a.pagesKeep() || pv.Number == keep {
			continue
		}
		pr, err := a.db.PullByNumber(ctx, rp.ID, pv.Number)
		if err != nil {
			continue
		}
		a.pagesRemovePreviewLocked(ctx, rp, pr, fmt.Sprintf("only the %d most recently updated previews are kept; push again to rebuild it", a.pagesKeep()))
	}
}

// pagesRemovePreview deletes a pull request's preview and says why in its comment.
func (a *App) pagesRemovePreview(ctx context.Context, rp store.Repo, pr store.Pull, why string) {
	defer pagesPreviewLock(rp.ID)()
	a.pagesRemovePreviewLocked(ctx, rp, pr, why)
}

func (a *App) pagesRemovePreviewLocked(ctx context.Context, rp store.Repo, pr store.Pull, why string) {
	unlock := pagesLock(rp.ID)
	defer unlock()
	pv, err := a.db.PagesPreview(ctx, rp.ID, pr.Number)
	if err != nil {
		return
	}
	files := a.pagesFiles()
	ds, _ := a.db.PagesPreviewDeploys(ctx, rp.ID)
	for _, d := range ds {
		if d.Ref == pagesPreviewRef(pr.Number) {
			_ = files.Remove(rp.ID, d.ID)
			_ = a.db.DeletePagesDeploy(ctx, rp.ID, d.ID)
		}
	}
	if pv.CommentID != 0 {
		body := "**Pages preview** removed: " + why + "."
		_ = a.db.SetPullCommentBody(ctx, pr.ID, pv.CommentID, body)
	}
	_ = a.db.DeletePagesPreview(ctx, rp.ID, pr.Number)
}

// pagesRemoveAllPreviews takes every preview of a repository down.
func (a *App) pagesRemoveAllPreviews(ctx context.Context, rp store.Repo, why string) {
	pvs, _ := a.db.PagesPreviews(ctx, rp.ID)
	for _, pv := range pvs {
		if pr, err := a.db.PullByNumber(ctx, rp.ID, pv.Number); err == nil {
			a.pagesRemovePreview(ctx, rp, pr, why)
		}
	}
}

// pagesPreviewComment writes the preview's state into its pull request
// comment, posting it the first time and editing it afterwards.
func (a *App) pagesPreviewComment(ctx context.Context, rp store.Repo, pr store.Pull, pv *store.PagesPreview) {
	short := pv.SHA
	if len(short) > 7 {
		short = short[:7]
	}
	var body string
	switch pv.Status {
	case pagesPreviewLive:
		u := a.pagesPreviewURL(rp, pr.Number)
		body = fmt.Sprintf("**Pages preview** of `%s` is ready: [%s](%s)\n\nIt's updated on every push and removed when the pull request is closed or merged.", short, u, u)
	case pagesPreviewWaiting:
		body = fmt.Sprintf("**Pages preview** of `%s` is waiting for approval: this pull request was opened by %s (%s), not the owner. "+
			"An admin can publish it from [Settings → Pages](/%s/%s/settings/pages#pages-previews).", short, pr.AuthorName, pr.AuthorKind, rp.Owner, rp.Name)
	default:
		body = fmt.Sprintf("**Pages preview** of `%s` failed: %s", short, pv.Error)
		if pv.LiveID != 0 {
			body += "\n\nThe previous preview is still up: " + a.pagesPreviewURL(rp, pr.Number)
		}
	}
	if pv.CommentID != 0 {
		err := a.db.SetPullCommentBody(ctx, pr.ID, pv.CommentID, body)
		if err == nil {
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			return
		}
	}
	c, err := a.db.AddPullComment(ctx, pr.ID, ActorBot, pagesPreviewBot, body)
	if err == nil {
		pv.CommentID = c.ID
	}
}

// ---------- settings ----------

func (a *App) pagesPreviewsToggle(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	on := r.FormValue("previews") == "on"
	if err := a.db.SetPagesPreviews(ctx, v.Repo.ID, on); err != nil {
		a.fail(w, r, err)
		return
	}
	if !on {
		a.pagesRemoveAllPreviews(ctx, v.Repo, "previews were turned off")
	}
	http.Redirect(w, r, v.Base+"/settings/pages?saved=1#pages-previews", http.StatusSeeOther)
}

// pagesPreviewApprove publishes a waiting preview: the approval covers the
// head commit it was given for (a later push from outside waits again).
func (a *App) pagesPreviewApprove(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	n, err := strconv.ParseInt(r.FormValue("number"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	pv, err := a.db.PagesPreview(ctx, v.Repo.ID, n)
	if err != nil || pv.Status != pagesPreviewWaiting {
		a.notFound(w, r)
		return
	}
	pr, err := a.db.PullByNumber(ctx, v.Repo.ID, n)
	if err != nil || !pr.IsOpen() {
		a.notFound(w, r)
		return
	}
	if sha := r.FormValue("sha"); sha != "" && sha != pv.SHA {
		a.pagesRender(w, r, "The pull request changed since you looked; review the new commit first.", http.StatusConflict)
		return
	}
	pv.ApprovedSHA = pv.SHA
	if err := a.db.SavePagesPreview(ctx, pv); err != nil {
		a.fail(w, r, err)
		return
	}
	a.pagesPreviewPull(ctx, v.Repo, pr)
	http.Redirect(w, r, v.Base+"/settings/pages?saved=1#pages-previews", http.StatusSeeOther)
}
