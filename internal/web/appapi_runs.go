package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"atelier/internal/actions"
	"atelier/internal/store"
	"atelier/internal/workflow"
)

// Runs, logs and agent approvals for the app API: the Actions pages' data as JSON.

const (
	appRunsDefault = 20
	appRunsMax     = 100
	appLogTail     = 200
	appLogTailMax  = 2000
)

func appJobKind(j store.ActionsJob, run store.ActionsRun) string {
	if j.JobKey == workflow.AgentJobID && workflow.IsAgenticFile(run.WorkflowPath) {
		return "agent"
	}
	return "job"
}

func (a *App) appRuns(w http.ResponseWriter, r *http.Request, c *appCaller) {
	ctx := r.Context()
	q := r.URL.Query()
	limit := appRunsDefault
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = min(n, appRunsMax)
	}
	_, readable, err := a.appReadable(ctx, c.Who)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	f := store.ActionsRunFilter{Limit: 400}
	if name := q.Get("repo"); name != "" {
		owner, repo, _ := strings.Cut(name, "/")
		rp, ok := a.appRepoNamed(w, r, c, owner, repo, PermRead)
		if !ok {
			return
		}
		f.RepoID = rp.ID
	}
	runner := strings.TrimSpace(q.Get("runner"))
	runs, err := a.db.ActionsRuns(ctx, f)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, run := range runs {
		if len(out) >= limit {
			break
		}
		rp, ok := readable[run.RepoID]
		if !ok {
			continue
		}
		jobs, err := a.db.ActionsJobs(ctx, run.ID)
		if err != nil {
			continue
		}
		onRunner := runner == ""
		js := []map[string]any{}
		for _, j := range jobs {
			onRunner = onRunner || strings.EqualFold(j.RunnerName, runner)
			js = append(js, map[string]any{"job_id": j.ID, "name": j.Name, "status": j.Status, "runner": appStrOrNil(j.RunnerName),
				"duration_s": int(j.Duration().Seconds()), "kind": appJobKind(j, run)})
		}
		if !onRunner {
			continue
		}
		out = append(out, map[string]any{
			"run_id": run.ID, "number": run.RunNumber, "repo": rp.FullName(), "workflow": run.WorkflowName, "event": run.Event,
			"branch": run.Branch(), "status": run.Outcome(), "started_at": appTime(run.StartedAt),
			"duration_s": int(run.Duration().Seconds()), "web_path": actionsRunURL(rp, run), "jobs": js,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// appLoadJob loads the job in the path with its run and repository, checking need.
func (a *App) appLoadJob(w http.ResponseWriter, r *http.Request, c *appCaller, need Perm) (store.ActionsJob, store.ActionsRun, store.Repo, bool) {
	ctx := r.Context()
	missing := func() { appError(w, http.StatusNotFound, "not_found", "There's no such job, or you can't see it.") }
	id, ok := appPathID(r, "id")
	if !ok {
		missing()
		return store.ActionsJob{}, store.ActionsRun{}, store.Repo{}, false
	}
	j, err := a.db.ActionsJob(ctx, id)
	if err != nil {
		missing()
		return j, store.ActionsRun{}, store.Repo{}, false
	}
	run, err := a.db.ActionsRunByID(ctx, j.RunID)
	if err != nil {
		missing()
		return j, run, store.Repo{}, false
	}
	rp, err := a.repoForRun(ctx, run)
	if err != nil || !a.can(ctx, c.Who, rp, PermRead) {
		missing()
		return j, run, rp, false
	}
	if need != PermRead && !a.can(ctx, c.Who, rp, need) {
		appError(w, http.StatusForbidden, "forbidden", "You don't have "+need.String()+" access to this repository.")
		return j, run, rp, false
	}
	return j, run, rp, true
}

// appJobLog returns the end of a job's log, as stored (the same masked text
// the run page shows).
func (a *App) appJobLog(w http.ResponseWriter, r *http.Request, c *appCaller) {
	j, _, _, ok := a.appLoadJob(w, r, c, PermRead)
	if !ok {
		return
	}
	tail := appLogTail
	if n, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && n > 0 {
		tail = min(n, appLogTailMax)
	}
	chunks, err := a.db.ActionsLogs(r.Context(), j.ID, 0, 0)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	lines := []string{}
	total := 0
	for _, ch := range chunks {
		var ls []actions.LogLine
		if json.Unmarshal([]byte(ch.Lines), &ls) != nil {
			continue
		}
		for _, l := range ls {
			total++
			lines = append(lines, l.Text)
		}
		if len(lines) > 2*appLogTailMax {
			lines = append([]string{}, lines[len(lines)-appLogTailMax:]...)
		}
	}
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "truncated": total > len(lines), "status": j.Status})
}

func (a *App) appJobCancel(w http.ResponseWriter, r *http.Request, c *appCaller) {
	j, _, rp, ok := a.appLoadJob(w, r, c, PermWrite)
	if !ok {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	st := a.actions()
	st.mu.Lock()
	fresh, err := a.db.ActionsJob(ctx, j.ID)
	if err != nil || fresh.Finished() {
		st.mu.Unlock()
		appError(w, http.StatusConflict, "finished", "That job has already finished.")
		return
	}
	a.cancelJobLocked(ctx, fresh, "Cancelled by "+c.Who.Name+".")
	st.signal()
	a.advanceRunLocked(ctx, fresh.RunID)
	a.updateRunStatusLocked(ctx, rp, fresh.RunID)
	st.mu.Unlock()
	a.app().hub.statusChanged(ctx, a, rp, c.Who, "running", "needs")
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// ---------- agents ----------

// appAgentReason shortens the approval sentence to a few words.
func appAgentReason(approval string) string {
	switch {
	case strings.Contains(approval, "first run"):
		return "first run"
	case strings.Contains(approval, "tools changed"):
		return "tools changed"
	case strings.Contains(approval, "not you"):
		return "outside author"
	case strings.Contains(approval, "environment"):
		return "environment"
	}
	return "needs approval"
}

// appAgentInfo describes a waiting or running agent job from its workflow.
type appAgentInfo struct {
	Workflow       string
	Engine         string
	Tools, Writes  []string
	MaxTurns       int
	TimeoutMinutes int
}

func (a *App) appAgentInfo(ctx context.Context, rp store.Repo, run store.ActionsRun) appAgentInfo {
	info := appAgentInfo{Workflow: run.WorkflowName, Engine: workflow.AgentEngine, Tools: []string{}, Writes: []string{}}
	spec := workflow.AgentSpecOf(a.runWorkflow(ctx, rp, run))
	if spec == nil {
		return info
	}
	info.Engine, info.MaxTurns, info.TimeoutMinutes = spec.Engine, spec.MaxTurns, spec.TimeoutMinutes
	for _, t := range spec.Tools {
		if !workflow.AgentToolAllowed(t) {
			continue
		}
		info.Tools = append(info.Tools, t)
		if workflow.AgentToolWrites(t) {
			info.Writes = append(info.Writes, t)
		}
	}
	return info
}

type appWaitingAgent struct {
	Job  store.ActionsJob
	Run  store.ActionsRun
	Repo store.Repo
	Info appAgentInfo
}

// appWaitingAgents lists the agent jobs waiting for approval in
// repositories who may read, newest first.
func (a *App) appWaitingAgents(ctx context.Context, readable map[int64]store.Repo) []appWaitingAgent {
	jobs, err := a.db.ActionsJobsWithStatus(ctx, actions.StatusWaiting)
	if err != nil {
		return nil
	}
	var out []appWaitingAgent
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		rp, ok := readable[j.RepoID]
		if !ok {
			continue
		}
		run, err := a.db.ActionsRunByID(ctx, j.RunID)
		if err != nil || appJobKind(j, run) != "agent" {
			continue
		}
		out = append(out, appWaitingAgent{Job: j, Run: run, Repo: rp, Info: a.appAgentInfo(ctx, rp, run)})
	}
	return out
}

func (a *App) appAgentsWaiting(w http.ResponseWriter, r *http.Request, c *appCaller) {
	ctx := r.Context()
	_, readable, err := a.appReadable(ctx, c.Who)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, wa := range a.appWaitingAgents(ctx, readable) {
		out = append(out, map[string]any{
			"job_id": wa.Job.ID, "run_id": wa.Run.ID, "repo": wa.Repo.FullName(), "workflow": wa.Info.Workflow,
			"reason": appAgentReason(wa.Job.Approval), "detail": wa.Job.Approval, "engine": wa.Info.Engine,
			"tools": wa.Info.Tools, "write_tools": wa.Info.Writes, "max_turns": wa.Info.MaxTurns,
			"timeout_minutes": wa.Info.TimeoutMinutes, "web_path": actionsRunURL(wa.Repo, wa.Run),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// appAgentApprove is the run page's Approve button for one agent job,
// recorded as approved by the caller ("device:MacBook").
func (a *App) appAgentApprove(w http.ResponseWriter, r *http.Request, c *appCaller) {
	j, run, rp, ok := a.appLoadJob(w, r, c, PermWrite)
	if !ok {
		return
	}
	if appJobKind(j, run) != "agent" {
		appError(w, http.StatusNotFound, "not_found", "That job isn't an agent.")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if a.approveRun(ctx, run, j.ID, c.Who) == 0 {
		appError(w, http.StatusConflict, "not_waiting", "That agent isn't waiting for approval.")
		return
	}
	a.app().hub.statusChanged(ctx, a, rp, c.Who, "needs", "running")
	a.autoMergeOnEvent(ctx, Event{Kind: "job.approved", RepoID: rp.ID, Repo: rp, Actor: c.Who})
	writeJSON(w, http.StatusOK, map[string]any{"approved": true})
}
