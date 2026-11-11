package store

import (
	"context"
	"errors"
)

// ErrAlreadySetUp means first-run setup has already created the owner.
var ErrAlreadySetUp = errors.New("Atelier is already set up")

// CreateFirstOwner is first-run setup: it creates the personal owner and
// stores their password hash together, in one transaction, and only if no
// personal owner exists yet.
//
// Issue #19: setup used to write the hash first and create the owner second,
// as two separate writes. When two setup posts raced (a double click, a
// browser or proxy re-sending the POST, a second tab), the request that lost
// the owner insert had already replaced the winner's hash with its own, so
// the browser that was signed in by setup could not sign in again with the
// password it had just chosen. Now the losing request changes nothing.
func (s *Store) CreateFirstOwner(ctx context.Context, name, display, passwordHash string) (Owner, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Owner{}, err
	}
	defer tx.Rollback()
	// The insert comes first so the transaction holds the write lock before
	// it decides anything; the NOT EXISTS makes "first" atomic.
	res, err := tx.ExecContext(ctx, `INSERT INTO owners (name, display_name, kind, created_at)
		SELECT ?, ?, 'user', ? WHERE NOT EXISTS (SELECT 1 FROM owners WHERE kind='user')`,
		name, display, now())
	if err != nil {
		return Owner{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Owner{}, ErrAlreadySetUp
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		SettingPasswordHash, passwordHash); err != nil {
		return Owner{}, err
	}
	if err := tx.Commit(); err != nil {
		return Owner{}, err
	}
	return s.OwnerByID(ctx, id)
}
