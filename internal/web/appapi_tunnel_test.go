//go:build !windows

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ---------- remote access ----------

// remoteAPI sends an app API request through the tunnel.
func (f *tunnelFix) remoteAPI(t *testing.T, method, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return appReq(t, f.a.tunnel(), method, appAPIPrefix+path, token, body, map[string]string{
		"Host": tunnelTestHost, "CF-Connecting-IP": "203.0.113.7", "CF-IPCountry": "PT"})
}

func TestAppRemoteAndTunnel(t *testing.T) {
	f := newTunnelFix(t)
	a, ctx := f.a, context.Background()
	tok, _ := appTestDevice(t, a, f.owner.ID, "MacBook")
	stream := httptest.NewServer(a)
	t.Cleanup(stream.Close)
	sse := sseOpen(t, stream.URL, tok)

	off := api(t, a, "GET", "/remote", tok, nil, 200)
	wantKeys(t, "remote", off, "on", "mode", "address", "exposed", "connected")
	if off["on"] != false || off["mode"] != "quick" || off["address"] != nil || fmt.Sprint(off["exposed"]) != "[web git]" || off["connected"] != float64(0) {
		t.Errorf("remote off = %v", off)
	}
	if got := api(t, a, "GET", "/remote/who", tok, nil, 200); len(list(got["sessions"])) != 0 {
		t.Errorf("who while off = %v", got)
	}
	wantErr(t, "link while off", api(t, a, "POST", "/remote/link", tok, nil, 409), "remote_off")
	wantErr(t, "no switch", api(t, a, "POST", "/remote", tok, map[string]any{}, 422), "invalid_request")

	// On, from home.
	if on := api(t, a, "POST", "/remote", tok, map[string]any{"on": true}, 200); on["on"] != true {
		t.Fatalf("remote on = %v", on)
	}
	if ev := sse.next(t, "remote"); ev["on"] != true {
		t.Errorf("remote event = %v", ev)
	}
	waitUntil(t, "tunnel address", func() bool { return a.tunnel().remoteHost() == tunnelTestHost })
	if on := api(t, a, "GET", "/remote", tok, nil, 200); on["address"] != "https://"+tunnelTestHost || on["on"] != true {
		t.Errorf("remote on = %v", on)
	}

	// The app API goes through the tunnel only when the owner exposes the API class.
	if rec, _ := f.remoteAPI(t, "GET", "/me", tok, nil); rec.Code != 404 {
		t.Errorf("app API through the tunnel, not exposed = %d, want 404", rec.Code)
	}
	c, _ := a.db.TunnelConfig(ctx)
	c.ExposeAPI = true
	if err := a.db.SaveTunnelConfig(ctx, nil, c, nil, "test"); err != nil {
		t.Fatal(err)
	}
	a.tunnel().apply(ctx)
	rec, me := f.remoteAPI(t, "GET", "/me", tok, nil)
	if rec.Code != 200 || obj(me["studio"])["url"] != "https://"+tunnelTestHost {
		t.Fatalf("app API through the tunnel = %d %v", rec.Code, me)
	}

	// A device can start signing in from outside, but is approved at home only.
	rec, start := f.remoteAPI(t, "POST", "/device/start", "", map[string]any{"name": "Phone", "kind": "cli"})
	if rec.Code != 200 || !strings.HasPrefix(start["verify_url"].(string), "https://atelier.infinityhomelab.lan/settings/devices/approve?code=") {
		t.Fatalf("remote start = %d %v", rec.Code, start)
	}
	uc, dc := start["user_code"].(string), start["device_code"].(string)
	rs := map[string]string{"Cookie": sessionCookie + "=" + f.remoteSession(t)}
	if rec := f.remote(t, "GET", "/settings/devices/approve?code="+uc, nil, rs); rec.Code != 403 || !strings.Contains(rec.Body.String(), "at home") {
		t.Errorf("approval page through the tunnel = %d", rec.Code)
	}
	if rec := f.remote(t, "POST", "/settings/devices/approve", url.Values{"code": {uc}, "decision": {"approve"}}, rs); rec.Code != 403 {
		t.Errorf("approving through the tunnel = %d, want 403", rec.Code)
	}
	if rec, out := f.remoteAPI(t, "POST", "/device/poll", "", map[string]any{"device_code": dc}); rec.Code != 202 || out["status"] != "pending" {
		t.Errorf("poll after a tunnelled approval = %d %v", rec.Code, out)
	}
	page := f.lan(t, "GET", "/settings/devices/approve?code="+uc, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Phone wants to sign in") || !strings.Contains(page.Body.String(), "203.0.113.7") {
		t.Errorf("approval page at home = %d", page.Code)
	}

	// Who's connected.
	who := api(t, a, "GET", "/remote/who", tok, nil, 200)
	s := find(who["sessions"], "ip", "203.0.113.7")
	wantKeys(t, "remote session", s, "ip", "country", "signed_in", "class", "since")
	if s["country"] != "PT" || s["signed_in"] != true || s["since"] == nil {
		t.Errorf("session = %v", s)
	}
	if on := api(t, a, "GET", "/remote", tok, nil, 200); on["connected"] != float64(1) || fmt.Sprint(on["exposed"]) != "[web git api]" {
		t.Errorf("remote = %v", on)
	}

	// The one-time passkey link: only from home.
	link := api(t, a, "POST", "/remote/link", tok, nil, 201)
	if !strings.HasPrefix(link["url"].(string), "https://"+tunnelTestHost+"/settings/remote/enroll?code=") || link["expires_in"] != float64(900) {
		t.Errorf("link = %v", link)
	}
	if rec, out := f.remoteAPI(t, "POST", "/remote/link", tok, nil); rec.Code != 403 || out["error"] != "remote_only_off" {
		t.Errorf("link through the tunnel = %d %v", rec.Code, out)
	}

	// Through the tunnel it can't be turned on or changed, only off, which cuts everything.
	if rec, out := f.remoteAPI(t, "POST", "/remote", tok, map[string]any{"on": true}); rec.Code != 403 || out["error"] != "remote_only_off" {
		t.Errorf("turning on through the tunnel = %d %v", rec.Code, out)
	}
	if !a.tunnel().on.Load() {
		t.Fatal("the tunnel went off")
	}
	rec, out := f.remoteAPI(t, "POST", "/remote", tok, map[string]any{"on": false})
	if rec.Code != 200 || out["on"] != false || out["address"] != nil {
		t.Errorf("turning off through the tunnel = %d %v", rec.Code, out)
	}
	if a.tunnel().on.Load() {
		t.Error("the tunnel is still on")
	}
	if cfg, _ := a.db.TunnelConfig(ctx); cfg.Enabled {
		t.Error("the setting is still on")
	}
	if rec, _ := f.remoteAPI(t, "GET", "/me", tok, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("after off, the tunnel answers %d", rec.Code)
	}
	for {
		if ev := sse.next(t, "remote"); ev["on"] == false {
			break
		}
	}
	// And off from home.
	api(t, a, "POST", "/remote", tok, map[string]any{"on": true}, 200)
	if got := api(t, a, "POST", "/remote", tok, map[string]any{"on": false}, 200); got["on"] != false {
		t.Errorf("off from home = %v", got)
	}
}
