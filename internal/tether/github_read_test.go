package tether

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type timedCache struct {
	mu sync.Mutex
	m  map[string]timedEntry
}

type timedEntry struct {
	etag string
	body []byte
	at   time.Time
}

func (c *timedCache) Get(ctx context.Context, k string) (string, []byte, bool) {
	e, b, _, ok := c.GetAt(ctx, k)
	return e, b, ok
}

func (c *timedCache) GetAt(_ context.Context, k string) (string, []byte, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v.etag, v.body, v.at, ok
}

func (c *timedCache) Put(_ context.Context, k, etag string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = timedEntry{etag, body, time.Now()}
}

func (c *timedCache) age(k string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.m[k]
	v.at = v.at.Add(-d)
	c.m[k] = v
}

func TestClientFreshAndStale(t *testing.T) {
	var hits atomic.Int32
	var mode atomic.Int32 // 0 ok, 1 rate limited, 2 server error
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch mode.Load() {
		case 1:
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "4102444800")
			w.WriteHeader(403)
			w.Write([]byte(`{"message":"API rate limit exceeded"}`))
			return
		case 2:
			w.WriteHeader(502)
			return
		}
		if r.Header.Get("If-None-Match") == `"p1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"p1"`)
		w.Write([]byte(`{"login":"dave","name":"Dave","followers":3}`))
	}))
	defer srv.Close()
	cache := &timedCache{m: map[string]timedEntry{}}
	var staleAt time.Time
	c := &Client{API: srv.URL, Token: "t-fresh", Cache: cache, MaxAge: time.Minute, StaleOK: true,
		OnStale: func(at time.Time, _ error) { staleAt = at }}
	defer c.Unblock()
	ctx := context.Background()

	p, err := c.Profile(ctx, "dave")
	if err != nil || p.Name != "Dave" || hits.Load() != 1 {
		t.Fatalf("first = %+v, %v, hits %d", p, err, hits.Load())
	}
	// Fresh: no request at all.
	if p, err := c.Profile(ctx, "dave"); err != nil || p.Followers != 3 || hits.Load() != 1 {
		t.Fatalf("fresh = %+v, %v, hits %d", p, err, hits.Load())
	}
	// Older than MaxAge: revalidated (304), which restarts MaxAge.
	cache.age("GET /users/dave", 2*time.Minute)
	if _, err := c.Profile(ctx, "dave"); err != nil || hits.Load() != 2 {
		t.Fatalf("revalidate: %v, hits %d", err, hits.Load())
	}
	if _, _, at, _ := cache.GetAt(ctx, "GET /users/dave"); time.Since(at) > time.Minute {
		t.Error("a 304 didn't refresh the entry's time")
	}
	// GitHub failing: the saved copy, with its age reported.
	cache.age("GET /users/dave", 2*time.Minute)
	mode.Store(2)
	if p, err := c.Profile(ctx, "dave"); err != nil || p.Name != "Dave" || staleAt.IsZero() {
		t.Fatalf("5xx stale = %+v, %v, %v", p, err, staleAt)
	}
	// Rate limited: the saved copy, then no more requests while blocked.
	mode.Store(1)
	staleAt = time.Time{}
	if p, err := c.Profile(ctx, "dave"); err != nil || p.Name != "Dave" || staleAt.IsZero() {
		t.Fatalf("limited stale = %+v, %v", p, err)
	}
	n := hits.Load()
	if _, err := c.Profile(ctx, "dave"); err != nil || hits.Load() != n {
		t.Fatalf("blocked: %v, hits %d → %d", err, n, hits.Load())
	}
	// Nothing saved: the rate-limit error.
	if _, err := c.Profile(ctx, "carol"); err == nil {
		t.Fatal("an uncached call succeeded while blocked")
	}
	// Without StaleOK (every existing caller), errors are errors.
	c2 := &Client{API: srv.URL, Token: "t-fresh", Cache: cache}
	if _, err := c2.Profile(ctx, "dave"); err == nil {
		t.Fatal("a client without StaleOK served a stale answer")
	}
}
