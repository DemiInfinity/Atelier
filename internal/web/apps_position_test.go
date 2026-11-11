package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"atelier/internal/gitrepo"
)

func TestAppsPositionMapping(t *testing.T) {
	f := &gitrepo.DiffFile{Path: "x.go", Lines: []gitrepo.DiffLine{
		{Kind: "hunk", Text: "@@ -1,4 +1,4 @@"},    // position 0 (not addressable)
		{Kind: "ctx", Old: 1, New: 1, Text: "a"},   // 1
		{Kind: "del", Old: 2, Text: "b"},           // 2
		{Kind: "add", New: 2, Text: "B"},           // 3
		{Kind: "ctx", Old: 3, New: 3, Text: "c"},   // 4
		{Kind: "hunk", Text: "@@ -20,3 +20,2 @@"},  // 5: later headers count
		{Kind: "ctx", Old: 20, New: 20, Text: "t"}, // 6
		{Kind: "del", Old: 21, Text: "u"},          // 7
		{Kind: "ctx", Old: 22, New: 21, Text: "v"}, // 8
		{Kind: "meta", Text: `\ No newline at end of file`},
	}}
	cases := []struct {
		pos  int
		line int
		side string
		ok   bool
	}{
		{0, 0, "", false},
		{1, 1, "new", true},
		{2, 2, "old", true},
		{3, 2, "new", true},
		{4, 3, "new", true},
		{5, 0, "", false}, // the second hunk header
		{6, 20, "new", true},
		{7, 21, "old", true},
		{8, 21, "new", true},
		{9, 0, "", false},
		{-1, 0, "", false},
	}
	for _, c := range cases {
		line, side, ok := appsPositionLine(f, c.pos)
		if line != c.line || side != c.side || ok != c.ok {
			t.Errorf("position %d = %d %q %v, want %d %q %v", c.pos, line, side, ok, c.line, c.side, c.ok)
		}
		if c.ok {
			if back, ok := appsLinePosition(f, c.line, c.side); !ok || back != c.pos {
				t.Errorf("line %d %s = position %d %v, want %d", c.line, c.side, back, ok, c.pos)
			}
		}
	}
	if _, ok := appsLinePosition(f, 10, "new"); ok {
		t.Error("a line outside the diff has a position")
	}
	if h := appsDiffHunk(f, 7); h != "@@ -20,3 +20,2 @@\n t\n-u" {
		t.Errorf("diff hunk = %q", h)
	}
	// Positions agree with the patch the API serves.
	patch := strings.Split(appsPatch(f), "\n")
	if patch[7] != "-u" || patch[3] != "+B" {
		t.Errorf("patch lines = %q", patch)
	}
}

// Bots can post review comments by diff position, and pull requests carry
// labels through /api/v3.
func TestAppsAPIPullLabelsAndPositions(t *testing.T) {
	f := newAppsFix(t)
	ctx := context.Background()
	var orig strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&orig, "line %d\n", i)
	}
	pullsCommit(t, f.work, "f.txt", orig.String(), "Add f")
	runGit(t, f.work, "push", "-q", "origin", "main")
	runGit(t, f.work, "checkout", "-q", "-b", "fix")
	changed := strings.Replace(orig.String(), "line 3\n", "line three\n", 1)
	changed = strings.Replace(changed, "line 25\n", "", 1)
	pullsCommit(t, f.work, "f.txt", changed, "Change f")
	runGit(t, f.work, "push", "-q", "origin", "fix")
	if rec := do(t, f.a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"fix"}, "title": {"Fix"}}, f.ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("open pull = %d", rec.Code)
	}
	fixSHA, _ := gitBranch(t, f.a, "fix")
	_, tok := f.createBot(t, "reviewer", []int64{f.repo.ID}, "comment", "suggest")

	// The patch from /files, to count positions the way a GitHub bot does.
	rec := apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/pulls/1/files", tok, nil)
	files := []map[string]any{}
	decodeJSON(t, rec.Body.Bytes(), &files)
	var patch []string
	for _, fl := range files {
		if fl["filename"] == "f.txt" {
			patch = strings.Split(fl["patch"].(string), "\n")
		}
	}
	find := func(text string) int {
		for i, l := range patch {
			if l == text {
				return i
			}
		}
		t.Fatalf("%q not in patch %q", text, patch)
		return 0
	}
	cases := []struct {
		text, side string
		line       float64
	}{
		{"+line three", "RIGHT", 3},
		{"-line 3", "LEFT", 3},
		{"-line 25", "LEFT", 25}, // in the second hunk
		{" line 26", "RIGHT", 25},
	}
	for _, c := range cases {
		pos := find(c.text)
		rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/pulls/1/comments", tok,
			map[string]any{"body": "about " + c.text, "commit_id": fixSHA, "path": "f.txt", "position": pos})
		m := jsonOf(t, rec)
		if rec.Code != 201 || m["line"] != c.line || m["side"] != c.side || m["position"] != float64(pos) {
			t.Errorf("comment at position %d (%s) = %d line %v side %v position %v", pos, c.text, rec.Code, m["line"], m["side"], m["position"])
		}
		if h, _ := m["diff_hunk"].(string); !strings.HasPrefix(h, "@@") || !strings.HasSuffix(h, c.text) {
			t.Errorf("diff_hunk for %s = %q", c.text, h)
		}
	}
	hdr := 0
	for i, l := range patch {
		if i > 0 && strings.HasPrefix(l, "@@") {
			hdr = i
		}
	}
	if hdr == 0 {
		t.Fatalf("expected two hunks: %q", patch)
	}
	for _, bad := range []int{0, hdr, len(patch) + 3} {
		rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/pulls/1/comments", tok,
			map[string]any{"body": "x", "commit_id": fixSHA, "path": "f.txt", "position": bad})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("position %d = %d, want 422", bad, rec.Code)
		}
	}
	// Listing reports positions for line-anchored notes too.
	rec = apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/pulls/1/comments", tok, nil)
	list := []map[string]any{}
	decodeJSON(t, rec.Body.Bytes(), &list)
	if len(list) != len(cases) {
		t.Fatalf("listed %d comments", len(list))
	}
	for _, m := range list {
		if m["position"] == nil {
			t.Errorf("listed comment without position: %v", m)
		}
	}

	// Labels on the pull request, through the issues endpoints as on GitHub.
	if err := f.a.db.EnsureDefaultLabels(ctx, f.repo.ID); err != nil {
		t.Fatal(err)
	}
	rec = apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/pulls/1", tok, nil)
	if l, _ := jsonOf(t, rec)["labels"].([]any); len(l) != 0 {
		t.Fatalf("new pull labels = %v", l)
	}
	rec = apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/issues/1/labels", tok, map[string][]string{"labels": {"bug", "enhancement"}})
	if rec.Code != 200 {
		t.Fatalf("label pull = %d %s", rec.Code, rec.Body)
	}
	rec = apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/pulls/1", tok, nil)
	ls, _ := jsonOf(t, rec)["labels"].([]any)
	if len(ls) != 2 || dig(ls[0], "name") != "bug" || dig(ls[0], "color") == nil || dig(ls[1], "name") != "enhancement" {
		t.Fatalf("pull labels = %v", ls)
	}
	if _, ok := ls[0].(map[string]any)["default"]; !ok {
		t.Errorf("label shape = %v", ls[0])
	}
	rec = apiReq(t, f.a, "DELETE", "/api/v3/repos/zoe/demo/issues/1/labels/BUG", tok, nil)
	if rec.Code != 200 {
		t.Fatalf("unlabel pull = %d %s", rec.Code, rec.Body)
	}
	rec = apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/issues/1/labels", tok, nil)
	var names []map[string]any
	decodeJSON(t, rec.Body.Bytes(), &names)
	if len(names) != 1 || names[0]["name"] != "enhancement" {
		t.Errorf("pull labels after delete = %v", names)
	}
	if rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/issues/1/labels", tok, []string{"no-such-label"}); rec.Code != 422 {
		t.Errorf("unknown label = %d", rec.Code)
	}
}

func decodeJSON(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("not JSON: %v\n%.500s", err, b)
	}
}
