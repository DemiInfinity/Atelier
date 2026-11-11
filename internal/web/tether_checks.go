package web

import (
	"context"
	"strings"

	"atelier/internal/actions"
	"atelier/internal/store"
	"atelier/internal/tether"
)

// Checks for GitHub pull requests (Home: GitHub). The pull request sync
// fetches each open head into refs/pull/N/head and starts Atelier Actions on
// it; here every job's result goes back to GitHub as a commit status on the
// head commit, context "atelier/<workflow>/<job>", linking to the Atelier
// run. Only changes are sent.

// tetherReportStatuses reports statuses for one link (takes the repo lock).
func (a *App) tetherReportStatuses(ctx context.Context, l store.TetherLink) {
	unlock := a.tether().lockRepo(l.RepoID)
	defer unlock()
	rp, err := a.repoByID(ctx, l.RepoID)
	if err != nil {
		return
	}
	c, _, err := a.tetherClient(ctx, l.PersonID)
	if err != nil {
		return
	}
	a.tetherReportStatusesLocked(ctx, c, rp, l)
}

func tetherStatusState(check actCheck) (state, desc string) {
	switch check.Status {
	case "success":
		return "success", "Passed in Atelier"
	case "skipped":
		return "success", "Skipped in Atelier"
	case "failure":
		if check.Detail == actions.StatusCancelled {
			return "error", "Cancelled in Atelier"
		}
		return "failure", "Failed in Atelier"
	}
	if check.Detail == actions.StatusWaiting {
		return "pending", "Waiting for approval in Atelier"
	}
	return "pending", "Running in Atelier"
}

func tetherStatusContext(check actCheck) string {
	clean := func(s string) string {
		s = strings.Map(func(r rune) rune {
			if r == '/' || r < ' ' {
				return '-'
			}
			return r
		}, strings.TrimSpace(s))
		if len(s) > 60 {
			s = s[:60]
		}
		return s
	}
	return "atelier/" + clean(check.Workflow) + "/" + clean(check.Name)
}

func (a *App) tetherReportStatusesLocked(ctx context.Context, c *tether.Client, rp store.Repo, l store.TetherLink) {
	if l.Home != store.TetherHomeGitHub {
		return
	}
	items, err := a.db.TetherItems(ctx, rp.ID, "pull")
	if err != nil {
		return
	}
	base := strings.TrimRight(a.actionsBaseURL(), "/")
	for _, it := range items {
		if it.Origin != "github" || it.GHState != "open" || it.HeadSHA == "" {
			continue
		}
		checks := a.commitChecks(ctx, rp, it.HeadSHA, nil)
		for _, ch := range checks.Checks {
			state, desc := tetherStatusState(ch)
			cx := tetherStatusContext(ch)
			if old, oldDesc, err := a.db.TetherStatus(ctx, rp.ID, it.HeadSHA, cx); err == nil && old == state && oldDesc == desc {
				continue
			}
			st := tether.Status{State: state, Context: cx, Description: desc}
			if base != "" && strings.HasPrefix(base, "http") {
				st.TargetURL = base + ch.URL
			}
			if err := c.CreateStatus(ctx, l.GHRepo, it.HeadSHA, st); err != nil {
				return // rate limited, offline or no permission: next time
			}
			_ = a.db.SaveTetherStatus(ctx, rp.ID, it.HeadSHA, cx, state, desc)
		}
	}
}
