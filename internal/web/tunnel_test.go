//go:build !windows

package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"atelier/internal/store"
	"atelier/internal/tunnel"
)

const tunnelTestHost = "quiet-fox-lamp.trycloudflare.com"

// fakeCloudflaredWeb is a stand-in for cloudflared: it records its
// arguments, prints a quick-tunnel address and a "Registered" line, starts a
// child (to prove the whole process group is killed) and waits.
func fakeCloudflaredWeb(t *testing.T) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "cloudflared")
	script := `#!/bin/sh
echo "$@" > "` + dir + `/args"
echo "INF |  https://` + tunnelTestHost + `  |" >&2
echo "INF Registered tunnel connection connIndex=0 protocol=quic" >&2
sleep 600 &
echo $! > "` + dir + `/child"
wait
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

type tunnelFix struct {
	a       *App
	owner   store.Owner
	lanSess string
	dir     string // the fake cloudflared's scratch directory
}

func newTunnelFix(t *testing.T) *tunnelFix {
	t.Helper()
	a := newTestApp(t)
	bin, dir := fakeCloudflaredWeb(t)
	a.cfg.TunnelBin = bin
	a.cfg.TunnelListen = "127.0.0.1:0"
	a.cfg.PublicURL = "https://atelier.infinityhomelab.lan"
	t.Cleanup(func() { a.tunnel().shutdown() })
	rec := do(t, a, "POST", "/setup", url.Values{
		"username": {"zoe"}, "password": {"correct horse"}, "password_confirm": {"correct horse"},
	}, map[string]string{"Sec-Fetch-Site": "same-origin"})
	f := &tunnelFix{a: a, lanSess: sessionFrom(t, rec), dir: dir}
	f.owner, _ = a.db.PersonalOwner(context.Background())
	return f
}

// lan sends a request to the main listener as the signed-in owner.
func (f *tunnelFix) lan(t *testing.T, method, target string, body url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.a, method, target, body, map[string]string{"Cookie": sessionCookie + "=" + f.lanSess, "Sec-Fetch-Site": "same-origin"})
}

// remote sends a request through the remote listener's handler, as
// cloudflared would deliver it.
func (f *tunnelFix) remote(t *testing.T, method, target string, body url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(body.Encode())
	}
	req := httptest.NewRequest(method, target, rd)
	req.Host = tunnelTestHost
	req.RemoteAddr = "127.0.0.1:50123"
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("CF-Connecting-IP", "203.0.113.7")
	req.Header.Set("CF-IPCountry", "PT")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.a.tunnel().ServeHTTP(rec, req)
	return rec
}

func (f *tunnelFix) turnOn(t *testing.T) {
	t.Helper()
	if rec := f.lan(t, "POST", "/settings/remote/on", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("turn on = %d\n%s", rec.Code, rec.Body)
	}
	waitUntil(t, "tunnel address", func() bool { return f.a.tunnel().remoteHost() == tunnelTestHost })
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func pidAlive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// remoteSession signs the owner in through the tunnel, as a passkey would.
func (f *tunnelFix) remoteSession(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	tok, err := f.a.db.CreateSessionFor(ctx, f.owner.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.a.db.MarkRemoteSession(ctx, tok, f.owner.ID, tunnelTestHost, false); err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestTunnelOffByDefault(t *testing.T) {
	f := newTunnelFix(t)
	ctx := context.Background()
	c, err := f.a.db.TunnelConfig(ctx)
	if err != nil || c.Enabled || c.Mode != "quick" || !c.ExposeWeb || !c.ExposeGit || c.ExposeMCP || c.ExposePages || c.ExposeAPI {
		t.Fatalf("fresh config = %+v, %v", c, err)
	}
	f.a.tunnel().apply(ctx) // what the server does at start
	if f.a.tunnel().on.Load() || f.a.tunnel().sup.Running() {
		t.Fatal("tunnel running on a fresh install")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "args")); err == nil {
		t.Fatal("cloudflared was started")
	}
	rec := f.lan(t, "GET", "/settings/remote", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Turn on remote access") {
		t.Fatalf("settings page = %d", rec.Code)
	}
	if strings.Contains(f.lan(t, "GET", "/", nil).Body.String(), "Remote access is on") {
		t.Error("banner shown while off")
	}
	if rec := f.remote(t, "GET", "/signin", nil, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("remote handler while off = %d", rec.Code)
	}
}

func TestTunnelOnShowsAddressAndOffKills(t *testing.T) {
	f := newTunnelFix(t)
	ctx := context.Background()
	f.turnOn(t)
	m := f.a.tunnel()

	args, _ := os.ReadFile(filepath.Join(f.dir, "args"))
	if want := "tunnel --no-autoupdate --url http://" + m.addr; strings.TrimSpace(string(args)) != want {
		t.Errorf("cloudflared args = %q, want %q", args, want)
	}
	if host, _, _ := net.SplitHostPort(m.addr); host != "127.0.0.1" {
		t.Errorf("remote listener on %s, want loopback", m.addr)
	}
	body := f.lan(t, "GET", "/settings/remote", nil).Body.String()
	if !strings.Contains(body, "https://"+tunnelTestHost) || !strings.Contains(body, "Turn off remote access") {
		t.Error("settings page doesn't show the address and the kill switch")
	}
	home := f.lan(t, "GET", "/", nil).Body.String()
	if !strings.Contains(home, "Remote access is on") || !strings.Contains(home, tunnelTestHost) {
		t.Error("banner missing while on")
	}

	// A real request over the remote listener's socket.
	resp, err := http.Get("http://" + m.addr + "/signin")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET over the remote listener = %v, %v", resp, err)
	}
	resp.Body.Close()

	remoteTok := f.remoteSession(t)
	if rec := f.remote(t, "GET", "/settings/tokens", nil, map[string]string{"Cookie": sessionCookie + "=" + remoteTok}); rec.Code != 200 {
		t.Fatalf("remote session not accepted: %d", rec.Code)
	}
	pid := m.sup.Status().PID
	child, _ := strconv.Atoi(strings.TrimSpace(readFile(t, filepath.Join(f.dir, "child"))))
	if !pidAlive(pid) || !pidAlive(child) {
		t.Fatalf("helper not running: pid %d child %d", pid, child)
	}

	start := time.Now()
	rec := f.lan(t, "POST", "/settings/remote/off", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("turn off = %d", rec.Code)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("turning off took %s", d)
	}
	waitUntil(t, "helper and its child to die", func() bool { return !pidAlive(pid) && !pidAlive(child) })
	if _, err := net.DialTimeout("tcp", m.addr, 200*time.Millisecond); err == nil {
		t.Error("remote listener still accepting connections")
	}
	if _, ok := f.a.db.SessionPerson(ctx, remoteTok); ok {
		t.Error("remote session survived turning off")
	}
	if _, ok := f.a.db.SessionPerson(ctx, f.lanSess); !ok {
		t.Error("the LAN session was signed out too")
	}
	if c, _ := f.a.db.TunnelConfig(ctx); c.Enabled {
		t.Error("setting still on")
	}
	if strings.Contains(f.lan(t, "GET", "/", nil).Body.String(), "Remote access is on") {
		t.Error("banner still shown")
	}
	// The watch loop doesn't bring it back.
	m.apply(ctx)
	if m.sup.Running() {
		t.Error("restarted after the kill switch")
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	var b []byte
	waitUntil(t, p, func() bool { b, _ = os.ReadFile(p); return len(b) > 0 })
	return string(b)
}

func TestTunnelShellOffIsNoticed(t *testing.T) {
	f := newTunnelFix(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.turnOn(t)
	m := f.a.tunnel()
	go m.run(ctx) // the server's watch loop
	pid := m.sup.Status().PID
	// What `atelier tunnel off` does: write the setting, kill from the pid file.
	if _, err := f.a.db.SetTunnelEnabled(ctx, false, "shell"); err != nil {
		t.Fatal(err)
	}
	// (The watch loop may already have stopped it, so "nothing to kill" is fine.)
	if _, err := tunnel.KillFromPIDFile(filepath.Join(f.a.cfg.DataDir, tunnelPIDFile), "cloudflared"); err != nil {
		t.Fatalf("kill from pid file: %v", err)
	}
	waitUntil(t, "server to notice", func() bool { return !m.on.Load() && !m.sup.Running() })
	if pidAlive(pid) {
		t.Error("helper still alive")
	}
}

func TestTunnelRemoteExposure(t *testing.T) {
	f := newTunnelFix(t)
	ctx := context.Background()
	f.turnOn(t)

	for _, p := range []string{"/v2/", "/v2/zoe/app/manifests/latest", "/npm/left-pad", "/go/example.com/x/@v/list",
		"/api/v1/packages/zoe/generic/a/1/a.bin", "/packages", "/packages/zoe/npm/x", "/zoe/lib?go-get=1",
		"/mcp", "/api/v3/user", "/api/actions/jobs", "/healthz"} {
		if rec := f.remote(t, "GET", p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("remote GET %s = %d, want 404", p, rec.Code)
		}
	}
	// Web UI is exposed by default but needs a session.
	if rec := f.remote(t, "GET", "/settings/tokens", nil, nil); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin") {
		t.Errorf("remote settings without a session = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// A LAN session cookie doesn't work through the tunnel.
	if rec := f.remote(t, "GET", "/settings/tokens", nil, map[string]string{"Cookie": sessionCookie + "=" + f.lanSess}); rec.Code != http.StatusSeeOther {
		t.Errorf("LAN session accepted remotely: %d", rec.Code)
	}

	// Password sign-in is refused, and no session is made.
	rec := f.remote(t, "POST", "/signin", url.Values{"password": {"correct horse"}}, nil)
	if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
		t.Errorf("remote password sign-in = %d, cookies %v", rec.Code, rec.Result().Cookies())
	}
	page := f.remote(t, "GET", "/signin", nil, nil).Body.String()
	if strings.Contains(page, `name="password"`) || !strings.Contains(page, "data-passkey-login") {
		t.Error("remote sign-in page should offer passkeys only")
	}
	for _, p := range []string{"/setup", "/auth/github", "/auth/github/callback"} {
		if rec := f.remote(t, "GET", p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("remote %s = %d, want 404", p, rec.Code)
		}
	}
	// The passkey flow is reachable (it answers in JSON, not with a 404 page).
	rec = f.remote(t, "POST", "/auth/passkey/begin", url.Values{}, nil)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Errorf("passkey begin through the tunnel = %d %s", rec.Code, rec.Body)
	}

	// Git: tokens only, never passwords, even for a public repository.
	if _, err := f.a.db.CreateRepo(ctx, f.owner.ID, "lib", "", false); err != nil {
		t.Fatal(err)
	}
	if err := f.a.git.Init(ctx, "zoe", "lib"); err != nil {
		t.Fatal(err)
	}
	refs := "/zoe/lib.git/info/refs?service=git-upload-pack"
	if rec := do(t, f.a, "GET", refs, nil, nil); rec.Code != 200 {
		t.Fatalf("LAN anonymous clone of a public repo = %d", rec.Code)
	}
	if rec := f.remote(t, "GET", refs, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("remote anonymous clone = %d, want 401", rec.Code)
	}
	basic := func(u, p string) map[string]string {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetBasicAuth(u, p)
		return map[string]string{"Authorization": r.Header.Get("Authorization")}
	}
	if rec := f.remote(t, "GET", refs, nil, basic("zoe", "correct horse")); rec.Code != http.StatusUnauthorized {
		t.Errorf("remote clone with the password = %d, want 401", rec.Code)
	}
	tok, _ := f.a.db.CreateToken(ctx, "laptop", tokenScopes["write"], time.Hour)
	if rec := f.remote(t, "GET", refs, nil, basic("zoe", tok)); rec.Code != 200 {
		t.Errorf("remote clone with a token = %d\n%s", rec.Code, rec.Body)
	}

	// MCP only when the owner exposes it; registries never.
	c, _ := f.a.db.TunnelConfig(ctx)
	c.ExposeMCP, c.ExposeAPI = true, true
	if err := f.a.db.SaveTunnelConfig(ctx, nil, c, nil, "test"); err != nil {
		t.Fatal(err)
	}
	f.a.tunnel().apply(ctx)
	if rec := f.remote(t, "POST", "/mcp", nil, nil); rec.Code == http.StatusNotFound {
		t.Error("MCP still 404 after exposing it")
	}
	if rec := f.remote(t, "GET", "/v2/", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("registry exposed: %d", rec.Code)
	}
	if !f.a.tunnel().sup.Running() || f.a.tunnel().remoteHost() != tunnelTestHost {
		t.Error("changing what's exposed restarted the helper")
	}
	// Changing settings through the tunnel isn't allowed; turning it off is.
	rs := f.remoteSession(t)
	if rec := f.remote(t, "POST", "/settings/remote", url.Values{"mode": {"quick"}, "expose_web": {"1"}}, map[string]string{"Cookie": sessionCookie + "=" + rs}); rec.Code != http.StatusForbidden {
		t.Errorf("remote settings change = %d, want 403", rec.Code)
	}
}

func TestTunnelClientIPAndURLs(t *testing.T) {
	f := newTunnelFix(t)
	f.a.cfg.TrustProxy = true // as in production behind nginx
	f.turnOn(t)

	// CF-Connecting-IP is ignored on the main listener…
	lanReq := httptest.NewRequest("GET", "/", nil)
	lanReq.RemoteAddr = "192.168.1.20:4000"
	lanReq.Header.Set("CF-Connecting-IP", "198.51.100.1")
	if ip := f.a.clientIP(lanReq); ip != "192.168.1.20" {
		t.Errorf("LAN clientIP = %q (CF-Connecting-IP must not be trusted there)", ip)
	}
	// …and from a loopback peer it means a tunnel pointed at the wrong port.
	mis := httptest.NewRequest("GET", "/signin", nil)
	mis.RemoteAddr = "127.0.0.1:4000"
	mis.Header.Set("Cf-Ray", "8a1b2c3d4e5f-LHR")
	mis.Header.Set("CF-Connecting-IP", "198.51.100.1")
	rec := httptest.NewRecorder()
	f.a.ServeHTTP(rec, mis)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("tunnelled request on the main listener = %d, want 421", rec.Code)
	}

	// On the remote listener it is the client, and X-Forwarded-* are ignored
	// even with TrustProxy on.
	probe := httptest.NewRequest("GET", "/", nil)
	probe.RemoteAddr = "127.0.0.1:5000"
	probe.Header.Set("CF-Connecting-IP", "203.0.113.9")
	probe.Header.Set("X-Forwarded-Host", "evil.example")
	probe.Header.Set("X-Forwarded-For", "10.0.0.1")
	for _, h := range proxyHeaders {
		probe.Header.Del(h) // what ServeHTTP does
	}
	seen := probe.WithContext(context.WithValue(probe.Context(), remoteKey{}, &remoteReq{Host: tunnelTestHost, IP: remoteClientIP(probe)}))
	if ip := f.a.clientIP(seen); ip != "203.0.113.9" {
		t.Errorf("remote clientIP = %q", ip)
	}
	if s, h := f.a.external(seen); s != "https" || h != tunnelTestHost {
		t.Errorf("remote external = %s://%s", s, h)
	}
	if b := f.a.baseURL(seen); b != "https://"+tunnelTestHost || !f.a.isHTTPS(seen) {
		t.Errorf("remote baseURL = %q", b)
	}
	if rp, origin, ok := f.a.passkeyRP(seen); rp != tunnelTestHost || origin != "https://"+tunnelTestHost || !ok {
		t.Errorf("remote passkey RP = %q %q %v", rp, origin, ok)
	}
	// The LAN still uses ATELIER_PUBLIC_URL.
	if b := f.a.baseURL(lanReq); b != "https://atelier.infinityhomelab.lan" {
		t.Errorf("LAN baseURL = %q", b)
	}

	// Pages built through the tunnel link to the tunnel address.
	rs := f.remoteSession(t)
	c, _ := f.a.db.TunnelConfig(context.Background())
	c.ExposeMCP = true
	_ = f.a.db.SaveTunnelConfig(context.Background(), nil, c, nil, "test")
	f.a.tunnel().apply(context.Background())
	body := f.remote(t, "GET", "/settings/mcp", nil, map[string]string{"Cookie": sessionCookie + "=" + rs}).Body.String()
	if !strings.Contains(body, "https://"+tunnelTestHost+"/mcp") || strings.Contains(body, "infinityhomelab.lan/mcp") {
		t.Error("MCP settings page reached remotely doesn't use the tunnel address")
	}

	// Rate limits key on the real client: one visitor locked out, another not.
	for i := 0; i < 8; i++ {
		f.remote(t, "GET", "/zoe/x.git/info/refs", nil, map[string]string{"CF-Connecting-IP": "203.0.113.50"})
	}
	if rec := f.remote(t, "GET", "/zoe/x.git/info/refs", nil, map[string]string{"CF-Connecting-IP": "203.0.113.50"}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("9th failure from one IP = %d, want 429", rec.Code)
	}
	if rec := f.remote(t, "GET", "/zoe/x.git/info/refs", nil, map[string]string{"CF-Connecting-IP": "203.0.113.51"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("another IP = %d, want 401", rec.Code)
	}
}

func TestTunnelEnrollLinkAndLog(t *testing.T) {
	f := newTunnelFix(t)
	ctx := context.Background()
	f.turnOn(t)

	rec := f.lan(t, "POST", "/settings/remote/enroll-link", url.Values{})
	body := rec.Body.String()
	i := strings.Index(body, "https://"+tunnelTestHost+"/settings/remote/enroll?code=")
	if rec.Code != 200 || i < 0 {
		t.Fatalf("enroll link = %d", rec.Code)
	}
	link := body[i : i+strings.IndexByte(body[i:], '"')]
	u, _ := url.Parse(strings.ReplaceAll(link, "&amp;", "&"))
	code := u.Query().Get("code")

	if rec := do(t, f.a, "GET", "/settings/remote/enroll?code="+code, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("enroll page on the LAN = %d, want 404", rec.Code)
	}
	if rec := f.remote(t, "GET", "/settings/remote/enroll?code="+code, nil, nil); rec.Code != 200 {
		t.Fatalf("enroll page = %d", rec.Code)
	}
	rec = f.remote(t, "POST", "/settings/remote/enroll", url.Values{"code": {code}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/passkeys" {
		t.Fatalf("enroll accept = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	var sess *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			sess = c
		}
	}
	if sess == nil || !sess.Secure || !sess.HttpOnly {
		t.Fatalf("enrol session cookie = %+v (must be Secure)", sess)
	}
	cookie := map[string]string{"Cookie": sessionCookie + "=" + sess.Value}
	// It can add a passkey for this address and nothing else.
	pk := f.remote(t, "GET", "/settings/passkeys", nil, cookie)
	if pk.Code != 200 || !strings.Contains(pk.Body.String(), tunnelTestHost) {
		t.Errorf("passkeys page through the tunnel = %d", pk.Code)
	}
	if rec := f.remote(t, "GET", "/settings/tokens", nil, cookie); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/passkeys" {
		t.Errorf("enrol session reached tokens: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := f.remote(t, "POST", "/settings/passkeys/delete", url.Values{"id": {"1"}}, cookie); rec.Code != http.StatusForbidden {
		t.Errorf("enrol session could delete passkeys: %d", rec.Code)
	}
	// One use only.
	if rec := f.remote(t, "POST", "/settings/remote/enroll", url.Values{"code": {code}}, nil); rec.Code != http.StatusNotFound {
		t.Errorf("second use of the link = %d", rec.Code)
	}

	// Who's connected.
	f.a.tunnel().flush(ctx)
	log, err := f.a.db.TunnelLog(ctx, 50)
	if err != nil || len(log) == 0 {
		t.Fatalf("log = %v, %v", log, err)
	}
	e := log[0]
	if e.IP != "203.0.113.7" || e.Country != "PT" || e.Class != classWeb || time.Since(e.At) > time.Minute {
		t.Errorf("newest log entry = %+v", e)
	}
	page := f.lan(t, "GET", "/settings/remote", nil).Body.String()
	if !strings.Contains(page, "203.0.113.7") || !strings.Contains(page, ">PT<") {
		t.Error("settings page doesn't show who's connected")
	}
	// Seven days.
	old := store.TunnelLogEntry{At: time.Now().Add(-8 * 24 * time.Hour), IP: "198.51.100.200", Class: "web", Method: "GET", Status: 200}
	_ = f.a.db.AddTunnelLog(ctx, []store.TunnelLogEntry{old})
	if n, _ := f.a.db.PruneTunnelLog(ctx, time.Now()); n != 1 {
		t.Errorf("pruned %d entries, want the one older than 7 days", n)
	}

	// Turning off revokes the enrolment session as well.
	f.lan(t, "POST", "/settings/remote/off", url.Values{})
	if _, ok := f.a.db.SessionPerson(ctx, sess.Value); ok {
		t.Error("enrolment session survived turning off")
	}
}

func TestTunnelSecondPersonCantSeeOrToggle(t *testing.T) {
	f := newTunnelFix(t)
	ctx := context.Background()
	sam, err := f.a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := f.a.db.CreateSessionFor(ctx, sam.ID, time.Hour)
	as := map[string]string{"Cookie": sessionCookie + "=" + tok, "Sec-Fetch-Site": "same-origin"}
	for _, r := range []struct{ m, p string }{
		{"GET", "/settings/remote"}, {"GET", "/settings/remote/status"}, {"POST", "/settings/remote/on"},
		{"POST", "/settings/remote"}, {"POST", "/settings/remote/enroll-link"}, {"POST", "/settings/remote/off"},
	} {
		var body url.Values
		if r.m == "POST" {
			body = url.Values{"mode": {"quick"}}
		}
		if rec := do(t, f.a, r.m, r.p, body, as); rec.Code != http.StatusNotFound {
			t.Errorf("sam %s %s = %d, want 404", r.m, r.p, rec.Code)
		}
	}
	if c, _ := f.a.db.TunnelConfig(ctx); c.Enabled {
		t.Fatal("sam turned it on")
	}
	f.turnOn(t)
	if body := do(t, f.a, "GET", "/", nil, as).Body.String(); strings.Contains(body, "Remote access is on") {
		t.Error("sam sees the banner")
	}
	if body := do(t, f.a, "GET", "/settings/does-not-exist", nil, nil).Body.String(); strings.Contains(body, tunnelTestHost) {
		t.Error("an anonymous page shows the tunnel address")
	}
}

func TestTunnelNamedSettings(t *testing.T) {
	f := newTunnelFix(t)
	ctx := context.Background()
	// Without a usable key the token can't be stored; the test sealer is the app's own.
	rec := f.lan(t, "POST", "/settings/remote", url.Values{"mode": {"named"}, "hostname": {"https://Git.Example.com/"},
		"token": {"eyJhIjoiYWNjb3VudCIsInQiOiJ0dW5uZWwiLCJzIjoic2VjcmV0In0"}, "expose_web": {"1"}, "expose_git": {"1"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save named = %d\n%s", rec.Code, rec.Body)
	}
	c, _ := f.a.db.TunnelConfig(ctx)
	if c.Mode != "named" || c.Hostname != "git.example.com" || !c.HasToken || c.Enabled {
		t.Fatalf("named config = %+v", c)
	}
	var ct []byte
	_ = f.a.db.DB().QueryRow(`SELECT token_ct FROM tunnel_config`).Scan(&ct)
	if strings.Contains(string(ct), "eyJh") {
		t.Fatal("token stored in the clear")
	}
	if got, err := f.a.db.TunnelToken(ctx, f.a.secretSealer()); err != nil || !strings.HasPrefix(got, "eyJh") {
		t.Fatalf("token round trip = %q, %v", got, err)
	}
	if !strings.Contains(f.lan(t, "GET", "/settings/remote", nil).Body.String(), "Saved (encrypted)") {
		t.Error("page doesn't say a token is saved")
	}
	f.lan(t, "POST", "/settings/remote/on", url.Values{})
	m := f.a.tunnel()
	waitUntil(t, "named tunnel", func() bool { return m.remoteHost() == "git.example.com" })
	args := readFile(t, filepath.Join(f.dir, "args"))
	if strings.TrimSpace(args) != "tunnel --no-autoupdate run" {
		t.Errorf("named args = %q", args)
	}
	if rec := f.lan(t, "POST", "/settings/remote", url.Values{"mode": {"named"}, "hostname": {"not a host"}}); rec.Code != http.StatusBadRequest {
		t.Errorf("bad hostname accepted: %d", rec.Code)
	}
}
