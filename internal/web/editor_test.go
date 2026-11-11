package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// editFixture: zoe/demo with main = README.md, app.txt, docs/guide.md.
func editFixture(t *testing.T) (gitFixture, map[string]string, string) {
	t.Helper()
	f := newGitFixture(t)
	work := pullsWork(t, f.a)
	pullsCommit(t, work, "README.md", "# Demo\n", "First")
	pullsCommit(t, work, "app.txt", "one\ntwo\n", "App")
	pullsCommit(t, work, "docs/guide.md", "Guide\n", "Docs")
	runGit(t, work, "push", "-q", "origin", "main")
	return f, pullsCookie(t, f.a), work
}

func editTip(t *testing.T, a *App, branch string) string {
	t.Helper()
	sha, err := gitBranch(t, a, branch)
	if err != nil {
		t.Fatalf("branch %s: %v", branch, err)
	}
	return sha
}

// editFiles returns path → content at a ref of zoe/demo.
func editFiles(t *testing.T, a *App, ref string) map[string]string {
	t.Helper()
	dir := a.git.Path("zoe", "demo")
	out := map[string]string{}
	for _, p := range strings.Split(runGit(t, dir, "ls-tree", "-r", "--name-only", ref), "\n") {
		if p != "" {
			out[p] = runGit(t, dir, "show", ref+":"+p)
		}
	}
	return out
}

func editLastCommit(t *testing.T, a *App, ref string) string {
	t.Helper()
	return runGit(t, a.git.Path("zoe", "demo"), "log", "-1", "--format=%an <%ae>|%B", ref)
}

func editPost(t *testing.T, a *App, target string, form url.Values, ck map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, a, "POST", target, form, ck)
}

func TestEditorCommits(t *testing.T) {
	f, ck, _ := editFixture(t)
	a := f.a

	// The file page has the pencil; the editor shows the file and its base.
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/blob/main/README.md", nil, ck), 200, "blob page")
	mustContain(t, body, "blob page", `href="/zoe/demo/edit/main/README.md"`, `href="/zoe/demo/delete-file/main/README.md"`)
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo", nil, ck), 200, "code page")
	mustContain(t, body, "code page", `href="/zoe/demo/new/main"`, `href="/zoe/demo/upload/main"`, "Open on…", "/static/workspaces.js")
	base := editTip(t, a, "main")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/edit/main/README.md", nil, ck), 200, "editor")
	mustContain(t, body, "editor", "# Demo", `name="base_commit" value="`+base+`"`, `placeholder="Update README.md"`,
		"Commit directly to the", "start a pull request", "/static/editor.js")

	// Edit.
	rec := editPost(t, a, "/zoe/demo/edit?at=main/README.md", url.Values{"path": {"README.md"}, "content": {"# Demo\r\n\r\nNow edited.\r\n"},
		"base_commit": {base}, "commit_choice": {"direct"}, "description": {"Longer story."}}, ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/zoe/demo/blob/main/README.md" {
		t.Fatalf("edit = %d %q\n%.2000s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if got := editFiles(t, a, "main")["README.md"]; got != "# Demo\n\nNow edited." {
		t.Fatalf("README after edit = %q (CRLF from the browser must become LF)", got)
	}
	if c := editLastCommit(t, a, "main"); !strings.HasPrefix(c, "zoe <zoe@atelier.local>|Update README.md\n\nLonger story.") {
		t.Errorf("edit commit = %q", c)
	}

	// Rename / move (content unchanged).
	base = editTip(t, a, "main")
	rec = editPost(t, a, "/zoe/demo/edit?at=main/app.txt", url.Values{"path": {"src/app.txt"}, "content": {"one\ntwo\n"},
		"base_commit": {base}, "commit_choice": {"direct"}}, ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/zoe/demo/blob/main/src/app.txt" {
		t.Fatalf("rename = %d %q\n%.2000s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	files := editFiles(t, a, "main")
	if _, old := files["app.txt"]; old || files["src/app.txt"] != "one\ntwo" {
		t.Fatalf("after rename: %v", files)
	}
	if c := editLastCommit(t, a, "main"); !strings.Contains(c, "|Rename app.txt to src/app.txt") {
		t.Errorf("rename commit = %q", c)
	}

	// Create, with new folders; then the same path again is refused.
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/new/main/docs", nil, ck), 200, "new file page")
	mustContain(t, body, "new file page", `value="docs/"`, `action="/zoe/demo/new?at=main%2Fdocs"`)
	base = editTip(t, a, "main")
	newForm := url.Values{"path": {"docs/how-to/deep.md"}, "content": {"Deep\n"}, "base_commit": {base}, "commit_choice": {"direct"},
		"message": {"Add the deep doc"}}
	rec = editPost(t, a, "/zoe/demo/new?at=main/docs", newForm, ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/zoe/demo/blob/main/docs/how-to/deep.md" {
		t.Fatalf("create = %d %q\n%.2000s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if files := editFiles(t, a, "main"); files["docs/how-to/deep.md"] != "Deep" {
		t.Fatalf("after create: %v", files)
	}
	if c := editLastCommit(t, a, "main"); !strings.Contains(c, "|Add the deep doc") {
		t.Errorf("create commit = %q", c)
	}
	newForm.Set("base_commit", editTip(t, a, "main"))
	body = expectStatus(t, editPost(t, a, "/zoe/demo/new?at=main/docs", newForm, ck), 422, "create existing")
	mustContain(t, body, "create existing", "already a file or folder at docs/how-to/deep.md")
	for _, bad := range []string{"../escape.txt", ".git/hooks/pre-receive", "docs/../x"} {
		newForm.Set("path", bad)
		expectStatus(t, editPost(t, a, "/zoe/demo/new?at=main", newForm, ck), 422, "create "+bad)
	}

	// Upload two files into docs/.
	base = editTip(t, a, "main")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, data := range map[string]string{"one.bin": "\x00\x01binary", "two.txt": "Two\n"} {
		w, _ := mw.CreateFormFile("files", name)
		w.Write([]byte(data))
	}
	mw.WriteField("base_commit", base)
	mw.WriteField("commit_choice", "direct")
	mw.Close()
	req := httptest.NewRequest("POST", "/zoe/demo/upload?at=main/docs", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Cookie", ck["Cookie"])
	rec = doReq(a, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/zoe/demo/tree/main/docs" {
		t.Fatalf("upload = %d %q\n%.2000s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	files = editFiles(t, a, "main")
	if files["docs/two.txt"] != "Two" || !strings.HasSuffix(files["docs/one.bin"], "binary") {
		t.Fatalf("after upload: %v", files)
	}
	if c := editLastCommit(t, a, "main"); !strings.Contains(c, "|Add files via upload") {
		t.Errorf("upload commit = %q", c)
	}

	// Delete.
	expectStatus(t, do(t, a, "GET", "/zoe/demo/delete-file/main/docs/guide.md", nil, ck), 200, "delete page")
	rec = editPost(t, a, "/zoe/demo/delete-file?at=main/docs/guide.md", url.Values{"base_commit": {editTip(t, a, "main")},
		"commit_choice": {"direct"}}, ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/zoe/demo/tree/main/docs" {
		t.Fatalf("delete = %d %q\n%.2000s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if _, ok := editFiles(t, a, "main")["docs/guide.md"]; ok {
		t.Fatal("deleted file still there")
	}
	if c := editLastCommit(t, a, "main"); !strings.Contains(c, "|Delete guide.md") {
		t.Errorf("delete commit = %q", c)
	}

	// Markdown preview is rendered server-side, with raw HTML escaped.
	body = expectStatus(t, editPost(t, a, "/zoe/demo/edit-preview", url.Values{"text": {"# Hi\n\n<script>x</script>"}}, ck), 200, "preview")
	if !strings.Contains(body, "<h1") || strings.Contains(body, "<script>") {
		t.Errorf("preview = %q", body)
	}

	// Tags and commits aren't editable.
	runGit(t, a.git.Path("zoe", "demo"), "tag", "v1", "main")
	expectStatus(t, do(t, a, "GET", "/zoe/demo/edit/v1/README.md", nil, ck), 422, "edit a tag")
}

func TestEditorEmptyRepository(t *testing.T) {
	f := newGitFixture(t)
	ck := pullsCookie(t, f.a)
	body := expectStatus(t, do(t, f.a, "GET", "/zoe/demo", nil, ck), 200, "empty code page")
	mustContain(t, body, "empty code page", `href="/zoe/demo/new/main"`, `href="/zoe/demo/upload/main"`)
	rec := editPost(t, f.a, "/zoe/demo/new?at=main", url.Values{"path": {"README.md"}, "content": {"# Hello\n"}, "commit_choice": {"direct"}}, ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("first file = %d\n%.2000s", rec.Code, rec.Body)
	}
	if got := editFiles(t, f.a, "main")["README.md"]; got != "# Hello" {
		t.Fatalf("README = %q", got)
	}
}

func TestEditorStaleBaseRefused(t *testing.T) {
	f, ck, work := editFixture(t)
	a := f.a
	base := editTip(t, a, "main")
	// Someone pushes a change to the same file after the editor opened.
	runGit(t, work, "pull", "-q", "origin", "main")
	pullsCommit(t, work, "README.md", "# Demo\n\nPushed from the laptop.\n", "Laptop edit")
	runGit(t, work, "push", "-q", "origin", "main")
	pushed := editTip(t, a, "main")

	form := url.Values{"path": {"README.md"}, "content": {"# Demo\n\nWeb edit.\n"}, "base_commit": {base}, "commit_choice": {"direct"}}
	body := expectStatus(t, editPost(t, a, "/zoe/demo/edit?at=main/README.md", form, ck), http.StatusConflict, "stale edit")
	mustContain(t, body, "stale edit", "main changed since you started", "Laptop edit", "Pushed from the laptop.",
		"Nothing was committed, and nothing was overwritten", "Web edit.", "Create a new branch")
	if editTip(t, a, "main") != pushed {
		t.Fatal("a stale edit moved main")
	}

	// A stale base for a file that did NOT change builds on the new tip.
	form = url.Values{"path": {"app.txt"}, "content": {"one\ntwo\nthree\n"}, "base_commit": {base}, "commit_choice": {"direct"}}
	expectStatus(t, editPost(t, a, "/zoe/demo/edit?at=main/app.txt", form, ck), http.StatusSeeOther, "edit other file")
	files := editFiles(t, a, "main")
	if files["README.md"] != "# Demo\n\nPushed from the laptop." || files["app.txt"] != "one\ntwo\nthree" {
		t.Fatalf("after edit on moved branch: %v", files)
	}

	// The same stale edit to a new branch starts where the person started and opens a PR.
	tip := editTip(t, a, "main")
	form = url.Values{"path": {"README.md"}, "content": {"# Demo\n\nWeb edit.\n"}, "base_commit": {base},
		"commit_choice": {"branch"}, "new_branch": {"zoe-patch-1"}}
	rec := editPost(t, a, "/zoe/demo/edit?at=main/README.md", form, ck)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/zoe/demo/pulls/") {
		t.Fatalf("stale edit to new branch = %d %q\n%.2000s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if editTip(t, a, "main") != tip {
		t.Fatal("new-branch commit moved main")
	}
	if p := runGit(t, a.git.Path("zoe", "demo"), "rev-parse", "zoe-patch-1^"); p != base {
		t.Errorf("new branch parent = %s, want the edit's base %s", p, base)
	}
}

func TestEditorProtectedBranchForcesPullRequest(t *testing.T) {
	f, ck, _ := editFixture(t)
	a := f.a
	ctx := context.Background()
	rp, _ := a.db.RepoByName(ctx, "zoe", "demo")
	if _, err := a.db.CreateBranchRule(ctx, store.BranchRule{RepoID: rp.ID, Pattern: "main", RequirePR: true}); err != nil {
		t.Fatal(err)
	}
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/edit/main/README.md", nil, ck), 200, "editor")
	mustContain(t, body, "editor", `main is protected by branch rule &#34;main&#34;: changes must go through a pull request`,
		`value="direct"  disabled`, `value="branch" checked`)

	base := editTip(t, a, "main")
	form := url.Values{"path": {"README.md"}, "content": {"# Protected edit\n"}, "base_commit": {base}, "commit_choice": {"direct"},
		"message": {"Tweak the title"}, "description": {"Why: clarity."}}
	body = expectStatus(t, editPost(t, a, "/zoe/demo/edit?at=main/README.md", form, ck), 422, "direct to protected")
	mustContain(t, body, "direct to protected", "changes must go through a pull request", "# Protected edit")
	if editTip(t, a, "main") != base {
		t.Fatal("direct commit reached a protected branch")
	}

	form.Set("commit_choice", "branch")
	form.Set("new_branch", "zoe-patch-1")
	rec := editPost(t, a, "/zoe/demo/edit?at=main/README.md", form, ck)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/zoe/demo/pulls/1" {
		t.Fatalf("new branch = %d %q\n%.2000s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	p, err := a.db.PullByNumber(ctx, rp.ID, 1)
	if err != nil || p.Head != "zoe-patch-1" || p.Base != "main" || p.Title != "Tweak the title" || p.Body != "Why: clarity." || !p.IsOpen() {
		t.Fatalf("pull = %+v, %v", p, err)
	}
	if editTip(t, a, "main") != base || editFiles(t, a, "zoe-patch-1")["README.md"] != "# Protected edit" {
		t.Fatal("protected branch changed, or the new branch lacks the edit")
	}
	// An existing branch name is refused, not reused.
	form.Set("base_commit", base)
	body = expectStatus(t, editPost(t, a, "/zoe/demo/edit?at=main/README.md", form, ck), 422, "branch exists")
	mustContain(t, body, "branch exists", "A branch called zoe-patch-1 already exists")
}

// Web commits go through the real pre-receive hook: branch rules and secret
// scanning push protection, exactly like a push.
func TestEditorRunsPushChecks(t *testing.T) {
	bin := buildAtelier(t)
	gitrepo.HookExecutable = bin
	t.Cleanup(func() { gitrepo.HookExecutable = "" })
	f, ck, _ := editFixture(t) // creating zoe/demo installs the hooks
	a := f.a
	g, _ := a.git.Open("zoe", "demo")
	if !g.HasPreReceiveHook() {
		t.Fatal("no pre-receive hook installed")
	}

	base := editTip(t, a, "main")
	gh := secFakeGitHub()
	form := url.Values{"path": {"config/app.env"}, "content": {"NAME=demo\nTOKEN=" + gh + "\n"}, "base_commit": {base}, "commit_choice": {"direct"}}
	body := expectStatus(t, editPost(t, a, "/zoe/demo/new?at=main", form, ck), 422, "secret commit")
	mustContain(t, body, "secret commit", "Atelier's push checks refused this commit", "secret scanning: GitHub personal access token in config/app.env:2",
		"Push protection blocked this push")
	if strings.Contains(body, "TOKEN="+gh+"\n") && strings.Count(body, gh) > 1 {
		t.Error("the secret is echoed outside the editor")
	}
	if editTip(t, a, "main") != base {
		t.Fatal("a commit with a secret reached main")
	}
	// Nothing from the refused commit landed in the object store.
	blob := strings.TrimSpace(gitStdin(t, "NAME=demo\nTOKEN="+gh+"\n", "hash-object", "--stdin"))
	if err := exec.Command("git", "-C", a.git.Path("zoe", "demo"), "cat-file", "-e", blob).Run(); err == nil {
		t.Error("the refused secret's blob is in the repository")
	}
	ctx := context.Background()
	rp, _ := a.db.RepoByName(ctx, "zoe", "demo")
	if blocks, _ := a.db.SecretPushBlocks(ctx, rp.ID, 10); len(blocks) != 1 || blocks[0].Path != "config/app.env" {
		t.Errorf("push blocks = %+v", blocks)
	}

	// A clean commit passes the hook.
	form.Set("content", "NAME=demo\n")
	expectStatus(t, editPost(t, a, "/zoe/demo/new?at=main", form, ck), http.StatusSeeOther, "clean commit")
}

func gitStdin(t *testing.T, in string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Stdin = strings.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

var (
	editEventsOnce sync.Once
	editEventsMu   sync.Mutex
	editEvents     []Event
)

func TestEditorCommitFiresPushEvents(t *testing.T) {
	editEventsOnce.Do(func() {
		registerEventHandler(func(ctx context.Context, a *App, ev Event) {
			editEventsMu.Lock()
			editEvents = append(editEvents, ev)
			editEventsMu.Unlock()
		})
	})
	editEventsMu.Lock()
	editEvents = nil
	editEventsMu.Unlock()

	f, ck, _ := editFixture(t)
	a := f.a
	base := editTip(t, a, "main")
	expectStatus(t, editPost(t, a, "/zoe/demo/edit?at=main/README.md", url.Values{"path": {"README.md"}, "content": {"# Changed\n"},
		"base_commit": {base}, "commit_choice": {"direct"}}, ck), http.StatusSeeOther, "edit")
	tip := editTip(t, a, "main")
	a.drainActionsPushes(context.Background())

	editEventsMu.Lock()
	defer editEventsMu.Unlock()
	var pushed []Event
	for _, ev := range editEvents {
		if ev.Kind == EventRepoPushed && ev.Repo.FullName() == "zoe/demo" {
			pushed = append(pushed, ev)
		}
	}
	if len(pushed) != 1 {
		t.Fatalf("repo.pushed events = %d (%+v)", len(pushed), editEvents)
	}
	p := pushed[0].Push
	if p == nil || p.Ref != "refs/heads/main" || p.Old != base || p.New != tip || p.Pusher != "owner:zoe" {
		t.Fatalf("push = %+v", p)
	}
}

func TestEditorSecondPersonCannotEdit(t *testing.T) {
	f, _, _ := editFixture(t)
	a := f.a
	ctx := context.Background()
	other, err := a.db.CreateOwner(ctx, "visitor", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := a.db.CreateSessionFor(ctx, other.ID, time.Hour)
	vk := map[string]string{"Cookie": sessionCookie + "=" + sess}
	base := editTip(t, a, "main")
	form := url.Values{"path": {"README.md"}, "content": {"# Visitor\n"}, "base_commit": {base}, "commit_choice": {"direct"}}

	// Private repository: it doesn't exist for them.
	expectStatus(t, do(t, a, "GET", "/zoe/demo/edit/main/README.md", nil, vk), 404, "visitor edit page")
	expectStatus(t, editPost(t, a, "/zoe/demo/edit?at=main/README.md", form, vk), 404, "visitor edit")
	expectStatus(t, editPost(t, a, "/zoe/demo/new?at=main", url.Values{"path": {"x.txt"}, "content": {"x"}, "base_commit": {base}}, vk), 404, "visitor create")
	// Public: they can read it, but not change it.
	rp, _ := a.db.RepoByName(ctx, "zoe", "demo")
	_ = a.db.UpdateRepo(ctx, rp.ID, "", false)
	expectStatus(t, do(t, a, "GET", "/zoe/demo/edit/main/README.md", nil, vk), 403, "visitor edit page (public)")
	expectStatus(t, editPost(t, a, "/zoe/demo/edit?at=main/README.md", form, vk), 403, "visitor edit (public)")
	expectStatus(t, editPost(t, a, "/zoe/demo/delete-file?at=main/README.md", url.Values{"base_commit": {base}}, vk), 403, "visitor delete (public)")
	if editTip(t, a, "main") != base {
		t.Fatal("a visitor changed main")
	}
}
