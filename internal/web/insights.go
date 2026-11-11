package web

// Insights: /{owner}/{repo}/insights (Pulse, Commit activity, Code frequency,
// Contributors, Languages, Actions usage) and /insights, the overview across
// every repository the viewer can read.
//
//   insights.go         pages and the numbers they show
//   insights_git.go     git statistics, languages, caching per commit, background work
//   insights_charts.go  inline SVG charts with data tables
//
// Everything goes through App.can(PermRead): repository pages use loadRepo
// (404 for a repository you can't read), and the account page only counts
// repositories you can read.

import (
	"bufio"
	"context"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
	"atelier/internal/workflow"
)

func init() {
	registerFeature(feature{
		name: "insights",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /insights", a.authed(a.insightsAccount))
			m.HandleFunc("GET /{owner}/{repo}/insights", a.authed(a.insightsRepo))
			m.HandleFunc("GET /{owner}/{repo}/insights/{section}", a.authed(a.insightsRepo))
		},
	})
}

const (
	insWeeksShown   = 52
	insActionsDays  = 30
	insMaxJobs      = 20000
	insRefreshEvery = "4" // seconds, while updating
)

var insSections = []struct{ Key, Label string }{
	{"", "Pulse"},
	{"commits", "Commit activity"},
	{"code-frequency", "Code frequency"},
	{"contributors", "Contributors"},
	{"languages", "Languages"},
	{"actions", "Actions usage"},
}

type insNavItem struct {
	Label, URL string
	Current    bool
}

type insStat struct {
	Label, Value, Hint string
	Class              string
}

type insAuthorView struct {
	Rank                int
	Name, Email         string
	Commits, Adds, Dels string
	Share               float64
	Meter               template.HTML
	Spark               template.HTML
	First, Last         time.Time
}

type insPulseView struct {
	Days                    int
	Periods                 []insNavItem
	Commits, Authors        int
	Adds, Dels              int
	Truncated               bool
	Pulse                   store.Pulse
	MergedMore, OpenedMore  bool
	ClosedMore, ReleaseMore bool
}

type insWorkflowRow struct {
	Name           string
	Minutes        string
	Runs           int
	Success        string
	Meter          template.HTML
	MinutesValue   float64
	SuccessKnown   bool
	SuccessPercent float64
}

type insRunnerRow struct {
	Name    string
	Minutes string
	Jobs    int
	Meter   template.HTML
}

type insSlowJob struct {
	Name, Workflow, Runner, Duration, URL, Repo string
}

type insActionsView struct {
	Days        int
	Jobs        int
	Minutes     string
	Runs        int
	Success     string
	Workflows   []insWorkflowRow
	Runners     []insRunnerRow
	Slowest     []insSlowJob
	AgentRuns   int
	AgentTokens string
	AgentCost   string
	Truncated   bool
}

type insRepoData struct {
	V        *repoView
	Section  string
	Heading  string
	Nav      []insNavItem
	Res      insResult
	Refresh  bool
	Stats    []insStat
	Chart    template.HTML
	Chart2   template.HTML
	Authors  []insAuthorView
	Others   int
	LangBar  template.HTML
	Langs    []insShare
	LangHint string
	Pulse    *insPulseView
	Actions  *insActionsView
	Since    string
}

func (a *App) insightsRepo(w http.ResponseWriter, r *http.Request) {
	section := r.PathValue("section")
	known := false
	for _, s := range insSections {
		if s.Key == section {
			known = true
		}
	}
	if !known {
		a.notFound(w, r)
		return
	}
	v, _, ok := a.loadRepo(w, r, "insights")
	if !ok {
		return
	}
	ctx := r.Context()
	d := &insRepoData{V: v, Section: section}
	for _, s := range insSections {
		u := v.Base + "/insights"
		if s.Key != "" {
			u += "/" + s.Key
		}
		d.Nav = append(d.Nav, insNavItem{Label: s.Label, URL: u, Current: s.Key == section})
		if s.Key == section {
			d.Heading = s.Label
		}
	}
	switch section {
	case "":
		d.Pulse = a.insPulse(ctx, r, v)
		d.Heading = fmt.Sprintf("Pulse · last %d days", d.Pulse.Days)
	case "actions":
		av, err := a.insActions(ctx, []store.Repo{v.Repo}, insActionsDays)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		d.Actions = av
	default:
		d.Res = a.insStatsFor(ctx, v.Repo, insWait)
		d.Refresh = d.Res.Updating
		if d.Res.Stats != nil {
			a.insFillGitSection(d, d.Res.Stats)
		}
	}
	a.render(w, r, "insights_repo", page{Title: d.Heading + " · " + v.Repo.FullName(), Nav: "repos", Data: d})
}

func insWeekLabel(t time.Time) string { return "Week of " + t.Format("2 Jan 2006") }

func insXTicks(weeks []time.Time) [3]string {
	return [3]string{weeks[0].Format("Jan 2006"), weeks[len(weeks)/2].Format("Jan 2006"), "This week"}
}

// insFillGitSection builds the commit activity, code frequency, contributors
// or languages section from cached statistics.
func (a *App) insFillGitSection(d *insRepoData, st *insStats) {
	weeks := insWeeks(insWeeksShown, time.Now())
	labels := make([]string, len(weeks))
	for i, wk := range weeks {
		labels[i] = insWeekLabel(wk)
	}
	d.Since = weeks[0].Format("2 Jan 2006")
	switch d.Section {
	case "commits":
		vals := make([]float64, len(weeks))
		total, busiest, busyIdx, active := 0, 0, 0, 0
		for i, wk := range weeks {
			n := st.Weeks[wk.Format("2006-01-02")].Commits
			vals[i] = float64(n)
			total += n
			if n > busiest {
				busiest, busyIdx = n, i
			}
			if n > 0 {
				active++
			}
		}
		desc := fmt.Sprintf("Weekly commits on %s for the last %d weeks: %d in total.", st.Branch, len(weeks), total)
		if busiest > 0 {
			desc += fmt.Sprintf(" Busiest: %s with %d.", strings.ToLower(labels[busyIdx][:1])+labels[busyIdx][1:], busiest)
		}
		d.Stats = []insStat{
			{Label: "Commits, 52 weeks", Value: insNum(float64(total)), Class: "ins-ink-commits"},
			{Label: "Average per week", Value: trimZero(fmt.Sprintf("%.1f", float64(total)/float64(len(weeks))))},
			{Label: "Active weeks", Value: fmt.Sprintf("%d of %d", active, len(weeks))},
			{Label: "Busiest week", Value: insNum(float64(busiest))},
		}
		d.Chart = insColumns{ID: "ins-commits", Title: "Commits per week", Desc: desc, Labels: labels, Up: vals,
			UpName: "Commits", UpClass: "ins-c-commits", Unit: "commits", XTicks: insXTicks(weeks), TableHead: "Week"}.HTML()
	case "code-frequency":
		adds := make([]float64, len(weeks))
		dels := make([]float64, len(weeks))
		ta, td := 0, 0
		for i, wk := range weeks {
			x := st.Weeks[wk.Format("2006-01-02")]
			adds[i], dels[i] = float64(x.Adds), float64(x.Dels)
			ta += x.Adds
			td += x.Dels
		}
		d.Stats = []insStat{
			{Label: "Lines added", Value: "+" + insNum(float64(ta)), Class: "ins-ink-add"},
			{Label: "Lines deleted", Value: "−" + insNum(float64(td)), Class: "ins-ink-del"},
			{Label: "Net change", Value: insSigned(ta - td)},
		}
		d.Chart = insColumns{ID: "ins-freq", Title: "Additions and deletions per week",
			Desc: fmt.Sprintf("Lines added (above the line) and deleted (below) on %s each week for the last %d weeks: %d added, %d deleted.",
				st.Branch, len(weeks), ta, td),
			Labels: labels, Up: adds, Down: dels, UpName: "Additions", DownName: "Deletions",
			UpClass: "ins-c-add", DownClass: "ins-c-del", Unit: "lines", XTicks: insXTicks(weeks), TableHead: "Week"}.HTML()
	case "contributors":
		total := 0
		for _, au := range st.Authors {
			total += au.Commits
		}
		// Commits per week, all authors, as the overview chart.
		vals := make([]float64, len(weeks))
		for i, wk := range weeks {
			vals[i] = float64(st.Weeks[wk.Format("2006-01-02")].Commits)
		}
		d.Chart = insColumns{ID: "ins-contrib-all", Title: "Commits per week, all contributors",
			Desc:   fmt.Sprintf("Weekly commits on %s by %d contributors over the last %d weeks.", st.Branch, len(st.Authors)+st.OtherAuthors, len(weeks)),
			Labels: labels, Up: vals, UpName: "Commits", UpClass: "ins-c-commits", Unit: "commits", XTicks: insXTicks(weeks), TableHead: "Week"}.HTML()
		top := 0
		if len(st.Authors) > 0 {
			top = st.Authors[0].Commits
		}
		for i, au := range st.Authors {
			if i == 25 {
				d.Others = len(st.Authors) - 25 + st.OtherAuthors
				break
			}
			series := make([]float64, len(weeks))
			for j, wk := range weeks {
				series[j] = float64(au.Weeks[wk.Format("2006-01-02")])
			}
			name := au.Name
			if name == "" {
				name = au.Email
			}
			share := 0.0
			if st.Commits > 0 {
				share = float64(au.Commits) / float64(st.Commits) * 100
			}
			d.Authors = append(d.Authors, insAuthorView{Rank: i + 1, Name: name, Email: au.Email,
				Commits: insNum(float64(au.Commits)), Adds: "+" + insNum(float64(au.Adds)), Dels: "−" + insNum(float64(au.Dels)),
				Share: share, Meter: insMeter("ins-c-commits", float64(au.Commits)/math.Max(1, float64(top))*100),
				Spark: insSpark(fmt.Sprintf("ins-spark-%d", i), name, series),
				First: time.Unix(au.First, 0), Last: time.Unix(au.Last, 0)})
		}
		if d.Others == 0 {
			d.Others = st.OtherAuthors
		}
		d.Stats = []insStat{
			{Label: "Contributors", Value: strconv.Itoa(len(st.Authors) + st.OtherAuthors)},
			{Label: "Commits on " + st.Branch, Value: insNum(float64(st.Commits)), Class: "ins-ink-commits"},
		}
	case "languages":
		for _, l := range st.Langs {
			pct := 0.0
			if st.LangBytes > 0 {
				pct = float64(l.Bytes) / float64(st.LangBytes) * 100
			}
			d.Langs = append(d.Langs, insShare{Name: l.Name, Color: l.Color, Value: float64(l.Bytes), Text: humanBytes(l.Bytes),
				Pct: pct, Files: l.Files, Swatch: insSwatch(l.Color)})
		}
		// Fold the long tail into "Other" in the bar (the table keeps every language).
		bar := d.Langs
		if len(bar) > 8 {
			other := insShare{Name: "Other", Color: "#8a847b"}
			for _, l := range bar[8:] {
				other.Value += l.Value
			}
			bar = append(append([]insShare{}, bar[:8]...), other)
		}
		d.LangBar = insStackBar("ins-langs", "Languages on "+st.Branch+" by size", bar)
		if st.LangTruncated {
			d.LangHint = fmt.Sprintf("Only the first %d files were counted.", insMaxFiles)
		}
	}
}

func insSigned(n int) string {
	if n > 0 {
		return "+" + insNum(float64(n))
	}
	if n < 0 {
		return "−" + insNum(float64(-n))
	}
	return "0"
}

// insPulse counts what happened in the last 7 or 30 days.
func (a *App) insPulse(ctx context.Context, r *http.Request, v *repoView) *insPulseView {
	days := 7
	if r.URL.Query().Get("period") == "30" {
		days = 30
	}
	p := &insPulseView{Days: days}
	for _, n := range []int{7, 30} {
		u := v.Base + "/insights"
		if n == 30 {
			u += "?period=30"
		}
		p.Periods = append(p.Periods, insNavItem{Label: fmt.Sprintf("%d days", n), URL: u, Current: n == days})
	}
	since := time.Now().AddDate(0, 0, -days)
	if pulse, err := a.db.InsightsPulse(ctx, v.Repo.ID, since); err == nil {
		cut := func(n int) bool { return n > 50 }
		p.MergedMore, p.OpenedMore = cut(len(pulse.Merged)), cut(len(pulse.OpenedIssues))
		p.ClosedMore, p.ReleaseMore = cut(len(pulse.ClosedIssues)), cut(len(pulse.Releases))
		if p.MergedMore {
			pulse.Merged = pulse.Merged[:50]
		}
		if p.OpenedMore {
			pulse.OpenedIssues = pulse.OpenedIssues[:50]
		}
		if p.ClosedMore {
			pulse.ClosedIssues = pulse.ClosedIssues[:50]
		}
		if p.ReleaseMore {
			pulse.Releases = pulse.Releases[:50]
		}
		p.Pulse = pulse
	}
	if !v.Empty {
		if g, err := a.git.Open(v.Repo.Owner, v.Repo.Name); err == nil {
			p.Commits, p.Authors, p.Adds, p.Dels, p.Truncated = insRecentCommits(ctx, g, v.Default, since)
		}
	}
	return p
}

const insPulseMaxCommits = 5000

// insRecentCommits counts commits on branch since a moment (bounded: at most
// 5000 commits and 10 seconds).
func insRecentCommits(ctx context.Context, g *gitrepo.Repo, branch string, since time.Time) (commits, authors, adds, dels int, truncated bool) {
	seen := map[string]bool{}
	err := g.StreamGit(ctx, 10*time.Second, func(rd io.Reader) error {
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "\x1e") {
				commits++
				seen[strings.ToLower(strings.TrimSpace(line[1:]))] = true
				continue
			}
			f := strings.SplitN(line, "\t", 3)
			if len(f) == 3 {
				if n, err := strconv.Atoi(f[0]); err == nil {
					adds += n
				}
				if n, err := strconv.Atoi(f[1]); err == nil {
					dels += n
				}
			}
		}
		return sc.Err()
	}, "log", branch, "--no-renames", "--numstat", "--format=%x1e%aE", "--since=@"+strconv.FormatInt(since.Unix(), 10),
		"--max-count="+strconv.Itoa(insPulseMaxCommits), "--")
	if err != nil {
		truncated = true
	}
	if commits >= insPulseMaxCommits {
		truncated = true
	}
	return commits, len(seen), adds, dels, truncated
}

// ---------- Actions usage ----------

func insMinutes(d time.Duration) string {
	m := d.Minutes()
	switch {
	case m == 0:
		return "0 min"
	case m < 1:
		return "<1 min"
	case m < 100:
		return trimZero(fmt.Sprintf("%.1f", m)) + " min"
	}
	return fmt.Sprintf("%s h", trimZero(fmt.Sprintf("%.1f", m/60)))
}

func insDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

func insPercent(n, of int) string {
	if of == 0 {
		return "—"
	}
	return trimZero(fmt.Sprintf("%.1f", float64(n)/float64(of)*100)) + "%"
}

// insActions sums Actions usage over the given repositories (which the caller
// has checked the viewer can read).
func (a *App) insActions(ctx context.Context, repos []store.Repo, days int) (*insActionsView, error) {
	since := time.Now().AddDate(0, 0, -days)
	ids := make([]int64, len(repos))
	byID := map[int64]store.Repo{}
	for i, rp := range repos {
		ids[i] = rp.ID
		byID[rp.ID] = rp
	}
	jobs, err := a.db.InsightsJobs(ctx, ids, since, insMaxJobs+1)
	if err != nil {
		return nil, err
	}
	outcomes, err := a.db.InsightsRunOutcomes(ctx, ids, since)
	if err != nil {
		return nil, err
	}
	av := &insActionsView{Days: days}
	if len(jobs) > insMaxJobs {
		jobs, av.Truncated = jobs[:insMaxJobs], true
	}
	av.Jobs = len(jobs)

	type acc struct {
		dur  time.Duration
		jobs int
	}
	wf := map[string]*acc{}
	rn := map[string]*acc{}
	var total time.Duration
	var agentTok int64
	var agentCost float64
	agentCostKnown := false
	for _, j := range jobs {
		if j.Status == "skipped" {
			continue
		}
		dur := j.Duration()
		total += dur
		name := j.WorkflowName
		if name == "" {
			name = j.WorkflowPath
		}
		if wf[name] == nil {
			wf[name] = &acc{}
		}
		wf[name].dur += dur
		wf[name].jobs++
		runner := j.RunnerName
		if runner == "" {
			runner = "Unknown runner"
		}
		if rn[runner] == nil {
			rn[runner] = &acc{}
		}
		rn[runner].dur += dur
		rn[runner].jobs++
		if j.JobKey == workflow.AgentJobID {
			if u, ok := agentUsageFromJob(store.ActionsJob{JobKey: j.JobKey, Outputs: j.Outputs}); ok {
				av.AgentRuns++
				agentTok += u.Input + u.Output + u.CacheRead + u.CacheWrite
				if u.HasCost {
					agentCost += u.Cost
					agentCostKnown = true
				}
			}
		}
	}
	av.Minutes = insMinutes(total)

	// Runs and success rate per workflow (cancelled runs don't count either way).
	type outc struct{ runs, ok, decided int }
	ow := map[string]*outc{}
	allOK, allDecided := 0, 0
	for _, o := range outcomes {
		if ow[o.WorkflowName] == nil {
			ow[o.WorkflowName] = &outc{}
		}
		x := ow[o.WorkflowName]
		x.runs += o.Runs
		av.Runs += o.Runs
		switch o.Conclusion {
		case "success":
			x.ok += o.Runs
			x.decided += o.Runs
			allOK += o.Runs
			allDecided += o.Runs
		case "failure", "timed_out", "startup_failure":
			x.decided += o.Runs
			allDecided += o.Runs
		}
	}
	av.Success = insPercent(allOK, allDecided)

	names := map[string]bool{}
	for n := range wf {
		names[n] = true
	}
	for n := range ow {
		names[n] = true
	}
	maxWF := time.Duration(0)
	for _, x := range wf {
		maxWF = max(maxWF, x.dur)
	}
	for n := range names {
		row := insWorkflowRow{Name: n}
		if x := wf[n]; x != nil {
			row.Minutes, row.MinutesValue = insMinutes(x.dur), x.dur.Minutes()
		} else {
			row.Minutes = "0 min"
		}
		if o := ow[n]; o != nil {
			row.Runs = o.runs
			row.Success = insPercent(o.ok, o.decided)
		} else {
			row.Success = "—"
		}
		if maxWF > 0 && wf[n] != nil {
			row.Meter = insMeter("ins-c-commits", float64(wf[n].dur)/float64(maxWF)*100)
		} else {
			row.Meter = insMeter("ins-c-commits", 0)
		}
		av.Workflows = append(av.Workflows, row)
	}
	sort.Slice(av.Workflows, func(i, j int) bool {
		if av.Workflows[i].MinutesValue != av.Workflows[j].MinutesValue {
			return av.Workflows[i].MinutesValue > av.Workflows[j].MinutesValue
		}
		return av.Workflows[i].Name < av.Workflows[j].Name
	})

	maxRn := time.Duration(0)
	for _, x := range rn {
		maxRn = max(maxRn, x.dur)
	}
	for n, x := range rn {
		pct := 0.0
		if maxRn > 0 {
			pct = float64(x.dur) / float64(maxRn) * 100
		}
		av.Runners = append(av.Runners, insRunnerRow{Name: n, Minutes: insMinutes(x.dur), Jobs: x.jobs, Meter: insMeter("ins-c-runner", pct)})
	}
	sort.Slice(av.Runners, func(i, j int) bool {
		if rn[av.Runners[i].Name].dur != rn[av.Runners[j].Name].dur {
			return rn[av.Runners[i].Name].dur > rn[av.Runners[j].Name].dur
		}
		return av.Runners[i].Name < av.Runners[j].Name
	})

	slow := append([]store.InsightsJob(nil), jobs...)
	sort.SliceStable(slow, func(i, j int) bool { return slow[i].Duration() > slow[j].Duration() })
	for _, j := range slow {
		if len(av.Slowest) == 5 || j.Duration() <= 0 {
			break
		}
		rp := byID[j.RepoID]
		av.Slowest = append(av.Slowest, insSlowJob{Name: j.Name, Workflow: j.WorkflowName, Runner: j.RunnerName,
			Duration: insDuration(j.Duration()), Repo: rp.FullName(),
			URL: fmt.Sprintf("/%s/actions/runs/%d", rp.FullName(), j.RunSeq)})
	}
	if av.AgentRuns > 0 {
		av.AgentTokens = agentTokens(agentTok)
		if agentCostKnown {
			av.AgentCost = "≈ " + agentDollars(agentCost)
		}
	}
	return av, nil
}

// ---------- account insights ----------

type insRepoActivity struct {
	Repo     store.Repo
	Commits  int
	Meter    template.HTML
	Updating bool
}

type insAccountData struct {
	Repos      int
	Calendar   template.HTML
	YearTotal  int
	Active     []insRepoActivity
	Actions    *insActionsView
	Updating   int
	Refresh    bool
	ActiveDays int
	Streak     int
}

func (a *App) insightsAccount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := a.db.Repos(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	repos := a.readableRepos(ctx, a.actor(r), all)
	d := &insAccountData{Repos: len(repos)}

	// Calendar: 53 weeks ending today, counted the same way as the Embers on
	// home and profiles (contributions.go): commits on each default branch.
	today := time.Now()
	start := contribStart(today)
	counts, perRepo := a.contributionDays(ctx, repos, start)
	monthAgo := today.AddDate(0, 0, -30).Format("2006-01-02")
	for _, rp := range repos {
		act := insRepoActivity{Repo: rp}
		for day, n := range perRepo[rp.ID] {
			if day >= monthAgo {
				act.Commits += n
			}
		}
		if act.Commits > 0 {
			d.Active = append(d.Active, act)
		}
	}
	var cells []insCalCell
	streak := 0
	for day := start; !day.After(today); day = day.AddDate(0, 0, 1) {
		n := counts[insDayKey(day)]
		cells = append(cells, insCalCell{Day: day, Count: n})
		d.YearTotal += n
		if n > 0 {
			d.ActiveDays++
			streak++
		} else if day.Format("2006-01-02") != today.Format("2006-01-02") {
			streak = 0
		}
	}
	d.Streak = streak
	d.Calendar = insCalendar("ins-cal", "Commits per day across your repositories", cells, d.YearTotal)

	sort.Slice(d.Active, func(i, j int) bool {
		if d.Active[i].Commits != d.Active[j].Commits {
			return d.Active[i].Commits > d.Active[j].Commits
		}
		return d.Active[i].Repo.FullName() < d.Active[j].Repo.FullName()
	})
	if len(d.Active) > 8 {
		d.Active = d.Active[:8]
	}
	for i := range d.Active {
		d.Active[i].Meter = insMeter("ins-c-commits", float64(d.Active[i].Commits)/float64(d.Active[0].Commits)*100)
	}
	if d.Actions, err = a.insActions(ctx, repos, insActionsDays); err != nil {
		a.fail(w, r, err)
		return
	}
	d.Refresh = d.Updating > 0
	a.render(w, r, "insights_account", page{Title: "Insights", Nav: "insights", Data: d})
}
