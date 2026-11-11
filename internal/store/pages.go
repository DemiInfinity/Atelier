package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Pages: static sites published from a repository (internal/web/pages_site*.go).
// pages_sites holds each repository's settings and which deploy is live;
// pages_deploys is the deploy history. The snapshot files themselves live
// under <data>/pages (internal/pagesdeploy).

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS pages_sites (
  repo_id    INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  enabled    INTEGER NOT NULL DEFAULT 0,
  source     TEXT NOT NULL DEFAULT 'branch' CHECK (source IN ('branch','actions')),
  branch     TEXT NOT NULL DEFAULT 'main',
  folder     TEXT NOT NULL DEFAULT '',
  live_id    INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS pages_deploys (
  id          INTEGER PRIMARY KEY,
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  source      TEXT NOT NULL,
  ref         TEXT NOT NULL DEFAULT '',
  sha         TEXT NOT NULL DEFAULT '',
  actor       TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'building' CHECK (status IN ('building','ready','failed')),
  error       TEXT NOT NULL DEFAULT '',
  files       INTEGER NOT NULL DEFAULT 0,
  bytes       INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  finished_at INTEGER
);
CREATE INDEX IF NOT EXISTS pages_deploys_repo ON pages_deploys (repo_id, id);

CREATE TABLE IF NOT EXISTS pages_prefs (
  repo_id  INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  previews INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS pages_domains (
  repo_id     INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  host        TEXT NOT NULL,
  token       TEXT NOT NULL,
  route       TEXT NOT NULL DEFAULT 'proxy',
  verified_at INTEGER,
  method      TEXT NOT NULL DEFAULT '',
  checked_at  INTEGER,
  failures    INTEGER NOT NULL DEFAULT 0,
  last_error  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS pages_domains_host ON pages_domains (host);
CREATE UNIQUE INDEX IF NOT EXISTS pages_domains_verified ON pages_domains (host) WHERE verified_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS pages_previews (
  repo_id      INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  number       INTEGER NOT NULL,
  status       TEXT NOT NULL DEFAULT 'waiting',
  live_id      INTEGER NOT NULL DEFAULT 0,
  sha          TEXT NOT NULL DEFAULT '',
  approved_sha TEXT NOT NULL DEFAULT '',
  comment_id   INTEGER NOT NULL DEFAULT 0,
  error        TEXT NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (repo_id, number)
);

CREATE TABLE IF NOT EXISTS pages_handoffs (
  token_hash TEXT PRIMARY KEY,
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  person_id  INTEGER NOT NULL,
  host       TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  used       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS pages_site_sessions (
  token_hash TEXT PRIMARY KEY,
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  person_id  INTEGER NOT NULL,
  host       TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
`)
}

// Pages sources.
const (
	PagesFromBranch  = "branch"  // a folder on a branch, published on every push
	PagesFromActions = "actions" // uploaded by atelier/deploy-pages in a workflow
	// PagesPreview marks a pull request preview deploy (ref refs/pull/<n>/head).
	// Previews never become the live site and have their own pruning.
	PagesFromPreview = "preview"
)

// PagesSite is a repository's Pages settings.
type PagesSite struct {
	RepoID    int64
	Enabled   bool
	Source    string
	Branch    string
	Folder    string // "" is the repository root
	LiveID    int64  // live deploy, 0 = nothing published
	UpdatedAt time.Time
}

// PagesDeploy is one deploy in the history.
type PagesDeploy struct {
	ID         int64
	RepoID     int64
	Source     string
	Ref, SHA   string
	Actor      string
	Status     string // building, ready, failed
	Error      string
	Files      int
	Bytes      int64
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// PagesSite returns the settings, or defaults (not enabled) when there are none.
func (s *Store) PagesSite(ctx context.Context, repoID int64) (PagesSite, error) {
	ps := PagesSite{RepoID: repoID, Source: PagesFromBranch, Branch: "main"}
	var u int64
	err := s.db.QueryRowContext(ctx, `SELECT enabled, source, branch, folder, live_id, updated_at FROM pages_sites WHERE repo_id=?`, repoID).
		Scan(&ps.Enabled, &ps.Source, &ps.Branch, &ps.Folder, &ps.LiveID, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return ps, nil
	}
	ps.UpdatedAt = time.Unix(u, 0)
	return ps, err
}

// SavePagesSite stores the settings (not the live deploy).
func (s *Store) SavePagesSite(ctx context.Context, ps PagesSite) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pages_sites (repo_id, enabled, source, branch, folder, updated_at) VALUES (?,?,?,?,?,?)
ON CONFLICT(repo_id) DO UPDATE SET enabled=excluded.enabled, source=excluded.source, branch=excluded.branch, folder=excluded.folder, updated_at=excluded.updated_at`,
		ps.RepoID, ps.Enabled, ps.Source, ps.Branch, ps.Folder, now())
	return err
}

// SetPagesLive switches the live deploy. deployID must be a ready deploy of
// the repository (0 takes the site offline).
func (s *Store) SetPagesLive(ctx context.Context, repoID, deployID int64) error {
	if deployID != 0 {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages_deploys WHERE id=? AND repo_id=? AND status='ready' AND source != 'preview'`, deployID, repoID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE pages_sites SET live_id=?, updated_at=? WHERE repo_id=?`, deployID, now(), repoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PagesSiteIDs lists repositories that have Pages settings (for
// cleaning up snapshot directories).
func (s *Store) PagesSiteIDs(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id FROM pages_sites`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// DeletePagesSite removes the settings and the whole deploy history,
// previews included (the custom domain claim stays).
func (s *Store) DeletePagesSite(ctx context.Context, repoID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pages_deploys WHERE repo_id=?`, repoID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pages_previews WHERE repo_id=?`, repoID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM pages_sites WHERE repo_id=?`, repoID)
	return err
}

// CreatePagesDeploy starts a deploy in the building state.
func (s *Store) CreatePagesDeploy(ctx context.Context, d PagesDeploy) (PagesDeploy, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO pages_deploys (repo_id, source, ref, sha, actor, status, created_at) VALUES (?,?,?,?,?,'building',?)`,
		d.RepoID, d.Source, d.Ref, d.SHA, d.Actor, t)
	if err != nil {
		return d, err
	}
	d.ID, _ = res.LastInsertId()
	d.Status, d.CreatedAt = "building", time.Unix(t, 0)
	return d, nil
}

// FinishPagesDeploy records the outcome: ready with its size, or failed with errMsg.
func (s *Store) FinishPagesDeploy(ctx context.Context, id int64, files int, bytes int64, errMsg string) error {
	status := "ready"
	if errMsg != "" {
		status = "failed"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE pages_deploys SET status=?, error=?, files=?, bytes=?, finished_at=? WHERE id=?`,
		status, errMsg, files, bytes, now(), id)
	return err
}

const pagesDeployCols = `id, repo_id, source, ref, sha, actor, status, error, files, bytes, created_at, finished_at`

func scanPagesDeploy(row interface{ Scan(...any) error }) (PagesDeploy, error) {
	var d PagesDeploy
	var c int64
	var f sql.NullInt64
	err := row.Scan(&d.ID, &d.RepoID, &d.Source, &d.Ref, &d.SHA, &d.Actor, &d.Status, &d.Error, &d.Files, &d.Bytes, &c, &f)
	d.CreatedAt = time.Unix(c, 0)
	d.FinishedAt = unixPtr(f)
	return d, err
}

// PagesDeploy finds one deploy of a repository.
func (s *Store) PagesDeploy(ctx context.Context, repoID, id int64) (PagesDeploy, error) {
	d, err := scanPagesDeploy(s.db.QueryRowContext(ctx, `SELECT `+pagesDeployCols+` FROM pages_deploys WHERE id=? AND repo_id=?`, id, repoID))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// PagesDeploys lists a repository's site deploys (not previews), newest first.
func (s *Store) PagesDeploys(ctx context.Context, repoID int64, limit int) ([]PagesDeploy, error) {
	return s.pagesDeployList(ctx, `SELECT `+pagesDeployCols+` FROM pages_deploys WHERE repo_id=? AND source != 'preview' ORDER BY id DESC LIMIT ?`, repoID, limit)
}

// PagesPreviewDeploys lists a repository's preview deploys, newest first.
func (s *Store) PagesPreviewDeploys(ctx context.Context, repoID int64) ([]PagesDeploy, error) {
	return s.pagesDeployList(ctx, `SELECT `+pagesDeployCols+` FROM pages_deploys WHERE repo_id=? AND source = 'preview' ORDER BY id DESC LIMIT ?`, repoID, 100000)
}

func (s *Store) pagesDeployList(ctx context.Context, q string, args ...any) ([]PagesDeploy, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PagesDeploy
	for rows.Next() {
		d, err := scanPagesDeploy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeletePagesDeploy removes one deploy from the history.
func (s *Store) DeletePagesDeploy(ctx context.Context, repoID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pages_deploys WHERE id=? AND repo_id=?`, id, repoID)
	return err
}

// FailStalePagesDeploys marks deploys left building (by a restart) as failed.
func (s *Store) FailStalePagesDeploys(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pages_deploys SET status='failed', error='interrupted by a restart', finished_at=? WHERE status='building'`, now())
	return err
}

// ---------- preferences ----------

// PagesPreviewsOn reports whether pull request previews are on (the default).
func (s *Store) PagesPreviewsOn(ctx context.Context, repoID int64) (bool, error) {
	var on bool
	err := s.db.QueryRowContext(ctx, `SELECT previews FROM pages_prefs WHERE repo_id=?`, repoID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return on, err
}

// SetPagesPreviews turns pull request previews on or off.
func (s *Store) SetPagesPreviews(ctx context.Context, repoID int64, on bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pages_prefs (repo_id, previews) VALUES (?,?)
ON CONFLICT(repo_id) DO UPDATE SET previews=excluded.previews`, repoID, on)
	return err
}

// ---------- custom domains ----------

// PagesDomain is a site's custom host name and its verification state.
type PagesDomain struct {
	RepoID     int64
	Host       string // normalised: lower case, no port, no trailing dot
	Token      string // what the TXT record or the file must contain
	Route      string // how TLS reaches Atelier: "proxy" (Nginx Proxy Manager) or "tunnel"
	VerifiedAt *time.Time
	Method     string // "txt" or "file", how it was last verified
	CheckedAt  *time.Time
	Failures   int // consecutive failed re-checks
	LastError  string
	CreatedAt  time.Time
}

func (d PagesDomain) Verified() bool { return d.VerifiedAt != nil }

const pagesDomainCols = `repo_id, host, token, route, verified_at, method, checked_at, failures, last_error, created_at`

func scanPagesDomain(row interface{ Scan(...any) error }) (PagesDomain, error) {
	var d PagesDomain
	var v, c sql.NullInt64
	var cr int64
	err := row.Scan(&d.RepoID, &d.Host, &d.Token, &d.Route, &v, &d.Method, &c, &d.Failures, &d.LastError, &cr)
	d.VerifiedAt, d.CheckedAt, d.CreatedAt = unixPtr(v), unixPtr(c), time.Unix(cr, 0)
	return d, err
}

// PagesDomain returns a repository's custom domain, or ErrNotFound.
func (s *Store) PagesDomain(ctx context.Context, repoID int64) (PagesDomain, error) {
	d, err := scanPagesDomain(s.db.QueryRowContext(ctx, `SELECT `+pagesDomainCols+` FROM pages_domains WHERE repo_id=?`, repoID))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// PagesDomains lists every custom domain (verified or not).
func (s *Store) PagesDomains(ctx context.Context) ([]PagesDomain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pagesDomainCols+` FROM pages_domains ORDER BY repo_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PagesDomain
	for rows.Next() {
		d, err := scanPagesDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// VerifiedPagesHosts maps every verified host to its repository.
func (s *Store) VerifiedPagesHosts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host, repo_id FROM pages_domains WHERE verified_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var h string
		var id int64
		if err := rows.Scan(&h, &id); err != nil {
			return nil, err
		}
		out[h] = id
	}
	return out, rows.Err()
}

// ErrPagesHostTaken is returned when another site has verified the host.
var ErrPagesHostTaken = errors.New("another site already uses that host name")

// SetPagesDomain claims host for a repository (unverified, with a new
// token), replacing any earlier claim of that repository.
func (s *Store) SetPagesDomain(ctx context.Context, repoID int64, host, token, route string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pages_domains WHERE host=? AND repo_id != ? AND verified_at IS NOT NULL`, host, repoID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrPagesHostTaken
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pages_domains (repo_id, host, token, route, created_at) VALUES (?,?,?,?,?)
ON CONFLICT(repo_id) DO UPDATE SET host=excluded.host, token=excluded.token, route=excluded.route, verified_at=NULL,
  method='', checked_at=NULL, failures=0, last_error='', created_at=excluded.created_at`, repoID, host, token, route, now())
	return err
}

// SetPagesDomainRoute records how TLS reaches Atelier for the domain.
func (s *Store) SetPagesDomainRoute(ctx context.Context, repoID int64, route string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pages_domains SET route=? WHERE repo_id=?`, route, repoID)
	return err
}

// PagesDomainVerified records a successful check. A host verified by
// another site first stays theirs (ErrPagesHostTaken).
func (s *Store) PagesDomainVerified(ctx context.Context, repoID int64, method string) error {
	t := now()
	_, err := s.db.ExecContext(ctx, `UPDATE pages_domains SET verified_at=COALESCE(verified_at, ?), method=?, checked_at=?, failures=0, last_error='' WHERE repo_id=?`,
		t, method, t, repoID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrPagesHostTaken
	}
	return err
}

// PagesDomainFailed records a failed check. After maxFailures consecutive
// failures a verified domain is un-verified; it reports whether that happened.
func (s *Store) PagesDomainFailed(ctx context.Context, repoID int64, msg string, maxFailures int) (bool, error) {
	if len(msg) > 300 {
		msg = msg[:300]
	}
	t := now()
	if _, err := s.db.ExecContext(ctx, `UPDATE pages_domains SET failures=failures+1, last_error=?, checked_at=? WHERE repo_id=?`, msg, t, repoID); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE pages_domains SET verified_at=NULL, method='' WHERE repo_id=? AND verified_at IS NOT NULL AND failures >= ?`, repoID, maxFailures)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeletePagesDomain removes the custom domain and every private-site
// session and handoff for the repository.
func (s *Store) DeletePagesDomain(ctx context.Context, repoID int64) error {
	for _, q := range []string{`DELETE FROM pages_domains WHERE repo_id=?`, `DELETE FROM pages_site_sessions WHERE repo_id=?`, `DELETE FROM pages_handoffs WHERE repo_id=?`} {
		if _, err := s.db.ExecContext(ctx, q, repoID); err != nil {
			return err
		}
	}
	return nil
}

// ---------- private sites: sign-in handoff and site sessions ----------

// CreatePagesHandoff stores a single-use handoff token (by hash).
func (s *Store) CreatePagesHandoff(ctx context.Context, tokenHash string, repoID, personID int64, host string, ttl time.Duration) error {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM pages_handoffs WHERE expires_at < ?`, now())
	_, err := s.db.ExecContext(ctx, `INSERT INTO pages_handoffs (token_hash, repo_id, person_id, host, expires_at) VALUES (?,?,?,?,?)`,
		tokenHash, repoID, personID, host, time.Now().Add(ttl).Unix())
	return err
}

// UsePagesHandoff consumes a handoff token for host. It works once, before
// it expires; the person it was made for is returned.
func (s *Store) UsePagesHandoff(ctx context.Context, tokenHash, host string) (repoID, personID int64, err error) {
	res, err := s.db.ExecContext(ctx, `UPDATE pages_handoffs SET used=1 WHERE token_hash=? AND host=? AND used=0 AND expires_at >= ?`, tokenHash, host, now())
	if err != nil {
		return 0, 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, 0, ErrNotFound
	}
	err = s.db.QueryRowContext(ctx, `SELECT repo_id, person_id FROM pages_handoffs WHERE token_hash=?`, tokenHash).Scan(&repoID, &personID)
	return repoID, personID, err
}

// CreatePagesSiteSession stores a private-site session (by hash).
func (s *Store) CreatePagesSiteSession(ctx context.Context, tokenHash string, repoID, personID int64, host string, ttl time.Duration) error {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM pages_site_sessions WHERE expires_at < ?`, now())
	_, err := s.db.ExecContext(ctx, `INSERT INTO pages_site_sessions (token_hash, repo_id, person_id, host, expires_at, created_at) VALUES (?,?,?,?,?,?)`,
		tokenHash, repoID, personID, host, time.Now().Add(ttl).Unix(), now())
	return err
}

// PagesSiteSession finds a live session for host and repository.
func (s *Store) PagesSiteSession(ctx context.Context, tokenHash, host string, repoID int64) (personID int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT person_id FROM pages_site_sessions WHERE token_hash=? AND host=? AND repo_id=? AND expires_at >= ?`,
		tokenHash, host, repoID, now()).Scan(&personID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return personID, err
}

// DeletePagesSiteSession ends one session.
func (s *Store) DeletePagesSiteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pages_site_sessions WHERE token_hash=?`, tokenHash)
	return err
}

// ---------- pull request previews ----------

// PagesPreview is one pull request's preview.
type PagesPreview struct {
	RepoID      int64
	Number      int64
	Status      string // waiting (for approval), live, failed
	LiveID      int64  // the preview deploy served, 0 = none
	SHA         string // head commit of the last attempt
	ApprovedSHA string // head commit the owner approved (outside pull requests)
	CommentID   int64  // the pull request comment kept up to date
	Error       string
	UpdatedAt   time.Time
}

const pagesPreviewCols = `repo_id, number, status, live_id, sha, approved_sha, comment_id, error, updated_at`

func scanPagesPreview(row interface{ Scan(...any) error }) (PagesPreview, error) {
	var p PagesPreview
	var u int64
	err := row.Scan(&p.RepoID, &p.Number, &p.Status, &p.LiveID, &p.SHA, &p.ApprovedSHA, &p.CommentID, &p.Error, &u)
	p.UpdatedAt = time.Unix(u, 0)
	return p, err
}

// PagesPreview finds a pull request's preview, or ErrNotFound.
func (s *Store) PagesPreview(ctx context.Context, repoID, number int64) (PagesPreview, error) {
	p, err := scanPagesPreview(s.db.QueryRowContext(ctx, `SELECT `+pagesPreviewCols+` FROM pages_previews WHERE repo_id=? AND number=?`, repoID, number))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// PagesPreviews lists a repository's previews, most recently updated first.
func (s *Store) PagesPreviews(ctx context.Context, repoID int64) ([]PagesPreview, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pagesPreviewCols+` FROM pages_previews WHERE repo_id=? ORDER BY updated_at DESC, number DESC`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PagesPreview
	for rows.Next() {
		p, err := scanPagesPreview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SavePagesPreview creates or replaces a preview row.
func (s *Store) SavePagesPreview(ctx context.Context, p PagesPreview) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pages_previews (`+pagesPreviewCols+`) VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(repo_id, number) DO UPDATE SET status=excluded.status, live_id=excluded.live_id, sha=excluded.sha,
  approved_sha=excluded.approved_sha, comment_id=excluded.comment_id, error=excluded.error, updated_at=excluded.updated_at`,
		p.RepoID, p.Number, p.Status, p.LiveID, p.SHA, p.ApprovedSHA, p.CommentID, p.Error, now())
	return err
}

// DeletePagesPreview removes a preview row.
func (s *Store) DeletePagesPreview(ctx context.Context, repoID, number int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pages_previews WHERE repo_id=? AND number=?`, repoID, number)
	return err
}

// SetPullCommentBody rewrites a comment Atelier posted (the preview link
// comment is updated in place on every push).
func (s *Store) SetPullCommentBody(ctx context.Context, pullID, commentID int64, body string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE pull_comments SET body=?, updated_at=? WHERE id=? AND pull_id=?`, body, now(), commentID, pullID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePagesSiteSessionsFor signs everyone out of a repository's private site.
func (s *Store) DeletePagesSiteSessionsFor(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pages_site_sessions WHERE repo_id=?`, repoID)
	return err
}
