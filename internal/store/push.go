package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Web Push subscriptions: one row per person per browser/device that said yes
// to notifications. The endpoint is a capability URL on the browser vendor's
// push service; p256dh and auth are the browser's keys for encrypting
// payloads (RFC 8291). None of them are ever shown in the UI.
//
// push_prefs holds a person's choice for one kind of notification; a missing
// row means "use the default" (see web/push.go).
func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS push_subscriptions (
  id            INTEGER PRIMARY KEY,
  person_id     INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  endpoint      TEXT NOT NULL,
  p256dh        TEXT NOT NULL,
  auth          TEXT NOT NULL,
  label         TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  last_success  INTEGER,
  failure_count INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT NOT NULL DEFAULT '',
  UNIQUE (person_id, endpoint)
);
CREATE INDEX IF NOT EXISTS push_subscriptions_person ON push_subscriptions(person_id);
CREATE TABLE IF NOT EXISTS push_prefs (
  person_id INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  kind      TEXT NOT NULL,
  enabled   INTEGER NOT NULL,
  PRIMARY KEY (person_id, kind)
);`)
}

// PushSubscription is one device's Web Push subscription.
type PushSubscription struct {
	ID           int64
	PersonID     int64
	Endpoint     string
	P256dh, Auth string // base64url, as the browser sent them
	Label        string
	CreatedAt    time.Time
	LastSuccess  *time.Time
	FailureCount int
	LastError    string
}

const pushSubSelect = `SELECT id, person_id, endpoint, p256dh, auth, label, created_at, last_success, failure_count, last_error FROM push_subscriptions`

func scanPushSub(row interface{ Scan(...any) error }) (PushSubscription, error) {
	var p PushSubscription
	var created int64
	var ok sql.NullInt64
	if err := row.Scan(&p.ID, &p.PersonID, &p.Endpoint, &p.P256dh, &p.Auth, &p.Label, &created, &ok, &p.FailureCount, &p.LastError); err != nil {
		return p, err
	}
	p.CreatedAt = time.Unix(created, 0)
	if ok.Valid {
		t := time.Unix(ok.Int64, 0)
		p.LastSuccess = &t
	}
	return p, nil
}

// SavePushSubscription adds a device for a person, or refreshes its keys and
// label when the same endpoint subscribes again. It returns the row's ID.
func (s *Store) SavePushSubscription(ctx context.Context, p PushSubscription) (int64, error) {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO push_subscriptions (person_id, endpoint, p256dh, auth, label, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (person_id, endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth, label = excluded.label,
  failure_count = 0, last_error = ''`, p.PersonID, p.Endpoint, p.P256dh, p.Auth, p.Label, now)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM push_subscriptions WHERE person_id = ? AND endpoint = ?`, p.PersonID, p.Endpoint).Scan(&id)
	return id, err
}

// PushSubscriptions lists a person's devices, newest first.
func (s *Store) PushSubscriptions(ctx context.Context, personID int64) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, pushSubSelect+` WHERE person_id = ? ORDER BY created_at DESC, id DESC`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		p, err := scanPushSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PushSubscription returns one of a person's devices; another person's ID is
// ErrNotFound.
func (s *Store) PushSubscription(ctx context.Context, personID, id int64) (PushSubscription, error) {
	p, err := scanPushSub(s.db.QueryRowContext(ctx, pushSubSelect+` WHERE person_id = ? AND id = ?`, personID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// CountPushSubscriptions is how many devices a person has.
func (s *Store) CountPushSubscriptions(ctx context.Context, personID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM push_subscriptions WHERE person_id = ?`, personID).Scan(&n)
	return n, err
}

// DeletePushSubscription removes one of a person's devices. Another person's
// ID is ErrNotFound.
func (s *Store) DeletePushSubscription(ctx context.Context, personID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE person_id = ? AND id = ?`, personID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeletePushSubscriptionByID drops a subscription the push service says is
// gone (404 or 410).
func (s *Store) DeletePushSubscriptionByID(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, id)
	return err
}

// PushDelivered records a successful delivery.
func (s *Store) PushDelivered(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions SET last_success = ?, failure_count = 0, last_error = '' WHERE id = ?`, at.Unix(), id)
	return err
}

// PushFailed records a delivery that gave up. msg is short and secret-free.
func (s *Store) PushFailed(ctx context.Context, id int64, msg string) error {
	if len(msg) > 200 {
		msg = msg[:200]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions SET failure_count = failure_count + 1, last_error = ? WHERE id = ?`, msg, id)
	return err
}

// PushPrefs returns the kinds a person has set explicitly.
func (s *Store) PushPrefs(ctx context.Context, personID int64) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, enabled FROM push_prefs WHERE person_id = ?`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		var on bool
		if err := rows.Scan(&k, &on); err != nil {
			return nil, err
		}
		out[k] = on
	}
	return out, rows.Err()
}

// SetPushPrefs stores a person's choice for each kind given.
func (s *Store) SetPushPrefs(ctx context.Context, personID int64, prefs map[string]bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, on := range prefs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO push_prefs (person_id, kind, enabled) VALUES (?, ?, ?)
ON CONFLICT (person_id, kind) DO UPDATE SET enabled = excluded.enabled`, personID, k, on); err != nil {
			return err
		}
	}
	return tx.Commit()
}
