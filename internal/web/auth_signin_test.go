package web

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Issue #19: sign-in sometimes rejected the password just chosen in /setup.
// These tests drive the real forms through a real HTTP server with a cookie
// jar, the way a browser does: set up, sign out, sign in again.

func browserClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func postForm(t *testing.T, c *http.Client, srv *httptest.Server, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", srv.URL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res, b.String()
}

func TestSetupThenSignInSamePassword(t *testing.T) {
	cases := map[string]string{
		"plain":            "correct horse battery",
		"spaces":           "  leading and trailing spaces  ",
		"inner tabs":       "tab\there\tand there",
		"non-ascii nfc":    "pässwörd-ünïcödé-✓",
		"non-ascii nfd":    "pässwörd-combining",
		"emoji":            "🦊🦊🦊 fox password",
		"exactly 72 bytes": strings.Repeat("x", 72),
		"symbols":          `"&<>'%+=;?#\`,
	}
	for name, pw := range cases {
		t.Run(name, func(t *testing.T) {
			a := newTestApp(t)
			srv := httptest.NewServer(a)
			defer srv.Close()
			c := browserClient(t)

			res, body := postForm(t, c, srv, "/setup", url.Values{
				"username": {"zoe"}, "display_name": {"Zoe"}, "password": {pw}, "password_confirm": {pw},
			})
			if res.StatusCode != http.StatusSeeOther {
				t.Fatalf("setup = %d\n%s", res.StatusCode, body)
			}
			if res, _ := postForm(t, c, srv, "/signout", url.Values{}); res.StatusCode != http.StatusSeeOther {
				t.Fatalf("signout = %d", res.StatusCode)
			}
			res, body = postForm(t, c, srv, "/signin", url.Values{"password": {pw}, "next": {"/"}})
			if res.StatusCode != http.StatusSeeOther {
				t.Fatalf("sign-in with the setup password = %d\n%s", res.StatusCode, body)
			}
		})
	}
}

// A password longer than bcrypt's 72-byte limit used to fail setup with a bare
// server error; it must be refused with a clear message and save nothing.
func TestSetupRejectsOverlongPassword(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(a)
	defer srv.Close()
	c := browserClient(t)
	pw := strings.Repeat("ü", 40) // 80 bytes, 40 characters
	res, body := postForm(t, c, srv, "/setup", url.Values{"username": {"zoe"}, "password": {pw}, "password_confirm": {pw}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "72") {
		t.Fatalf("overlong setup = %d, want a re-rendered form mentioning the limit\n%s", res.StatusCode, body)
	}
	if !a.needsSetup(t.Context()) {
		t.Fatal("overlong setup created the owner")
	}
}

// Two setup submissions racing (a double click, or a browser retrying the
// POST) must leave one owner whose password is the one that actually
// created it, never a hash written by a request that then failed.
func TestSetupDoubleSubmitKeepsWinningPassword(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(a)
	defer srv.Close()

	pws := []string{"first password typed", "second password typed"}
	signedIn := make([]bool, len(pws))
	var wg sync.WaitGroup
	for i, pw := range pws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _ := postForm(t, browserClient(t), srv, "/setup", url.Values{"username": {"zoe"}, "password": {pw}, "password_confirm": {pw}})
			for _, c := range res.Cookies() {
				if c.Name == sessionCookie && c.Value != "" {
					signedIn[i] = res.StatusCode == http.StatusSeeOther
				}
			}
			if res.StatusCode != http.StatusSeeOther {
				t.Errorf("setup post %d = %d, want a redirect either way", i, res.StatusCode)
			}
		}()
	}
	wg.Wait()

	owners, err := a.db.Owners(t.Context())
	if err != nil || len(owners) != 1 {
		t.Fatalf("owners after double setup = %+v, %v", owners, err)
	}
	winners := 0
	for i, pw := range pws {
		res, _ := postForm(t, browserClient(t), srv, "/signin", url.Values{"password": {pw}})
		ok := res.StatusCode == http.StatusSeeOther
		if ok != signedIn[i] {
			t.Errorf("password %d: setup signed in = %v but sign-in accepted = %v", i, signedIn[i], ok)
		}
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("%d passwords work after a double setup, want exactly 1", winners)
	}
}

// Running out of attempts must say so, not claim the password is wrong.
func TestSignInRateLimitMessage(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(a)
	defer srv.Close()
	c := browserClient(t)
	pw := "correct horse battery"
	postForm(t, c, srv, "/setup", url.Values{"username": {"zoe"}, "password": {pw}, "password_confirm": {pw}})
	postForm(t, c, srv, "/signout", url.Values{})

	res, body := postForm(t, c, srv, "/signin", url.Values{"password": {"nope nope nope"}})
	if res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "password isn") {
		t.Fatalf("wrong password = %d\n%s", res.StatusCode, body)
	}
	for range 10 {
		res, body = postForm(t, c, srv, "/signin", url.Values{"password": {"nope nope nope"}})
	}
	if res.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Too many") || strings.Contains(body, "password isn") {
		t.Fatalf("locked out = %d\n%s", res.StatusCode, body)
	}
	// Even the right password is refused while locked out, and says why.
	res, body = postForm(t, c, srv, "/signin", url.Values{"password": {pw}})
	if res.StatusCode != http.StatusTooManyRequests || strings.Contains(body, "password isn") {
		t.Fatalf("right password while locked out = %d\n%s", res.StatusCode, body)
	}
}

// Other failed credentials from the same address (a git client probing
// without a token, a stale runner) must not use up the sign-in allowance.
func TestSignInNotLockedOutByGitProbes(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(a)
	defer srv.Close()
	c := browserClient(t)
	pw := "correct horse battery"
	postForm(t, c, srv, "/setup", url.Values{"username": {"zoe"}, "password": {pw}, "password_confirm": {pw}})
	postForm(t, c, srv, "/signout", url.Values{})
	me, _ := a.db.PersonalOwner(t.Context())
	if _, err := a.db.CreateRepo(t.Context(), me.ID, "private", "", true); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		res, err := http.Get(srv.URL + "/zoe/private.git/info/refs?service=git-upload-pack")
		if err == nil {
			res.Body.Close()
		}
	}
	res, body := postForm(t, c, srv, "/signin", url.Values{"password": {pw}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in after git probes = %d\n%s", res.StatusCode, body)
	}
}
