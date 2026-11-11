package store

// Atelier Connect (internal/connect, web/connect*.go): this studio's
// federation state.
//
//   - connect_settings: one row, created switched off. Nothing federates
//     until the owner turns it on.
//   - connect_actors: a cache of other studios' actor documents (keys and
//     inboxes), refreshed when stale.
//   - connect_following / connect_followers: follows out and in, per local
//     actor (an account name). Followers can be pending (manual approval).
//   - connect_rules: blocked and allowed studios, by host.
//   - connect_feed: activities from people you follow, shown on Home.
//   - connect_deliveries: the outgoing queue, which doubles as the delivery log.
//   - connect_stars / connect_starred: stars from other studios on your
//     public repositories, and your stars on theirs.
//   - connect_seen: replay protection (signatures and activity ids).
//   - connect_announced: public repositories already announced.
//   - connect_badges: actor addresses read from GitHub badges.
//   - connect_offers: branches offered by other studios (remote pull requests).

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS connect_settings (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  enabled        INTEGER NOT NULL DEFAULT 0,
  auto_accept    INTEGER NOT NULL DEFAULT 0,
  allowlist_only INTEGER NOT NULL DEFAULT 0,
  expose_tunnel  INTEGER NOT NULL DEFAULT 0,
  changed_at     INTEGER NOT NULL DEFAULT 0,
  changed_by     TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO connect_settings (id) VALUES (1);
CREATE TABLE IF NOT EXISTS connect_actors (
  id            INTEGER PRIMARY KEY,
  uri           TEXT NOT NULL UNIQUE,
  host          TEXT NOT NULL,
  type          TEXT NOT NULL DEFAULT '',
  username      TEXT NOT NULL DEFAULT '',
  name          TEXT NOT NULL DEFAULT '',
  summary       TEXT NOT NULL DEFAULT '',
  url           TEXT NOT NULL DEFAULT '',
  inbox         TEXT NOT NULL,
  shared_inbox  TEXT NOT NULL DEFAULT '',
  outbox        TEXT NOT NULL DEFAULT '',
  key_id        TEXT NOT NULL DEFAULT '',
  key_pem       TEXT NOT NULL DEFAULT '',
  clone_uri     TEXT NOT NULL DEFAULT '',
  attributed_to TEXT NOT NULL DEFAULT '',
  fetched_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS connect_actors_key ON connect_actors(key_id);
CREATE TABLE IF NOT EXISTS connect_following (
  id          INTEGER PRIMARY KEY,
  local       TEXT NOT NULL COLLATE NOCASE,
  actor_uri   TEXT NOT NULL,
  state       TEXT NOT NULL CHECK (state IN ('pending','accepted','rejected')),
  activity_id TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  UNIQUE (local, actor_uri)
);
CREATE TABLE IF NOT EXISTS connect_followers (
  id          INTEGER PRIMARY KEY,
  local       TEXT NOT NULL COLLATE NOCASE,
  actor_uri   TEXT NOT NULL,
  state       TEXT NOT NULL CHECK (state IN ('pending','accepted')),
  activity_id TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  UNIQUE (local, actor_uri)
);
CREATE TABLE IF NOT EXISTS connect_rules (
  host       TEXT PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind IN ('block','allow')),
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS connect_feed (
  id          INTEGER PRIMARY KEY,
  activity_id TEXT NOT NULL UNIQUE,
  actor_uri   TEXT NOT NULL,
  kind        TEXT NOT NULL,
  text        TEXT NOT NULL,
  detail      TEXT NOT NULL DEFAULT '',
  link        TEXT NOT NULL DEFAULT '',
  object_uri  TEXT NOT NULL DEFAULT '',
  object_name TEXT NOT NULL DEFAULT '',
  published   INTEGER NOT NULL,
  received_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS connect_feed_published ON connect_feed(published);
CREATE TABLE IF NOT EXISTS connect_deliveries (
  id          INTEGER PRIMARY KEY,
  inbox       TEXT NOT NULL,
  host        TEXT NOT NULL,
  activity_id TEXT NOT NULL,
  kind        TEXT NOT NULL,
  body        BLOB NOT NULL,
  repo_id     INTEGER NOT NULL DEFAULT 0,
  status      TEXT NOT NULL CHECK (status IN ('queued','sent','failed','dropped')),
  attempts    INTEGER NOT NULL DEFAULT 0,
  next_at     INTEGER NOT NULL,
  last_status INTEGER NOT NULL DEFAULT 0,
  last_error  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS connect_deliveries_due ON connect_deliveries(status, next_at);
CREATE TABLE IF NOT EXISTS connect_stars (
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  actor_uri   TEXT NOT NULL,
  activity_id TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (repo_id, actor_uri)
);
CREATE TABLE IF NOT EXISTS connect_starred (
  object_uri  TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  url         TEXT NOT NULL DEFAULT '',
  inbox       TEXT NOT NULL,
  activity_id TEXT NOT NULL,
  created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS connect_seen (
  key        TEXT PRIMARY KEY,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS connect_announced (
  repo_id INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  at      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS connect_badges (
  login      TEXT PRIMARY KEY COLLATE NOCASE,
  actor      TEXT NOT NULL,
  checked_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS connect_offers (
  id          INTEGER PRIMARY KEY,
  activity_id TEXT NOT NULL UNIQUE,
  actor_uri   TEXT NOT NULL,
  repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  pull_id     INTEGER NOT NULL DEFAULT 0,
  branch      TEXT NOT NULL,
  local_ref   TEXT NOT NULL DEFAULT '',
  clone_uri   TEXT NOT NULL,
  title       TEXT NOT NULL,
  state       TEXT NOT NULL,
  error       TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL
);`)
}

// ---------- settings ----------

// ConnectSettings is the Connect switch and its options.
type ConnectSettings struct {
	Enabled       bool // federation on (also needs a public address)
	AutoAccept    bool // accept follow requests without asking
	AllowlistOnly bool // only studios on the allowlist may interact
	ExposeTunnel  bool // Connect endpoints are reachable through the tunnel
	ChangedAt     time.Time
	ChangedBy     string
}

func (s *Store) ConnectSettings(ctx context.Context) (ConnectSettings, error) {
	var c ConnectSettings
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT enabled, auto_accept, allowlist_only, expose_tunnel, changed_at, changed_by
		FROM connect_settings WHERE id=1`).Scan(&c.Enabled, &c.AutoAccept, &c.AllowlistOnly, &c.ExposeTunnel, &at, &c.ChangedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectSettings{}, nil
	}
	if at > 0 {
		c.ChangedAt = time.Unix(at, 0)
	}
	return c, err
}

// SaveConnectSettings stores every field.
func (s *Store) SaveConnectSettings(ctx context.Context, c ConnectSettings, by string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO connect_settings (id, enabled, auto_accept, allowlist_only, expose_tunnel, changed_at, changed_by)
		VALUES (1, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET enabled=excluded.enabled, auto_accept=excluded.auto_accept,
		allowlist_only=excluded.allowlist_only, expose_tunnel=excluded.expose_tunnel, changed_at=excluded.changed_at, changed_by=excluded.changed_by`,
		c.Enabled, c.AutoAccept, c.AllowlistOnly, c.ExposeTunnel, now(), by)
	return err
}

// ---------- remote actors ----------

// ConnectActor is a cached remote actor.
type ConnectActor struct {
	ID           int64
	URI          string
	Host         string
	Type         string
	Username     string
	Name         string
	Summary      string
	URL          string
	Inbox        string
	SharedInbox  string
	Outbox       string
	KeyID        string
	KeyPEM       string
	CloneURI     string
	AttributedTo string
	FetchedAt    time.Time
}

// Handle is @user@host (or the host).
func (c ConnectActor) Handle() string {
	if c.Username == "" {
		return c.Host
	}
	return "@" + c.Username + "@" + c.Host
}

// Label is the name to show.
func (c ConnectActor) Label() string {
	if c.Name != "" {
		return c.Name
	}
	if c.Username != "" {
		return c.Username
	}
	return c.Host
}

// DeliveryInbox is where to deliver: the shared inbox when there is one.
func (c ConnectActor) DeliveryInbox() string {
	if c.SharedInbox != "" {
		return c.SharedInbox
	}
	return c.Inbox
}

const connectActorCols = `id, uri, host, type, username, name, summary, url, inbox, shared_inbox, outbox, key_id, key_pem,
  clone_uri, attributed_to, fetched_at`

func scanConnectActor(row interface{ Scan(...any) error }) (ConnectActor, error) {
	var c ConnectActor
	var at int64
	err := row.Scan(&c.ID, &c.URI, &c.Host, &c.Type, &c.Username, &c.Name, &c.Summary, &c.URL, &c.Inbox, &c.SharedInbox,
		&c.Outbox, &c.KeyID, &c.KeyPEM, &c.CloneURI, &c.AttributedTo, &at)
	c.FetchedAt = time.Unix(at, 0)
	return c, notFound(err)
}

// SaveConnectActor inserts or refreshes a remote actor.
func (s *Store) SaveConnectActor(ctx context.Context, c ConnectActor) (ConnectActor, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO connect_actors (uri, host, type, username, name, summary, url, inbox, shared_inbox,
		outbox, key_id, key_pem, clone_uri, attributed_to, fetched_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(uri) DO UPDATE SET host=excluded.host, type=excluded.type, username=excluded.username, name=excluded.name,
		summary=excluded.summary, url=excluded.url, inbox=excluded.inbox, shared_inbox=excluded.shared_inbox,
		outbox=excluded.outbox, key_id=excluded.key_id, key_pem=excluded.key_pem, clone_uri=excluded.clone_uri,
		attributed_to=excluded.attributed_to, fetched_at=excluded.fetched_at`,
		c.URI, strings.ToLower(c.Host), c.Type, c.Username, c.Name, c.Summary, c.URL, c.Inbox, c.SharedInbox, c.Outbox,
		c.KeyID, c.KeyPEM, c.CloneURI, c.AttributedTo, now())
	if err != nil {
		return ConnectActor{}, err
	}
	return s.ConnectActor(ctx, c.URI)
}

func (s *Store) ConnectActor(ctx context.Context, uri string) (ConnectActor, error) {
	return scanConnectActor(s.db.QueryRowContext(ctx, `SELECT `+connectActorCols+` FROM connect_actors WHERE uri=?`, uri))
}

// ---------- follows ----------

// ConnectFollow is a follow, either way, with the remote actor.
type ConnectFollow struct {
	ID         int64
	Local      string
	ActorURI   string
	State      string
	ActivityID string
	CreatedAt  time.Time
	Actor      ConnectActor
}

func (s *Store) connectFollows(ctx context.Context, table, where string, args ...any) ([]ConnectFollow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, f.local, f.actor_uri, f.state, f.activity_id, f.created_at,
		coalesce(a.id,0), coalesce(a.host,''), coalesce(a.type,''), coalesce(a.username,''), coalesce(a.name,''),
		coalesce(a.url,''), coalesce(a.inbox,''), coalesce(a.shared_inbox,'')
		FROM `+table+` f LEFT JOIN connect_actors a ON a.uri = f.actor_uri WHERE `+where+` ORDER BY f.created_at DESC, f.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnectFollow
	for rows.Next() {
		var f ConnectFollow
		var at int64
		if err := rows.Scan(&f.ID, &f.Local, &f.ActorURI, &f.State, &f.ActivityID, &at, &f.Actor.ID, &f.Actor.Host,
			&f.Actor.Type, &f.Actor.Username, &f.Actor.Name, &f.Actor.URL, &f.Actor.Inbox, &f.Actor.SharedInbox); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(at, 0)
		f.Actor.URI = f.ActorURI
		if f.Actor.Host == "" {
			if i := strings.Index(f.ActorURI, "://"); i >= 0 {
				f.Actor.Host, _, _ = strings.Cut(f.ActorURI[i+3:], "/")
			}
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetConnectFollowing records (or restarts) a follow you sent.
func (s *Store) SetConnectFollowing(ctx context.Context, local, actorURI, state, activityID string) error {
	t := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO connect_following (local, actor_uri, state, activity_id, created_at, updated_at)
		VALUES (?,?,?,?,?,?) ON CONFLICT(local, actor_uri) DO UPDATE SET state=excluded.state, activity_id=excluded.activity_id,
		updated_at=excluded.updated_at`, local, actorURI, state, activityID, t, t)
	return err
}

// ConnectFollowingByActivity finds a follow you sent by its Follow activity id.
func (s *Store) ConnectFollowingByActivity(ctx context.Context, activityID string) (ConnectFollow, error) {
	fs, err := s.connectFollows(ctx, "connect_following", "f.activity_id=?", activityID)
	if err != nil {
		return ConnectFollow{}, err
	}
	if len(fs) == 0 {
		return ConnectFollow{}, ErrNotFound
	}
	return fs[0], nil
}

// ConnectFollowingOf finds your follow of one actor.
func (s *Store) ConnectFollowingOf(ctx context.Context, local, actorURI string) (ConnectFollow, error) {
	fs, err := s.connectFollows(ctx, "connect_following", "f.local=? AND f.actor_uri=?", local, actorURI)
	if err != nil {
		return ConnectFollow{}, err
	}
	if len(fs) == 0 {
		return ConnectFollow{}, ErrNotFound
	}
	return fs[0], nil
}

// SetConnectFollowingState updates a follow you sent.
func (s *Store) SetConnectFollowingState(ctx context.Context, id int64, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE connect_following SET state=?, updated_at=? WHERE id=?`, state, now(), id)
	return err
}

// ConnectFollowing lists who local follows (every state).
func (s *Store) ConnectFollowing(ctx context.Context, local string) ([]ConnectFollow, error) {
	return s.connectFollows(ctx, "connect_following", "f.local=?", local)
}

// FollowsAccepted says whether any local account follows actorURI and was accepted.
func (s *Store) ConnectFollowsAccepted(ctx context.Context, actorURI string) bool {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM connect_following WHERE actor_uri=? AND state='accepted'`, actorURI).Scan(&n)
	return n > 0
}

func (s *Store) DeleteConnectFollowing(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_following WHERE id=?`, id)
	return err
}

// SetConnectFollower records a follow request someone sent to local.
func (s *Store) SetConnectFollower(ctx context.Context, local, actorURI, state, activityID string) (ConnectFollow, error) {
	t := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO connect_followers (local, actor_uri, state, activity_id, created_at, updated_at)
		VALUES (?,?,?,?,?,?) ON CONFLICT(local, actor_uri) DO UPDATE SET
		state=CASE WHEN connect_followers.state='accepted' THEN 'accepted' ELSE excluded.state END,
		activity_id=excluded.activity_id, updated_at=excluded.updated_at`, local, actorURI, state, activityID, t, t)
	if err != nil {
		return ConnectFollow{}, err
	}
	fs, err := s.connectFollows(ctx, "connect_followers", "f.local=? AND f.actor_uri=?", local, actorURI)
	if err != nil || len(fs) == 0 {
		return ConnectFollow{}, errors.Join(err, ErrNotFound)
	}
	return fs[0], nil
}

// ConnectFollowers lists local's followers in a state ("" for every state).
func (s *Store) ConnectFollowers(ctx context.Context, local, state string) ([]ConnectFollow, error) {
	if state == "" {
		return s.connectFollows(ctx, "connect_followers", "f.local=?", local)
	}
	return s.connectFollows(ctx, "connect_followers", "f.local=? AND f.state=?", local, state)
}

// ConnectFollowerByID is one follower row.
func (s *Store) ConnectFollowerByID(ctx context.Context, id int64) (ConnectFollow, error) {
	fs, err := s.connectFollows(ctx, "connect_followers", "f.id=?", id)
	if err != nil {
		return ConnectFollow{}, err
	}
	if len(fs) == 0 {
		return ConnectFollow{}, ErrNotFound
	}
	return fs[0], nil
}

func (s *Store) SetConnectFollowerState(ctx context.Context, id int64, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE connect_followers SET state=?, updated_at=? WHERE id=?`, state, now(), id)
	return err
}

func (s *Store) DeleteConnectFollower(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_followers WHERE id=?`, id)
	return err
}

// DeleteConnectFollowerActor removes actorURI as a follower of local.
func (s *Store) DeleteConnectFollowerActor(ctx context.Context, local, actorURI string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM connect_followers WHERE local=? AND actor_uri=?`, local, actorURI)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ConnectAcceptedFollowerInboxes is where to deliver local's public activity:
// the inbox (shared when possible) of every accepted follower, once each.
func (s *Store) ConnectAcceptedFollowerInboxes(ctx context.Context, locals []string) ([]string, error) {
	if len(locals) == 0 {
		return nil, nil
	}
	q := `SELECT DISTINCT CASE WHEN a.shared_inbox != '' THEN a.shared_inbox ELSE a.inbox END
		FROM connect_followers f JOIN connect_actors a ON a.uri = f.actor_uri
		WHERE f.state='accepted' AND f.local IN (?` + strings.Repeat(",?", len(locals)-1) + `)`
	args := make([]any, len(locals))
	for i, l := range locals {
		args[i] = l
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var in string
		if err := rows.Scan(&in); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// ForgetConnectHost drops follows both ways, feed items and queued
// deliveries for a host (when it's blocked).
func (s *Store) ForgetConnectHost(ctx context.Context, host string) error {
	host = strings.ToLower(host)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM connect_followers WHERE actor_uri IN (SELECT uri FROM connect_actors WHERE host=?)`,
			`DELETE FROM connect_following WHERE actor_uri IN (SELECT uri FROM connect_actors WHERE host=?)`,
			`DELETE FROM connect_feed WHERE actor_uri IN (SELECT uri FROM connect_actors WHERE host=?)`,
			`DELETE FROM connect_stars WHERE actor_uri IN (SELECT uri FROM connect_actors WHERE host=?)`,
			`UPDATE connect_deliveries SET status='dropped', last_error='studio blocked' WHERE host=? AND status='queued'`,
		} {
			if _, err := tx.ExecContext(ctx, q, host); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------- block and allow ----------

// ConnectRule blocks or allows a studio host.
type ConnectRule struct {
	Host      string
	Kind      string // block, allow
	CreatedAt time.Time
}

func (s *Store) ConnectRules(ctx context.Context) ([]ConnectRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT host, kind, created_at FROM connect_rules ORDER BY kind, host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnectRule
	for rows.Next() {
		var r ConnectRule
		var at int64
		if err := rows.Scan(&r.Host, &r.Kind, &at); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ConnectRuleFor is "block", "allow" or "" for a host.
func (s *Store) ConnectRuleFor(ctx context.Context, host string) string {
	var k string
	_ = s.db.QueryRowContext(ctx, `SELECT kind FROM connect_rules WHERE host=?`, strings.ToLower(host)).Scan(&k)
	return k
}

func (s *Store) SetConnectRule(ctx context.Context, host, kind string) error {
	if kind != "block" && kind != "allow" {
		return errors.New("connect: rule must be block or allow")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO connect_rules (host, kind, created_at) VALUES (?,?,?)
		ON CONFLICT(host) DO UPDATE SET kind=excluded.kind`, strings.ToLower(host), kind, now())
	return err
}

func (s *Store) DeleteConnectRule(ctx context.Context, host string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_rules WHERE host=?`, strings.ToLower(host))
	return err
}

// ---------- feed ----------

// ConnectFeedItem is one activity from someone you follow.
type ConnectFeedItem struct {
	ID         int64
	ActivityID string
	ActorURI   string
	Kind       string // repo, push, release, star, note, share
	Text       string
	Detail     string
	Link       string
	ObjectURI  string
	ObjectName string
	Published  time.Time
	ReceivedAt time.Time
	Actor      ConnectActor
}

// AddConnectFeed stores an item; false if the activity was already there.
func (s *Store) AddConnectFeed(ctx context.Context, it ConnectFeedItem) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO connect_feed (activity_id, actor_uri, kind, text, detail, link,
		object_uri, object_name, published, received_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		it.ActivityID, it.ActorURI, it.Kind, it.Text, it.Detail, it.Link, it.ObjectURI, it.ObjectName, it.Published.Unix(), now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ConnectFeed is the newest items, newest first.
func (s *Store) ConnectFeed(ctx context.Context, limit int) ([]ConnectFeedItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, f.activity_id, f.actor_uri, f.kind, f.text, f.detail, f.link, f.object_uri,
		f.object_name, f.published, f.received_at, coalesce(a.host,''), coalesce(a.username,''), coalesce(a.name,''), coalesce(a.url,'')
		FROM connect_feed f LEFT JOIN connect_actors a ON a.uri = f.actor_uri ORDER BY f.published DESC, f.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnectFeedItem
	for rows.Next() {
		var it ConnectFeedItem
		var p, r int64
		if err := rows.Scan(&it.ID, &it.ActivityID, &it.ActorURI, &it.Kind, &it.Text, &it.Detail, &it.Link, &it.ObjectURI,
			&it.ObjectName, &p, &r, &it.Actor.Host, &it.Actor.Username, &it.Actor.Name, &it.Actor.URL); err != nil {
			return nil, err
		}
		it.Published, it.ReceivedAt = time.Unix(p, 0), time.Unix(r, 0)
		it.Actor.URI = it.ActorURI
		out = append(out, it)
	}
	return out, rows.Err()
}

// DeleteConnectFeedFrom removes an actor's items (after unfollowing).
func (s *Store) DeleteConnectFeedFrom(ctx context.Context, actorURI string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_feed WHERE actor_uri=?`, actorURI)
	return err
}

// ---------- deliveries ----------

// ConnectDelivery is one activity on its way to one inbox.
type ConnectDelivery struct {
	ID         int64
	Inbox      string
	Host       string
	ActivityID string
	Kind       string
	Body       []byte
	RepoID     int64
	Status     string
	Attempts   int
	NextAt     time.Time
	LastStatus int
	LastError  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const connectDeliveryCols = `id, inbox, host, activity_id, kind, body, repo_id, status, attempts, next_at, last_status, last_error, created_at, updated_at`

func scanConnectDeliveries(rows *sql.Rows) ([]ConnectDelivery, error) {
	defer rows.Close()
	var out []ConnectDelivery
	for rows.Next() {
		var d ConnectDelivery
		var next, c, u int64
		if err := rows.Scan(&d.ID, &d.Inbox, &d.Host, &d.ActivityID, &d.Kind, &d.Body, &d.RepoID, &d.Status, &d.Attempts,
			&next, &d.LastStatus, &d.LastError, &c, &u); err != nil {
			return nil, err
		}
		d.NextAt, d.CreatedAt, d.UpdatedAt = time.Unix(next, 0), time.Unix(c, 0), time.Unix(u, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) QueueConnectDelivery(ctx context.Context, d ConnectDelivery) error {
	t := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO connect_deliveries (inbox, host, activity_id, kind, body, repo_id, status,
		attempts, next_at, created_at, updated_at) VALUES (?,?,?,?,?,?,'queued',0,?,?,?)`,
		d.Inbox, strings.ToLower(d.Host), d.ActivityID, d.Kind, d.Body, d.RepoID, t, t, t)
	return err
}

// DueConnectDeliveries are queued deliveries whose time has come.
func (s *Store) DueConnectDeliveries(ctx context.Context, at time.Time, limit int) ([]ConnectDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+connectDeliveryCols+` FROM connect_deliveries
		WHERE status='queued' AND next_at<=? ORDER BY next_at, id LIMIT ?`, at.Unix(), limit)
	if err != nil {
		return nil, err
	}
	return scanConnectDeliveries(rows)
}

// FinishConnectDelivery records an attempt.
func (s *Store) FinishConnectDelivery(ctx context.Context, id int64, status string, attempts int, next time.Time, code int, msg string) error {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE connect_deliveries SET status=?, attempts=?, next_at=?, last_status=?, last_error=?,
		updated_at=? WHERE id=?`, status, attempts, next.Unix(), code, msg, now(), id)
	return err
}

// ConnectDeliveryLog is the latest deliveries, newest first.
func (s *Store) ConnectDeliveryLog(ctx context.Context, limit int) ([]ConnectDelivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+connectDeliveryCols+` FROM connect_deliveries ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanConnectDeliveries(rows)
}

// PruneConnect drops old delivery log entries and expired replay keys.
func (s *Store) PruneConnect(ctx context.Context, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_deliveries WHERE status!='queued' AND updated_at<?`,
		at.Add(-14*24*time.Hour).Unix())
	if err == nil {
		_, err = s.db.ExecContext(ctx, `DELETE FROM connect_seen WHERE expires_at<?`, at.Unix())
	}
	return err
}

// ---------- stars ----------

// AddConnectStar records a star from another studio; false if it was there.
func (s *Store) AddConnectStar(ctx context.Context, repoID int64, actorURI, activityID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO connect_stars (repo_id, actor_uri, activity_id, created_at)
		VALUES (?,?,?,?)`, repoID, actorURI, activityID, now())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) RemoveConnectStar(ctx context.Context, repoID int64, actorURI string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_stars WHERE repo_id=? AND actor_uri=?`, repoID, actorURI)
	return err
}

// RemoveConnectStarByActivity undoes a Like by its id.
func (s *Store) RemoveConnectStarByActivity(ctx context.Context, actorURI, activityID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_stars WHERE actor_uri=? AND activity_id=?`, actorURI, activityID)
	return err
}

func (s *Store) ConnectStarCount(ctx context.Context, repoID int64) int {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM connect_stars WHERE repo_id=?`, repoID).Scan(&n)
	return n
}

// ConnectStarCounts is the star count of every repository with stars.
func (s *Store) ConnectStarCounts(ctx context.Context) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id, count(*) FROM connect_stars GROUP BY repo_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ConnectStarred is your star on another studio's repository.
type ConnectStarred struct {
	ObjectURI  string
	Name       string
	URL        string
	Inbox      string
	ActivityID string
	CreatedAt  time.Time
}

func (s *Store) AddConnectStarred(ctx context.Context, st ConnectStarred) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO connect_starred (object_uri, name, url, inbox, activity_id, created_at)
		VALUES (?,?,?,?,?,?) ON CONFLICT(object_uri) DO UPDATE SET name=excluded.name, url=excluded.url, inbox=excluded.inbox,
		activity_id=excluded.activity_id`, st.ObjectURI, st.Name, st.URL, st.Inbox, st.ActivityID, now())
	return err
}

func (s *Store) ConnectStarredOne(ctx context.Context, objectURI string) (ConnectStarred, error) {
	var st ConnectStarred
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT object_uri, name, url, inbox, activity_id, created_at FROM connect_starred
		WHERE object_uri=?`, objectURI).Scan(&st.ObjectURI, &st.Name, &st.URL, &st.Inbox, &st.ActivityID, &at)
	st.CreatedAt = time.Unix(at, 0)
	return st, notFound(err)
}

func (s *Store) ConnectStarredList(ctx context.Context) ([]ConnectStarred, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT object_uri, name, url, inbox, activity_id, created_at FROM connect_starred
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnectStarred
	for rows.Next() {
		var st ConnectStarred
		var at int64
		if err := rows.Scan(&st.ObjectURI, &st.Name, &st.URL, &st.Inbox, &st.ActivityID, &at); err != nil {
			return nil, err
		}
		st.CreatedAt = time.Unix(at, 0)
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *Store) DeleteConnectStarred(ctx context.Context, objectURI string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_starred WHERE object_uri=?`, objectURI)
	return err
}

// ---------- replay protection ----------

// ConnectFirstSeen records key until ttl from now and reports whether it is
// new. A key seen before and not yet expired returns false.
func (s *Store) ConnectFirstSeen(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO connect_seen (key, expires_at) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET expires_at=excluded.expires_at WHERE connect_seen.expires_at < ?`,
		key, t+int64(ttl.Seconds()), t)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---------- announced repositories ----------

func (s *Store) ConnectAnnounced(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo_id FROM connect_announced`)
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

func (s *Store) MarkConnectAnnounced(ctx context.Context, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO connect_announced (repo_id, at) VALUES (?, ?)`, repoID, now())
	return err
}

// ---------- badges ----------

func (s *Store) SaveConnectBadge(ctx context.Context, login, actor string) error {
	if actor == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM connect_badges WHERE login=?`, login)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO connect_badges (login, actor, checked_at) VALUES (?,?,?)
		ON CONFLICT(login) DO UPDATE SET actor=excluded.actor, checked_at=excluded.checked_at`, login, actor, now())
	return err
}

func (s *Store) ConnectBadge(ctx context.Context, login string) string {
	var a string
	_ = s.db.QueryRowContext(ctx, `SELECT actor FROM connect_badges WHERE login=?`, login).Scan(&a)
	return a
}

// ---------- offers (remote pull requests) ----------

// ConnectOffer is a branch another studio offered to one of your repositories.
type ConnectOffer struct {
	ID         int64
	ActivityID string
	ActorURI   string
	RepoID     int64
	PullID     int64
	Branch     string
	LocalRef   string
	CloneURI   string
	Title      string
	State      string // fetching, opened, failed
	Error      string
	CreatedAt  time.Time
}

// AddConnectOffer stores an offer; false if the activity was already seen.
func (s *Store) AddConnectOffer(ctx context.Context, o ConnectOffer) (int64, bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO connect_offers (activity_id, actor_uri, repo_id, branch, clone_uri,
		title, state, created_at) VALUES (?,?,?,?,?,?,?,?)`, o.ActivityID, o.ActorURI, o.RepoID, o.Branch, o.CloneURI, o.Title, o.State, now())
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	id, _ := res.LastInsertId()
	return id, n > 0, nil
}

func (s *Store) FinishConnectOffer(ctx context.Context, id, pullID int64, localRef, state, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE connect_offers SET pull_id=?, local_ref=?, state=?, error=? WHERE id=?`,
		pullID, localRef, state, msg, id)
	return err
}

// ConnectOfferForPull is the offer behind a pull request, if any.
func (s *Store) ConnectOfferForPull(ctx context.Context, pullID int64) (ConnectOffer, error) {
	var o ConnectOffer
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT id, activity_id, actor_uri, repo_id, pull_id, branch, local_ref, clone_uri, title,
		state, error, created_at FROM connect_offers WHERE pull_id=?`, pullID).Scan(&o.ID, &o.ActivityID, &o.ActorURI, &o.RepoID,
		&o.PullID, &o.Branch, &o.LocalRef, &o.CloneURI, &o.Title, &o.State, &o.Error, &at)
	o.CreatedAt = time.Unix(at, 0)
	return o, notFound(err)
}

// DeleteConnectFollowerByActivity removes a follower by their Follow's id.
func (s *Store) DeleteConnectFollowerByActivity(ctx context.Context, actorURI, activityID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_followers WHERE actor_uri=? AND activity_id=?`, actorURI, activityID)
	return err
}

// DeleteConnectFeedActivity removes one feed item (an undone activity).
func (s *Store) DeleteConnectFeedActivity(ctx context.Context, actorURI, activityID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_feed WHERE actor_uri=? AND activity_id=?`, actorURI, activityID)
	return err
}

// DeleteConnectFeedObject removes feed items about a deleted object.
func (s *Store) DeleteConnectFeedObject(ctx context.Context, actorURI, objectURI string) error {
	if objectURI == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM connect_feed WHERE actor_uri=? AND object_uri=?`, actorURI, objectURI)
	return err
}
