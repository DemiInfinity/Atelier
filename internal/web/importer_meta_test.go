package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/store"
)

func TestImportGitHubIssuesAndResume(t *testing.T) {
	a := newTestApp(t)
	ck := featureSession(t, a)
	ctx := context.Background()
	bare := importSourceRepo(t)

	var mu sync.Mutex
	limited := true
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_ok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		lim := limited
		mu.Unlock()
		enc := json.NewEncoder(w)
		switch r.URL.Path {
		case "/user":
			enc.Encode(map[string]any{"login": "demi-gh"})
		case "/user/repos":
			enc.Encode([]map[string]any{{"full_name": "demi-gh/one", "name": "one", "owner": map[string]any{"login": "demi-gh"},
				"private": true, "clone_url": bare}})
		case "/repos/demi-gh/one/labels":
			enc.Encode([]map[string]any{{"name": "bug", "color": "d73a4a"}})
		case "/repos/demi-gh/one/milestones":
			enc.Encode([]map[string]any{})
		case "/repos/demi-gh/one/issues":
			enc.Encode([]map[string]any{
				{"number": 1, "title": "Broken", "state": "open", "user": map[string]any{"login": "alice"}, "comments": 1,
					"labels": []any{map[string]any{"name": "bug", "color": "d73a4a"}}, "created_at": "2024-01-01T00:00:00Z", "updated_at": "2024-01-02T00:00:00Z"},
				{"number": 2, "title": "Topic", "state": "open", "user": map[string]any{"login": "bob"}, "pull_request": map[string]any{},
					"created_at": "2024-01-03T00:00:00Z", "updated_at": "2024-01-03T00:00:00Z"},
			})
		case "/repos/demi-gh/one/issues/1/comments":
			if lim {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(-time.Second).Unix(), 10))
				w.WriteHeader(http.StatusForbidden)
				return
			}
			enc.Encode([]map[string]any{{"id": 7, "user": map[string]any{"login": "bob"}, "body": "same here", "created_at": "2024-01-02T00:00:00Z"}})
		case "/repos/demi-gh/one/pulls/2":
			enc.Encode(map[string]any{"head": map[string]any{"ref": "topic", "sha": "abc", "repo": map[string]any{"full_name": "demi-gh/one"}},
				"base": map[string]any{"ref": "main", "sha": "def", "repo": map[string]any{"full_name": "demi-gh/one"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	old := githubAPI
	githubAPI = api.URL
	defer func() { githubAPI = old }()

	rec := do(t, a, "POST", "/settings/import/list", url.Values{"source": {"github"}, "token": {"ghp_ok"}}, ck)
	loc := rec.Header().Get("Location")
	pick := do(t, a, "GET", loc, nil, ck).Body.String()
	for _, want := range []string{`name="meta_issues" checked`, `name="meta_pulls" checked`, `name="meta_labels" checked`, `name="meta_milestones" checked`} {
		if !strings.Contains(pick, want) {
			t.Errorf("pick page lacks %s", want)
		}
	}
	rec = do(t, a, "POST", loc, url.Values{"owner": {"demi"}, "repo": {"0"}, "meta_issues": {"on"}, "meta_pulls": {"on"},
		"meta_labels": {"on"}, "meta_milestones": {"on"}}, ck)
	if rec.Code != 303 {
		t.Fatalf("pick submit = %d\n%s", rec.Code, rec.Body)
	}
	jobs := waitJobs(t, a, ck, 1)
	j := jobs[0]
	if j.Status != "partial" || !j.Resumable || !strings.Contains(j.Message, "rate limit") || j.URL != "/demi/one" {
		t.Fatalf("job = %+v", j)
	}
	states := map[string]string{}
	for _, s := range j.Stages {
		states[s.Key] = s.State
	}
	if states["code"] != "done" || states["labels"] != "done" || states["issues"] != "partial" || states["pulls"] != "pending" {
		t.Errorf("stages = %+v", j.Stages)
	}
	repo, err := a.db.RepoByName(ctx, "demi", "one")
	if err != nil {
		t.Fatal(err)
	}
	if is, err := a.db.IssueByNumber(ctx, repo.ID, 1); err != nil || is.AuthorName != "github:alice" {
		t.Errorf("issue 1 = %+v %v", is, err)
	}

	page := do(t, a, "GET", "/settings/import", nil, ck).Body.String()
	for _, want := range []string{"im-stages", "im-st-partial", fmt.Sprintf("/settings/import/jobs/%d/resume", j.ID), `name="token"`, "Coming soon"} {
		if !strings.Contains(page, want) {
			t.Errorf("import page lacks %q", want)
		}
	}
	if strings.Contains(page, "ghp_ok") {
		t.Fatal("token on the page")
	}

	// Resuming needs the token again (one-off imports don't keep it).
	rec = do(t, a, "POST", fmt.Sprintf("/settings/import/jobs/%d/resume", j.ID), url.Values{}, ck)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "access token") {
		t.Fatalf("resume without token = %d", rec.Code)
	}
	mu.Lock()
	limited = false
	mu.Unlock()
	rec = do(t, a, "POST", fmt.Sprintf("/settings/import/jobs/%d/resume", j.ID), url.Values{"token": {"ghp_ok"}}, ck)
	if rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "resumed") {
		t.Fatalf("resume = %d %s", rec.Code, rec.Body)
	}
	jobs = waitJobs(t, a, ck, 2)
	j2 := jobs[0]
	if j2.Status != "done" || !strings.Contains(j2.Summary, "1 pull request") || !strings.Contains(j2.Summary, "1 comment") {
		t.Fatalf("resumed job = %+v", j2)
	}
	if j2.Stages[0].Key != "code" || j2.Stages[0].State != "skipped" {
		t.Errorf("code stage on resume = %+v", j2.Stages[0])
	}
	// Only the newest job for the repo offers to import again.
	if jobs[1].Resumable || !j2.Resumable {
		t.Errorf("resumable: old %v new %v", jobs[1].Resumable, j2.Resumable)
	}
	is, _ := a.db.IssueByNumber(ctx, repo.ID, 1)
	if cs, _ := a.db.IssueComments(ctx, is.ID); len(cs) != 1 {
		t.Errorf("comments = %d", len(cs))
	}
	p, err := a.db.PullByNumber(ctx, repo.ID, 2)
	if err != nil || p.State != "closed" {
		t.Errorf("pull 2 = %+v %v", p, err)
	}
	// The imported pull request page renders even though its branch is gone.
	if rec := do(t, a, "GET", "/demi/one/pulls/2", nil, ck); rec.Code != 200 || !strings.Contains(rec.Body.String(), "no longer exists") {
		t.Errorf("pull page = %d", rec.Code)
	}
	if rec := do(t, a, "GET", "/demi/one/issues/1", nil, ck); rec.Code != 200 || !strings.Contains(rec.Body.String(), "github:alice") {
		t.Errorf("issue page = %d", rec.Code)
	}
	// New issues continue after the imported numbers.
	if n, _ := a.db.RepoCounter(ctx, repo.ID); n != 3 {
		t.Errorf("counter = %d", n)
	}
	// Import again: nothing doubles.
	rec = do(t, a, "POST", fmt.Sprintf("/settings/import/jobs/%d/resume", j2.ID), url.Values{"token": {"ghp_ok"}}, ck)
	if rec.Code != 303 {
		t.Fatalf("again = %d", rec.Code)
	}
	jobs = waitJobs(t, a, ck, 3)
	if jobs[0].Status != "done" || !strings.Contains(jobs[0].Message, "Nothing new") {
		t.Errorf("third job = %+v", jobs[0])
	}
	if all, _ := a.db.Pulls(ctx, repo.ID, ""); len(all) != 1 {
		t.Errorf("pulls = %d", len(all))
	}
	for _, jj := range jobs {
		if strings.Contains(jj.Message, "ghp_ok") {
			t.Fatal("token in a job message")
		}
	}
}

func TestImportInterruptedStages(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()
	id, err := a.db.CreateImportJob(ctx, store.ImportJob{Source: "github", SourceURL: "https://github.com/o/r.git", Owner: "demi", Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	_ = a.db.SetImportJobStatus(ctx, id, "running", "Importing…")
	if err := a.db.SaveImportJobMeta(ctx, store.ImportJobMeta{JobID: id, Options: "issues",
		Stages: `[{"key":"code","label":"Code","state":"done"},{"key":"issues","label":"Issues","state":"running","done":3,"total":9}]`}); err != nil {
		t.Fatal(err)
	}
	a.failInterruptedStages(ctx)
	m, _ := a.db.ImportJobMeta(ctx, id)
	if !strings.Contains(m.Stages, `"state":"failed"`) || strings.Contains(m.Stages, `"running"`) {
		t.Errorf("stages = %s", m.Stages)
	}
}
