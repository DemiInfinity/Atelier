package gitrepo

// Commits made in the browser (the web editor): several file changes at once
// (edit, create, upload, delete, rename), built without a working tree.
//
// A web commit goes through the same gate as a push:
//
//  1. StageEdit writes the new blobs, tree and commit into a quarantine object
//     directory (like git receive-pack does), so nothing lands in the
//     repository's object store until the checks pass.
//  2. RunPreReceive runs the repository's own pre-receive hook (the
//     `atelier hook pre-receive` binary: branch rules, secret scanning push
//     protection, mirrors, Tether…) with the proposed ref update on stdin and
//     the quarantine visible, exactly as git does for a push.
//  3. Publish moves the objects into the repository and moves the branch with
//     compare-and-swap, so a push that landed meanwhile is never overwritten.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

var (
	// ErrNoChanges means the edit leaves the tree exactly as it was.
	ErrNoChanges = errors.New("these changes don't change any file")
	// ErrPathMissing means a file to change, delete or rename isn't there.
	ErrPathMissing = errors.New("the file doesn't exist")
	// ErrPathConflict means a new path clashes with an existing file or folder.
	ErrPathConflict = errors.New("a file or folder is already at that path")
)

// FileChange is one change in a web commit.
//
//   - Edit or create: Path and Content.
//   - Delete: Path and Delete.
//   - Rename / move: From (the old path) and Path; Content replaces the
//     content, or KeepContent keeps the old blob (binary files).
type FileChange struct {
	Path        string
	From        string
	Content     []byte
	KeepContent bool
	Delete      bool
	// MustNotExist refuses the change if Path is already a file (new files).
	MustNotExist bool
}

// Edit is a web commit: the changes on top of Parent ("" for the first
// commit of an empty repository).
type Edit struct {
	Parent    string
	Changes   []FileChange
	Message   string
	Author    Signature
	Committer Signature // defaults to Author
}

// ValidEditPath reports whether p is a sensible repository path for a file:
// relative, no empty, "." or ".." segments, nothing inside .git, no control
// characters, and not too long.
func ValidEditPath(p string) bool {
	if p == "" || len(p) > 4096 || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.HasPrefix(p, `"`) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return false
		case strings.EqualFold(seg, ".git"):
			return false
		case len(seg) > 255:
			return false
		case strings.TrimSpace(seg) != seg:
			return false
		}
		for _, c := range seg {
			if c < 0x20 || c == 0x7f || c == '\\' || c == unicode.ReplacementChar {
				return false
			}
		}
	}
	return true
}

// StagedEdit is a commit written to a quarantine directory, not yet visible
// in the repository.
type StagedEdit struct {
	r          *Repo
	SHA        string
	Tree       string
	quarantine string
}

func (r *Repo) objectsDir() string { return filepath.Join(r.dir, "objects") }

// qenv is the environment that makes git read the repository's objects and
// write new ones into the quarantine.
func (s *StagedEdit) qenv() []string {
	abs, _ := filepath.Abs(s.r.objectsDir())
	return []string{
		"GIT_OBJECT_DIRECTORY=" + s.quarantine,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + abs,
		"GIT_QUARANTINE_PATH=" + s.quarantine,
	}
}

type treeEntry struct{ mode, sha string }

// lsEntry returns the mode and blob of a file at commit:path ("" if none).
func (r *Repo) lsEntry(ctx context.Context, env []string, commit, p string) (treeEntry, string, error) {
	if commit == "" {
		return treeEntry{}, "", nil
	}
	var out bytes.Buffer
	if _, err := r.prGit(ctx, env, nil, &out, "ls-tree", "-z", commit, "--", p); err != nil {
		return treeEntry{}, "", err
	}
	rec := strings.TrimSuffix(out.String(), "\x00")
	meta, name, ok := strings.Cut(rec, "\t")
	if !ok || name != p {
		return treeEntry{}, "", nil
	}
	f := strings.Fields(meta)
	if len(f) < 3 {
		return treeEntry{}, "", nil
	}
	return treeEntry{mode: f[0], sha: f[2]}, f[1], nil
}

// StageEdit writes the commit for e into a quarantine. Call Publish to make it
// real or Discard to throw it away (always call one of them).
func (r *Repo) StageEdit(ctx context.Context, e Edit) (*StagedEdit, error) {
	if len(e.Changes) == 0 {
		return nil, ErrNoChanges
	}
	if strings.TrimSpace(e.Message) == "" {
		return nil, errors.New("a commit needs a message")
	}
	if e.Committer.Name == "" {
		e.Committer = e.Author
	}
	if err := os.MkdirAll(r.objectsDir(), 0o755); err != nil {
		return nil, err
	}
	q, err := os.MkdirTemp(r.objectsDir(), "incoming-edit-")
	if err != nil {
		return nil, err
	}
	s := &StagedEdit{r: r, quarantine: q}
	ok := false
	defer func() {
		if !ok {
			s.Discard()
		}
	}()

	idx, err := os.MkdirTemp("", "atelier-edit-index-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(idx)
	env := append(s.qenv(), "GIT_INDEX_FILE="+filepath.Join(idx, "index"))
	if e.Parent != "" {
		if _, err := r.prGit(ctx, env, nil, nil, "read-tree", e.Parent+"^{tree}"); err != nil {
			return nil, err
		}
	} else if _, err := r.prGit(ctx, env, nil, nil, "read-tree", "--empty"); err != nil {
		return nil, err
	}

	for _, c := range e.Changes {
		if err := s.apply(ctx, env, e.Parent, c); err != nil {
			return nil, err
		}
	}
	var tree bytes.Buffer
	if _, err := r.prGit(ctx, env, nil, &tree, "write-tree"); err != nil {
		return nil, err
	}
	s.Tree = strings.TrimSpace(tree.String())
	if e.Parent != "" {
		if old, _, err := r.prOut(ctx, "rev-parse", e.Parent+"^{tree}"); err == nil && old == s.Tree {
			return nil, ErrNoChanges
		}
	}
	args := []string{"commit-tree", s.Tree}
	if e.Parent != "" {
		args = append(args, "-p", e.Parent)
	}
	args = append(args, "-F", "-")
	msg := strings.TrimSpace(strings.ReplaceAll(e.Message, "\r\n", "\n")) + "\n"
	cenv := append(append(append([]string{}, s.qenv()...), e.Author.env("GIT_AUTHOR")...), e.Committer.env("GIT_COMMITTER")...)
	var out bytes.Buffer
	if _, err := r.prGit(ctx, cenv, strings.NewReader(msg), &out, args...); err != nil {
		return nil, err
	}
	s.SHA = strings.TrimSpace(out.String())
	ok = true
	return s, nil
}

func (s *StagedEdit) apply(ctx context.Context, env []string, parent string, c FileChange) error {
	r := s.r
	if !ValidEditPath(c.Path) || (c.From != "" && !ValidEditPath(c.From)) {
		return fmt.Errorf("invalid file path %q", c.Path)
	}
	switch {
	case c.Delete:
		cur, kind, err := r.lsEntry(ctx, env, parent, c.Path)
		if err != nil {
			return err
		}
		if kind != "blob" || cur.sha == "" {
			return fmt.Errorf("%s: %w", c.Path, ErrPathMissing)
		}
		return r.indexRemove(ctx, env, c.Path)
	}

	mode := "100644"
	var blob string
	if c.From != "" && c.From != c.Path {
		old, kind, err := r.lsEntry(ctx, env, parent, c.From)
		if err != nil {
			return err
		}
		if kind != "blob" || old.sha == "" {
			return fmt.Errorf("%s: %w", c.From, ErrPathMissing)
		}
		mode = old.mode
		if c.KeepContent {
			blob = old.sha
		}
		if err := r.indexRemove(ctx, env, c.From); err != nil {
			return err
		}
		c.MustNotExist = true // a rename never overwrites another file
	} else {
		cur, kind, err := r.lsEntry(ctx, env, parent, c.Path)
		if err != nil {
			return err
		}
		if kind == "tree" {
			return fmt.Errorf("%s: %w", c.Path, ErrPathConflict)
		}
		if cur.sha != "" {
			if c.MustNotExist {
				return fmt.Errorf("%s: %w", c.Path, ErrPathConflict)
			}
			mode = cur.mode
			if c.KeepContent {
				blob = cur.sha
			}
		}
	}
	if mode == "120000" && !c.KeepContent {
		mode = "100644" // editing a symlink's target text makes it a file
	}
	if blob == "" {
		var out bytes.Buffer
		if _, err := r.prGit(ctx, s.qenv(), bytes.NewReader(c.Content), &out, "hash-object", "-w", "--stdin"); err != nil {
			return err
		}
		blob = strings.TrimSpace(out.String())
	}
	if c.MustNotExist {
		// The index already reflects earlier changes in this commit.
		var out bytes.Buffer
		if _, err := r.prGit(ctx, env, nil, &out, "ls-files", "-z", "--", c.Path); err == nil && out.Len() > 0 {
			return fmt.Errorf("%s: %w", c.Path, ErrPathConflict)
		}
	}
	// No --replace: git refuses a file where a folder is (or the reverse).
	if _, err := r.prGit(ctx, env, nil, nil, "update-index", "--add", "--cacheinfo", mode+","+blob+","+c.Path); err != nil {
		if strings.Contains(err.Error(), "cannot add to the index") || strings.Contains(err.Error(), "appears as both") {
			return fmt.Errorf("%s: %w", c.Path, ErrPathConflict)
		}
		return err
	}
	return nil
}

// indexRemove drops a path from the temporary index. (--force-remove needs
// a work tree; --index-info with mode 0 doesn't.)
func (r *Repo) indexRemove(ctx context.Context, env []string, p string) error {
	_, err := r.prGit(ctx, env, strings.NewReader("0 "+ZeroSHA+"\t"+p+"\n"), nil, "update-index", "--index-info")
	return err
}

// Discard removes the quarantine (and with it every object of the edit).
func (s *StagedEdit) Discard() {
	if s != nil && s.quarantine != "" {
		_ = os.RemoveAll(s.quarantine)
		s.quarantine = ""
	}
}

// RefUpdate is one line of pre-receive input. Old or New is all zeros for a
// created or deleted ref.
type RefUpdate struct{ Old, New, Ref string }

// ZeroSHA is the all-zeros object name for created and deleted refs.
const ZeroSHA = "0000000000000000000000000000000000000000"

// HookRejected is returned when the pre-receive hook refuses the update.
type HookRejected struct {
	Messages []string // the hook's output, one line each, without the "atelier: " prefix
}

func (e *HookRejected) Error() string {
	return "rejected by the pre-receive hook: " + strings.Join(e.Messages, " ")
}

// HasPreReceiveHook reports whether the repository has an executable
// pre-receive hook.
func (r *Repo) HasPreReceiveHook() bool {
	fi, err := os.Stat(filepath.Join(r.dir, "hooks", "pre-receive"))
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

// RunPreReceive runs the repository's pre-receive hook for the updates, with
// the staged objects visible, as git receive-pack would. pusher is
// "<actor kind>:<name>" (REMOTE_USER for the hook). It reports ran=false when
// the repository has no hook.
func (s *StagedEdit) RunPreReceive(ctx context.Context, updates []RefUpdate, pusher string) (ran bool, err error) {
	if !s.r.HasPreReceiveHook() {
		return false, nil
	}
	var in strings.Builder
	for _, u := range updates {
		fmt.Fprintf(&in, "%s %s %s\n", u.Old, u.New, u.Ref)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(s.r.dir, "hooks", "pre-receive"))
	cmd.Dir = s.r.dir
	cmd.Env = append(append(os.Environ(), "GIT_DIR=.", "REMOTE_USER="+pusher, "LC_ALL=C"), s.qenv()...)
	cmd.Stdin = strings.NewReader(in.String())
	out := &capWriter{n: 64 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return true, err
		}
		var msgs []string
		for _, l := range strings.Split(out.buf.String(), "\n") {
			l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "atelier:"))
			if l != "" {
				msgs = append(msgs, l)
			}
		}
		if len(msgs) == 0 {
			msgs = []string{"the pre-receive hook declined the change"}
		}
		return true, &HookRejected{Messages: msgs}
	}
	return true, nil
}

// Publish moves the staged objects into the repository and points branch at
// the new commit, if the branch is still at old ("" or ZeroSHA creates the
// branch and fails with ErrBranchExists if it's there).
func (s *StagedEdit) Publish(ctx context.Context, branch, old string, who Signature) error {
	if !ValidBranchName(branch) {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	if err := s.migrate(); err != nil {
		return err
	}
	if old == "" {
		old = ZeroSHA
	}
	if err := s.r.casRef(ctx, "refs/heads/"+branch, s.SHA, old, "atelier: web edit", who); err != nil {
		if old == ZeroSHA && errors.Is(err, ErrRefMoved) {
			return ErrBranchExists
		}
		return err
	}
	return nil
}

// migrate moves the quarantined loose objects into the object store.
func (s *StagedEdit) migrate() error {
	if s.quarantine == "" {
		return errors.New("edit already discarded")
	}
	dst := s.r.objectsDir()
	err := filepath.WalkDir(s.quarantine, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(s.quarantine, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if _, err := os.Stat(target); err == nil {
			return nil // already there
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.Rename(p, target)
	})
	if err != nil {
		return err
	}
	s.Discard()
	return nil
}

// ChangedPaths returns which of paths differ between two commits.
func (r *Repo) ChangedPaths(ctx context.Context, from, to string, paths []string) ([]string, error) {
	if from == to || len(paths) == 0 {
		return nil, nil
	}
	args := append([]string{"diff", "--name-only", "-z", "--no-renames", from, to, "--"}, paths...)
	var out bytes.Buffer
	if _, err := r.prGit(ctx, nil, nil, &out, args...); err != nil {
		return nil, err
	}
	var changed []string
	for _, p := range strings.Split(out.String(), "\x00") {
		if p != "" {
			changed = append(changed, p)
		}
	}
	return changed, nil
}

// PathDiff is the unified diff of paths between two commits, cut at limit bytes.
func (r *Repo) PathDiff(ctx context.Context, from, to string, paths []string, limit int) (string, error) {
	args := append([]string{"diff", "--no-color", "--no-ext-diff", "--no-renames", from, to, "--"}, paths...)
	w := &capWriter{n: limit}
	if _, err := r.prGit(ctx, nil, nil, w, args...); err != nil {
		return "", err
	}
	return w.buf.String(), nil
}

// IsCommit reports whether sha names a commit in the repository.
func (r *Repo) IsCommit(ctx context.Context, sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, c := range sha {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	out, _, err := r.prOut(ctx, "cat-file", "-t", sha)
	return err == nil && out == "commit"
}

// BlobSHA is the object name of the file at commit:path ("" if it isn't a file).
func (r *Repo) BlobSHA(ctx context.Context, commit, p string) string {
	e, kind, err := r.lsEntry(ctx, nil, commit, p)
	if err != nil || kind != "blob" {
		return ""
	}
	return e.sha
}

var _ io.Writer = (*capWriter)(nil)
