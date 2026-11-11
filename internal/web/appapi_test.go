package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
	"atelier/internal/tether"
)

// ---------- helpers ----------

// appReq sends a JSON request to h with a bearer token and decodes a JSON object reply.
func appReq(t *testing.T, h http.Handler, method, target, token string, body any, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
		if k == "Host" {
			req.Host = v
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: reply isn't a JSON object: %v\n%.500s", method, target, err, rec.Body)
		}
	}
	return rec, out
}

// api calls the app API and requires a status.
func api(t *testing.T, a *App, method, path, token string, body any, want int) map[string]any {
	t.Helper()
	rec, out := appReq(t, a, method, appAPIPrefix+path, token, body, nil)
	if rec.Code != want {
		t.Fatalf("%s %s = %d, want %d\n%.1500s", method, path, rec.Code, want, rec.Body)
	}
	return out
}

// wantKeys checks a JSON object has every key the contract names.
func wantKeys(t *testing.T, what string, m map[string]any, keys ...string) {
	t.Helper()
	if m == nil {
		t.Fatalf("%s: no object", what)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("%s: missing %q in %v", what, k, m)
		}
	}
}

func wantErr(t *testing.T, what string, m map[string]any, code string) {
	t.Helper()
	if m["error"] != code || m["message"] == nil || m["message"] == "" {
		t.Errorf("%s: error = %v (message %v), want %q with a message", what, m["error"], m["message"], code)
	}
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func list(v any) []any         { l, _ := v.([]any); return l }

// find returns the first object in a list whose key equals val.
func find(l any, key string, val any) map[string]any {
	for _, it := range list(l) {
		if m := obj(it); m != nil && fmt.Sprint(m[key]) == fmt.Sprint(val) {
			return m
		}
	}
	return nil
}

// appTestDevice signs a device in for a person without the browser steps.
func appTestDevice(t *testing.T, a *App, personID int64, name string) (string, store.AppDevice) {
	t.Helper()
	ctx := context.Background()
	dc, uc, err := a.db.StartAppDeviceCode(ctx, store.AppDeviceCode{Name: name, Kind: "desktop", OS: "macos", Version: "0.1.0"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.DecideAppDeviceCode(ctx, uc, personID, true); err != nil {
		t.Fatal(err)
	}
	tok, d, err := a.db.ClaimAppDeviceCode(ctx, dc)
	if err != nil {
		t.Fatal(err)
	}
	return tok, d
}

func sessionFor(t *testing.T, a *App, personID int64) map[string]string {
	t.Helper()
	tok, err := a.db.CreateSessionFor(context.Background(), personID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Cookie": sessionCookie + "=" + tok, "Sec-Fetch-Site": "same-origin"}
}

type appFix struct {
	*actFixture
	rid   int64
	owner store.Owner
	tok   string // device "MacBook" of the owner
	dev   store.AppDevice
}

func newAppFix(t *testing.T) *appFix {
	t.Helper()
	af, rid := agentFixture(t)
	f := &appFix{actFixture: af, rid: rid}
	f.owner, _ = f.a.db.PersonalOwner(context.Background())
	f.tok, f.dev = appTestDevice(t, f.a, f.owner.ID, "MacBook")
	t.Cleanup(func() { f.a.app().bg.Wait(); f.a.actions().bg.Wait() })
	return f
}

func (f *appFix) settle() { f.a.actions().bg.Wait(); f.a.app().bg.Wait(); f.a.actions().bg.Wait() }

// ---------- devices ----------

var (
	appUserCodeRe = regexp.MustCompile(`^[A-Z]{3,6}-[0-9]{4}$`)
	appTokenRe    = regexp.MustCompile(`^atd_[0-9A-Za-z]{40}$`)
)

func TestAppDeviceFlow(t *testing.T) {
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	ck := sessionFor(t, a, f.owner.ID)

	start := func(name string) (string, string) {
		t.Helper()
		out := api(t, a, "POST", "/device/start", "", map[string]any{"name": name, "kind": "cli", "os": "linux", "version": "0.1.0"}, 200)
		wantKeys(t, "start", out, "device_code", "user_code", "verify_url", "expires_in", "interval")
		dc, uc := out["device_code"].(string), out["user_code"].(string)
		if len(dc) != 64 || !appUserCodeRe.MatchString(uc) || out["expires_in"] != float64(600) || out["interval"] != float64(2) {
			t.Fatalf("start = %v", out)
		}
		if !strings.HasSuffix(out["verify_url"].(string), "/settings/devices/approve?code="+uc) {
			t.Errorf("verify_url = %v", out["verify_url"])
		}
		return dc, uc
	}
	poll := func(dc string, want int) map[string]any {
		t.Helper()
		return api(t, a, "POST", "/device/poll", "", map[string]any{"device_code": dc}, want)
	}

	// Start → pending → approve in a session → the token, once.
	dc, uc := start("ThinkPad")
	if out := poll(dc, 202); out["status"] != "pending" {
		t.Errorf("pending poll = %v", out)
	}
	// The approval page needs a session: no cookie and a token both go to sign-in.
	if rec := do(t, a, "GET", "/settings/devices/approve?code="+uc, nil, nil); rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "/signin") {
		t.Errorf("approve page signed out = %d", rec.Code)
	}
	if rec := do(t, a, "POST", "/settings/devices/approve", url.Values{"code": {uc}, "decision": {"approve"}},
		map[string]string{"Authorization": "Bearer " + f.tok}); rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "/signin") {
		t.Errorf("approve with a device token = %d, want a redirect to sign-in", rec.Code)
	}
	// A wrong code.
	expectStatus(t, do(t, a, "GET", "/settings/devices/approve?code=NOPE-0000", nil, ck), 404, "wrong code")
	page := expectStatus(t, do(t, a, "GET", "/settings/devices/approve?code="+strings.ToLower(uc), nil, ck), 200, "approve page")
	mustContain(t, page, "approve page", "ThinkPad wants to sign in", uc, "Command line", "linux", `value="approve"`, `value="deny"`)
	// Cross-site posts are blocked.
	if rec := do(t, a, "POST", "/settings/devices/approve", url.Values{"code": {uc}, "decision": {"approve"}},
		map[string]string{"Cookie": ck["Cookie"], "Sec-Fetch-Site": "cross-site"}); rec.Code != 403 {
		t.Errorf("cross-site approve = %d, want 403", rec.Code)
	}
	if out := poll(dc, 202); out["status"] != "pending" {
		t.Errorf("still pending = %v", out)
	}
	rec := do(t, a, "POST", "/settings/devices/approve", url.Values{"code": {uc}, "decision": {"approve"}}, ck)
	if rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "/settings/devices") {
		t.Fatalf("approve = %d\n%.500s", rec.Code, rec.Body)
	}
	out := poll(dc, 200)
	wantKeys(t, "poll", out, "token", "device_id", "person", "studio")
	wantKeys(t, "poll person", obj(out["person"]), "name", "login")
	wantKeys(t, "poll studio", obj(out["studio"]), "name", "url", "version")
	token := out["token"].(string)
	if !appTokenRe.MatchString(token) || obj(out["person"])["login"] != "zoe" {
		t.Fatalf("poll = %v", out)
	}
	// Only the hash is stored.
	var n int
	if err := a.db.DB().QueryRow(`SELECT COUNT(*) FROM app_devices WHERE token_hash=?`, token).Scan(&n); err != nil || n != 0 {
		t.Errorf("plain token stored: %d %v", n, err)
	}
	if err := a.db.DB().QueryRow(`SELECT COUNT(*) FROM app_devices WHERE token_hash=?`, store.Hash(token)).Scan(&n); err != nil || n != 1 {
		t.Errorf("token hash rows = %d %v", n, err)
	}
	// The code is single use.
	wantErr(t, "second poll", poll(dc, 410), "expired")
	expectStatus(t, do(t, a, "GET", "/settings/devices/approve?code="+uc, nil, ck), 404, "used code")

	// The token resolves to the contract's actor and works.
	who, ok := a.resolveCredential(ctx, token)
	if !ok || who.Kind != ActorToken || who.Name != "device:ThinkPad" || who.PersonID != f.owner.ID ||
		strings.Join(who.Scopes, ",") != "app,repo:read,repo:write" {
		t.Fatalf("actor = %+v %v", who, ok)
	}
	me := api(t, a, "GET", "/me", token, nil, 200)
	wantKeys(t, "me", me, "person", "accounts", "studio", "device")
	wantKeys(t, "me person", obj(me["person"]), "name", "login", "avatar_url")
	wantKeys(t, "me studio", obj(me["studio"]), "name", "url", "version", "ssh", "tether", "connect")
	wantKeys(t, "me account", obj(list(me["accounts"])[0]), "name", "display", "kind", "commit_name", "commit_email")
	if d := obj(me["device"]); d["name"] != "ThinkPad" || d["kind"] != "cli" || d["id"] != out["device_id"] {
		t.Errorf("me device = %v", d)
	}

	// Deny.
	dc, uc = start("Stranger")
	expectStatus(t, do(t, a, "GET", "/settings/devices/approve?code="+uc, nil, ck), 200, "deny page")
	expectStatus(t, do(t, a, "POST", "/settings/devices/approve", url.Values{"code": {uc}, "decision": {"deny"}}, ck), 303, "deny")
	wantErr(t, "denied poll", poll(dc, 410), "denied")

	// Expiry.
	dc, uc = start("Sleepy")
	if _, err := a.db.DB().Exec(`UPDATE app_device_codes SET expires_at=? WHERE user_code=?`, time.Now().Add(-time.Second).Unix(), uc); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, do(t, a, "GET", "/settings/devices/approve?code="+uc, nil, ck), 404, "expired code page")
	expectStatus(t, do(t, a, "POST", "/settings/devices/approve", url.Values{"code": {uc}, "decision": {"approve"}}, ck), 404, "approve expired")
	wantErr(t, "expired poll", poll(dc, 410), "expired")
	// A device code nobody was given.
	wantErr(t, "wrong device code", poll(strings.Repeat("ab", 32), 410), "expired")
	wantErr(t, "short device code", poll("nope", 410), "expired")

	// Validation and the rate limit on start.
	wantErr(t, "bad kind", api(t, a, "POST", "/device/start", "", map[string]any{"name": "X", "kind": "toaster"}, 422), "invalid_kind")
	wantErr(t, "no name", api(t, a, "POST", "/device/start", "", map[string]any{"name": " ", "kind": "cli"}, 422), "invalid_name")
	limited := false
	for i := 0; i < 12 && !limited; i++ {
		rec, out := appReq(t, a, "POST", appAPIPrefix+"/device/start", "", map[string]any{"name": "Flood", "kind": "cli"}, nil)
		if rec.Code == 429 {
			limited = true
			wantErr(t, "start flood", out, "rate_limited")
		}
	}
	if !limited {
		t.Error("device/start was never rate limited")
	}

	// Settings → Devices lists them, linked from the settings menu.
	page = expectStatus(t, do(t, a, "GET", "/settings/devices", nil, ck), 200, "devices page")
	mustContain(t, page, "devices page", "ThinkPad", "MacBook", "Command line", "Desktop app", `action="/settings/devices/revoke"`, "Last seen")
	mustContain(t, expectStatus(t, do(t, a, "GET", "/settings/tokens", nil, ck), 200, "tokens page"), "settings menu", `href="/settings/devices"`, "App API")
}

func TestAppDeviceRevokeAndLimits(t *testing.T) {
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	ck := sessionFor(t, a, f.owner.ID)

	// This device's SSH key: created once, the same key again returns it.
	key, fp := sshkPublicKey(t, "zoe@macbook")
	out := api(t, a, "POST", "/device/ssh-key", f.tok, map[string]any{"title": "MacBook (Atelier Desktop)", "key": key}, 201)
	wantKeys(t, "ssh-key", out, "id", "fingerprint")
	if out["fingerprint"] != fp {
		t.Errorf("fingerprint = %v, want %s", out["fingerprint"], fp)
	}
	again := api(t, a, "POST", "/device/ssh-key", f.tok, map[string]any{"title": "again", "key": key}, 200)
	if again["id"] != out["id"] {
		t.Errorf("same key again = %v, want id %v", again, out["id"])
	}
	wantErr(t, "bad key", api(t, a, "POST", "/device/ssh-key", f.tok, map[string]any{"key": "not a key"}, 422), "invalid_key")
	k, err := a.db.SSHKeyByFingerprint(ctx, fp)
	if err != nil || k.Name != "MacBook (Atelier Desktop)" {
		t.Fatalf("key = %+v %v", k, err)
	}
	if person, _ := a.db.SSHKeyPerson(ctx, k.ID); person != f.owner.ID {
		t.Errorf("key person = %d", person)
	}
	// A key the owner added by hand isn't the device's to lose.
	manual, manualFP := sshkPublicKey(t, "by hand")
	expectStatus(t, do(t, a, "POST", "/settings/keys", url.Values{"name": {"By hand"}, "key": {manual}}, ck), 303, "add key by hand")
	api(t, a, "POST", "/device/ssh-key", f.tok, map[string]any{"key": manual}, 200)

	// A device token is a read/write token: it reads and pushes, never administers or reaches settings.
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	who, _ := a.resolveCredential(ctx, f.tok)
	if !a.can(ctx, who, demo, PermRead) || !a.can(ctx, who, demo, PermWrite) || a.can(ctx, who, demo, PermAdmin) {
		t.Errorf("device permissions: read %v write %v admin %v", a.can(ctx, who, demo, PermRead), a.can(ctx, who, demo, PermWrite), a.can(ctx, who, demo, PermAdmin))
	}
	bearer := map[string]string{"Authorization": "Bearer " + f.tok}
	for _, p := range []string{"/settings/tokens", "/settings/devices", "/settings/keys", "/settings/passkeys", "/settings/remote", "/zoe/demo/settings"} {
		if rec := do(t, a, "GET", p, nil, bearer); rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "/signin") {
			t.Errorf("GET %s with a device token = %d, want a redirect to sign-in", p, rec.Code)
		}
	}
	for p, form := range map[string]url.Values{
		"/settings/tokens":         {"name": {"x"}, "access": {"write"}, "expires": {"30"}},
		"/settings/devices/revoke": {"id": {itoa(f.dev.ID)}},
		"/settings/keys/delete":    {"id": {itoa(k.ID)}},
		"/zoe/demo/delete":         {},
		"/zoe/demo/settings":       {"description": {"hacked"}},
	} {
		if rec := do(t, a, "POST", p, form, bearer); rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "/signin") {
			t.Errorf("POST %s with a device token = %d, want a redirect to sign-in", p, rec.Code)
		}
	}
	if toks, _ := a.db.Tokens(ctx); len(toks) != 2 { // the fixture's read and write tokens
		t.Errorf("tokens = %d, want 2", len(toks))
	}
	// MCP needs its own scope.
	if rec, _ := appReq(t, a, "POST", "/mcp", f.tok, map[string]any{}, nil); rec.Code != 403 {
		t.Errorf("MCP with a device token = %d, want 403", rec.Code)
	}

	// Ordinary tokens: only with the app scope.
	appTok, _ := a.db.CreateToken(ctx, "script", tokenScopes["app"], time.Hour)
	if me := api(t, a, "GET", "/me", appTok, nil, 200); me["device"] != nil {
		t.Errorf("an ordinary token has a device: %v", me["device"])
	}
	wantErr(t, "ssh-key with a token", api(t, a, "POST", "/device/ssh-key", appTok, map[string]any{"key": key}, 403), "device_only")
	wantErr(t, "write token", api(t, a, "GET", "/me", f.write, nil, 403), "forbidden")
	wantErr(t, "no token", api(t, a, "GET", "/me", "", nil, 401), "unauthorized")
	wantErr(t, "junk token", api(t, a, "GET", "/me", store.AppDeviceTokenPrefix+strings.Repeat("0", 40), nil, 401), "unauthorized")
	if rec := do(t, a, "GET", appAPIPrefix+"/me", nil, ck); rec.Code != 401 {
		t.Errorf("a session cookie on the app API = %d, want 401", rec.Code)
	}
	wantErr(t, "unknown endpoint", api(t, a, "GET", "/nope", f.tok, nil, 404), "not_found")

	// Revoke: the token dies at once, with the SSH key it added.
	expectStatus(t, do(t, a, "POST", "/settings/devices/revoke", url.Values{"id": {itoa(f.dev.ID)}}, ck), 303, "revoke")
	wantErr(t, "revoked", api(t, a, "GET", "/me", f.tok, nil, 401), "revoked")
	if _, ok := a.resolveCredential(ctx, f.tok); ok {
		t.Error("a revoked device token still resolves (git, REST)")
	}
	if _, err := a.db.SSHKeyByFingerprint(ctx, fp); err == nil {
		t.Error("the device's SSH key survived the revoke")
	}
	if _, err := a.db.SSHKeyByFingerprint(ctx, manualFP); err != nil {
		t.Error("revoking a device removed a key added by hand")
	}
	if strings.Contains(expectStatus(t, do(t, a, "GET", "/settings/devices", nil, ck), 200, "devices"), "MacBook") {
		t.Error("the revoked device is still listed")
	}
	expectStatus(t, do(t, a, "POST", "/settings/devices/revoke", url.Values{"id": {itoa(f.dev.ID)}}, ck), 404, "revoke twice")
}

// ---------- status, runs, agents ----------

const appCIYML = `name: CI
on:
  push:
    branches: [main]
jobs:
  test:
    runs-on: linux
    steps:
      - run: make test
`

func TestAppStatusRunsAndAgents(t *testing.T) {
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")

	// Nothing yet.
	st := api(t, a, "GET", "/status", f.tok, nil, 200)
	wantKeys(t, "status", st, "needs", "running", "feed", "unread")
	if len(list(st["needs"])) != 0 || len(list(st["running"])) != 0 {
		t.Fatalf("fresh status = %v", st)
	}
	if fd := obj(list(st["feed"])[0]); fd["thread"] != "yours" || !strings.Contains(fd["text"].(string), "Initial") ||
		!strings.HasPrefix(fd["web_path"].(string), "/zoe/demo/commit/") || fd["at"] == nil {
		t.Errorf("feed = %v", st["feed"])
	}

	// One push: a CI run and an agent waiting for its first approval.
	pullsCommit(t, f.work, ".atelier/workflows/ci.yml", appCIYML, "Add CI")
	pullsCommit(t, f.work, ".atelier/workflows/review.md", agentReviewMD, "Add review agent")
	f.push(t, "main")
	f.settle()
	rn := f.registerRunner(t, "Box", []string{"linux"}, []string{"jobs", "agents"})
	job := rn.poll()
	if job == nil {
		t.Fatal("no CI job")
	}
	rn.logs(job.ID, 0, "compiling", "FAIL: TestThing", "exit status 1")

	st = api(t, a, "GET", "/status", f.tok, nil, 200)
	run := find(st["running"], "job_id", job.ID)
	wantKeys(t, "running", run, "job_id", "run_id", "repo", "workflow", "job", "kind", "engine", "runner", "started_at", "detail")
	if run["repo"] != "zoe/demo" || run["workflow"] != "CI" || run["job"] != "test" || run["kind"] != "job" || run["runner"] != "Box" || run["engine"] != nil {
		t.Errorf("running = %v", run)
	}

	// The log tail, as stored.
	lg := api(t, a, "GET", "/jobs/"+itoa(job.ID)+"/log?tail=2", f.tok, nil, 200)
	wantKeys(t, "log", lg, "lines", "truncated", "status")
	if ls := list(lg["lines"]); len(ls) != 2 || ls[1] != "exit status 1" || lg["truncated"] != true || lg["status"] != "running" {
		t.Errorf("log = %v", lg)
	}
	api(t, a, "GET", "/jobs/99999/log", f.tok, nil, 404)

	rn.runJob(job, actions.StatusFailure, nil)
	f.settle()

	// A pull request that is ready, one with a new review, and a secret alert.
	runGit(t, f.work, "checkout", "-q", "-b", "feature/ready")
	head := pullsCommit(t, f.work, "ready.txt", "ready\n", "Ready")
	runGit(t, f.work, "push", "-q", "origin", "feature/ready")
	ready, err := a.db.CreatePull(ctx, store.NewPull{RepoID: f.rid, Title: "Security update", Head: "feature/ready", Base: "main",
		HeadSHA: head, AuthorKind: ActorOwner, AuthorName: "zoe"})
	if err != nil {
		t.Fatal(err)
	}
	a.emit(ctx, pullEvent(EventPullCommented, appsView(demo), ready, Actor{Kind: ActorBot, Name: "reviewer"}, "Looks good to me"))
	if _, _, err := a.db.RecordSecretAlert(ctx, store.SecretAlert{RepoID: f.rid, Kind: "code", Source: "push", RuleID: "aws", RuleName: "AWS access key",
		Fingerprint: "fp1", Redacted: "AKIA…", Path: "config.env"}); err != nil {
		t.Fatal(err)
	}

	st = api(t, a, "GET", "/status", f.tok, nil, 200)
	needs := st["needs"]
	for _, n := range list(needs) {
		wantKeys(t, "need", obj(n), "id", "kind", "thread", "title", "detail", "repo", "web_path", "action")
		wantKeys(t, "need action", obj(obj(n)["action"]), "label", "type")
	}
	agent := find(needs, "kind", "agent_approval")
	if agent == nil || agent["thread"] != "agents" || agent["title"] != "Approve Review helper before it runs" ||
		agent["detail"] != "agent · wants pulls.comment" || agent["repo"] != "zoe/demo" || obj(agent["action"])["type"] != "approve_agent" ||
		!strings.HasPrefix(agent["web_path"].(string), "/zoe/demo/actions/runs/") {
		t.Fatalf("agent need = %v\nall: %v", agent, needs)
	}
	agentJob := int64(obj(agent["action"])["job_id"].(float64))
	if agent["id"] != "agent:"+itoa(agentJob) {
		t.Errorf("agent id = %v", agent["id"])
	}
	failed := find(needs, "kind", "run_failed")
	if failed == nil || failed["title"] != "CI failed on main" || failed["detail"] != "test job · ran on Box" || failed["thread"] != "yours" ||
		obj(failed["action"])["type"] != "open_run" || failed["id"] != "run:"+fmt.Sprint(obj(failed["action"])["run_id"]) {
		t.Errorf("failed run need = %v", failed)
	}
	pr := find(needs, "kind", "pull_ready")
	if pr == nil || pr["title"] != "Security update is ready to merge" || pr["id"] != "pull:zoe/demo:"+itoa(ready.Number) ||
		obj(pr["action"])["type"] != "open_pull" || obj(pr["action"])["number"] != float64(ready.Number) ||
		pr["web_path"] != "/zoe/demo/pulls/"+itoa(ready.Number) {
		t.Errorf("ready pull need = %v", pr)
	}
	if rv := find(needs, "kind", "pull_review"); rv == nil || rv["title"] != "New review on Security update" || !strings.Contains(rv["detail"].(string), "Looks good") {
		t.Errorf("review need = %v", rv)
	}
	if sec := find(needs, "kind", "security_alert"); sec == nil || sec["title"] != "1 secret found in demo" || sec["web_path"] != "/zoe/demo/security" {
		t.Errorf("security need = %v", sec)
	}
	if st["unread"].(float64) < 1 {
		t.Errorf("unread = %v", st["unread"])
	}
	if len(list(st["running"])) != 0 {
		t.Errorf("still running: %v", st["running"])
	}

	// Runs, newest first, with their jobs.
	runs := api(t, a, "GET", "/runs?limit=20", f.tok, nil, 200)
	ci := find(runs["runs"], "workflow", "CI")
	wantKeys(t, "run", ci, "run_id", "repo", "workflow", "event", "branch", "status", "started_at", "duration_s", "jobs")
	if ci["status"] != "failure" || ci["event"] != "push" || ci["branch"] != "main" || ci["repo"] != "zoe/demo" {
		t.Errorf("CI run = %v", ci)
	}
	cj := obj(list(ci["jobs"])[0])
	wantKeys(t, "run job", cj, "job_id", "name", "status", "runner", "duration_s", "kind")
	if cj["name"] != "test" || cj["status"] != "failure" || cj["runner"] != "Box" || cj["kind"] != "job" {
		t.Errorf("CI job = %v", cj)
	}
	if ag := find(runs["runs"], "workflow", "Review helper"); ag == nil || ag["status"] != "waiting" || obj(list(ag["jobs"])[0])["kind"] != "agent" {
		t.Errorf("agent run = %v", ag)
	}
	if got := list(api(t, a, "GET", "/runs?runner=Box&repo=zoe/demo", f.tok, nil, 200)["runs"]); len(got) != 1 {
		t.Errorf("runs on Box = %d, want 1", len(got))
	}
	if got := list(api(t, a, "GET", "/runs?limit=1", f.tok, nil, 200)["runs"]); len(got) != 1 {
		t.Errorf("limit=1 gave %d runs", len(got))
	}
	api(t, a, "GET", "/runs?repo=zoe/nope", f.tok, nil, 404)

	// Agents waiting, and approving one as the device.
	wait := api(t, a, "GET", "/agents/waiting", f.tok, nil, 200)
	wj := find(wait["jobs"], "job_id", agentJob)
	wantKeys(t, "waiting agent", wj, "job_id", "run_id", "repo", "workflow", "reason", "engine", "tools", "write_tools", "max_turns", "timeout_minutes")
	if wj["workflow"] != "Review helper" || wj["reason"] != "first run" || wj["engine"] != "claude" || wj["max_turns"] != float64(12) ||
		fmt.Sprint(wj["tools"]) != "[pulls.comment pulls.get]" || fmt.Sprint(wj["write_tools"]) != "[pulls.comment]" {
		t.Errorf("waiting agent = %v", wj)
	}
	if out := api(t, a, "POST", "/agents/jobs/"+itoa(agentJob)+"/approve", f.tok, nil, 200); out["approved"] != true {
		t.Errorf("approve = %v", out)
	}
	aj, _ := a.db.ActionsJob(ctx, agentJob)
	if aj.Status != actions.StatusQueued || aj.ApprovedBy != "device:MacBook" {
		t.Errorf("approved job = %s by %q", aj.Status, aj.ApprovedBy)
	}
	wantErr(t, "approve twice", api(t, a, "POST", "/agents/jobs/"+itoa(agentJob)+"/approve", f.tok, nil, 409), "not_waiting")
	api(t, a, "POST", "/agents/jobs/"+itoa(job.ID)+"/approve", f.tok, nil, 404) // not an agent
	if len(list(api(t, a, "GET", "/agents/waiting", f.tok, nil, 200)["jobs"])) != 0 {
		t.Error("the approved agent is still waiting")
	}
	if find(api(t, a, "GET", "/status", f.tok, nil, 200)["needs"], "kind", "agent_approval") != nil {
		t.Error("the approved agent still needs approval")
	}

	// Cancelling a job.
	if out := api(t, a, "POST", "/jobs/"+itoa(agentJob)+"/cancel", f.tok, nil, 200); out["cancelled"] != true {
		t.Errorf("cancel = %v", out)
	}
	if aj, _ = a.db.ActionsJob(ctx, agentJob); aj.Status != actions.StatusCancelled {
		t.Errorf("cancelled job = %s", aj.Status)
	}
	wantErr(t, "cancel twice", api(t, a, "POST", "/jobs/"+itoa(agentJob)+"/cancel", f.tok, nil, 409), "finished")

	// A newer run of the same workflow on the same branch replaces the failed one.
	runGit(t, f.work, "checkout", "-q", "main")
	pullsCommit(t, f.work, "fix.txt", "fix\n", "Fix the test")
	f.push(t, "main")
	f.settle()
	if n := find(api(t, a, "GET", "/status", f.tok, nil, 200)["needs"], "kind", "run_failed"); n != nil {
		t.Errorf("superseded failed run still needs attention: %v", n)
	}
}

// ---------- repositories ----------

func TestAppRepos(t *testing.T) {
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	a.cfg.PublicURL = "https://studio.example"

	out := api(t, a, "GET", "/repos", f.tok, nil, 200)
	rp := find(out["repos"], "full_name", "zoe/demo")
	wantKeys(t, "repo", rp, "full_name", "owner", "owner_kind", "name", "private", "description", "default_branch", "language",
		"language_color", "clone_https", "clone_ssh", "github", "open_pulls", "updated_at")
	if rp["owner"] != "zoe" || rp["owner_kind"] != "user" || rp["name"] != "demo" || rp["private"] != true || rp["default_branch"] != "main" ||
		rp["clone_https"] != "https://studio.example/zoe/demo.git" || rp["github"] != nil || rp["open_pulls"] != float64(0) {
		t.Errorf("repo = %v", rp)
	}
	if _, ok := out["github_hits"]; ok {
		t.Error("github_hits without github=1")
	}

	// Create, like the New repository page.
	made := api(t, a, "POST", "/repos", f.tok, map[string]any{"owner": "zoe", "name": "Ingle", "private": true, "description": "A hearth"}, 201)
	if made["full_name"] != "zoe/Ingle" || made["description"] != "A hearth" || made["private"] != true {
		t.Errorf("created = %v", made)
	}
	ingle, err := a.db.RepoByName(ctx, "zoe", "Ingle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.git.Open("zoe", "Ingle"); err != nil {
		t.Errorf("no git repository behind the new repo: %v", err)
	}
	if !ingle.Private {
		t.Error("the new repo isn't private")
	}
	wantErr(t, "taken", api(t, a, "POST", "/repos", f.tok, map[string]any{"owner": "zoe", "name": "ingle"}, 409), "name_taken")
	wantErr(t, "bad name", api(t, a, "POST", "/repos", f.tok, map[string]any{"owner": "zoe", "name": "bad name.git"}, 422), "invalid_name")
	wantErr(t, "no such owner", api(t, a, "POST", "/repos", f.tok, map[string]any{"owner": "nobody", "name": "x"}, 403), "forbidden")
	// A company account works; a read-only credential can't create.
	if _, err := a.db.CreateOwner(ctx, "elyndra", "Elyndra Studios", "company"); err != nil {
		t.Fatal(err)
	}
	if c := api(t, a, "POST", "/repos", f.tok, map[string]any{"owner": "elyndra", "name": "site", "private": false}, 201); c["owner_kind"] != "company" || c["private"] != false {
		t.Errorf("company repo = %v", c)
	}

	if got := list(api(t, a, "GET", "/repos?q=ING", f.tok, nil, 200)["repos"]); len(got) != 1 || obj(got[0])["name"] != "Ingle" {
		t.Errorf("q=ING → %v", got)
	}
	// GitHub hits need Tether; without it the list still answers.
	gh := api(t, a, "GET", "/repos?q=loop&github=1", f.tok, nil, 200)
	if hits, ok := gh["github_hits"].([]any); !ok || len(hits) != 0 || gh["github_error"] == nil {
		t.Errorf("github hits without Tether = %v", gh)
	}
	wantErr(t, "mirror without Tether", api(t, a, "POST", "/repos/mirror", f.tok, map[string]any{"github": "maralind/loopkit"}, 409), "tether_off")
	wantErr(t, "mirror nonsense", api(t, a, "POST", "/repos/mirror", f.tok, map[string]any{"github": "../etc"}, 422), "mirror_failed")
}

func TestAppReposThroughTether(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	f.connect(t)
	tok, _ := appTestDevice(t, a, f.me(t).ID, "MacBook")
	pub := f.gh.addRepo("dave/rocket", false)
	w := t.TempDir()
	runGit(t, w, "init", "-q", "--initial-branch=main", ".")
	pullsCommit(t, w, "README.md", "# rocket\n", "first")
	runGit(t, w, "push", "-q", f.gh.bare("dave/rocket"), "main")
	f.gh.mu.Lock()
	f.gh.search = []tether.Repo{*pub}
	f.gh.mu.Unlock()

	out := api(t, a, "GET", "/repos?q=rocket&github=1", tok, nil, 200)
	hit := find(out["github_hits"], "full_name", "dave/rocket")
	wantKeys(t, "github hit", hit, "full_name", "description", "atelier_badge")
	if hit["atelier_badge"] != false {
		t.Errorf("hit = %v", hit)
	}
	f.wait()

	if m := api(t, a, "POST", "/repos/mirror", tok, map[string]any{"github": "dave/rocket"}, 202); m["full_name"] != "zoe/rocket" {
		t.Errorf("mirror = %v", m)
	}
	waitFor(t, func() bool { _, err := a.db.RepoByName(ctx, "zoe", "rocket"); return err == nil })
	f.wait()
	if _, err := a.db.RepoByName(ctx, "zoe", "rocket"); err != nil {
		t.Fatal(err)
	}
}

// ---------- pull requests ----------

const appPRYML = `name: ci
on: pull_request
jobs:
  test:
    runs-on: linux
    steps:
      - run: make
`

// openPull makes a branch with one commit and opens a pull request on the web.
func (f *appFix) openPull(t *testing.T, branch, title string) store.Pull {
	t.Helper()
	runGit(t, f.work, "checkout", "-q", "main")
	runGit(t, f.work, "checkout", "-q", "-b", branch)
	pullsCommit(t, f.work, strings.ReplaceAll(branch, "/", "-")+".txt", branch+"\n", "Work on "+branch)
	f.push(t, branch)
	expectStatus(t, do(t, f.a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {branch}, "title": {title}}, f.ck), 303, "open PR")
	f.settle()
	p, err := f.a.db.OpenPullFor(context.Background(), f.rid, "main", branch)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *appFix) pull(t *testing.T, n int64) store.Pull {
	t.Helper()
	p, err := f.a.db.PullByNumber(context.Background(), f.rid, n)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *appFix) autoMerge(n int64) (store.AppAutoMerge, bool) {
	m, err := f.a.db.AppAutoMerge(context.Background(), f.rid, n)
	return m, err == nil
}

func TestAppPullsAndMerging(t *testing.T) {
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	pullsCommit(t, f.work, ".atelier/workflows/pr.yml", appPRYML, "PR workflow")
	f.push(t, "main")
	rn := f.registerRunner(t, "Box", []string{"linux"}, []string{"jobs"})
	base := "/repos/zoe/demo/pulls/"

	// --- reading ---
	p1 := f.openPull(t, "feature/a", "Remote access switch")
	if _, err := a.db.AddPullComment(ctx, p1.ID, "import", "github:CodeRabbit[bot]", "Consider a test."); err != nil {
		t.Fatal(err)
	}
	d := api(t, a, "GET", base+itoa(p1.Number), f.tok, nil, 200)
	wantKeys(t, "pull", d, "repo", "number", "title", "state", "head", "base", "author", "commits", "files", "via", "merge", "checks", "comments", "web_path")
	mg := obj(d["merge"])
	wantKeys(t, "merge", mg, "can_merge", "reason", "methods", "on_github", "auto_merge")
	if d["repo"] != "zoe/demo" || d["title"] != "Remote access switch" || d["state"] != "open" || d["head"] != "feature/a" || d["base"] != "main" ||
		d["author"] != "owner:zoe" || d["commits"] != float64(1) || d["files"] != float64(1) || d["via"] != nil ||
		d["web_path"] != "/zoe/demo/pulls/"+itoa(p1.Number) {
		t.Errorf("pull = %v", d)
	}
	if fmt.Sprint(mg["methods"]) != "[squash merge]" || mg["on_github"] != false || mg["auto_merge"] != nil || mg["can_merge"] != true ||
		mg["reason"] != "1 check is still running." {
		t.Errorf("merge = %v", mg)
	}
	chk := obj(list(d["checks"])[0])
	wantKeys(t, "check", chk, "name", "status", "detail", "run_id", "job_id")
	if chk["name"] != "ci · test" || chk["status"] != "pending" {
		t.Errorf("check = %v", chk)
	}
	cm := obj(list(d["comments"])[0])
	wantKeys(t, "comment", cm, "author", "via", "kind", "body", "at")
	if cm["author"] != "CodeRabbit[bot]" || cm["via"] != "tether" || cm["kind"] != "bot" || cm["body"] != "Consider a test." {
		t.Errorf("comment = %v", cm)
	}
	for _, path := range []string{"/pulls?state=open", "/pulls", "/repos/zoe/demo/pulls?state=all"} {
		sum := find(api(t, a, "GET", path, f.tok, nil, 200)["pulls"], "number", p1.Number)
		wantKeys(t, "summary "+path, sum, "repo", "number", "title", "state", "head", "base", "author", "commits", "files", "via", "merge", "checks", "web_path")
		if _, has := sum["comments"]; has {
			t.Errorf("%s: a summary carries comments", path)
		}
		if c := obj(sum["checks"]); c["pending"] != float64(1) || c["passed"] != float64(0) || c["failed"] != float64(0) || c["waiting"] != float64(0) {
			t.Errorf("%s: summary checks = %v", path, sum["checks"])
		}
	}
	if got := list(api(t, a, "GET", "/repos/zoe/demo/pulls?state=merged", f.tok, nil, 200)["pulls"]); len(got) != 0 {
		t.Errorf("merged pulls = %v", got)
	}
	wantErr(t, "bad state", api(t, a, "GET", "/pulls?state=sideways", f.tok, nil, 422), "invalid_state")
	api(t, a, "GET", base+"999", f.tok, nil, 404)

	// --- merge when green: merges once the run completes ---
	wantErr(t, "bad method", api(t, a, "POST", base+itoa(p1.Number)+"/merge", f.tok, map[string]any{"method": "rebase"}, 422), "invalid_method")
	out := api(t, a, "POST", base+itoa(p1.Number)+"/merge", f.tok, map[string]any{"method": "squash", "when_green": true}, 202)
	if am := obj(out["auto_merge"]); am["method"] != "squash" || am["by"] != "device:MacBook" {
		t.Fatalf("when_green = %v", out)
	}
	f.settle()
	if !f.pull(t, p1.Number).IsOpen() {
		t.Fatal("merged while a check was still running")
	}
	if am := obj(obj(api(t, a, "GET", base+itoa(p1.Number), f.tok, nil, 200)["merge"])["auto_merge"]); am["by"] != "device:MacBook" || am["method"] != "squash" {
		t.Errorf("auto_merge on the pull = %v", am)
	}
	page := expectStatus(t, do(t, a, "GET", "/zoe/demo/pulls/"+itoa(p1.Number), nil, f.ck), 200, "PR page")
	mustContain(t, page, "PR page", "Will merge when checks pass", "device:MacBook", `/pulls/`+itoa(p1.Number)+`/auto-merge"`, `value="cancel"`)
	// While it waits, it isn't offered as "ready to merge".
	if n := find(api(t, a, "GET", "/status", f.tok, nil, 200)["needs"], "kind", "pull_ready"); n != nil {
		t.Errorf("a pull waiting on checks is listed as ready: %v", n)
	}
	rn.runJob(rn.poll(), actions.StatusSuccess, nil)
	waitUntil(t, "merge when green", func() bool { return f.pull(t, p1.Number).IsMerged() })
	f.settle()
	if p := f.pull(t, p1.Number); p.MergedBy != "device:MacBook" || p.MergeMethod != "squash" {
		t.Errorf("merged by %q with %q", p.MergedBy, p.MergeMethod)
	}
	if _, waiting := f.autoMerge(p1.Number); waiting {
		t.Error("the merge is still recorded as waiting")
	}
	if d := api(t, a, "GET", base+itoa(p1.Number), f.tok, nil, 200); d["state"] != "merged" || obj(d["merge"])["can_merge"] != false {
		t.Errorf("merged pull = %v", d)
	}

	// --- cancelled when a check fails ---
	p2 := f.openPull(t, "feature/b", "Will fail")
	api(t, a, "POST", base+itoa(p2.Number)+"/merge", f.tok, map[string]any{"method": "squash", "when_green": true}, 202)
	f.settle()
	rn.runJob(rn.poll(), actions.StatusFailure, nil)
	waitUntil(t, "cancel on failure", func() bool { _, waiting := f.autoMerge(p2.Number); return !waiting })
	f.settle()
	if !f.pull(t, p2.Number).IsOpen() {
		t.Error("a pull with a failed check was merged")
	}
	ns, _ := a.db.Notifications(ctx, store.NotificationQuery{PersonID: f.owner.ID, SubjectTypes: []string{"pull"}, Limit: 50})
	told := false
	for _, n := range ns {
		told = told || (n.Kind == EventPullAutoMergeCancelled && n.Number == p2.Number && strings.Contains(n.Detail, "check failed"))
	}
	if !told {
		t.Errorf("no notification about the cancelled merge: %+v", ns)
	}
	if d := api(t, a, "GET", base+itoa(p2.Number), f.tok, nil, 200); obj(list(d["checks"])[0])["status"] != "failure" ||
		obj(list(d["checks"])[0])["detail"] != "Box · 0s" && !strings.HasPrefix(fmt.Sprint(obj(list(d["checks"])[0])["detail"]), "Box") {
		t.Errorf("failed check = %v", d["checks"])
	}

	// --- a new commit: the asker's own push keeps it, anyone else's cancels it ---
	p3 := f.openPull(t, "feature/c", "Moving target")
	api(t, a, "POST", base+itoa(p3.Number)+"/merge", f.tok, map[string]any{"method": "merge", "when_green": true, "delete_branch": true}, 202)
	f.settle()
	pullsCommit(t, f.work, "more.txt", "more\n", "More work")
	mine := f.push(t, "feature/c") // pushed as owner:zoe
	waitUntil(t, "own push keeps the merge", func() bool { m, ok := f.autoMerge(p3.Number); return ok && m.HeadSHA == mine })
	f.settle()
	pullsCommit(t, f.work, "bot.txt", "bot\n", "A bot's idea")
	runGit(t, f.work, "push", "-q", "origin", "feature/c")
	theirs := runGit(t, f.work, "rev-parse", "feature/c")
	if err := a.db.QueueActionsPush(ctx, store.ActionsPush{Owner: "zoe", Repo: "demo", Old: mine, New: theirs, Ref: "refs/heads/feature/c",
		Pusher: "bot:robot"}); err != nil {
		t.Fatal(err)
	}
	a.drainActionsPushes(ctx)
	waitUntil(t, "someone else's push cancels the merge", func() bool { _, waiting := f.autoMerge(p3.Number); return !waiting })
	f.settle()
	if !f.pull(t, p3.Number).IsOpen() {
		t.Error("merged after someone else pushed")
	}

	// --- cancelling by hand: DELETE, and the web button ---
	api(t, a, "POST", base+itoa(p3.Number)+"/merge", f.tok, map[string]any{"when_green": true}, 202)
	if out := api(t, a, "DELETE", base+itoa(p3.Number)+"/merge", f.tok, nil, 200); out["cancelled"] != true {
		t.Errorf("DELETE merge = %v", out)
	}
	wantErr(t, "nothing to cancel", api(t, a, "DELETE", base+itoa(p3.Number)+"/merge", f.tok, nil, 404), "no_auto_merge")
	api(t, a, "POST", base+itoa(p3.Number)+"/merge", f.tok, map[string]any{"when_green": true}, 202)
	expectStatus(t, do(t, a, "POST", "/zoe/demo/pulls/"+itoa(p3.Number)+"/auto-merge", url.Values{"action": {"cancel"}}, f.ck), 303, "web cancel")
	if _, waiting := f.autoMerge(p3.Number); waiting {
		t.Error("the web Cancel button didn't cancel")
	}
	f.settle()

	// --- merge now: branch rules as on the web ---
	rule, err := a.db.CreateBranchRule(ctx, store.BranchRule{RepoID: f.rid, Pattern: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetBranchRuleChecks(ctx, rule.ID, []string{"deploy-gate"}); err != nil {
		t.Fatal(err)
	}
	blocked := api(t, a, "POST", base+itoa(p3.Number)+"/merge", f.tok, map[string]any{"method": "merge"}, 409)
	wantErr(t, "blocked by a branch rule", blocked, "not_mergeable")
	if !strings.Contains(blocked["message"].(string), "deploy-gate") {
		t.Errorf("409 reason = %v", blocked["message"])
	}
	if mg := obj(api(t, a, "GET", base+itoa(p3.Number), f.tok, nil, 200)["merge"]); mg["can_merge"] != false || !strings.Contains(mg["reason"].(string), "deploy-gate") {
		t.Errorf("blocked merge = %v", mg)
	}
	if err := a.db.DeleteBranchRule(ctx, f.rid, rule.ID); err != nil {
		t.Fatal(err)
	}
	// The read-only fixture token has no app scope; a read-only app token can't merge.
	if _, err := a.db.DB().Exec(`INSERT INTO tokens (name, token_hash, prefix, scopes, created_at, person_id) VALUES ('ro', ?, 'atl_ro', 'app repo:read', ?, ?)`,
		store.Hash("atl_readonlyapptoken"), time.Now().Unix(), f.owner.ID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, "read-only merge", api(t, a, "POST", base+itoa(p3.Number)+"/merge", "atl_readonlyapptoken", map[string]any{}, 403), "forbidden")
	api(t, a, "GET", base+itoa(p3.Number), "atl_readonlyapptoken", nil, 200)

	merged := api(t, a, "POST", base+itoa(p3.Number)+"/merge", f.tok, map[string]any{"method": "merge"}, 200)
	if merged["merged"] != true || len(fmt.Sprint(merged["sha"])) != 40 {
		t.Fatalf("merge now = %v", merged)
	}
	if p := f.pull(t, p3.Number); !p.IsMerged() || p.MergedSHA != merged["sha"] || p.MergedBy != "device:MacBook" || p.MergeMethod != "merge" {
		t.Errorf("merged pull = %+v", p)
	}
	wantErr(t, "merge twice", api(t, a, "POST", base+itoa(p3.Number)+"/merge", f.tok, map[string]any{}, 409), "not_open")
	f.settle()
}

// ---------- live events ----------

type sseEvent struct {
	Event string
	Data  map[string]any
}

type sseClient struct {
	events chan sseEvent
	pings  chan struct{}
	cancel context.CancelFunc
	status int
}

func sseOpen(t *testing.T, base, token string) *sseClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+appAPIPrefix+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &sseClient{events: make(chan sseEvent, 100), pings: make(chan struct{}, 100), cancel: cancel, status: resp.StatusCode}
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	if resp.StatusCode != 200 {
		return c
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("events content type = %q", ct)
	}
	go func() {
		sc := bufio.NewScanner(resp.Body)
		name := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == ": ping":
				select {
				case c.pings <- struct{}{}:
				default:
				}
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var m map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil {
					c.events <- sseEvent{name, m}
				}
			}
		}
		close(c.events)
	}()
	return c
}

// notify waits for the next notify event (skipping status events).
func (c *sseClient) next(t *testing.T, event string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				t.Fatalf("stream closed while waiting for %s", event)
			}
			if ev.Event == event {
				return ev.Data
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %s event", event)
		}
	}
}

// quiet fails if anything but status events arrives for a short while.
func (c *sseClient) quiet(t *testing.T, what string, alsoStatus bool) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case ev, ok := <-c.events:
			if !ok {
				return
			}
			if ev.Event != "status" || alsoStatus {
				t.Fatalf("%s: got %s %v", what, ev.Event, ev.Data)
			}
		case <-deadline:
			return
		}
	}
}

func TestAppLiveEvents(t *testing.T) {
	oldPing := appPingEvery
	appPingEvery = 50 * time.Millisecond
	t.Cleanup(func() { appPingEvery = oldPing })
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	demo, _ := a.db.RepoByName(ctx, "zoe", "demo")
	tokB, _ := appTestDevice(t, a, f.owner.ID, "Studio PC")
	mallory, err := a.db.CreateOwner(ctx, "mallory", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	tokM, _ := appTestDevice(t, a, mallory.ID, "Elsewhere")

	sa, sb, sm := sseOpen(t, f.srv.URL, f.tok), sseOpen(t, f.srv.URL, tokB), sseOpen(t, f.srv.URL, tokM)
	if sa.status != 200 || sb.status != 200 || sm.status != 200 {
		t.Fatalf("streams = %d %d %d", sa.status, sb.status, sm.status)
	}
	select {
	case <-sa.pings:
	case <-time.After(2 * time.Second):
		t.Fatal("no ping")
	}
	if n := a.app().hub.count(); n != 3 {
		t.Fatalf("streams open = %d", n)
	}

	// Device B creates a repository: A hears, B (its own action) and Mallory (no access) don't.
	api(t, a, "POST", "/repos", tokB, map[string]any{"owner": "zoe", "name": "ingle"}, 201)
	n := sa.next(t, "notify")
	wantKeys(t, "notify", n, "id", "kind", "thread", "title", "detail", "repo", "web_path", "moment", "at")
	if n["kind"] != "repo_created" || n["repo"] != "zoe/ingle" || n["thread"] != "yours" || n["web_path"] != "/zoe/ingle" ||
		!strings.HasPrefix(n["id"].(string), "n:") || n["moment"] != nil {
		t.Errorf("notify = %v", n)
	}
	if s := sa.next(t, "status"); len(list(s["changed"])) == 0 {
		t.Errorf("status = %v", s)
	}
	sb.quiet(t, "B heard its own action", true)
	sm.quiet(t, "Mallory heard about a private repository", true)

	// Moments: a push by device A (B hears "push", A doesn't), a failed run, a merge, GitHub, security.
	a.emit(ctx, Event{Kind: EventRepoPushed, Repo: demo, RepoID: demo.ID, URL: "/zoe/demo", Actor: Actor{Kind: ActorToken, Name: "device:MacBook", PersonID: f.owner.ID},
		Push: &EventPush{Ref: "refs/heads/feature/ssh", Old: strings.Repeat("0", 40), New: strings.Repeat("a", 40), Pusher: "token:device:MacBook"}})
	if n := sb.next(t, "notify"); n["kind"] != "push" || n["moment"] != "push" || n["title"] != "Pushed feature/ssh to demo" {
		t.Errorf("push notify = %v", n)
	}
	sa.quiet(t, "A heard its own push", true)
	sm.quiet(t, "Mallory heard a push", true)

	a.emit(ctx, Event{Kind: EventRunFailed, Repo: demo, RepoID: demo.ID, Number: 1, Title: "ci #1 failed", URL: "/zoe/demo/actions/runs/1",
		Detail: "failure on main", Actor: Actor{Kind: ActorSystem, Name: "actions"}})
	if n := sa.next(t, "notify"); n["kind"] != "run_failed" || n["moment"] != "fail" || n["web_path"] != "/zoe/demo/actions/runs/1" {
		t.Errorf("failed run notify = %v", n)
	}
	if s := sa.next(t, "status"); fmt.Sprint(s["changed"]) != "[needs running unread]" {
		t.Errorf("run status = %v", s)
	}
	a.emit(ctx, Event{Kind: EventPullMerged, Repo: demo, RepoID: demo.ID, Number: 7, Title: "Pages custom domains", URL: "/zoe/demo/pulls/7",
		Actor: Actor{Kind: ActorOwner, Name: "zoe", PersonID: f.owner.ID}})
	if n := sa.next(t, "notify"); n["kind"] != "pull_merged" || n["moment"] != "merge" || n["thread"] != "yours" {
		t.Errorf("merge notify = %v", n)
	}
	a.emit(ctx, Event{Kind: EventPullOpened, Repo: demo, RepoID: demo.ID, Number: 8, Title: "From GitHub", URL: "/zoe/demo/pulls/8",
		Actor: Actor{Kind: "import", Name: "github:dave"}})
	if n := sa.next(t, "notify"); n["moment"] != "github" || n["thread"] != "github" || n["kind"] != "pull_opened" {
		t.Errorf("github notify = %v", n)
	}
	a.emit(ctx, Event{Kind: EventSecretDetected, Repo: demo, RepoID: demo.ID, Title: "A secret was pushed", URL: "/zoe/demo/security",
		Actor: Actor{Kind: ActorSystem, Name: "security"}})
	if n := sa.next(t, "notify"); n["moment"] != "security" || n["kind"] != "security_alert" {
		t.Errorf("security notify = %v", n)
	}
	// Instance-wide events reach the owner only.
	a.emit(ctx, Event{Kind: EventBackupFailed, Title: "Backup failed", URL: "/settings/backups", Actor: Actor{Kind: ActorSystem, Name: "backups"}})
	if n := sa.next(t, "notify"); n["kind"] != "backup_failed" || n["repo"] != nil {
		t.Errorf("backup notify = %v", n)
	}
	sm.quiet(t, "Mallory heard instance events", true)

	// A revoked device's stream ends.
	if _, err := a.db.RevokeAppDevice(ctx, mallory.ID, 3); err != nil {
		t.Fatal(err)
	}
	a.app().hub.dropDevice(3)
	waitUntil(t, "revoked stream to close", func() bool { return a.app().hub.count() == 2 })

	// Disconnecting cleans up; a person can't hold more than a few streams.
	sb.cancel()
	waitUntil(t, "disconnect cleanup", func() bool { return a.app().hub.count() == 1 })
	for i := 1; i < appMaxStreams; i++ {
		if c := sseOpen(t, f.srv.URL, f.tok); c.status != 200 {
			t.Fatalf("stream %d = %d", i+1, c.status)
		}
	}
	if c := sseOpen(t, f.srv.URL, tokB); c.status != 429 {
		t.Errorf("stream over the cap = %d, want 429", c.status)
	}
}

// ---------- messages ----------

func TestAppMessages(t *testing.T) {
	f := newAppFix(t)
	a, ctx := f.a, context.Background()

	m := api(t, a, "GET", "/messages", f.tok, nil, 200)
	wantKeys(t, "messages", m, "tone", "moments", "plain", "rules")
	moments := obj(m["moments"])
	wantKeys(t, "moments", moments, "push", "clean", "pass", "fail", "merge", "first", "late", "mile", "agent")
	push := obj(moments["push"])
	if m["tone"] != "friendly" || len(list(push["ours"])) != 3 || list(push["ours"])[0] != "Home and dry. {branch} is safe in your studio." ||
		len(list(push["yours"])) != 0 || obj(m["plain"])["merge"] != "Merged into {base}." || len(obj(m["plain"])) != 9 {
		t.Errorf("defaults = %v", m)
	}
	if r := obj(m["rules"]); r["quiet_fullscreen"] != true || r["terminal"] != true || r["milestones"] != true || len(r) != 3 {
		t.Errorf("default rules = %v", r)
	}

	put := api(t, a, "PUT", "/messages", f.tok, map[string]any{"tone": "plain", "yours": map[string]any{"push": []string{"Shipped. Go outside.", "{count} on {repo}"}},
		"rules": map[string]any{"terminal": false}}, 200)
	wantKeys(t, "put", put, "tone", "moments", "plain", "rules")
	if put["tone"] != "plain" || fmt.Sprint(obj(obj(put["moments"])["push"])["yours"]) != "[Shipped. Go outside. {count} on {repo}]" ||
		obj(put["rules"])["terminal"] != false || obj(put["rules"])["milestones"] != true {
		t.Errorf("put = %v", put)
	}
	// Every device of the person says the same; a partial update keeps the rest.
	tokB, _ := appTestDevice(t, a, f.owner.ID, "Studio PC")
	api(t, a, "PUT", "/messages", tokB, map[string]any{"yours": map[string]any{"merge": []string{"In."}}}, 200)
	got := api(t, a, "GET", "/messages", f.tok, nil, 200)
	if got["tone"] != "plain" || len(list(obj(obj(got["moments"])["push"])["yours"])) != 2 || fmt.Sprint(obj(obj(got["moments"])["merge"])["yours"]) != "[In.]" ||
		obj(got["rules"])["terminal"] != false {
		t.Errorf("after partial update = %v", got)
	}
	// Replacing a moment's list with nothing empties it.
	if e := api(t, a, "PUT", "/messages", f.tok, map[string]any{"yours": map[string]any{"push": []string{}}}, 200); len(list(obj(obj(e["moments"])["push"])["yours"])) != 0 {
		t.Errorf("emptied push = %v", e)
	}

	many := make([]string, 21)
	for i := range many {
		many[i] = "x"
	}
	for what, c := range map[string]struct {
		body any
		code string
	}{
		"tone":        {map[string]any{"tone": "shouty"}, "invalid_tone"},
		"moment":      {map[string]any{"yours": map[string]any{"lunch": []string{"x"}}}, "invalid_moment"},
		"too many":    {map[string]any{"yours": map[string]any{"push": many}}, "too_many_messages"},
		"too long":    {map[string]any{"yours": map[string]any{"push": []string{strings.Repeat("é", 141)}}}, "message_too_long"},
		"placeholder": {map[string]any{"yours": map[string]any{"push": []string{"Hi {password}"}}}, "invalid_placeholder"},
		"markup":      {map[string]any{"yours": map[string]any{"push": []string{"<b>hi</b>"}}}, "invalid_message"},
		"newline":     {map[string]any{"yours": map[string]any{"push": []string{"two\nlines"}}}, "invalid_message"},
		"empty":       {map[string]any{"yours": map[string]any{"push": []string{""}}}, "empty_message"},
		"rule":        {map[string]any{"rules": map[string]any{"loud": true}}, "invalid_rule"},
	} {
		wantErr(t, what, api(t, a, "PUT", "/messages", f.tok, c.body, 422), c.code)
	}
	api(t, a, "PUT", "/messages", f.tok, map[string]any{"yours": map[string]any{"push": []string{strings.Repeat("é", 140)}}}, 200)
	if rec, _ := appReq(t, a, "PUT", appAPIPrefix+"/messages", f.tok, map[string]any{"tone": strings.Repeat("x", appMaxBody)}, nil); rec.Code != 413 {
		t.Errorf("huge body = %d, want 413", rec.Code)
	}
	if rec := do(t, a, "PUT", appAPIPrefix+"/messages", nil, map[string]string{"Authorization": "Bearer " + f.tok, "Content-Type": "application/json"}); rec.Code != 200 {
		t.Errorf("empty PUT = %d", rec.Code)
	}

	// Someone else has their own.
	mallory, _ := a.db.CreateOwner(ctx, "mallory", "", "user")
	tokM, _ := appTestDevice(t, a, mallory.ID, "Elsewhere")
	if mm := api(t, a, "GET", "/messages", tokM, nil, 200); mm["tone"] != "friendly" || len(list(obj(obj(mm["moments"])["merge"])["yours"])) != 0 {
		t.Errorf("another person's messages = %v", mm)
	}
}

// ---------- a second person ----------

func TestAppSecondPersonSeesNothing(t *testing.T) {
	f := newAppFix(t)
	a, ctx := f.a, context.Background()
	pullsCommit(t, f.work, ".atelier/workflows/review.md", agentReviewMD, "Add review agent")
	f.push(t, "main")
	f.settle()
	p := f.openPull(t, "feature/a", "Private work")
	runs := f.runs(t)
	jobs, _ := a.db.ActionsJobs(ctx, runs[len(runs)-1].ID)

	mallory, err := a.db.CreateOwner(ctx, "mallory", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := appTestDevice(t, a, mallory.ID, "Elsewhere")
	me := api(t, a, "GET", "/me", tok, nil, 200)
	if obj(me["person"])["login"] != "mallory" || len(list(me["accounts"])) != 1 || obj(list(me["accounts"])[0])["name"] != "mallory" {
		t.Errorf("me = %v", me)
	}
	st := api(t, a, "GET", "/status", tok, nil, 200)
	if len(list(st["needs"])) != 0 || len(list(st["running"])) != 0 || len(list(st["feed"])) != 0 || st["unread"] != float64(0) {
		t.Errorf("status = %v", st)
	}
	// The owner sees the same things.
	if own := api(t, a, "GET", "/status", f.tok, nil, 200); len(list(own["needs"])) == 0 || len(list(own["feed"])) == 0 {
		t.Fatalf("owner status is empty too: %v", own)
	}
	for path, key := range map[string]string{"/repos": "repos", "/pulls": "pulls", "/runs": "runs", "/agents/waiting": "jobs"} {
		if got := list(api(t, a, "GET", path, tok, nil, 200)[key]); len(got) != 0 {
			t.Errorf("%s = %v", path, got)
		}
	}
	pulls := "/repos/zoe/demo/pulls"
	api(t, a, "GET", pulls, tok, nil, 404)
	api(t, a, "GET", pulls+"/"+itoa(p.Number), tok, nil, 404)
	api(t, a, "POST", pulls+"/"+itoa(p.Number)+"/merge", tok, map[string]any{"when_green": true}, 404)
	api(t, a, "DELETE", pulls+"/"+itoa(p.Number)+"/merge", tok, nil, 404)
	api(t, a, "GET", "/jobs/"+itoa(jobs[0].ID)+"/log", tok, nil, 404)
	api(t, a, "POST", "/jobs/"+itoa(jobs[0].ID)+"/cancel", tok, nil, 404)
	api(t, a, "POST", "/agents/jobs/"+itoa(jobs[0].ID)+"/approve", tok, nil, 404)
	api(t, a, "GET", "/runs?repo=zoe/demo", tok, nil, 404)
	wantErr(t, "create under the owner", api(t, a, "POST", "/repos", tok, map[string]any{"owner": "zoe", "name": "mine"}, 403), "forbidden")
	wantErr(t, "remote", api(t, a, "GET", "/remote", tok, nil, 403), "forbidden")
	wantErr(t, "remote on", api(t, a, "POST", "/remote", tok, map[string]any{"on": true}, 403), "forbidden")
	wantErr(t, "remote who", api(t, a, "GET", "/remote/who", tok, nil, 403), "forbidden")
	if f.pull(t, p.Number).State != "open" {
		t.Error("the pull changed")
	}
	if _, waiting := f.autoMerge(p.Number); waiting {
		t.Error("someone else set a merge when green")
	}

	// A code the owner opened is the owner's: Mallory can't see or approve it.
	out := api(t, a, "POST", "/device/start", "", map[string]any{"name": "Owner laptop", "kind": "desktop"}, 200)
	dc, uc := out["device_code"].(string), out["user_code"].(string)
	ownerCK, malloryCK := sessionFor(t, a, f.owner.ID), sessionFor(t, a, mallory.ID)
	expectStatus(t, do(t, a, "GET", "/settings/devices/approve?code="+uc, nil, ownerCK), 200, "owner opens the code")
	expectStatus(t, do(t, a, "GET", "/settings/devices/approve?code="+uc, nil, malloryCK), 404, "mallory opens the code")
	expectStatus(t, do(t, a, "POST", "/settings/devices/approve", url.Values{"code": {uc}, "decision": {"approve"}}, malloryCK), 404, "mallory approves")
	expectStatus(t, do(t, a, "POST", "/settings/devices/approve", url.Values{"code": {uc}, "decision": {"deny"}}, malloryCK), 404, "mallory denies")
	if got := api(t, a, "POST", "/device/poll", "", map[string]any{"device_code": dc}, 202); got["status"] != "pending" {
		t.Errorf("poll after mallory tried = %v", got)
	}
	expectStatus(t, do(t, a, "POST", "/settings/devices/approve", url.Values{"code": {uc}, "decision": {"approve"}}, ownerCK), 303, "owner approves")
	if got := api(t, a, "POST", "/device/poll", "", map[string]any{"device_code": dc}, 200); obj(got["person"])["login"] != "zoe" {
		t.Errorf("the device signed in as %v", got["person"])
	}
	// Each person's device list is their own, and they can't revoke each other's.
	page := expectStatus(t, do(t, a, "GET", "/settings/devices", nil, malloryCK), 200, "mallory's devices")
	if strings.Contains(page, "MacBook") || strings.Contains(page, "Owner laptop") || !strings.Contains(page, "Elsewhere") {
		t.Error("mallory's device list shows the owner's devices")
	}
	expectStatus(t, do(t, a, "POST", "/settings/devices/revoke", url.Values{"id": {itoa(f.dev.ID)}}, malloryCK), 404, "mallory revokes the owner's device")
	api(t, a, "GET", "/me", f.tok, nil, 200)
}
