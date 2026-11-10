package store

import (
	"context"

	"errors"
	"time"
)

// Git LFS: which objects each repository holds (the bytes live on disk,
// content-addressed under <data>/lfs/), and file locks.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS lfs_objects (
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  oid        TEXT NOT NULL,
  size       INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (repo_id, oid)
);
CREATE INDEX IF NOT EXISTS lfs_objects_oid ON lfs_objects (oid);

CREATE TABLE IF NOT EXISTS lfs_locks (
  id           INTEGER PRIMARY KEY,
  repo_id      INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  path         TEXT NOT NULL,
  ref          TEXT NOT NULL DEFAULT '',
  owner_kind   TEXT NOT NULL,
  owner_name   TEXT NOT NULL,
  owner_person INTEGER,
  locked_at    INTEGER NOT NULL,
  UNIQUE (repo_id, path)
);`)
}

// ErrLocked is returned when a path is already locked.
var ErrLocked = errors.New("already locked")

// AddLFSObject records that a repository holds an object.
func (s *Store) AddLFSObject(ctx context.Context, repoID int64, oid string, size int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO lfs_objects (repo_id, oid, size, created_at) VALUES (?,?,?,?)
		ON CONFLICT(repo_id, oid) DO NOTHING`, repoID, oid, size, now())
	return err
}

// LFSObject returns the size of an object the repository holds.
func (s *Store) LFSObject(ctx context.Context, repoID int64, oid string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT size FROM lfs_objects WHERE repo_id=? AND oid=?`, repoID, oid).Scan(&n)
	return n, notFound(err)
}

// LFSUsage is how many objects and bytes a repository stores (all repos when repoID is 0).
func (s *Store) LFSUsage(ctx context.Context, repoID int64) (objects int, bytes int64, err error) {
	q := `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM lfs_objects`
	var args []any
	if repoID != 0 {
		q += ` WHERE repo_id=?`
		args = append(args, repoID)
	}
	err = s.db.QueryRowContext(ctx, q, args...).Scan(&objects, &bytes)
	return
}

// ---------- locks ----------

type LFSLock struct {
	ID          int64
	RepoID      int64
	Path        string
	Ref         string
	OwnerKind   string
	OwnerName   string
	OwnerPerson int64
	LockedAt    time.Time
}

const lfsLockCols = `id, repo_id, path, ref, owner_kind, owner_name, COALESCE(owner_person, 0), locked_at`

func scanLFSLock(row interface{ Scan(...any) error }) (LFSLock, error) {
	var l LFSLock
	var at int64
	err := row.Scan(&l.ID, &l.RepoID, &l.Path, &l.Ref, &l.OwnerKind, &l.OwnerName, &l.OwnerPerson, &at)
	l.LockedAt = time.Unix(at, 0)
	return l, err
}

// CreateLFSLock locks a path. If it's already locked, the existing lock is
// returned with ErrLocked.
func (s *Store) CreateLFSLock(ctx context.Context, l LFSLock) (LFSLock, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO lfs_locks (repo_id, path, ref, owner_kind, owner_name, owner_person, locked_at)
		VALUES (?,?,?,?,?,?,?)`, l.RepoID, l.Path, l.Ref, l.OwnerKind, l.OwnerName, relNullID(l.OwnerPerson), now())
	if relUniqueErr(err) {
		cur, err := scanLFSLock(s.db.QueryRowContext(ctx, `SELECT `+lfsLockCols+` FROM lfs_locks WHERE repo_id=? AND path=?`, l.RepoID, l.Path))
		if err != nil {
			return LFSLock{}, notFound(err)
		}
		return cur, ErrLocked
	}
	if err != nil {
		return LFSLock{}, err
	}
	id, _ := res.LastInsertId()
	return s.LFSLockByID(ctx, l.RepoID, id)
}

func (s *Store) LFSLockByID(ctx context.Context, repoID, id int64) (LFSLock, error) {
	l, err := scanLFSLock(s.db.QueryRowContext(ctx, `SELECT `+lfsLockCols+` FROM lfs_locks WHERE id=? AND repo_id=?`, id, repoID))
	return l, notFound(err)
}

// LFSLocks lists locks, optionally filtered by path or id, in id order after
// the cursor (an id; 0 = from the start). limit <= 0 means 100.
func (s *Store) LFSLocks(ctx context.Context, repoID int64, path string, id, after int64, limit int) ([]LFSLock, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q := `SELECT ` + lfsLockCols + ` FROM lfs_locks WHERE repo_id=? AND id > ?`
	args := []any{repoID, after}
	if path != "" {
		q += ` AND path=?`
		args = append(args, path)
	}
	if id != 0 {
		q += ` AND id=?`
		args = append(args, id)
	}
	q += ` ORDER BY id LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LFSLock
	for rows.Next() {
		l, err := scanLFSLock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) DeleteLFSLock(ctx context.Context, repoID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM lfs_locks WHERE id=? AND repo_id=?`, id, repoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
