package store

// Dependency updates: per-repository update settings (grouping, schedule,
// lockfile regeneration), the results of the latest check, and which
// dependencies each grouped pull request contains.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS dep_update_settings (
  repo_id      INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  group_minor  INTEGER NOT NULL DEFAULT 0,  -- one PR for minor and patch updates per manager and folder
  schedule_day INTEGER NOT NULL DEFAULT -1, -- 0 Sunday … 6 Saturday; -1 any day (weekly)
  lockfiles    INTEGER NOT NULL DEFAULT 0,  -- Actions jobs may push lockfiles to update branches
  updated_at   INTEGER NOT NULL
);
-- Results of the latest check. Replaces dep_updates, whose key (repo,
-- ecosystem, name) can't hold the same package in two manifests.
CREATE TABLE IF NOT EXISTS dep_update_rows (
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  ecosystem  TEXT NOT NULL,              -- go | npm | maven | gradle
  name       TEXT NOT NULL,
  manifest   TEXT NOT NULL,              -- path in the repository
  current    TEXT NOT NULL,
  latest     TEXT NOT NULL DEFAULT '',
  new_spec   TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL DEFAULT '',
  vulns      INTEGER NOT NULL DEFAULT 0,
  vuln_ids   TEXT NOT NULL DEFAULT '',
  fixes      INTEGER NOT NULL DEFAULT 0,
  pr_number  INTEGER NOT NULL DEFAULT 0,
  checked_at INTEGER NOT NULL,
  PRIMARY KEY (repo_id, ecosystem, manifest, name)
);
CREATE TABLE IF NOT EXISTS dep_update_members (
  repo_id   INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  pr_number INTEGER NOT NULL,
  manifest  TEXT NOT NULL,
  name      TEXT NOT NULL,
  version   TEXT NOT NULL,
  PRIMARY KEY (repo_id, pr_number, manifest, name)
);`)
}

// DepUpdateSettings are a repository's dependency update options. "Security
// fixes only" and "all updates" stay in SecuritySettings.
type DepUpdateSettings struct {
	GroupMinor  bool // group minor and patch updates
	ScheduleDay int  // 0 (Sunday) … 6; -1: any day, a week after the last check
	Lockfiles   bool // let a workflow regenerate lockfiles on update branches
}

var DefaultDepUpdateSettings = DepUpdateSettings{ScheduleDay: -1}

func (s *Store) DepUpdateSettings(ctx context.Context, repoID int64) (DepUpdateSettings, error) {
	var st DepUpdateSettings
	err := s.db.QueryRowContext(ctx, `SELECT group_minor, schedule_day, lockfiles FROM dep_update_settings WHERE repo_id=?`, repoID).
		Scan(&st.GroupMinor, &st.ScheduleDay, &st.Lockfiles)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultDepUpdateSettings, nil
	}
	if st.ScheduleDay < -1 || st.ScheduleDay > 6 {
		st.ScheduleDay = -1
	}
	return st, err
}

func (s *Store) SetDepUpdateSettings(ctx context.Context, repoID int64, st DepUpdateSettings) error {
	if st.ScheduleDay < -1 || st.ScheduleDay > 6 {
		st.ScheduleDay = -1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO dep_update_settings (repo_id, group_minor, schedule_day, lockfiles, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(repo_id) DO UPDATE SET group_minor=excluded.group_minor,
		schedule_day=excluded.schedule_day, lockfiles=excluded.lockfiles, updated_at=excluded.updated_at`,
		repoID, st.GroupMinor, st.ScheduleDay, st.Lockfiles, now())
	return err
}

// DepUpdateMember is one dependency update inside a pull request.
type DepUpdateMember struct {
	PRNumber       int64
	Manifest, Name string
	Version        string
}

// AddDepUpdateMembers records what a (grouped) update pull request contains.
func (s *Store) AddDepUpdateMembers(ctx context.Context, repoID int64, ms []DepUpdateMember) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range ms {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO dep_update_members (repo_id, pr_number, manifest, name, version) VALUES (?,?,?,?,?)`,
			repoID, m.PRNumber, m.Manifest, m.Name, m.Version); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DepUpdateMembers lists the members of the given pull requests.
func (s *Store) DepUpdateMembers(ctx context.Context, repoID, prNumber int64) ([]DepUpdateMember, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pr_number, manifest, name, version FROM dep_update_members WHERE repo_id=? AND pr_number=?
		ORDER BY manifest, name`, repoID, prNumber)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DepUpdateMember
	for rows.Next() {
		var m DepUpdateMember
		if err := rows.Scan(&m.PRNumber, &m.Manifest, &m.Name, &m.Version); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LockfileJob is what decides whether an Actions job may push lockfiles.
type LockfileJob struct {
	Branch  string // the run's branch ("" when the run isn't for a branch)
	Running bool   // the job is still running
	BotPR   bool   // atelier-updates has an open pull request from Branch
	Enabled bool   // the repository turned on lockfile regeneration
}

// LockfileJobState looks up a job of repoID for the lockfile push checks
// (the web access policy and the pre-receive hook). botName is the updates
// bot ("atelier-updates").
func (s *Store) LockfileJobState(ctx context.Context, repoID, jobID int64, botName string) (LockfileJob, error) {
	var st LockfileJob
	j, err := s.ActionsJob(ctx, jobID)
	if err != nil {
		return st, err
	}
	if j.RepoID != repoID {
		return st, nil
	}
	run, err := s.ActionsRunByID(ctx, j.RunID)
	if err != nil {
		return st, err
	}
	if run.RepoID != repoID {
		return st, nil
	}
	st.Running = j.Status == "running"
	branch, ok := strings.CutPrefix(run.Ref, "refs/heads/")
	if !ok || branch == "" {
		return st, nil
	}
	st.Branch = branch
	prs, err := s.PullsByAuthorPrefix(ctx, repoID, "bot", botName, branch)
	if err != nil {
		return st, err
	}
	for _, p := range prs {
		if p.Head == branch && p.IsOpen() {
			st.BotPR = true
		}
	}
	set, err := s.DepUpdateSettings(ctx, repoID)
	if err != nil {
		return st, err
	}
	st.Enabled = set.Lockfiles
	return st, nil
}
