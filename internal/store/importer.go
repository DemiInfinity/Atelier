package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS import_jobs (
  id          INTEGER PRIMARY KEY,
  source      TEXT NOT NULL,              -- gitea, github, git
  source_url  TEXT NOT NULL,              -- clone URL without credentials
  owner       TEXT NOT NULL,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  private     INTEGER NOT NULL DEFAULT 1,
  mirror      INTEGER NOT NULL DEFAULT 0,
  status      TEXT NOT NULL DEFAULT 'queued', -- queued, running, done, failed
  message     TEXT NOT NULL DEFAULT '',
  actor_kind  TEXT NOT NULL DEFAULT '',
  actor_name  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS import_jobs_status ON import_jobs(status);
CREATE TABLE IF NOT EXISTS import_mirrors (
  repo_id      INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  source       TEXT NOT NULL,
  source_url   TEXT NOT NULL,
  username     TEXT NOT NULL DEFAULT '',
  secret       TEXT NOT NULL DEFAULT '',  -- token for private sources; never shown in the UI
  interval_sec INTEGER NOT NULL,
  last_sync_at INTEGER,
  last_error   TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL
);`)
}

// ImportJob is one repository being imported.
type ImportJob struct {
	ID          int64
	Source      string
	SourceURL   string
	Owner       string
	Name        string
	Description string
	Private     bool
	Mirror      bool
	Status      string
	Message     string
	ActorKind   string
	ActorName   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (j ImportJob) FullName() string { return j.Owner + "/" + j.Name }

const importJobCols = `id, source, source_url, owner, name, description, private, mirror, status, message,
	actor_kind, actor_name, created_at, updated_at`

func scanImportJob(row interface{ Scan(...any) error }) (ImportJob, error) {
	var j ImportJob
	var priv, mirror int
	var c, u int64
	err := row.Scan(&j.ID, &j.Source, &j.SourceURL, &j.Owner, &j.Name, &j.Description, &priv, &mirror, &j.Status,
		&j.Message, &j.ActorKind, &j.ActorName, &c, &u)
	j.Private, j.Mirror = priv != 0, mirror != 0
	j.CreatedAt, j.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return j, err
}

func (s *Store) CreateImportJob(ctx context.Context, j ImportJob) (int64, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO import_jobs
		(source, source_url, owner, name, description, private, mirror, status, message, actor_kind, actor_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', '', ?, ?, ?, ?)`,
		j.Source, j.SourceURL, j.Owner, j.Name, j.Description, j.Private, j.Mirror, j.ActorKind, j.ActorName, t, t)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) SetImportJobStatus(ctx context.Context, id int64, status, message string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE import_jobs SET status=?, message=?, updated_at=? WHERE id=?`,
		status, message, now(), id)
	return err
}

func (s *Store) ImportJob(ctx context.Context, id int64) (ImportJob, error) {
	j, err := scanImportJob(s.db.QueryRowContext(ctx, `SELECT `+importJobCols+` FROM import_jobs WHERE id=?`, id))
	return j, notFound(err)
}

// ImportJobs returns the most recent jobs first.
func (s *Store) ImportJobs(ctx context.Context, limit int) ([]ImportJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+importJobCols+` FROM import_jobs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportJob
	for rows.Next() {
		j, err := scanImportJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// FailUnfinishedImports marks jobs left queued or running by a restart as failed.
// Their credentials only ever lived in memory, so they can't be resumed.
func (s *Store) FailUnfinishedImports(ctx context.Context, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE import_jobs SET status='failed', message=?, updated_at=?
		WHERE status IN ('queued','running')`, msg, now())
	return err
}

// ImportMirror keeps an imported repository in sync with its source.
type ImportMirror struct {
	RepoID     int64
	Repo       string // owner/name
	Owner      string
	Name       string
	Source     string
	SourceURL  string
	Username   string
	Secret     string // never render this
	Interval   time.Duration
	LastSyncAt *time.Time
	LastError  string
	CreatedAt  time.Time
}

// HasSecret reports whether credentials are stored (safe to show).
func (m ImportMirror) HasSecret() bool { return m.Secret != "" }

func (s *Store) SaveImportMirror(ctx context.Context, m ImportMirror) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO import_mirrors
		(repo_id, source, source_url, username, secret, interval_sec, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id) DO UPDATE SET source=excluded.source, source_url=excluded.source_url,
		username=excluded.username, secret=excluded.secret, interval_sec=excluded.interval_sec`,
		m.RepoID, m.Source, m.SourceURL, m.Username, m.Secret, int64(m.Interval.Seconds()), now())
	return err
}

func (s *Store) DeleteImportMirror(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM import_mirrors WHERE repo_id=?`, repoID)
	return err
}

func (s *Store) MarkImportMirrorSynced(ctx context.Context, repoID int64, at time.Time, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE import_mirrors SET last_sync_at=?, last_error=? WHERE repo_id=?`,
		at.Unix(), errMsg, repoID)
	return err
}

func (s *Store) ImportMirrors(ctx context.Context) ([]ImportMirror, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.repo_id, o.name, r.name, m.source, m.source_url, m.username, m.secret,
		m.interval_sec, m.last_sync_at, m.last_error, m.created_at
		FROM import_mirrors m JOIN repos r ON r.id = m.repo_id JOIN owners o ON o.id = r.owner_id
		ORDER BY o.name, r.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportMirror
	for rows.Next() {
		var m ImportMirror
		var interval, created int64
		var last sql.NullInt64
		if err := rows.Scan(&m.RepoID, &m.Owner, &m.Name, &m.Source, &m.SourceURL, &m.Username, &m.Secret,
			&interval, &last, &m.LastError, &created); err != nil {
			return nil, err
		}
		m.Repo = m.Owner + "/" + m.Name
		m.Interval = time.Duration(interval) * time.Second
		m.CreatedAt = time.Unix(created, 0)
		if last.Valid {
			t := time.Unix(last.Int64, 0)
			m.LastSyncAt = &t
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ImportMirror returns one mirror by repository id.
func (s *Store) ImportMirror(ctx context.Context, repoID int64) (ImportMirror, error) {
	ms, err := s.ImportMirrors(ctx)
	if err != nil {
		return ImportMirror{}, err
	}
	for _, m := range ms {
		if m.RepoID == repoID {
			return m, nil
		}
	}
	return ImportMirror{}, ErrNotFound
}

// MirrorSource returns the source URL if the repository is kept in sync as a
// mirror (pushes to it would be overwritten), or "" if it isn't a mirror.
func (s *Store) MirrorSource(ctx context.Context, repoID int64) (string, error) {
	var src string
	err := s.db.QueryRowContext(ctx, `SELECT source_url FROM import_mirrors WHERE repo_id=?`, repoID).Scan(&src)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return src, err
}
