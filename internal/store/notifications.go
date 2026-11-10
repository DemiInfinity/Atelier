package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Notifications
//
// One row per person per subject (an issue, a pull request, the backups, an
// import job…). New activity on a subject the person already has a row for
// updates that row instead of adding another — it moves to the top, becomes
// unread and comes back to the inbox if it was marked done — like GitHub.
//
// person_id is always the recipient, so the Duo / Team / Company addons can
// fan one event out to several people without reshaping the table.

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS notifications (
  id             INTEGER PRIMARY KEY,
  person_id      INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  kind           TEXT NOT NULL,              -- latest event kind, e.g. issue.commented
  repo_id        INTEGER REFERENCES repos(id) ON DELETE CASCADE,
  subject_type   TEXT NOT NULL,              -- issue, pull, backup, import, action, runner, tether, release
  subject_number INTEGER NOT NULL DEFAULT 0, -- #number, job id, or 0 for one-per-person subjects
  title          TEXT NOT NULL,
  detail         TEXT NOT NULL DEFAULT '',
  url            TEXT NOT NULL DEFAULT '',
  actor_kind     TEXT NOT NULL DEFAULT '',
  actor_name     TEXT NOT NULL DEFAULT '',
  reason         TEXT NOT NULL DEFAULT '',
  unread         INTEGER NOT NULL DEFAULT 1,
  done           INTEGER NOT NULL DEFAULT 0,
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS notifications_inbox ON notifications(person_id, done, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS notifications_subject
  ON notifications(person_id, subject_type, IFNULL(repo_id, 0), subject_number);
CREATE TABLE IF NOT EXISTS notification_sources_seen (
  source TEXT NOT NULL,     -- e.g. backup_run, import_job
  ref_id INTEGER NOT NULL,
  PRIMARY KEY (source, ref_id)
);`)
}

// Notification is one row of a person's inbox.
type Notification struct {
	ID          int64
	PersonID    int64
	Kind        string
	RepoID      int64 // 0 = not about a repository
	Repo        string
	SubjectType string
	Number      int64
	Title       string
	Detail      string
	URL         string
	ActorKind   string
	ActorName   string
	Reason      string
	Unread      bool
	Done        bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewNotification is what Notify records for one recipient.
type NewNotification struct {
	PersonID    int64
	Kind        string
	RepoID      int64
	SubjectType string
	Number      int64
	Title       string
	Detail      string
	URL         string
	ActorKind   string
	ActorName   string
	Reason      string
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// Notify adds a notification, or coalesces it into the person's existing row
// for the same subject (bumping it, marking it unread and back in the inbox).
func (s *Store) Notify(ctx context.Context, n NewNotification) (int64, error) {
	t := now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM notifications
		WHERE person_id=? AND subject_type=? AND IFNULL(repo_id, 0)=? AND subject_number=?`,
		n.PersonID, n.SubjectType, n.RepoID, n.Number).Scan(&id)
	switch {
	case err == nil:
		_, err = tx.ExecContext(ctx, `UPDATE notifications SET kind=?, title=?, detail=?, url=?, actor_kind=?,
			actor_name=?, reason=?, unread=1, done=0, updated_at=? WHERE id=?`,
			n.Kind, n.Title, n.Detail, n.URL, n.ActorKind, n.ActorName, n.Reason, t, id)
	case err == sql.ErrNoRows:
		var res sql.Result
		res, err = tx.ExecContext(ctx, `INSERT INTO notifications (person_id, kind, repo_id, subject_type,
			subject_number, title, detail, url, actor_kind, actor_name, reason, unread, done, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 0, ?, ?)`,
			n.PersonID, n.Kind, nullID(n.RepoID), n.SubjectType, n.Number, n.Title, n.Detail, n.URL,
			n.ActorKind, n.ActorName, n.Reason, t, t)
		if err == nil {
			id, err = res.LastInsertId()
		}
	}
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// NotificationQuery selects part of one person's notifications.
type NotificationQuery struct {
	PersonID     int64
	Done         bool     // the done list instead of the inbox
	UnreadOnly   bool     //
	SubjectTypes []string // any of these subject types (empty = any)
	ActorKinds   []string // any of these actor kinds (empty = any)
	RepoID       int64    // one repository (0 = any)
	// ReadableRepos limits rows about repositories to these ids (rows that
	// aren't about a repository always pass). nil means no limit.
	ReadableRepos []int64
	Limit, Offset int
}

func inList(col string, vals []string, args *[]any) string {
	marks := make([]string, len(vals))
	for i, v := range vals {
		marks[i] = "?"
		*args = append(*args, v)
	}
	return col + ` IN (` + strings.Join(marks, ",") + `)`
}

func (q NotificationQuery) where(withState bool) (string, []any) {
	conds := []string{"n.person_id=?"}
	args := []any{q.PersonID}
	if withState {
		conds = append(conds, "n.done=?")
		args = append(args, q.Done)
		if q.UnreadOnly {
			conds = append(conds, "n.unread=1")
		}
	}
	if len(q.SubjectTypes) > 0 {
		conds = append(conds, inList("n.subject_type", q.SubjectTypes, &args))
	}
	if len(q.ActorKinds) > 0 {
		conds = append(conds, inList("n.actor_kind", q.ActorKinds, &args))
	}
	if q.RepoID != 0 {
		conds = append(conds, "n.repo_id=?")
		args = append(args, q.RepoID)
	}
	if q.ReadableRepos != nil {
		ids := make([]string, 0, len(q.ReadableRepos))
		for _, id := range q.ReadableRepos {
			ids = append(ids, "?")
			args = append(args, id)
		}
		if len(ids) == 0 {
			conds = append(conds, "n.repo_id IS NULL")
		} else {
			conds = append(conds, "(n.repo_id IS NULL OR n.repo_id IN ("+strings.Join(ids, ",")+"))")
		}
	}
	return strings.Join(conds, " AND "), args
}

// Notifications lists matching notifications, most recently updated first.
func (s *Store) Notifications(ctx context.Context, q NotificationQuery) ([]Notification, error) {
	where, args := q.where(true)
	if q.Limit <= 0 {
		q.Limit = 50
	}
	args = append(args, q.Limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, `SELECT n.id, n.person_id, n.kind, IFNULL(n.repo_id, 0),
		IFNULL(o.name || '/' || r.name, ''), n.subject_type, n.subject_number, n.title, n.detail, n.url,
		n.actor_kind, n.actor_name, n.reason, n.unread, n.done, n.created_at, n.updated_at
		FROM notifications n LEFT JOIN repos r ON r.id = n.repo_id LEFT JOIN owners o ON o.id = r.owner_id
		WHERE `+where+` ORDER BY n.updated_at DESC, n.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		var unread, done int
		var c, u int64
		if err := rows.Scan(&n.ID, &n.PersonID, &n.Kind, &n.RepoID, &n.Repo, &n.SubjectType, &n.Number, &n.Title,
			&n.Detail, &n.URL, &n.ActorKind, &n.ActorName, &n.Reason, &unread, &done, &c, &u); err != nil {
			return nil, err
		}
		n.Unread, n.Done = unread != 0, done != 0
		n.CreatedAt, n.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
		out = append(out, n)
	}
	return out, rows.Err()
}

// Notification returns one of a person's notifications.
func (s *Store) Notification(ctx context.Context, personID, id int64) (Notification, error) {
	var n Notification
	var unread, done int
	var c, u int64
	err := s.db.QueryRowContext(ctx, `SELECT id, person_id, kind, IFNULL(repo_id, 0), subject_type, subject_number,
		title, detail, url, actor_kind, actor_name, reason, unread, done, created_at, updated_at
		FROM notifications WHERE id=? AND person_id=?`, id, personID).
		Scan(&n.ID, &n.PersonID, &n.Kind, &n.RepoID, &n.SubjectType, &n.Number, &n.Title, &n.Detail, &n.URL,
			&n.ActorKind, &n.ActorName, &n.Reason, &unread, &done, &c, &u)
	if err != nil {
		return Notification{}, notFound(err)
	}
	n.Unread, n.Done = unread != 0, done != 0
	n.CreatedAt, n.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return n, nil
}

// UnreadNotificationCount is the bell count: unread rows still in the inbox.
// It is a single indexed COUNT, cheap enough for every page.
func (s *Store) UnreadNotificationCount(ctx context.Context, personID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE person_id=? AND done=0 AND unread=1`, personID).Scan(&n)
	return n, err
}

// NotificationGroup counts a person's inbox rows by subject, actor kind and repo
// (for the filter sidebar).
type NotificationGroup struct {
	SubjectType string
	ActorKind   string
	RepoID      int64
	Repo        string
	Count       int
	Unread      int
}

// NotificationGroups returns inbox (or done-list) counts, honouring
// q.ReadableRepos; the subject, actor and repo filters are ignored.
func (s *Store) NotificationGroups(ctx context.Context, q NotificationQuery) ([]NotificationGroup, error) {
	base := NotificationQuery{PersonID: q.PersonID, Done: q.Done, ReadableRepos: q.ReadableRepos}
	where, args := base.where(true)
	rows, err := s.db.QueryContext(ctx, `SELECT n.subject_type, n.actor_kind, IFNULL(n.repo_id, 0),
		IFNULL(o.name || '/' || r.name, ''), COUNT(*), SUM(n.unread)
		FROM notifications n LEFT JOIN repos r ON r.id = n.repo_id LEFT JOIN owners o ON o.id = r.owner_id
		WHERE `+where+` GROUP BY 1, 2, 3, 4`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationGroup
	for rows.Next() {
		var g NotificationGroup
		if err := rows.Scan(&g.SubjectType, &g.ActorKind, &g.RepoID, &g.Repo, &g.Count, &g.Unread); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MarkNotificationRead marks one notification read (it stays in the inbox).
func (s *Store) MarkNotificationRead(ctx context.Context, personID, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET unread=0 WHERE id=? AND person_id=?`, id, personID)
	return err
}

// MarkNotificationDone moves one notification out of the inbox.
func (s *Store) MarkNotificationDone(ctx context.Context, personID, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE notifications SET unread=0, done=1 WHERE id=? AND person_id=?`, id, personID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkNotificationsDone moves every inbox row matching q (state, subject,
// actor, repo and readable-repo filters; paging is ignored) out of the inbox.
// It returns how many rows moved.
func (s *Store) MarkNotificationsDone(ctx context.Context, q NotificationQuery) (int64, error) {
	q.Done = false
	where, args := q.where(true)
	where = strings.ReplaceAll(where, "n.", "")
	res, err := s.db.ExecContext(ctx, `UPDATE notifications SET unread=0, done=1 WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- watched sources ----------

// MarkNotificationSourceSeen records that a source row (a backup run, an
// import job…) has been turned into events. It reports whether this is the
// first time, so each row is announced once.
func (s *Store) MarkNotificationSourceSeen(ctx context.Context, source string, refID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO notification_sources_seen (source, ref_id) VALUES (?, ?)`, source, refID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
