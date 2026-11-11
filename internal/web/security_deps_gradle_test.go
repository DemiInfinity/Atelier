package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/store"
)

// secFakeAndroid serves Maven Central, Google Maven, the Gradle Plugin
// Portal, services.gradle.org, npm and OSV for an Android-and-web fixture.
func secFakeAndroid(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var hits []string
	meta := func(vs ...string) string {
		return `<metadata><versioning><versions><version>` + strings.Join(vs, `</version><version>`) + `</version></versions></versioning></metadata>`
	}
	files := map[string]string{
		"/google/androidx/core/core-ktx/maven-metadata.xml":                                                  meta("1.15.0", "1.19.1", "1.20.0-alpha01"),
		"/google/androidx/compose/compose-bom/maven-metadata.xml":                                            meta("2026.06.01", "2026.09.00"),
		"/google/com/android/billingclient/billing-ktx/maven-metadata.xml":                                   meta("7.1.1", "9.1.0"),
		"/google/com/android/application/com.android.application.gradle.plugin/maven-metadata.xml":           meta("9.4.1", "9.5.1"),
		"/portal/org/jetbrains/kotlin/android/org.jetbrains.kotlin.android.gradle.plugin/maven-metadata.xml": meta("2.2.10", "2.2.20"),
		"/central/org/robolectric/robolectric/maven-metadata.xml":                                            meta("4.16", "4.17"),
		"/npm/svelte": `{"versions":{"5.57.0":{},"5.57.3":{}}}`,
		"/npm/vite":   `{"versions":{"8.2.2":{},"8.3.7":{},"9.0.0-beta.1":{}}}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /gradle/versions/all", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"version":"9.10.0-rc-1","rcFor":"9.10.0"},{"version":"9.9.0"},{"version":"9.8.0"}]`))
	})
	mux.HandleFunc("POST /osv", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Queries []struct {
				Package struct{ Name, Ecosystem string }
				Version string
			}
		}
		json.NewDecoder(r.Body).Decode(&req)
		var res struct {
			Results []map[string][]map[string]string `json:"results"`
		}
		for _, q := range req.Queries {
			mu.Lock()
			hits = append(hits, "osv "+q.Package.Ecosystem+":"+q.Package.Name+"@"+q.Version)
			mu.Unlock()
			m := map[string][]map[string]string{}
			if q.Package.Ecosystem == "Maven" && q.Package.Name == "org.robolectric:robolectric" && q.Version == "4.16" {
				m["vulns"] = []map[string]string{{"id": "GHSA-robo-0001"}}
			}
			res.Results = append(res.Results, m)
		}
		json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.Path)
		mu.Unlock()
		if b, ok := files[r.URL.Path]; ok {
			w.Write([]byte(b))
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), hits...) }
}

func secUseFakeAndroid(t *testing.T, a *App) func() []string {
	srv, hits := secFakeAndroid(t)
	old := []string{secOSVURL, secMavenCentralURL, secGoogleMavenURL, secPluginPortalURL, secGradleURL}
	secOSVURL, secMavenCentralURL, secGoogleMavenURL, secPluginPortalURL, secGradleURL =
		srv.URL+"/osv", srv.URL+"/central", srv.URL+"/google", srv.URL+"/portal", srv.URL+"/gradle"
	t.Cleanup(func() {
		secOSVURL, secMavenCentralURL, secGoogleMavenURL, secPluginPortalURL, secGradleURL = old[0], old[1], old[2], old[3], old[4]
	})
	if err := a.secSaveRegistries(context.Background(), srv.URL+"/go", srv.URL+"/npm"); err != nil {
		t.Fatal(err)
	}
	return hits
}

const secMarblApp = `plugins {
    id("com.android.application")
}

dependencies {
    val composeBom = platform("androidx.compose:compose-bom:2026.06.01")
    implementation(composeBom)
    implementation("androidx.compose.ui:ui")
    implementation("androidx.core:core-ktx:1.15.0")
    implementation("com.android.billingclient:billing-ktx:7.1.1")
    testImplementation("org.robolectric:robolectric:4.16")
}
`

const secMarblRoot = `plugins {
    id("com.android.application") version "9.4.1" apply false
    id("org.jetbrains.kotlin.android") version "2.2.10" apply false
}
`

func TestSecurityDependencyUpdatesAndroid(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	hits := secUseFakeAndroid(t, a)

	work := pullsWork(t, a)
	pullsCommit(t, work, "README.md", "# Marbl\n", "readme")
	pullsCommit(t, work, "android/settings.gradle.kts", "rootProject.name = \"Marbl\"\ninclude(\":app\")\n", "settings")
	pullsCommit(t, work, "android/build.gradle.kts", secMarblRoot, "root build")
	pullsCommit(t, work, "android/app/build.gradle.kts", secMarblApp, "app build")
	pullsCommit(t, work, "android/gradle/wrapper/gradle-wrapper.properties",
		"distributionBase=GRADLE_USER_HOME\ndistributionUrl=https\\://services.gradle.org/distributions/gradle-9.8.0-bin.zip\n", "wrapper")
	pullsCommit(t, work, "mosaicplus-web/package.json", "{\n  \"name\": \"mosaicplus-web\",\n  \"devDependencies\": {\n    \"svelte\": \"^5.57.0\",\n    \"vite\": \"^8.2.2\"\n  }\n}\n", "web")
	pullsCommit(t, work, "mosaicplus-web/node_modules/left-pad/package.json", `{"name":"left-pad","dependencies":{"nope":"1.0.0"}}`, "vendored")
	pullsCommit(t, work, "android/app/build/generated/build.gradle", `dependencies { implementation "com.example:gen:1.0.0" }`, "build output")
	pullsCommit(t, work, ".atelier/workflows/lockfiles.yml", "name: Lockfiles\non:\n  push:\n    branches: [\"atelier-updates/**\"]\njobs:\n  lockfiles:\n    runs-on: [self-hosted]\n    steps:\n      - uses: atelier/update-lockfiles@v1\n", "lockfile workflow")
	runGit(t, work, "push", "-q", "origin", "main")
	rp := secRepo(t, a, "demo")

	if !a.secHasManifest(ctx, rp) {
		t.Fatal("no manifests found")
	}
	if err := a.db.SetSecuritySettings(ctx, rp.ID, store.SecuritySettings{AllPRs: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetDepUpdateSettings(ctx, rp.ID, store.DepUpdateSettings{GroupMinor: true, ScheduleDay: 1, Lockfiles: true}); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second check opens nothing new
		if err := a.secCheckDeps(ctx, rp); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range hits() {
		if strings.Contains(h, "node_modules") || strings.Contains(h, "com/example/gen") || strings.Contains(h, "/npm/nope") {
			t.Errorf("looked up a skipped manifest's dependency: %s", h)
		}
	}
	prs, _ := a.db.Pulls(ctx, rp.ID, "open")
	byTitle := map[string]store.Pull{}
	for _, p := range prs {
		byTitle[p.Title] = p
	}
	if len(prs) != 3 {
		t.Errorf("open PRs = %v", keysOf(byTitle))
	}
	gradle, ok := byTitle["Update minor and patch dependencies (gradle) in android"]
	if !ok || !strings.HasPrefix(gradle.Head, "atelier-updates/gradle@android/_group/") {
		t.Fatalf("gradle group PR = %+v (have %v)", gradle, keysOf(byTitle))
	}
	for _, want := range []string{"| Package | From | To | Type | Advisories | Links |", "`androidx.core:core-ktx` in `android/app/build.gradle.kts` | `1.15.0` | `1.19.1` | minor",
		"`gradle` in `android/gradle/wrapper/gradle-wrapper.properties` | `9.8.0` | `9.9.0`", "`com.android.application` in `android/build.gradle.kts` | `9.4.1` | `9.5.1`",
		"[GHSA-robo-0001](https://osv.dev/vulnerability/GHSA-robo-0001) (fixes 1)", "atelier/update-lockfiles"} {
		if !strings.Contains(gradle.Body, want) {
			t.Errorf("gradle PR body lacks %q:\n%s", want, gradle.Body)
		}
	}
	g, _ := a.git.Open("zoe", "demo")
	app, _ := g.Blob(ctx, gradle.HeadSHA, "android/app/build.gradle.kts", 0)
	wantApp := strings.NewReplacer(`core-ktx:1.15.0`, `core-ktx:1.19.1`, `compose-bom:2026.06.01`, `compose-bom:2026.09.00`, `robolectric:4.16`, `robolectric:4.17`).Replace(secMarblApp)
	if string(app.Data) != wantApp {
		t.Errorf("app/build.gradle.kts on the group branch:\n%s", app.Data)
	}
	root, _ := g.Blob(ctx, gradle.HeadSHA, "android/build.gradle.kts", 0)
	if !strings.Contains(string(root.Data), `id("com.android.application") version "9.5.1" apply false`) ||
		!strings.Contains(string(root.Data), `id("org.jetbrains.kotlin.android") version "2.2.20" apply false`) {
		t.Errorf("root build on the group branch:\n%s", root.Data)
	}
	wrapper, _ := g.Blob(ctx, gradle.HeadSHA, "android/gradle/wrapper/gradle-wrapper.properties", 0)
	if !strings.Contains(string(wrapper.Data), "gradle-9.9.0-bin.zip") {
		t.Errorf("wrapper on the group branch:\n%s", wrapper.Data)
	}
	if commits, _ := g.CommitsBetween(ctx, "main", gradle.Head, 10); len(commits) != 3 {
		t.Errorf("group commits = %d (one per manifest)", len(commits))
	}

	// The bot's new branches start push workflows, as a push would.
	a.actions().bg.Wait()
	runs, _ := a.db.ActionsRuns(ctx, store.ActionsRunFilter{RepoID: rp.ID})
	pushed := map[string]bool{}
	for _, run := range runs {
		if run.Event == "push" && run.ActorName == secBotName {
			pushed[run.Ref] = true
		}
	}
	if len(runs) != 3 || !pushed["refs/heads/"+gradle.Head] {
		t.Errorf("push runs = %v (%d runs)", pushed, len(runs))
	}

	billing, ok := byTitle["Bump com.android.billingclient:billing-ktx from 7.1.1 to 9.1.0 in android/app"]
	if !ok || billing.Head != "atelier-updates/gradle@android+app/com.android.billingclient-billing-ktx/9.1.0" {
		t.Errorf("major update PR = %+v (have %v)", billing, keysOf(byTitle))
	}
	web, ok := byTitle["Update minor and patch dependencies (npm) in mosaicplus-web"]
	if !ok || !strings.HasPrefix(web.Head, "atelier-updates/npm@mosaicplus-web/_group/") {
		t.Errorf("npm group PR = %+v (have %v)", web, keysOf(byTitle))
	}
	pj, _ := g.Blob(ctx, web.HeadSHA, "mosaicplus-web/package.json", 0)
	if !strings.Contains(string(pj.Data), `"svelte": "^5.57.3"`) || !strings.Contains(string(pj.Data), `"vite": "^8.3.7"`) {
		t.Errorf("package.json on the npm group branch:\n%s", pj.Data)
	}

	// The results list every dependency with its manifest and pull request.
	ups, _ := a.db.DepUpdates(ctx, rp.ID)
	byName := map[string]store.DepUpdate{}
	for _, u := range ups {
		byName[u.Name] = u
	}
	if u := byName["androidx.core:core-ktx"]; u.PRNumber != gradle.Number || u.Manifest != "android/app/build.gradle.kts" || u.Ecosystem != "maven" {
		t.Errorf("core-ktx row = %+v", u)
	}
	if u := byName["gradle"]; u.PRNumber != gradle.Number || u.Latest != "9.9.0" {
		t.Errorf("gradle row = %+v", u)
	}
	if u := byName["svelte"]; u.PRNumber != web.Number || u.Latest != "5.57.3" {
		t.Errorf("svelte row = %+v", u)
	}

	// Closing the group PR doesn't bring the same group back.
	if err := a.db.ClosePull(ctx, gradle.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.secCheckDeps(ctx, rp); err != nil {
		t.Fatal(err)
	}
	if open, _ := a.db.Pulls(ctx, rp.ID, "open"); len(open) != 2 {
		t.Errorf("open PRs after closing the group = %d", len(open))
	}
	if all, _ := a.db.Pulls(ctx, rp.ID, ""); len(all) != 3 {
		t.Errorf("all PRs after closing the group = %d", len(all))
	}

	// The page shows the manifests and the new settings.
	ck := pullsCookie(t, a)
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/security", nil, ck), 200, "page")
	mustContain(t, body, "page", "android/app/build.gradle.kts", "androidx.core:core-ktx", "Group minor and patch updates",
		"Regenerate lockfiles", `<option value="1" selected>Monday</option>`, "Mondays", "atelier/update-lockfiles@v1")

	// Saving the settings card stores the update options.
	form := url.Values{"push_protection": {"on"}, "security_prs": {"on"}, "group_minor": {"on"}, "schedule_day": {"4"}}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/security/settings", form, ck), 303, "save")
	if st, _ := a.db.DepUpdateSettings(ctx, rp.ID); !st.GroupMinor || st.ScheduleDay != 4 || st.Lockfiles {
		t.Errorf("saved settings = %+v", st)
	}
}

func keysOf(m map[string]store.Pull) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSecurityLockfileJobPushPolicy(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	work := pullsWork(t, a)
	sha := pullsCommit(t, work, "package.json", `{"dependencies":{}}`, "init")
	runGit(t, work, "push", "-q", "origin", "main")
	rp := secRepo(t, a, "demo")
	const branch = "atelier-updates/npm/left-pad/2.0.0"
	pr, err := a.db.CreatePull(ctx, store.NewPull{RepoID: rp.ID, Title: "Bump", Head: branch, Base: "main", HeadSHA: sha, AuthorKind: "bot", AuthorName: secBotName})
	if err != nil {
		t.Fatal(err)
	}
	job := func(ref, status string) Actor {
		_, jobs, err := a.db.CreateActionsRun(ctx, store.ActionsRun{RepoID: rp.ID, WorkflowPath: ".atelier/workflows/l.yml", Event: "push", Ref: ref, SHA: sha,
			Status: store.RunQueued, ActorKind: "owner", ActorName: "zoe"}, []store.ActionsJob{{JobKey: "l", Name: "l", Status: status}})
		if err != nil {
			t.Fatal(err)
		}
		return Actor{Kind: ActorJob, Name: fmt.Sprintf("actions-job-%d", jobs[0].ID),
			Scopes: []string{"repo:read", fmt.Sprintf("actions:repo:%d", rp.ID), fmt.Sprintf("actions:job:%d", jobs[0].ID), "packages:read"}}
	}
	onBranch := job("refs/heads/"+branch, "running")
	onMain := job("refs/heads/main", "running")
	finished := job("refs/heads/"+branch, "success")

	if a.can(ctx, onBranch, rp, PermPush) {
		t.Error("job push allowed while lockfile regeneration is off")
	}
	if err := a.db.SetDepUpdateSettings(ctx, rp.ID, store.DepUpdateSettings{Lockfiles: true, ScheduleDay: -1}); err != nil {
		t.Fatal(err)
	}
	if !a.can(ctx, onBranch, rp, PermPush) {
		t.Error("lockfile job can't push to its update branch")
	}
	for name, who := range map[string]Actor{"main": onMain, "finished": finished} {
		if a.can(ctx, who, rp, PermPush) {
			t.Errorf("%s job may push", name)
		}
	}
	// Push access isn't write access: no merges, releases or settings.
	if a.can(ctx, onBranch, rp, PermWrite) || a.can(ctx, onBranch, rp, PermAdmin) {
		t.Error("lockfile job got write or admin")
	}
	// Another repository.
	o := secOwner(t, a)
	other, err := a.db.CreateRepo(ctx, o.ID, "other", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if a.can(ctx, onBranch, other, PermPush) {
		t.Error("job may push to another repository")
	}
	// Once the bot's pull request is closed, the branch is off limits.
	if err := a.db.ClosePull(ctx, pr.ID); err != nil {
		t.Fatal(err)
	}
	if a.can(ctx, onBranch, rp, PermPush) {
		t.Error("job may push after the PR closed")
	}
}

func TestSecurityDepsSchedule(t *testing.T) {
	mon := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC) // a Monday
	for name, c := range map[string]struct {
		last time.Time
		now  time.Time
		day  int
		want bool
	}{
		"weekly, never":            {time.Time{}, mon, -1, true},
		"weekly, 3 days":           {mon.Add(-72 * time.Hour), mon, -1, false},
		"weekly, 8 days":           {mon.Add(-8 * 24 * time.Hour), mon, -1, true},
		"monday, on monday":        {mon.Add(-7 * 24 * time.Hour), mon, 1, true},
		"monday, already today":    {mon.Add(-time.Hour), mon, 1, false},
		"monday, on tuesday":       {mon.Add(-6 * 24 * time.Hour), mon.Add(24 * time.Hour), 1, false},
		"monday, never, on monday": {time.Time{}, mon, 1, true},
	} {
		if got := secDepsDue(c.last, c.now, c.day); got != c.want {
			t.Errorf("%s: due = %v", name, got)
		}
	}
}

// When HEAD names a missing branch, git falls back to the newest branch,
// which can be one of the bot's: the check refuses rather than basing
// updates on an update branch.
func TestSecurityDepsNeverBaseOnUpdateBranch(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	work := pullsWork(t, a)
	pullsCommit(t, work, "package.json", `{"dependencies":{"left-pad":"^1.2.0"}}`, "init")
	runGit(t, work, "push", "-q", "origin", "main:atelier-updates/npm/left-pad/2.0.0")
	rp := secRepo(t, a, "demo")
	err := a.secCheckDeps(ctx, rp)
	if err == nil || !strings.Contains(err.Error(), "default branch") {
		t.Errorf("check on a repository without its default branch = %v", err)
	}
	if prs, _ := a.db.Pulls(ctx, rp.ID, ""); len(prs) != 0 {
		t.Errorf("opened %d PRs", len(prs))
	}
}
