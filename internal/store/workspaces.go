package store

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"time"
)

// Device workspaces: "Open on <device>" asks a runner with the workspaces
// capability to clone or update a repository in its workspaces folder and
// open it with one of the runner's own presets. Requests carry a short-lived
// repository token (only its hash is stored). Runners and workspaces belong
// to a person, so the multi-person addons can share nothing by default.
// The logic lives in internal/web/workspaces*.go.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS workspace_requests (
  id            INTEGER PRIMARY KEY,
  runner_id     INTEGER NOT NULL REFERENCES actions_runners(id) ON DELETE CASCADE,
  person_id     INTEGER NOT NULL,
  repo_id       INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  branch        TEXT NOT NULL,
  preset        TEXT NOT NULL DEFAULT '',
  state         TEXT NOT NULL DEFAULT 'pending',
  message       TEXT NOT NULL DEFAULT '',
  path          TEXT NOT NULL DEFAULT '',
  dirty         INTEGER NOT NULL DEFAULT 0,
  token_hash    TEXT NOT NULL DEFAULT '',
  token_expires INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS workspace_requests_runner ON workspace_requests (runner_id, state);
CREATE INDEX IF NOT EXISTS workspace_requests_token ON workspace_requests (token_hash);

CREATE TABLE IF NOT EXISTS workspace_devices (
  runner_id  INTEGER PRIMARY KEY REFERENCES actions_runners(id) ON DELETE CASCADE,
  presets    TEXT NOT NULL DEFAULT '[]',
  root       TEXT NOT NULL DEFAULT '',
  ssh        INTEGER NOT NULL DEFAULT 0,
  seen_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS workspaces (
  id          INTEGER PRIMARY KEY,
  runner_id   INTEGER NOT NULL REFERENCES actions_runners(id) ON DELETE CASCADE,
  owner_name  TEXT NOT NULL,
  repo_name   TEXT NOT NULL,
  path        TEXT NOT NULL,
  branch      TEXT NOT NULL DEFAULT '',
  head        TEXT NOT NULL DEFAULT '',
  dirty       INTEGER NOT NULL DEFAULT 0,
  last_opened INTEGER NOT NULL DEFAULT 0,
  updated_at  INTEGER NOT NULL,
  UNIQUE (runner_id, path)
);
`)
}

// Workspace request states. pending → delivered → (cloning|fetching) →
// cloned|updated|opened|failed; a request nobody picks up expires.
const (
	WorkspacePending   = "pending"
	WorkspaceDelivered = "delivered"
	WorkspaceExpired   = "expired"
)

type WorkspaceRequest struct {
	ID           int64
	RunnerID     int64
	PersonID     int64
	RepoID       int64
	Branch       string
	Preset       string
	State        string
	Message      string
	Path         string
	Dirty        bool
	TokenExpires time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const wsReqCols = `id, runner_id, person_id, repo_id, branch, preset, state, message, path, dirty, token_expires, created_at, updated_at`

func scanWorkspaceRequest(row interface{ Scan(...any) error }) (WorkspaceRequest, error) {
	var w WorkspaceRequest
	var exp, c, u int64
	err := row.Scan(&w.ID, &w.RunnerID, &w.PersonID, &w.RepoID, &w.Branch, &w.Preset, &w.State, &w.Message, &w.Path,
		&w.Dirty, &exp, &c, &u)
	if err != nil {
		return w, notFound(err)
	}
	w.TokenExpires, w.CreatedAt, w.UpdatedAt = time.Unix(exp, 0), time.Unix(c, 0), time.Unix(u, 0)
	return w, nil
}

// CreateWorkspaceRequest queues a request for a runner.
func (s *Store) CreateWorkspaceRequest(ctx context.Context, w WorkspaceRequest) (WorkspaceRequest, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO workspace_requests (runner_id, person_id, repo_id, branch, preset, state,
		created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`, w.RunnerID, w.PersonID, w.RepoID, w.Branch, w.Preset, t, t)
	if err != nil {
		return w, err
	}
	id, _ := res.LastInsertId()
	return s.WorkspaceRequest(ctx, id)
}

func (s *Store) WorkspaceRequest(ctx context.Context, id int64) (WorkspaceRequest, error) {
	return scanWorkspaceRequest(s.db.QueryRowContext(ctx, `SELECT `+wsReqCols+` FROM workspace_requests WHERE id=?`, id))
}

// TakeWorkspaceRequest hands the oldest pending request for a runner to it,
// storing the hash of the token that comes with it. Requests older than
// maxAge expire instead.
func (s *Store) TakeWorkspaceRequest(ctx context.Context, runnerID int64, tokenHash string, ttl, maxAge time.Duration) (WorkspaceRequest, error) {
	t := now()
	if _, err := s.db.ExecContext(ctx, `UPDATE workspace_requests SET state='expired', message='The device didn''t pick this up in time.',
		updated_at=? WHERE runner_id=? AND state='pending' AND created_at < ?`, t, runnerID, time.Now().Add(-maxAge).Unix()); err != nil {
		return WorkspaceRequest{}, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `UPDATE workspace_requests SET state='delivered', token_hash=?, token_expires=?, updated_at=?
		WHERE id = (SELECT id FROM workspace_requests WHERE runner_id=? AND state='pending' ORDER BY id LIMIT 1)
		RETURNING id`, tokenHash, time.Now().Add(ttl).Unix(), t, runnerID).Scan(&id)
	if err != nil {
		return WorkspaceRequest{}, notFound(err)
	}
	return s.WorkspaceRequest(ctx, id)
}

// UpdateWorkspaceRequest records what the runner reported. A final state
// revokes the request's token.
func (s *Store) UpdateWorkspaceRequest(ctx context.Context, id int64, state, message, path string, dirty, final bool) error {
	q := `UPDATE workspace_requests SET state=?, message=?, path=?, dirty=?, updated_at=? WHERE id=?`
	if final {
		q = `UPDATE workspace_requests SET state=?, message=?, path=?, dirty=?, updated_at=?, token_hash='', token_expires=0 WHERE id=?`
	}
	_, err := s.db.ExecContext(ctx, q, state, message, path, dirty, now(), id)
	return err
}

// WorkspaceRequestByToken finds the request a live token belongs to.
func (s *Store) WorkspaceRequestByToken(ctx context.Context, tokenHash string) (WorkspaceRequest, error) {
	if tokenHash == "" {
		return WorkspaceRequest{}, ErrNotFound
	}
	var stored string
	var id, exp int64
	err := s.db.QueryRowContext(ctx, `SELECT id, token_hash, token_expires FROM workspace_requests WHERE token_hash=?`, tokenHash).
		Scan(&id, &stored, &exp)
	if err != nil {
		return WorkspaceRequest{}, notFound(err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(tokenHash)) != 1 || exp <= now() {
		return WorkspaceRequest{}, ErrNotFound
	}
	return s.WorkspaceRequest(ctx, id)
}

// RecentWorkspaceRequests lists a runner's latest requests.
func (s *Store) RecentWorkspaceRequests(ctx context.Context, runnerID int64, limit int) ([]WorkspaceRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+wsReqCols+` FROM workspace_requests WHERE runner_id=? ORDER BY id DESC LIMIT ?`, runnerID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceRequest
	for rows.Next() {
		w, err := scanWorkspaceRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---------- devices ----------

// WorkspaceDevice is what a runner last said about its workspaces setup.
type WorkspaceDevice struct {
	RunnerID int64
	Presets  []string // names of the runner's open presets
	Root     string   // the workspaces folder on the device
	SSH      bool     // the device sets remotes to SSH
	SeenAt   time.Time
}

func (s *Store) SaveWorkspaceDevice(ctx context.Context, d WorkspaceDevice) error {
	b, _ := json.Marshal(nonNilStrings(d.Presets))
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspace_devices (runner_id, presets, root, ssh, seen_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(runner_id) DO UPDATE SET presets=excluded.presets, root=excluded.root, ssh=excluded.ssh, seen_at=excluded.seen_at`,
		d.RunnerID, string(b), d.Root, d.SSH, now())
	return err
}

func (s *Store) WorkspaceDevice(ctx context.Context, runnerID int64) (WorkspaceDevice, error) {
	d := WorkspaceDevice{RunnerID: runnerID}
	var presets string
	var seen int64
	err := s.db.QueryRowContext(ctx, `SELECT presets, root, ssh, seen_at FROM workspace_devices WHERE runner_id=?`, runnerID).
		Scan(&presets, &d.Root, &d.SSH, &seen)
	if err != nil {
		return d, notFound(err)
	}
	_ = json.Unmarshal([]byte(presets), &d.Presets)
	d.SeenAt = time.Unix(seen, 0)
	return d, nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---------- workspaces on a device ----------

type Workspace struct {
	ID         int64
	RunnerID   int64
	OwnerName  string
	RepoName   string
	Path       string
	Branch     string
	Head       string
	Dirty      bool
	LastOpened time.Time // zero if never opened from Atelier
	UpdatedAt  time.Time
}

// ReplaceWorkspaces stores the full list a runner reported.
func (s *Store) ReplaceWorkspaces(ctx context.Context, runnerID int64, list []Workspace) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE runner_id=?`, runnerID); err != nil {
		return err
	}
	t := now()
	for _, w := range list {
		var opened int64
		if !w.LastOpened.IsZero() {
			opened = w.LastOpened.Unix()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces (runner_id, owner_name, repo_name, path, branch, head, dirty, last_opened, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(runner_id, path) DO NOTHING`,
			runnerID, w.OwnerName, w.RepoName, w.Path, w.Branch, w.Head, w.Dirty, opened, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Workspaces lists a runner's workspaces, most recently opened first.
func (s *Store) Workspaces(ctx context.Context, runnerID int64) ([]Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, runner_id, owner_name, repo_name, path, branch, head, dirty, last_opened, updated_at
		FROM workspaces WHERE runner_id=? ORDER BY last_opened DESC, owner_name, repo_name`, runnerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		var opened, u int64
		if err := rows.Scan(&w.ID, &w.RunnerID, &w.OwnerName, &w.RepoName, &w.Path, &w.Branch, &w.Head, &w.Dirty, &opened, &u); err != nil {
			return nil, err
		}
		if opened > 0 {
			w.LastOpened = time.Unix(opened, 0)
		}
		w.UpdatedAt = time.Unix(u, 0)
		out = append(out, w)
	}
	return out, rows.Err()
}
