package web

// Insights: computing a repository's statistics from git, caching them per
// (repository, default-branch commit), and doing it in the background so a
// large repository never holds a request for long.
//
// One pass of `git log --numstat` gives commits, additions and deletions per
// week and per day, and per author; `git ls-tree -r -l` gives bytes per
// language. Both are bounded (insMaxCommits, insMaxFiles, insGitTimeout).
// The result is stored in insights_cache under insStatsKind with the commit it
// was computed at; a page whose cache is behind shows the old numbers (or
// nothing yet) with an "Updating…" note and refreshes itself.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

const (
	insStatsKind  = "stats.v1"
	insMaxCommits = 100000
	insMaxFiles   = 200000
	insMaxAuthors = 100
)

var (
	// insGitTimeout bounds each git command of a computation.
	insGitTimeout = 90 * time.Second
	// insWait is how long a page waits for a computation it started before
	// showing the "Updating…" state instead.
	insWait = 1500 * time.Millisecond
	// insComputeCount counts computations (tests check the cache with it).
	insComputeCount atomic.Int64
)

// insWeek is one week (or day) of activity.
type insWeek struct {
	Commits int `json:"c"`
	Adds    int `json:"a"`
	Dels    int `json:"d"`
}

type insAuthor struct {
	Name    string         `json:"name"`
	Email   string         `json:"email"`
	Commits int            `json:"commits"`
	Adds    int            `json:"adds"`
	Dels    int            `json:"dels"`
	First   int64          `json:"first"`
	Last    int64          `json:"last"`
	Weeks   map[string]int `json:"weeks"` // week start (YYYY-MM-DD) → commits
}

type insLang struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}

// insStats is everything computed from git for one commit.
type insStats struct {
	Head          string             `json:"head"`
	Branch        string             `json:"branch"`
	Commits       int                `json:"commits"`
	Truncated     bool               `json:"truncated"` // stopped at insMaxCommits or the timeout
	Weeks         map[string]insWeek `json:"weeks"`     // week start (Sunday, server time) → totals
	Days          map[string]int     `json:"days"`      // YYYY-MM-DD → commits
	Authors       []insAuthor        `json:"authors"`   // most commits first, at most insMaxAuthors
	OtherAuthors  int                `json:"other_authors"`
	Langs         []insLang          `json:"langs"` // most bytes first
	LangBytes     int64              `json:"lang_bytes"`
	LangTruncated bool               `json:"lang_truncated"`
	ComputedAt    time.Time          `json:"computed_at"`
}

// insWeekStart is the Sunday that starts t's week, as YYYY-MM-DD.
func insWeekStart(t time.Time) time.Time {
	t = t.In(time.Local)
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
	return d.AddDate(0, 0, -int(d.Weekday()))
}

func insDayKey(t time.Time) string { return t.In(time.Local).Format("2006-01-02") }

// ---------- computing ----------

// computeInsStats runs git over the commit head (the tip of branch).
func computeInsStats(ctx context.Context, g *gitrepo.Repo, branch, head string) (*insStats, error) {
	insComputeCount.Add(1)
	st := &insStats{Head: head, Branch: branch, Weeks: map[string]insWeek{}, Days: map[string]int{}, ComputedAt: time.Now()}
	if err := insScanLog(ctx, g, head, st); err != nil {
		return nil, err
	}
	if err := insScanTree(ctx, g, head, st); err != nil {
		return nil, err
	}
	return st, nil
}

func insScanLog(ctx context.Context, g *gitrepo.Repo, head string, st *insStats) error {
	authors := map[string]*insAuthor{}
	var cur *insAuthor
	var curWeek, curDay string
	err := g.StreamGit(ctx, insGitTimeout, func(r io.Reader) error {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "\x1e") {
				if st.Commits >= insMaxCommits {
					st.Truncated = true
					return nil
				}
				f := strings.Split(line[1:], "\x1f")
				if len(f) < 4 {
					cur = nil
					continue
				}
				ts, _ := strconv.ParseInt(f[1], 10, 64)
				when := time.Unix(ts, 0)
				name, email := strings.TrimSpace(f[2]), strings.ToLower(strings.TrimSpace(f[3]))
				key := email
				if key == "" {
					key = "name:" + strings.ToLower(name)
				}
				a := authors[key]
				if a == nil {
					a = &insAuthor{Name: name, Email: email, First: ts, Last: ts, Weeks: map[string]int{}}
					authors[key] = a
				}
				if ts >= a.Last {
					a.Last = ts
					if name != "" {
						a.Name = name // the newest spelling of the name wins
					}
				}
				if ts < a.First {
					a.First = ts
				}
				a.Commits++
				curWeek, curDay = insWeekStart(when).Format("2006-01-02"), insDayKey(when)
				a.Weeks[curWeek]++
				w := st.Weeks[curWeek]
				w.Commits++
				st.Weeks[curWeek] = w
				st.Days[curDay]++
				st.Commits++
				cur = a
				continue
			}
			if cur == nil || line == "" {
				continue
			}
			// numstat: "<adds>\t<dels>\t<path>"; binary files show "-".
			f := strings.SplitN(line, "\t", 3)
			if len(f) < 3 || f[0] == "-" {
				continue
			}
			add, err1 := strconv.Atoi(f[0])
			del, err2 := strconv.Atoi(f[1])
			if err1 != nil || err2 != nil {
				continue
			}
			cur.Adds += add
			cur.Dels += del
			w := st.Weeks[curWeek]
			w.Adds += add
			w.Dels += del
			st.Weeks[curWeek] = w
		}
		return sc.Err()
	}, "log", head, "--no-renames", "--numstat", "--format=%x1e%H%x1f%at%x1f%aN%x1f%aE", "--")
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		st.Truncated = true // timed out: keep what was read
	}
	list := make([]insAuthor, 0, len(authors))
	for _, a := range authors {
		list = append(list, *a)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Commits != list[j].Commits {
			return list[i].Commits > list[j].Commits
		}
		return list[i].Email < list[j].Email
	})
	if len(list) > insMaxAuthors {
		st.OtherAuthors = len(list) - insMaxAuthors
		list = list[:insMaxAuthors]
	}
	st.Authors = list
	return nil
}

func insScanTree(ctx context.Context, g *gitrepo.Repo, head string, st *insStats) error {
	langs := map[string]*insLang{}
	files := 0
	err := g.StreamGit(ctx, insGitTimeout, func(r io.Reader) error {
		br := bufio.NewReaderSize(r, 64*1024)
		for {
			rec, err := br.ReadString(0)
			if len(rec) > 0 {
				files++
				if files > insMaxFiles {
					st.LangTruncated = true
					return nil
				}
				insAddTreeEntry(strings.TrimSuffix(rec, "\x00"), langs)
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}, "ls-tree", "-r", "-l", "-z", head)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		st.LangTruncated = true
	}
	for _, l := range langs {
		st.Langs = append(st.Langs, *l)
		st.LangBytes += l.Bytes
	}
	sort.Slice(st.Langs, func(i, j int) bool {
		if st.Langs[i].Bytes != st.Langs[j].Bytes {
			return st.Langs[i].Bytes > st.Langs[j].Bytes
		}
		return st.Langs[i].Name < st.Langs[j].Name
	})
	return nil
}

// insAddTreeEntry counts one "<mode> <type> <sha> <size>\t<path>" record.
func insAddTreeEntry(rec string, langs map[string]*insLang) {
	meta, p, ok := strings.Cut(rec, "\t")
	if !ok {
		return
	}
	f := strings.Fields(meta)
	if len(f) < 4 || f[1] != "blob" || f[0] == "120000" { // skip submodules and symlinks
		return
	}
	size, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil || size <= 0 {
		return
	}
	lang, ok := detectLanguage(p)
	if !ok {
		return
	}
	l := langs[lang.Name]
	if l == nil {
		l = &insLang{Name: lang.Name, Color: lang.Color}
		langs[lang.Name] = l
	}
	l.Bytes += size
	l.Files++
}

// ---------- languages ----------

type langDef struct{ Name, Color string }

// Languages counted in the language bar: programming and markup languages, as
// GitHub's linguist does (prose such as Markdown and data such as JSON or
// YAML are left out). Colours are linguist's, lightened where they would
// disappear on the dark background (see insLangColor).
var langByExt = map[string]langDef{}
var langByName = map[string]langDef{}

func init() {
	add := func(name, color string, exts ...string) {
		d := langDef{Name: name, Color: insLangColor(color)}
		for _, e := range exts {
			if strings.HasPrefix(e, ".") {
				langByExt[e] = d
			} else {
				langByName[e] = d
			}
		}
	}
	add("Go", "#00ADD8", ".go")
	add("JavaScript", "#f1e05a", ".js", ".mjs", ".cjs", ".jsx")
	add("TypeScript", "#3178c6", ".ts", ".tsx", ".mts", ".cts")
	add("Python", "#3572A5", ".py", ".pyi")
	add("Ruby", "#701516", ".rb", "Gemfile", "Rakefile")
	add("Rust", "#dea584", ".rs")
	add("Swift", "#F05138", ".swift")
	add("Kotlin", "#A97BFF", ".kt", ".kts")
	add("Java", "#b07219", ".java")
	add("C", "#555555", ".c", ".h")
	add("C++", "#f34b7d", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx")
	add("C#", "#178600", ".cs")
	add("Objective-C", "#438eff", ".m", ".mm")
	add("PHP", "#4F5D95", ".php")
	add("Shell", "#89e051", ".sh", ".bash", ".zsh", ".fish")
	add("PowerShell", "#012456", ".ps1", ".psm1")
	add("HTML", "#e34c26", ".html", ".htm", ".tmpl", ".gohtml")
	add("CSS", "#663399", ".css")
	add("SCSS", "#c6538c", ".scss", ".sass")
	add("Less", "#1d365d", ".less")
	add("Vue", "#41b883", ".vue")
	add("Svelte", "#ff3e00", ".svelte")
	add("Astro", "#ff5a03", ".astro")
	add("Dart", "#00B4AB", ".dart")
	add("Elixir", "#6e4a7e", ".ex", ".exs")
	add("Erlang", "#B83998", ".erl")
	add("Haskell", "#5e5086", ".hs")
	add("Lua", "#000080", ".lua")
	add("Perl", "#0298c3", ".pl", ".pm")
	add("R", "#198CE7", ".r", ".R")
	add("Scala", "#c22d40", ".scala")
	add("Zig", "#ec915c", ".zig")
	add("Nix", "#7e7eff", ".nix")
	add("SQL", "#e38c00", ".sql")
	add("Dockerfile", "#384d54", "Dockerfile", "Containerfile")
	add("Makefile", "#427819", "Makefile", "GNUmakefile", ".mk")
	add("HCL", "#844FBA", ".tf", ".hcl")
	add("Vim Script", "#199f4b", ".vim")
	add("Elm", "#60B5CC", ".elm")
	add("OCaml", "#ef7a08", ".ml", ".mli")
	add("Clojure", "#db5855", ".clj", ".cljs", ".cljc")
	add("Groovy", "#4298b8", ".groovy", ".gradle")
	add("Julia", "#a270ba", ".jl")
	add("Solidity", "#AA6746", ".sol")
	add("GLSL", "#5686a5", ".glsl", ".frag", ".vert")
	add("Metal", "#8f14e9", ".metal")
	add("WebAssembly", "#04133b", ".wat")
	add("Protocol Buffer", "#5a6a8a", ".proto")
	add("Batchfile", "#C1F12E", ".bat", ".cmd")
}

// insVendored: paths linguist treats as vendored, generated or documentation.
func insVendored(p string) bool {
	lp := "/" + strings.ToLower(p)
	for _, s := range []string{"/vendor/", "/node_modules/", "/third_party/", "/third-party/", "/dist/", "/build/",
		"/.yarn/", "/bower_components/", "/docs/", "/documentation/", "/testdata/", "/__snapshots__/", "/.git/"} {
		if strings.Contains(lp, s) {
			return true
		}
	}
	base := path.Base(lp)
	return strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") || strings.HasSuffix(base, ".pb.go") ||
		strings.HasSuffix(base, "_gen.go") || strings.HasSuffix(base, ".generated.ts") || strings.HasSuffix(base, ".bundle.js")
}

// detectLanguage maps a path to a counted language.
func detectLanguage(p string) (langDef, bool) {
	if insVendored(p) {
		return langDef{}, false
	}
	base := path.Base(p)
	if d, ok := langByName[base]; ok {
		return d, true
	}
	if strings.HasPrefix(base, "Dockerfile.") {
		return langByName["Dockerfile"], true
	}
	ext := path.Ext(base)
	if d, ok := langByExt[ext]; ok {
		return d, true
	}
	if d, ok := langByExt[strings.ToLower(ext)]; ok {
		return d, true
	}
	return langDef{}, false
}

// insLangColor lightens a colour until it stands out on the dark surface
// (--surface #1a1917): relative luminance of at least 0.13, about 3:1.
func insLangColor(hex string) string {
	r, g, b, ok := insParseHex(hex)
	if !ok {
		return "#a8a299"
	}
	for i := 0; i < 12 && insRelLum(r, g, b) < 0.13; i++ {
		r, g, b = r+(1-r)*0.18, g+(1-g)*0.18, b+(1-b)*0.18
	}
	return fmt.Sprintf("#%02x%02x%02x", int(math.Round(r*255)), int(math.Round(g*255)), int(math.Round(b*255)))
}

func insParseHex(h string) (r, g, b float64, ok bool) {
	h = strings.TrimPrefix(h, "#")
	if len(h) != 6 {
		return 0, 0, 0, false
	}
	n, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return float64(n>>16&0xff) / 255, float64(n>>8&0xff) / 255, float64(n&0xff) / 255, true
}

func insRelLum(r, g, b float64) float64 {
	lin := func(c float64) float64 {
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// ---------- caching and background work ----------

var insSem = make(chan struct{}, 2)

// insRunning de-duplicates computations: one per repository at a time.
var insRunning = struct {
	sync.Mutex
	m map[int64]*insFlight
}{m: map[int64]*insFlight{}}

type insFlight struct {
	head string
	done chan struct{}
}

// insResult is what a page gets: the newest stats there are (nil if none
// yet), whether they are behind the branch (Updating), and the branch head.
type insResult struct {
	Stats    *insStats
	Updating bool
	Empty    bool // no commits on the default branch
	Failed   bool // the last computation failed (logged)
}

// insStatsFor returns a repository's statistics for the tip of its default
// branch: from the cache when it's current, otherwise computed in the
// background (waiting up to wait for it).
func (a *App) insStatsFor(ctx context.Context, rp store.Repo, wait time.Duration) insResult {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil || g.IsEmpty(ctx) {
		return insResult{Empty: true}
	}
	branch := g.DefaultBranch(ctx)
	head, err := g.ResolveCommit(ctx, branch)
	if err != nil {
		return insResult{Empty: true}
	}
	cached := a.insCached(ctx, rp.ID)
	if cached != nil && cached.Head == head {
		return insResult{Stats: cached}
	}
	done := a.insStart(rp, g, branch, head)
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-done:
			if fresh := a.insCached(ctx, rp.ID); fresh != nil && fresh.Head == head {
				return insResult{Stats: fresh}
			}
			return insResult{Stats: cached, Failed: true}
		case <-t.C:
		case <-ctx.Done():
		}
	}
	return insResult{Stats: cached, Updating: true}
}

func (a *App) insCached(ctx context.Context, repoID int64) *insStats {
	e, err := a.db.InsightsCache(ctx, repoID, insStatsKind)
	if err != nil {
		return nil
	}
	var st insStats
	if err := json.Unmarshal(e.Data, &st); err != nil || st.Weeks == nil {
		return nil
	}
	st.Head = e.HeadSHA
	return &st
}

// insStart starts computing (unless that commit is already being computed)
// and returns a channel closed when it's done.
func (a *App) insStart(rp store.Repo, g *gitrepo.Repo, branch, head string) <-chan struct{} {
	insRunning.Lock()
	defer insRunning.Unlock()
	if f := insRunning.m[rp.ID]; f != nil {
		if f.head == head {
			return f.done
		}
		// Another commit is in progress: let it finish; the next page view
		// starts this one. Report "not done" with a channel that never closes
		// for this head.
		return make(chan struct{})
	}
	f := &insFlight{head: head, done: make(chan struct{})}
	insRunning.m[rp.ID] = f
	go func() {
		insSem <- struct{}{} // at most two computations at once (the account page starts many)
		defer func() {
			<-insSem
			insRunning.Lock()
			delete(insRunning.m, rp.ID)
			insRunning.Unlock()
			close(f.done)
		}()
		ctx := context.Background()
		st, err := computeInsStats(ctx, g, branch, head)
		if err != nil {
			log.Printf("insights: %s at %.7s: %v", rp.FullName(), head, err)
			return
		}
		b, err := json.Marshal(st)
		if err != nil {
			return
		}
		if err := a.db.SetInsightsCache(ctx, store.InsightsCacheEntry{RepoID: rp.ID, Kind: insStatsKind, HeadSHA: head,
			Data: b}); err != nil {
			log.Printf("insights: saving %s: %v", rp.FullName(), err)
		}
	}()
	return f.done
}

// insWeeks is the last n weeks (oldest first) ending with the current week.
func insWeeks(n int, now time.Time) []time.Time {
	end := insWeekStart(now)
	out := make([]time.Time, n)
	for i := range out {
		out[i] = end.AddDate(0, 0, -7*(n-1-i))
	}
	return out
}
