package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func editTree(t *testing.T, r *Repo, commit string) map[string]string {
	t.Helper()
	out := git(t, r.dir, "ls-tree", "-r", "--name-only", commit)
	files := map[string]string{}
	for _, p := range strings.Split(out, "\n") {
		if p == "" {
			continue
		}
		files[p] = git(t, r.dir, "show", commit+":"+p)
	}
	return files
}

func editStage(t *testing.T, r *Repo, e Edit) *StagedEdit {
	t.Helper()
	if e.Message == "" {
		e.Message = "Change"
	}
	e.Author = prOwnerSig
	s, err := r.StageEdit(context.Background(), e)
	if err != nil {
		t.Fatalf("StageEdit: %v", err)
	}
	return s
}

func TestStageEditChanges(t *testing.T) {
	_, r, _ := prRepo(t)
	ctx := context.Background()
	main, _ := r.BranchSHA(ctx, "main")

	// Edit, create (with new folders), delete and rename in one commit.
	s := editStage(t, r, Edit{Parent: main, Message: "Several things\n\nWith a body.", Changes: []FileChange{
		{Path: "README.md", Content: []byte("# Demo v2\n")},
		{Path: "docs/guide/intro.md", Content: []byte("Hello\n"), MustNotExist: true},
		{Path: "app.txt", From: "", Delete: true},
	}})
	defer s.Discard()
	// Not visible before Publish: the commit is quarantined.
	if _, _, err := r.prOut(ctx, "cat-file", "-e", s.SHA); err == nil {
		t.Fatal("staged commit is already in the object store")
	}
	if err := s.Publish(ctx, "main", main, prOwnerSig); err != nil {
		t.Fatal(err)
	}
	got := editTree(t, r, "main")
	want := map[string]string{"README.md": "# Demo v2", "docs/guide/intro.md": "Hello"}
	if len(got) != len(want) || got["README.md"] != want["README.md"] || got["docs/guide/intro.md"] != want["docs/guide/intro.md"] {
		t.Fatalf("tree = %v", got)
	}
	msg := git(t, r.dir, "log", "-1", "--format=%an <%ae>%n%B", "main")
	if !strings.Contains(msg, "Zoë <zoe@atelier.local>") || !strings.Contains(msg, "Several things\n\nWith a body.") {
		t.Errorf("commit = %q", msg)
	}
	if p := git(t, r.dir, "rev-parse", "main^"); p != main {
		t.Errorf("parent = %s, want %s", p, main)
	}

	// Rename keeping the content (binary-safe), then rename with new content.
	tip, _ := r.BranchSHA(ctx, "main")
	s2 := editStage(t, r, Edit{Parent: tip, Changes: []FileChange{{From: "docs/guide/intro.md", Path: "intro.md", KeepContent: true}}})
	if err := s2.Publish(ctx, "main", tip, prOwnerSig); err != nil {
		t.Fatal(err)
	}
	got = editTree(t, r, "main")
	if got["intro.md"] != "Hello" || len(got) != 2 {
		t.Fatalf("after rename: %v", got)
	}

	// Stale CAS: publishing on an old tip fails and leaves main alone.
	now, _ := r.BranchSHA(ctx, "main")
	s3 := editStage(t, r, Edit{Parent: tip, Changes: []FileChange{{Path: "x.txt", Content: []byte("x")}}})
	if err := s3.Publish(ctx, "main", tip, prOwnerSig); !errors.Is(err, ErrRefMoved) {
		t.Fatalf("stale publish = %v", err)
	}
	if after, _ := r.BranchSHA(ctx, "main"); after != now {
		t.Fatal("stale publish moved main")
	}
}

func TestStageEditRefusals(t *testing.T) {
	_, r, _ := prRepo(t)
	ctx := context.Background()
	main, _ := r.BranchSHA(ctx, "main")
	try := func(c FileChange) error {
		s, err := r.StageEdit(ctx, Edit{Parent: main, Message: "m", Author: prOwnerSig, Changes: []FileChange{c}})
		if err == nil {
			s.Discard()
		}
		return err
	}
	if err := try(FileChange{Path: "README.md", Content: []byte("x"), MustNotExist: true}); !errors.Is(err, ErrPathConflict) {
		t.Errorf("create over existing = %v", err)
	}
	if err := try(FileChange{Path: "README.md/inner", Content: []byte("x")}); !errors.Is(err, ErrPathConflict) {
		t.Errorf("file under a file = %v", err)
	}
	if err := try(FileChange{Path: "nope.txt", Delete: true}); !errors.Is(err, ErrPathMissing) {
		t.Errorf("delete missing = %v", err)
	}
	if err := try(FileChange{From: "app.txt", Path: "README.md", KeepContent: true}); !errors.Is(err, ErrPathConflict) {
		t.Errorf("rename over existing = %v", err)
	}
	if err := try(FileChange{Path: "README.md", Content: []byte("# Demo\n")}); !errors.Is(err, ErrNoChanges) {
		t.Errorf("no-op = %v", err)
	}
	for _, p := range []string{"../x", "a/../b", ".git/config", "a/.GIT/x", "/abs", "a//b", "a/", " lead", "a\x01b", "a\\b"} {
		if ValidEditPath(p) {
			t.Errorf("ValidEditPath(%q) = true", p)
		}
		if err := try(FileChange{Path: p, Content: []byte("x")}); err == nil {
			t.Errorf("path %q accepted", p)
		}
	}
	for _, p := range []string{"a.txt", "docs/x y.md", ".github/workflows/ci.yml", "ünï/çødé.go"} {
		if !ValidEditPath(p) {
			t.Errorf("ValidEditPath(%q) = false", p)
		}
	}
}

func TestStageEditQuarantine(t *testing.T) {
	_, r, _ := prRepo(t)
	ctx := context.Background()
	main, _ := r.BranchSHA(ctx, "main")
	s := editStage(t, r, Edit{Parent: main, Changes: []FileChange{{Path: "secret.txt", Content: []byte("quarantined content 42\n")}}})
	q := s.quarantine
	if _, err := os.Stat(q); err != nil {
		t.Fatal(err)
	}
	// A hook sees the staged objects: run one that inspects the commit.
	hook := filepath.Join(r.dir, "hooks", "pre-receive")
	_ = os.MkdirAll(filepath.Dir(hook), 0o755)
	script := "#!/bin/sh\nread old new ref\ngit cat-file -e \"$new\" || exit 3\necho \"atelier: nope for $REMOTE_USER on $ref\" >&2\nexit 1\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ran, err := s.RunPreReceive(ctx, []RefUpdate{{Old: main, New: s.SHA, Ref: "refs/heads/main"}}, "owner:zoe")
	var hr *HookRejected
	if !ran || !errors.As(err, &hr) || len(hr.Messages) != 1 || hr.Messages[0] != "nope for owner:zoe on refs/heads/main" {
		t.Fatalf("hook: ran=%v err=%v", ran, err)
	}
	s.Discard()
	if _, err := os.Stat(q); !os.IsNotExist(err) {
		t.Error("quarantine left behind")
	}
	if _, _, err := r.prOut(ctx, "cat-file", "-e", s.SHA); err == nil {
		t.Error("a rejected commit reached the object store")
	}
	_ = os.Remove(hook)
	if ran, err := s.RunPreReceive(ctx, nil, ""); ran || err != nil {
		t.Errorf("no hook: ran=%v err=%v", ran, err)
	}
}

func TestStageEditEmptyRepo(t *testing.T) {
	gitEnv(t)
	ctx := context.Background()
	s, err := New(filepath.Join(t.TempDir(), "repos"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Init(ctx, "zoe", "empty"); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Open("zoe", "empty")
	st := editStage(t, r, Edit{Changes: []FileChange{{Path: "README.md", Content: []byte("# Hi\n"), MustNotExist: true}}})
	if err := st.Publish(ctx, "main", "", prOwnerSig); err != nil {
		t.Fatal(err)
	}
	if got := editTree(t, r, "main"); got["README.md"] != "# Hi" {
		t.Fatalf("tree = %v", got)
	}
	// Creating it again is refused rather than overwriting.
	st2 := editStage(t, r, Edit{Changes: []FileChange{{Path: "other.md", Content: []byte("x")}}})
	if err := st2.Publish(ctx, "main", "", prOwnerSig); !errors.Is(err, ErrBranchExists) {
		t.Fatalf("second root commit = %v", err)
	}
}
