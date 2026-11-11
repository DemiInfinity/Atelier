package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
)

// "Today" for the app: what needs the person (agents waiting, failed runs,
// pull requests ready or reviewed, security alerts, previews waiting), what is
// running, the home feed, and the unread count. Everything comes from the
// same data the web pages show, limited to repositories the caller can read.

const (
	appNeedsMax      = 30
	appStatusPulls   = 60                  // open pull requests looked at for "ready to merge"
	appFailedRunDays = 14 * 24 * time.Hour // failed runs older than this stop asking for attention
	appFeedMax       = 24
)

type appNeed struct {
	at   time.Time
	body map[string]any
}

func appNeedItem(at time.Time, id, kind, thread, title, detail string, rp store.Repo, webPath string, action map[string]any) appNeed {
	return appNeed{at: at, body: map[string]any{
		"id": id, "kind": kind, "thread": thread, "title": title, "detail": detail, "repo": rp.FullName(),
		"web_path": webPath, "action": action, "at": appTime(at),
	}}
}

func (a *App) appStatus(w http.ResponseWriter, r *http.Request, c *appCaller) {
	ctx := r.Context()
	repos, readable, err := a.appReadable(ctx, c.Who)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	var needs []appNeed
	needs = append(needs, a.appNeedsAgents(ctx, readable)...)
	needs = append(needs, a.appNeedsRuns(ctx, readable)...)
	needs = append(needs, a.appNeedsPulls(ctx, c, repos)...)
	needs = append(needs, a.appNeedsReviews(ctx, c, repos, readable)...)
	needs = append(needs, a.appNeedsSecurity(ctx, repos)...)
	needs = append(needs, a.appNeedsPreviews(ctx, repos)...)
	sort.SliceStable(needs, func(i, j int) bool { return needs[i].at.After(needs[j].at) })
	if len(needs) > appNeedsMax {
		needs = needs[:appNeedsMax]
	}
	out := make([]map[string]any, 0, len(needs))
	for _, n := range needs {
		out = append(out, n.body)
	}
	unread, _ := a.db.UnreadNotificationCount(ctx, c.Who.PersonID)
	writeJSON(w, http.StatusOK, map[string]any{
		"needs": out, "running": a.appRunning(ctx, readable), "feed": a.appFeed(ctx, c, repos), "unread": unread,
	})
}

// appNeedsAgents: agent jobs waiting for approval.
func (a *App) appNeedsAgents(ctx context.Context, readable map[int64]store.Repo) []appNeed {
	var out []appNeed
	for _, wa := range a.appWaitingAgents(ctx, readable) {
		wants := "wants no tools"
		switch {
		case len(wa.Info.Writes) > 0:
			wants = "wants " + appAnd(wa.Info.Writes)
		case len(wa.Info.Tools) > 0:
			wants = "reads only: " + appAnd(wa.Info.Tools)
		}
		at := wa.Job.QueuedAt
		if at.IsZero() {
			at = wa.Job.UpdatedAt
		}
		out = append(out, appNeedItem(at, fmt.Sprintf("agent:%d", wa.Job.ID), "agent_approval", "agents",
			"Approve "+wa.Info.Workflow+" before it runs", "agent · "+wants, wa.Repo, actionsRunURL(wa.Repo, wa.Run),
			map[string]any{"label": "Review", "type": "approve_agent", "job_id": wa.Job.ID}))
	}
	return out
}

func appAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// appNeedsRuns: failed runs that nothing newer on the same branch and
// workflow has replaced.
func (a *App) appNeedsRuns(ctx context.Context, readable map[int64]store.Repo) []appNeed {
	runs, err := a.db.ActionsRuns(ctx, store.ActionsRunFilter{Limit: 300})
	if err != nil {
		return nil
	}
	var out []appNeed
	seen := map[string]bool{}
	cut := time.Now().Add(-appFailedRunDays)
	for _, run := range runs { // newest first
		key := fmt.Sprintf("%d\x00%s\x00%s", run.RepoID, run.WorkflowPath, run.Ref)
		if seen[key] {
			continue // a newer run of this workflow on this branch exists
		}
		seen[key] = true
		rp, ok := readable[run.RepoID]
		if !ok || !run.Done() || run.Conclusion != actions.StatusFailure || run.FinishedAt.Before(cut) {
			continue
		}
		detail := ""
		if jobs, err := a.db.ActionsJobs(ctx, run.ID); err == nil {
			for _, j := range jobs {
				if j.Status == actions.StatusFailure {
					detail = j.Name + " job"
					if j.RunnerName != "" {
						detail += " · ran on " + j.RunnerName
					}
					break
				}
			}
		}
		if detail == "" && len(run.Errors) > 0 {
			detail = eventExcerpt(run.Errors[0], 140)
		}
		thread := "yours"
		switch {
		case appIsAgentRun(run):
			thread = "agents"
		case a.appOnGitHub(ctx, rp.ID):
			thread = "github"
		}
		out = append(out, appNeedItem(run.FinishedAt, fmt.Sprintf("run:%d", run.ID), "run_failed", thread,
			run.WorkflowName+" failed on "+run.Branch(), detail, rp, actionsRunURL(rp, run),
			map[string]any{"label": "Open log", "type": "open_run", "run_id": run.ID}))
	}
	return out
}

// appNeedsPulls: open pull requests that can be merged with every check passed.
func (a *App) appNeedsPulls(ctx context.Context, c *appCaller, repos []store.Repo) []appNeed {
	var out []appNeed
	looked := 0
	for _, rp := range repos {
		pulls, err := a.db.Pulls(ctx, rp.ID, "open")
		if err != nil || len(pulls) == 0 {
			continue
		}
		var deps []store.DepUpdate
		for _, p := range pulls {
			if looked >= appStatusPulls {
				return out
			}
			looked++
			_, _, d, err := a.appBuildPull(ctx, rp, p)
			if err != nil || !appGreen(d) {
				continue
			}
			if _, err := a.db.AppAutoMerge(ctx, rp.ID, p.Number); err == nil {
				continue // already set to merge by itself
			}
			checks := "no checks"
			if d.Checks.Total > 0 {
				checks = "checks passed"
			}
			detail := checks
			if deps == nil {
				deps, _ = a.db.DepUpdates(ctx, rp.ID)
				if deps == nil {
					deps = []store.DepUpdate{}
				}
			}
			for _, u := range deps {
				if u.PRNumber == p.Number {
					detail = u.Name + " " + u.Latest
					if u.Fixes > 0 {
						detail += " fixes " + plural(u.Fixes, "advisory")
						if u.Fixes > 1 {
							detail = u.Name + " " + u.Latest + fmt.Sprintf(" fixes %d advisories", u.Fixes)
						}
					}
					detail += " · " + checks
					break
				}
			}
			out = append(out, appNeedItem(d.P.UpdatedAt, fmt.Sprintf("pull:%s:%d", rp.FullName(), p.Number), "pull_ready",
				appThread(p.AuthorKind, p.AuthorName), p.Title+" is ready to merge", detail, rp,
				fmt.Sprintf("/%s/pulls/%d", rp.FullName(), p.Number),
				map[string]any{"label": "Review", "type": "open_pull", "number": p.Number}))
		}
	}
	return out
}

// appNeedsReviews: open pull requests with comments or notes the person
// hasn't read yet (their unread notifications).
func (a *App) appNeedsReviews(ctx context.Context, c *appCaller, repos []store.Repo, readable map[int64]store.Repo) []appNeed {
	ids := make([]int64, 0, len(repos))
	for _, rp := range repos {
		ids = append(ids, rp.ID)
	}
	ns, err := a.db.Notifications(ctx, store.NotificationQuery{PersonID: c.Who.PersonID, UnreadOnly: true,
		SubjectTypes: []string{"pull", "tether"}, ReadableRepos: ids, Limit: 200})
	if err != nil {
		return nil
	}
	var out []appNeed
	seen := map[string]bool{}
	for _, n := range ns {
		if n.Kind != EventPullCommented && n.Kind != EventPullNote && n.Kind != EventTetherComment {
			continue
		}
		rp, ok := readable[n.RepoID]
		key := fmt.Sprintf("%d:%d", n.RepoID, n.Number)
		if !ok || n.Number == 0 || seen[key] {
			continue
		}
		p, err := a.db.PullByNumber(ctx, rp.ID, n.Number)
		if err != nil || !p.IsOpen() {
			continue
		}
		seen[key] = true
		who := strings.TrimPrefix(n.ActorName, tetherAuthorPrefix)
		thread := appThread(n.ActorKind, n.ActorName)
		if n.Kind == EventTetherComment {
			thread = "github"
		}
		out = append(out, appNeedItem(n.UpdatedAt, fmt.Sprintf("review:%s:%d", rp.FullName(), p.Number), "pull_review", thread,
			"New review on "+p.Title, appJoin(who, n.Detail), rp, fmt.Sprintf("/%s/pulls/%d", rp.FullName(), p.Number),
			map[string]any{"label": "Review", "type": "open_pull", "number": p.Number}))
	}
	return out
}

// appNeedsSecurity: open secret alerts and dependencies with known advisories.
func (a *App) appNeedsSecurity(ctx context.Context, repos []store.Repo) []appNeed {
	var out []appNeed
	for _, rp := range repos {
		path := "/" + rp.FullName() + "/security"
		action := map[string]any{"label": "Review", "type": "open_web"}
		if n, err := a.db.OpenSecretAlertCount(ctx, rp.ID); err == nil && n > 0 {
			at, detail := rp.UpdatedAt, "secret scanning"
			if alerts, err := a.db.SecretAlerts(ctx, rp.ID, 50); err == nil {
				for _, al := range alerts {
					if al.Status == store.AlertOpen {
						at = al.LastSeen
						detail = appJoin(al.RuleName, al.Path)
						break
					}
				}
			}
			title := plural(n, "secret") + " found in " + rp.Name
			out = append(out, appNeedItem(at, "security:"+rp.FullName()+":secrets", "security_alert", "yours", title, detail, rp, path, action))
		}
		if _, vulns, err := a.db.DepCounts(ctx, rp.ID); err == nil && vulns > 0 {
			at, detail := rp.UpdatedAt, "dependencies"
			if ups, err := a.db.DepUpdates(ctx, rp.ID); err == nil {
				var names []string
				for _, u := range ups {
					if u.Vulns > 0 {
						at = u.CheckedAt
						if len(names) < 3 {
							names = append(names, u.Name)
						}
					}
				}
				if len(names) > 0 {
					detail = strings.Join(names, ", ")
				}
			}
			title := plural(vulns, "dependency") + " with known advisories in " + rp.Name
			if vulns != 1 {
				title = fmt.Sprintf("%d dependencies with known advisories in %s", vulns, rp.Name)
			}
			out = append(out, appNeedItem(at, "security:"+rp.FullName()+":deps", "security_alert", "yours", title, detail, rp, path, action))
		}
	}
	return out
}

// appNeedsPreviews: Pages previews of outside pull requests waiting for approval.
func (a *App) appNeedsPreviews(ctx context.Context, repos []store.Repo) []appNeed {
	if !a.pagesEnabled() {
		return nil
	}
	var out []appNeed
	for _, rp := range repos {
		pvs, err := a.db.PagesPreviews(ctx, rp.ID)
		if err != nil {
			continue
		}
		for _, pv := range pvs {
			if pv.Status != pagesPreviewWaiting {
				continue
			}
			p, err := a.db.PullByNumber(ctx, rp.ID, pv.Number)
			if err != nil || !p.IsOpen() {
				continue
			}
			out = append(out, appNeedItem(pv.UpdatedAt, fmt.Sprintf("preview:%s:%d", rp.FullName(), pv.Number), "preview_approval",
				appThread(p.AuthorKind, p.AuthorName), "Approve the preview of "+p.Title,
				fmt.Sprintf("pull request %d · opened by %s", p.Number, strings.TrimPrefix(p.AuthorName, tetherAuthorPrefix)), rp,
				"/"+rp.FullName()+"/settings/pages#pages-previews",
				map[string]any{"label": "Review", "type": "open_web", "number": p.Number}))
		}
	}
	return out
}

func appIsAgentRun(run store.ActionsRun) bool {
	return strings.HasSuffix(strings.ToLower(run.WorkflowPath), ".md")
}

// appRunning lists the Actions jobs running now.
func (a *App) appRunning(ctx context.Context, readable map[int64]store.Repo) []map[string]any {
	out := []map[string]any{}
	jobs, err := a.db.ActionsJobsWithStatus(ctx, actions.StatusRunning)
	if err != nil {
		return out
	}
	for _, j := range jobs {
		rp, ok := readable[j.RepoID]
		if !ok {
			continue
		}
		run, err := a.db.ActionsRunByID(ctx, j.RunID)
		if err != nil {
			continue
		}
		kind := appJobKind(j, run)
		var engine any
		if kind == "agent" {
			engine = a.appAgentInfo(ctx, rp, run).Engine
		}
		detail := run.Event + " on " + run.Branch()
		if run.PullNumber != 0 {
			detail = fmt.Sprintf("pull request %d", run.PullNumber)
		}
		out = append(out, map[string]any{
			"job_id": j.ID, "run_id": run.ID, "repo": rp.FullName(), "workflow": run.WorkflowName, "job": j.Name,
			"kind": kind, "engine": engine, "runner": appStrOrNil(j.RunnerName), "started_at": appTime(j.StartedAt),
			"detail": detail, "web_path": actionsRunURL(rp, run),
		})
	}
	return out
}

// appFeed is the home page's feed (local commits, GitHub through Tether,
// other studios through Connect) as plain sentences.
func (a *App) appFeed(ctx context.Context, c *appCaller, repos []store.Repo) []map[string]any {
	acts := a.homeActivity(ctx, repos)
	th := a.tetherHomeData(ctx, c.Who.PersonID, "", "")
	// The feed belongs to the person, whichever credential asks for it.
	items := a.connectHomeFeed(ctx, personActor(c.Person), blendHomeFeed(acts, th.Feed, appFeedMax), appFeedMax)
	out := []map[string]any{}
	for _, it := range items {
		switch {
		case it.Local != nil:
			l := it.Local
			out = append(out, map[string]any{"thread": "yours",
				"text": fmt.Sprintf("%s committed “%s” to %s %s", l.Commit.Author, eventExcerpt(l.Commit.Subject, 100), l.Repo.Name, l.Branch),
				"at":   appTime(it.When), "web_path": "/" + l.Repo.FullName() + "/commit/" + l.Commit.Hash})
		case it.Tether != nil:
			t := it.Tether
			out = append(out, map[string]any{"thread": "github",
				"text": appSentence(t.Actor, t.Text, t.Repo, t.Detail), "at": appTime(it.When), "web_path": safeLocalURL(t.Link)})
		case it.Connect != nil:
			cn := it.Connect
			out = append(out, map[string]any{"thread": "connect",
				"text": appSentence(cn.Who, cn.Text, cn.ObjectName, cn.Detail), "at": appTime(it.When), "web_path": nil, "url": appStrOrNil(appHTTPURL(cn.Link))})
		}
	}
	return out
}

func appSentence(who, did, what, detail string) string {
	s := strings.Join(strings.Fields(who+" "+did+" "+what), " ")
	if detail = eventExcerpt(detail, 100); detail != "" {
		s += ": " + detail
	}
	return eventExcerpt(s, 240)
}

// appHTTPURL keeps only absolute http(s) links.
func appHTTPURL(u string) string {
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return u
	}
	return ""
}
