package store

import (
	"context"
	"errors"
)

// Moving repositories between accounts (personal ↔ company) and renaming them.
//
// A move keeps the repository's id, so everything tied to it — issues, pull
// requests, Actions runs, releases, LFS objects, branch rules, repo secrets,
// webhooks — goes with it. The old owner/name is kept as a redirect, so old
// links, clone URLs and SSH remotes keep working, until a new repository
// takes that name.

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS repo_redirects (
  owner_id   INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  name       TEXT NOT NULL COLLATE NOCASE,
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (owner_id, name)
);`)
}

var (
	ErrNameTaken = errors.New("that account already has a repository with this name")
	ErrSamePlace = errors.New("the repository is already there")
)

// RepoByID looks a repository up by id.
func (s *Store) RepoByID(ctx context.Context, id int64) (Repo, error) {
	rows, err := s.db.QueryContext(ctx, repoSelect+` WHERE r.id=?`, id)
	if err != nil {
		return Repo{}, err
	}
	repos, err := scanRepos(rows)
	if err != nil {
		return Repo{}, err
	}
	if len(repos) == 0 {
		return Repo{}, ErrNotFound
	}
	return repos[0], nil
}

func (s *Store) repoRedirect(ctx context.Context, owner, name string) (int64, bool) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT rr.repo_id FROM repo_redirects rr JOIN owners o ON o.id = rr.owner_id
		WHERE o.name=? AND rr.name=?`, owner, name).Scan(&id)
	return id, err == nil
}

func (s *Store) dropRedirect(ctx context.Context, ownerID int64, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repo_redirects WHERE owner_id=? AND name=?`, ownerID, name)
	return err
}

// RepoRedirects lists the old names that point at a repository.
func (s *Store) RepoRedirects(ctx context.Context, repoID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.name || '/' || rr.name FROM repo_redirects rr
		JOIN owners o ON o.id = rr.owner_id WHERE rr.repo_id=? ORDER BY rr.created_at DESC`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MoveRepo gives a repository a new owner and/or name and records a redirect
// from the old one. The caller moves the bare repository on disk.
func (s *Store) MoveRepo(ctx context.Context, repoID, newOwnerID int64, newName string) (Repo, error) {
	old, err := s.RepoByID(ctx, repoID)
	if err != nil {
		return Repo{}, err
	}
	if old.OwnerID == newOwnerID && old.Name == newName {
		return Repo{}, ErrSamePlace
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Repo{}, err
	}
	defer tx.Rollback()

	var clash int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM repos WHERE owner_id=? AND name=? AND id<>?`, newOwnerID, newName, repoID).Scan(&clash)
	if err == nil {
		return Repo{}, ErrNameTaken
	}
	// The new address is real now: drop any redirect that used to live there.
	if _, err := tx.ExecContext(ctx, `DELETE FROM repo_redirects WHERE owner_id=? AND name=?`, newOwnerID, newName); err != nil {
		return Repo{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repos SET owner_id=?, name=?, updated_at=? WHERE id=?`,
		newOwnerID, newName, now(), repoID); err != nil {
		return Repo{}, err
	}
	// Only a change of case keeps the same address; otherwise remember the old one.
	if !(old.OwnerID == newOwnerID && equalFold(old.Name, newName)) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO repo_redirects (owner_id, name, repo_id, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(owner_id, name) DO UPDATE SET repo_id=excluded.repo_id, created_at=excluded.created_at`,
			old.OwnerID, old.Name, repoID, now()); err != nil {
			return Repo{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Repo{}, err
	}
	return s.RepoByID(ctx, repoID)
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
