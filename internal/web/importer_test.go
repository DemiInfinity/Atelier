package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func importSourceRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "src.git")
	work := filepath.Join(root, "w")
	for _, args := range [][]string{
		{"init", "-q", "--bare", "-b", "main", bare},
		{"init", "-q", "-b", "main", work},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "a"}, {"push", "-q", bare, "main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return bare
}

func waitJobs(t *testing.T, a *App, ck map[string]string, n int) []jobJSON {
	t.Helper()
	for i := 0; i < 200; i++ {
		var jobs []jobJSON
		rec := do(t, a, "GET", "/settings/import/jobs", nil, ck)
		if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
			t.Fatalf("jobs JSON: %v\n%s", err, rec.Body)
		}
		finished := 0
		for _, j := range jobs {
			if j.Status == "done" || j.Status == "failed" {
				finished++
			}
		}
		if len(jobs) >= n && finished == len(jobs) {
			return jobs
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("imports never finished")
	return nil
}

func TestImportGitURLPage(t *testing.T) {
	a := newTestApp(t)
	ck := featureSession(t, a)
	ctx := context.Background()
	bare := importSourceRepo(t)

	for _, src := range []string{"", "?source=github", "?source=git"} {
		rec := do(t, a, "GET", "/settings/import"+src, nil, ck)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Coming soon") {
			t.Fatalf("GET import%s = %d", src, rec.Code)
		}
	}

	rec := do(t, a, "POST", "/settings/import/url", url.Values{
		"url": {"ftp://nope/x.git"}, "owner": {"demi"},
	}, ck)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad URL = %d", rec.Code)
	}

	rec = do(t, a, "POST", "/settings/import/url", url.Values{
		"url": {bare}, "owner": {"demi"}, "name": {"imported"}, "token": {"tok-SECRET"},
		"visibility": {"public"}, "mirror": {"on"}, "description": {"from disk"},
	}, ck)
	if rec.Code != 303 {
		t.Fatalf("POST url = %d\n%s", rec.Code, rec.Body)
	}
	jobs := waitJobs(t, a, ck, 1)
	if jobs[0].Status != "done" || jobs[0].URL != "/demi/imported" {
		t.Fatalf("job = %+v", jobs[0])
	}
	r, err := a.db.RepoByName(ctx, "demi", "imported")
	if err != nil || r.Private || r.Description != "from disk" {
		t.Fatalf("repo = %+v, %v", r, err)
	}
	page := do(t, a, "GET", "/settings/import", nil, ck).Body.String()
	if !strings.Contains(page, "demi/imported") || !strings.Contains(page, "Stop mirroring") {
		t.Error("page doesn't show the mirror")
	}
	if strings.Contains(page, "tok-SECRET") {
		t.Fatal("token rendered on the page")
	}

	// Same name again fails before queueing.
	rec = do(t, a, "POST", "/settings/import/url", url.Values{"url": {bare}, "owner": {"demi"}, "name": {"imported"}}, ck)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("duplicate = %d", rec.Code)
	}

	// Stop mirroring forgets the token but keeps the repo.
	rec = do(t, a, "POST", fmt.Sprintf("/settings/import/mirrors/%d/stop", r.ID), url.Values{}, ck)
	if rec.Code != 303 {
		t.Fatalf("stop = %d", rec.Code)
	}
	if ms, _ := a.db.ImportMirrors(ctx); len(ms) != 0 {
		t.Errorf("mirror kept: %+v", ms)
	}
	if _, err := a.db.RepoByName(ctx, "demi", "imported"); err != nil {
		t.Error("repo deleted with the mirror")
	}
}

func TestImportGitHubPick(t *testing.T) {
	a := newTestApp(t)
	ck := featureSession(t, a)
	ctx := context.Background()
	bare := importSourceRepo(t)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_ok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/user":
			json.NewEncoder(w).Encode(map[string]any{"login": "demi-gh"})
		case "/user/repos":
			json.NewEncoder(w).Encode([]map[string]any{
				{"full_name": "demi-gh/one", "name": "one", "owner": map[string]any{"login": "demi-gh"}, "private": true, "description": "first", "clone_url": bare},
				{"full_name": "org/two", "name": "two", "owner": map[string]any{"login": "org"}, "clone_url": bare},
			})
		}
	}))
	defer api.Close()
	old := githubAPI
	githubAPI = api.URL
	defer func() { githubAPI = old }()

	rec := do(t, a, "POST", "/settings/import/list", url.Values{"source": {"github"}, "token": {"bad"}}, ck)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "rejected") {
		t.Fatalf("bad token = %d", rec.Code)
	}
	rec = do(t, a, "POST", "/settings/import/list", url.Values{"source": {"github"}, "token": {"ghp_ok"}}, ck)
	loc := rec.Header().Get("Location")
	if rec.Code != 303 || !strings.HasPrefix(loc, "/settings/import/pick/") {
		t.Fatalf("list = %d %q", rec.Code, loc)
	}
	pick := do(t, a, "GET", loc, nil, ck)
	if pick.Code != 200 || !strings.Contains(pick.Body.String(), "org/two") || strings.Contains(pick.Body.String(), "ghp_ok") {
		t.Fatalf("pick page = %d", pick.Code)
	}

	// Nothing selected → error; then import both, renaming the second.
	if rec := do(t, a, "POST", loc, url.Values{"owner": {"demi"}}, ck); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("empty selection = %d", rec.Code)
	}
	rec = do(t, a, "POST", loc, url.Values{"owner": {"demi"}, "repo": {"0", "1"}, "name_1": {"two-renamed"}}, ck)
	if rec.Code != 303 {
		t.Fatalf("pick submit = %d\n%s", rec.Code, rec.Body)
	}
	if rec := do(t, a, "GET", loc, nil, ck); rec.Header().Get("Location") != "/settings/import" {
		t.Error("session (and its token) kept after submit")
	}
	jobs := waitJobs(t, a, ck, 2)
	for _, j := range jobs {
		if j.Status != "done" {
			t.Errorf("job %+v", j)
		}
	}
	one, err := a.db.RepoByName(ctx, "demi", "one")
	if err != nil || !one.Private || one.Description != "first" {
		t.Errorf("one = %+v, %v", one, err)
	}
	if two, err := a.db.RepoByName(ctx, "demi", "two-renamed"); err != nil || two.Private {
		t.Errorf("two = %+v, %v", two, err)
	}
	if ms, _ := a.db.ImportMirrors(ctx); len(ms) != 0 {
		t.Error("one-off imports stored credentials")
	}
}
