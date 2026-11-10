package gitrepo

// Merging pull requests on bare repositories, without a working tree:
// `merge-tree --write-tree` computes the result, `commit-tree` records it and
// `update-ref` moves the branch with compare-and-swap so a push that lands in
// the meantime is never overwritten. update-ref doesn't run receive hooks, so
// Atelier's own merges aren't blocked by branch rules that require PRs.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	// ErrMergeConflict means the branches can't be merged automatically.
	ErrMergeConflict = errors.New("the branches have merge conflicts")
	// ErrRefMoved means a branch changed while Atelier was working (e.g. a push landed).
	ErrRefMoved = errors.New("the branch changed while merging; try again")
	// ErrNothingToMerge means base already contains every commit on head.
	ErrNothingToMerge = errors.New("there is nothing to merge")
)

// MergeCheck is the result of a trial merge.
type MergeCheck struct {
	Clean     bool
	Tree      string   // the merged tree (also written when there are conflicts)
	Conflicts []string // files with conflicts
}

// CheckMerge does a trial merge of head into base (both commits) without
// touching any ref. The resulting tree object is written to the object store.
func (r *Repo) CheckMerge(ctx context.Context, base, head string) (*MergeCheck, error) {
	var out bytes.Buffer
	code, err := r.prGit(ctx, nil, nil, &out, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", base, head)
	if code != 0 && code != 1 {
		if strings.Contains(fmt.Sprint(err), "unrelated histories") {
			return nil, ErrUnrelated
		}
		return nil, err
	}
	parts := strings.Split(out.String(), "\x00")
	mc := &MergeCheck{Clean: code == 0, Tree: strings.TrimSpace(parts[0])}
	if mc.Tree == "" {
		return nil, fmt.Errorf("merge-tree: no tree in output")
	}
	seen := map[string]bool{}
	for _, p := range parts[1:] {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		mc.Conflicts = append(mc.Conflicts, p)
	}
	return mc, nil
}

// Signature is a commit author or committer.
type Signature struct {
	Name, Email string
	When        time.Time
}

func (s Signature) env(prefix string) []string {
	when := s.When
	if when.IsZero() {
		when = time.Now()
	}
	return []string{
		prefix + "_NAME=" + s.Name,
		prefix + "_EMAIL=" + s.Email,
		prefix + "_DATE=" + when.Format(time.RFC3339),
	}
}

// MergeRequest describes a merge of one branch into another.
type MergeRequest struct {
	Base, Head   string // branch names
	ExpectedHead string // if set, the head commit the user reviewed; a newer push aborts the merge
	Method       string // "merge" (two-parent commit) or "squash" (one-parent commit)
	Message      string
	Author       Signature // squash author (defaults to Committer)
	Committer    Signature
}

// MergeResult is what a merge did.
type MergeResult struct {
	SHA        string // the new commit on base
	BaseBefore string // base before the merge
	Head       string // the head commit that was merged
}

// MergeBranches merges Head into Base and moves Base with compare-and-swap.
func (r *Repo) MergeBranches(ctx context.Context, req MergeRequest) (*MergeResult, error) {
	if req.Method != "merge" && req.Method != "squash" {
		return nil, fmt.Errorf("unknown merge method %q", req.Method)
	}
	if strings.TrimSpace(req.Message) == "" {
		return nil, errors.New("a merge needs a commit message")
	}
	baseOld, err := r.BranchSHA(ctx, req.Base)
	if err != nil {
		return nil, fmt.Errorf("base branch %q: %w", req.Base, err)
	}
	head, err := r.BranchSHA(ctx, req.Head)
	if err != nil {
		return nil, fmt.Errorf("head branch %q: %w", req.Head, err)
	}
	if req.ExpectedHead != "" && head != req.ExpectedHead {
		return nil, ErrRefMoved
	}
	if done, err := r.IsAncestor(ctx, head, baseOld); err != nil {
		return nil, err
	} else if done {
		return nil, ErrNothingToMerge
	}
	mc, err := r.CheckMerge(ctx, baseOld, head)
	if err != nil {
		return nil, err
	}
	if !mc.Clean {
		return nil, ErrMergeConflict
	}

	author := req.Author
	if author.Name == "" {
		author = req.Committer
	}
	args := []string{"commit-tree", mc.Tree, "-p", baseOld}
	if req.Method == "merge" {
		args = append(args, "-p", head)
		author = req.Committer
	}
	args = append(args, "-F", "-")
	env := append(author.env("GIT_AUTHOR"), req.Committer.env("GIT_COMMITTER")...)
	var out bytes.Buffer
	msg := strings.TrimSpace(strings.ReplaceAll(req.Message, "\r\n", "\n")) + "\n"
	if _, err := r.prGit(ctx, env, strings.NewReader(msg), &out, args...); err != nil {
		return nil, err
	}
	sha := strings.TrimSpace(out.String())

	if err := r.casRef(ctx, "refs/heads/"+req.Base, sha, baseOld, "atelier: merge "+req.Head, req.Committer); err != nil {
		return nil, err
	}
	return &MergeResult{SHA: sha, BaseBefore: baseOld, Head: head}, nil
}

// casRef moves ref from old to new, failing with ErrRefMoved if it isn't at old.
// An empty newSHA deletes the ref.
func (r *Repo) casRef(ctx context.Context, ref, newSHA, old, reason string, who Signature) error {
	args := []string{"update-ref", "-m", reason, ref, newSHA, old}
	if newSHA == "" {
		args = []string{"update-ref", "-m", reason, "-d", ref, old}
	}
	if _, err := r.prGit(ctx, who.env("GIT_COMMITTER"), nil, nil, args...); err != nil {
		cur, _, _ := r.prOut(ctx, "rev-parse", "--verify", "--quiet", ref)
		if cur != old {
			return ErrRefMoved
		}
		return err
	}
	return nil
}

// DeleteBranch removes a branch if it still points at expected (when given).
func (r *Repo) DeleteBranch(ctx context.Context, branch, expected string, who Signature) error {
	cur, err := r.BranchSHA(ctx, branch)
	if err != nil {
		return err
	}
	if expected != "" && cur != expected {
		return ErrRefMoved
	}
	return r.casRef(ctx, "refs/heads/"+branch, "", cur, "atelier: delete branch", who)
}

// HasMergeCommits reports whether any commit reachable from to but not from
// from (or, when from is empty, not from any existing branch) has more than
// one parent.
func (r *Repo) HasMergeCommits(ctx context.Context, from, to string) (bool, error) {
	args := []string{"rev-list", "--min-parents=2", "--max-count=1", to}
	if from == "" {
		args = append(args, "--not", "--branches")
	} else {
		args = append(args, "^"+from)
	}
	out, _, err := r.prOut(ctx, append(args, "--")...)
	if err != nil {
		return false, err
	}
	return out != "", nil
}
