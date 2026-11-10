package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

// Pull requests, their conversation, notes to self, timeline events, and the
// branch rules that protect branches. Numbers come from NextNumber, so issues
// and pull requests share one #sequence per repository.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS pulls (
  id            INTEGER PRIMARY KEY,
  repo_id       INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  number        INTEGER NOT NULL,
  title         TEXT NOT NULL,
  body          TEXT NOT NULL DEFAULT '',
  head          TEXT NOT NULL,
  base          TEXT NOT NULL,
  head_sha      TEXT NOT NULL DEFAULT '',
  base_sha      TEXT NOT NULL DEFAULT '',
  state         TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','closed','merged')),
  merge_method  TEXT NOT NULL DEFAULT '',
  merged_sha    TEXT NOT NULL DEFAULT '',
  merged_at     INTEGER,
  merged_by     TEXT NOT NULL DEFAULT '',
  closed_at     INTEGER,
  author_kind   TEXT NOT NULL DEFAULT 'owner',
  author_name   TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  UNIQUE (repo_id, number)
);
CREATE INDEX IF NOT EXISTS pulls_repo_state ON pulls (repo_id, state, number);

CREATE TABLE IF NOT EXISTS pull_comments (
  id          INTEGER PRIMARY KEY,
  pull_id     INTEGER NOT NULL REFERENCES pulls(id) ON DELETE CASCADE,
  author_kind TEXT NOT NULL DEFAULT 'owner',
  author_name TEXT NOT NULL DEFAULT '',
  body        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS pull_comments_pull ON pull_comments (pull_id, id);

CREATE TABLE IF NOT EXISTS pull_notes (
  id               INTEGER PRIMARY KEY,
  pull_id          INTEGER NOT NULL REFERENCES pulls(id) ON DELETE CASCADE,
  commit_sha       TEXT NOT NULL,
  path             TEXT NOT NULL,
  line             INTEGER NOT NULL,
  side             TEXT NOT NULL CHECK (side IN ('old','new')),
  body             TEXT NOT NULL,
  resolved         INTEGER NOT NULL DEFAULT 0,
  resolved_by      TEXT NOT NULL DEFAULT '',
  author_kind      TEXT NOT NULL DEFAULT 'owner',
  author_name      TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS pull_notes_pull ON pull_notes (pull_id, id);

CREATE TABLE IF NOT EXISTS pull_events (
  id          INTEGER PRIMARY KEY,
  pull_id     INTEGER NOT NULL REFERENCES pulls(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,
  actor_kind  TEXT NOT NULL DEFAULT 'owner',
  actor_name  TEXT NOT NULL DEFAULT '',
  data        TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS pull_events_pull ON pull_events (pull_id, id);

CREATE TABLE IF NOT EXISTS branch_rules (
  id                     INTEGER PRIMARY KEY,
  repo_id                INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  pattern                TEXT NOT NULL,
  require_pr             INTEGER NOT NULL DEFAULT 1,
  block_force_push       INTEGER NOT NULL DEFAULT 1,
  require_resolved_notes INTEGER NOT NULL DEFAULT 0,
  linear_history         INTEGER NOT NULL DEFAULT 0,
  created_at             INTEGER NOT NULL,
  updated_at             INTEGER NOT NULL,
  UNIQUE (repo_id, pattern)
);`)
}

// ---------- pull requests ----------

type Pull struct {
	ID          int64
	RepoID      int64
	Number      int64
	Title       string
	Body        string
	Head        string // branch with the changes
	Base        string // branch to merge into
	HeadSHA     string // last head commit Atelier saw (kept after the branch is deleted)
	BaseSHA     string // base tip just before the merge
	State       string // open, closed, merged
	MergeMethod string // merge, squash
	MergedSHA   string
	MergedAt    *time.Time
	MergedBy    string
	ClosedAt    *time.Time
	AuthorKind  string
	AuthorName  string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (p Pull) IsOpen() bool   { return p.State == "open" }
func (p Pull) IsMerged() bool { return p.State == "merged" }
func (p Pull) IsClosed() bool { return p.State == "closed" }

const pullCols = `id, repo_id, number, title, body, head, base, head_sha, base_sha, state, merge_method,
merged_sha, merged_at, merged_by, closed_at, author_kind, author_name, created_at, updated_at`

func scanPull(row interface{ Scan(...any) error }) (Pull, error) {
	var p Pull
	var mergedAt, closedAt sql.NullInt64
	var c, u int64
	err := row.Scan(&p.ID, &p.RepoID, &p.Number, &p.Title, &p.Body, &p.Head, &p.Base, &p.HeadSHA, &p.BaseSHA,
		&p.State, &p.MergeMethod, &p.MergedSHA, &mergedAt, &p.MergedBy, &closedAt, &p.AuthorKind, &p.AuthorName, &c, &u)
	if err != nil {
		return p, err
	}
	p.MergedAt, p.ClosedAt = pullOptTime(mergedAt), pullOptTime(closedAt)
	p.CreatedAt, p.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return p, nil
}

func pullOptTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

// NewPull is what CreatePull needs.
type NewPull struct {
	RepoID                 int64
	Title, Body            string
	Head, Base, HeadSHA    string
	AuthorKind, AuthorName string
}

func (s *Store) CreatePull(ctx context.Context, in NewPull) (Pull, error) {
	n, err := s.NextNumber(ctx, in.RepoID)
	if err != nil {
		return Pull{}, err
	}
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO pulls (repo_id, number, title, body, head, base, head_sha,
		author_kind, author_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.RepoID, n, in.Title, in.Body, in.Head, in.Base, in.HeadSHA, in.AuthorKind, in.AuthorName, t, t)
	if err != nil {
		return Pull{}, err
	}
	id, _ := res.LastInsertId()
	return scanPull(s.db.QueryRowContext(ctx, `SELECT `+pullCols+` FROM pulls WHERE id=?`, id))
}

func (s *Store) PullByNumber(ctx context.Context, repoID, number int64) (Pull, error) {
	p, err := scanPull(s.db.QueryRowContext(ctx, `SELECT `+pullCols+` FROM pulls WHERE repo_id=? AND number=?`, repoID, number))
	return p, notFound(err)
}

// OpenPullFor finds an open pull request for the same head and base branches.
func (s *Store) OpenPullFor(ctx context.Context, repoID int64, base, head string) (Pull, error) {
	p, err := scanPull(s.db.QueryRowContext(ctx, `SELECT `+pullCols+` FROM pulls
		WHERE repo_id=? AND base=? AND head=? AND state='open' ORDER BY number DESC LIMIT 1`, repoID, base, head))
	return p, notFound(err)
}

// Pulls lists a repository's pull requests in one state ("" for all), newest first.
func (s *Store) Pulls(ctx context.Context, repoID int64, state string) ([]Pull, error) {
	q := `SELECT ` + pullCols + ` FROM pulls WHERE repo_id=?`
	args := []any{repoID}
	if state != "" {
		q += ` AND state=?`
		args = append(args, state)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY number DESC`, args...)
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

// PullCounts returns how many pull requests are in each state.
func (s *Store) PullCounts(ctx context.Context, repoID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM pulls WHERE repo_id=? GROUP BY state`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{"open": 0, "merged": 0, "closed": 0}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

func (s *Store) UpdatePullText(ctx context.Context, id int64, title, body string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pulls SET title=?, body=?, updated_at=? WHERE id=?`, title, body, now(), id)
	return err
}

// SetPullHeadSHA records the head commit Atelier last saw for an open pull request.
func (s *Store) SetPullHeadSHA(ctx context.Context, id int64, sha string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pulls SET head_sha=? WHERE id=?`, sha, id)
	return err
}

// ClosePull and ReopenPull only change open/closed pull requests; merged ones stay merged.
func (s *Store) ClosePull(ctx context.Context, id int64) error {
	t := now()
	return pullExpectOne(s.db.ExecContext(ctx, `UPDATE pulls SET state='closed', closed_at=?, updated_at=? WHERE id=? AND state='open'`, t, t, id))
}

func (s *Store) ReopenPull(ctx context.Context, id int64) error {
	return pullExpectOne(s.db.ExecContext(ctx, `UPDATE pulls SET state='open', closed_at=NULL, updated_at=? WHERE id=? AND state='closed'`, now(), id))
}

// MarkPullMerged records a merge done by Atelier.
func (s *Store) MarkPullMerged(ctx context.Context, id int64, method, mergedSHA, baseSHA, headSHA, by string) error {
	t := now()
	return pullExpectOne(s.db.ExecContext(ctx, `UPDATE pulls SET state='merged', merge_method=?, merged_sha=?, base_sha=?,
		head_sha=?, merged_at=?, merged_by=?, updated_at=? WHERE id=? AND state='open'`,
		method, mergedSHA, baseSHA, headSHA, t, by, t, id))
}

// ErrPullStateChanged means the row wasn't in the expected state (e.g. already merged).
var ErrPullStateChanged = errors.New("changed by someone else")

func pullExpectOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPullStateChanged
	}
	return nil
}

// ---------- conversation ----------

type PullComment struct {
	ID         int64
	PullID     int64
	AuthorKind string
	AuthorName string
	Body       string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (s *Store) AddPullComment(ctx context.Context, pullID int64, kind, name, body string) (PullComment, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO pull_comments (pull_id, author_kind, author_name, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, pullID, kind, name, body, t, t)
	if err != nil {
		return PullComment{}, err
	}
	id, _ := res.LastInsertId()
	_, _ = s.db.ExecContext(ctx, `UPDATE pulls SET updated_at=? WHERE id=?`, t, pullID)
	return PullComment{ID: id, PullID: pullID, AuthorKind: kind, AuthorName: name, Body: body,
		CreatedAt: time.Unix(t, 0), UpdatedAt: time.Unix(t, 0)}, nil
}

func (s *Store) PullComments(ctx context.Context, pullID int64) ([]PullComment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, pull_id, author_kind, author_name, body, created_at, updated_at
		FROM pull_comments WHERE pull_id=? ORDER BY id`, pullID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PullComment
	for rows.Next() {
		var c PullComment
		var cr, up int64
		if err := rows.Scan(&c.ID, &c.PullID, &c.AuthorKind, &c.AuthorName, &c.Body, &cr, &up); err != nil {
			return nil, err
		}
		c.CreatedAt, c.UpdatedAt = time.Unix(cr, 0), time.Unix(up, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- notes to self ----------

// PullNote is an inline note on one line of one file, as of a specific commit.
type PullNote struct {
	ID         int64
	PullID     int64
	CommitSHA  string
	Path       string
	Line       int
	Side       string // "old" (left, removed lines) or "new" (right)
	Body       string
	Resolved   bool
	ResolvedBy string
	AuthorKind string
	AuthorName string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type NewPullNote struct {
	PullID                 int64
	CommitSHA, Path, Side  string
	Line                   int
	Body                   string
	AuthorKind, AuthorName string
}

func (s *Store) AddPullNote(ctx context.Context, in NewPullNote) (PullNote, error) {
	if in.Side != "old" && in.Side != "new" {
		return PullNote{}, errors.New("side must be old or new")
	}
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO pull_notes (pull_id, commit_sha, path, line, side, body,
		author_kind, author_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.PullID, in.CommitSHA, in.Path, in.Line, in.Side, in.Body, in.AuthorKind, in.AuthorName, t, t)
	if err != nil {
		return PullNote{}, err
	}
	id, _ := res.LastInsertId()
	_, _ = s.db.ExecContext(ctx, `UPDATE pulls SET updated_at=? WHERE id=?`, t, in.PullID)
	return PullNote{ID: id, PullID: in.PullID, CommitSHA: in.CommitSHA, Path: in.Path, Line: in.Line, Side: in.Side,
		Body: in.Body, AuthorKind: in.AuthorKind, AuthorName: in.AuthorName, CreatedAt: time.Unix(t, 0), UpdatedAt: time.Unix(t, 0)}, nil
}

func (s *Store) PullNotes(ctx context.Context, pullID int64) ([]PullNote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, pull_id, commit_sha, path, line, side, body, resolved, resolved_by,
		author_kind, author_name, created_at, updated_at FROM pull_notes WHERE pull_id=? ORDER BY path, line, id`, pullID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PullNote
	for rows.Next() {
		var n PullNote
		var res int
		var cr, up int64
		if err := rows.Scan(&n.ID, &n.PullID, &n.CommitSHA, &n.Path, &n.Line, &n.Side, &n.Body, &res, &n.ResolvedBy,
			&n.AuthorKind, &n.AuthorName, &cr, &up); err != nil {
			return nil, err
		}
		n.Resolved = res != 0
		n.CreatedAt, n.UpdatedAt = time.Unix(cr, 0), time.Unix(up, 0)
		out = append(out, n)
	}
	return out, rows.Err()
}

// SetPullNoteResolved marks a note (which must belong to pullID) resolved or not.
func (s *Store) SetPullNoteResolved(ctx context.Context, pullID, noteID int64, resolved bool, by string) error {
	if !resolved {
		by = ""
	}
	res, err := s.db.ExecContext(ctx, `UPDATE pull_notes SET resolved=?, resolved_by=?, updated_at=? WHERE id=? AND pull_id=?`,
		resolved, by, now(), noteID, pullID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UnresolvedPullNotes(ctx context.Context, pullID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pull_notes WHERE pull_id=? AND resolved=0`, pullID).Scan(&n)
	return n, err
}

// ---------- timeline events ----------

type PullEvent struct {
	ID        int64
	PullID    int64
	Kind      string // closed, reopened, merged, renamed, edited, branch_deleted
	ActorKind string
	ActorName string
	Data      string
	CreatedAt time.Time
}

func (s *Store) AddPullEvent(ctx context.Context, pullID int64, kind, actorKind, actorName, data string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pull_events (pull_id, kind, actor_kind, actor_name, data, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, pullID, kind, actorKind, actorName, data, now())
	return err
}

func (s *Store) PullEvents(ctx context.Context, pullID int64) ([]PullEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, pull_id, kind, actor_kind, actor_name, data, created_at
		FROM pull_events WHERE pull_id=? ORDER BY id`, pullID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PullEvent
	for rows.Next() {
		var e PullEvent
		var c int64
		if err := rows.Scan(&e.ID, &e.PullID, &e.Kind, &e.ActorKind, &e.ActorName, &e.Data, &c); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(c, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- branch rules ----------

// BranchRule protects every branch whose name matches Pattern. In patterns
// "*" matches any run of characters except "/", and "**" also matches "/".
type BranchRule struct {
	ID                   int64
	RepoID               int64
	Pattern              string
	RequirePR            bool // no direct pushes: changes land through a merged pull request
	BlockForcePush       bool // no force pushes or deletions
	RequireResolvedNotes bool // every note to self must be resolved before merging
	LinearHistory        bool // no merge commits: squash merges only
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

const branchRuleCols = `id, repo_id, pattern, require_pr, block_force_push, require_resolved_notes, linear_history, created_at, updated_at`

func scanBranchRule(row interface{ Scan(...any) error }) (BranchRule, error) {
	var r BranchRule
	var c, u int64
	err := row.Scan(&r.ID, &r.RepoID, &r.Pattern, &r.RequirePR, &r.BlockForcePush, &r.RequireResolvedNotes, &r.LinearHistory, &c, &u)
	r.CreatedAt, r.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return r, err
}

var branchPatternRe = regexp.MustCompile(`^[A-Za-z0-9._/*-]{1,200}$`)

// ValidBranchPattern reports whether p is a usable branch rule pattern.
func ValidBranchPattern(p string) bool {
	return branchPatternRe.MatchString(p) && !strings.Contains(p, "..") && !strings.HasPrefix(p, "/") &&
		!strings.HasSuffix(p, "/") && !strings.Contains(p, "//") && !strings.Contains(p, "***")
}

// MatchBranch reports whether branch matches a rule pattern.
func MatchBranch(pattern, branch string) bool {
	return branchGlobRe(pattern).MatchString(branch)
}

func branchGlobRe(p string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch {
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i++
		case p[i] == '*':
			b.WriteString("[^/]*")
		default:
			b.WriteString(regexp.QuoteMeta(p[i : i+1]))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func (s *Store) BranchRules(ctx context.Context, repoID int64) ([]BranchRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+branchRuleCols+` FROM branch_rules WHERE repo_id=? ORDER BY pattern`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BranchRule
	for rows.Next() {
		r, err := scanBranchRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) BranchRuleByID(ctx context.Context, repoID, id int64) (BranchRule, error) {
	r, err := scanBranchRule(s.db.QueryRowContext(ctx, `SELECT `+branchRuleCols+` FROM branch_rules WHERE repo_id=? AND id=?`, repoID, id))
	return r, notFound(err)
}

// ErrBranchRuleExists means a branch rule with that pattern already exists.
var ErrBranchRuleExists = errors.New("already exists")

func (s *Store) CreateBranchRule(ctx context.Context, r BranchRule) (BranchRule, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO branch_rules (repo_id, pattern, require_pr, block_force_push,
		require_resolved_notes, linear_history, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RepoID, r.Pattern, r.RequirePR, r.BlockForcePush, r.RequireResolvedNotes, r.LinearHistory, t, t)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return BranchRule{}, ErrBranchRuleExists
		}
		return BranchRule{}, err
	}
	id, _ := res.LastInsertId()
	return s.BranchRuleByID(ctx, r.RepoID, id)
}

func (s *Store) UpdateBranchRule(ctx context.Context, r BranchRule) error {
	res, err := s.db.ExecContext(ctx, `UPDATE branch_rules SET pattern=?, require_pr=?, block_force_push=?,
		require_resolved_notes=?, linear_history=?, updated_at=? WHERE id=? AND repo_id=?`,
		r.Pattern, r.RequirePR, r.BlockForcePush, r.RequireResolvedNotes, r.LinearHistory, now(), r.ID, r.RepoID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrBranchRuleExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteBranchRule(ctx context.Context, repoID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM branch_rules WHERE id=? AND repo_id=?`, id, repoID)
	return err
}

// Protection is the combined effect of every rule matching one branch.
type Protection struct {
	Rules                []BranchRule // the matching rules
	RequirePR            bool
	BlockForcePush       bool
	RequireResolvedNotes bool
	LinearHistory        bool
}

func (p Protection) Protected() bool { return len(p.Rules) > 0 }

// Patterns lists the matching rule patterns, for messages.
func (p Protection) Patterns() string {
	var ps []string
	for _, r := range p.Rules {
		ps = append(ps, `"`+r.Pattern+`"`)
	}
	return strings.Join(ps, ", ")
}

// ProtectionFor combines the rules that match branch.
func (s *Store) ProtectionFor(ctx context.Context, repoID int64, branch string) (Protection, error) {
	rules, err := s.BranchRules(ctx, repoID)
	if err != nil {
		return Protection{}, err
	}
	return ProtectionFrom(rules, branch), nil
}

// ProtectionFrom combines the rules in rules that match branch.
func ProtectionFrom(rules []BranchRule, branch string) Protection {
	var p Protection
	for _, r := range rules {
		if !MatchBranch(r.Pattern, branch) {
			continue
		}
		p.Rules = append(p.Rules, r)
		p.RequirePR = p.RequirePR || r.RequirePR
		p.BlockForcePush = p.BlockForcePush || r.BlockForcePush
		p.RequireResolvedNotes = p.RequireResolvedNotes || r.RequireResolvedNotes
		p.LinearHistory = p.LinearHistory || r.LinearHistory
	}
	return p
}
