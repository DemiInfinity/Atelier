package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Actions: workflow runs, their jobs and steps, job logs, self-hosted
// runners and their tokens, and the queue of pushes recorded by the
// post-receive hook. The scheduling logic lives in internal/web/actions*.go.
//
// Times are unix seconds (0 = not yet). Lists and maps are stored as JSON.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS actions_runs (
  id                 INTEGER PRIMARY KEY,
  repo_id            INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  seq                INTEGER NOT NULL,
  workflow_path      TEXT NOT NULL,
  workflow_name      TEXT NOT NULL,
  run_number         INTEGER NOT NULL,
  event              TEXT NOT NULL,
  ref                TEXT NOT NULL DEFAULT '',
  sha                TEXT NOT NULL DEFAULT '',
  title              TEXT NOT NULL DEFAULT '',
  actor_kind         TEXT NOT NULL DEFAULT 'owner',
  actor_name         TEXT NOT NULL DEFAULT '',
  person_id          INTEGER REFERENCES owners(id) ON DELETE SET NULL,
  status             TEXT NOT NULL,
  conclusion         TEXT NOT NULL DEFAULT '',
  attempt            INTEGER NOT NULL DEFAULT 1,
  inputs             TEXT NOT NULL DEFAULT '{}',
  event_payload      TEXT NOT NULL DEFAULT '{}',
  warnings           TEXT NOT NULL DEFAULT '[]',
  errors             TEXT NOT NULL DEFAULT '[]',
  pins               TEXT NOT NULL DEFAULT '{}',
  lock_resolved      TEXT NOT NULL DEFAULT '{}',
  lock_generated     TEXT NOT NULL DEFAULT '',
  concurrency_group  TEXT NOT NULL DEFAULT '',
  cancel_in_progress INTEGER NOT NULL DEFAULT 0,
  pull_number        INTEGER NOT NULL DEFAULT 0,
  created_at         INTEGER NOT NULL,
  started_at         INTEGER NOT NULL DEFAULT 0,
  finished_at        INTEGER NOT NULL DEFAULT 0,
  updated_at         INTEGER NOT NULL,
  UNIQUE (repo_id, seq)
);
CREATE INDEX IF NOT EXISTS actions_runs_sha ON actions_runs (repo_id, sha);
CREATE INDEX IF NOT EXISTS actions_runs_status ON actions_runs (status);
CREATE INDEX IF NOT EXISTS actions_runs_workflow ON actions_runs (repo_id, workflow_path, run_number);

CREATE TABLE IF NOT EXISTS actions_jobs (
  id                INTEGER PRIMARY KEY,
  run_id            INTEGER NOT NULL REFERENCES actions_runs(id) ON DELETE CASCADE,
  repo_id           INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  job_key           TEXT NOT NULL,
  name              TEXT NOT NULL,
  matrix            TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL,
  runs_on           TEXT NOT NULL DEFAULT '[]',
  environment       TEXT NOT NULL DEFAULT '',
  needs_docker      INTEGER NOT NULL DEFAULT 0,
  approval          TEXT NOT NULL DEFAULT '',
  approved_by       TEXT NOT NULL DEFAULT '',
  runner_id         INTEGER,
  runner_name       TEXT NOT NULL DEFAULT '',
  payload           TEXT NOT NULL DEFAULT '',
  secret_names      TEXT NOT NULL DEFAULT '[]',
  outputs           TEXT NOT NULL DEFAULT '{}',
  error             TEXT NOT NULL DEFAULT '',
  timeout_minutes   INTEGER NOT NULL DEFAULT 360,
  continue_on_error INTEGER NOT NULL DEFAULT 0,
  packages_write    INTEGER NOT NULL DEFAULT 0,
  cancel_requested  INTEGER NOT NULL DEFAULT 0,
  log_bytes         INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  queued_at         INTEGER NOT NULL DEFAULT 0,
  started_at        INTEGER NOT NULL DEFAULT 0,
  finished_at       INTEGER NOT NULL DEFAULT 0,
  updated_at        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS actions_jobs_run ON actions_jobs (run_id, id);
CREATE INDEX IF NOT EXISTS actions_jobs_status ON actions_jobs (status, queued_at);

CREATE TABLE IF NOT EXISTS actions_steps (
  job_id      INTEGER NOT NULL REFERENCES actions_jobs(id) ON DELETE CASCADE,
  idx         INTEGER NOT NULL,
  name        TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'queued',
  conclusion  TEXT NOT NULL DEFAULT '',
  summary     TEXT NOT NULL DEFAULT '',
  exit_code   INTEGER NOT NULL DEFAULT 0,
  started_at  INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (job_id, idx)
);

CREATE TABLE IF NOT EXISTS actions_logs (
  id         INTEGER PRIMARY KEY,
  job_id     INTEGER NOT NULL REFERENCES actions_jobs(id) ON DELETE CASCADE,
  lines      TEXT NOT NULL,
  bytes      INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS actions_logs_job ON actions_logs (job_id, id);

CREATE TABLE IF NOT EXISTS actions_runners (
  id               INTEGER PRIMARY KEY,
  name             TEXT NOT NULL,
  os               TEXT NOT NULL DEFAULT '',
  arch             TEXT NOT NULL DEFAULT '',
  labels           TEXT NOT NULL DEFAULT '[]',
  capabilities     TEXT NOT NULL DEFAULT '[]',
  version          TEXT NOT NULL DEFAULT '',
  token_hash       TEXT NOT NULL UNIQUE,
  person_id        INTEGER REFERENCES owners(id) ON DELETE CASCADE,
  enabled          INTEGER NOT NULL DEFAULT 1,
  busy             INTEGER NOT NULL DEFAULT 0,
  last_seen        INTEGER NOT NULL DEFAULT 0,
  last_job_id      INTEGER NOT NULL DEFAULT 0,
  offline_notified INTEGER NOT NULL DEFAULT 0,
  discovery        TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS actions_runner_tokens (
  id         INTEGER PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  person_id  INTEGER REFERENCES owners(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS actions_job_tokens (
  job_id         INTEGER PRIMARY KEY REFERENCES actions_jobs(id) ON DELETE CASCADE,
  token_hash     TEXT NOT NULL UNIQUE,
  repo_id        INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  person_id      INTEGER,
  packages_write INTEGER NOT NULL DEFAULT 0,
  expires_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS actions_push_queue (
  id         INTEGER PRIMARY KEY,
  owner      TEXT NOT NULL,
  repo       TEXT NOT NULL,
  old_sha    TEXT NOT NULL,
  new_sha    TEXT NOT NULL,
  ref        TEXT NOT NULL,
  pusher     TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS actions_repo_settings (
  repo_id          INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  require_lockfile INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS branch_rule_checks (
  rule_id INTEGER NOT NULL REFERENCES branch_rules(id) ON DELETE CASCADE,
  name    TEXT NOT NULL,
  PRIMARY KEY (rule_id, name)
);`)
}

func actUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func actTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func actJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func actUnJSON(s string, v any) {
	if s != "" {
		_ = json.Unmarshal([]byte(s), v)
	}
}

// ---------- runs ----------

// Run statuses beyond the job statuses in package actions.
const (
	RunPending   = "pending" // held back by a concurrency group
	RunQueued    = "queued"
	RunWaiting   = "waiting"
	RunRunning   = "running"
	RunCompleted = "completed"
)

type ActionsRun struct {
	ID               int64
	RepoID           int64
	Seq              int64 // per-repository number used in URLs
	WorkflowPath     string
	WorkflowName     string
	RunNumber        int64 // per-workflow number shown as #n
	Event            string
	Ref              string
	SHA              string
	Title            string
	ActorKind        string
	ActorName        string
	PersonID         int64
	Status           string
	Conclusion       string
	Attempt          int
	Inputs           map[string]string
	EventPayload     map[string]any
	Warnings         []string
	Errors           []string
	Pins             map[string]string
	LockResolved     map[string]string
	LockGenerated    string
	ConcurrencyGroup string
	CancelInProgress bool
	PullNumber       int64
	CreatedAt        time.Time
	StartedAt        time.Time
	FinishedAt       time.Time
	UpdatedAt        time.Time
}

// Done reports whether the run has finished.
func (r ActionsRun) Done() bool { return r.Status == RunCompleted }

// Outcome is the conclusion when done, otherwise the status.
func (r ActionsRun) Outcome() string {
	if r.Done() {
		return r.Conclusion
	}
	return r.Status
}

// Branch is the short ref name (branch or tag).
func (r ActionsRun) Branch() string {
	if b, ok := strings.CutPrefix(r.Ref, "refs/heads/"); ok {
		return b
	}
	if t, ok := strings.CutPrefix(r.Ref, "refs/tags/"); ok {
		return t
	}
	return r.Ref
}

// Duration is how long the run took (or has taken so far).
func (r ActionsRun) Duration() time.Duration {
	if r.StartedAt.IsZero() {
		return 0
	}
	end := r.FinishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(r.StartedAt)
}

const actionsRunCols = `id, repo_id, seq, workflow_path, workflow_name, run_number, event, ref, sha, title, actor_kind, actor_name,
COALESCE(person_id, 0), status, conclusion, attempt, inputs, event_payload, warnings, errors, pins, lock_resolved, lock_generated,
concurrency_group, cancel_in_progress, pull_number, created_at, started_at, finished_at, updated_at`

func scanActionsRun(row interface{ Scan(...any) error }) (ActionsRun, error) {
	var r ActionsRun
	var inputs, payload, warnings, errs, pins, resolved string
	var c, st, fin, up int64
	err := row.Scan(&r.ID, &r.RepoID, &r.Seq, &r.WorkflowPath, &r.WorkflowName, &r.RunNumber, &r.Event, &r.Ref, &r.SHA,
		&r.Title, &r.ActorKind, &r.ActorName, &r.PersonID, &r.Status, &r.Conclusion, &r.Attempt, &inputs, &payload,
		&warnings, &errs, &pins, &resolved, &r.LockGenerated, &r.ConcurrencyGroup, &r.CancelInProgress, &r.PullNumber,
		&c, &st, &fin, &up)
	if err != nil {
		return r, err
	}
	actUnJSON(inputs, &r.Inputs)
	actUnJSON(payload, &r.EventPayload)
	actUnJSON(warnings, &r.Warnings)
	actUnJSON(errs, &r.Errors)
	actUnJSON(pins, &r.Pins)
	actUnJSON(resolved, &r.LockResolved)
	r.CreatedAt, r.StartedAt, r.FinishedAt, r.UpdatedAt = time.Unix(c, 0), actTime(st), actTime(fin), time.Unix(up, 0)
	return r, nil
}

// CreateActionsRun stores a run and its first jobs, numbering the run per
// repository (Seq) and per workflow (RunNumber).
func (s *Store) CreateActionsRun(ctx context.Context, r ActionsRun, jobs []ActionsJob) (ActionsRun, []ActionsJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, nil, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM actions_runs WHERE repo_id=?`, r.RepoID).Scan(&r.Seq); err != nil {
		return r, nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(run_number), 0) + 1 FROM actions_runs WHERE repo_id=? AND workflow_path=?`,
		r.RepoID, r.WorkflowPath).Scan(&r.RunNumber); err != nil {
		return r, nil, err
	}
	t := now()
	if r.Attempt == 0 {
		r.Attempt = 1
	}
	var person any
	if r.PersonID != 0 {
		person = r.PersonID
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO actions_runs (repo_id, seq, workflow_path, workflow_name, run_number, event, ref, sha,
		title, actor_kind, actor_name, person_id, status, conclusion, attempt, inputs, event_payload, warnings, errors, pins,
		lock_resolved, lock_generated, concurrency_group, cancel_in_progress, pull_number, created_at, started_at, finished_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RepoID, r.Seq, r.WorkflowPath, r.WorkflowName, r.RunNumber, r.Event, r.Ref, r.SHA, r.Title, r.ActorKind, r.ActorName,
		person, r.Status, r.Conclusion, r.Attempt, actJSON(actMap(r.Inputs)), actJSON(actAny(r.EventPayload)),
		actJSON(actList(r.Warnings)), actJSON(actList(r.Errors)), actJSON(actMap(r.Pins)), actJSON(actMap(r.LockResolved)),
		r.LockGenerated, r.ConcurrencyGroup, r.CancelInProgress, r.PullNumber, t, actUnix(r.StartedAt), actUnix(r.FinishedAt), t)
	if err != nil {
		return r, nil, err
	}
	r.ID, _ = res.LastInsertId()
	r.CreatedAt, r.UpdatedAt = time.Unix(t, 0), time.Unix(t, 0)
	out := make([]ActionsJob, 0, len(jobs))
	for _, j := range jobs {
		j.RunID, j.RepoID = r.ID, r.RepoID
		if j.ID, err = insertActionsJob(ctx, tx, j); err != nil {
			return r, nil, err
		}
		out = append(out, j)
	}
	return r, out, tx.Commit()
}

func actMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func actAny(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func actList(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// SaveActionsRun writes a run's mutable fields.
func (s *Store) SaveActionsRun(ctx context.Context, r ActionsRun) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_runs SET status=?, conclusion=?, attempt=?, warnings=?, errors=?,
		started_at=?, finished_at=?, updated_at=? WHERE id=?`,
		r.Status, r.Conclusion, r.Attempt, actJSON(actList(r.Warnings)), actJSON(actList(r.Errors)),
		actUnix(r.StartedAt), actUnix(r.FinishedAt), now(), r.ID)
	return err
}

func (s *Store) ActionsRunByID(ctx context.Context, id int64) (ActionsRun, error) {
	r, err := scanActionsRun(s.db.QueryRowContext(ctx, `SELECT `+actionsRunCols+` FROM actions_runs WHERE id=?`, id))
	return r, notFound(err)
}

func (s *Store) ActionsRunBySeq(ctx context.Context, repoID, seq int64) (ActionsRun, error) {
	r, err := scanActionsRun(s.db.QueryRowContext(ctx, `SELECT `+actionsRunCols+` FROM actions_runs WHERE repo_id=? AND seq=?`, repoID, seq))
	return r, notFound(err)
}

// ActionsRunFilter narrows ActionsRuns. Zero values match everything.
type ActionsRunFilter struct {
	RepoID   int64
	Workflow string
	Status   string // a run status, a conclusion, or "in_progress" (not completed)
	Branch   string
	SHA      string
	Group    string // concurrency group
	Limit    int
	Offset   int
}

func (s *Store) ActionsRuns(ctx context.Context, f ActionsRunFilter) ([]ActionsRun, error) {
	q := `SELECT ` + actionsRunCols + ` FROM actions_runs WHERE 1=1`
	var args []any
	if f.RepoID != 0 {
		q += ` AND repo_id=?`
		args = append(args, f.RepoID)
	}
	if f.Workflow != "" {
		q += ` AND workflow_path=?`
		args = append(args, f.Workflow)
	}
	switch f.Status {
	case "":
	case "in_progress":
		q += ` AND status<>'completed'`
	case RunPending, RunQueued, RunWaiting, RunRunning, RunCompleted:
		q += ` AND status=?`
		args = append(args, f.Status)
	default:
		q += ` AND status='completed' AND conclusion=?`
		args = append(args, f.Status)
	}
	if f.Branch != "" {
		q += ` AND (ref=? OR ref=?)`
		args = append(args, "refs/heads/"+f.Branch, "refs/tags/"+f.Branch)
	}
	if f.SHA != "" {
		q += ` AND sha=?`
		args = append(args, f.SHA)
	}
	if f.Group != "" {
		q += ` AND concurrency_group=?`
		args = append(args, f.Group)
	}
	q += ` ORDER BY id DESC`
	if f.Limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, f.Limit, f.Offset)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionsRun
	for rows.Next() {
		r, err := scanActionsRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActionsRunBranches lists the branches runs were made for, newest first.
func (s *Store) ActionsRunBranches(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ref FROM actions_runs WHERE repo_id=? GROUP BY ref ORDER BY MAX(id) DESC LIMIT 50`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out = append(out, ActionsRun{Ref: ref}.Branch())
	}
	return out, rows.Err()
}

// ---------- jobs ----------

// JobPending: waiting for the jobs it needs (internal; never sent to runners).
const JobPending = "pending"

type ActionsJob struct {
	ID              int64
	RunID           int64
	RepoID          int64
	JobKey          string // the job's id in the workflow
	Name            string
	Matrix          string // JSON object of the combination, "" without a matrix, "?" before a dynamic matrix is expanded
	Status          string
	RunsOn          []string
	Environment     string
	NeedsDocker     bool
	Approval        string // why it waits for approval
	ApprovedBy      string
	RunnerID        int64
	RunnerName      string
	Payload         string // the actions.Job sent to the runner, without secrets or tokens
	SecretNames     []string
	Outputs         map[string]string
	Error           string
	TimeoutMinutes  int
	ContinueOnError bool
	PackagesWrite   bool
	CancelRequested bool
	LogBytes        int64
	CreatedAt       time.Time
	QueuedAt        time.Time
	StartedAt       time.Time
	FinishedAt      time.Time
	UpdatedAt       time.Time
}

// Finished reports whether the job reached a final status.
func (j ActionsJob) Finished() bool {
	switch j.Status {
	case "success", "failure", "cancelled", "skipped":
		return true
	}
	return false
}

// Duration is how long the job ran (or has run so far).
func (j ActionsJob) Duration() time.Duration {
	if j.StartedAt.IsZero() {
		return 0
	}
	end := j.FinishedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(j.StartedAt)
}

const actionsJobCols = `id, run_id, repo_id, job_key, name, matrix, status, runs_on, environment, needs_docker, approval, approved_by,
COALESCE(runner_id, 0), runner_name, payload, secret_names, outputs, error, timeout_minutes, continue_on_error, packages_write,
cancel_requested, log_bytes, created_at, queued_at, started_at, finished_at, updated_at`

func scanActionsJob(row interface{ Scan(...any) error }) (ActionsJob, error) {
	var j ActionsJob
	var runsOn, secrets, outputs string
	var c, q, st, fin, up int64
	err := row.Scan(&j.ID, &j.RunID, &j.RepoID, &j.JobKey, &j.Name, &j.Matrix, &j.Status, &runsOn, &j.Environment,
		&j.NeedsDocker, &j.Approval, &j.ApprovedBy, &j.RunnerID, &j.RunnerName, &j.Payload, &secrets, &outputs, &j.Error,
		&j.TimeoutMinutes, &j.ContinueOnError, &j.PackagesWrite, &j.CancelRequested, &j.LogBytes, &c, &q, &st, &fin, &up)
	if err != nil {
		return j, err
	}
	actUnJSON(runsOn, &j.RunsOn)
	actUnJSON(secrets, &j.SecretNames)
	actUnJSON(outputs, &j.Outputs)
	j.CreatedAt, j.QueuedAt, j.StartedAt, j.FinishedAt, j.UpdatedAt = time.Unix(c, 0), actTime(q), actTime(st), actTime(fin), time.Unix(up, 0)
	return j, nil
}

type actExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertActionsJob(ctx context.Context, x actExecer, j ActionsJob) (int64, error) {
	t := now()
	var runner any
	if j.RunnerID != 0 {
		runner = j.RunnerID
	}
	res, err := x.ExecContext(ctx, `INSERT INTO actions_jobs (run_id, repo_id, job_key, name, matrix, status, runs_on, environment,
		needs_docker, approval, approved_by, runner_id, runner_name, payload, secret_names, outputs, error, timeout_minutes,
		continue_on_error, packages_write, cancel_requested, log_bytes, created_at, queued_at, started_at, finished_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.RunID, j.RepoID, j.JobKey, j.Name, j.Matrix, j.Status, actJSON(actList(j.RunsOn)), j.Environment, j.NeedsDocker,
		j.Approval, j.ApprovedBy, runner, j.RunnerName, j.Payload, actJSON(actList(j.SecretNames)), actJSON(actMap(j.Outputs)),
		j.Error, j.TimeoutMinutes, j.ContinueOnError, j.PackagesWrite, j.CancelRequested, j.LogBytes, t,
		actUnix(j.QueuedAt), actUnix(j.StartedAt), actUnix(j.FinishedAt), t)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddActionsJobs adds jobs to an existing run (a dynamic matrix being expanded).
func (s *Store) AddActionsJobs(ctx context.Context, runID, repoID int64, jobs []ActionsJob) ([]ActionsJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for i := range jobs {
		jobs[i].RunID, jobs[i].RepoID = runID, repoID
		if jobs[i].ID, err = insertActionsJob(ctx, tx, jobs[i]); err != nil {
			return nil, err
		}
	}
	return jobs, tx.Commit()
}

// SaveActionsJob writes a job's mutable fields.
func (s *Store) SaveActionsJob(ctx context.Context, j ActionsJob) error {
	var runner any
	if j.RunnerID != 0 {
		runner = j.RunnerID
	}
	_, err := s.db.ExecContext(ctx, `UPDATE actions_jobs SET name=?, matrix=?, status=?, runs_on=?, environment=?, needs_docker=?,
		approval=?, approved_by=?, runner_id=?, runner_name=?, payload=?, secret_names=?, outputs=?, error=?, timeout_minutes=?,
		continue_on_error=?, packages_write=?, cancel_requested=?, queued_at=?, started_at=?, finished_at=?, updated_at=? WHERE id=?`,
		j.Name, j.Matrix, j.Status, actJSON(actList(j.RunsOn)), j.Environment, j.NeedsDocker, j.Approval, j.ApprovedBy, runner,
		j.RunnerName, j.Payload, actJSON(actList(j.SecretNames)), actJSON(actMap(j.Outputs)), j.Error, j.TimeoutMinutes,
		j.ContinueOnError, j.PackagesWrite, j.CancelRequested, actUnix(j.QueuedAt), actUnix(j.StartedAt),
		actUnix(j.FinishedAt), now(), j.ID)
	return err
}

func (s *Store) ActionsJob(ctx context.Context, id int64) (ActionsJob, error) {
	j, err := scanActionsJob(s.db.QueryRowContext(ctx, `SELECT `+actionsJobCols+` FROM actions_jobs WHERE id=?`, id))
	return j, notFound(err)
}

func (s *Store) queryActionsJobs(ctx context.Context, where string, args ...any) ([]ActionsJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+actionsJobCols+` FROM actions_jobs WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionsJob
	for rows.Next() {
		j, err := scanActionsJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ActionsJobs lists a run's jobs in creation order.
func (s *Store) ActionsJobs(ctx context.Context, runID int64) ([]ActionsJob, error) {
	return s.queryActionsJobs(ctx, `run_id=? ORDER BY id`, runID)
}

// ActionsJobsWithStatus lists jobs in one status across all runs, oldest queued first.
func (s *Store) ActionsJobsWithStatus(ctx context.Context, status string) ([]ActionsJob, error) {
	return s.queryActionsJobs(ctx, `status=? ORDER BY queued_at, id`, status)
}

// ActionsJobsForSHA lists the jobs of every run for a commit, newest run first.
func (s *Store) ActionsJobsForSHA(ctx context.Context, repoID int64, sha string) ([]ActionsJob, error) {
	return s.queryActionsJobs(ctx, `run_id IN (SELECT id FROM actions_runs WHERE repo_id=? AND sha=?) ORDER BY run_id DESC, id`, repoID, sha)
}

// ClaimActionsJob assigns a queued job to a runner. It reports false when
// another runner got there first.
func (s *Store) ClaimActionsJob(ctx context.Context, jobID, runnerID int64, runnerName string) (bool, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `UPDATE actions_jobs SET status='running', runner_id=?, runner_name=?, started_at=?,
		updated_at=? WHERE id=? AND status='queued'`, runnerID, runnerName, t, t, jobID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		_, _ = s.db.ExecContext(ctx, `UPDATE actions_runners SET last_job_id=?, busy=1 WHERE id=?`, jobID, runnerID)
	}
	return n == 1, nil
}

// RequestJobCancel flags a job so its runner stops it at the next log upload.
func (s *Store) RequestJobCancel(ctx context.Context, jobID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_jobs SET cancel_requested=1, updated_at=? WHERE id=?`, now(), jobID)
	return err
}

// ---------- steps ----------

type ActionsStep struct {
	JobID      int64
	Index      int
	Name       string
	Status     string
	Conclusion string // set when continue-on-error hid a failure
	Summary    string // GITHUB_STEP_SUMMARY Markdown
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
}

func (st ActionsStep) Duration() time.Duration {
	if st.StartedAt.IsZero() || st.FinishedAt.IsZero() {
		return 0
	}
	return st.FinishedAt.Sub(st.StartedAt)
}

// ResetActionsSteps replaces a job's steps with fresh queued ones.
func (s *Store) ResetActionsSteps(ctx context.Context, jobID int64, names []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM actions_steps WHERE job_id=?`, jobID); err != nil {
		return err
	}
	for i, n := range names {
		if _, err := tx.ExecContext(ctx, `INSERT INTO actions_steps (job_id, idx, name) VALUES (?, ?, ?)`, jobID, i, n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetActionsStep records a step's result (from the runner).
func (s *Store) SetActionsStep(ctx context.Context, st ActionsStep) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO actions_steps (job_id, idx, name, status, conclusion, summary, exit_code, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(job_id, idx) DO UPDATE SET status=excluded.status, conclusion=excluded.conclusion,
		summary=excluded.summary, exit_code=excluded.exit_code, started_at=excluded.started_at, finished_at=excluded.finished_at`,
		st.JobID, st.Index, st.Name, st.Status, st.Conclusion, st.Summary, st.ExitCode, actUnix(st.StartedAt), actUnix(st.FinishedAt))
	return err
}

// MarkActionsStepsUnfinished sets every step that hasn't finished to status.
func (s *Store) MarkActionsStepsUnfinished(ctx context.Context, jobID int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_steps SET status=? WHERE job_id=? AND status IN ('queued','running')`, status, jobID)
	return err
}

// MarkActionsStepRunning marks a queued step as running (its first log line arrived).
func (s *Store) MarkActionsStepRunning(ctx context.Context, jobID int64, idx int, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_steps SET status='running', started_at=? WHERE job_id=? AND idx=? AND status='queued'`,
		at.Unix(), jobID, idx)
	return err
}

func (s *Store) ActionsSteps(ctx context.Context, jobID int64) ([]ActionsStep, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, idx, name, status, conclusion, summary, exit_code, started_at, finished_at
		FROM actions_steps WHERE job_id=? ORDER BY idx`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionsStep
	for rows.Next() {
		var st ActionsStep
		var a, b int64
		if err := rows.Scan(&st.JobID, &st.Index, &st.Name, &st.Status, &st.Conclusion, &st.Summary, &st.ExitCode, &a, &b); err != nil {
			return nil, err
		}
		st.StartedAt, st.FinishedAt = actTime(a), actTime(b)
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---------- logs ----------

// ActionsLogChunk is one upload of log lines (raw JSON array of actions.LogLine).
type ActionsLogChunk struct {
	ID    int64
	JobID int64
	Lines string
}

// AppendActionsLog stores a chunk if the job stays under maxBytes. It returns
// false (and stores nothing) once the cap would be exceeded.
func (s *Store) AppendActionsLog(ctx context.Context, jobID int64, lines string, n, maxBytes int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE actions_jobs SET log_bytes = log_bytes + ? WHERE id=? AND log_bytes + ? <= ?`, n, jobID, n, maxBytes)
	if err != nil {
		return false, err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO actions_logs (job_id, lines, bytes, created_at) VALUES (?, ?, ?, ?)`, jobID, lines, n, now()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ForceAppendActionsLog stores a chunk regardless of the cap (system notes).
func (s *Store) ForceAppendActionsLog(ctx context.Context, jobID int64, lines string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO actions_logs (job_id, lines, bytes, created_at) VALUES (?, ?, ?, ?)`, jobID, lines, len(lines), now())
	return err
}

// ActionsLogs returns a job's chunks after id `after`, oldest first.
func (s *Store) ActionsLogs(ctx context.Context, jobID, after int64, limit int) ([]ActionsLogChunk, error) {
	if limit <= 0 {
		limit = 100000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, job_id, lines FROM actions_logs WHERE job_id=? AND id>? ORDER BY id LIMIT ?`, jobID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionsLogChunk
	for rows.Next() {
		var c ActionsLogChunk
		if err := rows.Scan(&c.ID, &c.JobID, &c.Lines); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteActionsLogs removes a job's logs (before a re-run).
func (s *Store) DeleteActionsLogs(ctx context.Context, jobID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM actions_logs WHERE job_id=?`, jobID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE actions_jobs SET log_bytes=0 WHERE id=?`, jobID)
	return err
}

// PruneActionsLogs deletes logs of jobs that finished before cutoff.
func (s *Store) PruneActionsLogs(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM actions_logs WHERE job_id IN
		(SELECT id FROM actions_jobs WHERE finished_at > 0 AND finished_at < ?)`, cutoff.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- runners ----------

type ActionsRunner struct {
	ID           int64
	Name         string
	OS           string
	Arch         string
	Labels       []string
	Capabilities []string
	Version      string
	PersonID     int64
	Enabled      bool
	Busy         bool
	LastSeen     time.Time
	LastJobID    int64
	Offline      bool // offline_notified: an offline event was sent
	CreatedAt    time.Time
}

const actionsRunnerCols = `id, name, os, arch, labels, capabilities, version, COALESCE(person_id, 0), enabled, busy, last_seen, last_job_id,
offline_notified, created_at`

func scanActionsRunner(row interface{ Scan(...any) error }) (ActionsRunner, error) {
	var r ActionsRunner
	var labels, caps string
	var seen, c int64
	err := row.Scan(&r.ID, &r.Name, &r.OS, &r.Arch, &labels, &caps, &r.Version, &r.PersonID, &r.Enabled, &r.Busy, &seen,
		&r.LastJobID, &r.Offline, &c)
	if err != nil {
		return r, err
	}
	actUnJSON(labels, &r.Labels)
	actUnJSON(caps, &r.Capabilities)
	r.LastSeen, r.CreatedAt = actTime(seen), time.Unix(c, 0)
	return r, nil
}

// CreateActionsRunner registers a runner; only the token's hash is stored.
func (s *Store) CreateActionsRunner(ctx context.Context, r ActionsRunner, tokenHash string) (ActionsRunner, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO actions_runners (name, os, arch, labels, capabilities, version, token_hash,
		person_id, enabled, last_seen, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		r.Name, r.OS, r.Arch, actJSON(actList(r.Labels)), actJSON(actList(r.Capabilities)), r.Version, tokenHash,
		r.PersonID, t, t, t)
	if err != nil {
		return r, err
	}
	id, _ := res.LastInsertId()
	return s.ActionsRunnerByID(ctx, id)
}

func (s *Store) ActionsRunnerByID(ctx context.Context, id int64) (ActionsRunner, error) {
	r, err := scanActionsRunner(s.db.QueryRowContext(ctx, `SELECT `+actionsRunnerCols+` FROM actions_runners WHERE id=?`, id))
	return r, notFound(err)
}

// ActionsRunnerByToken finds the runner a token hash belongs to, comparing
// the stored hash in constant time.
func (s *Store) ActionsRunnerByToken(ctx context.Context, tokenHash string) (ActionsRunner, error) {
	var stored string
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id, token_hash FROM actions_runners WHERE token_hash=?`, tokenHash).Scan(&id, &stored)
	if err != nil {
		return ActionsRunner{}, notFound(err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(tokenHash)) != 1 {
		return ActionsRunner{}, ErrNotFound
	}
	return s.ActionsRunnerByID(ctx, id)
}

func (s *Store) ActionsRunners(ctx context.Context) ([]ActionsRunner, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+actionsRunnerCols+` FROM actions_runners ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionsRunner
	for rows.Next() {
		r, err := scanActionsRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TouchActionsRunner records a heartbeat and what the runner reports.
func (s *Store) TouchActionsRunner(ctx context.Context, id int64, labels, caps []string, version string, busy bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_runners SET labels=?, capabilities=?, version=CASE WHEN ?='' THEN version ELSE ? END,
		busy=?, last_seen=?, offline_notified=0, updated_at=? WHERE id=?`,
		actJSON(actList(labels)), actJSON(actList(caps)), version, version, busy, now(), now(), id)
	return err
}

// SeenActionsRunner records a heartbeat only.
func (s *Store) SeenActionsRunner(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_runners SET last_seen=?, offline_notified=0 WHERE id=?`, now(), id)
	return err
}

func (s *Store) SetActionsRunnerBusy(ctx context.Context, id int64, busy bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_runners SET busy=? WHERE id=?`, busy, id)
	return err
}

func (s *Store) MarkActionsRunnerOfflineNotified(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_runners SET offline_notified=1 WHERE id=?`, id)
	return err
}

func (s *Store) RenameActionsRunner(ctx context.Context, id int64, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_runners SET name=?, updated_at=? WHERE id=?`, name, now(), id)
	return err
}

func (s *Store) SetActionsRunnerEnabled(ctx context.Context, id int64, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_runners SET enabled=?, updated_at=? WHERE id=?`, enabled, now(), id)
	return err
}

func (s *Store) DeleteActionsRunner(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM actions_runners WHERE id=?`, id)
	return err
}

// SetActionsRunnerDiscovery stores the repositories a runner found (JSON).
func (s *Store) SetActionsRunnerDiscovery(ctx context.Context, id int64, data string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_runners SET discovery=?, last_seen=? WHERE id=?`, data, now(), id)
	return err
}

// ---------- registration tokens ----------

// CreateRunnerRegistration stores a one-time registration token's hash.
func (s *Store) CreateRunnerRegistration(ctx context.Context, tokenHash string, personID int64, ttl time.Duration) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM actions_runner_tokens WHERE expires_at < ? OR used_at > 0`, now())
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO actions_runner_tokens (token_hash, person_id, expires_at, created_at) VALUES (?, ?, ?, ?)`,
		tokenHash, personID, time.Now().Add(ttl).Unix(), now())
	return err
}

// UseRunnerRegistration consumes a registration token, returning who made it.
// A token works once and only before it expires.
func (s *Store) UseRunnerRegistration(ctx context.Context, tokenHash string) (int64, error) {
	var person sql.NullInt64
	err := s.db.QueryRowContext(ctx, `UPDATE actions_runner_tokens SET used_at=? WHERE token_hash=? AND used_at=0 AND expires_at>?
		RETURNING person_id`, now(), tokenHash, now()).Scan(&person)
	if err != nil {
		return 0, notFound(err)
	}
	return person.Int64, nil
}

// ---------- job tokens ----------

type ActionsJobToken struct {
	JobID         int64
	RepoID        int64
	PersonID      int64
	PackagesWrite bool
	ExpiresAt     time.Time
}

// SetActionsJobToken stores (or replaces) a job's token hash.
func (s *Store) SetActionsJobToken(ctx context.Context, t ActionsJobToken, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO actions_job_tokens (job_id, token_hash, repo_id, person_id, packages_write, expires_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(job_id) DO UPDATE SET token_hash=excluded.token_hash, expires_at=excluded.expires_at,
		packages_write=excluded.packages_write`, t.JobID, tokenHash, t.RepoID, t.PersonID, t.PackagesWrite, t.ExpiresAt.Unix())
	return err
}

// ActionsJobTokenByHash finds a live job token.
func (s *Store) ActionsJobTokenByHash(ctx context.Context, tokenHash string) (ActionsJobToken, error) {
	var t ActionsJobToken
	var stored string
	var person sql.NullInt64
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT job_id, token_hash, repo_id, person_id, packages_write, expires_at FROM actions_job_tokens
		WHERE token_hash=?`, tokenHash).Scan(&t.JobID, &stored, &t.RepoID, &person, &t.PackagesWrite, &exp)
	if err != nil {
		return t, notFound(err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(tokenHash)) != 1 || exp <= now() {
		return ActionsJobToken{}, ErrNotFound
	}
	t.PersonID, t.ExpiresAt = person.Int64, time.Unix(exp, 0)
	return t, nil
}

func (s *Store) DeleteActionsJobToken(ctx context.Context, jobID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM actions_job_tokens WHERE job_id=? OR expires_at < ?`, jobID, now())
	return err
}

// ---------- push queue ----------

type ActionsPush struct {
	ID          int64
	Owner, Repo string
	Old, New    string
	Ref         string
	Pusher      string
	CreatedAt   time.Time
}

// QueueActionsPush records a ref update (called by the post-receive hook).
func (s *Store) QueueActionsPush(ctx context.Context, p ActionsPush) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO actions_push_queue (owner, repo, old_sha, new_sha, ref, pusher, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, p.Owner, p.Repo, p.Old, p.New, p.Ref, p.Pusher, now())
	return err
}

// TakeActionsPushes removes and returns up to limit queued pushes, oldest first.
func (s *Store) TakeActionsPushes(ctx context.Context, limit int) ([]ActionsPush, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, owner, repo, old_sha, new_sha, ref, pusher, created_at FROM actions_push_queue
		ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var out []ActionsPush
	for rows.Next() {
		var p ActionsPush
		var c int64
		if err := rows.Scan(&p.ID, &p.Owner, &p.Repo, &p.Old, &p.New, &p.Ref, &p.Pusher, &c); err != nil {
			rows.Close()
			return nil, err
		}
		p.CreatedAt = time.Unix(c, 0)
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, p := range out {
		if _, err := tx.ExecContext(ctx, `DELETE FROM actions_push_queue WHERE id=?`, p.ID); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// ---------- repository settings ----------

// ActionsRequireLockfile reports whether unpinned actions make runs fail.
func (s *Store) ActionsRequireLockfile(ctx context.Context, repoID int64) (bool, error) {
	var v bool
	err := s.db.QueryRowContext(ctx, `SELECT require_lockfile FROM actions_repo_settings WHERE repo_id=?`, repoID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return v, err
}

func (s *Store) SetActionsRequireLockfile(ctx context.Context, repoID int64, on bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO actions_repo_settings (repo_id, require_lockfile) VALUES (?, ?)
		ON CONFLICT(repo_id) DO UPDATE SET require_lockfile=excluded.require_lockfile`, repoID, on)
	return err
}

// ---------- required checks on branch rules ----------

// BranchRuleChecks returns the required check names per rule id.
func (s *Store) BranchRuleChecks(ctx context.Context, ruleIDs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ruleIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ruleIDs)), ",")
	args := make([]any, len(ruleIDs))
	for i, id := range ruleIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rule_id, name FROM branch_rule_checks WHERE rule_id IN (`+ph+`) ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

// SetBranchRuleChecks replaces a rule's required checks.
func (s *Store) SetBranchRuleChecks(ctx context.Context, ruleID int64, names []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM branch_rule_checks WHERE rule_id=?`, ruleID); err != nil {
		return err
	}
	for _, n := range names {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO branch_rule_checks (rule_id, name) VALUES (?, ?)`, ruleID, n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MarkActionsLogFull records that a job's log hit its cap (so the note is written once).
func (s *Store) MarkActionsLogFull(ctx context.Context, jobID, maxBytes int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_jobs SET log_bytes=? WHERE id=?`, maxBytes+1, jobID)
	return err
}

// ActionsRunLogs returns log chunks for every job in a run after id `after`.
func (s *Store) ActionsRunLogs(ctx context.Context, runID, after int64, limit int) ([]ActionsLogChunk, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.id, l.job_id, l.lines FROM actions_logs l JOIN actions_jobs j ON j.id = l.job_id
		WHERE j.run_id=? AND l.id>? ORDER BY l.id LIMIT ?`, runID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActionsLogChunk
	for rows.Next() {
		var c ActionsLogChunk
		if err := rows.Scan(&c.ID, &c.JobID, &c.Lines); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
