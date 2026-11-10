package web

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshkSignedIn creates the owner and returns a session cookie header.
func sshkSignedIn(t *testing.T, a *App) map[string]string {
	t.Helper()
	ctx := context.Background()
	if _, err := a.db.CreateOwner(ctx, "zoe", "Zoë", "user"); err != nil {
		t.Fatal(err)
	}
	tok, err := a.db.CreateSession(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"Cookie": sessionCookie + "=" + tok, "Sec-Fetch-Site": "same-origin"}
}

func sshkPublicKey(t *testing.T, comment string) (string, string) {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + " " + comment, ssh.FingerprintSHA256(sp)
}

func TestSSHKeysSettings(t *testing.T) {
	a := newTestApp(t)
	hdr := sshkSignedIn(t, a)
	ctx := context.Background()

	if rec := do(t, a, "GET", "/settings/keys", nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("signed out GET /settings/keys = %d", rec.Code)
	}
	rec := do(t, a, "GET", "/settings/keys", nil, hdr)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No SSH keys yet") ||
		!strings.Contains(rec.Body.String(), `href="/settings/keys" aria-current="page"`) {
		t.Fatalf("GET /settings/keys = %d\n%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "SSH server is turned off") {
		t.Error("expected the SSH-off note with an empty SSHListen")
	}

	// Invalid keys are refused and the form keeps what was typed.
	for _, bad := range []string{"", "not a key", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"} {
		rec := do(t, a, "POST", "/settings/keys", url.Values{"name": {"bad"}, "key": {bad}}, hdr)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "flash-error") {
			t.Errorf("bad key %q = %d", bad, rec.Code)
		}
	}

	line, fp := sshkPublicKey(t, "zoe@laptop")
	rec = do(t, a, "POST", "/settings/keys", url.Values{"name": {""}, "key": {"\n  " + line + "\r\n"}}, hdr)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/keys?saved=1" {
		t.Fatalf("add key = %d %q\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	keys, _ := a.db.SSHKeys(ctx)
	if len(keys) != 1 || keys[0].Name != "zoe@laptop" || keys[0].Fingerprint != fp || keys[0].AddedBy != "zoe" ||
		strings.Contains(keys[0].PublicKey, "zoe@laptop") {
		t.Fatalf("stored keys = %+v", keys)
	}

	if rec := do(t, a, "POST", "/settings/keys", url.Values{"name": {"again"}, "key": {line}}, hdr); rec.Code != http.StatusConflict {
		t.Errorf("duplicate key = %d", rec.Code)
	}

	rec = do(t, a, "GET", "/settings/keys?saved=1", nil, hdr)
	body := rec.Body.String()
	if !strings.Contains(body, strings.ReplaceAll(fp, "+", "&#43;")) || !strings.Contains(body, "ssh-ed25519") || !strings.Contains(body, "Never used") || !strings.Contains(body, "flash-ok") {
		t.Errorf("list doesn't show the key:\n%s", body)
	}

	// Cross-site deletes are blocked; same-site deletes work.
	if rec := do(t, a, "POST", "/settings/keys/delete", url.Values{"id": {"1"}}, map[string]string{"Cookie": hdr["Cookie"], "Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site delete = %d", rec.Code)
	}
	if rec := do(t, a, "POST", "/settings/keys/delete", url.Values{"id": {"999"}}, hdr); rec.Code != http.StatusNotFound {
		t.Errorf("delete missing = %d", rec.Code)
	}
	if rec := do(t, a, "POST", "/settings/keys/delete", url.Values{"id": {strconv.FormatInt(keys[0].ID, 10)}}, hdr); rec.Code != http.StatusSeeOther {
		t.Errorf("delete = %d", rec.Code)
	}
	if keys, _ := a.db.SSHKeys(ctx); len(keys) != 0 {
		t.Errorf("key not deleted: %+v", keys)
	}
}

func TestSSHCloneInfo(t *testing.T) {
	a := newTestApp(t)
	hdr := sshkSignedIn(t, a)

	get := func() map[string]any {
		rec := do(t, a, "GET", "/settings/keys/clone.json", nil, hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("clone.json = %d", rec.Code)
		}
		var v map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := get(); v["enabled"] != false || v["base"] != "" {
		t.Errorf("SSH off: %v", v)
	}

	a.cfg.SSHListen = ":2222"
	if v := get(); v["base"] != "ssh://git@example.com:2222" {
		t.Errorf("request host: %v", v)
	}
	a.cfg.SSHPublicPort = 2223
	a.cfg.PublicURL = "https://git.example.com:8443"
	if v := get(); v["enabled"] != true || v["base"] != "ssh://git@git.example.com:2223" {
		t.Errorf("public URL: %v", v)
	}

	// The settings page shows how to connect.
	rec := do(t, a, "GET", "/settings/keys", nil, hdr)
	if !strings.Contains(rec.Body.String(), "ssh -T -p 2223 git@git.example.com") {
		t.Errorf("connect hint missing:\n%s", rec.Body)
	}

	// The clone box on a repository carries the HTTPS URL and loads the switch.
	ctx := context.Background()
	me, _ := a.db.PersonalOwner(ctx)
	r, err := a.db.CreateRepo(ctx, me.ID, "demo", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.git.Init(ctx, r.Owner, r.Name); err != nil {
		t.Fatal(err)
	}
	rec = do(t, a, "GET", "/zoe/demo", nil, hdr)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `data-clone-https="https://git.example.com:8443/zoe/demo.git"`) ||
		!strings.Contains(body, `/static/sshkeys.js`) || !strings.Contains(body, `data-clone-proto="ssh"`) {
		t.Errorf("repo page clone box:\n%s", body)
	}
	if rec := do(t, a, "GET", "/static/sshkeys.js", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("sshkeys.js = %d", rec.Code)
	}
}

func TestSSHHost(t *testing.T) {
	a := newTestApp(t)
	for _, tc := range []struct{ host, want string }{
		{"atelier.local:3000", "atelier.local"},
		{"[::1]:3000", "::1"},
		{"example.com", "example.com"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = tc.host
		if got := a.sshHost(r); got != tc.want {
			t.Errorf("sshHost(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
	a.cfg.SSHListen = ":2222"
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "[::1]:3000"
	if got := a.sshCloneBase(r); got != "ssh://git@[::1]:2222" {
		t.Errorf("IPv6 base = %q", got)
	}
}
