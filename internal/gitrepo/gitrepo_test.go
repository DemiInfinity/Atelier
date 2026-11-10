package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	cases := map[string]bool{
		"atelier":                true,
		"Atelier-2":              true,
		"my.repo_name":           true,
		"a":                      true,
		"0day":                   true,
		strings.Repeat("a", 100): true,
		strings.Repeat("a", 101): false,
		"":                       false,
		".hidden":                false,
		"-dash":                  false,
		"_under":                 false,
		"repo.git":               false,
		"Repo.GIT":               false,
		"a..b":                   false,
		"..":                     false,
		"a/b":                    false,
		"a b":                    false,
		"naïve":                  false,
		"repo\n":                 false,
	}
	for name, want := range cases {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

// gitEnv isolates git from the user's global config so tests are reproducible.
func gitEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GIT_AUTHOR_NAME", "Test Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test Author")
	t.Setenv("GIT_COMMITTER_EMAIL", "author@example.com")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepoLifecycle(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	s, err := New(filepath.Join(t.TempDir(), "repos"))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Init(ctx, "Zoe", "Demo"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bare := s.Path("zoe", "demo")
	if bare != filepath.Join(s.Root, "zoe", "demo.git") {
		t.Errorf("Path = %s", bare)
	}
	if s.RelPath("ZOE", "Demo") != "zoe/demo.git" {
		t.Errorf("RelPath = %s", s.RelPath("ZOE", "Demo"))
	}
	if got := git(t, bare, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Fatalf("not a bare repo: %s", got)
	}
	if got := git(t, bare, "config", "http.receivepack"); got != "true" {
		t.Errorf("http.receivepack = %q", got)
	}
	if err := s.Init(ctx, "zoe", "demo"); err == nil {
		t.Error("Init over an existing repo should fail")
	}
	if _, err := s.Open("zoe", "missing"); err != ErrNotFound {
		t.Errorf("Open missing = %v", err)
	}

	r, err := s.Open("ZOE", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsEmpty(ctx) {
		t.Error("new repo should be empty")
	}
	if r.DefaultBranch(ctx) != "main" {
		t.Errorf("DefaultBranch on empty = %q", r.DefaultBranch(ctx))
	}

	// Build history in a working clone and push it.
	work := t.TempDir()
	git(t, work, "init", "--initial-branch=main", ".")
	write(t, filepath.Join(work, "README.md"), "# Demo\n\nHello.\n")
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "Add README")
	write(t, filepath.Join(work, "docs", "guide", "intro.txt"), "line one\nline two\n")
	write(t, filepath.Join(work, "bin.dat"), "a\x00b")
	git(t, work, "add", ".")
	git(t, work, "commit", "-m", "Add docs", "-m", "With a body.")
	git(t, work, "remote", "add", "origin", bare)
	git(t, work, "push", "origin", "main")
	git(t, work, "switch", "-c", "feature/x")
	write(t, filepath.Join(work, "docs", "guide", "intro.txt"), "line one\nline 2\n")
	git(t, work, "commit", "-am", "Tweak intro on feature")
	git(t, work, "push", "origin", "feature/x")
	git(t, work, "tag", "v1.0", "main")
	git(t, work, "push", "origin", "v1.0")
	mainSHA := git(t, work, "rev-parse", "main")
	featSHA := git(t, work, "rev-parse", "feature/x")

	if r.IsEmpty(ctx) {
		t.Fatal("repo should not be empty after push")
	}
	if r.DefaultBranch(ctx) != "main" {
		t.Errorf("DefaultBranch = %q", r.DefaultBranch(ctx))
	}
	br := r.Branches(ctx)
	if len(br) != 2 || !contains(br, "main") || !contains(br, "feature/x") {
		t.Errorf("Branches = %v", br)
	}
	if tags := r.Tags(ctx); len(tags) != 1 || tags[0] != "v1.0" {
		t.Errorf("Tags = %v", tags)
	}

	// ResolveCommit
	if c, err := r.ResolveCommit(ctx, "feature/x"); err != nil || c != featSHA {
		t.Errorf("ResolveCommit(feature/x) = %s, %v", c, err)
	}
	for _, bad := range []string{"", "nope", "--all", "-n1", "main:README.md", "a b"} {
		if _, err := r.ResolveCommit(ctx, bad); err == nil {
			t.Errorf("ResolveCommit(%q) should fail", bad)
		}
	}

	// SplitRefPath with a slashed branch.
	for _, tc := range []struct{ in, ref, path string }{
		{"feature/x/docs/guide/intro.txt", "feature/x", "docs/guide/intro.txt"},
		{"feature/x", "feature/x", ""},
		{"main/docs", "main", "docs"},
		{"v1.0/README.md", "v1.0", "README.md"},
		{mainSHA + "/README.md", mainSHA, "README.md"},
	} {
		ref, p, ok := r.SplitRefPath(ctx, tc.in)
		if !ok || ref != tc.ref || p != tc.path {
			t.Errorf("SplitRefPath(%q) = %q, %q, %v; want %q, %q", tc.in, ref, p, ok, tc.ref, tc.path)
		}
	}
	if _, _, ok := r.SplitRefPath(ctx, "feature/y/docs"); ok {
		t.Error("SplitRefPath with unknown ref should fail")
	}

	// Tree: dirs first, then files, case-insensitively.
	entries, err := r.Tree(ctx, mainSHA, "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "docs,bin.dat,README.md" {
		t.Errorf("Tree root = %v", names)
	}
	if !entries[0].IsDir() || entries[2].Size != int64(len("# Demo\n\nHello.\n")) || entries[2].Path != "README.md" {
		t.Errorf("Tree entries = %+v", entries)
	}
	sub, err := r.Tree(ctx, mainSHA, "/docs/guide/")
	if err != nil || len(sub) != 1 || sub[0].Path != "docs/guide/intro.txt" || sub[0].Type != "blob" {
		t.Errorf("Tree(docs/guide) = %+v, %v", sub, err)
	}
	if _, err := r.Tree(ctx, mainSHA, "nope"); err != ErrNotFound {
		t.Errorf("Tree missing = %v", err)
	}

	// ObjectType
	for p, want := range map[string]string{"": "tree", "docs": "tree", "docs/guide/intro.txt": "blob", "missing": ""} {
		if got := r.ObjectType(ctx, mainSHA, p); got != want {
			t.Errorf("ObjectType(%q) = %q, want %q", p, got, want)
		}
	}

	// Blob
	b, err := r.Blob(ctx, featSHA, "docs/guide/intro.txt", MaxBlob)
	if err != nil || string(b.Data) != "line one\nline 2\n" || b.Binary || b.TooLarge || b.Size != 16 {
		t.Errorf("Blob = %+v, %v", b, err)
	}
	b, err = r.Blob(ctx, mainSHA, "bin.dat", MaxBlob)
	if err != nil || !b.Binary {
		t.Errorf("binary Blob = %+v, %v", b, err)
	}
	b, err = r.Blob(ctx, mainSHA, "README.md", 4)
	if err != nil || !b.TooLarge || b.Data != nil {
		t.Errorf("limited Blob = %+v, %v", b, err)
	}
	if _, err := r.Blob(ctx, mainSHA, "", MaxBlob); err != ErrNotFound {
		t.Errorf("Blob of root = %v", err)
	}

	// Path traversal is rejected everywhere.
	for _, p := range []string{"..", "../README.md", "docs/../README.md", "docs/guide/../../README.md"} {
		if _, err := r.Tree(ctx, mainSHA, p); err != ErrNotFound {
			t.Errorf("Tree(%q) = %v, want ErrNotFound", p, err)
		}
		if got := r.ObjectType(ctx, mainSHA, p); got != "" {
			t.Errorf("ObjectType(%q) = %q, want empty", p, got)
		}
		if _, err := r.Blob(ctx, mainSHA, p, MaxBlob); err != ErrNotFound {
			t.Errorf("Blob(%q) = %v, want ErrNotFound", p, err)
		}
	}

	// Log
	log, err := r.Log(ctx, featSHA, "", 0, 10)
	if err != nil || len(log) != 3 {
		t.Fatalf("Log = %+v, %v", log, err)
	}
	if log[0].Hash != featSHA || log[0].Subject != "Tweak intro on feature" || log[0].Short != featSHA[:7] ||
		log[0].Author != "Test Author" || log[0].Email != "author@example.com" || len(log[0].Parents) != 1 {
		t.Errorf("Log[0] = %+v", log[0])
	}
	if log[1].Subject != "Add docs" || log[1].Body != "With a body." {
		t.Errorf("Log[1] = %+v", log[1])
	}
	if len(log[2].Parents) != 0 {
		t.Errorf("root commit has parents: %v", log[2].Parents)
	}
	if l, _ := r.Log(ctx, featSHA, "docs/guide/intro.txt", 0, 10); len(l) != 2 {
		t.Errorf("Log for path = %d commits, want 2", len(l))
	}
	if l, _ := r.Log(ctx, featSHA, "", 1, 1); len(l) != 1 || l[0].Subject != "Add docs" {
		t.Errorf("Log skip/limit = %+v", l)
	}
	if lc := r.LastCommit(ctx, mainSHA, "README.md"); lc == nil || lc.Subject != "Add README" {
		t.Errorf("LastCommit = %+v", lc)
	}
	if n := r.CommitCount(ctx, featSHA); n != 3 {
		t.Errorf("CommitCount = %d", n)
	}

	// Show
	c, patch, err := r.Show(ctx, featSHA)
	if err != nil || c.Hash != featSHA {
		t.Fatalf("Show = %+v, %v", c, err)
	}
	for _, want := range []string{"diff --git a/docs/guide/intro.txt b/docs/guide/intro.txt", "-line two", "+line 2", "1 file changed"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch missing %q:\n%s", want, patch)
		}
	}
	if _, _, err := r.Show(ctx, strings.Repeat("0", 40)); err != ErrNotFound {
		t.Errorf("Show missing = %v", err)
	}

	if err := s.Remove("zoe", "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bare); !os.IsNotExist(err) {
		t.Errorf("repo still on disk after Remove: %v", err)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestIsGitPath(t *testing.T) {
	for _, tc := range []struct {
		path              string
		owner, repo, rest string
		ok                bool
	}{
		{"/zoe/demo.git/info/refs", "zoe", "demo", "info/refs", true},
		{"/zoe/Demo.GIT/git-upload-pack", "zoe", "Demo", "git-upload-pack", true},
		{"/zoe/demo.git/git-receive-pack", "zoe", "demo", "git-receive-pack", true},
		{"/zoe/demo.git/objects/info/packs", "zoe", "demo", "objects/info/packs", true},
		{"/zoe/demo.git/HEAD", "zoe", "demo", "HEAD", true},
		{"/zoe/demo.git/config", "zoe", "demo", "config", false},
		{"/zoe/demo/info/refs", "", "", "", false},
		{"/zoe", "", "", "", false},
	} {
		o, r, rest, ok := IsGitPath(tc.path)
		if ok != tc.ok || (ok && (o != tc.owner || r != tc.repo || rest != tc.rest)) {
			t.Errorf("IsGitPath(%q) = %q %q %q %v", tc.path, o, r, rest, ok)
		}
	}
}
