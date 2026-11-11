package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"atelier/internal/store"
)

// Private sites on a custom domain.
//
// A private repository's site is served only on its verified custom domain,
// which is its own origin: no other site's script runs there, and the app's
// session cookie never goes there. Signing in works like an OAuth code:
//
//  1. A visitor without a site session is redirected to the app,
//     GET /-/pages/signin?host=<host>&return=<path>.
//  2. The app needs its usual session and checks that the person can read
//     the repository. It stores the hash of a random single-use token
//     (valid for a minute, bound to the host and the person) and redirects
//     to https://<host>/.well-known/atelier-pages/_signin?token=…&return=….
//  3. The site host consumes the token (once), checks read access again and
//     sets its own cookie: host-only (no Domain), Path=/, HttpOnly,
//     SameSite=Lax, Secure and __Host- prefixed over HTTPS. It holds a
//     random session id stored hashed, valid for 8 hours, for this host
//     and repository only.
//  4. Every request on the site checks the session and the person's read
//     access, so losing access (or the repository changing hands) takes
//     effect at once. Changing or removing the domain ends every session.
//
// Private responses are Cache-Control: private and carry no
// Access-Control-Allow-Origin, so other origins can't read them.

const (
	pagesHandoffTTL     = time.Minute
	pagesSiteSessionTTL = 8 * time.Hour
	pagesSiteCookie     = "atelier-site"
	pagesSiteCookieTLS  = "__Host-atelier-site"
)

// pagesHandoffTTLFor lets tests expire handoff tokens.
var pagesHandoffTTLFor = func() time.Duration { return pagesHandoffTTL }

func init() {
	registerFeature(feature{
		name: "pages-private",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /-/pages/signin", a.authed(a.pagesHandoff))
		},
	})
}

func pagesRandom() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pagesSafeReturn keeps a return path on the same host: an absolute path,
// never "//host" or a backslash trick.
func pagesSafeReturn(p string) string {
	if p == "" || len(p) > 2048 || p[0] != '/' || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\x00\r\n") {
		return "/"
	}
	return p
}

func (a *App) pagesSiteCookieName(r *http.Request) (string, bool) {
	if a.isHTTPS(r) {
		return pagesSiteCookieTLS, true
	}
	return pagesSiteCookie, false
}

// pagesPersonCanRead re-checks a person's read access to the repository.
func (a *App) pagesPersonCanRead(ctx context.Context, personID int64, rp store.Repo) bool {
	p, err := a.db.Person(ctx, personID)
	if err != nil {
		return false
	}
	return a.can(ctx, Actor{Kind: ActorOwner, Name: p.Name, PersonID: p.ID}, rp, PermRead)
}

// pagesAppBase is the app address a site visitor is sent to for signing in:
// the tunnel's address for tunnel visitors (when the web UI is exposed),
// else ATELIER_PUBLIC_URL.
func (a *App) pagesAppBase(r *http.Request) string {
	if isRemote(r) {
		if c := a.tunnel().cfg.Load(); c != nil && c.Named() && c.ExposeWeb && c.Hostname != "" {
			return tunnelURL(c.Hostname)
		}
		return ""
	}
	return strings.TrimRight(a.cfg.PublicURL, "/")
}

// pagesSiteSignedIn reports whether the visitor of a private site may read
// it. If not, it has already answered (a redirect to sign in).
func (a *App) pagesSiteSignedIn(w http.ResponseWriter, r *http.Request, rp store.Repo, host string) bool {
	ctx := r.Context()
	name, _ := a.pagesSiteCookieName(r)
	if c, err := r.Cookie(name); err == nil && c.Value != "" && len(c.Value) <= 128 {
		if pid, err := a.db.PagesSiteSession(ctx, store.Hash(c.Value), host, rp.ID); err == nil && a.pagesPersonCanRead(ctx, pid, rp) {
			return true
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	base := a.pagesAppBase(r)
	if base == "" {
		pagesOwnPage(w, r, http.StatusForbidden, "Sign-in unavailable",
			`<h1>This site is private</h1><p class="lead">Signing in isn't available from here.</p>`)
		return false
	}
	target := base + "/-/pages/signin?host=" + url.QueryEscape(host) + "&return=" + url.QueryEscape(pagesSafeReturn(r.URL.RequestURI()))
	http.Redirect(w, r, target, http.StatusFound)
	return false
}

// pagesHandoff (on the app) mints a single-use token for a reader of the
// site's repository and sends them back to the site.
func (a *App) pagesHandoff(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	host := pagesHostName(r.URL.Query().Get("host"))
	if !a.pagesIsDomain(ctx, host) {
		a.notFound(w, r)
		return
	}
	repoID, _ := a.pagesVerifiedHost(ctx, host)
	rp, err := a.db.RepoByID(ctx, repoID)
	if err != nil {
		a.notFound(w, r)
		return
	}
	d, err := a.db.PagesDomain(ctx, rp.ID)
	if err != nil || !d.Verified() || d.Host != host {
		a.notFound(w, r)
		return
	}
	who := a.actor(r)
	if !a.can(ctx, who, rp, PermRead) || who.PersonID == 0 {
		a.notFound(w, r)
		return
	}
	ret := pagesSafeReturn(r.URL.Query().Get("return"))
	site := strings.TrimSuffix(a.pagesDomainURL(d), "/")
	if !rp.Private {
		pagesContinue(w, site+ret, d.Host)
		return
	}
	tok := pagesRandom()
	if err := a.db.CreatePagesHandoff(ctx, store.Hash(tok), rp.ID, who.PersonID, host, pagesHandoffTTLFor()); err != nil {
		a.fail(w, r, err)
		return
	}
	pagesContinue(w, site+pagesWellKnown+"_signin?token="+tok+"&return="+url.QueryEscape(ret), d.Host)
}

// pagesContinue sends the browser on to the site with a page that refreshes
// to it, not a redirect: after a password sign-in this request is itself
// the end of a form submission's redirects, and the app's CSP form-action
// would block a redirect to another host.
func pagesContinue(w http.ResponseWriter, target, host string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	t := html.EscapeString(target)
	_, _ = io.WriteString(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="referrer" content="no-referrer">`+
		`<meta http-equiv="refresh" content="0;url=`+t+`"><title>Opening `+html.EscapeString(host)+`</title>`+
		`<link rel="stylesheet" href="/static/atelier.css"></head><body><main class="wrap narrow stack"><p>Opening <a href="`+t+`">`+
		html.EscapeString(host)+`</a>…</p></main></body></html>`)
}

// pagesSiteSignin (on the custom domain) turns a handoff token into a site session.
func (a *App) pagesSiteSignin(w http.ResponseWriter, r *http.Request, rp store.Repo, host string) {
	ctx := r.Context()
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	tok := r.URL.Query().Get("token")
	bad := func() {
		pagesOwnPage(w, r, http.StatusForbidden, "Sign-in link expired",
			`<h1>This sign-in link has expired</h1><p class="lead">Links work once, for a minute.</p><p><a href="/">Back to the site</a></p>`)
	}
	if tok == "" || len(tok) > 128 || !rp.Private {
		bad()
		return
	}
	repoID, personID, err := a.db.UsePagesHandoff(ctx, store.Hash(tok), host)
	if err != nil || repoID != rp.ID || !a.pagesPersonCanRead(ctx, personID, rp) {
		bad()
		return
	}
	sid := pagesRandom()
	if err := a.db.CreatePagesSiteSession(ctx, store.Hash(sid), rp.ID, personID, host, pagesSiteSessionTTL); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	name, secure := a.pagesSiteCookieName(r)
	http.SetCookie(w, &http.Cookie{Name: name, Value: sid, Path: "/", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(pagesSiteSessionTTL / time.Second)})
	http.Redirect(w, r, pagesSafeReturn(r.URL.Query().Get("return")), http.StatusSeeOther)
}

// pagesSiteSignout ends the site session on this host.
func (a *App) pagesSiteSignout(w http.ResponseWriter, r *http.Request, host string) {
	name, secure := a.pagesSiteCookieName(r)
	if c, err := r.Cookie(name); err == nil && c.Value != "" {
		_ = a.db.DeletePagesSiteSession(r.Context(), store.Hash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	w.Header().Set("Cache-Control", "no-store")
	pagesOwnPage(w, r, http.StatusOK, "Signed out", `<h1>Signed out</h1><p class="lead">You're signed out of `+html.EscapeString(host)+`.</p>`)
}
