package gitrepo

// Helpers for the security feature: streaming patches to the secret scanner
// and writing single-file commits for dependency update pull requests.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StreamGit runs git in the repository and hands its stdout to consume. When
// consume returns (early or not) git is stopped, so a reader that stops at a
// limit never waits for the rest. The whole call is bounded by timeout.
func (r *Repo) StreamGit(ctx context.Context, timeout time.Duration, consume func(io.Reader) error, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.s.Git, args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	stderr := &capWriter{n: 4096}
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	er := &eofReader{r: out}
	cerr := consume(er)
	if !er.eof {
		cancel() // consume stopped early (a limit): stop git rather than read the rest
	}
	werr := cmd.Wait()
	if cerr != nil {
		return cerr
	}
	if werr != nil && er.eof {
		if ctx.Err() != nil {
			return ctx.Err() // timed out: what was read so far was consumed
		}
		return fmt.Errorf("git %s: %w: %s", args[0], werr, strings.TrimSpace(stderr.buf.String()))
	}
	return nil
}

type eofReader struct {
	r   io.Reader
	eof bool
}

func (e *eofReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		e.eof = true
	}
	return n, err
}

// FileCommit describes a commit that replaces one file's content on top of Parent.
type FileCommit struct {
	Parent    string // commit to build on
	Path      string
	Content   []byte
	Message   string
	Author    Signature
	Committer Signature // defaults to Author
}

// CommitFile writes a commit (not attached to any branch) that changes one
// file, using a temporary index so nothing else in the repository moves.
func (r *Repo) CommitFile(ctx context.Context, fc FileCommit) (string, error) {
	p, ok := cleanPath(fc.Path)
	if !ok || p == "" {
		return "", errors.New("invalid file path")
	}
	if fc.Committer.Name == "" {
		fc.Committer = fc.Author
	}
	tmp, err := os.MkdirTemp("", "atelier-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}

	if _, err := r.prGit(ctx, env, nil, nil, "read-tree", fc.Parent+"^{tree}"); err != nil {
		return "", err
	}
	mode := "100644"
	if out, _, err := r.prOut(ctx, "ls-tree", fc.Parent, "--", p); err == nil && strings.HasPrefix(out, "100755 ") {
		mode = "100755"
	}
	var blob bytes.Buffer
	if _, err := r.prGit(ctx, nil, bytes.NewReader(fc.Content), &blob, "hash-object", "-w", "--stdin"); err != nil {
		return "", err
	}
	if _, err := r.prGit(ctx, env, nil, nil, "update-index", "--add", "--cacheinfo", mode+","+strings.TrimSpace(blob.String())+","+p); err != nil {
		return "", err
	}
	var tree bytes.Buffer
	if _, err := r.prGit(ctx, env, nil, &tree, "write-tree"); err != nil {
		return "", err
	}
	var out bytes.Buffer
	msg := strings.TrimSpace(strings.ReplaceAll(fc.Message, "\r\n", "\n")) + "\n"
	cenv := append(fc.Author.env("GIT_AUTHOR"), fc.Committer.env("GIT_COMMITTER")...)
	if _, err := r.prGit(ctx, cenv, strings.NewReader(msg), &out, "commit-tree", strings.TrimSpace(tree.String()), "-p", fc.Parent, "-F", "-"); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// ErrBranchExists is returned by CreateBranch when the branch is already there.
var ErrBranchExists = errors.New("the branch already exists")

// CreateBranch points a new branch at sha. It fails with ErrBranchExists
// rather than moving an existing branch.
func (r *Repo) CreateBranch(ctx context.Context, branch, sha string, who Signature) error {
	if !ValidBranchName(branch) {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	if _, err := r.BranchSHA(ctx, branch); err == nil {
		return ErrBranchExists
	}
	zero := strings.Repeat("0", len(sha))
	if err := r.casRef(ctx, "refs/heads/"+branch, sha, zero, "atelier: create branch", who); err != nil {
		if errors.Is(err, ErrRefMoved) {
			return ErrBranchExists
		}
		return err
	}
	return nil
}
