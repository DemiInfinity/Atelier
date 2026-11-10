package store

// Issues, labels and milestones.
//
// Issues and pull requests share one #number sequence per repo (NextNumber),
// so an issue number lookup can legitimately miss when that number belongs to
// a pull request: IssueByNumber then returns ErrNotFound.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS milestones (
  id          INTEGER PRIMARY KEY,
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  title       TEXT NOT NULL COLLATE NOCASE,
  description TEXT NOT NULL DEFAULT '',
  due_on      INTEGER,
  state       TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','closed')),
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  closed_at   INTEGER,
  UNIQUE (repo_id, title)
);
CREATE TABLE IF NOT EXISTS issues (
  id           INTEGER PRIMARY KEY,
  repo_id      INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  number       INTEGER NOT NULL,
  title        TEXT NOT NULL,
  body         TEXT NOT NULL DEFAULT '',
  state        TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','closed')),
  state_reason TEXT NOT NULL DEFAULT '' CHECK (state_reason IN ('','completed','not_planned','reopened')),
  author_kind  TEXT NOT NULL,
  author_name  TEXT NOT NULL,
  milestone_id INTEGER REFERENCES milestones(id) ON DELETE SET NULL,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  closed_at    INTEGER,
  UNIQUE (repo_id, number)
);
CREATE INDEX IF NOT EXISTS issues_repo_state ON issues (repo_id, state, created_at);
CREATE INDEX IF NOT EXISTS issues_milestone ON issues (milestone_id);
CREATE TABLE IF NOT EXISTS issue_comments (
  id          INTEGER PRIMARY KEY,
  issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  author_kind TEXT NOT NULL,
  author_name TEXT NOT NULL,
  body        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS issue_comments_issue ON issue_comments (issue_id, created_at);
CREATE TABLE IF NOT EXISTS issue_events (
  id          INTEGER PRIMARY KEY,
  issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,
  actor_kind  TEXT NOT NULL,
  actor_name  TEXT NOT NULL,
  label_name  TEXT NOT NULL DEFAULT '',
  label_color TEXT NOT NULL DEFAULT '',
  milestone   TEXT NOT NULL DEFAULT '',
  old_title   TEXT NOT NULL DEFAULT '',
  new_title   TEXT NOT NULL DEFAULT '',
  reason      TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS issue_events_issue ON issue_events (issue_id, created_at);
CREATE TABLE IF NOT EXISTS labels (
  id          INTEGER PRIMARY KEY,
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  name        TEXT NOT NULL COLLATE NOCASE,
  color       TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  UNIQUE (repo_id, name)
);
CREATE TABLE IF NOT EXISTS issue_labels (
  issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  label_id INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
  PRIMARY KEY (issue_id, label_id)
);
CREATE INDEX IF NOT EXISTS issue_labels_label ON issue_labels (label_id);
CREATE TABLE IF NOT EXISTS label_seeds (
  repo_id INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE
);`)
}

// Issue event kinds shown in the timeline.
const (
	EventLabeled      = "labeled"
	EventUnlabeled    = "unlabeled"
	EventClosed       = "closed"
	EventReopened     = "reopened"
	EventRenamed      = "renamed"
	EventMilestoned   = "milestoned"
	EventDemilestoned = "demilestoned"
)

// Close reasons, as GitHub names them.
const (
	ReasonCompleted  = "completed"
	ReasonNotPlanned = "not_planned"
)

var (
	ErrInvalidLabel     = errors.New("label names need 1–50 characters and a 6-digit hex colour")
	ErrInvalidMilestone = errors.New("milestone titles need 1–100 characters")
	ErrInvalidIssue     = errors.New("issues need a title of 1–256 characters")
)

// ---------- types ----------

type Label struct {
	ID          int64
	RepoID      int64
	Name        string
	Color       string // six lowercase hex digits, no '#'
	Description string
	Issues      int // open issues using it (only filled by Labels)
}

type Milestone struct {
	ID          int64
	RepoID      int64
	Title       string
	Description string
	DueOn       *time.Time
	State       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    *time.Time
	Open        int // open issues (filled by Milestones / MilestoneByID)
	Closed      int // closed issues
}

// Percent is how much of the milestone is done, 0–100.
func (m Milestone) Percent() int {
	if m.Open+m.Closed == 0 {
		return 0
	}
	return m.Closed * 100 / (m.Open + m.Closed)
}

// Overdue reports whether an open milestone's due date has passed.
func (m Milestone) Overdue() bool {
	return m.State == "open" && m.DueOn != nil && time.Now().After(m.DueOn.Add(24*time.Hour))
}

type Issue struct {
	ID          int64
	RepoID      int64
	Number      int64
	Title       string
	Body        string
	State       string // open, closed
	StateReason string // completed, not_planned, reopened or ""
	AuthorKind  string
	AuthorName  string
	MilestoneID int64 // 0 = none
	Milestone   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    *time.Time
	Comments    int
	Labels      []Label
}

func (i Issue) IsOpen() bool { return i.State == "open" }

type IssueComment struct {
	ID         int64
	IssueID    int64
	AuthorKind string
	AuthorName string
	Body       string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (c IssueComment) Edited() bool { return c.UpdatedAt.After(c.CreatedAt) }

type IssueEvent struct {
	ID         int64
	IssueID    int64
	Kind       string
	ActorKind  string
	ActorName  string
	LabelName  string
	LabelColor string
	Milestone  string
	OldTitle   string
	NewTitle   string
	Reason     string
	CreatedAt  time.Time
}

// ---------- helpers ----------

func unixPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

var hexColor = regexp.MustCompile(`^[0-9a-f]{6}$`)

// NormalizeColor accepts "#AbC123", "abc123" or "#abc" and returns six lowercase hex digits.
func NormalizeColor(c string) (string, bool) {
	c = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(c), "#"))
	if len(c) == 3 {
		c = string([]byte{c[0], c[0], c[1], c[1], c[2], c[2]})
	}
	return c, hexColor.MatchString(c)
}

func (s *Store) issueEvent(ctx context.Context, tx *sql.Tx, e IssueEvent) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO issue_events
		(issue_id, kind, actor_kind, actor_name, label_name, label_color, milestone, old_title, new_title, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.IssueID, e.Kind, e.ActorKind, e.ActorName, e.LabelName, e.LabelColor, e.Milestone, e.OldTitle, e.NewTitle, e.Reason, now())
	return err
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func touchIssue(ctx context.Context, tx *sql.Tx, issueID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE issues SET updated_at=? WHERE id=?`, now(), issueID)
	return err
}

// ---------- issues ----------

const issueSelect = `SELECT i.id, i.repo_id, i.number, i.title, i.body, i.state, i.state_reason, i.author_kind, i.author_name,
  COALESCE(i.milestone_id, 0), COALESCE(m.title, ''), i.created_at, i.updated_at, i.closed_at,
  (SELECT COUNT(*) FROM issue_comments c WHERE c.issue_id = i.id)
FROM issues i LEFT JOIN milestones m ON m.id = i.milestone_id`

func scanIssue(row interface{ Scan(...any) error }) (Issue, error) {
	var i Issue
	var c, u int64
	var closed sql.NullInt64
	err := row.Scan(&i.ID, &i.RepoID, &i.Number, &i.Title, &i.Body, &i.State, &i.StateReason, &i.AuthorKind, &i.AuthorName,
		&i.MilestoneID, &i.Milestone, &c, &u, &closed, &i.Comments)
	i.CreatedAt, i.UpdatedAt, i.ClosedAt = time.Unix(c, 0), time.Unix(u, 0), unixPtr(closed)
	return i, err
}

func validIssueTitle(t string) bool { n := len([]rune(t)); return n >= 1 && n <= 256 }

// CreateIssue opens a new issue with the repo's next #number.
func (s *Store) CreateIssue(ctx context.Context, repoID int64, title, body, authorKind, authorName string) (Issue, error) {
	title = strings.TrimSpace(title)
	if !validIssueTitle(title) {
		return Issue{}, ErrInvalidIssue
	}
	n, err := s.NextNumber(ctx, repoID)
	if err != nil {
		return Issue{}, err
	}
	t := now()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO issues
		(repo_id, number, title, body, author_kind, author_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		repoID, n, title, body, authorKind, authorName, t, t); err != nil {
		return Issue{}, err
	}
	return s.IssueByNumber(ctx, repoID, n)
}

// IssueByNumber finds issue #number in a repo, with its labels. It returns
// ErrNotFound when the number is unused or belongs to a pull request.
func (s *Store) IssueByNumber(ctx context.Context, repoID, number int64) (Issue, error) {
	i, err := scanIssue(s.db.QueryRowContext(ctx, issueSelect+` WHERE i.repo_id=? AND i.number=?`, repoID, number))
	if err != nil {
		return Issue{}, notFound(err)
	}
	labels, err := s.issueLabels(ctx, []int64{i.ID})
	i.Labels = labels[i.ID]
	return i, err
}

// IssueNumbers returns every issue number in a repo (for #123 links).
func (s *Store) IssueNumbers(ctx context.Context, repoID int64) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT number FROM issues WHERE repo_id=?`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// UpdateIssue changes the title and body, recording a "renamed" event when the title changes.
func (s *Store) UpdateIssue(ctx context.Context, issueID int64, title, body, actorKind, actorName string) error {
	title = strings.TrimSpace(title)
	if !validIssueTitle(title) {
		return ErrInvalidIssue
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var old string
		if err := tx.QueryRowContext(ctx, `SELECT title FROM issues WHERE id=?`, issueID).Scan(&old); err != nil {
			return notFound(err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET title=?, body=?, updated_at=? WHERE id=?`, title, body, now(), issueID); err != nil {
			return err
		}
		if old != title {
			return s.issueEvent(ctx, tx, IssueEvent{IssueID: issueID, Kind: EventRenamed, ActorKind: actorKind, ActorName: actorName, OldTitle: old, NewTitle: title})
		}
		return nil
	})
}

// CloseIssue closes an open issue as completed (default) or not_planned. Closing
// a closed issue is a no-op.
func (s *Store) CloseIssue(ctx context.Context, issueID int64, actorKind, actorName, reason string) error {
	if reason != ReasonNotPlanned {
		reason = ReasonCompleted
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE issues SET state='closed', state_reason=?, closed_at=?, updated_at=? WHERE id=? AND state='open'`,
			reason, now(), now(), issueID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return s.issueEvent(ctx, tx, IssueEvent{IssueID: issueID, Kind: EventClosed, ActorKind: actorKind, ActorName: actorName, Reason: reason})
	})
}

// CloseIssueByNumber is for other features (e.g. a merged pull request that
// says "closes #N"). It returns ErrNotFound if #number isn't an issue.
func (s *Store) CloseIssueByNumber(ctx context.Context, repoID, number int64, actorKind, actorName, reason string) error {
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM issues WHERE repo_id=? AND number=?`, repoID, number).Scan(&id); err != nil {
		return notFound(err)
	}
	return s.CloseIssue(ctx, id, actorKind, actorName, reason)
}

// ReopenIssue reopens a closed issue. Reopening an open issue is a no-op.
func (s *Store) ReopenIssue(ctx context.Context, issueID int64, actorKind, actorName string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE issues SET state='open', state_reason='reopened', closed_at=NULL, updated_at=? WHERE id=? AND state='closed'`,
			now(), issueID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return s.issueEvent(ctx, tx, IssueEvent{IssueID: issueID, Kind: EventReopened, ActorKind: actorKind, ActorName: actorName})
	})
}

// ---------- search ----------

// IssueQuery is a parsed issue search such as `is:open label:bug "crash on start"`.
type IssueQuery struct {
	State       string   // "open", "closed" or "" for both
	Labels      []string // all must match
	NoLabel     bool
	Milestone   string
	NoMilestone bool
	Author      string
	Text        []string // free words/phrases; each must appear in the title or body
	Sort        string   // newest (default), oldest, updated
}

// tokenize splits on spaces but keeps "quoted phrases" (also after a qualifier, as in label:"good first issue").
func tokenize(q string) []string {
	var out []string
	var cur strings.Builder
	inQuote, has := false, false
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
			has = true
		case !inQuote && (r == ' ' || r == '\t' || r == '\n'):
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	return out
}

// ParseIssueQuery understands is:open, is:closed, label:x, milestone:"x",
// no:label, no:milestone, author:x and sort:created-asc|created-desc|updated-desc;
// everything else is free text. is:issue is accepted and ignored.
func ParseIssueQuery(q string) IssueQuery {
	var out IssueQuery
	for _, tok := range tokenize(q) {
		key, val, ok := strings.Cut(tok, ":")
		if !ok || val == "" {
			if tok != "" {
				out.Text = append(out.Text, tok)
			}
			continue
		}
		switch strings.ToLower(key) {
		case "is", "state":
			switch strings.ToLower(val) {
			case "open":
				out.State = "open"
			case "closed":
				out.State = "closed"
			case "issue":
			default:
				out.Text = append(out.Text, tok)
			}
		case "label":
			out.Labels = append(out.Labels, val)
		case "milestone":
			out.Milestone = val
		case "author":
			out.Author = val
		case "no":
			switch strings.ToLower(val) {
			case "label":
				out.NoLabel = true
			case "milestone":
				out.NoMilestone = true
			default:
				out.Text = append(out.Text, tok)
			}
		case "sort":
			switch strings.ToLower(val) {
			case "created-asc":
				out.Sort = "oldest"
			case "updated-desc", "updated":
				out.Sort = "updated"
			case "created-desc":
				out.Sort = "newest"
			default:
				out.Text = append(out.Text, tok)
			}
		default:
			out.Text = append(out.Text, tok)
		}
	}
	return out
}

func quoteIfNeeded(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return `"` + strings.ReplaceAll(s, `"`, "") + `"`
	}
	return s
}

// String turns a query back into search-box text (sort is kept separately).
func (q IssueQuery) String() string {
	var parts []string
	if q.State != "" {
		parts = append(parts, "is:"+q.State)
	}
	for _, l := range q.Labels {
		parts = append(parts, "label:"+quoteIfNeeded(l))
	}
	if q.NoLabel {
		parts = append(parts, "no:label")
	}
	if q.Milestone != "" {
		parts = append(parts, "milestone:"+quoteIfNeeded(q.Milestone))
	}
	if q.NoMilestone {
		parts = append(parts, "no:milestone")
	}
	if q.Author != "" {
		parts = append(parts, "author:"+quoteIfNeeded(q.Author))
	}
	for _, t := range q.Text {
		parts = append(parts, quoteIfNeeded(t))
	}
	return strings.Join(parts, " ")
}

// HasLabel reports whether the query filters on a label (case-insensitive).
func (q IssueQuery) HasLabel(name string) bool {
	for _, l := range q.Labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// where builds the filter, ignoring State when withState is false (for open/closed counts).
func (q IssueQuery) where(repoID int64, withState bool) (string, []any) {
	conds := []string{"i.repo_id = ?"}
	args := []any{repoID}
	if withState && q.State != "" {
		conds = append(conds, "i.state = ?")
		args = append(args, q.State)
	}
	for _, l := range q.Labels {
		conds = append(conds, `EXISTS (SELECT 1 FROM issue_labels il JOIN labels l ON l.id = il.label_id WHERE il.issue_id = i.id AND l.name = ?)`)
		args = append(args, l)
	}
	if q.NoLabel {
		conds = append(conds, `NOT EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = i.id)`)
	}
	if q.Milestone != "" {
		conds = append(conds, `i.milestone_id IN (SELECT id FROM milestones WHERE repo_id = ? AND title = ?)`)
		args = append(args, repoID, q.Milestone)
	}
	if q.NoMilestone {
		conds = append(conds, `i.milestone_id IS NULL`)
	}
	if q.Author != "" {
		conds = append(conds, `i.author_name = ? COLLATE NOCASE`)
		args = append(args, q.Author)
	}
	for _, t := range q.Text {
		if n, err := strconv.ParseInt(strings.TrimPrefix(t, "#"), 10, 64); err == nil && strings.HasPrefix(t, "#") {
			conds = append(conds, `i.number = ?`)
			args = append(args, n)
			continue
		}
		conds = append(conds, `(i.title LIKE ? ESCAPE '\' OR i.body LIKE ? ESCAPE '\')`)
		args = append(args, likeEscape(t), likeEscape(t))
	}
	return strings.Join(conds, " AND "), args
}

// IssueCounts returns how many issues match the query when open and when closed.
func (s *Store) IssueCounts(ctx context.Context, repoID int64, q IssueQuery) (open, closed int, err error) {
	w, args := q.where(repoID, false)
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(i.state='open'),0), COALESCE(SUM(i.state='closed'),0) FROM issues i WHERE `+w, args...).
		Scan(&open, &closed)
	return
}

// Issues lists matching issues (with labels) for one page; more reports whether another page follows.
func (s *Store) Issues(ctx context.Context, repoID int64, q IssueQuery, offset, limit int) (issues []Issue, more bool, err error) {
	w, args := q.where(repoID, true)
	order := "i.created_at DESC, i.number DESC"
	switch q.Sort {
	case "oldest":
		order = "i.created_at ASC, i.number ASC"
	case "updated":
		order = "i.updated_at DESC, i.number DESC"
	}
	args = append(args, limit+1, offset)
	rows, err := s.db.QueryContext(ctx, issueSelect+` WHERE `+w+` ORDER BY `+order+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, false, err
		}
		issues = append(issues, i)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(issues) > limit {
		issues, more = issues[:limit], true
	}
	ids := make([]int64, len(issues))
	for k, i := range issues {
		ids[k] = i.ID
	}
	labels, err := s.issueLabels(ctx, ids)
	if err != nil {
		return nil, false, err
	}
	for k := range issues {
		issues[k].Labels = labels[issues[k].ID]
	}
	return issues, more, nil
}

// ---------- comments ----------

func scanComment(row interface{ Scan(...any) error }) (IssueComment, error) {
	var c IssueComment
	var cr, up int64
	err := row.Scan(&c.ID, &c.IssueID, &c.AuthorKind, &c.AuthorName, &c.Body, &cr, &up)
	c.CreatedAt, c.UpdatedAt = time.Unix(cr, 0), time.Unix(up, 0)
	return c, err
}

const commentSelect = `SELECT id, issue_id, author_kind, author_name, body, created_at, updated_at FROM issue_comments`

func (s *Store) AddIssueComment(ctx context.Context, issueID int64, body, authorKind, authorName string) (IssueComment, error) {
	if strings.TrimSpace(body) == "" {
		return IssueComment{}, errors.New("comment is empty")
	}
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		t := now()
		res, err := tx.ExecContext(ctx, `INSERT INTO issue_comments (issue_id, author_kind, author_name, body, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			issueID, authorKind, authorName, body, t, t)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return touchIssue(ctx, tx, issueID)
	})
	if err != nil {
		return IssueComment{}, err
	}
	return s.IssueCommentByID(ctx, id)
}

func (s *Store) IssueCommentByID(ctx context.Context, id int64) (IssueComment, error) {
	c, err := scanComment(s.db.QueryRowContext(ctx, commentSelect+` WHERE id=?`, id))
	return c, notFound(err)
}

func (s *Store) IssueComments(ctx context.Context, issueID int64) ([]IssueComment, error) {
	rows, err := s.db.QueryContext(ctx, commentSelect+` WHERE issue_id=? ORDER BY created_at, id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IssueComment
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UpdateIssueComment(ctx context.Context, id int64, body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("comment is empty")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE issue_comments SET body=?, updated_at=? WHERE id=?`, body, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteIssueComment(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM issue_comments WHERE id=?`, id)
	return err
}

// ---------- events ----------

func (s *Store) IssueEvents(ctx context.Context, issueID int64) ([]IssueEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, issue_id, kind, actor_kind, actor_name, label_name, label_color, milestone,
		old_title, new_title, reason, created_at FROM issue_events WHERE issue_id=? ORDER BY created_at, id`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IssueEvent
	for rows.Next() {
		var e IssueEvent
		var c int64
		if err := rows.Scan(&e.ID, &e.IssueID, &e.Kind, &e.ActorKind, &e.ActorName, &e.LabelName, &e.LabelColor, &e.Milestone,
			&e.OldTitle, &e.NewTitle, &e.Reason, &c); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(c, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- labels ----------

// DefaultLabels are created for each repo the first time its issues are visited.
var DefaultLabels = []Label{
	{Name: "bug", Color: "d73a4a", Description: "Something isn't working"},
	{Name: "enhancement", Color: "a2eeef", Description: "New feature or request"},
	{Name: "docs", Color: "0075ca", Description: "Improvements or additions to documentation"},
	{Name: "infra", Color: "c5a3ff", Description: "Build, CI, deploy and tooling"},
	{Name: "question", Color: "d876e3", Description: "Further information is requested"},
}

// EnsureDefaultLabels seeds DefaultLabels once per repo. Later deletions stick.
func (s *Store) EnsureDefaultLabels(ctx context.Context, repoID int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO label_seeds (repo_id) VALUES (?)`, repoID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		for _, l := range DefaultLabels {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO labels (repo_id, name, color, description, created_at) VALUES (?, ?, ?, ?, ?)`,
				repoID, l.Name, l.Color, l.Description, now()); err != nil {
				return err
			}
		}
		return nil
	})
}

func validLabel(name, color string) (string, string, error) {
	name = strings.TrimSpace(name)
	c, ok := NormalizeColor(color)
	if n := len([]rune(name)); n < 1 || n > 50 || !ok || strings.ContainsAny(name, "\"\n\r\t") {
		return "", "", ErrInvalidLabel
	}
	return name, c, nil
}

func (s *Store) CreateLabel(ctx context.Context, repoID int64, name, color, desc string) (Label, error) {
	name, color, err := validLabel(name, color)
	if err != nil {
		return Label{}, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO labels (repo_id, name, color, description, created_at) VALUES (?, ?, ?, ?, ?)`,
		repoID, name, color, strings.TrimSpace(desc), now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Label{}, fmt.Errorf("a label named %q already exists", name)
		}
		return Label{}, err
	}
	id, _ := res.LastInsertId()
	return s.LabelByID(ctx, repoID, id)
}

func (s *Store) UpdateLabel(ctx context.Context, repoID, id int64, name, color, desc string) error {
	name, color, err := validLabel(name, color)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE labels SET name=?, color=?, description=? WHERE id=? AND repo_id=?`,
		name, color, strings.TrimSpace(desc), id, repoID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("a label named %q already exists", name)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteLabel(ctx context.Context, repoID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM labels WHERE id=? AND repo_id=?`, id, repoID)
	return err
}

func (s *Store) LabelByID(ctx context.Context, repoID, id int64) (Label, error) {
	var l Label
	err := s.db.QueryRowContext(ctx, `SELECT id, repo_id, name, color, description FROM labels WHERE id=? AND repo_id=?`, id, repoID).
		Scan(&l.ID, &l.RepoID, &l.Name, &l.Color, &l.Description)
	return l, notFound(err)
}

// Labels lists a repo's labels by name, with how many open issues use each.
func (s *Store) Labels(ctx context.Context, repoID int64) ([]Label, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.id, l.repo_id, l.name, l.color, l.description,
		(SELECT COUNT(*) FROM issue_labels il JOIN issues i ON i.id = il.issue_id WHERE il.label_id = l.id AND i.state = 'open')
		FROM labels l WHERE l.repo_id=? ORDER BY l.name`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Label
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.ID, &l.RepoID, &l.Name, &l.Color, &l.Description, &l.Issues); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) issueLabels(ctx context.Context, issueIDs []int64) (map[int64][]Label, error) {
	out := map[int64][]Label{}
	if len(issueIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(issueIDs)), ",")
	args := make([]any, len(issueIDs))
	for k, id := range issueIDs {
		args[k] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT il.issue_id, l.id, l.repo_id, l.name, l.color, l.description
		FROM issue_labels il JOIN labels l ON l.id = il.label_id WHERE il.issue_id IN (`+ph+`) ORDER BY l.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var iid int64
		var l Label
		if err := rows.Scan(&iid, &l.ID, &l.RepoID, &l.Name, &l.Color, &l.Description); err != nil {
			return nil, err
		}
		out[iid] = append(out[iid], l)
	}
	return out, rows.Err()
}

// SetIssueLabels replaces an issue's labels, recording labeled/unlabeled events.
// Label IDs from another repo are ignored.
func (s *Store) SetIssueLabels(ctx context.Context, issueID int64, labelIDs []int64, actorKind, actorName string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var repoID int64
		if err := tx.QueryRowContext(ctx, `SELECT repo_id FROM issues WHERE id=?`, issueID).Scan(&repoID); err != nil {
			return notFound(err)
		}
		want := map[int64]bool{}
		for _, id := range labelIDs {
			want[id] = true
		}
		type lab struct {
			id          int64
			name, color string
			has         bool
		}
		rows, err := tx.QueryContext(ctx, `SELECT l.id, l.name, l.color,
			EXISTS (SELECT 1 FROM issue_labels il WHERE il.issue_id = ? AND il.label_id = l.id)
			FROM labels l WHERE l.repo_id = ? ORDER BY l.name`, issueID, repoID)
		if err != nil {
			return err
		}
		var all []lab
		for rows.Next() {
			var l lab
			if err := rows.Scan(&l.id, &l.name, &l.color, &l.has); err != nil {
				rows.Close()
				return err
			}
			all = append(all, l)
		}
		rows.Close()
		changed := false
		for _, l := range all {
			switch {
			case want[l.id] && !l.has:
				if _, err := tx.ExecContext(ctx, `INSERT INTO issue_labels (issue_id, label_id) VALUES (?, ?)`, issueID, l.id); err != nil {
					return err
				}
				if err := s.issueEvent(ctx, tx, IssueEvent{IssueID: issueID, Kind: EventLabeled, ActorKind: actorKind, ActorName: actorName, LabelName: l.name, LabelColor: l.color}); err != nil {
					return err
				}
				changed = true
			case !want[l.id] && l.has:
				if _, err := tx.ExecContext(ctx, `DELETE FROM issue_labels WHERE issue_id=? AND label_id=?`, issueID, l.id); err != nil {
					return err
				}
				if err := s.issueEvent(ctx, tx, IssueEvent{IssueID: issueID, Kind: EventUnlabeled, ActorKind: actorKind, ActorName: actorName, LabelName: l.name, LabelColor: l.color}); err != nil {
					return err
				}
				changed = true
			}
		}
		if changed {
			return touchIssue(ctx, tx, issueID)
		}
		return nil
	})
}

// ---------- milestones ----------

const milestoneSelect = `SELECT m.id, m.repo_id, m.title, m.description, m.due_on, m.state, m.created_at, m.updated_at, m.closed_at,
  (SELECT COUNT(*) FROM issues i WHERE i.milestone_id = m.id AND i.state = 'open'),
  (SELECT COUNT(*) FROM issues i WHERE i.milestone_id = m.id AND i.state = 'closed')
FROM milestones m`

func scanMilestone(row interface{ Scan(...any) error }) (Milestone, error) {
	var m Milestone
	var c, u int64
	var due, closed sql.NullInt64
	err := row.Scan(&m.ID, &m.RepoID, &m.Title, &m.Description, &due, &m.State, &c, &u, &closed, &m.Open, &m.Closed)
	m.CreatedAt, m.UpdatedAt, m.DueOn, m.ClosedAt = time.Unix(c, 0), time.Unix(u, 0), unixPtr(due), unixPtr(closed)
	if m.DueOn != nil {
		d := m.DueOn.UTC()
		m.DueOn = &d
	}
	return m, err
}

func dueArg(due *time.Time) any {
	if due == nil {
		return nil
	}
	return due.Unix()
}

func validMilestone(title string) (string, error) {
	title = strings.TrimSpace(title)
	if n := len([]rune(title)); n < 1 || n > 100 || strings.ContainsAny(title, "\"\n\r\t") {
		return "", ErrInvalidMilestone
	}
	return title, nil
}

func (s *Store) CreateMilestone(ctx context.Context, repoID int64, title, desc string, due *time.Time) (Milestone, error) {
	title, err := validMilestone(title)
	if err != nil {
		return Milestone{}, err
	}
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO milestones (repo_id, title, description, due_on, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		repoID, title, strings.TrimSpace(desc), dueArg(due), t, t)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Milestone{}, fmt.Errorf("a milestone called %q already exists", title)
		}
		return Milestone{}, err
	}
	id, _ := res.LastInsertId()
	return s.MilestoneByID(ctx, repoID, id)
}

func (s *Store) UpdateMilestone(ctx context.Context, repoID, id int64, title, desc string, due *time.Time) error {
	title, err := validMilestone(title)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE milestones SET title=?, description=?, due_on=?, updated_at=? WHERE id=? AND repo_id=?`,
		title, strings.TrimSpace(desc), dueArg(due), now(), id, repoID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("a milestone called %q already exists", title)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMilestoneState opens or closes a milestone.
func (s *Store) SetMilestoneState(ctx context.Context, repoID, id int64, open bool) error {
	q := `UPDATE milestones SET state='closed', closed_at=?, updated_at=? WHERE id=? AND repo_id=?`
	args := []any{now(), now(), id, repoID}
	if open {
		q = `UPDATE milestones SET state='open', closed_at=NULL, updated_at=? WHERE id=? AND repo_id=?`
		args = args[1:]
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

func (s *Store) DeleteMilestone(ctx context.Context, repoID, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM milestones WHERE id=? AND repo_id=?`, id, repoID)
	return err
}

func (s *Store) MilestoneByID(ctx context.Context, repoID, id int64) (Milestone, error) {
	m, err := scanMilestone(s.db.QueryRowContext(ctx, milestoneSelect+` WHERE m.id=? AND m.repo_id=?`, id, repoID))
	return m, notFound(err)
}

// Milestones lists a repo's milestones in one state ("open", "closed" or "" for all):
// soonest due first, then undated ones by title.
func (s *Store) Milestones(ctx context.Context, repoID int64, state string) ([]Milestone, error) {
	q := milestoneSelect + ` WHERE m.repo_id=?`
	args := []any{repoID}
	if state != "" {
		q += ` AND m.state=?`
		args = append(args, state)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY m.due_on IS NULL, m.due_on, m.title`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Milestone
	for rows.Next() {
		m, err := scanMilestone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MilestoneCounts returns how many milestones are open and closed.
func (s *Store) MilestoneCounts(ctx context.Context, repoID int64) (open, closed int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(state='open'),0), COALESCE(SUM(state='closed'),0) FROM milestones WHERE repo_id=?`, repoID).
		Scan(&open, &closed)
	return
}

// SetIssueMilestone sets (milestoneID > 0) or clears (0) an issue's milestone, recording an event.
func (s *Store) SetIssueMilestone(ctx context.Context, issueID, milestoneID int64, actorKind, actorName string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var repoID, cur int64
		var curTitle string
		if err := tx.QueryRowContext(ctx, `SELECT i.repo_id, COALESCE(i.milestone_id, 0), COALESCE(m.title, '')
			FROM issues i LEFT JOIN milestones m ON m.id = i.milestone_id WHERE i.id=?`, issueID).Scan(&repoID, &cur, &curTitle); err != nil {
			return notFound(err)
		}
		if cur == milestoneID {
			return nil
		}
		var newTitle string
		var arg any
		if milestoneID > 0 {
			if err := tx.QueryRowContext(ctx, `SELECT title FROM milestones WHERE id=? AND repo_id=?`, milestoneID, repoID).Scan(&newTitle); err != nil {
				return notFound(err)
			}
			arg = milestoneID
		}
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET milestone_id=?, updated_at=? WHERE id=?`, arg, now(), issueID); err != nil {
			return err
		}
		if cur != 0 && milestoneID == 0 {
			return s.issueEvent(ctx, tx, IssueEvent{IssueID: issueID, Kind: EventDemilestoned, ActorKind: actorKind, ActorName: actorName, Milestone: curTitle})
		}
		return s.issueEvent(ctx, tx, IssueEvent{IssueID: issueID, Kind: EventMilestoned, ActorKind: actorKind, ActorName: actorName, Milestone: newTitle})
	})
}

// ---------- imports ----------
//
// Used by the importer to bring issues in from Gitea/Forgejo or GitHub with
// their original numbers, authors and timestamps. They don't record timeline
// events (the history happened elsewhere) and don't touch repo_counters: the
// importer bumps the counter once it knows the highest number.

// ImportedIssue is an issue as it existed at the source.
type ImportedIssue struct {
	RepoID      int64
	Number      int64
	Title       string
	Body        string
	State       string // open, closed
	StateReason string // completed, not_planned, reopened or ""
	AuthorKind  string // "import"
	AuthorName  string // e.g. "github:alice"
	MilestoneID int64
	LabelIDs    []int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    *time.Time
}

func optUnix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

// ImportIssue inserts an issue with an explicit number. It fails if the
// number is already used by an issue.
func (s *Store) ImportIssue(ctx context.Context, in ImportedIssue) (Issue, error) {
	title := strings.TrimSpace(in.Title)
	if r := []rune(title); len(r) > 256 {
		title = string(r[:256])
	}
	if title == "" {
		title = "(untitled)"
	}
	state, reason := "open", in.StateReason
	if in.State == "closed" {
		state = "closed"
		if reason != ReasonNotPlanned {
			reason = ReasonCompleted
		}
	} else if reason != "reopened" {
		reason = ""
	}
	closed := in.ClosedAt
	if state == "closed" && closed == nil {
		closed = &in.UpdatedAt
	}
	if state == "open" {
		closed = nil
	}
	var ms any
	if in.MilestoneID > 0 {
		ms = in.MilestoneID
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO issues (repo_id, number, title, body, state, state_reason, author_kind,
			author_name, milestone_id, created_at, updated_at, closed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.RepoID, in.Number, title, in.Body, state, reason, in.AuthorKind, in.AuthorName, ms,
			in.CreatedAt.Unix(), in.UpdatedAt.Unix(), optUnix(closed))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		for _, lid := range in.LabelIDs {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO issue_labels (issue_id, label_id)
				SELECT ?, id FROM labels WHERE id=? AND repo_id=?`, id, lid, in.RepoID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Issue{}, err
	}
	return s.IssueByNumber(ctx, in.RepoID, in.Number)
}

// ImportIssueComment adds a comment with its original author and timestamps.
// It doesn't change the issue's updated time.
func (s *Store) ImportIssueComment(ctx context.Context, issueID int64, body, authorKind, authorName string, created, updated time.Time) (IssueComment, error) {
	if updated.Before(created) {
		updated = created
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO issue_comments (issue_id, author_kind, author_name, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`, issueID, authorKind, authorName, body, created.Unix(), updated.Unix())
	if err != nil {
		return IssueComment{}, err
	}
	id, _ := res.LastInsertId()
	return s.IssueCommentByID(ctx, id)
}

// ImportMilestone creates a milestone with its original state and timestamps.
func (s *Store) ImportMilestone(ctx context.Context, repoID int64, title, desc string, due *time.Time, open bool, created, updated time.Time, closed *time.Time) (Milestone, error) {
	title, err := validMilestone(title)
	if err != nil {
		return Milestone{}, err
	}
	state := "open"
	if !open {
		state = "closed"
		if closed == nil {
			closed = &updated
		}
	} else {
		closed = nil
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO milestones (repo_id, title, description, due_on, state, created_at, updated_at, closed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, repoID, title, strings.TrimSpace(desc), dueArg(due), state,
		created.Unix(), updated.Unix(), optUnix(closed))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Milestone{}, fmt.Errorf("a milestone called %q already exists", title)
		}
		return Milestone{}, err
	}
	id, _ := res.LastInsertId()
	return s.MilestoneByID(ctx, repoID, id)
}

// MarkLabelsSeeded stops the default labels being added to a repo whose
// labels were imported.
func (s *Store) MarkLabelsSeeded(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO label_seeds (repo_id) VALUES (?)`, repoID)
	return err
}
