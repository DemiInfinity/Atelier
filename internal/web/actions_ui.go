package web

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"atelier/internal/actions"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
	"atelier/internal/workflow"
)

// Repository Actions pages: /{owner}/{repo}/actions (workflows and runs),
// /{owner}/{repo}/actions/runs/{n} (one run, its job graph and live logs).

func actionsUIRoutes(a *App, m *http.ServeMux) {
	m.HandleFunc("GET /{owner}/{repo}/actions", a.authed(a.actionsIndex))
	m.HandleFunc("POST /{owner}/{repo}/actions/dispatch", a.authed(a.actionsDispatch))
	m.HandleFunc("POST /{owner}/{repo}/actions/lockfile", a.authed(a.actionsLockSetting))
	m.HandleFunc("GET /{owner}/{repo}/actions/runs/{n}", a.authed(a.actionsRunPage))
	m.HandleFunc("POST /{owner}/{repo}/actions/runs/{n}/cancel", a.authed(a.actionsRunCancel))
	m.HandleFunc("POST /{owner}/{repo}/actions/runs/{n}/rerun", a.authed(a.actionsRunRerun))
	m.HandleFunc("POST /{owner}/{repo}/actions/runs/{n}/approve", a.authed(a.actionsRunApprove))
	m.HandleFunc("GET /{owner}/{repo}/actions/runs/{n}/lockfile", a.authed(a.actionsRunLockfile))
	m.HandleFunc("GET /{owner}/{repo}/actions/runs/{n}/jobs/{job}/logs", a.authed(a.actionsJobLogs))
	m.HandleFunc("GET /{owner}/{repo}/actions/runs/{n}/logs/stream", a.authed(a.actionsLogStream))
}

// ---------- workflows and runs ----------

type actWorkflowView struct {
	Path, Name, File string
	Agentic          bool
	Events           []string
	Errors           int
	Warnings         int
	Dispatch         *workflow.DispatchTrigger
	URL              string
	Active           bool
}

type actRunRow struct {
	store.ActionsRun
	URL string
}

type actRunnerDot struct {
	Name, OS string
	Online   bool
	Busy     bool
	Enabled  bool
}

type actIndexData struct {
	V                *repoView
	Workflows        []actWorkflowView
	Selected         *actWorkflowView
	Source           []codeLine
	SourceURL        string
	Runs             []actRunRow
	Runners          []actRunnerDot
	Lockfile         bool
	LockCount        int
	LockErr          string
	RequireLock      bool
	CanAdmin         bool
	CanWrite         bool
	Workflow         string
	Status           string
	Branch           string
	Branches         []string
	RunBranches      []string
	Page, Prev, Next int
	More             bool
	Filtered         bool
	Statuses         []string
}

var actStatusFilters = []string{"in_progress", "queued", "waiting", "success", "failure", "cancelled"}

func (a *App) actionsIndex(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "actions")
	if !ok {
		return
	}
	a.rememberBaseURL(a.baseURL(r))
	a.renderActionsIndex(w, r, v, g, "", http.StatusOK)
}

func (a *App) renderActionsIndex(w http.ResponseWriter, r *http.Request, v *repoView, g *gitrepo.Repo, errMsg string, code int) {
	ctx := r.Context()
	who := a.actor(r)
	q := r.URL.Query()
	d := &actIndexData{V: v, Workflow: q.Get("workflow"), Status: q.Get("status"), Branch: q.Get("branch"),
		Branches: v.Branches, CanAdmin: a.can(ctx, who, v.Repo, PermAdmin), CanWrite: a.can(ctx, who, v.Repo, PermWrite),
		Statuses: actStatusFilters}
	valid := false
	for _, s := range actStatusFilters {
		valid = valid || s == d.Status
	}
	if !valid {
		d.Status = ""
	}
	d.Filtered = d.Status != "" || d.Branch != ""

	if !v.Empty {
		if sha, err := g.BranchSHA(ctx, v.Default); err == nil {
			for _, wf := range a.loadWorkflows(ctx, v.Repo.ID, g, sha) {
				wv := actWorkflowView{Path: wf.Path, Name: wf.Name, File: wf.Path[strings.LastIndex(wf.Path, "/")+1:],
					Agentic: wf.Agentic, Events: wf.On.Events(), Errors: len(wf.Errors), Warnings: len(wf.Warnings),
					Dispatch: wf.On.Dispatch, URL: v.Base + "/actions?workflow=" + url.QueryEscape(wf.Path)}
				if wf.Path == d.Workflow {
					wv.Active = true
					d.Source = codeLines(wf.Source)
					d.SourceURL = v.Base + "/blob/" + v.Default + "/" + wf.Path
				}
				d.Workflows = append(d.Workflows, wv)
			}
			for i := range d.Workflows {
				if d.Workflows[i].Active {
					d.Selected = &d.Workflows[i]
				}
			}
			if b, err := g.Blob(ctx, sha, workflow.LockfilePath, workflow.MaxLockfile); err == nil && !b.TooLarge {
				d.Lockfile = true
				if l, err := workflow.ParseLockfile(b.Data); err == nil {
					d.LockCount = len(l)
				} else {
					d.LockErr = err.Error()
				}
			}
		}
	}
	d.RequireLock, _ = a.db.ActionsRequireLockfile(ctx, v.Repo.ID)

	pageNum, _ := strconv.Atoi(q.Get("page"))
	if pageNum < 1 {
		pageNum = 1
	}
	const per = 30
	runs, err := a.db.ActionsRuns(ctx, store.ActionsRunFilter{RepoID: v.Repo.ID, Workflow: d.Workflow, Status: d.Status,
		Branch: d.Branch, Limit: per + 1, Offset: (pageNum - 1) * per})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d.More = len(runs) > per
	if d.More {
		runs = runs[:per]
	}
	d.Page, d.Prev, d.Next = pageNum, pageNum-1, pageNum+1
	for _, run := range runs {
		d.Runs = append(d.Runs, actRunRow{ActionsRun: run, URL: actionsRunURL(v.Repo, run)})
	}
	d.RunBranches, _ = a.db.ActionsRunBranches(ctx, v.Repo.ID)
	if runners, err := a.db.ActionsRunners(ctx); err == nil {
		off := a.actions().offlineAfter
		for _, rn := range runners {
			d.Runners = append(d.Runners, actRunnerDot{Name: rn.Name, OS: runnerOSLabel(rn.OS), Online: time.Since(rn.LastSeen) < off,
				Busy: rn.Busy, Enabled: rn.Enabled})
		}
	}
	title := "Actions · " + v.Repo.FullName()
	if d.Selected != nil {
		title = d.Selected.Name + " · " + title
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "actions_index", page{Title: title, Nav: "repos", Error: errMsg, Notice: q.Get("saved"), Data: d})
}

func (a *App) actionsDispatch(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "actions")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	wfPath := r.FormValue("workflow")
	branch := r.FormValue("ref")
	if branch == "" {
		branch = v.Default
	}
	inputs := map[string]string{}
	for k, vals := range r.PostForm {
		if name, ok := strings.CutPrefix(k, "input."); ok && len(vals) > 0 {
			inputs[name] = vals[len(vals)-1]
		}
	}
	run, err := a.dispatchWorkflow(r.Context(), v.Repo, g, wfPath, branch, inputs, a.actor(r))
	if err != nil {
		r.URL.RawQuery = "workflow=" + url.QueryEscape(wfPath)
		a.renderActionsIndex(w, r, v, g, "Couldn't run the workflow: "+err.Error()+".", http.StatusUnprocessableEntity)
		return
	}
	http.Redirect(w, r, actionsRunURL(v.Repo, run), http.StatusSeeOther)
}

func (a *App) actionsLockSetting(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings") // admin
	if !ok {
		return
	}
	if err := a.db.SetActionsRequireLockfile(r.Context(), v.Repo.ID, r.FormValue("require") == "1"); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/actions?saved=1", http.StatusSeeOther)
}

// ---------- one run ----------

type actStepView struct {
	store.ActionsStep
	Lines    []actions.LogLine
	Hidden   int // lines not shown (too many)
	Open     bool
	Duration string
}

type actJobView struct {
	store.ActionsJob
	Summaries   []template.HTML // step summaries, rendered Markdown
	Setup       []actions.LogLine
	Steps       []actStepView
	NeedNames   []string
	Hint        string // why a queued job isn't running
	Open        bool
	Duration    string
	LogsURL     string
	MatrixLabel string
}

type actRunData struct {
	V         *repoView
	Run       store.ActionsRun
	URL       string
	Jobs      []actJobView
	Stages    [][]*actJobView
	Waiting   int
	CanWrite  bool
	CanRerun  bool
	CanCancel bool
	HasFailed bool
	Unpinned  []string
	LastLogID int64
	Duration  string
	Live      bool
	Commit    *gitrepo.Commit
}

const actMaxStepLines = 3000

func (a *App) loadRun(w http.ResponseWriter, r *http.Request) (*repoView, *gitrepo.Repo, store.ActionsRun, bool) {
	v, g, ok := a.loadRepo(w, r, "actions")
	if !ok {
		return nil, nil, store.ActionsRun{}, false
	}
	n, err := strconv.ParseInt(r.PathValue("n"), 10, 64)
	if err != nil || n < 1 {
		a.notFound(w, r)
		return nil, nil, store.ActionsRun{}, false
	}
	run, err := a.db.ActionsRunBySeq(r.Context(), v.Repo.ID, n)
	if err != nil {
		a.fail(w, r, err)
		return nil, nil, store.ActionsRun{}, false
	}
	return v, g, run, true
}

func fmtDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

func (a *App) actionsRunPage(w http.ResponseWriter, r *http.Request) {
	v, g, run, ok := a.loadRun(w, r)
	if !ok {
		return
	}
	a.renderRun(w, r, v, g, run, "", http.StatusOK)
}

func (a *App) renderRun(w http.ResponseWriter, r *http.Request, v *repoView, g *gitrepo.Repo, run store.ActionsRun, errMsg string, code int) {
	ctx := r.Context()
	who := a.actor(r)
	d := &actRunData{V: v, Run: run, URL: actionsRunURL(v.Repo, run), CanWrite: a.can(ctx, who, v.Repo, PermWrite),
		Duration: fmtDuration(run.Duration()), Live: !run.Done(), Commit: g.LastCommit(ctx, run.SHA, "")}
	d.CanCancel = d.CanWrite && !run.Done()
	d.CanRerun = d.CanWrite && run.Done() && len(run.Errors) == 0
	for k := range run.LockResolved {
		d.Unpinned = append(d.Unpinned, k)
	}
	sortStrings(d.Unpinned)

	jobs, err := a.db.ActionsJobs(ctx, run.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	runners, _ := a.db.ActionsRunners(ctx)
	byKey := map[string][]string{}
	for _, j := range jobs {
		byKey[j.JobKey] = append(byKey[j.JobKey], j.Name)
	}
	wf := a.runWorkflow(ctx, v.Repo, run)
	logs, _ := a.db.ActionsRunLogs(ctx, run.ID, 0, 1<<30)
	perJob := map[int64][]actions.LogLine{}
	for _, c := range logs {
		var lines []actions.LogLine
		if json.Unmarshal([]byte(c.Lines), &lines) == nil {
			perJob[c.JobID] = append(perJob[c.JobID], lines...)
		}
		d.LastLogID = c.ID
	}
	for _, j := range jobs {
		jv := actJobView{ActionsJob: j, Duration: fmtDuration(j.Duration()),
			LogsURL: fmt.Sprintf("%s/jobs/%d/logs", d.URL, j.ID)}
		if m, ok := parseActMatrix(j.Matrix); ok {
			jv.MatrixLabel = workflow.Combo{Keys: m.Keys, Values: m.Values}.Label()
		}
		if wf != nil {
			if wj := wf.Job(j.JobKey); wj != nil {
				for _, n := range wj.Needs {
					jv.NeedNames = append(jv.NeedNames, n)
				}
			}
		}
		steps, _ := a.db.ActionsSteps(ctx, j.ID)
		byStep := map[int][]actions.LogLine{}
		for _, l := range perJob[j.ID] {
			if l.Step < 0 {
				jv.Setup = append(jv.Setup, l)
			} else {
				byStep[l.Step] = append(byStep[l.Step], l)
			}
		}
		for _, s := range steps {
			sv := actStepView{ActionsStep: s, Lines: byStep[s.Index], Duration: fmtDuration(s.Duration()),
				Open: s.Status == actions.StatusFailure || s.Status == actions.StatusRunning}
			if n := len(sv.Lines); n > actMaxStepLines {
				sv.Hidden, sv.Lines = n-actMaxStepLines, sv.Lines[n-actMaxStepLines:]
			}
			jv.Steps = append(jv.Steps, sv)
			if strings.TrimSpace(s.Summary) != "" {
				jv.Summaries = append(jv.Summaries, renderMarkdown([]byte(s.Summary)))
			}
		}
		if n := len(jv.Setup); n > actMaxStepLines {
			jv.Setup = jv.Setup[n-actMaxStepLines:]
		}
		jv.Open = j.Status == actions.StatusRunning || j.Status == actions.StatusFailure || len(jobs) == 1
		if j.Status == actions.StatusQueued {
			jv.Hint = queuedHint(runners, j, a.actions().offlineAfter)
		}
		switch j.Status {
		case actions.StatusWaiting:
			d.Waiting++
		case actions.StatusFailure, actions.StatusCancelled:
			d.HasFailed = true
		}
		d.Jobs = append(d.Jobs, jv)
	}
	// Job graph: columns by depth in the needs graph.
	if wf != nil && wf.Valid() {
		col := map[string]int{}
		for i, stage := range workflow.Stages(wf.Jobs) {
			for _, id := range stage {
				col[id] = i
			}
		}
		maxCol := 0
		for _, c := range col {
			if c > maxCol {
				maxCol = c
			}
		}
		d.Stages = make([][]*actJobView, maxCol+1)
		for i := range d.Jobs {
			c := col[d.Jobs[i].JobKey]
			d.Stages[c] = append(d.Stages[c], &d.Jobs[i])
		}
	} else if len(d.Jobs) > 0 {
		d.Stages = [][]*actJobView{{}}
		for i := range d.Jobs {
			d.Stages[0] = append(d.Stages[0], &d.Jobs[i])
		}
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "actions_run", page{
		Title: fmt.Sprintf("%s #%d · %s", run.WorkflowName, run.RunNumber, v.Repo.FullName()),
		Nav:   "repos", Error: errMsg, Data: d,
	})
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// queuedHint explains why a queued job hasn't started.
func queuedHint(runners []store.ActionsRunner, j store.ActionsJob, offlineAfter time.Duration) string {
	want := strings.Join(j.RunsOn, ", ")
	if j.NeedsDocker {
		want += " (with Docker)"
	}
	if len(runners) == 0 {
		return "Waiting for a runner. Register one in Settings → Runners."
	}
	match, online := false, false
	for _, rn := range runners {
		if runnerCanRun(rn, j) {
			match = true
			if time.Since(rn.LastSeen) < offlineAfter {
				online = true
			}
		}
	}
	switch {
	case online:
		return "Waiting for a runner with " + want + " to pick it up."
	case match:
		return "The runners with " + want + " are offline."
	}
	return "No enabled runner has the labels " + want + "."
}

func (a *App) actionsRunCancel(w http.ResponseWriter, r *http.Request) {
	v, _, run, ok := a.loadRun(w, r)
	if !ok {
		return
	}
	a.cancelRun(r.Context(), run, a.actor(r))
	http.Redirect(w, r, actionsRunURL(v.Repo, run), http.StatusSeeOther)
}

func (a *App) actionsRunRerun(w http.ResponseWriter, r *http.Request) {
	v, g, run, ok := a.loadRun(w, r)
	if !ok {
		return
	}
	mode := r.FormValue("mode")
	if mode != "failed" {
		mode = "all"
	}
	if err := a.rerunRun(r.Context(), run, mode, a.actor(r)); err != nil {
		a.renderRun(w, r, v, g, run, "Couldn't re-run: "+err.Error()+".", http.StatusConflict)
		return
	}
	http.Redirect(w, r, actionsRunURL(v.Repo, run), http.StatusSeeOther)
}

func (a *App) actionsRunApprove(w http.ResponseWriter, r *http.Request) {
	v, _, run, ok := a.loadRun(w, r)
	if !ok {
		return
	}
	jobID, _ := strconv.ParseInt(r.FormValue("job"), 10, 64)
	a.approveRun(r.Context(), run, jobID, a.actor(r))
	http.Redirect(w, r, actionsRunURL(v.Repo, run), http.StatusSeeOther)
}

func (a *App) actionsRunLockfile(w http.ResponseWriter, r *http.Request) {
	_, _, run, ok := a.loadRun(w, r)
	if !ok {
		return
	}
	if run.LockGenerated == "" {
		a.notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="actions.lock"`)
	w.Header().Set("Content-Security-Policy", "sandbox")
	_, _ = w.Write([]byte(run.LockGenerated))
}

// actionsJobLogs serves a job's whole log as plain text.
func (a *App) actionsJobLogs(w http.ResponseWriter, r *http.Request) {
	_, _, run, ok := a.loadRun(w, r)
	if !ok {
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("job"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	j, err := a.db.ActionsJob(r.Context(), jobID)
	if err != nil || j.RunID != run.ID {
		a.notFound(w, r)
		return
	}
	chunks, err := a.db.ActionsLogs(r.Context(), j.ID, 0, 0)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	steps, _ := a.db.ActionsSteps(r.Context(), j.ID)
	names := map[int]string{}
	for _, s := range steps {
		names[s.Index] = s.Name
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "sandbox")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="job-%d.log"`, j.ID))
	}
	last := -2
	for _, c := range chunks {
		var lines []actions.LogLine
		if json.Unmarshal([]byte(c.Lines), &lines) != nil {
			continue
		}
		for _, l := range lines {
			if l.Step != last {
				last = l.Step
				title := "Set up job"
				if l.Step >= 0 {
					title = names[l.Step]
				}
				fmt.Fprintf(w, "##[group] %s\n", title)
			}
			fmt.Fprintf(w, "%s %s\n", l.Time.UTC().Format("2006-01-02T15:04:05Z"), l.Text)
		}
	}
}

// ---------- live logs (Server-Sent Events) ----------

type actStreamStatus struct {
	Run  string               `json:"run"`
	Done bool                 `json:"done"`
	Jobs []actStreamJobStatus `json:"jobs"`
}

type actStreamJobStatus struct {
	ID       int64           `json:"id"`
	Status   string          `json:"status"`
	Duration string          `json:"duration"`
	Steps    []actStreamStep `json:"steps"`
}

type actStreamStep struct {
	Index    int    `json:"index"`
	Status   string `json:"status"`
	Duration string `json:"duration"`
}

type actStreamLog struct {
	ID    int64             `json:"id"`
	Job   int64             `json:"job"`
	Lines []actions.LogLine `json:"lines"`
}

// actionsLogStream sends new log chunks and status changes for a run until
// it finishes. Browsers reconnect with Last-Event-ID; without JavaScript the
// page's reload link shows the same logs.
func (a *App) actionsLogStream(w http.ResponseWriter, r *http.Request) {
	v, _, run, ok := a.loadRun(w, r)
	if !ok {
		return
	}
	fl, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if q, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64); err == nil && q > after {
		after = q
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	st := a.actions()
	limit := time.NewTimer(10 * time.Minute) // browsers reconnect; keeps stuck connections from piling up
	defer limit.Stop()
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()
	lastStatus := ""
	for {
		wake := st.waitCh()
		chunks, err := a.db.ActionsRunLogs(ctx, run.ID, after, 200)
		if err != nil {
			return
		}
		for _, c := range chunks {
			var lines []actions.LogLine
			if json.Unmarshal([]byte(c.Lines), &lines) != nil {
				continue
			}
			b, _ := json.Marshal(actStreamLog{ID: c.ID, Job: c.JobID, Lines: lines})
			fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", c.ID, b)
			after = c.ID
		}
		status, done := a.streamStatus(ctx, v.Repo, run.ID)
		if status != lastStatus {
			fmt.Fprintf(w, "event: status\ndata: %s\n\n", status)
			lastStatus = status
		}
		fl.Flush()
		if len(chunks) == 200 {
			continue // more to send
		}
		if done {
			fmt.Fprint(w, "event: done\ndata: {}\n\n")
			fl.Flush()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-limit.C:
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case <-wake:
		}
	}
}

func (a *App) streamStatus(ctx context.Context, rp store.Repo, runID int64) (string, bool) {
	run, err := a.db.ActionsRunByID(ctx, runID)
	if err != nil {
		return "", true
	}
	s := actStreamStatus{Run: run.Outcome(), Done: run.Done()}
	jobs, _ := a.db.ActionsJobs(ctx, runID)
	for _, j := range jobs {
		js := actStreamJobStatus{ID: j.ID, Status: j.Status, Duration: fmtDuration(j.Duration())}
		steps, _ := a.db.ActionsSteps(ctx, j.ID)
		for _, st := range steps {
			js.Steps = append(js.Steps, actStreamStep{Index: st.Index, Status: st.Status, Duration: fmtDuration(st.Duration())})
		}
		s.Jobs = append(s.Jobs, js)
	}
	b, _ := json.Marshal(s)
	return string(b), run.Done()
}
