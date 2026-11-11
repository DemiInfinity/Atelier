package web

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/store"
	"atelier/internal/tether"
)

// ---------- a GitHub world for /gh/ (served through fakeGH.extra) ----------

const ghXSS = `<script>alert(1)</script>`

type ghWorld struct {
	profiles map[string]map[string]any
	repos    map[string][]map[string]any // owner → list (users/{x}/repos, orgs/{x}/repos)
	files    map[string]string           // "owner/repo/path" → content (at any ref)
	dirs     map[string][]string         // "owner/repo/dir" → entries ("name" or "name/" for dirs)
	branches map[string][]string
	commits  map[string][]map[string]any
	issues   map[string][]map[string]any
	comments map[string][]map[string]any // "owner/repo#n"
	pulls    map[string][]map[string]any
	prFiles  map[string][]map[string]any // "owner/repo#n"
	pinned   map[string][]map[string]any
	gqlFail  bool
}

func newGHWorld() *ghWorld {
	return &ghWorld{profiles: map[string]map[string]any{}, repos: map[string][]map[string]any{}, files: map[string]string{},
		dirs: map[string][]string{}, branches: map[string][]string{}, commits: map[string][]map[string]any{},
		issues: map[string][]map[string]any{}, comments: map[string][]map[string]any{}, pulls: map[string][]map[string]any{},
		prFiles: map[string][]map[string]any{}, pinned: map[string][]map[string]any{}}
}

const ghSHA1 = "1111111111111111111111111111111111111111"
const ghSHA2 = "2222222222222222222222222222222222222222"

func ghUserJSON(login string) map[string]any {
	return map[string]any{"login": login, "avatar_url": "https://avatars.githubusercontent.com/u/7", "type": "User"}
}

func (gw *ghWorld) serve(f *fakeGH) func(w http.ResponseWriter, r *http.Request, seg []string, body map[string]any) bool {
	return func(w http.ResponseWriter, r *http.Request, seg []string, body map[string]any) bool {
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/graphql" && r.Method == http.MethodPost:
			vars, _ := body["variables"].(map[string]any)
			login, _ := vars["login"].(string)
			if gw.gqlFail {
				f.write(w, r, 200, map[string]any{"errors": []map[string]string{{"message": "GraphQL is off"}}})
				return true
			}
			f.write(w, r, 200, map[string]any{"data": map[string]any{"repositoryOwner": map[string]any{
				"pinnedItems": map[string]any{"nodes": gw.pinned[login]}}}})
			return true
		case len(seg) == 2 && seg[0] == "users":
			if p, ok := gw.profiles[seg[1]]; ok {
				f.write(w, r, 200, p)
			} else {
				f.write(w, r, 404, map[string]string{"message": "Not Found"})
			}
			return true
		case len(seg) == 3 && (seg[0] == "users" || seg[0] == "orgs") && seg[2] == "repos":
			f.write(w, r, 200, gw.repos[seg[1]])
			return true
		case len(seg) < 4 || seg[0] != "repos":
			return false
		}
		full := seg[1] + "/" + seg[2]
		rest := seg[3:]
		if _, ok := gw.branches[full]; !ok {
			return false // not part of this world: the base fake answers
		}
		key := func(n string) string { return full + "#" + n }
		switch {
		case rest[0] == "contents":
			p := strings.Join(rest[1:], "/")
			if entries, ok := gw.dirs[full+"/"+p]; ok {
				var out []map[string]any
				for _, e := range entries {
					name, typ := strings.TrimSuffix(e, "/"), "file"
					if strings.HasSuffix(e, "/") {
						typ = "dir"
					}
					out = append(out, map[string]any{"type": typ, "name": name, "path": strings.TrimPrefix(p+"/"+name, "/"), "sha": "x"})
				}
				f.write(w, r, 200, out)
				return true
			}
			if c, ok := gw.files[full+"/"+p]; ok {
				e := map[string]any{"type": "file", "name": p[strings.LastIndex(p, "/")+1:], "path": p, "size": len(c), "sha": "y",
					"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(c))}
				if len(c) > 1<<20 {
					e["encoding"], e["content"] = "none", ""
				}
				f.write(w, r, 200, e)
				return true
			}
			f.write(w, r, 404, map[string]string{"message": "Not Found"})
		case rest[0] == "readme":
			dir := strings.Join(rest[1:], "/")
			p := strings.TrimPrefix(dir+"/README.md", "/")
			if c, ok := gw.files[full+"/"+p]; ok {
				f.write(w, r, 200, map[string]any{"type": "file", "name": "README.md", "path": p, "encoding": "base64",
					"content": base64.StdEncoding.EncodeToString([]byte(c))})
			} else {
				f.write(w, r, 404, map[string]string{"message": "Not Found"})
			}
		case rest[0] == "branches" && len(rest) == 1:
			var out []map[string]any
			for _, b := range gw.branches[full] {
				out = append(out, map[string]any{"name": b, "commit": map[string]string{"sha": ghSHA1}})
			}
			f.write(w, r, 200, out)
		case rest[0] == "tags":
			f.write(w, r, 200, []map[string]any{{"name": "v1.0", "commit": map[string]string{"sha": ghSHA2}}})
		case rest[0] == "commits" && len(rest) == 1:
			cs := gw.commits[full]
			if n, _ := strconv.Atoi(q.Get("per_page")); n > 0 && n < len(cs) {
				cs = cs[:n]
			}
			f.write(w, r, 200, cs)
		case rest[0] == "commits" && len(rest) == 2:
			for _, c := range gw.commits[full] {
				if c["sha"] == rest[1] {
					f.write(w, r, 200, c)
					return true
				}
			}
			f.write(w, r, 404, map[string]string{"message": "Not Found"})
		case rest[0] == "issues" && len(rest) == 1:
			var out []map[string]any
			for _, is := range gw.issues[full] {
				if q.Get("state") == "all" || is["state"] == q.Get("state") {
					out = append(out, is)
				}
			}
			f.write(w, r, 200, out)
		case rest[0] == "issues" && len(rest) == 2:
			for _, is := range gw.issues[full] {
				if fmt.Sprint(is["number"]) == rest[1] {
					f.write(w, r, 200, is)
					return true
				}
			}
			f.write(w, r, 404, map[string]string{"message": "Not Found"})
		case rest[0] == "issues" && len(rest) == 3 && rest[2] == "comments":
			f.write(w, r, 200, gw.comments[key(rest[1])])
		case rest[0] == "pulls" && len(rest) == 1:
			var out []map[string]any
			for _, p := range gw.pulls[full] {
				if p["state"] == q.Get("state") {
					out = append(out, p)
				}
			}
			f.write(w, r, 200, out)
		case rest[0] == "pulls" && len(rest) == 2:
			for _, p := range gw.pulls[full] {
				if fmt.Sprint(p["number"]) == rest[1] {
					f.write(w, r, 200, p)
					return true
				}
			}
			f.write(w, r, 404, map[string]string{"message": "Not Found"})
		case rest[0] == "pulls" && len(rest) == 3 && rest[2] == "files":
			f.write(w, r, 200, gw.prFiles[key(rest[1])])
		default:
			return false
		}
		return true
	}
}

type ghFixture struct {
	*tetherFixture
	w   *ghWorld
	img *ghImgServer
}

// ghImgServer stands in for GitHub's image hosts (every allowed host name
// resolves to it in tests).
type ghImgServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []*http.Request
	// what each path answers
	answers map[string]func(w http.ResponseWriter, r *http.Request)
}

func (s *ghImgServer) seen() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.hits...)
}

func ghPNG(t *testing.T) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func newGHFixture(t *testing.T) *ghFixture {
	t.Helper()
	tf := newTetherFixture(t)
	f := &ghFixture{tetherFixture: tf, w: newGHWorld()}
	tf.gh.extra = f.w.serve(tf.gh)
	tf.connect(t)

	// Image hosts.
	pngBytes := ghPNG(t)
	f.img = &ghImgServer{answers: map[string]func(w http.ResponseWriter, r *http.Request){
		"/ok.png": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngBytes)
		},
		"/dave/rocket/main/img/logo.png": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngBytes)
		},
		"/big.png": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngBytes)
			w.Write(make([]byte, ghImgMax+10))
		},
		"/page.html": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html><script>alert(1)</script></html>"))
		},
		"/fake.png": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("<html><script>alert(1)</script></html>"))
		},
		"/away.png": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://evil.example/x.png", http.StatusFound)
		},
		"/badge.svg": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/svg+xml")
			w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><script>alert(1)</script></svg>`))
		},
	}}
	f.img.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.img.mu.Lock()
		f.img.hits = append(f.img.hits, r.Clone(context.Background()))
		h := f.img.answers[r.URL.Path]
		f.img.mu.Unlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.img.srv.Close)
	st := f.a.gh().img
	addr := f.img.srv.Listener.Addr().String()
	st.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("140.82.112.3")}, nil // a public address
	}
	d := &net.Dialer{}
	st.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
	st.tls = &tls.Config{InsecureSkipVerify: true}

	f.seed(t)
	return f
}

func (f *ghFixture) seed(t *testing.T) {
	w := f.w
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	w.profiles["dave"] = map[string]any{"login": "dave", "name": "Dave Rocket", "type": "User", "bio": "Builds rockets " + ghXSS,
		"avatar_url": "https://avatars.githubusercontent.com/u/42", "followers": 1234, "following": 56, "public_repos": 7,
		"location": "Lisbon", "company": "@rockets", "blog": "dave.example", "twitter_username": "daverocket"}
	w.profiles["carol"] = map[string]any{"login": "carol", "name": "Carol", "type": "User", "avatar_url": "https://avatars.githubusercontent.com/u/43"}
	w.profiles["acme"] = map[string]any{"login": "acme", "name": "Acme Org", "type": "Organization", "description": "We make anvils"}
	w.repos["dave"] = []map[string]any{
		{"name": "rocket", "full_name": "dave/rocket", "description": "Fast rockets", "language": "Go", "stargazers_count": 900, "owner": ghUserJSON("dave")},
		{"name": "notes", "full_name": "dave/notes", "description": "Notes " + ghXSS, "stargazers_count": 3, "owner": ghUserJSON("dave")},
	}
	w.repos["acme"] = []map[string]any{{"name": "anvil", "full_name": "acme/anvil", "description": "Heavy", "owner": ghUserJSON("acme")}}
	w.pinned["dave"] = []map[string]any{{"name": "rocket", "nameWithOwner": "dave/rocket", "description": "Pinned rocket", "stargazerCount": 900,
		"primaryLanguage": map[string]string{"name": "Go"}, "owner": map[string]string{"login": "dave"}}}
	body, _ := json.Marshal(tetherBadgeFileJSON{Atelier: true, Studio: "https://studio.dave.dev/dave"})
	f.gh.files["dave/dave/.atelier"] = fakeFile{data: body, sha: "abc"}
	f.gh.repos["dave/dave"] = &tether.Repo{FullName: "dave/dave", Name: "dave"}

	full := "dave/rocket"
	f.gh.repos[full] = &tether.Repo{ID: 77, Name: "rocket", FullName: full, DefaultBranch: "main", Description: "Fast rockets for everyone",
		Language: "Go", Stars: 900, Forks: 12, Homepage: "https://rocket.example", Topics: []string{"space", "go"},
		Owner: tether.User{Login: "dave", AvatarURL: "https://avatars.githubusercontent.com/u/42"},
		License: &struct {
			Key    string `json:"key"`
			Name   string `json:"name"`
			SPDXID string `json:"spdx_id"`
		}{"mit", "MIT License", "MIT"}}
	evilBranch := "feat/<script>alert(4)</script>"
	w.branches[full] = []string{"main", "feature/long-name", evilBranch}
	w.dirs[full+"/"] = []string{"docs/", "README.md", "main.go", `"><svg onload=alert(3)>.txt`, "logo.bin"}
	w.dirs[full+"/docs"] = []string{"guide.md"}
	w.files[full+"/README.md"] = "# Rocket Readme\n\nRead the [guide](docs/guide.md) and [root](/main.go) and [up](../../etc/passwd).\n\n" +
		"![logo](img/logo.png) ![evil](https://evil.example/x.png)\n\n" +
		`<p align="center"><img src="https://camo.githubusercontent.com/abc/def" alt="build"></p>` + "\n\n" +
		ghXSS + "\n\n[click](javascript:alert(1)) <img src=x onerror=alert(5)>\n\n" +
		"[issue](" + f.gh.srv.URL + "/dave/rocket/issues/3) [elsewhere](https://example.org/x)\n"
	w.files[full+"/main.go"] = "package main\n\nfunc main() { println(\"" + ghXSS + "\") }\n"
	w.files[full+"/docs/guide.md"] = "# The guide\n\n![diagram](../img/logo.png)\n"
	w.files[full+"/"+`"><svg onload=alert(3)>.txt`] = "hello"
	w.files[full+"/logo.bin"] = "\x00\x01\x02binary"
	w.files[full+"/big.txt"] = strings.Repeat("a", 2<<20)
	when := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	w.commits[full] = []map[string]any{
		{"sha": ghSHA1, "commit": map[string]any{"message": "Launch the rocket " + ghXSS + "\n\nLonger body.", "author": map[string]any{"name": "Dave R", "date": when}},
			"author": ghUserJSON("dave"), "parents": []map[string]string{{"sha": ghSHA2}}, "stats": map[string]int{"additions": 3, "deletions": 1},
			"files": []map[string]any{{"filename": "main.go", "status": "modified", "additions": 3, "deletions": 1,
				"patch": "@@ -1,2 +1,4 @@\n package main\n-// old\n+// new " + ghXSS + "\n+func main() {}"}}},
		{"sha": ghSHA2, "commit": map[string]any{"message": "First", "author": map[string]any{"name": "Someone", "date": when}}},
	}
	created := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	w.issues[full] = []map[string]any{
		{"number": 3, "title": "Engine " + `<img src=x onerror=alert(2)>`, "state": "open", "body": "It explodes " + ghXSS + " see #4",
			"user": ghUserJSON("carol"), "comments": 1, "created_at": created, "labels": []map[string]string{{"name": "bug", "color": "red;}"}}},
		{"number": 4, "title": "Old closed thing", "state": "closed", "user": ghUserJSON("dave"), "created_at": created},
		{"number": 5, "title": "A PR as an issue", "state": "open", "user": ghUserJSON("dave"), "created_at": created, "pull_request": map[string]any{}},
	}
	w.comments[full+"#3"] = []map[string]any{{"id": 99, "body": "Me too! ![shot](https://user-images.githubusercontent.com/1/shot.png)", "user": ghUserJSON("dave"), "created_at": created}}
	w.pulls[full] = []map[string]any{
		{"number": 5, "title": "Add thrusters", "state": "open", "body": "Thrusters " + ghXSS, "user": ghUserJSON("dave"), "created_at": created,
			"head": map[string]any{"ref": evilBranch, "sha": ghSHA1, "repo": map[string]any{"full_name": full}},
			"base": map[string]any{"ref": "main"}, "commits": 2, "additions": 10, "deletions": 2, "changed_files": 1},
	}
	w.comments[full+"#5"] = []map[string]any{{"id": 100, "body": "LGTM", "user": ghUserJSON("carol"), "created_at": created}}
	w.prFiles[full+"#5"] = []map[string]any{{"filename": "thrust.go", "status": "added", "additions": 10, "deletions": 0, "patch": "@@ -0,0 +1 @@\n+package thrust"}}
	f.gh.releases[full] = []tether.Release{{TagName: "v1.0", Name: "First flight", Body: "## Notes\n\nFixed #3 " + ghXSS, PublishedAt: time.Now().Add(-time.Hour),
		HTMLURL: f.gh.srv.URL + "/dave/rocket/releases/tag/v1.0", Author: tether.User{Login: "dave"}}}

	// A private repository of the connected person, with an image.
	priv := "zoe-gh/secret"
	f.gh.repos[priv] = &tether.Repo{Name: "secret", FullName: priv, Private: true, DefaultBranch: "main", Owner: tether.User{Login: "zoe-gh"}}
	w.branches[priv] = []string{"main"}
	w.files[priv+"/img/p.png"] = string(ghPNG(t))
}

func (f *ghFixture) get(t *testing.T, path string, code int) string {
	t.Helper()
	return expectStatus(t, do(t, f.a, "GET", path, nil, f.ck), code, path)
}

func mustNotContain(t *testing.T, body, what string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(body, s) {
			t.Errorf("%s: must not contain %q", what, s)
		}
	}
}

// ghNoXSS checks a page carries none of the payloads unescaped.
func ghNoXSS(t *testing.T, body, what string) {
	t.Helper()
	mustNotContain(t, body, what, "<script>alert", "<svg onload", `href="javascript:`, "evil.example",
		"<img src=x", `href="../../etc`, fakeGHToken)
}

// ---------- tests ----------

func TestGHReservedName(t *testing.T) {
	if !reserved["gh"] {
		t.Fatal(`"gh" isn't reserved`)
	}
	f := newGitFixture(t)
	ck := pullsCookie(t, f.a)
	body := expectStatus(t, do(t, f.a, "POST", "/accounts/new", url.Values{"name": {"gh"}}, ck), 200, "new account gh")
	mustContain(t, body, "account gh", "Use letters")
	if _, err := f.a.db.OwnerByName(context.Background(), "gh"); err == nil {
		t.Fatal("an account named gh was created")
	}
}

func TestGHAccess(t *testing.T) {
	f := newGHFixture(t)
	a, ctx := f.a, context.Background()

	// Anonymous: sent to sign in, GitHub isn't asked.
	before := f.gh.hit("GET /users/dave")
	for _, p := range []string{"/gh/dave", "/gh/dave/rocket", "/gh/-/img?u=" + url.QueryEscape("https://camo.githubusercontent.com/ok.png")} {
		rec := do(t, a, "GET", p, nil, nil)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin?next=") {
			t.Errorf("anonymous %s = %d %s", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := do(t, a, "POST", "/gh/dave", url.Values{}, f.ck); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /gh/dave = %d", rec.Code)
	}

	// A second person without Tether: the connect page, never the owner's token.
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := a.db.CreateSessionFor(ctx, sam.ID, time.Hour)
	samCk := map[string]string{"Cookie": sessionCookie + "=" + sess}
	for _, p := range []string{"/gh/dave", "/gh/dave/rocket", "/gh/dave/rocket/issues/3", "/gh/-/img?u=" + url.QueryEscape("https://camo.githubusercontent.com/ok.png")} {
		body := expectStatus(t, do(t, a, "GET", p, nil, samCk), 200, "sam "+p)
		mustContain(t, body, "sam "+p, "Connect GitHub", "Settings → Tether")
		mustNotContain(t, body, "sam "+p, "Dave Rocket", "Fast rockets", "zoe-gh")
	}
	if f.gh.hit("GET /users/dave") != before || f.gh.hit("GET /repos/dave/rocket") != 0 || len(f.img.seen()) != 0 {
		t.Error("a person without Tether reached GitHub (someone else's token was used)")
	}
	// The owner's cache isn't shown to sam either.
	f.get(t, "/gh/dave", 200)
	body := expectStatus(t, do(t, a, "GET", "/gh/dave", nil, samCk), 200, "sam after owner")
	mustNotContain(t, body, "sam after owner", "Dave Rocket")

	// Unknown pages under /gh/ are 404s, not the account router.
	f.get(t, "/gh/dave/rocket/nope/x", 404)
	f.get(t, "/gh/..%2F..%2Fetc", 404)
}

func TestGHProfileAndBadge(t *testing.T) {
	f := newGHFixture(t)
	body := f.get(t, "/gh/dave", 200)
	mustContain(t, body, "profile", "Dave Rocket", "dave", "Builds rockets", "1234", "followers", "56", "Lisbon", "@rockets",
		`href="https://dave.example"`, "@daverocket", "Pinned", "Pinned rocket", `href="/gh/dave/rocket"`, "Fast rockets",
		"Repositories", "Open on GitHub", "https://avatars.githubusercontent.com/u/42", "On GitHub · via Tether",
		// The badge, checked right away.
		"gh-badge", "Visit their studio", "https://studio.dave.dev/dave", "has a studio of their own")
	ghNoXSS(t, body, "profile")

	body = f.get(t, "/gh/carol", 200)
	mustContain(t, body, "carol", "Carol")
	mustNotContain(t, body, "carol", "Visit their studio", "has a studio of their own", `class="gh-badge"`)

	// Organisations list their repositories; GraphQL failing falls back to popular ones.
	f.gh.mu.Lock()
	f.w.gqlFail = true
	f.gh.search = []tether.Repo{{Name: "anvil", FullName: "acme/anvil", Stars: 5, Owner: tether.User{Login: "acme"}}}
	f.gh.mu.Unlock()
	body = f.get(t, "/gh/acme", 200)
	mustContain(t, body, "org", "Acme Org", "Organisation", "We make anvils", `href="/gh/acme/anvil"`, "Popular repositories")
	if f.gh.hit("GET /orgs/acme/repos") == 0 {
		t.Error("the organisation's repositories weren't listed")
	}

	// Search and sort go through GitHub search.
	f.get(t, "/gh/dave?q=rock&sort=stars", 200)
	if f.gh.hit("GET /search/repositories") == 0 {
		t.Error("repository search wasn't used")
	}
	body = f.get(t, "/gh/dave?sort=name", 200)
	mustContain(t, body, "sorted", `aria-current="true">Name`)

	f.get(t, "/gh/nobody-here", 404)
	f.get(t, "/gh/bad..login", 404)
}

func TestGHRepoPages(t *testing.T) {
	f := newGHFixture(t)
	f.wait()

	code := f.get(t, "/gh/dave/rocket", 200)
	mustContain(t, code, "code", "On GitHub · via Tether", "Mirror to studio", "Open on GitHub", "Fast rockets for everyone",
		"space", "MIT License", "900", "12", "Go", `href="https://rocket.example"`, // About
		`href="/gh/dave/rocket/tree/main/docs"`, "README.md", `href="/gh/dave/rocket/blob/main/main.go"`, // tree
		"feature/long-name", "v1.0", // branches and tags
		"Launch the rocket", // latest commit
		"Rocket Readme",
		// relative links and images
		`href="/gh/dave/rocket/blob/main/docs/guide.md"`, `href="/gh/dave/rocket/blob/main/main.go"`,
		`src="/gh/-/img?u=https%3A%2F%2Fraw.githubusercontent.com%2Fdave%2Frocket%2Fmain%2Fimg%2Flogo.png"`,
		`src="/gh/-/img?u=https%3A%2F%2Fcamo.githubusercontent.com%2Fabc%2Fdef"`, `alt="build"`,
		"evil", // the alt text of the refused image
		`href="/gh/dave/rocket/issues/3"`, `href="https://example.org/x" rel="noopener noreferrer nofollow" target="_blank"`)
	ghNoXSS(t, code, "code")
	mustContain(t, code, "escaped", "&lt;script&gt;alert(4)&lt;/script&gt;", "&#34;&gt;&lt;svg onload=alert(3)&gt;.txt")
	if strings.Contains(code, `href="/gh/dave/rocket/blob/main/etc/passwd"`) {
		t.Error("a link climbing out of the repository was kept")
	}

	tree := f.get(t, "/gh/dave/rocket/tree/main/docs", 200)
	mustContain(t, tree, "tree", "guide.md", `href="/gh/dave/rocket/blob/main/docs/guide.md"`)

	blob := f.get(t, "/gh/dave/rocket/blob/main/main.go", 200)
	mustContain(t, blob, "blob", "main.go", "package main", "&lt;script&gt;alert(1)&lt;/script&gt;", "View on GitHub", "lines")
	ghNoXSS(t, blob, "blob")
	guide := f.get(t, "/gh/dave/rocket/blob/main/docs/guide.md", 200)
	mustContain(t, guide, "guide", "The guide", url.QueryEscape("https://raw.githubusercontent.com/dave/rocket/main/img/logo.png"))
	mustContain(t, f.get(t, "/gh/dave/rocket/blob/main/logo.bin", 200), "binary", "Binary file not shown", "View it on GitHub")
	mustContain(t, f.get(t, "/gh/dave/rocket/blob/main/big.txt", 200), "large", "too large to show here", "View it on GitHub")
	// A folder opened as a file, and a file opened as a folder.
	if rec := do(t, f.a, "GET", "/gh/dave/rocket/blob/main/docs", nil, f.ck); rec.Code != 303 || rec.Header().Get("Location") != "/gh/dave/rocket/tree/main/docs" {
		t.Errorf("blob of a folder = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(t, f.a, "GET", "/gh/dave/rocket/tree/main/main.go", nil, f.ck); rec.Code != 303 {
		t.Errorf("tree of a file = %d", rec.Code)
	}
	// A branch with a slash.
	mustContain(t, f.get(t, "/gh/dave/rocket/tree/feature/long-name", 200), "slashed branch", "feature/long-name")

	commits := f.get(t, "/gh/dave/rocket/commits", 200)
	mustContain(t, commits, "commits", "Launch the rocket", "First", "1111111", `href="/gh/dave/rocket/commit/`+ghSHA1+`"`, `href="/gh/dave"`)
	ghNoXSS(t, commits, "commits")
	commit := f.get(t, "/gh/dave/rocket/commit/"+ghSHA1, 200)
	mustContain(t, commit, "commit", "Launch the rocket", "Longer body.", "main.go", `class="dl add"`, `class="dl del"`, "+3", "2222222")
	ghNoXSS(t, commit, "commit")
	f.get(t, "/gh/dave/rocket/commit/not-a-sha", 404)

	rel := f.get(t, "/gh/dave/rocket/releases", 200)
	mustContain(t, rel, "releases", "First flight", "v1.0", "Notes", `href="/gh/dave/rocket/issues/3"`, `href="/gh/dave/rocket/tree/v1.0"`)
	ghNoXSS(t, rel, "releases")

	f.get(t, "/gh/dave/missing", 404)
}

func TestGHIssuesAndPulls(t *testing.T) {
	f := newGHFixture(t)
	list := f.get(t, "/gh/dave/rocket/issues", 200)
	mustContain(t, list, "issues", "Engine", "&lt;img src=x onerror=alert(2)&gt;", `href="/gh/dave/rocket/issues/3"`, "bug", `href="/gh/carol"`)
	mustNotContain(t, list, "issues", "Old closed thing", "A PR as an issue")
	ghNoXSS(t, list, "issues")
	closed := f.get(t, "/gh/dave/rocket/issues?state=closed", 200)
	mustContain(t, closed, "closed", "Old closed thing")

	issue := f.get(t, "/gh/dave/rocket/issues/3", 200)
	mustContain(t, issue, "issue", "Engine", "It explodes", `href="/gh/dave/rocket/issues/4"`, "Me too!",
		url.QueryEscape("https://user-images.githubusercontent.com/1/shot.png"), "Read-only here", "Comment on GitHub")
	ghNoXSS(t, issue, "issue")
	if rec := do(t, f.a, "GET", "/gh/dave/rocket/issues/5", nil, f.ck); rec.Code != 303 || rec.Header().Get("Location") != "/gh/dave/rocket/pull/5" {
		t.Errorf("a pull request opened as an issue = %d %s", rec.Code, rec.Header().Get("Location"))
	}
	f.get(t, "/gh/dave/rocket/issues/abc", 404)

	pulls := f.get(t, "/gh/dave/rocket/pulls", 200)
	mustContain(t, pulls, "pulls", "Add thrusters", `href="/gh/dave/rocket/pull/5"`)
	pull := f.get(t, "/gh/dave/rocket/pull/5", 200)
	mustContain(t, pull, "pull", "Add thrusters", "Thrusters", "LGTM", "wants to merge", "main", "Files changed", "+10", "−2",
		"feat/&lt;script&gt;alert(4)&lt;/script&gt;")
	ghNoXSS(t, pull, "pull")
	files := f.get(t, "/gh/dave/rocket/pull/5/files", 200)
	mustContain(t, files, "pull files", "thrust.go", "package thrust", `class="dl add"`)
	ghNoXSS(t, files, "pull files")
}

func TestGHImageProxy(t *testing.T) {
	f := newGHFixture(t)
	img := func(u string) *httptest.ResponseRecorder {
		return do(t, f.a, "GET", "/gh/-/img?"+url.Values{"u": {u}}.Encode(), nil, f.ck)
	}
	ok := img("https://camo.githubusercontent.com/ok.png")
	if ok.Code != 200 || ok.Header().Get("Content-Type") != "image/png" || !strings.Contains(ok.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("proxied png = %d %q %q", ok.Code, ok.Header().Get("Content-Type"), ok.Body.String())
	}
	if ok.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(ok.Header().Get("Cache-Control"), "max-age") {
		t.Error("missing safety or cache headers")
	}
	// Cached: a second request doesn't fetch again.
	n := len(f.img.seen())
	img("https://camo.githubusercontent.com/ok.png")
	if len(f.img.seen()) != n {
		t.Error("the image was fetched again")
	}
	// The raw host, from a public repository's README.
	if rec := img("https://raw.githubusercontent.com/dave/rocket/main/img/logo.png"); rec.Code != 200 {
		t.Errorf("raw image = %d %s", rec.Code, rec.Body)
	}
	// github.com/<o>/<r>/raw/… is rewritten to the raw host.
	if rec := img("https://github.com/dave/rocket/raw/main/img/logo.png"); rec.Code != 200 {
		t.Errorf("github.com raw = %d %s", rec.Code, rec.Body)
	}

	// Refused before any fetch: other hosts, other schemes, ports, credentials.
	for _, u := range []string{"https://evil.example/x.png", "http://camo.githubusercontent.com/ok.png", "https://camo.githubusercontent.com:8443/ok.png",
		"https://user:pw@camo.githubusercontent.com/ok.png", "https://github.com/dave/rocket/blob/main/x.png", "javascript:alert(1)",
		"https://raw.githubusercontent.com/dave/rocket/main/../../x.png", "https://camo.githubusercontent.com.evil.example/x.png", ""} {
		if rec := img(u); rec.Code != http.StatusBadRequest {
			t.Errorf("%q = %d, want 400", u, rec.Code)
		}
	}
	// Refused after fetching: too big, not an image, redirected elsewhere.
	for _, p := range []string{"/big.png", "/page.html", "/fake.png", "/away.png", "/missing.png"} {
		if rec := img("https://camo.githubusercontent.com" + p); rec.Code != http.StatusBadGateway {
			t.Errorf("%s = %d, want 502", p, rec.Code)
		}
	}
	// SVG: an attachment with a sandbox CSP.
	svg := img("https://camo.githubusercontent.com/badge.svg")
	if svg.Code != 200 || svg.Header().Get("Content-Type") != "image/svg+xml" || !strings.HasPrefix(svg.Header().Get("Content-Disposition"), "attachment") ||
		!strings.Contains(svg.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("svg = %d %v", svg.Code, svg.Header())
	}

	// Private and loopback addresses are refused, after DNS.
	st := f.a.gh().img
	before := len(f.img.seen())
	for _, ip := range []string{"10.0.0.5", "127.0.0.1", "169.254.169.254", "192.168.1.2", "::1"} {
		st.lookup = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr(ip)}, nil }
		if rec := img("https://user-images.githubusercontent.com/new-" + ip + ".png"); rec.Code != http.StatusBadGateway {
			t.Errorf("%s = %d, want 502", ip, rec.Code)
		}
	}
	if len(f.img.seen()) != before {
		t.Error("a private address was contacted")
	}

	// A private repository's image comes through the API (with the token);
	// the image host is never contacted for it.
	st.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("140.82.112.3")}, nil
	}
	before = len(f.img.seen())
	priv := img("https://raw.githubusercontent.com/zoe-gh/secret/main/img/p.png")
	if priv.Code != 200 || priv.Header().Get("Content-Type") != "image/png" {
		t.Errorf("private image = %d %s", priv.Code, priv.Body)
	}
	if len(f.img.seen()) != before || f.gh.hit("GET /repos/zoe-gh/secret/contents/img/p.png") == 0 {
		t.Error("a private repository's image didn't go through the API")
	}

	// The token never reached an image host.
	for _, r := range f.img.seen() {
		if r.Header.Get("Authorization") != "" || strings.Contains(fmt.Sprint(r.Header), fakeGHToken) || strings.Contains(r.URL.String(), fakeGHToken) {
			t.Fatalf("the image host saw credentials: %v %v", r.URL, r.Header)
		}
	}
}

func TestGHRateLimitShowsCache(t *testing.T) {
	f := newGHFixture(t)
	f.get(t, "/gh/dave/rocket", 200)
	f.get(t, "/gh/dave/rocket/issues/3", 200)
	f.wait()
	// Older than "fresh", then GitHub runs out of requests.
	f.a.gh().fresh = 0
	f.gh.mu.Lock()
	f.gh.limited = true
	f.gh.mu.Unlock()
	body := f.get(t, "/gh/dave/rocket", 200)
	mustContain(t, body, "limited code", "Fast rockets for everyone", "Rocket Readme", "As of", "rate limit", "saved copy")
	hits := f.gh.hit("GET /repos/dave/rocket")
	body = f.get(t, "/gh/dave/rocket/issues/3", 200)
	mustContain(t, body, "limited issue", "It explodes", "Me too!", "As of")
	if f.gh.hit("GET /repos/dave/rocket") != hits {
		t.Error("GitHub was called while rate limited")
	}
	// Nothing cached: a clear error page instead.
	body = expectStatus(t, do(t, f.a, "GET", "/gh/dave/rocket/issues/4", nil, f.ck), http.StatusServiceUnavailable, "uncached")
	mustContain(t, body, "uncached", "rate limit")
	c, _, _ := f.a.tetherClient(context.Background(), f.me(t).ID)
	c.Unblock()
}

func TestGHLinksFromSearchFeedAndLocalCopy(t *testing.T) {
	f := newGHFixture(t)
	a, ctx := f.a, context.Background()
	f.gh.mu.Lock()
	f.gh.search = []tether.Repo{*f.gh.repos["dave/rocket"]}
	f.gh.searchItems = []tether.Issue{{Number: 3, Title: "Engine", State: "open", User: tether.User{Login: "dave"},
		RepositoryURL: f.gh.srv.URL + "/repos/dave/rocket"}}
	f.gh.mu.Unlock()
	f.get(t, "/search?q=rocket&src=github", 200)
	f.wait() // badges are checked in the background
	page := f.get(t, "/search?q=rocket&src=github", 200)
	mustContain(t, page, "search", `href="/gh/dave/rocket"`, `href="/gh/dave"`, `href="/gh/dave/rocket/issues/3"`,
		`href="`+f.gh.srv.URL+`/dave/rocket"`, `href="`+f.gh.srv.URL+`/dave"`, "Open on GitHub", "Visit their studio")

	// The home feed.
	now := time.Now().Add(-time.Hour)
	ev := func(typ, actor, repo, payload string) tether.Event {
		e := tether.Event{ID: strconv.FormatInt(f.gh.id(), 10), Type: typ, Payload: json.RawMessage(payload), CreatedAt: now}
		e.Actor.Login, e.Actor.AvatarURL, e.Repo.Name = actor, "https://avatars.githubusercontent.com/u/5", repo
		return e
	}
	f.gh.mu.Lock()
	f.gh.events = []tether.Event{
		ev("WatchEvent", "alice", "alice/stars", `{}`),
		ev("PullRequestEvent", "bob", "bob/lib", `{"action":"opened","number":7,"pull_request":{"title":"Fix","html_url":"`+f.gh.srv.URL+`/bob/lib/pull/7"}}`),
	}
	f.gh.mu.Unlock()
	if err := a.tetherRefreshFeed(ctx, f.me(t).ID); err != nil {
		t.Fatal(err)
	}
	home := f.get(t, "/", 200)
	mustContain(t, home, "home", `href="/gh/alice/stars"`, `href="/gh/bob/lib/pull/7"`, `href="/gh/alice"`, `href="/gh/zoe-gh"`,
		`href="`+f.gh.srv.URL+`/bob/lib/pull/7"`)

	// Linked in Atelier: the page offers the local copy instead of mirroring.
	if err := a.db.SaveTetherLink(ctx, store.TetherLink{RepoID: f.repoID(t), PersonID: f.me(t).ID, GHRepo: "dave/rocket", Home: store.TetherHomeAtelier}); err != nil {
		t.Fatal(err)
	}
	code := f.get(t, "/gh/dave/rocket", 200)
	mustContain(t, code, "linked", `href="/zoe/demo"`, "Your linked copy")
	mustNotContain(t, code, "linked", "Mirror to studio")

	// Mirror to studio from a /gh/ page returns there on failure.
	rec := do(t, a, "POST", "/search/mirror", url.Values{"repo": {"../x"}, "back": {"/gh/dave/notes"}}, f.ck)
	if loc := rec.Header().Get("Location"); rec.Code != 303 || !strings.HasPrefix(loc, "/gh/dave/notes?err=") {
		t.Errorf("mirror failure went to %d %s", rec.Code, loc)
	}
	rec = do(t, a, "POST", "/search/mirror", url.Values{"repo": {"../x"}, "back": {"https://evil.example/"}}, f.ck)
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/search?") {
		t.Errorf("an outside back link was followed: %s", loc)
	}
}

func TestGHMarkdownRewrite(t *testing.T) {
	a := newTestApp(t)
	a.tether().web = "https://github.com"
	mc := ghMD{Repo: "o/r", Ref: "main", Dir: "docs", Issues: true}
	out := string(a.ghMarkdown("[a](x.md) [b](../README.md) [c](/src/y.go#L3) [d](../../../etc) [e](#top) [f](https://github.com/o/r/pull/9)\n\n"+
		"![i](pic.png) ![j](https://avatars.githubusercontent.com/u/1) ![k](data:image/png;base64,AAAA) ![l](https://github.com/o/r/blob/main/a.png?raw=true)\n\n"+
		`<img src='https://evil.example/a.png' alt="nope"> <a href="https://x" onclick="alert(1)">x</a> fixes #12`, mc))
	for _, want := range []string{`href="/gh/o/r/blob/main/docs/x.md"`, `href="/gh/o/r/blob/main/README.md"`, `href="/gh/o/r/blob/main/src/y.go#L3"`,
		`href="#top"`, `href="/gh/o/r/pull/9"`, url.QueryEscape("https://raw.githubusercontent.com/o/r/main/docs/pic.png"),
		url.QueryEscape("https://avatars.githubusercontent.com/u/1"), url.QueryEscape("https://github.com/o/r/raw/main/a.png"),
		`href="/gh/o/r/issues/12"`, "nope"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	for _, bad := range []string{"etc", "data:image", "evil.example", "onclick", "<a href=\"https://x\""} {
		if strings.Contains(out, bad) {
			t.Errorf("kept %q in\n%s", bad, out)
		}
	}
}
