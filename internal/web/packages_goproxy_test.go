package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// goRepoFixture creates zoe/lib with go.mod "atelier.test/zoe/lib", tags
// v1.0.0 and v1.1.0, a v2 line under /v2, and pushes it into Atelier.
func goRepoFixture(t *testing.T, f *pkgFixture, private bool) {
	t.Helper()
	ctx := context.Background()
	if err := f.a.git.Init(ctx, "zoe", "lib"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.db.CreateRepo(ctx, f.zoe.ID, "lib", "A tiny library", private); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, body string) {
		os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755)
		os.WriteFile(filepath.Join(work, name), []byte(body), 0o644)
	}
	git("init", "-q", "-b", "main")
	write("go.mod", "module atelier.test/zoe/lib\n\ngo 1.21\n")
	write("lib.go", "package lib\n\nfunc Hello() string { return \"hello v1.0\" }\n")
	write("LICENSE", "MIT\n")
	write("vendor/x/x.go", "package x\n") // never in module zips
	git("add", ".")
	git("commit", "-q", "-m", "v1.0.0")
	git("tag", "v1.0.0")
	write("lib.go", "package lib\n\nfunc Hello() string { return \"hello v1.1\" }\n")
	git("commit", "-q", "-am", "v1.1.0")
	git("tag", "v1.1.0")
	git("tag", "not-semver")
	write("go.mod", "module atelier.test/zoe/lib/v2\n\ngo 1.21\n")
	write("lib.go", "package lib\n\nfunc Hello() string { return \"hello v2\" }\n")
	git("commit", "-q", "-am", "v2")
	git("tag", "v2.0.0")
	write("lib.go", "package lib\n\nfunc Hello() string { return \"hello main\" }\n")
	git("commit", "-q", "-am", "untagged work")
	git("push", "-q", f.a.git.Path("zoe", "lib"), "main", "--tags")
}

func TestGoProxyProtocol(t *testing.T) {
	f := newPkgFixture(t)
	goRepoFixture(t, f, true)
	const mod = "/go/atelier.test/zoe/lib/@v/"

	if resp, _ := f.call(t, "GET", mod+"list", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("anonymous list of private module = %d, want 404", resp.StatusCode)
	}
	auth := basicAuth("zoe", f.read) // packages:read is enough for the proxy
	resp, body := f.call(t, "GET", mod+"list", nil, auth...)
	if resp.StatusCode != 200 || string(body) != "v1.1.0\nv1.0.0\n" {
		t.Fatalf("list = %d %q", resp.StatusCode, body)
	}
	resp, body = f.call(t, "GET", "/go/atelier.test/zoe/lib/@latest", nil, auth...)
	var info struct{ Version, Time string }
	json.Unmarshal(body, &info)
	if resp.StatusCode != 200 || info.Version != "v1.1.0" {
		t.Errorf("@latest = %d %s", resp.StatusCode, body)
	}
	if _, err := time.Parse(time.RFC3339, info.Time); err != nil {
		t.Errorf("info time %q: %v", info.Time, err)
	}
	if resp, body := f.call(t, "GET", mod+"v1.0.0.mod", nil, auth...); resp.StatusCode != 200 || !strings.Contains(string(body), "module atelier.test/zoe/lib") {
		t.Errorf(".mod = %d %q", resp.StatusCode, body)
	}
	resp, body = f.call(t, "GET", mod+"v1.0.0.zip", nil, auth...)
	if resp.StatusCode != 200 {
		t.Fatalf(".zip = %d %s", resp.StatusCode, body)
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, zf := range zr.File {
		names[zf.Name] = true
	}
	if !names["atelier.test/zoe/lib@v1.0.0/lib.go"] || !names["atelier.test/zoe/lib@v1.0.0/go.mod"] || names["atelier.test/zoe/lib@v1.0.0/vendor/x/x.go"] {
		t.Errorf("zip files = %v", names)
	}
	tmp := filepath.Join(t.TempDir(), "m.zip")
	os.WriteFile(tmp, body, 0o644)
	if _, err := modzip.CheckZip(module.Version{Path: "atelier.test/zoe/lib", Version: "v1.0.0"}, tmp); err != nil {
		t.Errorf("zip fails module checks: %v", err)
	}
	// Major version suffix maps to v2 tags.
	if resp, body := f.call(t, "GET", "/go/atelier.test/zoe/lib/v2/@v/list", nil, auth...); string(body) != "v2.0.0\n" {
		t.Errorf("v2 list = %d %q", resp.StatusCode, body)
	}
	if resp, _ := f.call(t, "GET", mod+"v2.0.0.info", nil, auth...); resp.StatusCode != http.StatusNotFound { // a semver query is never a branch
		t.Errorf("v2 tag through the v1 path = %d, want 404", resp.StatusCode)
	}
	// Branches resolve to pseudo-versions, which then work for .mod/.zip.
	resp, body = f.call(t, "GET", mod+"main.info", nil, auth...)
	json.Unmarshal(body, &info)
	if resp.StatusCode != 200 || !module.IsPseudoVersion(info.Version) {
		t.Fatalf("main.info = %d %s", resp.StatusCode, body)
	}
	// main's go.mod says /v2, so the v0 pseudo-version has no module here.
	if resp, _ := f.call(t, "GET", mod+info.Version+".zip", nil, auth...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("zip for a commit whose go.mod is another module = %d, want 404", resp.StatusCode)
	}
	resp, body = f.call(t, "GET", "/go/atelier.test/zoe/lib/v2/@v/main.info", nil, auth...)
	json.Unmarshal(body, &info)
	if resp, _ := f.call(t, "GET", "/go/atelier.test/zoe/lib/v2/@v/"+info.Version+".zip", nil, auth...); resp.StatusCode != 200 {
		t.Errorf("pseudo-version zip = %d", resp.StatusCode)
	}
	// Unknown versions, bad paths, repo-only tokens still read (they can clone anyway).
	if resp, _ := f.call(t, "GET", mod+"v9.9.9.info", nil, auth...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown version = %d", resp.StatusCode)
	}
	if resp, _ := f.call(t, "GET", "/go/atelier.test/zoe/nope/@v/list", nil, auth...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown module = %d", resp.StatusCode)
	}
	// Downloads are counted on a "go" package.
	if resp, body := f.call(t, "GET", "/packages/zoe/go/lib", nil, f.cookie()...); resp.StatusCode != 200 || !strings.Contains(string(body), "go get ") {
		t.Errorf("go package page = %d", resp.StatusCode)
	}
	p, err := f.a.db.Package(context.Background(), f.zoe.ID, "go", "lib")
	if err != nil || p.Downloads < 1 {
		t.Errorf("go downloads not counted: %+v %v", p, err)
	}
}

func TestGoGetMeta(t *testing.T) {
	f := newPkgFixture(t)
	goRepoFixture(t, f, false)
	resp, body := f.call(t, "GET", "/zoe/lib/sub/pkg?go-get=1", nil)
	host := strings.TrimPrefix(f.srv.URL, "http://")
	want := `<meta name="go-import" content="` + host + `/zoe/lib git ` + f.srv.URL + `/zoe/lib.git">`
	if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
		t.Errorf("go-get = %d\n%s\nwant %s", resp.StatusCode, body, want)
	}
	// Private repositories don't reveal themselves to anonymous go-get.
	f.a.db.UpdateRepo(context.Background(), mustRepo(t, f, "zoe", "lib").ID, "", true)
	if resp, body := f.call(t, "GET", "/zoe/lib?go-get=1", nil); strings.Contains(string(body), "go-import") {
		t.Errorf("private repo go-get = %d leaked meta", resp.StatusCode)
	}
	if _, body := f.call(t, "GET", "/zoe/lib?go-get=1", nil, basicAuth("zoe", f.read)...); !strings.Contains(string(body), "go-import") {
		t.Errorf("authenticated go-get of private repo has no meta")
	}
}

func mustRepo(t *testing.T, f *pkgFixture, owner, name string) (r struct{ ID int64 }) {
	t.Helper()
	rp, err := f.a.db.RepoByName(context.Background(), owner, name)
	if err != nil {
		t.Fatal(err)
	}
	r.ID = rp.ID
	return r
}

// TestGoModDownload fetches a module with the real go command through the proxy.
func TestGoModDownload(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	f := newPkgFixture(t)
	// The go command only sends credentials (~/.netrc, or user:pass in
	// GOPROXY) over HTTPS, so against the plain-HTTP test server the module
	// is public; TestGoProxyProtocol covers private modules with Basic auth.
	goRepoFixture(t, f, false)
	home := t.TempDir()
	proxy := f.srv.URL + "/go"
	cmd := exec.Command(goBin, "mod", "download", "-json", "atelier.test/zoe/lib@v1.1.0")
	cmd.Dir = home
	cmd.Env = append(os.Environ(), "HOME="+home, "GOPROXY="+proxy, "GOSUMDB=off",
		"GONOSUMDB=*", "GOFLAGS=-modcacherw", "GOMODCACHE="+filepath.Join(home, "modcache"), "GOPATH="+filepath.Join(home, "gopath"),
		"GOPRIVATE=", "GONOPROXY=", "GOTOOLCHAIN=local", "GO111MODULE=on")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod download: %v\n%s", err, out)
	}
	var res struct{ Version, Dir, Error string }
	json.Unmarshal(out, &res)
	if res.Version != "v1.1.0" || res.Error != "" {
		t.Fatalf("go mod download = %s", out)
	}
	src, err := os.ReadFile(filepath.Join(res.Dir, "lib.go"))
	if err != nil || !strings.Contains(string(src), "hello v1.1") {
		t.Errorf("downloaded lib.go = %q %v", src, err)
	}
}
