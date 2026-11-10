package web

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// pkClient drives the app like a browser on one origin, keeping cookies.
type pkClient struct {
	t       *testing.T
	a       *App
	host    string
	cookies map[string]string
}

func (c *pkClient) do(method, target string, body []byte, form url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	var req *http.Request
	switch {
	case form != nil:
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case body != nil:
		req = httptest.NewRequest(method, target, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	default:
		req = httptest.NewRequest(method, target, nil)
	}
	req.Host = c.host
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	rec := httptest.NewRecorder()
	c.a.ServeHTTP(rec, req)
	for _, ck := range rec.Result().Cookies() {
		if ck.MaxAge < 0 || ck.Value == "" {
			delete(c.cookies, ck.Name)
		} else {
			c.cookies[ck.Name] = ck.Value
		}
	}
	return rec
}

func (c *pkClient) json(target string, v any) (*httptest.ResponseRecorder, map[string]any) {
	c.t.Helper()
	var body []byte
	if v != nil {
		body, _ = json.Marshal(v)
	} else {
		body = []byte("{}")
	}
	rec := c.do("POST", target, body, nil)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// pkAuthenticator is a minimal software passkey (ES256, "none" attestation).
type pkAuthenticator struct {
	key      *ecdsa.PrivateKey
	credID   []byte
	userID   []byte
	counter  uint32
	rpIDHash [32]byte
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pkDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func (p *pkAuthenticator) create(t *testing.T, opts map[string]any, origin string) map[string]any {
	t.Helper()
	pk := opts["publicKey"].(map[string]any)
	rp := pk["rp"].(map[string]any)
	user := pk["user"].(map[string]any)
	p.userID = pkDecode(t, user["id"].(string))
	p.rpIDHash = sha256.Sum256([]byte(rp["id"].(string)))
	p.key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p.credID = make([]byte, 16)
	_, _ = rand.Read(p.credID)

	pub, _ := p.key.PublicKey.ECDH()
	raw := pub.Bytes() // 0x04 || X || Y
	cose, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         1, XCoord: raw[1:33], YCoord: raw[33:],
	})
	if err != nil {
		t.Fatal(err)
	}
	var authData bytes.Buffer
	authData.Write(p.rpIDHash[:])
	authData.WriteByte(0x45) // UP | UV | AT
	_ = binary.Write(&authData, binary.BigEndian, p.counter)
	authData.Write(make([]byte, 16)) // AAGUID
	_ = binary.Write(&authData, binary.BigEndian, uint16(len(p.credID)))
	authData.Write(p.credID)
	authData.Write(cose)

	att, err := webauthncbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": authData.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	cdj, _ := json.Marshal(map[string]any{"type": "webauthn.create", "challenge": pk["challenge"], "origin": origin, "crossOrigin": false})
	return map[string]any{
		"id": b64u(p.credID), "rawId": b64u(p.credID), "type": "public-key",
		"response":               map[string]any{"clientDataJSON": b64u(cdj), "attestationObject": b64u(att)},
		"clientExtensionResults": map[string]any{},
	}
}

func (p *pkAuthenticator) get(t *testing.T, opts map[string]any, origin string, userHandle []byte) map[string]any {
	t.Helper()
	pk := opts["publicKey"].(map[string]any)
	p.counter++
	var authData bytes.Buffer
	authData.Write(p.rpIDHash[:])
	authData.WriteByte(0x05) // UP | UV
	_ = binary.Write(&authData, binary.BigEndian, p.counter)
	cdj, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": pk["challenge"], "origin": origin, "crossOrigin": false})
	h := sha256.Sum256(cdj)
	signed := append(append([]byte{}, authData.Bytes()...), h[:]...)
	digest := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, p.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"id": b64u(p.credID), "rawId": b64u(p.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64u(cdj), "authenticatorData": b64u(authData.Bytes()),
			"signature": b64u(sig), "userHandle": b64u(userHandle),
		},
		"clientExtensionResults": map[string]any{},
	}
}

func TestPasskeyRegisterAndSignIn(t *testing.T) {
	a := newTestApp(t)
	hdr := sshkSignedIn(t, a)
	ctx := context.Background()
	sess := strings.TrimPrefix(hdr["Cookie"], sessionCookie+"=")
	const origin = "http://localhost:3000"
	owner := &pkClient{t: t, a: a, host: "localhost:3000", cookies: map[string]string{sessionCookie: sess}}

	rec := owner.do("GET", "/settings/passkeys", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data-passkey-register") || strings.Contains(rec.Body.String(), "Passkeys need HTTPS") {
		t.Fatalf("settings page on localhost = %d\n%s", rec.Code, rec.Body)
	}

	// Signed-out visitors can't register.
	anon := &pkClient{t: t, a: a, host: "localhost:3000", cookies: map[string]string{}}
	if rec, _ := anon.json("/settings/passkeys/register/begin", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous register = %d", rec.Code)
	}
	// Nothing to sign in with yet.
	if rec, _ := anon.json("/auth/passkey/begin", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("login with no passkeys = %d", rec.Code)
	}

	// Register.
	rec, opts := owner.json("/settings/passkeys/register/begin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("register begin = %d %s", rec.Code, rec.Body)
	}
	pkOpts := opts["publicKey"].(map[string]any)
	if pkOpts["rp"].(map[string]any)["id"] != "localhost" {
		t.Fatalf("rp = %v", pkOpts["rp"])
	}
	if sel := pkOpts["authenticatorSelection"].(map[string]any); sel["residentKey"] != "required" {
		t.Fatalf("authenticatorSelection = %v", sel)
	}
	dev := &pkAuthenticator{}
	cred := dev.create(t, opts, origin)

	// A response from the wrong origin is refused (and uses up the ceremony).
	bad := dev.create(t, opts, "http://evil.localhost")
	if rec, _ := owner.json("/settings/passkeys/register/finish", map[string]any{"name": "x", "credential": bad}); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong origin register = %d %s", rec.Code, rec.Body)
	}
	_, opts = owner.json("/settings/passkeys/register/begin", nil)
	cred = dev.create(t, opts, origin)
	rec, out := owner.json("/settings/passkeys/register/finish", map[string]any{"name": "  Test   key ", "credential": cred})
	if rec.Code != http.StatusOK || out["redirect"] != "/settings/passkeys?saved=1" {
		t.Fatalf("register finish = %d %s", rec.Code, rec.Body)
	}
	// Replaying the finish without a live ceremony fails.
	if rec, _ := owner.json("/settings/passkeys/register/finish", map[string]any{"credential": cred}); rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed register = %d", rec.Code)
	}
	pks, _ := a.db.Passkeys(ctx)
	if len(pks) != 1 || pks[0].Name != "Test key" || pks[0].AddedBy != "zoe" || !bytes.Equal(pks[0].CredentialID, dev.credID) {
		t.Fatalf("stored passkeys = %+v", pks)
	}

	// Sign in with it from a fresh browser.
	signin := anon.do("GET", "/signin", nil, nil)
	if !strings.Contains(signin.Body.String(), "Sign in with a passkey") || !strings.Contains(signin.Body.String(), "/static/passkeys.js") {
		t.Fatalf("sign-in page lacks the passkey button:\n%s", signin.Body)
	}
	rec, lopts := anon.json("/auth/passkey/begin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login begin = %d %s", rec.Code, rec.Body)
	}
	if ac, ok := lopts["publicKey"].(map[string]any)["allowCredentials"]; ok && ac != nil {
		t.Fatalf("discoverable login should not list credentials: %v", ac)
	}

	// Wrong user handle → refused.
	assertion := dev.get(t, lopts, origin, []byte("someone else"))
	if rec, _ := anon.json("/auth/passkey/finish", map[string]any{"credential": assertion}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong user handle = %d %s", rec.Code, rec.Body)
	}
	if _, ok := anon.cookies[sessionCookie]; ok {
		t.Fatal("session issued for a failed passkey sign-in")
	}

	_, lopts = anon.json("/auth/passkey/begin", nil)
	assertion = dev.get(t, lopts, origin, dev.userID)
	rec, out = anon.json("/auth/passkey/finish", map[string]any{"next": "//evil.com", "credential": assertion})
	if rec.Code != http.StatusOK || out["redirect"] != "/" {
		t.Fatalf("login finish = %d %s", rec.Code, rec.Body)
	}
	if tok := anon.cookies[sessionCookie]; tok == "" || !a.db.ValidSession(ctx, tok) {
		t.Fatal("no valid session after passkey sign-in")
	}
	pks, _ = a.db.Passkeys(ctx)
	if pks[0].LastUsedAt == nil {
		t.Error("last used not recorded")
	}

	// Rename and delete.
	id := strconv.FormatInt(pks[0].ID, 10)
	if rec := owner.do("POST", "/settings/passkeys/rename", nil, url.Values{"id": {id}, "name": {"Phone"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("rename = %d", rec.Code)
	}
	if rec := owner.do("POST", "/settings/passkeys/rename", nil, url.Values{"id": {id}, "name": {"  "}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank rename = %d", rec.Code)
	}
	if body := owner.do("GET", "/settings/passkeys", nil, nil).Body.String(); !strings.Contains(body, "Phone") {
		t.Error("renamed passkey not listed")
	}
	if rec := owner.do("POST", "/settings/passkeys/delete", nil, url.Values{"id": {id}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d", rec.Code)
	}
	if pks, _ := a.db.Passkeys(ctx); len(pks) != 0 {
		t.Fatalf("not deleted: %+v", pks)
	}
}

func TestPasskeysNeedHTTPS(t *testing.T) {
	a := newTestApp(t)
	hdr := sshkSignedIn(t, a)
	sess := strings.TrimPrefix(hdr["Cookie"], sessionCookie+"=")
	c := &pkClient{t: t, a: a, host: "192.168.1.20:3000", cookies: map[string]string{sessionCookie: sess}}

	rec := c.do("GET", "/settings/passkeys", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Passkeys need HTTPS") {
		t.Fatalf("plain-HTTP settings page = %d\n%s", rec.Code, rec.Body)
	}
	if rec, out := c.json("/settings/passkeys/register/begin", nil); rec.Code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "HTTPS") {
		t.Fatalf("register begin over HTTP = %d %v", rec.Code, out)
	}

	for _, tc := range []struct {
		public, host string
		trust        bool
		xfp          string
		rpID, origin string
		ok           bool
	}{
		{"https://git.example.com", "10.0.0.2:3000", false, "", "git.example.com", "https://git.example.com", true},
		{"https://git.example.com:8443", "x", false, "", "git.example.com", "https://git.example.com:8443", true},
		{"", "localhost:3000", false, "", "localhost", "http://localhost:3000", true},
		{"", "atelier.lan:3000", false, "", "atelier.lan", "http://atelier.lan:3000", false},
		{"", "atelier.example.com", true, "https", "atelier.example.com", "https://atelier.example.com", true},
		{"", "127.0.0.1:3000", false, "", "127.0.0.1", "http://127.0.0.1:3000", false},
	} {
		a.cfg.PublicURL, a.cfg.TrustProxy = tc.public, tc.trust
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = tc.host
		if tc.xfp != "" {
			r.Header.Set("X-Forwarded-Proto", tc.xfp)
		}
		rpID, origin, ok := a.passkeyRP(r)
		if rpID != tc.rpID || origin != tc.origin || ok != tc.ok {
			t.Errorf("passkeyRP(%q, %q) = %q %q %v; want %q %q %v", tc.public, tc.host, rpID, origin, ok, tc.rpID, tc.origin, tc.ok)
		}
	}
}
