package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"atelier/internal/connect"
	"atelier/internal/store"
)

// cnStudio is one Atelier instance on its own httptest server, with its own
// data directory, talking to the others over real HTTP (loopback allowed).
type cnStudio struct {
	a    *App
	srv  *httptest.Server
	ck   map[string]string
	me   store.Owner
	base string
}

func newCNStudio(t *testing.T, name string) *cnStudio {
	t.Helper()
	a := newTestApp(t)
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	a.cfg.PublicURL = srv.URL
	a.connect().allowLocal = true // two studios on one machine
	rec := do(t, a, "POST", "/setup", url.Values{"username": {name}, "password": {"correct horse battery"},
		"password_confirm": {"correct horse battery"}}, nil)
	s := &cnStudio{a: a, srv: srv, ck: map[string]string{"Cookie": sessionCookie + "=" + sessionFrom(t, rec)}, base: srv.URL}
	s.me, _ = a.db.PersonalOwner(context.Background())
	return s
}

func (s *cnStudio) post(t *testing.T, path string, form url.Values, want int) string {
	t.Helper()
	return expectStatus(t, do(t, s.a, "POST", path, form, s.ck), want, "POST "+path)
}

func (s *cnStudio) get(t *testing.T, path string, want int) string {
	t.Helper()
	return expectStatus(t, do(t, s.a, "GET", path, nil, s.ck), want, "GET "+path)
}

// flush waits for background work and sends everything due.
func (s *cnStudio) flush(t *testing.T) {
	t.Helper()
	s.a.connect().bg.Wait()
	for i := 0; i < 10 && s.a.connectDeliverDue(context.Background()) > 0; i++ {
	}
}

func (s *cnStudio) on(t *testing.T) {
	t.Helper()
	s.post(t, "/settings/connect", url.Values{"action": {"on"}}, http.StatusSeeOther)
}

func (s *cnStudio) host() string { return connect.HostOf(s.base) }

func (s *cnStudio) repo(t *testing.T, name string, private bool) store.Repo {
	t.Helper()
	ctx := context.Background()
	if err := s.a.git.Init(ctx, s.me.Name, name); err != nil {
		t.Fatal(err)
	}
	rp, err := s.a.db.CreateRepo(ctx, s.me.ID, name, "about "+name, private)
	if err != nil {
		t.Fatal(err)
	}
	return rp
}

// commit pushes a commit straight to a bare repository and returns the new head.
func (s *cnStudio) commit(t *testing.T, rp store.Repo, branch, file, msg string) (old, sha string) {
	t.Helper()
	work := t.TempDir()
	bare := s.a.git.Path(rp.Owner, rp.Name)
	runGit(t, work, "init", "-q", "-b", branch)
	if out, err := runGitErr(work, "fetch", "-q", bare, branch); err == nil {
		runGit(t, work, "reset", "-q", "--hard", "FETCH_HEAD")
		old = runGit(t, work, "rev-parse", "HEAD")
	} else if !strings.Contains(out, "couldn't find remote ref") {
		t.Logf("fetch: %s", out)
	}
	content := msg + "\n"
	if strings.HasSuffix(file, ".yml") {
		content = "name: ci\non: pull_request\njobs:\n  build:\n    runs-on: linux\n    steps:\n      - run: echo " + msg + "\n"
	}
	_ = os.MkdirAll(filepath.Dir(filepath.Join(work, file)), 0o755)
	if err := os.WriteFile(filepath.Join(work, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-q", "-m", msg)
	runGit(t, work, "push", "-q", bare, "HEAD:refs/heads/"+branch)
	return old, runGit(t, work, "rev-parse", "HEAD")
}

func (s *cnStudio) pushed(t *testing.T, rp store.Repo, branch, old, sha string) {
	t.Helper()
	if old == "" {
		old = zeroSHA
	}
	s.a.emit(context.Background(), Event{Kind: EventRepoPushed, Repo: rp, RepoID: rp.ID,
		Push: &EventPush{Ref: "refs/heads/" + branch, Old: old, New: sha}, Actor: Actor{Kind: ActorOwner, Name: rp.Owner}})
}

func (s *cnStudio) deliveries(t *testing.T) []store.ConnectDelivery {
	t.Helper()
	ds, err := s.a.db.ConnectDeliveryLog(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func TestConnectTwoStudios(t *testing.T) {
	ctx := context.Background()
	alice := newCNStudio(t, "alice")
	bob := newCNStudio(t, "bob")

	// Off by default: nothing is served.
	if rec := do(t, alice.a, "GET", "/.well-known/webfinger?resource=acct:alice@"+alice.host(), nil, nil); rec.Code != 404 {
		t.Fatalf("webfinger while off = %d", rec.Code)
	}
	if rec := do(t, alice.a, "GET", "/connect/users/alice", nil, nil); rec.Code != 404 {
		t.Fatalf("actor while off = %d", rec.Code)
	}
	alice.on(t)
	bob.on(t)

	// Identity: WebFinger and the actor document, fetched over HTTP.
	resp, err := http.Get(alice.srv.URL + "/.well-known/webfinger?resource=" + url.QueryEscape("acct:alice@"+alice.host()))
	if err != nil {
		t.Fatal(err)
	}
	var jrd connect.JRD
	_ = json.NewDecoder(resp.Body).Decode(&jrd)
	resp.Body.Close()
	if resp.StatusCode != 200 || jrd.SelfLink() != alice.base+"/connect/users/alice" {
		t.Fatalf("webfinger = %d %+v", resp.StatusCode, jrd)
	}
	body := expectStatus(t, do(t, alice.a, "GET", "/connect/users/alice", nil, nil), 200, "actor")
	mustContain(t, body, "actor doc", `"type":"Person"`, `"inbox":"`+alice.base+`/connect/users/alice/inbox"`,
		`"publicKeyPem":"-----BEGIN PUBLIC KEY-----`, `"manuallyApprovesFollowers":true`, `"sharedInbox"`)
	if st, err := os.Stat(filepath.Join(alice.a.cfg.DataDir, "connect", "studio-key.pem")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("studio key file: %v %v", st, err)
	}

	// Bob follows @alice@<host>. Alice approves by hand (the default).
	bob.post(t, "/settings/connect/follow", url.Values{"target": {"@alice@" + alice.host()}}, http.StatusSeeOther)
	bob.flush(t)
	pending, _ := alice.a.db.ConnectFollowers(ctx, "alice", "pending")
	if len(pending) != 1 || pending[0].Actor.Username != "bob" {
		t.Fatalf("alice's pending followers = %+v", pending)
	}
	mustContain(t, alice.get(t, "/settings/connect", 200), "settings page", "Follow requests", "@bob@"+bob.host())
	alice.post(t, "/settings/connect/followers/"+itoa(pending[0].ID)+"/accept", url.Values{}, http.StatusSeeOther)
	alice.flush(t)
	f, err := bob.a.db.ConnectFollowingOf(ctx, "bob", alice.base+"/connect/users/alice")
	if err != nil || f.State != "accepted" {
		t.Fatalf("bob's follow = %+v %v", f, err)
	}

	// Alice's activity: a public and a private repository, pushes and releases.
	hello := alice.repo(t, "hello", false)
	secret := alice.repo(t, "secret", true)
	alice.a.connectAnnounceRepos(ctx)
	old, sha := alice.commit(t, hello, "main", "README.md", "Say hello")
	alice.pushed(t, hello, "main", old, sha)
	old, sha = alice.commit(t, secret, "main", "plan.txt", "Top secret plan")
	alice.pushed(t, secret, "main", old, sha)
	alice.a.announceRelease(ctx, hello, store.Release{ID: 1, Tag: "v1.0", Title: "v1.0"}, Actor{Kind: ActorOwner, Name: "alice"})
	alice.a.announceRelease(ctx, secret, store.Release{ID: 2, Tag: "v9", Title: "v9"}, Actor{Kind: ActorOwner, Name: "alice"})
	alice.flush(t)

	for _, d := range alice.deliveries(t) {
		if strings.Contains(string(d.Body), "secret") || d.RepoID == secret.ID {
			t.Fatalf("a private repository's activity was queued: %s", d.Body)
		}
		if d.Status != "sent" {
			t.Errorf("delivery %s to %s = %s (%s)", d.Kind, d.Host, d.Status, d.LastError)
		}
	}
	feed, _ := bob.a.db.ConnectFeed(ctx, 50)
	kinds := map[string]bool{}
	for _, it := range feed {
		kinds[it.Kind] = true
		if strings.Contains(it.Text+it.Detail+it.ObjectName+it.Link, "secret") {
			t.Fatalf("private repository reached the feed: %+v", it)
		}
	}
	if !kinds["repo"] || !kinds["push"] || !kinds["release"] {
		t.Fatalf("bob's feed kinds = %v (%+v)", kinds, feed)
	}
	home := bob.get(t, "/", 200)
	mustContain(t, home, "bob's home", "via Connect", "cn-feed", "Say hello", "alice/hello", "released v1.0 of alice/hello", "/static/connect.css")
	mustNotContain(t, home, "bob's home", "Top secret plan")
	// Private repositories aren't served to other studios either.
	expectStatus(t, do(t, alice.a, "GET", "/connect/repos/alice/secret", nil, nil), 404, "private repo actor")
	expectStatus(t, do(t, alice.a, "GET", "/connect/git/alice/secret.git/info/refs?service=git-upload-pack", nil, nil), 404, "private clone")
	expectStatus(t, do(t, alice.a, "GET", "/connect/git/alice/hello.git/info/refs?service=git-receive-pack", nil, nil), 403, "push via connect")

	// A push queued while public, then made private before delivery: dropped.
	old, sha = alice.commit(t, hello, "main", "more.md", "Second thought")
	alice.pushed(t, hello, "main", old, sha)
	alice.a.connect().bg.Wait()
	if err := alice.a.db.UpdateRepo(ctx, hello.ID, hello.Description, true); err != nil {
		t.Fatal(err)
	}
	alice.flush(t)
	if d := alice.deliveries(t)[0]; d.Status != "dropped" || d.Kind != "Push" {
		t.Fatalf("push to a now-private repo = %s %s", d.Kind, d.Status)
	}
	_ = alice.a.db.UpdateRepo(ctx, hello.ID, hello.Description, false)

	// Profiles list followers and following.
	mustContain(t, alice.get(t, "/alice", 200), "alice's profile", "@alice@"+alice.host(), "<strong>1</strong> followers", "@bob@"+bob.host())
	mustContain(t, bob.get(t, "/bob", 200), "bob's profile", "<strong>1</strong> following")

	// Remote star: bob stars alice/hello; alice sees the count.
	helloURI := alice.base + "/connect/repos/alice/hello"
	bob.post(t, "/settings/connect/star", url.Values{"repo": {helloURI}}, http.StatusSeeOther)
	bob.flush(t)
	if n := alice.a.db.ConnectStarCount(ctx, hello.ID); n != 1 {
		t.Fatalf("stars on alice/hello = %d", n)
	}
	mustContain(t, alice.get(t, "/alice/hello", 200), "repo page", "★ 1 via Connect")
	bob.post(t, "/settings/connect/unstar", url.Values{"repo": {helloURI}}, http.StatusSeeOther)
	bob.flush(t)
	if n := alice.a.db.ConnectStarCount(ctx, hello.ID); n != 0 {
		t.Fatalf("stars after unstar = %d", n)
	}

	// Remote pull request: bob offers a branch of his public copy.
	fork := bob.repo(t, "hello-fork", false)
	runGit(t, bob.a.git.Path("bob", "hello-fork"), "fetch", "-q", alice.a.git.Path("alice", "hello"), "main:main")
	_, _ = bob.commit(t, fork, "idea", ".atelier/workflows/ci.yml", "An idea for alice")
	bob.post(t, "/settings/connect/offer", url.Values{"repo": {"bob/hello-fork"}, "branch": {"idea"}, "target": {helloURI},
		"title": {"Add an idea"}, "body": {"Please take **this**."}}, http.StatusSeeOther)
	bob.flush(t)
	alice.a.connect().bg.Wait()
	pulls, _ := alice.a.db.Pulls(ctx, hello.ID, "open")
	if len(pulls) != 1 {
		t.Fatalf("alice's pull requests = %+v", pulls)
	}
	p := pulls[0]
	if p.AuthorKind != "connect" || p.AuthorName != "connect:@bob@"+bob.host() || !strings.HasPrefix(p.Head, "connect/") ||
		p.Title != "Add an idea" || !strings.Contains(p.Body, "Offered through Atelier Connect") {
		t.Fatalf("pull = %+v", p)
	}
	g, _ := alice.a.git.Open("alice", "hello")
	if sha, err := g.BranchSHA(ctx, p.Head); err != nil || sha != p.HeadSHA {
		t.Fatalf("head branch %s = %s %v", p.Head, sha, err)
	}
	mustContain(t, alice.get(t, "/alice/hello/pulls/"+itoa(p.Number), 200), "remote PR page", "Add an idea", "@bob@"+bob.host())
	// Its Actions wait for alice's approval, like any outside pull request.
	alice.a.actions().bg.Wait()
	runs, err := alice.a.db.ActionsRuns(ctx, store.ActionsRunFilter{RepoID: hello.ID})
	if err != nil || len(runs) != 1 || runs[0].Status != store.RunWaiting {
		t.Fatalf("runs for the remote pull request = %+v %v", runs, err)
	}

	// Unfollow: alice loses the follower, bob's feed forgets alice.
	bob.post(t, "/settings/connect/unfollow", url.Values{"actor": {alice.base + "/connect/users/alice"}}, http.StatusSeeOther)
	bob.flush(t)
	if fs, _ := alice.a.db.ConnectFollowers(ctx, "alice", ""); len(fs) != 0 {
		t.Fatalf("followers after unfollow = %+v", fs)
	}
	if feed, _ := bob.a.db.ConnectFeed(ctx, 50); len(feed) != 0 {
		t.Fatalf("feed after unfollow = %d items", len(feed))
	}

	// Block: alice blocks bob's studio; bob's next follow is refused.
	alice.post(t, "/settings/connect/rules", url.Values{"host": {bob.host()}, "kind": {"block"}}, http.StatusSeeOther)
	bob.post(t, "/settings/connect/follow", url.Values{"target": {alice.base + "/connect/users/alice"}}, http.StatusSeeOther)
	bob.flush(t)
	if d := bob.deliveries(t)[0]; d.Kind != "Follow" || d.Status != "failed" || d.LastStatus != 403 {
		t.Fatalf("follow to a studio that blocked us = %+v", d)
	}
	if fs, _ := alice.a.db.ConnectFollowers(ctx, "alice", ""); len(fs) != 0 {
		t.Fatalf("blocked studio became a follower")
	}

	// Connect off: nothing served, nothing queued, nothing sent.
	alice.post(t, "/settings/connect/rules", url.Values{"host": {bob.host()}, "kind": {"remove"}}, http.StatusSeeOther)
	bob.post(t, "/settings/connect", url.Values{"action": {"off"}}, http.StatusSeeOther)
	expectStatus(t, do(t, bob.a, "GET", "/connect/users/bob", nil, nil), 404, "actor after off")
	if err := bob.a.connectQueue(ctx, connect.Activity(bob.base+"/connect/activities/x", "Follow", bob.base+"/connect/users/bob",
		"x", nil, nil, time.Time{}), []string{alice.base + "/connect/inbox"}, 0); err != errConnectOff {
		t.Fatalf("queue while off = %v", err)
	}
	before := len(bob.deliveries(t))
	_ = bob.a.db.QueueConnectDelivery(ctx, store.ConnectDelivery{Inbox: alice.base + "/connect/inbox", Host: alice.host(),
		ActivityID: "x", Kind: "Follow", Body: []byte("{}")})
	if n := bob.a.connectDeliverDue(ctx); n != 0 {
		t.Fatalf("delivered %d while off", n)
	}
	if ds := bob.deliveries(t); len(ds) != before+1 || ds[0].Status != "queued" {
		t.Fatalf("queued delivery changed while off: %+v", ds[0])
	}
	mustContain(t, bob.get(t, "/settings/connect", 200), "settings off", "Turn Connect on")
}

// signedTo builds a request to studio's shared inbox signed as from.
func signedTo(t *testing.T, from, to *cnStudio, body []byte, at time.Time) *http.Request {
	t.Helper()
	k, err := from.a.connectKey()
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", to.base+"/connect/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", connect.ContentType)
	if err := connect.SignRequest(req, from.base+"/connect/users/"+from.me.Name+"#main-key", k, body, at); err != nil {
		t.Fatal(err)
	}
	return req
}

func send(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestConnectInboxRefusesBadRequests(t *testing.T) {
	alice := newCNStudio(t, "alice")
	bob := newCNStudio(t, "bob")
	alice.on(t)
	bob.on(t)
	follow := func(id string) []byte {
		b, _ := json.Marshal(map[string]any{"@context": connect.Context, "id": bob.base + "/connect/activities/" + id, "type": "Follow",
			"actor": bob.base + "/connect/users/bob", "object": alice.base + "/connect/users/alice"})
		return b
	}
	now := time.Now()

	// Unsigned.
	req, _ := http.NewRequest("POST", alice.base+"/connect/inbox", bytes.NewReader(follow("u")))
	req.Header.Set("Content-Type", connect.ContentType)
	if code, _ := send(t, req); code != 401 {
		t.Errorf("unsigned = %d", code)
	}
	// Body changed after signing.
	req = signedTo(t, bob, alice, follow("t1"), now)
	req.Body = io.NopCloser(bytes.NewReader(follow("t2")))
	req.ContentLength = int64(len(follow("t2")))
	if code, msg := send(t, req); code != 401 || !strings.Contains(msg, "digest") {
		t.Errorf("tampered body = %d %s", code, msg)
	}
	// Signature bytes changed.
	req = signedTo(t, bob, alice, follow("t3"), now)
	req.Header.Set("Signature", strings.Replace(req.Header.Get("Signature"), `signature="`, `signature="AAAA`, 1))
	if code, _ := send(t, req); code != 401 {
		t.Errorf("bad signature = %d", code)
	}
	// Signed by bob's key but claiming another actor.
	other, _ := json.Marshal(map[string]any{"id": bob.base + "/connect/activities/o", "type": "Follow",
		"actor": bob.base + "/connect/users/mallory", "object": alice.base + "/connect/users/alice"})
	if code, _ := send(t, signedTo(t, bob, alice, other, now)); code != 403 {
		t.Errorf("actor isn't signer = %d", code)
	}
	// Too old (outside the clock window).
	if code, msg := send(t, signedTo(t, bob, alice, follow("old"), now.Add(-10*time.Minute))); code != 401 || !strings.Contains(msg, "clock") {
		t.Errorf("stale date = %d %s", code, msg)
	}
	// Valid, then replayed byte for byte.
	req = signedTo(t, bob, alice, follow("ok"), now)
	replay := req.Clone(context.Background())
	replay.Body = io.NopCloser(bytes.NewReader(follow("ok")))
	if code, msg := send(t, req); code != 202 {
		t.Fatalf("valid follow = %d %s", code, msg)
	}
	if code, msg := send(t, replay); code != 401 || !strings.Contains(msg, "replayed") {
		t.Errorf("replay = %d %s", code, msg)
	}
	// Oversized body.
	big := append(follow("big")[:0:0], `{"id":"`+bob.base+`/connect/activities/big","type":"Note","content":"`...)
	big = append(big, bytes.Repeat([]byte("x"), connect.MaxBody)...)
	big = append(big, `"}`...)
	if code, _ := send(t, signedTo(t, bob, alice, big, now)); code != 413 {
		t.Errorf("oversized = %d", code)
	}
	// Malformed JSON with a valid signature.
	if code, _ := send(t, signedTo(t, bob, alice, []byte(`{"id":1}{"x":2}`), now)); code != 400 {
		t.Errorf("malformed = %d", code)
	}
	// Feed items only come from people we follow.
	note, _ := json.Marshal(map[string]any{"id": bob.base + "/connect/activities/n1", "type": "Create", "actor": bob.base + "/connect/users/bob",
		"object": map[string]any{"id": bob.base + "/n/1", "type": "Note", "content": "spam"}})
	if code, _ := send(t, signedTo(t, bob, alice, note, now)); code != 202 {
		t.Errorf("note = %d", code)
	}
	if feed, _ := alice.a.db.ConnectFeed(context.Background(), 10); len(feed) != 0 {
		t.Errorf("stranger's note reached the feed: %+v", feed)
	}
	// Rate limit per remote studio.
	alice.a.connect().hostRate = newLimiter(1, time.Minute)
	if code, _ := send(t, signedTo(t, bob, alice, follow("r1"), now)); code != 202 {
		t.Errorf("first within limit = %d", code)
	}
	if code, _ := send(t, signedTo(t, bob, alice, follow("r2"), now)); code != 429 {
		t.Errorf("over the limit = %d", code)
	}
	alice.a.connect().hostRate = newLimiter(connectRatePerMin, time.Minute)
	// Allowlist only: bob isn't on it.
	alice.post(t, "/settings/connect", url.Values{"allowlist_only": {"1"}}, http.StatusSeeOther)
	if code, _ := send(t, signedTo(t, bob, alice, follow("al"), now)); code != 403 {
		t.Errorf("not on allowlist = %d", code)
	}
}

func TestConnectSecondPersonIsNotAnActor(t *testing.T) {
	ctx := context.Background()
	alice := newCNStudio(t, "alice")
	alice.on(t)
	eve, err := alice.a.db.CreateOwner(ctx, "eve", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, do(t, alice.a, "GET", "/connect/users/eve", nil, nil), 404, "eve's actor")
	expectStatus(t, do(t, alice.a, "GET", "/.well-known/webfinger?resource=acct:eve@"+alice.host(), nil, nil), 404, "eve's webfinger")
	tok, err := alice.a.db.CreateSessionFor(ctx, eve.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	eveCk := map[string]string{"Cookie": sessionCookie + "=" + tok}
	expectStatus(t, do(t, alice.a, "GET", "/settings/connect", nil, eveCk), 404, "eve's connect settings")
	expectStatus(t, do(t, alice.a, "POST", "/settings/connect/follow", url.Values{"target": {"@x@example.com"}}, eveCk), 404, "eve following")
	// A company account is an Organization.
	if _, err := alice.a.db.CreateOwner(ctx, "studio", "Studio Ltd", "company"); err != nil {
		t.Fatal(err)
	}
	mustContain(t, expectStatus(t, do(t, alice.a, "GET", "/connect/users/studio", nil, nil), 200, "company actor"), "company", `"type":"Organization"`)
}

func TestConnectNeedsPublicAddress(t *testing.T) {
	a := newTestApp(t)
	ck := featureSession(t, a)
	a.cfg.PublicURL = "http://atelier.lan"
	expectStatus(t, do(t, a, "POST", "/settings/connect", url.Values{"action": {"on"}}, ck), 303, "turn on")
	body := expectStatus(t, do(t, a, "GET", "/settings/connect", nil, ck), 200, "settings")
	mustContain(t, body, "LAN only", "only works on your own network", "Waiting for a public address")
	expectStatus(t, do(t, a, "GET", "/connect/users/demi", nil, nil), 404, "actor without address")
	if a.connectActive(context.Background()) != "" {
		t.Fatal("active without a public address")
	}
	a.cfg.PublicURL = "https://git.example.com"
	if a.connectActive(context.Background()) != "https://git.example.com" {
		t.Fatal("public https address not used")
	}
	if got := a.ConnectBadgeActor(context.Background(), mustPersonal(t, a).ID); got != "https://git.example.com/connect/users/demi" {
		t.Fatalf("badge actor = %q", got)
	}
}

func mustPersonal(t *testing.T, a *App) store.Owner {
	t.Helper()
	o, err := a.db.PersonalOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestConnectTunnelClass(t *testing.T) {
	for p, want := range map[string]string{
		"/.well-known/webfinger": classConnect, "/connect/inbox": classConnect,
		"/connect/git/a/b.git/info/refs": classConnect, "/a/b.git/info/refs": classGit, "/connectx": classWeb,
	} {
		r := httptest.NewRequest("GET", p, nil)
		if got := tunnelClass(r); got != want {
			t.Errorf("tunnelClass(%s) = %s, want %s", p, got, want)
		}
	}
}
