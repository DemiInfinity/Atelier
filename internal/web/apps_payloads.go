package web

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// GitHub-compatible JSON for webhooks and /api/v3: the common keys use
// GitHub's names and shapes so existing receivers and bots work unchanged.
// Atelier-only details go under an "atelier" key.

func init() {
	registerEventHandler(appsOnEvent)
}

// appsBase is the absolute server URL for links in payloads.
func (a *App) appsBase() string {
	if b := strings.TrimRight(a.actionsBaseURL(), "/"); b != "" {
		return b
	}
	return "http://localhost"
}

func ghTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func ghTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return ghTime(*t)
}

// ghUser is a GitHub user object for an actor kind and name.
func (a *App) ghUser(ctx context.Context, kind, name string) map[string]any {
	base := a.appsBase()
	login, typ, id := name, "User", int64(0)
	switch kind {
	case "bot", "agent":
		typ = "Bot"
		if !strings.HasSuffix(login, "[bot]") {
			login += "[bot]"
		}
		if b, err := a.db.BotByName(ctx, name); err == nil {
			id = b.ID
		}
	case "tether":
		typ = "Bot"
	case ActorJob:
		typ, login = "Bot", "atelier-actions[bot]"
	case ActorOwner, ActorToken, "", "import":
		if o, err := a.db.PersonalOwner(ctx); err == nil {
			if kind != "import" || name == "" {
				login = o.Name
			}
			if login == o.Name {
				id = o.ID
			}
		}
	case ActorSystem:
		typ, login = "Bot", "atelier[bot]"
	}
	if login == "" {
		login = "ghost"
	}
	return map[string]any{
		"login": login, "id": id, "node_id": "", "type": typ, "site_admin": false,
		"avatar_url": "", "url": base + "/api/v3/users/" + login, "html_url": base + "/" + login,
	}
}

func (a *App) ghOwner(o string, kind string) map[string]any {
	typ := "User"
	if kind == "company" {
		typ = "Organization"
	}
	var id int64
	if ow, err := a.db.OwnerByName(context.Background(), o); err == nil {
		id = ow.ID
	}
	base := a.appsBase()
	return map[string]any{"login": o, "id": id, "node_id": "", "type": typ, "site_admin": false, "avatar_url": "",
		"url": base + "/api/v3/users/" + o, "html_url": base + "/" + o}
}

// ghRepo is a GitHub repository object.
func (a *App) ghRepo(ctx context.Context, rp store.Repo) map[string]any {
	base := a.appsBase()
	def := "main"
	if g, err := a.git.Open(rp.Owner, rp.Name); err == nil && !g.IsEmpty(ctx) {
		def = g.DefaultBranch(ctx)
	}
	vis := "private"
	if !rp.Private {
		vis = "public"
	}
	api := base + "/api/v3/repos/" + rp.FullName()
	return map[string]any{
		"id": rp.ID, "node_id": "", "name": rp.Name, "full_name": rp.FullName(), "private": rp.Private, "visibility": vis,
		"owner": a.ghOwner(rp.Owner, rp.OwnerKind), "html_url": base + "/" + rp.FullName(), "description": nilIfEmpty(rp.Description),
		"fork": false, "url": api, "clone_url": base + "/" + rp.FullName() + ".git", "git_url": base + "/" + rp.FullName() + ".git",
		"default_branch": def, "created_at": ghTime(rp.CreatedAt), "updated_at": ghTime(rp.UpdatedAt), "pushed_at": ghTime(rp.UpdatedAt),
		"archived": false, "disabled": false, "has_issues": true, "has_projects": false, "has_wiki": false,
		"issues_url": api + "/issues{/number}", "pulls_url": api + "/pulls{/number}", "statuses_url": api + "/statuses/{sha}",
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func ghLabels(ls []store.Label) []map[string]any {
	out := []map[string]any{}
	for _, l := range ls {
		out = append(out, map[string]any{"id": l.ID, "node_id": "", "name": l.Name, "color": l.Color,
			"description": nilIfEmpty(l.Description), "default": false})
	}
	return out
}

func (a *App) ghIssue(ctx context.Context, rp store.Repo, is store.Issue) map[string]any {
	base := a.appsBase()
	api := fmt.Sprintf("%s/api/v3/repos/%s/issues/%d", base, rp.FullName(), is.Number)
	m := map[string]any{
		"id": is.ID, "node_id": "", "number": is.Number, "title": is.Title, "body": nilIfEmpty(is.Body), "state": is.State,
		"state_reason": nilIfEmpty(is.StateReason), "locked": false, "comments": is.Comments,
		"user": a.ghUser(ctx, is.AuthorKind, is.AuthorName), "labels": ghLabels(is.Labels), "assignees": []any{}, "assignee": nil,
		"milestone": nil, "created_at": ghTime(is.CreatedAt), "updated_at": ghTime(is.UpdatedAt), "closed_at": ghTimePtr(is.ClosedAt),
		"author_association": "OWNER", "url": api, "comments_url": api + "/comments", "labels_url": api + "/labels{/name}",
		"html_url":       fmt.Sprintf("%s/%s/issues/%d", base, rp.FullName(), is.Number),
		"repository_url": base + "/api/v3/repos/" + rp.FullName(),
	}
	if is.MilestoneID != 0 {
		m["milestone"] = map[string]any{"id": is.MilestoneID, "title": is.Milestone}
	}
	return m
}

// ghPullAsIssue is a pull request in issue form (issue_comment on a PR).
func (a *App) ghPullAsIssue(ctx context.Context, rp store.Repo, p store.Pull) map[string]any {
	base := a.appsBase()
	api := fmt.Sprintf("%s/api/v3/repos/%s/issues/%d", base, rp.FullName(), p.Number)
	state := "open"
	if !p.IsOpen() {
		state = "closed"
	}
	return map[string]any{
		"id": p.ID, "node_id": "", "number": p.Number, "title": p.Title, "body": nilIfEmpty(p.Body), "state": state, "locked": false,
		"user": a.ghUser(ctx, p.AuthorKind, p.AuthorName), "labels": []any{}, "assignees": []any{}, "comments": 0,
		"created_at": ghTime(p.CreatedAt), "updated_at": ghTime(p.UpdatedAt), "closed_at": ghTimePtr(appsPullClosedAt(p)),
		"author_association": "OWNER", "url": api, "comments_url": api + "/comments",
		"html_url":       fmt.Sprintf("%s/%s/pull/%d", base, rp.FullName(), p.Number),
		"repository_url": base + "/api/v3/repos/" + rp.FullName(),
		"pull_request": map[string]any{
			"url":      fmt.Sprintf("%s/api/v3/repos/%s/pulls/%d", base, rp.FullName(), p.Number),
			"html_url": fmt.Sprintf("%s/%s/pulls/%d", base, rp.FullName(), p.Number),
			"diff_url": fmt.Sprintf("%s/%s/pulls/%d.diff", base, rp.FullName(), p.Number), "merged_at": ghTimePtr(p.MergedAt),
		},
	}
}

func appsPullClosedAt(p store.Pull) *time.Time {
	if p.MergedAt != nil {
		return p.MergedAt
	}
	return p.ClosedAt
}

// appsPullSHAs returns the head and base commits a pull request shows now.
func (a *App) appsPullSHAs(ctx context.Context, rp store.Repo, p store.Pull) (head, base string) {
	head, base = p.HeadSHA, p.BaseSHA
	if p.IsMerged() {
		return
	}
	if g, err := a.git.Open(rp.Owner, rp.Name); err == nil {
		if s, err := g.BranchSHA(ctx, p.Head); err == nil {
			head = s
		}
		if s, err := g.BranchSHA(ctx, p.Base); err == nil {
			base = s
		}
	}
	return
}

func (a *App) ghPull(ctx context.Context, rp store.Repo, p store.Pull) map[string]any {
	base := a.appsBase()
	api := fmt.Sprintf("%s/api/v3/repos/%s/pulls/%d", base, rp.FullName(), p.Number)
	html := fmt.Sprintf("%s/%s/pulls/%d", base, rp.FullName(), p.Number)
	head, baseSHA := a.appsPullSHAs(ctx, rp, p)
	repo := a.ghRepo(ctx, rp)
	owner := a.ghOwner(rp.Owner, rp.OwnerKind)
	state := "open"
	if !p.IsOpen() {
		state = "closed"
	}
	var mergedBy any
	if p.IsMerged() {
		mergedBy = a.ghUser(ctx, ActorOwner, p.MergedBy)
	}
	labels, _ := a.db.PullLabels(ctx, p.ID)
	return map[string]any{
		"id": p.ID, "node_id": "", "number": p.Number, "state": state, "locked": false, "title": p.Title, "body": nilIfEmpty(p.Body),
		"user": a.ghUser(ctx, p.AuthorKind, p.AuthorName), "draft": false, "merged": p.IsMerged(), "mergeable": nil,
		"merge_commit_sha": nilIfEmpty(p.MergedSHA), "merged_at": ghTimePtr(p.MergedAt), "merged_by": mergedBy,
		"created_at": ghTime(p.CreatedAt), "updated_at": ghTime(p.UpdatedAt), "closed_at": ghTimePtr(appsPullClosedAt(p)),
		"labels": ghLabels(labels), "assignees": []any{}, "requested_reviewers": []any{}, "author_association": "OWNER",
		"head": map[string]any{"label": rp.Owner + ":" + p.Head, "ref": p.Head, "sha": head, "user": owner, "repo": repo},
		"base": map[string]any{"label": rp.Owner + ":" + p.Base, "ref": p.Base, "sha": baseSHA, "user": owner, "repo": repo},
		"url":  api, "html_url": html, "diff_url": html + ".diff", "patch_url": html + ".patch",
		"issue_url":           fmt.Sprintf("%s/api/v3/repos/%s/issues/%d", base, rp.FullName(), p.Number),
		"comments_url":        fmt.Sprintf("%s/api/v3/repos/%s/issues/%d/comments", base, rp.FullName(), p.Number),
		"review_comments_url": api + "/comments", "commits_url": api + "/commits",
		"statuses_url": fmt.Sprintf("%s/api/v3/repos/%s/statuses/%s", base, rp.FullName(), head),
	}
}

func (a *App) ghComment(ctx context.Context, rp store.Repo, number, id int64, kind, name, body string, created, updated time.Time, isPull bool) map[string]any {
	base := a.appsBase()
	seg := "issues"
	if isPull {
		seg = "pulls"
	}
	return map[string]any{
		"id": id, "node_id": "", "body": body, "user": a.ghUser(ctx, kind, name), "author_association": "OWNER",
		"created_at": ghTime(created), "updated_at": ghTime(updated),
		"url":       fmt.Sprintf("%s/api/v3/repos/%s/issues/comments/%d", base, rp.FullName(), id),
		"html_url":  fmt.Sprintf("%s/%s/%s/%d#comment-%d", base, rp.FullName(), seg, number, id),
		"issue_url": fmt.Sprintf("%s/api/v3/repos/%s/issues/%d", base, rp.FullName(), number),
	}
}

func (a *App) ghReviewComment(ctx context.Context, rp store.Repo, p store.Pull, n store.PullNote) map[string]any {
	base := a.appsBase()
	side := "RIGHT"
	if n.Side == "old" {
		side = "LEFT"
	}
	return map[string]any{
		"id": n.ID, "node_id": "", "body": n.Body, "path": n.Path, "line": n.Line, "original_line": n.Line, "side": side,
		"start_line": nil, "commit_id": n.CommitSHA, "original_commit_id": n.CommitSHA, "diff_hunk": "", "position": nil,
		"user": a.ghUser(ctx, n.AuthorKind, n.AuthorName), "author_association": "OWNER",
		"created_at": ghTime(n.CreatedAt), "updated_at": ghTime(n.UpdatedAt),
		"url":              fmt.Sprintf("%s/api/v3/repos/%s/pulls/comments/%d", base, rp.FullName(), n.ID),
		"html_url":         fmt.Sprintf("%s/%s/pulls/%d/files#note-%d", base, rp.FullName(), p.Number, n.ID),
		"pull_request_url": fmt.Sprintf("%s/api/v3/repos/%s/pulls/%d", base, rp.FullName(), p.Number),
		"atelier":          map[string]any{"resolved": n.Resolved},
	}
}

func (a *App) ghRelease(ctx context.Context, rp store.Repo, rel store.Release) map[string]any {
	base := a.appsBase()
	return map[string]any{
		"id": rel.ID, "node_id": "", "tag_name": rel.Tag, "target_commitish": rel.Target, "name": rel.Name(), "body": rel.Body,
		"draft": rel.Draft, "prerelease": rel.Prerelease, "created_at": ghTime(rel.CreatedAt), "published_at": ghTimePtr(rel.PublishedAt),
		"author": a.ghUser(ctx, rel.AuthorKind, rel.AuthorName), "assets": []any{},
		"url":      fmt.Sprintf("%s/api/v1/repos/%s/releases/%d", base, rp.FullName(), rel.ID),
		"html_url": base + releaseTagURL(rp, rel.Tag),
	}
}

func (a *App) ghWorkflowRun(ctx context.Context, rp store.Repo, run store.ActionsRun) map[string]any {
	base := a.appsBase()
	branch := strings.TrimPrefix(run.Ref, "refs/heads/")
	var concl any
	if run.Conclusion != "" {
		concl = run.Conclusion
	}
	status := "in_progress"
	if run.Done() {
		status = "completed"
	}
	return map[string]any{
		"id": run.ID, "node_id": "", "name": run.WorkflowName, "display_title": run.Title, "path": run.WorkflowPath,
		"run_number": run.RunNumber, "run_attempt": run.Attempt, "event": run.Event, "status": status, "conclusion": concl,
		"head_branch": branch, "head_sha": run.SHA, "created_at": ghTime(run.CreatedAt), "updated_at": ghTime(run.UpdatedAt),
		"run_started_at": ghTime(run.StartedAt), "html_url": base + actionsRunURL(rp, run),
		"actor": a.ghUser(ctx, run.ActorKind, run.ActorName), "repository": a.ghRepo(ctx, rp),
		"atelier": map[string]any{"seq": run.Seq},
	}
}

// ---------- events → webhooks ----------

// appsCommentID reads the comment id from a "#comment-N" link.
func appsCommentID(url string) int64 {
	_, frag, ok := strings.Cut(url, "#comment-")
	if !ok {
		return 0
	}
	id, _ := strconv.ParseInt(frag, 10, 64)
	return id
}

func appsOnEvent(ctx context.Context, a *App, ev Event) {
	if ev.RepoID == 0 {
		return
	}
	rp := ev.Repo
	if rp.ID == 0 {
		var err error
		if rp, err = a.db.RepoByIDApps(ctx, ev.RepoID); err != nil {
			return
		}
	}
	a.appsTetherOnEvent(ctx, rp, ev)

	hooks, err := a.db.HooksFor(ctx, rp)
	if err != nil || len(hooks) == 0 {
		return
	}
	payload, event, action := a.appsEventPayload(ctx, rp, ev)
	if payload == nil {
		return
	}
	payload["repository"] = a.ghRepo(ctx, rp)
	payload["sender"] = a.ghUser(ctx, ev.Actor.Kind, ev.Actor.Name)
	if rp.OwnerKind == "company" {
		payload["organization"] = a.ghOwner(rp.Owner, rp.OwnerKind)
	}
	a.appsQueue(ctx, rp, event, action, payload)
}

// appsEventPayload maps an Atelier event to a GitHub event, action and payload.
func (a *App) appsEventPayload(ctx context.Context, rp store.Repo, ev Event) (map[string]any, string, string) {
	switch ev.Kind {
	case EventIssueOpened, EventIssueClosed, EventIssueReopened:
		is, err := a.db.IssueByNumber(ctx, rp.ID, ev.Number)
		if err != nil {
			return nil, "", ""
		}
		action := strings.TrimPrefix(ev.Kind, "issue.")
		return map[string]any{"action": action, "issue": a.ghIssue(ctx, rp, is)}, "issues", action

	case EventIssueLabeled:
		is, err := a.db.IssueByNumber(ctx, rp.ID, ev.Number)
		if err != nil {
			return nil, "", ""
		}
		out := map[string]any{"action": "labeled", "issue": a.ghIssue(ctx, rp, is)}
		for _, l := range is.Labels {
			if l.Name == ev.Detail {
				out["label"] = ghLabels([]store.Label{l})[0]
			}
		}
		return out, "issues", "labeled"

	case EventIssueCommented:
		is, err := a.db.IssueByNumber(ctx, rp.ID, ev.Number)
		if err != nil {
			return nil, "", ""
		}
		c, err := a.db.IssueCommentByID(ctx, appsCommentID(ev.URL))
		if err != nil || c.IssueID != is.ID {
			return nil, "", ""
		}
		return map[string]any{"action": "created", "issue": a.ghIssue(ctx, rp, is),
			"comment": a.ghComment(ctx, rp, is.Number, c.ID, c.AuthorKind, c.AuthorName, c.Body, c.CreatedAt, c.UpdatedAt, false)}, "issue_comment", "created"

	case EventPullCommented:
		p, err := a.db.PullByNumber(ctx, rp.ID, ev.Number)
		if err != nil {
			return nil, "", ""
		}
		cs, err := a.db.PullComments(ctx, p.ID)
		if err != nil {
			return nil, "", ""
		}
		id := appsCommentID(ev.URL)
		for _, c := range cs {
			if c.ID == id {
				return map[string]any{"action": "created", "issue": a.ghPullAsIssue(ctx, rp, p),
					"comment": a.ghComment(ctx, rp, p.Number, c.ID, c.AuthorKind, c.AuthorName, c.Body, c.CreatedAt, c.UpdatedAt, true)}, "issue_comment", "created"
			}
		}
		return nil, "", ""

	case EventPullOpened, EventPullMerged, EventPullClosed, EventPullReopened:
		p, err := a.db.PullByNumber(ctx, rp.ID, ev.Number)
		if err != nil {
			return nil, "", ""
		}
		action := map[string]string{EventPullOpened: "opened", EventPullMerged: "closed", EventPullClosed: "closed", EventPullReopened: "reopened"}[ev.Kind]
		return map[string]any{"action": action, "number": p.Number, "pull_request": a.ghPull(ctx, rp, p)}, "pull_request", action

	case EventReleasePublished:
		rel, err := a.db.ReleaseByID(ctx, rp.ID, ev.Number)
		if err != nil {
			return nil, "", ""
		}
		return map[string]any{"action": "published", "release": a.ghRelease(ctx, rp, rel)}, "release", "published"

	case EventRunCompleted, EventRunFailed:
		run, err := a.db.ActionsRunBySeq(ctx, rp.ID, ev.Number)
		if err != nil {
			return nil, "", ""
		}
		return map[string]any{"action": "completed", "workflow_run": a.ghWorkflowRun(ctx, rp, run),
			"workflow": map[string]any{"name": run.WorkflowName, "path": run.WorkflowPath}}, "workflow_run", "completed"
	}
	return nil, "", ""
}

// appsPingPayload is the ping GitHub sends when a hook is created.
func (a *App) appsPingPayload(ctx context.Context, h store.Webhook, who Actor) map[string]any {
	typ := "Organization"
	if h.RepoID != 0 {
		typ = "Repository"
	}
	p := map[string]any{
		"zen":     "Keep it logically awesome.",
		"hook_id": h.ID,
		"hook": map[string]any{"type": typ, "id": h.ID, "name": "web", "active": h.Active, "events": h.Events,
			"config":     map[string]any{"content_type": appsContentType(h.ContentType), "insecure_ssl": "0", "url": h.URL},
			"created_at": ghTime(h.CreatedAt), "updated_at": ghTime(h.UpdatedAt)},
		"sender": a.ghUser(ctx, who.Kind, who.Name),
	}
	if h.RepoID != 0 {
		if rp, err := a.db.RepoByIDApps(ctx, h.RepoID); err == nil {
			p["repository"] = a.ghRepo(ctx, rp)
		}
	} else if o, err := a.db.OwnerByID(ctx, h.OwnerID); err == nil {
		p["organization"] = a.ghOwner(o.Name, o.Kind)
	}
	return p
}

// appsPing queues a ping for one hook.
func (a *App) appsPing(ctx context.Context, h store.Webhook, who Actor) error {
	if err := a.appsQueueOne(ctx, h, "ping", "", a.appsPingPayload(ctx, h, who), ""); err != nil {
		return err
	}
	a.apps().signal()
	return nil
}

// ---------- pushes (ref scanning) ----------
//
// Each watched repository's refs are compared with a snapshot. A repo.pushed
// event scans that repository at once (with the real pusher); a slow timer
// is the fallback for ref changes that don't emit one.

const appsZeroSHA = "0000000000000000000000000000000000000000"

func (a *App) appsRefs(ctx context.Context, rp store.Repo) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+a.git.Path(rp.Owner, rp.Name), "for-each-ref",
		"--format=%(objectname) %(refname)", "refs/heads", "refs/tags")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		sha, ref, ok := strings.Cut(line, " ")
		if ok {
			refs[ref] = sha
		}
	}
	return refs, nil
}

// appsScanRefs looks for ref changes in watched repositories and sends push
// and pull_request synchronize deliveries (and refreshes Tether mirrors).
func (a *App) appsScanRefs(ctx context.Context) {
	st := a.apps()
	st.refMu.Lock()
	defer st.refMu.Unlock()
	ids, err := a.db.ReposWithHooks(ctx)
	if err != nil {
		return
	}
	tids, _ := a.db.TetherReviewRepos(ctx)
	seen := map[int64]bool{}
	for _, id := range append(ids, tids...) {
		if seen[id] || ctx.Err() != nil {
			continue
		}
		seen[id] = true
		rp, err := a.db.RepoByIDApps(ctx, id)
		if err != nil {
			continue
		}
		a.appsScanRepo(ctx, rp)
	}
}

type appsRefChange struct{ Ref, Old, New string }

func (a *App) appsScanRepo(ctx context.Context, rp store.Repo) {
	a.appsScanRepoAs(ctx, rp, Actor{})
}

// appsOnPushed scans a watched repository as soon as it's pushed to.
func appsOnPushed(ctx context.Context, a *App, ev Event) {
	if ev.Kind != EventRepoPushed || ev.RepoID == 0 {
		return
	}
	ids, err := a.db.ReposWithHooks(ctx)
	if err != nil {
		return
	}
	tids, _ := a.db.TetherReviewRepos(ctx)
	for _, id := range append(ids, tids...) {
		if id != ev.RepoID {
			continue
		}
		rp, err := a.db.RepoByIDApps(ctx, id)
		if err != nil {
			return
		}
		st := a.apps()
		st.refMu.Lock()
		defer st.refMu.Unlock()
		a.appsScanRepoAs(ctx, rp, ev.Actor)
		return
	}
}

func init() { registerEventHandler(appsOnPushed) }

// appsScanRepoAs is appsScanRepo with a known pusher (zero Actor: the owner).
func (a *App) appsScanRepoAs(ctx context.Context, rp store.Repo, pusher Actor) {
	cur, err := a.appsRefs(ctx, rp)
	if err != nil {
		return
	}
	prev, err := a.db.RefSnapshot(ctx, rp.ID)
	if err != nil {
		return
	}
	if prev == nil { // first look: remember, don't announce history
		_ = a.db.SaveRefSnapshot(ctx, rp.ID, cur)
		return
	}
	delete(prev, "")
	var changes []appsRefChange
	for ref, sha := range cur {
		if prev[ref] != sha {
			old := prev[ref]
			if old == "" {
				old = appsZeroSHA
			}
			changes = append(changes, appsRefChange{ref, old, sha})
		}
	}
	for ref, sha := range prev {
		if _, ok := cur[ref]; !ok {
			changes = append(changes, appsRefChange{ref, sha, appsZeroSHA})
		}
	}
	if len(changes) == 0 {
		return
	}
	if err := a.db.SaveRefSnapshot(ctx, rp.ID, cur); err != nil {
		log.Printf("apps: saving refs for %s: %v", rp.FullName(), err)
		return
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Ref < changes[j].Ref })
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return
	}
	if pusher.Kind == "" {
		me, _ := a.db.PersonalOwner(ctx)
		pusher = Actor{Kind: ActorOwner, Name: me.Name, PersonID: me.ID}
	}
	for _, c := range changes {
		a.appsPushed(ctx, rp, g, c, pusher)
	}
}

func (a *App) appsPushed(ctx context.Context, rp store.Repo, g *gitrepo.Repo, c appsRefChange, pusher Actor) {
	base := a.appsBase()
	created, deleted := c.Old == appsZeroSHA, c.New == appsZeroSHA
	forced := false
	if !created && !deleted {
		if ff, err := g.IsAncestor(ctx, c.Old, c.New); err == nil && !ff {
			forced = true
		}
	}
	commits := []map[string]any{}
	var head any
	if !deleted {
		from := c.Old
		if created || forced {
			from = ""
		}
		limit := 20
		if created {
			limit = 1
		}
		list, _ := g.RangeCommits(ctx, from, c.New, limit)
		for i := len(list) - 1; i >= 0; i-- { // oldest first, like GitHub
			cm := list[i]
			msg := cm.Subject
			if cm.Body != "" {
				msg += "\n\n" + cm.Body
			}
			commits = append(commits, map[string]any{
				"id": cm.Hash, "tree_id": "", "distinct": true, "message": msg, "timestamp": ghTime(cm.When),
				"url":       fmt.Sprintf("%s/%s/commit/%s", base, rp.FullName(), cm.Hash),
				"author":    map[string]any{"name": cm.Author, "email": cm.Email, "username": nil},
				"committer": map[string]any{"name": cm.Author, "email": cm.Email, "username": nil},
				"added":     []string{}, "removed": []string{}, "modified": []string{},
			})
		}
		if len(commits) > 0 {
			head = commits[len(commits)-1]
		}
	}
	compare := fmt.Sprintf("%s/%s/commit/%s", base, rp.FullName(), c.New)
	if !created && !deleted {
		compare = fmt.Sprintf("%s/%s/compare?base=%s&head=%s", base, rp.FullName(), c.Old, c.New)
	}
	payload := map[string]any{
		"ref": c.Ref, "before": c.Old, "after": c.New, "created": created, "deleted": deleted, "forced": forced,
		"base_ref": nil, "compare": compare, "commits": commits, "head_commit": head,
		"pusher":     map[string]any{"name": pusher.Name, "email": nil},
		"repository": a.ghRepo(ctx, rp), "sender": a.ghUser(ctx, pusher.Kind, pusher.Name),
	}
	if rp.OwnerKind == "company" {
		payload["organization"] = a.ghOwner(rp.Owner, rp.OwnerKind)
	}
	a.appsQueue(ctx, rp, "push", "", payload)

	branch, ok := strings.CutPrefix(c.Ref, "refs/heads/")
	if !ok || deleted || created { // a new branch can't have moved under an open pull request
		return
	}
	pulls, err := a.db.Pulls(ctx, rp.ID, "open")
	if err != nil {
		return
	}
	for _, p := range pulls {
		if p.Head != branch {
			continue
		}
		pp := map[string]any{"action": "synchronize", "number": p.Number, "before": c.Old, "after": c.New,
			"pull_request": a.ghPull(ctx, rp, p), "repository": a.ghRepo(ctx, rp), "sender": a.ghUser(ctx, pusher.Kind, pusher.Name)}
		a.appsQueue(ctx, rp, "pull_request", "synchronize", pp)
		a.appsTetherSync(ctx, rp, p, c.New)
	}
}

func (a *App) appsRefLoop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.appsScanRefs(ctx)
		}
	}
}
