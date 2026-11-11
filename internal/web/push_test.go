package web

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/backup"
	"atelier/internal/config"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// ---------- a fake push service ----------

// fakeDevice is a browser's side of a subscription: its ECDH key and auth secret.
type fakeDevice struct {
	priv     *ecdh.PrivateKey
	auth     []byte
	endpoint string
}

func newFakeDevice(t *testing.T, host string) fakeDevice {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	return fakeDevice{priv: priv, auth: auth, endpoint: fmt.Sprintf("https://%s/3/device/%x", host, id)}
}

func (d fakeDevice) p256dh() string  { return b64url.EncodeToString(d.priv.PublicKey().Bytes()) }
func (d fakeDevice) authB64() string { return b64url.EncodeToString(d.auth) }

func (d fakeDevice) subscribeJSON() string {
	b, _ := json.Marshal(map[string]any{"endpoint": d.endpoint, "keys": map[string]string{"p256dh": d.p256dh(), "auth": d.authB64()}, "standalone": true})
	return string(b)
}

// testHKDF is RFC 5869 written out with HMAC, independent of crypto/hkdf.
func testHKDF(salt, ikm, info []byte, n int) []byte {
	ext := hmac.New(sha256.New, salt)
	ext.Write(ikm)
	prk := ext.Sum(nil)
	exp := hmac.New(sha256.New, prk)
	exp.Write(info)
	exp.Write([]byte{1})
	return exp.Sum(nil)[:n]
}

// decrypt opens an aes128gcm body (RFC 8188/8291) the way a browser would.
func (d fakeDevice) decrypt(body []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("short body")
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if rs < 18 || idlen != 65 || len(body) < 21+idlen {
		return nil, fmt.Errorf("bad header rs=%d idlen=%d", rs, idlen)
	}
	asPub, err := ecdh.P256().NewPublicKey(body[21 : 21+idlen])
	if err != nil {
		return nil, err
	}
	shared, err := d.priv.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), d.priv.PublicKey().Bytes()...), asPub.Bytes()...)
	ikm := testHKDF(d.auth, shared, info, 32)
	cek := testHKDF(salt, ikm, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := testHKDF(salt, ikm, []byte("Content-Encoding: nonce\x00"), 12)
	blk, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(blk)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 0x02 {
		return nil, errors.New("no last-record delimiter")
	}
	return plain[:len(plain)-1], nil
}

type fakePushReq struct {
	Endpoint string
	Header   http.Header
	Body     []byte
}

// fakePush stands in for the push service: it records requests and answers
// with status (201 unless set).
type fakePush struct {
	mu     sync.Mutex
	reqs   []fakePushReq
	status []int // per attempt; the last one repeats
}

func (f *fakePush) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.reqs = append(f.reqs, fakePushReq{Endpoint: r.URL.String(), Header: r.Header.Clone(), Body: b})
	code := http.StatusCreated
	if n := len(f.status); n > 0 {
		code = f.status[min(len(f.reqs)-1, n-1)]
	}
	f.mu.Unlock()
	rec := httptest.NewRecorder()
	rec.WriteHeader(code)
	return rec.Result(), nil
}

func (f *fakePush) got() []fakePushReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePushReq(nil), f.reqs...)
}

func installFakePush(a *App) *fakePush {
	f := &fakePush{}
	st := a.push()
	st.transport = f
	st.backoff = func(int) time.Duration { return 0 }
	return f
}

// verifyVAPID checks a VAPID Authorization header with the given public key
// and returns the JWT claims.
func verifyVAPID(t *testing.T, header string, pub []byte) map[string]any {
	t.Helper()
	if !strings.HasPrefix(header, "vapid t=") {
		t.Fatalf("Authorization = %.20q…, want vapid", header)
	}
	tok, k, ok := strings.Cut(strings.TrimPrefix(header, "vapid t="), ", k=")
	if !ok || k != b64url.EncodeToString(pub) {
		t.Fatalf("k= is not the stored public key")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	sig, err := b64url.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature encoding: %v len %d", err, len(sig))
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), pub) //nolint:staticcheck
	key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("VAPID signature does not verify with the stored key")
	}
	var hdr, claims map[string]any
	hb, _ := b64url.DecodeString(parts[0])
	cb, _ := b64url.DecodeString(parts[1])
	if json.Unmarshal(hb, &hdr) != nil || hdr["alg"] != "ES256" || hdr["typ"] != "JWT" {
		t.Fatalf("JWT header = %s", hb)
	}
	if json.Unmarshal(cb, &claims) != nil {
		t.Fatalf("JWT claims = %s", cb)
	}
	return claims
}

// ---------- fixture ----------

type pushFixture struct {
	a      *App
	fake   *fakePush
	zoe    store.Owner
	repo   store.Repo
	cookie map[string]string
}

func newPushFixture(t *testing.T) pushFixture {
	t.Helper()
	f := newGitFixture(t)
	ctx := context.Background()
	zoe, err := f.a.db.OwnerByName(ctx, "zoe")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := f.a.db.RepoByName(ctx, "zoe", "demo")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := f.a.db.CreateSessionFor(ctx, zoe.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return pushFixture{a: f.a, fake: installFakePush(f.a), zoe: zoe, repo: repo, cookie: map[string]string{"Cookie": sessionCookie + "=" + sess}}
}

func postJSON(t *testing.T, a *App, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return rec
}

func withHdr(base map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func (f pushFixture) subscribe(t *testing.T, d fakeDevice) int64 {
	t.Helper()
	rec := postJSON(t, f.a, "/settings/notifications/subscribe", d.subscribeJSON(), f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("subscribe = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		ID    int64
		Label string
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.ID == 0 || out.Label != "iPhone · Home Screen app" {
		t.Fatalf("subscribe answered %s", rec.Body)
	}
	return out.ID
}

func (f pushFixture) emit(kind string, actor Actor) {
	f.a.emit(context.Background(), Event{Kind: kind, Repo: f.repo, RepoID: f.repo.ID, Number: 7,
		Title: "Add the thing", Detail: "A longer description", URL: "/zoe/demo/pull/7", Actor: actor})
	f.a.pushWait()
}

var claudeToken = Actor{Kind: ActorToken, Name: "claude"}

// ---------- tests ----------

func TestPushVAPIDKeysPersist(t *testing.T) {
	isolateGit(t)
	dir := t.TempDir()
	open := func() *App {
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
			t.Fatal(err)
		}
		return a
	}
	a1 := open()
	k1, err := a1.vapid()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, pushDir, vapidFile)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("vapid.pem mode = %o, want 600", st.Mode().Perm())
	}
	if len(k1.pub) != 65 || k1.pub[0] != 4 {
		t.Errorf("public key isn't an uncompressed P-256 point")
	}
	// A restore writes files 0640; loading tightens the mode again.
	_ = os.Chmod(path, 0o640)
	a2 := open() // a restart: a new App over the same data dir
	k2, err := a2.vapid()
	if err != nil {
		t.Fatal(err)
	}
	if k1.PublicKey() != k2.PublicKey() {
		t.Error("VAPID key changed across a restart")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode after reload = %o", st.Mode().Perm())
	}
	if strings.Contains(fmt.Sprint(k1), "PRIVATE") || !strings.Contains(fmt.Sprint(k1), k1.PublicKey()) {
		t.Error("vapidKeys prints something other than its public key")
	}
	found := false
	for _, d := range backup.DataDirs() {
		found = found || d == pushDir
	}
	if !found {
		t.Error("the push dir isn't in backups")
	}
}

func TestPushEncryptionDecrypts(t *testing.T) {
	d := newFakeDevice(t, "web.push.apple.com")
	msg := []byte(`{"title":"hello","url":"/x"}`)
	body, err := pushEncrypt(msg, d.priv.PublicKey().Bytes(), d.auth)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.decrypt(body)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
	// Two encryptions of the same message differ (fresh key and salt).
	body2, _ := pushEncrypt(msg, d.priv.PublicKey().Bytes(), d.auth)
	if bytes.Equal(body, body2) {
		t.Error("encryption is deterministic")
	}
	// Another device can't read it.
	other := newFakeDevice(t, "web.push.apple.com")
	if _, err := other.decrypt(body); err == nil {
		t.Error("another device decrypted the payload")
	}
	if _, err := pushEncrypt(make([]byte, pushMaxPlaintext+1), d.priv.PublicKey().Bytes(), d.auth); err == nil {
		t.Error("an oversized payload was encrypted")
	}
}

func TestPushSubscribeDeliverUnsubscribe(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	d := newFakeDevice(t, "web.push.apple.com")
	id := f.subscribe(t, d)
	// Subscribing again refreshes the same row.
	if again := f.subscribe(t, d); again != id {
		t.Errorf("resubscribe made row %d, want %d", again, id)
	}

	// The settings page lists it by label and endpoint hash, never the endpoint.
	page := expectStatus(t, do(t, f.a, "GET", "/settings/notifications", nil, f.cookie), 200, "settings page")
	keys, _ := f.a.vapid()
	mustContain(t, page, "settings page", "iPhone · Home Screen app", pushEndpointHash(d.endpoint), `data-push-key="`+keys.PublicKey()+`"`,
		"Add to Home Screen", "iOS 16.4", `/static/push.js`, `href="/settings/notifications" aria-current="page"`)
	if strings.Contains(page, d.endpoint) || strings.Contains(page, d.authB64()) || strings.Contains(page, d.p256dh()) {
		t.Error("the page shows the endpoint or the device keys")
	}

	// A pull request opened by Claude's token reaches the phone.
	f.emit(EventPullOpened, claudeToken)
	reqs := f.fake.got()
	if len(reqs) != 1 {
		t.Fatalf("push requests = %d, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Endpoint != d.endpoint {
		t.Errorf("sent to %s", r.Endpoint)
	}
	claims := verifyVAPID(t, r.Header.Get("Authorization"), keys.pub)
	if claims["aud"] != "https://web.push.apple.com" || claims["sub"] == "" {
		t.Errorf("claims = %v", claims)
	}
	if exp, _ := claims["exp"].(float64); exp <= float64(time.Now().Unix()) || exp > float64(time.Now().Add(24*time.Hour).Unix()) {
		t.Errorf("exp = %v, want within 24h", claims["exp"])
	}
	if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") == "" || r.Header.Get("Urgency") != "normal" || len(r.Header.Get("Topic")) != 32 {
		t.Errorf("headers = %v", r.Header)
	}
	plain, err := d.decrypt(r.Body)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(plain, &p); err != nil {
		t.Fatalf("payload %s: %v", plain, err)
	}
	if p["title"] != "Add the thing" || p["url"] != "/zoe/demo/pull/7" || p["tag"] != "pull:/zoe/demo/pull/7" ||
		p["body"] != "zoe/demo: A longer description" || p["badge"] == nil {
		t.Errorf("payload = %s", plain)
	}
	if len(plain) > 512 {
		t.Errorf("payload is %d bytes; keep it short", len(plain))
	}
	sub, err := f.a.db.PushSubscription(ctx, f.zoe.ID, id)
	if err != nil || sub.LastSuccess == nil || sub.FailureCount != 0 {
		t.Errorf("after delivery: %+v %v", sub, err)
	}

	// Remove: gone from the list and no more pushes.
	expectStatus(t, do(t, f.a, "POST", "/settings/notifications/remove", url.Values{"id": {fmt.Sprint(id)}}, f.cookie), 303, "remove")
	if subs, _ := f.a.db.PushSubscriptions(ctx, f.zoe.ID); len(subs) != 0 {
		t.Errorf("devices after remove = %d", len(subs))
	}
	f.emit(EventPullOpened, claudeToken)
	if n := len(f.fake.got()); n != 1 {
		t.Errorf("pushed to a removed device (%d requests)", n)
	}
}

func TestPushRejectsBadSubscriptions(t *testing.T) {
	f := newPushFixture(t)
	good := newFakeDevice(t, "web.push.apple.com")
	for _, ep := range []string{
		"http://web.push.apple.com/x",                                        // not https
		"https://evil.example.com/x",                                         // not a push service
		"https://push.apple.com.evil.com/x",                                  // suffix trick
		"https://169.254.169.254/latest",                                     // an address
		"https://127.0.0.1/x",                                                // loopback
		"https://web.push.apple.com:8443/x",                                  // odd port
		"https://u:p@fcm.googleapis.com/fcm/x",                               // credentials
		"https://localhost/x",                                                // this machine
		"javascript:alert(1)",                                                // not a URL we'd ever call
		"https://fcm.googleapis.com/" + strings.Repeat("a", pushMaxEndpoint), // too long
	} {
		d := good
		d.endpoint = ep
		rec := postJSON(t, f.a, "/settings/notifications/subscribe", d.subscribeJSON(), f.cookie)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("subscribe %s = %d, want 400", ep, rec.Code)
		}
	}
	for _, ep := range []string{"https://web.push.apple.com/x", "https://api.push.apple.com/x", "https://fcm.googleapis.com/fcm/send/x",
		"https://wns2-par02p.notify.windows.com/w/?token=x", "https://updates.push.services.mozilla.com/wpush/v2/x", "https://push.services.mozilla.com/x"} {
		if _, err := f.a.push().checkEndpoint(ep); err != nil {
			t.Errorf("%s refused: %v", ep, err)
		}
	}
	// Bad keys.
	for _, body := range []string{
		`{"endpoint":"https://web.push.apple.com/x","keys":{"p256dh":"AAAA","auth":"` + good.authB64() + `"}}`,
		`{"endpoint":"https://web.push.apple.com/x","keys":{"p256dh":"` + good.p256dh() + `","auth":"AAAA"}}`,
		`{"endpoint":"https://web.push.apple.com/x","keys":{"p256dh":"` + b64url.EncodeToString(append([]byte{4}, make([]byte, 64)...)) + `","auth":"` + good.authB64() + `"}}`,
		`not json`,
	} {
		if rec := postJSON(t, f.a, "/settings/notifications/subscribe", body, f.cookie); rec.Code != http.StatusBadRequest {
			t.Errorf("subscribe %.60s = %d, want 400", body, rec.Code)
		}
	}
	// Signed out: refused.
	if rec := postJSON(t, f.a, "/settings/notifications/subscribe", good.subscribeJSON(), nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed-out subscribe = %d", rec.Code)
	}
	// Cross-site: refused.
	if rec := postJSON(t, f.a, "/settings/notifications/subscribe", good.subscribeJSON(), withHdr(f.cookie, "Sec-Fetch-Site", "cross-site")); rec.Code == http.StatusOK {
		t.Errorf("cross-site subscribe = %d", rec.Code)
	}
	if subs, _ := f.a.db.PushSubscriptions(context.Background(), f.zoe.ID); len(subs) != 0 {
		t.Errorf("%d bad subscriptions were saved", len(subs))
	}
}

func TestPushGoneRemovesSubscription(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	gone := newFakeDevice(t, "web.push.apple.com")
	idGone := f.subscribe(t, gone)
	f.fake.status = []int{http.StatusGone}
	f.emit(EventRunFailed, Actor{Kind: ActorSystem, Name: "actions"})
	if _, err := f.a.db.PushSubscription(ctx, f.zoe.ID, idGone); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("410 left the subscription: %v", err)
	}

	// 404 too.
	nf := newFakeDevice(t, "fcm.googleapis.com")
	idNF := f.subscribe(t, nf)
	f.fake.mu.Lock()
	f.fake.reqs, f.fake.status = nil, []int{http.StatusNotFound}
	f.fake.mu.Unlock()
	f.emit(EventRunFailed, Actor{Kind: ActorSystem, Name: "actions"})
	if _, err := f.a.db.PushSubscription(ctx, f.zoe.ID, idNF); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("404 left the subscription: %v", err)
	}

	// 503 is retried, then recorded as a failure; the device stays.
	d := newFakeDevice(t, "web.push.apple.com")
	id := f.subscribe(t, d)
	f.fake.mu.Lock()
	f.fake.reqs, f.fake.status = nil, []int{http.StatusServiceUnavailable}
	f.fake.mu.Unlock()
	f.emit(EventRunFailed, Actor{Kind: ActorSystem, Name: "actions"})
	if n := len(f.fake.got()); n != pushAttemptsBase {
		t.Errorf("503: %d attempts, want %d", n, pushAttemptsBase)
	}
	if r := f.fake.got(); len(r) > 0 && r[0].Header.Get("Urgency") != "high" {
		t.Errorf("a failure was sent with Urgency %q", r[0].Header.Get("Urgency"))
	}
	sub, err := f.a.db.PushSubscription(ctx, f.zoe.ID, id)
	if err != nil || sub.FailureCount != 1 || !strings.Contains(sub.LastError, "503") || strings.Contains(sub.LastError, "/3/device/") {
		t.Errorf("after 503s: %+v %v", sub, err)
	}
	// Then a success clears it.
	f.fake.mu.Lock()
	f.fake.reqs, f.fake.status = nil, []int{http.StatusServiceUnavailable, http.StatusCreated}
	f.fake.mu.Unlock()
	f.emit(EventRunFailed, Actor{Kind: ActorSystem, Name: "actions"})
	if sub, _ := f.a.db.PushSubscription(ctx, f.zoe.ID, id); sub.FailureCount != 0 || sub.LastSuccess == nil {
		t.Errorf("after a retry succeeded: %+v", sub)
	}
}

func TestPushSSRFGuard(t *testing.T) {
	a := newTestApp(t)
	st := a.push()
	// Push service names that resolve to internal addresses are never dialled.
	var dialled []string
	st.guard.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialled = append(dialled, addr)
		return nil, errors.New("test: not connecting")
	}
	for _, ip := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.10", "169.254.169.254", "::1", "fd00::1"} {
		st.guard.lookup = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr(ip)}, nil }
		_, retry, err := a.pushOnce(context.Background(), testSub(t, "https://web.push.apple.com/x"), []byte("{}"), "normal", "")
		if err == nil || retry || !strings.Contains(err.Error(), "blocked") {
			t.Errorf("%s: err=%v retry=%v, want blocked", ip, err, retry)
		}
	}
	if len(dialled) != 0 {
		t.Errorf("dialled %v", dialled)
	}
	// A public answer is dialled, at the address that was checked.
	st.guard.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("17.57.144.10")}, nil
	}
	_, retry, err := a.pushOnce(context.Background(), testSub(t, "https://web.push.apple.com/x"), []byte("{}"), "normal", "")
	if err == nil || !retry || len(dialled) != 1 || dialled[0] != "17.57.144.10:443" {
		t.Errorf("public address: err=%v retry=%v dialled=%v", err, retry, dialled)
	}
	// A stored endpoint that isn't on the allowlist (say, edited in the
	// database) is refused before any connection.
	dialled = nil
	if _, _, err := a.pushOnce(context.Background(), testSub(t, "https://attacker.example/x"), []byte("{}"), "normal", ""); err == nil || len(dialled) != 0 {
		t.Errorf("off-list endpoint: err=%v dialled=%v", err, dialled)
	}
	// No redirects are followed.
	f := &fakePush{status: []int{http.StatusFound}}
	st.transport = f
	if _, _, err := a.pushOnce(context.Background(), testSub(t, "https://web.push.apple.com/x"), []byte("{}"), "normal", ""); err == nil || len(f.got()) != 1 {
		t.Errorf("redirect: err=%v requests=%d", err, len(f.got()))
	}
}

func testSub(t *testing.T, endpoint string) store.PushSubscription {
	d := newFakeDevice(t, "web.push.apple.com")
	return store.PushSubscription{ID: 1, PersonID: 1, Endpoint: endpoint, P256dh: d.p256dh(), Auth: d.authB64()}
}

func TestPushPreferencesFilterKinds(t *testing.T) {
	f := newPushFixture(t)
	f.subscribe(t, newFakeDevice(t, "web.push.apple.com"))
	count := func() int { return len(f.fake.got()) }

	// Defaults: high-signal kinds only.
	for _, c := range []struct {
		kind string
		want bool
	}{
		{EventPullOpened, true}, {EventRunFailed, true}, {EventJobWaiting, true}, {EventSecretDetected, true},
		{EventIssueOpened, false}, {EventReleasePublished, false}, {EventTetherComment, false}, {EventRunCompleted, false},
	} {
		before := count()
		f.emit(c.kind, claudeToken)
		if got := count() > before; got != c.want {
			t.Errorf("default %s pushed = %v, want %v", c.kind, got, c.want)
		}
	}

	// Turn PRs off and issues on.
	form := url.Values{}
	for _, k := range pushKinds {
		if k.Key != "pulls" && (k.Default || k.Key == "mentions") {
			form.Set("kind_"+k.Key, "on")
		}
	}
	expectStatus(t, do(t, f.a, "POST", "/settings/notifications/preferences", form, f.cookie), 303, "save prefs")
	before := count()
	f.emit(EventPullOpened, claudeToken)
	if count() != before {
		t.Error("a PR was pushed after PRs were turned off")
	}
	f.emit(EventIssueOpened, claudeToken)
	if count() != before+1 {
		t.Error("an issue wasn't pushed after issues were turned on")
	}
	page := expectStatus(t, do(t, f.a, "GET", "/settings/notifications", nil, f.cookie), 200, "page")
	if !strings.Contains(page, `name="kind_mentions" value="on" checked`) || strings.Contains(page, `name="kind_pulls" value="on" checked`) {
		t.Error("the page doesn't show the saved preferences")
	}

	for kind, want := range map[string]string{
		"run.failed": "failures", "runner.offline": "failures", "backup.failed": "failures", "agent.failed": "failures",
		"job.waiting": "approvals", "agent.approval_requested": "approvals", "secret.detected": "security",
		"pull.merged": "pulls", "issue.commented": "mentions", "release.published": "releases", "package.published": "releases",
		"tether.comment": "tether", "run.completed": "", "secret.updated": "", "backup.succeeded": "",
	} {
		if got := pushCategory(kind, ""); got != want {
			t.Errorf("pushCategory(%s) = %q, want %q", kind, got, want)
		}
	}
	if pushCategory("issue.commented", "mention") != "mentions" || pushCategory("pull.opened", "review_requested") != "pulls" {
		t.Error("reasons aren't mapped")
	}
}

func TestPushNotForOwnActions(t *testing.T) {
	f := newPushFixture(t)
	f.subscribe(t, newFakeDevice(t, "web.push.apple.com"))
	f.emit(EventPullOpened, Actor{Kind: ActorOwner, Name: "zoe", PersonID: f.zoe.ID})
	if n := len(f.fake.got()); n != 0 {
		t.Errorf("zoe was pushed about her own pull request (%d)", n)
	}
	// Claude using zoe's token isn't zoe acting in person.
	f.emit(EventPullOpened, Actor{Kind: ActorToken, Name: "claude", PersonID: f.zoe.ID})
	if n := len(f.fake.got()); n != 1 {
		t.Errorf("token action pushes = %d, want 1", n)
	}
}

func TestPushDevicesArePrivate(t *testing.T) {
	f := newPushFixture(t)
	ctx := context.Background()
	d := newFakeDevice(t, "web.push.apple.com")
	id := f.subscribe(t, d)

	sam, err := f.a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := f.a.db.CreateSessionFor(ctx, sam.ID, time.Hour)
	samCookie := map[string]string{"Cookie": sessionCookie + "=" + sess}

	page := expectStatus(t, do(t, f.a, "GET", "/settings/notifications", nil, samCookie), 200, "sam's page")
	if strings.Contains(page, pushEndpointHash(d.endpoint)) || strings.Contains(page, fmt.Sprintf(`data-push-id="%d"`, id)) {
		t.Error("sam sees zoe's device")
	}
	mustContain(t, page, "sam's page", "No devices yet")

	if rec := do(t, f.a, "POST", "/settings/notifications/remove", url.Values{"id": {fmt.Sprint(id)}}, samCookie); rec.Code != http.StatusNotFound {
		t.Errorf("sam removing zoe's device = %d, want 404", rec.Code)
	}
	if rec := postJSON(t, f.a, "/settings/notifications/test", "", withHdr(samCookie, "Content-Type", "application/x-www-form-urlencoded")); rec.Code != http.StatusNotFound {
		t.Errorf("sam testing without id = %d", rec.Code)
	}
	if rec := do(t, f.a, "POST", "/settings/notifications/test", url.Values{"id": {fmt.Sprint(id)}}, samCookie); rec.Code != http.StatusNotFound {
		t.Errorf("sam testing zoe's device = %d, want 404", rec.Code)
	}
	if _, err := f.a.db.PushSubscription(ctx, f.zoe.ID, id); err != nil {
		t.Errorf("zoe's device is gone: %v", err)
	}
	if len(f.fake.got()) != 0 {
		t.Error("sam's test reached zoe's phone")
	}
	// Sam's own device doesn't get zoe's private-repo pushes.
	sd := newFakeDevice(t, "fcm.googleapis.com")
	rec := postJSON(t, f.a, "/settings/notifications/subscribe", sd.subscribeJSON(), samCookie)
	expectStatus(t, rec, 200, "sam subscribes")
	f.emit(EventPullOpened, claudeToken)
	for _, r := range f.fake.got() {
		if r.Endpoint == sd.endpoint {
			t.Error("sam got a push about zoe's private repository")
		}
	}
}

func TestPushTestButton(t *testing.T) {
	f := newPushFixture(t)
	d := newFakeDevice(t, "web.push.apple.com")
	id := f.subscribe(t, d)
	rec := postJSON(t, f.a, "/settings/notifications/test", "id="+fmt.Sprint(id), withHdr(f.cookie, "Content-Type", "application/x-www-form-urlencoded"))
	body := expectStatus(t, rec, 200, "test (JSON)")
	if !strings.Contains(body, `"ok":true`) {
		t.Errorf("test answered %s", body)
	}
	reqs := f.fake.got()
	if len(reqs) != 1 {
		t.Fatalf("test pushes = %d", len(reqs))
	}
	plain, err := d.decrypt(reqs[0].Body)
	if err != nil || !strings.Contains(string(plain), "Atelier test notification") || !strings.Contains(string(plain), `"url":"/settings/notifications"`) {
		t.Errorf("test payload %s, %v", plain, err)
	}
	// Without JS it's a form post with a redirect.
	expectStatus(t, do(t, f.a, "POST", "/settings/notifications/test", url.Values{"id": {fmt.Sprint(id)}}, f.cookie), 303, "test (form)")
	// A dead device says so and is removed.
	f.fake.status = []int{http.StatusGone}
	rec = postJSON(t, f.a, "/settings/notifications/test", "id="+fmt.Sprint(id), withHdr(f.cookie, "Content-Type", "application/x-www-form-urlencoded"))
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "removed") {
		t.Errorf("test on a gone device = %d %s", rec.Code, rec.Body)
	}
}

func TestServiceWorkerPushHandlers(t *testing.T) {
	b, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, s := range []string{"addEventListener('push'", "addEventListener('notificationclick'", "u.origin === self.location.origin",
		"setAppBadge", "showNotification(", "openWindow(target)"} {
		if !strings.Contains(src, s) {
			t.Errorf("sw.js lacks %q", s)
		}
	}
}
