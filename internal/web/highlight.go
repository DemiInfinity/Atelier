package web

// Syntax highlighting glue: template helpers for file views and diffs, and
// the editor's live-highlighting endpoint. The colouring itself is in
// internal/highlight; its stylesheet is static/highlight.css.
//
//	POST /{owner}/{repo}/edit-highlight   highlighted lines for the editor (JSON)

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"

	"atelier/internal/highlight"
)

func init() {
	registerFeature(feature{name: "highlight", routes: func(a *App, m *http.ServeMux) {
		m.HandleFunc("POST /{owner}/{repo}/edit-highlight", a.authed(a.editHighlight))
	}})
}

type hlLine struct {
	N    int
	HTML template.HTML
}

// hlView is a highlighted file for the code tables (repo, snippet and gh
// blob pages).
type hlView struct {
	Lang  string
	Note  string
	Lines []hlLine
}

func highlightFile(name string, data []byte) hlView {
	r := highlight.File(name, data)
	v := hlView{Lang: r.Lang, Note: r.Note, Lines: make([]hlLine, len(r.Lines))}
	for i, l := range r.Lines {
		v.Lines[i] = hlLine{N: i + 1, HTML: l}
	}
	return v
}

// codeLang is the language shown next to a file's size ("Kotlin").
func codeLang(name string, data []byte) string {
	return highlight.Detect(name, data).Name
}

// highlightDiff fills in Code for unified-diff lines. The file comes from
// each "diff --git" header, or from name for a single-file patch (GitHub's).
func highlightDiff(out []diffLine, name string) {
	start, file := 0, name
	flush := func(end int) {
		if end <= start {
			return
		}
		seg := out[start:end]
		in := make([]highlight.DiffLine, len(seg))
		for i, d := range seg {
			kind, text := "", d.Text
			switch d.Class {
			case "add", "del":
				kind, text = d.Class, d.Text[1:]
			case "hunk", "file":
				kind = d.Class
			case "":
				if strings.HasPrefix(d.Text, " ") {
					kind, text = "ctx", d.Text[1:]
				} else if d.Text == "" {
					kind = "ctx" // an empty context line some tools strip the space from
				}
			}
			in[i] = highlight.DiffLine{Kind: kind, Text: text}
		}
		hl := highlight.Diff(file, in)
		for i := range seg {
			switch in[i].Kind {
			case "add":
				seg[i].Code = "+" + hl[i]
			case "del":
				seg[i].Code = "-" + hl[i]
			case "ctx":
				if seg[i].Text == "" {
					seg[i].Code = ""
				} else {
					seg[i].Code = " " + hl[i]
				}
			default:
				seg[i].Code = template.HTML(template.HTMLEscapeString(seg[i].Text))
			}
		}
	}
	for i, d := range out {
		if d.Class == "file" {
			flush(i)
			start = i
			file = diffGitPath(d.Text)
		}
	}
	flush(len(out))
}

// diffGitPath reads the new path from "diff --git a/x b/y".
func diffGitPath(header string) string {
	if i := strings.LastIndex(header, " b/"); i >= 0 {
		return header[i+3:]
	}
	return ""
}

// highlightPullFile colours a pull request file's diff lines.
func highlightPullFile(path string, lines []pullLineView) {
	in := make([]highlight.DiffLine, len(lines))
	for i, l := range lines {
		in[i] = highlight.DiffLine{Kind: l.Kind, Text: l.Text}
	}
	hl := highlight.Diff(path, in)
	for i := range lines {
		lines[i].Code = hl[i]
	}
}

// editHighlight returns the editor's text as highlighted lines, for the
// overlay behind its textarea: {"lang": "Kotlin", "lines": ["<span…>", …]}.
// The language is the picker's choice, or detected from the path.
func (a *App) editHighlight(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.loadRepo(w, r, "code"); !ok { // POST: needs write
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*editMaxText)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	text := strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n")
	path := r.PostFormValue("path")
	lexer, lang := "", ""
	if choice := r.PostFormValue("lang"); choice != "" {
		lexer, lang = highlight.LexerForHint(choice), choice
	} else {
		l := highlight.Detect(path, []byte(text))
		lexer, lang = l.Lexer, l.Name
	}
	lines := strings.Split(text, "\n")
	hl := highlight.Lines(lexer, lines)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"lang": lang, "lines": hl})
}

// editLanguages is the editor's language picker.
func editLanguages() []string { return highlight.Languages() }
