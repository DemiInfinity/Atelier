package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// importedRepo clones sourceRepo (main, feature, tag v1) into Atelier.
func importedRepo(t *testing.T) (*store.Store, *gitrepo.Store, store.Repo, string, string) {
	t.Helper()
	isolateGit(t)
	db, gs, _ := newStores(t)
	bare, work := sourceRepo(t)
	repo, err := Import(context.Background(), db, gs, Request{Source: "github", URL: bare, Owner: "demi", Name: "proj"})
	if err != nil {
		t.Fatal(err)
	}
	mainSHA := git(t, work, "rev-parse", "main")
	featSHA := git(t, work, "rev-parse", "feature")
	return db, gs, repo, mainSHA, featSHA
}

var t0 = time.Date(2024, 3, 1, 10, 0, 0, 0, time.UTC)

func ts(days int) string { return t0.Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339) }

func user(login string) map[string]any { return map[string]any{"login": login} }

// fakeGitHub serves one repository's labels, milestones, issues and pull
// requests, two items per page, with Link headers like GitHub.
type fakeGitHub struct {
	mu       sync.Mutex
	mainSHA  string
	featSHA  string
	limitAt  string // path+query that answers 403 rate limited
	zeroAt   string // path+query that answers 200 with X-RateLimit-Remaining: 0
	requests []string
}

func (f *fakeGitHub) issues() []map[string]any {
	return []map[string]any{
		{"number": 1, "title": "Crash on start", "body": "It crashes. See #2.", "state": "open", "user": user("alice"),
			"labels":    []any{map[string]any{"name": "bug", "color": "d73a4a"}},
			"milestone": map[string]any{"title": "v1.0", "state": "open", "created_at": ts(0), "due_on": ts(30)},
			"comments":  3, "created_at": ts(1), "updated_at": ts(2), "html_url": "https://github.com/o/r/issues/1"},
		{"number": 2, "title": "Fix crash", "body": "Fixes #1", "state": "closed", "user": user("bob"), "comments": 1,
			"created_at": ts(2), "updated_at": ts(3), "closed_at": ts(3), "pull_request": map[string]any{"merged_at": ts(3)}},
		{"number": 3, "title": "Abandoned idea", "state": "closed", "user": user("bob"), "created_at": ts(3),
			"updated_at": ts(4), "closed_at": ts(4), "pull_request": map[string]any{}},
		{"number": 5, "title": "Won't do", "state": "closed", "state_reason": "not_planned", "user": nil,
			"labels":     []any{map[string]any{"name": "wontfix", "color": "123456"}},
			"created_at": ts(5), "updated_at": ts(6), "closed_at": ts(6)},
		{"number": 6, "title": "Gone branch", "state": "open", "user": user("carol"), "created_at": ts(6),
			"updated_at": ts(6), "pull_request": map[string]any{}},
		{"number": 7, "title": "Still open", "state": "open", "user": user("carol"), "created_at": ts(7),
			"updated_at": ts(7), "pull_request": map[string]any{}},
		{"number": 8, "title": "From a fork", "state": "open", "user": user("dave"), "created_at": ts(8),
			"updated_at": ts(8), "pull_request": map[string]any{}},
	}
}

func branch(ref, sha, repo string) map[string]any {
	b := map[string]any{"ref": ref, "sha": sha, "label": strings.Split(repo, "/")[0] + ":" + ref}
	if repo != "" {
		b["repo"] = map[string]any{"full_name": repo}
	} else {
		b["repo"] = nil
	}
	return b
}

func (f *fakeGitHub) pulls() map[string]any {
	return map[string]any{
		"2": map[string]any{"head": branch("feature", f.featSHA, "o/r"), "base": branch("main", f.mainSHA, "o/r"),
			"merged": true, "merged_at": ts(3), "merge_commit_sha": f.mainSHA, "merged_by": user("carol")},
		"3": map[string]any{"head": branch("feature", f.featSHA, "o/r"), "base": branch("main", f.mainSHA, "o/r")},
		"6": map[string]any{"head": branch("gone", "0123456789abcdef0123456789abcdef01234567", "o/r"), "base": branch("main", f.mainSHA, "o/r")},
		"7": map[string]any{"head": branch("feature", f.featSHA, "o/r"), "base": branch("main", f.mainSHA, "o/r")},
		"8": map[string]any{"head": branch("main", f.featSHA, "dave/r"), "base": branch("main", f.mainSHA, "o/r")},
	}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.RequestURI())
	limitAt, zeroAt := f.limitAt, f.zeroAt
	f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer ghp_meta" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page == 0 {
		page = 1
	}
	key := r.URL.Path + "?page=" + strconv.Itoa(page)
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	if key == limitAt {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", reset)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		return
	}
	if key == zeroAt {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", reset)
	} else {
		w.Header().Set("X-RateLimit-Remaining", "4000")
	}
	// paged serves two items per page with a GitHub Link header.
	paged := func(items []map[string]any) {
		start, end := (page-1)*2, page*2
		if start > len(items) {
			start = len(items)
		}
		if end >= len(items) {
			end = len(items)
		} else {
			q := r.URL.Query()
			q.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
		}
		json.NewEncoder(w).Encode(items[start:end])
	}
	switch p := strings.TrimPrefix(r.URL.Path, "/repos/o/r"); {
	case p == "/labels":
		paged([]map[string]any{
			{"name": "bug", "color": "d73a4a", "description": "Broken"},
			{"name": `needs "triage"`, "color": "ABCDEF"},
			{"name": "area/ui", "color": "00ff00"},
		})
	case p == "/milestones":
		if r.URL.Query().Get("state") != "all" {
			t := "milestones without state=all"
			http.Error(w, t, 400)
			return
		}
		paged([]map[string]any{
			{"title": "v1.0", "state": "open", "description": "First", "created_at": ts(0), "due_on": ts(30)},
			{"title": "v0.9", "state": "closed", "created_at": ts(-10), "closed_at": ts(-1)},
		})
	case p == "/issues":
		if r.URL.Query().Get("state") != "all" {
			http.Error(w, "issues without state=all", 400)
			return
		}
		paged(f.issues())
	case strings.HasPrefix(p, "/issues/") && strings.HasSuffix(p, "/comments"):
		n := strings.TrimSuffix(strings.TrimPrefix(p, "/issues/"), "/comments")
		var cs []map[string]any
		switch n {
		case "1":
			cs = []map[string]any{
				{"id": 101, "user": user("bob"), "body": "Me too", "created_at": ts(1), "updated_at": ts(1)},
				{"id": 102, "user": user("alice"), "body": "Logs attached", "created_at": ts(2)},
				{"id": 103, "user": user("carol"), "body": "Fixed in #2", "created_at": ts(3)},
			}
		case "2":
			cs = []map[string]any{{"id": 201, "user": user("carol"), "body": "LGTM", "created_at": ts(3)}}
		}
		paged(cs)
	case strings.HasPrefix(p, "/pulls/"):
		pr, ok := f.pulls()[strings.TrimPrefix(p, "/pulls/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(pr)
	default:
		http.NotFound(w, r)
	}
}

func allOptions() MetaOptions {
	return MetaOptions{Labels: true, Milestones: true, Issues: true, Pulls: true}
}

func TestImportMetaGitHub(t *testing.T) {
	ctx := context.Background()
	db, gs, repo, mainSHA, featSHA := importedRepo(t)
	fake := &fakeGitHub{mainSHA: mainSHA, featSHA: featSHA}
	api := httptest.NewServer(fake)
	defer api.Close()
	req := MetaRequest{Source: "github", APIBase: api.URL, Repo: "o/r", Token: "ghp_meta", Options: allOptions()}

	var seen []string
	res, stages, err := ImportMeta(ctx, db, gs, repo, req, nil, func(st []Stage) {
		for _, s := range st {
			if s.State == "running" && (len(seen) == 0 || seen[len(seen)-1] != s.Key) {
				seen = append(seen, s.Key)
			}
		}
	})
	if err != nil || res.Partial {
		t.Fatalf("ImportMeta: %v %+v", err, res)
	}
	if got := strings.Join(seen, ","); got != "labels,milestones,issues,pulls" {
		t.Errorf("stage order = %s", got)
	}
	for _, s := range stages {
		if s.Key != "code" && s.State != "done" {
			t.Errorf("stage %s = %s", s.Key, s.State)
		}
	}
	if res.Issues != 2 || res.Pulls != 5 || res.Labels != 4 || res.Milestones != 2 || res.Comments != 4 {
		t.Errorf("result = %+v", res)
	}
	if s := res.Summary(); s != "2 issues, 5 pull requests, 4 labels, 2 milestones, 4 comments" {
		t.Errorf("summary = %q", s)
	}

	// Issues keep their numbers, authors, timestamps, labels and milestone.
	is1, err := db.IssueByNumber(ctx, repo.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if is1.AuthorKind != "import" || is1.AuthorName != "github:alice" || !is1.CreatedAt.Equal(t0.Add(24*time.Hour)) ||
		is1.Milestone != "v1.0" || len(is1.Labels) != 1 || is1.Labels[0].Name != "bug" || is1.State != "open" {
		t.Errorf("issue 1 = %+v", is1)
	}
	cs, _ := db.IssueComments(ctx, is1.ID)
	if len(cs) != 3 || cs[0].AuthorName != "github:bob" || cs[2].Body != "Fixed in #2" || cs[0].AuthorKind != "import" {
		t.Errorf("comments = %+v", cs)
	}
	is5, _ := db.IssueByNumber(ctx, repo.ID, 5)
	if is5.State != "closed" || is5.StateReason != "not_planned" || is5.AuthorName != "github:ghost" ||
		len(is5.Labels) != 1 || is5.Labels[0].Color != "123456" {
		t.Errorf("issue 5 = %+v", is5)
	}
	// Pull requests are not issues.
	if _, err := db.IssueByNumber(ctx, repo.ID, 2); err == nil {
		t.Error("pull request #2 imported as an issue")
	}

	// Labels: by name, with source colours; quotes made acceptable; no defaults added.
	if err := db.EnsureDefaultLabels(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	ls, _ := db.Labels(ctx, repo.ID)
	var names []string
	for _, l := range ls {
		names = append(names, l.Name+"="+l.Color)
	}
	if got := strings.Join(names, " "); got != "area/ui=00ff00 bug=d73a4a needs 'triage'=abcdef wontfix=123456" {
		t.Errorf("labels = %s", got)
	}
	ms, _ := db.Milestones(ctx, repo.ID, "")
	if len(ms) != 2 {
		t.Fatalf("milestones = %+v", ms)
	}
	for _, m := range ms {
		if m.Title == "v0.9" && (m.State != "closed" || m.ClosedAt == nil) {
			t.Errorf("v0.9 = %+v", m)
		}
		if m.Title == "v1.0" && (m.State != "open" || m.DueOn == nil || m.Open != 1) {
			t.Errorf("v1.0 = %+v", m)
		}
	}

	// Pull requests: merged, closed, branch gone, open, fork.
	p2, _ := db.PullByNumber(ctx, repo.ID, 2)
	if p2.State != "merged" || p2.MergedSHA != mainSHA || p2.MergedBy != "github:carol" || p2.MergedAt == nil ||
		p2.Head != "feature" || p2.Base != "main" || p2.AuthorName != "github:bob" || p2.HeadSHA != featSHA {
		t.Errorf("pull 2 = %+v", p2)
	}
	if pc, _ := db.PullComments(ctx, p2.ID); len(pc) != 1 || pc[0].AuthorName != "github:carol" {
		t.Errorf("pull 2 comments = %+v", pc)
	}
	p3, _ := db.PullByNumber(ctx, repo.ID, 3)
	if p3.State != "closed" || p3.HeadSHA != featSHA {
		t.Errorf("pull 3 = %+v", p3)
	}
	p6, _ := db.PullByNumber(ctx, repo.ID, 6)
	pc6, _ := db.PullComments(ctx, p6.ID)
	if p6.State != "closed" || p6.HeadSHA != "" || len(pc6) != 1 || !strings.Contains(pc6[0].Body, "no longer exists") ||
		!strings.Contains(pc6[0].Body, "gone") {
		t.Errorf("pull 6 = %+v %+v", p6, pc6)
	}
	p7, _ := db.PullByNumber(ctx, repo.ID, 7)
	if p7.State != "open" || p7.HeadSHA != featSHA || p7.Head != "feature" {
		t.Errorf("pull 7 = %+v", p7)
	}
	p8, _ := db.PullByNumber(ctx, repo.ID, 8)
	pc8, _ := db.PullComments(ctx, p8.ID)
	if p8.State != "closed" || p8.Head != "dave:main" || len(pc8) != 1 {
		t.Errorf("pull 8 (fork) = %+v %+v", p8, pc8)
	}

	// The counter continues after the highest number.
	if n, _ := db.RepoCounter(ctx, repo.ID); n != 9 {
		t.Errorf("counter = %d", n)
	}
	next, err := db.CreateIssue(ctx, repo.ID, "New here", "", "owner", "demi")
	if err != nil || next.Number != 9 {
		t.Errorf("new issue = %+v %v", next, err)
	}

	// Running again duplicates nothing.
	res2, _, err := ImportMeta(ctx, db, gs, repo, req, nil, nil)
	if err != nil || res2.Issues+res2.Pulls+res2.Labels+res2.Milestones+res2.Comments != 0 || res2.Existing == 0 {
		t.Fatalf("second run = %+v %v", res2, err)
	}
	if !strings.HasPrefix(res2.Summary(), "Nothing new") {
		t.Errorf("summary = %q", res2.Summary())
	}
	if cs, _ := db.IssueComments(ctx, is1.ID); len(cs) != 3 {
		t.Errorf("comments after re-run = %d", len(cs))
	}
	if pc6, _ := db.PullComments(ctx, p6.ID); len(pc6) != 1 {
		t.Errorf("notes after re-run = %d", len(pc6))
	}
	if all, _ := db.Pulls(ctx, repo.ID, ""); len(all) != 5 {
		t.Errorf("pulls after re-run = %d", len(all))
	}
	if n, _ := db.RepoCounter(ctx, repo.ID); n != 10 {
		t.Errorf("counter after re-run = %d", n)
	}
}

func TestImportMetaGitHubRateLimit(t *testing.T) {
	ctx := context.Background()
	db, gs, repo, mainSHA, featSHA := importedRepo(t)
	fake := &fakeGitHub{mainSHA: mainSHA, featSHA: featSHA, limitAt: "/repos/o/r/issues/1/comments?page=2"}
	api := httptest.NewServer(fake)
	defer api.Close()
	req := MetaRequest{Source: "github", APIBase: api.URL, Repo: "o/r", Token: "ghp_meta", Options: allOptions()}

	res, stages, err := ImportMeta(ctx, db, gs, repo, req, nil, nil)
	if err != nil || !res.Partial || res.Reset.IsZero() {
		t.Fatalf("expected a partial import: %+v %v", res, err)
	}
	states := map[string]string{}
	for _, s := range stages {
		states[s.Key] = s.State
	}
	if states["labels"] != "done" || states["milestones"] != "done" || states["issues"] != "partial" || states["pulls"] != "pending" {
		t.Errorf("stages = %v", states)
	}
	// It stopped at the limit instead of carrying on.
	fake.mu.Lock()
	last := fake.requests[len(fake.requests)-1]
	n := len(fake.requests)
	fake.mu.Unlock()
	if !strings.HasPrefix(last, "/repos/o/r/issues/1/comments") {
		t.Errorf("kept going after the limit: %s (%d requests)", last, n)
	}
	is1, _ := db.IssueByNumber(ctx, repo.ID, 1)
	if cs, _ := db.IssueComments(ctx, is1.ID); len(cs) != 2 {
		t.Errorf("comments before the limit = %d", len(cs))
	}
	// The counter already covers every listed number.
	if c, _ := db.RepoCounter(ctx, repo.ID); c != 9 {
		t.Errorf("counter = %d", c)
	}

	// Resume once the limit resets: the rest comes in, nothing doubles.
	fake.mu.Lock()
	fake.limitAt = ""
	fake.mu.Unlock()
	res, _, err = ImportMeta(ctx, db, gs, repo, req, nil, nil)
	if err != nil || res.Partial {
		t.Fatalf("resume: %+v %v", res, err)
	}
	if res.Issues != 1 || res.Pulls != 5 || res.Labels != 1 || res.Comments != 2 {
		t.Errorf("resume result = %+v", res)
	}
	if cs, _ := db.IssueComments(ctx, is1.ID); len(cs) != 3 {
		t.Errorf("comments after resume = %d", len(cs))
	}

	// A 200 answer saying no requests are left: no further request is made.
	db2, gs2, repo2, _, _ := importedRepo(t)
	fake2 := &fakeGitHub{mainSHA: mainSHA, featSHA: featSHA, zeroAt: "/repos/o/r/labels?page=1"}
	api2 := httptest.NewServer(fake2)
	defer api2.Close()
	req.APIBase = api2.URL
	res, stages, err = ImportMeta(ctx, db2, gs2, repo2, req, nil, nil)
	if err != nil || !res.Partial || res.Labels != 2 || len(fake2.requests) != 1 {
		t.Errorf("zero remaining: %+v %v, %d requests", res, err, len(fake2.requests))
	}
	if stages[1].Key != "labels" || stages[1].State != "partial" {
		t.Errorf("stages = %+v", stages)
	}
}

func TestImportMetaNumberTaken(t *testing.T) {
	ctx := context.Background()
	db, gs, repo, mainSHA, featSHA := importedRepo(t)
	if _, err := db.CreateIssue(ctx, repo.ID, "Made here first", "", "owner", "demi"); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(&fakeGitHub{mainSHA: mainSHA, featSHA: featSHA})
	defer api.Close()
	res, _, err := ImportMeta(ctx, db, gs, repo, MetaRequest{Source: "github", APIBase: api.URL, Repo: "o/r",
		Token: "ghp_meta", Options: MetaOptions{Issues: true}}, nil, nil)
	if err != nil || res.Issues != 1 || len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "#1") {
		t.Fatalf("result = %+v %v", res, err)
	}
	if is, _ := db.IssueByNumber(ctx, repo.ID, 1); is.Title != "Made here first" {
		t.Errorf("#1 overwritten: %+v", is)
	}
}

// ---------- Gitea ----------

func TestImportMetaGitea(t *testing.T) {
	ctx := context.Background()
	db, gs, repo, mainSHA, featSHA := importedRepo(t)
	var labels []map[string]any
	for i := 0; i < 51; i++ { // a full page of 50 and one more
		labels = append(labels, map[string]any{"name": fmt.Sprintf("l%02d", i), "color": "#aabbcc"})
	}
	var reqs []string
	var mu sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.URL.RequestURI())
		mu.Unlock()
		if r.Header.Get("Authorization") != "token gtok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		page, _ := strconv.Atoi(q.Get("page"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit != 50 && !strings.Contains(r.URL.Path, "/pulls/") {
			http.Error(w, "limit", 400)
			return
		}
		pageOf := func(items []map[string]any) {
			start, end := (page-1)*limit, page*limit
			if start > len(items) {
				start = len(items)
			}
			if end > len(items) {
				end = len(items)
			}
			json.NewEncoder(w).Encode(items[start:end])
		}
		switch r.URL.Path {
		case "/api/v1/repos/o/r/labels":
			pageOf(labels)
		case "/api/v1/repos/o/r/milestones":
			pageOf([]map[string]any{{"id": 1, "title": "Spring", "state": "open", "created_at": ts(0)}})
		case "/api/v1/repos/o/r/issues":
			if q.Get("state") != "all" {
				http.Error(w, "state", 400)
				return
			}
			switch q.Get("type") {
			case "issues":
				pageOf([]map[string]any{
					{"number": 3, "title": "Later", "state": "open", "user": map[string]any{"login": "bob"}, "created_at": ts(3), "updated_at": ts(3)},
					{"number": 1, "title": "First", "state": "closed", "user": map[string]any{"login": "bob"}, "comments": 1,
						"milestone":  map[string]any{"title": "Spring", "state": "open"},
						"labels":     []any{map[string]any{"name": "l00", "color": "aabbcc"}},
						"created_at": ts(1), "updated_at": ts(2), "closed_at": ts(2)},
				})
			case "pulls":
				pageOf([]map[string]any{{"number": 2, "title": "Feature", "state": "closed", "user": map[string]any{"login": "eve"},
					"comments": 1, "created_at": ts(2), "updated_at": ts(3), "closed_at": ts(3),
					"pull_request": map[string]any{"merged": true, "merged_at": ts(3)}}})
			default:
				http.Error(w, "type", 400)
			}
		case "/api/v1/repos/o/r/issues/1/comments", "/api/v1/repos/o/r/issues/2/comments":
			// Older Gitea ignores paging: the same list on every page.
			json.NewEncoder(w).Encode([]map[string]any{{"id": 9, "user": map[string]any{"login": "bob"}, "body": "hi", "created_at": ts(2)}})
		case "/api/v1/repos/o/r/pulls/2":
			json.NewEncoder(w).Encode(map[string]any{"merged": true, "merged_at": ts(3), "merge_commit_sha": mainSHA,
				"merged_by": map[string]any{"login": "eve"},
				"head":      map[string]any{"ref": "feature", "sha": featSHA, "repo": map[string]any{"full_name": "o/r"}},
				"base":      map[string]any{"ref": "main", "sha": mainSHA, "repo": map[string]any{"full_name": "o/r"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	req := MetaRequest{Source: "gitea", APIBase: api.URL, Repo: "o/r", Token: "gtok", Options: allOptions()}
	res, _, err := ImportMeta(ctx, db, gs, repo, req, nil, nil)
	if err != nil || res.Partial {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Labels != 51 || res.Milestones != 1 || res.Issues != 2 || res.Pulls != 1 {
		t.Errorf("result = %+v", res)
	}
	is1, _ := db.IssueByNumber(ctx, repo.ID, 1)
	if is1.AuthorName != "gitea:bob" || is1.State != "closed" || is1.StateReason != "completed" || is1.Milestone != "Spring" ||
		len(is1.Labels) != 1 || is1.Labels[0].Color != "aabbcc" {
		t.Errorf("issue 1 = %+v", is1)
	}
	if cs, _ := db.IssueComments(ctx, is1.ID); len(cs) != 1 {
		t.Errorf("comments = %d (paging ignored by server must not repeat them)", len(cs))
	}
	p2, err := db.PullByNumber(ctx, repo.ID, 2)
	if err != nil || p2.State != "merged" || p2.MergedSHA != mainSHA || p2.MergedBy != "gitea:eve" {
		t.Errorf("pull 2 = %+v %v", p2, err)
	}
	if n, _ := db.RepoCounter(ctx, repo.ID); n != 4 {
		t.Errorf("counter = %d", n)
	}
	mu.Lock()
	joined := strings.Join(reqs, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "/labels?limit=50&page=2") || strings.Contains(joined, "/labels?limit=50&page=3") {
		t.Errorf("label paging:\n%s", joined)
	}
	if strings.Contains(joined, "gtok") {
		t.Error("token in a URL")
	}

	// Again: nothing new.
	res, _, err = ImportMeta(ctx, db, gs, repo, req, nil, nil)
	if err != nil || res.Labels+res.Issues+res.Pulls+res.Comments+res.Milestones != 0 {
		t.Errorf("re-run = %+v %v", res, err)
	}
}

func TestMetaOptionsRoundTrip(t *testing.T) {
	o := MetaOptions{Issues: true, Milestones: true}
	if got := ParseMetaOptions(o.String()); got != o || o.String() != "milestones issues" {
		t.Errorf("%q → %+v", o.String(), got)
	}
	if (MetaOptions{}).Any() {
		t.Error("empty options say Any")
	}
	st := Stages(MetaOptions{Pulls: true, Labels: true})
	if len(st) != 3 || st[0].Key != "code" || st[1].Key != "labels" || st[2].Key != "pulls" {
		t.Errorf("stages = %+v", st)
	}
}
