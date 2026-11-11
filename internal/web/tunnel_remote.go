package web

// The remote listener: everything that arrives through the tunnel.
//
// cloudflared connects to a dedicated loopback listener (ATELIER_TUNNEL_LISTEN,
// 127.0.0.1:3001), never the main one, so "came through the tunnel" is a
// property of the socket, not of a header anyone could send. Requests on it
// are marked remote in their context and get their own rules:
//
//   - only the path classes the owner exposed are served, everything else is
//     a 404 (registries always are);
//   - no password or GitHub sign-in and no first-run setup: browsers sign in
//     with a passkey, git and APIs use access tokens (git always needs one,
//     even for public repositories);
//   - only sessions started through the tunnel, on the current address, are
//     accepted, and turning the tunnel off deletes them;
//   - links, clone URLs, redirects and passkeys use the tunnel address, and
//     cookies are Secure;
//   - the client IP for rate limits comes from CF-Connecting-IP, which is
//     trusted here and nowhere else.

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// remoteReq marks a request that came through the tunnel.
type remoteReq struct {
	Host    string // the tunnel address's host, e.g. quiet-fox.trycloudflare.com
	IP      string // the visitor, from CF-Connecting-IP
	Country string // CF-IPCountry, when Cloudflare sends it
}

type remoteKey struct{}

// remoteOf returns the remote marking of a request, if it came through the tunnel.
func remoteOf(r *http.Request) (*remoteReq, bool) {
	rm, ok := r.Context().Value(remoteKey{}).(*remoteReq)
	return rm, ok && rm != nil
}

func isRemote(r *http.Request) bool { _, ok := remoteOf(r); return ok }

// Path classes.
const (
	classWeb      = "web"
	classGit      = "git"
	classMCP      = "mcp"
	classPages    = "pages"
	classAPI      = "api"
	classRegistry = "registry"
	classOther    = "other"
	classConnect  = "connect" // Atelier Connect's federation endpoints (connect.go), exposed from Settings → Remote access
)

// tunnelClass sorts a request path into what the owner can expose. Anything
// that isn't a machine endpoint is the web UI (which needs a session anyway).
func tunnelClass(r *http.Request) string {
	p := r.URL.Path
	under := func(prefix string) bool { return p == prefix || strings.HasPrefix(p, prefix+"/") }
	switch {
	case connectClassPath(p):
		return classConnect
	case under("/v2"), under("/npm"), strings.HasPrefix(p, "/go/"), p == "/go",
		strings.HasPrefix(p, "/api/v1/packages/"), under("/packages"),
		r.URL.Query().Get("go-get") == "1":
		return classRegistry
	case under("/mcp"), strings.HasPrefix(p, "/.well-known/oauth"):
		return classMCP
	case under("/api"), under("/osv"):
		return classAPI
	case under("/healthz"):
		return classOther
	}
	if _, _, _, ok := gitrepo.IsGitPath(p); ok {
		return classGit
	}
	if _, _, _, ok := lfsPath(p); ok {
		return classGit
	}
	if _, _, ok := snippetGitPath(p); ok {
		return classGit
	}
	if strings.Contains(strings.ToLower(p), ".git/") || strings.HasSuffix(strings.ToLower(p), ".git") {
		return classGit
	}
	return classWeb
}

func tunnelExposes(c *store.TunnelConfig, class string) bool {
	switch class {
	case classWeb:
		return c.ExposeWeb
	case classGit:
		return c.ExposeGit
	case classMCP:
		return c.ExposeMCP
	case classPages:
		return c.ExposePages
	case classAPI:
		return c.ExposeAPI
	}
	return false // registries and anything else: never
}

// remoteClientIP is the visitor's address on the remote listener:
// CF-Connecting-IP when it parses as an IP, else the socket peer.
func remoteClientIP(r *http.Request) string {
	if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); ip != nil {
		return ip.String()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func remoteCountry(r *http.Request) string {
	c := strings.ToUpper(strings.TrimSpace(r.Header.Get("CF-IPCountry")))
	if len(c) != 2 {
		return ""
	}
	for _, ch := range c {
		if (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
			return ""
		}
	}
	return c
}

// proxyHeaders are dropped on the remote listener: nothing there may claim a
// different scheme, host or client through them.
var proxyHeaders = []string{"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP", "Forwarded"}

// statusWriter remembers the response status for the log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// ServeHTTP is the remote listener's handler.
func (m *tunnelManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := m.cfg.Load()
	host := m.remoteHost()
	if !m.on.Load() || cfg == nil || host == "" {
		w.Header().Set("Connection", "close")
		http.Error(w, "remote access is off", http.StatusServiceUnavailable)
		return
	}
	m.inflight.Add(1)
	defer m.inflight.Add(-1)

	for _, h := range proxyHeaders {
		r.Header.Del(h)
	}
	rm := &remoteReq{Host: host, IP: remoteClientIP(r), Country: remoteCountry(r)}
	r = r.WithContext(context.WithValue(r.Context(), remoteKey{}, rm))

	class := tunnelClass(r)
	reqHost := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(reqHost); err == nil {
		reqHost = h
	}
	if cfg.Mode == "named" && cfg.PagesHostname != "" && reqHost == cfg.PagesHostname {
		class = classPages
	}
	// A verified Pages custom domain added as a public hostname on the named tunnel.
	customPages := cfg.Mode == "named" && reqHost != cfg.Hostname && reqHost != cfg.PagesHostname && m.a.pagesIsDomain(r.Context(), reqHost)
	if customPages {
		class = classPages
	}

	sw := &statusWriter{ResponseWriter: w}
	a := m.a
	signedIn, who := false, ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		if rs, ok := a.db.RemoteSessionFor(r.Context(), c.Value); ok && rs.Host == host && !rs.EnrollOnly {
			signedIn = true
			if p, err := a.db.Person(r.Context(), rs.PersonID); err == nil {
				who = p.Name
			}
		}
	}
	defer func() {
		if class == classWeb && (strings.HasPrefix(r.URL.Path, "/static/") || strings.HasPrefix(r.URL.Path, "/-/avatar/")) {
			return // page furniture: one line per page view is enough
		}
		st := sw.status
		if st == 0 {
			st = http.StatusOK
		}
		if !signedIn && st < 400 && r.Header.Get("Authorization") != "" {
			signedIn, who = true, "token"
		}
		m.record(store.TunnelLogEntry{At: time.Now(), IP: rm.IP, Country: rm.Country, Class: class,
			Method: r.Method, Status: st, SignedIn: signedIn, Who: who})
	}()

	if !tunnelExposes(cfg, class) && !(class == classConnect && a.connectTunnelExposed(r.Context())) {
		http.NotFound(sw, r)
		return
	}
	if class == classPages {
		if a.pagesBase() == "" {
			http.NotFound(sw, r)
			return
		}
		if customPages {
			if !a.servePagesDomain(sw, r, reqHost) {
				http.NotFound(sw, r)
			}
			return
		}
		a.servePages(sw, r)
		return
	}
	if a.remoteGuard(sw, r, class) {
		return
	}
	a.ServeHTTP(sw, r)
}

// enrollPaths are all a one-time enrolment session may reach.
var enrollPaths = map[string]bool{
	"/settings/passkeys": true, "/settings/passkeys/register/begin": true, "/settings/passkeys/register/finish": true,
	"/signout": true, "/signin": true, "/auth/passkey/begin": true, "/auth/passkey/finish": true,
	"/manifest.webmanifest": true, "/sw.js": true, "/settings/remote/enroll": true, "/settings/remote/off": true,
}

// remoteGuard applies the sign-in rules for remote requests. It returns true
// if it answered.
func (a *App) remoteGuard(w http.ResponseWriter, r *http.Request, class string) bool {
	p := r.URL.Path
	switch {
	case p == "/setup" || strings.HasPrefix(p, "/setup/"),
		p == "/auth/github" || strings.HasPrefix(p, "/auth/github/"):
		http.NotFound(w, r)
		return true
	case p == "/signin" && r.Method == http.MethodPost:
		logErr(r, errString("password sign-in refused through the tunnel from "+a.clientIP(r)))
		w.WriteHeader(http.StatusForbidden)
		a.render(w, r, "signin", page{Title: "Sign in", Bare: true,
			Data: a.signinData(r, "Password sign-in is off for remote access. Use a passkey.")})
		return true
	}

	switch class {
	case classWeb:
		if c, err := r.Cookie(sessionCookie); err == nil {
			if rs, ok := a.db.RemoteSessionFor(r.Context(), c.Value); ok && rs.EnrollOnly &&
				!enrollPaths[p] && !strings.HasPrefix(p, "/static/") {
				if r.Method == http.MethodGet || r.Method == http.MethodHead {
					http.Redirect(w, r, "/settings/passkeys", http.StatusSeeOther)
				} else {
					http.Error(w, "this session can only add a passkey", http.StatusForbidden)
				}
				return true
			}
		}
	case classGit:
		// Tokens only, always: public repositories aren't anonymous on the internet.
		ip := a.clientIP(r)
		if !a.lim.allow(ip) {
			http.Error(w, "too many attempts, try again in a minute", http.StatusTooManyRequests)
			return true
		}
		ok := false
		if u, pw, has := r.BasicAuth(); has {
			for _, cand := range []string{pw, u} {
				if _, good := a.resolveCredential(r.Context(), cand); good {
					ok = true
					break
				}
			}
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="Atelier", charset="UTF-8"`)
			http.Error(w, "authentication required: use an Atelier access token as the password", http.StatusUnauthorized)
			return true
		}
		a.lim.reset(ip)
	}
	return false
}

type errString string

func (e errString) Error() string { return string(e) }

// remoteSessionOK says whether a session cookie may be used for this
// request: anywhere on the LAN, but through the tunnel only sessions that
// were started through it, on the current address.
func (a *App) remoteSessionOK(r *http.Request, tok string) bool {
	rm, ok := remoteOf(r)
	if !ok {
		return true
	}
	rs, ok := a.db.RemoteSessionFor(r.Context(), tok)
	return ok && rs.Host == rm.Host
}

// markRemoteSession records a session started through the tunnel.
func (a *App) markRemoteSession(r *http.Request, tok string, personID int64) error {
	rm, ok := remoteOf(r)
	if !ok {
		return nil
	}
	return a.db.MarkRemoteSession(r.Context(), tok, personID, rm.Host, false)
}

// tunnelMisrouted refuses requests that reached the main listener with
// Cloudflare's edge headers from a loopback peer: a named tunnel whose
// dashboard service points at port 3000 instead of the remote listener.
// Serving them would skip every remote-access rule.
func (a *App) tunnelMisrouted(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Cf-Ray") == "" && r.Header.Get("CF-Connecting-IP") == "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}
	logErr(r, errString("tunnelled request reached the main listener; point the tunnel's service at "+a.cfg.TunnelListen))
	http.Error(w, "This request came through a tunnel to the wrong port. In the Cloudflare dashboard, set the service to http://"+
		a.cfg.TunnelListen+".", http.StatusMisdirectedRequest)
	return true
}

// tunnelURL is https://host for a host, or "".
func tunnelURL(host string) string {
	if host == "" {
		return ""
	}
	return (&url.URL{Scheme: "https", Host: host}).String()
}

// isTunnelURL reports whether base is the tunnel's current address (which
// must never be remembered as the server's own URL for background work).
func (a *App) isTunnelURL(base string) bool {
	h := a.tunnel().remoteHost()
	return h != "" && strings.EqualFold(strings.TrimRight(base, "/"), tunnelURL(h))
}
