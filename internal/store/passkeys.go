package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Passkeys (WebAuthn credentials) let the owner sign in without a password.
// The credential itself is kept as opaque JSON produced by the WebAuthn
// library; the store only indexes it by credential ID.
func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS passkeys (
  id             INTEGER PRIMARY KEY,
  credential_id  BLOB NOT NULL UNIQUE,
  name           TEXT NOT NULL,
  credential     TEXT NOT NULL,
  added_by       TEXT NOT NULL DEFAULT '',
  added_by_kind  TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  last_used_at   INTEGER
);`)
}

// SettingWebAuthnUserID holds the owner's random WebAuthn user handle (hex).
const SettingWebAuthnUserID = "webauthn.user_id"

type Passkey struct {
	ID           int64
	CredentialID []byte
	Name         string
	Credential   []byte // JSON from the WebAuthn library
	AddedBy      string
	AddedByKind  string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
}

const passkeySelect = `SELECT id, credential_id, name, credential, added_by, added_by_kind, created_at, last_used_at FROM passkeys`

func scanPasskey(row interface{ Scan(...any) error }) (Passkey, error) {
	var p Passkey
	var cred string
	var created int64
	var used sql.NullInt64
	if err := row.Scan(&p.ID, &p.CredentialID, &p.Name, &cred, &p.AddedBy, &p.AddedByKind, &created, &used); err != nil {
		return p, err
	}
	p.Credential = []byte(cred)
	p.CreatedAt = time.Unix(created, 0)
	if used.Valid {
		v := time.Unix(used.Int64, 0)
		p.LastUsedAt = &v
	}
	return p, nil
}

// WebAuthnUserID returns the owner's stable WebAuthn user handle, creating it
// the first time it's needed.
func (s *Store) WebAuthnUserID(ctx context.Context) ([]byte, error) {
	v, err := s.Setting(ctx, SettingWebAuthnUserID)
	if errors.Is(err, ErrNotFound) {
		v = RandomToken(32)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, SettingWebAuthnUserID, v); err != nil {
			return nil, err
		}
		v, err = s.Setting(ctx, SettingWebAuthnUserID)
	}
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(v)
}

func (s *Store) AddPasskey(ctx context.Context, credentialID []byte, name string, credential []byte, addedBy, addedByKind string) (Passkey, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO passkeys (credential_id, name, credential, added_by, added_by_kind, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		credentialID, name, string(credential), addedBy, addedByKind, now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Passkey{}, ErrKeyExists
		}
		return Passkey{}, err
	}
	id, _ := res.LastInsertId()
	return scanPasskey(s.db.QueryRowContext(ctx, passkeySelect+` WHERE id=?`, id))
}

// Passkeys lists passkeys, newest first.
func (s *Store) Passkeys(ctx context.Context) ([]Passkey, error) {
	rows, err := s.db.QueryContext(ctx, passkeySelect+` ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		p, err := scanPasskey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) PasskeyByCredentialID(ctx context.Context, credentialID []byte) (Passkey, error) {
	p, err := scanPasskey(s.db.QueryRowContext(ctx, passkeySelect+` WHERE credential_id=?`, credentialID))
	return p, notFound(err)
}

// UsePasskey stores the updated credential (sign count, flags) after a sign-in.
func (s *Store) UsePasskey(ctx context.Context, id int64, credential []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE passkeys SET credential=?, last_used_at=? WHERE id=?`, string(credential), now(), id)
	return err
}

func (s *Store) RenamePasskey(ctx context.Context, id int64, name string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE passkeys SET name=? WHERE id=?`, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeletePasskey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM passkeys WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
