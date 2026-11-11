package tether

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memCache struct {
	mu sync.Mutex
	m  map[string][2]string
}

func (c *memCache) Get(_ context.Context, k string) (string, []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v[0], []byte(v[1]), ok
}

func (c *memCache) Put(_ context.Context, k, etag string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = [2]string{etag, string(body)}
}

func TestClientETagAndRateLimit(t *testing.T) {
	const tok = "ghp_secretsecret"
	var hits, notModified atomic.Int32
	var limited atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(401)
			w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		if limited.Load() {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "4102444800") // far future
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		}
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Reset", "4102444800")
		if r.Header.Get("If-None-Match") == `"v1"` {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(`{"id":1,"full_name":"acme/tool","name":"tool","stargazers_count":7}`))
	}))
	defer srv.Close()

	var rate Rate
	c := &Client{API: srv.URL, Token: tok, Cache: &memCache{m: map[string][2]string{}}, OnRate: func(r Rate) { rate = r }}
	defer c.Unblock()
	ctx := context.Background()
	r1, err := c.Repo(ctx, "acme/tool")
	if err != nil || r1.Stars != 7 {
		t.Fatalf("first = %+v, %v", r1, err)
	}
	r2, err := c.Repo(ctx, "acme/tool")
	if err != nil || r2.Stars != 7 || notModified.Load() != 1 {
		t.Fatalf("second (conditional) = %+v, %v, 304s=%d", r2, err, notModified.Load())
	}
	if rate.Remaining != 4321 || rate.Limit != 5000 {
		t.Errorf("rate = %+v", rate)
	}

	limited.Store(true)
	_, err = c.Repo(ctx, "acme/other")
	var rl *RateLimitError
	if !errors.As(err, &rl) || !errors.Is(err, ErrRateLimited) || rl.Reset.Year() < 2100 {
		t.Fatalf("limited = %v", err)
	}
	before := hits.Load()
	if _, err := c.Repo(ctx, "acme/tool"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("blocked call = %v", err)
	}
	if hits.Load() != before {
		t.Error("a blocked client still called GitHub")
	}
	if _, ok := c.BlockedUntil(); !ok {
		t.Error("BlockedUntil should report the block")
	}
	c.Unblock()
	limited.Store(false)

	// Bad tokens: a clear error that never contains the token.
	bad := &Client{API: srv.URL, Token: "ghp_wrongwrong"}
	_, err = bad.Repo(ctx, "acme/tool")
	if !IsStatus(err, 401) || strings.Contains(err.Error(), "ghp_wrongwrong") {
		t.Errorf("bad token = %v", err)
	}
	if _, err := c.Repo(ctx, "../etc"); err == nil {
		t.Error("an invalid repository name was accepted")
	}
}

func TestClientRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &Client{API: srv.URL, Token: "t2"}
	defer c.Unblock()
	_, err := c.Repo(context.Background(), "a/b")
	var rl *RateLimitError
	if !errors.As(err, &rl) || time.Until(rl.Reset) < 100*time.Second {
		t.Fatalf("429 = %v", err)
	}
}

func TestNames(t *testing.T) {
	for s, want := range map[string]bool{"acme/tool": true, "a-b/c.d_e": true, "-x/y": false, "x/..": false,
		"x/y.git": false, "x": false, "x/y/z": false, "x/ y": false} {
		if ValidRepo(s) != want {
			t.Errorf("ValidRepo(%q) = %v", s, !want)
		}
	}
	if !ValidLogin("octo-cat") || ValidLogin("-bad") || ValidLogin("a/b") {
		t.Error("ValidLogin")
	}
	if (Comment{IssueURL: "https://api.github.com/repos/a/b/issues/42"}).IssueNumber() != 42 {
		t.Error("IssueNumber")
	}
}

type fakeBridge struct{}

func (fakeBridge) Linked(context.Context, int64) (string, bool, error) { return "me/x", true, nil }
func (fakeBridge) MirrorPull(context.Context, int64, int64) (int, string, error) {
	return 1, "u", nil
}
func (fakeBridge) ClosePull(context.Context, int64, int64, bool, string) error { return nil }

func TestBridgeRegistry(t *testing.T) {
	defer RegisterBridge(nil)
	RegisterBridge(nil)
	if gh, ok, err := Get().Linked(context.Background(), 1); ok || err != nil || gh != "" {
		t.Errorf("no-op Linked = %q %v %v", gh, ok, err)
	}
	if _, _, err := Get().MirrorPull(context.Background(), 1, 1); !errors.Is(err, ErrNotLinked) {
		t.Errorf("no-op MirrorPull = %v", err)
	}
	RegisterBridge(fakeBridge{})
	if gh, ok, _ := Get().Linked(context.Background(), 1); !ok || gh != "me/x" {
		t.Error("registered bridge not returned")
	}
}
