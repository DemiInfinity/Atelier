package main

import (
	"context"
	"errors"
	"fmt"

	"atelier/internal/store"
)

// When a repository's Tether home is GitHub, its default branch follows
// GitHub. A push to it here would only diverge (and need reconciling), so
// refuse it and point at GitHub instead. Other branches are fine: Tether
// pushes them to GitHub.
func init() { preReceiveChecks = append(preReceiveChecks, checkTetherHome) }

func checkTetherHome(ctx context.Context, pc pushContext) ([]string, error) {
	l, err := pc.DB.TetherLink(ctx, pc.Repo.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil || l.Home != "github" {
		return nil, err
	}
	def := "refs/heads/" + pc.Git.DefaultBranch(ctx)
	var out []string
	for _, u := range pc.Updates {
		if u.Ref == def {
			out = append(out, fmt.Sprintf("%s: this repository's home is GitHub (%s), so %s follows GitHub. "+
				"Push a branch and open a pull request, or push to GitHub directly.", u.Ref, l.GHRepo, u.Ref))
		}
	}
	return out, nil
}
