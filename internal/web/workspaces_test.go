package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"atelier/internal/actions"
	"atelier/internal/runner"
	"atelier/internal/store"
)

// wsFixture: zoe/demo with main and feature pushed, zoe/other, a runner
// registered to zoe with the workspaces capability, and a live server.
type wsFixture struct {
	gitFixture
	srv   *httptest.Server
	ck    map[string]string
	rn    store.ActionsRunner
	rnTok string
	work  string
}

func newWsFixture(t *testing.T) *wsFixture {
	t.Helper()
	f, ck, work := editFixture(t)
	a := f.a
	ctx := context.Background()
	runGit(t, work, "checkout", "-q", "-b", "feature")
	pullsCommit(t, work, "feature.txt", "feature\n", "Feature")
	runGit(t, work, "push", "-q", "origin", "feature")
	me, _ := a.db.PersonalOwner(ctx)
	if err := a.git.Init(ctx, "zoe", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.CreateRepo(ctx, me.ID, "other", "", true); err != nil {
		t.Fatal(err)
	}
	tok := runnerTokenPrefix + store.RandomToken(16)
	rn, err := a.db.CreateActionsRunner(ctx, store.ActionsRunner{Name: "MacBook", OS: "darwin", Arch: "arm64", PersonID: me.ID,
		Capabilities: []string{actions.CapWorkspaces}}, store.Hash(tok))
	if err != nil {
		t.Fatal(err)
	}
	a.ws().pollWait = 300 * time.Millisecond
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return &wsFixture{gitFixture: f, srv: srv, ck: ck, rn: rn, rnTok: tok, work: work}
}

func (w *wsFixture) api(t *testing.T, path string, in, out any) int {
	t.Helper()
	b, _ := json.Marshal(in)
	req, _ := http.NewRequest("POST", w.srv.URL+actions.APIPrefix+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+w.rnTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	} else {
		_, _ = io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode
}

func (w *wsFixture) open(t *testing.T, branch, preset string) int64 {
	t.Helper()
	rec := do(t, w.a, "POST", "/zoe/demo/workspaces", url.Values{"runner": {strconv.FormatInt(w.rn.ID, 10)},
		"branch": {branch}, "preset": {preset}}, w.ck)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.Contains(loc, "request=") {
		t.Fatalf("open = %d %q\n%.2000s", rec.Code, loc, rec.Body)
	}
	id, _ := strconv.ParseInt(loc[strings.LastIndex(loc, "=")+1:], 10, 64)
	return id
}

func (w *wsFixture) status(t *testing.T, id int64) wsRequestView {
	t.Helper()
	rec := do(t, w.a, "GET", "/zoe/demo/workspaces/requests/"+strconv.FormatInt(id, 10), nil, w.ck)
	var v wsRequestView
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	return v
}

func (w *wsFixture) waitDone(t *testing.T, id int64) wsRequestView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if v := w.status(t, id); v.Done {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("request %d never finished: %+v", id, w.status(t, id))
	return wsRequestView{}
}

// The whole flow with the real runner code on the other end.
func TestWorkspaceOpenOnDeviceEndToEnd(t *testing.T) {
	w := newWsFixture(t)
	a := w.a
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := filepath.Join(t.TempDir(), "Atelier")
	marker := filepath.Join(t.TempDir(), "opened")
	wc := &runner.WorkspaceConfig{Root: root, Presets: map[string]string{"mark": "touch " + marker}}
	rr := &runner.Runner{
		Config: &runner.Config{ServerURL: w.srv.URL, RunnerToken: w.rnTok, Name: "MacBook", Capabilities: []string{actions.CapWorkspaces}},
		Client: &runner.Client{BaseURL: w.srv.URL, Token: w.rnTok}, Version: "test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxBackoff: 200 * time.Millisecond,
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = rr.RunWorkspaces(ctx, wc) }()
	defer func() { cancel(); <-done }()

	// The device shows up in the "Open on…" menu once it polls.
	var devs struct {
		Devices []wsDevice `json:"devices"`
		Action  string     `json:"action"`
	}
	for i := 0; i < 200; i++ {
		rec := do(t, a, "GET", "/zoe/demo/workspaces/devices", nil, w.ck)
		_ = json.Unmarshal(rec.Body.Bytes(), &devs)
		if len(devs.Devices) == 1 && devs.Devices[0].Online {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(devs.Devices) != 1 || !devs.Devices[0].Online || devs.Devices[0].Name != "MacBook" ||
		len(devs.Devices[0].Presets) != 1 || devs.Devices[0].Presets[0] != "mark" || devs.Action != "/zoe/demo/workspaces" {
		t.Fatalf("devices = %+v", devs)
	}
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/workspaces?ref=main", nil, w.ck), 200, "open page")
	mustContain(t, body, "open page", "MacBook", "Online", "Open with mark")

	// Clone and open with the device's own preset.
	id := w.open(t, "main", "mark")
	v := w.waitDone(t, id)
	ws := filepath.Join(root, "zoe", "demo")
	realWs, _ := filepath.EvalSymlinks(ws)
	if v.State != actions.WorkspaceOpened || v.Failed || v.Path != realWs {
		t.Fatalf("first open = %+v", v)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the preset didn't run")
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "README.md")); string(b) != "# Demo\n" {
		t.Errorf("README in workspace = %q", b)
	}
	cfg, _ := os.ReadFile(filepath.Join(ws, ".git", "config"))
	if strings.Contains(string(cfg), wsTokenPrefix) || strings.Contains(string(cfg), "extraHeader") || strings.Contains(string(cfg), "Authorization") {
		t.Fatalf("the token reached .git/config:\n%s", cfg)
	}
	if origin := runGit(t, ws, "remote", "get-url", "origin"); origin != w.srv.URL+"/zoe/demo.git" {
		t.Errorf("origin = %q", origin)
	}
	body = expectStatus(t, do(t, a, "GET", "/zoe/demo/workspaces?request="+strconv.FormatInt(id, 10), nil, w.ck), 200, "status page")
	mustContain(t, body, "status page", "Opened", "on MacBook", realWs)

	// Again, for another branch: fetch and check it out.
	id = w.open(t, "feature", "")
	v = w.waitDone(t, id)
	if v.State != actions.WorkspaceUpdated || v.Failed {
		t.Fatalf("second open = %+v", v)
	}
	if b := runGit(t, ws, "symbolic-ref", "--short", "HEAD"); b != "feature" {
		t.Errorf("checked out %q", b)
	}

	// Local work is never overwritten: a dirty workspace stays on its branch.
	os.WriteFile(filepath.Join(ws, "feature.txt"), []byte("local change\n"), 0o644)
	id = w.open(t, "main", "")
	v = w.waitDone(t, id)
	if v.Failed || !v.Dirty || !strings.Contains(v.Message, "uncommitted changes") {
		t.Fatalf("dirty open = %+v", v)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "feature.txt")); string(b) != "local change\n" {
		t.Error("local change lost")
	}

	// An unknown preset is refused by the server…
	rec := do(t, a, "POST", "/zoe/demo/workspaces", url.Values{"runner": {strconv.FormatInt(w.rn.ID, 10)}, "branch": {"main"},
		"preset": {"rm -rf ~"}}, w.ck)
	expectStatus(t, rec, 422, "unknown preset")

	// The device's workspace list reaches Settings → Runners.
	var list []store.Workspace
	for i := 0; i < 100; i++ {
		list, _ = a.db.Workspaces(context.Background(), w.rn.ID)
		if len(list) == 1 && list[0].Dirty {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(list) != 1 || list[0].OwnerName != "zoe" || list[0].RepoName != "demo" || list[0].Branch != "feature" || !list[0].Dirty || list[0].LastOpened.IsZero() {
		t.Fatalf("workspaces = %+v", list)
	}
	body = expectStatus(t, do(t, a, "GET", "/settings/workspaces?runner="+strconv.FormatInt(w.rn.ID, 10), nil, w.ck), 200, "device page")
	mustContain(t, body, "device page", "Workspaces on MacBook", "zoe/demo", realWs, "Changes", "Opened")
	body = expectStatus(t, do(t, a, "GET", "/settings/runners", nil, w.ck), 200, "runners page")
	mustContain(t, body, "runners page", `href="/settings/workspaces?runner=`+strconv.FormatInt(w.rn.ID, 10)+`"`)
}

// The protocol by hand: the token's scope and lifetime, and who may ask.
func TestWorkspaceRequestTokenAndAccess(t *testing.T) {
	w := newWsFixture(t)
	a := w.a
	ctx := context.Background()

	// A poll without the capability is refused.
	if code := w.api(t, "/workspaces/poll", actions.WorkspacePollRequest{Capabilities: []string{"jobs"}}, nil); code != 403 {
		t.Fatalf("poll without capability = %d", code)
	}
	var none actions.WorkspacePollResponse
	if code := w.api(t, "/workspaces/poll", actions.WorkspacePollRequest{Capabilities: []string{actions.CapWorkspaces},
		Presets: []string{"vscode"}, Root: "/Users/zoe/Atelier"}, &none); code != 200 || none.Request != nil {
		t.Fatalf("empty poll = %d %+v", code, none)
	}
	id := w.open(t, "main", "vscode")
	var got actions.WorkspacePollResponse
	w.api(t, "/workspaces/poll", actions.WorkspacePollRequest{Capabilities: []string{actions.CapWorkspaces}, Presets: []string{"vscode"}}, &got)
	req := got.Request
	if req == nil || req.ID != id || req.Branch != "main" || req.Preset != "vscode" || req.Repo.Owner != "zoe" ||
		req.CloneURL != w.srv.URL+"/zoe/demo.git" || !strings.HasPrefix(req.Token, wsTokenPrefix) {
		t.Fatalf("request = %+v", req)
	}
	// It's handed out once.
	var again actions.WorkspacePollResponse
	w.api(t, "/workspaces/poll", actions.WorkspacePollRequest{Capabilities: []string{actions.CapWorkspaces}}, &again)
	if again.Request != nil {
		t.Fatal("request delivered twice")
	}

	// The token reads this repository, and nothing else.
	u, _ := url.Parse(w.srv.URL)
	lsRemote := func(repo string) error {
		cmd := exec.Command("git", "ls-remote", "http://atelier:"+req.Token+"@"+u.Host+"/zoe/"+repo+".git")
		return cmd.Run()
	}
	if err := lsRemote("demo"); err != nil {
		t.Fatalf("token can't read its repository: %v", err)
	}
	if err := lsRemote("other"); err == nil {
		t.Fatal("token reads another repository")
	}
	who, ok := a.wsActor(ctx, req.Token)
	rp, _ := a.db.RepoByName(ctx, "zoe", "demo")
	if !ok || !a.can(ctx, who, rp, PermWrite) || a.can(ctx, who, rp, PermAdmin) {
		t.Fatalf("token access: ok=%v write=%v admin=%v", ok, a.can(ctx, who, rp, PermWrite), a.can(ctx, who, rp, PermAdmin))
	}

	// Another runner can't report on it; the final report revokes the token.
	if code := w.api(t, "/workspaces/"+strconv.FormatInt(id+99, 10)+"/status", actions.WorkspaceStatus{State: "cloned"}, nil); code != 404 {
		t.Errorf("status for an unknown request = %d", code)
	}
	if code := w.api(t, "/workspaces/"+strconv.FormatInt(id, 10)+"/status", actions.WorkspaceStatus{State: actions.WorkspaceOpened,
		Path: "/Users/zoe/Atelier/zoe/demo", Final: true}, nil); code != 200 {
		t.Fatalf("status = %d", code)
	}
	if err := lsRemote("demo"); err == nil {
		t.Fatal("token still works after the final report")
	}
	if v := w.status(t, id); v.State != actions.WorkspaceOpened || !v.Done || v.Device != "MacBook" {
		t.Errorf("status view = %+v", v)
	}

	// A second person can't see or use zoe's device or requests.
	other, _ := a.db.CreateOwner(ctx, "visitor", "", "user")
	sess, _ := a.db.CreateSessionFor(ctx, other.ID, time.Hour)
	vk := map[string]string{"Cookie": sessionCookie + "=" + sess}
	_ = a.db.UpdateRepo(ctx, rp.ID, "", false) // public, so they can read it
	expectStatus(t, do(t, a, "POST", "/zoe/demo/workspaces", url.Values{"runner": {strconv.FormatInt(w.rn.ID, 10)}, "branch": {"main"}}, vk), 403, "visitor open")
	expectStatus(t, do(t, a, "GET", "/zoe/demo/workspaces/requests/"+strconv.FormatInt(id, 10), nil, vk), 404, "visitor status")
	expectStatus(t, do(t, a, "GET", "/settings/workspaces?runner="+strconv.FormatInt(w.rn.ID, 10), nil, vk), 404, "visitor device page")
	rec := do(t, a, "GET", "/zoe/demo/workspaces/devices", nil, vk)
	if strings.Contains(rec.Body.String(), "MacBook") {
		t.Error("visitor sees zoe's device")
	}
}
