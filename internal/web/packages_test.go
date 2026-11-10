package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atelier/internal/backup"
	"atelier/internal/packages"
	"atelier/internal/store"
)

// pkgFixture is an Atelier with a person "zoe", a company "elyndra", tokens
// and a real HTTP server (registry clients need absolute URLs).
type pkgFixture struct {
	a        *App
	srv      *httptest.Server
	zoe      store.Owner
	elyndra  store.Owner
	write    string // packages:read + packages:write
	read     string // packages:read
	repoOnly string // repo:read + repo:write (no packages scopes)
	both     string // repo + packages, read and write
	sess     string // zoe's browser session
}

func newPkgFixture(t *testing.T) *pkgFixture {
	t.Helper()
	a := newTestApp(t)
	ctx := context.Background()
	zoe, err := a.db.CreateOwner(ctx, "zoe", "Zoë", "user")
	if err != nil {
		t.Fatal(err)
	}
	ely, err := a.db.CreateOwner(ctx, "elyndra", "Elyndra Studios", "company")
	if err != nil {
		t.Fatal(err)
	}
	tok := func(access string) string {
		s, err := a.db.CreateToken(ctx, access, tokenScopes[access], time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	sess, err := a.db.CreateSessionFor(ctx, zoe.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f := &pkgFixture{a: a, zoe: zoe, elyndra: ely, write: tok("packages-write"), read: tok("packages-read"),
		repoOnly: tok("write"), both: tok("write-packages"), sess: sess}
	f.srv = httptest.NewServer(a)
	t.Cleanup(f.srv.Close)
	return f
}

// call makes a request against the test server. hdr is key, value pairs.
func (f *pkgFixture) call(t *testing.T, method, path string, body io.Reader, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := noRedirect().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func bearer(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

func (f *pkgFixture) cookie() []string { return []string{"Cookie", sessionCookie + "=" + f.sess} }

func TestCanPackageScopes(t *testing.T) {
	f := newPkgFixture(t)
	ctx := context.Background()
	a := f.a
	me := f.zoe
	tokActor := func(scopes ...string) Actor {
		return Actor{Kind: ActorToken, Name: "t", PersonID: me.ID, Scopes: scopes}
	}
	owner := Actor{Kind: ActorOwner, Name: me.Name, PersonID: me.ID}
	anon := Actor{Kind: ActorAnonymous}
	stranger := Actor{Kind: ActorOwner, Name: "x", PersonID: me.ID + 99}
	cases := []struct {
		name    string
		who     Actor
		private bool
		need    Perm
		want    bool
	}{
		{"owner reads private", owner, true, PermRead, true},
		{"owner administers", owner, true, PermAdmin, true},
		{"packages:read reads private", tokActor("packages:read"), true, PermRead, true},
		{"packages:read can't publish", tokActor("packages:read"), true, PermWrite, false},
		{"packages:write publishes", tokActor("packages:read", "packages:write"), true, PermWrite, true},
		{"packages:write alone also reads", tokActor("packages:write"), true, PermRead, true},
		{"tokens never administer", tokActor("packages:read", "packages:write"), true, PermAdmin, false},
		{"repo-only token can't read packages", tokActor("repo:read", "repo:write"), true, PermRead, false},
		{"repo-only token can't publish", tokActor("repo:read", "repo:write"), false, PermWrite, false},
		{"anonymous reads public", anon, false, PermRead, true},
		{"anonymous can't read private", anon, true, PermRead, false},
		{"anonymous can't publish", anon, false, PermWrite, false},
		{"another person is locked out", stranger, true, PermRead, false},
	}
	for _, c := range cases {
		if got := a.canPackage(ctx, c.who, me, c.private, c.need); got != c.want {
			t.Errorf("%s: canPackage(%s) = %v, want %v", c.name, c.need, got, c.want)
		}
	}
}

func TestGenericFiles(t *testing.T) {
	f := newPkgFixture(t)
	const p = "/api/v1/packages/zoe/generic/app/1.0.0/app.tar.gz"
	content := []byte("hello generic world")

	if resp, _ := f.call(t, "PUT", p, bytes.NewReader(content)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous PUT = %d, want 401", resp.StatusCode)
	}
	if resp, _ := f.call(t, "PUT", p, bytes.NewReader(content), bearer(f.read)...); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("read token PUT = %d, want 403", resp.StatusCode)
	}
	if resp, _ := f.call(t, "PUT", p, bytes.NewReader(content), bearer(f.repoOnly)...); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("repo-only token PUT = %d, want 403", resp.StatusCode)
	}
	resp, body := f.call(t, "PUT", p, bytes.NewReader(content), bearer(f.write)...)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.call(t, "PUT", p, bytes.NewReader(content), bearer(f.write)...); resp.StatusCode != http.StatusConflict {
		t.Errorf("second PUT of the same file = %d, want 409", resp.StatusCode)
	}
	// Wrong checksum header.
	if resp, _ := f.call(t, "PUT", "/api/v1/packages/zoe/generic/app/1.0.0/other.bin", bytes.NewReader(content),
		append(bearer(f.write), "X-Checksum-Sha256", strings.Repeat("0", 64))...); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad checksum = %d, want 400", resp.StatusCode)
	}
	// Private by default: anonymous can't see it, a read token can.
	if resp, _ := f.call(t, "GET", p, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET private = %d, want 401", resp.StatusCode)
	}
	if resp, _ := f.call(t, "GET", p, nil, bearer(f.repoOnly)...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("repo-only token GET = %d, want 404", resp.StatusCode)
	}
	resp, body = f.call(t, "GET", p, nil, bearer(f.read)...)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("GET = %d %q", resp.StatusCode, body)
	}
	// The browser (session cookie) can download too.
	if resp, _ := f.call(t, "GET", p, nil, f.cookie()...); resp.StatusCode != http.StatusOK {
		t.Errorf("GET with session = %d", resp.StatusCode)
	}
	resp, body = f.call(t, "GET", "/api/v1/packages/zoe/generic/app/1.0.0", nil, bearer(f.read)...)
	var list struct {
		Files []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"files"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &list) != nil || len(list.Files) != 1 || list.Files[0].Size != int64(len(content)) {
		t.Errorf("list = %d %s", resp.StatusCode, body)
	}
	// Size limit.
	saved := pkgLimits.Blob
	pkgLimits.Blob = 10
	t.Cleanup(func() { pkgLimits.Blob = saved })
	if resp, _ := f.call(t, "PUT", "/api/v1/packages/zoe/generic/app/1.0.1/big.bin", strings.NewReader(strings.Repeat("x", 11)), bearer(f.write)...); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized PUT = %d, want 413", resp.StatusCode)
	}
	pkgLimits.Blob = saved
	if resp, _ := f.call(t, "DELETE", p, nil, bearer(f.write)...); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE = %d", resp.StatusCode)
	}
	if resp, _ := f.call(t, "GET", p, nil, bearer(f.read)...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d", resp.StatusCode)
	}
	// Names can't escape.
	if resp, _ := f.call(t, "PUT", "/api/v1/packages/zoe/generic/app/1.0.0/..", strings.NewReader("x"), bearer(f.write)...); resp.StatusCode == http.StatusCreated {
		t.Errorf("'..' file name accepted")
	}
}

// Private packages are invisible to a second person (as a future addon
// would add), in the UI and through every registry.
func TestPrivatePackagesHiddenFromSecondPerson(t *testing.T) {
	f := newPkgFixture(t)
	ctx := context.Background()
	npmPublish(t, f, f.write, "@zoe/secret", "1.0.0", []byte("tarball"), nil)
	if resp, _ := f.call(t, "PUT", "/api/v1/packages/zoe/generic/secret-file/1/a.txt", strings.NewReader("a"), bearer(f.write)...); resp.StatusCode != http.StatusCreated {
		t.Fatal("generic upload failed")
	}

	other, err := f.a.db.CreateOwner(ctx, "visitor", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := f.a.db.CreateSessionFor(ctx, other.ID, time.Hour)
	tok, _ := f.a.db.CreateToken(ctx, "visitor", tokenScopes["packages-write"], time.Hour)
	if err := f.a.db.AssignTokenPerson(ctx, tok, other.ID); err != nil {
		t.Fatal(err)
	}
	ck := []string{"Cookie", sessionCookie + "=" + sess}

	if resp, body := f.call(t, "GET", "/packages", nil, ck...); resp.StatusCode != http.StatusOK || strings.Contains(string(body), "secret") {
		t.Errorf("visitor's /packages = %d, shows secret: %v", resp.StatusCode, strings.Contains(string(body), "secret"))
	}
	if resp, _ := f.call(t, "GET", "/packages/zoe/npm/secret", nil, ck...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("visitor's package page = %d, want 404", resp.StatusCode)
	}
	if resp, _ := f.call(t, "GET", "/npm/@zoe%2fsecret", nil, bearer(tok)...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("visitor's npm GET = %d, want 404", resp.StatusCode)
	}
	if resp, _ := f.call(t, "GET", "/api/v1/packages/zoe/generic/secret-file/1/a.txt", nil, bearer(tok)...); resp.StatusCode != http.StatusNotFound {
		t.Errorf("visitor's generic GET = %d, want 404", resp.StatusCode)
	}
	// Nor can they publish into zoe's scope.
	body := npmBody("@zoe/secret", "2.0.0", []byte("x"), nil)
	if resp, _ := f.call(t, "PUT", "/npm/@zoe%2fsecret", bytes.NewReader(body), append(bearer(tok), "Content-Type", "application/json")...); resp.StatusCode == http.StatusCreated {
		t.Errorf("visitor published into zoe's package")
	}
	// The owner still sees everything.
	if resp, body := f.call(t, "GET", "/packages", nil, f.cookie()...); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "@zoe/secret") {
		t.Errorf("owner's /packages = %d, missing secret", resp.StatusCode)
	}
}

func TestPackagesGCSafety(t *testing.T) {
	f := newPkgFixture(t)
	ctx := context.Background()
	b := f.a.pkgBlobs()
	npmPublish(t, f, f.write, "@zoe/keep", "1.0.0", []byte("kept tarball"), nil)
	if resp, _ := f.call(t, "PUT", "/api/v1/packages/zoe/generic/g/1/f.bin", strings.NewReader("generic kept"), bearer(f.write)...); resp.StatusCode != http.StatusCreated {
		t.Fatal("generic upload failed")
	}
	orphan, err := b.Put(strings.NewReader("nobody uses me"), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := b.Put(strings.NewReader("written just now"), 0, "")

	// Age every blob so only references protect them.
	old := time.Now().Add(-48 * time.Hour)
	ageAll := func() {
		filepath.WalkDir(filepath.Join(b.Root, "blobs"), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				os.Chtimes(p, old, old)
			}
			return nil
		})
	}
	ageAll()
	now := time.Now()
	os.Chtimes(b.Path(fresh.Digest), now, now)

	res, err := f.a.PackagesGC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Errorf("gc removed %d blobs, want 1 (%s)", res.Removed, res)
	}
	if _, ok := b.Stat(orphan.Digest); ok {
		t.Error("orphan survived gc")
	}
	if _, ok := b.Stat(fresh.Digest); !ok {
		t.Error("gc deleted a blob inside the grace period")
	}
	refs, _ := f.a.db.ReferencedBlobs(ctx, 0)
	if len(refs) != 2 {
		t.Fatalf("want 2 referenced blobs, got %d", len(refs))
	}
	for d := range refs {
		if _, ok := b.Stat(d); !ok {
			t.Errorf("gc deleted referenced blob %s", d)
		}
	}
	// Still downloadable.
	if resp, body := f.call(t, "GET", "/api/v1/packages/zoe/generic/g/1/f.bin", nil, bearer(f.read)...); resp.StatusCode != 200 || string(body) != "generic kept" {
		t.Errorf("generic file after gc = %d %q", resp.StatusCode, body)
	}
	// Once the version is gone, its blob goes on the next run.
	p, _ := f.a.db.Package(ctx, f.zoe.ID, store.PkgGeneric, "g")
	if err := f.a.db.DeleteVersion(ctx, p.ID, "1"); err != nil {
		t.Fatal(err)
	}
	ageAll()
	if res, _ := f.a.PackagesGC(ctx); res.Removed != 2 { // the generic file and the "fresh" blob
		t.Errorf("second gc removed %d, want 2", res.Removed)
	}
	refs, _ = f.a.db.ReferencedBlobs(ctx, 0)
	for d := range refs {
		if _, ok := b.Stat(d); !ok {
			t.Errorf("gc deleted referenced blob %s", d)
		}
	}
}

func TestBlobStoreLimitsAndUploads(t *testing.T) {
	b, err := packages.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(strings.NewReader("12345"), 4, ""); err != packages.ErrTooLarge {
		t.Errorf("Put over limit = %v", err)
	}
	if _, err := b.Put(strings.NewReader("1234"), 4, ""); err != nil {
		t.Errorf("Put at limit = %v", err)
	}
	if err := b.StartUpload("u1"); err != nil {
		t.Fatal(err)
	}
	if n, err := b.AppendUpload("u1", strings.NewReader("abc"), 5); err != nil || n != 3 {
		t.Fatalf("append = %d %v", n, err)
	}
	if _, err := b.AppendUpload("u1", strings.NewReader("def"), 5); err != packages.ErrTooLarge {
		t.Errorf("append over limit = %v", err)
	}
	if n, _ := b.UploadSize("u1"); n != 3 {
		t.Errorf("failed append left %d bytes, want 3", n)
	}
	if _, err := b.FinishUpload("u1", "sha256:"+strings.Repeat("a", 64)); err != packages.ErrDigest {
		t.Errorf("finish with wrong digest = %v", err)
	}
	if err := b.StartUpload("../x"); err == nil {
		t.Error("upload id with a path was accepted")
	}
}

func TestPackagesUI(t *testing.T) {
	f := newPkgFixture(t)
	npmPublish(t, f, f.write, "@zoe/ui-kit", "1.0.0", []byte("v1"), nil)
	npmPublish(t, f, f.write, "@zoe/ui-kit", "1.1.0", []byte("v2"), nil)
	f.call(t, "PUT", "/api/v1/packages/elyndra/generic/tool/2.0/tool.zip", strings.NewReader("zip"), bearer(f.write)...)

	resp, body := f.call(t, "GET", "/packages", nil, f.cookie()...)
	if resp.StatusCode != 200 {
		t.Fatalf("/packages = %d", resp.StatusCode)
	}
	for _, want := range []string{"@zoe/ui-kit", "elyndra/tool", "pk-chip-npm", "@elyndra:registry=", "Your npm registry", "docker login"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/packages lacks %q", want)
		}
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("/packages has no CSP")
	}
	if _, body := f.call(t, "GET", "/packages?type=generic", nil, f.cookie()...); strings.Contains(string(body), "@zoe/ui-kit") {
		t.Error("type filter didn't filter")
	}
	if _, body := f.call(t, "GET", "/packages/elyndra", nil, f.cookie()...); strings.Contains(string(body), "@zoe/ui-kit") || !strings.Contains(string(body), "elyndra/tool") {
		t.Error("owner page shows the wrong packages")
	}
	resp, body = f.call(t, "GET", "/packages/zoe/npm/ui-kit", nil, f.cookie()...)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "npm install @zoe/ui-kit@1.1.0") || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("package page = %d\n%s", resp.StatusCode, body)
	}
	if resp, _ := f.call(t, "GET", "/packages/zoe/npm/ui-kit", nil); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("signed-out package page = %d, want redirect to sign-in", resp.StatusCode)
	}
	// Cross-site posts are refused; same-origin delete works.
	form := url.Values{"action": {"delete-version"}, "version": {"1.0.0"}}.Encode()
	if resp, _ := f.call(t, "POST", "/packages/zoe/npm/ui-kit", strings.NewReader(form), append(f.cookie(),
		"Content-Type", "application/x-www-form-urlencoded", "Sec-Fetch-Site", "cross-site")...); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site delete = %d, want 403", resp.StatusCode)
	}
	resp, _ = f.call(t, "POST", "/packages/zoe/npm/ui-kit", strings.NewReader(form), append(f.cookie(),
		"Content-Type", "application/x-www-form-urlencoded", "Sec-Fetch-Site", "same-origin")...)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete version = %d", resp.StatusCode)
	}
	p, _ := f.a.db.Package(context.Background(), f.zoe.ID, store.PkgNpm, "ui-kit")
	if p.Versions != 1 {
		t.Errorf("versions after delete = %d", p.Versions)
	}
	// Settings: make it public, then anonymous npm reads work.
	form = url.Values{"action": {"settings"}, "visibility": {"public"}, "description": {"Kit"}}.Encode()
	f.call(t, "POST", "/packages/zoe/npm/ui-kit", strings.NewReader(form), append(f.cookie(),
		"Content-Type", "application/x-www-form-urlencoded", "Sec-Fetch-Site", "same-origin")...)
	if resp, _ := f.call(t, "GET", "/npm/@zoe%2fui-kit", nil); resp.StatusCode != 200 {
		t.Errorf("anonymous GET of public package = %d", resp.StatusCode)
	}
	if resp, _ := f.call(t, "GET", "/settings/packages", nil, f.cookie()...); resp.StatusCode != 200 {
		t.Errorf("/settings/packages = %d", resp.StatusCode)
	}
	if resp, body := f.call(t, "GET", "/settings/tokens", nil, f.cookie()...); resp.StatusCode != 200 || !strings.Contains(string(body), "packages-write") {
		t.Errorf("tokens page lacks the packages access option")
	}
}

func TestBackupIncludesPackages(t *testing.T) {
	found := false
	for _, d := range backup.DataDirs() {
		if d == "packages" {
			found = true
		}
	}
	if !found {
		t.Fatal("snapshots don't include <data>/packages")
	}
}
