package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSnippetComments(t *testing.T) {
	e := newSnipEnv(t)
	a := e.a
	ctx := context.Background()
	sn := e.create(t, snippetForm("zoe", "", "private", "a.txt", "a\n"))
	base := "/snippets/" + sn.UID

	// The owner comments; Markdown renders and raw HTML stays escaped.
	rec := do(t, a, "POST", base+"/comments", url.Values{"body": {"Looks **good**\r\n\r\n<script>alert(1)</script>"}}, e.owner)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "#snippet-comment-") {
		t.Fatalf("comment = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cs, _ := a.db.SnippetComments(ctx, sn.ID)
	if len(cs) != 1 || cs[0].AuthorName != "zoe" || strings.Contains(cs[0].Body, "\r") {
		t.Fatalf("comments = %+v", cs)
	}
	c := cs[0]
	body := do(t, a, "GET", base, nil, e.owner).Body.String()
	if !strings.Contains(body, "<strong>good</strong>") || strings.Contains(body, "<script>alert(1)") ||
		!strings.Contains(body, fmt.Sprintf(`id="snippet-comment-%d"`, c.ID)) || !strings.Contains(body, `action="`+base+`/comments"`) {
		t.Errorf("snippet page doesn't show the comment as expected")
	}
	// Empty comments are refused.
	if rec := do(t, a, "POST", base+"/comments", url.Values{"body": {"  \n "}}, e.owner); !strings.Contains(rec.Header().Get("Location"), "comment=empty") {
		t.Errorf("empty comment = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if cs, _ := a.db.SnippetComments(ctx, sn.ID); len(cs) != 1 {
		t.Errorf("empty comment saved: %d comments", len(cs))
	}

	// A private snippet's comments are hidden from someone who can't read it:
	// bob can't see, add, edit or delete them, and nothing reveals it exists.
	if rec := do(t, a, "GET", base, nil, e.other); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "good") {
		t.Errorf("bob view of private snippet = %d", rec.Code)
	}
	for _, p := range []string{base + "/comments", fmt.Sprintf("%s/comments/%d/edit", base, c.ID), fmt.Sprintf("%s/comments/%d/delete", base, c.ID)} {
		if rec := do(t, a, "POST", p, url.Values{"body": {"bob"}}, e.other); rec.Code != http.StatusNotFound {
			t.Errorf("bob POST %s = %d, want 404", p, rec.Code)
		}
	}
	// Anonymous: the same, through the sign-in redirect.
	if rec := do(t, a, "POST", base+"/comments", url.Values{"body": {"anon"}}, nil); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin") {
		t.Errorf("anonymous comment = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Public: anyone who can read sees comments, but commenting needs write,
	// exactly like issue comments.
	if err := a.db.UpdateSnippet(ctx, sn.ID, "", false); err != nil {
		t.Fatal(err)
	}
	rec = do(t, a, "GET", base, nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<strong>good</strong>") || strings.Contains(rec.Body.String(), `action="`+base+`/comments"`) {
		t.Errorf("anonymous view of a public snippet's comments = %d", rec.Code)
	}
	body = do(t, a, "GET", base, nil, e.other).Body.String()
	if !strings.Contains(body, "<strong>good</strong>") || strings.Contains(body, `action="`+base+`/comments"`) || strings.Contains(body, "/delete\" data-confirm=\"Delete this comment") {
		t.Errorf("bob sees a comment box or delete button on a public snippet")
	}
	if rec := do(t, a, "POST", base+"/comments", url.Values{"body": {"bob"}}, e.other); rec.Code != http.StatusForbidden {
		t.Errorf("bob comment on public snippet = %d, want 403", rec.Code)
	}
	if rec := do(t, a, "POST", fmt.Sprintf("%s/comments/%d/delete", base, c.ID), nil, e.other); rec.Code != http.StatusForbidden {
		t.Errorf("bob delete = %d, want 403", rec.Code)
	}

	// The author edits and deletes their own comment.
	rec = do(t, a, "POST", fmt.Sprintf("%s/comments/%d/edit", base, c.ID), url.Values{"body": {"Edited _now_"}}, e.owner)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit = %d", rec.Code)
	}
	if c2, _ := a.db.SnippetCommentByID(ctx, c.ID); c2.Body != "Edited _now_" {
		t.Errorf("edited body = %q", c2.Body)
	}
	// A comment id from another snippet is not found here.
	other := e.create(t, snippetForm("zoe", "", "private", "b.txt", "b\n"))
	if rec := do(t, a, "POST", fmt.Sprintf("/snippets/%s/comments/%d/delete", other.UID, c.ID), nil, e.owner); rec.Code != http.StatusNotFound {
		t.Errorf("cross-snippet delete = %d, want 404", rec.Code)
	}
	if rec := do(t, a, "POST", fmt.Sprintf("%s/comments/%d/delete", base, c.ID), nil, e.owner); rec.Code != http.StatusSeeOther {
		t.Errorf("delete = %d", rec.Code)
	}
	if cs, _ := a.db.SnippetComments(ctx, sn.ID); len(cs) != 0 {
		t.Errorf("comments after delete = %d", len(cs))
	}

	// Deleting the snippet deletes its comments.
	do(t, a, "POST", base+"/comments", url.Values{"body": {"again"}}, e.owner)
	if rec := do(t, a, "POST", base+"/delete", nil, e.owner); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete snippet = %d", rec.Code)
	}
	var n int
	_ = a.db.DB().QueryRow(`SELECT COUNT(*) FROM snippet_comments WHERE snippet_id=?`, sn.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d comments left after deleting the snippet", n)
	}
}
