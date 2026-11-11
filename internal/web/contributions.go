package web

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"atelier/internal/store"
)

// Contributions: one way of counting commits, shared by the home and profile
// Embers and the account Insights calendar (issue #12: Embers used to count
// every branch while Insights counted the default branch only).
//
// Like GitHub, a commit counts once it is on a repository's default branch.
// Work-in-progress branches, pull request heads and rejected experiments
// don't, and a branch that is merged later counts then, on the commit's
// author date. Commits reachable from several refs are counted once.

const contribGitTimeout = 20 * time.Second

// contribStart is the first day of the 53-week grid ending today: the Sunday
// 52 weeks before the start of this week, in local time.
func contribStart(today time.Time) time.Time { return insWeekStart(today).AddDate(0, 0, -52*7) }

// contributionDays counts commits per local day (YYYY-MM-DD) on the default
// branch of each repository, from since onwards. It also returns the count
// per repository.
func (a *App) contributionDays(ctx context.Context, repos []store.Repo, since time.Time) (map[string]int, map[int64]map[string]int) {
	days := map[string]int{}
	perRepo := map[int64]map[string]int{}
	for _, rp := range repos {
		g, err := a.git.Open(rp.Owner, rp.Name)
		if err != nil || g.IsEmpty(ctx) {
			continue
		}
		head, err := g.ResolveCommit(ctx, g.DefaultBranch(ctx))
		if err != nil {
			continue
		}
		mine := map[string]int{}
		_ = g.StreamGit(ctx, contribGitTimeout, func(r io.Reader) error {
			sc := bufio.NewScanner(r)
			for sc.Scan() {
				n, err := strconv.ParseInt(strings.TrimSpace(sc.Text()), 10, 64)
				if err != nil {
					continue
				}
				when := time.Unix(n, 0)
				if when.Before(since) {
					continue
				}
				k := insDayKey(when)
				mine[k]++
				days[k]++
			}
			return sc.Err()
		}, "log", head, "--format=%at", "--since="+strconv.FormatInt(since.Unix(), 10), "--")
		perRepo[rp.ID] = mine
	}
	return days, perRepo
}
