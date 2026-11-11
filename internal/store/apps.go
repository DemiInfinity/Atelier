package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Apps and bots: webhooks and their delivery log, bot accounts with scoped
// tokens, commit statuses / check runs posted through the GitHub-compatible
// API, and the per-repository Tether review switch. The logic lives in
// internal/web/apps*.go.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS apps_webhooks (
  id               INTEGER PRIMARY KEY,
  owner_id         INTEGER REFERENCES owners(id) ON DELETE CASCADE,
  repo_id          INTEGER REFERENCES repos(id) ON DELETE CASCADE,
  url              TEXT NOT NULL,
  secret_ct        BLOB,
  content_type     TEXT NOT NULL DEFAULT 'json',
  events           TEXT NOT NULL DEFAULT 'push',
  active           INTEGER NOT NULL DEFAULT 1,
  allow_local      INTEGER NOT NULL DEFAULT 0,
  created_by       TEXT NOT NULL DEFAULT '',
  person_id        INTEGER,
  last_status      TEXT NOT NULL DEFAULT '',
  last_delivery_at INTEGER,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  CHECK ((owner_id IS NULL) <> (repo_id IS NULL))
);
CREATE INDEX IF NOT EXISTS apps_webhooks_repo ON apps_webhooks (repo_id);
CREATE INDEX IF NOT EXISTS apps_webhooks_owner ON apps_webhooks (owner_id);

CREATE TABLE IF NOT EXISTS apps_deliveries (
  id           INTEGER PRIMARY KEY,
  hook_id      INTEGER NOT NULL REFERENCES apps_webhooks(id) ON DELETE CASCADE,
  guid         TEXT NOT NULL,
  event        TEXT NOT NULL,
  action       TEXT NOT NULL DEFAULT '',
  redelivery   INTEGER NOT NULL DEFAULT 0,
  status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','ok','failed')),
  attempts     INTEGER NOT NULL DEFAULT 0,
  next_at      INTEGER NOT NULL,
  payload      TEXT NOT NULL,
  req_headers  TEXT NOT NULL DEFAULT '',
  resp_status  INTEGER NOT NULL DEFAULT 0,
  resp_headers TEXT NOT NULL DEFAULT '',
  resp_body    TEXT NOT NULL DEFAULT '',
  error        TEXT NOT NULL DEFAULT '',
  duration_ms  INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  delivered_at INTEGER
);
CREATE INDEX IF NOT EXISTS apps_deliveries_due ON apps_deliveries (status, next_at);
CREATE INDEX IF NOT EXISTS apps_deliveries_hook ON apps_deliveries (hook_id, id);

CREATE TABLE IF NOT EXISTS apps_bots (
  id          INTEGER PRIMARY KEY,
  person_id   INTEGER REFERENCES owners(id) ON DELETE CASCADE,
  name        TEXT NOT NULL UNIQUE COLLATE NOCASE,
  initial     TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  token_id    INTEGER,
  can_comment INTEGER NOT NULL DEFAULT 1,
  can_suggest INTEGER NOT NULL DEFAULT 1,
  can_checks  INTEGER NOT NULL DEFAULT 1,
  can_push    INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS apps_bot_repos (
  bot_id  INTEGER NOT NULL REFERENCES apps_bots(id) ON DELETE CASCADE,
  repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  PRIMARY KEY (bot_id, repo_id)
);

CREATE TABLE IF NOT EXISTS apps_statuses (
  id           INTEGER PRIMARY KEY,
  repo_id      INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  sha          TEXT NOT NULL,
  context      TEXT NOT NULL,
  kind         TEXT NOT NULL DEFAULT 'status' CHECK (kind IN ('status','check_run')),
  state        TEXT NOT NULL,
  run_status   TEXT NOT NULL DEFAULT '',
  conclusion   TEXT NOT NULL DEFAULT '',
  description  TEXT NOT NULL DEFAULT '',
  target_url   TEXT NOT NULL DEFAULT '',
  creator_kind TEXT NOT NULL DEFAULT '',
  creator_name TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  UNIQUE (repo_id, sha, context)
);

CREATE TABLE IF NOT EXISTS apps_repo_settings (
  repo_id       INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  tether_review INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS apps_tether_mirrors (
  pull_id    INTEGER PRIMARY KEY REFERENCES pulls(id) ON DELETE CASCADE,
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  gh_number  INTEGER NOT NULL,
  url        TEXT NOT NULL DEFAULT '',
  head_sha   TEXT NOT NULL DEFAULT '',
  closed     INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS apps_tether_bots (
  repo_id  INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  name     TEXT NOT NULL COLLATE NOCASE,
  comments INTEGER NOT NULL DEFAULT 0,
  last_at  INTEGER NOT NULL,
  PRIMARY KEY (repo_id, name)
);
CREATE TABLE IF NOT EXISTS apps_ref_snapshots (
  repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  ref     TEXT NOT NULL,
  sha     TEXT NOT NULL,
  PRIMARY KEY (repo_id, ref)
);`)
}

// ---------- webhooks ----------

// Webhook events Atelier can send (GitHub names).
var WebhookEvents = []string{"push", "pull_request", "issues", "issue_comment", "release", "workflow_run", "ping"}

type Webhook struct {
	ID             int64
	OwnerID        int64 // account-wide hook (0 for a repository hook)
	RepoID         int64 // repository hook (0 for an account hook)
	URL            string
	HasSecret      bool
	ContentType    string
	Events         []string
	Active         bool
	AllowLocal     bool
	CreatedBy      string
	PersonID       int64
	LastStatus     string // ok, failed or ""
	LastDeliveryAt *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Wants reports whether the hook subscribes to event (ping always goes).
func (h Webhook) Wants(event string) bool {
	if event == "ping" {
		return true
	}
	for _, e := range h.Events {
		if e == event || e == "*" {
			return true
		}
	}
	return false
}

const webhookCols = `id, COALESCE(owner_id,0), COALESCE(repo_id,0), url, secret_ct IS NOT NULL AND length(secret_ct) > 0,
content_type, events, active, allow_local, created_by, COALESCE(person_id,0), last_status, last_delivery_at, created_at, updated_at`

func scanWebhook(row interface{ Scan(...any) error }) (Webhook, error) {
	var h Webhook
	var events string
	var last sql.NullInt64
	var c, u int64
	err := row.Scan(&h.ID, &h.OwnerID, &h.RepoID, &h.URL, &h.HasSecret, &h.ContentType, &events, &h.Active, &h.AllowLocal,
		&h.CreatedBy, &h.PersonID, &h.LastStatus, &last, &c, &u)
	h.Events = strings.Fields(events)
	h.CreatedAt, h.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	if last.Valid {
		t := time.Unix(last.Int64, 0)
		h.LastDeliveryAt = &t
	}
	return h, err
}

func (s *Store) scanWebhooks(rows *sql.Rows, err error) ([]Webhook, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		h, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SaveWebhook creates (ID 0) or updates a webhook. The secret is set separately.
func (s *Store) SaveWebhook(ctx context.Context, h Webhook) (Webhook, error) {
	t := now()
	var owner, repo any
	if h.RepoID != 0 {
		repo = h.RepoID
	} else {
		owner = h.OwnerID
	}
	ct := h.ContentType
	if ct == "" {
		ct = "json"
	}
	events := strings.Join(h.Events, " ")
	if h.ID == 0 {
		var person any
		if h.PersonID != 0 {
			person = h.PersonID
		}
		res, err := s.db.ExecContext(ctx, `INSERT INTO apps_webhooks (owner_id, repo_id, url, content_type, events, active, allow_local,
			created_by, person_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			owner, repo, h.URL, ct, events, h.Active, h.AllowLocal, h.CreatedBy, person, t, t)
		if err != nil {
			return Webhook{}, err
		}
		h.ID, _ = res.LastInsertId()
	} else {
		res, err := s.db.ExecContext(ctx, `UPDATE apps_webhooks SET url=?, content_type=?, events=?, active=?, allow_local=?, updated_at=?
			WHERE id=?`, h.URL, ct, events, h.Active, h.AllowLocal, t, h.ID)
		if err := pullExpectOne(res, err); err != nil {
			return Webhook{}, err
		}
	}
	return s.WebhookByID(ctx, h.ID)
}

func webhookAAD(id int64) []byte {
	return []byte("apps.webhook/" + strconv.FormatInt(id, 10) + "/secret")
}

// SetWebhookSecret seals and stores a webhook's secret ("" removes it).
func (s *Store) SetWebhookSecret(ctx context.Context, sealer SecretSealer, id int64, plain string) error {
	var ct []byte
	if plain != "" {
		var err error
		if ct, err = sealer.Seal(webhookAAD(id), []byte(plain)); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE apps_webhooks SET secret_ct=?, updated_at=? WHERE id=?`, ct, now(), id)
	return err
}

// WebhookSecret opens a webhook's secret ("" when none is set).
func (s *Store) WebhookSecret(ctx context.Context, sealer SecretSealer, id int64) (string, error) {
	var ct []byte
	if err := s.db.QueryRowContext(ctx, `SELECT secret_ct FROM apps_webhooks WHERE id=?`, id).Scan(&ct); err != nil {
		return "", notFound(err)
	}
	if len(ct) == 0 {
		return "", nil
	}
	pt, err := sealer.Open(webhookAAD(id), ct)
	return string(pt), err
}

func (s *Store) WebhookByID(ctx context.Context, id int64) (Webhook, error) {
	h, err := scanWebhook(s.db.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM apps_webhooks WHERE id=?`, id))
	return h, notFound(err)
}

func (s *Store) DeleteWebhook(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM apps_webhooks WHERE id=?`, id)
	return err
}

// RepoWebhooks lists a repository's own hooks.
func (s *Store) RepoWebhooks(ctx context.Context, repoID int64) ([]Webhook, error) {
	return s.scanWebhooks(s.db.QueryContext(ctx, `SELECT `+webhookCols+` FROM apps_webhooks WHERE repo_id=? ORDER BY id`, repoID))
}

// AccountWebhooks lists an account's hooks (they fire for all its repositories).
func (s *Store) AccountWebhooks(ctx context.Context, ownerID int64) ([]Webhook, error) {
	return s.scanWebhooks(s.db.QueryContext(ctx, `SELECT `+webhookCols+` FROM apps_webhooks WHERE owner_id=? ORDER BY id`, ownerID))
}

// HooksFor lists the active hooks that hear about a repository: its own and its account's.
func (s *Store) HooksFor(ctx context.Context, repo Repo) ([]Webhook, error) {
	return s.scanWebhooks(s.db.QueryContext(ctx, `SELECT `+webhookCols+` FROM apps_webhooks
		WHERE active=1 AND (repo_id=? OR owner_id=?) ORDER BY id`, repo.ID, repo.OwnerID))
}

// AllWebhooks lists every hook (for the Apps page).
func (s *Store) AllWebhooks(ctx context.Context) ([]Webhook, error) {
	return s.scanWebhooks(s.db.QueryContext(ctx, `SELECT `+webhookCols+` FROM apps_webhooks ORDER BY id`))
}

// ---------- deliveries ----------

type Delivery struct {
	ID          int64
	HookID      int64
	GUID        string
	Event       string
	Action      string
	Redelivery  bool
	Status      string // pending, ok, failed
	Attempts    int
	NextAt      time.Time
	Payload     string
	ReqHeaders  string // JSON object, secrets redacted
	RespStatus  int
	RespHeaders string // JSON object, redacted
	RespBody    string
	Error       string
	DurationMS  int64
	CreatedAt   time.Time
	DeliveredAt *time.Time
}

const deliveryCols = `id, hook_id, guid, event, action, redelivery, status, attempts, next_at, payload, req_headers,
resp_status, resp_headers, resp_body, error, duration_ms, created_at, delivered_at`

func scanDelivery(row interface{ Scan(...any) error }) (Delivery, error) {
	var d Delivery
	var next, c int64
	var del sql.NullInt64
	err := row.Scan(&d.ID, &d.HookID, &d.GUID, &d.Event, &d.Action, &d.Redelivery, &d.Status, &d.Attempts, &next, &d.Payload,
		&d.ReqHeaders, &d.RespStatus, &d.RespHeaders, &d.RespBody, &d.Error, &d.DurationMS, &c, &del)
	d.NextAt, d.CreatedAt = time.Unix(next, 0), time.Unix(c, 0)
	if del.Valid {
		t := time.Unix(del.Int64, 0)
		d.DeliveredAt = &t
	}
	return d, err
}

// QueueDelivery adds a delivery to send now.
func (s *Store) QueueDelivery(ctx context.Context, d Delivery) (Delivery, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO apps_deliveries (hook_id, guid, event, action, redelivery, status, next_at, payload, created_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?)`, d.HookID, d.GUID, d.Event, d.Action, d.Redelivery, t, d.Payload, t)
	if err != nil {
		return Delivery{}, err
	}
	id, _ := res.LastInsertId()
	return s.DeliveryByID(ctx, id)
}

func (s *Store) DeliveryByID(ctx context.Context, id int64) (Delivery, error) {
	d, err := scanDelivery(s.db.QueryRowContext(ctx, `SELECT `+deliveryCols+` FROM apps_deliveries WHERE id=?`, id))
	return d, notFound(err)
}

// DueDeliveries claims up to limit pending deliveries whose time has come by
// pushing their next attempt lease seconds into the future.
func (s *Store) DueDeliveries(ctx context.Context, at time.Time, limit int, lease time.Duration) ([]Delivery, error) {
	var out []Delivery
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+deliveryCols+` FROM apps_deliveries WHERE status='pending' AND next_at<=?
			ORDER BY next_at, id LIMIT ?`, at.Unix(), limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			d, err := scanDelivery(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range out {
			if _, err := tx.ExecContext(ctx, `UPDATE apps_deliveries SET next_at=? WHERE id=?`, at.Add(lease).Unix(), d.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// DeliveryResult is what one attempt produced.
type DeliveryResult struct {
	Status      string // ok, failed, pending (retry at NextAt)
	NextAt      time.Time
	ReqHeaders  string
	RespStatus  int
	RespHeaders string
	RespBody    string
	Error       string
	DurationMS  int64
}

// FinishAttempt records an attempt and updates the hook's last status.
func (s *Store) FinishAttempt(ctx context.Context, d Delivery, r DeliveryResult) error {
	t := now()
	next := r.NextAt.Unix()
	if r.NextAt.IsZero() {
		next = t
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE apps_deliveries SET status=?, attempts=attempts+1, next_at=?, req_headers=?, resp_status=?,
			resp_headers=?, resp_body=?, error=?, duration_ms=?, delivered_at=? WHERE id=?`,
			r.Status, next, r.ReqHeaders, r.RespStatus, r.RespHeaders, r.RespBody, r.Error, r.DurationMS, t, d.ID); err != nil {
			return err
		}
		last := "failed"
		if r.Status == "ok" {
			last = "ok"
		}
		_, err := tx.ExecContext(ctx, `UPDATE apps_webhooks SET last_status=?, last_delivery_at=? WHERE id=?`, last, t, d.HookID)
		return err
	})
}

// Deliveries lists a hook's newest deliveries.
func (s *Store) Deliveries(ctx context.Context, hookID int64, limit int) ([]Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM apps_deliveries WHERE hook_id=? ORDER BY id DESC LIMIT ?`, hookID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PruneDeliveries keeps the newest keep deliveries per hook.
func (s *Store) PruneDeliveries(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM apps_deliveries WHERE status<>'pending' AND id NOT IN (
		SELECT id FROM (SELECT id, ROW_NUMBER() OVER (PARTITION BY hook_id ORDER BY id DESC) AS rn FROM apps_deliveries) WHERE rn<=?)`, keep)
	return err
}

// ---------- bots ----------

type Bot struct {
	ID          int64
	PersonID    int64
	Name        string
	Initial     string
	Description string
	TokenID     int64
	CanComment  bool
	CanSuggest  bool
	CanChecks   bool
	CanPush     bool
	RepoIDs     []int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Avatar is the bot's one-letter avatar.
func (b Bot) Avatar() string {
	if b.Initial != "" {
		return b.Initial
	}
	if b.Name == "" {
		return "?"
	}
	return strings.ToUpper(b.Name[:1])
}

// HasRepo reports whether the bot may work in a repository.
func (b Bot) HasRepo(id int64) bool {
	for _, r := range b.RepoIDs {
		if r == id {
			return true
		}
	}
	return false
}

const botCols = `id, COALESCE(person_id,0), name, initial, description, COALESCE(token_id,0), can_comment, can_suggest, can_checks, can_push, created_at, updated_at`

func scanBot(row interface{ Scan(...any) error }) (Bot, error) {
	var b Bot
	var c, u int64
	err := row.Scan(&b.ID, &b.PersonID, &b.Name, &b.Initial, &b.Description, &b.TokenID, &b.CanComment, &b.CanSuggest, &b.CanChecks,
		&b.CanPush, &c, &u)
	b.CreatedAt, b.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return b, err
}

func (s *Store) botRepos(ctx context.Context, b *Bot) error {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id FROM apps_bot_repos WHERE bot_id=? ORDER BY repo_id`, b.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	b.RepoIDs = nil
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		b.RepoIDs = append(b.RepoIDs, id)
	}
	return rows.Err()
}

func (s *Store) oneBot(ctx context.Context, where string, arg any) (Bot, error) {
	b, err := scanBot(s.db.QueryRowContext(ctx, `SELECT `+botCols+` FROM apps_bots WHERE `+where, arg))
	if err != nil {
		return Bot{}, notFound(err)
	}
	return b, s.botRepos(ctx, &b)
}

func (s *Store) BotByID(ctx context.Context, id int64) (Bot, error) { return s.oneBot(ctx, "id=?", id) }
func (s *Store) BotByName(ctx context.Context, n string) (Bot, error) {
	return s.oneBot(ctx, "name=?", n)
}

// BotByToken finds the bot an access token belongs to.
func (s *Store) BotByToken(ctx context.Context, tokenID int64) (Bot, error) {
	if tokenID == 0 {
		return Bot{}, ErrNotFound
	}
	return s.oneBot(ctx, "token_id=?", tokenID)
}

// Bots lists a person's bots (personID 0: everyone's).
func (s *Store) Bots(ctx context.Context, personID int64) ([]Bot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+botCols+` FROM apps_bots WHERE ?=0 OR person_id=? ORDER BY name`, personID, personID)
	if err != nil {
		return nil, err
	}
	var out []Bot
	for rows.Next() {
		b, err := scanBot(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.botRepos(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ErrBotNameTaken means another bot already has the name.
var ErrBotNameTaken = errors.New("a bot with that name already exists")

// SaveBot creates (ID 0) or updates a bot and its repositories.
func (s *Store) SaveBot(ctx context.Context, b Bot) (Bot, error) {
	t := now()
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if b.ID == 0 {
			res, err := tx.ExecContext(ctx, `INSERT INTO apps_bots (person_id, name, initial, description, can_comment, can_suggest,
				can_checks, can_push, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				b.PersonID, b.Name, b.Initial, b.Description, b.CanComment, b.CanSuggest, b.CanChecks, b.CanPush, t, t)
			if err != nil {
				if strings.Contains(err.Error(), "UNIQUE") {
					return ErrBotNameTaken
				}
				return err
			}
			b.ID, _ = res.LastInsertId()
		} else {
			res, err := tx.ExecContext(ctx, `UPDATE apps_bots SET initial=?, description=?, can_comment=?, can_suggest=?, can_checks=?,
				can_push=?, updated_at=? WHERE id=?`, b.Initial, b.Description, b.CanComment, b.CanSuggest, b.CanChecks, b.CanPush, t, b.ID)
			if err := pullExpectOne(res, err); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM apps_bot_repos WHERE bot_id=?`, b.ID); err != nil {
				return err
			}
		}
		for _, id := range b.RepoIDs {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO apps_bot_repos (bot_id, repo_id) VALUES (?, ?)`, b.ID, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Bot{}, err
	}
	return s.BotByID(ctx, b.ID)
}

// SetBotToken links a bot to its (new) access token, revoking the old one.
func (s *Store) SetBotToken(ctx context.Context, botID int64, plain string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var old sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT token_id FROM apps_bots WHERE id=?`, botID).Scan(&old); err != nil {
			return notFound(err)
		}
		var tid int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tokens WHERE token_hash=?`, Hash(plain)).Scan(&tid); err != nil {
			return notFound(err)
		}
		if old.Valid && old.Int64 != tid {
			if _, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE id=?`, old.Int64); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE apps_bots SET token_id=?, updated_at=? WHERE id=?`, tid, now(), botID)
		return err
	})
}

// DeleteBot removes a bot and revokes its token.
func (s *Store) DeleteBot(ctx context.Context, id int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE id=(SELECT token_id FROM apps_bots WHERE id=?)`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM apps_bots WHERE id=?`, id)
		return err
	})
}

// ---------- statuses and check runs ----------

type CommitStatus struct {
	ID          int64
	RepoID      int64
	SHA         string
	Context     string
	Kind        string // status or check_run
	State       string // success, failure, error, pending
	RunStatus   string // check runs: queued, in_progress, completed
	Conclusion  string // check runs
	Description string
	TargetURL   string
	CreatorKind string
	CreatorName string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const statusCols = `id, repo_id, sha, context, kind, state, run_status, conclusion, description, target_url, creator_kind, creator_name, created_at, updated_at`

func scanStatus(row interface{ Scan(...any) error }) (CommitStatus, error) {
	var st CommitStatus
	var c, u int64
	err := row.Scan(&st.ID, &st.RepoID, &st.SHA, &st.Context, &st.Kind, &st.State, &st.RunStatus, &st.Conclusion, &st.Description,
		&st.TargetURL, &st.CreatorKind, &st.CreatorName, &c, &u)
	st.CreatedAt, st.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return st, err
}

// SetCommitStatus creates or replaces the status with the same context on a commit.
func (s *Store) SetCommitStatus(ctx context.Context, st CommitStatus) (CommitStatus, error) {
	t := now()
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO apps_statuses (repo_id, sha, context, kind, state, run_status, conclusion, description,
		target_url, creator_kind, creator_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, sha, context) DO UPDATE SET kind=excluded.kind, state=excluded.state, run_status=excluded.run_status,
		conclusion=excluded.conclusion, description=excluded.description, target_url=excluded.target_url,
		creator_kind=excluded.creator_kind, creator_name=excluded.creator_name, updated_at=excluded.updated_at RETURNING id`,
		st.RepoID, st.SHA, st.Context, st.Kind, st.State, st.RunStatus, st.Conclusion, st.Description, st.TargetURL,
		st.CreatorKind, st.CreatorName, t, t).Scan(&id)
	if err != nil {
		return CommitStatus{}, err
	}
	return s.CommitStatusByID(ctx, st.RepoID, id)
}

func (s *Store) CommitStatusByID(ctx context.Context, repoID, id int64) (CommitStatus, error) {
	st, err := scanStatus(s.db.QueryRowContext(ctx, `SELECT `+statusCols+` FROM apps_statuses WHERE repo_id=? AND id=?`, repoID, id))
	return st, notFound(err)
}

// CommitStatuses lists the statuses and check runs on a commit.
func (s *Store) CommitStatuses(ctx context.Context, repoID int64, sha string) ([]CommitStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+statusCols+` FROM apps_statuses WHERE repo_id=? AND sha=? ORDER BY context`, repoID, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitStatus
	for rows.Next() {
		st, err := scanStatus(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---------- Tether review ----------

func (s *Store) TetherReview(ctx context.Context, repoID int64) bool {
	var on bool
	_ = s.db.QueryRowContext(ctx, `SELECT tether_review FROM apps_repo_settings WHERE repo_id=?`, repoID).Scan(&on)
	return on
}

func (s *Store) SetTetherReview(ctx context.Context, repoID int64, on bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO apps_repo_settings (repo_id, tether_review) VALUES (?, ?)
		ON CONFLICT(repo_id) DO UPDATE SET tether_review=excluded.tether_review`, repoID, on)
	return err
}

// TetherReviewRepos lists the repositories with GitHub review switched on.
func (s *Store) TetherReviewRepos(ctx context.Context) ([]int64, error) {
	return s.int64s(ctx, `SELECT repo_id FROM apps_repo_settings WHERE tether_review=1 ORDER BY repo_id`)
}

type TetherMirror struct {
	PullID    int64
	RepoID    int64
	GHNumber  int
	URL       string
	HeadSHA   string
	Closed    bool
	UpdatedAt time.Time
}

func (s *Store) TetherMirror(ctx context.Context, pullID int64) (TetherMirror, error) {
	var m TetherMirror
	var u int64
	err := s.db.QueryRowContext(ctx, `SELECT pull_id, repo_id, gh_number, url, head_sha, closed, updated_at FROM apps_tether_mirrors
		WHERE pull_id=?`, pullID).Scan(&m.PullID, &m.RepoID, &m.GHNumber, &m.URL, &m.HeadSHA, &m.Closed, &u)
	m.UpdatedAt = time.Unix(u, 0)
	return m, notFound(err)
}

func (s *Store) SaveTetherMirror(ctx context.Context, m TetherMirror) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO apps_tether_mirrors (pull_id, repo_id, gh_number, url, head_sha, closed, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(pull_id) DO UPDATE SET gh_number=excluded.gh_number, url=excluded.url,
		head_sha=excluded.head_sha, closed=excluded.closed, updated_at=excluded.updated_at`,
		m.PullID, m.RepoID, m.GHNumber, m.URL, m.HeadSHA, m.Closed, now())
	return err
}

// NoteTetherBot records that a GitHub bot commented through Tether.
func (s *Store) NoteTetherBot(ctx context.Context, repoID int64, name string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO apps_tether_bots (repo_id, name, comments, last_at) VALUES (?, ?, 1, ?)
		ON CONFLICT(repo_id, name) DO UPDATE SET comments=comments+1, last_at=excluded.last_at`, repoID, name, now())
	return err
}

// TetherBots lists the GitHub bots seen through Tether, newest first.
func (s *Store) TetherBots(ctx context.Context) ([]BotAuthor, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, repo_id, comments, last_at FROM apps_tether_bots ORDER BY last_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BotAuthor
	for rows.Next() {
		b := BotAuthor{Kind: "tether"}
		var last int64
		if err := rows.Scan(&b.Name, &b.RepoID, &b.Count, &last); err != nil {
			return nil, err
		}
		b.Last = time.Unix(last, 0)
		out = append(out, b)
	}
	return out, rows.Err()
}

// TetherMirrors lists a repository's mirrored pull requests.
func (s *Store) TetherMirrors(ctx context.Context, repoID int64) ([]TetherMirror, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pull_id, repo_id, gh_number, url, head_sha, closed, updated_at FROM apps_tether_mirrors
		WHERE repo_id=? ORDER BY updated_at DESC LIMIT 50`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TetherMirror
	for rows.Next() {
		var m TetherMirror
		var u int64
		if err := rows.Scan(&m.PullID, &m.RepoID, &m.GHNumber, &m.URL, &m.HeadSHA, &m.Closed, &u); err != nil {
			return nil, err
		}
		m.UpdatedAt = time.Unix(u, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// BotAuthor is someone who wrote on pull requests or issues (for the Installed list).
type BotAuthor struct {
	Kind, Name string
	RepoID     int64
	Count      int
	Last       time.Time
}

// BotAuthors lists distinct authors of the given kinds across comments and notes.
func (s *Store) BotAuthors(ctx context.Context, kinds ...string) ([]BotAuthor, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	var args []any
	for i := 0; i < 3; i++ {
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, name, repo_id, COUNT(*), MAX(at) FROM (
		SELECT c.author_kind AS kind, c.author_name AS name, p.repo_id AS repo_id, c.created_at AS at FROM pull_comments c JOIN pulls p ON p.id=c.pull_id WHERE c.author_kind IN (`+ph+`)
		UNION ALL SELECT n.author_kind, n.author_name, p.repo_id, n.created_at FROM pull_notes n JOIN pulls p ON p.id=n.pull_id WHERE n.author_kind IN (`+ph+`)
		UNION ALL SELECT c.author_kind, c.author_name, i.repo_id, c.created_at FROM issue_comments c JOIN issues i ON i.id=c.issue_id WHERE c.author_kind IN (`+ph+`)
	) GROUP BY kind, name COLLATE NOCASE, repo_id ORDER BY MAX(at) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BotAuthor
	for rows.Next() {
		var b BotAuthor
		var last int64
		if err := rows.Scan(&b.Kind, &b.Name, &b.RepoID, &b.Count, &last); err != nil {
			return nil, err
		}
		b.Last = time.Unix(last, 0)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---------- ref snapshots (push detection) ----------

// RefSnapshot returns the refs last seen for a repository (nil: never scanned).
func (s *Store) RefSnapshot(ctx context.Context, repoID int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ref, sha FROM apps_ref_snapshots WHERE repo_id=?`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out map[string]string
	for rows.Next() {
		var ref, sha string
		if err := rows.Scan(&ref, &sha); err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]string{}
		}
		out[ref] = sha
	}
	return out, rows.Err()
}

// SaveRefSnapshot replaces a repository's ref snapshot. An empty map is
// stored as a marker row so the repository counts as scanned.
func (s *Store) SaveRefSnapshot(ctx context.Context, repoID int64, refs map[string]string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM apps_ref_snapshots WHERE repo_id=?`, repoID); err != nil {
			return err
		}
		if len(refs) == 0 {
			refs = map[string]string{"": ""}
		}
		for ref, sha := range refs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO apps_ref_snapshots (repo_id, ref, sha) VALUES (?, ?, ?)`, repoID, ref, sha); err != nil {
				return err
			}
		}
		return nil
	})
}

// ReposWithHooks lists repositories that have (or whose account has) an active webhook.
func (s *Store) ReposWithHooks(ctx context.Context) ([]int64, error) {
	return s.int64s(ctx, `SELECT DISTINCT r.id FROM repos r JOIN apps_webhooks h ON h.active=1 AND (h.repo_id=r.id OR h.owner_id=r.owner_id)
		ORDER BY r.id`)
}

// RepoByID finds a repository by id.
func (s *Store) RepoByIDApps(ctx context.Context, id int64) (Repo, error) {
	rows, err := s.db.QueryContext(ctx, repoSelect+` WHERE r.id=?`, id)
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

func (s *Store) int64s(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PullByID finds a pull request by its row id.
func (s *Store) PullByIDApps(ctx context.Context, id int64) (Pull, error) {
	var repoID, number int64
	if err := s.db.QueryRowContext(ctx, `SELECT repo_id, number FROM pulls WHERE id=?`, id).Scan(&repoID, &number); err != nil {
		return Pull{}, notFound(err)
	}
	return s.PullByNumber(ctx, repoID, number)
}
