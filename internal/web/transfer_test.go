package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestMoveRepoBetweenAccounts(t *testing.T) {
	f := newGitFixture(t) // owner zoe, private zoe/demo, tokens read/write
	ctx := context.Background()
	co, _ := f.a.db.CreateOwner(ctx, "studio", "Studio", "company")
	srv := httptest.NewServer(f.a)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	at := func(path string) string { return "http://zoe:" + f.write + "@" + u.Host + path }

	// Some history to carry over.
	work := newWorkRepo(t)
	runGit(t, work, "push", at("/zoe/demo.git"), "main")

	sess, _ := f.a.db.CreateSession(ctx, time.Hour)
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
		return doReq(f.a, req)
	}

	// Wrong confirmation: nothing moves.
	if rec := post("/zoe/demo/transfer", url.Values{"owner": {"studio"}, "confirm": {"nope"}}); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "moveerr") {
		t.Fatalf("bad confirm: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Move to the company account.
	rec := post("/zoe/demo/transfer", url.Values{"owner": {"studio"}, "confirm": {"zoe/demo"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/studio/demo/settings?saved=1" {
		t.Fatalf("move: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	r, err := f.a.db.RepoByName(ctx, "studio", "demo")
	if err != nil || r.OwnerID != co.ID {
		t.Fatalf("not moved: %+v %v", r, err)
	}
	if g, err := f.a.git.Open("studio", "demo"); err != nil || g.IsEmpty(ctx) {
		t.Fatalf("history not at the new path: %v", err)
	}

	// The old web address redirects.
	req := httptest.NewRequest("GET", "/zoe/demo/commits/main?page=1", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
	if rec := doReq(f.a, req); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/studio/demo/commits/main?page=1" {
		t.Errorf("old link: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Clone and push through the old HTTPS address still work.
	dst := t.TempDir() + "/c"
	runGit(t, t.TempDir(), "clone", at("/zoe/demo.git"), dst)
	runGit(t, work, "commit", "--allow-empty", "-m", "After the move")
	runGit(t, work, "push", at("/zoe/demo.git"), "main")
	if g, _ := f.a.git.Open("studio", "demo"); g.LastCommit(ctx, "main", "").Subject != "After the move" {
		t.Error("push through the old address didn't land in the moved repo")
	}

	// Someone else can't move it.
	other, _ := f.a.db.CreateOwner(ctx, "visitor", "", "user")
	osess, _ := f.a.db.CreateSessionFor(ctx, other.ID, time.Hour)
	req = httptest.NewRequest("POST", "/studio/demo/transfer", strings.NewReader("owner=visitor&confirm=studio%2Fdemo"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: osess})
	if rec := doReq(f.a, req); rec.Code == http.StatusSeeOther && !strings.Contains(rec.Header().Get("Location"), "moveerr") {
		t.Errorf("visitor moved the repo: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if r2, _ := f.a.db.RepoByID(ctx, r.ID); r2.Owner != "studio" {
		t.Errorf("repo owner changed to %s", r2.Owner)
	}
	_ = exec.Command
}
