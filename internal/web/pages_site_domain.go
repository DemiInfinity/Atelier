package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"atelier/internal/store"
)

// Custom domains: a site may have one host name of its own, such as
// docs.example.com.
//
//   - Claiming a host gives the site a random token. The host is verified by
//     a TXT record _atelier-pages.<host> holding the token, or by the file
//     /.well-known/atelier-pages/<token> (containing the token) served over
//     HTTPS at the host by whatever serves it today. Verification re-runs
//     daily; three failures in a row un-verify the host.
//   - Only a verified host is served, at "/", by servePagesDomain. Hosts are
//     matched exactly (lower case, port and trailing dot dropped); there are
//     no wildcards. Anything else, an unverified claim included, falls
//     through to the app as before.
//   - The app's host, the Pages host and the tunnel's host names can't be
//     claimed, and the app's host is never served as a site even if a claim
//     somehow existed.
//   - TLS is terminated in front of Atelier: an Nginx Proxy Manager proxy
//     host or a public hostname on the named Cloudflare tunnel. The settings
//     page shows the steps for the route the owner picks.
//
// A custom domain is the site's own origin, so sites there are sandboxed
// like on the Pages host but keep their origin (allow-same-origin): nothing
// else is served on it, and private sites (pages_site_private.go) need
// their own resources to carry the site cookie.

const (
	pagesDomainCSP = "sandbox allow-scripts allow-same-origin allow-forms allow-popups allow-popups-to-escape-sandbox allow-downloads allow-modals; " +
		"default-src 'self' https: data: blob: 'unsafe-inline'; object-src 'none'; base-uri 'self'; frame-ancestors 'self'"

	pagesWellKnown       = "/.well-known/atelier-pages/"
	pagesDomainRecheck   = 24 * time.Hour // verified domains
	pagesPendingRecheck  = time.Hour      // claims waiting for DNS
	pagesPendingFor      = 7 * 24 * time.Hour
	pagesDomainMaxFails  = 3
	pagesDomainCacheTTL  = 30 * time.Second
	pagesRouteProxy      = "proxy"
	pagesRouteTunnel     = "tunnel"
	pagesTXTPrefix       = "_atelier-pages."
	pagesVerifyMaxBody   = 1 << 10
	pagesVerifyRedirects = 3
)

func init() {
	registerFeature(feature{
		name: "pages-domains",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("POST /{owner}/{repo}/settings/pages/domain", a.authed(a.pagesDomainSave))
			m.HandleFunc("POST /{owner}/{repo}/settings/pages/domain/verify", a.authed(a.pagesDomainVerify))
			m.HandleFunc("POST /{owner}/{repo}/settings/pages/domain/remove", a.authed(a.pagesDomainRemove))
		},
		start: func(ctx context.Context, a *App) {
			t := time.NewTicker(15 * time.Minute)
			defer t.Stop()
			for {
				a.pagesRecheckDomains(ctx, time.Now())
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		},
	})
}

// pagesVerifyTimeout bounds each DNS lookup and file fetch (tests shorten it).
var pagesVerifyTimeout = 8 * time.Second

// ---------- host names ----------

// pagesHostName reduces a Host header (or what a person typed) to a bare
// host name: lower case, without port, brackets or trailing dot.
func pagesHostName(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if name, _, err := net.SplitHostPort(h); err == nil {
		h = name
	}
	h = strings.Trim(h, "[]")
	return strings.TrimSuffix(h, ".")
}

// pagesCleanDomain validates a host name typed in settings. A pasted URL is
// accepted for its host. IP addresses, single labels, localhost and
// wildcards are refused.
func pagesCleanDomain(in string) (string, error) {
	in = strings.TrimSpace(in)
	if strings.Contains(in, "://") {
		u, err := url.Parse(in)
		if err != nil || u.Host == "" {
			return "", errors.New("Enter a host name such as docs.example.com.")
		}
		in = u.Host
	}
	h := pagesHostName(in)
	if strings.HasSuffix(h, ".") { // "name.." and the like
		return "", errors.New("Enter a host name such as docs.example.com.")
	}
	if h == "" || len(h) > 253 || strings.Contains(h, "*") {
		return "", errors.New("Enter one host name such as docs.example.com (no wildcards).")
	}
	if net.ParseIP(h) != nil {
		return "", errors.New("Use a host name, not an IP address.")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", errors.New("Use a full host name such as docs.example.com.")
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", errors.New("That isn't a valid host name.")
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", errors.New("That isn't a valid host name (use the xn-- form for international names).")
			}
		}
	}
	tld := labels[len(labels)-1]
	if strings.Trim(tld, "0123456789") == "" {
		return "", errors.New("That isn't a valid host name.")
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return "", errors.New("localhost names can't be verified.")
	}
	return h, nil
}

// pagesAppHostName is the bare host name of ATELIER_PUBLIC_URL ("" if unset).
func (a *App) pagesAppHostName() string {
	if a.cfg.PublicURL == "" {
		return ""
	}
	u, err := url.Parse(a.cfg.PublicURL)
	if err != nil {
		return ""
	}
	return pagesHostName(u.Host)
}

// pagesReservedHost says why host can't be claimed ("" if it can).
func (a *App) pagesReservedHost(ctx context.Context, host string) string {
	if app := a.pagesAppHostName(); app == "" {
		return "Custom domains need ATELIER_PUBLIC_URL to be set, so the app's own host name can never be claimed."
	} else if host == app {
		return "That is Atelier's own host name."
	}
	if host == pagesHostName(a.pagesHost()) {
		return "That is the Pages host name."
	}
	if c, err := a.db.TunnelConfig(ctx); err == nil {
		if host == pagesHostName(c.Hostname) || host == pagesHostName(c.PagesHostname) {
			return "That is one of the remote access tunnel's host names."
		}
	}
	if h := a.tunnel().remoteHost(); h != "" && host == pagesHostName(h) {
		return "That is the remote access tunnel's host name."
	}
	return ""
}

// ---------- the verified-host table, cached ----------

type pagesDomainCache struct {
	mu    sync.Mutex
	at    time.Time
	hosts map[string]int64
}

var pagesDomainCaches sync.Map // *App → *pagesDomainCache

func (a *App) pagesDomainCache() *pagesDomainCache {
	c, _ := pagesDomainCaches.LoadOrStore(a, &pagesDomainCache{})
	return c.(*pagesDomainCache)
}

// pagesDomainsChanged drops the cache after a claim, verification or removal.
func (a *App) pagesDomainsChanged() {
	c := a.pagesDomainCache()
	c.mu.Lock()
	c.hosts = nil
	c.mu.Unlock()
}

// pagesVerifiedHost returns the repository a verified host belongs to.
func (a *App) pagesVerifiedHost(ctx context.Context, host string) (int64, bool) {
	if host == "" {
		return 0, false
	}
	c := a.pagesDomainCache()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hosts == nil || time.Since(c.at) > pagesDomainCacheTTL {
		hosts, err := a.db.VerifiedPagesHosts(ctx)
		if err != nil {
			return 0, false
		}
		c.hosts, c.at = hosts, time.Now()
	}
	id, ok := c.hosts[host]
	return id, ok
}

// pagesIsDomain reports whether reqHost is a verified custom domain that may
// be served (never the app's own host).
func (a *App) pagesIsDomain(ctx context.Context, reqHost string) bool {
	if a.pagesBase() == "" {
		return false
	}
	host := pagesHostName(reqHost)
	if host == "" || host == a.pagesAppHostName() {
		return false
	}
	_, ok := a.pagesVerifiedHost(ctx, host)
	return ok
}

// ---------- serving ----------

// servePagesDomain serves a site on its verified custom domain, at "/". It
// returns false (and writes nothing) for any other host.
func (a *App) servePagesDomain(w http.ResponseWriter, r *http.Request, reqHost string) bool {
	if a.pagesBase() == "" {
		return false
	}
	ctx := r.Context()
	host := pagesHostName(reqHost)
	if host == "" || host == a.pagesAppHostName() {
		return false
	}
	repoID, ok := a.pagesVerifiedHost(ctx, host)
	if !ok {
		return false
	}
	// From here on the host is the site's: nothing falls through to the app.
	h := w.Header()
	pagesSecurityHeaders(h, pagesDomainCSP)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	rp, err := a.db.RepoByID(ctx, repoID)
	if err != nil {
		pagesPlain404(w, r)
		return true
	}
	switch r.URL.Path {
	case pagesWellKnown + "_signin":
		a.pagesSiteSignin(w, r, rp, host)
		return true
	case pagesWellKnown + "_signout":
		a.pagesSiteSignout(w, r, host)
		return true
	}
	ps, err := a.db.PagesSite(ctx, rp.ID)
	if err != nil || !ps.Enabled || ps.LiveID == 0 {
		pagesPlain404(w, r)
		return true
	}
	if rp.Private && !a.pagesSiteSignedIn(w, r, rp, host) {
		return true
	}
	segs, ok := pagesSplitPath(r.URL.Path)
	if !ok {
		pagesPlain404(w, r)
		return true
	}
	a.pagesServeFile(w, r, rp.ID, ps.LiveID, strings.Join(segs, "/"), rp.Private)
	return true
}

// pagesDomainURL is https://host/ (http only when Pages itself is plain
// http and the domain isn't behind the tunnel).
func (a *App) pagesDomainURL(d store.PagesDomain) string {
	scheme := "https"
	if d.Route != pagesRouteTunnel && strings.HasPrefix(a.pagesBase(), "http://") {
		scheme = "http"
	}
	return scheme + "://" + d.Host + "/"
}

// pagesPublicURL is where a repository's site is read: its verified custom
// domain if it has one, else its Pages address.
func (a *App) pagesPublicURL(ctx context.Context, rp store.Repo) string {
	if a.pagesBase() == "" {
		return ""
	}
	if d, err := a.db.PagesDomain(ctx, rp.ID); err == nil && d.Verified() {
		return a.pagesDomainURL(d)
	}
	if rp.Private {
		return ""
	}
	return a.pagesSiteURL(rp)
}

// ---------- verification ----------

// pagesLookupTXT resolves TXT records (tests replace it). Only TXT records
// are read: nothing is fetched from the addresses DNS returns.
var pagesLookupTXT = func(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// pagesVerifyClient fetches the verification file: HTTPS only, through the
// webhook SSRF guard (no private, loopback or metadata addresses, checked
// on every connection), time- and size-limited. Tests replace it.
var pagesVerifyClient = func(a *App) *http.Client {
	tr := &http.Transport{
		Proxy:                  nil, // a proxy would bypass the guard
		DialContext:            a.apps().guardedDial(false),
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  pagesVerifyTimeout,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 16 << 10,
	}
	return &http.Client{Transport: tr, Timeout: pagesVerifyTimeout, CheckRedirect: pagesVerifyRedirect}
}

func pagesVerifyRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > pagesVerifyRedirects {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" {
		return errors.New("redirected away from https")
	}
	return nil
}

func pagesNewToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// pagesCheckDomain tries the TXT record, then the file. It returns the
// method that worked or an error describing both attempts.
func (a *App) pagesCheckDomain(ctx context.Context, d store.PagesDomain) (string, error) {
	txtErr := pagesCheckTXT(ctx, d)
	if txtErr == nil {
		return "txt", nil
	}
	fileErr := a.pagesCheckFile(ctx, d)
	if fileErr == nil {
		return "file", nil
	}
	return "", fmt.Errorf("TXT record: %v; file: %v", txtErr, fileErr)
}

func pagesCheckTXT(ctx context.Context, d store.PagesDomain) error {
	ctx, cancel := context.WithTimeout(ctx, pagesVerifyTimeout)
	defer cancel()
	recs, err := pagesLookupTXT(ctx, pagesTXTPrefix+d.Host)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return fmt.Errorf("no TXT record at %s%s", pagesTXTPrefix, d.Host)
		}
		if ctx.Err() != nil {
			return errors.New("DNS lookup timed out")
		}
		return errors.New("DNS lookup failed")
	}
	for i, rec := range recs {
		if i >= 20 {
			break
		}
		if strings.TrimSpace(rec) == d.Token {
			return nil
		}
	}
	return fmt.Errorf("the TXT record at %s%s doesn't contain the token", pagesTXTPrefix, d.Host)
}

func (a *App) pagesCheckFile(ctx context.Context, d store.PagesDomain) error {
	ctx, cancel := context.WithTimeout(ctx, pagesVerifyTimeout)
	defer cancel()
	u := "https://" + d.Host + pagesWellKnown + d.Token
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Atelier-Pages-Verify/1.0")
	resp, err := pagesVerifyClient(a).Do(req)
	if err != nil {
		if errors.Is(err, errAppsBlocked) || strings.Contains(err.Error(), errAppsBlocked.Error()) {
			return fmt.Errorf("%s resolves to an address Atelier won't fetch from (private or local)", d.Host)
		}
		return fmt.Errorf("couldn't fetch %s", u)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", u, resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, pagesVerifyMaxBody))
	if strings.TrimSpace(string(body)) != d.Token {
		return fmt.Errorf("%s doesn't contain the token", u)
	}
	return nil
}

// pagesVerifyNow checks one claim and records the outcome.
func (a *App) pagesVerifyNow(ctx context.Context, d store.PagesDomain) error {
	defer a.pagesDomainsChanged()
	if why := a.pagesReservedHost(ctx, d.Host); why != "" {
		_, _ = a.db.PagesDomainFailed(ctx, d.RepoID, why, 1)
		return errors.New(why)
	}
	method, err := a.pagesCheckDomain(ctx, d)
	if err != nil {
		lost, _ := a.db.PagesDomainFailed(ctx, d.RepoID, err.Error(), pagesDomainMaxFails)
		if lost {
			log.Printf("pages: %s failed verification %d times and is no longer served: %v", d.Host, pagesDomainMaxFails, err)
		}
		return err
	}
	return a.db.PagesDomainVerified(ctx, d.RepoID, method)
}

// pagesRecheckDomains re-verifies verified domains daily and pending claims
// hourly (for a week), so DNS changes are picked up without a click.
func (a *App) pagesRecheckDomains(ctx context.Context, now time.Time) {
	if a.pagesBase() == "" {
		return
	}
	ds, err := a.db.PagesDomains(ctx)
	if err != nil {
		return
	}
	for _, d := range ds {
		if ctx.Err() != nil {
			return
		}
		every := pagesDomainRecheck
		if !d.Verified() {
			if now.Sub(d.CreatedAt) > pagesPendingFor {
				continue
			}
			every = pagesPendingRecheck
		}
		if d.CheckedAt != nil && now.Sub(*d.CheckedAt) < every {
			continue
		}
		_ = a.pagesVerifyNow(ctx, d)
	}
}

// ---------- settings ----------

func (a *App) pagesDomainSave(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	if !a.pagesEnabled() {
		a.pagesRender(w, r, "Pages is off on this server.", http.StatusBadRequest)
		return
	}
	route := r.FormValue("route")
	if route != pagesRouteTunnel {
		route = pagesRouteProxy
	}
	host, err := pagesCleanDomain(r.FormValue("host"))
	if err != nil {
		a.pagesRender(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	if why := a.pagesReservedHost(ctx, host); why != "" {
		a.pagesRender(w, r, why, http.StatusBadRequest)
		return
	}
	if cur, err := a.db.PagesDomain(ctx, v.Repo.ID); err == nil && cur.Host == host {
		if err := a.db.SetPagesDomainRoute(ctx, v.Repo.ID, route); err != nil {
			a.fail(w, r, err)
			return
		}
	} else {
		if err := a.db.SetPagesDomain(ctx, v.Repo.ID, host, pagesNewToken(), route); err != nil {
			if errors.Is(err, store.ErrPagesHostTaken) {
				a.pagesRender(w, r, "Another site already uses "+host+".", http.StatusConflict)
				return
			}
			a.fail(w, r, err)
			return
		}
		_ = a.db.DeletePagesSiteSessionsFor(ctx, v.Repo.ID)
	}
	a.pagesDomainsChanged()
	http.Redirect(w, r, v.Base+"/settings/pages?saved=1#pages-domain", http.StatusSeeOther)
}

func (a *App) pagesDomainVerify(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	d, err := a.db.PagesDomain(ctx, v.Repo.ID)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.pagesVerifyNow(ctx, d); err != nil {
		if errors.Is(err, store.ErrPagesHostTaken) {
			a.pagesRender(w, r, "Another site already uses "+d.Host+".", http.StatusConflict)
			return
		}
		a.pagesRender(w, r, "Not verified yet: "+err.Error(), http.StatusOK)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/pages?saved=1#pages-domain", http.StatusSeeOther)
}

func (a *App) pagesDomainRemove(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	if err := a.db.DeletePagesDomain(r.Context(), v.Repo.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	a.pagesDomainsChanged()
	http.Redirect(w, r, v.Base+"/settings/pages?saved=1#pages-domain", http.StatusSeeOther)
}
