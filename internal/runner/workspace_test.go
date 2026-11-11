package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/actions"
)

// wsFake is a fake Atelier for workspaces: git over HTTP (git http-backend)
// that insists on the request token, and the three workspace endpoints.
type wsFake struct {
	t        *testing.T
	srv      *httptest.Server
	token    string
	mu       sync.Mutex
	queue    []*actions.WorkspaceRequest
	statuses map[int64][]actions.WorkspaceStatus
	syncs    []actions.WorkspaceSyncRequest
	polls    []actions.WorkspacePollRequest
	gitAuth  []string
}

func wsGitEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "T")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "T")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func wsRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newWsFake(t *testing.T) *wsFake {
	t.Helper()
	wsGitEnv(t)
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	// zoe/demo.git with main and feature.
	repos := t.TempDir()
	bare := filepath.Join(repos, "zoe", "demo.git")
	wsRun(t, repos, "init", "-q", "--bare", bare)
	work := t.TempDir()
	wsRun(t, work, "init", "-q", "--initial-branch=main", ".")
	os.WriteFile(filepath.Join(work, "README.md"), []byte("# Demo\n"), 0o644)
	wsRun(t, work, "add", ".")
	wsRun(t, work, "commit", "-q", "-m", "First")
	wsRun(t, work, "checkout", "-q", "-b", "feature")
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("f\n"), 0o644)
	wsRun(t, work, "add", ".")
	wsRun(t, work, "commit", "-q", "-m", "Feature")
	wsRun(t, work, "push", "-q", bare, "main", "feature")
	wsRun(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")

	f := &wsFake{t: t, token: "atw_secret_request_token", statuses: map[int64][]actions.WorkspaceStatus{}}
	backend := &cgi.Handler{Path: gitBin, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + repos, "GIT_HTTP_EXPORT_ALL=1"}}
	mux := http.NewServeMux()
	p := actions.APIPrefix
	mux.HandleFunc("/zoe/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.gitAuth = append(f.gitAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("atelier:"+f.token))
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	})
	mux.HandleFunc("POST "+p+"/workspaces/poll", func(w http.ResponseWriter, r *http.Request) {
		var in actions.WorkspacePollRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.polls = append(f.polls, in)
		var out actions.WorkspacePollResponse
		if len(f.queue) > 0 {
			out.Request, f.queue = f.queue[0], f.queue[1:]
		}
		f.mu.Unlock()
		if out.Request == nil {
			time.Sleep(20 * time.Millisecond)
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("POST "+p+"/workspaces/{id}/status", func(w http.ResponseWriter, r *http.Request) {
		var in actions.WorkspaceStatus
		_ = json.NewDecoder(r.Body).Decode(&in)
		var id int64
		_, _ = io.WriteString(io.Discard, "")
		for _, c := range r.PathValue("id") {
			id = id*10 + int64(c-'0')
		}
		f.mu.Lock()
		f.statuses[id] = append(f.statuses[id], in)
		f.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST "+p+"/workspaces/sync", func(w http.ResponseWriter, r *http.Request) {
		var in actions.WorkspaceSyncRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.syncs = append(f.syncs, in)
		f.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *wsFake) runner() *Runner {
	return &Runner{
		Config: &Config{ServerURL: f.srv.URL, RunnerToken: "rnr", Capabilities: []string{actions.CapWorkspaces}},
		Client: &Client{BaseURL: f.srv.URL, Token: "rnr"}, Version: "test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxBackoff: 100 * time.Millisecond,
	}
}

func (f *wsFake) request(id int64, branch, preset string) *actions.WorkspaceRequest {
	return &actions.WorkspaceRequest{ID: id, Repo: actions.RepoRef{Owner: "zoe", Name: "demo"}, Branch: branch,
		CloneURL: f.srv.URL + "/zoe/demo.git", SSHURL: "ssh://git@example.test:2222/zoe/demo.git", Token: f.token, Preset: preset}
}

func (f *wsFake) final(id int64) actions.WorkspaceStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.statuses[id]
	if len(st) == 0 || !st[len(st)-1].Final {
		f.t.Fatalf("request %d has no final status: %+v", id, st)
	}
	return st[len(st)-1]
}

func TestWorkspaceCloneUpdateAndOpen(t *testing.T) {
	f := newWsFake(t)
	r := f.runner()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "Atelier")
	marker := filepath.Join(t.TempDir(), "opened")
	wc := &WorkspaceConfig{Root: root, Presets: map[string]string{"mark": "touch " + marker}}

	r.handleWorkspace(ctx, wc, root, f.request(1, "main", "mark"))
	st := f.final(1)
	ws := filepath.Join(root, "zoe", "demo")
	real, _ := filepath.EvalSymlinks(ws)
	if st.State != actions.WorkspaceOpened || st.Path != real {
		t.Fatalf("status = %+v", st)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("preset didn't run")
	}
	// The token was used (only) as a header, never stored.
	cfg, _ := os.ReadFile(filepath.Join(ws, ".git", "config"))
	for _, s := range []string{f.token, "extraHeader", "Authorization", base64.StdEncoding.EncodeToString([]byte("atelier:" + f.token))} {
		if strings.Contains(string(cfg), s) {
			t.Fatalf(".git/config contains %q:\n%s", s, cfg)
		}
	}
	if origin := wsRun(t, ws, "remote", "get-url", "origin"); origin != f.srv.URL+"/zoe/demo.git" {
		t.Errorf("origin = %q", origin)
	}
	if b, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".git-credentials")); len(b) > 0 {
		t.Error("a credential file was written")
	}

	// Again for another branch: fetch and check out, origin untouched.
	r.handleWorkspace(ctx, wc, root, f.request(2, "feature", ""))
	if st := f.final(2); st.State != actions.WorkspaceUpdated || st.Branch != "feature" {
		t.Fatalf("update = %+v", st)
	}
	if b := wsRun(t, ws, "symbolic-ref", "--short", "HEAD"); b != "feature" {
		t.Errorf("HEAD = %s", b)
	}

	// The list reports branch, dirty state and when Atelier last opened it.
	os.WriteFile(filepath.Join(ws, "f.txt"), []byte("mine\n"), 0o644)
	list := ListWorkspaces(ctx, root)
	if len(list) != 1 || list[0].Owner != "zoe" || list[0].Repo != "demo" || list[0].Branch != "feature" || !list[0].Dirty || list[0].LastOpened.IsZero() {
		t.Fatalf("list = %+v", list)
	}
	// Dirty: stays on its branch.
	r.handleWorkspace(ctx, wc, root, f.request(3, "main", ""))
	if st := f.final(3); st.State != actions.WorkspaceUpdated || st.Branch != "feature" || !st.Dirty {
		t.Fatalf("dirty update = %+v", st)
	}
}

func TestWorkspaceSSHRemote(t *testing.T) {
	f := newWsFake(t)
	root := t.TempDir()
	f.runner().handleWorkspace(context.Background(), &WorkspaceConfig{Root: root, Remote: "ssh"}, root, f.request(1, "main", ""))
	if st := f.final(1); st.State != actions.WorkspaceCloned {
		t.Fatalf("status = %+v", st)
	}
	if origin := wsRun(t, filepath.Join(root, "zoe", "demo"), "remote", "get-url", "origin"); origin != "ssh://git@example.test:2222/zoe/demo.git" {
		t.Errorf("origin = %q", origin)
	}
}

func TestWorkspaceOnlyPresetCommands(t *testing.T) {
	f := newWsFake(t)
	r := f.runner()
	root := filepath.Join(t.TempDir(), "Atelier")
	wc := &WorkspaceConfig{Root: root, Presets: map[string]string{"vscode": "code {path}"}}
	// A preset this device doesn't have: refused before anything happens.
	for i, preset := range []string{"rm -rf ~", "touch /tmp/pwned", "VSCODE", "vscode2"} {
		id := int64(10 + i)
		r.handleWorkspace(context.Background(), wc, root, f.request(id, "main", preset))
		if st := f.final(id); st.State != actions.WorkspaceFailed || !strings.Contains(st.Message, "no open preset") {
			t.Errorf("preset %q: %+v", preset, st)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "zoe")); !os.IsNotExist(err) {
		t.Error("something was cloned for an unknown preset")
	}

	// Quoting and {path}, with no shell involved.
	for _, c := range []struct {
		cmd  string
		want []string
	}{
		{"code {path}", []string{"code", "/w s"}},
		{"open -a Fork", []string{"open", "-a", "Fork", "/w s"}},
		{`open -a "Sublime Text" {path}`, []string{"open", "-a", "Sublime Text", "/w s"}},
		{"idea --folder={path} ; rm -rf ~", []string{"idea", "--folder=/w s", ";", "rm", "-rf", "~"}},
	} {
		got, err := presetArgv(c.cmd, "/w s")
		if err != nil || strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("presetArgv(%q) = %q, %v", c.cmd, got, err)
		}
	}
	for _, bad := range []string{"", "  ", `code "unclosed`, "{path} --evil"} {
		if _, err := presetArgv(bad, "/w"); err == nil {
			t.Errorf("presetArgv(%q) accepted", bad)
		}
	}
}

func TestWorkspacePathContainment(t *testing.T) {
	wsGitEnv(t)
	root := t.TempDir()
	for _, c := range [][2]string{{"..", "x"}, {"zoe", ".."}, {"zoe", "../../etc"}, {"a/b", "c"}, {"zoe", ""}, {".", "x"},
		{"zoe", "x\\..\\y"}, {"-zoe", "x"}, {"zoe", "demo..git"}} {
		if p, err := workspacePath(root, c[0], c[1]); err == nil {
			t.Errorf("workspacePath(%q, %q) = %s", c[0], c[1], p)
		}
	}
	// A symlinked owner folder pointing out of the root is refused.
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspacePath(root, "evil", "demo"); err == nil {
		t.Error("followed a symlink out of the root")
	}
	p, err := workspacePath(root, "zoe", "demo.v2")
	real, _ := filepath.EvalSymlinks(root)
	if err != nil || p != filepath.Join(real, "zoe", "demo.v2") {
		t.Errorf("workspacePath = %q, %v", p, err)
	}

	// The server can only point at its own host, without credentials.
	f := newWsFake(t)
	r := f.runner()
	for i, u := range []string{"https://evil.example/zoe/demo.git", strings.Replace(f.srv.URL, "http://", "http://u:p@", 1) + "/zoe/demo.git",
		"file:///etc", "ext::sh -c touch% /tmp/pwned"} {
		req := f.request(int64(20+i), "main", "")
		req.CloneURL = u
		r.handleWorkspace(context.Background(), &WorkspaceConfig{Root: root}, root, req)
		if st := f.final(int64(20 + i)); st.State != actions.WorkspaceFailed {
			t.Errorf("clone URL %q accepted: %+v", u, st)
		}
	}
	// Odd branch names are refused.
	for i, b := range []string{"--upload-pack=touch /tmp/x", "a..b", ""} {
		r.handleWorkspace(context.Background(), &WorkspaceConfig{Root: root}, root, f.request(int64(30+i), b, ""))
		if st := f.final(int64(30 + i)); st.State != actions.WorkspaceFailed {
			t.Errorf("branch %q accepted: %+v", b, st)
		}
	}
	// A folder holding another repository is left alone.
	other := filepath.Join(root, "zoe", "demo")
	os.MkdirAll(other, 0o755)
	wsRun(t, other, "init", "-q", ".")
	wsRun(t, other, "remote", "add", "origin", "https://github.com/someone/else.git")
	r.handleWorkspace(context.Background(), &WorkspaceConfig{Root: root}, root, f.request(40, "main", ""))
	if st := f.final(40); st.State != actions.WorkspaceFailed || !strings.Contains(st.Message, "different repository") {
		t.Errorf("foreign repo: %+v", st)
	}
}

func TestWorkspaceLoopPollsAndSyncs(t *testing.T) {
	f := newWsFake(t)
	r := f.runner()
	root := filepath.Join(t.TempDir(), "Atelier")
	f.queue = append(f.queue, f.request(7, "main", ""))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.RunWorkspaces(ctx, &WorkspaceConfig{Root: root, Presets: map[string]string{"vscode": "code {path}", "finder": "open {path}"}})
	}()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.syncs)
		f.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.polls) == 0 || strings.Join(f.polls[0].Presets, ",") != "finder,vscode" || !strings.HasSuffix(f.polls[0].Root, "Atelier") ||
		f.polls[0].Capabilities[0] != actions.CapWorkspaces {
		t.Fatalf("polls = %+v", f.polls)
	}
	if st := f.statuses[7]; len(st) == 0 || st[len(st)-1].State != actions.WorkspaceCloned {
		t.Fatalf("statuses = %+v", st)
	}
	last := f.syncs[len(f.syncs)-1]
	if len(last.Workspaces) != 1 || last.Workspaces[0].Repo != "demo" || last.Workspaces[0].Branch != "main" {
		t.Fatalf("sync = %+v", last)
	}
	for _, h := range f.gitAuth {
		if h == "" {
			continue
		}
		if !strings.HasPrefix(h, "Basic ") {
			t.Errorf("git auth header %q", h)
		}
	}
}

func TestWorkspaceConfigFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "workspaces.json")
	if c, err := LoadWorkspaceConfig(p); err != nil || c.Root != "" || len(c.Presets) != 0 {
		t.Fatalf("missing file = %+v, %v", c, err)
	}
	c := &WorkspaceConfig{Root: "~/Code", Presets: map[string]string{"vscode": "code {path}"}, Remote: "ssh"}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	got, err := LoadWorkspaceConfig(p)
	if err != nil || got.Presets["vscode"] != "code {path}" || got.Remote != "ssh" {
		t.Fatalf("load = %+v, %v", got, err)
	}
	home, _ := os.UserHomeDir()
	if d, _ := got.RootDir(); d != filepath.Join(home, "Code") {
		t.Errorf("RootDir = %q", d)
	}
	for _, bad := range []string{`{"presets":{"Bad Name":"x"}}`, `{"presets":{"x":"\"open"}}`, `{"remote":"ftp"}`} {
		os.WriteFile(p, []byte(bad), 0o600)
		if _, err := LoadWorkspaceConfig(p); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
