package web

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
	"atelier/internal/workflow"
	"atelier/internal/workflow/expr"
)

// The scheduler moves jobs through their lifecycle:
//
//	pending → (needs finished, if: true) → queued | waiting (approval) → running → success | failure | cancelled
//	pending → (if: false, or a needed job failed)                    → skipped
//
// Everything here runs with actionsState.mu held (the *Locked functions).

// actNeed is a finished job as the needs context sees it.
type actNeed struct {
	Result  string
	Outputs map[string]string
}

// actNeedsOf works out whether the jobs `keys` have finished and their results.
func actNeedsOf(jobs []store.ActionsJob, keys []string) (bool, map[string]actNeed) {
	out := map[string]actNeed{}
	for _, k := range keys {
		n := actNeed{Outputs: map[string]string{}}
		rows, failed, cancelled, skipped := 0, false, false, 0
		for _, j := range jobs {
			if j.JobKey != k {
				continue
			}
			rows++
			if !j.Finished() {
				return false, nil
			}
			switch j.Status {
			case actions.StatusFailure:
				if !j.ContinueOnError {
					failed = true
				}
			case actions.StatusCancelled:
				cancelled = true
			case actions.StatusSkipped:
				skipped++
			}
			for ok, ov := range j.Outputs {
				n.Outputs[ok] = ov
			}
		}
		switch {
		case failed:
			n.Result = actions.StatusFailure
		case cancelled:
			n.Result = actions.StatusCancelled
		case rows > 0 && skipped == rows:
			n.Result = actions.StatusSkipped
		default:
			n.Result = actions.StatusSuccess
		}
		out[k] = n
	}
	return true, out
}

func actNeedsContext(n map[string]actNeed) map[string]any {
	out := map[string]any{}
	for k, v := range n {
		outs := map[string]any{}
		for ok, ov := range v.Outputs {
			outs[ok] = ov
		}
		out[k] = map[string]any{"result": v.Result, "outputs": outs}
	}
	return out
}

// runWorkflow loads the workflow a run was made from.
func (a *App) runWorkflow(ctx context.Context, rp store.Repo, run store.ActionsRun) *workflow.Workflow {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return nil
	}
	return actFindWorkflow(a.loadWorkflows(ctx, rp.ID, g, run.SHA), run.WorkflowPath)
}

func (a *App) repoForRun(ctx context.Context, run store.ActionsRun) (store.Repo, error) {
	return a.repoByIDForActions(ctx, run.RepoID)
}

// advanceRun re-evaluates a run after something changed.
func (a *App) advanceRun(ctx context.Context, runID int64) {
	st := a.actions()
	st.mu.Lock()
	defer st.mu.Unlock()
	a.advanceRunLocked(ctx, runID)
}

func (a *App) advanceRunLocked(ctx context.Context, runID int64) {
	run, err := a.db.ActionsRunByID(ctx, runID)
	if err != nil || run.Done() || run.Status == store.RunPending {
		return
	}
	rp, err := a.repoForRun(ctx, run)
	if err != nil {
		return
	}
	wf := a.runWorkflow(ctx, rp, run)
	cancelled := run.Conclusion == actions.StatusCancelled
	queued := false

	for pass := 0; pass < 300; pass++ {
		jobs, err := a.db.ActionsJobs(ctx, run.ID)
		if err != nil {
			log.Printf("actions: run %d jobs: %v", run.ID, err)
			return
		}
		changed := false
		for i := range jobs {
			j := &jobs[i]
			if j.Status != store.JobPending {
				continue
			}
			if cancelled || wf == nil {
				j.Status, j.FinishedAt = actions.StatusCancelled, time.Now()
				if wf == nil {
					j.Status, j.Error = actions.StatusFailure, "The workflow file is gone from this commit."
				}
				_ = a.db.SaveActionsJob(ctx, *j)
				changed = true
				continue
			}
			wj := wf.Job(j.JobKey)
			if wj == nil {
				j.Status, j.Error, j.FinishedAt = actions.StatusFailure, "The workflow no longer has this job.", time.Now()
				_ = a.db.SaveActionsJob(ctx, *j)
				changed = true
				continue
			}
			done, needs := actNeedsOf(jobs, wj.Needs)
			if !done {
				continue
			}
			if a.throttled(jobs, *j, wj) {
				continue
			}
			res := a.readyJobLocked(ctx, rp, run, wf, wj, j, needs)
			if res != "" {
				changed = true
				queued = queued || res == actions.StatusQueued
			}
			if res == "expanded" {
				break // new rows were added: reload
			}
		}
		if !changed {
			break
		}
	}
	if queued {
		a.actions().signal()
	}
	a.updateRunStatusLocked(ctx, rp, run.ID)
}

// throttled: strategy.max-parallel limits how many matrix jobs run at once.
func (a *App) throttled(jobs []store.ActionsJob, j store.ActionsJob, wj *workflow.Job) bool {
	if wj.Strategy == nil || wj.Strategy.MaxParallel <= 0 || j.Matrix == "" || j.Matrix == "?" {
		return false
	}
	active := 0
	for _, o := range jobs {
		if o.JobKey == j.JobKey && (o.Status == actions.StatusQueued || o.Status == actions.StatusRunning || o.Status == actions.StatusWaiting) {
			active++
		}
	}
	return active >= wj.Strategy.MaxParallel
}

// jobEvaluator builds the expression context for one job.
func (a *App) jobEvaluator(ctx context.Context, rp store.Repo, run store.ActionsRun, wf *workflow.Workflow, j *store.ActionsJob, needs map[string]actNeed) *expr.Evaluator {
	gh := a.githubContext(rp, run, j.JobKey)
	vars, err := actions.Secrets().Variables(ctx, rp.ID, j.Environment)
	if err != nil {
		vars = map[string]string{}
	}
	varsCtx := map[string]any{}
	for k, v := range vars {
		varsCtx[k] = v
	}
	matrix := map[string]any{}
	strategy := map[string]any{"job-index": 0, "job-total": 1, "fail-fast": true, "max-parallel": 1}
	if m, ok := parseActMatrix(j.Matrix); ok {
		matrix = m.Values
		strategy["job-index"], strategy["job-total"], strategy["max-parallel"] = m.Index, m.Total, m.Total
	}
	if wj := wf.Job(j.JobKey); wj != nil && wj.Strategy != nil {
		strategy["fail-fast"] = wj.Strategy.FailFast
		if wj.Strategy.MaxParallel > 0 {
			strategy["max-parallel"] = wj.Strategy.MaxParallel
		}
	}
	return &expr.Evaluator{Ctx: expr.Context{
		"github": gh, "atelier": gh, "inputs": actInputsContext(run.Inputs, wf), "vars": varsCtx,
		"needs": actNeedsContext(needs), "matrix": matrix, "strategy": strategy,
	}, Known: actKnownContexts}
}

// readyJobLocked decides a pending job whose needs have finished. It returns
// the new status ("" if it stays pending, "expanded" if a matrix was expanded).
func (a *App) readyJobLocked(ctx context.Context, rp store.Repo, run store.ActionsRun, wf *workflow.Workflow, wj *workflow.Job, j *store.ActionsJob, needs map[string]actNeed) string {
	fail := func(msg string) string {
		j.Status, j.Error, j.FinishedAt = actions.StatusFailure, msg, time.Now()
		_ = a.db.SaveActionsJob(ctx, *j)
		return j.Status
	}
	ev := a.jobEvaluator(ctx, rp, run, wf, j, needs)

	// A dynamic matrix is expanded once its needs are done.
	if j.Matrix == "?" {
		combos, err := wj.Strategy.Matrix.Expand(ev)
		if err != nil {
			return fail("Matrix: " + err.Error())
		}
		var extra []store.ActionsJob
		for i, c := range combos {
			jb := *j
			jb.Matrix = actMatrix{Keys: c.Keys, Values: c.Values, Index: i, Total: len(combos)}.json()
			jb.Name = actMatrixJobName(wj, c)
			if i == 0 {
				*j = jb
				continue
			}
			jb.ID = 0
			extra = append(extra, jb)
		}
		if err := a.db.SaveActionsJob(ctx, *j); err != nil {
			return ""
		}
		if len(extra) > 0 {
			if _, err := a.db.AddActionsJobs(ctx, run.ID, rp.ID, extra); err != nil {
				log.Printf("actions: expanding matrix: %v", err)
			}
		}
		return "expanded"
	}

	// The job's if: (an implicit success() unless it uses a status function).
	cond := strings.TrimSpace(expr.Strip(wj.If))
	if cond == "" {
		cond = "success()"
	} else if !expr.UsesStatusFunc(cond) {
		cond = "success() && (" + cond + ")"
	}
	status := &expr.Status{Success: true, Cancelled: run.Conclusion == actions.StatusCancelled}
	for _, n := range needs {
		if n.Result != actions.StatusSuccess {
			status.Success = false
		}
		if n.Result == actions.StatusFailure {
			status.Failure = true
		}
	}
	ev.Status = status
	var ok bool
	var err error
	if expr.HasExpr(cond) { // "${{ a }} == b" style: interpolate, then test the text
		var s string
		s, err = ev.EvalString(cond)
		ok = err == nil && s != "" && s != "false" && s != "0"
	} else {
		ok, err = ev.EvalBool(cond)
	}
	ev.Status = nil
	if err != nil {
		return fail("if: " + err.Error())
	}
	if !ok {
		j.Status, j.FinishedAt = actions.StatusSkipped, time.Now()
		_ = a.db.SaveActionsJob(ctx, *j)
		return j.Status
	}

	// Name with expressions (needs, matrix, inputs…).
	if wj.Name != "" && expr.HasExpr(wj.Name) {
		if n, err := ev.Interpolate(wj.Name); err == nil && !expr.HasExpr(n) {
			j.Name = n
		}
	}

	// runs-on, environment and its gate.
	labels := []string{}
	for _, l := range wj.RunsOn {
		v, err := ev.EvalString(l)
		if err != nil {
			return fail("runs-on: " + err.Error())
		}
		if v = strings.TrimSpace(v); v != "" {
			labels = append(labels, v)
		}
	}
	if len(labels) == 0 {
		return fail("runs-on resolved to no labels.")
	}
	if wj.Environment != "" {
		env, err := ev.EvalString(wj.Environment)
		if err != nil {
			return fail("environment: " + err.Error())
		}
		j.Environment = env
	}
	if j.Environment != "" {
		gate, gateLabels, err := actions.Secrets().EnvironmentGate(ctx, rp.ID, j.Environment)
		if err != nil {
			return fail("environment " + j.Environment + ": " + err.Error())
		}
		labels = append(labels, gateLabels...)
		if gate {
			reason := "The " + j.Environment + " environment needs your approval before this job runs."
			if j.Approval != reason {
				j.Approval, j.ApprovedBy = reason, ""
			}
		}
	}
	j.RunsOn = actDedupeLabels(labels)

	payload, secretNames, err := a.buildPayload(rp, run, wf, wj, j, ev)
	if err != nil {
		return fail(err.Error())
	}
	j.Payload, j.SecretNames = payload, secretNames
	var stepNames []string
	if pj, err := actDecodeJob(payload); err == nil {
		for _, s := range pj.Steps {
			stepNames = append(stepNames, s.Name)
		}
	}
	if err := a.db.ResetActionsSteps(ctx, j.ID, stepNames); err != nil {
		return ""
	}
	if j.Approval != "" && j.ApprovedBy == "" {
		j.Status = actions.StatusWaiting
		if err := a.db.SaveActionsJob(ctx, *j); err != nil {
			return ""
		}
		a.emit(ctx, Event{Kind: EventJobWaiting, Repo: rp, RepoID: rp.ID, Number: run.Seq,
			Title: j.Name + " is waiting for approval", URL: actionsRunURL(rp, run), Detail: j.Approval,
			Actor: Actor{Kind: ActorSystem, Name: "actions"}})
		return j.Status
	}
	j.Status, j.QueuedAt = actions.StatusQueued, time.Now()
	if err := a.db.SaveActionsJob(ctx, *j); err != nil {
		return ""
	}
	return j.Status
}

func actDedupeLabels(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range in {
		k := strings.ToLower(l)
		if !seen[k] {
			seen[k] = true
			out = append(out, l)
		}
	}
	return out
}

func actionsRunURL(rp store.Repo, run store.ActionsRun) string {
	return fmt.Sprintf("/%s/actions/runs/%d", rp.FullName(), run.Seq)
}

var actSecretRefRe = regexp.MustCompile(`(?i)secrets\s*(?:\.\s*([A-Za-z_][A-Za-z0-9_]*)|\[\s*'([^']{1,100})'\s*\])`)

// buildPayload resolves what the server can of a job and returns it as JSON
// (an actions.Job without secrets, token or clone URL) plus the secret names
// it refers to.
func (a *App) buildPayload(rp store.Repo, run store.ActionsRun, wf *workflow.Workflow, wj *workflow.Job, j *store.ActionsJob, ev *expr.Evaluator) (string, []string, error) {
	interp := func(where, s string) (string, error) {
		out, err := ev.Interpolate(s)
		if err != nil {
			return "", fmt.Errorf("%s: %v", where, err)
		}
		return out, nil
	}
	interpMap := func(where string, m map[string]string) (map[string]string, error) {
		if len(m) == 0 {
			return nil, nil
		}
		out := make(map[string]string, len(m))
		for k, v := range m {
			r, err := interp(where+"."+k, v)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	}
	pj := actions.Job{
		ID: j.ID, RunID: run.ID, Name: j.Name, Workflow: wf.Path, Repo: actions.RepoRef{Owner: rp.Owner, Name: rp.Name},
		Event: run.Event, Ref: run.Ref, SHA: run.SHA, RunsOn: j.RunsOn, TimeoutMinutes: j.TimeoutMinutes,
		Context: a.githubContext(rp, run, j.JobKey),
	}
	// The runner also reads these contexts for step if: and expressions.
	for _, k := range []string{"matrix", "needs", "vars", "strategy", "inputs"} {
		if v, ok := ev.Ctx[k]; ok {
			pj.Context[k] = v
		}
	}
	env := map[string]string{}
	for k, v := range wf.Env {
		env[k] = v
	}
	for k, v := range wj.Env {
		env[k] = v
	}
	var err error
	if pj.Env, err = interpMap("env", env); err != nil {
		return "", nil, err
	}
	if wj.Container != nil {
		img, err := interp("container.image", wj.Container.Image)
		if err != nil {
			return "", nil, err
		}
		cenv, err := interpMap("container.env", wj.Container.Env)
		if err != nil {
			return "", nil, err
		}
		pj.Container = &actions.Container{Image: img, Env: cenv}
	}
	if pj.Outputs, err = interpMap("outputs", wj.Outputs); err != nil {
		return "", nil, err
	}
	for i, s := range wj.Steps {
		where := fmt.Sprintf("steps[%d]", i)
		ps := actions.Step{Index: i, ID: s.ID, Shell: s.Shell, ContinueOnError: s.ContinueOnError, TimeoutMinutes: s.TimeoutMinutes}
		name := s.Name
		if name == "" {
			if s.Uses != "" {
				name = "Run " + s.Uses
			} else {
				first, _, _ := strings.Cut(strings.TrimSpace(s.Run), "\n")
				name = "Run " + first
			}
		}
		if ps.Name, err = interp(where+".name", name); err != nil {
			return "", nil, err
		}
		if len(ps.Name) > 200 {
			ps.Name = ps.Name[:200] + "…"
		}
		if c := strings.TrimSpace(expr.Strip(s.If)); c != "" {
			if expr.HasExpr(c) {
				if ps.If, err = interp(where+".if", c); err != nil {
					return "", nil, err
				}
			} else {
				out, _, _, err := ev.Reduce(c)
				if err != nil {
					return "", nil, fmt.Errorf("%s.if: %v", where, err)
				}
				ps.If = out
			}
		}
		if ps.Run, err = interp(where+".run", s.Run); err != nil {
			return "", nil, err
		}
		if s.Uses != "" {
			pinned, ok := run.Pins[s.Uses]
			if !ok {
				return "", nil, fmt.Errorf("%s: %s wasn't pinned to a commit", where, s.Uses)
			}
			ps.Uses = pinned
			if r, err := workflow.ParseUses(s.Uses); err == nil && r.NeedsPin() {
				ps.UsesTag = r.Ref
			}
		}
		if ps.With, err = interpMap(where+".with", s.With); err != nil {
			return "", nil, err
		}
		if ps.Env, err = interpMap(where+".env", s.Env); err != nil {
			return "", nil, err
		}
		if ps.WorkingDirectory, err = interp(where+".working-directory", s.WorkingDirectory); err != nil {
			return "", nil, err
		}
		pj.Steps = append(pj.Steps, ps)
	}
	b, err := actJSONMarshal(pj)
	if err != nil {
		return "", nil, err
	}
	// Every secret the job mentions (resolved when a runner takes the job).
	seen := map[string]bool{}
	var names []string
	for _, m := range actSecretRefRe.FindAllStringSubmatch(string(b), -1) {
		n := m[1]
		if n == "" {
			n = m[2]
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return string(b), names, nil
}

// updateRunStatusLocked derives the run's status from its jobs and finishes
// it when every job has.
func (a *App) updateRunStatusLocked(ctx context.Context, rp store.Repo, runID int64) {
	run, err := a.db.ActionsRunByID(ctx, runID)
	if err != nil || run.Done() {
		return
	}
	jobs, err := a.db.ActionsJobs(ctx, runID)
	if err != nil {
		return
	}
	all, running, waiting, active := true, false, false, false
	failed, cancelledJob := false, false
	for _, j := range jobs {
		if !j.Finished() {
			all = false
		}
		switch j.Status {
		case actions.StatusRunning:
			running = true
		case actions.StatusWaiting:
			waiting = true
		case actions.StatusFailure:
			active = true
			if !j.ContinueOnError {
				failed = true
			}
		case actions.StatusCancelled:
			active, cancelledJob = true, true
		case actions.StatusSuccess, actions.StatusSkipped:
			active = true
		}
	}
	prev := run.Status
	switch {
	case all:
		run.Status = store.RunCompleted
		switch {
		case run.Conclusion == actions.StatusCancelled:
		case failed:
			run.Conclusion = actions.StatusFailure
		case cancelledJob:
			run.Conclusion = actions.StatusCancelled
		default:
			run.Conclusion = actions.StatusSuccess
		}
		run.FinishedAt = time.Now()
	case running:
		run.Status = store.RunRunning
	case waiting:
		run.Status = store.RunWaiting
	case active:
		run.Status = store.RunRunning
	default:
		run.Status = store.RunQueued
	}
	startedNow := false
	if run.StartedAt.IsZero() && run.Status != store.RunQueued {
		run.StartedAt, startedNow = time.Now(), true
	}
	if run.Status == prev && !startedNow {
		return
	}
	if err := a.db.SaveActionsRun(ctx, run); err != nil {
		return
	}
	a.actions().signal()
	if run.Done() {
		a.emitRunDone(ctx, rp, run)
		a.startNextInGroupLocked(ctx, run)
	}
}

// emitRunDone tells notifications about a finished run.
func (a *App) emitRunDone(ctx context.Context, rp store.Repo, run store.ActionsRun) {
	kind := EventRunCompleted
	if run.Conclusion == actions.StatusFailure {
		kind = EventRunFailed
	}
	detail := run.Conclusion
	if b := run.Branch(); b != "" {
		detail += " on " + b
	}
	if len(run.Errors) > 0 {
		detail += ": " + run.Errors[0]
	}
	a.emit(ctx, Event{Kind: kind, Repo: rp, RepoID: rp.ID, Number: run.Seq,
		Title: fmt.Sprintf("%s #%d %s", run.WorkflowName, run.RunNumber, actOutcomeWord(run.Conclusion)),
		URL:   actionsRunURL(rp, run), Detail: detail, Actor: Actor{Kind: ActorSystem, Name: "actions"}})
}

func actOutcomeWord(c string) string {
	switch c {
	case actions.StatusSuccess:
		return "succeeded"
	case actions.StatusFailure:
		return "failed"
	case actions.StatusCancelled:
		return "was cancelled"
	}
	return c
}

// startNextInGroupLocked starts the oldest run held back by a concurrency group.
func (a *App) startNextInGroupLocked(ctx context.Context, done store.ActionsRun) {
	if done.ConcurrencyGroup == "" {
		return
	}
	runs, err := a.db.ActionsRuns(ctx, store.ActionsRunFilter{RepoID: done.RepoID, Group: done.ConcurrencyGroup, Status: "in_progress"})
	if err != nil || len(runs) == 0 {
		return
	}
	for _, r := range runs {
		if r.Status != store.RunPending {
			return // something in the group is still going
		}
	}
	next := runs[len(runs)-1] // ActionsRuns is newest first
	next.Status = store.RunQueued
	if err := a.db.SaveActionsRun(ctx, next); err == nil {
		a.advanceRunLocked(ctx, next.ID)
	}
}

// ---------- finishing, cancelling, approving, re-running ----------

// finishJobLocked records a job's result and moves the run along.
func (a *App) finishJobLocked(ctx context.Context, j store.ActionsJob, status string, outputs map[string]string, errMsg string) {
	if j.Finished() {
		return
	}
	switch status {
	case actions.StatusSuccess, actions.StatusFailure, actions.StatusCancelled:
	default:
		status = actions.StatusFailure
	}
	if j.CancelRequested && status != actions.StatusSuccess {
		status = actions.StatusCancelled
	}
	j.Status, j.FinishedAt = status, time.Now()
	if j.StartedAt.IsZero() {
		j.StartedAt = j.FinishedAt
	}
	if outputs != nil {
		j.Outputs = outputs
	}
	if errMsg != "" {
		j.Error = errMsg
	}
	if err := a.db.SaveActionsJob(ctx, j); err != nil {
		log.Printf("actions: finishing job %d: %v", j.ID, err)
		return
	}
	stepStatus := actions.StatusSkipped
	if status == actions.StatusCancelled {
		stepStatus = actions.StatusCancelled
	}
	_ = a.db.MarkActionsStepsUnfinished(ctx, j.ID, stepStatus)
	_ = a.db.DeleteActionsJobToken(ctx, j.ID)
	if j.RunnerID != 0 {
		_ = a.db.SetActionsRunnerBusy(ctx, j.RunnerID, false)
	}
	// fail-fast: a failed matrix job cancels its siblings.
	if status == actions.StatusFailure && !j.ContinueOnError && j.Matrix != "" {
		if run, err := a.db.ActionsRunByID(ctx, j.RunID); err == nil {
			if rp, err := a.repoForRun(ctx, run); err == nil {
				if wf := a.runWorkflow(ctx, rp, run); wf != nil {
					if wj := wf.Job(j.JobKey); wj != nil && wj.Strategy != nil && wj.Strategy.FailFast {
						a.cancelSiblingsLocked(ctx, j)
					}
				}
			}
		}
	}
	a.actions().signal()
	a.advanceRunLocked(ctx, j.RunID)
}

func (a *App) cancelSiblingsLocked(ctx context.Context, failed store.ActionsJob) {
	jobs, err := a.db.ActionsJobs(ctx, failed.RunID)
	if err != nil {
		return
	}
	for _, o := range jobs {
		if o.ID == failed.ID || o.JobKey != failed.JobKey || o.Finished() {
			continue
		}
		a.cancelJobLocked(ctx, o, "Cancelled because "+failed.Name+" failed (fail-fast).")
	}
}

// cancelJobLocked stops one job: running ones are told through the logs reply.
func (a *App) cancelJobLocked(ctx context.Context, j store.ActionsJob, note string) {
	if j.Finished() {
		return
	}
	if j.Status == actions.StatusRunning {
		if !j.CancelRequested {
			_ = a.db.RequestJobCancel(ctx, j.ID)
			if note != "" {
				_ = a.db.ForceAppendActionsLog(ctx, j.ID, actionsSystemLines(note))
			}
		}
		return
	}
	j.Status, j.FinishedAt, j.CancelRequested = actions.StatusCancelled, time.Now(), true
	if note != "" && j.Error == "" {
		j.Error = note
	}
	_ = a.db.SaveActionsJob(ctx, j)
	_ = a.db.MarkActionsStepsUnfinished(ctx, j.ID, actions.StatusCancelled)
}

// cancelRunLocked cancels a run and every job in it.
func (a *App) cancelRunLocked(ctx context.Context, run store.ActionsRun, who Actor) {
	if run.Done() {
		return
	}
	wasPending := run.Status == store.RunPending
	run.Conclusion = actions.StatusCancelled
	if wasPending {
		run.Status = store.RunQueued // let the status update below complete it
	}
	if err := a.db.SaveActionsRun(ctx, run); err != nil {
		return
	}
	jobs, _ := a.db.ActionsJobs(ctx, run.ID)
	for _, j := range jobs {
		a.cancelJobLocked(ctx, j, "Cancelled by "+who.Name+".")
	}
	a.actions().signal()
	if rp, err := a.repoForRun(ctx, run); err == nil {
		a.updateRunStatusLocked(ctx, rp, run.ID)
	}
}

func (a *App) cancelRun(ctx context.Context, run store.ActionsRun, who Actor) {
	st := a.actions()
	st.mu.Lock()
	defer st.mu.Unlock()
	if fresh, err := a.db.ActionsRunByID(ctx, run.ID); err == nil {
		a.cancelRunLocked(ctx, fresh, who)
	}
}

// approveRun approves waiting jobs (one, or all when jobID is 0). Approving
// the run also approves jobs still pending for the same pull request reason.
func (a *App) approveRun(ctx context.Context, run store.ActionsRun, jobID int64, who Actor) int {
	st := a.actions()
	st.mu.Lock()
	defer st.mu.Unlock()
	jobs, err := a.db.ActionsJobs(ctx, run.ID)
	if err != nil {
		return 0
	}
	n := 0
	for _, j := range jobs {
		if jobID != 0 && j.ID != jobID {
			continue
		}
		switch {
		case j.Status == actions.StatusWaiting:
			j.ApprovedBy, j.Status, j.QueuedAt = who.Name, actions.StatusQueued, time.Now()
			n++
		case jobID == 0 && j.Status == store.JobPending && j.Approval != "" && j.ApprovedBy == "":
			j.ApprovedBy = who.Name
		default:
			continue
		}
		_ = a.db.SaveActionsJob(ctx, j)
	}
	st.signal()
	a.advanceRunLocked(ctx, run.ID)
	return n
}

// rerunRun starts a run again: every job ("all") or only the failed and
// cancelled ones plus the jobs that need them ("failed").
func (a *App) rerunRun(ctx context.Context, run store.ActionsRun, mode string, who Actor) error {
	st := a.actions()
	st.mu.Lock()
	defer st.mu.Unlock()
	run, err := a.db.ActionsRunByID(ctx, run.ID)
	if err != nil {
		return err
	}
	if !run.Done() {
		return fmt.Errorf("the run is still in progress")
	}
	if len(run.Errors) > 0 {
		return fmt.Errorf("this run can't start: %s", run.Errors[0])
	}
	rp, err := a.repoForRun(ctx, run)
	if err != nil {
		return err
	}
	wf := a.runWorkflow(ctx, rp, run)
	if wf == nil {
		return fmt.Errorf("the workflow file is gone from this commit")
	}
	jobs, err := a.db.ActionsJobs(ctx, run.ID)
	if err != nil {
		return err
	}
	reset := map[int64]bool{}
	if mode == "failed" {
		keys := map[string]bool{}
		for _, j := range jobs {
			if j.Status == actions.StatusFailure || j.Status == actions.StatusCancelled {
				reset[j.ID], keys[j.JobKey] = true, true
			}
		}
		if len(reset) == 0 {
			return fmt.Errorf("no jobs failed")
		}
		// Jobs that (transitively) need a reset job run again too.
		for grew := true; grew; {
			grew = false
			for _, wj := range wf.Jobs {
				if keys[wj.ID] {
					continue
				}
				for _, n := range wj.Needs {
					if keys[n] {
						keys[wj.ID], grew = true, true
						break
					}
				}
			}
		}
		for _, j := range jobs {
			if keys[j.JobKey] {
				reset[j.ID] = true
			}
		}
	} else {
		for _, j := range jobs {
			reset[j.ID] = true
		}
	}
	for _, j := range jobs {
		if !reset[j.ID] {
			continue
		}
		wj := wf.Job(j.JobKey)
		nj := j
		nj.Status, nj.Error, nj.Outputs, nj.RunnerID, nj.RunnerName = store.JobPending, "", nil, 0, ""
		nj.CancelRequested, nj.Payload, nj.SecretNames = false, "", nil
		nj.QueuedAt, nj.StartedAt, nj.FinishedAt = time.Time{}, time.Time{}, time.Time{}
		nj.ApprovedBy = who.Name // re-running is the owner's approval
		if wj == nil {
			nj.Status, nj.Error, nj.FinishedAt = actions.StatusFailure, "The workflow no longer has this job.", time.Now()
		} else if wj.Unsupported != "" {
			nj.Status, nj.Error, nj.FinishedAt = actions.StatusFailure, wj.Unsupported, time.Now()
		}
		_ = a.db.DeleteActionsLogs(ctx, j.ID)
		_ = a.db.ResetActionsSteps(ctx, j.ID, nil)
		_ = a.db.DeleteActionsJobToken(ctx, j.ID)
		_ = a.db.SaveActionsJob(ctx, nj)
	}
	run.Attempt++
	run.Status, run.Conclusion, run.FinishedAt, run.StartedAt = store.RunQueued, "", time.Time{}, time.Time{}
	if err := a.db.SaveActionsRun(ctx, run); err != nil {
		return err
	}
	a.advanceRunLocked(ctx, run.ID)
	return nil
}

// ---------- maintenance ----------

// actionsMaintenance enforces timeouts, notices offline runners and gives up
// on jobs no runner takes.
func (a *App) actionsMaintenance(ctx context.Context) {
	st := a.actions()
	now := time.Now()

	runners, err := a.db.ActionsRunners(ctx)
	if err != nil {
		return
	}
	offline := map[int64]bool{}
	for _, r := range runners {
		if now.Sub(r.LastSeen) > st.offlineAfter {
			offline[r.ID] = true
			if !r.Offline && r.Enabled {
				_ = a.db.MarkActionsRunnerOfflineNotified(ctx, r.ID)
				a.emit(ctx, Event{Kind: EventRunnerOffline, Title: r.Name + " went offline", URL: "/settings/runners",
					Detail: "Last seen " + r.LastSeen.Format("2 Jan 15:04"), Actor: Actor{Kind: ActorSystem, Name: "actions"},
					Recipients: []int64{r.PersonID}})
			}
		}
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	running, _ := a.db.ActionsJobsWithStatus(ctx, actions.StatusRunning)
	for _, j := range running {
		limit := time.Duration(j.TimeoutMinutes) * time.Minute
		switch {
		case !j.StartedAt.IsZero() && now.Sub(j.StartedAt) > limit+2*time.Minute:
			a.finishJobLocked(ctx, j, actions.StatusFailure, nil, fmt.Sprintf("The job exceeded its %d-minute timeout.", j.TimeoutMinutes))
		case !j.StartedAt.IsZero() && now.Sub(j.StartedAt) > limit && !j.CancelRequested:
			_ = a.db.ForceAppendActionsLog(ctx, j.ID, actionsSystemLines(fmt.Sprintf("The job exceeded its %d-minute timeout and is being stopped.", j.TimeoutMinutes)))
			j.CancelRequested, j.Error = true, fmt.Sprintf("The job exceeded its %d-minute timeout.", j.TimeoutMinutes)
			_ = a.db.SaveActionsJob(ctx, j)
		case j.RunnerID != 0 && offline[j.RunnerID] && runnerLastSeen(runners, j.RunnerID).Before(now.Add(-2*st.offlineAfter)):
			a.finishJobLocked(ctx, j, actions.StatusFailure, nil, "The runner "+j.RunnerName+" went offline during the job.")
		}
	}
	queued, _ := a.db.ActionsJobsWithStatus(ctx, actions.StatusQueued)
	for _, j := range queued {
		if !j.QueuedAt.IsZero() && now.Sub(j.QueuedAt) > 24*time.Hour {
			j.Status, j.FinishedAt, j.Error = actions.StatusCancelled, now, "No runner with the labels "+strings.Join(j.RunsOn, ", ")+" picked this job up within 24 hours."
			_ = a.db.SaveActionsJob(ctx, j)
			a.advanceRunLocked(ctx, j.RunID)
		}
	}
}

func runnerLastSeen(rs []store.ActionsRunner, id int64) time.Time {
	for _, r := range rs {
		if r.ID == id {
			return r.LastSeen
		}
	}
	return time.Time{}
}
