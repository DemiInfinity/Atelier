package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
	"atelier/internal/tether"
)

// A repository's GitHub twin.
//
// Home: Atelier — Atelier owns the default branch. Every branch and tag is
// mirrored to GitHub after it changes (the loop compares refs every 30
// seconds, so Atelier's own merges are mirrored too). The default branch is
// never force-pushed: if GitHub's has commits Atelier lacks, the link shows a
// reconcile banner. Protecting the branch on GitHub is up to you (the page
// shows the steps).
//
// Home: GitHub — GitHub owns the default branch. Atelier fetches GitHub's
// branches into refs/tether/heads/* and fast-forwards its own; merges happen
// on GitHub ("Merge on GitHub"); Atelier-only branches are pushed so they
// can be merged there.
//
// Switching home needs both default branches to be identical.

func tetherRepoRoutes(a *App, m *http.ServeMux) {
	m.HandleFunc("GET /{owner}/{repo}/settings/tether", a.authed(a.tetherRepoPage))
	m.HandleFunc("POST /{owner}/{repo}/settings/tether/link", a.authed(a.tetherRepoLink))
	m.HandleFunc("POST /{owner}/{repo}/settings/tether/home", a.authed(a.tetherRepoHome))
	m.HandleFunc("POST /{owner}/{repo}/settings/tether/reconcile", a.authed(a.tetherRepoReconcile))
	m.HandleFunc("POST /{owner}/{repo}/settings/tether/sync", a.authed(a.tetherRepoSyncNow))
	m.HandleFunc("POST /{owner}/{repo}/settings/tether/unlink", a.authed(a.tetherRepoUnlink))
	m.HandleFunc("GET /{owner}/{repo}/tether/status", a.authed(a.tetherRepoStatus))
}

// ---------- pages ----------

func (a *App) tetherRepoPage(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	a.renderTetherRepo(w, r, v, "", http.StatusOK)
}

func (a *App) renderTetherRepo(w http.ResponseWriter, r *http.Request, v *repoView, errMsg string, code int) {
	ctx := r.Context()
	who := a.actor(r)
	d := map[string]any{"V": v}
	acc, err := a.db.TetherAccount(ctx, who.PersonID)
	if err == nil {
		d["Account"] = acc
		d["Suggest"] = acc.Login + "/" + v.Repo.Name
	}
	if l, err := a.db.TetherLink(ctx, v.Repo.ID); err == nil {
		d["Link"] = l
		d["GHURL"] = a.tetherWebURL(l.GHRepo)
		d["ProtectURL"] = a.tetherWebURL(l.GHRepo + "/settings/branches")
		d["OtherPerson"] = l.PersonID != who.PersonID
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "tether_repo", page{Title: "Tether · " + v.Repo.FullName(), Nav: "repos", Error: errMsg,
		Notice: r.URL.Query().Get("saved"), Data: d})
}

// tetherRepoStatus feeds the "Tether this repo" card on the code page.
func (a *App) tetherRepoStatus(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "code")
	if !ok {
		return
	}
	ctx := r.Context()
	out := map[string]any{"linked": false}
	admin := a.can(ctx, a.actor(r), v.Repo, PermAdmin)
	if admin {
		out["settings_url"] = v.Base + "/settings/tether"
	}
	if l, err := a.db.TetherLink(ctx, v.Repo.ID); err == nil {
		out["linked"] = true
		out["gh_repo"] = l.GHRepo
		out["url"] = a.tetherWebURL(l.GHRepo)
		out["home"] = l.Home
		if admin {
			out["diverged"] = l.Diverged != ""
			out["problem"] = l.PushError != "" || l.FetchError != "" || l.SyncError != ""
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// ---------- linking ----------

func (a *App) tetherRepoLink(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	gh := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(r.FormValue("gh_repo"), a.tether().web+"/"), ".git"))
	home := r.FormValue("home")
	if home != store.TetherHomeAtelier && home != store.TetherHomeGitHub {
		home = store.TetherHomeAtelier
	}
	if !tether.ValidRepo(gh) {
		a.renderTetherRepo(w, r, v, "Enter the GitHub repository as owner/name.", http.StatusUnprocessableEntity)
		return
	}
	c, acc, err := a.tetherClient(ctx, who.PersonID)
	if err != nil {
		a.renderTetherRepo(w, r, v, tetherErrText(err), http.StatusUnprocessableEntity)
		return
	}
	rp, err := c.Repo(ctx, gh)
	if tether.IsStatus(err, http.StatusNotFound) && r.FormValue("create") != "" {
		owner, name, _ := strings.Cut(gh, "/")
		if !strings.EqualFold(owner, acc.Login) {
			a.renderTetherRepo(w, r, v, "Tether can only create the twin under your own account ("+acc.Login+").", http.StatusUnprocessableEntity)
			return
		}
		rp, err = c.CreateRepo(ctx, name, eventExcerpt(v.Repo.Description, 300), true)
	}
	if err != nil {
		a.renderTetherRepo(w, r, v, "Couldn't use "+gh+": "+tetherErrText(err), http.StatusUnprocessableEntity)
		return
	}
	if rp.Permissions != nil && !rp.Permissions.Push {
		a.renderTetherRepo(w, r, v, "Your token can't push to "+gh+". Give it Contents: read and write on that repository.", http.StatusUnprocessableEntity)
		return
	}
	if rp.FullName != "" && tether.ValidRepo(rp.FullName) {
		gh = rp.FullName
	}
	l := store.TetherLink{RepoID: v.Repo.ID, PersonID: who.PersonID, GHRepo: gh, Home: home, SyncIssues: r.FormValue("sync_issues") != ""}
	if err := a.db.SaveTetherLink(ctx, l); err != nil {
		a.fail(w, r, err)
		return
	}
	a.tetherSyncSoon(ctx, v.Repo.ID)
	http.Redirect(w, r, v.Base+"/settings/tether?saved=1", http.StatusSeeOther)
}

// tetherSyncSoon syncs a repository in the background.
func (a *App) tetherSyncSoon(ctx context.Context, repoID int64) {
	bctx := context.WithoutCancel(ctx)
	a.tether().goBG(fmt.Sprintf("sync:%d", repoID), func() { a.tetherSyncRepo(bctx, repoID, true) })
}

func (a *App) tetherRepoSyncNow(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	a.tetherSyncSoon(r.Context(), v.Repo.ID)
	http.Redirect(w, r, v.Base+"/settings/tether?saved=1", http.StatusSeeOther)
}

func (a *App) tetherRepoUnlink(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	unlock := a.tether().lockRepo(v.Repo.ID)
	err := a.db.DeleteTetherLink(r.Context(), v.Repo.ID)
	unlock()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/tether?saved=1", http.StatusSeeOther)
}

// ---------- home ----------

// tetherMains compares the default branch here and on GitHub.
func (a *App) tetherMains(ctx context.Context, rp store.Repo, l store.TetherLink, token string) (local, remote, branch string, err error) {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return "", "", "", err
	}
	branch = g.DefaultBranch(ctx)
	local, _ = g.BranchSHA(ctx, branch)
	out, err := a.tetherGit(ctx, a.git.Path(rp.Owner, rp.Name), token, "", "ls-remote", "--", a.tetherRemote(l.GHRepo), "refs/heads/"+branch)
	if err != nil {
		return local, "", branch, err
	}
	if f := strings.Fields(out); len(f) >= 1 {
		remote = f[0]
	}
	return local, remote, branch, nil
}

func (a *App) tetherRepoHome(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	home := r.FormValue("home")
	if home != store.TetherHomeAtelier && home != store.TetherHomeGitHub {
		a.renderTetherRepo(w, r, v, "Choose Atelier or GitHub.", http.StatusUnprocessableEntity)
		return
	}
	err := a.tetherSwitchHome(ctx, v.Repo, home)
	var div *tetherDiverged
	switch {
	case errors.As(err, &div):
		a.renderTetherRepo(w, r, v, "Both default branches must be identical to switch home. "+div.Error(), http.StatusConflict)
		return
	case err != nil:
		a.renderTetherRepo(w, r, v, "Couldn't switch home: "+tetherErrText(err), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/tether?saved=1", http.StatusSeeOther)
}

// tetherDiverged says the two default branches differ.
type tetherDiverged struct{ Why string }

func (e *tetherDiverged) Error() string { return e.Why }

// tetherSwitchHome changes which side owns the default branch.
func (a *App) tetherSwitchHome(ctx context.Context, rp store.Repo, home string) error {
	unlock := a.tether().lockRepo(rp.ID)
	defer unlock()
	l, err := a.db.TetherLink(ctx, rp.ID)
	if err != nil {
		return err
	}
	if l.Home == home {
		return nil
	}
	tok, err := a.db.TetherToken(ctx, a.secretSealer(), l.PersonID)
	if err != nil {
		return errTetherNotConnected
	}
	local, remote, branch, err := a.tetherMains(ctx, rp, l, tok)
	if err != nil {
		return err
	}
	if local != remote {
		why := a.tetherDivergence(ctx, rp, branch, local, remote)
		l.Diverged = why
		_ = a.db.SaveTetherLink(ctx, l)
		return &tetherDiverged{Why: why}
	}
	l.Home, l.Diverged, l.PushError, l.FetchError = home, "", "", ""
	l.PushedRefs = "" // re-learnt on the next sync in the new direction
	return a.db.SaveTetherLink(ctx, l)
}

// tetherDivergence describes how local and remote differ.
func (a *App) tetherDivergence(ctx context.Context, rp store.Repo, branch, local, remote string) string {
	dir := a.git.Path(rp.Owner, rp.Name)
	switch {
	case remote == "":
		return fmt.Sprintf("GitHub has no %s branch yet.", branch)
	case local == "":
		return fmt.Sprintf("Atelier has no %s branch yet.", branch)
	}
	// Make sure GitHub's commit is here to compare (best effort).
	if !a.tetherHasCommit(ctx, dir, remote) {
		return fmt.Sprintf("%s differs: GitHub is at %s, Atelier at %s.", branch, shortSHA(remote), shortSHA(local))
	}
	switch {
	case a.tetherIsAncestor(ctx, dir, remote, local):
		return fmt.Sprintf("Atelier's %s has commits GitHub doesn't (GitHub is behind at %s).", branch, shortSHA(remote))
	case a.tetherIsAncestor(ctx, dir, local, remote):
		return fmt.Sprintf("GitHub's %s has commits Atelier doesn't (Atelier is behind at %s).", branch, shortSHA(local))
	}
	return fmt.Sprintf("%s has different commits on each side (GitHub %s, Atelier %s).", branch, shortSHA(remote), shortSHA(local))
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func (a *App) tetherHasCommit(ctx context.Context, dir, sha string) bool {
	_, err := a.tetherGit(ctx, dir, "", "", "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// tetherRepoReconcile makes the default branches identical, one way or the other.
func (a *App) tetherRepoReconcile(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	if err := a.tetherReconcile(r.Context(), v.Repo, r.FormValue("keep")); err != nil {
		a.renderTetherRepo(w, r, v, "Couldn't reconcile: "+tetherErrText(err), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/tether?saved=1", http.StatusSeeOther)
}

// tetherReconcile: keep "atelier" force-pushes Atelier's default branch to
// GitHub; keep "github" moves Atelier's to GitHub's (the old tip is kept
// under refs/tether/backup/).
func (a *App) tetherReconcile(ctx context.Context, rp store.Repo, keep string) error {
	if keep != "atelier" && keep != "github" {
		return errors.New("choose which side to keep")
	}
	unlock := a.tether().lockRepo(rp.ID)
	defer unlock()
	l, err := a.db.TetherLink(ctx, rp.ID)
	if err != nil {
		return err
	}
	tok, err := a.db.TetherToken(ctx, a.secretSealer(), l.PersonID)
	if err != nil {
		return errTetherNotConnected
	}
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return err
	}
	dir := a.git.Path(rp.Owner, rp.Name)
	branch := g.DefaultBranch(ctx)
	ref := "refs/heads/" + branch
	remote := a.tetherRemote(l.GHRepo)
	if keep == "atelier" {
		if _, err := g.BranchSHA(ctx, branch); err != nil {
			return fmt.Errorf("Atelier has no %s branch", branch)
		}
		if _, err := a.tetherGit(ctx, dir, tok, "", "push", "--porcelain", "--", remote, "+"+ref+":"+ref); err != nil {
			return err
		}
	} else {
		if _, err := a.tetherGit(ctx, dir, tok, "", "fetch", "--no-tags", "--", remote, "+"+ref+":refs/tether/heads/"+branch); err != nil {
			return err
		}
		refs, err := a.tetherRefs(ctx, dir, "refs/tether/heads/"+branch, ref)
		if err != nil {
			return err
		}
		theirs, mine := refs["refs/tether/heads/"+branch], refs[ref]
		if theirs == "" {
			return fmt.Errorf("GitHub has no %s branch", branch)
		}
		if mine != "" && mine != theirs {
			backup := fmt.Sprintf("refs/tether/backup/%s-%d", branch, time.Now().Unix())
			if err := a.tetherUpdateRef(ctx, dir, backup, mine, ""); err != nil {
				return err
			}
		}
		if err := a.tetherUpdateRef(ctx, dir, ref, theirs, mine); err != nil {
			return err
		}
		if mine != theirs {
			a.tetherQueuePush(ctx, rp, ref, mine, theirs)
		}
	}
	l.Diverged, l.PushError, l.FetchError = "", "", ""
	return a.db.SaveTetherLink(ctx, l)
}

// tetherQueuePush hands a ref update Tether made to Actions, as the
// post-receive hook would.
func (a *App) tetherQueuePush(ctx context.Context, rp store.Repo, ref, old, nw string) {
	if old == "" {
		old = strings.Repeat("0", 40)
	}
	if err := a.db.QueueActionsPush(ctx, store.ActionsPush{Owner: rp.Owner, Repo: rp.Name, Old: old, New: nw, Ref: ref,
		Pusher: "bot:github"}); err != nil {
		log.Printf("tether: queueing push for %s: %v", rp.FullName(), err)
	}
}

// ---------- syncing ----------

// tetherSyncRepo brings one linked repository up to date. A full sync also
// fetches issues, pull requests and comments; a light one only mirrors
// pushes (Home: Atelier).
func (a *App) tetherSyncRepo(ctx context.Context, repoID int64, full bool) {
	st := a.tether()
	unlock := st.lockRepo(repoID)
	defer unlock()
	l, err := a.db.TetherLink(ctx, repoID)
	if err != nil {
		return
	}
	rp, err := a.repoByID(ctx, repoID)
	if err != nil {
		return
	}
	if l.Home == store.TetherHomeAtelier {
		a.tetherPush(ctx, rp, &l)
	} else if !full {
		return
	}
	if !full {
		_ = a.db.SaveTetherLink(ctx, l)
		return
	}
	c, acc, cerr := a.tetherClient(ctx, l.PersonID)
	if cerr != nil {
		l.SyncError = tetherErrText(cerr)
		_ = a.db.SaveTetherLink(ctx, l)
		return
	}
	if _, blocked := c.BlockedUntil(); blocked {
		_ = a.db.SaveTetherLink(ctx, l)
		return
	}
	var ghPulls []tether.Pull
	var pullsErr error
	if l.Home == store.TetherHomeGitHub || l.SyncIssues {
		ghPulls, pullsErr = c.OpenPulls(ctx, l.GHRepo)
	}
	if l.Home == store.TetherHomeGitHub {
		heads := map[string]bool{}
		for _, p := range ghPulls {
			heads[p.Head.Ref] = true
		}
		a.tetherFetch(ctx, rp, &l, heads)
	}
	var problems []string
	if pullsErr != nil {
		problems = append(problems, tetherErrText(pullsErr))
	} else if l.Home == store.TetherHomeGitHub || l.SyncIssues {
		if err := a.tetherSyncPulls(ctx, c, acc, rp, &l, ghPulls); err != nil {
			problems = append(problems, tetherErrText(err))
		}
	}
	if l.SyncIssues {
		if err := a.tetherSyncIssues(ctx, c, rp, &l); err != nil {
			problems = append(problems, tetherErrText(err))
		}
	}
	if err := a.tetherSyncComments(ctx, c, acc, rp, &l); err != nil {
		problems = append(problems, tetherErrText(err))
	}
	now := time.Now()
	l.LastSyncAt, l.SyncError = &now, strings.Join(problems, " ")
	if err := a.db.SaveTetherLink(ctx, l); err != nil {
		log.Printf("tether: saving link for %s: %v", rp.FullName(), err)
	}
	if l.Home == store.TetherHomeGitHub {
		a.tetherReportStatusesLocked(ctx, c, rp, l)
	}
}

// tetherPush mirrors changed branches and tags to GitHub (Home: Atelier).
func (a *App) tetherPush(ctx context.Context, rp store.Repo, l *store.TetherLink) {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil || g.IsEmpty(ctx) {
		return
	}
	dir := a.git.Path(rp.Owner, rp.Name)
	now, err := a.tetherRefs(ctx, dir, "refs/heads", "refs/tags")
	if err != nil {
		return
	}
	last := parseRefList(l.PushedRefs)
	def := "refs/heads/" + g.DefaultBranch(ctx)
	var specs []string
	for ref, sha := range now {
		if last[ref] == sha {
			continue
		}
		if ref == def {
			specs = append(specs, ref+":"+ref) // never force the default branch
		} else {
			specs = append(specs, "+"+ref+":"+ref)
		}
	}
	for ref := range last {
		if _, ok := now[ref]; !ok {
			specs = append(specs, ":"+ref)
		}
	}
	if len(specs) == 0 {
		return
	}
	tok, err := a.db.TetherToken(ctx, a.secretSealer(), l.PersonID)
	if err != nil {
		l.PushError = errTetherNotConnected.Error()
		return
	}
	out, perr := a.tetherGit(ctx, dir, tok, "", append([]string{"push", "--porcelain", "--", a.tetherRemote(l.GHRepo)}, specs...)...)
	// Porcelain lines: "<flag>\t<from>:<to>\t<summary>".
	rejectedDefault := false
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 2 || len(parts[0]) != 1 {
			continue
		}
		_, to, _ := strings.Cut(parts[1], ":")
		switch parts[0] {
		case "-":
			delete(last, to)
		case "!":
			if to == def {
				rejectedDefault = true
			}
		default: // " ", "+", "*", "="
			if sha, ok := now[to]; ok {
				last[to] = sha
			}
		}
	}
	l.PushedRefs = formatRefList(last)
	t := time.Now()
	l.LastPushAt = &t
	switch {
	case rejectedDefault:
		l.PushError = ""
		l.Diverged = "GitHub's " + strings.TrimPrefix(def, "refs/heads/") + " has commits Atelier doesn't, so it wasn't overwritten."
	case perr != nil:
		l.PushError = perr.Error()
	default:
		l.PushError, l.Diverged = "", ""
	}
}

// tetherFetch brings GitHub's branches into Atelier (Home: GitHub). Branches
// that are the head of an open GitHub pull request aren't queued as pushes:
// the pull request sync starts their checks (so fork code waits for approval).
func (a *App) tetherFetch(ctx context.Context, rp store.Repo, l *store.TetherLink, prHeads map[string]bool) {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return
	}
	dir := a.git.Path(rp.Owner, rp.Name)
	tok, err := a.db.TetherToken(ctx, a.secretSealer(), l.PersonID)
	if err != nil {
		l.FetchError = errTetherNotConnected.Error()
		return
	}
	remote := a.tetherRemote(l.GHRepo)
	_, ferr := a.tetherGit(ctx, dir, tok, "", "fetch", "--prune", "--no-tags", "--", remote,
		"+refs/heads/*:refs/tether/heads/*", "+refs/tags/*:refs/tags/*")
	t := time.Now()
	l.LastFetchAt = &t
	if ferr != nil {
		l.FetchError = ferr.Error()
		return
	}
	l.FetchError = ""
	theirs, err := a.tetherRefs(ctx, dir, "refs/tether/heads")
	if err != nil {
		return
	}
	mine, err := a.tetherRefs(ctx, dir, "refs/heads")
	if err != nil {
		return
	}
	wasEmpty := len(mine) == 0
	def := g.DefaultBranch(ctx)
	if wasEmpty {
		if gh, err := a.tetherDefaultBranch(ctx, l); err == nil && gh != "" && gitrepo.ValidBranchName(gh) {
			_, _ = a.tetherGit(ctx, dir, "", "", "symbolic-ref", "HEAD", "refs/heads/"+gh)
			def = gh
		}
	}
	lastSeen := parseRefList(l.PushedRefs)
	seen := map[string]string{}
	l.Diverged = ""
	for tref, sha := range theirs {
		branch := strings.TrimPrefix(tref, "refs/tether/heads/")
		if !gitrepo.ValidBranchName(branch) || strings.HasPrefix(branch, "tether/") {
			continue
		}
		ref := "refs/heads/" + branch
		seen[ref] = sha
		cur := mine[ref]
		switch {
		case cur == sha:
			continue
		case cur == "" || a.tetherIsAncestor(ctx, dir, cur, sha):
			if err := a.tetherUpdateRef(ctx, dir, ref, sha, cur); err != nil {
				continue
			}
			if !prHeads[branch] {
				a.tetherQueuePush(ctx, rp, ref, cur, sha)
			}
		case branch == def:
			l.Diverged = a.tetherDivergence(ctx, rp, def, cur, sha)
		case a.tetherIsAncestor(ctx, dir, sha, cur):
			// Atelier has more on a branch: offer it to GitHub (never forced).
			_, _ = a.tetherGit(ctx, dir, tok, "", "push", "--porcelain", "--", remote, ref+":"+ref)
		}
	}
	// Branches deleted on GitHub go here too, unless Atelier moved them since.
	for ref, sha := range lastSeen {
		if _, still := seen[ref]; still || ref == "refs/heads/"+def {
			continue
		}
		if mine[ref] == sha {
			_ = a.tetherUpdateRefDelete(ctx, dir, ref, sha)
		}
	}
	// Atelier-only branches are offered to GitHub so they can be merged there.
	for ref := range mine {
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if _, onGitHub := seen[ref]; onGitHub || strings.HasPrefix(branch, "tether/") || lastSeen[ref] != "" || branch == def {
			continue
		}
		_, _ = a.tetherGit(ctx, dir, tok, "", "push", "--porcelain", "--", remote, ref+":"+ref)
	}
	l.PushedRefs = formatRefList(seen)
	_ = a.db.TouchRepo(ctx, rp.ID)
}

func (a *App) tetherUpdateRefDelete(ctx context.Context, dir, ref, old string) error {
	_, err := a.tetherGit(ctx, dir, "", "", "update-ref", "-d", ref, old)
	return err
}

// tetherDefaultBranch asks GitHub for the twin's default branch.
func (a *App) tetherDefaultBranch(ctx context.Context, l *store.TetherLink) (string, error) {
	c, _, err := a.tetherClient(ctx, l.PersonID)
	if err != nil {
		return "", err
	}
	rp, err := c.Repo(ctx, l.GHRepo)
	return rp.DefaultBranch, err
}
