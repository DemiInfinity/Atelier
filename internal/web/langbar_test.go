package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// langFixture: zoe/demo (private) with known sizes. Counted: Go 700 bytes,
// TypeScript 200, CSS 100 (70% / 20% / 10%). Not counted: Markdown, docs/,
// vendor/.
func langFixture(t *testing.T) (gitFixture, string) {
	t.Helper()
	f := newGitFixture(t)
	work := pullsWork(t, f.a)
	pullsCommit(t, work, "main.go", strings.Repeat("g", 700), "go")
	pullsCommit(t, work, "web/app.ts", strings.Repeat("t", 200), "ts")
	pullsCommit(t, work, "web/site.css", strings.Repeat("c", 100), "css")
	pullsCommit(t, work, "README.md", strings.Repeat("m", 5000), "readme")
	pullsCommit(t, work, "docs/example.go", strings.Repeat("d", 5000), "docs")
	pullsCommit(t, work, "vendor/lib/x.js", strings.Repeat("v", 5000), "vendor")
	runGit(t, work, "push", "-q", "origin", "main")
	return f, work
}

// langWaitFor waits until the language cache is at head.
func langWaitFor(t *testing.T, a *App, repoID int64, head string) *langSummary {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if s := a.langsCached(context.Background(), repoID); s != nil && s.Head == head {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("languages for %s never computed", head)
	return nil
}

// langSection cuts the Languages section out of a page.
func langSection(body string) string {
	i := strings.Index(body, `<section class="stack-sm langbar-section"`)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</section>")
	return body[i : i+j]
}

func TestLangBarRepoOverview(t *testing.T) {
	f, work := langFixture(t)
	a := f.a
	ck := pullsCookie(t, a)
	rp := secRepo(t, a, "demo")
	head := runGit(t, work, "rev-parse", "HEAD")

	// Nothing cached: the first view doesn't wait, shows no section and starts the scan.
	before := langsComputeCount.Load()
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo", nil, ck), 200, "first view")
	if langSection(body) != "" && a.langsCached(context.Background(), rp.ID) == nil {
		t.Error("a section was drawn without any counted languages")
	}
	if langsComputeCount.Load() == before {
		t.Fatal("the first view didn't start counting languages")
	}
	s := langWaitFor(t, a, rp.ID, head)
	if s.Bytes != 1000 || len(s.Langs) != 3 {
		t.Fatalf("summary = %+v", s)
	}

	body = expectStatus(t, do(t, a, "GET", "/zoe/demo", nil, ck), 200, "second view")
	sec := langSection(body)
	if sec == "" {
		t.Fatal("no Languages section once counted")
	}
	mustContain(t, sec, "section", ">Languages</h2>", `<svg class="langbar"`, `aria-hidden="true"`,
		`<ul class="langbar-legend" aria-label="Languages by share of code">`,
		`<span class="langbar-name">Go</span> <span class="langbar-pct">70.0%</span>`,
		`<span class="langbar-name">TypeScript</span> <span class="langbar-pct">20.0%</span>`,
		`<span class="langbar-name">CSS</span> <span class="langbar-pct">10.0%</span>`,
		`href="/zoe/demo/insights/languages"`, `fill="`+langColor("Go")+`"`, `<rect x="0.00" y="0" width="697.00"`)
	for _, bad := range []string{"style=", "Markdown", "JavaScript", "Other"} {
		if strings.Contains(sec, bad) {
			t.Errorf("section contains %q:\n%s", bad, sec)
		}
	}
	// The language link resolves.
	expectStatus(t, do(t, a, "GET", "/zoe/demo/insights/languages", nil, ck), 200, "insights languages")
	// Subfolders don't show the bar.
	if sec := langSection(expectStatus(t, do(t, a, "GET", "/zoe/demo/tree/main/web", nil, ck), 200, "subfolder")); sec != "" {
		t.Error("a subfolder shows the language bar")
	}
	// The owner's repository list shows the primary language with its dot.
	body = expectStatus(t, do(t, a, "GET", "/zoe", nil, ck), 200, "owner page")
	mustContain(t, body, "owner page", `<span class="lang-tag"><svg class="lang-dot"`, `<span>Go</span></span>`)
	body = expectStatus(t, do(t, a, "GET", "/", nil, ck), 200, "home")
	mustContain(t, body, "home", `<span class="lang-tag">`)
}

func TestLangBarUncachedHeadDoesNotBlock(t *testing.T) {
	f, work := langFixture(t)
	a := f.a
	ck := pullsCookie(t, a)
	rp := secRepo(t, a, "demo")
	head := runGit(t, work, "rev-parse", "HEAD")
	a.repoLangs(context.Background(), rp, nil)
	langWaitFor(t, a, rp.ID, head)

	// A new commit; hold every scan slot so the scan can't finish.
	pullsCommit(t, work, "web/more.css", strings.Repeat("c", 1000), "more css")
	runGit(t, work, "push", "-q", "origin", "main")
	head2 := runGit(t, work, "rev-parse", "HEAD")
	for i := 0; i < cap(langsSem); i++ {
		langsSem <- struct{}{}
	}
	released := false
	release := func() {
		if !released {
			for i := 0; i < cap(langsSem); i++ {
				<-langsSem
			}
			released = true
		}
	}
	t.Cleanup(release)
	before := langsComputeCount.Load()
	start := time.Now()
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo", nil, ck), 200, "view behind")
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("the page waited %v", d)
	}
	if langsComputeCount.Load() != before+1 {
		t.Errorf("scans started = %d, want 1", langsComputeCount.Load()-before)
	}
	// The last known bar is shown meanwhile.
	mustContain(t, langSection(body), "stale section", "70.0%")
	// A second view while it's running doesn't start another.
	expectStatus(t, do(t, a, "GET", "/zoe/demo", nil, ck), 200, "view again")
	if n := langsComputeCount.Load() - before; n != 1 {
		t.Errorf("scans started = %d, want 1", n)
	}
	release()
	s := langWaitFor(t, a, rp.ID, head2)
	if s.Bytes != 2000 || s.Primary() != "CSS" {
		t.Errorf("after the scan: %+v", s)
	}
	mustContain(t, langSection(expectStatus(t, do(t, a, "GET", "/zoe/demo", nil, ck), 200, "fresh")), "fresh section",
		`<span class="langbar-name">CSS</span> <span class="langbar-pct">55.0%</span>`)
}

func TestLangBarInsightsFillsCache(t *testing.T) {
	f, work := langFixture(t)
	a := f.a
	rp := secRepo(t, a, "demo")
	head := runGit(t, work, "rev-parse", "HEAD")
	if r := a.insStatsFor(context.Background(), rp, 20*time.Second); r.Stats == nil {
		t.Fatal("insights didn't compute")
	}
	// Insights saved the language entry too: no separate scan.
	before := langsComputeCount.Load()
	s, current, _ := a.repoLangs(context.Background(), rp, nil)
	if !current || s == nil || s.Head != head || s.Primary() != "Go" || langsComputeCount.Load() != before {
		t.Errorf("repoLangs after insights = %+v current=%v scans=%d", s, current, langsComputeCount.Load()-before)
	}
}

func TestLangBarEmptyRepo(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo", nil, pullsCookie(t, a)), 200, "empty repo")
	if strings.Contains(body, "langbar") {
		t.Error("an empty repository shows a Languages section")
	}
	before := langsComputeCount.Load()
	if s, _, empty := a.repoLangs(context.Background(), secRepo(t, a, "demo"), nil); !empty || s != nil {
		t.Errorf("empty repo: %+v empty=%v", s, empty)
	}
	if langsComputeCount.Load() != before {
		t.Error("an empty repository started a scan")
	}
}

func TestLangBarOtherGrouping(t *testing.T) {
	var parts []langPart
	for i, n := range []string{"Go", "TypeScript", "CSS", "HTML", "Shell", "Python", "Rust", "Ruby", "Lua", "Zig"} {
		parts = append(parts, langPart{Name: n, Color: langColor(n), Bytes: int64(10000 - i*500)})
	}
	parts = append(parts, langPart{Name: "Nix", Color: langColor("Nix"), Bytes: 20}) // < 0.1%
	b := buildLangBar(parts, func(string) (string, bool) { return "/x", false })
	if len(b.Entries) != langMaxEntries {
		t.Fatalf("entries = %d, want %d", len(b.Entries), langMaxEntries)
	}
	last := b.Entries[len(b.Entries)-1]
	if last.Name != "Other" || last.Color != langOtherColor {
		t.Fatalf("last entry = %+v", last)
	}
	// Other = Ruby + Lua + Zig + Nix.
	var total int64
	for _, p := range parts {
		total += p.Bytes
	}
	want := langPct(float64(6500+6000+5500+20) / float64(total) * 100)
	if last.Pct != want {
		t.Errorf("Other = %s, want %s", last.Pct, want)
	}
	// Only slivers: a short list plus Other.
	b = buildLangBar([]langPart{{Name: "Go", Bytes: 100000}, {Name: "C", Bytes: 50}, {Name: "Lua", Bytes: 40}}, nil)
	if len(b.Entries) != 2 || b.Entries[0].Pct != "99.9%" || b.Entries[1].Name != "Other" || b.Entries[1].Pct != "0.1%" {
		t.Errorf("slivers = %+v", b.Entries)
	}
	if strings.Contains(string(b.SVG), "style=") || !strings.HasPrefix(string(b.SVG), "<svg") {
		t.Errorf("svg = %s", b.SVG)
	}
	if buildLangBar(nil, nil) != nil || buildLangBar([]langPart{{Name: "Go"}}, nil) != nil {
		t.Error("a bar with no bytes")
	}
	// Names and colours are escaped.
	b = buildLangBar([]langPart{{Name: `<x>`, Color: `"red`, Bytes: 1}}, nil)
	if strings.Contains(string(b.SVG), `"red`) || strings.Contains(string(b.Entries[0].Dot), `"red`) {
		t.Errorf("colour not escaped: %s", b.SVG)
	}
	if langColor("Go") != langByExt[".go"].Color || langColor("Jupyter Notebook") == langOtherColor || langColor("Nope") != langOtherColor {
		t.Error("langColor")
	}
}

func TestLangBarPrivateRepo(t *testing.T) {
	f, work := langFixture(t)
	a := f.a
	ctx := context.Background()
	rp := secRepo(t, a, "demo")
	a.repoLangs(ctx, rp, nil)
	langWaitFor(t, a, rp.ID, runGit(t, work, "rev-parse", "HEAD"))

	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.db.CreateSessionFor(ctx, sam.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other := map[string]string{"Cookie": sessionCookie + "=" + sess}
	for _, p := range []string{"/zoe/demo", "/zoe/demo/insights/languages"} {
		body := expectStatus(t, do(t, a, "GET", p, nil, other), 404, "sam "+p)
		if strings.Contains(body, "TypeScript") || strings.Contains(body, "langbar") {
			t.Errorf("%s leaks languages to sam", p)
		}
	}
	for _, p := range []string{"/zoe", "/sam", "/", "/search?q=demo"} {
		rec := do(t, a, "GET", p, nil, other)
		// (The home feed's commit lines are not about languages: only the tag and names count here.)
		if rec.Code == http.StatusOK && regexp.MustCompile(`lang-tag|TypeScript|langbar`).MatchString(rec.Body.String()) {
			t.Errorf("%s shows the private repository's language to sam", p)
		}
	}
	// Anonymous visitors get nothing either.
	if rec := do(t, a, "GET", "/zoe/demo", nil, nil); rec.Code == 200 && strings.Contains(rec.Body.String(), "langbar") {
		t.Error("anonymous visitor sees languages")
	}
}

func TestLangBarGitHubRepo(t *testing.T) {
	f := newGHFixture(t)
	f.gh.mu.Lock()
	prev := f.gh.extra
	f.gh.extra = func(w http.ResponseWriter, r *http.Request, seg []string, body map[string]any) bool {
		if r.URL.Path == "/repos/dave/rocket/languages" {
			f.gh.write(w, r, 200, map[string]int64{"Go": 9000, "Shell": 900, "Dockerfile": 96, "Roff": 4, "<b>Evil</b>": 0})
			return true
		}
		return prev(w, r, seg, body)
	}
	f.gh.mu.Unlock()

	body := f.get(t, "/gh/dave/rocket", 200)
	sec := langSection(body)
	if sec == "" {
		t.Fatal("no Languages section on /gh/dave/rocket")
	}
	mustContain(t, sec, "gh section", `<svg class="langbar"`,
		`<span class="langbar-name">Go</span> <span class="langbar-pct">90.0%</span>`,
		`<span class="langbar-name">Shell</span> <span class="langbar-pct">9.0%</span>`,
		`<span class="langbar-name">Dockerfile</span> <span class="langbar-pct">1.0%</span>`,
		`<span class="langbar-name">Other</span> <span class="langbar-pct">&lt;0.1%</span>`, // Roff, 0.04%
		`fill="`+langColor("Go")+`"`, `rel="noopener noreferrer nofollow" target="_blank"`, "/dave/rocket/search?l=go")
	for _, bad := range []string{"style=", "Roff", "Evil"} {
		if strings.Contains(sec, bad) {
			t.Errorf("gh section contains %q", bad)
		}
	}
	if n := f.gh.hit("GET /repos/dave/rocket/languages"); n != 1 {
		t.Errorf("languages endpoint asked %d times, want 1", n)
	}
	// Subfolders don't ask.
	f.get(t, "/gh/dave/rocket/tree/main/docs", 200)
	if n := f.gh.hit("GET /repos/dave/rocket/languages"); n != 1 {
		t.Errorf("languages endpoint asked %d times after a subfolder", n)
	}
}
