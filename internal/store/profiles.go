package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Profiles
//
// Every account (a person or a company) can have a profile: a short bio,
// location, website, pronouns and an avatar style, plus up to MaxPins pinned
// repositories shown first on its profile page. Rows are keyed by the account
// (owner) they describe, so a person's profile and each company profile live
// side by side, and the Team / Company addons can let more people edit them.

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS profiles (
  owner_id          INTEGER PRIMARY KEY REFERENCES owners(id) ON DELETE CASCADE,
  bio               TEXT NOT NULL DEFAULT '',
  location          TEXT NOT NULL DEFAULT '',
  website           TEXT NOT NULL DEFAULT '',
  pronouns          TEXT NOT NULL DEFAULT '',
  avatar            TEXT NOT NULL DEFAULT '',
  updated_at        INTEGER NOT NULL,
  updated_by        TEXT NOT NULL DEFAULT '',
  updated_by_kind   TEXT NOT NULL DEFAULT '',
  updated_by_person INTEGER REFERENCES owners(id) ON DELETE SET NULL
);
CREATE TABLE IF NOT EXISTS pinned_repos (
  owner_id  INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  repo_id   INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  position  INTEGER NOT NULL,
  PRIMARY KEY (owner_id, repo_id)
);
CREATE INDEX IF NOT EXISTS pinned_repos_order ON pinned_repos (owner_id, position);`)
}

// MaxPins is how many repositories an account can pin to its profile.
const MaxPins = 6

// ErrTooManyPins is returned when more than MaxPins repositories are pinned.
var ErrTooManyPins = errors.New("too many pinned repositories")

// ErrPinNotOwned is returned when pinning a repository another account owns.
var ErrPinNotOwned = errors.New("pinned repository belongs to another account")

// Profile is what an account says about itself.
type Profile struct {
	OwnerID   int64
	Bio       string
	Location  string
	Website   string // stored already sanitised by the caller (http/https only)
	Pronouns  string
	Avatar    string // avatar style; "" means the default
	UpdatedAt time.Time
	// Who last changed it (an Actor's Name / Kind / PersonID).
	UpdatedBy       string
	UpdatedByKind   string
	UpdatedByPerson int64
}

// IsEmpty reports whether nothing has been filled in.
func (p Profile) IsEmpty() bool {
	return p.Bio == "" && p.Location == "" && p.Website == "" && p.Pronouns == ""
}

// Profile returns an account's profile. An account that never saved one gets
// an empty profile (not ErrNotFound), so callers can always render it.
func (s *Store) Profile(ctx context.Context, ownerID int64) (Profile, error) {
	p := Profile{OwnerID: ownerID}
	var updated int64
	var person sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT bio, location, website, pronouns, avatar, updated_at,
		updated_by, updated_by_kind, updated_by_person FROM profiles WHERE owner_id=?`, ownerID).
		Scan(&p.Bio, &p.Location, &p.Website, &p.Pronouns, &p.Avatar, &updated, &p.UpdatedBy, &p.UpdatedByKind, &person)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return Profile{}, err
	}
	p.UpdatedAt = time.Unix(updated, 0)
	p.UpdatedByPerson = person.Int64
	return p, nil
}

// SaveProfile creates or replaces an account's profile.
func (s *Store) SaveProfile(ctx context.Context, p Profile) error {
	if p.OwnerID == 0 {
		return errors.New("profile needs an account")
	}
	var person any
	if p.UpdatedByPerson != 0 {
		person = p.UpdatedByPerson
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO profiles
		(owner_id, bio, location, website, pronouns, avatar, updated_at, updated_by, updated_by_kind, updated_by_person)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner_id) DO UPDATE SET bio=excluded.bio, location=excluded.location, website=excluded.website,
		pronouns=excluded.pronouns, avatar=excluded.avatar, updated_at=excluded.updated_at,
		updated_by=excluded.updated_by, updated_by_kind=excluded.updated_by_kind, updated_by_person=excluded.updated_by_person`,
		p.OwnerID, p.Bio, p.Location, p.Website, p.Pronouns, p.Avatar, now(), p.UpdatedBy, p.UpdatedByKind, person)
	return err
}

// SetDisplayName changes the name an account shows (its handle never changes).
func (s *Store) SetDisplayName(ctx context.Context, ownerID int64, display string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE owners SET display_name=? WHERE id=?`, display, ownerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PinnedRepos returns an account's pinned repositories in order. Callers must
// still filter them by what the viewer may read.
func (s *Store) PinnedRepos(ctx context.Context, ownerID int64) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, repoSelect+` JOIN pinned_repos p ON p.repo_id = r.id
		WHERE p.owner_id=? ORDER BY p.position, p.repo_id`, ownerID)
	if err != nil {
		return nil, err
	}
	return scanRepos(rows)
}

// SetPinnedRepos replaces an account's pins with repoIDs, in that order.
// Duplicates are dropped. Every repository must belong to the account, and at
// most MaxPins may be pinned.
func (s *Store) SetPinnedRepos(ctx context.Context, ownerID int64, repoIDs []int64) error {
	seen := map[int64]bool{}
	var ids []int64
	for _, id := range repoIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) > MaxPins {
		return ErrTooManyPins
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var owner int64
		if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM repos WHERE id=?`, id).Scan(&owner); err != nil {
			return notFound(err)
		}
		if owner != ownerID {
			return ErrPinNotOwned
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pinned_repos WHERE owner_id=?`, ownerID); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pinned_repos (owner_id, repo_id, position) VALUES (?, ?, ?)`,
			ownerID, id, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}
