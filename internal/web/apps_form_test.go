package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Webhooks can deliver form-encoded bodies (payload=<JSON>), like GitHub's
// "application/x-www-form-urlencoded" content type, signed over the exact
// bytes sent.
func TestAppsFormEncodedDelivery(t *testing.T) {
	f := newAppsFix(t)
	rc := newAppsReceiver(t)
	const secret = "form-secret-123"
	form := url.Values{"url": {rc.URL + "/hook"}, "secret": {secret}, "active": {"1"}, "events": {"issues"},
		"allow_local": {"1"}, "content_type": {"form"}}
	if rec := do(t, f.a, "POST", "/zoe/demo/settings/webhooks", form, f.ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("add form hook = %d\n%.1500s", rec.Code, rec.Body)
	}
	hooks, _ := f.a.db.RepoWebhooks(context.Background(), f.repo.ID)
	h := hooks[len(hooks)-1]
	if h.ContentType != "form" {
		t.Fatalf("stored content type = %q, want form", h.ContentType)
	}
	page := expectStatus(t, do(t, f.a, "GET", "/zoe/demo/settings/webhooks/"+itoa(h.ID), nil, f.ck), 200, "hook page")
	mustContain(t, page, "hook page", `<option value="form" selected>application/x-www-form-urlencoded</option>`)

	f.a.appsDeliverDue(context.Background()) // the ping
	hits := rc.all()
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want the ping", len(hits))
	}
	hit := hits[0]
	if got := hit.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", got)
	}
	if !strings.HasPrefix(string(hit.Body), "payload=") {
		t.Fatalf("body = %.200s", hit.Body)
	}
	// The signature covers the exact form body, not the inner JSON.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(hit.Body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if hit.Header.Get("X-Hub-Signature-256") != want || hit.Header.Get("X-Atelier-Signature-256") != want {
		t.Errorf("signature %q, want %q over the form body", hit.Header.Get("X-Hub-Signature-256"), want)
	}
	if !AppsVerifySignature(secret, hit.Body, hit.Header.Get("X-Hub-Signature-256")) {
		t.Error("receiver-side verification failed")
	}
	vals, err := url.ParseQuery(string(hit.Body))
	if err != nil {
		t.Fatal(err)
	}
	var ping map[string]any
	if err := json.Unmarshal([]byte(vals.Get("payload")), &ping); err != nil {
		t.Fatalf("payload parameter isn't JSON: %v", err)
	}
	if dig(ping, "hook", "config", "content_type") != "form" || ping["zen"] == nil {
		t.Errorf("ping = %v", ping)
	}

	// Switching the hook back to JSON changes how a redelivery is sent.
	upd := url.Values{"url": {rc.URL + "/hook"}, "active": {"1"}, "events": {"issues"}, "allow_local": {"1"}, "content_type": {"json"}}
	if rec := do(t, f.a, "POST", "/zoe/demo/settings/webhooks/"+itoa(h.ID), upd, f.ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("update hook = %d\n%.800s", rec.Code, rec.Body)
	}
	ds := f.deliveries(t, h)
	if rec := do(t, f.a, "POST", "/zoe/demo/settings/webhooks/"+itoa(h.ID)+"/deliveries/"+itoa(ds[0].ID)+"/redeliver", url.Values{}, f.ck); rec.Code != http.StatusSeeOther {
		t.Fatalf("redeliver = %d", rec.Code)
	}
	f.a.appsDeliverDue(context.Background())
	hits = rc.all()
	last := hits[len(hits)-1]
	if last.Header.Get("Content-Type") != "application/json" || string(last.Body) != ds[0].Payload {
		t.Errorf("JSON redelivery: content type %q, body is the raw JSON = %v", last.Header.Get("Content-Type"), string(last.Body) == ds[0].Payload)
	}
	if !AppsVerifySignature(secret, last.Body, last.Header.Get("X-Hub-Signature-256")) {
		t.Error("JSON redelivery signature doesn't verify")
	}
}

func TestAppsEncodeBody(t *testing.T) {
	body, ct := appsEncodeBody("form", `{"a":"b & c=d"}`)
	if ct != "application/x-www-form-urlencoded" || string(body) != "payload=%7B%22a%22%3A%22b+%26+c%3Dd%22%7D" {
		t.Errorf("form = %q %q", ct, body)
	}
	body, ct = appsEncodeBody("json", `{"a":1}`)
	if ct != "application/json" || string(body) != `{"a":1}` {
		t.Errorf("json = %q %q", ct, body)
	}
	for in, want := range map[string]string{"form": "form", "application/x-www-form-urlencoded": "form", "json": "json", "": "json", "xml": "json"} {
		if got := appsContentType(in); got != want {
			t.Errorf("appsContentType(%q) = %q, want %q", in, got, want)
		}
	}
}
