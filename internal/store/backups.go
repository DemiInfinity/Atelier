package store

import (
	"context"
	"database/sql"
	"time"
)

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS backup_runs (
  id           INTEGER PRIMARY KEY,
  started_at   INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL DEFAULT 0,
  file         TEXT NOT NULL DEFAULT '',
  size         INTEGER NOT NULL DEFAULT 0,
  sha256       TEXT NOT NULL DEFAULT '',
  destinations TEXT NOT NULL DEFAULT '',
  ok           INTEGER NOT NULL DEFAULT 0,
  error        TEXT NOT NULL DEFAULT '',
  run_trigger  TEXT NOT NULL DEFAULT 'schedule',
  actor_kind   TEXT NOT NULL DEFAULT '',
  actor_name   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS backup_runs_started ON backup_runs(started_at);`)
}

// BackupRun is one attempt at taking a snapshot.
type BackupRun struct {
	ID           int64
	StartedAt    time.Time
	Duration     time.Duration
	File         string
	Size         int64
	SHA256       string
	Destinations string // e.g. "local, s3"
	OK           bool
	Error        string
	Trigger      string // "schedule" or "manual"
	ActorKind    string
	ActorName    string
}

func (s *Store) AddBackupRun(ctx context.Context, r BackupRun) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO backup_runs
		(started_at, duration_ms, file, size, sha256, destinations, ok, error, run_trigger, actor_kind, actor_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.StartedAt.Unix(), r.Duration.Milliseconds(), r.File, r.Size, r.SHA256, r.Destinations, r.OK, r.Error,
		r.Trigger, r.ActorKind, r.ActorName)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// BackupRuns returns the most recent runs first.
func (s *Store) BackupRuns(ctx context.Context, limit int) ([]BackupRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, started_at, duration_ms, file, size, sha256, destinations, ok, error,
		run_trigger, actor_kind, actor_name FROM backup_runs ORDER BY started_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackupRun
	for rows.Next() {
		var r BackupRun
		var started, ms int64
		var ok int
		if err := rows.Scan(&r.ID, &started, &ms, &r.File, &r.Size, &r.SHA256, &r.Destinations, &ok, &r.Error,
			&r.Trigger, &r.ActorKind, &r.ActorName); err != nil {
			return nil, err
		}
		r.StartedAt = time.Unix(started, 0)
		r.Duration = time.Duration(ms) * time.Millisecond
		r.OK = ok != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastBackupRun returns the newest run, or ErrNotFound.
func (s *Store) LastBackupRun(ctx context.Context) (BackupRun, error) {
	runs, err := s.BackupRuns(ctx, 1)
	if err != nil {
		return BackupRun{}, err
	}
	if len(runs) == 0 {
		return BackupRun{}, notFound(sql.ErrNoRows)
	}
	return runs[0], nil
}
