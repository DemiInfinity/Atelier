package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"atelier/internal/actions"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
	"atelier/internal/workflow"
	"atelier/internal/workflow/expr"
)

// Actions: workflows, runs, the job queue and the runner API.
//
//   actions.go          triggers (push, pull requests, dispatch, schedules,
//                       releases) and creating runs
//   actions_sched.go    the needs graph: queueing, approvals, finishing,
//                       cancelling and re-running jobs
//   actions_api.go      the runner API (internal/actions/contract.go) and job tokens
//   actions_ui.go       repository pages: workflows, runs, logs
//   actions_runners.go  Settings → Runners
//   actions_checks.go   jobs as checks on commits and pull requests
//
// Pushes reach Actions through the post-receive hook (`atelier hook
// post-receive`), which records each ref update in actions_push_queue. The
// start loop drains that table every second. A table rather than an HTTP
// callback means no shared secret or loopback port is needed, the hook
// stays fast, and pushes made while the server is restarting aren't lost.
// Atelier's own merges move refs without hooks, so pull.merged events are
// queued the same way.

const (
	EventRunCompleted  = "run.completed"
	EventRunFailed     = "run.failed"
	EventJobWaiting    = "job.waiting"
	EventRunnerOffline = "runner.offline"
	// EventReleasePublished is emitted by the releases feature. Actions reads
	// the tag from Detail, or from a URL ending in /releases/tag/<tag>.
	EventReleasePublished = "release.published"
)

// ActorJob is an Actions job acting with its job token.
const ActorJob = "job"

// actionsState is the Actions engine's per-App state.
type actionsState struct {
	mu sync.Mutex // serialises scheduling decisions

	wakeMu sync.Mutex
	wake   chan struct{} // closed and replaced whenever jobs or logs change

	bg sync.WaitGroup // background trigger work (tests wait on it)

	resolveRemote workflow.ResolveFunc
	pollWait      time.Duration
	offlineAfter  time.Duration
	maxLogBytes   int64

	cacheMu sync.Mutex
	wfCache map[string][]*workflow.Workflow // "repoID:sha" → workflows
	cronAt  time.Time                       // last minute schedules were checked

	baseMu  sync.Mutex
	baseURL string // last external URL seen, for background work without a request
}

var actionsStates sync.Map // *App → *actionsState

func (a *App) actions() *actionsState {
	if st, ok := actionsStates.Load(a); ok {
		return st.(*actionsState)
	}
	ls := &workflow.LsRemote{Base: "https://github.com"}
	st, _ := actionsStates.LoadOrStore(a, &actionsState{
		wake: make(chan struct{}), resolveRemote: ls.Resolve, pollWait: actions.PollWait,
		offlineAfter: 90 * time.Second, maxLogBytes: 64 << 20, wfCache: map[string][]*workflow.Workflow{},
	})
	return st.(*actionsState)
}

// signal wakes long-polling runners and live log streams.
func (st *actionsState) signal() {
	st.wakeMu.Lock()
	close(st.wake)
	st.wake = make(chan struct{})
	st.wakeMu.Unlock()
}

func (st *actionsState) waitCh() <-chan struct{} {
	st.wakeMu.Lock()
	defer st.wakeMu.Unlock()
	return st.wake
}

func (st *actionsState) go_(f func()) {
	st.bg.Add(1)
	go func() {
		defer st.bg.Done()
		f()
	}()
}

func (a *App) actionsBaseURL() string {
	if a.cfg.PublicURL != "" {
		return a.cfg.PublicURL
	}
	st := a.actions()
	st.baseMu.Lock()
	defer st.baseMu.Unlock()
	return st.baseURL
}

func (a *App) rememberBaseURL(base string) {
	if a.cfg.PublicURL != "" || base == "" {
		return
	}
	st := a.actions()
	st.baseMu.Lock()
	st.baseURL = base
	st.baseMu.Unlock()
}

const settingActionsLogDays = "actions.log_retention_days"

func init() {
	registerFeature(feature{
		name:   "actions",
		routes: actionsRoutes,
		start:  func(ctx context.Context, a *App) { a.actionsLoop(ctx) },
	})
	registerEventHandler(actionsEventHandler)
}

func actionsRoutes(a *App, m *http.ServeMux) {
	actionsUIRoutes(a, m)
	actionsRunnerRoutes(a, m)
	actionsAPIRoutes(a, m)
}

// actionsLoop drains the push queue, fires schedules and enforces timeouts.
func (a *App) actionsLoop(ctx context.Context) {
	a.ensureActionsHooks(ctx)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lastMaint, lastPrune time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			a.drainActionsPushes(ctx)
			a.fireSchedules(ctx, now)
			if now.Sub(lastMaint) >= 10*time.Second {
				lastMaint = now
				a.actionsMaintenance(ctx)
			}
			if now.Sub(lastPrune) >= 6*time.Hour {
				lastPrune = now
				a.pruneActionsLogs(ctx)
			}
		}
	}
}

// ensureActionsHooks installs the post-receive hook on every repository.
func (a *App) ensureActionsHooks(ctx context.Context) {
	repos, err := a.db.Repos(ctx)
	if err != nil {
		return
	}
	for _, rp := range repos {
		if _, err := a.git.Open(rp.Owner, rp.Name); err != nil {
			continue
		}
		if err := a.git.InstallPostReceiveHook(rp.Owner, rp.Name); err != nil {
			log.Printf("actions: installing post-receive hook for %s: %v", rp.FullName(), err)
		}
	}
}

func (a *App) pruneActionsLogs(ctx context.Context) {
	days := a.actionsLogDays(ctx)
	if days <= 0 {
		return
	}
	if n, err := a.db.PruneActionsLogs(ctx, time.Now().AddDate(0, 0, -days)); err != nil {
		log.Printf("actions: pruning logs: %v", err)
	} else if n > 0 {
		log.Printf("actions: pruned %d log chunks older than %d days", n, days)
	}
}

func (a *App) actionsLogDays(ctx context.Context) int {
	if v, err := a.db.Setting(ctx, settingActionsLogDays); err == nil {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			return n
		}
	}
	return 90
}

// ---------- reading workflows from git ----------

// loadWorkflows reads the workflows at a commit (cached: commits don't change).
func (a *App) loadWorkflows(ctx context.Context, repoID int64, g *gitrepo.Repo, sha string) []*workflow.Workflow {
	st := a.actions()
	key := fmt.Sprintf("%d:%s", repoID, sha)
	st.cacheMu.Lock()
	if w, ok := st.wfCache[key]; ok {
		st.cacheMu.Unlock()
		return w
	}
	st.cacheMu.Unlock()

	var out []*workflow.Workflow
	for _, dir := range workflow.Dirs {
		entries, err := g.Tree(ctx, sha, dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !(workflow.IsWorkflowFile(e.Name) || workflow.IsAgenticFile(e.Name)) {
				continue
			}
			b, err := g.Blob(ctx, sha, e.Path, workflow.MaxFileSize)
			if err != nil {
				continue
			}
			if b.TooLarge {
				out = append(out, &workflow.Workflow{Path: e.Path, Name: e.Name,
					Errors: []string{fmt.Sprintf("The workflow file is larger than %d KB.", workflow.MaxFileSize/1024)}})
				continue
			}
			if workflow.IsAgenticFile(e.Name) {
				wf, _ := workflow.ParseAgentic(e.Path, b.Data)
				out = append(out, wf)
				continue
			}
			out = append(out, workflow.Parse(e.Path, b.Data))
		}
		if len(out) > 0 {
			break // .github/workflows only when .atelier/workflows has none
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	st.cacheMu.Lock()
	if len(st.wfCache) > 256 {
		st.wfCache = map[string][]*workflow.Workflow{}
	}
	st.wfCache[key] = out
	st.cacheMu.Unlock()
	return out
}

func actFindWorkflow(wfs []*workflow.Workflow, p string) *workflow.Workflow {
	for _, w := range wfs {
		if w.Path == p {
			return w
		}
	}
	return nil
}

// changedFiles lists files changed between two commits, or nil when unknown.
func (a *App) changedFiles(ctx context.Context, rp store.Repo, from, to string) []string {
	if from == "" || actZeroSHA(from) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.git.Git, "-c", "core.quotePath=false", "diff", "--name-only", "-z", "--no-renames", from, to, "--")
	cmd.Dir = a.git.Path(rp.Owner, rp.Name)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	files := []string{}
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
		if len(files) > 5000 {
			return nil // too many to matter: treat as "anything changed"
		}
	}
	return files
}

func actZeroSHA(s string) bool { return strings.Trim(s, "0") == "" }

// ---------- triggers ----------

// actTrigger is one event that may start workflow runs.
type actTrigger struct {
	Event      string // push, pull_request, workflow_dispatch, schedule, release
	Action     string // pull_request: opened/synchronize/reopened; release: published
	Ref        string // refs/heads/… or refs/tags/…
	SHA        string
	Before     string // push: the previous sha
	Actor      Actor
	Inputs     map[string]string
	Payload    map[string]any
	PullNumber int64
	PullBase   string
	PullHead   string
	Approval   string // jobs wait for approval with this reason (outside pull requests)
	Title      string
	Workflow   string // workflow_dispatch / schedule: only this workflow
	Changed    []string
}

// drainActionsPushes turns queued ref updates into runs.
func (a *App) drainActionsPushes(ctx context.Context) {
	for i := 0; i < 20; i++ {
		pushes, err := a.db.TakeActionsPushes(ctx, 50)
		if err != nil {
			log.Printf("actions: reading push queue: %v", err)
			return
		}
		for _, p := range pushes {
			a.emitPushed(ctx, p)
			a.handlePush(ctx, p)
		}
		if len(pushes) < 50 {
			return
		}
	}
}

// emitPushed announces a ref update as repo.pushed (see events.go).
func (a *App) emitPushed(ctx context.Context, p store.ActionsPush) {
	rp, err := a.db.RepoByName(ctx, p.Owner, p.Repo)
	if err != nil {
		return
	}
	a.emit(ctx, Event{Kind: EventRepoPushed, RepoID: rp.ID, Repo: rp, URL: "/" + rp.FullName(),
		Actor: a.pusherActor(ctx, p.Pusher), Push: &EventPush{Ref: p.Ref, Old: p.Old, New: p.New, Pusher: p.Pusher}})
}

// handlePush starts push workflows and pull_request synchronize runs.
func (a *App) handlePush(ctx context.Context, p store.ActionsPush) {
	if actZeroSHA(p.New) || !(strings.HasPrefix(p.Ref, "refs/heads/") || strings.HasPrefix(p.Ref, "refs/tags/")) {
		return
	}
	rp, err := a.db.RepoByName(ctx, p.Owner, p.Repo)
	if err != nil {
		return
	}
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return
	}
	sha, err := g.ResolveCommit(ctx, p.New)
	if err != nil {
		return // a tag pointing at something that isn't a commit
	}
	who := a.pusherActor(ctx, p.Pusher)
	before := p.Old
	if actZeroSHA(before) && strings.HasPrefix(p.Ref, "refs/heads/") {
		// A new branch: compare with where it left the default branch.
		def := g.DefaultBranch(ctx)
		if "refs/heads/"+def != p.Ref {
			if defSHA, err := g.BranchSHA(ctx, def); err == nil {
				if mb, err := g.MergeBase(ctx, defSHA, sha); err == nil {
					before = mb
				}
			}
		}
	}
	changed := a.changedFiles(ctx, rp, before, sha)
	title := ""
	if c := g.LastCommit(ctx, sha, ""); c != nil {
		title = c.Subject
	}
	payload := map[string]any{
		"ref": p.Ref, "before": p.Old, "after": sha, "created": actZeroSHA(p.Old),
		"head_commit": map[string]any{"id": sha, "message": title},
		"pusher":      map[string]any{"name": who.Name},
		"repository":  map[string]any{"full_name": rp.FullName(), "name": rp.Name, "owner": map[string]any{"login": rp.Owner}},
	}
	a.triggerRuns(ctx, rp, g, actTrigger{Event: "push", Ref: p.Ref, SHA: sha, Before: p.Old, Actor: who,
		Payload: payload, Title: title, Changed: changed})

	// Pushing to a pull request's head branch synchronizes it.
	if branch, ok := strings.CutPrefix(p.Ref, "refs/heads/"); ok {
		pulls, err := a.db.Pulls(ctx, rp.ID, "open")
		if err != nil {
			return
		}
		for _, pr := range pulls {
			if pr.Head == branch {
				a.triggerPull(ctx, rp, g, pr, "synchronize", who)
			}
		}
	}
}

// pusherActor maps the hook's "kind:name" to an Actor.
func (a *App) pusherActor(ctx context.Context, pusher string) Actor {
	kind, name, _ := strings.Cut(pusher, ":")
	who := Actor{Kind: kind, Name: name}
	me, err := a.db.PersonalOwner(ctx)
	if err == nil {
		who.PersonID = me.ID
	}
	switch kind {
	case ActorToken, "bot", "agent":
	default: // owner sessions, SSH keys, and anything unknown act as the owner
		who.Kind, who.Name = ActorOwner, me.Name
	}
	if who.Name == "" {
		who.Name = "atelier"
	}
	return who
}

// triggerPull starts pull_request runs for one pull request.
func (a *App) triggerPull(ctx context.Context, rp store.Repo, g *gitrepo.Repo, pr store.Pull, action string, who Actor) {
	head, err := g.BranchSHA(ctx, pr.Head)
	if err != nil {
		return
	}
	var changed []string
	if base, err := g.BranchSHA(ctx, pr.Base); err == nil {
		if mb, err := g.MergeBase(ctx, base, head); err == nil {
			changed = a.changedFiles(ctx, rp, mb, head)
		}
	}
	approval := ""
	if pr.AuthorKind != ActorOwner && pr.AuthorKind != ActorToken {
		approval = fmt.Sprintf("Pull request #%d was opened by %s (%s), so its jobs wait for your approval.", pr.Number, pr.AuthorName, pr.AuthorKind)
	}
	payload := map[string]any{
		"action": action, "number": pr.Number,
		"pull_request": map[string]any{
			"number": pr.Number, "title": pr.Title, "body": pr.Body,
			"head": map[string]any{"ref": pr.Head, "sha": head},
			"base": map[string]any{"ref": pr.Base},
			"user": map[string]any{"login": pr.AuthorName, "type": pr.AuthorKind},
		},
		"repository": map[string]any{"full_name": rp.FullName(), "name": rp.Name, "owner": map[string]any{"login": rp.Owner}},
	}
	a.triggerRuns(ctx, rp, g, actTrigger{Event: "pull_request", Action: action, Ref: "refs/heads/" + pr.Head, SHA: head,
		Actor: who, Payload: payload, PullNumber: pr.Number, PullBase: pr.Base, PullHead: pr.Head, Approval: approval,
		Title: pr.Title, Changed: changed})
}

// triggerRuns starts every workflow at t.SHA that the trigger matches.
func (a *App) triggerRuns(ctx context.Context, rp store.Repo, g *gitrepo.Repo, t actTrigger) []store.ActionsRun {
	var started []store.ActionsRun
	for _, wf := range a.loadWorkflows(ctx, rp.ID, g, t.SHA) {
		if t.Workflow != "" && wf.Path != t.Workflow {
			continue
		}
		if !actionsMatches(wf, t) {
			continue
		}
		run, err := a.startRun(ctx, rp, g, wf, t)
		if err != nil {
			log.Printf("actions: starting %s in %s: %v", wf.Path, rp.FullName(), err)
			continue
		}
		started = append(started, run)
	}
	return started
}

func actionsMatches(wf *workflow.Workflow, t actTrigger) bool {
	if !wf.Valid() {
		// A broken workflow shows up as a failed run on pushes, so it isn't silently ignored.
		return t.Event == "push" && (wf.On.Push == nil || wf.On.Push.MatchPush(t.Ref, t.Changed))
	}
	switch t.Event {
	case "push":
		return wf.On.Push.MatchPush(t.Ref, t.Changed)
	case "pull_request":
		return wf.On.PullRequest.MatchPullRequest(t.Action, t.PullBase, t.Changed)
	case "workflow_dispatch":
		return wf.On.Dispatch != nil
	case "schedule":
		return len(wf.On.Schedule) > 0
	case "release":
		return wf.On.Release.MatchRelease(t.Action)
	}
	return false
}

// fireSchedules starts scheduled workflows on the default branch, once per minute.
func (a *App) fireSchedules(ctx context.Context, now time.Time) {
	st := a.actions()
	minute := now.Truncate(time.Minute)
	if !st.cronAt.IsZero() && !minute.After(st.cronAt) {
		return
	}
	first := st.cronAt.IsZero()
	st.cronAt = minute
	if first {
		return // don't fire for a minute that may already have passed before start
	}
	a.runSchedules(ctx, minute)
}

// runSchedules starts every workflow whose cron matches minute.
func (a *App) runSchedules(ctx context.Context, minute time.Time) {
	repos, err := a.db.Repos(ctx)
	if err != nil {
		return
	}
	for _, rp := range repos {
		g, err := a.git.Open(rp.Owner, rp.Name)
		if err != nil || g.IsEmpty(ctx) {
			continue
		}
		def := g.DefaultBranch(ctx)
		sha, err := g.BranchSHA(ctx, def)
		if err != nil {
			continue
		}
		for _, wf := range a.loadWorkflows(ctx, rp.ID, g, sha) {
			if !wf.Valid() {
				continue
			}
			for _, s := range wf.On.Schedule {
				if s.Cron.Match(minute) {
					t := actTrigger{Event: "schedule", Ref: "refs/heads/" + def, SHA: sha, Workflow: wf.Path,
						Actor:   Actor{Kind: ActorSystem, Name: "schedule"},
						Payload: map[string]any{"schedule": s.Spec}, Title: "Scheduled: " + s.Spec}
					if _, err := a.startRun(ctx, rp, g, wf, t); err != nil {
						log.Printf("actions: scheduled %s in %s: %v", wf.Path, rp.FullName(), err)
					}
					break
				}
			}
		}
	}
}

// actionsEventHandler reacts to pull requests and releases.
func actionsEventHandler(ctx context.Context, a *App, ev Event) {
	switch ev.Kind {
	case EventPullOpened, EventPullReopened, EventPullMerged:
	case EventReleasePublished:
	default:
		return
	}
	rp := ev.Repo
	if rp.ID == 0 {
		found, err := a.repoByIDForActions(ctx, ev.RepoID)
		if err != nil {
			return
		}
		rp = found
	}
	st := a.actions()
	st.go_(func() {
		ctx := context.WithoutCancel(ctx)
		g, err := a.git.Open(rp.Owner, rp.Name)
		if err != nil {
			return
		}
		switch ev.Kind {
		case EventPullOpened, EventPullReopened:
			pr, err := a.db.PullByNumber(ctx, rp.ID, ev.Number)
			if err != nil {
				return
			}
			action := "opened"
			if ev.Kind == EventPullReopened {
				action = "reopened"
			}
			a.triggerPull(ctx, rp, g, pr, action, ev.Actor)
		case EventPullMerged:
			// Atelier's merges move the base branch without running hooks: queue it as a push.
			pr, err := a.db.PullByNumber(ctx, rp.ID, ev.Number)
			if err != nil || pr.MergedSHA == "" {
				return
			}
			push := store.ActionsPush{Owner: rp.Owner, Repo: rp.Name, Old: pr.BaseSHA, New: pr.MergedSHA,
				Ref: "refs/heads/" + pr.Base, Pusher: ev.Actor.Kind + ":" + ev.Actor.Name}
			a.emitPushed(ctx, push)
			a.handlePush(ctx, push)
		case EventReleasePublished:
			a.triggerRelease(ctx, rp, g, ev)
		}
	})
}

func (a *App) repoByIDForActions(ctx context.Context, id int64) (store.Repo, error) {
	repos, err := a.db.Repos(ctx)
	if err != nil {
		return store.Repo{}, err
	}
	for _, r := range repos {
		if r.ID == id {
			return r, nil
		}
	}
	return store.Repo{}, store.ErrNotFound
}

func (a *App) triggerRelease(ctx context.Context, rp store.Repo, g *gitrepo.Repo, ev Event) {
	tag := ""
	if i := strings.LastIndex(ev.URL, "/releases/tag/"); i >= 0 {
		tag = ev.URL[i+len("/releases/tag/"):]
	}
	if tag == "" {
		tag = strings.TrimSpace(ev.Detail)
	}
	if tag == "" || !gitrepo.ValidBranchName(tag) {
		return
	}
	sha, err := g.ResolveCommit(ctx, "refs/tags/"+tag)
	if err != nil {
		return
	}
	payload := map[string]any{"action": "published", "release": map[string]any{"tag_name": tag, "name": ev.Title}}
	a.triggerRuns(ctx, rp, g, actTrigger{Event: "release", Action: "published", Ref: "refs/tags/" + tag, SHA: sha,
		Actor: ev.Actor, Payload: payload, Title: ev.Title})
}

// ---------- creating runs ----------

// actKnownContexts are the contexts the server resolves (see the contract).
func actKnownContexts(root string) bool {
	switch root {
	case "github", "atelier", "inputs", "vars", "matrix", "needs", "strategy":
		return true
	}
	return false
}

// githubContext is the github (and atelier) context for a run.
func (a *App) githubContext(rp store.Repo, run store.ActionsRun, jobKey string) map[string]any {
	refType, refName := "branch", run.Branch()
	if strings.HasPrefix(run.Ref, "refs/tags/") {
		refType = "tag"
	}
	base := a.actionsBaseURL()
	c := map[string]any{
		"event_name": run.Event, "event": run.EventPayload, "sha": run.SHA, "ref": run.Ref, "ref_name": refName,
		"ref_type": refType, "repository": rp.FullName(), "repository_owner": rp.Owner, "actor": run.ActorName,
		"triggering_actor": run.ActorName, "run_id": fmt.Sprint(run.ID), "run_number": fmt.Sprint(run.RunNumber),
		"run_attempt": fmt.Sprint(run.Attempt), "workflow": run.WorkflowName, "workflow_ref": rp.FullName() + "/" + run.WorkflowPath + "@" + run.Ref,
		"server_url": base, "api_url": base + "/api/v3", "job": jobKey, "head_ref": "", "base_ref": "",
	}
	if run.Event == "pull_request" {
		if pr, ok := run.EventPayload["pull_request"].(map[string]any); ok {
			if h, ok := pr["head"].(map[string]any); ok {
				c["head_ref"] = h["ref"]
			}
			if b, ok := pr["base"].(map[string]any); ok {
				c["base_ref"] = b["ref"]
			}
		}
	}
	if c["event"] == nil {
		c["event"] = map[string]any{}
	}
	return c
}

func actInputsContext(in map[string]string, wf *workflow.Workflow) map[string]any {
	out := map[string]any{}
	types := map[string]string{}
	if wf != nil && wf.On.Dispatch != nil {
		for _, i := range wf.On.Dispatch.Inputs {
			types[i.Name] = i.Type
		}
	}
	for k, v := range in {
		switch types[k] {
		case "boolean":
			out[k] = v == "true"
		case "number":
			out[k] = expr.ToNumber(v)
		default:
			out[k] = v
		}
	}
	return out
}

// startRun creates a run for wf and schedules its first jobs.
func (a *App) startRun(ctx context.Context, rp store.Repo, g *gitrepo.Repo, wf *workflow.Workflow, t actTrigger) (store.ActionsRun, error) {
	run := store.ActionsRun{
		RepoID: rp.ID, WorkflowPath: wf.Path, WorkflowName: wf.Name, Event: t.Event, Ref: t.Ref, SHA: t.SHA,
		Title: t.Title, ActorKind: t.Actor.Kind, ActorName: t.Actor.Name, PersonID: t.Actor.PersonID,
		Status: store.RunQueued, Inputs: t.Inputs, EventPayload: t.Payload, PullNumber: t.PullNumber,
		Warnings: append([]string(nil), wf.Warnings...), Errors: append([]string(nil), wf.Errors...),
	}
	if run.ActorName == "" {
		run.ActorName = "atelier"
	}
	if run.Title == "" {
		if c := g.LastCommit(ctx, t.SHA, ""); c != nil {
			run.Title = c.Subject
		}
	}
	if run.EventPayload == nil {
		run.EventPayload = map[string]any{}
	}
	if t.Event == "workflow_dispatch" {
		run.EventPayload["inputs"] = t.Inputs
	}

	// Pin every action to a commit.
	if wf.Valid() {
		pins := a.pinActions(ctx, rp, g, wf, t.SHA)
		run.Pins = pins.Pins
		run.LockResolved = pins.Resolved
		if len(pins.Resolved) > 0 {
			run.LockGenerated = string(pins.Generated.Marshal())
		}
		run.Errors = append(run.Errors, pins.Errors...)
	}

	// Concurrency group.
	if wf.Valid() && wf.Concurrency != nil {
		ev := &expr.Evaluator{Ctx: expr.Context{
			"github": a.githubContext(rp, run, ""), "inputs": actInputsContext(t.Inputs, wf),
		}, Known: func(r string) bool { return r == "github" || r == "inputs" || r == "atelier" }}
		ev.Ctx["atelier"] = ev.Ctx["github"]
		if grp, err := ev.EvalString(wf.Concurrency.Group); err == nil {
			run.ConcurrencyGroup = rp.FullName() + ":" + grp
			run.CancelInProgress = wf.Concurrency.CancelInProgress
		} else {
			run.Warnings = append(run.Warnings, "concurrency.group couldn't be resolved, so it was ignored: "+err.Error())
		}
	}

	var jobs []store.ActionsJob
	if len(run.Errors) == 0 {
		jobs = a.buildJobs(rp, run, wf, t)
		if wf.Agentic {
			a.agentGateJobs(ctx, rp, wf, t, jobs) // first run, changed tools and outside authors need approval
		}
	} else {
		run.Status, run.Conclusion = store.RunCompleted, actions.StatusFailure
		run.StartedAt, run.FinishedAt = time.Now(), time.Now()
	}

	st := a.actions()
	st.mu.Lock()
	defer st.mu.Unlock()
	if run.ConcurrencyGroup != "" && len(run.Errors) == 0 {
		others, err := a.db.ActionsRuns(ctx, store.ActionsRunFilter{RepoID: rp.ID, Group: run.ConcurrencyGroup, Status: "in_progress"})
		if err != nil {
			return run, err
		}
		for _, o := range others {
			switch {
			case run.CancelInProgress || o.Status == store.RunPending:
				// Newer runs replace older ones in the same group.
				a.cancelRunLocked(ctx, o, Actor{Kind: ActorSystem, Name: "concurrency"})
			default:
				run.Status = store.RunPending
			}
		}
	}
	run, _, err := a.db.CreateActionsRun(ctx, run, jobs)
	if err != nil {
		return run, err
	}
	if run.Done() {
		a.emitRunDone(ctx, rp, run)
		return run, nil
	}
	if run.Status != store.RunPending {
		a.advanceRunLocked(ctx, run.ID)
	}
	return a.db.ActionsRunByID(ctx, run.ID)
}

// pinActions pins the workflow's actions using the lockfile at sha.
func (a *App) pinActions(ctx context.Context, rp store.Repo, g *gitrepo.Repo, wf *workflow.Workflow, sha string) workflow.PinResult {
	uses := wf.Uses()
	var lock workflow.Lockfile
	var lockErr string
	if b, err := g.Blob(ctx, sha, workflow.LockfilePath, workflow.MaxLockfile); err == nil {
		if b.TooLarge {
			lockErr = workflow.LockfilePath + " is too large."
		} else if l, err := workflow.ParseLockfile(b.Data); err != nil {
			lockErr = workflow.LockfilePath + ": " + err.Error()
		} else {
			lock = l
		}
	}
	require, _ := a.db.ActionsRequireLockfile(ctx, rp.ID)
	st := a.actions()
	resolve := func(ctx context.Context, r workflow.ActionRef) (string, error) {
		if r.Kind == workflow.UsesAtelier {
			return a.resolveAtelierAction(ctx, rp, r)
		}
		return st.resolveRemote(ctx, r)
	}
	res := workflow.Pin(ctx, uses, lock, resolve, require)
	if lockErr != "" {
		res.Errors = append([]string{lockErr}, res.Errors...)
	}
	return res
}

// resolveAtelierAction resolves atelier:owner/repo@ref against this server.
// The runner fetches it from CloneURL's server with its job token, so only
// repositories that run can read are allowed.
func (a *App) resolveAtelierAction(ctx context.Context, from store.Repo, r workflow.ActionRef) (string, error) {
	rp, err := a.db.RepoByName(ctx, r.Owner, r.Repo)
	if err != nil {
		return "", fmt.Errorf("no repository %s/%s here", r.Owner, r.Repo)
	}
	if rp.Private && rp.ID != from.ID && rp.OwnerID != from.OwnerID {
		return "", fmt.Errorf("%s is private to another account", rp.FullName())
	}
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return "", err
	}
	for _, cand := range []string{"refs/tags/" + r.Ref, "refs/heads/" + r.Ref} {
		if sha, err := g.ResolveCommit(ctx, cand); err == nil {
			return sha, nil
		}
	}
	return "", fmt.Errorf("no tag or branch %q in %s", r.Ref, rp.FullName())
}

// actMatrix is a job's matrix combination as stored on the job.
type actMatrix struct {
	Keys   []string       `json:"keys"`
	Values map[string]any `json:"values"`
	Index  int            `json:"index"`
	Total  int            `json:"total"`
}

func (m actMatrix) json() string {
	b, _ := json.Marshal(m)
	return string(b)
}

func parseActMatrix(s string) (actMatrix, bool) {
	if s == "" || s == "?" {
		return actMatrix{}, false
	}
	var m actMatrix
	if json.Unmarshal([]byte(s), &m) != nil {
		return actMatrix{}, false
	}
	return m, true
}

// buildJobs makes the job rows for a new run: one per job, or one per matrix
// combination (a dynamic matrix gets a placeholder expanded when it's ready).
func (a *App) buildJobs(rp store.Repo, run store.ActionsRun, wf *workflow.Workflow, t actTrigger) []store.ActionsJob {
	var out []store.ActionsJob
	for _, j := range wf.Jobs {
		base := store.ActionsJob{JobKey: j.ID, Name: j.ID, Status: store.JobPending, Environment: j.Environment,
			TimeoutMinutes: j.TimeoutMinutes, ContinueOnError: j.ContinueOnError, Approval: t.Approval,
			PackagesWrite: j.Permissions.PackagesWrite(), NeedsDocker: j.NeedsDocker()}
		if j.Name != "" && !expr.HasExpr(j.Name) {
			base.Name = j.Name
		}
		if j.Unsupported != "" {
			base.Status, base.Error = actions.StatusFailure, j.Unsupported
			base.FinishedAt = time.Now()
			out = append(out, base)
			continue
		}
		if j.Strategy == nil || j.Strategy.Matrix == nil {
			out = append(out, base)
			continue
		}
		if j.Strategy.Matrix.Dynamic() {
			base.Matrix = "?"
			out = append(out, base)
			continue
		}
		combos, err := j.Strategy.Matrix.Expand(nil)
		if err != nil {
			base.Status, base.Error = actions.StatusFailure, "Matrix: "+err.Error()
			base.FinishedAt = time.Now()
			out = append(out, base)
			continue
		}
		for i, c := range combos {
			jb := base
			jb.Matrix = actMatrix{Keys: c.Keys, Values: c.Values, Index: i, Total: len(combos)}.json()
			jb.Name = actMatrixJobName(j, c)
			out = append(out, jb)
		}
	}
	return out
}

// actMatrixJobName is "id (a, b)" or the job's name with the matrix filled in.
func actMatrixJobName(j *workflow.Job, c workflow.Combo) string {
	if j.Name != "" {
		e := &expr.Evaluator{Ctx: expr.Context{"matrix": c.Map()}, Known: func(r string) bool { return r == "matrix" }}
		if n, err := e.Interpolate(j.Name); err == nil && !expr.HasExpr(n) {
			return n
		}
		if !expr.HasExpr(j.Name) {
			return j.Name + " (" + c.Label() + ")"
		}
	}
	return j.ID + " (" + c.Label() + ")"
}

// dispatchWorkflow starts a workflow_dispatch run (from the UI).
func (a *App) dispatchWorkflow(ctx context.Context, rp store.Repo, g *gitrepo.Repo, wfPath, branch string, inputs map[string]string, who Actor) (store.ActionsRun, error) {
	sha, err := g.BranchSHA(ctx, branch)
	if err != nil {
		return store.ActionsRun{}, fmt.Errorf("the branch %s doesn't exist", branch)
	}
	wf := actFindWorkflow(a.loadWorkflows(ctx, rp.ID, g, sha), wfPath)
	if wf == nil {
		return store.ActionsRun{}, fmt.Errorf("%s doesn't exist on %s", wfPath, branch)
	}
	if wf.Valid() && wf.On.Dispatch == nil {
		return store.ActionsRun{}, fmt.Errorf("%s on %s has no workflow_dispatch trigger", wf.Name, branch)
	}
	clean, err := actCheckInputs(wf, inputs)
	if err != nil {
		return store.ActionsRun{}, err
	}
	t := actTrigger{Event: "workflow_dispatch", Ref: "refs/heads/" + branch, SHA: sha, Actor: who, Inputs: clean,
		Payload: map[string]any{"ref": "refs/heads/" + branch}, Workflow: wf.Path, Title: "Manually run by " + who.Name}
	return a.startRun(ctx, rp, g, wf, t)
}

// actCheckInputs validates inputs against the workflow's declarations.
func actCheckInputs(wf *workflow.Workflow, in map[string]string) (map[string]string, error) {
	out := map[string]string{}
	if wf.On.Dispatch == nil {
		return out, nil
	}
	for _, d := range wf.On.Dispatch.Inputs {
		v, ok := in[d.Name]
		if !ok || v == "" {
			v = d.Default
		}
		if len(v) > 4096 {
			return nil, fmt.Errorf("%s is too long", d.Name)
		}
		switch d.Type {
		case "boolean":
			if v == "" {
				v = "false"
			}
			if v != "true" && v != "false" {
				return nil, fmt.Errorf("%s must be true or false", d.Name)
			}
		case "choice":
			found := false
			for _, o := range d.Options {
				found = found || o == v
			}
			if !found && (v != "" || d.Required) {
				return nil, fmt.Errorf("%s must be one of %s", d.Name, strings.Join(d.Options, ", "))
			}
		case "number":
			if v != "" && actIsNaN(expr.ToNumber(v)) {
				return nil, fmt.Errorf("%s must be a number", d.Name)
			}
		}
		if d.Required && v == "" {
			return nil, fmt.Errorf("%s is required", d.Name)
		}
		out[d.Name] = v
	}
	return out, nil
}

func actIsNaN(f float64) bool { return f != f }

// actionsLogLine builds one system log line.
func actionsSystemLines(texts ...string) string {
	lines := make([]actions.LogLine, len(texts))
	for i, t := range texts {
		lines[i] = actions.LogLine{Time: time.Now().UTC(), Step: -1, Stream: "system", Text: t}
	}
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(lines)
	return strings.TrimSpace(b.String())
}
