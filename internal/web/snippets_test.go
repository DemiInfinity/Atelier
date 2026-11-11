package web

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atelier/internal/backup"
	"atelier/internal/store"
)

// snipEnv is a git fixture (owner zoe, tokens) plus sessions for zoe and for a
// second person, bob.
type snipEnv struct {
	gitFixture
	owner, other map[string]string
}

func newSnipEnv(t *testing.T) snipEnv {
	t.Helper()
	f := newGitFixture(t)
	ctx := context.Background()
	sess, err := f.a.db.CreateSession(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := f.a.db.CreateOwner(ctx, "bob", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	bobSess, err := f.a.db.CreateSessionFor(ctx, bob.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return snipEnv{gitFixture: f,
		owner: map[string]string{"Cookie": sessionCookie + "=" + sess},
		other: map[string]string{"Cookie": sessionCookie + "=" + bobSess}}
}

func snippetForm(owner, desc, vis string, files ...string) url.Values {
	v := url.Values{"owner": {owner}, "description": {desc}, "visibility": {vis}}
	for i := 0; i+1 < len(files); i += 2 {
		v.Add("name", files[i])
		v.Add("content", files[i+1])
	}
	return v
}

// createTestSnippet creates a snippet through the web form and returns it.
func (e snipEnv) create(t *testing.T, form url.Values) store.Snippet {
	t.Helper()
	rec := do(t, e.a, "POST", "/snippets", form, e.owner)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create snippet = %d\n%.800s", rec.Code, rec.Body)
	}
	uid := strings.TrimPrefix(rec.Header().Get("Location"), "/snippets/")
	sn, err := e.a.db.SnippetByUID(context.Background(), uid)
	if err != nil {
		t.Fatalf("snippet %q: %v", uid, err)
	}
	return sn
}

func TestSnippetCRUDAndRevisions(t *testing.T) {
	e := newSnipEnv(t)
	a := e.a
	ctx := context.Background()

	// The editor's Add file and Remove buttons re-render without saving.
	rec := do(t, a, "POST", "/snippets", url.Values{"owner": {"zoe"}, "name": {"a.txt"}, "content": {"x"}, "op": {"add"}}, e.owner)
	if rec.Code != http.StatusOK || strings.Count(rec.Body.String(), "<textarea") != 2 {
		t.Errorf("Add file = %d with %d textareas", rec.Code, strings.Count(rec.Body.String(), "<textarea"))
	}
	if n, _ := a.db.Snippets(ctx, "", 10); len(n) != 0 {
		t.Errorf("Add file saved a snippet")
	}
	// Bad input is refused.
	for name, form := range map[string]url.Values{
		"duplicate": snippetForm("zoe", "", "private", "a.txt", "1", "A.TXT", "2"),
		"slash":     snippetForm("zoe", "", "private", "dir/a.txt", "1"),
		"empty":     snippetForm("zoe", "", "private", "a.txt", "  "),
		"dotgit":    snippetForm("zoe", "", "private", ".git", "x"),
		"no files":  snippetForm("zoe", "", "private"),
	} {
		if rec := do(t, a, "POST", "/snippets", form, e.owner); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: create = %d, want 400", name, rec.Code)
		}
	}
	// Bob may not create snippets in zoe's account.
	if rec := do(t, a, "POST", "/snippets", snippetForm("zoe", "", "private", "a.txt", "x"), e.other); rec.Code != http.StatusForbidden {
		t.Errorf("bob create in zoe = %d, want 403", rec.Code)
	}

	sn := e.create(t, snippetForm("zoe", "Deploy helpers", "private",
		"redeploy.sh", "#!/bin/sh\necho redeploy\n", "README.md", "# Notes\n\nSome *notes*.\n"))
	if !validSnippetUID(sn.UID) || !sn.Private || sn.Owner != "zoe" || strings.Join(sn.Files, ",") != "README.md,redeploy.sh" {
		t.Fatalf("created snippet = %+v", sn)
	}
	// Snippet repos live apart from repositories and never become one.
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, "snippets", sn.UID[:2], sn.UID+".git", "HEAD")); err != nil {
		t.Errorf("snippet repo not under <data>/snippets: %v", err)
	}
	if repos, _ := a.db.Repos(ctx); len(repos) != 1 {
		t.Errorf("repos = %d, want only the fixture repo", len(repos))
	}

	base := "/snippets/" + sn.UID
	rec = do(t, a, "GET", base, nil, e.owner)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "redeploy.sh") || !strings.Contains(body, "<h1>Notes</h1>") ||
		!strings.Contains(body, "<em>notes</em>") || !strings.Contains(body, sn.UID+".git") {
		t.Errorf("view = %d\n%.1500s", rec.Code, body)
	}
	for _, p := range []string{"/snippets", "/snippets?tab=starred", "/snippets?q=deploy", "/snippets/new", base + "/edit", base + "/revisions"} {
		if rec := do(t, a, "GET", p, nil, e.owner); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d\n%.500s", p, rec.Code, rec.Body)
		}
	}
	if rec := do(t, a, "GET", "/snippets?q=deploy", nil, e.owner); !strings.Contains(rec.Body.String(), sn.UID) {
		t.Errorf("snippet search misses the snippet")
	}
	if rec := do(t, a, "GET", "/snippets?q=nomatch", nil, e.owner); strings.Contains(rec.Body.String(), sn.UID) {
		t.Errorf("snippet search for nomatch shows the snippet")
	}
	if rec := do(t, a, "GET", base+"/raw/redeploy.sh", nil, e.owner); rec.Code != 200 || rec.Body.String() != "#!/bin/sh\necho redeploy\n" ||
		rec.Header().Get("Content-Security-Policy") != "sandbox" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("raw = %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	rec = do(t, a, "GET", base+"/archive.zip", nil, e.owner)
	if zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err != nil || len(zr.File) < 2 {
		t.Errorf("zip = %d, err %v", rec.Code, err)
	}

	// Edit: a new revision with a diff.
	g, _ := a.openSnippet(sn.UID)
	tip := a.snippetTip(ctx, g)
	edit := snippetForm("", "Deploy helpers v2", "private",
		"redeploy.sh", "#!/bin/sh\necho redeploy now\n", "README.md", "# Notes\n\nSome *notes*.\n", "extra.txt", "more")
	edit.Set("parent", tip)
	if rec := do(t, a, "POST", base+"/edit", edit, e.owner); rec.Code != http.StatusSeeOther {
		t.Fatalf("edit = %d\n%.800s", rec.Code, rec.Body)
	}
	if n := g.CommitCount(ctx, a.snippetTip(ctx, g)); n != 2 {
		t.Errorf("commits after edit = %d, want 2", n)
	}
	sn, _ = a.db.SnippetByUID(ctx, sn.UID)
	if sn.Description != "Deploy helpers v2" || len(sn.Files) != 3 {
		t.Errorf("after edit = %+v", sn)
	}
	rec = do(t, a, "GET", base+"/revisions", nil, e.owner)
	if body := rec.Body.String(); !strings.Contains(body, `"dl add">+<span class="hl-nb">echo</span> redeploy now`) || !strings.Contains(body, `"dl del">-<span class="hl-nb">echo</span> redeploy<`) ||
		!strings.Contains(body, "created this snippet") {
		t.Errorf("revisions page lacks the diff\n%.2000s", body)
	}
	// The old revision is still viewable, and raw at it.
	if rec := do(t, a, "GET", base+"/raw/redeploy.sh?rev="+tip, nil, e.owner); rec.Body.String() != "#!/bin/sh\necho redeploy\n" {
		t.Errorf("raw at old revision = %q", rec.Body)
	}
	if rec := do(t, a, "GET", base+"?rev="+tip, nil, e.owner); !strings.Contains(rec.Body.String(), "older revision") {
		t.Errorf("old revision view lacks the notice")
	}
	// Saving unchanged files (description only) makes no empty revision.
	same := snippetForm("", "Desc only", "private",
		"redeploy.sh", "#!/bin/sh\necho redeploy now\n", "README.md", "# Notes\n\nSome *notes*.\n", "extra.txt", "more")
	same.Set("parent", a.snippetTip(ctx, g))
	do(t, a, "POST", base+"/edit", same, e.owner)
	if n := g.CommitCount(ctx, a.snippetTip(ctx, g)); n != 2 {
		t.Errorf("commits after a description-only edit = %d, want 2", n)
	}
	// A stale parent is a conflict, not a silent overwrite.
	stale := snippetForm("", "x", "private", "redeploy.sh", "lost?")
	stale.Set("parent", tip)
	if rec := do(t, a, "POST", base+"/edit", stale, e.owner); rec.Code != http.StatusConflict {
		t.Errorf("stale edit = %d, want 409", rec.Code)
	}

	// Stars.
	if rec := do(t, a, "POST", base+"/star", url.Values{"star": {"1"}}, e.owner); rec.Code != http.StatusSeeOther {
		t.Errorf("star = %d", rec.Code)
	}
	if rec := do(t, a, "GET", "/snippets?tab=starred", nil, e.owner); !strings.Contains(rec.Body.String(), sn.UID) {
		t.Errorf("starred tab misses the snippet")
	}

	// Private: 404 for bob and for anonymous, everywhere.
	for _, p := range []string{base, base + "/raw/redeploy.sh", base + "/archive.zip", base + "/revisions"} {
		if rec := do(t, a, "GET", p, nil, nil); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "redeploy") {
			t.Errorf("anonymous GET %s = %d", p, rec.Code)
		}
		if rec := do(t, a, "GET", p, nil, e.other); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "redeploy") {
			t.Errorf("bob GET %s = %d", p, rec.Code)
		}
	}
	if rec := do(t, a, "GET", "/snippets", nil, e.other); strings.Contains(rec.Body.String(), sn.UID) {
		t.Errorf("bob's snippet list shows zoe's private snippet")
	}
	for _, p := range []string{base + "/edit", base + "/delete", base + "/star"} {
		if rec := do(t, a, "POST", p, url.Values{}, e.other); rec.Code != http.StatusNotFound {
			t.Errorf("bob POST %s = %d, want 404", p, rec.Code)
		}
	}
	if rec := do(t, a, "GET", "/snippets/0000000000000000beef", nil, e.owner); rec.Code != http.StatusNotFound {
		t.Errorf("unknown snippet = %d", rec.Code)
	}

	// Public: anyone can read; only the owner can change it.
	pub := snippetForm("", "Deploy helpers v2", "public",
		"redeploy.sh", "#!/bin/sh\necho redeploy now\n", "README.md", "# Notes\n\nSome *notes*.\n", "extra.txt", "more")
	pub.Set("parent", a.snippetTip(ctx, g))
	do(t, a, "POST", base+"/edit", pub, e.owner)
	rec = do(t, a, "GET", base, nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "redeploy.sh") || strings.Contains(rec.Body.String(), "Sign out") {
		t.Errorf("anonymous public view = %d\n%.600s", rec.Code, rec.Body)
	}
	if rec := do(t, a, "GET", base, nil, e.other); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "/edit\"") {
		t.Errorf("bob public view = %d (or shows Edit)", rec.Code)
	}
	if rec := do(t, a, "POST", base+"/edit", pub, e.other); rec.Code != http.StatusForbidden {
		t.Errorf("bob edit public = %d, want 403", rec.Code)
	}
	if rec := do(t, a, "POST", base+"/delete", nil, e.other); rec.Code != http.StatusForbidden {
		t.Errorf("bob delete public = %d, want 403", rec.Code)
	}

	// Delete removes the row and the repository.
	if rec := do(t, a, "POST", base+"/delete", nil, e.owner); rec.Code != http.StatusSeeOther {
		t.Errorf("delete = %d", rec.Code)
	}
	if rec := do(t, a, "GET", base, nil, e.owner); rec.Code != http.StatusNotFound {
		t.Errorf("after delete = %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, "snippets", sn.UID[:2], sn.UID+".git")); !os.IsNotExist(err) {
		t.Errorf("snippet repo still on disk: %v", err)
	}
}

func TestSnippetGitOverHTTPS(t *testing.T) {
	e := newSnipEnv(t)
	a := e.a
	ctx := context.Background()
	sn := e.create(t, snippetForm("zoe", "", "private", "hello.txt", "hello\n"))
	srv := httptest.NewServer(a)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	remote := func(tok string) string { return "http://zoe:" + tok + "@" + u.Host + "/snippets/" + sn.UID + ".git" }

	// Anonymous: a private snippet needs credentials, like a private repository.
	if rec := do(t, a, "GET", "/snippets/"+sn.UID+".git/info/refs?service=git-upload-pack", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous info/refs = %d, want 401", rec.Code)
	}
	if rec := do(t, a, "GET", "/snippets/0000000000000000dead.git/info/refs?service=git-upload-pack", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown snippet info/refs = %d, want 404", rec.Code)
	}
	cmd := exec.Command("git", "clone", srv.URL+"/snippets/"+sn.UID+".git", filepath.Join(t.TempDir(), "anon"))
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("anonymous clone of a private snippet succeeded:\n%s", out)
	}

	// Clone with a read token, push with a write token.
	work := filepath.Join(t.TempDir(), "work")
	runGit(t, t.TempDir(), "clone", remote(e.read), work)
	if b, _ := os.ReadFile(filepath.Join(work, "hello.txt")); string(b) != "hello\n" {
		t.Fatalf("cloned hello.txt = %q", b)
	}
	if err := os.WriteFile(filepath.Join(work, "pushed.md"), []byte("# Pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "Add pushed.md")
	cmd = exec.Command("git", "push", remote(e.read), "main")
	cmd.Dir = work
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("push with a read token succeeded:\n%s", out)
	}
	runGit(t, work, "push", remote(e.write), "main")

	sn2, _ := a.db.SnippetByUID(ctx, sn.UID)
	if strings.Join(sn2.Files, ",") != "hello.txt,pushed.md" {
		t.Errorf("files after push = %v", sn2.Files)
	}
	rec := do(t, a, "GET", "/snippets/"+sn.UID, nil, e.owner)
	if !strings.Contains(rec.Body.String(), "<h1>Pushed</h1>") {
		t.Errorf("view after push lacks pushed.md")
	}
	// The web editor builds on the pushed commit.
	g, _ := a.openSnippet(sn.UID)
	if n := g.CommitCount(ctx, a.snippetTip(ctx, g)); n != 2 {
		t.Errorf("commits after push = %d", n)
	}

	// Public snippets clone anonymously; pushing still needs a token.
	_ = a.db.UpdateSnippet(ctx, sn.ID, "", false)
	runGit(t, t.TempDir(), "clone", srv.URL+"/snippets/"+sn.UID+".git", filepath.Join(t.TempDir(), "pub"))
	if rec := do(t, a, "GET", "/snippets/"+sn.UID+".git/info/refs?service=git-receive-pack", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous receive-pack on a public snippet = %d", rec.Code)
	}
}

func TestBackupIncludesSnippets(t *testing.T) {
	e := newSnipEnv(t)
	a := e.a
	sn := e.create(t, snippetForm("zoe", "", "private", "keep.txt", "keep me\n"))
	res, err := backup.Create(context.Background(), backup.Options{DB: a.db.DB(), RepoRoot: a.cfg.RepoRoot(),
		DataDir: a.cfg.DataDir, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	prefix := "snippets/" + sn.UID[:2] + "/" + sn.UID + ".git/"
	var head bool
	n := 0
	for _, f := range res.Manifest.Files {
		if strings.HasPrefix(f.Path, prefix) {
			n++
			head = head || f.Path == prefix+"HEAD"
		}
	}
	if !head || n < 3 {
		t.Errorf("snapshot has %d files of the snippet repo (HEAD %v)", n, head)
	}
	for _, rp := range res.Manifest.Repos {
		if strings.Contains(rp.Name, sn.UID) {
			t.Errorf("snippet backed up as a repository: %+v", rp)
		}
	}
}

func TestReservedSnippetNames(t *testing.T) {
	e := newSnipEnv(t)
	for _, name := range []string{"snippets", "Snippets", "-"} {
		rec := do(t, e.a, "POST", "/accounts/new", url.Values{"name": {name}}, e.owner)
		if rec.Code == http.StatusSeeOther {
			t.Errorf("account %q was created", name)
		}
		if _, err := e.a.db.OwnerByName(context.Background(), name); err == nil {
			t.Errorf("account %q exists", name)
		}
	}
}
