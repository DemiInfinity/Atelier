package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// SSH keys let the owner push and clone over SSH. Each row is one public key
// in authorized_keys form ("ssh-ed25519 AAAA…"), identified by its SHA256
// fingerprint. Kind "owner" keys have full access to every repository.
func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS ssh_keys (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL,
  kind          TEXT NOT NULL DEFAULT 'owner',
  key_type      TEXT NOT NULL,
  fingerprint   TEXT NOT NULL UNIQUE,
  public_key    TEXT NOT NULL,
  added_by      TEXT NOT NULL DEFAULT '',
  added_by_kind TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  last_used_at  INTEGER
);`)
}

// ErrKeyExists is returned when adding an SSH key or passkey that is already registered.
var ErrKeyExists = errors.New("already exists")

type SSHKey struct {
	ID          int64
	Name        string
	Kind        string // "owner" (full access)
	Type        string // e.g. "ssh-ed25519"
	Fingerprint string // "SHA256:…"
	PublicKey   string // authorized_keys line without comment
	AddedBy     string
	AddedByKind string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
}

const sshKeySelect = `SELECT id, name, kind, key_type, fingerprint, public_key, added_by, added_by_kind, created_at, last_used_at FROM ssh_keys`

func scanSSHKey(row interface{ Scan(...any) error }) (SSHKey, error) {
	var k SSHKey
	var created int64
	var used sql.NullInt64
	if err := row.Scan(&k.ID, &k.Name, &k.Kind, &k.Type, &k.Fingerprint, &k.PublicKey, &k.AddedBy, &k.AddedByKind, &created, &used); err != nil {
		return k, err
	}
	k.CreatedAt = time.Unix(created, 0)
	if used.Valid {
		v := time.Unix(used.Int64, 0)
		k.LastUsedAt = &v
	}
	return k, nil
}

// AddSSHKey stores an already-validated public key. It returns ErrKeyExists
// when a key with the same fingerprint exists.
func (s *Store) AddSSHKey(ctx context.Context, name, keyType, fingerprint, publicKey, addedBy, addedByKind string) (SSHKey, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ssh_keys (name, kind, key_type, fingerprint, public_key, added_by, added_by_kind, created_at)
		 VALUES (?, 'owner', ?, ?, ?, ?, ?, ?)`,
		name, keyType, fingerprint, publicKey, addedBy, addedByKind, now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return SSHKey{}, ErrKeyExists
		}
		return SSHKey{}, err
	}
	id, _ := res.LastInsertId()
	return scanSSHKey(s.db.QueryRowContext(ctx, sshKeySelect+` WHERE id=?`, id))
}

// SSHKeys lists keys, newest first.
func (s *Store) SSHKeys(ctx context.Context) ([]SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, sshKeySelect+` ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHKey
	for rows.Next() {
		k, err := scanSSHKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SSHKeyByFingerprint finds a key by its "SHA256:…" fingerprint.
func (s *Store) SSHKeyByFingerprint(ctx context.Context, fingerprint string) (SSHKey, error) {
	k, err := scanSSHKey(s.db.QueryRowContext(ctx, sshKeySelect+` WHERE fingerprint=?`, fingerprint))
	return k, notFound(err)
}

// TouchSSHKey records that a key was just used to authenticate.
func (s *Store) TouchSSHKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE ssh_keys SET last_used_at=? WHERE id=?`, now(), id)
	return err
}

// DeleteSSHKey removes a key. Deleting a missing key returns ErrNotFound.
func (s *Store) DeleteSSHKey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM ssh_keys WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
