package connect

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKeyCreatedPrivateAndReloaded(t *testing.T) {
	p := filepath.Join(t.TempDir(), KeyDir, KeyFile)
	k1, err := LoadOrCreateKey(p)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %v, %v; want 0600", st.Mode().Perm(), err)
	}
	if err := os.Chmod(p, 0o640); err != nil { // as a restore writes it
		t.Fatal(err)
	}
	k2, err := LoadOrCreateKey(p)
	if err != nil || !bytes.Equal(k1.Pub, k2.Pub) {
		t.Fatalf("reloaded key differs: %v", err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode not tightened back: %v", st.Mode().Perm())
	}
	pub, err := ParsePublicKeyPEM(k1.PublicPEM())
	if err != nil {
		t.Fatal(err)
	}
	if epk, ok := pub.(ed25519.PublicKey); !ok || !epk.Equal(k1.Pub) {
		t.Fatalf("unexpected key type %T", pub)
	}
}

func signedPost(t *testing.T, k *Key, body []byte, at time.Time) *http.Request {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://b.example/connect/inbox", bytes.NewReader(body))
	if err := SignRequest(req, "https://a.example/connect/users/alice#main-key", k, body, at); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestSignAndVerify(t *testing.T) {
	k, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "k.pem"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	body := []byte(`{"type":"Follow"}`)
	req := signedPost(t, k, body, now)
	p, err := ParseSignature(req.Header.Get("Signature"))
	if err != nil {
		t.Fatal(err)
	}
	if p.KeyID != "https://a.example/connect/users/alice#main-key" || p.Algorithm != "hs2019" ||
		strings.Join(p.Headers, " ") != "(request-target) host date digest" {
		t.Fatalf("params = %+v", p)
	}
	if err := VerifyRequest(req, body, k.Pub, p, now, 5*time.Minute); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Tampered body: the digest no longer matches.
	if err := VerifyRequest(req, []byte(`{"type":"Undo"}`), k.Pub, p, now, 5*time.Minute); !errors.Is(err, ErrSigDigest) {
		t.Errorf("tampered body = %v", err)
	}
	// Another key.
	other, _ := LoadOrCreateKey(filepath.Join(t.TempDir(), "o.pem"))
	if err := VerifyRequest(req, body, other.Pub, p, now, 5*time.Minute); !errors.Is(err, ErrSigInvalid) {
		t.Errorf("wrong key = %v", err)
	}
	// Outside the clock window.
	if err := VerifyRequest(req, body, k.Pub, p, now.Add(10*time.Minute), 5*time.Minute); !errors.Is(err, ErrSigClock) {
		t.Errorf("late = %v", err)
	}
	// A different path.
	req2 := req.Clone(context.Background())
	req2.URL.Path = "/connect/users/bob/inbox"
	if err := VerifyRequest(req2, body, k.Pub, p, now, 5*time.Minute); !errors.Is(err, ErrSigInvalid) {
		t.Errorf("moved request = %v", err)
	}
	// Not covering digest is refused for a POST.
	p2 := p
	p2.Headers = []string{"(request-target)", "host", "date"}
	if err := VerifyRequest(req, body, k.Pub, p2, now, 5*time.Minute); !errors.Is(err, ErrSigHeaders) {
		t.Errorf("no digest coverage = %v", err)
	}
}

func TestParseSignatureRejectsJunk(t *testing.T) {
	for _, h := range []string{"", `keyId="x"`, `signature="!!"`, `keyId="a",keyId="b",signature="AA=="`, `keyId="a,signature="AA=="`} {
		if _, err := ParseSignature(h); err == nil {
			t.Errorf("ParseSignature(%q) accepted", h)
		}
	}
}

func TestDecodeJSONStrict(t *testing.T) {
	if _, err := DecodeJSON([]byte(`{"a":1} {"b":2}`), MaxBody); err == nil {
		t.Error("trailing data accepted")
	}
	if _, err := DecodeJSON([]byte(`[1,2]`), MaxBody); err == nil {
		t.Error("array accepted")
	}
	if _, err := DecodeJSON([]byte(`{"a":"`+strings.Repeat("x", 100)+`"}`), 50); err == nil {
		t.Error("oversized accepted")
	}
	d, err := DecodeJSON([]byte(`{"actor":{"id":"https://a.example/u"},"to":["x",{"id":"y"}],"object":"https://b"}`), MaxBody)
	if err != nil || d.ID("actor") != "https://a.example/u" || strings.Join(d.IDs("to"), ",") != "x,y" || d.ID("object") != "https://b" {
		t.Fatalf("accessors: %v %v", d, err)
	}
}

func TestParseActorChecksHosts(t *testing.T) {
	doc := func(inbox string) Doc {
		d, _ := DecodeJSON([]byte(`{"id":"https://a.example/u/al","type":"Person","inbox":"`+inbox+`",
			"publicKey":{"id":"https://a.example/u/al#main-key","owner":"https://a.example/u/al","publicKeyPem":"x"},
			"name":"<b>Al</b> &amp; co"}`), MaxBody)
		return d
	}
	ra, err := ParseActor(doc("https://a.example/u/al/inbox"), "https://a.example/u/al")
	if err != nil || ra.Name != "Al & co" || ra.KeyID == "" {
		t.Fatalf("actor = %+v, %v", ra, err)
	}
	if _, err := ParseActor(doc("https://evil.example/inbox"), "https://a.example/u/al"); err == nil {
		t.Error("inbox on another host accepted")
	}
	if _, err := ParseActor(doc("https://a.example/u/al/inbox"), "https://a.example/u/other"); err == nil {
		t.Error("id mismatch accepted")
	}
}

func TestHandlesAndURLGuard(t *testing.T) {
	for in, want := range map[string]string{"@demi@studio.example": "demi studio.example", "acct:demi@Studio.Example": "demi studio.example",
		"demi@127.0.0.1:3990": "demi 127.0.0.1:3990"} {
		u, h, err := ParseHandle(in)
		if err != nil || u+" "+h != want {
			t.Errorf("ParseHandle(%q) = %q %q %v", in, u, h, err)
		}
	}
	for _, bad := range []string{"demi", "@demi", "de mi@x.example", "demi@-x", "demi@a..b"} {
		if _, _, err := ParseHandle(bad); err == nil {
			t.Errorf("ParseHandle(%q) accepted", bad)
		}
	}
	c := &Client{}
	for _, bad := range []string{"http://studio.example/x", "https://127.0.0.1/x", "https://localhost/x", "https://10.0.0.5/",
		"https://169.254.169.254/latest", "https://user:pw@studio.example/", "ftp://studio.example/"} {
		if _, err := c.CheckURL(bad); err == nil {
			t.Errorf("CheckURL(%q) allowed", bad)
		}
	}
	if _, err := c.CheckURL("https://studio.example/connect/users/demi"); err != nil {
		t.Errorf("public https refused: %v", err)
	}
	local := &Client{AllowLocal: true}
	if _, err := local.CheckURL("http://127.0.0.1:3990/x"); err != nil {
		t.Errorf("allow-local refused loopback: %v", err)
	}
	if _, err := local.CheckURL("http://169.254.169.254/"); err == nil {
		t.Error("metadata address allowed even with allow-local")
	}
	// The dial-time guard refuses names that resolve to private addresses.
	_, err := c.Get(context.Background(), "https://localtest.me/x", ContentType)
	if err == nil {
		t.Error("expected an error fetching a name that resolves to loopback")
	}
	if PlainText(`<script>alert(1)</script>hi &lt;b&gt;`) != "alert(1) hi <b>" {
		t.Errorf("PlainText = %q", PlainText(`<script>alert(1)</script>hi &lt;b&gt;`))
	}
}
