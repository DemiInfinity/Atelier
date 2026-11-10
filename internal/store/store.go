// Package store keeps Atelier's metadata in a single SQLite file.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

const schema = `
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS owners (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE COLLATE NOCASE,
  display_name TEXT NOT NULL DEFAULT '',
  kind         TEXT NOT NULL CHECK (kind IN ('user','company')),
  created_at   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS repos (
  id          INTEGER PRIMARY KEY,
  owner_id    INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  name        TEXT NOT NULL COLLATE NOCASE,
  description TEXT NOT NULL DEFAULT '',
  private     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  UNIQUE (owner_id, name)
);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,
  prefix       TEXT NOT NULL,
  scopes       TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,
  last_used_at INTEGER
);
`

// extraSchemas are added by feature files in this package via RegisterSchema.
// Each must be idempotent (CREATE TABLE/INDEX IF NOT EXISTS). Never alter a
// table another feature owns; add a new table instead.
var extraSchemas []string

// RegisterSchema adds tables for a feature. Call it from an init() function.
func RegisterSchema(sqlText string) { extraSchemas = append(extraSchemas, sqlText) }

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS repo_counters (
  repo_id INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  next    INTEGER NOT NULL
);`)
}

// NextNumber hands out the next #number for a repo. Issues and pull requests
// share one sequence per repo, like GitHub.
func (s *Store) NextNumber(ctx context.Context, repoID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO repo_counters (repo_id, next) VALUES (?, 2)
		ON CONFLICT(repo_id) DO UPDATE SET next = next + 1 RETURNING next - 1`, repoID).Scan(&n)
	return n, err
}

// DB exposes the connection to feature files in this package.
func (s *Store) DB() *sql.DB { return s.db }

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: one writer keeps things simple and safe
	for i, sqlText := range append([]string{schema}, extraSchemas...) {
		if _, err := db.Exec(sqlText); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate (schema %d): %w", i, err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---------- helpers ----------

func now() int64 { return time.Now().Unix() }

func RandomToken(nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ---------- settings ----------

func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v, notFound(err)
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---------- owners ----------

type Owner struct {
	ID          int64
	Name        string
	DisplayName string
	Kind        string // "user" or "company"
	CreatedAt   time.Time
}

func (o Owner) IsCompany() bool { return o.Kind == "company" }
func (o Owner) Label() string {
	if o.DisplayName != "" {
		return o.DisplayName
	}
	return o.Name
}
func (o Owner) Initial() string {
	l := o.Label()
	if l == "" {
		return "?"
	}
	return strings.ToUpper(l[:1])
}

func scanOwner(row interface{ Scan(...any) error }) (Owner, error) {
	var o Owner
	var created int64
	err := row.Scan(&o.ID, &o.Name, &o.DisplayName, &o.Kind, &created)
	o.CreatedAt = time.Unix(created, 0)
	return o, err
}

func (s *Store) CreateOwner(ctx context.Context, name, display, kind string) (Owner, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO owners (name, display_name, kind, created_at) VALUES (?, ?, ?, ?)`, name, display, kind, now())
	if err != nil {
		return Owner{}, err
	}
	id, _ := res.LastInsertId()
	return s.OwnerByID(ctx, id)
}

func (s *Store) OwnerByID(ctx context.Context, id int64) (Owner, error) {
	o, err := scanOwner(s.db.QueryRowContext(ctx, `SELECT id, name, display_name, kind, created_at FROM owners WHERE id=?`, id))
	return o, notFound(err)
}

func (s *Store) OwnerByName(ctx context.Context, name string) (Owner, error) {
	o, err := scanOwner(s.db.QueryRowContext(ctx, `SELECT id, name, display_name, kind, created_at FROM owners WHERE name=?`, name))
	return o, notFound(err)
}

// Owners returns the personal account first, then companies by name.
func (s *Store) Owners(ctx context.Context) ([]Owner, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, display_name, kind, created_at FROM owners ORDER BY kind='company', name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Owner
	for rows.Next() {
		o, err := scanOwner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PersonalOwner is the single human account.
func (s *Store) PersonalOwner(ctx context.Context) (Owner, error) {
	o, err := scanOwner(s.db.QueryRowContext(ctx,
		`SELECT id, name, display_name, kind, created_at FROM owners WHERE kind='user' ORDER BY id LIMIT 1`))
	return o, notFound(err)
}

// ---------- repos ----------

type Repo struct {
	ID          int64
	OwnerID     int64
	Owner       string
	OwnerKind   string
	Name        string
	Description string
	Private     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

const repoSelect = `SELECT r.id, r.owner_id, o.name, o.kind, r.name, r.description, r.private, r.created_at, r.updated_at
FROM repos r JOIN owners o ON o.id = r.owner_id`

func scanRepos(rows *sql.Rows) ([]Repo, error) {
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		var r Repo
		var priv int
		var c, u int64
		if err := rows.Scan(&r.ID, &r.OwnerID, &r.Owner, &r.OwnerKind, &r.Name, &r.Description, &priv, &c, &u); err != nil {
			return nil, err
		}
		r.Private = priv != 0
		r.CreatedAt, r.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CreateRepo(ctx context.Context, ownerID int64, name, desc string, private bool) (Repo, error) {
	t := now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO repos (owner_id, name, description, private, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		ownerID, name, desc, private, t, t)
	if err != nil {
		return Repo{}, err
	}
	o, err := s.OwnerByID(ctx, ownerID)
	if err != nil {
		return Repo{}, err
	}
	return s.RepoByName(ctx, o.Name, name)
}

func (s *Store) DeleteRepo(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repos WHERE id=?`, id)
	return err
}

func (s *Store) RepoByName(ctx context.Context, owner, name string) (Repo, error) {
	rows, err := s.db.QueryContext(ctx, repoSelect+` WHERE o.name=? AND r.name=?`, owner, name)
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

func (s *Store) Repos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, repoSelect+` ORDER BY r.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	return scanRepos(rows)
}

func (s *Store) ReposByOwner(ctx context.Context, ownerID int64) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, repoSelect+` WHERE r.owner_id=? ORDER BY r.updated_at DESC`, ownerID)
	if err != nil {
		return nil, err
	}
	return scanRepos(rows)
}

func (s *Store) UpdateRepo(ctx context.Context, id int64, desc string, private bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET description=?, private=?, updated_at=? WHERE id=?`, desc, private, now(), id)
	return err
}

func (s *Store) TouchRepo(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE repos SET updated_at=? WHERE id=?`, now(), id)
	return err
}

// ---------- sessions ----------

func (s *Store) CreateSession(ctx context.Context, ttl time.Duration) (string, error) {
	tok := RandomToken(32)
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, expires_at) VALUES (?, ?)`,
		Hash(tok), time.Now().Add(ttl).Unix())
	return tok, err
}

func (s *Store) ValidSession(ctx context.Context, tok string) bool {
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM sessions WHERE token_hash=?`, Hash(tok)).Scan(&exp)
	return err == nil && exp > now()
}

// SettingPasswordHash is where the owner's bcrypt password hash is kept.
const SettingPasswordHash = "owner.password_hash"

// DeleteAllSessions signs every browser out (used after a password reset).
func (s *Store) DeleteAllSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions`)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tok string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=? OR expires_at < ?`, Hash(tok), now())
	return err
}

// ---------- access tokens ----------

type Token struct {
	ID         int64
	Name       string
	Prefix     string
	Scopes     []string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
}

func (t Token) Has(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

const TokenPrefix = "atl_"

// CreateToken returns the plaintext token once; only its hash is stored.
func (s *Store) CreateToken(ctx context.Context, name string, scopes []string, ttl time.Duration) (string, error) {
	plain := TokenPrefix + RandomToken(20)
	var exp any
	if ttl > 0 {
		exp = time.Now().Add(ttl).Unix()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (name, token_hash, prefix, scopes, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		name, Hash(plain), plain[:len(TokenPrefix)+6], strings.Join(scopes, " "), now(), exp)
	return plain, err
}

func scanToken(row interface{ Scan(...any) error }) (Token, error) {
	var t Token
	var scopes string
	var created int64
	var exp, used sql.NullInt64
	if err := row.Scan(&t.ID, &t.Name, &t.Prefix, &scopes, &created, &exp, &used); err != nil {
		return t, err
	}
	t.Scopes = strings.Fields(scopes)
	t.CreatedAt = time.Unix(created, 0)
	if exp.Valid {
		v := time.Unix(exp.Int64, 0)
		t.ExpiresAt = &v
	}
	if used.Valid {
		v := time.Unix(used.Int64, 0)
		t.LastUsedAt = &v
	}
	return t, nil
}

func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, prefix, scopes, created_at, expires_at, last_used_at FROM tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CheckToken validates a plaintext token and records its use.
func (s *Store) CheckToken(ctx context.Context, plain string) (Token, error) {
	if !strings.HasPrefix(plain, TokenPrefix) {
		return Token{}, ErrNotFound
	}
	t, err := scanToken(s.db.QueryRowContext(ctx,
		`SELECT id, name, prefix, scopes, created_at, expires_at, last_used_at FROM tokens WHERE token_hash=?`, Hash(plain)))
	if err != nil {
		return Token{}, notFound(err)
	}
	if t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) {
		return Token{}, ErrNotFound
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at=? WHERE id=?`, now(), t.ID)
	return t, nil
}

func (s *Store) DeleteToken(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id=?`, id)
	return err
}
