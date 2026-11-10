package gitrepo

// Helpers for releases: tags, commit ranges for release notes, and source
// archives.

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
)

// ErrTagExists is returned by CreateTag when the tag is already there.
var ErrTagExists = errors.New("tag already exists")

// ValidTagName reports whether s is usable as a tag name (git's own rules,
// plus no leading dash and nothing that would confuse URLs).
func (s *Store) ValidTagName(ctx context.Context, tag string) bool {
	if tag == "" || len(tag) > 200 || strings.HasPrefix(tag, "-") || strings.ContainsAny(tag, " \t\n?#%\\") {
		return false
	}
	_, err := s.run(ctx, s.Root, "check-ref-format", "refs/tags/"+tag)
	return err == nil
}

// TagCommit returns the commit a tag points at.
func (r *Repo) TagCommit(ctx context.Context, tag string) (string, bool) {
	if tag == "" || strings.HasPrefix(tag, "-") {
		return "", false
	}
	out, err := r.run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// CreateTag makes a lightweight tag pointing at commit. It fails with
// ErrTagExists rather than moving an existing tag.
func (r *Repo) CreateTag(ctx context.Context, tag, commit string) error {
	if !r.s.ValidTagName(ctx, tag) {
		return fmt.Errorf("invalid tag name %q", tag)
	}
	if _, ok := r.TagCommit(ctx, tag); ok {
		return ErrTagExists
	}
	sha, err := r.ResolveCommit(ctx, commit)
	if err != nil {
		return err
	}
	// The empty old value makes update-ref refuse if the ref appeared meanwhile.
	if _, err := r.run(ctx, "update-ref", "refs/tags/"+tag, sha, ""); err != nil {
		if _, ok := r.TagCommit(ctx, tag); ok {
			return ErrTagExists
		}
		return err
	}
	return nil
}

// RangeCommits lists commits in from..to along the first-parent chain,
// newest first, leaving out merge commits. from may be empty (everything
// reachable from to). At most limit commits are returned.
func (r *Repo) RangeCommits(ctx context.Context, from, to string, limit int) ([]Commit, error) {
	rng := to
	if from != "" {
		rng = from + ".." + to
	}
	out, err := r.run(ctx, "log", "--first-parent", "--no-merges", "--format="+logFormat, "-n", strconv.Itoa(limit), rng, "--")
	if err != nil {
		return nil, err
	}
	return parseCommits(out), nil
}

// RangeSHAs lists every commit hash in from..to (from may be empty).
func (r *Repo) RangeSHAs(ctx context.Context, from, to string) (map[string]bool, error) {
	args := []string{"rev-list", "--max-count=5000", to}
	if from != "" {
		args = append(args, "^"+from)
	}
	out, err := r.run(ctx, append(args, "--")...)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, l := range strings.Fields(string(out)) {
		set[l] = true
	}
	return set, nil
}

// Archive streams `git archive` of commit in format "zip" or "tar.gz" to w,
// with every path under prefix/.
func (r *Repo) Archive(ctx context.Context, w io.Writer, commit, format, prefix string) error {
	if format != "zip" && format != "tar.gz" {
		return fmt.Errorf("unknown archive format %q", format)
	}
	cmd := exec.CommandContext(ctx, r.s.Git, "archive", "--format="+format, "--prefix="+prefix+"/", commit)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git archive: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
