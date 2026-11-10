package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atelier/internal/config"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// isolateGit keeps the user's git config and credential helpers out of the tests.
func isolateGit(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("SSH_ASKPASS", "")
	t.Setenv("GIT_AUTHOR_NAME", "Test Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test Author")
	t.Setenv("GIT_COMMITTER_EMAIL", "author@example.com")
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	isolateGit(t)
	dir := t.TempDir()
	cfg := config.Config{Listen: "127.0.0.1:0", DataDir: dir}
	db, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	g, err := gitrepo.New(cfg.RepoRoot())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cfg, db, g)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return a
}

// noRedirect is a client that returns redirects instead of following them.
func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func do(t *testing.T, a *App, method, target string, body url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(body.Encode())
	}
	req := httptest.NewRequest(method, target, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}

func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("no session cookie in response (status %d)", rec.Code)
	return ""
}

func TestSetupAndSignIn(t *testing.T) {
	a := newTestApp(t)
	ctx := context.Background()

	// Before setup, everything protected goes to /setup.
	rec := do(t, a, "GET", "/", nil, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("GET / before setup = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(t, a, "GET", "/signin", nil, nil); rec.Header().Get("Location") != "/setup" {
		t.Errorf("GET /signin before setup should redirect to /setup, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(t, a, "GET", "/setup", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET /setup = %d", rec.Code)
	}

	// Bad setup submissions re-render the form.
	for _, form := range []url.Values{
		{"username": {"signin"}, "password": {"longenough1"}, "password_confirm": {"longenough1"}},
		{"username": {"bad name"}, "password": {"longenough1"}, "password_confirm": {"longenough1"}},
		{"username": {"zoe"}, "password": {"short"}, "password_confirm": {"short"}},
		{"username": {"zoe"}, "password": {"longenough1"}, "password_confirm": {"longenough2"}},
	} {
		if rec := do(t, a, "POST", "/setup", form, nil); rec.Code != http.StatusOK {
			t.Errorf("bad setup %v = %d, want 200 re-render", form, rec.Code)
		}
	}
	if !a.needsSetup(ctx) {
		t.Fatal("invalid setup created an owner")
	}

	rec = do(t, a, "POST", "/setup", url.Values{
		"username": {"zoe"}, "display_name": {"Zoë"}, "password": {"correct horse"}, "password_confirm": {"correct horse"},
	}, map[string]string{"Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("POST /setup = %d %q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	sess := sessionFrom(t, rec)
	owner, err := a.db.PersonalOwner(ctx)
	if err != nil || owner.Name != "zoe" || owner.DisplayName != "Zoë" || owner.Kind != "user" {
		t.Fatalf("owner after setup = %+v, %v", owner, err)
	}
	if !a.db.ValidSession(ctx, sess) {
		t.Fatal("setup session not valid")
	}

	// Setup can't be run twice.
	if rec := do(t, a, "GET", "/setup", nil, nil); rec.Header().Get("Location") != "/" {
		t.Errorf("GET /setup after setup = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	do(t, a, "POST", "/setup", url.Values{"username": {"mallory"}, "password": {"0123456789"}, "password_confirm": {"0123456789"}}, nil)
	if owners, _ := a.db.Owners(ctx); len(owners) != 1 {
		t.Errorf("second setup created another owner: %+v", owners)
	}

	// Unauthenticated requests now go to /signin with next.
	rec = do(t, a, "GET", "/zoe?tab=x", nil, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/signin?next="+url.QueryEscape("/zoe?tab=x") {
		t.Errorf("GET /zoe signed out = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// With the session cookie, the home page renders.
	cookie := map[string]string{"Cookie": sessionCookie + "=" + sess}
	if rec := do(t, a, "GET", "/", nil, cookie); rec.Code != http.StatusOK {
		t.Errorf("GET / signed in = %d\n%s", rec.Code, rec.Body)
	}
	if rec := do(t, a, "GET", "/", nil, map[string]string{"Cookie": sessionCookie + "=forged"}); rec.Code != http.StatusSeeOther {
		t.Errorf("forged session accepted: %d", rec.Code)
	}

	// Password sign-in.
	if rec := do(t, a, "POST", "/signin", url.Values{"password": {"wrong password"}}, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d", rec.Code)
	}
	rec = do(t, a, "POST", "/signin", url.Values{"password": {"correct horse"}, "next": {"//evil.com"}}, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Errorf("sign-in with evil next = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	sess2 := sessionFrom(t, rec)
	rec = do(t, a, "POST", "/signin", url.Values{"password": {"correct horse"}, "next": {"/zoe"}}, nil)
	if rec.Header().Get("Location") != "/zoe" {
		t.Errorf("sign-in next = %q", rec.Header().Get("Location"))
	}

	// Sign out kills the session.
	rec = do(t, a, "POST", "/signout", nil, map[string]string{"Cookie": sessionCookie + "=" + sess2})
	if rec.Code != http.StatusSeeOther || a.db.ValidSession(ctx, sess2) {
		t.Errorf("signout = %d, session still valid = %v", rec.Code, a.db.ValidSession(ctx, sess2))
	}

	if rec := do(t, a, "GET", "/healthz", nil, nil); rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "/",
		"/":                  "/",
		"/zoe/demo":          "/zoe/demo",
		"/zoe?tab=1":         "/zoe?tab=1",
		"//evil.com":         "/",
		"//evil.com/path":    "/",
		"/\\evil.com":        "/",
		"https://evil.com":   "/",
		"http://evil.com/":   "/",
		"javascript:alert()": "/",
		"evil.com":           "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCrossSitePostBlocked(t *testing.T) {
	a := newTestApp(t)
	for _, hdr := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
		{"Origin": "https://evil.com"},
		{"Origin": "null"},
	} {
		for _, path := range []string{"/setup", "/signin", "/signout", "/settings/tokens"} {
			rec := do(t, a, "POST", path, url.Values{"username": {"x"}}, hdr)
			if rec.Code != http.StatusForbidden {
				t.Errorf("POST %s with %v = %d, want 403", path, hdr, rec.Code)
			}
		}
	}
	// Same-origin and header-less (non-browser) posts get through to the handler.
	for _, hdr := range []map[string]string{{"Sec-Fetch-Site": "same-origin"}, {"Origin": "http://example.com"}, nil} {
		if rec := do(t, a, "POST", "/signin", url.Values{}, hdr); rec.Code == http.StatusForbidden {
			t.Errorf("POST /signin with %v blocked", hdr)
		}
	}
	if !a.needsSetup(context.Background()) {
		t.Error("a blocked cross-site setup post still created an owner")
	}
}

// gitFixture sets up an owner, a private repo and tokens.
type gitFixture struct {
	a                 *App
	read, write, junk string
}

func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	a := newTestApp(t)
	ctx := context.Background()
	o, err := a.db.CreateOwner(ctx, "zoe", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.git.Init(ctx, "zoe", "demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.CreateRepo(ctx, o.ID, "demo", "", true); err != nil {
		t.Fatal(err)
	}
	read, _ := a.db.CreateToken(ctx, "read", tokenScopes["read"], time.Hour)
	write, _ := a.db.CreateToken(ctx, "write", tokenScopes["write"], time.Hour)
	return gitFixture{a: a, read: read, write: write, junk: store.TokenPrefix + strings.Repeat("0", 40)}
}

func TestGitSmartHTTPAuth(t *testing.T) {
	f := newGitFixture(t)
	a := f.a
	basic := func(user, pass string) map[string]string {
		req, _ := http.NewRequest("GET", "/", nil)
		req.SetBasicAuth(user, pass)
		return map[string]string{"Authorization": req.Header.Get("Authorization")}
	}
	const upload = "/zoe/demo.git/info/refs?service=git-upload-pack"
	const receive = "/zoe/demo.git/info/refs?service=git-receive-pack"

	rec := do(t, a, "GET", upload, nil, nil)
	if rec.Code != http.StatusUnauthorized || !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Basic ") {
		t.Errorf("anonymous private info/refs = %d, WWW-Authenticate %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if rec := do(t, a, "GET", upload, nil, basic("zoe", f.junk)); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown token = %d", rec.Code)
	}

	rec = do(t, a, "GET", upload, nil, basic("zoe", f.read))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-git-upload-pack-advertisement" {
		t.Errorf("read token upload-pack = %d %q\n%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	// Token in the username works too, and the URL is case-insensitive.
	if rec := do(t, a, "GET", "/ZOE/Demo.git/info/refs?service=git-upload-pack", nil, basic(f.read, "x")); rec.Code != http.StatusOK {
		t.Errorf("token-as-username, mixed case = %d", rec.Code)
	}

	if rec := do(t, a, "GET", receive, nil, basic("zoe", f.read)); rec.Code != http.StatusUnauthorized {
		t.Errorf("read token receive-pack = %d, want 401", rec.Code)
	}
	if rec := do(t, a, "POST", "/zoe/demo.git/git-receive-pack", nil, basic("zoe", f.read)); rec.Code != http.StatusUnauthorized {
		t.Errorf("read token POST git-receive-pack = %d, want 401", rec.Code)
	}
	rec = do(t, a, "GET", receive, nil, basic("zoe", f.write))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/x-git-receive-pack-advertisement" {
		t.Errorf("write token receive-pack = %d %q\n%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}

	if rec := do(t, a, "GET", "/zoe/nope.git/info/refs?service=git-upload-pack", nil, basic("zoe", f.write)); rec.Code != http.StatusNotFound {
		t.Errorf("missing repo = %d", rec.Code)
	}

	// Public repos can be read anonymously but never pushed to.
	ctx := context.Background()
	rp, _ := a.db.RepoByName(ctx, "zoe", "demo")
	_ = a.db.UpdateRepo(ctx, rp.ID, "", false)
	if rec := do(t, a, "GET", upload, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("anonymous public info/refs = %d", rec.Code)
	}
	if rec := do(t, a, "GET", receive, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous public receive-pack = %d", rec.Code)
	}
}

func TestGitAuthRateLimited(t *testing.T) {
	f := newGitFixture(t)
	var last int
	for i := 0; i < 10; i++ {
		last = do(t, f.a, "GET", "/zoe/demo.git/info/refs?service=git-upload-pack", nil, nil).Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after 10 failed attempts got %d, want 429", last)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitPushAndClone(t *testing.T) {
	f := newGitFixture(t)
	srv := httptest.NewServer(f.a)
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	remote := "http://zoe:" + f.write + "@" + u.Host + "/zoe/demo.git"

	work := t.TempDir()
	runGit(t, work, "init", "--initial-branch=main", ".")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A large, incompressible file makes git exceed http.postBuffer (1 MiB) and
	// send the pack with chunked transfer encoding, like real pushes do.
	big := make([]byte, 3<<20)
	rand.Read(big)
	if err := os.WriteFile(filepath.Join(work, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "First commit")
	runGit(t, work, "push", remote, "main")
	head := runGit(t, work, "rev-parse", "HEAD")

	g, err := f.a.git.Open("zoe", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if g.IsEmpty(context.Background()) {
		t.Fatal("bare repo still empty after push")
	}

	// Read-only token can't push.
	readRemote := "http://zoe:" + f.read + "@" + u.Host + "/zoe/demo.git"
	runGit(t, work, "commit", "--allow-empty", "-m", "Second")
	cmd := exec.Command("git", "push", readRemote, "main")
	cmd.Dir = work
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("push with a read token succeeded:\n%s", out)
	}

	// Clone with the read token.
	dst := filepath.Join(t.TempDir(), "clone")
	runGit(t, t.TempDir(), "clone", readRemote, dst)
	if got := runGit(t, dst, "rev-parse", "HEAD"); got != head {
		t.Errorf("cloned HEAD = %s, want %s", got, head)
	}
	got, err := os.ReadFile(filepath.Join(dst, "big.bin"))
	if err != nil || !bytes.Equal(got, big) {
		t.Errorf("big.bin did not round-trip (err %v)", err)
	}

	// The UI renders every repository view for the pushed repo.
	sess, _ := f.a.db.CreateSession(context.Background(), time.Hour)
	cookie := map[string]string{"Cookie": sessionCookie + "=" + sess}
	for _, p := range []string{
		"/", "/zoe", "/zoe/demo", "/zoe/demo/tree/main", "/zoe/demo/blob/main/README.md",
		"/zoe/demo/raw/main/README.md", "/zoe/demo/commits/main", "/zoe/demo/commit/" + head,
		"/zoe/demo/settings", "/settings/tokens", "/new", "/accounts/new", "/static/atelier.css",
	} {
		if rec := do(t, f.a, "GET", p, nil, cookie); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d\n%.500s", p, rec.Code, rec.Body)
		}
	}
	for _, p := range []string{"/zoe/demo/blob/main/nope.md", "/zoe/demo/tree/nope", "/zoe/nope", "/nobody"} {
		if rec := do(t, f.a, "GET", p, nil, cookie); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}

	// Clone without credentials fails cleanly (no prompt).
	cmd = exec.Command("git", "clone", srv.URL+"/zoe/demo.git", filepath.Join(t.TempDir(), "anon"))
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("anonymous clone of a private repo succeeded:\n%s", out)
	}
}
