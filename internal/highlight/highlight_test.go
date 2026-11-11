package highlight

import (
	"bytes"
	"flag"
	"html"
	"html/template"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

var update = flag.Bool("update", false, "rewrite internal/web/static/highlight.css")

const cssPath = "../web/static/highlight.css"

func TestCSSUpToDate(t *testing.T) {
	css := CSS()
	if *update {
		if err := os.WriteFile(cssPath, []byte(css), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(cssPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != css {
		t.Fatalf("%s is stale: run go test ./internal/highlight -run TestCSSUpToDate -update", cssPath)
	}
	for _, want := range []string{".hl-k {", ".hl-s {", ".hl-c {", ".hl-nf {"} {
		if !strings.Contains(css, want) {
			t.Errorf("CSS lacks %q", want)
		}
	}
	if strings.Contains(css, "hl-chroma") || strings.Contains(css, "background") {
		t.Errorf("CSS should only colour tokens:\n%s", css)
	}
}

func TestDetect(t *testing.T) {
	cases := map[string]string{
		"main.go": "Go", "go.mod": "Go Module",
		"src/app.ts": "TypeScript", "src/App.tsx": "TypeScript", "app.js": "JavaScript", "App.jsx": "JavaScript",
		"src/routes/+page.svelte": "Svelte", "App.vue": "Vue",
		"app/src/main/java/com/x/MainActivity.kt": "Kotlin", "Main.java": "Java", "View.swift": "Swift",
		"AppDelegate.m": "Objective-C", "main.c": "C", "util.h": "C", "x.cpp": "C++", "x.hpp": "C++", "Program.cs": "C#",
		"lib.rs": "Rust", "app.py": "Python", "app.rb": "Ruby", "Gemfile": "Ruby", "index.php": "PHP", "main.dart": "Dart",
		"build.sh": "Shell", "setup.ps1": "PowerShell", "schema.sql": "SQL",
		".github/workflows/ci.yml": "YAML", "config.yaml": "YAML", "Cargo.toml": "TOML", "pyproject.toml": "TOML",
		"package.json": "JSON", "tsconfig.json": "JSON", "AndroidManifest.xml": "XML", "index.html": "HTML",
		"style.css": "CSS", "style.scss": "SCSS", "README.md": "Markdown",
		"Dockerfile": "Dockerfile", "Dockerfile.dev": "Dockerfile", "Makefile": "Makefile",
		"build.gradle": "Groovy", "app/build.gradle.kts": "Kotlin", "settings.gradle.kts": "Kotlin",
		"api.proto": "Protocol Buffer", "schema.graphql": "GraphQL", "init.lua": "Lua", "mix.exs": "Elixir",
		"Main.hs": "Haskell", "build.zig": "Zig", "flake.nix": "Nix", "main.tf": "HCL", "config.hcl": "HCL",
		"setup.cfg": "INI", "php.ini": "INI", ".env": "Env", ".env.local": "Env", "gradle.properties": "Properties",
		"electron/main.mjs": "JavaScript", "svelte.config.js": "JavaScript", "vite.config.ts": "TypeScript",
	}
	for name, want := range cases {
		l := Detect(name, nil)
		if l.Name != want {
			t.Errorf("Detect(%q) = %q, want %q", name, l.Name, want)
		}
		if l.Lexer == "" {
			t.Errorf("Detect(%q) has no lexer", name)
		}
	}
	if l := Detect("notes.txt", nil); l.Lexer != "" {
		t.Errorf("txt should be plain, got %+v", l)
	}
}

func TestDetectShebang(t *testing.T) {
	cases := map[string]string{
		"#!/bin/bash\necho hi\n":              "Shell",
		"#!/usr/bin/env python3\nprint(1)\n":  "Python",
		"#!/usr/bin/env -S node --flag\nx\n":  "JavaScript",
		"#!/usr/bin/python3.12\nimport os\n":  "Python",
		"#!/usr/bin/env ruby\nputs 1\n":       "Ruby",
		"#!/usr/bin/env bun\nconsole.log()\n": "TypeScript",
	}
	for src, want := range cases {
		if l := Detect("bin/tool", []byte(src)); l.Name != want {
			t.Errorf("shebang %q = %q, want %q", src, l.Name, want)
		}
	}
	// The name wins over the shebang.
	if l := Detect("x.py", []byte("#!/bin/sh\n")); l.Name != "Python" {
		t.Errorf("name should win: %q", l.Name)
	}
}

func TestFileUsesClassesOnly(t *testing.T) {
	src := "package main\n\n// hello\nfunc main() {\n\ts := \"hi\"\n\tprintln(s, 42)\n}\n"
	r := File("main.go", []byte(src))
	if r.Lang != "Go" || r.Note != "" {
		t.Fatalf("lang %q note %q", r.Lang, r.Note)
	}
	if len(r.Lines) != 7 {
		t.Fatalf("lines = %d", len(r.Lines))
	}
	all := join(r.Lines)
	for _, want := range []string{`<span class="hl-kn">package</span>`, `class="hl-c1">// hello</span>`, `class="hl-s">&#34;hi&#34;</span>`, `class="hl-mi">42</span>`} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %s in\n%s", want, all)
		}
	}
	if strings.Contains(all, "style=") || strings.Contains(all, "\n") {
		t.Errorf("inline style or newline in output:\n%s", all)
	}
}

func TestEscaping(t *testing.T) {
	evil := []string{"</script><script>alert(1)</script>", `<img src=x onerror="alert(1)">`, "&amp; &lt;", "'><svg onload=alert(1)>"}
	files := map[string]string{
		"x.go":     "package x\n// %s\nvar s = \"%s\"\nvar r = `%s`\n",
		"x.html":   "<p>%s</p>\n<!-- %s -->\n<script>var a = '%s';</script>\n",
		"x.svelte": "<script>\n  let a = \"%s\";\n</script>\n<p title=\"%s\">%s</p>\n",
		"x.md":     "# %s\n\n`%s`\n\n%s\n",
		"x.kt":     "val a = \"%s\" // %s\n/* %s */\n",
		"x.py":     "a = '%s'  # %s\n\"\"\"%s\"\"\"\n",
		"x.ts":     "const a = `%s`; // %s\n/** %s */\n",
		"x.txt":    "%s %s %s\n",
	}
	for name, tmpl := range files {
		for _, e := range evil {
			src := strings.ReplaceAll(tmpl, "%s", e)
			r := File(name, []byte(src))
			out := join(r.Lines)
			checkEscaped(t, name, out)
			if got := unescapeText(out); got != strings.Join(strings.Split(strings.TrimSuffix(src, "\n"), "\n"), "") {
				t.Errorf("%s: text changed:\n got %q\nwant %q", name, got, src)
			}
		}
	}
}

func checkEscaped(t *testing.T, name, out string) {
	t.Helper()
	for _, bad := range []string{"<script", "</script", "<img", "<svg", "<p", "<!--", "onerror=\"", "style="} {
		if strings.Contains(out, bad) {
			t.Errorf("%s: unescaped %q in %s", name, bad, out)
		}
	}
	// Only our own span tags.
	rest := strings.NewReplacer("</span>", "").Replace(out)
	for i := strings.Index(rest, "<"); i >= 0; i = strings.Index(rest, "<") {
		if !strings.HasPrefix(rest[i:], `<span class="hl-`) {
			t.Fatalf("%s: unexpected tag at %q", name, rest[i:min(len(rest), i+40)])
		}
		rest = rest[i+1:]
	}
}

// unescapeText strips our spans and unescapes entities.
func unescapeText(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		if strings.HasPrefix(s, "<") {
			j := strings.IndexByte(s, '>')
			s = s[j+1:]
			continue
		}
		b.WriteByte(s[0])
		s = s[1:]
	}
	return html.UnescapeString(b.String())
}

func TestLargeAndMinified(t *testing.T) {
	big := bytes.Repeat([]byte("var x = 1;\n"), MaxBytes/10)
	r := File("big.js", big)
	if r.Note == "" || r.Lang != "JavaScript" || strings.Contains(string(r.Lines[0]), "hl-") {
		t.Errorf("large file should be plain with a note: %q %q", r.Note, r.Lines[0])
	}
	min := []byte("var a=1;" + strings.Repeat("function f(){return 1}", 400) + "\n")
	r = File("app.min.js", min)
	if !strings.Contains(r.Note, "long lines") || strings.Contains(string(r.Lines[0]), "hl-") {
		t.Errorf("minified file should be plain: %q", r.Note)
	}
	wide := []byte(strings.Repeat(strings.Repeat("a", 600)+"\n", 20))
	if r := File("x.js", wide); !strings.Contains(r.Note, "minified") {
		t.Errorf("mostly long lines should be plain: %q", r.Note)
	}
	// The plain fallback is still escaped.
	r = File("x.js", []byte(strings.Repeat("<", MaxLineLen+1)))
	if strings.Contains(string(r.Lines[0]), "<") {
		t.Error("plain fallback not escaped")
	}
}

func TestBudget(t *testing.T) {
	lines := strings.Split(strings.Repeat("x = 1\n", 50000), "\n")
	if _, ok := highlightLines("Python", lines, time.Now().Add(-time.Second)); ok {
		t.Error("an expired budget should give up")
	}
}

func TestCache(t *testing.T) {
	src := []byte("fun main() { println(\"hi\") }\n")
	a := File("Main.kt", src)
	if _, ok := cache.get(cacheKey("Kotlin", src)); !ok {
		t.Fatal("not cached")
	}
	b := File("Other.kt", src)
	if join(a.Lines) != join(b.Lines) {
		t.Error("cached result differs")
	}
	for i := 0; i < 100; i++ {
		File("x.kt", []byte(strings.Repeat("val a = 1\n", i+1)))
	}
	if cache.ll.Len() > cache.max {
		t.Errorf("cache grew to %d", cache.ll.Len())
	}
}

func TestDiffKeepsLinesAndCarriesState(t *testing.T) {
	lines := []DiffLine{
		{"hunk", "@@ -1,4 +1,5 @@"},
		{"ctx", "package x"},
		{"del", "/* old"},
		{"add", "/* new comment"},
		{"add", "still comment */"},
		{"del", "end */"},
		{"ctx", "func f() {}"},
		{"meta", `\ No newline at end of file`},
		{"hunk", "@@ -20,2 +21,2 @@"},
		{"add", "var s = \"<img onerror=x>\""},
	}
	out := Diff("x.go", lines)
	if len(out) != len(lines) {
		t.Fatalf("got %d lines, want %d", len(out), len(lines))
	}
	if string(out[0]) != "@@ -1,4 +1,5 @@" {
		t.Errorf("hunk header changed: %s", out[0])
	}
	if !strings.Contains(string(out[4]), "hl-cm") {
		t.Errorf("an open comment should carry to the next added line: %s", out[4])
	}
	if !strings.Contains(string(out[5]), "hl-cm") {
		t.Errorf("an open comment should carry to the next removed line: %s", out[5])
	}
	if !strings.Contains(string(out[6]), "hl-kd") && !strings.Contains(string(out[6]), "hl-k") {
		t.Errorf("context after a closed comment should be code: %s", out[6])
	}
	for i, o := range out {
		checkEscaped(t, "diff", string(o))
		if got := unescapeText(string(o)); got != lines[i].Text {
			t.Errorf("line %d text changed: %q != %q", i, got, lines[i].Text)
		}
	}
	// Unknown language: escaped plain text.
	if out := Diff("notes.txt", []DiffLine{{"add", "<b>"}}); out[0] != "&lt;b&gt;" {
		t.Errorf("plain diff: %s", out[0])
	}
}

func TestMarkdownFences(t *testing.T) {
	md := goldmark.New(goldmark.WithExtensions(extension.GFM, Markdown))
	src := "# Hi\n\n```kotlin\nfun main() = println(\"</script><img onerror=alert(1)>\")\n```\n\n```\nplain <b>\n```\n\n```nosuchlang\nx < y\n```\n\n```ts\nconst a: number = 1\n```\n"
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{`<pre class="hl-pre"><code class="language-kotlin"><span class="hl-k">fun</span>`, "<pre><code>plain &lt;b&gt;\n</code></pre>",
		`<pre><code class="language-nosuchlang">x &lt; y`, `<code class="language-ts"><span class="hl-kr">const</span>`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, "<img") || strings.Contains(out, "</script>") || strings.Contains(out, "style=") {
		t.Errorf("unsafe output:\n%s", out)
	}
	// An attribute-breaking fence language is dropped.
	buf.Reset()
	_ = md.Convert([]byte("```go\"><script>\nx\n```\n"), &buf)
	if strings.Contains(buf.String(), "<script>") {
		t.Errorf("fence info leaked: %s", buf.String())
	}
}

func TestLexerForHint(t *testing.T) {
	for hint, want := range map[string]string{"go": "Go", "golang": "Go", "kotlin": "Kotlin", "kt": "Kotlin", "ts": "TypeScript",
		"tsx": "TypeScript", "jsx": "react", "py": "Python", "sh": "Bash", "bash": "Bash", "yaml": "YAML", "svelte": "Svelte",
		"Protocol Buffer": "Protocol Buffer", "text": "", "": "", "hcl": "Terraform"} {
		if got := LexerForHint(hint); got != want {
			t.Errorf("LexerForHint(%q) = %q, want %q", hint, got, want)
		}
	}
	if langs := Languages(); len(langs) < 40 {
		t.Errorf("only %d languages", len(langs))
	}
}

func join(l []template.HTML) string {
	var b strings.Builder
	for _, x := range l {
		b.WriteString(string(x))
	}
	return b.String()
}
