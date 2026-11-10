package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"atelier/internal/store"
)

func basicHdr(user, pass string) map[string]string {
	req, _ := http.NewRequest("GET", "/", nil)
	req.SetBasicAuth(user, pass)
	return map[string]string{"Authorization": req.Header.Get("Authorization")}
}

func lfsDo(t *testing.T, a *App, method, target string, body any, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	h := with(hdr, "Accept", lfsMediaType)
	if _, raw := body.([]byte); !raw && body != nil {
		h["Content-Type"] = lfsMediaType
	}
	rec := doRaw(t, a, method, target, rd, h)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func batchObjects(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	objs, _ := out["objects"].([]any)
	var res []map[string]any
	for _, o := range objs {
		res = append(res, o.(map[string]any))
	}
	return res
}

// hrefPath turns an absolute action href into a request path.
func hrefPath(t *testing.T, obj map[string]any, action string) string {
	t.Helper()
	acts, ok := obj["actions"].(map[string]any)
	if !ok {
		t.Fatalf("object has no actions: %v", obj)
	}
	act, ok := acts[action].(map[string]any)
	if !ok {
		t.Fatalf("object has no %s action: %v", action, obj)
	}
	u, err := url.Parse(act["href"].(string))
	if err != nil {
		t.Fatal(err)
	}
	return u.Path
}

func TestLFSProtocol(t *testing.T) {
	f := newGitFixture(t)
	a, ctx := f.a, context.Background()
	const base = "/zoe/demo.git/info/lfs"
	w, r := basicHdr("zoe", f.write), basicHdr("zoe", f.read)
	content := []byte("big binary design file\n")
	oid := sum256(content)
	ptr := map[string]any{"oid": oid, "size": len(content)}

	// Authentication like git over HTTPS.
	rec, _ := lfsDo(t, a, "POST", base+"/objects/batch", map[string]any{"operation": "download", "objects": []any{ptr}}, nil)
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("LFS-Authenticate") == "" {
		t.Errorf("anonymous private batch = %d, LFS-Authenticate %q", rec.Code, rec.Header().Get("LFS-Authenticate"))
	}
	if rec, _ := lfsDo(t, a, "POST", base+"/objects/batch", map[string]any{"operation": "upload", "objects": []any{ptr}}, r); rec.Code != http.StatusForbidden {
		t.Errorf("read token upload batch = %d", rec.Code)
	}
	if rec, _ := lfsDo(t, a, "POST", "/zoe/nope.git/info/lfs/objects/batch", map[string]any{"operation": "download"}, w); rec.Code != 404 {
		t.Errorf("missing repo = %d", rec.Code)
	}

	// Upload batch → upload → verify.
	rec, out := lfsDo(t, a, "POST", base+"/objects/batch", map[string]any{"operation": "upload", "transfers": []string{"basic"}, "objects": []any{ptr}}, w)
	if rec.Code != 200 || out["transfer"] != "basic" || rec.Header().Get("Content-Type") != lfsMediaType {
		t.Fatalf("upload batch = %d %s", rec.Code, rec.Body)
	}
	obj := batchObjects(t, out)[0]
	up, verify := hrefPath(t, obj, "upload"), hrefPath(t, obj, "verify")
	if up != base+"/objects/"+oid || verify != base+"/objects/verify" {
		t.Errorf("hrefs = %s %s", up, verify)
	}
	hdr := obj["actions"].(map[string]any)["upload"].(map[string]any)["header"].(map[string]any)
	if hdr["Authorization"] != w["Authorization"] {
		t.Errorf("upload action doesn't carry the credential: %v", hdr)
	}

	// Wrong content or size is rejected and nothing is stored.
	if rec, _ := lfsDo(t, a, "PUT", up, []byte("not the right bytes"), w); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("wrong content = %d", rec.Code)
	}
	if rec, _ := lfsDo(t, a, "PUT", base+"/objects/"+sum256([]byte("x")), []byte("xy"), w); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("wrong size = %d", rec.Code)
	}
	if rec, _ := lfsDo(t, a, "POST", verify, ptr, w); rec.Code != 404 {
		t.Errorf("verify before upload = %d", rec.Code)
	}
	if rec, _ := lfsDo(t, a, "PUT", up, content, r); rec.Code != http.StatusForbidden {
		t.Errorf("read token PUT = %d", rec.Code)
	}
	if rec, _ := lfsDo(t, a, "PUT", up, content, w); rec.Code != 200 {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	disk := filepath.Join(a.cfg.DataDir, "lfs", oid[:2], oid[2:4], oid)
	if got, err := os.ReadFile(disk); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("stored object: %v", err)
	}
	if rec, _ := lfsDo(t, a, "POST", verify, ptr, w); rec.Code != 200 {
		t.Errorf("verify = %d %s", rec.Code, rec.Body)
	}
	if rec, _ := lfsDo(t, a, "POST", verify, map[string]any{"oid": oid, "size": 3}, w); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("verify wrong size = %d", rec.Code)
	}

	// Uploading again: no actions, nothing to send.
	_, out = lfsDo(t, a, "POST", base+"/objects/batch", map[string]any{"operation": "upload", "objects": []any{ptr}}, w)
	if o := batchObjects(t, out)[0]; o["actions"] != nil {
		t.Errorf("existing object asked for upload: %v", o)
	}

	// Download batch and download.
	missing := map[string]any{"oid": sum256([]byte("nope")), "size": 4}
	_, out = lfsDo(t, a, "POST", base+"/objects/batch", map[string]any{"operation": "download", "objects": []any{ptr, missing, map[string]any{"oid": "../../etc", "size": 1}}}, r)
	objs := batchObjects(t, out)
	if len(objs) != 3 {
		t.Fatalf("download batch = %v", out)
	}
	dl := hrefPath(t, objs[0], "download")
	if e, _ := objs[1]["error"].(map[string]any); e == nil || e["code"].(float64) != 404 {
		t.Errorf("missing object = %v", objs[1])
	}
	if e, _ := objs[2]["error"].(map[string]any); e == nil || e["code"].(float64) != 422 {
		t.Errorf("bad oid = %v", objs[2])
	}
	if rec := doRaw(t, a, "GET", dl, nil, r); rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Errorf("download = %d", rec.Code)
	}
	if rec := doRaw(t, a, "GET", dl, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous private download = %d", rec.Code)
	}

	// Another repository can't fetch the object by its id, even though the
	// bytes are on disk; once public, this one can be read anonymously.
	other := profRepo(t, a, mustOwner(t, a, "zoe"), "other", false)
	_ = other
	if rec := doRaw(t, a, "GET", "/zoe/other.git/info/lfs/objects/"+oid, nil, r); rec.Code != 404 {
		t.Errorf("cross-repo download = %d", rec.Code)
	}
	_ = a.db.UpdateRepo(ctx, f.mustRepo(t, "demo").ID, "", false)
	if rec := doRaw(t, a, "GET", dl, nil, nil); rec.Code != 200 {
		t.Errorf("anonymous public download = %d", rec.Code)
	}
	if rec, _ := lfsDo(t, a, "POST", base+"/objects/batch", map[string]any{"operation": "upload", "objects": []any{ptr}}, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous public upload batch = %d", rec.Code)
	}

	// Size limit.
	t.Setenv("ATELIER_LFS_OBJECT_MAX", "16")
	_, out = lfsDo(t, a, "POST", base+"/objects/batch", map[string]any{"operation": "upload", "objects": []any{map[string]any{"oid": sum256(bytes.Repeat([]byte("z"), 32)), "size": 32}}}, w)
	if e, _ := batchObjects(t, out)[0]["error"].(map[string]any); e == nil || e["code"].(float64) != 422 {
		t.Errorf("over-limit batch = %v", out)
	}

	// Usage for the settings page.
	v := &repoView{Repo: f.mustRepo(t, "demo")}
	if u := v.LFSUsage(); u.Objects != 1 || u.Bytes != int64(len(content)) {
		t.Errorf("usage = %+v", u)
	}
}

func mustOwner(t *testing.T, a *App, name string) store.Owner {
	t.Helper()
	o, err := a.db.OwnerByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (f gitFixture) mustRepo(t *testing.T, name string) store.Repo {
	t.Helper()
	r, err := f.a.db.RepoByName(context.Background(), "zoe", name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLFSLocks(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	repo := f.mustRepo(t, "demo")
	job := fakeJobTokens(t, repo.ID)
	const base = "/zoe/demo.git/info/lfs/locks"
	w, r, j := basicHdr("zoe", f.write), basicHdr("zoe", f.read), basicHdr("atelier", job)

	if rec, _ := lfsDo(t, a, "POST", base, map[string]any{"path": "art/logo.psd"}, r); rec.Code != http.StatusForbidden {
		t.Errorf("read token lock = %d", rec.Code)
	}
	rec, out := lfsDo(t, a, "POST", base, map[string]any{"path": "art/logo.psd", "ref": map[string]any{"name": "refs/heads/main"}}, w)
	if rec.Code != http.StatusCreated {
		t.Fatalf("lock = %d %s", rec.Code, rec.Body)
	}
	lock := out["lock"].(map[string]any)
	id := lock["id"].(string)
	if lock["path"] != "art/logo.psd" || lock["owner"].(map[string]any)["name"] != "write" {
		t.Errorf("lock = %v", lock)
	}
	rec, out = lfsDo(t, a, "POST", base, map[string]any{"path": "art/logo.psd"}, j)
	if rec.Code != http.StatusConflict || out["lock"].(map[string]any)["id"] != id {
		t.Errorf("second lock = %d %s", rec.Code, rec.Body)
	}
	lfsDo(t, a, "POST", base, map[string]any{"path": "build/app.bin"}, j)

	// List (read access), with path filter.
	rec, out = lfsDo(t, a, "GET", base, nil, r)
	if rec.Code != 200 || len(out["locks"].([]any)) != 2 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	_, out = lfsDo(t, a, "GET", base+"?path=art/logo.psd", nil, r)
	if len(out["locks"].([]any)) != 1 {
		t.Errorf("filtered list = %v", out)
	}
	_, out = lfsDo(t, a, "GET", base+"?limit=1", nil, r)
	if len(out["locks"].([]any)) != 1 || out["next_cursor"] == nil {
		t.Errorf("paged list = %v", out)
	}

	// Verify: ours and theirs.
	rec, out = lfsDo(t, a, "POST", base+"/verify", map[string]any{}, w)
	if rec.Code != 200 || len(out["ours"].([]any)) != 1 || len(out["theirs"].([]any)) != 1 {
		t.Errorf("verify = %d %s", rec.Code, rec.Body)
	}

	// Unlocking someone else's lock needs force.
	_, out = lfsDo(t, a, "GET", base+"?path=build/app.bin", nil, r)
	jobLock := out["locks"].([]any)[0].(map[string]any)["id"].(string)
	if rec, _ := lfsDo(t, a, "POST", base+"/"+jobLock+"/unlock", map[string]any{}, w); rec.Code != http.StatusForbidden {
		t.Errorf("unlock theirs = %d", rec.Code)
	}
	if rec, _ := lfsDo(t, a, "POST", base+"/"+jobLock+"/unlock", map[string]any{"force": true}, w); rec.Code != 200 {
		t.Errorf("force unlock = %d", rec.Code)
	}
	if rec, _ := lfsDo(t, a, "POST", base+"/"+id+"/unlock", map[string]any{}, w); rec.Code != 200 {
		t.Errorf("unlock ours = %d", rec.Code)
	}
	if rec, _ := lfsDo(t, a, "POST", base+"/"+id+"/unlock", map[string]any{}, w); rec.Code != 404 {
		t.Errorf("unlock again = %d", rec.Code)
	}
	if _, out := lfsDo(t, a, "GET", base, nil, r); len(out["locks"].([]any)) != 0 {
		t.Errorf("locks left: %v", out)
	}
}

// TestLFSWithGitClient runs the real git-lfs client against the server.
func TestLFSWithGitClient(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs is not installed")
	}
	f := newGitFixture(t)
	srv := httptest.NewServer(f.a)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	remote := "http://zoe:" + f.write + "@" + u.Host + "/zoe/demo.git"

	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", ".")
	runGit(t, work, "lfs", "install", "--local")
	runGit(t, work, "lfs", "track", "*.bin")
	big := make([]byte, 256<<10)
	rand.Read(big)
	if err := os.WriteFile(filepath.Join(work, "asset.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-q", "-m", "LFS asset")
	runGit(t, work, "config", "lfs.locksverify", "true")
	runGit(t, work, "push", "-q", remote, "main")

	oid := sum256(big)
	if _, err := os.Stat(filepath.Join(f.a.cfg.DataDir, "lfs", oid[:2], oid[2:4], oid)); err != nil {
		t.Fatalf("object not stored on the server: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", "-q", remote, dst)
	got, err := os.ReadFile(filepath.Join(dst, "asset.bin"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("clone did not get the LFS content (err %v, %d bytes)", err, len(got))
	}
	runGit(t, dst, "lfs", "lock", "asset.bin")
	if out := runGit(t, dst, "lfs", "locks"); !strings.Contains(out, "asset.bin") {
		t.Errorf("git lfs locks = %q", out)
	}
	runGit(t, dst, "lfs", "unlock", "asset.bin")
}
