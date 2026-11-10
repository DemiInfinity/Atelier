package gitrepo

// Comparing branches for pull requests: merge-base, the commits on head that
// base doesn't have, and the diff split per file with line numbers so notes
// can be pinned to a line.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrUnrelated means two commits share no history.
var ErrUnrelated = errors.New("the branches have no common history")

// prGit runs git in the repository with extra environment and optional stdin.
// It returns stdout and git's exit code; err is set for any non-zero exit.
func (r *Repo) prGit(ctx context.Context, env []string, stdin io.Reader, stdout io.Writer, args ...string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.s.Git, args...)
	cmd.Dir = r.dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"), env...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	return code, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
}

func (r *Repo) prOut(ctx context.Context, args ...string) (string, int, error) {
	var out bytes.Buffer
	code, err := r.prGit(ctx, nil, nil, &out, args...)
	return strings.TrimSpace(out.String()), code, err
}

// BranchSHA returns the commit a branch points at, or ErrNotFound.
func (r *Repo) BranchSHA(ctx context.Context, branch string) (string, error) {
	if !ValidBranchName(branch) {
		return "", ErrNotFound
	}
	out, _, err := r.prOut(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil || out == "" {
		return "", ErrNotFound
	}
	return out, nil
}

// ValidBranchName is a conservative check that a branch name is safe to put
// in a ref and on a git command line.
func ValidBranchName(b string) bool {
	if b == "" || len(b) > 250 || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") ||
		strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") || strings.Contains(b, "..") ||
		strings.Contains(b, "//") || strings.Contains(b, "@{") || b == "@" {
		return false
	}
	for _, c := range b {
		if c <= ' ' || c == 0x7f || strings.ContainsRune("~^:?*[\\", c) {
			return false
		}
	}
	return true
}

// MergeBase returns the best common ancestor of two commits.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	out, code, err := r.prOut(ctx, "merge-base", a, b)
	if code == 1 {
		return "", ErrUnrelated
	}
	if err != nil {
		return "", err
	}
	return out, nil
}

// IsAncestor reports whether commit a is reachable from commit b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, code, err := r.prOut(ctx, "merge-base", "--is-ancestor", a, b)
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

// CommitsBetween lists commits reachable from head but not from base, oldest
// first, at most limit of them (the newest ones when there are more).
func (r *Repo) CommitsBetween(ctx context.Context, base, head string, limit int) ([]Commit, error) {
	var out bytes.Buffer
	if _, err := r.prGit(ctx, nil, nil, &out, "log", "--format="+logFormat, "-n", strconv.Itoa(limit), base+".."+head, "--"); err != nil {
		return nil, err
	}
	cs := parseCommits(out.Bytes())
	for i, j := 0, len(cs)-1; i < j; i, j = i+1, j-1 {
		cs[i], cs[j] = cs[j], cs[i]
	}
	return cs, nil
}

// AheadBehind counts the commits head has that base doesn't (ahead) and
// the other way round (behind).
func (r *Repo) AheadBehind(ctx context.Context, base, head string) (ahead, behind int, err error) {
	out, _, err := r.prOut(ctx, "rev-list", "--left-right", "--count", base+"..."+head, "--")
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("rev-list: unexpected output %q", out)
	}
	behind, _ = strconv.Atoi(f[0])
	ahead, _ = strconv.Atoi(f[1])
	return ahead, behind, nil
}

// ---------- diffs ----------

// DiffLine is one rendered line of a file diff.
type DiffLine struct {
	Kind string // "hunk", "ctx", "add", "del", "meta"
	Old  int    // line number on the old side (0 if none)
	New  int    // line number on the new side (0 if none)
	Text string // the line without its +/-/space marker (hunk and meta lines are kept whole)
}

// DiffFile is the diff of one file.
type DiffFile struct {
	Path      string // new path (old path for deletions)
	OldPath   string // set for renames and copies
	Status    string // added, deleted, modified, renamed
	Additions int
	Deletions int
	Binary    bool
	Truncated bool // lines were cut to keep the page fast
	Lines     []DiffLine
}

// Diff is a whole comparison.
type Diff struct {
	Files     []DiffFile
	Additions int
	Deletions int
	Truncated bool
}

const (
	maxDiffBytes     = 4 << 20
	maxDiffFileLines = 4000
)

// capWriter keeps the first n bytes and silently discards the rest.
type capWriter struct {
	buf  bytes.Buffer
	n    int
	over bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.n - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.over = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.over = true
	}
	return len(p), nil
}

// DiffCommits diffs two commits (from → to) with rename detection.
func (r *Repo) DiffCommits(ctx context.Context, from, to string) (*Diff, error) {
	var num bytes.Buffer
	if _, err := r.prGit(ctx, nil, nil, &num, "-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff",
		"--find-renames", "--numstat", "-z", from, to, "--"); err != nil {
		return nil, err
	}
	files := parseNumstatZ(num.Bytes())

	patch := &capWriter{n: maxDiffBytes}
	if _, err := r.prGit(ctx, nil, nil, patch, "-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff",
		"--find-renames", "--patch", from, to, "--"); err != nil {
		return nil, err
	}
	chunks := splitPatch(patch.buf.String())
	d := &Diff{Truncated: patch.over}
	for i := range files {
		f := &files[i]
		if i < len(chunks) {
			fillFile(f, chunks[i])
		} else {
			f.Truncated = !f.Binary
		}
		if f.Truncated {
			d.Truncated = true
		}
		d.Additions += f.Additions
		d.Deletions += f.Deletions
	}
	d.Files = files
	return d, nil
}

// parseNumstatZ reads `git diff --numstat -z`. Normal records are
// "add\tdel\tpath\0"; renames are "add\tdel\t\0old\0new\0"; binaries use "-".
func parseNumstatZ(b []byte) []DiffFile {
	var files []DiffFile
	parts := strings.Split(string(b), "\x00")
	for i := 0; i < len(parts); i++ {
		rec := parts[i]
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\t", 3)
		if len(f) < 3 {
			continue
		}
		df := DiffFile{Status: "modified"}
		if f[0] == "-" && f[1] == "-" {
			df.Binary = true
		} else {
			df.Additions, _ = strconv.Atoi(f[0])
			df.Deletions, _ = strconv.Atoi(f[1])
		}
		if f[2] == "" && i+2 < len(parts) {
			df.OldPath, df.Path = parts[i+1], parts[i+2]
			df.Status = "renamed"
			i += 2
		} else {
			df.Path = f[2]
		}
		files = append(files, df)
	}
	return files
}

// splitPatch cuts a patch into one chunk per "diff --git" header.
func splitPatch(p string) []string {
	var chunks []string
	start := -1
	for i := 0; i < len(p); {
		end := strings.IndexByte(p[i:], '\n')
		if end < 0 {
			end = len(p) - i
		}
		if strings.HasPrefix(p[i:], "diff --git ") {
			if start >= 0 {
				chunks = append(chunks, p[start:i])
			}
			start = i
		}
		i += end + 1
	}
	if start >= 0 {
		chunks = append(chunks, p[start:])
	}
	return chunks
}

func fillFile(f *DiffFile, chunk string) {
	lines := strings.Split(strings.TrimSuffix(chunk, "\n"), "\n")
	var oldN, newN int
	inHunk := false
	for _, l := range lines[1:] { // skip "diff --git"
		if !inHunk {
			switch {
			case strings.HasPrefix(l, "new file mode"):
				f.Status = "added"
			case strings.HasPrefix(l, "deleted file mode"):
				f.Status = "deleted"
			case strings.HasPrefix(l, "Binary files"):
				f.Binary = true
			}
		}
		if strings.HasPrefix(l, "@@") {
			inHunk = true
			oldN, newN = parseHunk(l)
			f.Lines = append(f.Lines, DiffLine{Kind: "hunk", Text: l})
			continue
		}
		if !inHunk {
			continue
		}
		if len(f.Lines) >= maxDiffFileLines {
			f.Truncated = true
			break
		}
		switch {
		case strings.HasPrefix(l, "+"):
			f.Lines = append(f.Lines, DiffLine{Kind: "add", New: newN, Text: l[1:]})
			newN++
		case strings.HasPrefix(l, "-"):
			f.Lines = append(f.Lines, DiffLine{Kind: "del", Old: oldN, Text: l[1:]})
			oldN++
		case strings.HasPrefix(l, " "), l == "":
			text := l
			if l != "" {
				text = l[1:]
			}
			f.Lines = append(f.Lines, DiffLine{Kind: "ctx", Old: oldN, New: newN, Text: text})
			oldN++
			newN++
		case strings.HasPrefix(l, `\`):
			f.Lines = append(f.Lines, DiffLine{Kind: "meta", Text: l})
		}
	}
}

// parseHunk reads the starting line numbers from "@@ -a,b +c,d @@".
func parseHunk(h string) (oldStart, newStart int) {
	f := strings.Fields(h)
	if len(f) < 3 {
		return 1, 1
	}
	num := func(s string) int {
		s = strings.TrimLeft(s, "-+")
		if i := strings.IndexByte(s, ','); i >= 0 {
			s = s[:i]
		}
		n, _ := strconv.Atoi(s)
		return n
	}
	return num(f[1]), num(f[2])
}

// ---------- the whole comparison ----------

// Comparison is what a pull request (or the compare page) shows.
type Comparison struct {
	Base, Head   string // resolved commit hashes
	MergeBase    string
	Commits      []Commit // oldest first
	Ahead        int
	Behind       int
	Diff         *Diff
	Unrelated    bool
	CommitsShown int
}

const maxCompareCommits = 250

// Compare compares two commits the way a pull request does: the commits on
// head since it forked from base, and the diff from the merge base to head.
func (r *Repo) Compare(ctx context.Context, base, head string) (*Comparison, error) {
	c := &Comparison{Base: base, Head: head}
	mb, err := r.MergeBase(ctx, base, head)
	if errors.Is(err, ErrUnrelated) {
		c.Unrelated = true
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	c.MergeBase = mb
	if c.Ahead, c.Behind, err = r.AheadBehind(ctx, base, head); err != nil {
		return nil, err
	}
	if c.Commits, err = r.CommitsBetween(ctx, base, head, maxCompareCommits); err != nil {
		return nil, err
	}
	c.CommitsShown = len(c.Commits)
	if c.Diff, err = r.DiffCommits(ctx, mb, head); err != nil {
		return nil, err
	}
	return c, nil
}
