package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"math/big"
	"strings"
	"time"
)

// App API (docs/app-api.md): devices signed in with the device flow, the SSH
// keys they added, pull requests waiting to merge when their checks pass, and
// each person's friendly messages.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS app_devices (
  id         INTEGER PRIMARY KEY,
  person_id  INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  kind       TEXT NOT NULL,
  os         TEXT NOT NULL DEFAULT '',
  version    TEXT NOT NULL DEFAULT '',
  token_hash TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL DEFAULT 0,
  last_ip    TEXT NOT NULL DEFAULT '',
  revoked_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS app_devices_person ON app_devices(person_id);

CREATE TABLE IF NOT EXISTS app_device_codes (
  device_hash TEXT PRIMARY KEY,
  user_code   TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL,
  os          TEXT NOT NULL DEFAULT '',
  version     TEXT NOT NULL DEFAULT '',
  ip          TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'pending',
  viewer_id   INTEGER NOT NULL DEFAULT 0,
  person_id   INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS app_device_keys (
  device_id INTEGER NOT NULL REFERENCES app_devices(id) ON DELETE CASCADE,
  key_id    INTEGER NOT NULL,
  PRIMARY KEY (device_id, key_id)
);

CREATE TABLE IF NOT EXISTS app_auto_merges (
  repo_id       INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  number        INTEGER NOT NULL,
  method        TEXT NOT NULL,
  by_kind       TEXT NOT NULL,
  by_name       TEXT NOT NULL,
  person_id     INTEGER NOT NULL,
  device_id     INTEGER NOT NULL DEFAULT 0,
  head_sha      TEXT NOT NULL DEFAULT '',
  delete_branch INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  PRIMARY KEY (repo_id, number)
);

CREATE TABLE IF NOT EXISTS app_messages (
  person_id  INTEGER PRIMARY KEY REFERENCES owners(id) ON DELETE CASCADE,
  data       TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);`)
}

// ---------- devices ----------

// AppDeviceTokenPrefix starts every device token.
const AppDeviceTokenPrefix = "atd_"

const appBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// appRandomBase62 returns n random base62 characters.
func appRandomBase62(n int) string {
	max := big.NewInt(int64(len(appBase62)))
	b := make([]byte, n)
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = appBase62[v.Int64()]
	}
	return string(b)
}

// AppDevice is a desktop app or CLI signed in on one machine.
type AppDevice struct {
	ID        int64
	PersonID  int64
	Name      string
	Kind      string // desktop or cli
	OS        string
	Version   string
	CreatedAt time.Time
	LastSeen  time.Time // zero = never
	LastIP    string
	RevokedAt time.Time // zero = active
}

func (d AppDevice) Revoked() bool { return !d.RevokedAt.IsZero() }

const appDeviceCols = `id, person_id, name, kind, os, version, created_at, last_seen, last_ip, revoked_at`

func appTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func scanAppDevice(row interface{ Scan(...any) error }) (AppDevice, error) {
	var d AppDevice
	var c, seen, rev int64
	err := row.Scan(&d.ID, &d.PersonID, &d.Name, &d.Kind, &d.OS, &d.Version, &c, &seen, &d.LastIP, &rev)
	d.CreatedAt, d.LastSeen, d.RevokedAt = time.Unix(c, 0), appTime(seen), appTime(rev)
	return d, err
}

// AppDeviceByToken finds the device a plaintext token belongs to, revoked or
// not (callers check Revoked). Only the token's hash is stored.
func (s *Store) AppDeviceByToken(ctx context.Context, plain string) (AppDevice, error) {
	if !strings.HasPrefix(plain, AppDeviceTokenPrefix) {
		return AppDevice{}, ErrNotFound
	}
	h := Hash(plain)
	var stored string
	row := s.db.QueryRowContext(ctx, `SELECT token_hash, `+appDeviceCols+` FROM app_devices WHERE token_hash=?`, h)
	var d AppDevice
	var c, seen, rev int64
	if err := row.Scan(&stored, &d.ID, &d.PersonID, &d.Name, &d.Kind, &d.OS, &d.Version, &c, &seen, &d.LastIP, &rev); err != nil {
		return AppDevice{}, notFound(err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(h)) != 1 {
		return AppDevice{}, ErrNotFound
	}
	d.CreatedAt, d.LastSeen, d.RevokedAt = time.Unix(c, 0), appTime(seen), appTime(rev)
	return d, nil
}

// AppDevice finds one of a person's devices.
func (s *Store) AppDevice(ctx context.Context, personID, id int64) (AppDevice, error) {
	d, err := scanAppDevice(s.db.QueryRowContext(ctx, `SELECT `+appDeviceCols+` FROM app_devices WHERE id=? AND person_id=?`, id, personID))
	return d, notFound(err)
}

// AppDeviceByID finds a device by id alone (background work).
func (s *Store) AppDeviceByID(ctx context.Context, id int64) (AppDevice, error) {
	d, err := scanAppDevice(s.db.QueryRowContext(ctx, `SELECT `+appDeviceCols+` FROM app_devices WHERE id=?`, id))
	return d, notFound(err)
}

// AppDevices lists a person's active devices, newest first.
func (s *Store) AppDevices(ctx context.Context, personID int64) ([]AppDevice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appDeviceCols+` FROM app_devices WHERE person_id=? AND revoked_at=0 ORDER BY id DESC`, personID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppDevice
	for rows.Next() {
		d, err := scanAppDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TouchAppDevice records that a device was just seen, at most once a minute
// unless its address changed. ip may be "" to keep the last one.
func (s *Store) TouchAppDevice(ctx context.Context, d AppDevice, ip string) error {
	if ip == "" {
		ip = d.LastIP
	}
	if ip == d.LastIP && time.Since(d.LastSeen) < time.Minute {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE app_devices SET last_seen=?, last_ip=? WHERE id=?`, now(), ip, d.ID)
	return err
}

// RevokeAppDevice revokes one of a person's devices: its token stops working
// and the SSH keys it added are removed. It returns how many keys went.
func (s *Store) RevokeAppDevice(ctx context.Context, personID, id int64) (keys int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE app_devices SET revoked_at=? WHERE id=? AND person_id=? AND revoked_at=0`, now(), id, personID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM ssh_keys WHERE id IN (SELECT key_id FROM app_device_keys WHERE device_id=?)`, id)
	if err != nil {
		return 0, err
	}
	keys, _ = res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_device_keys WHERE device_id=?`, id); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_auto_merges WHERE device_id=?`, id); err != nil {
		return 0, err
	}
	return keys, tx.Commit()
}

// LinkAppDeviceKey records that a device added an SSH key, and makes the key
// its person's.
func (s *Store) LinkAppDeviceKey(ctx context.Context, d AppDevice, keyID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE ssh_keys SET person_id=? WHERE id=?`, d.PersonID, keyID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO app_device_keys (device_id, key_id) VALUES (?, ?)`, d.ID, keyID); err != nil {
		return err
	}
	return tx.Commit()
}

// AppDeviceKeyIDs lists the SSH keys a device added.
func (s *Store) AppDeviceKeyIDs(ctx context.Context, deviceID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_id FROM app_device_keys WHERE device_id=? ORDER BY key_id`, deviceID)
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

// ---------- the device flow ----------

// AppUserCodeWords are the 32 words a user code starts with.
var AppUserCodeWords = []string{
	"AMBER", "BIRCH", "CEDAR", "CORAL", "DELTA", "EMBER", "FERN", "FLINT",
	"GROVE", "HAZEL", "INDIGO", "IVORY", "JADE", "KELP", "LARCH", "LINEN",
	"MAPLE", "MOSS", "NORTH", "OPAL", "PEARL", "PINE", "QUARTZ", "RIVER",
	"SAGE", "SLATE", "TIDE", "UMBER", "VIOLET", "WILLOW", "WREN", "ZINC",
}

// Device code states.
const (
	AppCodePending  = "pending"
	AppCodeApproved = "approved"
	AppCodeDenied   = "denied"
	AppCodeClaimed  = "claimed"
)

// ErrAppCodeExpired: the code ran out, or its token was already handed over.
var ErrAppCodeExpired = errors.New("device code expired")

// ErrAppCodeDenied: the owner said no.
var ErrAppCodeDenied = errors.New("device code denied")

// ErrAppCodePending: nobody has decided yet.
var ErrAppCodePending = errors.New("device code pending")

// AppDeviceCode is one sign-in attempt.
type AppDeviceCode struct {
	UserCode  string
	Name      string
	Kind      string
	OS        string
	Version   string
	IP        string
	Status    string
	ViewerID  int64 // the person who opened the approval page first
	PersonID  int64 // the person who approved
	CreatedAt time.Time
	ExpiresAt time.Time
}

func appNewUserCode() string {
	w, err := rand.Int(rand.Reader, big.NewInt(int64(len(AppUserCodeWords))))
	if err != nil {
		panic(err)
	}
	n, err := rand.Int(rand.Reader, big.NewInt(10000))
	if err != nil {
		panic(err)
	}
	d := n.Int64()
	return AppUserCodeWords[w.Int64()] + "-" + string([]byte{byte('0' + d/1000), byte('0' + d/100%10), byte('0' + d/10%10), byte('0' + d%10)})
}

// NormalizeAppUserCode tidies what someone typed: "amber 4127" → "AMBER-4127".
func NormalizeAppUserCode(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.NewReplacer(" ", "-", "_", "-").Replace(s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// StartAppDeviceCode begins a sign-in: it returns the secret device code (64
// hex characters, never stored) and the short code the owner types or sees.
func (s *Store) StartAppDeviceCode(ctx context.Context, c AppDeviceCode, ttl time.Duration) (deviceCode, userCode string, err error) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM app_device_codes WHERE expires_at < ?`, now()-3600)
	deviceCode = RandomToken(32)
	for try := 0; try < 20; try++ {
		userCode = appNewUserCode()
		// An expired or finished code frees its user code.
		if _, err = s.db.ExecContext(ctx, `DELETE FROM app_device_codes WHERE user_code=? AND (expires_at <= ? OR status IN ('denied','claimed'))`,
			userCode, now()); err != nil {
			return "", "", err
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO app_device_codes (device_hash, user_code, name, kind, os, version, ip, status, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
			Hash(deviceCode), userCode, c.Name, c.Kind, c.OS, c.Version, c.IP, now(), time.Now().Add(ttl).Unix())
		if err == nil {
			return deviceCode, userCode, nil
		}
		if !strings.Contains(err.Error(), "UNIQUE") {
			return "", "", err
		}
	}
	return "", "", errors.New("no free device code, try again")
}

const appCodeCols = `user_code, name, kind, os, version, ip, status, viewer_id, person_id, created_at, expires_at`

func scanAppCode(row interface{ Scan(...any) error }) (AppDeviceCode, error) {
	var c AppDeviceCode
	var created, exp int64
	err := row.Scan(&c.UserCode, &c.Name, &c.Kind, &c.OS, &c.Version, &c.IP, &c.Status, &c.ViewerID, &c.PersonID, &created, &exp)
	c.CreatedAt, c.ExpiresAt = time.Unix(created, 0), time.Unix(exp, 0)
	return c, err
}

// ViewAppDeviceCode returns the pending code a person is looking at on the
// approval page. The first person to open a code keeps it: nobody else can
// see or decide it afterwards. Unknown, expired, decided and someone else's
// codes are all ErrNotFound.
func (s *Store) ViewAppDeviceCode(ctx context.Context, userCode string, personID int64) (AppDeviceCode, error) {
	userCode = NormalizeAppUserCode(userCode)
	if userCode == "" || personID == 0 {
		return AppDeviceCode{}, ErrNotFound
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE app_device_codes SET viewer_id=? WHERE user_code=? AND status='pending' AND viewer_id=0 AND expires_at > ?`,
		personID, userCode, now()); err != nil {
		return AppDeviceCode{}, err
	}
	c, err := scanAppCode(s.db.QueryRowContext(ctx, `SELECT `+appCodeCols+` FROM app_device_codes WHERE user_code=?`, userCode))
	if err != nil {
		return AppDeviceCode{}, notFound(err)
	}
	if subtle.ConstantTimeCompare([]byte(c.UserCode), []byte(userCode)) != 1 || c.Status != AppCodePending ||
		c.ViewerID != personID || !c.ExpiresAt.After(time.Now()) {
		return AppDeviceCode{}, ErrNotFound
	}
	return c, nil
}

// DecideAppDeviceCode approves or denies a code the person has opened on the
// approval page. A code is decided once.
func (s *Store) DecideAppDeviceCode(ctx context.Context, userCode string, personID int64, approve bool) (AppDeviceCode, error) {
	c, err := s.ViewAppDeviceCode(ctx, userCode, personID)
	if err != nil {
		return AppDeviceCode{}, err
	}
	status := AppCodeDenied
	if approve {
		status = AppCodeApproved
	}
	res, err := s.db.ExecContext(ctx, `UPDATE app_device_codes SET status=?, person_id=? WHERE user_code=? AND status='pending' AND viewer_id=? AND expires_at > ?`,
		status, personID, c.UserCode, personID, now())
	if err != nil {
		return AppDeviceCode{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return AppDeviceCode{}, ErrNotFound
	}
	c.Status, c.PersonID = status, personID
	return c, nil
}

// ClaimAppDeviceCode is the device's poll. Once the code is approved it
// creates the device and returns its token, exactly once: the code is used up
// in the same transaction. ErrAppCodePending, ErrAppCodeDenied and
// ErrAppCodeExpired (also for unknown and already used codes) say why not.
func (s *Store) ClaimAppDeviceCode(ctx context.Context, deviceCode string) (token string, d AppDevice, err error) {
	h := Hash(deviceCode)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", d, err
	}
	defer tx.Rollback()
	var stored string
	var c AppDeviceCode
	var created, exp int64
	err = tx.QueryRowContext(ctx, `SELECT device_hash, `+appCodeCols+` FROM app_device_codes WHERE device_hash=?`, h).
		Scan(&stored, &c.UserCode, &c.Name, &c.Kind, &c.OS, &c.Version, &c.IP, &c.Status, &c.ViewerID, &c.PersonID, &created, &exp)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", d, ErrAppCodeExpired
		}
		return "", d, err
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(h)) != 1 {
		return "", d, ErrAppCodeExpired
	}
	switch {
	case c.Status == AppCodeDenied:
		return "", d, ErrAppCodeDenied
	case c.Status == AppCodeClaimed, exp <= now():
		return "", d, ErrAppCodeExpired
	case c.Status == AppCodePending:
		return "", d, ErrAppCodePending
	case c.Status != AppCodeApproved || c.PersonID == 0:
		return "", d, ErrAppCodeExpired
	}
	res, err := tx.ExecContext(ctx, `UPDATE app_device_codes SET status='claimed' WHERE device_hash=? AND status='approved'`, h)
	if err != nil {
		return "", d, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", d, ErrAppCodeExpired
	}
	// Device names are unique per person, so "device:<name>" names one device.
	name := c.Name
	for i := 2; ; i++ {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM app_devices WHERE person_id=? AND revoked_at=0 AND name=? COLLATE NOCASE`,
			c.PersonID, name).Scan(&n); err != nil {
			return "", d, err
		}
		if n == 0 {
			break
		}
		name = c.Name + " " + string(rune('0'+i%10))
		if i >= 10 {
			name = c.Name + " " + appRandomBase62(4)
		}
	}
	token = AppDeviceTokenPrefix + appRandomBase62(40)
	ins, err := tx.ExecContext(ctx, `INSERT INTO app_devices (person_id, name, kind, os, version, token_hash, created_at, last_seen, last_ip)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.PersonID, name, c.Kind, c.OS, c.Version, Hash(token), now(), now(), c.IP)
	if err != nil {
		return "", d, err
	}
	id, _ := ins.LastInsertId()
	d, err = scanAppDevice(tx.QueryRowContext(ctx, `SELECT `+appDeviceCols+` FROM app_devices WHERE id=?`, id))
	if err != nil {
		return "", d, err
	}
	return token, d, tx.Commit()
}

// ---------- merge when green ----------

// AppAutoMerge is a pull request that merges as soon as it can.
type AppAutoMerge struct {
	RepoID       int64
	Number       int64
	Method       string
	ByKind       string
	ByName       string
	PersonID     int64
	DeviceID     int64  // 0 = asked with an ordinary access token
	HeadSHA      string // the head commit it was asked for (or the asker pushed since)
	DeleteBranch bool
	CreatedAt    time.Time
}

// By is who asked, as "device:MacBook" or the token's name.
func (m AppAutoMerge) By() string { return m.ByName }

const appAutoMergeCols = `repo_id, number, method, by_kind, by_name, person_id, device_id, head_sha, delete_branch, created_at`

func scanAppAutoMerge(row interface{ Scan(...any) error }) (AppAutoMerge, error) {
	var m AppAutoMerge
	var c int64
	err := row.Scan(&m.RepoID, &m.Number, &m.Method, &m.ByKind, &m.ByName, &m.PersonID, &m.DeviceID, &m.HeadSHA, &m.DeleteBranch, &c)
	m.CreatedAt = time.Unix(c, 0)
	return m, err
}

// SetAppAutoMerge asks for (or replaces) a merge when green.
func (s *Store) SetAppAutoMerge(ctx context.Context, m AppAutoMerge) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO app_auto_merges (`+appAutoMergeCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(repo_id, number) DO UPDATE SET method=excluded.method, by_kind=excluded.by_kind, by_name=excluded.by_name,
		person_id=excluded.person_id, device_id=excluded.device_id, head_sha=excluded.head_sha, delete_branch=excluded.delete_branch,
		created_at=excluded.created_at`,
		m.RepoID, m.Number, m.Method, m.ByKind, m.ByName, m.PersonID, m.DeviceID, m.HeadSHA, m.DeleteBranch, now())
	return err
}

func (s *Store) AppAutoMerge(ctx context.Context, repoID, number int64) (AppAutoMerge, error) {
	m, err := scanAppAutoMerge(s.db.QueryRowContext(ctx, `SELECT `+appAutoMergeCols+` FROM app_auto_merges WHERE repo_id=? AND number=?`, repoID, number))
	return m, notFound(err)
}

// AppAutoMerges lists every pending merge when green (repoID 0 = all).
func (s *Store) AppAutoMerges(ctx context.Context, repoID int64) ([]AppAutoMerge, error) {
	q, args := `SELECT `+appAutoMergeCols+` FROM app_auto_merges`, []any{}
	if repoID != 0 {
		q += ` WHERE repo_id=?`
		args = append(args, repoID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, repo_id, number`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppAutoMerge
	for rows.Next() {
		m, err := scanAppAutoMerge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetAppAutoMergeHead moves a pending merge to a new head commit.
func (s *Store) SetAppAutoMergeHead(ctx context.Context, repoID, number int64, sha string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE app_auto_merges SET head_sha=? WHERE repo_id=? AND number=?`, sha, repoID, number)
	return err
}

// DeleteAppAutoMerge cancels one; false if there was none.
func (s *Store) DeleteAppAutoMerge(ctx context.Context, repoID, number int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM app_auto_merges WHERE repo_id=? AND number=?`, repoID, number)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---------- friendly messages ----------

// AppMessages returns a person's saved messages document (JSON), or
// ErrNotFound when they never changed the defaults.
func (s *Store) AppMessages(ctx context.Context, personID int64) (string, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT data FROM app_messages WHERE person_id=?`, personID).Scan(&data)
	return data, notFound(err)
}

func (s *Store) SetAppMessages(ctx context.Context, personID int64, data string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO app_messages (person_id, data, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(person_id) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`, personID, data, now())
	return err
}
