package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// People
//
// A person is an owner of kind "user". Core Atelier has exactly one (you), but
// everything that grants access — sessions, access tokens, SSH keys and
// passkeys — records which person it belongs to, so the Duo / Team / Company
// addons can add more people later without reshaping the data.

// ---------- versioned migrations ----------
//
// RegisterSchema only creates tables. Changes to existing tables go here, in
// order, and each runs exactly once. Never edit or reorder a released entry;
// append a new one.

type migration struct {
	name string
	run  func(ctx context.Context, tx *sql.Tx) error
}

var migrations = []migration{
	{"person ownership for sessions, tokens, ssh keys and passkeys", migratePersonOwnership},
	{"Tailscale as a remote access provider", migrateTunnelTailscale},
	{"devices allowed to rename and delete repositories", migrateAppDeviceAdmin},
}

const settingSchemaVersion = "schema.version"

func (s *Store) migrate(ctx context.Context) error {
	cur := 0
	if v, err := s.Setting(ctx, settingSchemaVersion); err == nil {
		cur, _ = strconv.Atoi(v)
	}
	for i := cur; i < len(migrations); i++ {
		m := migrations[i]
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := m.run(ctx, tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d (%s): %w", i+1, m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			settingSchemaVersion, strconv.Itoa(i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func hasColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func migratePersonOwnership(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{"sessions", "tokens", "ssh_keys", "passkeys"} {
		ok, err := hasColumn(ctx, tx, table, "person_id")
		if err != nil {
			return err
		}
		if !ok {
			if _, err := tx.ExecContext(ctx,
				`ALTER TABLE `+table+` ADD COLUMN person_id INTEGER REFERENCES owners(id) ON DELETE CASCADE`); err != nil {
				return err
			}
		}
		// Everything that exists today belongs to the one person there is.
		if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET person_id =
			(SELECT id FROM owners WHERE kind='user' ORDER BY id LIMIT 1) WHERE person_id IS NULL`); err != nil {
			return err
		}
	}
	return nil
}

// personalOwnerID is the SQL for "the personal owner", used as the default
// person for rows created through the single-person code paths.
const personalOwnerID = `(SELECT id FROM owners WHERE kind='user' ORDER BY id LIMIT 1)`

// personOrOwner is the SQL used when a row predates its person being known:
// it falls back to the personal owner.
const personOrOwner = `COALESCE(person_id, (SELECT id FROM owners WHERE kind='user' ORDER BY id LIMIT 1))`

// ---------- people ----------

// Person looks up a person (an owner of kind "user") by id.
func (s *Store) Person(ctx context.Context, id int64) (Owner, error) {
	o, err := s.OwnerByID(ctx, id)
	if err != nil {
		return Owner{}, err
	}
	if o.Kind != "user" {
		return Owner{}, ErrNotFound
	}
	return o, nil
}

// ---------- sessions ----------

// CreateSessionFor signs a specific person in.
func (s *Store) CreateSessionFor(ctx context.Context, personID int64, ttl time.Duration) (string, error) {
	if personID == 0 {
		return "", errors.New("session needs a person")
	}
	tok := RandomToken(32)
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, expires_at, person_id) VALUES (?, ?, ?)`,
		Hash(tok), time.Now().Add(ttl).Unix(), personID)
	return tok, err
}

// SessionPerson returns who a session belongs to, if it's valid.
func (s *Store) SessionPerson(ctx context.Context, tok string) (int64, bool) {
	var exp int64
	var person sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT expires_at, `+personOrOwner+` FROM sessions WHERE token_hash=?`, Hash(tok)).
		Scan(&exp, &person)
	if err != nil || exp <= now() || !person.Valid {
		return 0, false
	}
	return person.Int64, true
}

// ---------- tokens ----------

// TokenPerson returns who an access token belongs to.
func (s *Store) TokenPerson(ctx context.Context, tokenID int64) (int64, error) {
	var p sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT `+personOrOwner+` FROM tokens WHERE id=?`, tokenID).Scan(&p)
	if err != nil {
		return 0, notFound(err)
	}
	if !p.Valid {
		return 0, ErrNotFound
	}
	return p.Int64, nil
}

// AssignTokenPerson records which person a newly created token belongs to.
func (s *Store) AssignTokenPerson(ctx context.Context, plain string, personID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET person_id=? WHERE token_hash=?`, personID, Hash(plain))
	return err
}

// ---------- ssh keys and passkeys ----------

// SSHKeyPerson returns who an SSH key belongs to.
func (s *Store) SSHKeyPerson(ctx context.Context, keyID int64) (int64, error) {
	var p sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT `+personOrOwner+` FROM ssh_keys WHERE id=?`, keyID).Scan(&p)
	if err != nil {
		return 0, notFound(err)
	}
	if !p.Valid {
		return 0, ErrNotFound
	}
	return p.Int64, nil
}

// PasskeyPerson returns who a passkey belongs to.
func (s *Store) PasskeyPerson(ctx context.Context, passkeyID int64) (int64, error) {
	var p sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT `+personOrOwner+` FROM passkeys WHERE id=?`, passkeyID).Scan(&p)
	if err != nil {
		return 0, notFound(err)
	}
	if !p.Valid {
		return 0, ErrNotFound
	}
	return p.Int64, nil
}
