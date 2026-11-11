package web

// Remote access: Settings → Remote access turns the built-in tunnel on and
// off (ticket #7). The switch lives in the database and is off by default;
// a server watches it every second, so `atelier tunnel off` from the shell
// takes effect at once too. See tunnel_remote.go for what remote requests may
// do, and internal/tunnel for the helper process.

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"atelier/internal/store"
	"atelier/internal/tunnel"
)

func init() {
	registerFeature(feature{
		name: "tunnel",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/remote", a.authed(a.tunnelOwner(a.tunnelPage)))
			m.HandleFunc("POST /settings/remote", a.authed(a.tunnelOwner(a.tunnelSave)))
			m.HandleFunc("GET /settings/remote/status", a.authed(a.tunnelOwner(a.tunnelStatusJSON)))
			m.HandleFunc("POST /settings/remote/on", a.authed(a.tunnelOwner(a.tunnelOn)))
			m.HandleFunc("POST /settings/remote/off", a.authed(a.tunnelOwner(a.tunnelOff)))
			m.HandleFunc("POST /settings/remote/enroll-link", a.authed(a.tunnelOwner(a.tunnelEnrollLink)))
			m.HandleFunc("POST /settings/remote/forget-node", a.authed(a.tunnelOwner(a.tunnelForgetNode)))
			// The one-time passkey link is opened through the tunnel, signed out.
			m.HandleFunc("GET /settings/remote/enroll", a.tunnelEnrollPage)
			m.HandleFunc("POST /settings/remote/enroll", a.tunnelEnrollAccept)
		},
		start: func(ctx context.Context, a *App) { a.tunnel().run(ctx) },
	})
	registerTemplateFuncs(func(a *App) template.FuncMap {
		return template.FuncMap{"tunnelBanner": a.tunnelBanner}
	})
}

const (
	tunnelEnrollTTL     = 15 * time.Minute
	tunnelEnrollSession = 15 * time.Minute
	tunnelLogShown      = 100
	tunnelPIDFile       = "tunnel.pid"
	tailscaleStateDir   = "tailscale" // under the data directory; the node's identity, never backed up
)

// ---------- the manager ----------

type tunnelManager struct {
	a   *App
	sup *tunnel.Supervisor // cloudflared
	ts  *tunnel.Tailscale  // tailscaled and tailscale serve/funnel; one provider runs at a time

	mu      sync.Mutex // serialises apply and kill
	srv     *http.Server
	addr    string // the remote listener's bound address
	applied int64  // config generation applied
	procKey string // what the running helper was started with
	tsKey   string // hash of the Tailscale auth key it was started with ("" = none)
	killed  bool   // turned off here; stays off until turned on again
	lastErr string

	on       atomic.Bool
	cfg      atomic.Pointer[store.TunnelConfig]
	inflight atomic.Int64
	conns    atomic.Int64

	logMu   sync.Mutex
	pending []store.TunnelLogEntry
}

var tunnelManagers sync.Map // *App → *tunnelManager

func (a *App) tunnel() *tunnelManager {
	if m, ok := tunnelManagers.Load(a); ok {
		return m.(*tunnelManager)
	}
	m := &tunnelManager{a: a, sup: &tunnel.Supervisor{
		Bin: a.cfg.TunnelBin, Provider: tunnel.Cloudflare{},
		PIDFile: filepath.Join(a.cfg.DataDir, tunnelPIDFile), Logf: log.Printf,
	}}
	// Never restart a crashed helper once the setting says off.
	m.sup.Allow = func() bool {
		_, enabled, err := a.db.TunnelGeneration(context.Background())
		return err == nil && enabled
	}
	m.ts = &tunnel.Tailscale{
		Bin: a.cfg.TailscaleBin, DaemonBin: a.cfg.TailscaledBin,
		StateDir: filepath.Join(a.cfg.DataDir, tailscaleStateDir), PIDDir: a.cfg.DataDir,
		Logf: log.Printf, Allow: m.sup.Allow,
		// Signed in: remember the node's name and drop the auth key, which
		// isn't needed again (the node's own key is in the state directory).
		OnNode: func(dns string) {
			if err := a.db.TailscaleSignedIn(context.Background(), dns); err != nil {
				log.Printf("tunnel: remember tailscale node: %v", err)
			}
		},
	}
	actual, _ := tunnelManagers.LoadOrStore(a, m)
	return actual.(*tunnelManager)
}

// run applies the setting, then watches it until ctx ends.
func (m *tunnelManager) run(ctx context.Context) {
	if n, err := m.a.db.PruneTunnelLog(ctx, time.Now()); err == nil && n > 0 {
		log.Printf("tunnel: pruned %d old log entries", n)
	}
	m.apply(ctx)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastPrune := time.Now()
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			m.flush(context.Background())
			return
		case <-tick.C:
		}
		if gen, enabled, err := m.a.db.TunnelGeneration(ctx); err == nil {
			m.mu.Lock()
			changed := gen != m.applied || enabled != m.on.Load()
			m.mu.Unlock()
			if changed {
				m.apply(ctx)
			}
		}
		m.flush(ctx)
		if time.Since(lastPrune) > time.Hour {
			lastPrune = time.Now()
			_, _ = m.a.db.PruneTunnelLog(ctx, time.Now())
		}
	}
}

// apply makes the running state match the database.
func (m *tunnelManager) apply(ctx context.Context) {
	c, err := m.a.db.TunnelConfig(ctx)
	if err != nil {
		log.Printf("tunnel: read setting: %v", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applied = c.Generation
	if !c.Enabled || m.killed {
		m.stopLocked()
		return
	}
	m.cfg.Store(&c)
	if c.Tailscale() {
		m.applyTailscaleLocked(ctx, c)
		return
	}
	m.ts.Stop()
	token := ""
	if c.Mode == tunnel.ModeNamed {
		if token, err = m.a.db.TunnelToken(ctx, m.a.secretSealer()); err != nil {
			m.lastErr = "The tunnel token couldn't be read: " + err.Error()
			m.stopLocked()
			return
		}
	}
	if err := m.listenLocked(); err != nil {
		m.lastErr = err.Error()
		m.stopLocked()
		return
	}
	key := c.Mode + "\x00" + c.Hostname + "\x00" + store.Hash(token) + "\x00" + m.addr
	if m.sup.Running() && key == m.procKey {
		m.on.Store(true) // only what's exposed changed
		return
	}
	set := tunnel.Settings{Mode: c.Mode, Origin: "http://" + m.addr, Token: token, Hostname: c.Hostname}
	if err := m.sup.Start(set); err != nil {
		m.lastErr = err.Error()
		m.stopLocked()
		return
	}
	m.procKey, m.lastErr = key, ""
	m.on.Store(true)
	log.Printf("tunnel: remote access on (%s), remote listener %s", c.Mode, m.addr)
}

// applyTailscaleLocked is apply for the Tailscale provider.
func (m *tunnelManager) applyTailscaleLocked(ctx context.Context, c store.TunnelConfig) {
	authKey := ""
	if c.HasTSKey {
		var err error
		if authKey, err = m.a.db.TailscaleAuthKey(ctx, m.a.secretSealer()); err != nil {
			m.lastErr = "The Tailscale auth key couldn't be read: " + err.Error()
			m.stopLocked()
			return
		}
	}
	if err := m.listenLocked(); err != nil {
		m.lastErr = err.Error()
		m.stopLocked()
		return
	}
	mode := tunnel.ModeTailscalePrivate
	if c.TSMode == store.TailscaleFunnel {
		mode = tunnel.ModeTailscaleFunnel
	}
	key := "tailscale\x00" + mode + "\x00" + c.TSHostname + "\x00" + m.addr
	keyHash := ""
	if authKey != "" {
		keyHash = store.Hash(authKey)
	}
	// A key that went away was used up by signing in: nothing to restart.
	if m.ts.Running() && key == m.procKey && (keyHash == "" || keyHash == m.tsKey) {
		m.on.Store(true)
		return
	}
	m.sup.Stop()
	set := tunnel.Settings{Mode: mode, Origin: "http://" + m.addr, Token: authKey, Hostname: c.TSHostname}
	if err := m.ts.Start(set); err != nil {
		m.lastErr = err.Error()
		m.stopLocked()
		return
	}
	m.procKey, m.tsKey, m.lastErr = key, keyHash, ""
	m.on.Store(true)
	log.Printf("tunnel: remote access on (%s), remote listener %s", mode, m.addr)
}

// status is the running helper's state, whichever provider it is.
func (m *tunnelManager) status() tunnel.Status {
	if m.ts.Running() {
		return m.ts.Status()
	}
	return m.sup.Status()
}

// listenLocked opens the remote listener if it isn't open. It must be a
// loopback address: only the helper, inside the container, may reach it.
func (m *tunnelManager) listenLocked() error {
	if m.srv != nil {
		return nil
	}
	host, _, err := net.SplitHostPort(m.a.cfg.TunnelListen)
	if err != nil {
		return errors.New("ATELIER_TUNNEL_LISTEN must be host:port")
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("ATELIER_TUNNEL_LISTEN must be a loopback address such as 127.0.0.1:3001")
	}
	ln, err := net.Listen("tcp", m.a.cfg.TunnelListen)
	if err != nil {
		return errors.New("the remote listener couldn't start: " + err.Error())
	}
	srv := &http.Server{
		Handler:           m,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		ConnState: func(_ net.Conn, s http.ConnState) {
			switch s {
			case http.StateNew:
				m.conns.Add(1)
			case http.StateClosed, http.StateHijacked:
				m.conns.Add(-1)
			}
		},
	}
	m.srv, m.addr = srv, ln.Addr().String()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("tunnel: remote listener: %v", err)
		}
	}()
	return nil
}

// stopLocked kills the helper and closes the remote listener with every
// connection on it. Nothing tunnelled survives it.
func (m *tunnelManager) stopLocked() {
	wasOn := m.on.Swap(false)
	m.sup.Stop()
	m.ts.Stop()
	if m.srv != nil {
		_ = m.srv.Close()
		m.srv = nil
	}
	m.procKey, m.tsKey = "", ""
	if wasOn {
		log.Print("tunnel: remote access off")
	}
}

// kill is the kill switch: stop now, and don't start again until turned on.
func (m *tunnelManager) kill() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.killed = true
	m.stopLocked()
}

func (m *tunnelManager) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
}

// StopRemoteAccess kills the tunnel helpers and closes the remote listener
// as the server exits, without touching the setting: it comes back as it
// was on the next start. The watch loop does the same when its context
// ends, but the process may be gone before it gets there.
func (a *App) StopRemoteAccess() { a.tunnel().shutdown() }

// remoteHost is the tunnel address's host, once known.
func (m *tunnelManager) remoteHost() string {
	addr := m.status().Address
	return strings.TrimPrefix(addr, "https://")
}

// ---------- who's connected ----------

func (m *tunnelManager) record(e store.TunnelLogEntry) {
	m.logMu.Lock()
	if len(m.pending) < 5000 {
		m.pending = append(m.pending, e)
	}
	m.logMu.Unlock()
}

func (m *tunnelManager) flush(ctx context.Context) {
	m.logMu.Lock()
	batch := m.pending
	m.pending = nil
	m.logMu.Unlock()
	if err := m.a.db.AddTunnelLog(ctx, batch); err != nil {
		log.Printf("tunnel: write log: %v", err)
	}
}

// ---------- settings ----------

// tunnelOwner limits a handler to the personal owner; anyone else gets a 404.
func (a *App) tunnelOwner(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who := a.actor(r)
		if who.Kind != ActorOwner || !a.isOwnerPerson(r.Context(), who.PersonID) {
			a.notFound(w, r)
			return
		}
		h(w, r)
	}
}

type tunnelView struct {
	Config     store.TunnelConfig
	Status     tunnel.Status
	On         bool
	Address    string
	Phase      string
	LastError  string
	Remote     bool // this page is being viewed through the tunnel
	Listen     string
	Binary     string
	HaveBinary bool
	// The Tailscale provider's binaries.
	TSBinary, TSDaemonBinary string
	HaveTailscale            bool
	Helper                   string // whose output the log at the bottom is
	Forgot                   string // "1" after forgetting the node, "offline" if Tailscale couldn't be told
	InFlight                 int64
	Conns                    int64
	Visitors                 int
	Sessions                 int
	Log                      []store.TunnelLogEntry
	EnrollLink               string
	// ConnectExposed: the Connect class (connect.go) is stored with Connect's settings.
	ConnectExposed bool
	Saved          bool
	Error          string
}

func (a *App) tunnelData(r *http.Request) (tunnelView, error) {
	ctx := r.Context()
	m := a.tunnel()
	c, err := a.db.TunnelConfig(ctx)
	if err != nil {
		return tunnelView{}, err
	}
	st := m.status()
	v := tunnelView{Config: c, Status: st, On: c.Enabled, Address: st.Address, Phase: st.Phase,
		Remote: isRemote(r), Listen: a.cfg.TunnelListen, Binary: a.cfg.TunnelBin,
		TSBinary: a.cfg.TailscaleBin, TSDaemonBinary: a.cfg.TailscaledBin, Helper: "cloudflared",
		InFlight: m.inflight.Load(), Conns: m.conns.Load()}
	if c.Tailscale() {
		v.Helper = "tailscaled and tailscale"
	}
	v.HaveTailscale = a.haveTailscale()
	m.mu.Lock()
	v.LastError = m.lastErr
	if v.LastError == "" {
		v.LastError = st.LastError
	}
	m.mu.Unlock()
	if c.Enabled && v.Phase == tunnel.PhaseOff {
		v.Phase = tunnel.PhaseStarting
	}
	_, err = exec.LookPath(a.cfg.TunnelBin)
	v.HaveBinary = err == nil
	v.Visitors, _ = a.db.TunnelVisitors(ctx, time.Now().Add(-5*time.Minute))
	v.Sessions, _ = a.db.RemoteSessionCount(ctx)
	m.flush(ctx)
	v.Log, _ = a.db.TunnelLog(ctx, tunnelLogShown)
	if cs, err := a.db.ConnectSettings(ctx); err == nil {
		v.ConnectExposed = cs.ExposeTunnel
	}
	return v, nil
}

func (a *App) tunnelRender(w http.ResponseWriter, r *http.Request, status int, v tunnelView) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	a.render(w, r, "tunnel_settings", page{Title: "Remote access", Nav: "tokens", Error: v.Error, Data: v})
}

func (a *App) tunnelPage(w http.ResponseWriter, r *http.Request) {
	v, err := a.tunnelData(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v.Saved = r.URL.Query().Get("saved") != ""
	v.Forgot = r.URL.Query().Get("forgot")
	a.tunnelRender(w, r, http.StatusOK, v)
}

func (a *App) tunnelStatusJSON(w http.ResponseWriter, r *http.Request) {
	v, err := a.tunnelData(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"on": v.On, "phase": v.Phase, "address": v.Address, "error": v.LastError,
		"in_flight": v.InFlight, "connections": v.Conns, "visitors": v.Visitors, "sessions": v.Sessions,
	})
}

// tunnelLANOnly refuses changes made through the tunnel itself: from there
// you can only turn it off.
func (a *App) tunnelLANOnly(w http.ResponseWriter, r *http.Request) bool {
	if !isRemote(r) {
		return true
	}
	v, _ := a.tunnelData(r)
	v.Error = "Remote access can only be changed from your home network. From here you can turn it off."
	a.tunnelRender(w, r, http.StatusForbidden, v)
	return false
}

func (a *App) tunnelSave(w http.ResponseWriter, r *http.Request) {
	if !a.tunnelLANOnly(w, r) {
		return
	}
	ctx := r.Context()
	c, err := a.db.TunnelConfig(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	fail := func(msg string) {
		v, _ := a.tunnelData(r)
		v.Error = msg
		a.tunnelRender(w, r, http.StatusBadRequest, v)
	}
	// Forms from before the provider choice (and scripts) send no provider:
	// that keeps the one in use.
	if p := r.FormValue("provider"); p != "" {
		if p != store.TunnelCloudflare && p != store.TunnelTailscale {
			fail("Choose Cloudflare or Tailscale.")
			return
		}
		c.Provider = p
	}
	if v := r.FormValue("mode"); v != "" || !c.Tailscale() {
		c.Mode = v
	}
	if c.Mode != tunnel.ModeQuick && c.Mode != tunnel.ModeNamed {
		fail("Choose a quick tunnel or a named tunnel.")
		return
	}
	if v := r.FormValue("ts_mode"); v != "" {
		if v != store.TailscalePrivate && v != store.TailscaleFunnel {
			fail("Choose Private or Public for Tailscale.")
			return
		}
		c.TSMode = v
	}
	if _, sent := r.Form["ts_hostname"]; sent {
		h, ok := tunnel.NormalizeTailscaleHostname(r.FormValue("ts_hostname"))
		if !ok {
			fail("The Tailscale hostname should be one word of letters, digits and hyphens, like atelier.")
			return
		}
		c.TSHostname = h
	}
	var tsKey *string
	if k := strings.TrimSpace(r.FormValue("ts_authkey")); k != "" {
		if len(k) > 512 || strings.ContainsAny(k, " \t\r\n") || !strings.HasPrefix(k, "tskey-") {
			fail("That doesn't look like a Tailscale auth key. It starts with tskey-auth- and comes from Settings → Keys in the Tailscale admin console.")
			return
		}
		tsKey = &k
	} else if r.FormValue("remove_ts_authkey") != "" {
		empty := ""
		tsKey = &empty
	}
	c.Hostname, c.PagesHostname = "", ""
	if h := strings.TrimSpace(r.FormValue("hostname")); h != "" {
		norm, ok := tunnel.NormalizeHostname(h)
		if !ok {
			fail("The hostname should look like git.example.com.")
			return
		}
		c.Hostname = norm
	}
	if h := strings.TrimSpace(r.FormValue("pages_hostname")); h != "" {
		norm, ok := tunnel.NormalizeHostname(h)
		if !ok || norm == c.Hostname {
			fail("The Pages hostname should be a different name, like pages.example.com.")
			return
		}
		c.PagesHostname = norm
	}
	on := func(k string) bool { return r.FormValue(k) != "" }
	c.ExposeWeb, c.ExposeGit, c.ExposeMCP, c.ExposePages, c.ExposeAPI =
		on("expose_web"), on("expose_git"), on("expose_mcp"), on("expose_pages"), on("expose_api")
	var token *string
	if t := strings.TrimSpace(r.FormValue("token")); t != "" {
		if len(t) > 4096 || strings.ContainsAny(t, " \t\r\n") {
			fail("That doesn't look like a tunnel token. Copy the long value after --token in the Cloudflare dashboard.")
			return
		}
		token = &t
	} else if on("remove_token") {
		empty := ""
		token = &empty
	}
	if err := a.db.SaveTunnelConfig(ctx, a.secretSealer(), c, token, a.actor(r).Name); err != nil {
		a.fail(w, r, err)
		return
	}
	if tsKey != nil {
		if err := a.db.SetTailscaleAuthKey(ctx, a.secretSealer(), *tsKey, a.actor(r).Name); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	if cs, err := a.db.ConnectSettings(ctx); err == nil && cs.ExposeTunnel != on("expose_connect") {
		cs.ExposeTunnel = on("expose_connect")
		if err := a.db.SaveConnectSettings(ctx, cs, a.actor(r).Name); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	a.tunnel().apply(ctx)
	http.Redirect(w, r, "/settings/remote?saved=1", http.StatusSeeOther)
}

func (a *App) tunnelOn(w http.ResponseWriter, r *http.Request) {
	if !a.tunnelLANOnly(w, r) {
		return
	}
	problem, err := a.tunnelTurnOn(r.Context(), a.actor(r).Name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if problem != "" {
		v, _ := a.tunnelData(r)
		v.Error = problem
		a.tunnelRender(w, r, http.StatusBadRequest, v)
		return
	}
	http.Redirect(w, r, "/settings/remote", http.StatusSeeOther)
}

// tunnelTurnOn turns remote access on for by (the web switch and the app
// API). problem says why it can't be turned on yet, in a sentence.
func (a *App) tunnelTurnOn(ctx context.Context, by string) (problem string, err error) {
	c, err := a.db.TunnelConfig(ctx)
	if err != nil {
		return "", err
	}
	switch {
	case c.Tailscale():
		if !a.haveTailscale() {
			return "Tailscale isn't installed here (" + a.cfg.TailscaleBin + " and " + a.cfg.TailscaledBin + "). The Docker image includes both; set ATELIER_TAILSCALE and ATELIER_TAILSCALED when running elsewhere.", nil
		}
		if !c.HasTSKey && c.TSNode == "" {
			return "Tailscale needs an auth key the first time. Paste one below and save.", nil
		}
	default:
		if _, err := exec.LookPath(a.cfg.TunnelBin); err != nil {
			return "cloudflared isn't installed here (" + a.cfg.TunnelBin + "). The Docker image includes it; set ATELIER_CLOUDFLARED when running elsewhere.", nil
		}
		if c.Mode == tunnel.ModeNamed && (!c.HasToken || c.Hostname == "") {
			return "A named tunnel needs its token and public hostname. Save them first.", nil
		}
	}
	if !c.ExposeWeb && !c.ExposeGit && !c.ExposeMCP && !c.ExposePages && !c.ExposeAPI && !a.connectTunnelExposed(ctx) {
		return "Choose at least one thing to expose.", nil
	}
	m := a.tunnel()
	m.mu.Lock()
	m.killed = false
	m.mu.Unlock()
	if _, err := a.db.SetTunnelEnabled(ctx, true, by); err != nil {
		return "", err
	}
	m.apply(ctx)
	return "", nil
}

// haveTailscale reports whether both Tailscale binaries can be found.
func (a *App) haveTailscale() bool {
	_, err1 := exec.LookPath(a.cfg.TailscaleBin)
	_, err2 := exec.LookPath(a.cfg.TailscaledBin)
	return err1 == nil && err2 == nil
}

// tunnelForgetNode signs this server out of the tailnet and deletes the
// node's identity. If Tailscale is what remote access is running on, that
// turns it off first. The next start needs a new auth key.
func (a *App) tunnelForgetNode(w http.ResponseWriter, r *http.Request) {
	if !a.tunnelLANOnly(w, r) {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	m := a.tunnel()
	by := a.actor(r).Name
	c, err := a.db.TunnelConfig(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if c.Tailscale() && c.Enabled {
		m.kill()
		n, err := a.db.SetTunnelEnabled(ctx, false, by)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		log.Printf("tunnel: turned off by %s (forgetting the Tailscale node); %d remote session(s) revoked", by, n)
	}
	m.mu.Lock()
	loggedOut, ferr := m.ts.Forget(ctx)
	m.mu.Unlock()
	if ferr != nil {
		a.fail(w, r, ferr)
		return
	}
	if err := a.db.TailscaleForget(ctx, by); err != nil {
		a.fail(w, r, err)
		return
	}
	log.Printf("tunnel: Tailscale node forgotten by %s (signed out at Tailscale: %t)", by, loggedOut)
	q := "1"
	if !loggedOut {
		q = "offline"
	}
	http.Redirect(w, r, "/settings/remote?forgot="+q, http.StatusSeeOther)
}

// tunnelOff is the kill switch. It works from anywhere, including through
// the tunnel itself.
func (a *App) tunnelOff(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithoutCancel(r.Context())
	m := a.tunnel()
	by := a.actor(r).Name
	if isRemote(r) {
		// Answer first, then cut: closing the listener ends this connection too.
		m.on.Store(false)
		n, err := a.db.SetTunnelEnabled(ctx, false, by)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Connection", "close")
		if err != nil {
			logErr(r, err)
		}
		_, _ = w.Write([]byte("Remote access is off. This address no longer reaches your Atelier.\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		m.kill()
		log.Printf("tunnel: turned off by %s through the tunnel; %d remote session(s) revoked", by, n)
		return
	}
	m.kill()
	n, err := a.db.SetTunnelEnabled(ctx, false, by)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	log.Printf("tunnel: turned off by %s; %d remote session(s) revoked", by, n)
	next := safeNext(r.FormValue("next"))
	if next == "/" {
		next = "/settings/remote"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// tunnelEnrollLink makes a one-time link that lets a browser on the tunnel
// address add a passkey for it. Passkeys belong to one address, so the
// passkey you use at home doesn't work at the tunnel address, and password
// sign-in is off there.
func (a *App) tunnelEnrollLink(w http.ResponseWriter, r *http.Request) {
	if !a.tunnelLANOnly(w, r) {
		return
	}
	ctx := r.Context()
	host := a.tunnel().remoteHost()
	v, err := a.tunnelData(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !v.On || host == "" {
		v.Error = "Turn remote access on and wait for its address first."
		a.tunnelRender(w, r, http.StatusConflict, v)
		return
	}
	code, err := a.db.CreateTunnelEnroll(ctx, a.actor(r).PersonID, host, tunnelEnrollTTL)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v.EnrollLink = tunnelURL(host) + "/settings/remote/enroll?code=" + code
	a.tunnelRender(w, r, http.StatusOK, v)
}

// tunnelEnrollPage asks before using the link (so link previews can't use it up).
func (a *App) tunnelEnrollPage(w http.ResponseWriter, r *http.Request) {
	rm, ok := remoteOf(r)
	if !ok {
		a.notFound(w, r)
		return
	}
	code := r.URL.Query().Get("code")
	valid := code != "" && a.db.CheckTunnelEnroll(r.Context(), code, rm.Host)
	status := http.StatusOK
	if !valid {
		status = http.StatusNotFound
	}
	w.WriteHeader(status)
	a.render(w, r, "tunnel_enroll", page{Title: "Add a passkey", Bare: true,
		Data: map[string]any{"Code": code, "Valid": valid, "Host": rm.Host}})
}

func (a *App) tunnelEnrollAccept(w http.ResponseWriter, r *http.Request) {
	rm, ok := remoteOf(r)
	if !ok {
		a.notFound(w, r)
		return
	}
	ctx := r.Context()
	ip := a.clientIP(r)
	if !a.lim.allow(ip) {
		http.Error(w, "too many attempts, try again in a minute", http.StatusTooManyRequests)
		return
	}
	person, ok := a.db.UseTunnelEnroll(ctx, r.FormValue("code"), rm.Host)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		a.render(w, r, "tunnel_enroll", page{Title: "Add a passkey", Bare: true, Data: map[string]any{"Valid": false, "Host": rm.Host}})
		return
	}
	a.lim.reset(ip)
	tok, err := a.db.CreateSessionFor(ctx, person, tunnelEnrollSession)
	if err == nil {
		err = a.db.MarkRemoteSession(ctx, tok, person, rm.Host, true)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(tunnelEnrollSession.Seconds())})
	log.Printf("tunnel: one-time passkey link used from %s", ip)
	http.Redirect(w, r, "/settings/passkeys", http.StatusSeeOther)
}

// ---------- the banner ----------

type tunnelBannerData struct {
	Address string
	Phase   string
}

// tunnelBanner is shown across the app to the owner while remote access is
// on. accounts is page.Accounts: empty for anyone not signed in.
func (a *App) tunnelBanner(me store.Owner, accounts []store.Owner) *tunnelBannerData {
	if len(accounts) == 0 || me.ID == 0 || accounts[0].ID != me.ID {
		return nil
	}
	ctx := context.Background()
	_, enabled, err := a.db.TunnelGeneration(ctx)
	m := a.tunnel()
	if (err != nil || !enabled) && !m.on.Load() {
		return nil
	}
	if !a.isOwnerPerson(ctx, me.ID) {
		return nil
	}
	st := m.status()
	d := &tunnelBannerData{Address: st.Address, Phase: st.Phase}
	if d.Phase == tunnel.PhaseOff {
		d.Phase = tunnel.PhaseStarting
	}
	return d
}
