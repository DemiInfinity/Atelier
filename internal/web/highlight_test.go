package web

import (
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"atelier/internal/gitrepo"
	"atelier/internal/highlight"
)

var hlTag = regexp.MustCompile(`<[^>]*>`)

func hlText(s string) string { return html.UnescapeString(hlTag.ReplaceAllString(s, "")) }

func TestHighlightLanguagesAlignWithLanguageBar(t *testing.T) {
	for ext, d := range langByExt {
		got := highlight.Detect("x"+ext, nil)
		if got.Name != d.Name {
			t.Errorf("%s: highlight says %q, the language bar %q", ext, got.Name, d.Name)
		}
	}
	for name, d := range langByName {
		if got := highlight.Detect(name, nil); got.Name != d.Name {
			t.Errorf("%s: highlight says %q, the language bar %q", name, got.Name, d.Name)
		}
	}
}

func TestDiffLinesHighlighted(t *testing.T) {
	patch := "diff --git a/main.go b/main.go\nindex 1..2 100644\n--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,3 @@\n package main\n-var s = \"</script><img onerror=alert(1)>\"\n+var s = `/* not a comment */`\n \n\\ No newline at end of file\n" +
		"diff --git a/b.txt b/b.txt\n--- a/b.txt\n+++ b/b.txt\n@@ -1 +1 @@\n-<b>old\n+<i>new\n"
	got := diffLines(patch)
	plainKinds := []string{"file", "meta", "meta", "meta", "hunk", "", "del", "add", "", "", "file", "meta", "meta", "hunk", "del", "add", ""}
	if len(got) != len(plainKinds) {
		t.Fatalf("%d lines, want %d", len(got), len(plainKinds))
	}
	for i, l := range got {
		if l.Class != plainKinds[i] {
			t.Errorf("line %d kind %q, want %q (%q)", i, l.Class, plainKinds[i], l.Text)
		}
		if hlText(string(l.Code)) != l.Text {
			t.Errorf("line %d text changed: %q → %q", i, l.Text, l.Code)
		}
		if strings.Contains(string(l.Code), "style=") || strings.Contains(string(l.Code), "<img") || strings.Contains(string(l.Code), "</script") {
			t.Errorf("line %d unsafe: %s", i, l.Code)
		}
	}
	if !strings.Contains(string(got[5].Code), `class="hl-kn">package`) || !strings.Contains(string(got[6].Code), "hl-s") {
		t.Errorf("Go lines not coloured: %s | %s", got[5].Code, got[6].Code)
	}
	if strings.Contains(string(got[14].Code), "hl-") || got[14].Code != "-&lt;b&gt;old" {
		t.Errorf("text file should be plain: %s", got[14].Code)
	}
	// GitHub's per-file patches have no header: the name comes from the caller.
	gh := diffLines("@@ -1 +1 @@\n-fun a() = 1\n+fun b() = 2", "app/Main.kt")
	if !strings.Contains(string(gh[2].Code), "hl-") || gh[2].Class != "add" {
		t.Errorf("gh patch not coloured: %+v", gh[2])
	}
}

func TestPullFileViewsHighlighted(t *testing.T) {
	diff := &gitrepo.Diff{Files: []gitrepo.DiffFile{{Path: "src/App.svelte", Lines: []gitrepo.DiffLine{
		{Kind: "hunk", Text: "@@ -1,3 +1,4 @@"},
		{Kind: "ctx", Old: 1, New: 1, Text: "<script>"},
		{Kind: "del", Old: 2, Text: "  let n = 1;"},
		{Kind: "add", New: 2, Text: "  let n = \"<img onerror=x>\";"},
		{Kind: "add", New: 3, Text: "  // note"},
		{Kind: "ctx", Old: 3, New: 4, Text: "</script>"},
	}}}}
	views := pullFileViews(diff, nil, "abc", nil, "/zoe/demo/pulls/1", "/zoe/demo")
	lv := views[0].LineViews
	if len(lv) != 6 {
		t.Fatalf("%d lines", len(lv))
	}
	want := []struct {
		kind, side string
		num        int
	}{{"hunk", "", 0}, {"ctx", "new", 1}, {"del", "old", 2}, {"add", "new", 2}, {"add", "new", 3}, {"ctx", "new", 4}}
	for i, w := range want {
		if lv[i].Kind != w.kind || lv[i].Side != w.side || lv[i].Num != w.num {
			t.Errorf("line %d: %s %s %d, want %+v", i, lv[i].Kind, lv[i].Side, lv[i].Num, w)
		}
		if hlText(string(lv[i].Code)) != lv[i].Text {
			t.Errorf("line %d text changed: %q", i, lv[i].Code)
		}
	}
	if !strings.Contains(string(lv[2].Code), "hl-") || !strings.Contains(string(lv[4].Code), "hl-c") {
		t.Errorf("not coloured: %s | %s", lv[2].Code, lv[4].Code)
	}
	if strings.Contains(string(lv[3].Code), "<img") {
		t.Errorf("unescaped: %s", lv[3].Code)
	}
}

func TestMarkdownFencesHighlighted(t *testing.T) {
	out := string(renderMarkdown([]byte("```go\nfunc main() {}\n```\n\n```\n<script>x</script>\n```\n")))
	mustContain(t, out, "markdown", `<pre class="hl-pre"><code class="language-go"><span class="hl-kd">func</span>`, "&lt;script&gt;x&lt;/script&gt;")
	issue := string(renderIssueMarkdown("See #1\n\n```python\ndef f(): pass\n```\n", "/zoe/demo", func(int64) bool { return true }))
	mustContain(t, issue, "issue markdown", `<span class="hl-k">def</span>`)
}

func TestHighlightPages(t *testing.T) {
	f, ck, work := editFixture(t)
	a := f.a
	pullsCommit(t, work, "app/src/main/kotlin/Main.kt", "package app\n\n// </script><img src=x onerror=alert(1)>\nfun main() {\n    println(\"hi <b>\")\n}\n", "Kotlin")
	pullsCommit(t, work, "web/src/routes/+page.svelte", "<script lang=\"ts\">\n  let n: number = 1;\n</script>\n<p>{n}</p>\n", "Svelte")
	runGit(t, work, "push", "-q", "origin", "main")

	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/blob/main/app/src/main/kotlin/Main.kt", nil, ck), 200, "kotlin blob")
	mustContain(t, body, "kotlin blob", `<span class="hl-lang">Kotlin</span> · 6 lines`, `id="L3"`, `href="#L3"`, `<span class="hl-k">fun</span>`,
		"&lt;/script&gt;&lt;img src=x onerror=alert(1)&gt;", `/static/highlight.css`)
	if strings.Contains(body, "<img src=x") || strings.Contains(body, "style=") {
		t.Error("blob page has unescaped code or inline style")
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/blob/main/web/src/routes/+page.svelte", nil, ck), 200, "svelte blob")
	mustContain(t, body, "svelte blob", `<span class="hl-lang">Svelte</span>`, `hl-kt`)

	sha := strings.TrimSpace(runGit(t, work, "rev-parse", "HEAD"))
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/commit/"+sha, nil, ck), 200, "commit")
	mustContain(t, body, "commit", `<span class="dl add">+<span class="hl-`)

	// The editor: overlay, picker, and the textarea still carries the text (no JS).
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/edit/main/app/src/main/kotlin/Main.kt", nil, ck), 200, "editor")
	mustContain(t, body, "editor", `data-edit-stack data-hl-url="/zoe/demo/edit-highlight"`, `<pre class="edit-hl" aria-hidden="true" data-edit-hl>`,
		`<option value="">Auto (Kotlin)</option>`, `<option value="Python">Python</option>`, `data-edit-lang-field hidden`,
		`data-edit-text>package app`)
	rec := do(t, a, "POST", "/zoe/demo/edit-highlight", url.Values{"text": {"fun a() = \"</script>\"\n// x\n"}, "path": {"Main.kt"}}, ck)
	var res struct {
		Lang  string
		Lines []string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != 200 {
		t.Fatalf("edit-highlight %d: %s", rec.Code, rec.Body)
	}
	if res.Lang != "Kotlin" || len(res.Lines) != 3 || !strings.Contains(res.Lines[1], "hl-c1") || strings.Contains(res.Lines[0], "</script>") {
		t.Errorf("edit-highlight: %+v", res)
	}
	rec = do(t, a, "POST", "/zoe/demo/edit-highlight", url.Values{"text": {"def f(): pass"}, "path": {"Main.kt"}, "lang": {"Python"}}, ck)
	if !strings.Contains(rec.Body.String(), `"lang":"Python"`) || !strings.Contains(rec.Body.String(), "hl-k") {
		t.Errorf("picker choice ignored: %s", rec.Body)
	}
	if rec := do(t, a, "POST", "/zoe/demo/edit-highlight", url.Values{"text": {"x"}}, nil); rec.Code == 200 {
		t.Errorf("edit-highlight without a session: %d", rec.Code)
	}
}
