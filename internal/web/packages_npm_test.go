package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// npmBody builds what `npm publish` sends.
func npmBody(name, version string, tgz []byte, extra map[string]any) []byte {
	bare := name[strings.LastIndex(name, "/")+1:]
	v := map[string]any{
		"name": name, "version": version, "description": "test package",
		"dist": map[string]any{"shasum": npmSHA1(tgz), "integrity": npmSRI(tgz)},
	}
	for k, x := range extra {
		v[k] = x
	}
	body := map[string]any{
		"_id": name, "name": name, "description": "test package",
		"dist-tags": map[string]string{"latest": version},
		"versions":  map[string]any{version: v},
		"access":    nil,
		"_attachments": map[string]any{name + "-" + version + ".tgz": map[string]any{
			"content_type": "application/octet-stream", "data": base64.StdEncoding.EncodeToString(tgz), "length": len(tgz),
		}},
	}
	_ = bare
	b, _ := json.Marshal(body)
	return b
}

func npmEscape(name string) string { return strings.Replace(name, "/", "%2f", 1) }

func npmPublish(t *testing.T, f *pkgFixture, tok, name, version string, tgz []byte, extra map[string]any) {
	t.Helper()
	resp, body := f.call(t, "PUT", "/npm/"+npmEscape(name), bytes.NewReader(npmBody(name, version, tgz, extra)),
		append(bearer(tok), "Content-Type", "application/json")...)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("publish %s@%s = %d %s", name, version, resp.StatusCode, body)
	}
}

type packument struct {
	Name     string            `json:"name"`
	Rev      string            `json:"_rev"`
	DistTags map[string]string `json:"dist-tags"`
	Versions map[string]struct {
		Version    string `json:"version"`
		Deprecated string `json:"deprecated"`
		Dist       struct {
			Tarball   string `json:"tarball"`
			Shasum    string `json:"shasum"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	} `json:"versions"`
}

func getPackument(t *testing.T, f *pkgFixture, name string, hdr ...string) packument {
	t.Helper()
	resp, body := f.call(t, "GET", "/npm/"+npmEscape(name), nil, hdr...)
	if resp.StatusCode != 200 {
		t.Fatalf("GET packument %s = %d %s", name, resp.StatusCode, body)
	}
	var p packument
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNpmRegistryProtocol(t *testing.T) {
	f := newPkgFixture(t)
	tgz := []byte("pretend this is a gzipped tarball")
	body := npmBody("@zoe/hello", "1.0.0", tgz, nil)
	jsonHdr := []string{"Content-Type", "application/json"}

	// Auth and scopes.
	if resp, _ := f.call(t, "PUT", "/npm/@zoe%2fhello", bytes.NewReader(body), jsonHdr...); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous publish = %d, want 401", resp.StatusCode)
	}
	if resp, _ := f.call(t, "PUT", "/npm/@zoe%2fhello", bytes.NewReader(body), append(bearer(f.read), jsonHdr...)...); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read-token publish = %d, want 403", resp.StatusCode)
	}
	if resp, _ := f.call(t, "PUT", "/npm/@zoe%2fhello", bytes.NewReader(body), append(bearer(f.repoOnly), jsonHdr...)...); resp.StatusCode != http.StatusForbidden {
		t.Errorf("repo-only token publish = %d, want 403", resp.StatusCode)
	}
	if resp, _ := f.call(t, "PUT", "/npm/@zoe%2fhello", bytes.NewReader(body), append(bearer("atl_nonsense"), jsonHdr...)...); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token publish = %d, want 401", resp.StatusCode)
	}
	if resp, _ := f.call(t, "PUT", "/npm/hello", bytes.NewReader(npmBody("hello", "1.0.0", tgz, nil)), append(bearer(f.write), jsonHdr...)...); resp.StatusCode != http.StatusForbidden {
		t.Errorf("unscoped publish = %d, want 403", resp.StatusCode)
	}
	// Tampered tarball.
	bad := npmBody("@zoe/hello", "1.0.0", tgz, map[string]any{"dist": map[string]any{"shasum": strings.Repeat("0", 40)}})
	if resp, _ := f.call(t, "PUT", "/npm/@zoe%2fhello", bytes.NewReader(bad), append(bearer(f.write), jsonHdr...)...); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad shasum publish = %d, want 400", resp.StatusCode)
	}
	if _, err := f.a.db.Package(context.Background(), f.zoe.ID, "npm", "hello"); err == nil {
		t.Error("failed first publish left an empty package behind")
	}

	npmPublish(t, f, f.write, "@zoe/hello", "1.0.0", tgz, map[string]any{"repository": map[string]string{"url": "git+" + f.srv.URL + "/zoe/nope.git"}})
	if resp, _ := f.call(t, "PUT", "/npm/@zoe%2fhello", bytes.NewReader(body), append(bearer(f.write), jsonHdr...)...); resp.StatusCode != http.StatusConflict {
		t.Errorf("republish 1.0.0 = %d, want 409", resp.StatusCode)
	}
	// Company scope works as its own owner.
	npmPublish(t, f, f.both, "@elyndra/ui", "0.1.0", []byte("ui"), nil)

	// Install: packument with tarball URLs from the server's URL.
	if resp, _ := f.call(t, "GET", "/npm/@zoe%2fhello", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("anonymous GET of private package = %d, want 404", resp.StatusCode)
	}
	if resp, _ := f.call(t, "GET", "/npm/@zoe%2fhello", nil, bearer(f.repoOnly)...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("repo-only token GET = %d, want 404", resp.StatusCode)
	}
	p := getPackument(t, f, "@zoe/hello", bearer(f.read)...)
	v := p.Versions["1.0.0"]
	wantURL := f.srv.URL + "/npm/@zoe/hello/-/hello-1.0.0.tgz"
	if p.Name != "@zoe/hello" || p.DistTags["latest"] != "1.0.0" || v.Dist.Tarball != wantURL || v.Dist.Integrity != npmSRI(tgz) {
		t.Fatalf("packument = %+v", p)
	}
	resp, got := f.call(t, "GET", strings.TrimPrefix(v.Dist.Tarball, f.srv.URL), nil, bearer(f.read)...)
	if resp.StatusCode != 200 || !bytes.Equal(got, tgz) {
		t.Fatalf("tarball = %d", resp.StatusCode)
	}
	// Also with an encoded slash, as some clients send.
	if resp, _ := f.call(t, "GET", "/npm/@zoe%2Fhello/-/hello-1.0.0.tgz", nil, bearer(f.read)...); resp.StatusCode != 200 {
		t.Errorf("tarball via %%2F = %d", resp.StatusCode)
	}

	// whoami / ping.
	if resp, body := f.call(t, "GET", "/npm/-/whoami", nil, bearer(f.read)...); resp.StatusCode != 200 || !strings.Contains(string(body), `"zoe"`) {
		t.Errorf("whoami = %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.call(t, "GET", "/npm/-/whoami", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous whoami = %d", resp.StatusCode)
	}
	if resp, _ := f.call(t, "GET", "/npm/-/ping", nil); resp.StatusCode != 200 {
		t.Errorf("ping = %d", resp.StatusCode)
	}

	// A second version, dist-tags.
	npmPublish(t, f, f.write, "@zoe/hello", "1.1.0-beta.1", []byte("beta"), nil)
	if resp, body := f.call(t, "PUT", "/npm/-/package/@zoe%2fhello/dist-tags/beta", strings.NewReader(`"1.1.0-beta.1"`), append(bearer(f.write), jsonHdr...)...); resp.StatusCode != http.StatusCreated {
		t.Fatalf("add dist-tag = %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.call(t, "PUT", "/npm/-/package/@zoe%2fhello/dist-tags/beta", strings.NewReader(`"1.0.0"`), append(bearer(f.read), jsonHdr...)...); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read token dist-tag = %d, want 403", resp.StatusCode)
	}
	// Publishing 1.1.0-beta.1 with dist-tags latest moved latest; put it back.
	f.call(t, "PUT", "/npm/-/package/@zoe%2fhello/dist-tags/latest", strings.NewReader(`"1.0.0"`), append(bearer(f.write), jsonHdr...)...)
	resp, tagsBody := f.call(t, "GET", "/npm/-/package/@zoe%2fhello/dist-tags", nil, bearer(f.read)...)
	var tags map[string]string
	json.Unmarshal(tagsBody, &tags)
	if resp.StatusCode != 200 || tags["beta"] != "1.1.0-beta.1" || tags["latest"] != "1.0.0" {
		t.Errorf("dist-tags = %d %v", resp.StatusCode, tags)
	}
	if resp, _ := f.call(t, "DELETE", "/npm/-/package/@zoe%2fhello/dist-tags/beta", nil, bearer(f.write)...); resp.StatusCode != 200 {
		t.Errorf("delete dist-tag = %d", resp.StatusCode)
	}

	// Deprecate: npm sends the packument back with "deprecated" set.
	p = getPackument(t, f, "@zoe/hello", bearer(f.write)...)
	var raw map[string]any
	_, pb := f.call(t, "GET", "/npm/@zoe%2fhello", nil, bearer(f.write)...)
	json.Unmarshal(pb, &raw)
	raw["versions"].(map[string]any)["1.0.0"].(map[string]any)["deprecated"] = "use 2.x"
	db, _ := json.Marshal(raw)
	if resp, body := f.call(t, "PUT", "/npm/@zoe%2fhello", bytes.NewReader(db), append(bearer(f.write), jsonHdr...)...); resp.StatusCode != 200 {
		t.Fatalf("deprecate = %d %s", resp.StatusCode, body)
	}
	if p := getPackument(t, f, "@zoe/hello", bearer(f.read)...); p.Versions["1.0.0"].Deprecated != "use 2.x" {
		t.Errorf("deprecation not stored: %+v", p.Versions["1.0.0"])
	}

	// Unpublish one version: PUT /-rev/ without it, then DELETE the tarball.
	delete(raw["versions"].(map[string]any), "1.1.0-beta.1")
	db, _ = json.Marshal(raw)
	if resp, body := f.call(t, "PUT", "/npm/@zoe%2fhello/-rev/"+p.Rev, bytes.NewReader(db), append(bearer(f.write), jsonHdr...)...); resp.StatusCode != 200 {
		t.Fatalf("unpublish PUT = %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.call(t, "DELETE", "/npm/@zoe%2fhello/-/hello-1.1.0-beta.1.tgz/-rev/x", nil, bearer(f.write)...); resp.StatusCode != 200 {
		t.Errorf("unpublish tarball DELETE = %d", resp.StatusCode)
	}
	if p := getPackument(t, f, "@zoe/hello", bearer(f.read)...); len(p.Versions) != 1 || p.DistTags["latest"] != "1.0.0" {
		t.Errorf("after unpublish: %+v", p)
	}
	// Unpublish the whole package.
	if resp, _ := f.call(t, "DELETE", "/npm/@zoe%2fhello/-rev/x", nil, bearer(f.read)...); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read-token unpublish = %d, want 403", resp.StatusCode)
	}
	if resp, _ := f.call(t, "DELETE", "/npm/@zoe%2fhello/-rev/x", nil, bearer(f.write)...); resp.StatusCode != 200 {
		t.Errorf("unpublish package = %d", resp.StatusCode)
	}
	if resp, _ := f.call(t, "GET", "/npm/@zoe%2fhello", nil, bearer(f.read)...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after unpublish = %d", resp.StatusCode)
	}
	// Unknown scope without the upstream: 404.
	if resp, _ := f.call(t, "GET", "/npm/@nobody%2fthing", nil, bearer(f.read)...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown scope = %d", resp.StatusCode)
	}
	// Size limit for publishes.
	saved := pkgLimits.Npm
	pkgLimits.Npm = 64
	t.Cleanup(func() { pkgLimits.Npm = saved })
	if resp, _ := f.call(t, "PUT", "/npm/@zoe%2fbig", bytes.NewReader(npmBody("@zoe/big", "1.0.0", bytes.Repeat([]byte("x"), 200), nil)), append(bearer(f.write), jsonHdr...)...); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized publish = %d, want 413", resp.StatusCode)
	}
}

func TestNpmUpstreamCache(t *testing.T) {
	f := newPkgFixture(t)
	ctx := context.Background()
	tgz := []byte("left-pad tarball")
	var hits, tarHits atomic.Int32
	var down atomic.Bool
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/left-pad", "/@zoe%2Fsecret", "/@zoe/secret":
			hits.Add(1)
			name := strings.TrimPrefix(r.URL.Path, "/")
			integrity := npmSRI(tgz)
			if name != "left-pad" {
				integrity = npmSRI([]byte("evil"))
			}
			json.NewEncoder(w).Encode(map[string]any{
				"name": "left-pad", "dist-tags": map[string]string{"latest": "1.3.0"},
				"versions": map[string]any{"1.3.0": map[string]any{"name": "left-pad", "version": "1.3.0",
					"dist": map[string]any{"tarball": up.URL + "/left-pad/-/left-pad-1.3.0.tgz", "integrity": integrity}}},
			})
		case "/bad-pkg":
			json.NewEncoder(w).Encode(map[string]any{"name": "bad-pkg", "versions": map[string]any{"1.0.0": map[string]any{
				"dist": map[string]any{"tarball": up.URL + "/bad-pkg/-/bad-pkg-1.0.0.tgz", "integrity": npmSRI([]byte("something else"))}}}})
		case "/left-pad/-/left-pad-1.3.0.tgz", "/bad-pkg/-/bad-pkg-1.0.0.tgz":
			tarHits.Add(1)
			w.Write(tgz)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	// Off by default.
	if resp, _ := f.call(t, "GET", "/npm/left-pad", nil, bearer(f.read)...); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("upstream off: %d, want 404", resp.StatusCode)
	}
	f.a.db.SetSetting(ctx, settingNpmUpstream, "on")
	f.a.db.SetSetting(ctx, settingNpmUpstreamURL, up.URL)

	// Not an open proxy.
	if resp, _ := f.call(t, "GET", "/npm/left-pad", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("anonymous upstream = %d, want 404", resp.StatusCode)
	}
	if resp, _ := f.call(t, "GET", "/npm/left-pad", nil, bearer(f.repoOnly)...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("repo-only token upstream = %d, want 404", resp.StatusCode)
	}
	p := getPackument(t, f, "left-pad", bearer(f.read)...)
	if got := p.Versions["1.3.0"].Dist.Tarball; got != f.srv.URL+"/npm/left-pad/-/left-pad-1.3.0.tgz" {
		t.Fatalf("tarball URL not rewritten: %q", got)
	}
	getPackument(t, f, "left-pad", bearer(f.read)...)
	if hits.Load() != 1 {
		t.Errorf("packument fetched %d times, want 1 (cached)", hits.Load())
	}
	resp, body := f.call(t, "GET", "/npm/left-pad/-/left-pad-1.3.0.tgz", nil, bearer(f.read)...)
	if resp.StatusCode != 200 || !bytes.Equal(body, tgz) {
		t.Fatalf("upstream tarball = %d", resp.StatusCode)
	}
	// Integrity failure.
	if resp, _ := f.call(t, "GET", "/npm/bad-pkg/-/bad-pkg-1.0.0.tgz", nil, bearer(f.read)...); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("bad integrity = %d, want 502", resp.StatusCode)
	}
	// Local scopes never go upstream, even when the name isn't published here.
	if resp, _ := f.call(t, "GET", "/npm/@zoe%2fsecret", nil, bearer(f.read)...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("local scope proxied: %d", resp.StatusCode)
	}

	// Upstream goes away: the tarball is cached forever, the stale packument is served.
	down.Store(true)
	f.a.db.DB().Exec(`UPDATE package_npm_cache SET fetched_at = ?`, time.Now().Add(-time.Hour).Unix())
	if p := getPackument(t, f, "left-pad", bearer(f.read)...); p.DistTags["latest"] != "1.3.0" {
		t.Error("stale packument not served while upstream is down")
	}
	before := tarHits.Load()
	if resp, body := f.call(t, "GET", "/npm/left-pad/-/left-pad-1.3.0.tgz", nil, bearer(f.read)...); resp.StatusCode != 200 || !bytes.Equal(body, tgz) {
		t.Errorf("cached tarball = %d", resp.StatusCode)
	}
	if tarHits.Load() != before {
		t.Error("cached tarball was fetched again")
	}
	// GC keeps cached tarballs.
	refs, _ := f.a.db.ReferencedBlobs(ctx, 0)
	if len(refs) != 1 {
		t.Errorf("cached tarball not referenced: %v", refs)
	}
}

// TestNpmCLI publishes and installs with the real npm, if it's installed.
func TestNpmCLI(t *testing.T) {
	npm, err := exec.LookPath("npm")
	if err != nil {
		t.Skip("npm not installed")
	}
	f := newPkgFixture(t)
	home := t.TempDir()
	host := strings.TrimPrefix(f.srv.URL, "http://")
	npmrc := "@zoe:registry=" + f.srv.URL + "/npm/\n//" + host + "/npm/:_authToken=" + f.write + "\n"
	env := append(os.Environ(), "HOME="+home, "npm_config_userconfig="+filepath.Join(home, ".npmrc"),
		"npm_config_cache="+filepath.Join(home, "cache"), "npm_config_update_notifier=false", "npm_config_fund=false",
		"npm_config_audit=false", "npm_config_loglevel=warn")
	if err := os.WriteFile(filepath.Join(home, ".npmrc"), []byte(npmrc), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, npm, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("npm %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	pkgDir := filepath.Join(home, "pkg")
	os.MkdirAll(pkgDir, 0o755)
	os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"@zoe/greet","version":"1.2.3","main":"index.js","license":"MIT"}`), 0o644)
	os.WriteFile(filepath.Join(pkgDir, "index.js"), []byte("module.exports = () => 'hi from atelier';\n"), 0o644)
	run(pkgDir, "publish")
	if out := run(pkgDir, "whoami", "--registry", f.srv.URL+"/npm/"); !strings.Contains(out, "zoe") {
		t.Errorf("npm whoami = %q", out)
	}
	if out := run(pkgDir, "view", "@zoe/greet", "version"); strings.TrimSpace(out) != "1.2.3" {
		t.Errorf("npm view = %q", out)
	}

	app := filepath.Join(home, "app")
	os.MkdirAll(app, 0o755)
	os.WriteFile(filepath.Join(app, "package.json"), []byte(`{"name":"app","version":"0.0.0","private":true}`), 0o644)
	run(app, "install", "@zoe/greet@1.2.3")
	got, err := os.ReadFile(filepath.Join(app, "node_modules", "@zoe", "greet", "index.js"))
	if err != nil || !strings.Contains(string(got), "hi from atelier") {
		t.Fatalf("installed file: %v %q", err, got)
	}
	run(pkgDir, "deprecate", "@zoe/greet@1.2.3", "old")
	run(pkgDir, "dist-tag", "add", "@zoe/greet@1.2.3", "stable")
	if out := run(pkgDir, "dist-tag", "ls", "@zoe/greet"); !strings.Contains(out, "stable: 1.2.3") {
		t.Errorf("dist-tag ls = %q", out)
	}
	run(pkgDir, "unpublish", "@zoe/greet@1.2.3", "--force")
	if _, err := f.a.db.Package(context.Background(), f.zoe.ID, "npm", "greet"); err == nil {
		if p, _ := f.a.db.Package(context.Background(), f.zoe.ID, "npm", "greet"); p.Versions != 0 {
			t.Errorf("unpublish left %d versions", p.Versions)
		}
	}
}
