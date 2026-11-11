package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Account pictures and links
//
// An account (a person or a company) may have a profile picture. The image
// files live in <data>/avatars/<owner id>/ (written by the web package, which
// decodes and re-encodes every upload); this table records which version is
// current, so pages can link to it with a cache-busting hash.
//
// account_github remembers which GitHub organisation a company account
// stands for (a person's GitHub login comes from their Tether connection).
// account_emails maps commit author emails to accounts, so commits show the
// right picture.

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS account_avatars (
  owner_id     INTEGER PRIMARY KEY REFERENCES owners(id) ON DELETE CASCADE,
  hash         TEXT NOT NULL,
  source       TEXT NOT NULL DEFAULT 'upload',
  source_login TEXT NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS account_github (
  owner_id INTEGER PRIMARY KEY REFERENCES owners(id) ON DELETE CASCADE,
  login    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS account_emails (
  email    TEXT PRIMARY KEY,
  owner_id INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS account_emails_owner ON account_emails (owner_id);`)
}

// Avatar sources.
const (
	AvatarUpload = "upload"
	AvatarGitHub = "github"
)

// AccountAvatar is an account's current picture.
type AccountAvatar struct {
	OwnerID     int64
	Hash        string // of the stored files; changes with every new picture
	Source      string // AvatarUpload or AvatarGitHub
	SourceLogin string // the GitHub login it came from
	UpdatedAt   time.Time
}

// AccountAvatar returns an account's picture, or ErrNotFound.
func (s *Store) AccountAvatar(ctx context.Context, ownerID int64) (AccountAvatar, error) {
	v := AccountAvatar{OwnerID: ownerID}
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT hash, source, source_login, updated_at FROM account_avatars WHERE owner_id=?`, ownerID).
		Scan(&v.Hash, &v.Source, &v.SourceLogin, &at)
	if err != nil {
		return AccountAvatar{}, notFound(err)
	}
	v.UpdatedAt = time.Unix(at, 0)
	return v, nil
}

// AccountAvatars returns every account's picture, by owner id.
func (s *Store) AccountAvatars(ctx context.Context) (map[int64]AccountAvatar, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_id, hash, source, source_login, updated_at FROM account_avatars`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]AccountAvatar{}
	for rows.Next() {
		var v AccountAvatar
		var at int64
		if err := rows.Scan(&v.OwnerID, &v.Hash, &v.Source, &v.SourceLogin, &at); err != nil {
			return nil, err
		}
		v.UpdatedAt = time.Unix(at, 0)
		out[v.OwnerID] = v
	}
	return out, rows.Err()
}

// SaveAccountAvatar records an account's new picture.
func (s *Store) SaveAccountAvatar(ctx context.Context, v AccountAvatar) error {
	if v.OwnerID == 0 || v.Hash == "" {
		return errors.New("avatar needs an account and a hash")
	}
	if v.Source == "" {
		v.Source = AvatarUpload
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_avatars (owner_id, hash, source, source_login, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(owner_id) DO UPDATE SET hash=excluded.hash, source=excluded.source,
		source_login=excluded.source_login, updated_at=excluded.updated_at`, v.OwnerID, v.Hash, v.Source, v.SourceLogin, now())
	return err
}

// DeleteAccountAvatar forgets an account's picture.
func (s *Store) DeleteAccountAvatar(ctx context.Context, ownerID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM account_avatars WHERE owner_id=?`, ownerID)
	return err
}

// AccountGitHub is the GitHub login (an organisation) a company account is
// linked to, or "" when none was set.
func (s *Store) AccountGitHub(ctx context.Context, ownerID int64) (string, error) {
	var login string
	err := s.db.QueryRowContext(ctx, `SELECT login FROM account_github WHERE owner_id=?`, ownerID).Scan(&login)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return login, err
}

// AccountGitHubs returns every explicit GitHub link, by owner id.
func (s *Store) AccountGitHubs(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT owner_id, login FROM account_github`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var login string
		if err := rows.Scan(&id, &login); err != nil {
			return nil, err
		}
		out[id] = login
	}
	return out, rows.Err()
}

// SetAccountGitHub links (or, with "", unlinks) an account to a GitHub login.
func (s *Store) SetAccountGitHub(ctx context.Context, ownerID int64, login string) error {
	if login == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM account_github WHERE owner_id=?`, ownerID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_github (owner_id, login) VALUES (?, ?)
		ON CONFLICT(owner_id) DO UPDATE SET login=excluded.login`, ownerID, login)
	return err
}

// ErrEmailTaken is returned when an email already belongs to another account.
var ErrEmailTaken = errors.New("that email belongs to another account")

// AccountEmails lists the commit emails of an account.
func (s *Store) AccountEmails(ctx context.Context, ownerID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email FROM account_emails WHERE owner_id=? ORDER BY email`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AllAccountEmails maps every known commit email (lower case) to its account.
func (s *Store) AllAccountEmails(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT email, owner_id FROM account_emails`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var e string
		var id int64
		if err := rows.Scan(&e, &id); err != nil {
			return nil, err
		}
		out[e] = id
	}
	return out, rows.Err()
}

// SetAccountEmails replaces an account's commit emails. Emails are stored in
// lower case; one that belongs to another account fails with ErrEmailTaken.
func (s *Store) SetAccountEmails(ctx context.Context, ownerID int64, emails []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_emails WHERE owner_id=?`, ownerID); err != nil {
		return err
	}
	for _, e := range emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		var other int64
		err := tx.QueryRowContext(ctx, `SELECT owner_id FROM account_emails WHERE email=?`, e).Scan(&other)
		if err == nil {
			if other != ownerID {
				return ErrEmailTaken
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_emails (email, owner_id) VALUES (?, ?)`, e, ownerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
