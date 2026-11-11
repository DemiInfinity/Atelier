package web

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
)

// pagesHostGet requests path with an arbitrary Host header.
func pagesHostGet(t *testing.T, a *App, host, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = host
	if u, err := url.Parse(path); err == nil {
		req.URL = u
	} else {
		req.URL.Path = path
	}
	req.RequestURI = path
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}

// fakeTXT makes pagesLookupTXT answer from records.
func fakeTXT(t *testing.T, records map[string][]string) {
	t.Helper()
	saved := pagesLookupTXT
	t.Cleanup(func() { pagesLookupTXT = saved })
	pagesLookupTXT = func(ctx context.Context, name string) ([]string, error) {
		if v, ok := records[name]; ok {
			return v, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
}

// noFileVerify makes file verification fail fast.
func noFileVerify(t *testing.T) {
	t.Helper()
	saved := pagesVerifyClient
	t.Cleanup(func() { pagesVerifyClient = saved })
	pagesVerifyClient = func(*App) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("no network in tests")
		})}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// pagesVerifiedDomain claims host for repo through settings and verifies it by TXT.
func (f *pagesFix) verifyDomain(t *testing.T, repo, host string) store.PagesDomain {
	t.Helper()
	ctx := context.Background()
	expectStatus(t, do(t, f.a, "POST", "/zoe/"+repo+"/settings/pages/domain", url.Values{"host": {host}, "route": {"proxy"}}, f.ck), http.StatusSeeOther, "claim "+host)
	rp, _ := f.a.db.RepoByName(ctx, "zoe", repo)
	d, err := f.a.db.PagesDomain(ctx, rp.ID)
	if err != nil {
		t.Fatal(err)
	}
	fakeTXT(t, map[string][]string{"_atelier-pages." + d.Host: {"v=other", " " + d.Token + " "}})
	expectStatus(t, do(t, f.a, "POST", "/zoe/"+repo+"/settings/pages/domain/verify", url.Values{}, f.ck), http.StatusSeeOther, "verify "+host)
	d, _ = f.a.db.PagesDomain(ctx, rp.ID)
	if !d.Verified() || d.Method != "txt" {
		t.Fatalf("domain = %+v", d)
	}
	return d
}

func TestPagesDomainHostNames(t *testing.T) {
	for in, want := range map[string]string{
		"Docs.Example.COM": "docs.example.com", "docs.example.com.": "docs.example.com", "docs.example.com:8443": "docs.example.com",
		"docs.example.com.:443": "docs.example.com", "  docs.example.com ": "docs.example.com", "[::1]:80": "::1",
	} {
		if got := pagesHostName(in); got != want {
			t.Errorf("pagesHostName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"Docs.Example.com.": "docs.example.com", "https://docs.example.com/x": "docs.example.com", "a-b.c.example.org": "a-b.c.example.org",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
	} {
		if got, err := pagesCleanDomain(in); err != nil || got != want {
			t.Errorf("pagesCleanDomain(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "localhost", "docs", "*.example.com", "10.0.0.1", "[::1]", "a..example.com", "-a.example.com",
		"a_b.example.com", "bücher.example", "x.localhost", "1.2.3.4.5", "example.com..", strings.Repeat("a", 64) + ".example.com"} {
		if got, err := pagesCleanDomain(bad); err == nil {
			t.Errorf("pagesCleanDomain(%q) = %q, want an error", bad, got)
		}
	}
}

func TestPagesDomainVerifyAndServe(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	noFileVerify(t)
	pullsCommit(t, f.work, "index.html", "<h1>Custom home</h1>", "home")
	pullsCommit(t, f.work, "guide/index.html", "<h1>Custom guide</h1>", "guide")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)

	// Claim: stored normalised, not served until verified.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/domain", url.Values{"host": {"Docs.Example.com."}, "route": {"proxy"}}, f.ck), http.StatusSeeOther, "claim")
	d, err := a.db.PagesDomain(ctx, f.demo.ID)
	if err != nil || d.Host != "docs.example.com" || d.Verified() || len(d.Token) != 32 {
		t.Fatalf("claim = %+v, %v", d, err)
	}
	body := do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String()
	mustContain(t, body, "domain card", "Not verified", "_atelier-pages.docs.example.com", d.Token, "Nginx Proxy Manager → Proxy Hosts")
	unverified := pagesHostGet(t, a, "docs.example.com", "/", nil)
	if strings.Contains(unverified.Body.String(), "Custom home") || unverified.Header().Get("Content-Security-Policy") == pagesDomainCSP {
		t.Fatalf("unverified host served the site: %d", unverified.Code)
	}
	// Unverified: the app answers as for any host (here: its sign-in redirect).
	if unverified.Code != http.StatusSeeOther || !strings.HasPrefix(unverified.Header().Get("Location"), "/signin") {
		t.Errorf("unverified host = %d %q, want the app's own handling", unverified.Code, unverified.Header().Get("Location"))
	}

	// Verify without the record: still not verified, and the page says why.
	fakeTXT(t, map[string][]string{})
	rec := do(t, a, "POST", "/zoe/demo/settings/pages/domain/verify", url.Values{}, f.ck)
	mustContain(t, rec.Body.String(), "failed verify", "Not verified yet", "no TXT record at _atelier-pages.docs.example.com")
	fakeTXT(t, map[string][]string{"_atelier-pages.docs.example.com": {"something-else"}})
	mustContain(t, do(t, a, "POST", "/zoe/demo/settings/pages/domain/verify", url.Values{}, f.ck).Body.String(), "wrong TXT", "doesn&#39;t contain the token")
	if d, _ := a.db.PagesDomain(ctx, f.demo.ID); d.Verified() {
		t.Fatal("verified with a wrong record")
	}

	d = f.verifyDomain(t, "demo", "docs.example.com")
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String(), "verified card", "Verified", "http://docs.example.com/")

	// Served at "/", whatever the case, port or trailing dot.
	for _, host := range []string{"docs.example.com", "DOCS.Example.COM", "docs.example.com:8080", "docs.example.com.", "docs.example.com.:443"} {
		rec := pagesHostGet(t, a, host, "/", map[string]string{"Cookie": f.ck["Cookie"]})
		pagesWant(t, rec, 200, "text/html; charset=utf-8", "Custom home")
		h := rec.Header()
		if h.Get("Content-Security-Policy") != pagesDomainCSP || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") == "" {
			t.Errorf("%s headers = %v", host, h)
		}
		if len(h.Values("Set-Cookie")) > 0 {
			t.Errorf("%s set cookies", host)
		}
	}
	pagesWant(t, pagesHostGet(t, a, "docs.example.com", "/guide/", nil), 200, "", "Custom guide")
	if rec := pagesHostGet(t, a, "docs.example.com", "/guide", nil); rec.Code != 301 || rec.Header().Get("Location") != "/guide/" {
		t.Errorf("/guide = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Not under /<owner>/<repo>/ there, and no app routes, git or static files.
	for _, p := range []string{"/zoe/demo/", "/signin", "/settings/tokens", "/static/atelier.css", "/zoe/demo.git/info/refs", "/api/v3/user", "/-/pages/signin?host=docs.example.com"} {
		rec := pagesHostGet(t, a, "docs.example.com", p, map[string]string{"Cookie": f.ck["Cookie"]})
		if rec.Code != 404 || len(rec.Header().Values("Set-Cookie")) > 0 {
			t.Errorf("custom host %s = %d", p, rec.Code)
		}
	}
	req := httptest.NewRequest("POST", "/signin", strings.NewReader("password=x"))
	req.Host = "docs.example.com"
	rec = httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || len(rec.Header().Values("Set-Cookie")) > 0 {
		t.Errorf("POST on custom host = %d", rec.Code)
	}
	// Exact matching: no parents, children or look-alikes.
	for _, host := range []string{"example.com", "www.docs.example.com", "docs.example.com.evil.test", "docs-example.com", "docs.example.co"} {
		if rec := pagesHostGet(t, a, host, "/", nil); strings.Contains(rec.Body.String(), "Custom home") {
			t.Errorf("%s served the site", host)
		}
	}
	// The shared Pages address keeps working; the app host never serves the site.
	pagesWant(t, pagesGet(t, a, "/zoe/demo/", nil), 200, "", "Custom home")
	if rec := pagesHostGet(t, a, "git.test", "/zoe/demo/", f.ck); strings.Contains(rec.Body.String(), "Custom home") {
		t.Error("app host served the site")
	}

	// Another site can't take a verified host.
	other := profRepo(t, a, f.zoe, "other", false, "index.html", "x")
	_ = a.db.SavePagesSite(ctx, store.PagesSite{RepoID: other.ID, Enabled: true, Source: store.PagesFromBranch, Branch: "main"})
	expectStatus(t, do(t, a, "POST", "/zoe/other/settings/pages/domain", url.Values{"host": {"docs.example.com"}}, f.ck), http.StatusConflict, "taken")

	// Changing the route keeps the token and the verification.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/domain", url.Values{"host": {"docs.example.com"}, "route": {"tunnel"}}, f.ck), http.StatusSeeOther, "route")
	if d2, _ := a.db.PagesDomain(ctx, f.demo.ID); d2.Token != d.Token || !d2.Verified() || d2.Route != "tunnel" {
		t.Errorf("after route change: %+v", d2)
	}
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String(), "tunnel steps", "Public hostname → Add", "https://docs.example.com/")

	// Removing it stops serving at once.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/domain/remove", url.Values{}, f.ck), http.StatusSeeOther, "remove")
	if rec := pagesHostGet(t, a, "docs.example.com", "/", nil); strings.Contains(rec.Body.String(), "Custom home") {
		t.Error("removed domain still served")
	}
}

func TestPagesDomainReservedHosts(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	for _, host := range []string{"git.test", "GIT.test.", "git.test:8080", "pages.test", "http://pages.test/"} {
		rec := do(t, a, "POST", "/zoe/demo/settings/pages/domain", url.Values{"host": {host}}, f.ck)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("claim %q = %d", host, rec.Code)
		}
	}
	if _, err := a.db.PagesDomain(ctx, f.demo.ID); err == nil {
		t.Error("a reserved host was stored")
	}
	// The tunnel's host names too.
	if err := a.db.SaveTunnelConfig(ctx, nil, store.TunnelConfig{Mode: "named", Hostname: "git.example.net", PagesHostname: "pages.example.net", ExposeWeb: true}, nil, "test"); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"git.example.net", "pages.example.net"} {
		expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/domain", url.Values{"host": {host}}, f.ck), http.StatusBadRequest, "tunnel host "+host)
	}
	// Without ATELIER_PUBLIC_URL the app's host isn't known, so nothing can be claimed.
	a.cfg.PublicURL = ""
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/domain", url.Values{"host": {"docs.example.com"}}, f.ck), http.StatusBadRequest, "no public URL")
	a.cfg.PublicURL = "http://git.test"

	// Even a verified row for the app's host (written behind the settings'
	// back) never turns the app into a site.
	pullsCommit(t, f.work, "index.html", "<h1>Hijack</h1>", "home")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)
	if err := a.db.SetPagesDomain(ctx, f.demo.ID, "git.test", "t", "proxy"); err != nil {
		t.Fatal(err)
	}
	if err := a.db.PagesDomainVerified(ctx, f.demo.ID, "txt"); err != nil {
		t.Fatal(err)
	}
	a.pagesDomainsChanged()
	rec := pagesHostGet(t, a, "git.test", "/", f.ck)
	if strings.Contains(rec.Body.String(), "Hijack") || rec.Header().Get("Content-Security-Policy") == pagesDomainCSP {
		t.Error("the app host was served as a site")
	}
	if rec := pagesHostGet(t, a, "git.test", "/zoe/demo", f.ck); rec.Code != 200 {
		t.Errorf("app repo page = %d", rec.Code)
	}
	// Re-verification refuses it too.
	d, _ := a.db.PagesDomain(ctx, f.demo.ID)
	if err := a.pagesVerifyNow(ctx, d); err == nil {
		t.Error("re-verifying the app host succeeded")
	}
}

func TestPagesDomainFileVerification(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	fakeTXT(t, map[string][]string{})
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/domain", url.Values{"host": {"docs.example.com"}}, f.ck), http.StatusSeeOther, "claim")
	d, _ := a.db.PagesDomain(ctx, f.demo.ID)

	served := "wrong"
	var gotHost, gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		if r.URL.Path == "/.well-known/atelier-pages/"+d.Token {
			fmt.Fprintln(w, served)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	saved := pagesVerifyClient
	t.Cleanup(func() { pagesVerifyClient = saved })
	pagesVerifyClient = func(*App) *http.Client {
		// docs.example.com → the fake server, keeping the redirect rule.
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
			},
		}
		return &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: pagesVerifyRedirect}
	}
	mustContain(t, do(t, a, "POST", "/zoe/demo/settings/pages/domain/verify", url.Values{}, f.ck).Body.String(), "wrong file", "Not verified yet", "doesn&#39;t contain the token")
	if gotHost != "docs.example.com" || gotPath != "/.well-known/atelier-pages/"+d.Token {
		t.Errorf("fetched %s %s", gotHost, gotPath)
	}
	served = d.Token
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/pages/domain/verify", url.Values{}, f.ck), http.StatusSeeOther, "verify by file")
	if d2, _ := a.db.PagesDomain(ctx, f.demo.ID); !d2.Verified() || d2.Method != "file" {
		t.Fatalf("domain = %+v", d2)
	}

	// The real client goes through the SSRF guard: names resolving to
	// loopback or private addresses are refused before connecting, and
	// plain http isn't used.
	pagesVerifyClient = saved
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.10", "169.254.169.254", "::1"} {
		a.apps().lookup = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr(ip)}, nil }
		err := a.pagesCheckFile(ctx, d)
		if err == nil || !strings.Contains(err.Error(), "private or local") {
			t.Errorf("%s: %v", ip, err)
		}
	}
	if err := pagesVerifyRedirect(httptest.NewRequest("GET", "http://docs.example.com/x", nil), nil); err == nil {
		t.Error("redirect to http allowed")
	}
}

func TestPagesDomainRecheck(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	noFileVerify(t)
	pullsCommit(t, f.work, "index.html", "<h1>Kept</h1>", "home")
	f.push(t, "main")
	f.save(t, url.Values{"source": {"branch"}, "branch": {"main"}}, http.StatusSeeOther)
	f.verifyDomain(t, "demo", "docs.example.com")
	pagesWant(t, pagesHostGet(t, a, "docs.example.com", "/", nil), 200, "", "Kept")

	// Not due yet: nothing is looked up.
	lookups := 0
	pagesLookupTXT = func(ctx context.Context, name string) ([]string, error) {
		lookups++
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	now := time.Now()
	a.pagesRecheckDomains(ctx, now.Add(time.Hour))
	if lookups != 0 {
		t.Errorf("re-checked early (%d lookups)", lookups)
	}
	// The record is gone: two failed daily checks keep the site, the third un-verifies.
	for i := 1; i <= 3; i++ {
		a.pagesRecheckDomains(ctx, now.Add(time.Duration(i)*25*time.Hour))
		d, _ := a.db.PagesDomain(ctx, f.demo.ID)
		if d.Failures != i || d.Verified() != (i < 3) {
			t.Fatalf("after %d failures: %+v", i, d)
		}
		if i < 3 {
			pagesWant(t, pagesHostGet(t, a, "docs.example.com", "/", nil), 200, "", "Kept")
		}
	}
	if rec := pagesHostGet(t, a, "docs.example.com", "/", nil); strings.Contains(rec.Body.String(), "Kept") {
		t.Error("un-verified host still served")
	}
	mustContain(t, do(t, a, "GET", "/zoe/demo/settings/pages", nil, f.ck).Body.String(), "unverified card", "Not verified", "no TXT record")

	// A pending claim is re-checked hourly and picked up when DNS appears.
	d, _ := a.db.PagesDomain(ctx, f.demo.ID)
	fakeTXT(t, map[string][]string{"_atelier-pages.docs.example.com": {d.Token}})
	a.pagesRecheckDomains(ctx, time.Now().Add(2*time.Hour))
	if d, _ := a.db.PagesDomain(ctx, f.demo.ID); !d.Verified() || d.Failures != 0 {
		t.Errorf("pending claim not picked up: %+v", d)
	}
	pagesWant(t, pagesHostGet(t, a, "docs.example.com", "/", nil), 200, "", "Kept")

	// DNS that hangs times out.
	pagesVerifyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { pagesVerifyTimeout = 8 * time.Second })
	pagesLookupTXT = func(ctx context.Context, name string) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	if err := pagesCheckTXT(ctx, d); err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 2*time.Second {
		t.Errorf("hanging DNS: %v after %s", err, time.Since(start))
	}
}

func TestPagesDomainSettingsNeedAdmin(t *testing.T) {
	f := newPagesFix(t)
	a := f.a
	ctx := context.Background()
	noFileVerify(t)
	f.verifyDomain(t, "demo", "docs.example.com")
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sc := cookieFor(t, a, sam.ID)
	for _, p := range []string{"/zoe/demo/settings/pages/domain", "/zoe/demo/settings/pages/domain/verify", "/zoe/demo/settings/pages/domain/remove",
		"/zoe/demo/settings/pages/previews", "/zoe/demo/settings/pages/previews/approve"} {
		form := url.Values{"host": {"evil.example.com"}, "previews": {""}, "number": {"1"}}
		if rec := do(t, a, "POST", p, form, sc); rec.Code != http.StatusForbidden {
			t.Errorf("sam POST %s = %d", p, rec.Code)
		}
		if rec := do(t, a, "POST", p, form, nil); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin") {
			t.Errorf("anonymous POST %s = %d", p, rec.Code)
		}
	}
	if d, _ := a.db.PagesDomain(ctx, f.demo.ID); d.Host != "docs.example.com" || !d.Verified() {
		t.Errorf("domain changed by sam: %+v", d)
	}
	if on, _ := a.db.PagesPreviewsOn(ctx, f.demo.ID); !on {
		t.Error("previews turned off by sam")
	}
}

// A custom domain added as a public hostname on the named tunnel reaches
// the remote listener, which serves it only when Pages is exposed.
func TestPagesDomainThroughTunnel(t *testing.T) {
	f := newTunnelFix(t)
	a := f.a
	ctx := context.Background()
	f.turnOn(t)
	a.cfg.PagesURL = "https://pages.infinityhomelab.lan"
	rp := profRepo(t, a, f.owner, "demo", false, "index.html", "<h1>Tunnelled</h1>")
	_ = a.db.SavePagesSite(ctx, store.PagesSite{RepoID: rp.ID, Enabled: true, Source: store.PagesFromBranch, Branch: "main"})
	ps, _ := a.db.PagesSite(ctx, rp.ID)
	if _, err := a.pagesDeployBranch(ctx, rp, ps, "", "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetPagesDomain(ctx, rp.ID, "docs.example.com", "tok", pagesRouteTunnel); err != nil {
		t.Fatal(err)
	}
	if err := a.db.PagesDomainVerified(ctx, rp.ID, "txt"); err != nil {
		t.Fatal(err)
	}
	a.pagesDomainsChanged()
	named := store.TunnelConfig{Enabled: true, Mode: "named", Hostname: tunnelTestHost, ExposeWeb: true}
	a.tunnel().cfg.Store(&named)
	get := func(host string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = host
		req.RemoteAddr = "127.0.0.1:50123"
		req.Header.Set("CF-Connecting-IP", "203.0.113.7")
		rec := httptest.NewRecorder()
		a.tunnel().ServeHTTP(rec, req)
		return rec
	}
	if rec := get("docs.example.com"); rec.Code != 404 || strings.Contains(rec.Body.String(), "Tunnelled") {
		t.Errorf("Pages not exposed: %d", rec.Code)
	}
	named.ExposePages = true
	rec := get("Docs.Example.com:443")
	pagesWant(t, rec, 200, "", "Tunnelled")
	if rec.Header().Get("Content-Security-Policy") != pagesDomainCSP || len(rec.Header().Values("Set-Cookie")) > 0 {
		t.Errorf("tunnel custom domain headers = %v", rec.Header())
	}
	// Unverified names get the remote listener's ordinary handling.
	if rec := get("other.example.com"); strings.Contains(rec.Body.String(), "Tunnelled") {
		t.Error("unverified host served through the tunnel")
	}
	// The tunnel's own host stays the app.
	if rec := get(tunnelTestHost); strings.Contains(rec.Body.String(), "Tunnelled") {
		t.Error("tunnel host served the site")
	}
	// The private-site sign-in sends tunnel visitors to the tunnel's app address.
	_ = a.db.UpdateRepo(ctx, rp.ID, "", true)
	rec = get("docs.example.com")
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "https://"+tunnelTestHost+"/-/pages/signin?host=docs.example.com") {
		t.Errorf("private through tunnel = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}
