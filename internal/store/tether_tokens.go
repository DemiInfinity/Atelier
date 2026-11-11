package store

// More GitHub tokens for Tether.
//
// A fine-grained GitHub token covers one owner (a user or one organisation),
// so a person may need several. The token in tether_accounts stays the
// person's main token (search, feed, profiles, badges, and any owner no other
// token covers). Every extra token lives in tether_tokens with the GitHub
// owners it covers; repository work for one of those owners uses it.
//
// Nothing is migrated: an existing tether_accounts row keeps working exactly
// as before, and a person with no rows here behaves as they always did.
//
// Like the main token, each extra token is stored only as ciphertext sealed by
// a SecretSealer, bound to its person and its own id, and only
// TetherExtraTokenSecret returns it.

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS tether_tokens (
  id             INTEGER PRIMARY KEY,
  person_id      INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  label          TEXT NOT NULL DEFAULT '',
  gh_login       TEXT NOT NULL DEFAULT '',
  owners         TEXT NOT NULL DEFAULT '',
  method         TEXT NOT NULL DEFAULT 'pat',
  scopes         TEXT NOT NULL DEFAULT '',
  token_ct       BLOB NOT NULL,
  rate_limit     INTEGER NOT NULL DEFAULT 0,
  rate_remaining INTEGER NOT NULL DEFAULT -1,
  rate_reset     INTEGER NOT NULL DEFAULT 0,
  last_error     TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS tether_tokens_person ON tether_tokens(person_id);`)
}

// MaxTetherTokens bounds how many extra tokens one person may add.
const MaxTetherTokens = 20

// TetherExtraToken is one of a person's extra GitHub tokens (never the token).
type TetherExtraToken struct {
	ID            int64
	PersonID      int64
	Label         string
	Login         string   // the GitHub login the token belongs to
	Owners        []string // GitHub owners it covers, as typed (compared case-insensitively)
	Method        string
	Scopes        string
	RateLimit     int
	RateRemaining int // -1 = unknown
	RateReset     time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Covers reports whether the token covers a GitHub owner (case-insensitive).
func (t TetherExtraToken) Covers(owner string) bool {
	for _, o := range t.Owners {
		if strings.EqualFold(o, owner) {
			return true
		}
	}
	return false
}

func tetherTokenAAD(id, personID int64) []byte {
	return []byte("atelier-tether-token\x00" + strconv.FormatInt(personID, 10) + "\x00" + strconv.FormatInt(id, 10))
}

// NormalizeTetherOwners splits a list of owners (commas or spaces), drops
// duplicates (case-insensitively) and empties, and keeps the typed case.
func NormalizeTetherOwners(owners []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, o := range owners {
		for _, f := range strings.FieldsFunc(o, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r' }) {
			k := strings.ToLower(f)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

const tetherTokenCols = `id, person_id, label, gh_login, owners, method, scopes, rate_limit, rate_remaining, rate_reset,
	last_error, created_at, updated_at`

func scanTetherExtraToken(row interface{ Scan(...any) error }) (TetherExtraToken, error) {
	var t TetherExtraToken
	var owners string
	var reset, c, u int64
	err := row.Scan(&t.ID, &t.PersonID, &t.Label, &t.Login, &owners, &t.Method, &t.Scopes, &t.RateLimit, &t.RateRemaining,
		&reset, &t.LastError, &c, &u)
	t.Owners = NormalizeTetherOwners([]string{owners})
	if reset > 0 {
		t.RateReset = time.Unix(reset, 0)
	}
	t.CreatedAt, t.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return t, err
}

// AddTetherExtraToken stores another token for a person. The token is sealed
// with an AAD bound to the person and the new row's id.
func (s *Store) AddTetherExtraToken(ctx context.Context, sealer SecretSealer, t TetherExtraToken, token string) (TetherExtraToken, error) {
	if sealer == nil {
		return t, errNoSealer
	}
	t.Owners = NormalizeTetherOwners(t.Owners)
	if t.PersonID == 0 || token == "" {
		return t, errors.New("tether: a token needs a person and a value")
	}
	if len(t.Owners) == 0 {
		return t, errors.New("tether: say which GitHub owners the token covers")
	}
	if t.Method == "" {
		t.Method = "pat"
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM tether_tokens WHERE person_id=?`, t.PersonID).Scan(&n); err != nil {
			return err
		}
		if n >= MaxTetherTokens {
			return errors.New("tether: that's as many tokens as one person can add")
		}
		ts := now()
		res, err := tx.ExecContext(ctx, `INSERT INTO tether_tokens (person_id, label, gh_login, owners, method, scopes, token_ct,
			created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, x'', ?, ?)`,
			t.PersonID, t.Label, t.Login, strings.Join(t.Owners, ","), t.Method, t.Scopes, ts, ts)
		if err != nil {
			return err
		}
		if t.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		ct, err := sealer.Seal(tetherTokenAAD(t.ID, t.PersonID), []byte(token))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE tether_tokens SET token_ct=? WHERE id=?`, ct, t.ID)
		t.CreatedAt, t.UpdatedAt, t.RateRemaining = time.Unix(ts, 0), time.Unix(ts, 0), -1
		return err
	})
	return t, err
}

// TetherExtraTokens lists a person's extra tokens, oldest first.
func (s *Store) TetherExtraTokens(ctx context.Context, personID int64) ([]TetherExtraToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tetherTokenCols+` FROM tether_tokens WHERE person_id=? ORDER BY id`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TetherExtraToken
	for rows.Next() {
		t, err := scanTetherExtraToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TetherExtraToken returns one of a person's extra tokens (not the secret).
func (s *Store) TetherExtraToken(ctx context.Context, personID, id int64) (TetherExtraToken, error) {
	t, err := scanTetherExtraToken(s.db.QueryRowContext(ctx, `SELECT `+tetherTokenCols+` FROM tether_tokens WHERE id=? AND person_id=?`, id, personID))
	return t, notFound(err)
}

// TetherTokenForOwner finds the person's extra token covering a GitHub owner
// (case-insensitive; the oldest wins). ErrNotFound means the main token applies.
func (s *Store) TetherTokenForOwner(ctx context.Context, personID int64, owner string) (TetherExtraToken, error) {
	if owner == "" {
		return TetherExtraToken{}, ErrNotFound
	}
	all, err := s.TetherExtraTokens(ctx, personID)
	if err != nil {
		return TetherExtraToken{}, err
	}
	for _, t := range all {
		if t.Covers(owner) {
			return t, nil
		}
	}
	return TetherExtraToken{}, ErrNotFound
}

// TetherExtraTokenSecret opens one of a person's extra tokens. Callers must
// never log or render it. Another person's id is simply not found.
func (s *Store) TetherExtraTokenSecret(ctx context.Context, sealer SecretSealer, personID, id int64) (string, error) {
	if sealer == nil {
		return "", errNoSealer
	}
	var ct []byte
	if err := s.db.QueryRowContext(ctx, `SELECT token_ct FROM tether_tokens WHERE id=? AND person_id=?`, id, personID).Scan(&ct); err != nil {
		return "", notFound(err)
	}
	pt, err := sealer.Open(tetherTokenAAD(id, personID), ct)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// SetTetherExtraTokenOwners changes which owners a token covers.
func (s *Store) SetTetherExtraTokenOwners(ctx context.Context, personID, id int64, owners []string) error {
	owners = NormalizeTetherOwners(owners)
	if len(owners) == 0 {
		return errors.New("tether: say which GitHub owners the token covers")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tether_tokens SET owners=?, updated_at=? WHERE id=? AND person_id=?`,
		strings.Join(owners, ","), now(), id, personID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTetherExtraToken removes one of a person's extra tokens and what was
// cached with it.
func (s *Store) DeleteTetherExtraToken(ctx context.Context, personID, id int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM tether_tokens WHERE id=? AND person_id=?`, id, personID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM tether_cache WHERE person_id=? AND key LIKE ?`, personID, "%"+TetherTokenCacheSuffix(id))
		return err
	})
}

// DeleteTetherExtraTokens removes all of a person's extra tokens.
func (s *Store) DeleteTetherExtraTokens(ctx context.Context, personID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tether_tokens WHERE person_id=?`, personID)
	return err
}

// TetherTokenCacheSuffix marks cache keys stored with an extra token, so what
// one token could see is never answered to another.
func TetherTokenCacheSuffix(id int64) string { return "#token-" + strconv.FormatInt(id, 10) }

// SetTetherExtraTokenRate records the last rate limit GitHub reported for a token.
func (s *Store) SetTetherExtraTokenRate(ctx context.Context, personID, id int64, limit, remaining int, reset time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tether_tokens SET rate_limit=?, rate_remaining=?, rate_reset=? WHERE id=? AND person_id=?`,
		limit, remaining, reset.Unix(), id, personID)
	return err
}

// SetTetherExtraTokenError records (or clears) a token's last problem.
func (s *Store) SetTetherExtraTokenError(ctx context.Context, personID, id int64, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tether_tokens SET last_error=? WHERE id=? AND person_id=?`, msg, id, personID)
	return err
}
