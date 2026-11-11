package store

// Security: secret scanning alerts, push protection blocks and one-time
// bypasses, scan status, and dependency update results. Secret values are
// never stored: only a fingerprint (a hash), a redacted form and where the
// secret was seen.

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS security_settings (
  repo_id         INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  push_protection INTEGER NOT NULL DEFAULT 1,
  scan_logs       INTEGER NOT NULL DEFAULT 1,
  security_prs    INTEGER NOT NULL DEFAULT 1,
  all_prs         INTEGER NOT NULL DEFAULT 0,
  updated_at      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS secret_alerts (
  id          INTEGER PRIMARY KEY,
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,              -- code | log
  source      TEXT NOT NULL,              -- push | bypass | history | log
  rule_id     TEXT NOT NULL,
  rule_name   TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  redacted    TEXT NOT NULL,
  path        TEXT NOT NULL DEFAULT '',
  line        INTEGER NOT NULL DEFAULT 0,
  commit_sha  TEXT NOT NULL DEFAULT '',
  run_seq     INTEGER NOT NULL DEFAULT 0,
  job_id      INTEGER NOT NULL DEFAULT 0,
  job_name    TEXT NOT NULL DEFAULT '',
  step_name   TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'open', -- open | resolved | false_positive
  status_by   TEXT NOT NULL DEFAULT '',
  first_seen  INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  UNIQUE (repo_id, kind, fingerprint)
);
CREATE INDEX IF NOT EXISTS secret_alerts_repo ON secret_alerts(repo_id, status);
CREATE TABLE IF NOT EXISTS secret_push_blocks (
  id          INTEGER PRIMARY KEY,
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  fingerprint TEXT NOT NULL,
  rule_name   TEXT NOT NULL,
  redacted    TEXT NOT NULL,
  path        TEXT NOT NULL,
  line        INTEGER NOT NULL,
  commit_sha  TEXT NOT NULL,
  ref         TEXT NOT NULL,
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS secret_push_blocks_repo ON secret_push_blocks(repo_id, created_at);
CREATE TABLE IF NOT EXISTS secret_bypasses (
  id          INTEGER PRIMARY KEY,
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  fingerprint TEXT NOT NULL,
  created_by  TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  used_at     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS security_scans (
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,             -- history | deps
  status      TEXT NOT NULL,             -- running | done | failed
  started_at  INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0,
  commits     INTEGER NOT NULL DEFAULT 0,
  findings    INTEGER NOT NULL DEFAULT 0,
  truncated   INTEGER NOT NULL DEFAULT 0,
  error       TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (repo_id, kind)
);
CREATE TABLE IF NOT EXISTS dep_updates (
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  ecosystem  TEXT NOT NULL,              -- go | npm
  name       TEXT NOT NULL,
  manifest   TEXT NOT NULL,
  current    TEXT NOT NULL,              -- version or range in the manifest
  latest     TEXT NOT NULL DEFAULT '',   -- '' when there's no update (vulnerable only)
  new_spec   TEXT NOT NULL DEFAULT '',
  kind       TEXT NOT NULL DEFAULT '',   -- patch | minor | major
  vulns      INTEGER NOT NULL DEFAULT 0,
  vuln_ids   TEXT NOT NULL DEFAULT '',
  fixes      INTEGER NOT NULL DEFAULT 0, -- advisories the update fixes
  pr_number  INTEGER NOT NULL DEFAULT 0,
  checked_at INTEGER NOT NULL,
  PRIMARY KEY (repo_id, ecosystem, name)
);`)
}

// ---------- settings ----------

// SecuritySettings are a repository's security switches.
type SecuritySettings struct {
	PushProtection bool // block pushes that contain secrets
	ScanLogs       bool // scan Actions logs after each run
	SecurityPRs    bool // open PRs for updates that fix a known vulnerability
	AllPRs         bool // open PRs for every update
}

// DefaultSecuritySettings apply until a repository saves its own.
var DefaultSecuritySettings = SecuritySettings{PushProtection: true, ScanLogs: true, SecurityPRs: true}

func (s *Store) SecuritySettings(ctx context.Context, repoID int64) (SecuritySettings, error) {
	var st SecuritySettings
	err := s.db.QueryRowContext(ctx, `SELECT push_protection, scan_logs, security_prs, all_prs FROM security_settings WHERE repo_id=?`, repoID).
		Scan(&st.PushProtection, &st.ScanLogs, &st.SecurityPRs, &st.AllPRs)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultSecuritySettings, nil
	}
	return st, err
}

func (s *Store) SetSecuritySettings(ctx context.Context, repoID int64, st SecuritySettings) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO security_settings (repo_id, push_protection, scan_logs, security_prs, all_prs, updated_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(repo_id) DO UPDATE SET push_protection=excluded.push_protection,
		scan_logs=excluded.scan_logs, security_prs=excluded.security_prs, all_prs=excluded.all_prs, updated_at=excluded.updated_at`,
		repoID, st.PushProtection, st.ScanLogs, st.SecurityPRs, st.AllPRs, now())
	return err
}

// ---------- alerts ----------

// SecretAlert is a detected secret.
type SecretAlert struct {
	ID                  int64
	RepoID              int64
	Kind                string // "code" or "log"
	Source              string // push, bypass, history, log
	RuleID, RuleName    string
	Fingerprint         string
	Redacted            string
	Path                string
	Line                int
	CommitSHA           string
	RunSeq, JobID       int64
	JobName, StepName   string
	Status              string // open, resolved, false_positive
	StatusBy            string
	FirstSeen, LastSeen time.Time
}

const (
	AlertOpen          = "open"
	AlertResolved      = "resolved"
	AlertFalsePositive = "false_positive"
)

const secretAlertCols = `id, repo_id, kind, source, rule_id, rule_name, fingerprint, redacted, path, line, commit_sha, run_seq, job_id,
job_name, step_name, status, status_by, first_seen, last_seen`

func scanSecretAlert(row interface{ Scan(...any) error }) (SecretAlert, error) {
	var a SecretAlert
	var f, l int64
	err := row.Scan(&a.ID, &a.RepoID, &a.Kind, &a.Source, &a.RuleID, &a.RuleName, &a.Fingerprint, &a.Redacted, &a.Path, &a.Line,
		&a.CommitSHA, &a.RunSeq, &a.JobID, &a.JobName, &a.StepName, &a.Status, &a.StatusBy, &f, &l)
	a.FirstSeen, a.LastSeen = time.Unix(f, 0), time.Unix(l, 0)
	return a, err
}

// RecordSecretAlert stores an alert, or refreshes last_seen on the existing
// one for the same secret. created is true for a new alert. A resolved alert
// seen again reopens; a false positive stays dismissed.
func (s *Store) RecordSecretAlert(ctx context.Context, a SecretAlert) (SecretAlert, bool, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO secret_alerts (repo_id, kind, source, rule_id, rule_name, fingerprint, redacted, path, line,
		commit_sha, run_seq, job_id, job_name, step_name, status, first_seen, last_seen) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,'open',?,?)
		ON CONFLICT(repo_id, kind, fingerprint) DO NOTHING`,
		a.RepoID, a.Kind, a.Source, a.RuleID, a.RuleName, a.Fingerprint, a.Redacted, a.Path, a.Line, a.CommitSHA, a.RunSeq, a.JobID,
		a.JobName, a.StepName, t, t)
	if err != nil {
		return SecretAlert{}, false, err
	}
	created := false
	if n, _ := res.RowsAffected(); n > 0 {
		created = true
	} else if _, err := s.db.ExecContext(ctx, `UPDATE secret_alerts SET last_seen=?,
		status=CASE WHEN status='resolved' THEN 'open' ELSE status END WHERE repo_id=? AND kind=? AND fingerprint=?`,
		t, a.RepoID, a.Kind, a.Fingerprint); err != nil {
		return SecretAlert{}, false, err
	}
	got, err := scanSecretAlert(s.db.QueryRowContext(ctx, `SELECT `+secretAlertCols+` FROM secret_alerts WHERE repo_id=? AND kind=? AND fingerprint=?`,
		a.RepoID, a.Kind, a.Fingerprint))
	return got, created, err
}

// SecretAlerts lists a repository's alerts, open ones first, newest first.
func (s *Store) SecretAlerts(ctx context.Context, repoID int64, limit int) ([]SecretAlert, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+secretAlertCols+` FROM secret_alerts WHERE repo_id=?
		ORDER BY status='open' DESC, last_seen DESC, id DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretAlert
	for rows.Next() {
		a, err := scanSecretAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) SecretAlert(ctx context.Context, repoID, id int64) (SecretAlert, error) {
	a, err := scanSecretAlert(s.db.QueryRowContext(ctx, `SELECT `+secretAlertCols+` FROM secret_alerts WHERE repo_id=? AND id=?`, repoID, id))
	return a, notFound(err)
}

// OpenSecretAlertCount counts open alerts.
func (s *Store) OpenSecretAlertCount(ctx context.Context, repoID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secret_alerts WHERE repo_id=? AND status='open'`, repoID).Scan(&n)
	return n, err
}

func (s *Store) SetSecretAlertStatus(ctx context.Context, repoID, id int64, status, by string) error {
	switch status {
	case AlertOpen, AlertResolved, AlertFalsePositive:
	default:
		return errors.New("unknown alert status")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE secret_alerts SET status=?, status_by=? WHERE repo_id=? AND id=?`, status, by, repoID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- push protection ----------

// SecretPushBlock is a secret that stopped a push.
type SecretPushBlock struct {
	ID          int64
	RepoID      int64
	Fingerprint string
	RuleName    string
	Redacted    string
	Path        string
	Line        int
	CommitSHA   string
	Ref         string
	CreatedAt   time.Time
}

// RecordSecretPushBlock remembers a blocked secret (the last 50 per repository are kept).
func (s *Store) RecordSecretPushBlock(ctx context.Context, b SecretPushBlock) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO secret_push_blocks (repo_id, fingerprint, rule_name, redacted, path, line, commit_sha, ref, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, b.RepoID, b.Fingerprint, b.RuleName, b.Redacted, b.Path, b.Line, b.CommitSHA, b.Ref, now()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM secret_push_blocks WHERE repo_id=? AND id NOT IN
		(SELECT id FROM secret_push_blocks WHERE repo_id=? ORDER BY id DESC LIMIT 50)`, b.RepoID, b.RepoID)
	return err
}

// SecretPushBlocks lists recent blocks, newest first, one per fingerprint.
func (s *Store) SecretPushBlocks(ctx context.Context, repoID int64, limit int) ([]SecretPushBlock, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo_id, fingerprint, rule_name, redacted, path, line, commit_sha, ref, created_at
		FROM secret_push_blocks WHERE id IN (SELECT MAX(id) FROM secret_push_blocks WHERE repo_id=? GROUP BY fingerprint)
		ORDER BY id DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretPushBlock
	for rows.Next() {
		var b SecretPushBlock
		var c int64
		if err := rows.Scan(&b.ID, &b.RepoID, &b.Fingerprint, &b.RuleName, &b.Redacted, &b.Path, &b.Line, &b.CommitSHA, &b.Ref, &c); err != nil {
			return nil, err
		}
		b.CreatedAt = time.Unix(c, 0)
		out = append(out, b)
	}
	return out, rows.Err()
}

// SecretPushBlockByID returns one block.
func (s *Store) SecretPushBlockByID(ctx context.Context, repoID, id int64) (SecretPushBlock, error) {
	var b SecretPushBlock
	var c int64
	err := s.db.QueryRowContext(ctx, `SELECT id, repo_id, fingerprint, rule_name, redacted, path, line, commit_sha, ref, created_at
		FROM secret_push_blocks WHERE repo_id=? AND id=?`, repoID, id).
		Scan(&b.ID, &b.RepoID, &b.Fingerprint, &b.RuleName, &b.Redacted, &b.Path, &b.Line, &b.CommitSHA, &b.Ref, &c)
	b.CreatedAt = time.Unix(c, 0)
	return b, notFound(err)
}

// CreateSecretBypass allows one push containing the secret with this
// fingerprint, within ttl.
func (s *Store) CreateSecretBypass(ctx context.Context, repoID int64, fingerprint, by string, ttl time.Duration) error {
	t := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO secret_bypasses (repo_id, fingerprint, created_by, created_at, expires_at) VALUES (?,?,?,?,?)`,
		repoID, fingerprint, by, t, t+int64(ttl.Seconds()))
	return err
}

// HasSecretBypass reports whether an unused, unexpired bypass exists.
func (s *Store) HasSecretBypass(ctx context.Context, repoID int64, fingerprint string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM secret_bypasses WHERE repo_id=? AND fingerprint=? AND used_at=0 AND expires_at>?`,
		repoID, fingerprint, now()).Scan(&n)
	return n > 0, err
}

// UseSecretBypass consumes one bypass. ok is false if there was none.
func (s *Store) UseSecretBypass(ctx context.Context, repoID int64, fingerprint string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE secret_bypasses SET used_at=? WHERE id=(SELECT id FROM secret_bypasses
		WHERE repo_id=? AND fingerprint=? AND used_at=0 AND expires_at>? ORDER BY id LIMIT 1)`, now(), repoID, fingerprint, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PendingSecretBypasses lists the fingerprints with an unused bypass.
func (s *Store) PendingSecretBypasses(ctx context.Context, repoID int64) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT fingerprint FROM secret_bypasses WHERE repo_id=? AND used_at=0 AND expires_at>?`, repoID, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, err
		}
		out[fp] = true
	}
	return out, rows.Err()
}

// ---------- scans ----------

// SecurityScan is the state of a background scan.
type SecurityScan struct {
	RepoID     int64
	Kind       string
	Status     string // running, done, failed
	StartedAt  time.Time
	FinishedAt time.Time
	Commits    int
	Findings   int
	Truncated  bool
	Error      string
}

// StartSecurityScan marks a scan as running. It returns false if one is
// already running (and started less than staleAfter ago).
func (s *Store) StartSecurityScan(ctx context.Context, repoID int64, kind string, staleAfter time.Duration) (bool, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO security_scans (repo_id, kind, status, started_at) VALUES (?, ?, 'running', ?)
		ON CONFLICT(repo_id, kind) DO UPDATE SET status='running', started_at=excluded.started_at, finished_at=0, commits=0,
		findings=0, truncated=0, error='' WHERE security_scans.status!='running' OR security_scans.started_at<?`,
		repoID, kind, t, t-int64(staleAfter.Seconds()))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) FinishSecurityScan(ctx context.Context, sc SecurityScan) error {
	status := "done"
	if sc.Error != "" {
		status = "failed"
	}
	_, err := s.db.ExecContext(ctx, `UPDATE security_scans SET status=?, finished_at=?, commits=?, findings=?, truncated=?, error=?
		WHERE repo_id=? AND kind=?`, status, now(), sc.Commits, sc.Findings, sc.Truncated, sc.Error, sc.RepoID, sc.Kind)
	return err
}

// SecurityScanState returns the last scan of a kind (ErrNotFound if none).
func (s *Store) SecurityScanState(ctx context.Context, repoID int64, kind string) (SecurityScan, error) {
	sc := SecurityScan{RepoID: repoID, Kind: kind}
	var st, fin int64
	err := s.db.QueryRowContext(ctx, `SELECT status, started_at, finished_at, commits, findings, truncated, error FROM security_scans
		WHERE repo_id=? AND kind=?`, repoID, kind).Scan(&sc.Status, &st, &fin, &sc.Commits, &sc.Findings, &sc.Truncated, &sc.Error)
	sc.StartedAt = time.Unix(st, 0)
	if fin > 0 {
		sc.FinishedAt = time.Unix(fin, 0)
	}
	return sc, notFound(err)
}

// ---------- dependency updates ----------

// DepUpdate is a dependency with an available update and/or known vulnerabilities.
type DepUpdate struct {
	RepoID    int64
	Ecosystem string // go, npm, maven, gradle
	Name      string
	Manifest  string // path in the repository (results live in dep_update_rows, see depupdates.go)
	Current   string
	Latest    string
	NewSpec   string
	Kind      string // patch, minor, major
	Vulns     int
	VulnIDs   string // comma-separated advisory IDs
	Fixes     int
	PRNumber  int64
	CheckedAt time.Time
}

// ReplaceDepUpdates replaces a repository's results with a fresh check.
func (s *Store) ReplaceDepUpdates(ctx context.Context, repoID int64, ups []DepUpdate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM dep_update_rows WHERE repo_id=?`, repoID); err != nil {
		return err
	}
	t := now()
	for _, u := range ups {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO dep_update_rows (repo_id, ecosystem, name, manifest, current, latest, new_spec, kind,
			vulns, vuln_ids, fixes, pr_number, checked_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			repoID, u.Ecosystem, u.Name, u.Manifest, u.Current, u.Latest, u.NewSpec, u.Kind, u.Vulns, u.VulnIDs, u.Fixes, u.PRNumber, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DepUpdates(ctx context.Context, repoID int64) ([]DepUpdate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id, ecosystem, name, manifest, current, latest, new_spec, kind, vulns, vuln_ids, fixes,
		pr_number, checked_at FROM dep_update_rows WHERE repo_id=? ORDER BY vulns > 0 DESC, ecosystem, manifest, name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DepUpdate
	for rows.Next() {
		var u DepUpdate
		var c int64
		if err := rows.Scan(&u.RepoID, &u.Ecosystem, &u.Name, &u.Manifest, &u.Current, &u.Latest, &u.NewSpec, &u.Kind, &u.Vulns, &u.VulnIDs,
			&u.Fixes, &u.PRNumber, &c); err != nil {
			return nil, err
		}
		u.CheckedAt = time.Unix(c, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

// DepCounts returns the number of available updates and of known vulnerabilities.
func (s *Store) DepCounts(ctx context.Context, repoID int64) (updates, vulns int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(CASE WHEN latest!='' THEN 1 END), COALESCE(SUM(vulns), 0) FROM dep_update_rows WHERE repo_id=?`,
		repoID).Scan(&updates, &vulns)
	return
}

// PullsByAuthorPrefix lists pull requests by an author whose head branch
// starts with prefix (bots use it to find the PRs they opened).
func (s *Store) PullsByAuthorPrefix(ctx context.Context, repoID int64, authorKind, authorName, headPrefix string) ([]Pull, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pullCols+` FROM pulls WHERE repo_id=? AND author_kind=? AND author_name=?
		AND substr(head, 1, ?)=? ORDER BY number DESC`, repoID, authorKind, authorName, len(headPrefix), headPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pull
	for rows.Next() {
		p, err := scanPull(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RewriteActionsLogChunk replaces one stored log chunk (used to mask secrets
// the runner missed). Only finished jobs' logs should be rewritten.
func (s *Store) RewriteActionsLogChunk(ctx context.Context, chunkID int64, lines string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions_logs SET lines=? WHERE id=?`, lines, chunkID)
	return err
}
