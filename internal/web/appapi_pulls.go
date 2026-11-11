package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atelier/internal/actions"
	"atelier/internal/gitrepo"
	"atelier/internal/importer"
	"atelier/internal/store"
	"atelier/internal/workflow"
)

// Pull requests for the app API: thin JSON over the pull request page's own
// data (buildPull, pullStatus, commitChecks), merging through mergePull or
// "Merge on GitHub", and merge when green.

var (
	// appAutoMergeSweep is how often waiting merges are re-checked without an event.
	appAutoMergeSweep = 15 * time.Second
	// appAutoMergeHeadGrace is how long a moved head branch may go
	// unexplained (no repo.pushed event yet) before the merge is cancelled.
	appAutoMergeHeadGrace = 30 * time.Second
)

// EventPullAutoMergeCancelled tells the person a merge when green was called off; Detail says why.
const EventPullAutoMergeCancelled = "pull.auto_merge_cancelled"

func appAutoMergeRoutes(a *App, m *http.ServeMux) {
	// The Cancel button beside "Will merge when checks pass" on the pull request page.
	m.HandleFunc("POST /{owner}/{repo}/pulls/{number}/auto-merge", a.authed(a.pullAutoMergeCancel))
}

func init() {
	registerTemplateFuncs(func(a *App) template.FuncMap {
		return template.FuncMap{
			// pullAutoMerge is the pending merge when green of a pull request, or nil.
			"pullAutoMerge": func(repoID, number int64) *store.AppAutoMerge {
				m, err := a.db.AppAutoMerge(context.Background(), repoID, number)
				if err != nil {
					return nil
				}
				return &m
			},
		}
	})
}

// ---------- building ----------

// appRepoView is the repoView the pull request code needs, without a request.
func (a *App) appRepoView(ctx context.Context, rp store.Repo) (*repoView, *gitrepo.Repo, error) {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return nil, nil, err
	}
	v := appsView(rp)
	v.Tab, v.Empty, v.Default = "pulls", g.IsEmpty(ctx), "main"
	if !v.Empty {
		v.Default = g.DefaultBranch(ctx)
	}
	return v, g, nil
}

// appBuildPull builds the pull request page's data for p.
func (a *App) appBuildPull(ctx context.Context, rp store.Repo, p store.Pull) (*repoView, *gitrepo.Repo, *pullPageData, error) {
	v, g, err := a.appRepoView(ctx, rp)
	if err != nil {
		return nil, nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.Base, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	d, err := a.buildPull(req, v, g, p, "overview")
	return v, g, d, err
}

func (a *App) appOnGitHub(ctx context.Context, repoID int64) bool {
	l, err := a.db.TetherLink(ctx, repoID)
	return err == nil && l.Home == store.TetherHomeGitHub
}

// appVia says where an author's words came from: "tether" (GitHub),
// "connect" (another studio), "bot", or nil for someone here.
func appVia(kind, name string) any {
	switch {
	case kind == "tether", kind == importer.AuthorImport && strings.HasPrefix(name, tetherAuthorPrefix):
		return "tether"
	case kind == "connect":
		return "connect"
	case kind == ActorBot:
		return "bot"
	}
	return nil
}

// appThread sorts an actor into the app's threads: yours, github, agents, connect.
func appThread(kind, name string) string {
	switch {
	case kind == "agent", kind == ActorJob, strings.HasPrefix(name, "agent:"):
		return "agents"
	case kind == "connect":
		return "connect"
	case kind == "tether", kind == importer.AuthorImport, strings.HasPrefix(name, tetherAuthorPrefix), kind == ActorBot && name == "github":
		return "github"
	}
	return "yours"
}

type appCheckCounts struct{ Passed, Failed, Pending, Waiting int }

// appGreen: every check passed and nothing blocks the merge.
func appGreen(d *pullPageData) bool {
	return d.P.IsOpen() && d.Status.CanMerge && d.Checks.Failed == 0 && d.Checks.Pending == 0
}

// appChecks turns the page's checks into the API's list and counts.
func (a *App) appChecks(ctx context.Context, d *pullPageData, detail bool) ([]map[string]any, appCheckCounts) {
	var n appCheckCounts
	out := []map[string]any{}
	for _, c := range d.Checks.Checks {
		status := c.Status
		if status == "pending" && c.Detail == actions.StatusWaiting {
			status = "waiting"
		}
		switch status {
		case "success", "skipped":
			n.Passed++
		case "failure":
			n.Failed++
		case "waiting":
			n.Waiting++
		default:
			n.Pending++
		}
		if !detail {
			continue
		}
		name := c.Name
		if c.Workflow != "" && c.JobID != 0 {
			name = c.Workflow + " · " + c.Name
		}
		row := map[string]any{"name": name, "status": status, "detail": c.Detail, "run_id": nil, "job_id": nil, "required": c.Required}
		if c.JobID != 0 {
			row["run_id"], row["job_id"] = c.RunID, c.JobID
			if j, err := a.db.ActionsJob(ctx, c.JobID); err == nil {
				agent := appIsAgentJob(j)
				switch {
				case status == "waiting" && agent:
					row["detail"], row["action"] = "agent · needs your approval to run", "approve_agent"
				case status == "waiting":
					row["detail"] = "needs your approval to run"
				case j.Status == actions.StatusRunning:
					row["detail"] = appJoin(j.RunnerName, "running "+fmtDuration(j.Duration()))
				case j.Finished():
					row["detail"] = appJoin(j.RunnerName, fmtDuration(j.Duration()))
					if j.Status == actions.StatusCancelled {
						row["detail"] = appJoin(j.RunnerName, "cancelled")
					}
				default:
					row["detail"] = j.Status
				}
			}
		}
		out = append(out, row)
	}
	return out, n
}

func appJoin(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " · ")
}

// appIsAgentJob reports whether a job is the agent of an agentic workflow
// (a job that hasn't been prepared yet is judged by its id alone).
func appIsAgentJob(j store.ActionsJob) bool {
	if j.JobKey != workflow.AgentJobID {
		return false
	}
	return j.Payload == "" || len(agentJobCaps(j)) > 0
}

// appChecksSummary is the compact checks object of a pull summary, counted
// from the head commit's checks (pullChecks). A check waiting for the
// person's approval is counted in waiting, not pending, and holds the state
// at pending like a running one.
func appChecksSummary(n appCheckCounts) map[string]any {
	state := "passing"
	switch {
	case n.Passed+n.Failed+n.Pending+n.Waiting == 0:
		state = "none"
	case n.Failed > 0:
		state = "failing"
	case n.Pending+n.Waiting > 0:
		state = "pending"
	}
	return map[string]any{"total": n.Passed + n.Failed + n.Pending + n.Waiting, "passed": n.Passed, "failed": n.Failed,
		"pending": n.Pending, "waiting": n.Waiting, "state": state}
}

// appChecksSentence describes checks still in the way, for merge.reason.
func appChecksSentence(n appCheckCounts) string {
	var parts []string
	if n.Failed > 0 {
		parts = append(parts, plural(n.Failed, "check")+" failed")
	}
	if n.Pending > 0 {
		s := plural(n.Pending, "check") + " is still running"
		if n.Pending > 1 {
			s = plural(n.Pending, "check") + " are still running"
		}
		parts = append(parts, s)
	}
	if n.Waiting > 0 {
		s := plural(n.Waiting, "check") + " is waiting for you"
		if n.Waiting > 1 {
			s = plural(n.Waiting, "check") + " are waiting for you"
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " and ") + "."
}

// appPullJSON is the contract's pull object: the summary, or with full the
// checks list and the comments.
func (a *App) appPullJSON(ctx context.Context, rp store.Repo, d *pullPageData, full bool) map[string]any {
	p := d.P
	files := 0
	if d.Cmp != nil && d.Cmp.Diff != nil {
		files = len(d.Cmp.Diff.Files)
	}
	onGitHub := a.appOnGitHub(ctx, rp.ID)
	checks, counts := a.appChecks(ctx, d, full)
	methods := []string{}
	reason := ""
	switch {
	case !p.IsOpen():
		reason = "This pull request is " + p.State + "."
	case onGitHub:
		methods = []string{"squash", "merge", "rebase"}
	default:
		for _, m := range d.Status.Methods {
			if m.Allowed {
				methods = append(methods, m.Value)
			}
		}
	}
	if p.IsOpen() {
		reason = d.Status.Reason
		if reason == "" {
			reason = appChecksSentence(counts)
		}
	}
	var auto any
	if am, err := a.db.AppAutoMerge(ctx, rp.ID, p.Number); err == nil && p.IsOpen() {
		auto = map[string]any{"method": am.Method, "by": am.By()}
	}
	out := map[string]any{
		"repo": rp.FullName(), "number": p.Number, "title": p.Title, "state": p.State,
		"head": p.Head, "base": p.Base, "author": p.AuthorKind + ":" + p.AuthorName,
		"commits": d.Commits, "files": files, "via": appVia(p.AuthorKind, p.AuthorName),
		"merge": map[string]any{"can_merge": p.IsOpen() && d.Status.CanMerge, "green": appGreen(d), "reason": reason,
			"methods": methods, "on_github": onGitHub, "auto_merge": auto},
		"web_path":   fmt.Sprintf("/%s/pulls/%d", rp.FullName(), p.Number),
		"updated_at": appTime(p.UpdatedAt),
	}
	if !full {
		out["checks"] = appChecksSummary(counts)
		return out
	}
	out["checks"] = checks
	comments := []map[string]any{}
	if cs, err := a.db.PullComments(ctx, p.ID); err == nil {
		for _, c := range cs {
			author := strings.TrimPrefix(c.AuthorName, tetherAuthorPrefix)
			kind := "person"
			switch {
			case c.AuthorKind == ActorBot, c.AuthorKind == "tether", strings.HasSuffix(author, "[bot]"):
				kind = "bot"
			case c.AuthorKind == "agent", c.AuthorKind == ActorJob:
				kind = "agent"
			}
			comments = append(comments, map[string]any{"author": author, "via": appVia(c.AuthorKind, c.AuthorName), "kind": kind,
				"body": c.Body, "at": appTime(c.CreatedAt)})
		}
	}
	out["comments"] = comments
	return out
}

// ---------- reading ----------

func appPullState(w http.ResponseWriter, r *http.Request) (string, bool) {
	switch s := r.URL.Query().Get("state"); s {
	case "", "open":
		return "open", true
	case "closed", "merged":
		return s, true
	case "all":
		return "", true
	}
	appError(w, http.StatusUnprocessableEntity, "invalid_state", "state must be open, closed, merged or all.")
	return "", false
}

const appPullsMax = 100

func (a *App) appPullSummaries(ctx context.Context, repos []store.Repo, state string) []map[string]any {
	out := []map[string]any{}
	for _, rp := range repos {
		pulls, err := a.db.Pulls(ctx, rp.ID, state)
		if err != nil {
			continue
		}
		for _, p := range pulls {
			if len(out) >= appPullsMax {
				return out
			}
			_, _, d, err := a.appBuildPull(ctx, rp, p)
			if err != nil {
				log.Printf("appapi: pull %s#%d: %v", rp.FullName(), p.Number, err)
				continue
			}
			out = append(out, a.appPullJSON(ctx, rp, d, false))
		}
	}
	return out
}

func (a *App) appPullsAll(w http.ResponseWriter, r *http.Request, c *appCaller) {
	state, ok := appPullState(w, r)
	if !ok {
		return
	}
	var repos []store.Repo
	if name := r.URL.Query().Get("repo"); name != "" { // one repository, like /runs?repo=
		owner, repo, _ := strings.Cut(name, "/")
		rp, ok := a.appRepoNamed(w, r, c, owner, repo, PermRead)
		if !ok {
			return
		}
		repos = []store.Repo{rp}
	} else {
		var err error
		if repos, _, err = a.appReadable(r.Context(), c.Who); err != nil {
			a.appFail(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pulls": a.appPullSummaries(r.Context(), repos, state)})
}

func (a *App) appPullsRepo(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, ok := a.appRepo(w, r, c, PermRead)
	if !ok {
		return
	}
	state, ok := appPullState(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pulls": a.appPullSummaries(r.Context(), []store.Repo{rp}, state)})
}

// appLoadPull loads the repository (checking need) and the pull request in the path.
func (a *App) appLoadPull(w http.ResponseWriter, r *http.Request, c *appCaller, need Perm) (store.Repo, store.Pull, bool) {
	rp, ok := a.appRepo(w, r, c, need)
	if !ok {
		return rp, store.Pull{}, false
	}
	n, ok := appPathID(r, "n")
	if !ok {
		appError(w, http.StatusNotFound, "not_found", "There's no such pull request.")
		return rp, store.Pull{}, false
	}
	p, err := a.db.PullByNumber(r.Context(), rp.ID, n)
	if err != nil {
		appError(w, http.StatusNotFound, "not_found", "There's no such pull request.")
		return rp, store.Pull{}, false
	}
	return rp, p, true
}

func (a *App) appPull(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, p, ok := a.appLoadPull(w, r, c, PermRead)
	if !ok {
		return
	}
	_, _, d, err := a.appBuildPull(r.Context(), rp, p)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.appPullJSON(r.Context(), rp, d, true))
}

// ---------- merging ----------

func (a *App) appPullMerge(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, p, ok := a.appLoadPull(w, r, c, PermWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	var in struct {
		Method       string `json:"method"`
		WhenGreen    bool   `json:"when_green"`
		DeleteBranch bool   `json:"delete_branch"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	if in.Method == "" {
		in.Method = "squash"
	}
	if !p.IsOpen() {
		appError(w, http.StatusConflict, "not_open", "This pull request isn't open.")
		return
	}
	v, g, d, err := a.appBuildPull(ctx, rp, p)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	onGitHub := a.appOnGitHub(ctx, rp.ID)
	allowed := false
	if onGitHub {
		allowed = in.Method == "squash" || in.Method == "merge" || in.Method == "rebase"
	} else {
		for _, m := range d.Status.Methods {
			allowed = allowed || (m.Value == in.Method && m.Allowed)
		}
	}
	if !allowed {
		appError(w, http.StatusUnprocessableEntity, "invalid_method", "That merge method isn't allowed here.")
		return
	}

	if in.WhenGreen {
		am := store.AppAutoMerge{RepoID: rp.ID, Number: p.Number, Method: in.Method, ByKind: c.Who.Kind, ByName: c.Who.Name,
			PersonID: c.Who.PersonID, HeadSHA: d.HeadSHA, DeleteBranch: in.DeleteBranch}
		if c.Device != nil {
			am.DeviceID = c.Device.ID
		}
		if err := a.db.SetAppAutoMerge(ctx, am); err != nil {
			a.appFail(w, r, err)
			return
		}
		a.app().hub.statusChanged(ctx, a, rp, c.Who, "needs")
		a.autoMergeSoon(ctx, rp.ID, p.Number)
		writeJSON(w, http.StatusAccepted, map[string]any{"auto_merge": map[string]any{"method": am.Method, "by": am.By()}})
		return
	}

	sha, err := a.appMergeNow(ctx, rp, v, g, d, pullMergeOpts{Method: in.Method, DeleteBranch: in.DeleteBranch}, c.Who)
	var me *pullMergeError
	switch {
	case errors.As(err, &me) && me.Code == http.StatusBadGateway:
		appError(w, me.Code, "github_failed", me.Msg)
		return
	case errors.As(err, &me) && me.Code == http.StatusUnprocessableEntity:
		appError(w, me.Code, "invalid_method", me.Msg)
		return
	case errors.As(err, &me):
		appError(w, http.StatusConflict, "not_mergeable", me.Msg)
		return
	case err != nil:
		a.appFail(w, r, err)
		return
	}
	_, _ = a.db.DeleteAppAutoMerge(ctx, rp.ID, p.Number)
	writeJSON(w, http.StatusOK, map[string]any{"merged": true, "sha": sha})
}

// appMergeNow merges like the web button: through mergePull, or on GitHub
// when that is the repository's home.
func (a *App) appMergeNow(ctx context.Context, rp store.Repo, v *repoView, g *gitrepo.Repo, d *pullPageData, opts pullMergeOpts, who Actor) (string, error) {
	if a.appOnGitHub(ctx, rp.ID) {
		sha, err := a.tetherMergeOnGitHub(ctx, rp, d.P, opts.Method, who)
		if err != nil {
			return "", &pullMergeError{http.StatusBadGateway, "Couldn't merge on GitHub: " + tetherErrText(err)}
		}
		return sha, nil
	}
	res, err := a.mergePull(ctx, v, g, d, opts, who)
	if err != nil {
		return "", err
	}
	return res.SHA, nil
}

func (a *App) appPullMergeCancel(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, p, ok := a.appLoadPull(w, r, c, PermWrite)
	if !ok {
		return
	}
	found, err := a.db.DeleteAppAutoMerge(r.Context(), rp.ID, p.Number)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	if !found {
		appError(w, http.StatusNotFound, "no_auto_merge", "This pull request isn't waiting to merge.")
		return
	}
	a.app().hub.statusChanged(r.Context(), a, rp, c.Who, "needs")
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// pullAutoMergeCancel is the web page's Cancel button.
func (a *App) pullAutoMergeCancel(w http.ResponseWriter, r *http.Request) {
	v, _, p, ok := a.loadPull(w, r) // POST: needs write
	if !ok {
		return
	}
	if r.FormValue("action") == "cancel" {
		if _, err := a.db.DeleteAppAutoMerge(r.Context(), v.Repo.ID, p.Number); err != nil {
			a.fail(w, r, err)
			return
		}
		a.app().hub.statusChanged(r.Context(), a, v.Repo, a.actor(r), "needs")
	}
	http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d", v.Base, p.Number), http.StatusSeeOther)
}

// ---------- merge when green ----------

// autoMergeSoon re-checks one waiting merge in the background.
func (a *App) autoMergeSoon(ctx context.Context, repoID, number int64) {
	ctx = context.WithoutCancel(ctx)
	a.app().goBG(func() { a.autoMergeEval(ctx, repoID, number) })
}

func (a *App) autoMergeSweep(ctx context.Context) {
	ams, err := a.db.AppAutoMerges(ctx, 0)
	if err != nil {
		return
	}
	for _, am := range ams {
		if ctx.Err() != nil {
			return
		}
		a.autoMergeEval(ctx, am.RepoID, am.Number)
	}
}

// autoMergeActor rebuilds who asked, as they are now: a revoked device or a
// deleted token no longer merges anything.
func (a *App) autoMergeActor(ctx context.Context, am store.AppAutoMerge) (Actor, bool) {
	if am.DeviceID != 0 {
		d, err := a.db.AppDeviceByID(ctx, am.DeviceID)
		if err != nil || d.Revoked() || d.PersonID != am.PersonID {
			return Actor{}, false
		}
		return appDeviceActor(d), true
	}
	toks, err := a.db.Tokens(ctx)
	if err != nil {
		return Actor{}, false
	}
	for _, t := range toks {
		if t.Name != am.ByName || (t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt)) {
			continue
		}
		if person, err := a.db.TokenPerson(ctx, t.ID); err == nil && person == am.PersonID && t.Has(appScope) {
			return Actor{Kind: ActorToken, Name: t.Name, PersonID: person, Scopes: t.Scopes}, true
		}
	}
	return Actor{}, false
}

// autoMergeCancel calls a waiting merge off and tells the person why.
func (a *App) autoMergeCancel(ctx context.Context, rp store.Repo, p store.Pull, why string) {
	found, err := a.db.DeleteAppAutoMerge(ctx, rp.ID, p.Number)
	if err != nil || !found {
		return
	}
	delete(a.app().headMoved, fmt.Sprintf("%d:%d", rp.ID, p.Number))
	log.Printf("appapi: merge when green cancelled for %s#%d: %s", rp.FullName(), p.Number, why)
	a.emit(ctx, Event{Kind: EventPullAutoMergeCancelled, Repo: rp, RepoID: rp.ID, Number: p.Number, Title: p.Title,
		URL: fmt.Sprintf("/%s/pulls/%d", rp.FullName(), p.Number), Detail: "Won't merge by itself: " + why,
		Actor: Actor{Kind: ActorSystem, Name: "auto-merge"}})
}

// autoMergeEval decides one waiting merge: merge it if everything is green,
// cancel it if a check failed, the head moved under it or it can't be merged
// by who asked any more, and otherwise leave it waiting.
func (a *App) autoMergeEval(ctx context.Context, repoID, number int64) {
	st := a.app()
	st.mergeMu.Lock()
	defer st.mergeMu.Unlock()
	am, err := a.db.AppAutoMerge(ctx, repoID, number)
	if err != nil {
		return
	}
	key := fmt.Sprintf("%d:%d", repoID, number)
	drop := func() {
		_, _ = a.db.DeleteAppAutoMerge(ctx, repoID, number)
		delete(st.headMoved, key)
	}
	rp, err := a.repoByID(ctx, repoID)
	if err != nil {
		drop()
		return
	}
	p, err := a.db.PullByNumber(ctx, repoID, number)
	if err != nil || !p.IsOpen() {
		drop() // merged or closed: nothing left to wait for
		return
	}
	who, ok := a.autoMergeActor(ctx, am)
	if !ok || !a.can(ctx, who, rp, PermWrite) {
		a.autoMergeCancel(ctx, rp, p, am.By()+" can no longer merge here.")
		return
	}
	v, g, d, err := a.appBuildPull(ctx, rp, p)
	if err != nil {
		log.Printf("appapi: merge when green %s#%d: %v", rp.FullName(), number, err)
		return
	}
	switch {
	case !d.HeadExists:
		a.autoMergeCancel(ctx, rp, d.P, "the branch "+p.Head+" was deleted.")
		return
	case d.HeadSHA != am.HeadSHA:
		// A push the repo.pushed handler hasn't explained yet (it moves the
		// merge along when the asker pushed, and cancels it otherwise).
		first, seen := st.headMoved[key]
		if !seen {
			st.headMoved[key] = time.Now()
		} else if time.Since(first) > appAutoMergeHeadGrace {
			a.autoMergeCancel(ctx, rp, d.P, "new commits were pushed to "+p.Head+".")
		}
		return
	}
	delete(st.headMoved, key)
	if d.Checks.Failed > 0 {
		var names []string
		for _, c := range d.Checks.Checks {
			if c.Status == "failure" {
				names = append(names, c.Name)
			}
		}
		a.autoMergeCancel(ctx, rp, d.P, pluralWord(len(names), "check")+" failed: "+strings.Join(names, ", ")+".")
		return
	}
	if !appGreen(d) {
		return // still waiting
	}
	sha, err := a.appMergeNow(ctx, rp, v, g, d, pullMergeOpts{Method: am.Method, ExpectedHead: am.HeadSHA, DeleteBranch: am.DeleteBranch}, who)
	var me *pullMergeError
	switch {
	case errors.As(err, &me) && me.Code == http.StatusUnprocessableEntity:
		a.autoMergeCancel(ctx, rp, d.P, "the "+am.Method+" method isn't allowed any more.")
	case errors.As(err, &me):
		log.Printf("appapi: merge when green %s#%d not merged yet: %s", rp.FullName(), number, me.Msg)
	case err != nil:
		log.Printf("appapi: merge when green %s#%d: %v", rp.FullName(), number, err)
	default:
		drop()
		log.Printf("appapi: merged %s#%d when green for %s as %s", rp.FullName(), number, am.By(), shortSHA(sha))
	}
}

// autoMergeOnEvent keeps waiting merges in step with what happens: pushes to
// the head branch, finished runs, and anything about the pull request itself.
// The work happens in the background so an event handler never waits.
func (a *App) autoMergeOnEvent(ctx context.Context, ev Event) {
	if ev.RepoID == 0 || ev.Kind == EventPullAutoMergeCancelled {
		return
	}
	ams, err := a.db.AppAutoMerges(ctx, ev.RepoID)
	if err != nil || len(ams) == 0 {
		return
	}
	rp := ev.Repo
	if rp.ID != ev.RepoID {
		if rp, err = a.repoByID(ctx, ev.RepoID); err != nil {
			return
		}
	}
	ctx = context.WithoutCancel(ctx)
	for _, am := range ams {
		if ev.Subject() == "pull" && ev.Number != am.Number {
			continue
		}
		a.app().goBG(func() {
			if ev.Kind == EventRepoPushed && ev.Push != nil {
				a.autoMergePushed(ctx, rp, am, ev)
			}
			a.autoMergeEval(ctx, am.RepoID, am.Number)
		})
	}
}

// autoMergePushed handles a push to a waiting merge's head branch: the
// asker's own push moves the merge along to the new commit, anyone else's
// cancels it.
func (a *App) autoMergePushed(ctx context.Context, rp store.Repo, am store.AppAutoMerge, ev Event) {
	branch, ok := strings.CutPrefix(ev.Push.Ref, "refs/heads/")
	p, err := a.db.PullByNumber(ctx, am.RepoID, am.Number)
	if !ok || err != nil || p.Head != branch {
		return
	}
	st := a.app()
	st.mergeMu.Lock()
	defer st.mergeMu.Unlock()
	if am, err = a.db.AppAutoMerge(ctx, am.RepoID, am.Number); err != nil {
		return
	}
	same := ev.Actor.PersonID == am.PersonID &&
		(ev.Actor.Kind == ActorOwner || (ev.Actor.Kind == ActorToken && ev.Actor.Name == am.ByName))
	switch {
	case actZeroSHA(ev.Push.New):
		a.autoMergeCancel(ctx, rp, p, "the branch "+branch+" was deleted.")
	case ev.Push.New == am.HeadSHA:
	case same:
		_ = a.db.SetAppAutoMergeHead(ctx, am.RepoID, am.Number, ev.Push.New)
		delete(st.headMoved, strconv.FormatInt(am.RepoID, 10)+":"+strconv.FormatInt(am.Number, 10))
	default:
		a.autoMergeCancel(ctx, rp, p, ev.Actor.Name+" pushed new commits to "+branch+".")
	}
}
