package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
)

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

func basicAuth(user, pass string) []string {
	return []string{"Authorization", "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))}
}

// ociClient is a minimal registry client, enough to push and pull images.
type ociClient struct {
	t    *testing.T
	f    *pkgFixture
	auth []string
}

func (c ociClient) do(method, path string, body []byte, hdr ...string) (*http.Response, []byte) {
	c.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	return c.f.call(c.t, method, path, rd, append(append([]string{}, c.auth...), hdr...)...)
}

// pushMonolithic uploads a blob in one POST.
func (c ociClient) pushMonolithic(repo string, blob []byte) string {
	c.t.Helper()
	d := sha(blob)
	resp, body := c.do("POST", "/v2/"+repo+"/blobs/uploads/?digest="+d, blob, "Content-Type", "application/octet-stream")
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Docker-Content-Digest") != d {
		c.t.Fatalf("monolithic push = %d %s", resp.StatusCode, body)
	}
	return d
}

// pushChunked uploads a blob in chunks: POST, PATCH…, PUT.
func (c ociClient) pushChunked(repo string, blob []byte, chunk int) string {
	c.t.Helper()
	resp, body := c.do("POST", "/v2/"+repo+"/blobs/uploads/", nil)
	if resp.StatusCode != http.StatusAccepted {
		c.t.Fatalf("start upload = %d %s", resp.StatusCode, body)
	}
	loc := resp.Header.Get("Location")
	for off := 0; off < len(blob); off += chunk {
		end := min(off+chunk, len(blob))
		resp, body = c.do("PATCH", loc, blob[off:end], "Content-Type", "application/octet-stream",
			"Content-Range", fmt.Sprintf("%d-%d", off, end-1))
		if resp.StatusCode != http.StatusAccepted || resp.Header.Get("Range") != fmt.Sprintf("0-%d", end-1) {
			c.t.Fatalf("PATCH = %d range %q %s", resp.StatusCode, resp.Header.Get("Range"), body)
		}
		loc = resp.Header.Get("Location")
	}
	d := sha(blob)
	resp, body = c.do("PUT", loc+"?digest="+d, nil)
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("finish upload = %d %s", resp.StatusCode, body)
	}
	return d
}

func (c ociClient) putManifest(repo, ref, mediaType string, m []byte) *http.Response {
	c.t.Helper()
	resp, _ := c.do("PUT", "/v2/"+repo+"/manifests/"+ref, m, "Content-Type", mediaType)
	return resp
}

func imageManifest(config []byte, layers ...[]byte) []byte {
	ls := []map[string]any{}
	for _, l := range layers {
		ls = append(ls, map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": sha(l), "size": len(l)})
	}
	b, _ := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": mtOCIManifest,
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": sha(config), "size": len(config)},
		"layers": ls,
	})
	return b
}

func TestOCIRegistry(t *testing.T) {
	f := newPkgFixture(t)
	anon := ociClient{t: t, f: f}
	push := ociClient{t: t, f: f, auth: basicAuth("zoe", f.write)}
	pull := ociClient{t: t, f: f, auth: basicAuth("zoe", f.read)}

	// The /v2/ check drives `docker login`.
	resp, _ := anon.do("GET", "/v2/", nil)
	if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic ") ||
		resp.Header.Get("Docker-Distribution-API-Version") != "registry/2.0" {
		t.Fatalf("anonymous /v2/ = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if resp, _ := push.do("GET", "/v2/", nil); resp.StatusCode != 200 {
		t.Fatalf("authed /v2/ = %d", resp.StatusCode)
	}
	if resp, _ := (ociClient{t: t, f: f, auth: basicAuth("zoe", "atl_wrong")}).do("GET", "/v2/", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong password /v2/ = %d", resp.StatusCode)
	}

	const repo = "zoe/atelier"
	config := []byte(`{"architecture":"arm64","os":"linux"}`)
	layer := bytes.Repeat([]byte("layer-bytes-"), 1000)

	// Scopes: read tokens and repo-only tokens can't push.
	if resp, _ := pull.do("POST", "/v2/"+repo+"/blobs/uploads/", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read token upload = %d, want 403", resp.StatusCode)
	}
	if resp, _ := (ociClient{t: t, f: f, auth: basicAuth("zoe", f.repoOnly)}).do("POST", "/v2/"+repo+"/blobs/uploads/", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("repo-only token upload = %d, want 403", resp.StatusCode)
	}
	if resp, _ := anon.do("POST", "/v2/"+repo+"/blobs/uploads/", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous upload = %d, want 401", resp.StatusCode)
	}

	cd := push.pushMonolithic(repo, config)
	ld := push.pushChunked(repo, layer, 5000)

	// Chunk out of order.
	resp, _ = push.do("POST", "/v2/"+repo+"/blobs/uploads/", nil)
	loc := resp.Header.Get("Location")
	if resp, _ := push.do("PATCH", loc, []byte("abc"), "Content-Range", "10-12"); resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("out-of-order chunk = %d, want 416", resp.StatusCode)
	}
	if resp, _ := push.do("GET", loc, nil); resp.StatusCode != http.StatusNoContent || resp.Header.Get("Range") != "0-0" {
		t.Errorf("upload status = %d %q", resp.StatusCode, resp.Header.Get("Range"))
	}
	if resp, _ := push.do("PUT", loc+"?digest="+sha([]byte("nope")), []byte("other")); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("wrong digest = %d, want 400", resp.StatusCode)
	}
	if resp, _ := push.do("DELETE", loc, nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("cancel upload = %d", resp.StatusCode)
	}

	// Manifest referring to a missing blob is refused.
	if resp := push.putManifest(repo, "broken", mtOCIManifest, imageManifest(config, []byte("missing layer"))); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("manifest with unknown blob = %d, want 400", resp.StatusCode)
	}
	man := imageManifest(config, layer)
	resp = push.putManifest(repo, "v1", mtOCIManifest, man)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Docker-Content-Digest") != sha(man) {
		t.Fatalf("PUT manifest = %d", resp.StatusCode)
	}
	push.putManifest(repo, "latest", mtOCIManifest, man)

	// Pull: private by default.
	if resp, _ := anon.do("GET", "/v2/"+repo+"/manifests/v1", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous pull of private image = %d, want 401", resp.StatusCode)
	}
	if resp, _ := (ociClient{t: t, f: f, auth: basicAuth("zoe", f.repoOnly)}).do("GET", "/v2/"+repo+"/manifests/v1", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("repo-only token pull = %d, want 404", resp.StatusCode)
	}
	resp, body := pull.do("GET", "/v2/"+repo+"/manifests/v1", nil, "Accept", mtOCIManifest)
	if resp.StatusCode != 200 || !bytes.Equal(body, man) || resp.Header.Get("Content-Type") != mtOCIManifest || resp.Header.Get("Docker-Content-Digest") != sha(man) {
		t.Fatalf("GET manifest = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := pull.do("HEAD", "/v2/"+repo+"/manifests/"+sha(man), nil); resp.StatusCode != 200 || resp.Header.Get("Content-Length") != fmt.Sprint(len(man)) {
		t.Errorf("HEAD manifest by digest = %d len %q", resp.StatusCode, resp.Header.Get("Content-Length"))
	}
	resp, body = pull.do("GET", "/v2/"+repo+"/blobs/"+ld, nil)
	if resp.StatusCode != 200 || !bytes.Equal(body, layer) {
		t.Fatalf("GET layer = %d", resp.StatusCode)
	}
	if resp, body := pull.do("GET", "/v2/"+repo+"/blobs/"+ld, nil, "Range", "bytes=0-4"); resp.StatusCode != http.StatusPartialContent || string(body) != "layer" {
		t.Errorf("ranged blob GET = %d %q", resp.StatusCode, body)
	}
	if resp, _ := pull.do("HEAD", "/v2/"+repo+"/blobs/"+cd, nil); resp.StatusCode != 200 {
		t.Errorf("HEAD config = %d", resp.StatusCode)
	}

	// Multi-arch index.
	config2 := []byte(`{"architecture":"amd64","os":"linux"}`)
	push.pushMonolithic(repo, config2)
	man2 := imageManifest(config2, layer)
	if resp := push.putManifest(repo, sha(man2), mtOCIManifest, man2); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT manifest by digest = %d", resp.StatusCode)
	}
	index, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mtOCIIndex, "manifests": []map[string]any{
		{"mediaType": mtOCIManifest, "digest": sha(man), "size": len(man), "platform": map[string]string{"os": "linux", "architecture": "arm64"}},
		{"mediaType": mtOCIManifest, "digest": sha(man2), "size": len(man2), "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
	}})
	if resp := push.putManifest(repo, "multi", mtOCIIndex, index); resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT index = %d", resp.StatusCode)
	}
	if resp, body := pull.do("GET", "/v2/"+repo+"/manifests/multi", nil); resp.StatusCode != 200 || resp.Header.Get("Content-Type") != mtOCIIndex || !bytes.Equal(body, index) {
		t.Errorf("GET index = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	badIndex, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": mtOCIIndex, "manifests": []map[string]any{
		{"mediaType": mtOCIManifest, "digest": sha([]byte("x")), "size": 1}}})
	if resp := push.putManifest(repo, "bad", mtOCIIndex, badIndex); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("index with unknown child = %d, want 400", resp.StatusCode)
	}

	// Tags list with paging.
	resp, body = pull.do("GET", "/v2/"+repo+"/tags/list", nil)
	var tl struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	json.Unmarshal(body, &tl)
	if resp.StatusCode != 200 || tl.Name != repo || strings.Join(tl.Tags, ",") != "latest,multi,v1" {
		t.Errorf("tags = %d %+v", resp.StatusCode, tl)
	}
	resp, body = pull.do("GET", "/v2/"+repo+"/tags/list?n=1&last=latest", nil)
	json.Unmarshal(body, &tl)
	if strings.Join(tl.Tags, ",") != "multi" || !strings.Contains(resp.Header.Get("Link"), "last=multi") {
		t.Errorf("paged tags = %+v link %q", tl.Tags, resp.Header.Get("Link"))
	}

	// Mount into another repository (and into a company account).
	other := "elyndra/base"
	resp, _ = push.do("POST", "/v2/"+other+"/blobs/uploads/?mount="+ld+"&from="+repo, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("mount = %d", resp.StatusCode)
	}
	if resp, _ := push.do("HEAD", "/v2/"+other+"/blobs/"+ld, nil); resp.StatusCode != 200 {
		t.Errorf("mounted blob HEAD = %d", resp.StatusCode)
	}
	// A blob that was never pushed here isn't visible here.
	if resp, _ := push.do("HEAD", "/v2/"+other+"/blobs/"+cd, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unlinked blob HEAD = %d, want 404", resp.StatusCode)
	}

	// Catalog.
	if resp, body := pull.do("GET", "/v2/_catalog", nil); resp.StatusCode != 200 || !strings.Contains(string(body), `"zoe/atelier"`) {
		t.Errorf("catalog = %d %s", resp.StatusCode, body)
	}

	// Size limit.
	saved := pkgLimits.Blob
	pkgLimits.Blob = 100
	if resp, _ := push.do("POST", "/v2/"+repo+"/blobs/uploads/?digest="+sha(layer), layer); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized blob = %d, want 413", resp.StatusCode)
	}
	resp, _ = push.do("POST", "/v2/"+repo+"/blobs/uploads/", nil)
	if r2, _ := push.do("PATCH", resp.Header.Get("Location"), layer); r2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized chunk = %d, want 413", r2.StatusCode)
	}
	pkgLimits.Blob = saved

	// Deletes: a tag, then a manifest.
	if resp, _ := pull.do("DELETE", "/v2/"+repo+"/manifests/latest", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("read token delete = %d, want 403", resp.StatusCode)
	}
	if resp, _ := push.do("DELETE", "/v2/"+repo+"/manifests/latest", nil); resp.StatusCode != http.StatusAccepted {
		t.Errorf("delete tag = %d", resp.StatusCode)
	}
	if resp, _ := pull.do("GET", "/v2/"+repo+"/manifests/latest", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleted tag still resolves: %d", resp.StatusCode)
	}
	if resp, _ := pull.do("GET", "/v2/"+repo+"/manifests/v1", nil); resp.StatusCode != 200 {
		t.Errorf("other tag of the same manifest gone: %d", resp.StatusCode)
	}

	// UI shows the image with its compose line.
	if resp, body := f.call(t, "GET", "/packages/zoe/container/atelier", nil, f.cookie()...); resp.StatusCode != 200 ||
		!strings.Contains(string(body), "image: "+strings.TrimPrefix(f.srv.URL, "http://")+"/zoe/atelier:") || !strings.Contains(string(body), "linux/arm64") {
		t.Errorf("container page = %d\n%s", resp.StatusCode, body)
	}

	if resp, _ := push.do("DELETE", "/v2/"+repo+"/manifests/"+sha(man2), nil); resp.StatusCode != http.StatusAccepted {
		t.Errorf("delete manifest = %d", resp.StatusCode)
	}
	if resp, _ := pull.do("GET", "/v2/"+repo+"/manifests/"+sha(man2), nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleted manifest = %d", resp.StatusCode)
	}

	// Public images can be pulled anonymously.
	p, _ := f.a.db.Package(context.Background(), f.zoe.ID, store.PkgContainer, "atelier")
	f.a.db.UpdatePackage(context.Background(), p.ID, false, "", 0)
	if resp, _ := anon.do("GET", "/v2/"+repo+"/manifests/v1", nil); resp.StatusCode != 200 {
		t.Errorf("anonymous pull of public image = %d", resp.StatusCode)
	}
	if resp, _ := anon.do("PUT", "/v2/"+repo+"/manifests/v2", man); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous push to public image = %d, want 401", resp.StatusCode)
	}
}

// TestOCICrane pushes and pulls with crane, if it's installed.
func TestOCICrane(t *testing.T) {
	crane, err := exec.LookPath("crane")
	if err != nil {
		t.Skip("crane not installed")
	}
	f := newPkgFixture(t)
	host := strings.TrimPrefix(f.srv.URL, "http://")
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "DOCKER_CONFIG="+home)
	run := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, crane, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("crane %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	run("auth", "login", host, "-u", "zoe", "-p", f.write, "--insecure")
	tarball := t.TempDir() + "/empty.tar"
	os.WriteFile(tarball, make([]byte, 1024), 0o644)
	run("append", "--insecure", "-f", tarball, "-t", host+"/zoe/crane-test:v1")
	if out := run("ls", "--insecure", host+"/zoe/crane-test"); !strings.Contains(out, "v1") {
		t.Errorf("crane ls = %q", out)
	}
	run("pull", "--insecure", host+"/zoe/crane-test:v1", t.TempDir()+"/img.tar")
}
