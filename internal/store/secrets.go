package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Secrets, variables and environments for Actions.
//
// Secrets and variables live at three levels: an account (a person or a
// company), a repository, and an environment of a repository. When a job runs,
// environment beats repository beats account for the same name.
//
// Secret values are stored only as ciphertext sealed by a SecretSealer (see
// internal/secrets), and no method here returns a plaintext value except
// ResolveSecrets, which the Actions engine uses to build a job.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS secrets_environments (
  id                INTEGER PRIMARY KEY,
  repo_id           INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  name              TEXT NOT NULL COLLATE NOCASE,
  require_approval  INTEGER NOT NULL DEFAULT 0,
  runner_labels     TEXT NOT NULL DEFAULT '',
  branches          TEXT NOT NULL DEFAULT '',
  wait_minutes      INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  created_by        TEXT NOT NULL DEFAULT '',
  created_by_kind   TEXT NOT NULL DEFAULT '',
  created_by_person INTEGER,
  updated_at        INTEGER NOT NULL,
  updated_by        TEXT NOT NULL DEFAULT '',
  updated_by_kind   TEXT NOT NULL DEFAULT '',
  updated_by_person INTEGER,
  UNIQUE (repo_id, name)
);
CREATE TABLE IF NOT EXISTS secrets_values (
  id                INTEGER PRIMARY KEY,
  scope             TEXT NOT NULL CHECK (scope IN ('account','repo','env')),
  scope_id          INTEGER NOT NULL,
  owner_id          INTEGER REFERENCES owners(id) ON DELETE CASCADE,
  repo_id           INTEGER REFERENCES repos(id) ON DELETE CASCADE,
  env_id            INTEGER REFERENCES secrets_environments(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,
  ciphertext        BLOB NOT NULL,
  created_at        INTEGER NOT NULL,
  created_by        TEXT NOT NULL DEFAULT '',
  created_by_kind   TEXT NOT NULL DEFAULT '',
  created_by_person INTEGER,
  updated_at        INTEGER NOT NULL,
  updated_by        TEXT NOT NULL DEFAULT '',
  updated_by_kind   TEXT NOT NULL DEFAULT '',
  updated_by_person INTEGER,
  UNIQUE (scope, scope_id, name)
);
CREATE TABLE IF NOT EXISTS secrets_variables (
  id                INTEGER PRIMARY KEY,
  scope             TEXT NOT NULL CHECK (scope IN ('account','repo','env')),
  scope_id          INTEGER NOT NULL,
  owner_id          INTEGER REFERENCES owners(id) ON DELETE CASCADE,
  repo_id           INTEGER REFERENCES repos(id) ON DELETE CASCADE,
  env_id            INTEGER REFERENCES secrets_environments(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,
  value             TEXT NOT NULL,
  created_at        INTEGER NOT NULL,
  created_by        TEXT NOT NULL DEFAULT '',
  created_by_kind   TEXT NOT NULL DEFAULT '',
  created_by_person INTEGER,
  updated_at        INTEGER NOT NULL,
  updated_by        TEXT NOT NULL DEFAULT '',
  updated_by_kind   TEXT NOT NULL DEFAULT '',
  updated_by_person INTEGER,
  UNIQUE (scope, scope_id, name)
);`)
}

// Limits.
const (
	MaxSecretValue    = 64 * 1024
	MaxVariableValue  = 64 * 1024
	MaxSecretName     = 200
	MaxEnvironmentLen = 100
	MaxRunnerLabels   = 20
	MaxEnvBranches    = 50
	MaxWaitMinutes    = 43200 // 30 days, like GitHub
)

var (
	ErrSecretName    = errors.New("names may only contain letters, numbers and underscores, can't start with a number, and can't start with GITHUB_ or ATELIER_")
	ErrSecretValue   = fmt.Errorf("secret values must be between 1 byte and %d KB", MaxSecretValue/1024)
	ErrVariableValue = fmt.Errorf("variable values must be at most %d KB", MaxVariableValue/1024)
	ErrEnvName       = errors.New("environment names may only contain letters, numbers, dots, dashes and underscores")
	ErrEnvExists     = errors.New("an environment with that name already exists")
	ErrEnvLabels     = errors.New("runner labels may only contain letters, numbers, dots, dashes and underscores")
	ErrEnvBranches   = errors.New("use branch names or patterns like release/* (letters, numbers, . _ - / and *)")
	ErrEnvWait       = fmt.Errorf("the wait timer must be between 0 and %d minutes", MaxWaitMinutes)
	errNoSealer      = errors.New("secrets: no sealer")
	secretNameRe     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	envNameRe        = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	runnerLabelRe    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	reservedPrefixes = []string{"GITHUB_", "ATELIER_"}
)

// NormalizeSecretName upper-cases and trims a name as typed. Names are
// case-insensitive, like GitHub's, and stored upper-case.
func NormalizeSecretName(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// ValidSecretName checks an already-normalised secret or variable name.
func ValidSecretName(name string) bool {
	if name == "" || len(name) > MaxSecretName || !secretNameRe.MatchString(name) {
		return false
	}
	for _, p := range reservedPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// ValidEnvironmentName checks an environment name.
func ValidEnvironmentName(name string) bool {
	return name != "" && len(name) <= MaxEnvironmentLen && envNameRe.MatchString(name)
}

// SecretSealer encrypts and decrypts secret values; *secrets.Keyring is one.
type SecretSealer interface {
	Seal(aad, plaintext []byte) ([]byte, error)
	Open(aad, ciphertext []byte) ([]byte, error)
}

// Secret scopes.
const (
	ScopeAccount = "account"
	ScopeRepo    = "repo"
	ScopeEnv     = "env"
)

// SecretScope says where a secret or variable lives.
type SecretScope struct {
	Kind string // ScopeAccount, ScopeRepo or ScopeEnv
	ID   int64  // owner id, repo id or environment id
}

func AccountScope(ownerID int64) SecretScope { return SecretScope{ScopeAccount, ownerID} }
func RepoScope(repoID int64) SecretScope     { return SecretScope{ScopeRepo, repoID} }
func EnvScope(envID int64) SecretScope       { return SecretScope{ScopeEnv, envID} }

func (sc SecretScope) valid() bool {
	return (sc.Kind == ScopeAccount || sc.Kind == ScopeRepo || sc.Kind == ScopeEnv) && sc.ID > 0
}

// fks returns owner_id, repo_id, env_id for the scope (one set, two nil).
func (sc SecretScope) fks() (any, any, any) {
	switch sc.Kind {
	case ScopeAccount:
		return sc.ID, nil, nil
	case ScopeRepo:
		return nil, sc.ID, nil
	}
	return nil, nil, sc.ID
}

// secretAAD binds a ciphertext to the row it belongs to.
func secretAAD(sc SecretScope, name string) []byte {
	return []byte("atelier-secret\x00" + sc.Kind + "\x00" + strconv.FormatInt(sc.ID, 10) + "\x00" + name)
}

// SecretsBy records who made a change (the web Actor's Name, Kind, PersonID).
type SecretsBy struct {
	Name     string
	Kind     string
	PersonID int64
}

func (b SecretsBy) person() any {
	if b.PersonID == 0 {
		return nil
	}
	return b.PersonID
}

// SecretMeta is what may be shown about a secret: never its value.
type SecretMeta struct {
	Name          string
	CreatedAt     time.Time
	CreatedBy     string
	UpdatedAt     time.Time
	UpdatedBy     string
	UpdatedByKind string
}

// Variable is a plain-text configuration variable.
type Variable struct {
	Name          string
	Value         string
	CreatedAt     time.Time
	CreatedBy     string
	UpdatedAt     time.Time
	UpdatedBy     string
	UpdatedByKind string
}

// ---------- secrets ----------

// SetSecret creates or replaces a secret's value.
func (s *Store) SetSecret(ctx context.Context, sealer SecretSealer, sc SecretScope, name, value string, by SecretsBy) error {
	if sealer == nil {
		return errNoSealer
	}
	if !sc.valid() {
		return errors.New("secrets: bad scope")
	}
	if !ValidSecretName(name) {
		return ErrSecretName
	}
	if value == "" || len(value) > MaxSecretValue {
		return ErrSecretValue
	}
	ct, err := sealer.Seal(secretAAD(sc, name), []byte(value))
	if err != nil {
		return err
	}
	o, r, e := sc.fks()
	t := now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO secrets_values
		(scope, scope_id, owner_id, repo_id, env_id, name, ciphertext, created_at, created_by, created_by_kind, created_by_person,
		 updated_at, updated_by, updated_by_kind, updated_by_person)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope, scope_id, name) DO UPDATE SET ciphertext=excluded.ciphertext,
		 updated_at=excluded.updated_at, updated_by=excluded.updated_by, updated_by_kind=excluded.updated_by_kind,
		 updated_by_person=excluded.updated_by_person`,
		sc.Kind, sc.ID, o, r, e, name, ct, t, by.Name, by.Kind, by.person(), t, by.Name, by.Kind, by.person())
	return err
}

// DeleteSecret removes a secret. Deleting a missing one returns ErrNotFound.
func (s *Store) DeleteSecret(ctx context.Context, sc SecretScope, name string) error {
	return deleteScoped(ctx, s.db, "secrets_values", sc, name)
}

func deleteScoped(ctx context.Context, db *sql.DB, table string, sc SecretScope, name string) error {
	res, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE scope=? AND scope_id=? AND name=?`, sc.Kind, sc.ID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Secrets lists the secrets in a scope by name, without values.
func (s *Store) Secrets(ctx context.Context, sc SecretScope) ([]SecretMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, created_at, created_by, updated_at, updated_by, updated_by_kind
		FROM secrets_values WHERE scope=? AND scope_id=? ORDER BY name`, sc.Kind, sc.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretMeta
	for rows.Next() {
		var m SecretMeta
		var c, u int64
		if err := rows.Scan(&m.Name, &c, &m.CreatedBy, &u, &m.UpdatedBy, &m.UpdatedByKind); err != nil {
			return nil, err
		}
		m.CreatedAt, m.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------- variables ----------

// SetVariable creates or replaces a variable.
func (s *Store) SetVariable(ctx context.Context, sc SecretScope, name, value string, by SecretsBy) error {
	if !sc.valid() {
		return errors.New("secrets: bad scope")
	}
	if !ValidSecretName(name) {
		return ErrSecretName
	}
	if len(value) > MaxVariableValue {
		return ErrVariableValue
	}
	o, r, e := sc.fks()
	t := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO secrets_variables
		(scope, scope_id, owner_id, repo_id, env_id, name, value, created_at, created_by, created_by_kind, created_by_person,
		 updated_at, updated_by, updated_by_kind, updated_by_person)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope, scope_id, name) DO UPDATE SET value=excluded.value,
		 updated_at=excluded.updated_at, updated_by=excluded.updated_by, updated_by_kind=excluded.updated_by_kind,
		 updated_by_person=excluded.updated_by_person`,
		sc.Kind, sc.ID, o, r, e, name, value, t, by.Name, by.Kind, by.person(), t, by.Name, by.Kind, by.person())
	return err
}

// DeleteVariable removes a variable.
func (s *Store) DeleteVariable(ctx context.Context, sc SecretScope, name string) error {
	return deleteScoped(ctx, s.db, "secrets_variables", sc, name)
}

// Variables lists the variables in a scope by name.
func (s *Store) Variables(ctx context.Context, sc SecretScope) ([]Variable, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, value, created_at, created_by, updated_at, updated_by, updated_by_kind
		FROM secrets_variables WHERE scope=? AND scope_id=? ORDER BY name`, sc.Kind, sc.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Variable
	for rows.Next() {
		var v Variable
		var c, u int64
		if err := rows.Scan(&v.Name, &v.Value, &c, &v.CreatedBy, &u, &v.UpdatedBy, &v.UpdatedByKind); err != nil {
			return nil, err
		}
		v.CreatedAt, v.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---------- environments ----------

// Environment is a deployment target of a repository (e.g. production).
type Environment struct {
	ID              int64
	RepoID          int64
	Name            string
	RequireApproval bool
	RunnerLabels    []string // a job may only run on a runner with all of these
	Branches        []string // branch patterns allowed to use it; empty = any branch
	WaitMinutes     int
	CreatedAt       time.Time
	CreatedBy       string
	UpdatedAt       time.Time
	UpdatedBy       string
	UpdatedByKind   string
}

// AllowsRef reports whether a job on ref ("refs/heads/main" or "main") may use
// the environment.
func (e Environment) AllowsRef(ref string) bool {
	if len(e.Branches) == 0 {
		return true
	}
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok && strings.HasPrefix(ref, "refs/") {
		return false // tags, pull request refs…
	}
	if branch == "" {
		return false
	}
	for _, p := range e.Branches {
		if MatchBranch(p, branch) {
			return true
		}
	}
	return false
}

// ParseList splits a comma, space or newline separated list, dropping blanks
// and duplicates.
func ParseList(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' })
	var out []string
	seen := map[string]bool{}
	for _, x := range f {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// Validate checks an environment's fields.
func (e Environment) Validate() error {
	if !ValidEnvironmentName(e.Name) {
		return ErrEnvName
	}
	if len(e.RunnerLabels) > MaxRunnerLabels {
		return ErrEnvLabels
	}
	for _, l := range e.RunnerLabels {
		if !runnerLabelRe.MatchString(l) {
			return ErrEnvLabels
		}
	}
	if len(e.Branches) > MaxEnvBranches {
		return ErrEnvBranches
	}
	for _, b := range e.Branches {
		if !ValidBranchPattern(b) {
			return ErrEnvBranches
		}
	}
	if e.WaitMinutes < 0 || e.WaitMinutes > MaxWaitMinutes {
		return ErrEnvWait
	}
	return nil
}

const envSelect = `SELECT id, repo_id, name, require_approval, runner_labels, branches, wait_minutes,
	created_at, created_by, updated_at, updated_by, updated_by_kind FROM secrets_environments`

func scanEnv(row interface{ Scan(...any) error }) (Environment, error) {
	var e Environment
	var labels, branches string
	var appr int
	var c, u int64
	err := row.Scan(&e.ID, &e.RepoID, &e.Name, &appr, &labels, &branches, &e.WaitMinutes, &c, &e.CreatedBy, &u, &e.UpdatedBy, &e.UpdatedByKind)
	e.RequireApproval = appr != 0
	e.RunnerLabels, e.Branches = ParseList(labels), ParseList(branches)
	e.CreatedAt, e.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return e, err
}

// Environments lists a repository's environments by name.
func (s *Store) Environments(ctx context.Context, repoID int64) ([]Environment, error) {
	rows, err := s.db.QueryContext(ctx, envSelect+` WHERE repo_id=? ORDER BY name COLLATE NOCASE`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Environment
	for rows.Next() {
		e, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnvironmentByName finds a repository's environment (case-insensitive).
func (s *Store) EnvironmentByName(ctx context.Context, repoID int64, name string) (Environment, error) {
	e, err := scanEnv(s.db.QueryRowContext(ctx, envSelect+` WHERE repo_id=? AND name=?`, repoID, name))
	return e, notFound(err)
}

// EnvironmentByID finds an environment of a repository.
func (s *Store) EnvironmentByID(ctx context.Context, repoID, id int64) (Environment, error) {
	e, err := scanEnv(s.db.QueryRowContext(ctx, envSelect+` WHERE repo_id=? AND id=?`, repoID, id))
	return e, notFound(err)
}

// SaveEnvironment creates (ID 0) or updates an environment.
func (s *Store) SaveEnvironment(ctx context.Context, e Environment, by SecretsBy) (Environment, error) {
	if err := e.Validate(); err != nil {
		return Environment{}, err
	}
	labels, branches := strings.Join(e.RunnerLabels, ","), strings.Join(e.Branches, ",")
	t := now()
	if e.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO secrets_environments
			(repo_id, name, require_approval, runner_labels, branches, wait_minutes,
			 created_at, created_by, created_by_kind, created_by_person, updated_at, updated_by, updated_by_kind, updated_by_person)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.RepoID, e.Name, e.RequireApproval, labels, branches, e.WaitMinutes, t, by.Name, by.Kind, by.person(), t, by.Name, by.Kind, by.person())
		if err != nil {
			if isUnique(err) {
				return Environment{}, ErrEnvExists
			}
			return Environment{}, err
		}
		id, _ := res.LastInsertId()
		return s.EnvironmentByID(ctx, e.RepoID, id)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE secrets_environments SET name=?, require_approval=?, runner_labels=?, branches=?,
		wait_minutes=?, updated_at=?, updated_by=?, updated_by_kind=?, updated_by_person=? WHERE id=? AND repo_id=?`,
		e.Name, e.RequireApproval, labels, branches, e.WaitMinutes, t, by.Name, by.Kind, by.person(), e.ID, e.RepoID)
	if err != nil {
		if isUnique(err) {
			return Environment{}, ErrEnvExists
		}
		return Environment{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Environment{}, ErrNotFound
	}
	return s.EnvironmentByID(ctx, e.RepoID, e.ID)
}

// DeleteEnvironment removes an environment with its secrets and variables.
func (s *Store) DeleteEnvironment(ctx context.Context, repoID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets_environments WHERE id=? AND repo_id=?`, id, repoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---------- resolving for jobs ----------

// resolveScopes returns the scopes that apply to a job, lowest precedence
// first: account, repository, environment (if env names one that exists).
func (s *Store) resolveScopes(ctx context.Context, repoID int64, env string) ([]SecretScope, error) {
	var ownerID int64
	if err := s.db.QueryRowContext(ctx, `SELECT owner_id FROM repos WHERE id=?`, repoID).Scan(&ownerID); err != nil {
		return nil, notFound(err)
	}
	scopes := []SecretScope{AccountScope(ownerID), RepoScope(repoID)}
	if env != "" {
		e, err := s.EnvironmentByName(ctx, repoID, env)
		switch {
		case err == nil:
			scopes = append(scopes, EnvScope(e.ID))
		case !errors.Is(err, ErrNotFound):
			return nil, err
		}
	}
	return scopes, nil
}

// ResolveSecrets returns the values of the requested secrets for a job in a
// repository (and environment, if env isn't empty). Only names that were
// asked for are returned, keyed as they were asked; names are matched
// case-insensitively. Environment beats repository beats account.
func (s *Store) ResolveSecrets(ctx context.Context, sealer SecretSealer, repoID int64, env string, names []string) (map[string]string, error) {
	out := map[string]string{}
	want := map[string][]string{} // normalised → as asked
	for _, n := range names {
		norm := NormalizeSecretName(n)
		if ValidSecretName(norm) {
			want[norm] = append(want[norm], n)
		}
	}
	if len(want) == 0 {
		return out, nil
	}
	if sealer == nil {
		return nil, errNoSealer
	}
	scopes, err := s.resolveScopes(ctx, repoID, env)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	found := map[string]string{}
	for _, sc := range scopes { // later scopes overwrite earlier ones
		args := []any{sc.Kind, sc.ID}
		for _, k := range keys {
			args = append(args, k)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT name, ciphertext FROM secrets_values WHERE scope=? AND scope_id=? AND name IN (?`+
			strings.Repeat(",?", len(keys)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		type sealed struct {
			name string
			ct   []byte
		}
		var got []sealed
		for rows.Next() {
			var x sealed
			if err := rows.Scan(&x.name, &x.ct); err != nil {
				rows.Close()
				return nil, err
			}
			got = append(got, x)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, x := range got {
			pt, err := sealer.Open(secretAAD(sc, x.name), x.ct)
			if err != nil {
				return nil, fmt.Errorf("secret %s: %w", x.name, err)
			}
			found[x.name] = string(pt)
		}
	}
	for norm, v := range found {
		for _, asked := range want[norm] {
			out[asked] = v
		}
	}
	return out, nil
}

// ResolveVariables returns every variable visible to a job, merged with
// environment beating repository beating account.
func (s *Store) ResolveVariables(ctx context.Context, repoID int64, env string) (map[string]string, error) {
	scopes, err := s.resolveScopes(ctx, repoID, env)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, sc := range scopes {
		vars, err := s.Variables(ctx, sc)
		if err != nil {
			return nil, err
		}
		for _, v := range vars {
			out[v.Name] = v.Value
		}
	}
	return out, nil
}

// ---------- key rotation ----------

// RotateSecrets re-encrypts every secret: each value is opened with from and
// sealed with to, in one transaction. It returns how many were re-encrypted.
func (s *Store) RotateSecrets(ctx context.Context, from, to SecretSealer) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	type row struct {
		id    int64
		sc    SecretScope
		name  string
		ct    []byte
		fresh []byte
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, scope, scope_id, name, ciphertext FROM secrets_values ORDER BY id`)
	if err != nil {
		return 0, err
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.sc.Kind, &r.sc.ID, &r.name, &r.ct); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for i := range all {
		r := &all[i]
		aad := secretAAD(r.sc, r.name)
		pt, err := from.Open(aad, r.ct)
		if err != nil {
			return 0, fmt.Errorf("secret %s (%s %d): %w", r.name, r.sc.Kind, r.sc.ID, err)
		}
		if r.fresh, err = to.Seal(aad, pt); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE secrets_values SET ciphertext=? WHERE id=?`, r.fresh, r.id); err != nil {
			return 0, err
		}
	}
	n := len(all)
	for _, c := range sealedColumns {
		m, err := rotateSealedColumn(ctx, tx, c, from, to)
		if err != nil {
			return 0, err
		}
		n += m
	}
	return n, tx.Commit()
}

// sealedColumn is another feature's encrypted column that rotate-key must
// re-seal along with Actions secrets.
type sealedColumn struct {
	what, table, idCol, ctCol string
	aad                       func(id int64) []byte
}

var sealedColumns = []sealedColumn{
	{what: "webhook secret", table: "apps_webhooks", idCol: "id", ctCol: "secret_ct", aad: webhookAAD},
	{what: "Tether token", table: "tether_accounts", idCol: "person_id", ctCol: "token_ct", aad: tetherAAD},
}

func rotateSealedColumn(ctx context.Context, tx *sql.Tx, c sealedColumn, from, to SecretSealer) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+c.idCol+`, `+c.ctCol+` FROM `+c.table+
		` WHERE `+c.ctCol+` IS NOT NULL AND length(`+c.ctCol+`) > 0`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id int64
		ct []byte
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.ct); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range all {
		pt, err := from.Open(c.aad(r.id), r.ct)
		if err != nil {
			return 0, fmt.Errorf("%s %d: %w", c.what, r.id, err)
		}
		fresh, err := to.Seal(c.aad(r.id), pt)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+c.table+` SET `+c.ctCol+`=? WHERE `+c.idCol+`=?`, fresh, r.id); err != nil {
			return 0, err
		}
	}
	return len(all), nil
}

// SecretCount is how many secrets are stored in total (all scopes).
func (s *Store) SecretCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets_values`).Scan(&n)
	return n, err
}
