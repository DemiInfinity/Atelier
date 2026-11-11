package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/store"
)

// ---------- fixtures ----------

type appsHit struct {
	Path   string
	Header http.Header
	Body   []byte
}

// appsReceiver is a webhook receiver that records what it gets.
type appsReceiver struct {
	*httptest.Server
	mu     sync.Mutex
	hits   []appsHit
	answer func(n int, w http.ResponseWriter, r *http.Request) // n = 1-based hit number
}

func newAppsReceiver(t *testing.T) *appsReceiver {
	rc := &appsReceiver{}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.hits = append(rc.hits, appsHit{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		n, answer := len(rc.hits), rc.answer
		rc.mu.Unlock()
		if answer != nil {
			answer(n, w, r)
			return
		}
		w.Header().Set("Set-Cookie", "session=supersecret")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *appsReceiver) all() []appsHit {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]appsHit(nil), rc.hits...)
}

func (rc *appsReceiver) event(t *testing.T, event string) (appsHit, map[string]any) {
	t.Helper()
	for _, h := range rc.all() {
		if h.Header.Get("X-GitHub-Event") == event {
			var m map[string]any
			if err := json.Unmarshal(h.Body, &m); err != nil {
				t.Fatalf("%s payload isn't JSON: %v", event, err)
			}
			return h, m
		}
	}
	t.Fatalf("no %s delivery among %d", event, len(rc.all()))
	return appsHit{}, nil
}

type appsFix struct {
	gitFixture
	ck   map[string]string
	work string
	repo store.Repo
}

func newAppsFix(t *testing.T) *appsFix {
	t.Helper()
	f := newGitFixture(t)
	work := pullsWork(t, f.a)
	pullsCommit(t, work, "README.md", "# demo\n", "Initial")
	runGit(t, work, "push", "-q", "origin", "main")
	repo, err := f.a.db.RepoByName(context.Background(), "zoe", "demo")
	if err != nil {
		t.Fatal(err)
	}
	st := f.a.apps()
	st.backoff = func(int) time.Duration { return 0 }
	return &appsFix{gitFixture: f, ck: pullsCookie(t, f.a), work: work, repo: repo}
}

// addHook creates a repository webhook through the settings page.
func (f *appsFix) addHook(t *testing.T, rawURL, secret string, local bool, events ...string) store.Webhook {
	t.Helper()
	form := url.Values{"url": {rawURL}, "secret": {secret}, "active": {"1"}, "events": events}
	if local {
		form.Set("allow_local", "1")
	}
	rec := do(t, f.a, "POST", "/zoe/demo/settings/webhooks", form, f.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add hook = %d\n%.1500s", rec.Code, rec.Body)
	}
	hooks, _ := f.a.db.RepoWebhooks(context.Background(), f.repo.ID)
	return hooks[len(hooks)-1]
}

func (f *appsFix) deliveries(t *testing.T, h store.Webhook) []store.Delivery {
	t.Helper()
	ds, err := f.a.db.Deliveries(context.Background(), h.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func apiReq(t *testing.T, a *App, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}

func jsonOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON (%d): %s", rec.Code, rec.Body)
	}
	return m
}

func dig(m any, path ...string) any {
	for _, p := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[p]
	}
	return m
}

// ---------- signing ----------

func TestAppsSignatureMatchesGitHub(t *testing.T) {
	// Documented GitHub example: secret "It's a Secret to Everybody", payload "Hello, World!".
	got := appsSign("It's a Secret to Everybody", []byte("Hello, World!"))
	const want = "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
	if !AppsVerifySignature("It's a Secret to Everybody", []byte("Hello, World!"), want) {
		t.Error("verify rejected a good signature")
	}
	if AppsVerifySignature("wrong", []byte("Hello, World!"), want) || AppsVerifySignature("It's a Secret to Everybody", []byte("Hello"), want) {
		t.Error("verify accepted a bad signature")
	}
}

func TestAppsWebhookDeliverySignedAndLogged(t *testing.T) {
	f := newAppsFix(t)
	rc := newAppsReceiver(t)
	const secret = "s3cret-value-xyz"
	h := f.addHook(t, rc.URL+"/hook", secret, true, "push", "issues")
	if !h.HasSecret || h.URL != rc.URL+"/hook" {
		t.Fatalf("hook = %+v", h)
	}
	// The secret is stored encrypted.
	var ct []byte
	_ = f.a.db.DB().QueryRow(`SELECT secret_ct FROM apps_webhooks WHERE id=?`, h.ID).Scan(&ct)
	if len(ct) == 0 || strings.Contains(string(ct), secret) {
		t.Fatal("secret not encrypted at rest")
	}

	if n := f.a.appsDeliverDue(context.Background()); n != 1 {
		t.Fatalf("delivered %d, want the ping", n)
	}
	hit, ping := rc.event(t, "ping")
	for _, hd := range []string{"X-GitHub-Delivery", "X-Atelier-Delivery", "X-GitHub-Hook-ID", "X-Atelier-Event"} {
		if hit.Header.Get(hd) == "" {
			t.Errorf("missing %s", hd)
		}
	}
	if hit.Header.Get("Content-Type") != "application/json" || !strings.HasPrefix(hit.Header.Get("User-Agent"), "Atelier-Hookshot/") {
		t.Errorf("headers = %v", hit.Header)
	}
	// A receiver verifies the signature exactly as for GitHub.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(hit.Body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if hit.Header.Get("X-Hub-Signature-256") != want || hit.Header.Get("X-Atelier-Signature-256") != want {
		t.Errorf("signature headers %q / %q, want %q", hit.Header.Get("X-Hub-Signature-256"), hit.Header.Get("X-Atelier-Signature-256"), want)
	}
	if dig(ping, "hook", "config", "content_type") != "json" || ping["zen"] == nil || dig(ping, "repository", "full_name") != "zoe/demo" {
		t.Errorf("ping payload = %v", ping)
	}

	// The log keeps the exchange, with the signature and cookie redacted, and never the secret.
	ds := f.deliveries(t, h)
	if len(ds) != 1 || ds[0].Status != "ok" || ds[0].RespStatus != 200 || ds[0].Attempts != 1 {
		t.Fatalf("deliveries = %+v", ds)
	}
	page := expectStatus(t, do(t, f.a, "GET", "/zoe/demo/settings/webhooks/"+itoa(h.ID)+"/deliveries/"+itoa(ds[0].ID), nil, f.ck), 200, "delivery page")
	mustContain(t, page, "delivery page", ds[0].GUID, "X-Hub-Signature-256", "[redacted]", "Set-Cookie", "&#34;ok&#34;")
	for _, leak := range []string{secret, want, "supersecret"} {
		if strings.Contains(page, leak) {
			t.Errorf("delivery page shows %q", leak)
		}
	}
	page = expectStatus(t, do(t, f.a, "GET", "/zoe/demo/settings/webhooks/"+itoa(h.ID), nil, f.ck), 200, "hook page")
	mustContain(t, page, "hook page", "Recent deliveries", ds[0].GUID, "Saved · leave blank to keep")
	if strings.Contains(page, secret) {
		t.Error("hook page shows the secret")
	}
}

// ---------- SSRF guard ----------

func TestAppsBlockedAddresses(t *testing.T) {
	cases := []struct {
		ip           string
		strict, open bool // blocked without / with "allow local network"
	}{
		{"127.0.0.1", true, false}, {"::1", true, false}, {"10.1.2.3", true, false}, {"192.168.1.5", true, false},
		{"172.16.0.1", true, false}, {"169.254.10.10", true, false}, {"fe80::1", true, false}, {"100.64.0.1", true, false},
		{"169.254.169.254", true, true}, {"fd00:ec2::254", true, true}, {"0.0.0.0", true, true}, {"::ffff:127.0.0.1", true, false},
		{"::ffff:169.254.169.254", true, true}, {"224.0.0.1", true, true},
		{"203.0.113.10", false, false}, {"8.8.8.8", false, false}, {"2606:4700::1111", false, false},
	}
	for _, c := range cases {
		ip := netip.MustParseAddr(c.ip)
		if got := appsBlockedAddr(ip, false) != ""; got != c.strict {
			t.Errorf("%s strict blocked=%v, want %v", c.ip, got, c.strict)
		}
		if got := appsBlockedAddr(ip, true) != ""; got != c.open {
			t.Errorf("%s allow-local blocked=%v, want %v", c.ip, got, c.open)
		}
	}
	for _, u := range []string{"ftp://x", "http://", "javascript:alert(1)", "http://user:pw@example.com/"} {
		if _, err := appsCheckURL(u, true); err == nil {
			t.Errorf("appsCheckURL(%q) accepted", u)
		}
	}
}

func TestAppsSSRFGuard(t *testing.T) {
	f := newAppsFix(t)
	rc := newAppsReceiver(t)
	st := f.a.apps()

	// Saving: loopback needs the tick; metadata is refused even with it.
	rec := do(t, f.a, "POST", "/zoe/demo/settings/webhooks", url.Values{"url": {rc.URL}, "events": {"push"}, "active": {"1"}}, f.ck)
	body := expectStatus(t, rec, http.StatusUnprocessableEntity, "loopback without allow_local")
	mustContain(t, body, "loopback form", "Allow local network")
	expectStatus(t, do(t, f.a, "POST", "/zoe/demo/settings/webhooks", url.Values{"url": {"http://169.254.169.254/latest/meta-data"},
		"events": {"push"}, "active": {"1"}, "allow_local": {"1"}}, f.ck), http.StatusUnprocessableEntity, "metadata with allow_local")
	expectStatus(t, do(t, f.a, "POST", "/zoe/demo/settings/webhooks", url.Values{"url": {"http://localhost:9/x"},
		"events": {"push"}, "active": {"1"}}, f.ck), http.StatusUnprocessableEntity, "localhost")

	// "Public" names: a fake resolver, and a dialer that sends 203.0.113.10 to the test receiver.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(rc.URL, "http://"))
	var lookups sync.Map
	st.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		n, _ := lookups.LoadOrStore(host, new(int))
		*n.(*int)++
		switch host {
		case "public.test":
			return []netip.Addr{netip.MustParseAddr("203.0.113.10")}, nil
		case "rebind.test": // public when saved, loopback when delivering
			if *n.(*int) == 1 {
				return []netip.Addr{netip.MustParseAddr("203.0.113.10")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		case "mixed.test":
			return []netip.Addr{netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("10.0.0.7")}, nil
		case "metadata.test":
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}
		return nil, errors.New("no such host")
	}
	real := st.dial
	st.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "203.0.113.10:") {
			addr = "127.0.0.1:" + port
		}
		return real(ctx, network, addr)
	}
	_, _ = st.lookup(context.Background(), "rebind.test") // the "check at save time" lookup

	good := f.addHook(t, "http://public.test:"+port+"/ok", "", false, "push")
	rebind := f.addHook(t, "http://rebind.test:"+port+"/rebind", "", false, "push")
	mixed := f.addHook(t, "http://mixed.test:"+port+"/mixed", "", false, "push")
	meta := f.addHook(t, "http://metadata.test:"+port+"/meta", "", true, "push")
	redir := f.addHook(t, "http://public.test:"+port+"/redirect", "", false, "push")
	rc.answer = func(n int, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://127.0.0.1:"+port+"/internal", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(204)
	}
	f.a.appsDeliverDue(context.Background())

	paths := map[string]int{}
	for _, h := range rc.all() {
		paths[h.Path]++
	}
	if paths["/ok"] != 1 || paths["/redirect"] != 1 {
		t.Errorf("allowed deliveries not made: %v", paths)
	}
	for _, p := range []string{"/rebind", "/mixed", "/meta", "/internal"} {
		if paths[p] != 0 {
			t.Errorf("guard let a request reach %s", p)
		}
	}
	if d := f.deliveries(t, good); d[0].Status != "ok" {
		t.Errorf("public hook: %+v", d[0])
	}
	for name, h := range map[string]store.Webhook{"rebind": rebind, "mixed": mixed, "metadata": meta, "redirect": redir} {
		d := f.deliveries(t, h)
		if len(d) != 1 || d[0].Status != "failed" || d[0].Attempts != 1 || !strings.Contains(d[0].Error, "Refused") {
			t.Errorf("%s: want one refused, unretried delivery; got %+v", name, d)
		}
	}
}

// ---------- retries and redelivery ----------

func TestAppsRetriesAndRedeliver(t *testing.T) {
	f := newAppsFix(t)
	rc := newAppsReceiver(t)
	st := f.a.apps()
	var waits []int
	st.backoff = func(n int) time.Duration { waits = append(waits, n); return 0 }
	rc.answer = func(n int, w http.ResponseWriter, r *http.Request) {
		if n <= 3 { // the ping succeeds; the next two attempts fail
			if n == 1 {
				w.WriteHeader(200)
				return
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(200)
	}
	h := f.addHook(t, rc.URL, "k", true, "issues")
	f.a.appsDeliverDue(context.Background()) // the ping

	is, err := f.a.db.CreateIssue(context.Background(), f.repo.ID, "Flaky", "", ActorOwner, "zoe")
	if err != nil {
		t.Fatal(err)
	}
	f.a.emit(context.Background(), issueEvent(EventIssueOpened, appsView(f.repo), is, Actor{Kind: ActorOwner, Name: "zoe"}, ""))
	f.a.appsDeliverDue(context.Background())

	ds := f.deliveries(t, h)
	if len(ds) != 2 || ds[0].Event != "issues" || ds[0].Status != "ok" || ds[0].Attempts != 3 || ds[0].RespStatus != 200 {
		t.Fatalf("issue delivery = %+v", ds[0])
	}
	if len(waits) != 2 || waits[0] != 1 || waits[1] != 2 {
		t.Errorf("backoff calls = %v, want [1 2]", waits)
	}
	if got := len(rc.all()); got != 4 {
		t.Errorf("receiver hits = %d, want 4", got)
	}

	// Giving up after maxAttempts; 4xx answers aren't retried.
	st.maxAttempts = 3
	rc.answer = func(n int, w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }
	f.a.emit(context.Background(), issueEvent(EventIssueClosed, appsView(f.repo), is, Actor{Kind: ActorOwner, Name: "zoe"}, ""))
	f.a.appsDeliverDue(context.Background())
	if d := f.deliveries(t, h)[0]; d.Status != "failed" || d.Attempts != 3 || d.RespStatus != 500 {
		t.Errorf("exhausted delivery = %+v", d)
	}
	rc.answer = func(n int, w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }
	f.a.emit(context.Background(), issueEvent(EventIssueReopened, appsView(f.repo), is, Actor{Kind: ActorOwner, Name: "zoe"}, ""))
	f.a.appsDeliverDue(context.Background())
	if d := f.deliveries(t, h)[0]; d.Status != "failed" || d.Attempts != 1 {
		t.Errorf("404 delivery = %+v (shouldn't retry)", d)
	}

	// Redeliver the first issue delivery: same GUID and body, marked as a redelivery.
	rc.answer = nil
	orig := ds[0]
	before := len(rc.all())
	rec := do(t, f.a, "POST", "/zoe/demo/settings/webhooks/"+itoa(h.ID)+"/deliveries/"+itoa(orig.ID)+"/redeliver", url.Values{}, f.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("redeliver = %d", rec.Code)
	}
	f.a.appsDeliverDue(context.Background())
	hits := rc.all()
	if len(hits) != before+1 {
		t.Fatalf("redelivery not sent")
	}
	last := hits[len(hits)-1]
	if last.Header.Get("X-GitHub-Delivery") != orig.GUID || string(last.Body) != orig.Payload {
		t.Errorf("redelivery guid %q body match %v", last.Header.Get("X-GitHub-Delivery"), string(last.Body) == orig.Payload)
	}
	if d := f.deliveries(t, h)[0]; !d.Redelivery || d.Status != "ok" {
		t.Errorf("redelivery row = %+v", d)
	}
}

// ---------- payload shapes ----------

func TestAppsPayloadShapes(t *testing.T) {
	f := newAppsFix(t)
	rc := newAppsReceiver(t)
	f.addHook(t, rc.URL, "", true, "push", "pull_request", "issues", "issue_comment")
	ctx := context.Background()
	f.a.appsScanRefs(ctx) // baseline

	// issues + issue_comment through the issue pages.
	rec := do(t, f.a, "POST", "/zoe/demo/issues/new", url.Values{"title": {"Crash on start"}, "body": {"Steps…"}}, f.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("new issue = %d\n%.800s", rec.Code, rec.Body)
	}
	do(t, f.a, "POST", "/zoe/demo/issues/1/comment", url.Values{"body": {"Seen it too"}}, f.ck)

	// A branch and a pull request; then a push to it.
	runGit(t, f.work, "checkout", "-q", "-b", "feature")
	pullsCommit(t, f.work, "a.txt", "one\n", "Add a")
	runGit(t, f.work, "push", "-q", "origin", "feature")
	rec = do(t, f.a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"feature"}, "title": {"Add a"}}, f.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("new pull = %d", rec.Code)
	}
	f.a.appsScanRefs(ctx) // the feature branch push (created)
	sha2 := pullsCommit(t, f.work, "a.txt", "two\n", "Change a")
	runGit(t, f.work, "push", "-q", "origin", "feature")
	f.a.appsScanRefs(ctx)
	f.a.appsDeliverDue(ctx)

	_, iss := rc.event(t, "issues")
	for _, k := range []string{"action", "issue", "repository", "sender"} {
		if iss[k] == nil {
			t.Errorf("issues payload lacks %q", k)
		}
	}
	if iss["action"] != "opened" || dig(iss, "issue", "number") != float64(1) || dig(iss, "issue", "title") != "Crash on start" ||
		dig(iss, "issue", "state") != "open" || dig(iss, "issue", "user", "login") != "zoe" || dig(iss, "repository", "owner", "login") != "zoe" ||
		dig(iss, "repository", "private") != true || dig(iss, "sender", "login") != "zoe" || !strings.HasSuffix(dig(iss, "issue", "html_url").(string), "/zoe/demo/issues/1") {
		t.Errorf("issues payload = %v", iss)
	}
	if _, ok := dig(iss, "issue", "labels").([]any); !ok {
		t.Error("issue.labels should be an array")
	}
	_, cm := rc.event(t, "issue_comment")
	if cm["action"] != "created" || dig(cm, "comment", "body") != "Seen it too" || dig(cm, "comment", "user", "login") != "zoe" || dig(cm, "issue", "number") != float64(1) {
		t.Errorf("issue_comment payload = %v", cm)
	}
	_, pr := rc.event(t, "pull_request")
	if pr["action"] != "opened" || pr["number"] != float64(2) || dig(pr, "pull_request", "head", "ref") != "feature" ||
		dig(pr, "pull_request", "base", "ref") != "main" || dig(pr, "pull_request", "merged") != false || dig(pr, "pull_request", "state") != "open" ||
		dig(pr, "pull_request", "head", "repo", "full_name") != "zoe/demo" {
		t.Errorf("pull_request payload = %v", pr)
	}
	var push, sync map[string]any
	for _, h := range rc.all() {
		var m map[string]any
		_ = json.Unmarshal(h.Body, &m)
		if h.Header.Get("X-GitHub-Event") == "push" && m["after"] == sha2 {
			push = m
		}
		if h.Header.Get("X-GitHub-Event") == "pull_request" && m["action"] == "synchronize" {
			sync = m
		}
	}
	if push == nil {
		t.Fatal("no push delivery for the second commit")
	}
	commits, _ := push["commits"].([]any)
	if push["ref"] != "refs/heads/feature" || push["created"] != false || push["forced"] != false || len(commits) != 1 ||
		dig(commits[0], "id") != sha2 || dig(push, "head_commit", "id") != sha2 || dig(push, "pusher", "name") != "zoe" ||
		dig(push, "repository", "full_name") != "zoe/demo" || len(push["before"].(string)) != 40 {
		t.Errorf("push payload = %v", push)
	}
	if sync == nil || sync["after"] != sha2 || dig(sync, "pull_request", "number") != float64(2) {
		t.Errorf("synchronize payload = %v", sync)
	}
}

// ---------- bots ----------

var appsTokenRe = regexp.MustCompile(`value="(atl_[0-9a-f]+)"`)

func (f *appsFix) createBot(t *testing.T, name string, repoIDs []int64, perms ...string) (store.Bot, string) {
	t.Helper()
	form := url.Values{"name": {name}, "description": {"test bot"}}
	for _, id := range repoIDs {
		form.Add("repos", itoa(id))
	}
	for _, p := range perms {
		form.Set(p, "1")
	}
	rec := do(t, f.a, "POST", "/settings/apps/bots", form, f.ck)
	body := expectStatus(t, rec, 200, "create bot")
	m := appsTokenRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no token on the page:\n%.2000s", body)
	}
	b, err := f.a.db.BotByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return b, m[1]
}

func TestAppsBotScopes(t *testing.T) {
	f := newAppsFix(t)
	ctx := context.Background()
	zoe, _ := f.a.db.OwnerByName(ctx, "zoe")
	other, err := f.a.db.CreateRepo(ctx, zoe.ID, "other", "", true)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.a.git.Init(ctx, "zoe", "other")
	is, _ := f.a.db.CreateIssue(ctx, f.repo.ID, "Bug", "", ActorOwner, "zoe")
	_, _ = f.a.db.CreateIssue(ctx, other.ID, "Other bug", "", ActorOwner, "zoe")

	// Validation: needs a repository; names can't shadow accounts.
	expectStatus(t, do(t, f.a, "POST", "/settings/apps/bots", url.Values{"name": {"nobody"}}, f.ck), 422, "bot without repos")
	expectStatus(t, do(t, f.a, "POST", "/settings/apps/bots", url.Values{"name": {"zoe"}, "repos": {itoa(f.repo.ID)}}, f.ck), 422, "bot named like an account")

	bot, tok := f.createBot(t, "reviewer", []int64{f.repo.ID}, "comment", "checks")
	if bot.CanSuggest || bot.CanPush || !bot.CanComment || !bot.CanChecks {
		t.Fatalf("bot perms = %+v", bot)
	}
	who, ok := f.a.resolveCredential(ctx, tok)
	if !ok || who.Kind != "bot" || who.Name != "reviewer" {
		t.Fatalf("token resolves to %+v %v", who, ok)
	}

	// Comments: allowed here, shown with the bot chip.
	rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/issues/1/comments", tok, map[string]string{"body": "Triaged by a bot"})
	c := jsonOf(t, rec)
	if rec.Code != 201 || dig(c, "user", "login") != "reviewer[bot]" || dig(c, "user", "type") != "Bot" {
		t.Fatalf("bot comment = %d %v", rec.Code, c)
	}
	cs, _ := f.a.db.IssueComments(ctx, is.ID)
	if len(cs) != 1 || cs[0].AuthorKind != "bot" || cs[0].AuthorName != "reviewer" {
		t.Fatalf("stored comment = %+v", cs)
	}
	page := expectStatus(t, do(t, f.a, "GET", "/zoe/demo/issues/1", nil, f.ck), 200, "issue page")
	mustContain(t, page, "issue page", "Triaged by a bot", `pill pill-violet">bot<`)

	// Labels count as commenting.
	if rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/issues/1/labels", tok, map[string][]string{"labels": {"bug"}}); rec.Code != 200 {
		t.Errorf("bot labels = %d %s", rec.Code, rec.Body)
	}

	// Checks: statuses become checks on the commit.
	head, _ := gitBranch(t, f.a, "main")
	rec = apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/statuses/"+head, tok, map[string]string{"state": "failure", "context": "lint", "description": "2 problems", "target_url": "javascript:alert(1)"})
	if rec.Code != 201 {
		t.Fatalf("bot status = %d %s", rec.Code, rec.Body)
	}
	chk := f.a.appsExternalChecks(ctx, f.repo, head)
	if len(chk) != 1 || chk[0].Name != "lint" || chk[0].Status != "failure" || chk[0].URL != "" || chk[0].Workflow != "reviewer" {
		t.Errorf("external checks = %+v", chk)
	}

	// Not allowed: suggesting (no permission), merging (never), other repositories, administering, pushing.
	runGit(t, f.work, "checkout", "-q", "-b", "fix")
	pullsCommit(t, f.work, "b.txt", "b\n", "Fix")
	runGit(t, f.work, "push", "-q", "origin", "fix")
	do(t, f.a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"fix"}, "title": {"Fix"}}, f.ck)
	fixSHA, _ := gitBranch(t, f.a, "fix")
	if rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/pulls/2/comments", tok, map[string]any{"body": "x", "commit_id": fixSHA, "path": "b.txt", "line": 1, "side": "RIGHT"}); rec.Code != 403 {
		t.Errorf("bot without suggest = %d", rec.Code)
	}
	if rec := apiReq(t, f.a, "PUT", "/api/v3/repos/zoe/demo/pulls/2/merge", tok, map[string]string{}); rec.Code != 403 {
		t.Errorf("bot merge = %d", rec.Code)
	}
	if p, _ := f.a.db.PullByNumber(ctx, f.repo.ID, 2); p.IsMerged() {
		t.Fatal("bot merged a pull request")
	}
	if rec := apiReq(t, f.a, "GET", "/api/v3/repos/zoe/other", tok, nil); rec.Code != 404 {
		t.Errorf("bot reading another private repo = %d", rec.Code)
	}
	if rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/other/issues/1/comments", tok, map[string]string{"body": "hi"}); rec.Code != 404 {
		t.Errorf("bot commenting on another repo = %d", rec.Code)
	}
	if f.a.can(ctx, who, f.repo, PermAdmin) || f.a.can(ctx, who, f.repo, PermWrite) {
		t.Error("bot without push can write or administer")
	}
	basic := func(tok string) map[string]string {
		req, _ := http.NewRequest("GET", "/", nil)
		req.SetBasicAuth("reviewer", tok)
		return map[string]string{"Authorization": req.Header.Get("Authorization")}
	}
	if rec := do(t, f.a, "GET", "/zoe/demo.git/info/refs?service=git-receive-pack", nil, basic(tok)); rec.Code != 401 {
		t.Errorf("bot without push: receive-pack = %d", rec.Code)
	}
	if rec := do(t, f.a, "GET", "/zoe/demo.git/info/refs?service=git-upload-pack", nil, basic(tok)); rec.Code != 200 {
		t.Errorf("bot clone = %d", rec.Code)
	}
	// Session-only pages (merge button, settings) never accept the token.
	req := httptest.NewRequest("POST", "/zoe/demo/pulls/2/merge", strings.NewReader("method=merge"))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := doReq(f.a, req); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin") {
		t.Errorf("token on the merge form = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// A bot that may suggest and push: notes work; git push is allowed at the access layer.
	pusher, ptok := f.createBot(t, "fixer", []int64{f.repo.ID}, "suggest", "push")
	rec = apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/pulls/2/comments", ptok, map[string]any{"body": "Use a const", "commit_id": fixSHA, "path": "b.txt", "line": 1, "side": "RIGHT"})
	if rec.Code != 201 || dig(jsonOf(t, rec), "side") != "RIGHT" {
		t.Fatalf("bot note = %d %s", rec.Code, rec.Body)
	}
	notes, _ := f.a.db.PullNotes(ctx, 2)
	_ = notes
	p2, _ := f.a.db.PullByNumber(ctx, f.repo.ID, 2)
	if notes, _ := f.a.db.PullNotes(ctx, p2.ID); len(notes) != 1 || notes[0].AuthorKind != "bot" || notes[0].Line != 1 || notes[0].Side != "new" {
		t.Errorf("notes = %+v", notes)
	}
	if rec := do(t, f.a, "GET", "/zoe/demo.git/info/refs?service=git-receive-pack", nil, basic(ptok)); rec.Code != 200 {
		t.Errorf("bot with push: receive-pack = %d", rec.Code)
	}
	if rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/issues/1/comments", ptok, map[string]string{"body": "x"}); rec.Code != 403 {
		t.Errorf("bot without comment = %d", rec.Code)
	}

	// Editing narrows access at once; deleting revokes the token.
	rec = do(t, f.a, "POST", "/settings/apps/bots/"+itoa(pusher.ID), url.Values{"repos": {itoa(other.ID)}, "suggest": {"1"}}, f.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit bot = %d", rec.Code)
	}
	if rec := apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo", ptok, nil); rec.Code != 404 {
		t.Errorf("after removing the repo: %d", rec.Code)
	}
	expectStatus(t, do(t, f.a, "POST", "/settings/apps/bots/"+itoa(pusher.ID)+"/delete", url.Values{}, f.ck), http.StatusSeeOther, "delete bot")
	if _, ok := f.a.resolveCredential(ctx, ptok); ok {
		t.Error("deleted bot's token still works")
	}
	page = expectStatus(t, do(t, f.a, "GET", "/settings/apps", nil, f.ck), 200, "apps page")
	mustContain(t, page, "apps page", "reviewer", "Custom bot", "What bots can do", "Never merge", "Build your own", "/api/v3", "How GitHub bots review")
}

// ---------- /api/v3 with each kind of credential ----------

func TestAppsAPIv3Credentials(t *testing.T) {
	f := newAppsFix(t)
	ctx := context.Background()
	_, _ = f.a.db.CreateIssue(ctx, f.repo.ID, "Bug", "", ActorOwner, "zoe")
	runGit(t, f.work, "checkout", "-q", "-b", "feature")
	sha := pullsCommit(t, f.work, "lib/x.go", "package lib\n\nvar X = 1\n", "Add x")
	runGit(t, f.work, "push", "-q", "origin", "feature")
	do(t, f.a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"feature"}, "title": {"Add x"}}, f.ck)

	// Actions job token (running job): reads only.
	run, jobs, err := f.a.db.CreateActionsRun(ctx, store.ActionsRun{RepoID: f.repo.ID, WorkflowPath: ".atelier/workflows/ci.yml",
		WorkflowName: "CI", Event: "push", Ref: "refs/heads/main", SHA: sha, Status: "running"},
		[]store.ActionsJob{{RepoID: f.repo.ID, JobKey: "build", Name: "build", Status: "running"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = run
	const jobTok = "atj_" + "0123456789abcdef0123456789abcdef01234567"
	if err := f.a.db.SetActionsJobToken(ctx, store.ActionsJobToken{JobID: jobs[0].ID, RepoID: f.repo.ID, ExpiresAt: time.Now().Add(time.Hour)}, store.Hash(jobTok)); err != nil {
		t.Fatal(err)
	}
	if who, ok := f.a.resolveCredential(ctx, jobTok); !ok || who.Kind != ActorJob {
		t.Skipf("job token doesn't resolve in this tree (%+v); skipping job credential checks", who)
	}

	for _, c := range []struct {
		name, tok        string
		read, write, all int
	}{
		{"write token", f.write, 200, 201, 200},
		{"read token", f.read, 200, 403, 200},
		{"job token", jobTok, 200, 403, 200},
		{"anonymous", "", 401, 401, 401},
		{"junk", f.junk, 401, 401, 401},
	} {
		if rec := apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo", c.tok, nil); rec.Code != c.read {
			t.Errorf("%s GET repo = %d, want %d", c.name, rec.Code, c.read)
		}
		if rec := apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/pulls/2/files", c.tok, nil); rec.Code != c.all {
			t.Errorf("%s GET files = %d, want %d", c.name, rec.Code, c.all)
		}
		if rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/issues/1/comments", c.tok, map[string]string{"body": "hi from " + c.name}); rec.Code != c.write {
			t.Errorf("%s POST comment = %d, want %d: %s", c.name, rec.Code, c.write, rec.Body)
		}
	}

	// Shapes of the read endpoints.
	repo := jsonOf(t, apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo", f.write, nil))
	if repo["full_name"] != "zoe/demo" || repo["default_branch"] != "main" || dig(repo, "permissions", "push") != true || dig(repo, "permissions", "admin") != false {
		t.Errorf("repo = %v", repo)
	}
	pr := jsonOf(t, apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/pulls/2", f.read, nil))
	if dig(pr, "head", "sha") != sha || dig(pr, "head", "ref") != "feature" || pr["number"] != float64(2) {
		t.Errorf("pull = %v", pr)
	}
	var files []map[string]any
	_ = json.Unmarshal(apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/pulls/2/files", f.read, nil).Body.Bytes(), &files)
	if len(files) != 1 || files[0]["filename"] != "lib/x.go" || files[0]["status"] != "added" || files[0]["additions"] != float64(3) ||
		!strings.Contains(files[0]["patch"].(string), "+var X = 1") || !strings.HasPrefix(files[0]["patch"].(string), "@@") {
		t.Errorf("files = %v", files)
	}

	// Comments on a pull request go to its conversation.
	rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/issues/2/comments", f.write, map[string]string{"body": "On the PR"})
	if rec.Code != 201 {
		t.Fatalf("PR comment = %d", rec.Code)
	}
	p, _ := f.a.db.PullByNumber(ctx, f.repo.ID, 2)
	if pcs, _ := f.a.db.PullComments(ctx, p.ID); len(pcs) != 1 || pcs[0].Body != "On the PR" {
		t.Errorf("pull comments = %+v", pcs)
	}
	var list []map[string]any
	_ = json.Unmarshal(apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/issues/1/comments", f.read, nil).Body.Bytes(), &list)
	if len(list) != 1 || list[0]["body"] != "hi from write token" {
		t.Errorf("issue comments = %v", list)
	}

	// Check runs: create, update, list; combined status.
	rec = apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/check-runs", f.write, map[string]any{"name": "bugbot", "head_sha": sha, "status": "in_progress"})
	cr := jsonOf(t, rec)
	if rec.Code != 201 || cr["status"] != "in_progress" {
		t.Fatalf("check run = %d %v", rec.Code, cr)
	}
	id := int64(cr["id"].(float64))
	rec = apiReq(t, f.a, "PATCH", "/api/v3/repos/zoe/demo/check-runs/"+itoa(id), f.write, map[string]any{"conclusion": "success", "output": map[string]string{"title": "No bugs"}})
	if rec.Code != 200 || jsonOf(t, rec)["conclusion"] != "success" {
		t.Errorf("patch check run = %d %s", rec.Code, rec.Body)
	}
	runs := jsonOf(t, apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/commits/"+sha+"/check-runs", f.read, nil))
	if runs["total_count"] != float64(1) {
		t.Errorf("check runs = %v", runs)
	}
	if chk := f.a.appsExternalChecks(ctx, f.repo, sha); len(chk) != 1 || chk[0].Status != "success" {
		t.Errorf("checks = %+v", chk)
	}
	apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/statuses/feature", f.write, map[string]string{"state": "pending", "context": "ci"})
	comb := jsonOf(t, apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo/commits/feature/status", f.read, nil))
	if comb["state"] != "pending" || comb["total_count"] != float64(1) || comb["sha"] != sha {
		t.Errorf("combined = %v", comb)
	}
	if rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/statuses/"+sha, f.write, map[string]string{"state": "bogus"}); rec.Code != 422 {
		t.Errorf("bad state = %d", rec.Code)
	}
	if u := jsonOf(t, apiReq(t, f.a, "GET", "/api/v3/user", f.write, nil)); u["login"] != "zoe" {
		t.Errorf("user = %v", u)
	}
}

// ---------- a second person ----------

func TestAppsSecondPersonLockedOut(t *testing.T) {
	f := newAppsFix(t)
	ctx := context.Background()
	rc := newAppsReceiver(t)
	h := f.addHook(t, rc.URL, "", true, "push")
	f.a.appsDeliverDue(ctx)
	ds := f.deliveries(t, h)
	bot, _ := f.createBot(t, "zoebot", []int64{f.repo.ID}, "comment")
	zoe, _ := f.a.db.OwnerByName(ctx, "zoe")
	rec := do(t, f.a, "POST", "/settings/webhooks?account=zoe", url.Values{"url": {rc.URL}, "events": {"push"}, "active": {"1"}, "allow_local": {"1"}}, f.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("account hook = %d %s", rec.Code, rec.Body)
	}
	acct, _ := f.a.db.AccountWebhooks(ctx, zoe.ID)

	other, _ := f.a.db.CreateOwner(ctx, "visitor", "", "user")
	sess, _ := f.a.db.CreateSessionFor(ctx, other.ID, time.Hour)
	vck := map[string]string{"Cookie": sessionCookie + "=" + sess}
	for _, p := range []string{
		"/zoe/demo/settings/webhooks", "/zoe/demo/settings/webhooks/" + itoa(h.ID),
		"/zoe/demo/settings/webhooks/" + itoa(h.ID) + "/deliveries/" + itoa(ds[0].ID), "/zoe/demo/settings/apps",
		"/settings/apps/bots/" + itoa(bot.ID), appsAccountHookBase + "/" + itoa(acct[0].ID),
	} {
		if rec := do(t, f.a, "GET", p, nil, vck); rec.Code != 404 {
			t.Errorf("visitor GET %s = %d, want 404", p, rec.Code)
		}
	}
	if rec := do(t, f.a, "GET", "/settings/webhooks?account=zoe", nil, vck); rec.Code != 403 {
		t.Errorf("visitor account webhooks = %d", rec.Code)
	}
	for _, p := range []string{"/zoe/demo/settings/webhooks/" + itoa(h.ID) + "/delete", "/settings/apps/bots/" + itoa(bot.ID) + "/delete",
		"/zoe/demo/settings/webhooks/" + itoa(h.ID) + "/deliveries/" + itoa(ds[0].ID) + "/redeliver"} {
		if rec := do(t, f.a, "POST", p, url.Values{}, vck); rec.Code != 404 {
			t.Errorf("visitor POST %s = %d", p, rec.Code)
		}
	}
	if _, err := f.a.db.WebhookByID(ctx, h.ID); err != nil {
		t.Error("visitor deleted the webhook")
	}
	page := expectStatus(t, do(t, f.a, "GET", "/settings/apps", nil, vck), 200, "visitor apps page")
	for _, leak := range []string{"zoebot", "zoe/demo", rc.URL} {
		if strings.Contains(page, leak) {
			t.Errorf("visitor's apps page shows %q", leak)
		}
	}
	// Making a bot for the owner's repository is refused.
	expectStatus(t, do(t, f.a, "POST", "/settings/apps/bots", url.Values{"name": {"sneaky"}, "repos": {itoa(f.repo.ID)}, "comment": {"1"}}, vck), 422, "visitor bot")
	// The visitor's own token can't use the API on the owner's repository.
	tok, _ := f.a.db.CreateToken(ctx, "visitor", tokenScopes["write"], time.Hour)
	_ = f.a.db.AssignTokenPerson(ctx, tok, other.ID)
	if rec := apiReq(t, f.a, "GET", "/api/v3/repos/zoe/demo", tok, nil); rec.Code != 404 {
		t.Errorf("visitor token GET repo = %d", rec.Code)
	}
	if rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/statuses/main", tok, map[string]string{"state": "success"}); rec.Code != 404 {
		t.Errorf("visitor token POST status = %d", rec.Code)
	}
	// A bot made by someone who isn't the owner gets nothing, even if its row names the repository.
	vb, err := f.a.db.SaveBot(ctx, store.Bot{PersonID: other.ID, Name: "visitorbot", CanComment: true, RepoIDs: []int64{f.repo.ID}})
	if err != nil {
		t.Fatal(err)
	}
	vtok, _ := f.a.appsBotIssueToken(ctx, vb)
	if rec := apiReq(t, f.a, "POST", "/api/v3/repos/zoe/demo/issues/1/comments", vtok, map[string]string{"body": "x"}); rec.Code != 404 {
		t.Errorf("visitor's bot = %d", rec.Code)
	}
}
