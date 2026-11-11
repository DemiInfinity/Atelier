package web

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// page is what every template receives.
type page struct {
	Title  string
	Nav    string // active top-bar item
	Bare   bool   // no top bar (setup, sign-in)
	Error  string
	Notice string
	Me     store.Owner
	Data   any
}

// pageNames lists every page template: each templates/*.html file except the
// layout and partials files is a page named after its file.
func pageNames() ([]string, []string, error) {
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, nil, err
	}
	var pages, partials []string
	for _, f := range files {
		base := strings.TrimSuffix(path.Base(f), ".html")
		switch {
		case base == "layout":
		case strings.HasPrefix(base, "partials"):
			partials = append(partials, f)
		default:
			pages = append(pages, base)
		}
	}
	return pages, partials, nil
}

var md = goldmark.New(goldmark.WithExtensions(extension.GFM)) // raw HTML stays escaped (safe default)

func (a *App) loadTemplates() error {
	funcs := template.FuncMap{
		"ago": ago,
		// actorKind shows comments Tether synced from GitHub as "tether".
		"actorKind": func(kind, name string) string {
			if kind == "import" && strings.HasPrefix(name, "github:") {
				return "tether"
			}
			return kind
		},
		"bytes":      humanBytes,
		"markdown":   renderMarkdown,
		"isMarkdown": isMarkdown,
		"lines":      codeLines,
		"diffLines":  diffLines,
		"join":       path.Join,
		"short": func(s string) string {
			if len(s) > 7 {
				return s[:7]
			}
			return s
		},
		"date":     func(t time.Time) string { return t.Format("2 Jan 2006") },
		"datetime": func(t time.Time) string { return t.Format("2 Jan 2006, 15:04") },
		"initial": func(s string) string {
			if s == "" {
				return "?"
			}
			return strings.ToUpper(s[:1])
		},
	}
	a.pages = map[string]*template.Template{}
	names, partials, err := pageNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		patterns := append([]string{"templates/layout.html"}, partials...)
		patterns = append(patterns, "templates/"+name+".html")
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, patterns...)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		a.pages[name] = t
	}
	return nil
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, p page) {
	if !p.Bare {
		if me, err := a.db.PersonalOwner(r.Context()); err == nil {
			p.Me = me
		}
	}
	var buf bytes.Buffer
	if err := a.pages[name].Execute(&buf, p); err != nil {
		logErr(r, err)
		http.Error(w, "something went wrong rendering this page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = buf.WriteTo(w)
}

func (a *App) notFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	a.render(w, r, "error", page{Title: "Not found", Data: map[string]string{
		"Heading": "Nothing here", "Message": "That page, repository or file doesn't exist.",
	}})
}

func (a *App) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, gitrepo.ErrNotFound) {
		a.notFound(w, r)
		return
	}
	logErr(r, err)
	w.WriteHeader(http.StatusInternalServerError)
	a.render(w, r, "error", page{Title: "Error", Data: map[string]string{
		"Heading": "Something went wrong", "Message": "The error has been logged on the server.",
	}})
}

// ---------- template helpers ----------

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	case d < 48*time.Hour:
		return "yesterday"
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day") + " ago"
	}
	return "on " + t.Format("2 Jan 2006")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
}

func isMarkdown(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".md") || strings.HasSuffix(n, ".markdown")
}

func renderMarkdown(src []byte) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(string(src)))
	}
	return template.HTML(buf.String()) // goldmark escapes raw HTML and unsafe links by default
}

type codeLine struct {
	N    int
	Text string
}

func codeLines(data []byte) []codeLine {
	s := strings.TrimSuffix(string(data), "\n")
	parts := strings.Split(s, "\n")
	out := make([]codeLine, len(parts))
	for i, p := range parts {
		out[i] = codeLine{N: i + 1, Text: strings.TrimSuffix(p, "\r")}
	}
	return out
}

type diffLine struct {
	Class, Text string
}

func diffLines(patch string) []diffLine {
	var out []diffLine
	for _, l := range strings.Split(patch, "\n") {
		c := ""
		switch {
		case strings.HasPrefix(l, "diff --git"):
			c = "file"
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"), strings.HasPrefix(l, "index "),
			strings.HasPrefix(l, "new file"), strings.HasPrefix(l, "deleted file"), strings.HasPrefix(l, "similarity"),
			strings.HasPrefix(l, "rename "):
			c = "meta"
		case strings.HasPrefix(l, "@@"):
			c = "hunk"
		case strings.HasPrefix(l, "+"):
			c = "add"
		case strings.HasPrefix(l, "-"):
			c = "del"
		}
		out = append(out, diffLine{Class: c, Text: l})
	}
	return out
}
