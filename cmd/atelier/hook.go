package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"atelier/internal/config"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// `atelier hook pre-receive <owner> <repo>` is run by git from each
// repository's pre-receive hook (installed by internal/gitrepo/hooks.go) for
// pushes over both HTTPS and SSH. It reads "<old> <new> <ref>" lines on stdin
// and exits non-zero to reject the whole push. Atelier's own merges move refs
// with update-ref, which doesn't run hooks, so they are never blocked here.

func init() { commands["hook"] = runHook }

func runHook(args []string) error {
	const usage = "usage: atelier hook pre-receive|post-receive <owner> <repo>"
	if len(args) != 3 {
		return errors.New(usage)
	}
	switch args[0] {
	case "pre-receive":
		return preReceive(args[1], args[2], os.Stdin, os.Stderr)
	case "post-receive":
		return postReceive(args[1], args[2], os.Getenv("REMOTE_USER"), os.Stdin, os.Stderr)
	}
	return errors.New(usage)
}

// refUpdate is one line of pre-receive input.
type refUpdate struct{ Old, New, Ref string }

func (u refUpdate) isCreate() bool { return strings.Trim(u.Old, "0") == "" }
func (u refUpdate) isDelete() bool { return strings.Trim(u.New, "0") == "" }

func parseRefUpdates(r io.Reader) ([]refUpdate, error) {
	var ups []refUpdate
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("unexpected hook input %q", sc.Text())
		}
		ups = append(ups, refUpdate{Old: f[0], New: f[1], Ref: f[2]})
	}
	return ups, sc.Err()
}

// pushContext is what a pre-receive check gets.
type pushContext struct {
	DB      *store.Store
	Repo    store.Repo
	Git     *gitrepo.Repo
	Updates []refUpdate
}

// A preReceiveCheck returns one human-readable message per rejected update.
// Other features (secret scanning, …) can append to preReceiveChecks from
// their own file's init().
type preReceiveCheck func(ctx context.Context, pc pushContext) ([]string, error)

var preReceiveChecks = []preReceiveCheck{checkBranchRules}

func preReceive(owner, name string, stdin io.Reader, stderr io.Writer) error {
	ups, err := parseRefUpdates(stdin)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cfg := config.FromEnv()
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		fmt.Fprintln(stderr, "atelier: can't read branch rules, push rejected:", err)
		return errors.New("push rejected")
	}
	defer db.Close()
	repo, err := db.RepoByName(ctx, owner, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil // not an Atelier repository (or being deleted): nothing to enforce
	}
	if err != nil {
		fmt.Fprintln(stderr, "atelier: can't read branch rules, push rejected:", err)
		return errors.New("push rejected")
	}
	gs, err := gitrepo.New(cfg.RepoRoot())
	if err != nil {
		return err
	}
	g, err := gs.Open(repo.Owner, repo.Name)
	if err != nil {
		return err
	}
	pc := pushContext{DB: db, Repo: repo, Git: g, Updates: ups}
	var msgs []string
	for _, check := range preReceiveChecks {
		m, err := check(ctx, pc)
		if err != nil {
			fmt.Fprintln(stderr, "atelier: push check failed, push rejected:", err)
			return errors.New("push rejected")
		}
		msgs = append(msgs, m...)
	}
	if len(msgs) == 0 {
		return nil
	}
	fmt.Fprintln(stderr, "")
	for _, m := range msgs {
		fmt.Fprintln(stderr, "atelier: "+m)
	}
	fmt.Fprintln(stderr, "")
	return errors.New("push rejected")
}

// checkBranchRules enforces require_pr, block_force_push and linear_history.
func checkBranchRules(ctx context.Context, pc pushContext) ([]string, error) {
	rules, err := pc.DB.BranchRules(ctx, pc.Repo.ID)
	if err != nil || len(rules) == 0 {
		return nil, err
	}
	var msgs []string
	for _, u := range pc.Updates {
		branch, ok := strings.CutPrefix(u.Ref, "refs/heads/")
		if !ok {
			continue
		}
		p := store.ProtectionFrom(rules, branch)
		if !p.Protected() {
			continue
		}
		msg, err := branchRuleViolation(ctx, pc.Git, u, branch, p)
		if err != nil {
			return nil, err
		}
		if msg != "" {
			msgs = append(msgs, msg)
		}
	}
	return msgs, nil
}

func branchRuleViolation(ctx context.Context, g *gitrepo.Repo, u refUpdate, branch string, p store.Protection) (string, error) {
	rule := "branch rule " + p.Patterns()
	switch {
	case u.isCreate():
		// Creating a protected branch (e.g. the first push of main) is allowed,
		// but it must still have linear history if the rule asks for it.
		if p.LinearHistory {
			merges, err := g.HasMergeCommits(ctx, "", u.New)
			if err != nil {
				return "", err
			}
			if merges {
				return fmt.Sprintf("%s is protected by %s: merge commits aren't allowed (linear history).", branch, rule), nil
			}
		}
		return "", nil
	case u.isDelete():
		if p.BlockForcePush || p.RequirePR {
			return fmt.Sprintf("%s is protected by %s: it can't be deleted.", branch, rule), nil
		}
		return "", nil
	}
	if p.BlockForcePush {
		ff, err := g.IsAncestor(ctx, u.Old, u.New)
		if err != nil {
			return "", err
		}
		if !ff {
			return fmt.Sprintf("%s is protected by %s: force pushes aren't allowed.", branch, rule), nil
		}
	}
	if p.RequirePR {
		return fmt.Sprintf("%s is protected by %s: changes must go through a pull request. "+
			"Push to another branch and open a pull request in Atelier.", branch, rule), nil
	}
	if p.LinearHistory {
		merges, err := g.HasMergeCommits(ctx, u.Old, u.New)
		if err != nil {
			return "", err
		}
		if merges {
			return fmt.Sprintf("%s is protected by %s: merge commits aren't allowed (linear history).", branch, rule), nil
		}
	}
	return "", nil
}
