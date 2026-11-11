package store

// Remote access (the built-in tunnel, internal/tunnel and web/tunnel*.go).
//
// tunnel_config is one row. It is created switched off and nothing but
// SetTunnelEnabled turns it on, so a new install is off. A full restore runs
// TunnelForceOff (cmd/atelier), so a snapshot taken while it was on comes
// back off. The named-tunnel token and the Tailscale auth key are sealed
// with the SecretSealer and re-sealed by `atelier secrets rotate-key` (see
// sealedColumns).
//
// The provider columns (provider, ts_*) were added by a migration
// (migrateTunnelTailscale): `mode` stays the Cloudflare mode, so switching
// provider and back keeps each one's settings.
//
// tunnel_sessions marks the browser sessions that were started through the
// tunnel; they are the only sessions the remote listener accepts, and
// turning the tunnel off deletes them. tunnel_log is the "who's connected"
// log, kept for TunnelLogKeep.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS tunnel_config (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  enabled        INTEGER NOT NULL DEFAULT 0,
  mode           TEXT NOT NULL DEFAULT 'quick' CHECK (mode IN ('quick','named')),
  hostname       TEXT NOT NULL DEFAULT '',
  pages_hostname TEXT NOT NULL DEFAULT '',
  token_ct       BLOB,
  expose_web     INTEGER NOT NULL DEFAULT 1,
  expose_git     INTEGER NOT NULL DEFAULT 1,
  expose_mcp     INTEGER NOT NULL DEFAULT 0,
  expose_pages   INTEGER NOT NULL DEFAULT 0,
  expose_api     INTEGER NOT NULL DEFAULT 0,
  generation     INTEGER NOT NULL DEFAULT 0,
  changed_at     INTEGER NOT NULL DEFAULT 0,
  changed_by     TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO tunnel_config (id) VALUES (1);
CREATE TABLE IF NOT EXISTS tunnel_sessions (
  token_hash  TEXT PRIMARY KEY REFERENCES sessions(token_hash) ON DELETE CASCADE,
  person_id   INTEGER NOT NULL,
  host        TEXT NOT NULL,
  enroll_only INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tunnel_enroll (
  code_hash  TEXT PRIMARY KEY,
  person_id  INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  host       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tunnel_log (
  id        INTEGER PRIMARY KEY,
  at        INTEGER NOT NULL,
  ip        TEXT NOT NULL,
  country   TEXT NOT NULL DEFAULT '',
  class     TEXT NOT NULL,
  method    TEXT NOT NULL,
  status    INTEGER NOT NULL,
  signed_in INTEGER NOT NULL DEFAULT 0,
  who       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS tunnel_log_at ON tunnel_log(at);`)

	sealedColumns = append(sealedColumns, sealedColumn{
		what: "tunnel token", table: "tunnel_config", idCol: "id", ctCol: "token_ct", aad: tunnelTokenAAD,
	}, sealedColumn{
		what: "Tailscale auth key", table: "tunnel_config", idCol: "id", ctCol: "ts_authkey_ct", aad: tailscaleKeyAAD,
	})
}

// migrateTunnelTailscale adds the provider choice and Tailscale's settings
// to tunnel_config, and the tailnet login to the log.
func migrateTunnelTailscale(ctx context.Context, tx *sql.Tx) error {
	for _, c := range []struct{ table, col, def string }{
		{"tunnel_config", "provider", `TEXT NOT NULL DEFAULT 'cloudflare'`},
		{"tunnel_config", "ts_mode", `TEXT NOT NULL DEFAULT 'private'`},
		{"tunnel_config", "ts_hostname", `TEXT NOT NULL DEFAULT 'atelier'`},
		{"tunnel_config", "ts_authkey_ct", `BLOB`},
		{"tunnel_config", "ts_node", `TEXT NOT NULL DEFAULT ''`},
		{"tunnel_log", "tailnet", `TEXT NOT NULL DEFAULT ''`},
	} {
		ok, err := hasColumn(ctx, tx, c.table, c.col)
		if err != nil {
			return err
		}
		if !ok {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE `+c.table+` ADD COLUMN `+c.col+` `+c.def); err != nil {
				return err
			}
		}
	}
	return nil
}

// TunnelLogKeep is how long the "who's connected" log is kept.
const TunnelLogKeep = 7 * 24 * time.Hour

func tunnelTokenAAD(id int64) []byte  { return []byte("atelier/tunnel-token/v1") }
func tailscaleKeyAAD(id int64) []byte { return []byte("atelier/tailscale-authkey/v1") }

// Remote access providers and Tailscale's two modes, as stored.
const (
	TunnelCloudflare   = "cloudflare"
	TunnelTailscale    = "tailscale"
	TailscalePrivate   = "private" // tailscale serve: the owner's own devices
	TailscaleFunnel    = "funnel"  // tailscale funnel: the internet
	tailscaleHostnameD = "atelier"
)

// TunnelConfig is the remote-access setting.
type TunnelConfig struct {
	Enabled       bool
	Provider      string // "cloudflare" or "tailscale"
	Mode          string // Cloudflare: "quick" or "named"
	Hostname      string // named: the public hostname set up in Cloudflare
	PagesHostname string // named: an optional second hostname for Pages
	HasToken      bool
	TSMode        string // Tailscale: "private" or "funnel"
	TSHostname    string // Tailscale: the node's name on the tailnet
	HasTSKey      bool   // an auth key is stored (until the node has signed in)
	TSNode        string // Tailscale: the node's DNS name once signed in, e.g. atelier.tail1234.ts.net
	ExposeWeb     bool
	ExposeGit     bool
	ExposeMCP     bool
	ExposePages   bool
	ExposeAPI     bool
	Generation    int64 // bumped on every change, so a running server notices
	ChangedAt     time.Time
	ChangedBy     string
}

const tunnelCols = `enabled, mode, hostname, pages_hostname, token_ct IS NOT NULL AND length(token_ct) > 0,
  expose_web, expose_git, expose_mcp, expose_pages, expose_api, generation, changed_at, changed_by,
  provider, ts_mode, ts_hostname, ts_authkey_ct IS NOT NULL AND length(ts_authkey_ct) > 0, ts_node`

// Tailscale reports whether Tailscale is the chosen provider.
func (c TunnelConfig) Tailscale() bool { return c.Provider == TunnelTailscale }

// Named reports whether this is a Cloudflare named tunnel (the only kind
// with hostnames of the owner's own).
func (c TunnelConfig) Named() bool { return !c.Tailscale() && c.Mode == "named" }

// TailnetOnly reports whether only the owner's own devices can reach it.
func (c TunnelConfig) TailnetOnly() bool { return c.Tailscale() && c.TSMode != TailscaleFunnel }

// APIMode is the mode as clients see it: quick, named, tailscale-private or
// tailscale-funnel.
func (c TunnelConfig) APIMode() string {
	switch {
	case c.Tailscale() && c.TSMode == TailscaleFunnel:
		return "tailscale-funnel"
	case c.Tailscale():
		return "tailscale-private"
	case c.Mode == "":
		return "quick"
	}
	return c.Mode
}

// TunnelConfig reads the setting (off, quick, web and git exposed when
// nothing was ever saved).
func (s *Store) TunnelConfig(ctx context.Context) (TunnelConfig, error) {
	var c TunnelConfig
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT `+tunnelCols+` FROM tunnel_config WHERE id=1`).Scan(
		&c.Enabled, &c.Mode, &c.Hostname, &c.PagesHostname, &c.HasToken,
		&c.ExposeWeb, &c.ExposeGit, &c.ExposeMCP, &c.ExposePages, &c.ExposeAPI, &c.Generation, &at, &c.ChangedBy,
		&c.Provider, &c.TSMode, &c.TSHostname, &c.HasTSKey, &c.TSNode)
	if errors.Is(err, sql.ErrNoRows) {
		return TunnelConfig{Provider: TunnelCloudflare, Mode: "quick", TSMode: TailscalePrivate, TSHostname: tailscaleHostnameD,
			ExposeWeb: true, ExposeGit: true}, nil
	}
	if at > 0 {
		c.ChangedAt = time.Unix(at, 0)
	}
	return c, err
}

// TunnelGeneration is a cheap read for the server's watch loop.
func (s *Store) TunnelGeneration(ctx context.Context) (gen int64, enabled bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT generation, enabled FROM tunnel_config WHERE id=1`).Scan(&gen, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return gen, enabled, err
}

// SaveTunnelConfig stores everything but the on/off switch, the Tailscale
// auth key and the node's name. token is the new named-tunnel token: nil
// keeps the stored one, "" removes it.
func (s *Store) SaveTunnelConfig(ctx context.Context, sealer SecretSealer, c TunnelConfig, token *string, by string) error {
	if c.Mode != "quick" && c.Mode != "named" {
		return errors.New("tunnel: unknown mode")
	}
	if c.Provider == "" {
		c.Provider = TunnelCloudflare
	}
	if c.TSMode == "" {
		c.TSMode = TailscalePrivate
	}
	if c.TSHostname == "" {
		c.TSHostname = tailscaleHostnameD
	}
	if c.Provider != TunnelCloudflare && c.Provider != TunnelTailscale {
		return errors.New("tunnel: unknown provider")
	}
	if c.TSMode != TailscalePrivate && c.TSMode != TailscaleFunnel {
		return errors.New("tunnel: unknown Tailscale mode")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tunnel_config (id) VALUES (1)`); err != nil {
			return err
		}
		if token != nil {
			var ct []byte
			if t := strings.TrimSpace(*token); t != "" {
				if sealer == nil {
					return errNoSealer
				}
				var err error
				if ct, err = sealer.Seal(tunnelTokenAAD(1), []byte(t)); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE tunnel_config SET token_ct=? WHERE id=1`, ct); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE tunnel_config SET mode=?, hostname=?, pages_hostname=?, expose_web=?, expose_git=?,
			expose_mcp=?, expose_pages=?, expose_api=?, provider=?, ts_mode=?, ts_hostname=?,
			generation=generation+1, changed_at=?, changed_by=? WHERE id=1`,
			c.Mode, c.Hostname, c.PagesHostname, c.ExposeWeb, c.ExposeGit, c.ExposeMCP, c.ExposePages, c.ExposeAPI,
			c.Provider, c.TSMode, c.TSHostname, now(), by)
		return err
	})
}

// SetTailscaleAuthKey stores the Tailscale auth key, sealed ("" removes it).
func (s *Store) SetTailscaleAuthKey(ctx context.Context, sealer SecretSealer, key, by string) error {
	var ct []byte
	if k := strings.TrimSpace(key); k != "" {
		if sealer == nil {
			return errNoSealer
		}
		var err error
		if ct, err = sealer.Seal(tailscaleKeyAAD(1), []byte(k)); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE tunnel_config SET ts_authkey_ct=?, generation=generation+1, changed_at=?, changed_by=? WHERE id=1`,
		ct, now(), by)
	return err
}

// TailscaleAuthKey opens the Tailscale auth key. Never log or render it.
func (s *Store) TailscaleAuthKey(ctx context.Context, sealer SecretSealer) (string, error) {
	if sealer == nil {
		return "", errNoSealer
	}
	var ct []byte
	if err := s.db.QueryRowContext(ctx, `SELECT ts_authkey_ct FROM tunnel_config WHERE id=1`).Scan(&ct); err != nil {
		return "", notFound(err)
	}
	if len(ct) == 0 {
		return "", ErrNotFound
	}
	pt, err := sealer.Open(tailscaleKeyAAD(1), ct)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// TailscaleSignedIn records the node's DNS name once it has joined the
// tailnet and forgets the auth key, which has done its job. The generation
// isn't bumped: nothing has to restart.
func (s *Store) TailscaleSignedIn(ctx context.Context, node string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tunnel_config SET ts_node=?, ts_authkey_ct=NULL WHERE id=1`, strings.ToLower(node))
	return err
}

// TailscaleForget clears the node's name and any stored auth key (after
// "Forget this node").
func (s *Store) TailscaleForget(ctx context.Context, by string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tunnel_config SET ts_node='', ts_authkey_ct=NULL, generation=generation+1,
		changed_at=?, changed_by=? WHERE id=1`, now(), by)
	return err
}

// TunnelToken opens the named-tunnel token. Never log or render it.
func (s *Store) TunnelToken(ctx context.Context, sealer SecretSealer) (string, error) {
	if sealer == nil {
		return "", errNoSealer
	}
	var ct []byte
	if err := s.db.QueryRowContext(ctx, `SELECT token_ct FROM tunnel_config WHERE id=1`).Scan(&ct); err != nil {
		return "", notFound(err)
	}
	if len(ct) == 0 {
		return "", ErrNotFound
	}
	pt, err := sealer.Open(tunnelTokenAAD(1), ct)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// SetTunnelEnabled flips the switch. Turning it off also signs out every
// session started through the tunnel and forgets unused enrolment links, in
// the same transaction. It returns how many remote sessions were revoked.
func (s *Store) SetTunnelEnabled(ctx context.Context, on bool, by string) (revoked int64, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tunnel_config (id) VALUES (1)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tunnel_config SET enabled=?, generation=generation+1, changed_at=?, changed_by=? WHERE id=1`,
			on, now(), by); err != nil {
			return err
		}
		if on {
			return nil
		}
		var e error
		revoked, e = revokeRemoteSessions(ctx, tx)
		return e
	})
	return revoked, err
}

func revokeRemoteSessions(ctx context.Context, tx *sql.Tx) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash IN (SELECT token_hash FROM tunnel_sessions)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	for _, q := range []string{`DELETE FROM tunnel_sessions`, `DELETE FROM tunnel_enroll`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return 0, err
		}
	}
	return n, nil
}

// TunnelForceOff turns remote access off after a restore (or from the shell).
func (s *Store) TunnelForceOff(ctx context.Context, by string) error {
	_, err := s.SetTunnelEnabled(ctx, false, by)
	return err
}

// ---------- remote sessions ----------

// RemoteSession is a session started through the tunnel.
type RemoteSession struct {
	PersonID   int64
	Host       string // the tunnel address it was started on
	EnrollOnly bool   // may only add a passkey (from a one-time link)
}

// MarkRemoteSession records that a session was started through the tunnel.
func (s *Store) MarkRemoteSession(ctx context.Context, tok string, personID int64, host string, enrollOnly bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO tunnel_sessions (token_hash, person_id, host, enroll_only, created_at)
		VALUES (?, ?, ?, ?, ?)`, Hash(tok), personID, strings.ToLower(host), enrollOnly, now())
	return err
}

// RemoteSessionFor returns the remote marking of a valid session.
func (s *Store) RemoteSessionFor(ctx context.Context, tok string) (RemoteSession, bool) {
	var rs RemoteSession
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT t.person_id, t.host, t.enroll_only, s.expires_at FROM tunnel_sessions t
		JOIN sessions s ON s.token_hash = t.token_hash WHERE t.token_hash=?`, Hash(tok)).Scan(&rs.PersonID, &rs.Host, &rs.EnrollOnly, &exp)
	if err != nil || exp <= now() {
		return RemoteSession{}, false
	}
	return rs, true
}

// RemoteSessionCount is how many remote sessions are live.
func (s *Store) RemoteSessionCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tunnel_sessions t JOIN sessions s ON s.token_hash = t.token_hash
		WHERE s.expires_at > ?`, now()).Scan(&n)
	return n, err
}

// ---------- one-time passkey enrolment links ----------

// CreateTunnelEnroll makes a one-time code that lets a browser on the tunnel
// address add a passkey for that address.
func (s *Store) CreateTunnelEnroll(ctx context.Context, personID int64, host string, ttl time.Duration) (string, error) {
	code := RandomToken(20)
	t := now()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tunnel_enroll WHERE expires_at < ?`, t); err != nil {
		return "", err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tunnel_enroll (code_hash, person_id, host, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		Hash(code), personID, strings.ToLower(host), t, t+int64(ttl.Seconds()))
	return code, err
}

// CheckTunnelEnroll reports whether a code is valid for host, without using it.
func (s *Store) CheckTunnelEnroll(ctx context.Context, code, host string) bool {
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM tunnel_enroll WHERE code_hash=? AND host=?`,
		Hash(code), strings.ToLower(host)).Scan(&exp)
	return err == nil && exp > now()
}

// UseTunnelEnroll consumes a code for host and returns whose it was.
func (s *Store) UseTunnelEnroll(ctx context.Context, code, host string) (int64, bool) {
	var person, exp int64
	err := s.db.QueryRowContext(ctx, `DELETE FROM tunnel_enroll WHERE code_hash=? AND host=? RETURNING person_id, expires_at`,
		Hash(code), strings.ToLower(host)).Scan(&person, &exp)
	if err != nil || exp <= now() {
		return 0, false
	}
	return person, true
}

// ---------- who's connected ----------

// TunnelLogEntry is one request that came through the tunnel.
type TunnelLogEntry struct {
	At       time.Time
	IP       string
	Country  string
	Class    string // web, git, mcp, pages, api, registry, other
	Method   string
	Status   int
	SignedIn bool
	Who      string
	// Tailnet is the Tailscale login of the visitor's device (private
	// mode). It says whose device it was; it is not an Atelier sign-in.
	Tailnet string
}

// AddTunnelLog stores a batch of entries.
func (s *Store) AddTunnelLog(ctx context.Context, es []TunnelLogEntry) error {
	if len(es) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, e := range es {
			if _, err := tx.ExecContext(ctx, `INSERT INTO tunnel_log (at, ip, country, class, method, status, signed_in, who, tailnet)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, e.At.Unix(), e.IP, e.Country, e.Class, e.Method, e.Status, e.SignedIn, e.Who, e.Tailnet); err != nil {
				return err
			}
		}
		return nil
	})
}

// TunnelLog returns the newest entries first.
func (s *Store) TunnelLog(ctx context.Context, limit int) ([]TunnelLogEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, ip, country, class, method, status, signed_in, who, tailnet FROM tunnel_log
		ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TunnelLogEntry
	for rows.Next() {
		var e TunnelLogEntry
		var at int64
		if err := rows.Scan(&at, &e.IP, &e.Country, &e.Class, &e.Method, &e.Status, &e.SignedIn, &e.Who, &e.Tailnet); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TunnelVisitors counts distinct client IPs since t.
func (s *Store) TunnelVisitors(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT ip) FROM tunnel_log WHERE at >= ?`, since.Unix()).Scan(&n)
	return n, err
}

// PruneTunnelLog deletes entries older than TunnelLogKeep.
func (s *Store) PruneTunnelLog(ctx context.Context, nowT time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tunnel_log WHERE at < ?`, nowT.Add(-TunnelLogKeep).Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
