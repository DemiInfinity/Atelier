package store

// Insights: the cache of per-repository git statistics (keyed by the commit
// they were computed at) and the database side of Pulse and Actions usage.
// The git work and the pages live in internal/web/insights*.go.

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS insights_cache (
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,
  head_sha    TEXT NOT NULL,
  data        TEXT NOT NULL,
  computed_at INTEGER NOT NULL,
  PRIMARY KEY (repo_id, kind)
);`)
}

// InsightsCacheEntry is one cached computation for a repository: Data (JSON)
// was computed at commit HeadSHA.
type InsightsCacheEntry struct {
	RepoID     int64
	Kind       string
	HeadSHA    string
	Data       []byte
	ComputedAt time.Time
}

// InsightsCache returns the cached entry of this kind (ErrNotFound if none).
func (s *Store) InsightsCache(ctx context.Context, repoID int64, kind string) (InsightsCacheEntry, error) {
	e := InsightsCacheEntry{RepoID: repoID, Kind: kind}
	var data string
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT head_sha, data, computed_at FROM insights_cache WHERE repo_id=? AND kind=?`,
		repoID, kind).Scan(&e.HeadSHA, &data, &at)
	if err != nil {
		return e, notFound(err)
	}
	e.Data, e.ComputedAt = []byte(data), time.Unix(at, 0)
	return e, nil
}

// SetInsightsCache stores (or replaces) an entry.
func (s *Store) SetInsightsCache(ctx context.Context, e InsightsCacheEntry) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO insights_cache (repo_id, kind, head_sha, data, computed_at) VALUES (?,?,?,?,?)
		ON CONFLICT(repo_id, kind) DO UPDATE SET head_sha=excluded.head_sha, data=excluded.data, computed_at=excluded.computed_at`,
		e.RepoID, e.Kind, e.HeadSHA, string(e.Data), now())
	return err
}

// ---------- Pulse ----------

type PulsePull struct {
	Number     int64
	Title      string
	AuthorName string
	At         time.Time
}

type PulseIssue struct {
	Number     int64
	Title      string
	AuthorName string
	At         time.Time
}

type PulseRelease struct {
	Tag, Title string
	At         time.Time
}

// Pulse is what happened in a repository since a moment.
type Pulse struct {
	Merged       []PulsePull // merged pull requests, newest first
	OpenedPulls  int
	OpenedIssues []PulseIssue
	ClosedIssues []PulseIssue
	Releases     []PulseRelease
}

const pulseLimit = 50

// InsightsPulse lists merged pull requests, opened and closed issues and
// published releases since the given time (each list capped at 50; the
// counts on the page come from the lists, so a busy repository shows "50+").
func (s *Store) InsightsPulse(ctx context.Context, repoID int64, since time.Time) (Pulse, error) {
	var p Pulse
	t := since.Unix()
	rows, err := s.db.QueryContext(ctx, `SELECT number, title, author_name, merged_at FROM pulls
		WHERE repo_id=? AND state='merged' AND merged_at >= ? ORDER BY merged_at DESC LIMIT ?`, repoID, t, pulseLimit+1)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var x PulsePull
		var at int64
		if err := rows.Scan(&x.Number, &x.Title, &x.AuthorName, &at); err != nil {
			rows.Close()
			return p, err
		}
		x.At = time.Unix(at, 0)
		p.Merged = append(p.Merged, x)
	}
	rows.Close()
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pulls WHERE repo_id=? AND created_at >= ?`, repoID, t).Scan(&p.OpenedPulls); err != nil {
		return p, err
	}
	issues := func(q string) ([]PulseIssue, error) {
		rows, err := s.db.QueryContext(ctx, q, repoID, t, pulseLimit+1)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []PulseIssue
		for rows.Next() {
			var x PulseIssue
			var at int64
			if err := rows.Scan(&x.Number, &x.Title, &x.AuthorName, &at); err != nil {
				return nil, err
			}
			x.At = time.Unix(at, 0)
			out = append(out, x)
		}
		return out, rows.Err()
	}
	if p.OpenedIssues, err = issues(`SELECT number, title, author_name, created_at FROM issues
		WHERE repo_id=? AND created_at >= ? ORDER BY created_at DESC LIMIT ?`); err != nil {
		return p, err
	}
	if p.ClosedIssues, err = issues(`SELECT number, title, author_name, closed_at FROM issues
		WHERE repo_id=? AND state='closed' AND closed_at >= ? ORDER BY closed_at DESC LIMIT ?`); err != nil {
		return p, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT tag, title, published_at FROM releases
		WHERE repo_id=? AND draft=0 AND published_at >= ? ORDER BY published_at DESC LIMIT ?`, repoID, t, pulseLimit+1)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var x PulseRelease
		var at int64
		if err := rows.Scan(&x.Tag, &x.Title, &at); err != nil {
			return p, err
		}
		x.At = time.Unix(at, 0)
		p.Releases = append(p.Releases, x)
	}
	return p, rows.Err()
}

// ---------- Actions usage ----------

// InsightsJob is one finished Actions job with what usage needs.
type InsightsJob struct {
	RepoID       int64
	RunSeq       int64
	RunNumber    int64
	WorkflowName string
	WorkflowPath string
	JobKey       string
	Name         string
	RunnerName   string
	Status       string // success, failure, cancelled, skipped
	StartedAt    time.Time
	FinishedAt   time.Time
	Outputs      map[string]string // only for agent jobs (job key "agent")
}

// Duration is how long the job ran.
func (j InsightsJob) Duration() time.Duration {
	if j.StartedAt.IsZero() || j.FinishedAt.Before(j.StartedAt) {
		return 0
	}
	return j.FinishedAt.Sub(j.StartedAt)
}

func inIDs(ids []int64, args *[]any) string {
	ph := make([]string, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		*args = append(*args, id)
	}
	return strings.Join(ph, ",")
}

// InsightsJobs lists jobs in these repositories that started since the given
// time and have finished, newest first, at most limit of them.
func (s *Store) InsightsJobs(ctx context.Context, repoIDs []int64, since time.Time, limit int) ([]InsightsJob, error) {
	if len(repoIDs) == 0 {
		return nil, nil
	}
	var args []any
	in := inIDs(repoIDs, &args)
	args = append(args, since.Unix(), limit)
	rows, err := s.db.QueryContext(ctx, `SELECT j.repo_id, r.seq, r.run_number, r.workflow_name, r.workflow_path, j.job_key, j.name,
		j.runner_name, j.status, j.started_at, j.finished_at, CASE WHEN j.job_key='agent' THEN j.outputs ELSE '' END
		FROM actions_jobs j JOIN actions_runs r ON r.id = j.run_id
		WHERE j.repo_id IN (`+in+`) AND j.started_at >= ? AND j.started_at > 0 AND j.finished_at > 0
		  AND j.status IN ('success','failure','cancelled','skipped')
		ORDER BY j.started_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InsightsJob
	for rows.Next() {
		var j InsightsJob
		var st, fin int64
		var outputs sql.NullString
		if err := rows.Scan(&j.RepoID, &j.RunSeq, &j.RunNumber, &j.WorkflowName, &j.WorkflowPath, &j.JobKey, &j.Name,
			&j.RunnerName, &j.Status, &st, &fin, &outputs); err != nil {
			return nil, err
		}
		j.StartedAt, j.FinishedAt = time.Unix(st, 0), time.Unix(fin, 0)
		if outputs.String != "" {
			actUnJSON(outputs.String, &j.Outputs)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// InsightsRunOutcome counts completed runs per workflow and conclusion.
type InsightsRunOutcome struct {
	WorkflowName string
	Conclusion   string
	Runs         int
}

// InsightsRunOutcomes counts runs created since the given time that completed.
func (s *Store) InsightsRunOutcomes(ctx context.Context, repoIDs []int64, since time.Time) ([]InsightsRunOutcome, error) {
	if len(repoIDs) == 0 {
		return nil, nil
	}
	var args []any
	in := inIDs(repoIDs, &args)
	args = append(args, since.Unix())
	rows, err := s.db.QueryContext(ctx, `SELECT workflow_name, conclusion, COUNT(*) FROM actions_runs
		WHERE repo_id IN (`+in+`) AND created_at >= ? AND status='completed'
		GROUP BY workflow_name, conclusion ORDER BY workflow_name, conclusion`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InsightsRunOutcome
	for rows.Next() {
		var o InsightsRunOutcome
		if err := rows.Scan(&o.WorkflowName, &o.Conclusion, &o.Runs); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
