package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// "Update branch": merge a pull request's base branch into its head branch
// when the base has moved on. One operation (updatePullBranch) serves the web
// button, the app API and the MCP tool. It uses the same bare-repository
// merge as merging a pull request (gitrepo.MergeBranches, with the branches
// the other way round), then queues the ref update like a push so repo.pushed
// fires and the pull request's checks run again.

// ---------- conflicts, cached ----------

// pullMergeChecks remembers trial merges by (base commit, head commit), so
// lists of pull requests don't run merge-tree again for branches that haven't
// moved. Commits never change, so an entry never goes stale.
var pullMergeChecks = struct {
	sync.Mutex
	m map[string]*gitrepo.MergeCheck
}{m: map[string]*gitrepo.MergeCheck{}}

const pullMergeChecksMax = 4096

// pullMergeCheck is g.CheckMerge(base, head) through the cache.
func pullMergeCheck(ctx context.Context, g *gitrepo.Repo, base, head string) (*gitrepo.MergeCheck, error) {
	key := base + " " + head
	pullMergeChecks.Lock()
	mc := pullMergeChecks.m[key]
	pullMergeChecks.Unlock()
	if mc != nil {
		return mc, nil
	}
	mc, err := g.CheckMerge(ctx, base, head)
	if err != nil {
		return nil, err
	}
	pullMergeChecks.Lock()
	if len(pullMergeChecks.m) >= pullMergeChecksMax {
		clear(pullMergeChecks.m)
	}
	pullMergeChecks.m[key] = mc
	pullMergeChecks.Unlock()
	return mc, nil
}

// pullConflicts reports whether an open pull request's head can't be merged
// cleanly into its base right now (false when a branch is missing or the
// merge can't be checked). It is what the list page's "Conflicts" pill shows.
func pullConflicts(ctx context.Context, g *gitrepo.Repo, p store.Pull) bool {
	if !p.IsOpen() {
		return false
	}
	head, err := g.BranchSHA(ctx, p.Head)
	if err != nil {
		return false
	}
	base, err := g.BranchSHA(ctx, p.Base)
	if err != nil {
		return false
	}
	mc, err := pullMergeCheck(ctx, g, base, head)
	return err == nil && !mc.Clean
}

// ---------- the operation ----------

// pullUpdateInfo is what the pull request page and the API say about
// "Update branch" for one open pull request.
type pullUpdateInfo struct {
	Behind    int      // commits on the base that the head doesn't have
	Can       bool     // the update would go through now
	Reason    string   // why not (empty when Can, or when there's nothing to update)
	Conflicts []string // files that conflict
}

// pullUpdateResult is what an update did.
type pullUpdateResult struct {
	Updated bool   // false: the head already had everything on the base
	HeadSHA string // the head branch's commit now
}

// pullUpdateError is why an update was refused. Kind is the app API's error
// code: forbidden, not_open, on_github, branch_missing, unrelated,
// branch_protected, conflicts, branch_moved.
type pullUpdateError struct {
	Code  int
	Kind  string
	Msg   string
	Files []string // conflicts: the conflicting files
}

func (e *pullUpdateError) Error() string { return e.Msg }

// pullHeadRuleRefusal says why a branch rule on the head branch keeps Atelier
// from adding to it ("" when it may). It mirrors what the pre-receive hook
// would say to a push of the same commit (cmd/atelier/hook.go). The update is
// always a fast-forward of the branch, so "no force pushes" never applies.
func (a *App) pullHeadRuleRefusal(ctx context.Context, repoID int64, branch string, mergeCommit bool) string {
	pr, err := a.db.ProtectionFor(ctx, repoID, branch)
	switch {
	case err != nil:
		return "Atelier couldn't read the branch rules for " + branch + "."
	case pr.RequirePR:
		return branch + " is protected by branch rule " + pr.Patterns() + ": changes must go through a pull request."
	case pr.LinearHistory && mergeCommit:
		return branch + " is protected by branch rule " + pr.Patterns() + ": merge commits aren't allowed (linear history)."
	}
	return ""
}

// pullUpdatePlan works out, without writing anything, what updating p's head
// branch would do for someone who may write here: nothing (res set, already
// up to date), a refusal (err is a *pullUpdateError), or a merge that can go
// ahead (both nil).
func (a *App) pullUpdatePlan(ctx context.Context, rp store.Repo, g *gitrepo.Repo, p store.Pull) (*pullUpdateResult, error) {
	refuse := func(code int, kind, msg string) (*pullUpdateResult, error) {
		return nil, &pullUpdateError{Code: code, Kind: kind, Msg: msg}
	}
	if !p.IsOpen() {
		return refuse(http.StatusUnprocessableEntity, "not_open", "This pull request isn't open.")
	}
	if a.appOnGitHub(ctx, rp.ID) {
		return refuse(http.StatusUnprocessableEntity, "on_github",
			"This repository's home is GitHub, so Atelier doesn't change its branches. Update the branch on GitHub.")
	}
	head, err := g.BranchSHA(ctx, p.Head)
	if err != nil {
		return refuse(http.StatusUnprocessableEntity, "branch_missing", "The head branch "+p.Head+" isn't in this repository.")
	}
	base, err := g.BranchSHA(ctx, p.Base)
	if err != nil {
		return refuse(http.StatusUnprocessableEntity, "branch_missing", "The base branch "+p.Base+" no longer exists.")
	}
	if current, err := g.IsAncestor(ctx, base, head); err != nil {
		return nil, err
	} else if current {
		return &pullUpdateResult{HeadSHA: head}, nil
	}
	ff, err := g.IsAncestor(ctx, head, base)
	if err != nil {
		return nil, err
	}
	if msg := a.pullHeadRuleRefusal(ctx, rp.ID, p.Head, !ff); msg != "" {
		return refuse(http.StatusForbidden, "branch_protected", msg)
	}
	if ff {
		return nil, nil
	}
	// The same trial merge the page's "No conflicts" pill comes from.
	mc, err := pullMergeCheck(ctx, g, base, head)
	if errors.Is(err, gitrepo.ErrUnrelated) {
		return refuse(http.StatusUnprocessableEntity, "unrelated", "The branches have no common history.")
	}
	if err != nil {
		return nil, err
	}
	if !mc.Clean {
		return nil, pullUpdateConflict(p, mc.Conflicts)
	}
	return nil, nil
}

func pullUpdateConflict(p store.Pull, files []string) *pullUpdateError {
	return &pullUpdateError{Code: http.StatusConflict, Kind: "conflicts", Files: files,
		Msg: fmt.Sprintf("%s can't be merged into %s automatically: %s in %s. Resolve them locally: "+
			"git fetch origin, git checkout %s, git merge origin/%s, fix the conflicts, then git commit and git push.",
			p.Base, p.Head, pluralWord(len(files), "conflict"), strings.Join(files, ", "), p.Head, p.Base)}
}

// pullUpdateInfoFor fills the page's and the API's view of "Update branch".
func (a *App) pullUpdateInfoFor(ctx context.Context, rp store.Repo, g *gitrepo.Repo, d *pullPageData) pullUpdateInfo {
	var u pullUpdateInfo
	if !d.P.IsOpen() || d.Cmp == nil || d.Cmp.Unrelated || d.Cmp.Behind == 0 {
		return u
	}
	u.Behind = d.Cmp.Behind
	res, err := a.pullUpdatePlan(ctx, rp, g, d.P)
	var ue *pullUpdateError
	switch {
	case errors.As(err, &ue):
		u.Reason, u.Conflicts = ue.Msg, ue.Files
		if ue.Kind == "conflicts" {
			u.Reason = "Resolve the conflicts with " + d.P.Base + " locally first."
		}
	case err != nil:
		u.Reason = "Atelier couldn't check this update."
	default:
		u.Can = res == nil
	}
	return u
}

// updatePullBranch merges p's base branch into its head branch for who: a
// merge commit "Merge branch '<base>' into <head>" by who (a fast-forward
// when the head has nothing of its own). Nothing is written when the
// branches conflict. Refusals come back as *pullUpdateError.
func (a *App) updatePullBranch(ctx context.Context, rp store.Repo, g *gitrepo.Repo, p store.Pull, who Actor) (*pullUpdateResult, error) {
	if !a.can(ctx, who, rp, PermWrite) {
		return nil, &pullUpdateError{Code: http.StatusForbidden, Kind: "forbidden", Msg: "You don't have write access to this repository."}
	}
	if res, err := a.pullUpdatePlan(ctx, rp, g, p); res != nil || err != nil {
		return res, err
	}
	sig := a.editorIdentity(ctx, who) // the identity pull request merges use
	res, err := g.MergeBranches(ctx, gitrepo.MergeRequest{
		// The other way round from merging the pull request: the head branch
		// is the one that moves.
		Base: p.Head, Head: p.Base, Method: "merge", FastForward: true,
		Message: fmt.Sprintf("Merge branch '%s' into %s", p.Base, p.Head), Committer: sig, Author: sig,
	})
	var ce *gitrepo.MergeConflictError
	switch {
	case errors.As(err, &ce):
		return nil, pullUpdateConflict(p, ce.Files)
	case errors.Is(err, gitrepo.ErrNothingToMerge):
		head, err := g.BranchSHA(ctx, p.Head)
		return &pullUpdateResult{HeadSHA: head}, err
	case errors.Is(err, gitrepo.ErrRefMoved):
		return nil, &pullUpdateError{Code: http.StatusConflict, Kind: "branch_moved",
			Msg: p.Head + " changed while updating (probably a new push). Try again."}
	case errors.Is(err, gitrepo.ErrUnrelated):
		return nil, &pullUpdateError{Code: http.StatusUnprocessableEntity, Kind: "unrelated", Msg: "The branches have no common history."}
	case err != nil:
		return nil, err
	}

	// The pull request follows its branch, and the update is queued exactly
	// like a push (as web edits are): repo.pushed, Actions and the pull
	// request's own checks see it the same way.
	_ = a.db.SetPullHeadSHA(ctx, p.ID, res.SHA)
	_ = a.db.TouchRepo(ctx, rp.ID)
	if err := a.db.QueueActionsPush(ctx, store.ActionsPush{Owner: rp.Owner, Repo: rp.Name, Old: res.BaseBefore, New: res.SHA,
		Ref: "refs/heads/" + p.Head, Pusher: who.Kind + ":" + who.Name}); err != nil {
		log.Printf("pulls: Actions won't see %s on %s of %s: %v", res.SHA, p.Head, rp.FullName(), err)
	}
	return &pullUpdateResult{Updated: true, HeadSHA: res.SHA}, nil
}

// ---------- web ----------

// pullUpdateBranch is the "Update branch" button on the pull request page.
func (a *App) pullUpdateBranch(w http.ResponseWriter, r *http.Request) {
	v, g, p, ok := a.loadPull(w, r) // POST: needs write
	if !ok {
		return
	}
	res, err := a.updatePullBranch(r.Context(), v.Repo, g, p, a.actor(r))
	var ue *pullUpdateError
	switch {
	case errors.As(err, &ue):
		a.renderPull(w, r, v, g, p, "overview", ue.Msg, ue.Code)
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	note := "current"
	if res.Updated {
		note = "updated"
	}
	http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d?branch=%s", v.Base, p.Number, note), http.StatusSeeOther)
}

// ---------- app API ----------

func (a *App) appPullUpdate(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, p, ok := a.appLoadPull(w, r, c, PermWrite)
	if !ok {
		return
	}
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	res, err := a.updatePullBranch(r.Context(), rp, g, p, c.Who)
	var ue *pullUpdateError
	switch {
	case errors.As(err, &ue) && ue.Kind == "conflicts":
		writeJSON(w, ue.Code, map[string]any{"error": ue.Kind, "message": ue.Msg, "files": ue.Files})
		return
	case errors.As(err, &ue):
		appError(w, ue.Code, ue.Kind, ue.Msg)
		return
	case err != nil:
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": res.Updated, "head_sha": res.HeadSHA})
}
