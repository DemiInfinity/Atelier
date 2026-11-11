package store

// Tether: Atelier's link to GitHub.
//
// Everything here belongs to a person (tether_accounts, tether_cache) or to a
// repository (tether_links and the rows that hang off a link). The GitHub
// token is stored only as ciphertext sealed by a SecretSealer, bound to its
// person, and only TetherToken returns it.

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS tether_accounts (
  person_id      INTEGER PRIMARY KEY REFERENCES owners(id) ON DELETE CASCADE,
  login          TEXT NOT NULL,
  name           TEXT NOT NULL DEFAULT '',
  avatar_url     TEXT NOT NULL DEFAULT '',
  method         TEXT NOT NULL DEFAULT 'pat',
  scopes         TEXT NOT NULL DEFAULT '',
  token_ct       BLOB NOT NULL,
  private_mirror INTEGER NOT NULL DEFAULT 0,
  badge          INTEGER NOT NULL DEFAULT 0,
  rate_limit     INTEGER NOT NULL DEFAULT 0,
  rate_remaining INTEGER NOT NULL DEFAULT -1,
  rate_reset     INTEGER NOT NULL DEFAULT 0,
  last_error     TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tether_cache (
  person_id  INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  key        TEXT NOT NULL,
  etag       TEXT NOT NULL DEFAULT '',
  body       BLOB NOT NULL,
  fetched_at INTEGER NOT NULL,
  PRIMARY KEY (person_id, key)
);
CREATE TABLE IF NOT EXISTS tether_links (
  repo_id        INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  person_id      INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  gh_repo        TEXT NOT NULL,
  home           TEXT NOT NULL CHECK (home IN ('atelier','github')),
  sync_issues    INTEGER NOT NULL DEFAULT 1,
  pushed_refs    TEXT NOT NULL DEFAULT '',
  last_push_at   INTEGER,
  push_error     TEXT NOT NULL DEFAULT '',
  last_fetch_at  INTEGER,
  fetch_error    TEXT NOT NULL DEFAULT '',
  diverged       TEXT NOT NULL DEFAULT '',
  issues_since   INTEGER NOT NULL DEFAULT 0,
  comments_since INTEGER NOT NULL DEFAULT 0,
  last_sync_at   INTEGER,
  sync_error     TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tether_items (
  repo_id   INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  kind      TEXT NOT NULL CHECK (kind IN ('issue','pull')),
  number    INTEGER NOT NULL,
  gh_number INTEGER NOT NULL,
  gh_url    TEXT NOT NULL DEFAULT '',
  origin    TEXT NOT NULL CHECK (origin IN ('github','atelier')),
  head_sha  TEXT NOT NULL DEFAULT '',
  gh_state  TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (repo_id, number),
  UNIQUE (repo_id, gh_number)
);
CREATE TABLE IF NOT EXISTS tether_comments (
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  gh_id      INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  atelier_id INTEGER NOT NULL,
  direction  TEXT NOT NULL CHECK (direction IN ('in','out')),
  PRIMARY KEY (repo_id, gh_id)
);
CREATE INDEX IF NOT EXISTS tether_comments_atelier ON tether_comments(kind, atelier_id);
CREATE TABLE IF NOT EXISTS tether_statuses (
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  sha         TEXT NOT NULL,
  context     TEXT NOT NULL,
  state       TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (repo_id, sha, context)
);
CREATE TABLE IF NOT EXISTS tether_badges (
  login      TEXT PRIMARY KEY COLLATE NOCASE,
  present    INTEGER NOT NULL DEFAULT 0,
  studio     TEXT NOT NULL DEFAULT '',
  etag       TEXT NOT NULL DEFAULT '',
  checked_at INTEGER NOT NULL
);`)
}

// ---------- accounts ----------

// TetherAccount is a person's linked GitHub account (never the token).
type TetherAccount struct {
	PersonID      int64
	Login         string
	Name          string
	AvatarURL     string
	Method        string // pat or oauth
	Scopes        string
	PrivateMirror bool
	Badge         bool
	RateLimit     int
	RateRemaining int // -1 = unknown
	RateReset     time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func tetherAAD(personID int64) []byte {
	return []byte("atelier-tether\x00" + strconv.FormatInt(personID, 10))
}

const tetherAccountCols = `person_id, login, name, avatar_url, method, scopes, private_mirror, badge, rate_limit,
	rate_remaining, rate_reset, last_error, created_at, updated_at`

func scanTetherAccount(row interface{ Scan(...any) error }) (TetherAccount, error) {
	var t TetherAccount
	var pm, badge int
	var reset, c, u int64
	err := row.Scan(&t.PersonID, &t.Login, &t.Name, &t.AvatarURL, &t.Method, &t.Scopes, &pm, &badge, &t.RateLimit,
		&t.RateRemaining, &reset, &t.LastError, &c, &u)
	t.PrivateMirror, t.Badge = pm != 0, badge != 0
	if reset > 0 {
		t.RateReset = time.Unix(reset, 0)
	}
	t.CreatedAt, t.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return t, err
}

// SaveTetherAccount links (or relinks) a person's GitHub account. The token is
// sealed before it's stored.
func (s *Store) SaveTetherAccount(ctx context.Context, sealer SecretSealer, a TetherAccount, token string) error {
	if sealer == nil {
		return errNoSealer
	}
	if a.PersonID == 0 || token == "" {
		return errors.New("tether: account needs a person and a token")
	}
	ct, err := sealer.Seal(tetherAAD(a.PersonID), []byte(token))
	if err != nil {
		return err
	}
	t := now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO tether_accounts (person_id, login, name, avatar_url, method, scopes, token_ct,
		private_mirror, badge, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(person_id) DO UPDATE SET login=excluded.login, name=excluded.name, avatar_url=excluded.avatar_url,
		method=excluded.method, scopes=excluded.scopes, token_ct=excluded.token_ct, private_mirror=excluded.private_mirror,
		last_error='', rate_remaining=-1, updated_at=excluded.updated_at`,
		a.PersonID, a.Login, a.Name, a.AvatarURL, a.Method, a.Scopes, ct, a.PrivateMirror, a.Badge, t, t)
	return err
}

func (s *Store) TetherAccount(ctx context.Context, personID int64) (TetherAccount, error) {
	a, err := scanTetherAccount(s.db.QueryRowContext(ctx, `SELECT `+tetherAccountCols+` FROM tether_accounts WHERE person_id=?`, personID))
	return a, notFound(err)
}

func (s *Store) TetherAccounts(ctx context.Context) ([]TetherAccount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tetherAccountCols+` FROM tether_accounts ORDER BY person_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TetherAccount
	for rows.Next() {
		a, err := scanTetherAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TetherToken opens a person's GitHub token. Callers must never log or render it.
func (s *Store) TetherToken(ctx context.Context, sealer SecretSealer, personID int64) (string, error) {
	if sealer == nil {
		return "", errNoSealer
	}
	var ct []byte
	if err := s.db.QueryRowContext(ctx, `SELECT token_ct FROM tether_accounts WHERE person_id=?`, personID).Scan(&ct); err != nil {
		return "", notFound(err)
	}
	pt, err := sealer.Open(tetherAAD(personID), ct)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// DeleteTetherAccount unlinks GitHub (the main token and every extra one) and
// forgets everything cached for the person.
func (s *Store) DeleteTetherAccount(ctx context.Context, personID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM tether_accounts WHERE person_id=?`, `DELETE FROM tether_tokens WHERE person_id=?`,
			`DELETE FROM tether_cache WHERE person_id=?`} {
			if _, err := tx.ExecContext(ctx, q, personID); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetTetherRate records the last rate limit GitHub reported.
func (s *Store) SetTetherRate(ctx context.Context, personID int64, limit, remaining int, reset time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tether_accounts SET rate_limit=?, rate_remaining=?, rate_reset=? WHERE person_id=?`,
		limit, remaining, reset.Unix(), personID)
	return err
}

func (s *Store) SetTetherAccountError(ctx context.Context, personID int64, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tether_accounts SET last_error=? WHERE person_id=?`, msg, personID)
	return err
}

func (s *Store) SetTetherBadge(ctx context.Context, personID int64, on bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tether_accounts SET badge=?, updated_at=? WHERE person_id=?`, on, now(), personID)
	return err
}

// ---------- cache ----------

// TetherCacheGet returns a cached body, its ETag and when it was fetched.
func (s *Store) TetherCacheGet(ctx context.Context, personID int64, key string) (etag string, body []byte, at time.Time, err error) {
	var t int64
	err = s.db.QueryRowContext(ctx, `SELECT etag, body, fetched_at FROM tether_cache WHERE person_id=? AND key=?`, personID, key).
		Scan(&etag, &body, &t)
	return etag, body, time.Unix(t, 0), notFound(err)
}

func (s *Store) TetherCachePut(ctx context.Context, personID int64, key, etag string, body []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tether_cache (person_id, key, etag, body, fetched_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(person_id, key) DO UPDATE SET etag=excluded.etag, body=excluded.body, fetched_at=excluded.fetched_at`,
		personID, key, etag, body, now())
	return err
}

// PruneTetherCache drops HTTP validators older than cutoff (feeds are kept).
func (s *Store) PruneTetherCache(ctx context.Context, cutoff time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tether_cache WHERE fetched_at < ? AND key LIKE 'GET %'`, cutoff.Unix())
	return err
}

// ---------- repository links ----------

// TetherLink ties an Atelier repository to its GitHub twin.
type TetherLink struct {
	RepoID        int64
	PersonID      int64 // whose GitHub token the link uses
	GHRepo        string
	Home          string // atelier or github
	SyncIssues    bool
	PushedRefs    string // "ref sha\n…" as last pushed to (or fetched from) GitHub
	LastPushAt    *time.Time
	PushError     string
	LastFetchAt   *time.Time
	FetchError    string
	Diverged      string // why the mains differ, "" when they match
	IssuesSince   time.Time
	CommentsSince time.Time
	LastSyncAt    *time.Time
	SyncError     string
	CreatedAt     time.Time
}

// Home values.
const (
	TetherHomeAtelier = "atelier"
	TetherHomeGitHub  = "github"
)

const tetherLinkCols = `repo_id, person_id, gh_repo, home, sync_issues, pushed_refs, last_push_at, push_error, last_fetch_at,
	fetch_error, diverged, issues_since, comments_since, last_sync_at, sync_error, created_at`

func optTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0)
	return &t
}

func timeOpt(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func scanTetherLink(row interface{ Scan(...any) error }) (TetherLink, error) {
	var l TetherLink
	var sync int
	var push, fetch, synced sql.NullInt64
	var is, cs, c int64
	err := row.Scan(&l.RepoID, &l.PersonID, &l.GHRepo, &l.Home, &sync, &l.PushedRefs, &push, &l.PushError, &fetch,
		&l.FetchError, &l.Diverged, &is, &cs, &synced, &l.SyncError, &c)
	l.SyncIssues = sync != 0
	l.LastPushAt, l.LastFetchAt, l.LastSyncAt = optTime(push), optTime(fetch), optTime(synced)
	if is > 0 {
		l.IssuesSince = time.Unix(is, 0)
	}
	if cs > 0 {
		l.CommentsSince = time.Unix(cs, 0)
	}
	l.CreatedAt = time.Unix(c, 0)
	return l, err
}

// SaveTetherLink creates or replaces a link.
func (s *Store) SaveTetherLink(ctx context.Context, l TetherLink) error {
	if l.Home != TetherHomeAtelier && l.Home != TetherHomeGitHub {
		return errors.New("tether: home must be atelier or github")
	}
	var is, cs int64
	if !l.IssuesSince.IsZero() {
		is = l.IssuesSince.Unix()
	}
	if !l.CommentsSince.IsZero() {
		cs = l.CommentsSince.Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tether_links (`+tetherLinkCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id) DO UPDATE SET person_id=excluded.person_id, gh_repo=excluded.gh_repo, home=excluded.home,
		sync_issues=excluded.sync_issues, pushed_refs=excluded.pushed_refs, last_push_at=excluded.last_push_at,
		push_error=excluded.push_error, last_fetch_at=excluded.last_fetch_at, fetch_error=excluded.fetch_error,
		diverged=excluded.diverged, issues_since=excluded.issues_since, comments_since=excluded.comments_since,
		last_sync_at=excluded.last_sync_at, sync_error=excluded.sync_error`,
		l.RepoID, l.PersonID, l.GHRepo, l.Home, l.SyncIssues, l.PushedRefs, timeOpt(l.LastPushAt), l.PushError,
		timeOpt(l.LastFetchAt), l.FetchError, l.Diverged, is, cs, timeOpt(l.LastSyncAt), l.SyncError, now())
	return err
}

func (s *Store) TetherLink(ctx context.Context, repoID int64) (TetherLink, error) {
	l, err := scanTetherLink(s.db.QueryRowContext(ctx, `SELECT `+tetherLinkCols+` FROM tether_links WHERE repo_id=?`, repoID))
	return l, notFound(err)
}

func (s *Store) TetherLinks(ctx context.Context) ([]TetherLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tetherLinkCols+` FROM tether_links ORDER BY repo_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TetherLink
	for rows.Next() {
		l, err := scanTetherLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteTetherLink unlinks a repository and forgets its synced state.
func (s *Store) DeleteTetherLink(ctx context.Context, repoID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, t := range []string{"tether_links", "tether_items", "tether_comments", "tether_statuses"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE repo_id=?`, repoID); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------- items (issues and pull requests that exist on both sides) ----------

type TetherItem struct {
	RepoID   int64
	Kind     string // issue or pull
	Number   int64  // in Atelier
	GHNumber int64  // on GitHub
	GHURL    string
	Origin   string // github (synced in) or atelier (mirrored out)
	HeadSHA  string // pull requests: the head last seen on GitHub
	GHState  string // open, closed, merged
}

const tetherItemCols = `repo_id, kind, number, gh_number, gh_url, origin, head_sha, gh_state`

func scanTetherItem(row interface{ Scan(...any) error }) (TetherItem, error) {
	var t TetherItem
	err := row.Scan(&t.RepoID, &t.Kind, &t.Number, &t.GHNumber, &t.GHURL, &t.Origin, &t.HeadSHA, &t.GHState)
	return t, err
}

func (s *Store) SaveTetherItem(ctx context.Context, t TetherItem) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tether_items (`+tetherItemCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, number) DO UPDATE SET kind=excluded.kind, gh_number=excluded.gh_number, gh_url=excluded.gh_url,
		origin=excluded.origin, head_sha=excluded.head_sha, gh_state=excluded.gh_state`,
		t.RepoID, t.Kind, t.Number, t.GHNumber, t.GHURL, t.Origin, t.HeadSHA, t.GHState)
	return err
}

// TetherItem finds the GitHub side of an Atelier issue or pull request.
func (s *Store) TetherItem(ctx context.Context, repoID, number int64) (TetherItem, error) {
	t, err := scanTetherItem(s.db.QueryRowContext(ctx, `SELECT `+tetherItemCols+` FROM tether_items WHERE repo_id=? AND number=?`, repoID, number))
	return t, notFound(err)
}

// TetherItemByGH finds the Atelier side of a GitHub issue or pull request.
func (s *Store) TetherItemByGH(ctx context.Context, repoID, ghNumber int64) (TetherItem, error) {
	t, err := scanTetherItem(s.db.QueryRowContext(ctx, `SELECT `+tetherItemCols+` FROM tether_items WHERE repo_id=? AND gh_number=?`, repoID, ghNumber))
	return t, notFound(err)
}

// TetherItems lists a repository's synced items of one kind.
func (s *Store) TetherItems(ctx context.Context, repoID int64, kind string) ([]TetherItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tetherItemCols+` FROM tether_items WHERE repo_id=? AND kind=? ORDER BY number`, repoID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TetherItem
	for rows.Next() {
		t, err := scanTetherItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------- comments ----------

// MarkTetherComment records that a GitHub comment and an Atelier comment are
// the same, so neither side copies it again. direction is "in" (from GitHub)
// or "out" (posted to GitHub).
func (s *Store) MarkTetherComment(ctx context.Context, repoID, ghID int64, kind string, atelierID int64, direction string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO tether_comments (repo_id, gh_id, kind, atelier_id, direction)
		VALUES (?, ?, ?, ?, ?)`, repoID, ghID, kind, atelierID, direction)
	return err
}

// TetherCommentKnown reports whether a GitHub comment is already on both sides.
func (s *Store) TetherCommentKnown(ctx context.Context, repoID, ghID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tether_comments WHERE repo_id=? AND gh_id=?`, repoID, ghID).Scan(&n)
	return n > 0, err
}

// TetherCommentOf returns the GitHub id an Atelier comment is linked to (0 if none).
func (s *Store) TetherCommentOf(ctx context.Context, kind string, atelierID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT gh_id FROM tether_comments WHERE kind=? AND atelier_id=?`, kind, atelierID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// ---------- commit statuses reported to GitHub ----------

// TetherStatus is the last state reported for a commit and context.
func (s *Store) TetherStatus(ctx context.Context, repoID int64, sha, context string) (state, desc string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT state, description FROM tether_statuses WHERE repo_id=? AND sha=? AND context=?`,
		repoID, sha, context).Scan(&state, &desc)
	return state, desc, notFound(err)
}

func (s *Store) SaveTetherStatus(ctx context.Context, repoID int64, sha, context, state, desc string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tether_statuses (repo_id, sha, context, state, description, updated_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(repo_id, sha, context) DO UPDATE SET state=excluded.state,
		description=excluded.description, updated_at=excluded.updated_at`, repoID, sha, context, state, desc, now())
	return err
}

// ---------- "I use Atelier" badges ----------

type TetherBadge struct {
	Login     string
	Present   bool
	Studio    string
	ETag      string
	CheckedAt time.Time
}

func (s *Store) TetherBadge(ctx context.Context, login string) (TetherBadge, error) {
	var b TetherBadge
	var present int
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT login, present, studio, etag, checked_at FROM tether_badges WHERE login=?`, login).
		Scan(&b.Login, &present, &b.Studio, &b.ETag, &at)
	b.Present, b.CheckedAt = present != 0, time.Unix(at, 0)
	return b, notFound(err)
}

func (s *Store) SaveTetherBadge(ctx context.Context, b TetherBadge) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO tether_badges (login, present, studio, etag, checked_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(login) DO UPDATE SET present=excluded.present, studio=excluded.studio, etag=excluded.etag,
		checked_at=excluded.checked_at`, b.Login, b.Present, b.Studio, b.ETag, b.CheckedAt.Unix())
	return err
}

// ---------- local search ----------

// LocalItem is an issue or pull request found by a studio search.
type LocalItem struct {
	RepoID    int64
	Kind      string // issue or pull
	Number    int64
	Title     string
	State     string
	UpdatedAt time.Time
}

// SearchLocalItems finds issues and pull requests in repoIDs whose title
// contains every word of q (case-insensitive), newest first.
func (s *Store) SearchLocalItems(ctx context.Context, repoIDs []int64, q string, limit int) ([]LocalItem, error) {
	words := strings.Fields(q)
	if len(repoIDs) == 0 || len(words) == 0 {
		return nil, nil
	}
	if len(words) > 8 {
		words = words[:8]
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(repoIDs)), ",")
	var where []string
	var args []any
	for _, id := range repoIDs {
		args = append(args, id)
	}
	for _, w := range words {
		where = append(where, `title LIKE ? ESCAPE '\'`)
		args = append(args, likeEscape(w))
	}
	cond := `repo_id IN (` + ph + `) AND ` + strings.Join(where, " AND ")
	all := append(append([]any{}, args...), args...)
	all = append(all, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id, 'issue', number, title, state, updated_at FROM issues WHERE `+cond+`
		UNION ALL SELECT repo_id, 'pull', number, title, state, updated_at FROM pulls WHERE `+cond+`
		ORDER BY 6 DESC LIMIT ?`, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LocalItem
	for rows.Next() {
		var it LocalItem
		var u int64
		if err := rows.Scan(&it.RepoID, &it.Kind, &it.Number, &it.Title, &it.State, &u); err != nil {
			return nil, err
		}
		it.UpdatedAt = time.Unix(u, 0)
		out = append(out, it)
	}
	return out, rows.Err()
}
