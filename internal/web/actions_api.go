package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
	"atelier/internal/workflow"
)

// The runner API (see internal/actions/contract.go). Runners authenticate
// with a long-lived bearer token whose hash is stored; jobs get a short-lived
// job token for cloning (git over HTTPS) and, when the workflow grants it,
// publishing packages.

const (
	runnerTokenPrefix = "atr_"
	jobTokenPrefix    = "atj_"
	registrationTTL   = time.Hour
	regTokenPrefix    = "atreg_"

	maxRegisterBody  = 64 << 10
	maxPollBody      = 64 << 10
	maxLogsBody      = 4 << 20
	maxStepBody      = 2 << 20
	maxStepSummary   = 1 << 20
	maxFinishBody    = 1 << 20
	maxDiscoveryBody = 2 << 20
	maxLogLines      = 10000
	maxLogLineBytes  = 64 << 10
)

func actionsAPIRoutes(a *App, m *http.ServeMux) {
	p := actions.APIPrefix
	m.HandleFunc("POST "+p+"/register", a.runnerRegister)
	m.HandleFunc("POST "+p+"/poll", a.runnerAuthed(a.runnerPoll))
	m.HandleFunc("POST "+p+"/jobs/{id}/logs", a.runnerAuthed(a.runnerLogs))
	m.HandleFunc("POST "+p+"/jobs/{id}/steps/{index}", a.runnerAuthed(a.runnerStep))
	m.HandleFunc("POST "+p+"/jobs/{id}/finish", a.runnerAuthed(a.runnerFinish))
	m.HandleFunc("POST "+p+"/discovery", a.runnerAuthed(a.runnerDiscovery))
}

func init() {
	registerTokenResolver(func(ctx context.Context, a *App, plain string) (Actor, bool) { return a.jobActor(ctx, plain) })
	registerAccessPolicy(jobAccessPolicy)
}

// ---------- helpers ----------

func actJSONMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func actDecodeJob(payload string) (actions.Job, error) {
	var j actions.Job
	err := json.Unmarshal([]byte(payload), &j)
	return j, err
}

func runnerJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func runnerError(w http.ResponseWriter, code int, msg string) {
	runnerJSON(w, code, map[string]string{"error": msg})
}

// runnerReadJSON decodes a size-limited JSON body.
func runnerReadJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			runnerError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			runnerError(w, http.StatusBadRequest, "invalid JSON")
		}
		return false
	}
	_, _ = io.Copy(io.Discard, r.Body)
	return true
}

// runnerAuthed checks the runner's bearer token.
func (a *App) runnerAuthed(h func(http.ResponseWriter, *http.Request, store.ActionsRunner)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok || !strings.HasPrefix(tok, runnerTokenPrefix) || len(tok) > 200 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="atelier-runner"`)
			runnerError(w, http.StatusUnauthorized, "runner token required")
			return
		}
		runner, err := a.db.ActionsRunnerByToken(r.Context(), store.Hash(tok))
		if err != nil {
			if !a.lim.allow(a.clientIP(r)) {
				runnerError(w, http.StatusTooManyRequests, "too many attempts")
				return
			}
			runnerError(w, http.StatusUnauthorized, "unknown runner token; register the runner again")
			return
		}
		a.rememberBaseURL(a.baseURL(r))
		h(w, r, runner)
	}
}

func (a *App) runnerJob(w http.ResponseWriter, r *http.Request, runner store.ActionsRunner) (store.ActionsJob, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		runnerError(w, http.StatusNotFound, "no such job")
		return store.ActionsJob{}, false
	}
	j, err := a.db.ActionsJob(r.Context(), id)
	if err != nil || j.RunnerID != runner.ID {
		runnerError(w, http.StatusNotFound, "no such job for this runner")
		return store.ActionsJob{}, false
	}
	return j, true
}

// ---------- registration ----------

func (a *App) runnerRegister(w http.ResponseWriter, r *http.Request) {
	var req actions.RegisterRequest
	if !runnerReadJSON(w, r, maxRegisterBody, &req) {
		return
	}
	ip := a.clientIP(r)
	if req.Protocol != 0 && req.Protocol != actions.ProtocolVersion {
		runnerError(w, http.StatusBadRequest, fmt.Sprintf("this server speaks runner protocol %d", actions.ProtocolVersion))
		return
	}
	if !strings.HasPrefix(req.Token, regTokenPrefix) || len(req.Token) > 200 {
		a.lim.allow(ip)
		runnerError(w, http.StatusUnauthorized, "invalid registration token")
		return
	}
	if !a.lim.allow(ip) {
		runnerError(w, http.StatusTooManyRequests, "too many attempts, try again in a minute")
		return
	}
	person, err := a.db.UseRunnerRegistration(r.Context(), store.Hash(req.Token))
	if err != nil {
		runnerError(w, http.StatusUnauthorized, "the registration token is unknown, used or expired; generate a new one in Settings → Runners")
		return
	}
	a.lim.reset(ip)
	name := cleanRunnerText(req.Name, 80)
	if name == "" {
		name = "runner"
	}
	plain := runnerTokenPrefix + store.RandomToken(32)
	rn, err := a.db.CreateActionsRunner(r.Context(), store.ActionsRunner{
		Name: name, OS: cleanRunnerText(req.OS, 20), Arch: cleanRunnerText(req.Arch, 20),
		Labels: runnerCleanLabels(req.Labels), Capabilities: runnerCleanLabels(req.Capabilities), Version: cleanRunnerText(req.Version, 40),
		PersonID: person,
	}, store.Hash(plain))
	if err != nil {
		logErr(r, err)
		runnerError(w, http.StatusInternalServerError, "couldn't register the runner")
		return
	}
	log.Printf("actions: runner %q (%s/%s) registered", rn.Name, rn.OS, rn.Arch)
	runnerJSON(w, http.StatusOK, actions.RegisterResponse{RunnerID: rn.ID, RunnerToken: plain, ServerURL: a.baseURL(r)})
}

func cleanRunnerText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > n {
		s = s[:n]
	}
	return s
}

func runnerCleanLabels(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range in {
		l = cleanRunnerText(l, 60)
		k := strings.ToLower(l)
		if l == "" || seen[k] || len(out) >= 50 {
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	return out
}

// ---------- polling and assignment ----------

func runnerOSLabel(goos string) string {
	switch goos {
	case "darwin":
		return "macos"
	}
	return strings.ToLower(goos)
}

func runnerArchLabel(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "386":
		return "x86"
	}
	return strings.ToLower(goarch)
}

// runnerLabels is everything a runner answers to: its own labels plus
// self-hosted and its OS and architecture.
func runnerLabels(rn store.ActionsRunner) map[string]bool {
	set := map[string]bool{"self-hosted": true}
	for _, l := range rn.Labels {
		set[strings.ToLower(l)] = true
	}
	if rn.OS != "" {
		set[runnerOSLabel(rn.OS)] = true
	}
	if rn.Arch != "" {
		set[runnerArchLabel(rn.Arch)] = true
	}
	return set
}

func runnerHasCap(caps []string, c string) bool {
	for _, x := range caps {
		if strings.EqualFold(x, c) {
			return true
		}
	}
	return false
}

// runnerCanRun reports whether a runner may take a job.
func runnerCanRun(rn store.ActionsRunner, j store.ActionsJob) bool {
	if !rn.Enabled || !runnerHasCap(rn.Capabilities, actions.CapJobs) {
		return false
	}
	if j.NeedsDocker && !runnerHasCap(rn.Capabilities, actions.CapDocker) {
		return false
	}
	labels := runnerLabels(rn)
	for _, l := range j.RunsOn {
		if !labels[strings.ToLower(l)] {
			return false
		}
	}
	return true
}

func (a *App) runnerPoll(w http.ResponseWriter, r *http.Request, rn store.ActionsRunner) {
	var req actions.PollRequest
	if !runnerReadJSON(w, r, maxPollBody, &req) {
		return
	}
	ctx := r.Context()
	rn.Labels, rn.Capabilities = runnerCleanLabels(req.Labels), runnerCleanLabels(req.Capabilities)
	if err := a.db.TouchActionsRunner(ctx, rn.ID, rn.Labels, rn.Capabilities, cleanRunnerText(req.Version, 40), req.Busy); err != nil {
		runnerError(w, http.StatusInternalServerError, "server error")
		return
	}
	if req.Busy {
		runnerJSON(w, http.StatusOK, actions.PollResponse{})
		return
	}
	st := a.actions()
	deadline := time.NewTimer(st.pollWait)
	defer deadline.Stop()
	base := a.baseURL(r)
	for {
		wake := st.waitCh()
		if rn.Enabled {
			if job := a.assignJob(ctx, rn, base); job != nil {
				runnerJSON(w, http.StatusOK, actions.PollResponse{Job: job})
				return
			}
		}
		select {
		case <-wake:
			if fresh, err := a.db.ActionsRunnerByID(ctx, rn.ID); err == nil {
				rn.Enabled = fresh.Enabled
			} else {
				runnerError(w, http.StatusUnauthorized, "the runner was removed")
				return
			}
		case <-deadline.C:
			_ = a.db.SeenActionsRunner(ctx, rn.ID)
			runnerJSON(w, http.StatusOK, actions.PollResponse{})
			return
		case <-ctx.Done():
			return
		}
	}
}

// assignJob claims the oldest queued job this runner can run and returns it
// ready to execute (secrets, token and clone URL filled in).
func (a *App) assignJob(ctx context.Context, rn store.ActionsRunner, base string) *actions.Job {
	st := a.actions()
	for attempt := 0; attempt < 10; attempt++ {
		st.mu.Lock()
		queued, err := a.db.ActionsJobsWithStatus(ctx, actions.StatusQueued)
		if err != nil {
			st.mu.Unlock()
			return nil
		}
		var claimed *store.ActionsJob
		for i := range queued {
			if !runnerCanRun(rn, queued[i]) {
				continue
			}
			ok, err := a.db.ClaimActionsJob(ctx, queued[i].ID, rn.ID, rn.Name)
			if err == nil && ok {
				claimed = &queued[i]
				break
			}
		}
		if claimed == nil {
			st.mu.Unlock()
			return nil
		}
		j, _ := a.db.ActionsJob(ctx, claimed.ID)
		job, errMsg := a.prepareJob(ctx, j, base)
		if job == nil {
			a.finishJobLocked(ctx, j, actions.StatusFailure, nil, errMsg)
			st.mu.Unlock()
			continue
		}
		_ = a.db.ForceAppendActionsLog(ctx, j.ID, actionsSystemLines(fmt.Sprintf("Job assigned to runner %s.", rn.Name)))
		if run, err := a.db.ActionsRunByID(ctx, j.RunID); err == nil {
			if rp, err := a.repoForRun(ctx, run); err == nil {
				a.updateRunStatusLocked(ctx, rp, run.ID)
			}
		}
		st.mu.Unlock()
		st.signal()
		return job
	}
	return nil
}

// prepareJob adds the clone URL, a fresh job token and the secrets.
func (a *App) prepareJob(ctx context.Context, j store.ActionsJob, base string) (*actions.Job, string) {
	pj, err := actDecodeJob(j.Payload)
	if err != nil {
		return nil, "The job couldn't be prepared."
	}
	run, err := a.db.ActionsRunByID(ctx, j.RunID)
	if err != nil {
		return nil, "The run is gone."
	}
	rp, err := a.repoForRun(ctx, run)
	if err != nil {
		return nil, "The repository is gone."
	}
	pj.ID, pj.RunID = j.ID, j.RunID
	pj.CloneURL = strings.TrimRight(base, "/") + "/" + rp.FullName() + ".git"
	if workflow.IsAgenticFile(pj.Workflow) {
		// Before the token exists: the agent record decides the token's MCP scopes.
		if msg := a.prepareAgentJob(ctx, &pj, j, run, rp, base); msg != "" {
			return nil, msg
		}
	}
	person := run.PersonID
	if person == 0 {
		if me, err := a.db.PersonalOwner(ctx); err == nil {
			person = me.ID
		}
	}
	plain := jobTokenPrefix + store.RandomToken(32)
	exp := time.Now().Add(time.Duration(j.TimeoutMinutes)*time.Minute + 15*time.Minute)
	if err := a.db.SetActionsJobToken(ctx, store.ActionsJobToken{JobID: j.ID, RepoID: rp.ID, PersonID: person,
		PackagesWrite: j.PackagesWrite, ExpiresAt: exp}, store.Hash(plain)); err != nil {
		return nil, "The job token couldn't be created."
	}
	pj.JobToken = plain
	var names []string
	wantToken := false
	for _, n := range j.SecretNames {
		switch strings.ToUpper(n) {
		case "GITHUB_TOKEN", "ATELIER_TOKEN":
			wantToken = true
		default:
			names = append(names, n)
		}
	}
	secrets := map[string]string{}
	if len(names) > 0 {
		got, err := actions.Secrets().Resolve(ctx, rp.ID, j.Environment, names)
		if err != nil {
			log.Printf("actions: resolving secrets for job %d: %v", j.ID, err)
			return nil, "The job's secrets couldn't be loaded."
		}
		for k, v := range got {
			secrets[k] = v
		}
	}
	if wantToken {
		secrets["GITHUB_TOKEN"], secrets["ATELIER_TOKEN"] = plain, plain
	}
	if len(secrets) > 0 {
		pj.Secrets = secrets
	}
	return &pj, ""
}

// ---------- logs, steps, finish ----------

func (a *App) runnerLogs(w http.ResponseWriter, r *http.Request, rn store.ActionsRunner) {
	j, ok := a.runnerJob(w, r, rn)
	if !ok {
		return
	}
	var req actions.LogsRequest
	if !runnerReadJSON(w, r, maxLogsBody, &req) {
		return
	}
	ctx := r.Context()
	_ = a.db.SeenActionsRunner(ctx, rn.ID)
	if j.Status != actions.StatusRunning {
		runnerJSON(w, http.StatusOK, actions.LogsResponse{Cancel: true})
		return
	}
	if len(req.Lines) > maxLogLines {
		req.Lines = req.Lines[:maxLogLines]
	}
	steps := map[int]time.Time{}
	for i := range req.Lines {
		l := &req.Lines[i]
		if len(l.Text) > maxLogLineBytes {
			l.Text = l.Text[:maxLogLineBytes] + "… (line truncated)"
		}
		switch l.Stream {
		case "stdout", "stderr", "system":
		default:
			l.Stream = "stdout"
		}
		if l.Time.IsZero() {
			l.Time = time.Now().UTC()
		}
		if l.Step >= 0 {
			if _, seen := steps[l.Step]; !seen {
				steps[l.Step] = l.Time
			}
		} else {
			l.Step = -1
		}
	}
	if len(req.Lines) > 0 {
		b, _ := json.Marshal(req.Lines)
		st := a.actions()
		stored, err := a.db.AppendActionsLog(ctx, j.ID, string(b), int64(len(b)), st.maxLogBytes)
		if err != nil {
			runnerError(w, http.StatusInternalServerError, "server error")
			return
		}
		if !stored && j.LogBytes <= st.maxLogBytes {
			_ = a.db.ForceAppendActionsLog(ctx, j.ID, actionsSystemLines(fmt.Sprintf("The log reached its %d MB limit; later output isn't kept.", st.maxLogBytes>>20)))
			_ = a.db.MarkActionsLogFull(ctx, j.ID, st.maxLogBytes)
		}
		for idx, at := range steps {
			_ = a.db.MarkActionsStepRunning(ctx, j.ID, idx, at)
		}
		st.signal()
	}
	runnerJSON(w, http.StatusOK, actions.LogsResponse{Cancel: j.CancelRequested})
}

func actValidStepStatus(s string) bool {
	switch s {
	case actions.StatusSuccess, actions.StatusFailure, actions.StatusCancelled, actions.StatusSkipped, actions.StatusRunning:
		return true
	}
	return false
}

func (a *App) runnerStep(w http.ResponseWriter, r *http.Request, rn store.ActionsRunner) {
	j, ok := a.runnerJob(w, r, rn)
	if !ok {
		return
	}
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || idx < 0 || idx > 10000 {
		runnerError(w, http.StatusNotFound, "no such step")
		return
	}
	var res actions.StepResult
	if !runnerReadJSON(w, r, maxStepBody, &res) {
		return
	}
	ctx := r.Context()
	if j.Status != actions.StatusRunning {
		runnerError(w, http.StatusConflict, "the job isn't running")
		return
	}
	if !actValidStepStatus(res.Status) {
		runnerError(w, http.StatusBadRequest, "unknown step status")
		return
	}
	steps, _ := a.db.ActionsSteps(ctx, j.ID)
	name := ""
	found := false
	for _, s := range steps {
		if s.Index == idx {
			name, found = s.Name, true
		}
	}
	if !found {
		runnerError(w, http.StatusNotFound, "no such step")
		return
	}
	summary := res.Summary
	if len(summary) > maxStepSummary {
		summary = summary[:maxStepSummary] + "\n\n… (summary truncated)"
	}
	conclusion := ""
	if actValidStepStatus(res.Conclusion) {
		conclusion = res.Conclusion
	}
	if err := a.db.SetActionsStep(ctx, store.ActionsStep{JobID: j.ID, Index: idx, Name: name, Status: res.Status,
		Conclusion: conclusion, Summary: summary, ExitCode: res.ExitCode, StartedAt: res.StartedAt, FinishedAt: res.FinishedAt}); err != nil {
		runnerError(w, http.StatusInternalServerError, "server error")
		return
	}
	_ = a.db.SeenActionsRunner(ctx, rn.ID)
	a.actions().signal()
	runnerJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) runnerFinish(w http.ResponseWriter, r *http.Request, rn store.ActionsRunner) {
	j, ok := a.runnerJob(w, r, rn)
	if !ok {
		return
	}
	var req actions.FinishRequest
	if !runnerReadJSON(w, r, maxFinishBody, &req) {
		return
	}
	ctx := r.Context()
	_ = a.db.SeenActionsRunner(ctx, rn.ID)
	outputs := map[string]string{}
	total := 0
	for k, v := range req.Outputs {
		if len(outputs) >= 100 || len(k) > 200 {
			break
		}
		total += len(k) + len(v)
		if total > 512<<10 {
			break
		}
		outputs[k] = v
	}
	errMsg := cleanRunnerText(req.Error, 2000)
	st := a.actions()
	st.mu.Lock()
	j, err := a.db.ActionsJob(ctx, j.ID)
	if err == nil && j.Status == actions.StatusRunning {
		if errMsg != "" {
			_ = a.db.ForceAppendActionsLog(ctx, j.ID, actionsSystemLines("Runner error: "+errMsg))
		}
		a.finishJobLocked(ctx, j, req.Status, outputs, errMsg)
	}
	st.mu.Unlock()
	runnerJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) runnerDiscovery(w http.ResponseWriter, r *http.Request, rn store.ActionsRunner) {
	var req actions.DiscoveryRequest
	if !runnerReadJSON(w, r, maxDiscoveryBody, &req) {
		return
	}
	if len(req.Repos) > 5000 {
		req.Repos = req.Repos[:5000]
	}
	b, _ := json.Marshal(req.Repos)
	if err := a.db.SetActionsRunnerDiscovery(r.Context(), rn.ID, string(b)); err != nil {
		runnerError(w, http.StatusInternalServerError, "server error")
		return
	}
	runnerJSON(w, http.StatusOK, map[string]int{"repos": len(req.Repos)})
}

// ---------- job tokens ----------

// jobActor resolves a job token (the git password the runner uses, or a
// bearer token other features accept) to an Actor of kind "job". Its scopes:
//
//	repo:read                read the job's repository (and atelier: actions it uses)
//	actions:repo:<id>        a repository the job may read
//	actions:job:<id>         the job
//	actions:owner:<id>       the account owning the job's repository
//	packages:read            read that account's packages
//	packages:write           the workflow granted `permissions: packages: write`
//
// Other features (packages, releases) call it to accept job tokens.
func (a *App) jobActor(ctx context.Context, plain string) (Actor, bool) {
	if !strings.HasPrefix(plain, jobTokenPrefix) || len(plain) > 200 {
		return Actor{}, false
	}
	t, err := a.db.ActionsJobTokenByHash(ctx, store.Hash(plain))
	if err != nil {
		return Actor{}, false
	}
	j, err := a.db.ActionsJob(ctx, t.JobID)
	if err != nil || j.Status != actions.StatusRunning {
		return Actor{}, false
	}
	scopes := []string{"repo:read", fmt.Sprintf("actions:repo:%d", t.RepoID), fmt.Sprintf("actions:job:%d", t.JobID), "packages:read"}
	if t.PackagesWrite {
		scopes = append(scopes, "packages:write")
	}
	// Agentic jobs: mcp:<tool> for each allowed tool, and an agent name.
	if who, ok := a.agentJobActor(ctx, t, scopes); ok {
		return who, true
	}
	if rp, err := a.repoByIDForActions(ctx, t.RepoID); err == nil {
		scopes = append(scopes, fmt.Sprintf("actions:owner:%d", rp.OwnerID))
	}
	// Actions from other repositories on this server (uses: atelier:owner/repo@ref).
	if run, err := a.db.ActionsRunByID(ctx, j.RunID); err == nil {
		for _, pinned := range run.Pins {
			if r, err := workflow.ParseUses(pinned); err == nil && r.Kind == workflow.UsesAtelier {
				if rp, err := a.db.RepoByName(ctx, r.Owner, r.Repo); err == nil {
					scopes = append(scopes, fmt.Sprintf("actions:repo:%d", rp.ID))
				}
			}
		}
	}
	return Actor{Kind: ActorJob, Name: fmt.Sprintf("actions-job-%d", t.JobID), PersonID: t.PersonID, Scopes: scopes}, true
}

// jobRepoID returns the repository a job actor belongs to (0 if not a job).
func jobRepoID(who Actor) int64 {
	if who.Kind != ActorJob || len(who.Scopes) < 2 {
		return 0
	}
	id, _ := strconv.ParseInt(strings.TrimPrefix(who.Scopes[1], "actions:repo:"), 10, 64)
	return id
}

// jobAccessPolicy: a job token reads its own repository (and the repositories
// of atelier: actions it uses) and public ones. It never writes or
// administers repositories. For packages (canPackage checks a stand-in repo
// with ID 0 for the owning account) it reads the job repository owner's
// packages, and writes them when the workflow granted packages: write.
func jobAccessPolicy(ctx context.Context, a *App, who Actor, repo store.Repo, need Perm) (bool, bool) {
	if who.Kind != ActorJob {
		return false, false
	}
	if repo.ID == 0 {
		if !who.hasScope(fmt.Sprintf("actions:owner:%d", repo.OwnerID)) {
			return need == PermRead && !repo.Private, true
		}
		switch need {
		case PermRead:
			return who.hasScope("packages:read") || who.hasScope("packages:write"), true
		case PermWrite:
			return who.hasScope("packages:write"), true
		}
		return false, true
	}
	if need != PermRead {
		return false, true
	}
	return !repo.Private || who.hasScope(fmt.Sprintf("actions:repo:%d", repo.ID)), true
}
