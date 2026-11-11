package web

// The language bar: GitHub's "Languages" sidebar section (a thin bar in
// linguist colours and a legend with percentages) on a repository's
// overview, on /gh/ repository pages, and the primary language beside
// repositories in lists.
//
// Bytes per language come from Insights' tree scan (insScanTree,
// detectLanguage in insights_git.go), cached per (repository, commit) in
// insights_cache under langsKind. The scan of the tree alone is cheap, but a
// page never waits for it: when the cache is behind the default branch the
// page shows the last known bar (or none) and the scan runs in the
// background. A finished Insights computation fills this cache too.
//
// CSP forbids style attributes, so the bar is an inline SVG whose <rect>
// widths and fill colours are attributes.

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

const (
	langsKind       = "langs.v1"
	langMaxEntries  = 8   // legend entries, "Other" included
	langMinPct      = 0.1 // smaller languages fold into "Other"
	langOtherColor  = "#8a847b"
	langOtherName   = "Other"
	langListRetry   = 10 * time.Minute
	langListLookups = 2 * time.Second
)

// langsComputeCount counts tree scans started for the bar (tests use it).
var langsComputeCount atomic.Int64

// langSummary is the cached bytes per language at one commit.
type langSummary struct {
	Head      string    `json:"-"`
	Langs     []insLang `json:"langs"` // most bytes first
	Bytes     int64     `json:"bytes"`
	Truncated bool      `json:"truncated"`
}

// Primary is the language with the most bytes ("" when none).
func (s *langSummary) Primary() string {
	if s == nil || len(s.Langs) == 0 {
		return ""
	}
	return s.Langs[0].Name
}

func (a *App) langsCached(ctx context.Context, repoID int64) *langSummary {
	e, err := a.db.InsightsCache(ctx, repoID, langsKind)
	if err != nil {
		return nil
	}
	var s langSummary
	if err := json.Unmarshal(e.Data, &s); err != nil {
		return nil
	}
	s.Head = e.HeadSHA
	return &s
}

// saveLangs stores the language part of a computation.
func (a *App) saveLangs(ctx context.Context, repoID int64, head string, st *insStats) {
	s := langSummary{Langs: st.Langs, Bytes: st.LangBytes, Truncated: st.LangTruncated}
	if s.Langs == nil {
		s.Langs = []insLang{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	if err := a.db.SetInsightsCache(ctx, store.InsightsCacheEntry{RepoID: repoID, Kind: langsKind, HeadSHA: head, Data: b}); err != nil {
		log.Printf("languages: saving repo %d: %v", repoID, err)
	}
}

// repoLangs returns a repository's languages at the tip of its default
// branch without waiting: current is false when the summary (possibly nil)
// is older than the tip, in which case a background scan has been started.
func (a *App) repoLangs(ctx context.Context, rp store.Repo, g *gitrepo.Repo) (sum *langSummary, current, empty bool) {
	if g == nil {
		var err error
		if g, err = a.git.Open(rp.Owner, rp.Name); err != nil {
			return nil, false, true
		}
	}
	if g.IsEmpty(ctx) {
		return nil, true, true
	}
	head, err := g.ResolveCommit(ctx, g.DefaultBranch(ctx))
	if err != nil {
		return nil, true, true
	}
	cached := a.langsCached(ctx, rp.ID)
	if cached != nil && cached.Head == head {
		return cached, true, false
	}
	// Insights may already have counted this commit.
	if st := a.insCached(ctx, rp.ID); st != nil && st.Head == head {
		a.saveLangs(ctx, rp.ID, head, st)
		return &langSummary{Head: head, Langs: st.Langs, Bytes: st.LangBytes, Truncated: st.LangTruncated}, true, false
	}
	a.langsStart(rp, g, head)
	return cached, false, false
}

var langsSem = make(chan struct{}, 2)

var langsRunning = struct {
	sync.Mutex
	m map[langsKey]string // → head being scanned
}{m: map[langsKey]string{}}

type langsKey struct {
	a  *App
	id int64
}

// langsStart scans head's tree in the background (one scan per repository
// at a time) and returns a channel closed when it's done.
func (a *App) langsStart(rp store.Repo, g *gitrepo.Repo, head string) <-chan struct{} {
	done := make(chan struct{})
	k := langsKey{a, rp.ID}
	langsRunning.Lock()
	if _, busy := langsRunning.m[k]; busy {
		langsRunning.Unlock()
		close(done) // the running scan's result shows on the next view
		return done
	}
	langsRunning.m[k] = head
	langsRunning.Unlock()
	langsComputeCount.Add(1)
	go func() {
		langsSem <- struct{}{}
		defer func() {
			<-langsSem
			langsRunning.Lock()
			delete(langsRunning.m, k)
			langsRunning.Unlock()
			close(done)
		}()
		ctx := context.Background()
		st := &insStats{}
		if err := insScanTree(ctx, g, head, st); err != nil {
			log.Printf("languages: %s at %.7s: %v", rp.FullName(), head, err)
			return
		}
		a.saveLangs(ctx, rp.ID, head, st)
	}()
	return done
}

// ---------- the bar ----------

// langEntry is one legend entry.
type langEntry struct {
	Name, Color string
	Pct         string // "71.2%"
	URL         string
	External    bool
	Dot         template.HTML
}

// langBar is what the "lang-bar" partial draws.
type langBar struct {
	SVG     template.HTML
	Entries []langEntry
	Label   string // the list's accessible name
}

type langPart struct {
	Name, Color string
	Bytes       int64
}

// buildLangBar folds languages under langMinPct (and past the legend's
// room) into "Other" and draws the bar. Each entry links to link(name).
// It returns nil when there's nothing to show.
func buildLangBar(parts []langPart, link func(name string) (string, bool)) *langBar {
	var total int64
	for _, p := range parts {
		if p.Bytes > 0 {
			total += p.Bytes
		}
	}
	if total <= 0 {
		return nil
	}
	sorted := make([]langPart, 0, len(parts))
	for _, p := range parts {
		if p.Bytes > 0 {
			sorted = append(sorted, p)
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Bytes != sorted[j].Bytes {
			return sorted[i].Bytes > sorted[j].Bytes
		}
		return sorted[i].Name < sorted[j].Name
	})
	var shown []langPart
	var other int64
	for _, p := range sorted {
		if float64(p.Bytes)/float64(total)*100 < langMinPct || p.Name == langOtherName {
			other += p.Bytes
			continue
		}
		shown = append(shown, p)
	}
	if room := langMaxEntries; len(shown) > room || (other > 0 && len(shown) == room) {
		for _, p := range shown[room-1:] {
			other += p.Bytes
		}
		shown = shown[:room-1]
	}
	if other > 0 {
		shown = append(shown, langPart{Name: langOtherName, Color: langOtherColor, Bytes: other})
	}
	b := &langBar{Label: "Languages by share of code"}
	const w, h = 1000.0, 10.0
	var svg strings.Builder
	fmt.Fprintf(&svg, `<svg class="langbar" viewBox="0 0 %.0f %.0f" width="100%%" height="8" preserveAspectRatio="none" aria-hidden="true" focusable="false">`, w, h)
	x := 0.0
	for i, p := range shown {
		pct := float64(p.Bytes) / float64(total) * 100
		pw := pct / 100 * w
		draw := pw
		if i < len(shown)-1 {
			draw = pw - 3 // a hairline gap between segments
		}
		if draw < 2 {
			draw = math.Max(pw, 2) // tiny shares stay visible
		}
		color := p.Color
		if color == "" {
			color = langOtherColor
		}
		fmt.Fprintf(&svg, `<rect x="%.2f" y="0" width="%.2f" height="%.0f" fill="%s"/>`, x, draw, h, insEsc(color))
		x += pw
		e := langEntry{Name: p.Name, Color: color, Pct: langPct(pct), Dot: langDot(color)}
		if link != nil {
			e.URL, e.External = link(p.Name)
		}
		b.Entries = append(b.Entries, e)
	}
	svg.WriteString(`</svg>`)
	b.SVG = template.HTML(svg.String())
	return b
}

// langPct formats a share like GitHub: one decimal, "<0.1%" for slivers.
func langPct(pct float64) string {
	if pct > 0 && pct < 0.05 {
		return "<0.1%"
	}
	return fmt.Sprintf("%.1f%%", pct)
}

// langDot is a legend dot in a language's colour (an SVG fill attribute,
// since CSP forbids style attributes).
func langDot(color string) template.HTML {
	if color == "" {
		color = langOtherColor
	}
	return template.HTML(`<svg class="lang-dot" width="10" height="10" viewBox="0 0 10 10" aria-hidden="true" focusable="false"><circle cx="5" cy="5" r="5" fill="` +
		insEsc(color) + `"/></svg>`)
}

// repoLangBar is the bar for an Atelier repository at the tip of its default
// branch (nil when empty, not counted yet, or without any counted language).
func (a *App) repoLangBar(ctx context.Context, rp store.Repo, g *gitrepo.Repo) *langBar {
	sum, _, empty := a.repoLangs(ctx, rp, g)
	if empty || sum == nil {
		return nil
	}
	parts := make([]langPart, 0, len(sum.Langs))
	for _, l := range sum.Langs {
		parts = append(parts, langPart{Name: l.Name, Color: l.Color, Bytes: l.Bytes})
	}
	insights := "/" + rp.FullName() + "/insights/languages"
	return buildLangBar(parts, func(string) (string, bool) { return insights, false })
}

// ghLangBar is the bar from GitHub's languages endpoint. Entries link to the
// languages of the Atelier copy when there is one (local), else to GitHub.
func (a *App) ghLangBar(ctx context.Context, g *ghReq, v *ghRepoView) *langBar {
	m, err := g.c.Languages(ctx, v.Full)
	if err != nil || len(m) == 0 {
		return nil
	}
	parts := make([]langPart, 0, len(m))
	for name, n := range m {
		parts = append(parts, langPart{Name: name, Color: langColor(name), Bytes: n})
	}
	return buildLangBar(parts, func(name string) (string, bool) {
		if v.Local != "" {
			return v.Local + "/insights/languages", false
		}
		if name == langOtherName {
			return v.GH, true
		}
		return v.GH + "/search?" + url.Values{"l": {strings.ToLower(name)}}.Encode(), true
	})
}

// ---------- colours by language name ----------

var (
	langColorsOnce sync.Once
	langColors     map[string]string
)

// langExtraColors: linguist colours for languages GitHub reports that
// Insights' extension map doesn't count.
var langExtraColors = map[string]string{
	"Jupyter Notebook": "#DA5B0B", "Objective-C++": "#6866fb", "TeX": "#3D6117", "MDX": "#fcb32c",
	"Starlark": "#76d275", "CMake": "#DA3434", "Assembly": "#6E4C13", "Cython": "#fedf5b", "Fortran": "#4d41b1",
	"Handlebars": "#f7931e", "Pug": "#a86454", "Smarty": "#f0c040", "Twig": "#c1d026", "Mustache": "#724b3b",
	"Jinja": "#a52a22", "Nunjucks": "#3d8137", "EJS": "#a91e50", "Roff": "#ecdebe", "Procfile": "#3B2F63",
	"Just": "#384d54", "Emacs Lisp": "#c065db", "Common Lisp": "#3fb68b", "Racket": "#3c5caa", "Scheme": "#1e4aec",
	"Crystal": "#000100", "Nim": "#ffc200", "V": "#4f87c4", "D": "#ba595e", "F#": "#b845fc",
	"Visual Basic .NET": "#945db7", "MATLAB": "#e16737", "Ada": "#02f88c", "Gleam": "#ffaff3", "Mojo": "#ff4c1f",
	"Odin": "#60AFFE", "Typst": "#239dad", "Hack": "#878787", "Liquid": "#67b8de", "Jsonnet": "#0064bd",
	"CUE": "#5886E1", "Bicep": "#519aba", "HLSL": "#aace60", "ShaderLab": "#222c37", "Sass": "#a53b70",
	"Stylus": "#ff6347", "CoffeeScript": "#244776", "PureScript": "#1D222D", "ReScript": "#ed5051",
	"Reason": "#ff5847", "Prolog": "#74283c", "Tcl": "#e4cc98", "Awk": "#c30e9b", "Raku": "#0000fb",
	"Vala": "#a56de2", "Haxe": "#df7900", "GDScript": "#355570", "Pascal": "#E3F171", "Smalltalk": "#596706",
	"Apex": "#1797c0", "Gherkin": "#5B2063", "Mermaid": "#ff3670", "Nushell": "#4E9906", "Move": "#4a137a",
	"Cairo": "#ff4a48", "Vyper": "#2980b9", "Q#": "#fed659", "Verilog": "#b2b7f8", "VHDL": "#adb2cb",
	"SystemVerilog": "#DAE1C2", "Processing": "#0096D8", "AutoHotkey": "#6594b9", "Lean": "#D7E4F7",
}

// langColor is a language's colour by name (linguist's, lightened for the
// dark background like Insights' colours); unknown languages are grey.
func langColor(name string) string {
	langColorsOnce.Do(func() {
		langColors = map[string]string{}
		for n, c := range langExtraColors {
			langColors[n] = insLangColor(c)
		}
		for _, m := range []map[string]langDef{langByExt, langByName} {
			for _, d := range m {
				langColors[d.Name] = d.Color
			}
		}
	})
	if c, ok := langColors[name]; ok {
		return c
	}
	return langOtherColor
}

// ---------- the primary language in repository lists ----------

var langListTried sync.Map // langsKey → time of the last background start

// repoLangTag is the coloured dot and name of a repository's primary
// language for lists (empty when it isn't known yet). It only reads the
// cache; a repository never counted starts a background scan.
func (a *App) repoLangTag(id int64) template.HTML {
	ctx, cancel := context.WithTimeout(context.Background(), langListLookups)
	defer cancel()
	name := ""
	if s := a.langsCached(ctx, id); s != nil {
		name = s.Primary()
	} else if st := a.insCached(ctx, id); st != nil {
		a.saveLangs(ctx, id, st.Head, st)
		if len(st.Langs) > 0 {
			name = st.Langs[0].Name
		}
	} else {
		a.langListKick(id)
	}
	if name == "" {
		return ""
	}
	return template.HTML(`<span class="lang-tag">` + string(langDot(langColor(name))) + `<span>` + insEsc(name) + `</span></span>`)
}

// langListKick counts a repository that has never been counted, in the
// background, at most once per langListRetry.
func (a *App) langListKick(id int64) {
	k := langsKey{a, id}
	if t, ok := langListTried.Load(k); ok && time.Since(t.(time.Time)) < langListRetry {
		return
	}
	langListTried.Store(k, time.Now())
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), insGitTimeout)
		defer cancel()
		rp, err := a.db.RepoByID(ctx, id)
		if err != nil {
			return
		}
		a.repoLangs(ctx, rp, nil)
	}()
}

// langFuncs are the template functions of the language bar.
func (a *App) langFuncs() template.FuncMap {
	return template.FuncMap{
		"repoLang": a.repoLangTag,
		"langDot":  func(name string) template.HTML { return langDot(langColor(name)) },
	}
}

// overviewLangs is the bar for the repository's overview (the root of the
// code tab); deeper folders don't show it.
func (v *repoView) overviewLangs(ctx context.Context, a *App, g *gitrepo.Repo) *langBar {
	if v.Path != "" || v.Empty {
		return nil
	}
	return a.repoLangBar(ctx, v.Repo, g)
}
