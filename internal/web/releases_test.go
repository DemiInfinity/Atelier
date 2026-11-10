package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// relFixture: owner zoe with private repo zoe/demo holding two commits on
// main, a second person sam, read/write tokens, and an event recorder.
type relFixture struct {
	gitFixture
	zoe, sam   store.Owner
	zc, sc     map[string]string
	demo       store.Repo
	head, prev string
	events     *eventLog
}

type eventLog struct {
	mu  sync.Mutex
	evs []Event
}

func (l *eventLog) kinds(kind string) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func recordEvents(t *testing.T) *eventLog {
	t.Helper()
	l := &eventLog{}
	saved := eventHandlers
	t.Cleanup(func() { eventHandlers = saved })
	registerEventHandler(func(_ context.Context, _ *App, ev Event) {
		l.mu.Lock()
		l.evs = append(l.evs, ev)
		l.mu.Unlock()
	})
	return l
}

// commitTo commits files (name, content pairs; one commit each) on main of
// the bare repo and returns the new head.
func commitTo(t *testing.T, a *App, owner, name string, files ...string) string {
	t.Helper()
	work := t.TempDir()
	bare := a.git.Path(owner, name)
	runGit(t, work, "init", "-q", "--initial-branch=main", ".")
	if _, err := runGitErr(work, "fetch", "-q", bare, "main"); err == nil {
		runGit(t, work, "reset", "-q", "--hard", "FETCH_HEAD")
	}
	for i := 0; i+1 < len(files); i += 2 {
		if err := os.WriteFile(filepath.Join(work, files[i]), []byte(files[i+1]), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, work, "add", ".")
		runGit(t, work, "commit", "-q", "-m", "Add "+files[i])
	}
	runGit(t, work, "push", "-q", bare, "main")
	return runGit(t, work, "rev-parse", "HEAD")
}

func runGitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func newRelFixture(t *testing.T) relFixture {
	t.Helper()
	f := relFixture{gitFixture: newGitFixture(t)}
	ctx := context.Background()
	a := f.a
	f.zoe, _ = a.db.OwnerByName(ctx, "zoe")
	var err error
	f.sam, err = a.db.CreateOwner(ctx, "sam", "Sam", "user")
	if err != nil {
		t.Fatal(err)
	}
	f.zc, f.sc = cookieFor(t, a, f.zoe.ID), cookieFor(t, a, f.sam.ID)
	f.prev = commitTo(t, a, "zoe", "demo", "README.md", "# Demo\n")
	f.head = commitTo(t, a, "zoe", "demo", "main.go", "package main\n")
	f.demo, _ = a.db.RepoByName(ctx, "zoe", "demo")
	f.events = recordEvents(t)
	return f
}

func form(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Add(kv[i], kv[i+1])
	}
	return v
}

func (f relFixture) tagCommit(t *testing.T, tag string) (string, bool) {
	t.Helper()
	g, err := f.a.git.Open("zoe", "demo")
	if err != nil {
		t.Fatal(err)
	}
	return g.TagCommit(context.Background(), tag)
}

func TestReleaseCRUDAndTags(t *testing.T) {
	f := newRelFixture(t)
	a, ctx := f.a, context.Background()

	// Empty list.
	rec := do(t, a, "GET", "/zoe/demo/releases", nil, f.zc)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "No releases yet") {
		t.Fatalf("empty list = %d\n%.400s", rec.Code, rec.Body)
	}

	// Publish v1.0.0: the tag is created on main.
	rec = do(t, a, "POST", "/zoe/demo/releases/new", form("tag", "v1.0.0", "target", "main", "title", "First light",
		"body", "## Hello\n\n- one", "action", "publish"), f.zc)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/zoe/demo/releases/tag/v1.0.0") {
		t.Fatalf("create = %d %q\n%.600s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if sha, ok := f.tagCommit(t, "v1.0.0"); !ok || sha != f.head {
		t.Fatalf("tag v1.0.0 = %q %v, want %s", sha, ok, f.head)
	}
	rel, err := a.db.ReleaseByTag(ctx, f.demo.ID, "v1.0.0")
	if err != nil || rel.Draft || rel.PublishedAt == nil || rel.CommitSHA != f.head || rel.AuthorName != "zoe" || rel.AuthorKind != ActorOwner {
		t.Fatalf("stored release = %+v, %v", rel, err)
	}
	if evs := f.events.kinds(EventReleasePublished); len(evs) != 1 || evs[0].Detail != "v1.0.0" || evs[0].RepoID != f.demo.ID ||
		evs[0].URL != "/zoe/demo/releases/tag/v1.0.0" || evs[0].Actor.Name != "zoe" {
		t.Fatalf("events = %+v", evs)
	}

	// Pages render; the header tab shows the count; latest redirects.
	rec = do(t, a, "GET", "/zoe/demo/releases", nil, f.zc)
	body := rec.Body.String()
	for _, want := range []string{"First light", "Latest", "<h2>Hello</h2>", `Releases <span class="pill">1</span>`, "Draft a new release", "/api/v1/repos/zoe/demo/releases"} {
		if !strings.Contains(body, want) {
			t.Errorf("list page lacks %q", want)
		}
	}
	if rec := do(t, a, "GET", "/zoe/demo/releases/tag/v1.0.0", nil, f.zc); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Source code (tar.gz)") {
		t.Errorf("view = %d", rec.Code)
	}
	if rec := do(t, a, "GET", "/zoe/demo/releases/latest", nil, f.zc); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/zoe/demo/releases/tag/v1.0.0" {
		t.Errorf("latest = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(t, a, "GET", "/zoe/demo/releases/new", nil, f.zc); rec.Code != 200 {
		t.Errorf("new form = %d", rec.Code)
	}

	// Existing tags are reused, not moved.
	commitTo(t, a, "zoe", "demo", "later.txt", "x")
	g, _ := a.git.Open("zoe", "demo")
	if err := g.CreateTag(ctx, "v0.9", f.prev); err != nil {
		t.Fatal(err)
	}
	do(t, a, "POST", "/zoe/demo/releases/new", form("tag", "v0.9", "target", "main", "action", "publish"), f.zc)
	if old, err := a.db.ReleaseByTag(ctx, f.demo.ID, "v0.9"); err != nil || old.CommitSHA != f.prev {
		t.Errorf("release on existing tag = %+v, %v", old, err)
	}
	if err := g.CreateTag(ctx, "v0.9", f.head); err == nil {
		t.Error("CreateTag moved an existing tag")
	}

	// Drafts don't create tags or announce anything until published.
	rec = do(t, a, "POST", "/zoe/demo/releases/new", form("tag", "v2.0.0", "target", "main", "title", "Two", "action", "draft"), f.zc)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("draft = %d\n%.500s", rec.Code, rec.Body)
	}
	if _, ok := f.tagCommit(t, "v2.0.0"); ok {
		t.Error("draft created its tag")
	}
	if n := len(f.events.kinds(EventReleasePublished)); n != 2 {
		t.Errorf("published events after draft = %d, want 2", n)
	}
	draft, _ := a.db.ReleaseByTag(ctx, f.demo.ID, "v2.0.0")
	if !draft.Draft || draft.PublishedAt != nil {
		t.Fatalf("draft = %+v", draft)
	}
	if !strings.Contains(do(t, a, "GET", "/zoe/demo/releases", nil, f.zc).Body.String(), "Continue editing") {
		t.Error("list doesn't offer to continue the draft")
	}
	editURL := fmt.Sprintf("/zoe/demo/releases/edit/%d", draft.ID)
	if rec := do(t, a, "GET", editURL, nil, f.zc); rec.Code != 200 {
		t.Fatalf("edit form = %d", rec.Code)
	}
	rec = do(t, a, "POST", editURL, form("tag", "v2.0.0", "target", "main", "title", "Two, final", "body", "done", "action", "publish"), f.zc)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("publish draft = %d\n%.500s", rec.Code, rec.Body)
	}
	if _, ok := f.tagCommit(t, "v2.0.0"); !ok {
		t.Error("publishing didn't create the tag")
	}
	if evs := f.events.kinds(EventReleasePublished); len(evs) != 3 || evs[2].Title != "Two, final" {
		t.Errorf("events = %+v", evs)
	}
	latest, _ := a.db.LatestRelease(ctx, f.demo.ID)
	if latest.Tag != "v2.0.0" {
		t.Errorf("latest = %s", latest.Tag)
	}

	// Editing a published release doesn't announce it again.
	rec = do(t, a, "POST", editURL, form("tag", "v2.0.0", "target", "main", "title", "Two!", "action", "update"), f.zc)
	if rec.Code != http.StatusSeeOther || len(f.events.kinds(EventReleasePublished)) != 3 {
		t.Errorf("update = %d, events %d", rec.Code, len(f.events.kinds(EventReleasePublished)))
	}

	// Mistakes come back as form errors.
	for _, c := range []struct{ tag, want string }{{"bad tag", "valid tag name"}, {"v1.0.0", "already a release"}, {"", "Choose a tag"}} {
		rec := do(t, a, "POST", "/zoe/demo/releases/new", form("tag", c.tag, "action", "publish"), f.zc)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("tag %q = %d, want 422 with %q", c.tag, rec.Code, c.want)
		}
	}

	// Delete: the release goes, the tag stays.
	rec = do(t, a, "POST", fmt.Sprintf("/zoe/demo/releases/delete/%d", rel.ID), nil, f.zc)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d", rec.Code)
	}
	if _, err := a.db.ReleaseByTag(ctx, f.demo.ID, "v1.0.0"); err != store.ErrNotFound {
		t.Errorf("release still there: %v", err)
	}
	if _, ok := f.tagCommit(t, "v1.0.0"); !ok {
		t.Error("deleting the release removed the tag")
	}
	// The tag page now offers to create a release.
	if rec := do(t, a, "GET", "/zoe/demo/releases/tag/v1.0.0", nil, f.zc); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Create a release from this tag") {
		t.Errorf("tag-only page = %d", rec.Code)
	}
	if rec := do(t, a, "GET", "/zoe/demo/releases/tag/nope", nil, f.zc); rec.Code != 404 {
		t.Errorf("unknown tag = %d", rec.Code)
	}
}

// multipartRelease builds a release form with files (name, content pairs).
func multipartRelease(t *testing.T, fields url.Values, files ...string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, vs := range fields {
		if k == "action" {
			continue
		}
		for _, v := range vs {
			mw.WriteField(k, v)
		}
	}
	for i := 0; i+1 < len(files); i += 2 {
		w, _ := mw.CreateFormFile("assets", files[i])
		w.Write([]byte(files[i+1]))
	}
	// Like a browser: the submit button comes last, after the files.
	if v := fields.Get("action"); v != "" {
		mw.WriteField("action", v)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func doRaw(t *testing.T, a *App, method, target string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}

func with(h map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func sum256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestReleaseAssetsUploadAndDownload(t *testing.T) {
	f := newRelFixture(t)
	a, ctx := f.a, context.Background()
	bin := []byte("#!/bin/sh\necho runner\n")
	body, ct := multipartRelease(t, form("tag", "v1", "target", "main", "title", "One", "action", "publish"),
		"atelier-runner-linux-amd64", string(bin), "checksums.txt", "abc  runner\n")
	rec := doRaw(t, a, "POST", "/zoe/demo/releases/new", body, with(f.zc, "Content-Type", ct))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("multipart create = %d\n%.600s", rec.Code, rec.Body)
	}
	rel, err := a.db.ReleaseByTag(ctx, f.demo.ID, "v1")
	if err != nil || rel.Draft {
		t.Fatalf("release = %+v, %v (should be published after the files arrived)", rel, err)
	}
	if n := len(f.events.kinds(EventReleasePublished)); n != 1 {
		t.Errorf("published events = %d", n)
	}
	assets, _ := a.db.ReleaseAssets(ctx, rel.ID)
	if len(assets) != 2 {
		t.Fatalf("assets = %+v", assets)
	}
	as, _ := a.db.ReleaseAssetByName(ctx, rel.ID, "atelier-runner-linux-amd64")
	if as.Size != int64(len(bin)) || as.SHA256 != sum256(bin) || as.UploaderName != "zoe" {
		t.Errorf("asset = %+v", as)
	}
	onDisk := filepath.Join(a.cfg.DataDir, "releases", as.SHA256[:2], as.SHA256[2:4], as.SHA256)
	if got, err := os.ReadFile(onDisk); err != nil || !bytes.Equal(got, bin) {
		t.Fatalf("content-addressed file %s: %v", onDisk, err)
	}

	// Download counts, with headers that force a download.
	dl := "/zoe/demo/releases/download/v1/atelier-runner-linux-amd64"
	for i := 0; i < 2; i++ {
		rec := do(t, a, "GET", dl, nil, f.zc)
		if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), bin) {
			t.Fatalf("download = %d", rec.Code)
		}
		if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "atelier-runner-linux-amd64") {
			t.Errorf("Content-Disposition = %q", cd)
		}
	}
	// A resumed download (later range) and HEAD don't count again.
	do(t, a, "GET", dl, nil, with(f.zc, "Range", "bytes=5-"))
	do(t, a, "HEAD", dl, nil, f.zc)
	as, _ = a.db.ReleaseAssetByID(ctx, rel.ID, as.ID)
	if as.Downloads != 2 {
		t.Errorf("downloads = %d, want 2", as.Downloads)
	}
	// Tokens work for downloads too; anonymous can't see a private repo.
	if rec := do(t, a, "GET", dl, nil, map[string]string{"Authorization": "Bearer " + f.read}); rec.Code != 200 {
		t.Errorf("token download = %d", rec.Code)
	}
	if rec := do(t, a, "GET", dl, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous private download = %d", rec.Code)
	}
	if rec := do(t, a, "GET", "/zoe/demo/releases/download/v1/nope", nil, f.zc); rec.Code != 404 {
		t.Errorf("missing asset = %d", rec.Code)
	}
	// Source archives.
	rec = do(t, a, "GET", "/zoe/demo/archive/v1.tar.gz", nil, f.zc)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/gzip" || rec.Body.Len() < 50 {
		t.Errorf("tar.gz = %d %q %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	if rec := do(t, a, "GET", "/zoe/demo/archive/v1.zip", nil, f.zc); rec.Code != 200 || !bytes.HasPrefix(rec.Body.Bytes(), []byte("PK")) {
		t.Errorf("zip = %d", rec.Code)
	}

	// The same name twice is refused; deleting an asset removes its file
	// unless another asset shares the content.
	body, ct = multipartRelease(t, form("tag", "v1", "target", "main", "title", "One", "action", "update"),
		"checksums.txt", "again", "copy-of-runner", string(bin))
	rec = doRaw(t, a, "POST", fmt.Sprintf("/zoe/demo/releases/edit/%d", rel.ID), body, with(f.zc, "Content-Type", ct))
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "error=") {
		t.Errorf("duplicate name = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if as2, err := a.db.ReleaseAssetByName(ctx, rel.ID, "copy-of-runner"); err != nil || as2.SHA256 != as.SHA256 {
		t.Fatalf("copy = %+v %v", as2, err)
	}
	if rec := do(t, a, "POST", fmt.Sprintf("/zoe/demo/releases/assets/%d/delete", as.ID), nil, f.zc); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete asset = %d", rec.Code)
	}
	if _, err := os.Stat(onDisk); err != nil {
		t.Error("file removed while another asset still uses it")
	}
	copyAs, _ := a.db.ReleaseAssetByName(ctx, rel.ID, "copy-of-runner")
	do(t, a, "POST", fmt.Sprintf("/zoe/demo/releases/assets/%d/delete", copyAs.ID), nil, f.zc)
	if _, err := os.Stat(onDisk); !os.IsNotExist(err) {
		t.Errorf("unused file kept: %v", err)
	}
}

func TestReleaseAssetSizeLimitAndStreaming(t *testing.T) {
	f := newRelFixture(t)
	a, ctx := f.a, context.Background()
	rel, err := a.createRelease(ctx, f.demo, mustOpen(t, a), Actor{Kind: ActorOwner, Name: "zoe", PersonID: f.zoe.ID}, releaseInput{Tag: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	auth := map[string]string{"Authorization": "Bearer " + f.write}
	up := fmt.Sprintf("/api/v1/repos/zoe/demo/releases/%d/assets?name=", rel.ID)

	t.Setenv("ATELIER_RELEASE_ASSET_MAX", "1K")
	big := bytes.Repeat([]byte("x"), 2048)
	// Declared too large: refused before reading.
	rec := doRaw(t, a, "POST", up+"big.bin", bytes.NewReader(big), auth)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over limit = %d %s", rec.Code, rec.Body)
	}
	// Unknown length (chunked): cut off while streaming.
	req := httptest.NewRequest("POST", up+"big.bin", io.MultiReader(bytes.NewReader(big)))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+f.write)
	rr := httptest.NewRecorder()
	a.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("streamed over limit = %d %s", rr.Code, rr.Body)
	}
	if n := countFiles(t, filepath.Join(a.cfg.DataDir, "releases")); n != 0 {
		t.Errorf("%d files left behind by refused uploads", n)
	}
	if as, _ := a.db.ReleaseAssets(ctx, rel.ID); len(as) != 0 {
		t.Errorf("assets recorded: %+v", as)
	}
	exact := bytes.Repeat([]byte("y"), 1024)
	if rec := doRaw(t, a, "POST", up+"ok.bin", bytes.NewReader(exact), auth); rec.Code != http.StatusCreated {
		t.Errorf("at the limit = %d %s", rec.Code, rec.Body)
	}

	// A large upload through a real server streams to disk instead of being
	// held in memory.
	t.Setenv("ATELIER_RELEASE_ASSET_MAX", "")
	srv := httptest.NewServer(a)
	defer srv.Close()
	const size = 48 << 20
	h := sha256.New()
	src := io.TeeReader(io.LimitReader(rand.Reader, size), h)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	req, _ = http.NewRequest("PUT", fmt.Sprintf("%s/api/v1/repos/zoe/demo/releases/%d/assets/large.bin", srv.URL, rel.ID), src)
	req.Header.Set("Authorization", "Bearer "+f.write)
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got apiAsset
	json.NewDecoder(res.Body).Decode(&got)
	res.Body.Close()
	runtime.ReadMemStats(&after)
	if res.StatusCode != http.StatusCreated || got.Size != size || got.Digest != "sha256:"+hex.EncodeToString(h.Sum(nil)) {
		t.Fatalf("large upload = %d %+v", res.StatusCode, got)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > size/2 {
		t.Errorf("uploading %d MB allocated %d MB: not streaming", size>>20, grew>>20)
	}
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

func mustOpen(t *testing.T, a *App) *gitrepo.Repo {
	t.Helper()
	g, err := a.git.Open("zoe", "demo")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func apiDo(t *testing.T, a *App, method, target, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	hdr := map[string]string{"Content-Type": "application/json"}
	if token != "" {
		hdr["Authorization"] = "Bearer " + token
	}
	rec := doRaw(t, a, method, target, rd, hdr)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestReleasesAPI(t *testing.T) {
	f := newRelFixture(t)
	a, ctx := f.a, context.Background()
	const api = "/api/v1/repos/zoe/demo/releases"

	// Authentication.
	if rec, _ := apiDo(t, a, "GET", api, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous private list = %d", rec.Code)
	}
	if rec, _ := apiDo(t, a, "GET", api, f.junk, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("junk token = %d", rec.Code)
	}
	if rec, _ := apiDo(t, a, "POST", api, f.read, map[string]any{"tag_name": "v1"}); rec.Code != http.StatusForbidden {
		t.Errorf("read token create = %d", rec.Code)
	}
	if rec, _ := apiDo(t, a, "GET", "/api/v1/repos/zoe/nope/releases", f.write, nil); rec.Code != 404 {
		t.Errorf("missing repo = %d", rec.Code)
	}
	// Session cookies are not API credentials.
	if rec := do(t, a, "GET", api, nil, f.zc); rec.Code != http.StatusUnauthorized {
		t.Errorf("cookie on API = %d", rec.Code)
	}

	// Create a draft, upload with POST and PUT, publish.
	rec, out := apiDo(t, a, "POST", api, f.write, map[string]any{"tag_name": "v1.2.0", "target_commitish": "main", "name": "Twelve", "body": "Notes", "draft": true})
	if rec.Code != http.StatusCreated || out["draft"] != true || out["tag_name"] != "v1.2.0" {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	id := int64(out["id"].(float64))
	if !strings.HasSuffix(out["upload_url"].(string), fmt.Sprintf("/releases/%d/assets{?name,label}", id)) {
		t.Errorf("upload_url = %v", out["upload_url"])
	}
	payload := []byte("binary build artefact")
	rec = doRaw(t, a, "POST", fmt.Sprintf("%s/%d/assets?name=app-linux-amd64", api, id), bytes.NewReader(payload),
		map[string]string{"Authorization": "Bearer " + f.write, "Content-Type": "application/octet-stream"})
	var as apiAsset
	json.Unmarshal(rec.Body.Bytes(), &as)
	if rec.Code != http.StatusCreated || as.Size != int64(len(payload)) || as.Digest != "sha256:"+sum256(payload) || as.Uploader.Login != "write" {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	if !strings.HasSuffix(as.BrowserDownloadURL, "/zoe/demo/releases/download/v1.2.0/app-linux-amd64") {
		t.Errorf("download url = %s", as.BrowserDownloadURL)
	}
	rec = doRaw(t, a, "PUT", fmt.Sprintf("%s/%d/assets/app.sha256", api, id), strings.NewReader("abc"), map[string]string{"Authorization": "Bearer " + f.write})
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT upload = %d %s", rec.Code, rec.Body)
	}
	rec = doRaw(t, a, "POST", fmt.Sprintf("%s/%d/assets?name=app-linux-amd64", api, id), strings.NewReader("again"), map[string]string{"Authorization": "Bearer " + f.write})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("duplicate asset = %d", rec.Code)
	}
	if rec := doRaw(t, a, "POST", fmt.Sprintf("%s/%d/assets?name=../evil", api, id), strings.NewReader("x"), map[string]string{"Authorization": "Bearer " + f.write}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("path-like name = %d", rec.Code)
	}
	if len(f.events.kinds(EventReleasePublished)) != 0 {
		t.Error("draft announced")
	}
	// Drafts are hidden from read-only callers.
	if rec, _ := apiDo(t, a, "GET", api+"/tags/v1.2.0", f.read, nil); rec.Code != 404 {
		t.Errorf("read token sees draft = %d", rec.Code)
	}
	rec, out = apiDo(t, a, "PATCH", fmt.Sprintf("%s/%d", api, id), f.write, map[string]any{"draft": false})
	if rec.Code != 200 || out["draft"] != false || out["published_at"] == nil {
		t.Fatalf("publish = %d %s", rec.Code, rec.Body)
	}
	if _, ok := f.tagCommit(t, "v1.2.0"); !ok {
		t.Error("publishing via the API didn't create the tag")
	}
	if evs := f.events.kinds(EventReleasePublished); len(evs) != 1 || evs[0].Actor.Kind != ActorToken {
		t.Errorf("events = %+v", evs)
	}

	// Read endpoints.
	rec, out = apiDo(t, a, "GET", api+"/tags/v1.2.0", f.read, nil)
	if rec.Code != 200 || len(out["assets"].([]any)) != 2 || out["tarball_url"] == "" {
		t.Errorf("by tag = %d %s", rec.Code, rec.Body)
	}
	if rec, out := apiDo(t, a, "GET", api+"/latest", f.read, nil); rec.Code != 200 || out["name"] != "Twelve" {
		t.Errorf("latest = %d %v", rec.Code, out["name"])
	}
	if rec := doRaw(t, a, "GET", api, nil, map[string]string{"Authorization": "token " + f.read}); rec.Code != 200 || !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		t.Errorf("list = %d", rec.Code)
	}
	req, _ := http.NewRequest("GET", "/", nil)
	req.SetBasicAuth("zoe", f.read)
	if rec := doRaw(t, a, "GET", fmt.Sprintf("%s/%d", api, id), nil, map[string]string{"Authorization": req.Header.Get("Authorization")}); rec.Code != 200 {
		t.Errorf("basic auth get = %d", rec.Code)
	}

	// Public repos: anonymous reads, never writes.
	_ = a.db.UpdateRepo(ctx, f.demo.ID, "", false)
	if rec, _ := apiDo(t, a, "GET", api, "", nil); rec.Code != 200 {
		t.Errorf("anonymous public list = %d", rec.Code)
	}
	if rec, _ := apiDo(t, a, "POST", api, "", map[string]any{"tag_name": "v9"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous create = %d", rec.Code)
	}
	if rec := do(t, a, "GET", "/zoe/demo/releases/download/v1.2.0/app-linux-amd64", nil, nil); rec.Code != 200 || rec.Body.String() != string(payload) {
		t.Errorf("anonymous public download = %d", rec.Code)
	}

	// Validation, generate-notes and delete.
	if rec, _ := apiDo(t, a, "POST", api, f.write, map[string]any{"tag_name": "v1.2.0"}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("duplicate = %d", rec.Code)
	}
	if rec, _ := apiDo(t, a, "POST", api, f.write, map[string]any{"tag_name": "v3", "target_commitish": "nope"}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad target = %d", rec.Code)
	}
	rec, out = apiDo(t, a, "POST", api+"/generate-notes", f.write, map[string]any{"tag_name": "v1.3.0"})
	if rec.Code != 200 || !strings.Contains(out["body"].(string), "No changes since v1.2.0") {
		t.Errorf("generate-notes = %d %s", rec.Code, rec.Body)
	}
	rec, out = apiDo(t, a, "POST", api, f.write, map[string]any{"tag_name": "v1.3.0", "generate_release_notes": true})
	if rec.Code != 201 || !strings.Contains(out["body"].(string), "v1.2.0") {
		t.Errorf("create with notes = %d %s", rec.Code, rec.Body)
	}
	assetID := as.ID
	if rec, _ := apiDo(t, a, "DELETE", fmt.Sprintf("%s/assets/%d", api, assetID), f.write, nil); rec.Code != 204 {
		t.Errorf("delete asset = %d", rec.Code)
	}
	if rec, _ := apiDo(t, a, "DELETE", fmt.Sprintf("%s/%d", api, id), f.write, nil); rec.Code != 204 {
		t.Errorf("delete = %d", rec.Code)
	}
	if rec, _ := apiDo(t, a, "GET", fmt.Sprintf("%s/%d", api, id), f.write, nil); rec.Code != 404 {
		t.Errorf("after delete = %d", rec.Code)
	}
}

// A stand-in for Actions job tokens: the Actions feature registers a
// TokenResolver and an AccessPolicy; releases only rely on resolveCredential
// and a.can.
func fakeJobTokens(t *testing.T, repoID int64) string {
	t.Helper()
	const plain = "atljob_test_0123456789"
	savedR, savedP := tokenResolvers, accessPolicies
	t.Cleanup(func() { tokenResolvers, accessPolicies = savedR, savedP })
	registerTokenResolver(func(_ context.Context, _ *App, s string) (Actor, bool) {
		if s == plain {
			return Actor{Kind: "testjob", Name: "MacBook"}, true
		}
		return Actor{}, false
	})
	registerAccessPolicy(func(_ context.Context, _ *App, who Actor, r store.Repo, need Perm) (bool, bool) {
		if who.Kind != "testjob" {
			return false, false
		}
		return r.ID == repoID && need != PermAdmin, true
	})
	return plain
}

func TestReleasesWithJobToken(t *testing.T) {
	f := newRelFixture(t)
	a := f.a
	job := fakeJobTokens(t, f.demo.ID)
	const api = "/api/v1/repos/zoe/demo/releases"
	rec, out := apiDo(t, a, "POST", api, job, map[string]any{"tag_name": "v5", "name": "Five"})
	if rec.Code != 201 || out["built_by"] != "MacBook" {
		t.Fatalf("job create = %d %s", rec.Code, rec.Body)
	}
	id := int64(out["id"].(float64))
	rec = doRaw(t, a, "PUT", fmt.Sprintf("%s/%d/assets/runner-darwin-arm64", api, id), strings.NewReader("mach-o"), map[string]string{"Authorization": "Bearer " + job})
	if rec.Code != 201 {
		t.Fatalf("job upload = %d %s", rec.Code, rec.Body)
	}
	if evs := f.events.kinds(EventReleasePublished); len(evs) != 1 || evs[0].Actor.Kind != "testjob" {
		t.Errorf("events = %+v", evs)
	}
	page := do(t, a, "GET", "/zoe/demo/releases", nil, f.zc).Body.String()
	if !strings.Contains(page, "Built by Actions on MacBook") || !strings.Contains(page, "runner-darwin-arm64") {
		t.Errorf("list page lacks the build info:\n%.2000s", page)
	}
	// Job tokens are limited to their repository.
	other := profRepo(t, a, f.zoe, "other", true, "a.txt", "a")
	_ = other
	if rec, _ := apiDo(t, a, "POST", "/api/v1/repos/zoe/other/releases", job, map[string]any{"tag_name": "v1"}); rec.Code != 404 {
		t.Errorf("job token on another repo = %d", rec.Code)
	}
}

func TestReleasePermissions(t *testing.T) {
	f := newRelFixture(t)
	a, ctx := f.a, context.Background()
	if _, err := a.createRelease(ctx, f.demo, mustOpen(t, a), Actor{Kind: ActorOwner, Name: "zoe", PersonID: f.zoe.ID}, releaseInput{Tag: "v1", Title: "One"}); err != nil {
		t.Fatal(err)
	}
	draft, err := a.createRelease(ctx, f.demo, mustOpen(t, a), Actor{Kind: ActorOwner, Name: "zoe", PersonID: f.zoe.ID}, releaseInput{Tag: "v2", Title: "Secret draft", Draft: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.addReleaseAsset(ctx, draft, Actor{Kind: ActorOwner, Name: "zoe"}, "d.bin", "", strings.NewReader("d"), 1); err != nil {
		t.Fatal(err)
	}

	// Private: sam can't even see it.
	for _, p := range []string{"/zoe/demo/releases", "/zoe/demo/releases/tag/v1", "/zoe/demo/releases/new"} {
		if rec := do(t, a, "GET", p, nil, f.sc); rec.Code != 404 {
			t.Errorf("sam GET %s on private = %d", p, rec.Code)
		}
	}
	// Public: sam reads published releases but can't change anything or see drafts.
	_ = a.db.UpdateRepo(ctx, f.demo.ID, "", false)
	rec := do(t, a, "GET", "/zoe/demo/releases", nil, f.sc)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "Secret draft") || strings.Contains(rec.Body.String(), "Draft a new release") {
		t.Errorf("sam list = %d, shows drafts or the new button", rec.Code)
	}
	if rec := do(t, a, "GET", "/zoe/demo/releases/tag/v2", nil, f.sc); rec.Code != 404 {
		t.Errorf("sam sees draft = %d", rec.Code)
	}
	if rec := do(t, a, "GET", "/zoe/demo/releases/download/v2/d.bin", nil, f.sc); rec.Code != 404 {
		t.Errorf("sam downloads draft file = %d", rec.Code)
	}
	if rec := do(t, a, "GET", "/zoe/demo/releases/new", nil, f.sc); rec.Code != 403 {
		t.Errorf("sam new form = %d", rec.Code)
	}
	for _, p := range []string{"/zoe/demo/releases/new", fmt.Sprintf("/zoe/demo/releases/edit/%d", draft.ID),
		fmt.Sprintf("/zoe/demo/releases/delete/%d", draft.ID), "/zoe/demo/releases/notes"} {
		if rec := do(t, a, "POST", p, form("tag", "v9", "action", "publish"), f.sc); rec.Code != 403 {
			t.Errorf("sam POST %s = %d", p, rec.Code)
		}
	}
	if _, err := a.db.ReleaseByTag(ctx, f.demo.ID, "v9"); err == nil {
		t.Error("sam created a release")
	}
	// The owner sees the draft.
	if !strings.Contains(do(t, a, "GET", "/zoe/demo/releases", nil, f.zc).Body.String(), "Secret draft") {
		t.Error("owner doesn't see the draft")
	}
}

func TestReleaseNotesGeneration(t *testing.T) {
	f := newRelFixture(t)
	a, ctx := f.a, context.Background()
	zoe := Actor{Kind: ActorOwner, Name: "zoe", PersonID: f.zoe.ID}
	if _, err := a.createRelease(ctx, f.demo, mustOpen(t, a), zoe, releaseInput{Tag: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	// After v1: a squash-merged pull request and a direct commit.
	prSHA := commitTo(t, a, "zoe", "demo", "feature.go", "package feature\n")
	direct := commitTo(t, a, "zoe", "demo", "fix.txt", "fixed\n")
	pr, err := a.db.CreatePull(ctx, store.NewPull{RepoID: f.demo.ID, Title: "Add the [feature]", Head: "feature", Base: "main", AuthorKind: "owner", AuthorName: "zoe"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.db.MarkPullMerged(ctx, pr.ID, "squash", prSHA, f.head, prSHA, "zoe"); err != nil {
		t.Fatal(err)
	}
	// An older merged PR (before v1) must not appear.
	old, _ := a.db.CreatePull(ctx, store.NewPull{RepoID: f.demo.ID, Title: "Ancient change", Head: "old", Base: "main"})
	_ = a.db.MarkPullMerged(ctx, old.ID, "squash", f.head, f.prev, f.head, "zoe")

	notes, prev, err := a.generateReleaseNotes(ctx, f.demo, mustOpen(t, a), "v1.1.0", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if prev != "v1.0.0" {
		t.Errorf("previous = %q", prev)
	}
	for _, want := range []string{"## What's changed", `Add the \[feature\] ([#` + fmt.Sprint(pr.Number) + `](/zoe/demo/pulls/` + fmt.Sprint(pr.Number) + `))`,
		"## Other commits", "Add fix.txt ([" + direct[:7] + "](/zoe/demo/commit/" + direct + "))", "**Full changelog:**"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	for _, not := range []string{"Ancient change", "Add main.go", "Add feature.go"} {
		if strings.Contains(notes, not) {
			t.Errorf("notes include %q:\n%s", not, notes)
		}
	}
	// The UI endpoint returns the same as JSON.
	rec := do(t, a, "POST", "/zoe/demo/releases/notes", form("tag", "v1.1.0", "target", "main"), f.zc)
	var out map[string]string
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out["body"] != notes || out["previous_tag"] != "v1.0.0" {
		t.Errorf("notes endpoint = %d %v", rec.Code, out)
	}
	// First release: everything so far, no changelog link.
	first, _, _ := a.generateReleaseNotes(ctx, f.demo, mustOpen(t, a), "v1.0.0", "", "")
	if !strings.Contains(first, "Add README.md") || strings.Contains(first, "Full changelog") {
		t.Errorf("first notes:\n%s", first)
	}
}

func TestReleaseRepoViewHelpers(t *testing.T) {
	f := newRelFixture(t)
	a, ctx := f.a, context.Background()
	v := &repoView{Repo: f.demo, Base: "/zoe/demo"}
	if v.ReleaseCount() != 0 || v.LatestRelease() != nil {
		t.Fatal("helpers report releases before there are any")
	}
	if _, err := a.createRelease(ctx, f.demo, mustOpen(t, a), Actor{Kind: ActorOwner, Name: "zoe"}, releaseInput{Tag: "v1", Title: "One"}); err != nil {
		t.Fatal(err)
	}
	if v.ReleaseCount() != 1 {
		t.Errorf("count = %d", v.ReleaseCount())
	}
	if l := v.LatestRelease(); l == nil || l.Tag != "v1" || l.Name != "One" || l.URL != "/zoe/demo/releases/tag/v1" {
		t.Errorf("latest = %+v", l)
	}
	// Every repository page carries the Releases tab.
	if !strings.Contains(do(t, a, "GET", "/zoe/demo", nil, f.zc).Body.String(), `href="/zoe/demo/releases"`) {
		t.Error("repo header has no Releases tab")
	}
}
