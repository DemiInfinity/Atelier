package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atelier/internal/importer"
	"atelier/internal/store"
	"atelier/internal/tether"
)

// Issues, pull requests and comments between a linked repository and GitHub.
//
// In: new GitHub issues (when "Sync issues" is on) and open pull requests
// (Home: GitHub, or with issue sync) become Atelier items, keeping GitHub's
// number when it's free. Authors are stored like the importer does (author
// kind "import", name "github:<login>"). New comments on any linked item come
// in as comments and announce themselves with a tether.comment event.
//
// Out: comments written in Atelier on a linked item are posted to GitHub.
//
// tether_items maps numbers both ways, tether_comments remembers which
// comments exist on both sides, so nothing is copied twice.

const tetherAuthorPrefix = "github:"

func tetherAuthor(login string) string { return tetherAuthorPrefix + login }

// tetherNumberFor picks the Atelier number for a GitHub item: the same one if free.
func (a *App) tetherNumberFor(ctx context.Context, repoID, ghNumber int64) (int64, error) {
	used, _, _, err := a.db.NumberOwner(ctx, repoID, ghNumber)
	if err != nil {
		return 0, err
	}
	if used == "" {
		if err := a.db.BumpRepoCounter(ctx, repoID, ghNumber+1); err != nil {
			return 0, err
		}
		return ghNumber, nil
	}
	return a.db.NextNumber(ctx, repoID)
}

// tetherFindItem finds the Atelier side of a GitHub number, adopting items
// the importer brought in earlier.
func (a *App) tetherFindItem(ctx context.Context, rp store.Repo, l store.TetherLink, ghNumber int64, kind string) (store.TetherItem, bool) {
	if it, err := a.db.TetherItemByGH(ctx, rp.ID, ghNumber); err == nil {
		return it, true
	}
	for _, k := range []string{kind, "issue", "pull"} {
		if k == "" {
			continue
		}
		link, err := a.db.ImportLinkFor(ctx, rp.ID, k, strconv.FormatInt(ghNumber, 10))
		if err != nil || link.Number == 0 {
			continue
		}
		if !strings.Contains(strings.ToLower(link.SourceURL), strings.ToLower(l.GHRepo)) {
			continue
		}
		it := store.TetherItem{RepoID: rp.ID, Kind: k, Number: link.Number, GHNumber: ghNumber, GHURL: link.SourceURL, Origin: "github", GHState: "open"}
		if err := a.db.SaveTetherItem(ctx, it); err == nil {
			return it, true
		}
	}
	return store.TetherItem{}, false
}

// ---------- issues ----------

func (a *App) tetherSyncIssues(ctx context.Context, run *tetherIssueRun, l *store.TetherLink) error {
	since := l.IssuesSince
	if !since.IsZero() {
		since = since.Add(-time.Second)
	}
	issues, err := run.c.IssuesSince(ctx, l.GHRepo, since)
	for _, is := range issues {
		if !is.IsPull() { // pull requests are handled from the pulls list
			if serr := a.tetherSyncIssue(ctx, run, is); serr != nil {
				return serr // the cursor stays: the rest is tried again next time
			}
		}
		if is.UpdatedAt.After(l.IssuesSince) {
			l.IssuesSince = is.UpdatedAt
		}
	}
	return err
}

// tetherSyncIssue brings one GitHub issue in: a new one is created (with its
// labels and milestone), a known one is compared with Atelier's side.
// Problems writing to GitHub don't stop the inbound pass (they're reported).
func (a *App) tetherSyncIssue(ctx context.Context, run *tetherIssueRun, is tether.Issue) error {
	rp, l := run.rp, run.l
	it, ok := a.tetherFindItem(ctx, rp, l, is.Number, "issue")
	if ok {
		if it.Kind != "issue" {
			return nil
		}
		if it.GHState != is.State {
			it.GHState = is.State
			if err := a.db.SaveTetherItem(ctx, it); err != nil {
				return err
			}
		}
		if err := a.tetherReconcileIssue(ctx, run, it, &is); err != nil {
			run.problem(fmt.Errorf("issue #%d: %w", it.Number, err))
		}
		return nil
	}
	n, err := a.tetherNumberFor(ctx, rp.ID, is.Number)
	if err != nil {
		return err
	}
	in := store.ImportedIssue{RepoID: rp.ID, Number: n, Title: tetherTitle(is.Title), Body: tetherBody(is.Body), State: is.State,
		StateReason: is.StateReason, AuthorKind: importer.AuthorImport, AuthorName: tetherAuthor(is.User.Login),
		CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt, ClosedAt: is.ClosedAt}
	if in.StateReason != "completed" && in.StateReason != "not_planned" && in.StateReason != "reopened" {
		in.StateReason = ""
	}
	for i := range is.Labels {
		if !tetherLabelOK(is.Labels[i].Name) {
			continue
		}
		if lb, ok := run.localLabel(ctx, tetherKey(is.Labels[i].Name), &is.Labels[i]); ok {
			in.LabelIDs = append(in.LabelIDs, lb.ID)
		}
	}
	if is.Milestone != nil {
		if m, ok := run.localMilestone(ctx, *is.Milestone); ok {
			in.MilestoneID = m.ID
		}
	}
	local, err := a.db.ImportIssue(ctx, in)
	if err != nil {
		return fmt.Errorf("issue #%d: %w", is.Number, err)
	}
	if err := a.db.SaveTetherItem(ctx, store.TetherItem{RepoID: rp.ID, Kind: "issue", Number: local.Number, GHNumber: is.Number,
		GHURL: a.tetherHTMLURL(is.HTMLURL, fmt.Sprintf("%s/issues/%d", l.GHRepo, is.Number)), Origin: "github", GHState: is.State}); err != nil {
		return err
	}
	run.done[local.Number] = true
	// What both sides now agree on: the issue as it arrived.
	if err := a.tetherSaveSnapshot(ctx, rp.ID, local.Number, tetherLocalSide(local), is.UpdatedAt, local.UpdatedAt); err != nil {
		return err
	}
	if is.State == "open" {
		a.emit(ctx, Event{Kind: EventIssueOpened, Repo: rp, RepoID: rp.ID, Number: local.Number, Title: local.Title,
			URL: fmt.Sprintf("/%s/issues/%d", rp.FullName(), local.Number), Actor: Actor{Kind: "bot", Name: tetherAuthor(is.User.Login)},
			Detail: eventExcerpt(is.Body, 140)})
	}
	return nil
}

// ---------- pull requests ----------

// tetherSyncPulls adopts open GitHub pull requests, follows their heads and
// notices when they're closed or merged on GitHub.
func (a *App) tetherSyncPulls(ctx context.Context, c *tether.Client, acc store.TetherAccount, rp store.Repo, l *store.TetherLink, prs []tether.Pull) error {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return err
	}
	dir := a.git.Path(rp.Owner, rp.Name)
	tok, err := a.tetherLinkToken(ctx, l.PersonID, l.GHRepo)
	if err != nil {
		return errTetherNotConnected
	}
	// Bring every open head in as refs/pull/N/head, like GitHub has it.
	var specs []string
	open := map[int64]bool{}
	for _, p := range prs {
		open[p.Number] = true
		if it, err := a.db.TetherItemByGH(ctx, rp.ID, p.Number); err == nil && it.Origin == "atelier" {
			continue // mirrored out from Atelier: Atelier already has it
		}
		specs = append(specs, fmt.Sprintf("+refs/pull/%d/head:refs/pull/%d/head", p.Number, p.Number))
	}
	if len(specs) > 0 {
		if _, err := a.tetherGit(ctx, dir, tok, "", append([]string{"fetch", "--no-tags", "--", a.tetherRemote(l.GHRepo)}, specs...)...); err != nil {
			return err
		}
	}
	for _, p := range prs {
		it, found := a.tetherFindItem(ctx, rp, *l, p.Number, "pull")
		if found && it.Origin == "atelier" {
			continue
		}
		head, err := a.tetherGit(ctx, dir, "", "", "rev-parse", "--verify", "--quiet", fmt.Sprintf("refs/pull/%d/head^{commit}", p.Number))
		head = strings.TrimSpace(head)
		if err != nil || head == "" {
			continue
		}
		sameRepo := p.Head.Repo != nil && strings.EqualFold(p.Head.Repo.FullName, l.GHRepo)
		branch := fmt.Sprintf("tether/pr-%d", p.Number)
		if sameRepo {
			if sha, err := g.BranchSHA(ctx, p.Head.Ref); err == nil && sha == head {
				branch = p.Head.Ref
			}
		}
		if branch != p.Head.Ref || !sameRepo {
			cur, _ := g.BranchSHA(ctx, branch)
			if cur != head {
				if err := a.tetherUpdateRef(ctx, dir, "refs/heads/"+branch, head, cur); err != nil {
					continue
				}
			}
		}
		author := Actor{Kind: "bot", Name: tetherAuthor(p.User.Login)}
		authorKind, authorName := importer.AuthorImport, tetherAuthor(p.User.Login)
		if sameRepo && a.tetherIsMe(ctx, l.PersonID, acc, p.User.Login) {
			// Your own pull request from your own repository: no approval gate.
			if me, err := a.db.Person(ctx, l.PersonID); err == nil {
				authorKind, authorName = ActorOwner, me.Name
				author = Actor{Kind: ActorOwner, Name: me.Name, PersonID: me.ID}
			}
		}
		if !found {
			n, err := a.tetherNumberFor(ctx, rp.ID, p.Number)
			if err != nil {
				return err
			}
			pr, err := a.db.ImportPull(ctx, store.ImportedPull{RepoID: rp.ID, Number: n, Title: eventExcerpt(p.Title, 256), Body: p.Body,
				Head: branch, Base: p.Base.Ref, HeadSHA: head, State: "open", AuthorKind: authorKind, AuthorName: authorName,
				CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt})
			if err != nil {
				return fmt.Errorf("pull request #%d: %w", p.Number, err)
			}
			if err := a.db.SaveTetherItem(ctx, store.TetherItem{RepoID: rp.ID, Kind: "pull", Number: pr.Number, GHNumber: p.Number,
				GHURL: a.tetherHTMLURL(p.HTMLURL, fmt.Sprintf("%s/pull/%d", l.GHRepo, p.Number)), Origin: "github",
				HeadSHA: p.Head.SHA, GHState: "open"}); err != nil {
				return err
			}
			if l.Home == store.TetherHomeGitHub {
				// Starts pull_request workflows (and the approval gate for outsiders).
				a.emit(ctx, Event{Kind: EventPullOpened, Repo: rp, RepoID: rp.ID, Number: pr.Number, Title: pr.Title,
					URL: fmt.Sprintf("/%s/pulls/%d", rp.FullName(), pr.Number), Actor: author, Detail: eventExcerpt(p.Body, 140)})
			}
			continue
		}
		if it.HeadSHA != p.Head.SHA {
			pr, err := a.db.PullByNumber(ctx, rp.ID, it.Number)
			if err != nil {
				continue
			}
			_ = a.db.SetPullHeadSHA(ctx, pr.ID, head)
			it.HeadSHA = p.Head.SHA
			if err := a.db.SaveTetherItem(ctx, it); err != nil {
				return err
			}
			if l.Home == store.TetherHomeGitHub && pr.IsOpen() {
				pr.HeadSHA = head
				st := a.actions()
				bctx := context.WithoutCancel(ctx)
				st.go_(func() { a.triggerPull(bctx, rp, g, pr, "synchronize", author) })
			}
		}
	}
	// Pull requests that were open on GitHub and aren't any more.
	items, err := a.db.TetherItems(ctx, rp.ID, "pull")
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.Origin != "github" || it.GHState != "open" || open[it.GHNumber] {
			continue
		}
		gp, err := c.Pull(ctx, l.GHRepo, it.GHNumber)
		if err != nil {
			return err
		}
		if gp.State == "open" {
			continue
		}
		a.tetherPullClosedOnGitHub(ctx, rp, it, gp)
	}
	return nil
}

// tetherPullClosedOnGitHub records a GitHub merge or close in Atelier.
func (a *App) tetherPullClosedOnGitHub(ctx context.Context, rp store.Repo, it store.TetherItem, gp tether.Pull) {
	pr, err := a.db.PullByNumber(ctx, rp.ID, it.Number)
	if err != nil {
		return
	}
	if pr.IsOpen() {
		if gp.Merged {
			base := ""
			if g, err := a.git.Open(rp.Owner, rp.Name); err == nil {
				base, _ = g.BranchSHA(ctx, pr.Base)
			}
			if err := a.db.MarkPullMerged(ctx, pr.ID, "merge", gp.MergeCommitSHA, base, pr.HeadSHA, "github"); err == nil {
				_ = a.db.AddPullEvent(ctx, pr.ID, "merged", importer.AuthorImport, "github", "merge "+gp.MergeCommitSHA)
			}
		} else if err := a.db.ClosePull(ctx, pr.ID); err == nil {
			_ = a.db.AddPullEvent(ctx, pr.ID, "closed", importer.AuthorImport, "github", "")
		}
	}
	it.GHState = "closed"
	if gp.Merged {
		it.GHState = "merged"
	}
	_ = a.db.SaveTetherItem(ctx, it)
}

// ---------- comments ----------

// tetherSyncComments brings in new GitHub comments on linked items: the
// conversation and, for pull requests, review comments on lines (bots
// post most of theirs there).
func (a *App) tetherSyncComments(ctx context.Context, c *tether.Client, acc store.TetherAccount, rp store.Repo, l *store.TetherLink) error {
	since := l.CommentsSince
	if !since.IsZero() {
		since = since.Add(-time.Second)
	}
	comments, err := c.CommentsSince(ctx, l.GHRepo, since)
	newest := l.CommentsSince
	for _, cm := range comments {
		if err := a.tetherCommentIn(ctx, rp, *l, cm, cm.IssueNumber(), ""); err != nil {
			return err
		}
		if cm.UpdatedAt.After(newest) {
			newest = cm.UpdatedAt
		}
	}
	if err != nil {
		l.CommentsSince = newest
		return err
	}
	reviews, rerr := c.ReviewCommentsSince(ctx, l.GHRepo, since)
	for _, rc := range reviews {
		where := ""
		if rc.Path != "" {
			where = "`" + strings.ReplaceAll(rc.Path, "`", "'") + "`"
			if rc.Line > 0 {
				where += fmt.Sprintf(" line %d", rc.Line)
			}
		}
		if err := a.tetherCommentIn(ctx, rp, *l, rc.Comment, rc.PullNumber(), where); err != nil {
			return err
		}
		if rc.UpdatedAt.After(newest) {
			newest = rc.UpdatedAt
		}
	}
	l.CommentsSince = newest
	return rerr
}

// tetherCommentIn copies one GitHub comment onto its Atelier item.
func (a *App) tetherCommentIn(ctx context.Context, rp store.Repo, l store.TetherLink, cm tether.Comment, ghNumber int64, where string) error {
	if cm.ID == 0 || ghNumber == 0 || strings.TrimSpace(cm.Body) == "" {
		return nil
	}
	if known, err := a.db.TetherCommentKnown(ctx, rp.ID, cm.ID); err != nil || known {
		return err
	}
	it, ok := a.tetherFindItem(ctx, rp, l, ghNumber, "")
	if !ok {
		return nil // not an item Atelier has
	}
	body := cm.Body
	if where != "" {
		body = "On " + where + ":\n\n" + body
	}
	author := tetherAuthor(cm.User.Login)
	var id int64
	var title, url string
	switch it.Kind {
	case "issue":
		is, err := a.db.IssueByNumber(ctx, rp.ID, it.Number)
		if err != nil {
			return nil
		}
		ic, err := a.db.ImportIssueComment(ctx, is.ID, body, importer.AuthorImport, author, cm.CreatedAt, cm.UpdatedAt)
		if err != nil {
			return err
		}
		id, title, url = ic.ID, is.Title, fmt.Sprintf("/%s/issues/%d#comment-%d", rp.FullName(), is.Number, ic.ID)
	default:
		pr, err := a.db.PullByNumber(ctx, rp.ID, it.Number)
		if err != nil {
			return nil
		}
		pc, err := a.db.ImportPullComment(ctx, pr.ID, importer.AuthorImport, author, body, cm.CreatedAt, cm.UpdatedAt)
		if err != nil {
			return err
		}
		id, title, url = pc.ID, pr.Title, fmt.Sprintf("/%s/pulls/%d#comment-%d", rp.FullName(), pr.Number, pc.ID)
	}
	if err := a.db.MarkTetherComment(ctx, rp.ID, cm.ID, it.Kind+"_comment", id, "in"); err != nil {
		return err
	}
	a.emit(ctx, Event{Kind: EventTetherComment, Repo: rp, RepoID: rp.ID, Number: it.Number, Title: title, URL: url,
		Actor: Actor{Kind: "bot", Name: author}, Detail: eventExcerpt(cm.Body, 140)})
	return nil
}

// tetherCommentOut posts a comment written in Atelier to the linked GitHub item.
func (a *App) tetherCommentOut(ctx context.Context, ev Event) {
	if ev.Actor.Kind == importer.AuthorImport || strings.HasPrefix(ev.Actor.Name, tetherAuthorPrefix) {
		return
	}
	_, frag, ok := strings.Cut(ev.URL, "#comment-")
	if !ok {
		return
	}
	commentID, err := strconv.ParseInt(frag, 10, 64)
	if err != nil {
		return
	}
	l, err := a.db.TetherLink(ctx, ev.RepoID)
	if err != nil {
		return
	}
	kind := "issue"
	if ev.Kind == EventPullCommented {
		kind = "pull"
	}
	rp := ev.Repo
	if rp.ID != ev.RepoID {
		if rp, err = a.repoByID(ctx, ev.RepoID); err != nil {
			return
		}
	}
	bctx := context.WithoutCancel(ctx)
	a.tether().goBG("", func() {
		unlock := a.tether().lockRepo(rp.ID)
		defer unlock()
		if err := a.tetherPostComment(bctx, rp, l, kind, ev.Number, commentID, ev.Actor); err != nil {
			log.Printf("tether: posting comment to %s: %s", l.GHRepo, tetherErrText(err))
		}
	})
}

func (a *App) tetherPostComment(ctx context.Context, rp store.Repo, l store.TetherLink, kind string, number, commentID int64, who Actor) error {
	it, err := a.db.TetherItem(ctx, rp.ID, number)
	if errors.Is(err, store.ErrNotFound) {
		// Imported earlier by the importer (same numbers)?
		var ok bool
		if it, ok = a.tetherFindItem(ctx, rp, l, number, kind); !ok || it.Number != number {
			return nil
		}
	} else if err != nil {
		return err
	}
	if gh, err := a.db.TetherCommentOf(ctx, kind+"_comment", commentID); err != nil || gh != 0 {
		return err
	}
	var body string
	if kind == "issue" {
		ic, err := a.db.IssueCommentByID(ctx, commentID)
		if err != nil {
			return err
		}
		body = ic.Body
	} else {
		pr, err := a.db.PullByNumber(ctx, rp.ID, number)
		if err != nil {
			return err
		}
		cs, err := a.db.PullComments(ctx, pr.ID)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.ID == commentID {
				body = c.Body
			}
		}
	}
	if strings.TrimSpace(body) == "" {
		return nil
	}
	if who.Kind != ActorOwner {
		body += "\n\n_(" + who.Name + " via Atelier)_"
	}
	c, _, err := a.tetherLinkClient(ctx, l.PersonID, l.GHRepo)
	if err != nil {
		return err
	}
	gc, err := c.CreateComment(ctx, l.GHRepo, it.GHNumber, body)
	if err != nil {
		return err
	}
	return a.db.MarkTetherComment(ctx, rp.ID, gc.ID, kind+"_comment", commentID, "out")
}

// ---------- Merge on GitHub ----------

// tetherMergePathHandler answers POST /{owner}/{repo}/pulls/{n}/merge for
// repositories whose home is GitHub: the merge happens on GitHub, then
// Atelier fetches the result. Merging inside Atelier isn't possible there.
func tetherMergePathHandler(a *App, w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/merge") {
		return false
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 5 || parts[2] != "pulls" || parts[4] != "merge" {
		return false
	}
	ctx := r.Context()
	rp, err := a.db.RepoByName(ctx, parts[0], parts[1])
	if err != nil {
		return false
	}
	l, err := a.db.TetherLink(ctx, rp.ID)
	if err != nil || l.Home != store.TetherHomeGitHub {
		return false
	}
	if !a.sameOrigin(r) {
		http.Error(w, "cross-site request blocked", http.StatusForbidden)
		return true
	}
	who, ok := a.sessionActor(r)
	if !ok {
		return false // the normal route sends them to sign in
	}
	r = withActor(r, who)
	r.SetPathValue("owner", parts[0])
	r.SetPathValue("repo", parts[1])
	r.SetPathValue("number", parts[3])
	v, g, p, ok := a.loadPull(w, r)
	if !ok {
		return true
	}
	fail := func(msg string, code int) { a.renderPull(w, r, v, g, p, "overview", msg, code) }
	if !p.IsOpen() {
		fail("This pull request isn't open.", http.StatusConflict)
		return true
	}
	method := r.FormValue("method")
	if method != "squash" && method != "merge" && method != "rebase" {
		method = "merge"
	}
	sha, err := a.tetherMergeOnGitHub(ctx, rp, p, method, who)
	if err != nil {
		fail("Couldn't merge on GitHub: "+tetherErrText(err), http.StatusBadGateway)
		return true
	}
	http.Redirect(w, r, fmt.Sprintf("/%s/pulls/%d?merged=%s", rp.FullName(), p.Number, shortSHA(sha)), http.StatusSeeOther)
	return true
}

// tetherMergeOnGitHub merges p on GitHub (mirroring it there first if it's
// Atelier's own), then brings the result back.
func (a *App) tetherMergeOnGitHub(ctx context.Context, rp store.Repo, p store.Pull, method string, who Actor) (string, error) {
	it, err := a.db.TetherItem(ctx, rp.ID, p.Number)
	if errors.Is(err, store.ErrNotFound) {
		if _, _, err := a.tetherMirrorPull(ctx, rp.ID, p.Number); err != nil {
			return "", err
		}
		it, err = a.db.TetherItem(ctx, rp.ID, p.Number)
	}
	if err != nil {
		return "", err
	}
	unlock := a.tether().lockRepo(rp.ID)
	l, err := a.db.TetherLink(ctx, rp.ID)
	if err != nil {
		unlock()
		return "", err
	}
	c, acc, err := a.tetherLinkClient(ctx, l.PersonID, l.GHRepo)
	if err != nil {
		unlock()
		return "", err
	}
	title := fmt.Sprintf("%s (#%d)", p.Title, it.GHNumber)
	sha, err := c.MergePull(ctx, l.GHRepo, it.GHNumber, method, it.HeadSHA, title)
	if err != nil {
		unlock()
		return "", err
	}
	base := ""
	if g, err := a.git.Open(rp.Owner, rp.Name); err == nil {
		base, _ = g.BranchSHA(ctx, p.Base)
	}
	// Fetch the merge (GitHub's push to the base branch becomes an Actions push).
	a.tetherFetch(ctx, rp, &l, nil)
	_ = a.db.SaveTetherLink(ctx, l)
	unlock()
	if err := a.db.MarkPullMerged(ctx, p.ID, method, sha, base, p.HeadSHA, who.Name); err != nil {
		return sha, err
	}
	_ = a.db.AddPullEvent(ctx, p.ID, "merged", who.Kind, who.Name, method+" "+sha+" on GitHub")
	it.GHState = "merged"
	_ = a.db.SaveTetherItem(ctx, it)
	_ = acc
	a.closeReferencedIssues(ctx, rp.ID, p, who)
	return sha, nil
}
