package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Pages: static sites published from a repository (internal/web/pages_site*.go).
// pages_sites holds each repository's settings and which deploy is live;
// pages_deploys is the deploy history. The snapshot files themselves live
// under <data>/pages (internal/pagesdeploy).

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS pages_sites (
  repo_id    INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  enabled    INTEGER NOT NULL DEFAULT 0,
  source     TEXT NOT NULL DEFAULT 'branch' CHECK (source IN ('branch','actions')),
  branch     TEXT NOT NULL DEFAULT 'main',
  folder     TEXT NOT NULL DEFAULT '',
  live_id    INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS pages_deploys (
  id          INTEGER PRIMARY KEY,
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  source      TEXT NOT NULL,
  ref         TEXT NOT NULL DEFAULT '',
  sha         TEXT NOT NULL DEFAULT '',
  actor       TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'building' CHECK (status IN ('building','ready','failed')),
  error       TEXT NOT NULL DEFAULT '',
  files       INTEGER NOT NULL DEFAULT 0,
  bytes       INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  finished_at INTEGER
);
CREATE INDEX IF NOT EXISTS pages_deploys_repo ON pages_deploys (repo_id, id);
`)
}

// Pages sources.
const (
	PagesFromBranch  = "branch"  // a folder on a branch, published on every push
	PagesFromActions = "actions" // uploaded by atelier/deploy-pages in a workflow
)

// PagesSite is a repository's Pages settings.
type PagesSite struct {
	RepoID    int64
	Enabled   bool
	Source    string
	Branch    string
	Folder    string // "" is the repository root
	LiveID    int64  // live deploy, 0 = nothing published
	UpdatedAt time.Time
}

// PagesDeploy is one deploy in the history.
type PagesDeploy struct {
	ID         int64
	RepoID     int64
	Source     string
	Ref, SHA   string
	Actor      string
	Status     string // building, ready, failed
	Error      string
	Files      int
	Bytes      int64
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// PagesSite returns the settings, or defaults (not enabled) when there are none.
func (s *Store) PagesSite(ctx context.Context, repoID int64) (PagesSite, error) {
	ps := PagesSite{RepoID: repoID, Source: PagesFromBranch, Branch: "main"}
	var u int64
	err := s.db.QueryRowContext(ctx, `SELECT enabled, source, branch, folder, live_id, updated_at FROM pages_sites WHERE repo_id=?`, repoID).
		Scan(&ps.Enabled, &ps.Source, &ps.Branch, &ps.Folder, &ps.LiveID, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return ps, nil
	}
	ps.UpdatedAt = time.Unix(u, 0)
	return ps, err
}

// SavePagesSite stores the settings (not the live deploy).
func (s *Store) SavePagesSite(ctx context.Context, ps PagesSite) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pages_sites (repo_id, enabled, source, branch, folder, updated_at) VALUES (?,?,?,?,?,?)
ON CONFLICT(repo_id) DO UPDATE SET enabled=excluded.enabled, source=excluded.source, branch=excluded.branch, folder=excluded.folder, updated_at=excluded.updated_at`,
		ps.RepoID, ps.Enabled, ps.Source, ps.Branch, ps.Folder, now())
	return err
}

// SetPagesLive switches the live deploy. deployID must be a ready deploy of
// the repository (0 takes the site offline).
func (s *Store) SetPagesLive(ctx context.Context, repoID, deployID int64) error {
	if deployID != 0 {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages_deploys WHERE id=? AND repo_id=? AND status='ready'`, deployID, repoID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE pages_sites SET live_id=?, updated_at=? WHERE repo_id=?`, deployID, now(), repoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PagesSiteIDs lists repositories that have Pages settings (for
// cleaning up snapshot directories).
func (s *Store) PagesSiteIDs(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id FROM pages_sites`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// DeletePagesSite removes the settings and the whole deploy history.
func (s *Store) DeletePagesSite(ctx context.Context, repoID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pages_deploys WHERE repo_id=?`, repoID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM pages_sites WHERE repo_id=?`, repoID)
	return err
}

// CreatePagesDeploy starts a deploy in the building state.
func (s *Store) CreatePagesDeploy(ctx context.Context, d PagesDeploy) (PagesDeploy, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO pages_deploys (repo_id, source, ref, sha, actor, status, created_at) VALUES (?,?,?,?,?,'building',?)`,
		d.RepoID, d.Source, d.Ref, d.SHA, d.Actor, t)
	if err != nil {
		return d, err
	}
	d.ID, _ = res.LastInsertId()
	d.Status, d.CreatedAt = "building", time.Unix(t, 0)
	return d, nil
}

// FinishPagesDeploy records the outcome: ready with its size, or failed with errMsg.
func (s *Store) FinishPagesDeploy(ctx context.Context, id int64, files int, bytes int64, errMsg string) error {
	status := "ready"
	if errMsg != "" {
		status = "failed"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE pages_deploys SET status=?, error=?, files=?, bytes=?, finished_at=? WHERE id=?`,
		status, errMsg, files, bytes, now(), id)
	return err
}

const pagesDeployCols = `id, repo_id, source, ref, sha, actor, status, error, files, bytes, created_at, finished_at`

func scanPagesDeploy(row interface{ Scan(...any) error }) (PagesDeploy, error) {
	var d PagesDeploy
	var c int64
	var f sql.NullInt64
	err := row.Scan(&d.ID, &d.RepoID, &d.Source, &d.Ref, &d.SHA, &d.Actor, &d.Status, &d.Error, &d.Files, &d.Bytes, &c, &f)
	d.CreatedAt = time.Unix(c, 0)
	d.FinishedAt = unixPtr(f)
	return d, err
}

// PagesDeploy finds one deploy of a repository.
func (s *Store) PagesDeploy(ctx context.Context, repoID, id int64) (PagesDeploy, error) {
	d, err := scanPagesDeploy(s.db.QueryRowContext(ctx, `SELECT `+pagesDeployCols+` FROM pages_deploys WHERE id=? AND repo_id=?`, id, repoID))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// PagesDeploys lists a repository's deploys, newest first.
func (s *Store) PagesDeploys(ctx context.Context, repoID int64, limit int) ([]PagesDeploy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pagesDeployCols+` FROM pages_deploys WHERE repo_id=? ORDER BY id DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PagesDeploy
	for rows.Next() {
		d, err := scanPagesDeploy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeletePagesDeploy removes one deploy from the history.
func (s *Store) DeletePagesDeploy(ctx context.Context, repoID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pages_deploys WHERE id=? AND repo_id=?`, id, repoID)
	return err
}

// FailStalePagesDeploys marks deploys left building (by a restart) as failed.
func (s *Store) FailStalePagesDeploys(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pages_deploys SET status='failed', error='interrupted by a restart', finished_at=? WHERE status='building'`, now())
	return err
}
