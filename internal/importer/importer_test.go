package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

func isolateGit(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// sourceRepo makes a bare repo with main, a feature branch, a tag and a
// GitHub-style pull ref. It returns the bare path and a working clone.
func sourceRepo(t *testing.T) (bare, work string) {
	t.Helper()
	root := t.TempDir()
	bare = filepath.Join(root, "src.git")
	work = filepath.Join(root, "work")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	git(t, root, "init", "-q", "-b", "main", work)
	commit(t, work, "README.md", "hello")
	git(t, work, "tag", "v1")
	git(t, work, "checkout", "-q", "-b", "feature")
	commit(t, work, "f.txt", "feature")
	git(t, work, "checkout", "-q", "main")
	git(t, work, "push", "-q", bare, "main", "feature", "v1", "feature:refs/pull/1/head")
	return bare, work
}

func commit(t *testing.T, work, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "change "+file)
	return git(t, work, "rev-parse", "HEAD")
}

func newStores(t *testing.T) (*store.Store, *gitrepo.Store, store.Owner) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	gs, err := gitrepo.New(filepath.Join(dir, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	me, err := db.CreateOwner(context.Background(), "demi", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	return db, gs, me
}

func TestImportLocalGitURL(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	db, gs, _ := newStores(t)
	bare, work := sourceRepo(t)

	u, creds, err := CleanURL(bare)
	if err != nil || u != bare || !creds.Empty() {
		t.Fatalf("CleanURL = %q %+v %v", u, creds, err)
	}
	repo, err := Import(ctx, db, gs, Request{Source: "git", URL: u, Owner: "demi", Name: "copy", Description: "  a copy ", Private: true})
	if err != nil {
		t.Fatal(err)
	}
	if repo.FullName() != "demi/copy" || repo.Description != "a copy" || !repo.Private {
		t.Fatalf("repo = %+v", repo)
	}
	p := gs.Path("demi", "copy")
	if got, want := git(t, p, "rev-parse", "main"), git(t, work, "rev-parse", "main"); got != want {
		t.Errorf("main = %s, want %s", got, want)
	}
	if git(t, p, "rev-parse", "v1") == "" || git(t, p, "rev-parse", "feature") == "" {
		t.Error("tag or branch missing")
	}
	refs := git(t, p, "for-each-ref", "--format=%(refname)")
	if strings.Contains(refs, "refs/pull/") {
		t.Errorf("pull refs kept: %s", refs)
	}
	if git(t, p, "config", "http.receivepack") != "true" {
		t.Error("receivepack not enabled")
	}
	if out, _ := exec.Command("git", "-C", p, "config", "--get-regexp", "^remote\\.").CombinedOutput(); len(out) > 0 {
		t.Errorf("one-off import kept a remote: %s", out)
	}
	if ms, _ := db.ImportMirrors(ctx); len(ms) != 0 {
		t.Errorf("one-off import created a mirror: %+v", ms)
	}
	if _, err := os.Stat(p + ".importing"); !os.IsNotExist(err) {
		t.Error("temporary clone left behind")
	}

	// Same name again is refused; a bad source leaves nothing behind.
	if _, err := Import(ctx, db, gs, Request{URL: u, Owner: "demi", Name: "copy"}); err == nil {
		t.Error("duplicate import allowed")
	}
	if _, err := Import(ctx, db, gs, Request{URL: filepath.Join(t.TempDir(), "nope.git"), Owner: "demi", Name: "broken"}); err == nil {
		t.Error("import from a missing path succeeded")
	}
	if _, err := os.Stat(gs.Path("demi", "broken")); !os.IsNotExist(err) {
		t.Error("failed import left a repo on disk")
	}
	if _, err := db.RepoByName(ctx, "demi", "broken"); err == nil {
		t.Error("failed import left a row")
	}
	if _, err := Import(ctx, db, gs, Request{URL: u, Owner: "nobody", Name: "x"}); err == nil {
		t.Error("import to a missing owner allowed")
	}
}

func TestMirrorSync(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	db, gs, _ := newStores(t)
	bare, work := sourceRepo(t)

	repo, err := Import(ctx, db, gs, Request{Source: "git", URL: bare, Owner: "demi", Name: "mirror", Mirror: true, MirrorInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := db.ImportMirrors(ctx)
	if err != nil || len(ms) != 1 || ms[0].RepoID != repo.ID || ms[0].Interval != time.Hour || ms[0].LastSyncAt == nil {
		t.Fatalf("mirrors = %+v, %v", ms, err)
	}
	if Due(ms[0], time.Now()) || !Due(ms[0], time.Now().Add(2*time.Hour)) {
		t.Error("Due wrong")
	}

	// Upstream moves on: new commit on main, feature deleted, a new pull ref.
	newMain := commit(t, work, "README.md", "hello again")
	git(t, work, "push", "-q", bare, "main", ":feature", "main:refs/pull/2/head")
	if err := SyncMirror(ctx, db, gs, ms[0]); err != nil {
		t.Fatal(err)
	}
	p := gs.Path("demi", "mirror")
	if got := git(t, p, "rev-parse", "main"); got != newMain {
		t.Errorf("main after sync = %s, want %s", got, newMain)
	}
	refs := git(t, p, "for-each-ref", "--format=%(refname)")
	if strings.Contains(refs, "refs/heads/feature") || strings.Contains(refs, "refs/pull/") {
		t.Errorf("refs after sync: %s", refs)
	}
	ms, _ = db.ImportMirrors(ctx)
	if ms[0].LastError != "" {
		t.Errorf("last error = %q", ms[0].LastError)
	}

	// A broken source is recorded, not fatal.
	os.RemoveAll(bare)
	if err := SyncMirror(ctx, db, gs, ms[0]); err == nil {
		t.Error("sync from a deleted source succeeded")
	}
	ms, _ = db.ImportMirrors(ctx)
	if ms[0].LastError == "" {
		t.Error("sync error not recorded")
	}
}

// TestCredentialsOverHTTP serves a repo with git http-backend behind basic
// auth and checks the token reaches git only through the environment.
func TestCredentialsOverHTTP(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	db, gs, _ := newStores(t)
	bare, _ := sourceRepo(t)
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	const token = "s3cr3t-token-value"
	backend := &cgi.Handler{
		Path: gitBin, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bare), "GIT_HTTP_EXPORT_ALL=1"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "demi" || p != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	defer srv.Close()

	// Credentials pasted into the URL are lifted out of it.
	cleanU, creds, err := CleanURL(strings.Replace(srv.URL, "http://", "http://demi:"+token+"@", 1) + "/src.git")
	if err != nil || strings.Contains(cleanU, token) || creds.Token != token || creds.Username != "demi" {
		t.Fatalf("CleanURL = %q %+v %v", cleanU, creds, err)
	}
	if _, err := Import(ctx, db, gs, Request{URL: cleanU, Creds: creds, Owner: "demi", Name: "private", Mirror: true}); err != nil {
		t.Fatal(err)
	}
	p := gs.Path("demi", "private")
	cfg, _ := os.ReadFile(filepath.Join(p, "config"))
	if strings.Contains(string(cfg), token) {
		t.Error("token written to the repo config")
	}
	ms, _ := db.ImportMirrors(ctx)
	if len(ms) != 1 || ms[0].Secret != token || !ms[0].HasSecret() {
		t.Fatalf("mirror creds not stored: %+v", ms)
	}
	if err := SyncMirror(ctx, db, gs, ms[0]); err != nil {
		t.Fatalf("sync with stored creds: %v", err)
	}

	// Wrong token: fails, and the error never contains the token.
	wrong := Creds{Username: "demi", Token: "wrong-" + token}
	_, err = Import(ctx, db, gs, Request{URL: cleanU, Creds: wrong, Owner: "demi", Name: "denied"})
	if err == nil {
		t.Fatal("import with a wrong token succeeded")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestCleanURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://example.com/a/b.git":    true,
		"http://example.com/a/b":         true,
		"ssh://git@example.com/a/b.git":  true,
		"git://example.com/a/b.git":      true,
		"file:///srv/git/a.git":          true,
		"/srv/git/a.git":                 true,
		"ext::sh -c touch% /tmp/pwned":   false,
		"-uhttps://x":                    false,
		"ftp://example.com/a.git":        false,
		"example.com/a.git":              false,
		"ssh://u:pw@example.com/a/b.git": false,
		"https:///nohost":                false,
		"https://tok@github.com/a/b.git": true,
		"https://u:p@github.com/a/b.git": true,
		"":                               false,
	} {
		_, _, err := CleanURL(raw)
		if (err == nil) != ok {
			t.Errorf("CleanURL(%q) err = %v", raw, err)
		}
	}
	for in, want := range map[string]string{
		"https://github.com/me/proj.git": "proj",
		"git@github.com:me/proj.git":     "proj",
		"/srv/x/thing/":                  "thing",
	} {
		if got := NameFromURL(in); got != want {
			t.Errorf("NameFromURL(%q) = %q", in, got)
		}
	}
	env := strings.Join(gitEnv(Creds{Username: "u", Token: "tok"}), "\n")
	if !strings.Contains(env, "GIT_CONFIG_KEY_1=http.extraHeader") || !strings.Contains(env, "Authorization: Basic ") {
		t.Errorf("env missing extraHeader:\n%s", env)
	}
	if got := scrub("fatal: tok and dTp0b2s=", Creds{Username: "u", Token: "tok"}); strings.Contains(got, "tok") || strings.Contains(got, "dTp0b2s=") {
		t.Errorf("scrub = %q", got)
	}
}

func repoJSON(owner, name string, private bool) map[string]any {
	return map[string]any{
		"full_name": owner + "/" + name, "name": name, "owner": map[string]any{"login": owner, "username": owner},
		"description": "about " + name, "private": private, "clone_url": "https://src/" + owner + "/" + name + ".git",
		"default_branch": "main",
	}
}

func TestListGitea(t *testing.T) {
	const token = "gitea-token"
	var userRepos []map[string]any
	for i := 0; i < 53; i++ { // forces a second page at limit=50
		userRepos = append(userRepos, repoJSON("demi", fmt.Sprintf("r%02d", i), i%2 == 0))
	}
	orgRepos := []map[string]any{repoJSON("elyndra", "site", false), repoJSON("demi", "r00", true)} // r00 duplicates
	page := func(all []map[string]any, r *http.Request) []map[string]any {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if limit == 0 || p == 0 {
			t.Errorf("missing pagination on %s", r.URL)
			return nil
		}
		start := (p - 1) * limit
		if start >= len(all) {
			return []map[string]any{}
		}
		return all[start:min(len(all), start+limit)]
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token "+token {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var v any
		switch r.URL.Path {
		case "/api/v1/user":
			v = map[string]any{"login": "demi"}
		case "/api/v1/user/repos":
			v = page(userRepos, r)
		case "/api/v1/user/orgs":
			v = page([]map[string]any{{"username": "elyndra"}}, r)
		case "/api/v1/orgs/elyndra/repos":
			v = page(orgRepos, r)
		default:
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(v)
	}))
	defer srv.Close()

	repos, login, err := ListGitea(context.Background(), srv.Client(), srv.URL+"/", token)
	if err != nil {
		t.Fatal(err)
	}
	if login != "demi" || len(repos) != 54 {
		t.Fatalf("login %q, %d repos", login, len(repos))
	}
	var site *Remote
	for i := range repos {
		if repos[i].FullName == "elyndra/site" {
			site = &repos[i]
		}
	}
	if site == nil || site.Private || site.Owner != "elyndra" || site.Description != "about site" || site.CloneURL == "" {
		t.Errorf("site = %+v", site)
	}
	if !repos[0].Private || repos[0].FullName != "demi/r00" {
		t.Errorf("first = %+v", repos[0])
	}
	if _, _, err := ListGitea(context.Background(), srv.Client(), srv.URL, "bad"); err != ErrUnauthorized {
		t.Errorf("bad token err = %v", err)
	}
	if _, _, err := ListGitea(context.Background(), nil, "ftp://x", token); err == nil {
		t.Error("ftp server accepted")
	}
}

func TestListGitHub(t *testing.T) {
	const token = "ghp_test"
	var all []map[string]any
	for i := 0; i < 230; i++ {
		owner := "demi"
		if i%10 == 0 {
			owner = "elyndra"
		}
		all = append(all, repoJSON(owner, fmt.Sprintf("p%03d", i), i%3 == 0))
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/user":
			json.NewEncoder(w).Encode(map[string]any{"login": "demi"})
		case "/user/repos":
			if r.URL.Query().Get("page") == "" && !strings.Contains(r.URL.Query().Get("affiliation"), "organization_member") {
				t.Errorf("affiliation = %q", r.URL.Query().Get("affiliation"))
			}
			per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
			p, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if p == 0 {
				p = 1
			}
			start := (p - 1) * per
			end := min(len(all), start+per)
			if end < len(all) {
				w.Header().Set("Link", fmt.Sprintf(`<%s/user/repos?per_page=%d&page=%d>; rel="next", <%s/user/repos?page=9>; rel="last"`, srv.URL, per, p+1, srv.URL))
			}
			json.NewEncoder(w).Encode(all[start:end])
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	repos, login, err := ListGitHub(context.Background(), srv.Client(), srv.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	if login != "demi" || len(repos) != 230 {
		t.Fatalf("login %q, %d repos", login, len(repos))
	}
	orgs := 0
	for _, r := range repos {
		if r.Owner == "elyndra" {
			orgs++
		}
	}
	if orgs != 23 {
		t.Errorf("org repos = %d", orgs)
	}
	if _, _, err := ListGitHub(context.Background(), srv.Client(), srv.URL, "nope"); err != ErrUnauthorized {
		t.Errorf("bad token err = %v", err)
	}
}

func TestGitHubNextLinkStaysOnHost(t *testing.T) {
	var hits int
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			json.NewEncoder(w).Encode(map[string]any{"login": "demi"})
			return
		}
		w.Header().Set("Link", `<`+evil.URL+`/user/repos?page=2>; rel="next"`)
		json.NewEncoder(w).Encode([]map[string]any{repoJSON("demi", "a", false)})
	}))
	defer srv.Close()
	repos, _, err := ListGitHub(context.Background(), srv.Client(), srv.URL, "t")
	if err != nil || len(repos) != 1 || hits != 0 {
		t.Errorf("repos %d, err %v, evil hits %d", len(repos), err, hits)
	}
}
