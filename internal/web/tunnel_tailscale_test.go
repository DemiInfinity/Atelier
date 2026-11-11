//go:build !windows

package web

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
	"atelier/internal/tunnel"
	"atelier/internal/tunnel/tunneltest"
)

const (
	tsWebKey  = "tskey-auth-kTESTONLY11CNTRL-notARealKeyJustForTests0000"
	tsWebHost = "atelier." + tunneltest.Tailnet
)

// tsFix is tunnelFix with fake tailscale and tailscaled binaries.
type tsWebFix struct {
	*tunnelFix
	ts string // the fakes' scratch directory
}

func newTSWebFix(t *testing.T) *tsWebFix {
	t.Helper()
	f := newTunnelFix(t)
	dir := t.TempDir()
	bin, daemon, err := tunneltest.FakeTailscale(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.a.cfg.TailscaleBin, f.a.cfg.TailscaledBin = bin, daemon
	m := f.a.tunnel()
	m.ts.Bin, m.ts.DaemonBin, m.ts.Poll = bin, daemon, 20*time.Millisecond
	x := &tsWebFix{tunnelFix: f, ts: dir}
	x.set(t, "goodkey", tsWebKey)
	x.set(t, "caps", `"https":null`)
	return x
}

func (f *tsWebFix) set(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.ts, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *tsWebFix) got(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.ts, name))
	return strings.TrimSpace(string(b))
}

func (f *tsWebFix) pid(name string) int {
	n, _ := strconv.Atoi(f.got(name))
	return n
}

// save posts the settings form as the owner at home.
func (f *tsWebFix) save(t *testing.T, mode, key string) *httptest.ResponseRecorder {
	t.Helper()
	v := url.Values{"provider": {"tailscale"}, "mode": {"quick"}, "ts_mode": {mode}, "ts_hostname": {"atelier"},
		"expose_web": {"1"}, "expose_git": {"1"}}
	if key != "" {
		v.Set("ts_authkey", key)
	}
	return f.lan(t, "POST", "/settings/remote", v)
}

func (f *tsWebFix) connected() bool {
	m := f.a.tunnel()
	return m.remoteHost() == tsWebHost && m.status().Phase == tunnel.PhaseConnected
}

// turnOnTS saves Tailscale settings with the key and turns remote access on.
func (f *tsWebFix) turnOnTS(t *testing.T, mode string) {
	t.Helper()
	if rec := f.save(t, mode, tsWebKey); rec.Code != http.StatusSeeOther {
		t.Fatalf("save = %d\n%s", rec.Code, rec.Body)
	}
	if rec := f.lan(t, "POST", "/settings/remote/on", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("turn on = %d\n%s", rec.Code, rec.Body)
	}
	waitUntil(t, "the ts.net address", f.connected)
}

// tsRemote sends a request through the remote listener's handler as
// tailscaled's serve proxy delivers it: the Host kept, X-Forwarded-For set
// by tailscaled, no Cloudflare headers.
func (f *tsWebFix) tsRemote(t *testing.T, method, target string, body url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(body.Encode())
	}
	req := httptest.NewRequest(method, target, rd)
	req.Host = tsWebHost
	req.RemoteAddr = "127.0.0.1:50123"
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("X-Forwarded-For", "100.64.0.5")
	req.Header.Set("X-Forwarded-Host", tsWebHost)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.a.tunnel().ServeHTTP(rec, req)
	return rec
}

func (f *tsWebFix) tsSession(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	tok, err := f.a.db.CreateSessionFor(ctx, f.owner.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.a.db.MarkRemoteSession(ctx, tok, f.owner.ID, tsWebHost, false); err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestTailscaleOffByDefault(t *testing.T) {
	f := newTSWebFix(t)
	ctx := context.Background()
	c, err := f.a.db.TunnelConfig(ctx)
	if err != nil || c.Enabled || c.Provider != "cloudflare" || c.Tailscale() || c.TSMode != "private" || c.TSHostname != "atelier" || c.HasTSKey || c.TSNode != "" {
		t.Fatalf("fresh config = %+v, %v", c, err)
	}
	f.a.tunnel().apply(ctx) // what the server does at start
	if f.a.tunnel().ts.Running() || f.got("daemon.args") != "" {
		t.Fatal("tailscaled started on a fresh install")
	}
	body := f.lan(t, "GET", "/settings/remote", nil).Body.String()
	for _, want := range []string{`name="provider" value="cloudflare"`, `name="provider" value="tailscale"`, "Private (your tailnet only)", "Public (Funnel)", `name="ts_authkey"`, `name="expose_git"`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page lacks %q", want)
		}
	}
	// Choosing Tailscale doesn't turn anything on, and it can't be turned on
	// before there is an auth key.
	if rec := f.save(t, "private", ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("save = %d", rec.Code)
	}
	if c, _ := f.a.db.TunnelConfig(ctx); !c.Tailscale() || c.Enabled {
		t.Fatalf("after choosing Tailscale: %+v", c)
	}
	rec := f.lan(t, "POST", "/settings/remote/on", url.Values{})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "auth key") {
		t.Errorf("turn on without a key = %d", rec.Code)
	}
	if c, _ := f.a.db.TunnelConfig(ctx); c.Enabled || f.a.tunnel().ts.Running() {
		t.Error("turned on without a key")
	}
	for _, bad := range []url.Values{
		{"provider": {"wireguard"}, "mode": {"quick"}},
		{"provider": {"tailscale"}, "mode": {"quick"}, "ts_mode": {"everyone"}},
		{"provider": {"tailscale"}, "mode": {"quick"}, "ts_mode": {"private"}, "ts_hostname": {"not a host"}},
		{"provider": {"tailscale"}, "mode": {"quick"}, "ts_mode": {"private"}, "ts_authkey": {"hunter2"}},
	} {
		if rec := f.lan(t, "POST", "/settings/remote", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("bad settings %v accepted: %d", bad, rec.Code)
		}
	}
}

func TestTailscalePrivateThenFunnelThenOff(t *testing.T) {
	f := newTSWebFix(t)
	ctx := context.Background()
	a, m := f.a, f.a.tunnel()
	apiTok, _ := appTestDevice(t, a, f.owner.ID, "MacBook")

	// The key is sealed, never echoed.
	rec := f.save(t, "private", tsWebKey)
	if rec.Code != http.StatusSeeOther || strings.Contains(rec.Body.String(), "kTESTONLY") {
		t.Fatalf("save = %d", rec.Code)
	}
	var ct []byte
	_ = a.db.DB().QueryRow(`SELECT ts_authkey_ct FROM tunnel_config`).Scan(&ct)
	if len(ct) == 0 || strings.Contains(string(ct), "tskey") || strings.Contains(string(ct), "kTESTONLY") {
		t.Fatal("the auth key isn't stored sealed")
	}
	if got, err := a.db.TailscaleAuthKey(ctx, a.secretSealer()); err != nil || got != tsWebKey {
		t.Fatalf("key round trip = %v", err)
	}
	page := f.lan(t, "GET", "/settings/remote", nil).Body.String()
	if !strings.Contains(page, "Saved (encrypted)") || strings.Contains(page, "kTESTONLY") {
		t.Error("settings page should say a key is saved and never show it")
	}
	if off := api(t, a, "GET", "/remote", apiTok, nil, 200); off["on"] != false || off["mode"] != "tailscale-private" || off["address"] != nil {
		t.Errorf("app API while off = %v", off)
	}

	// On: tailscaled, sign-in, serve, the address.
	if rec := f.lan(t, "POST", "/settings/remote/on", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("turn on = %d\n%s", rec.Code, rec.Body)
	}
	waitUntil(t, "the ts.net address", f.connected)
	if got := f.got("serve.args"); got != "serve --https=443 http://"+m.addr {
		t.Errorf("serve args = %q", got)
	}
	if host, _, _ := net.SplitHostPort(m.addr); host != "127.0.0.1" {
		t.Errorf("remote listener on %s, want loopback", m.addr)
	}
	if !strings.Contains(f.got("daemon.args"), "--statedir="+filepath.Join(a.cfg.DataDir, "tailscale")) {
		t.Errorf("tailscaled args = %q", f.got("daemon.args"))
	}
	if m.sup.Running() || f.tunnelFix.dir == "" {
		t.Error("cloudflared is running beside Tailscale")
	}
	if _, err := os.Stat(filepath.Join(f.tunnelFix.dir, "args")); err == nil {
		t.Error("cloudflared was started")
	}
	// Signed in: the node is remembered and the key is gone.
	waitUntil(t, "the node to be remembered", func() bool { c, _ := a.db.TunnelConfig(ctx); return c.TSNode == tsWebHost })
	if c, _ := a.db.TunnelConfig(ctx); c.HasTSKey {
		t.Error("the auth key is still stored after signing in")
	}
	page = f.lan(t, "GET", "/settings/remote", nil).Body.String()
	for _, want := range []string{"https://" + tsWebHost, "Connected to your tailnet as", "Turn off remote access", "Forget this node", "only devices signed in to your tailnet"} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page while on lacks %q", want)
		}
	}
	if strings.Contains(page, "kTESTONLY") {
		t.Error("the auth key is on the page (helper output?)")
	}
	if home := f.lan(t, "GET", "/", nil).Body.String(); !strings.Contains(home, "Remote access is on") || !strings.Contains(home, tsWebHost) {
		t.Error("banner missing while on")
	}
	if on := api(t, a, "GET", "/remote", apiTok, nil, 200); on["on"] != true || on["mode"] != "tailscale-private" || on["address"] != "https://"+tsWebHost {
		t.Errorf("app API while on = %v", on)
	}
	wantKeys(t, "remote", api(t, a, "GET", "/remote", apiTok, nil, 200), "on", "mode", "address", "exposed", "connected")

	// A real request over the remote listener's socket, and a remote session.
	resp, err := http.Get("http://" + m.addr + "/signin")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET over the remote listener = %v, %v", resp, err)
	}
	resp.Body.Close()
	remoteTok := f.tsSession(t)
	if rec := f.tsRemote(t, "GET", "/settings/tokens", nil, map[string]string{"Cookie": sessionCookie + "=" + remoteTok}); rec.Code != 200 {
		t.Fatalf("remote session not accepted: %d", rec.Code)
	}
	// A session made for another address (the old Cloudflare one) isn't.
	if rec := f.tsRemote(t, "GET", "/settings/tokens", nil, map[string]string{"Cookie": sessionCookie + "=" + f.remoteSession(t)}); rec.Code != http.StatusSeeOther {
		t.Errorf("session for another host accepted: %d", rec.Code)
	}

	// Changing what's exposed doesn't restart anything, although the key is gone now.
	daemonPID := f.pid("daemon.pid")
	c, _ := a.db.TunnelConfig(ctx)
	c.ExposeAPI = true
	if err := a.db.SaveTunnelConfig(ctx, nil, c, nil, "test"); err != nil {
		t.Fatal(err)
	}
	m.apply(ctx)
	if f.pid("daemon.pid") != daemonPID || !f.connected() {
		t.Error("changing what's exposed restarted tailscaled")
	}

	// Switch to Funnel while the tailnet doesn't allow it: a plain message.
	if rec := f.save(t, "funnel", ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("switch to funnel = %d", rec.Code)
	}
	waitUntil(t, "the funnel message", func() bool { return m.status().LastError == tunnel.MsgTailscaleFunnel })
	page = f.lan(t, "GET", "/settings/remote", nil).Body.String()
	if !strings.Contains(page, "Funnel isn&#39;t allowed for this machine") || !strings.Contains(page, "Access controls") {
		t.Error("settings page doesn't explain how to enable Funnel")
	}
	// Enabled in the admin console: public.
	f.set(t, "caps", `"https":null,"funnel":null`)
	waitUntil(t, "funnel", func() bool { return f.connected() && strings.HasPrefix(f.got("serve.args"), "funnel ") })
	if got := f.got("serve.args"); got != "funnel --https=443 http://"+m.addr {
		t.Errorf("funnel args = %q", got)
	}
	if on := api(t, a, "GET", "/remote", apiTok, nil, 200); on["mode"] != "tailscale-funnel" || on["address"] != "https://"+tsWebHost {
		t.Errorf("app API in funnel mode = %v", on)
	}
	if page := f.lan(t, "GET", "/settings/remote", nil).Body.String(); !strings.Contains(page, "anyone on the internet can open this address") {
		t.Error("settings page doesn't say the address is public")
	}

	// Off: everything dies, the listener closes, remote sessions are revoked.
	remoteTok = f.tsSession(t)
	pids := []int{f.pid("daemon.pid"), f.pid("daemon.child"), f.pid("serve.child")}
	for _, p := range pids {
		if !pidAlive(p) {
			t.Fatalf("helper pid %d not running (%v)", p, pids)
		}
	}
	start := time.Now()
	if rec := f.lan(t, "POST", "/settings/remote/off", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("turn off = %d", rec.Code)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("turning off took %s", d)
	}
	waitUntil(t, "tailscaled, serve and their children to die", func() bool {
		for _, p := range pids {
			if pidAlive(p) {
				return false
			}
		}
		return true
	})
	if _, err := net.DialTimeout("tcp", m.addr, 200*time.Millisecond); err == nil {
		t.Error("remote listener still accepting connections")
	}
	if _, ok := a.db.SessionPerson(ctx, remoteTok); ok {
		t.Error("remote session survived turning off")
	}
	if _, ok := a.db.SessionPerson(ctx, f.lanSess); !ok {
		t.Error("the LAN session was signed out too")
	}
	if c, _ := a.db.TunnelConfig(ctx); c.Enabled || c.TSNode != tsWebHost {
		t.Errorf("after off: %+v", c)
	}
	if rec := f.tsRemote(t, "GET", "/signin", nil, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("remote handler after off = %d", rec.Code)
	}
	if off := api(t, a, "GET", "/remote", apiTok, nil, 200); off["on"] != false || off["address"] != nil || off["mode"] != "tailscale-funnel" {
		t.Errorf("app API after off = %v", off)
	}
	m.apply(ctx) // the watch loop doesn't bring it back
	time.Sleep(100 * time.Millisecond)
	if m.ts.Running() || pidAlive(f.pid("daemon.pid")) {
		t.Error("restarted after the kill switch")
	}
	// On again needs no key: the node is signed in.
	if rec := f.lan(t, "POST", "/settings/remote/on", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("turn on again = %d\n%s", rec.Code, rec.Body)
	}
	waitUntil(t, "the same address again", f.connected)
	if f.got("up.runs") != "1" {
		t.Errorf("tailscale up ran %s times; the key is needed once", f.got("up.runs"))
	}

	// Back to Cloudflare: Tailscale stops, cloudflared takes over.
	if rec := f.lan(t, "POST", "/settings/remote", url.Values{"provider": {"cloudflare"}, "mode": {"quick"}, "expose_web": {"1"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("back to cloudflare = %d", rec.Code)
	}
	waitUntil(t, "cloudflared's address", func() bool { return m.remoteHost() == tunnelTestHost })
	if m.ts.Running() || pidAlive(f.pid("daemon.pid")) {
		t.Error("tailscaled still running under the Cloudflare provider")
	}
	if on := api(t, a, "GET", "/remote", apiTok, nil, 200); on["mode"] != "quick" {
		t.Errorf("app API mode back on Cloudflare = %v", on["mode"])
	}
}

func TestTailscaleHTTPSNotEnabledMessage(t *testing.T) {
	f := newTSWebFix(t)
	f.set(t, "caps", ``)
	f.save(t, "private", tsWebKey)
	if rec := f.lan(t, "POST", "/settings/remote/on", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("turn on = %d", rec.Code)
	}
	m := f.a.tunnel()
	waitUntil(t, "the HTTPS message", func() bool { return m.status().LastError == tunnel.MsgTailscaleHTTPS })
	page := f.lan(t, "GET", "/settings/remote", nil).Body.String()
	if !strings.Contains(page, "HTTPS certificates aren&#39;t enabled for your tailnet") || !strings.Contains(page, "Enable HTTPS") {
		t.Error("settings page doesn't explain how to enable HTTPS")
	}
	rec := f.lan(t, "GET", "/settings/remote/status", nil)
	if !strings.Contains(rec.Body.String(), "HTTPS certificates") || strings.Contains(rec.Body.String(), `"phase":"connected"`) {
		t.Errorf("status JSON = %s", rec.Body)
	}
	if f.got("serve.args") != "" {
		t.Error("serve was started without HTTPS")
	}
	f.set(t, "caps", `"https":null`)
	waitUntil(t, "serve after enabling", f.connected)
}

func TestTailscaleClientIPAndIdentity(t *testing.T) {
	f := newTSWebFix(t)
	ctx := context.Background()
	a := f.a
	a.cfg.TrustProxy = true // as in production behind nginx
	f.turnOnTS(t, "private")

	// A device on the tailnet: tailscaled names its owner and its tailnet
	// address. A CF-Connecting-IP the visitor made up counts for nothing.
	rec := f.tsRemote(t, "GET", "/settings/tokens", nil, map[string]string{
		"Tailscale-User-Login": "zoe@example.com", "Tailscale-User-Name": "Zoe", "Tailscale-Headers-Info": "https://tailscale.com/s/serve-headers",
		"CF-Connecting-IP": "198.51.100.9", "CF-IPCountry": "PT"})
	// Being on the tailnet is not being signed in to Atelier.
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin") {
		t.Fatalf("tailnet identity without a session = %d %q, want a redirect to sign in", rec.Code, rec.Header().Get("Location"))
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a session was made from a tailnet identity")
	}
	a.tunnel().flush(ctx)
	log, _ := a.db.TunnelLog(ctx, 10)
	if len(log) == 0 || log[0].IP != "100.64.0.5" || log[0].Tailnet != "zoe@example.com" || log[0].SignedIn || log[0].Who != "" || log[0].Country != "" {
		t.Fatalf("log entry = %+v", log)
	}
	page := f.lan(t, "GET", "/settings/remote", nil).Body.String()
	if !strings.Contains(page, "zoe@example.com") || !strings.Contains(page, "100.64.0.5") || strings.Contains(page, "198.51.100.9") {
		t.Error("who's connected should show the tailnet login and tailscaled's client address only")
	}
	// Password sign-in stays off, tailnet or not.
	rec = f.tsRemote(t, "POST", "/signin", url.Values{"password": {"correct horse"}}, map[string]string{"Tailscale-User-Login": "zoe@example.com"})
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Errorf("remote password sign-in = %d", rec.Code)
	}
	// A non-ASCII login arrives RFC 2047 encoded.
	probe := httptest.NewRequest("GET", "/", nil)
	probe.Header.Set("Tailscale-User-Login", "=?utf-8?q?z=C3=B6e@example.com?=")
	if got := tailscaleLogin(probe); got != "zöe@example.com" {
		t.Errorf("decoded login = %q", got)
	}
	probe.Header.Set("Tailscale-Funnel-Request", "?1")
	if got := tailscaleLogin(probe); got != "" {
		t.Errorf("a Funnel request has no tailnet login, got %q", got)
	}

	// The request the app sees: the ts.net address, https, tailscaled's client
	// IP; X-Forwarded-Host can't move it elsewhere.
	seen := httptest.NewRequest("GET", "/", nil)
	seen.RemoteAddr = "127.0.0.1:5000"
	seen.Header.Set("X-Forwarded-For", "100.64.0.7")
	seen.Header.Set("CF-Connecting-IP", "198.51.100.9")
	ip := tailscaleClientIP(seen)
	seen.Header.Set("X-Forwarded-Host", "evil.example")
	for _, h := range proxyHeaders {
		seen.Header.Del(h) // what ServeHTTP does
	}
	seen = seen.WithContext(context.WithValue(seen.Context(), remoteKey{}, &remoteReq{Host: tsWebHost, IP: ip}))
	if got := a.clientIP(seen); got != "100.64.0.7" {
		t.Errorf("remote clientIP = %q", got)
	}
	if b := a.baseURL(seen); b != "https://"+tsWebHost || !a.isHTTPS(seen) {
		t.Errorf("remote baseURL = %q", b)
	}
	// Passkeys belong to the ts.net address.
	if rp, origin, ok := a.passkeyRP(seen); rp != tsWebHost || origin != "https://"+tsWebHost || !ok {
		t.Errorf("remote passkey RP = %q %q %v", rp, origin, ok)
	}
	rec = f.tsRemote(t, "POST", "/auth/passkey/begin", url.Values{}, nil)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("passkey begin through Tailscale = %d %s", rec.Code, rec.Body)
	}
	// More than one X-Forwarded-For value isn't tailscaled's doing: ignored.
	odd := httptest.NewRequest("GET", "/", nil)
	odd.RemoteAddr = "127.0.0.1:5000"
	odd.Header.Set("X-Forwarded-For", "203.0.113.1, 100.64.0.7")
	if got := tailscaleClientIP(odd); got != "127.0.0.1" {
		t.Errorf("a list in X-Forwarded-For gave %q", got)
	}

	// Rate limits key on tailscaled's client address, not on anything else
	// the visitor sends.
	refs := "/zoe/x.git/info/refs"
	for i := 0; i < 8; i++ {
		f.tsRemote(t, "GET", refs, nil, map[string]string{"X-Forwarded-For": "100.64.0.50", "CF-Connecting-IP": fmt.Sprintf("198.51.100.%d", i)})
	}
	if rec := f.tsRemote(t, "GET", refs, nil, map[string]string{"X-Forwarded-For": "100.64.0.50", "CF-Connecting-IP": "198.51.100.77"}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("9th failure from one tailnet address = %d, want 429", rec.Code)
	}
	if rec := f.tsRemote(t, "GET", refs, nil, map[string]string{"X-Forwarded-For": "100.64.0.51"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("another address = %d, want 401", rec.Code)
	}

	// Private means private: a Funnel request is refused outright.
	if rec := f.tsRemote(t, "GET", "/signin", nil, map[string]string{"Tailscale-Funnel-Request": "?1", "X-Forwarded-For": "203.0.113.80"}); rec.Code != http.StatusForbidden {
		t.Errorf("Funnel request in private mode = %d, want 403", rec.Code)
	}

	// The main listener trusts none of it.
	lan := httptest.NewRequest("GET", "/signin", nil)
	lan.RemoteAddr = "192.168.1.20:4000"
	lan.Header.Set("Tailscale-User-Login", "zoe@example.com")
	lan.Header.Set("CF-Connecting-IP", "198.51.100.1")
	if got := a.clientIP(lan); got != "192.168.1.20" {
		t.Errorf("LAN clientIP = %q", got)
	}
	if _, ok := remoteOf(lan); ok {
		t.Error("a LAN request is marked remote")
	}
	lrec := httptest.NewRecorder()
	a.ServeHTTP(lrec, lan)
	if lrec.Code != 200 {
		t.Errorf("LAN request with a made-up Tailscale header = %d (it is simply ignored there)", lrec.Code)
	}
	a.tunnel().flush(ctx)
	before, _ := a.db.TunnelLog(ctx, 500)
	for _, e := range before {
		if e.IP == "192.168.1.20" {
			t.Error("a main-listener request reached the remote log")
		}
	}
	// And from loopback, tailscaled's headers mean someone pointed tailscale
	// serve at the main port: refused.
	for name, hdr := range map[string]map[string]string{
		"identity header": {"Tailscale-User-Login": "zoe@example.com"},
		"funnel header":   {"Tailscale-Funnel-Request": "?1"},
		"forwarded host":  {"X-Forwarded-Host": tsWebHost, "X-Forwarded-For": "100.64.0.5"},
	} {
		mis := httptest.NewRequest("GET", "/signin", nil)
		mis.RemoteAddr = "127.0.0.1:4000"
		for k, v := range hdr {
			mis.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, mis)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("tailscale-proxied request on the main listener (%s) = %d, want 421", name, rec.Code)
		}
	}
	mis := httptest.NewRequest("GET", "/signin", nil)
	mis.RemoteAddr = "127.0.0.1:4000"
	mis.Host = tsWebHost
	mrec := httptest.NewRecorder()
	a.ServeHTTP(mrec, mis)
	if mrec.Code != http.StatusMisdirectedRequest {
		t.Errorf("ts.net Host on the main listener from loopback = %d, want 421", mrec.Code)
	}
	// The container's own health check is untouched.
	hc := httptest.NewRequest("GET", "/healthz", nil)
	hc.RemoteAddr = "127.0.0.1:4000"
	hc.Host = "127.0.0.1:3000"
	hrec := httptest.NewRecorder()
	a.ServeHTTP(hrec, hc)
	if hrec.Code != 200 {
		t.Errorf("health check from loopback = %d", hrec.Code)
	}
}

// With Cloudflare as the provider, Tailscale's headers and X-Forwarded-For
// mean nothing on the remote listener either.
func TestTunnelCloudflareIgnoresTailscaleHeaders(t *testing.T) {
	f := newTunnelFix(t)
	ctx := context.Background()
	f.turnOn(t)
	f.remote(t, "GET", "/signin", nil, map[string]string{"Tailscale-User-Login": "mallory@example.com", "X-Forwarded-For": "100.64.0.99"})
	f.a.tunnel().flush(ctx)
	log, _ := f.a.db.TunnelLog(ctx, 5)
	if len(log) == 0 || log[0].IP != "203.0.113.7" || log[0].Tailnet != "" {
		t.Errorf("log entry under Cloudflare = %+v", log)
	}
}

func TestTailscaleEnrollLinkAndForget(t *testing.T) {
	f := newTSWebFix(t)
	ctx := context.Background()
	a := f.a
	f.turnOnTS(t, "private")

	// The one-time passkey link is for the ts.net address.
	rec := f.lan(t, "POST", "/settings/remote/enroll-link", url.Values{})
	body := rec.Body.String()
	i := strings.Index(body, "https://"+tsWebHost+"/settings/remote/enroll?code=")
	if rec.Code != 200 || i < 0 {
		t.Fatalf("enroll link = %d", rec.Code)
	}
	link := body[i : i+strings.IndexByte(body[i:], '"')]
	u, _ := url.Parse(strings.ReplaceAll(link, "&amp;", "&"))
	code := u.Query().Get("code")
	rec = f.tsRemote(t, "POST", "/settings/remote/enroll", url.Values{"code": {code}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/passkeys" {
		t.Fatalf("enroll accept = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	var sess *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			sess = c
		}
	}
	if sess == nil || !sess.Secure {
		t.Fatalf("enrol session cookie = %+v", sess)
	}
	cookie := map[string]string{"Cookie": sessionCookie + "=" + sess.Value}
	if pk := f.tsRemote(t, "GET", "/settings/passkeys", nil, cookie); pk.Code != 200 || !strings.Contains(pk.Body.String(), tsWebHost) {
		t.Errorf("passkeys page through Tailscale = %d", pk.Code)
	}
	if rs, ok := a.db.RemoteSessionFor(ctx, sess.Value); !ok || rs.Host != tsWebHost || !rs.EnrollOnly {
		t.Errorf("enrol session = %+v, %v", rs, ok)
	}

	// Forgetting the node can't be done through the tunnel…
	rs := map[string]string{"Cookie": sessionCookie + "=" + f.tsSession(t)}
	if rec := f.tsRemote(t, "POST", "/settings/remote/forget-node", url.Values{}, rs); rec.Code != http.StatusForbidden {
		t.Errorf("forget through the tunnel = %d, want 403", rec.Code)
	}
	if !f.connected() {
		t.Fatal("remote access went off")
	}
	// …and from home it turns remote access off, signs out and deletes the state.
	stateDir := filepath.Join(a.cfg.DataDir, "tailscale")
	if err := os.WriteFile(filepath.Join(stateDir, "tailscaled.state"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon := f.pid("daemon.pid")
	rec = f.lan(t, "POST", "/settings/remote/forget-node", url.Values{})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/remote?forgot=1" {
		t.Fatalf("forget = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if _, err := os.Stat(filepath.Join(f.ts, "loggedout")); err != nil {
		t.Error("tailscale logout wasn't run")
	}
	if _, err := os.Stat(stateDir); err == nil {
		t.Error("the node's state survived")
	}
	c, _ := a.db.TunnelConfig(ctx)
	if c.Enabled || c.TSNode != "" || c.HasTSKey {
		t.Errorf("after forget: %+v", c)
	}
	if a.tunnel().ts.Running() || pidAlive(daemon) || a.tunnel().on.Load() {
		t.Error("still running after forget")
	}
	if _, ok := a.db.SessionPerson(ctx, sess.Value); ok {
		t.Error("remote sessions survived forgetting the node")
	}
	page := f.lan(t, "GET", "/settings/remote?forgot=1", nil).Body.String()
	if !strings.Contains(page, "signed out of your tailnet") || strings.Contains(page, "Connected to your tailnet as") {
		t.Error("settings page after forget")
	}
	if rec := f.lan(t, "POST", "/settings/remote/on", url.Values{}); rec.Code != http.StatusBadRequest {
		t.Errorf("turn on after forget = %d, want 400 (needs a new key)", rec.Code)
	}
}

func TestTailscaleSecondPersonCantSeeOrChange(t *testing.T) {
	f := newTSWebFix(t)
	ctx := context.Background()
	a := f.a
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := a.db.CreateSessionFor(ctx, sam.ID, time.Hour)
	as := map[string]string{"Cookie": sessionCookie + "=" + tok, "Sec-Fetch-Site": "same-origin"}
	f.save(t, "private", tsWebKey)
	before, _ := a.db.TunnelConfig(ctx)

	for _, r := range []struct {
		m, p string
		body url.Values
	}{
		{"GET", "/settings/remote", nil},
		{"GET", "/settings/remote/status", nil},
		{"POST", "/settings/remote", url.Values{"provider": {"tailscale"}, "mode": {"quick"}, "ts_mode": {"funnel"}, "ts_hostname": {"sams"}, "ts_authkey": {"tskey-auth-sams-own-key"}}},
		{"POST", "/settings/remote", url.Values{"provider": {"cloudflare"}, "mode": {"quick"}}},
		{"POST", "/settings/remote/on", url.Values{}},
		{"POST", "/settings/remote/off", url.Values{}},
		{"POST", "/settings/remote/forget-node", url.Values{}},
		{"POST", "/settings/remote/enroll-link", url.Values{}},
	} {
		rec := do(t, a, r.m, r.p, r.body, as)
		if rec.Code != http.StatusNotFound {
			t.Errorf("sam %s %s = %d, want 404", r.m, r.p, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "tailnet") || strings.Contains(rec.Body.String(), "ts.net") {
			t.Errorf("sam %s %s shows Tailscale details", r.m, r.p)
		}
	}
	after, _ := a.db.TunnelConfig(ctx)
	if after != before {
		t.Fatalf("sam changed the settings:\n%+v\n%+v", before, after)
	}
	if got, err := a.db.TailscaleAuthKey(ctx, a.secretSealer()); err != nil || got != tsWebKey {
		t.Error("sam replaced the auth key")
	}
	// The app API: sam's device is refused too.
	samTok, _ := appTestDevice(t, a, sam.ID, "Sam's laptop")
	wantErr(t, "sam reads remote", api(t, a, "GET", "/remote", samTok, nil, 403), "forbidden")
	wantErr(t, "sam turns it on", api(t, a, "POST", "/remote", samTok, map[string]any{"on": true}, 403), "forbidden")
	wantErr(t, "sam asks who", api(t, a, "GET", "/remote/who", samTok, nil, 403), "forbidden")
	wantErr(t, "sam asks for a link", api(t, a, "POST", "/remote/link", samTok, nil, 403), "forbidden")

	// While it is on: no banner, no address for sam; a tailnet login doesn't help.
	if rec := f.lan(t, "POST", "/settings/remote/on", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("owner turns on = %d", rec.Code)
	}
	waitUntil(t, "address", f.connected)
	if body := do(t, a, "GET", "/", nil, as).Body.String(); strings.Contains(body, "Remote access is on") || strings.Contains(body, tsWebHost) {
		t.Error("sam sees the banner or the address")
	}
	if rec := do(t, a, "POST", "/settings/remote/forget-node", url.Values{}, as); rec.Code != http.StatusNotFound || !f.connected() {
		t.Errorf("sam forgot the node: %d", rec.Code)
	}
	wantErr(t, "sam turns it off", api(t, a, "POST", "/remote", samTok, map[string]any{"on": false}, 403), "forbidden")
	if !f.connected() {
		t.Error("sam turned it off")
	}
}

func TestTailscaleShellOffIsNoticed(t *testing.T) {
	f := newTSWebFix(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.turnOnTS(t, "private")
	m := f.a.tunnel()
	go m.run(ctx) // the server's watch loop
	daemon, child := f.pid("daemon.pid"), f.pid("serve.child")
	// What `atelier tunnel off` does: write the setting, kill from the pid files.
	if _, err := f.a.db.SetTunnelEnabled(ctx, false, "shell"); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{tunnel.TailscaleServePIDFile: "tailscale", tunnel.TailscaledPIDFile: "tailscaled"} {
		if _, err := tunnel.KillFromPIDFile(filepath.Join(f.a.cfg.DataDir, file), want); err != nil {
			t.Fatalf("kill from %s: %v", file, err)
		}
	}
	waitUntil(t, "server to notice", func() bool { return !m.on.Load() && !m.ts.Running() })
	waitUntil(t, "helpers to die", func() bool { return !pidAlive(daemon) && !pidAlive(child) })
	time.Sleep(150 * time.Millisecond)
	if pidAlive(f.pid("daemon.pid")) {
		t.Error("tailscaled came back after the shell turned it off")
	}
}

func TestTailscaleConnectNeedsFunnel(t *testing.T) {
	f := newTSWebFix(t)
	f.turnOnTS(t, "private")
	base, why := f.a.connectBaseFor(store.ConnectSettings{Enabled: true, ExposeTunnel: true})
	if base != "" || !strings.Contains(why, "private") {
		t.Errorf("Connect base in private mode = %q, %q", base, why)
	}
	f.set(t, "caps", `"https":null,"funnel":null`)
	f.save(t, "funnel", "")
	waitUntil(t, "funnel", func() bool { return f.connected() && strings.HasPrefix(f.got("serve.args"), "funnel ") })
	if base, why := f.a.connectBaseFor(store.ConnectSettings{Enabled: true, ExposeTunnel: true}); base != "https://"+tsWebHost {
		t.Errorf("Connect base with Funnel = %q, %q", base, why)
	}
}
