package web

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"atelier/internal/store"
	"atelier/internal/workflow"
)

// insCommit commits a file as a given author at a given time.
func insCommit(t *testing.T, work, author string, when time.Time, file, content, msg string) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_DATE", when.Format(time.RFC3339))
	t.Setenv("GIT_COMMITTER_DATE", when.Format(time.RFC3339))
	name, email, _ := strings.Cut(author, " <")
	t.Setenv("GIT_AUTHOR_NAME", name)
	t.Setenv("GIT_AUTHOR_EMAIL", strings.TrimSuffix(email, ">"))
	pullsCommit(t, work, file, content, msg)
}

// insFixture: zoe/demo (private) with five commits by two authors.
//
//	20 days ago  Ann  README.md      +2         (Markdown: not a language)
//	13 days ago  Ann  main.go        +5         (Go)
//	 2 days ago  Bob  main.go        +1 −1
//	 2 days ago  Bob  web/style.css  +3         (CSS, 30 bytes)
//	 1 day ago   Ann  README.md      +1 −1
const (
	insGoSrc  = "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(1) }\n"
	insCSSSrc = ".a{color:red}\n.bb{color:red}\n\n"
)

func insFixture(t *testing.T) (gitFixture, string, time.Time) {
	t.Helper()
	f := newGitFixture(t)
	work := pullsWork(t, f.a)
	now := time.Now()
	ann, bob := "Ann Lee <ann@example.com>", "Bob <BOB@example.com>"
	insCommit(t, work, ann, now.AddDate(0, 0, -20), "README.md", "# demo\nhello\n", "Readme")
	insCommit(t, work, ann, now.AddDate(0, 0, -13), "main.go", insGoSrc, "Main")
	insCommit(t, work, bob, now.AddDate(0, 0, -2), "main.go", strings.Replace(insGoSrc, "Println(1)", "Println(2)", 1), "Two")
	insCommit(t, work, bob, now.AddDate(0, 0, -2), "web/style.css", insCSSSrc, "Style")
	insCommit(t, work, ann, now.AddDate(0, 0, -1), "README.md", "# demo\nhello there\n", "Readme 2")
	runGit(t, work, "push", "-q", "origin", "main")
	return f, work, now
}

func TestInsightsStatsFromGit(t *testing.T) {
	f, work, now := insFixture(t)
	ctx := context.Background()
	g, err := f.a.git.Open("zoe", "demo")
	if err != nil {
		t.Fatal(err)
	}
	head := runGit(t, work, "rev-parse", "HEAD")
	st, err := computeInsStats(ctx, g, "main", head)
	if err != nil {
		t.Fatal(err)
	}
	if st.Commits != 5 || st.Truncated || st.Head != head {
		t.Fatalf("commits = %d (truncated %v) head %s", st.Commits, st.Truncated, st.Head)
	}
	// Authors: grouped by email (case-insensitive), most commits first.
	if len(st.Authors) != 2 || st.Authors[0].Email != "ann@example.com" || st.Authors[0].Commits != 3 ||
		st.Authors[1].Email != "bob@example.com" || st.Authors[1].Commits != 2 || st.Authors[0].Name != "Ann Lee" {
		t.Fatalf("authors = %+v", st.Authors)
	}
	// Ann: +2 (readme) +5 (main.go) +1 −1 (readme 2). Bob: +1 −1 (main.go), +3 (css).
	if a := st.Authors[0]; a.Adds != 8 || a.Dels != 1 {
		t.Errorf("Ann adds/dels = %d/%d, want 8/1", a.Adds, a.Dels)
	}
	if b := st.Authors[1]; b.Adds != 4 || b.Dels != 1 {
		t.Errorf("Bob adds/dels = %d/%d, want 4/1", b.Adds, b.Dels)
	}
	// Weekly totals land in the right weeks.
	wk := func(daysAgo int) insWeek {
		return st.Weeks[insWeekStart(now.AddDate(0, 0, -daysAgo)).Format("2006-01-02")]
	}
	totalC, totalA, totalD := 0, 0, 0
	for _, w := range st.Weeks {
		totalC += w.Commits
		totalA += w.Adds
		totalD += w.Dels
	}
	if totalC != 5 || totalA != 12 || totalD != 2 {
		t.Errorf("weekly totals = %d commits +%d −%d", totalC, totalA, totalD)
	}
	if w := wk(20); w.Commits < 1 || w.Adds < 2 {
		t.Errorf("week 20 days ago = %+v", w)
	}
	if st.Days[now.AddDate(0, 0, -13).Format("2006-01-02")] != 1 || st.Days[now.AddDate(0, 0, -2).Format("2006-01-02")] != 2 {
		t.Errorf("days = %v", st.Days)
	}
	// Languages: Go and CSS by bytes; Markdown isn't counted.
	goBytes, cssBytes := int64(len(insGoSrc)), int64(len(insCSSSrc))
	if len(st.Langs) != 2 || st.Langs[0].Name != "Go" || st.Langs[0].Bytes != goBytes || st.Langs[1].Name != "CSS" ||
		st.Langs[1].Bytes != cssBytes || st.LangBytes != goBytes+cssBytes {
		t.Errorf("langs = %+v (%d bytes)", st.Langs, st.LangBytes)
	}
}

func TestDetectLanguage(t *testing.T) {
	for p, want := range map[string]string{
		"main.go": "Go", "web/app.TSX": "TypeScript", "Dockerfile": "Dockerfile", "Dockerfile.dev": "Dockerfile",
		"scripts/x.sh": "Shell", "Makefile": "Makefile", "ui/App.vue": "Vue",
		"README.md": "", "go.sum": "", "data.json": "", "vendor/x/y.go": "", "node_modules/a/b.js": "",
		"static/app.min.js": "", "api/x.pb.go": "", "docs/site.js": "",
	} {
		got, ok := detectLanguage(p)
		if (want == "") == ok || got.Name != want {
			t.Errorf("detectLanguage(%q) = %q %v, want %q", p, got.Name, ok, want)
		}
	}
	// Dark linguist colours are lightened to show on the dark surface.
	if c := insLangColor("#000080"); c == "#000080" {
		t.Errorf("Lua's navy wasn't lightened: %s", c)
	}
	if c := insLangColor("#f1e05a"); c != "#f1e05a" {
		t.Errorf("a light colour changed: %s", c)
	}
}

// insWaitFor waits for the background computation of head to be cached.
func insWaitFor(t *testing.T, a *App, repoID int64, head string) *insStats {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st := a.insCached(context.Background(), repoID); st != nil && st.Head == head {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stats for %s never computed", head)
	return nil
}

func TestInsightsCachedPerHead(t *testing.T) {
	f, work, now := insFixture(t)
	a := f.a
	ctx := context.Background()
	rp := secRepo(t, a, "demo")
	head := runGit(t, work, "rev-parse", "HEAD")

	before := insComputeCount.Load()
	res := a.insStatsFor(ctx, rp, 20*time.Second)
	if res.Updating || res.Stats == nil || res.Stats.Head != head || res.Stats.Commits != 5 {
		t.Fatalf("first view = %+v", res)
	}
	if n := insComputeCount.Load() - before; n != 1 {
		t.Fatalf("computed %d times, want 1", n)
	}
	e, err := a.db.InsightsCache(ctx, rp.ID, insStatsKind)
	if err != nil || e.HeadSHA != head {
		t.Fatalf("cache row = %+v, %v", e, err)
	}
	// Same HEAD: served from the cache, nothing recomputed.
	for i := 0; i < 3; i++ {
		if res := a.insStatsFor(ctx, rp, 0); res.Updating || res.Stats == nil || res.Stats.Head != head {
			t.Fatalf("cached view = %+v", res)
		}
	}
	if n := insComputeCount.Load() - before; n != 1 {
		t.Fatalf("recomputed for the same HEAD (%d computations)", n)
	}

	// A new commit: the next view (not waiting) shows the old numbers while
	// it updates in the background, then the new ones.
	insCommit(t, work, "Ann Lee <ann@example.com>", now, "main.go", "package main\n", "Shrink")
	runGit(t, work, "push", "-q", "origin", "main")
	head2 := runGit(t, work, "rev-parse", "HEAD")
	res = a.insStatsFor(ctx, rp, 0)
	if !res.Updating || res.Stats == nil || res.Stats.Head != head {
		t.Fatalf("view after push = updating %v, stats %+v", res.Updating, res.Stats)
	}
	st := insWaitFor(t, a, rp.ID, head2)
	if st.Commits != 6 {
		t.Errorf("after push: %d commits, want 6", st.Commits)
	}
	if n := insComputeCount.Load() - before; n != 2 {
		t.Errorf("computations = %d, want 2", n)
	}
	// The page while updating says so and refreshes itself.
	insCommit(t, work, "Ann Lee <ann@example.com>", now, "main.go", "package main\n\n", "Again")
	runGit(t, work, "push", "-q", "origin", "main")
	head3 := runGit(t, work, "rev-parse", "HEAD")
	saved := insWait
	insWait = 0
	t.Cleanup(func() { insWait = saved })
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/insights/commits", nil, pullsCookie(t, a)), 200, "updating page")
	mustContain(t, body, "updating page", `http-equiv="refresh"`, "Updating…", "from an earlier commit ("+head2[:7]+")")
	insWaitFor(t, a, rp.ID, head3)
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/insights/commits", nil, pullsCookie(t, a)), 200, "updated page")
	if strings.Contains(body, `http-equiv="refresh"`) || strings.Contains(body, "Updating…") {
		t.Error("page still updating after the computation finished")
	}
}

func TestInsightsPages(t *testing.T) {
	f, _, _ := insFixture(t)
	a := f.a
	ctx := context.Background()
	rp := secRepo(t, a, "demo")
	ck := pullsCookie(t, a)
	saved := insWait
	insWait = 20 * time.Second
	t.Cleanup(func() { insWait = saved })

	// Pulse data: a merged pull request, an opened and a closed issue, a release.
	db := a.db.DB()
	nowU := time.Now().Unix()
	old := time.Now().AddDate(0, 0, -60).Unix()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO pulls (repo_id, number, title, head, base, state, merged_at, author_name, created_at, updated_at)
		VALUES (?, 1, 'Add greeting', 'feat', 'main', 'merged', ?, 'zoe', ?, ?)`, rp.ID, nowU, nowU, nowU)
	mustExec(`INSERT INTO pulls (repo_id, number, title, head, base, state, merged_at, author_name, created_at, updated_at)
		VALUES (?, 2, 'Ancient merge', 'old', 'main', 'merged', ?, 'zoe', ?, ?)`, rp.ID, old, old, old)
	mustExec(`INSERT INTO issues (repo_id, number, title, state, author_kind, author_name, created_at, updated_at)
		VALUES (?, 3, 'Fresh bug', 'open', 'owner', 'zoe', ?, ?)`, rp.ID, nowU, nowU)
	mustExec(`INSERT INTO issues (repo_id, number, title, state, author_kind, author_name, created_at, updated_at, closed_at)
		VALUES (?, 4, 'Fixed bug', 'closed', 'owner', 'zoe', ?, ?, ?)`, rp.ID, old, nowU, nowU)
	mustExec(`INSERT INTO releases (repo_id, tag, title, created_at, published_at) VALUES (?, 'v1.0.0', 'First', ?, ?)`, rp.ID, nowU, nowU)

	p, err := a.db.InsightsPulse(ctx, rp.ID, time.Now().AddDate(0, 0, -7))
	if err != nil || len(p.Merged) != 1 || p.Merged[0].Number != 1 || len(p.OpenedIssues) != 1 || len(p.ClosedIssues) != 1 ||
		len(p.Releases) != 1 || p.OpenedPulls != 1 {
		t.Fatalf("pulse = %+v, %v", p, err)
	}
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/insights", nil, ck), 200, "pulse")
	mustContain(t, body, "pulse", "Pulse · last 7 days", "Add greeting", "Fresh bug", "Fixed bug", "v1.0.0",
		`href="/zoe/demo/insights" aria-current="page"`, "/static/insights.css")
	if strings.Contains(body, "Ancient merge") {
		t.Error("pulse shows a pull request merged 60 days ago")
	}
	// Commits in the last 7 days: Bob's two and Ann's last one, by two authors.
	mustContain(t, body, "pulse commits", `ins-ink-commits">3<`, "by 2 authors")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/insights?period=30", nil, ck), 200, "pulse 30")
	mustContain(t, body, "pulse 30", "Pulse · last 30 days", `ins-ink-commits">5<`)

	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/insights/commits", nil, ck), 200, "commits")
	mustContain(t, body, "commit activity", "Commits per week", `role="img"`, `<title id="ins-commits-t">`, `<desc id="ins-commits-d">`,
		"Show data table", "<table", "This week", "5 in total")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/insights/code-frequency", nil, ck), 200, "frequency")
	mustContain(t, body, "code frequency", "&#43;12", "−2", "Additions", "Deletions", "ins-c-del")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/insights/contributors", nil, ck), 200, "contributors")
	mustContain(t, body, "contributors", "Ann Lee", "ann@example.com", "Bob", "ins-spark")
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/insights/languages", nil, ck), 200, "languages")
	goPct := float64(len(insGoSrc)) / float64(len(insGoSrc)+len(insCSSSrc)) * 100
	mustContain(t, body, "languages", "Go", "CSS", fmt.Sprintf("%.1f%%", goPct), fmt.Sprintf("%.1f%%", 100-goPct))
	if strings.Contains(body, ">Markdown<") {
		t.Error("Markdown counted as a language")
	}
	expectStatus(t, do(t, a, "GET", "/zoe/demo/insights/nope", nil, ck), 404, "unknown section")
	expectStatus(t, do(t, a, "GET", "/zoe/demo/insights/actions", nil, ck), 200, "actions")

	// The repository header links to Insights.
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo", nil, ck), 200, "repo")
	mustContain(t, body, "repo head", `href="/zoe/demo/insights"`)

	// Account insights.
	body = expectStatus(t, do(t, a, "GET", "/insights", nil, ck), 200, "account")
	mustContain(t, body, "account", "Across the 1 repository", "Contributions", "zoe/demo", "ins-cal")
}

func insSeedRun(t *testing.T, a *App, repoID int64, wfName, conclusion string, jobs ...store.ActionsJob) {
	t.Helper()
	_, _, err := a.db.CreateActionsRun(context.Background(), store.ActionsRun{RepoID: repoID, WorkflowPath: ".atelier/workflows/" + wfName + ".yml",
		WorkflowName: wfName, Event: "push", Status: store.RunCompleted, Conclusion: conclusion}, jobs)
	if err != nil {
		t.Fatal(err)
	}
}

func TestInsightsActionsUsage(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	rp := secRepo(t, a, "demo")
	start := time.Now().Add(-2 * time.Hour)
	job := func(name, runner, status string, mins float64) store.ActionsJob {
		return store.ActionsJob{JobKey: "build", Name: name, Status: status, RunnerName: runner,
			StartedAt: start, FinishedAt: start.Add(time.Duration(mins * float64(time.Minute)))}
	}
	insSeedRun(t, a, rp.ID, "ci", "success", job("test", "MacBook", "success", 10), job("lint", "MacBook", "success", 2))
	insSeedRun(t, a, rp.ID, "ci", "failure", job("test", "Server", "failure", 30))
	insSeedRun(t, a, rp.ID, "ci", "cancelled", job("test", "Server", "cancelled", 1), job("deploy", "Server", "skipped", 0))
	agent := store.ActionsJob{JobKey: workflow.AgentJobID, Name: "Agent: review", Status: "success", RunnerName: "MacBook",
		StartedAt: start, FinishedAt: start.Add(3 * time.Minute),
		Outputs: map[string]string{"model": "claude-sonnet-4-5", "input_tokens": "1000", "output_tokens": "500", "cost_usd": "0.25"}}
	insSeedRun(t, a, rp.ID, "review", "success", agent)
	// Too old to count.
	old := job("ancient", "MacBook", "success", 99)
	old.StartedAt, old.FinishedAt = time.Now().AddDate(0, 0, -45), time.Now().AddDate(0, 0, -45).Add(time.Hour)
	insSeedRun(t, a, rp.ID, "ci", "success", old)

	av, err := a.insActions(ctx, []store.Repo{rp}, 30)
	if err != nil {
		t.Fatal(err)
	}
	// 10 + 2 + 30 + 1 + 3 = 46 minutes over 5 jobs (the skipped job and the old one don't count).
	if av.Minutes != "46 min" || av.Jobs != 6 {
		t.Errorf("minutes = %q over %d jobs", av.Minutes, av.Jobs)
	}
	// Runs: ci ×3 (+1 old run, created now, so it counts as a run) and review ×1.
	// Success: (ci 2 success + review 1) / (3 success + 1 failure) = 75%.
	if av.Runs != 5 || av.Success != "75%" {
		t.Errorf("runs = %d, success = %q", av.Runs, av.Success)
	}
	if len(av.Workflows) != 2 || av.Workflows[0].Name != "ci" || av.Workflows[0].Minutes != "43 min" ||
		av.Workflows[0].Success != "66.7%" || av.Workflows[1].Name != "review" || av.Workflows[1].Success != "100%" {
		t.Errorf("workflows = %+v", av.Workflows)
	}
	if len(av.Runners) != 2 || av.Runners[0].Name != "Server" || av.Runners[0].Minutes != "31 min" || av.Runners[0].Jobs != 2 ||
		av.Runners[1].Name != "MacBook" || av.Runners[1].Minutes != "15 min" || av.Runners[1].Jobs != 3 {
		t.Errorf("runners = %+v", av.Runners)
	}
	if len(av.Slowest) == 0 || av.Slowest[0].Duration != "30m 00s" || av.Slowest[0].Runner != "Server" ||
		av.Slowest[0].URL != "/zoe/demo/actions/runs/2" {
		t.Errorf("slowest = %+v", av.Slowest)
	}
	if av.AgentRuns != 1 || av.AgentTokens != "1.5k tokens" || av.AgentCost != "≈ $0.25" {
		t.Errorf("agent = %d %q %q", av.AgentRuns, av.AgentTokens, av.AgentCost)
	}
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/insights/actions", nil, pullsCookie(t, a)), 200, "actions page")
	mustContain(t, body, "actions page", "46 min", "75%", "MacBook", "Server", "Slowest jobs", "1.5k tokens")
	body = expectStatus(t, do(t, a, "GET", "/insights", nil, pullsCookie(t, a)), 200, "account")
	mustContain(t, body, "account devices", "Actions time per device", "MacBook", "31 min")
}

func TestInsightsPermissions(t *testing.T) {
	f, work, _ := insFixture(t)
	a := f.a
	ctx := context.Background()
	saved := insWait
	insWait = 20 * time.Second
	t.Cleanup(func() { insWait = saved })
	// A public repository next to the private one.
	o, _ := a.db.PersonalOwner(ctx)
	if err := a.git.Init(ctx, "zoe", "open"); err != nil {
		t.Fatal(err)
	}
	pub, err := a.db.CreateRepo(ctx, o.ID, "open", "", false)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "push", "-q", a.git.Path("zoe", "open"), "main")
	rp := secRepo(t, a, "demo")
	insSeedRun(t, a, rp.ID, "secret-ci", "success", store.ActionsJob{JobKey: "build", Name: "build", Status: "success",
		RunnerName: "PrivateBox", StartedAt: time.Now().Add(-time.Hour), FinishedAt: time.Now()})

	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.db.CreateSessionFor(ctx, sam.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other := map[string]string{"Cookie": sessionCookie + "=" + sess}
	for _, p := range []string{"/zoe/demo/insights", "/zoe/demo/insights/commits", "/zoe/demo/insights/contributors",
		"/zoe/demo/insights/languages", "/zoe/demo/insights/actions", "/zoe/demo/insights/code-frequency"} {
		body := expectStatus(t, do(t, a, "GET", p, nil, other), 404, "sam "+p)
		if strings.Contains(body, "Ann Lee") {
			t.Errorf("%s leaks contributors", p)
		}
	}
	expectStatus(t, do(t, a, "GET", "/zoe/open/insights/contributors", nil, other), 200, "sam public repo")

	// Account insights only count what sam can read.
	a.insStatsFor(ctx, pub, 20*time.Second)
	a.insStatsFor(ctx, rp, 20*time.Second)
	body := expectStatus(t, do(t, a, "GET", "/insights", nil, other), 200, "sam account")
	mustContain(t, body, "sam account", "Across the 1 repository", "zoe/open")
	for _, leak := range []string{"zoe/demo", "PrivateBox", "secret-ci"} {
		if strings.Contains(body, leak) {
			t.Errorf("sam's insights mention %q", leak)
		}
	}
	body = expectStatus(t, do(t, a, "GET", "/insights", nil, pullsCookie(t, a)), 200, "owner account")
	mustContain(t, body, "owner account", "Across the 2 repositories", "zoe/demo", "zoe/open", "PrivateBox")
	// Anonymous visitors are sent to sign in.
	if rec := do(t, a, "GET", "/insights", nil, nil); rec.Code != 303 {
		t.Errorf("anonymous /insights = %d", rec.Code)
	}
}
