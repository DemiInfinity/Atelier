package web

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestPWAManifest(t *testing.T) {
	a := newTestApp(t)
	rec := do(t, a, "GET", "/manifest.webmanifest", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/manifest+json") {
		t.Errorf("manifest Content-Type = %q", ct)
	}
	var m webManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest JSON: %v", err)
	}
	if m.Name != "Atelier" || m.ShortName != "Atelier" || m.Display != "standalone" || m.StartURL == "" || m.Scope != "/" ||
		m.ThemeColor != "#1a1917" || m.BackgroundColor != "#131211" {
		t.Errorf("manifest = %+v", m)
	}
	want := map[string]bool{"192x192 any": false, "512x512 any": false, "512x512 maskable": false}
	for _, ic := range m.Icons {
		key := ic.Sizes + " " + ic.Purpose
		if _, ok := want[key]; ok {
			want[key] = true
		}
		// Every icon is served.
		r := do(t, a, "GET", ic.Src, nil, nil)
		if r.Code != http.StatusOK || r.Header().Get("Content-Type") != ic.Type {
			t.Errorf("icon %s = %d %q, want 200 %q", ic.Src, r.Code, r.Header().Get("Content-Type"), ic.Type)
		}
	}
	for k, ok := range want {
		if !ok {
			t.Errorf("manifest has no %s icon", k)
		}
	}
	if r := do(t, a, "GET", "/static/icons/apple-touch-icon.png", nil, nil); r.Code != http.StatusOK || r.Header().Get("Content-Type") != "image/png" {
		t.Errorf("apple touch icon = %d %q", r.Code, r.Header().Get("Content-Type"))
	}
	if r := do(t, a, "GET", "/static/offline.html", nil, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "Can't reach your studio") {
		t.Errorf("offline page = %d", r.Code)
	}
}

func TestServiceWorkerServed(t *testing.T) {
	a := newTestApp(t)
	rec := do(t, a, "GET", "/sw.js", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sw.js = %d", rec.Code)
	}
	h := rec.Header()
	if ct := h.Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("sw.js Content-Type = %q", ct)
	}
	if h.Get("Service-Worker-Allowed") != "/" || h.Get("Cache-Control") != "no-cache" {
		t.Errorf("sw.js headers = %v", h)
	}
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "worker-src") && !strings.Contains(csp, "worker-src 'self'") {
		t.Errorf("CSP doesn't allow a same-origin worker: %q", csp)
	}
	body := rec.Body.String()
	if strings.Contains(body, pwaVersionPlaceholder) || !strings.Contains(body, "const VERSION = '"+buildVersion()+"'") {
		t.Errorf("sw.js version not filled in")
	}
	if !strings.Contains(body, "const STATIC_CACHE = PREFIX + 'static-' + VERSION;") || !strings.Contains(body, "/static/offline.html") {
		t.Error("sw.js lacks the versioned cache or the offline page")
	}
	// POST to sw.js isn't a thing.
	if r := do(t, a, "POST", "/sw.js", nil, nil); r.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /sw.js = %d", r.Code)
	}
}

// swNeverCache extracts the NEVER_CACHE regular expressions from sw.js and
// compiles them with Go's regexp (the patterns are plain enough for both).
func swNeverCache(t *testing.T, src string) []*regexp.Regexp {
	t.Helper()
	start := strings.Index(src, "const NEVER_CACHE = [")
	end := strings.Index(src[start:], "];")
	if start < 0 || end < 0 {
		t.Fatal("no NEVER_CACHE list in sw.js")
	}
	var out []*regexp.Regexp
	for _, line := range strings.Split(src[start:start+end], "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if !strings.HasPrefix(line, "/") {
			continue
		}
		i := strings.LastIndex(line, "/")
		pat, flags := line[1:i], line[i+1:]
		if strings.Contains(flags, "i") {
			pat = "(?i)" + pat
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			t.Fatalf("pattern %s: %v", line, err)
		}
		out = append(out, re)
	}
	return out
}

func TestServiceWorkerNeverCaches(t *testing.T) {
	b, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	res := swNeverCache(t, src)
	excluded := func(p string) bool {
		for _, re := range res {
			if re.MatchString(p) {
				return true
			}
		}
		return false
	}
	for _, p := range []string{
		"/api/v3/repos/zoe/demo", "/api/actions/jobs", "/mcp", "/npm/@zoe/lib", "/v2/zoe/app/manifests/latest",
		"/go/example.com/mod/@v/list", "/packages/zoe/generic/x", "/osv/query", "/signin", "/auth/github/callback",
		"/zoe/demo.git/info/refs", "/zoe/demo.git/git-upload-pack", "/zoe/demo.GIT/git-receive-pack",
		"/zoe/demo.git/info/lfs/objects/batch", "/zoe/demo/raw/main/secret.env", "/zoe/demo/archive/main.zip",
		"/zoe/demo/releases/download/v1/app.tar.gz", "/zoe/demo/actions/runs/3/logs/stream",
		"/zoe/demo/actions/runs/3/jobs/7/logs", "/notifications/count", "/sw.js",
	} {
		if !excluded(p) {
			t.Errorf("sw.js would handle %s", p)
		}
	}
	for _, p := range []string{"/", "/static/atelier.css", "/zoe/demo", "/zoe/demo/insights", "/notifications", "/manifest.webmanifest"} {
		if excluded(p) {
			t.Errorf("sw.js excludes %s, which it should handle", p)
		}
	}
	// Responses the server marks no-store or private are never put in a cache,
	// nor is anything that isn't a same-origin GET or carries credentials.
	for _, s := range []string{"no-store|private", "request.method !== 'GET'", "url.origin !== self.location.origin",
		"request.headers.has('Authorization')", "if (storable(response))", "caches.delete(k)"} {
		if !strings.Contains(src, s) {
			t.Errorf("sw.js lacks %q", s)
		}
	}
	if strings.Count(src, "cache.put(") != 1 || strings.Count(src, "c.put(") != 1 {
		t.Error("every cache write in sw.js must be guarded by storable()")
	}
}

func TestLayoutHasAppTags(t *testing.T) {
	f := newGitFixture(t)
	body := expectStatus(t, do(t, f.a, "GET", "/zoe/demo", nil, pullsCookie(t, f.a)), 200, "repo page")
	mustContain(t, body, "layout", `<link rel="manifest" href="/manifest.webmanifest">`,
		`<link rel="apple-touch-icon" href="/static/icons/apple-touch-icon.png?v=`+buildVersion()+`">`, `<meta name="theme-color" content="#1a1917">`,
		`viewport-fit=cover`, `<script src="/static/pwa.js?v=`+buildVersion()+`" defer></script>`, `href="/zoe/demo/insights"`)
	head := body[:strings.Index(body, "</head>")]
	if i := strings.LastIndex(head, "/static/mobile.css"); i < 0 || strings.Contains(head[i:], `rel="stylesheet"`) {
		t.Error("mobile.css must be the last stylesheet in <head>")
	}
	for _, name := range []string{"insights", "sw.js", "manifest.webmanifest"} {
		if !reserved[name] {
			t.Errorf("%q isn't a reserved account name", name)
		}
	}
}
