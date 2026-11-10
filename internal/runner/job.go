package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"atelier/internal/actions"
	"atelier/internal/runner/expr"
)

var (
	errCancelled   = errors.New("the job was cancelled")
	errShutdown    = errors.New("the runner is shutting down")
	errJobTimeout  = errors.New("the job exceeded its timeout")
	errStepTimeout = errors.New("the step exceeded its timeout")
)

// timeUnit is what timeout-minutes counts in (shortened by tests).
var timeUnit = time.Minute

const (
	defaultJobTimeout   = 360 * time.Minute
	alwaysStepTimeout   = 10 * time.Minute // steps that run after a cancel, without their own timeout
	maxCompositeDepth   = 10
	containerMountRoot  = "/__w"
	containerWorkspace  = "/github/workspace"
	containerLabelJobID = "dev.atelier.job"
)

// stepDef is a step from the job or from a composite action.
type stepDef struct {
	Index            int
	ID               string
	Name             string
	If               string
	Run              string
	Shell            string
	Uses             string
	With             map[string]string
	Env              map[string]string
	WorkingDirectory string
	ContinueOnError  string // "true"/"false" or an expression
	TimeoutMinutes   int
}

func (s stepDef) displayName() string {
	switch {
	case s.Name != "":
		return s.Name
	case s.Uses != "":
		return s.Uses
	case s.Run != "":
		l, _, _ := strings.Cut(strings.TrimSpace(s.Run), "\n")
		return "Run " + l
	}
	return fmt.Sprintf("Step %d", s.Index)
}

// scope is the steps context of the job or of one composite action.
type scope struct {
	steps      map[string]any
	inputs     map[string]any
	status     string // success | failure
	logStep    int    // Step.Index the logs belong to
	actionPath string
	parentEnv  map[string]string // env of the step that called a composite action
	depth      int
}

func newScope() *scope {
	return &scope{steps: map[string]any{}, status: actions.StatusSuccess, logStep: -1}
}

type postAction struct {
	sc     *scope
	st     stepDef
	act    *actionDef
	dir    string
	inputs map[string]string
	state  map[string]string
}

type jobRun struct {
	r    *Runner
	spec *JobSpec
	job  *actions.Job
	log  *logStream
	mask *Masker
	slog *slog.Logger

	root, ws, temp, files, toolCache, eventPath string

	ctx    context.Context
	cancel context.CancelCauseFunc

	mu      sync.Mutex
	env     envMap   // job env + GITHUB_ENV additions
	paths   []string // GITHUB_PATH additions, newest first
	posts   []postAction
	github  map[string]any
	seq     int
	ctr     *jobContainer
	actDirs map[string]string // uses → downloaded directory
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runJob executes one job and reports it. It never panics the runner.
func (r *Runner) runJob(parent context.Context, spec *JobSpec) {
	started := time.Now().UTC()
	job := &spec.Job
	m := NewMasker()
	for _, v := range job.Secrets {
		m.Add(v)
	}
	m.Add(job.JobToken)

	base, cancel := context.WithCancelCause(parent)
	timeout := defaultJobTimeout
	if job.TimeoutMinutes > 0 {
		timeout = time.Duration(job.TimeoutMinutes) * timeUnit
	}
	ctx, cancelTimeout := context.WithTimeoutCause(base, timeout, errJobTimeout)
	defer cancelTimeout()
	r.trackJob(job.ID, cancel)
	defer r.untrackJob(job.ID)

	j := &jobRun{r: r, spec: spec, job: job, mask: m, slog: r.logger(), ctx: ctx, cancel: cancel, actDirs: map[string]string{}}
	j.log = newLogStream(r.Client, job.ID, m, func() { cancel(errCancelled) }, r.logger())

	status, outputs, setupErr := j.execute()

	fin := actions.FinishRequest{Status: status, StartedAt: started, FinishedAt: time.Now().UTC(), Outputs: m.MaskMap(outputs)}
	if setupErr != nil {
		fin.Error = m.Mask(setupErr.Error())
		j.log.System(-1, "Error: "+setupErr.Error())
	}
	j.log.System(-1, "Job finished: "+status)
	j.log.Close()
	r.report(func(ctx context.Context) error { return r.Client.Finish(ctx, job.ID, fin) }, "finish", job.ID)
	r.logger().Info("job finished", "job", job.ID, "status", status)
}

// report retries a report call for up to ~2 minutes; it is not tied to the
// job's (possibly cancelled) context.
func (r *Runner) report(call func(context.Context) error, what string, jobID int64) {
	var err error
	delay := 500 * time.Millisecond
	for range 8 {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = call(ctx)
		cancel()
		var he *HTTPError
		if err == nil || (errors.As(err, &he) && he.Status/100 == 4 && he.Status != 429) {
			break
		}
		time.Sleep(delay)
		delay = min(delay*2, 20*time.Second)
	}
	if err != nil {
		r.logger().Warn("report failed", "what", what, "job", jobID, "err", err)
	}
}

func (j *jobRun) cause() error { return context.Cause(j.ctx) }

// interrupted: cancelled, shutting down or timed out.
func (j *jobRun) interrupted() bool { return j.ctx.Err() != nil }

func (j *jobRun) execute() (status string, outputs map[string]string, setupErr error) {
	defer func() {
		if p := recover(); p != nil {
			status, setupErr = actions.StatusFailure, fmt.Errorf("runner error: %v", p)
		}
	}()
	work, _ := j.r.Config.Dirs()
	j.root = filepath.Join(work, fmt.Sprintf("job-%d-%s", j.job.ID, randHex(4)))
	j.ws = filepath.Join(j.root, "w")
	j.temp = filepath.Join(j.root, "t")
	j.files = filepath.Join(j.root, "f")
	j.toolCache = filepath.Join(work, "_tool")
	for _, d := range []string{j.ws, j.temp, j.files, j.toolCache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return actions.StatusFailure, nil, fmt.Errorf("create workspace: %w", err)
		}
	}
	defer func() {
		if j.ctr != nil {
			j.ctr.stop(j)
		}
		if err := removeAll(j.root); err != nil {
			j.slog.Warn("could not remove job workspace", "dir", j.root, "err", err)
		}
	}()

	j.log.System(-1, fmt.Sprintf("Runner %s (%s/%s), labels: %s", j.r.Config.Name, OSLabel(), ArchLabel(), strings.Join(AllLabels(j.r.Config.Labels), ", ")))
	j.log.System(-1, fmt.Sprintf("Job %q of %s@%s (%s)", j.job.Name, j.repoFull(), shortSHA(j.job.SHA), j.job.Workflow))
	j.setupContexts()

	top := newScope()
	// job-level env (may reference secrets.*, env.* of earlier keys, ...)
	j.env = envMap{}
	for _, k := range sortedKeys(j.job.Env) {
		v, err := j.eval(top, nil).Interpolate(j.job.Env[k])
		if err != nil {
			return actions.StatusFailure, nil, fmt.Errorf("env %s: %w", k, err)
		}
		j.env.Set(k, v)
	}

	if j.job.Container != nil && j.job.Container.Image != "" {
		if !j.r.Config.HasCap(actions.CapDocker) {
			return actions.StatusFailure, nil, errors.New("this job uses container: but the docker capability is off on this runner")
		}
		ctr, err := j.startContainer(top)
		if err != nil {
			return actions.StatusFailure, nil, fmt.Errorf("start job container: %w", err)
		}
		j.ctr = ctr
	}

	steps := make([]stepDef, len(j.job.Steps))
	for i, s := range j.job.Steps {
		coe := ""
		if s.ContinueOnError {
			coe = "true"
		}
		steps[i] = stepDef{Index: s.Index, ID: s.ID, Name: s.Name, If: s.If, Run: s.Run, Shell: s.Shell, Uses: s.Uses,
			With: s.With, Env: s.Env, WorkingDirectory: s.WorkingDirectory, ContinueOnError: coe, TimeoutMinutes: s.TimeoutMinutes}
	}
	if err := j.prepareActions(top, steps); err != nil {
		return actions.StatusFailure, nil, err
	}

	j.runSteps(top, steps, true)
	j.runPosts(top)

	outputs = map[string]string{}
	for _, k := range sortedKeys(j.spec.Outputs) {
		v, err := j.eval(top, nil).Interpolate(j.spec.Outputs[k])
		if err != nil {
			j.log.System(-1, fmt.Sprintf("Warning: job output %s: %v", k, err))
			continue
		}
		outputs[k] = v
	}
	switch c := j.cause(); {
	case errors.Is(c, errCancelled), errors.Is(c, errShutdown):
		return actions.StatusCancelled, outputs, nil
	case errors.Is(c, errJobTimeout):
		j.log.System(-1, fmt.Sprintf("Error: the job exceeded its timeout (%d minutes)", j.job.TimeoutMinutes))
		return actions.StatusFailure, outputs, nil
	}
	return top.status, outputs, nil
}

func (j *jobRun) repoFull() string { return j.job.Repo.Owner + "/" + j.job.Repo.Name }

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// setupContexts builds the github/atelier context and the event file.
func (j *jobRun) setupContexts() {
	g := map[string]any{}
	maps.Copy(g, j.job.Context)
	set := func(k string, v any) {
		if _, ok := g[k]; !ok || g[k] == nil || g[k] == "" {
			g[k] = v
		}
	}
	refName, refType := refNameType(j.job.Ref)
	set("sha", j.job.SHA)
	set("ref", j.job.Ref)
	set("ref_name", refName)
	set("ref_type", refType)
	set("repository", j.repoFull())
	set("repository_owner", j.job.Repo.Owner)
	set("event_name", j.job.Event)
	set("run_id", strconv.FormatInt(j.job.RunID, 10))
	set("job", j.job.Name)
	set("workflow", j.job.Workflow)
	set("server_url", strings.TrimRight(j.r.Config.ServerURL, "/"))
	set("api_url", strings.TrimRight(j.r.Config.ServerURL, "/")+"/api/v3")
	g["workspace"] = j.ws
	g["token"] = j.job.JobToken
	if _, ok := g["event"]; !ok {
		g["event"] = map[string]any{}
	}
	j.eventPath = filepath.Join(j.temp, "_event.json")
	if b, err := json.Marshal(g["event"]); err == nil {
		_ = os.WriteFile(j.eventPath, b, 0o644)
	}
	j.github = g
}

func refNameType(ref string) (string, string) {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return strings.TrimPrefix(ref, "refs/heads/"), "branch"
	case strings.HasPrefix(ref, "refs/tags/"):
		return strings.TrimPrefix(ref, "refs/tags/"), "tag"
	}
	return ref, ""
}

func ctxString(m map[string]any, k string) string {
	if v, ok := m[k]; ok && v != nil {
		return expr.ToString(expr.Normalize(v))
	}
	return ""
}

// eval builds an expression environment for a scope. stepEnv is the env
// context for this step (nil: job env only).
func (j *jobRun) eval(sc *scope, stepEnv envMap) *expr.Env {
	j.mu.Lock()
	envCtx := map[string]any{}
	for k, v := range j.env {
		envCtx[k] = v
	}
	j.mu.Unlock()
	for k, v := range sc.parentEnv {
		envCtx[k] = v
	}
	for k, v := range stepEnv {
		envCtx[k] = v
	}
	g := j.github
	if sc.actionPath != "" {
		g = maps.Clone(j.github)
		g["action_path"] = sc.actionPath
	}
	secrets := map[string]any{"GITHUB_TOKEN": j.job.JobToken, "ATELIER_TOKEN": j.job.JobToken}
	for k, v := range j.job.Secrets {
		secrets[k] = v
	}
	jobCtx := map[string]any{"status": j.jobStatus(sc)}
	if j.ctr != nil {
		jobCtx["container"] = map[string]any{"id": j.ctr.id, "network": ""}
	}
	ctxs := map[string]any{
		"github":  g,
		"atelier": g,
		"env":     envCtx,
		"steps":   sc.steps,
		"secrets": secrets,
		"job":     jobCtx,
		"runner": map[string]any{
			"name": j.r.Config.Name, "os": runnerOS(), "arch": runnerArch(), "temp": j.temp,
			"tool_cache": j.toolCache, "workspace": j.root, "environment": "self-hosted", "debug": "",
		},
		"inputs": sc.inputs,
	}
	for _, k := range []string{"matrix", "needs", "vars", "strategy"} {
		if v, ok := j.job.Context[k]; ok {
			ctxs[k] = v
		}
	}
	return &expr.Env{
		Contexts:  ctxs,
		JobStatus: sc.status,
		Cancelled: j.interrupted(),
		HashFiles: func(p []string) (string, error) { return hashFiles(j.ws, p) },
	}
}

func (j *jobRun) jobStatus(sc *scope) string {
	if c := j.cause(); errors.Is(c, errCancelled) || errors.Is(c, errShutdown) {
		return actions.StatusCancelled
	}
	if j.interrupted() {
		return actions.StatusFailure
	}
	return sc.status
}

func runnerOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	}
	return "Linux"
}

func runnerArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "X64"
	case "arm64":
		return "ARM64"
	case "386":
		return "X86"
	}
	return strings.ToUpper(runtime.GOARCH)
}

// runSteps runs a list of steps in a scope; top reports each result.
func (j *jobRun) runSteps(sc *scope, steps []stepDef, top bool) {
	for i, st := range steps {
		if top {
			sc.logStep = st.Index
		}
		started := time.Now().UTC()
		key := st.ID
		if key == "" {
			key = fmt.Sprintf("__%d", i)
		}
		res := j.runOneStep(sc, st)
		ctxEntry := map[string]any{"outputs": toAnyMap(res.outputs), "outcome": res.outcome, "conclusion": res.conclusion}
		sc.steps[key] = ctxEntry
		if res.conclusion == actions.StatusFailure {
			sc.status = actions.StatusFailure
		}
		if top {
			j.log.Flush()
			rep := StepReport{StepResult: actions.StepResult{
				Status: res.outcome, ExitCode: res.exitCode, StartedAt: started, FinishedAt: time.Now().UTC(),
				Outputs: j.mask.MaskMap(res.outputs)}, Summary: j.mask.Mask(res.summary)}
			if res.conclusion != res.outcome {
				rep.Conclusion = res.conclusion
			}
			idx := st.Index
			j.r.report(func(ctx context.Context) error { return j.r.Client.Step(ctx, j.job.ID, idx, rep) }, "step", j.job.ID)
		}
	}
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type stepResult struct {
	outcome, conclusion string
	exitCode            int
	outputs             map[string]string
	summary             string
}

func (j *jobRun) runOneStep(sc *scope, st stepDef) stepResult {
	skip := stepResult{outcome: actions.StatusSkipped, conclusion: actions.StatusSkipped}
	name := st.displayName()
	if errors.Is(j.cause(), errShutdown) {
		j.log.System(sc.logStep, "Skipped (runner shutting down): "+name)
		return skip
	}
	ok, err := j.eval(sc, nil).EvalCondition(st.If)
	if err != nil {
		j.log.System(sc.logStep, fmt.Sprintf("Error: if: %v", err))
		return stepResult{outcome: actions.StatusFailure, conclusion: actions.StatusFailure, exitCode: 1}
	}
	if !ok {
		j.log.System(sc.logStep, "Skipped: "+name)
		return skip
	}
	j.log.System(sc.logStep, "Run "+strings.TrimPrefix(name, "Run "))

	// When the job was already interrupted (an always() step), give it a
	// fresh, bounded context.
	parent := j.ctx
	limit := time.Duration(st.TimeoutMinutes) * timeUnit
	if j.interrupted() {
		parent = context.WithoutCancel(j.ctx)
		if limit == 0 {
			limit = alwaysStepTimeout
		}
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if limit > 0 {
		var c2 context.CancelFunc
		ctx, c2 = context.WithTimeoutCause(ctx, limit, errStepTimeout)
		defer c2()
	}

	out, code, err := j.execStep(ctx, sc, st)
	res := stepResult{outcome: actions.StatusSuccess, exitCode: code, outputs: out.outputs, summary: out.summary}
	switch {
	case ctx.Err() != nil:
		c := context.Cause(ctx)
		switch {
		case errors.Is(c, errStepTimeout):
			j.log.System(sc.logStep, fmt.Sprintf("Error: the step timed out after %d minutes", st.TimeoutMinutes))
			res.outcome = actions.StatusFailure
		case errors.Is(c, errJobTimeout):
			j.log.System(sc.logStep, "Error: the job timed out")
			res.outcome = actions.StatusFailure
		default:
			j.log.System(sc.logStep, "The step was cancelled")
			res.outcome = actions.StatusCancelled
		}
		if res.exitCode == 0 {
			res.exitCode = -1
		}
	case err != nil:
		j.log.System(sc.logStep, "Error: "+err.Error())
		res.outcome = actions.StatusFailure
		if res.exitCode == 0 {
			res.exitCode = 1
		}
	case code != 0:
		j.log.System(sc.logStep, fmt.Sprintf("Error: process completed with exit code %d", code))
		res.outcome = actions.StatusFailure
	}
	res.conclusion = res.outcome
	if res.outcome == actions.StatusFailure && j.continueOnError(sc, st) {
		j.log.System(sc.logStep, "continue-on-error: the step failed but the job continues")
		res.conclusion = actions.StatusSuccess
	}
	if res.outcome == actions.StatusCancelled {
		res.conclusion = actions.StatusCancelled
	}
	return res
}

func (j *jobRun) continueOnError(sc *scope, st stepDef) bool {
	v := strings.TrimSpace(st.ContinueOnError)
	if v == "" || v == "false" {
		return false
	}
	if v == "true" {
		return true
	}
	r, err := j.eval(sc, nil).Eval(v)
	return err == nil && expr.Truthy(r)
}

// stepOut is what a step produced through workflow commands and files.
type stepOut struct {
	outputs map[string]string
	summary string
	state   map[string]string
}

// execStep prepares env and file commands, runs the step and applies the
// GITHUB_ENV / GITHUB_PATH / GITHUB_OUTPUT results.
func (j *jobRun) execStep(ctx context.Context, sc *scope, st stepDef) (stepOut, int, error) {
	stepEnv, err := j.stepEnv(sc, st)
	if err != nil {
		return stepOut{}, 1, err
	}
	fc, err := j.newFileCmds()
	if err != nil {
		return stepOut{}, 1, err
	}
	defer fc.remove()
	cs := newCmdState()
	var code int
	switch {
	case st.Run != "":
		code, err = j.runScript(ctx, sc, st, stepEnv, fc, cs)
	case st.Uses != "":
		var actOut map[string]string
		actOut, code, err = j.runUses(ctx, sc, st, stepEnv, fc, cs)
		for k, v := range actOut {
			cs.setOutput(k, v)
		}
	default:
		err = errors.New("the step has neither run nor uses")
	}
	out := j.applyFileCmds(sc, fc, cs)
	return out, code, err
}

// stepEnv evaluates the step's own env (and a calling composite step's env).
func (j *jobRun) stepEnv(sc *scope, st stepDef) (envMap, error) {
	e := envMap{}
	for k, v := range sc.parentEnv {
		e.Set(k, v)
	}
	for _, k := range sortedKeys(st.Env) {
		v, err := j.eval(sc, e).Interpolate(st.Env[k])
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		e.Set(k, v)
	}
	return e, nil
}

// pathMapper turns host paths into the paths a process sees.
type pathMapper func(string) string

func hostPaths(p string) string { return p }

// containerPaths maps the job directory into /__w and the workspace to /github/workspace.
func (j *jobRun) containerPaths(p string) string {
	if rel, err := filepath.Rel(j.ws, p); err == nil && !strings.HasPrefix(rel, "..") {
		return strings.TrimSuffix(containerWorkspace+"/"+filepath.ToSlash(rel), "/.")
	}
	if rel, err := filepath.Rel(j.root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return strings.TrimSuffix(containerMountRoot+"/"+filepath.ToSlash(rel), "/.")
	}
	return p
}

// processEnv is the environment a step process gets. On the host it starts
// from the runner's own environment; in a container only the declared vars.
func (j *jobRun) processEnv(sc *scope, stepEnv envMap, fc *fileCmds, mp pathMapper, inContainer bool, actionPath string) envMap {
	e := envMap{}
	if !inContainer {
		e = osEnv()
	}
	server := strings.TrimRight(j.r.Config.ServerURL, "/")
	refName, refType := refNameType(j.job.Ref)
	vars := map[string]string{
		"CI": "true", "GITHUB_ACTIONS": "true", "ATELIER": "true", "ATELIER_ACTIONS": "true",
		"GITHUB_WORKSPACE": mp(j.ws), "GITHUB_SHA": j.job.SHA, "GITHUB_REF": j.job.Ref,
		"GITHUB_REF_NAME": refName, "GITHUB_REF_TYPE": refType,
		"GITHUB_REPOSITORY": j.repoFull(), "GITHUB_REPOSITORY_OWNER": j.job.Repo.Owner,
		"GITHUB_EVENT_NAME": j.job.Event, "GITHUB_EVENT_PATH": mp(j.eventPath),
		"GITHUB_RUN_ID": strconv.FormatInt(j.job.RunID, 10), "GITHUB_JOB": j.job.Name, "GITHUB_WORKFLOW": j.job.Workflow,
		"GITHUB_RUN_NUMBER": ctxString(j.github, "run_number"), "GITHUB_RUN_ATTEMPT": ctxString(j.github, "run_attempt"),
		"GITHUB_ACTOR": ctxString(j.github, "actor"), "GITHUB_HEAD_REF": ctxString(j.github, "head_ref"), "GITHUB_BASE_REF": ctxString(j.github, "base_ref"),
		"GITHUB_SERVER_URL": server, "GITHUB_API_URL": server + "/api/v3", "GITHUB_GRAPHQL_URL": server + "/api/graphql",
		"RUNNER_OS": runnerOS(), "RUNNER_ARCH": runnerArch(), "RUNNER_NAME": j.r.Config.Name, "RUNNER_TEMP": mp(j.temp),
		"RUNNER_TOOL_CACHE": mp(j.toolCache), "RUNNER_WORKSPACE": mp(j.root), "RUNNER_ENVIRONMENT": "self-hosted",
		"ATELIER_WORKSPACE": mp(j.ws), "ATELIER_SHA": j.job.SHA, "ATELIER_REF": j.job.Ref, "ATELIER_REPOSITORY": j.repoFull(),
		"ATELIER_EVENT_NAME": j.job.Event, "ATELIER_RUN_ID": strconv.FormatInt(j.job.RunID, 10),
		"ATELIER_JOB_ID": strconv.FormatInt(j.job.ID, 10), "ATELIER_SERVER_URL": server,
	}
	if inContainer {
		vars["RUNNER_TOOL_CACHE"] = "/opt/hostedtoolcache"
	}
	if fc != nil {
		vars["GITHUB_OUTPUT"] = mp(fc.output)
		vars["GITHUB_ENV"] = mp(fc.env)
		vars["GITHUB_PATH"] = mp(fc.path)
		vars["GITHUB_STEP_SUMMARY"] = mp(fc.summary)
		vars["GITHUB_STATE"] = mp(fc.state)
	}
	if actionPath != "" {
		vars["GITHUB_ACTION_PATH"] = mp(actionPath)
	} else if sc.actionPath != "" {
		vars["GITHUB_ACTION_PATH"] = mp(sc.actionPath)
	}
	for k, v := range vars {
		e.Set(k, v)
	}
	j.mu.Lock()
	for k, v := range j.env {
		e.Set(k, v)
	}
	paths := append([]string(nil), j.paths...)
	j.mu.Unlock()
	for k, v := range stepEnv {
		e.Set(k, v)
	}
	if len(paths) > 0 {
		pk := e.pathKey()
		cur := e.Get(pk)
		if inContainer {
			cur = j.ctr.path
			pk = "PATH"
		}
		sep := string(os.PathListSeparator)
		if inContainer {
			sep = ":"
		}
		mapped := make([]string, len(paths))
		for i, p := range paths {
			mapped[i] = mp(p)
		}
		e.Set(pk, strings.Join(append(mapped, cur), sep))
	}
	return e
}

func (j *jobRun) workingDir(sc *scope, st stepDef, stepEnv envMap) (string, error) {
	wd := j.ws
	if st.WorkingDirectory != "" {
		v, err := j.eval(sc, stepEnv).Interpolate(st.WorkingDirectory)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(v) {
			wd = v
		} else {
			wd = filepath.Join(j.ws, v)
		}
	}
	return wd, nil
}

// runScript runs a `run:` step through its shell, from a temp script file.
func (j *jobRun) runScript(ctx context.Context, sc *scope, st stepDef, stepEnv envMap, fc *fileCmds, cs *cmdState) (int, error) {
	ev := j.eval(sc, stepEnv)
	script, err := ev.Interpolate(st.Run)
	if err != nil {
		return 1, err
	}
	shell, err := ev.Interpolate(st.Shell)
	if err != nil {
		return 1, err
	}
	wd, err := j.workingDir(sc, st, stepEnv)
	if err != nil {
		return 1, err
	}
	inCtr := j.ctr != nil
	mp := pathMapper(hostPaths)
	goos := runtime.GOOS
	if inCtr {
		mp = j.containerPaths
		goos = "linux"
	}
	env := j.processEnv(sc, stepEnv, fc, mp, inCtr, "")
	sh, err := resolveShell(shell, goos, env.Get(env.pathKey()), inCtr)
	if err != nil {
		return 1, err
	}
	if sh.wrap != nil {
		script = sh.wrap(script)
	}
	if sh.ext == ".cmd" || (goos == "windows" && sh.ext == ".ps1") {
		script = strings.ReplaceAll(strings.ReplaceAll(script, "\r\n", "\n"), "\n", "\r\n")
	}
	f, err := os.CreateTemp(j.temp, "step-*"+sh.ext)
	if err != nil {
		return 1, err
	}
	scriptPath := f.Name()
	defer os.Remove(scriptPath)
	_, werr := f.WriteString(script)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return 1, werr
	}
	_ = os.Chmod(scriptPath, 0o755)
	if inCtr {
		sh = sh.withScript(mp(scriptPath))
		return j.ctr.exec(ctx, j, sc.logStep, mp(wd), env, sh.argv, cs)
	}
	sh = sh.withScript(scriptPath)
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return 1, err
	}
	return j.run(ctx, sc.logStep, sh.argv, sh.cmdLine, wd, env, cs)
}

func removeAll(dir string) error {
	err := os.RemoveAll(dir)
	if err != nil {
		// read-only files (e.g. git objects) on Windows
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		err = os.RemoveAll(dir)
	}
	return err
}
