package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atelier/internal/depupdates"
	"atelier/internal/store"
)

// Lockfile regeneration on update branches (see internal/depupdates/lockfiles.go).
//
// The server never runs package managers. A workflow that runs on the
// update branch uses the runner's built-in `atelier/update-lockfiles@v1`
// step, which regenerates package-lock.json, pnpm-lock.yaml, yarn.lock,
// go.sum and Gradle lockfiles and pushes one commit with the job token:
//
//	on:
//	  push:
//	    branches: ["atelier-updates/**"]
//	jobs:
//	  lockfiles:
//	    runs-on: [self-hosted]
//	    steps:
//	      - uses: actions/checkout@v4
//	      - uses: atelier/update-lockfiles@v1
//
// Job tokens normally can't push at all. This policy lets one push only when
// the repository turned on "Regenerate lockfiles", the job's run is for an
// atelier-updates/* branch, and the updates bot has an open pull request from
// that branch. The pre-receive hook (cmd/atelier/hook_depupdates.go) then
// accepts nothing but a fast-forward of that one branch that changes only
// lockfiles.

func init() {
	// First, so the job policy (which never grants pushes) doesn't decide it.
	accessPolicies = append([]AccessPolicy{lockfileJobPolicy}, accessPolicies...)
}

func lockfileJobPolicy(ctx context.Context, a *App, who Actor, repo store.Repo, need Perm) (bool, bool) {
	if who.Kind != ActorJob || need != PermPush {
		return false, false
	}
	return a.lockfileJobMayPush(ctx, who, repo), true
}

func jobIDOf(who Actor) int64 {
	for _, s := range who.Scopes {
		if v, ok := strings.CutPrefix(s, "actions:job:"); ok {
			id, _ := strconv.ParseInt(v, 10, 64)
			return id
		}
	}
	return 0
}

func (a *App) lockfileJobMayPush(ctx context.Context, who Actor, repo store.Repo) bool {
	if repo.ID == 0 || jobRepoID(who) != repo.ID {
		return false
	}
	id := jobIDOf(who)
	if id == 0 {
		return false
	}
	st, err := a.db.LockfileJobState(ctx, repo.ID, id, secBotName)
	if err != nil {
		return false
	}
	return st.Running && st.Enabled && st.BotPR && depupdates.IsUpdateBranch(st.Branch)
}

// ---------- settings ----------

type secWeekday struct {
	Value    int
	Name     string
	Selected bool
}

func secWeekdays(sel int) []secWeekday {
	out := []secWeekday{{Value: -1, Name: "Any day (weekly)", Selected: sel < 0}}
	for _, d := range []int{1, 2, 3, 4, 5, 6, 0} {
		out = append(out, secWeekday{Value: d, Name: time.Weekday(d).String(), Selected: sel == d})
	}
	return out
}

// secSaveDepSettings saves the update options from the Security settings form.
func (a *App) secSaveDepSettings(ctx context.Context, repoID int64, r *http.Request) error {
	day, err := strconv.Atoi(r.FormValue("schedule_day"))
	if err != nil || day < -1 || day > 6 {
		day = -1
	}
	return a.db.SetDepUpdateSettings(ctx, repoID, store.DepUpdateSettings{
		GroupMinor:  r.FormValue("group_minor") == "on",
		ScheduleDay: day,
		Lockfiles:   r.FormValue("lockfiles") == "on",
	})
}
