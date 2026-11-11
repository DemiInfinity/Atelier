package web

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"atelier/internal/actions"
	"atelier/internal/secretscan"
	"atelier/internal/store"
)

// Fake secrets are assembled at runtime so this file holds nothing a
// scanner would flag.
var secRng = rand.New(rand.NewPCG(7, 9))

func secFakeGitHub() string {
	const set = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 36)
	for i := range b {
		b[i] = set[secRng.IntN(len(set))]
	}
	return "gh" + "p_" + string(b)
}

func secOwner(t *testing.T, a *App) store.Owner {
	t.Helper()
	o, err := a.db.PersonalOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func secRepo(t *testing.T, a *App, name string) store.Repo {
	t.Helper()
	rp, err := a.db.RepoByName(context.Background(), "zoe", name)
	if err != nil {
		t.Fatal(err)
	}
	return rp
}

func TestSecurityPagePermissions(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	work := pullsWork(t, a)
	pullsCommit(t, work, "README.md", "# demo\n", "First")
	runGit(t, work, "push", "-q", "origin", "main")
	rp := secRepo(t, a, "demo")
	gh := secFakeGitHub()
	fp := secretscan.Fingerprint(gh)
	if _, _, err := a.db.RecordSecretAlert(ctx, store.SecretAlert{RepoID: rp.ID, Kind: "code", Source: "history", RuleID: "github-pat",
		RuleName: "GitHub personal access token", Fingerprint: fp, Redacted: secretscan.Redact(gh), Path: "config/app.env", Line: 3,
		CommitSHA: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	if err := a.db.ReplaceDepUpdates(ctx, rp.ID, []store.DepUpdate{{RepoID: rp.ID, Ecosystem: "go", Name: "example.com/lib", Manifest: "go.mod",
		Current: "v1.0.0", Latest: "v1.1.0", NewSpec: "v1.1.0", Kind: "minor", Vulns: 2, VulnIDs: "GO-1,GO-2"}}); err != nil {
		t.Fatal(err)
	}

	owner := pullsCookie(t, a)
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/security", nil, owner), 200, "owner security page")
	mustContain(t, body, "owner page", "Secrets found", "Known vulnerabilities", "GitHub personal access token", secretscan.Redact(gh),
		"config/app.env:3", "Rotate and store as a secret", "/zoe/demo/settings/secrets", "example.com/lib", "v1.0.0 → v1.1.0",
		"2 advisories", "Scan history now", "Check now", "Block pushes that contain secrets", `href="/zoe/demo/security" aria-current="page"`)
	if strings.Contains(body, gh) {
		t.Fatal("page shows the full secret")
	}

	// A second person: sees counts on a public repository, never details.
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.db.CreateSessionFor(ctx, sam.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other := map[string]string{"Cookie": sessionCookie + "=" + sess}
	expectStatus(t, do(t, a, "GET", "/zoe/demo/security", nil, other), 404, "private repo for sam")
	if err := a.db.UpdateRepo(ctx, rp.ID, "", false); err != nil {
		t.Fatal(err)
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/security", nil, other), 200, "public repo for sam")
	mustContain(t, body, "sam's page", "Secrets found", "Details are for repository admins")
	for _, secret := range []string{secretscan.Redact(gh), fp, "config/app.env", "example.com/lib", "Allow once", "Scan history now"} {
		if strings.Contains(body, secret) {
			t.Errorf("sam sees %q", secret)
		}
	}
	for _, post := range []string{"/zoe/demo/security/settings", "/zoe/demo/security/scan", "/zoe/demo/security/dependencies/check", "/zoe/demo/security/alerts/1"} {
		expectStatus(t, do(t, a, "POST", post, url.Values{"status": {"resolved"}}, other), 403, "sam POST "+post)
	}

	// The owner manages the alert and settings.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/security/alerts/1", url.Values{"status": {"false_positive"}}, owner), 303, "dismiss")
	if n, _ := a.db.OpenSecretAlertCount(ctx, rp.ID); n != 0 {
		t.Errorf("open alerts after dismissing = %d", n)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/security/alerts/1", url.Values{"status": {"bogus"}}, owner), 400, "bogus status")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/security/settings", url.Values{"scan_logs": {"on"}, "all_prs": {"on"},
		"goproxy": {"https://goproxy.test/a, direct"}, "npm_registry": {"javascript:alert(1)"}}, owner), 303, "save settings")
	st, _ := a.db.SecuritySettings(ctx, rp.ID)
	if st.PushProtection || !st.ScanLogs || st.SecurityPRs || !st.AllPRs {
		t.Errorf("settings = %+v", st)
	}
	if got := a.secGoProxySetting(ctx); got != "https://goproxy.test/a, direct" {
		t.Errorf("goproxy = %q", got)
	}
	if got := a.secNpmRegistrySetting(ctx); got != defaultSecNpmRegistry {
		t.Errorf("bad npm registry saved: %q", got)
	}
}

func TestSecurityLogScanning(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	rp := secRepo(t, a, "demo")
	gh := secFakeGitHub()
	run, jobs, err := a.db.CreateActionsRun(ctx, store.ActionsRun{RepoID: rp.ID, WorkflowPath: ".atelier/workflows/ci.yml", WorkflowName: "ci",
		Event: "push", Ref: "refs/heads/feature/ssh", SHA: strings.Repeat("c", 40), Status: store.RunCompleted, Conclusion: "success"},
		[]store.ActionsJob{{JobKey: "deploy", Name: "deploy", Status: "success"}})
	if err != nil {
		t.Fatal(err)
	}
	job := jobs[0]
	if err := a.db.ResetActionsSteps(ctx, job.ID, []string{"Checkout", "deploy"}); err != nil {
		t.Fatal(err)
	}
	chunk := func(lines ...actions.LogLine) {
		b, _ := json.Marshal(lines)
		if _, err := a.db.AppendActionsLog(ctx, job.ID, string(b), int64(len(b)), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	chunk(actions.LogLine{Step: 0, Text: "Cloning"}, actions.LogLine{Step: 0, Text: "done"})
	chunk(actions.LogLine{Step: 1, Text: "deploying with ***"}, actions.LogLine{Step: 1, Text: "debug: auth header token " + gh + " ok"})

	a.emit(ctx, Event{Kind: EventRunCompleted, Repo: rp, RepoID: rp.ID, Number: run.Seq, Title: "ci #1 succeeded", Actor: Actor{Kind: ActorSystem, Name: "actions"}})
	secBackground.Wait()

	alerts, err := a.db.SecretAlerts(ctx, rp.ID, 0)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("alerts = %+v, %v", alerts, err)
	}
	al := alerts[0]
	if al.Kind != "log" || al.RunSeq != run.Seq || al.JobName != "deploy" || al.StepName != "deploy" || al.Line != 4 ||
		al.Fingerprint != secretscan.Fingerprint(gh) || al.Redacted != secretscan.Redact(gh) {
		t.Errorf("alert = %+v", al)
	}
	logs, _ := a.db.ActionsLogs(ctx, job.ID, 0, 0)
	all := ""
	for _, c := range logs {
		all += c.Lines
	}
	if strings.Contains(all, gh) || !strings.Contains(all, "auth header token *** ok") {
		t.Errorf("stored log not masked: %s", all)
	}
	notes, _ := a.db.Notifications(ctx, store.NotificationQuery{PersonID: secOwner(t, a).ID})
	found := false
	for _, n := range notes {
		found = found || n.Kind == EventSecretDetected && n.RepoID == rp.ID
	}
	if !found {
		t.Error("no secret.detected notification")
	}
	// The run page link is shown to the owner.
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/security", nil, pullsCookie(t, a)), 200, "page")
	mustContain(t, body, "page", "Actions run #1 · deploy · step “deploy” · line 4", "/zoe/demo/actions/runs/1")

	// A second scan of the same run adds nothing; with log scanning off nothing is scanned.
	if n, err := a.secScanRunLogs(ctx, rp.ID, run.Seq); err != nil || n != 0 {
		t.Errorf("rescan = %d, %v", n, err)
	}
	if err := a.db.SetSecuritySettings(ctx, rp.ID, store.SecuritySettings{}); err != nil {
		t.Fatal(err)
	}
	chunk(actions.LogLine{Step: 1, Text: "again " + secFakeGitHub()})
	if n, err := a.secScanRunLogs(ctx, rp.ID, run.Seq); err != nil || n != 0 {
		t.Errorf("scan with log scanning off = %d, %v", n, err)
	}
}

func TestSecurityHistoryScan(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	rp := secRepo(t, a, "demo")
	work := pullsWork(t, a)
	first, second, third := secFakeGitHub(), secFakeGitHub(), secFakeGitHub()
	pullsCommit(t, work, "a.env", "NAME=x\nTOKEN="+first+"\n", "Add env")
	pullsCommit(t, work, ".atelier/secret-scanning.yml", "paths:\n  - testdata/**\n", "Allowlist")
	pullsCommit(t, work, "testdata/fixture.env", "TOKEN="+second+"\n", "Fixture")
	pullsCommit(t, work, "src/b.go", "package b\n\nconst k = \""+third+"\"\n", "Oops")
	runGit(t, work, "push", "-q", "origin", "main")

	// Bounded: only the newest commit.
	old := secHistoryMaxCommits
	secHistoryMaxCommits = 1
	sc := a.secScanHistory(ctx, rp)
	secHistoryMaxCommits = old
	if sc.Error != "" || sc.Commits != 1 || !sc.Truncated || sc.Findings != 1 {
		t.Fatalf("bounded scan = %+v", sc)
	}

	expectStatus(t, do(t, a, "POST", "/zoe/demo/security/scan", nil, pullsCookie(t, a)), 303, "scan now")
	secBackground.Wait()
	st, err := a.db.SecurityScanState(ctx, rp.ID, "history")
	if err != nil || st.Status != "done" || st.Commits != 4 || st.Truncated || st.Findings != 2 {
		t.Fatalf("scan state = %+v, %v", st, err)
	}
	alerts, _ := a.db.SecretAlerts(ctx, rp.ID, 0)
	got := map[string]store.SecretAlert{}
	for _, al := range alerts {
		got[al.Fingerprint] = al
	}
	if len(alerts) != 2 || got[secretscan.Fingerprint(first)].Path != "a.env" || got[secretscan.Fingerprint(first)].Line != 2 ||
		got[secretscan.Fingerprint(third)].Path != "src/b.go" || got[secretscan.Fingerprint(third)].Line != 3 {
		t.Errorf("alerts = %+v", alerts)
	}
	if _, ok := got[secretscan.Fingerprint(second)]; ok {
		t.Error("allowlisted path reported")
	}
}

// secFakeRegistries serves a Go module proxy, an npm registry and OSV, and
// counts what was asked.
func secFakeRegistries(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /go/example.com/one/@v/list", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("v1.2.3\nv1.2.4\nv1.3.0\n"))
	})
	mux.HandleFunc("GET /npm/left-pad", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"left-pad","versions":{"1.2.0":{},"1.3.0":{},"2.0.0":{}}}`))
	})
	mux.HandleFunc("POST /osv", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Queries []struct {
				Package struct{ Name, Ecosystem string }
				Version string
			}
		}
		json.NewDecoder(r.Body).Decode(&req)
		type vuln struct {
			ID string `json:"id"`
		}
		var res struct {
			Results []map[string][]vuln `json:"results"`
		}
		for _, q := range req.Queries {
			m := map[string][]vuln{}
			if q.Package.Ecosystem == "Go" && q.Package.Name == "example.com/one" && q.Version == "v1.2.3" {
				m["vulns"] = []vuln{{"GO-2026-0001"}}
			}
			res.Results = append(res.Results, m)
		}
		json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("fake registry: unexpected %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSecurityDependencyUpdates(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	srv := secFakeRegistries(t)
	oldOSV := secOSVURL
	secOSVURL = srv.URL + "/osv"
	t.Cleanup(func() { secOSVURL = oldOSV })
	if err := a.secSaveRegistries(ctx, srv.URL+"/go", srv.URL+"/npm"); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetSetting(ctx, settingSecurityBaseURL, "https://atelier.test"); err != nil {
		t.Fatal(err)
	}

	// A module hosted in Atelier itself, resolved through Atelier's Go proxy.
	o := secOwner(t, a)
	if err := a.git.Init(ctx, "zoe", "lib"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.CreateRepo(ctx, o.ID, "lib", "", true); err != nil {
		t.Fatal(err)
	}
	lib := t.TempDir()
	runGit(t, lib, "init", "-q", "--initial-branch=main", ".")
	runGit(t, lib, "remote", "add", "origin", a.git.Path("zoe", "lib"))
	pullsCommit(t, lib, "go.mod", "module atelier.test/zoe/lib\n", "lib")
	runGit(t, lib, "tag", "v1.0.0")
	pullsCommit(t, lib, "lib.go", "package lib\n", "more")
	runGit(t, lib, "tag", "v1.1.0")
	runGit(t, lib, "push", "-q", "origin", "main", "--tags")

	work := pullsWork(t, a)
	gomod := "module atelier.test/zoe/demo\n\ngo 1.22\n\nrequire (\n\texample.com/one v1.2.3\n\tatelier.test/zoe/lib v1.0.0\n)\n"
	pullsCommit(t, work, "go.mod", gomod, "go.mod")
	pullsCommit(t, work, "package.json", "{\n  \"name\": \"demo\",\n  \"dependencies\": {\n    \"left-pad\": \"^1.2.0\"\n  }\n}\n", "package.json")
	runGit(t, work, "push", "-q", "origin", "main")
	rp := secRepo(t, a, "demo")

	// Default settings: security PRs only.
	if err := a.secCheckDeps(ctx, rp); err != nil {
		t.Fatal(err)
	}
	ups, _ := a.db.DepUpdates(ctx, rp.ID)
	by := map[string]store.DepUpdate{}
	for _, u := range ups {
		by[u.Name] = u
	}
	one, pad, local := by["example.com/one"], by["left-pad"], by["atelier.test/zoe/lib"]
	if one.Latest != "v1.3.0" || one.Kind != "minor" || one.Vulns != 1 || one.Fixes != 1 || one.PRNumber == 0 {
		t.Errorf("example.com/one = %+v", one)
	}
	if pad.Latest != "2.0.0" || pad.NewSpec != "^2.0.0" || pad.Kind != "major" || pad.PRNumber != 0 {
		t.Errorf("left-pad = %+v", pad)
	}
	if local.Latest != "v1.1.0" || local.Kind != "minor" {
		t.Errorf("local module = %+v", local)
	}
	if n, v, _ := a.db.DepCounts(ctx, rp.ID); n != 3 || v != 1 {
		t.Errorf("counts = %d updates, %d vulns", n, v)
	}
	pr, err := a.db.PullByNumber(ctx, rp.ID, one.PRNumber)
	if err != nil {
		t.Fatal(err)
	}
	if pr.AuthorKind != "bot" || pr.AuthorName != "atelier-updates" || pr.Base != "main" || pr.Head != "atelier-updates/go/example.com-one/1.3.0" ||
		!strings.Contains(pr.Body, "go.sum") || !strings.Contains(pr.Body, "GO-2026-0001") || !pr.IsOpen() {
		t.Errorf("pull = %+v", pr)
	}
	g, _ := a.git.Open("zoe", "demo")
	b, err := g.Blob(ctx, pr.HeadSHA, "go.mod", 0)
	if err != nil || !strings.Contains(string(b.Data), "example.com/one v1.3.0") || !strings.Contains(string(b.Data), "atelier.test/zoe/lib v1.0.0") {
		t.Errorf("go.mod on the PR branch:\n%s", b.Data)
	}
	if commits, _ := g.CommitsBetween(ctx, "main", pr.Head, 10); len(commits) != 1 || commits[0].Author != "atelier-updates" {
		t.Errorf("PR commits = %+v", commits)
	}

	// Idempotent: checking again opens nothing new.
	if err := a.secCheckDeps(ctx, rp); err != nil {
		t.Fatal(err)
	}
	if prs, _ := a.db.Pulls(ctx, rp.ID, "open"); len(prs) != 1 {
		t.Errorf("open pulls after second check = %d", len(prs))
	}

	// All updates: one PR per dependency, still none duplicated.
	if err := a.db.SetSecuritySettings(ctx, rp.ID, store.SecuritySettings{AllPRs: true}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.secCheckDeps(ctx, rp); err != nil {
			t.Fatal(err)
		}
	}
	prs, _ := a.db.Pulls(ctx, rp.ID, "open")
	heads := map[string]bool{}
	for _, p := range prs {
		heads[p.Head] = true
	}
	if len(prs) != 3 || !heads["atelier-updates/npm/left-pad/2.0.0"] || !heads["atelier-updates/go/atelier.test-zoe-lib/1.1.0"] {
		t.Errorf("open pulls = %v", heads)
	}
	ups, _ = a.db.DepUpdates(ctx, rp.ID)
	for _, u := range ups {
		if u.Name == "left-pad" {
			b, _ := g.BranchSHA(ctx, "atelier-updates/npm/left-pad/2.0.0")
			blob, _ := g.Blob(ctx, b, "package.json", 0)
			if !strings.Contains(string(blob.Data), `"left-pad": "^2.0.0"`) || u.PRNumber == 0 {
				t.Errorf("left-pad PR: %+v\n%s", u, blob.Data)
			}
		}
	}
	// A closed PR for the same version isn't reopened or duplicated.
	if err := a.db.ClosePull(ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.secCheckDeps(ctx, rp); err != nil {
		t.Fatal(err)
	}
	if prs, _ := a.db.Pulls(ctx, rp.ID, "open"); len(prs) != 2 {
		t.Errorf("open pulls after closing one = %d", len(prs))
	}

	// "Check now" runs in the background and the page lists the results.
	ck := pullsCookie(t, a)
	expectStatus(t, do(t, a, "POST", "/zoe/demo/security/dependencies/check", nil, ck), 303, "check now")
	secBackground.Wait()
	if st, err := a.db.SecurityScanState(ctx, rp.ID, "deps"); err != nil || st.Status != "done" {
		t.Errorf("deps scan = %+v, %v", st, err)
	}
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/security", nil, ck), 200, "page")
	mustContain(t, body, "page", "example.com/one", "left-pad", "^1.2.0 → ^2.0.0", "major", "1 advisory", "Pull request #")
}
