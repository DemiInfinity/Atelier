package web

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"atelier/internal/store"
	"atelier/internal/tether"
)

// ---------- a fake GitHub (REST API + bare git repositories on disk) ----------

const fakeGHToken = "github_pat_TESTTOKEN_never_show"

type fakeStatus struct {
	Repo, SHA string
	tether.Status
}

type fakeFile struct {
	data []byte
	sha  string
}

type fakeGH struct {
	t       *testing.T
	srv     *httptest.Server
	gitRoot string

	mu          sync.Mutex
	login       string
	repos       map[string]*tether.Repo
	issues      map[string][]tether.Issue
	comments    map[string][]tether.Comment
	reviews     map[string][]tether.ReviewComment
	pulls       map[string]map[int64]*tether.Pull
	statuses    []fakeStatus
	files       map[string]fakeFile // "owner/repo/path"
	events      []tether.Event
	starred     []tether.Repo
	releases    map[string][]tether.Release
	search      []tether.Repo
	searchItems []tether.Issue
	limited     bool
	// extra answers requests first when set (f.mu held); gh_test.go uses it.
	extra       func(w http.ResponseWriter, r *http.Request, seg []string, body map[string]any) bool
	hits        map[string]int
	notModified int
	nextID      int64

	// More tokens (tether_tokens_test.go): each extra token sees only its
	// owners' repositories; mainDenied hides owners (lower case) from the
	// main token. byToken counts "token METHOD path".
	tokens     map[string]fakeToken
	mainDenied map[string]bool
	byToken    map[string]int

	// Issue sync (tether_issues_test.go): repository labels and milestones,
	// and pull request reviews by number.
	labels      map[string][]tether.Label
	milestones  map[string][]*tether.Milestone
	prReviews   map[string]map[int64][]tether.Review
	lastPayload map[string]map[string]any // "METHOD path" → last JSON body
}

type fakeToken struct {
	login  string
	owners []string
}

func (t fakeToken) covers(owner string) bool {
	for _, o := range t.owners {
		if strings.EqualFold(o, owner) {
			return true
		}
	}
	return false
}

func newFakeGH(t *testing.T) *fakeGH {
	t.Helper()
	f := &fakeGH{t: t, gitRoot: t.TempDir(), login: "zoe-gh", repos: map[string]*tether.Repo{}, issues: map[string][]tether.Issue{},
		comments: map[string][]tether.Comment{}, reviews: map[string][]tether.ReviewComment{}, pulls: map[string]map[int64]*tether.Pull{},
		files: map[string]fakeFile{}, releases: map[string][]tether.Release{}, hits: map[string]int{}, nextID: 1000,
		labels: map[string][]tether.Label{}, milestones: map[string][]*tether.Milestone{}, prReviews: map[string]map[int64][]tether.Review{},
		lastPayload: map[string]map[string]any{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// use points an App's Tether at the fake.
func (f *fakeGH) use(a *App) {
	st := a.tether()
	st.api, st.web, st.gitBase, st.hc = f.srv.URL, f.srv.URL, f.gitRoot, f.srv.Client()
}

func (f *fakeGH) bare(full string) string {
	return filepath.Join(f.gitRoot, filepath.FromSlash(full)+".git")
}

// addRepo creates a twin (with an empty bare git repository).
func (f *fakeGH) addRepo(full string, private bool) *tether.Repo {
	f.t.Helper()
	owner, name, _ := strings.Cut(full, "/")
	if err := os.MkdirAll(f.bare(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	runGit(f.t, f.bare(full), "init", "-q", "--bare", "--initial-branch=main", ".")
	rp := &tether.Repo{ID: f.id(), Name: name, FullName: full, Private: private, DefaultBranch: "main",
		Owner: tether.User{Login: owner, AvatarURL: "https://avatars.githubusercontent.com/u/1"}, Description: "Twin of " + name,
		HTMLURL: f.srv.URL + "/" + full, Stars: 42}
	rp.Permissions = &struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
		Pull  bool `json:"pull"`
	}{true, true, true}
	f.mu.Lock()
	f.repos[full] = rp
	f.mu.Unlock()
	return rp
}

func (f *fakeGH) id() int64 { f.nextID++; return f.nextID }

func (f *fakeGH) hit(k string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[k]
}

func (f *fakeGH) write(w http.ResponseWriter, r *http.Request, code int, v any) {
	b, _ := json.Marshal(v)
	if code == 200 && r.Method == http.MethodGet {
		sum := sha1.Sum(b)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			f.notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

func since(r *http.Request) time.Time {
	t, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	return t
}

func (f *fakeGH) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	f.hits[r.Method+" "+p]++
	if r.URL.Path == "/login/oauth/access_token" {
		f.write(w, r, 200, map[string]string{"access_token": fakeGHToken})
		return
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if f.byToken == nil {
		f.byToken = map[string]int{}
	}
	f.byToken[auth+" "+r.Method+" "+p]++
	var extraTok *fakeToken
	if auth != fakeGHToken {
		ft, ok := f.tokens[auth]
		if !ok || auth == "" {
			f.write(w, r, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		extraTok = &ft
	}
	if f.limited {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		f.write(w, r, 403, map[string]string{"message": "API rate limit exceeded"})
		return
	}
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "4990")
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	w.Header().Set("X-RateLimit-Resource", "core")
	var body map[string]any
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}
	if body != nil {
		f.lastPayload[r.Method+" "+p] = body
	}
	str := func(k string) string { s, _ := body[k].(string); return s }
	seg := strings.Split(strings.Trim(p, "/"), "/")
	if len(seg) >= 2 && seg[0] == "repos" {
		if (extraTok != nil && !extraTok.covers(seg[1])) || (extraTok == nil && f.mainDenied[strings.ToLower(seg[1])]) {
			f.write(w, r, 404, map[string]string{"message": "Not Found"})
			return
		}
	}
	if extraTok != nil && p == "/user" {
		f.write(w, r, 200, tether.User{Login: extraTok.login})
		return
	}
	if extraTok != nil && p == "/user/repos" && r.Method == http.MethodGet {
		var out []tether.Repo
		for _, rp := range f.repos {
			if extraTok.covers(rp.Owner.Login) {
				out = append(out, *rp)
			}
		}
		f.write(w, r, 200, out)
		return
	}
	if f.extra != nil && f.extra(w, r, seg, body) {
		return
	}
	switch {
	case p == "/user":
		f.write(w, r, 200, tether.User{Login: f.login, Name: "Zoe", AvatarURL: "https://avatars.githubusercontent.com/u/9"})
		return
	case p == "/user/repos" && r.Method == http.MethodPost:
		full := f.login + "/" + str("name")
		f.mu.Unlock()
		rp := f.addRepo(full, body["private"] == true)
		f.mu.Lock()
		f.write(w, r, 201, rp)
		return
	case p == "/search/repositories":
		f.write(w, r, 200, map[string]any{"total_count": len(f.search), "items": f.search})
		return
	case p == "/search/issues":
		f.write(w, r, 200, map[string]any{"total_count": len(f.searchItems), "items": f.searchItems})
		return
	case len(seg) == 3 && seg[0] == "users" && seg[2] == "received_events":
		f.write(w, r, 200, f.events)
		return
	case p == "/user/starred":
		f.write(w, r, 200, f.starred)
		return
	}
	if len(seg) < 3 || seg[0] != "repos" {
		f.write(w, r, 404, map[string]string{"message": "Not Found"})
		return
	}
	full := seg[1] + "/" + seg[2]
	rest := seg[3:]
	if len(rest) >= 1 && rest[0] == "contents" {
		key := full + "/" + strings.Join(rest[1:], "/")
		cur, ok := f.files[key]
		switch r.Method {
		case http.MethodGet:
			if !ok {
				f.write(w, r, 404, map[string]string{"message": "Not Found"})
				return
			}
			f.write(w, r, 200, tether.Content{SHA: cur.sha, Encoding: "base64", Content: base64.StdEncoding.EncodeToString(cur.data)})
		case http.MethodPut:
			if _, exists := f.repos[full]; !exists {
				f.write(w, r, 404, map[string]string{"message": "Not Found"})
				return
			}
			if ok && str("sha") != cur.sha {
				f.write(w, r, 409, map[string]string{"message": "sha mismatch"})
				return
			}
			data, _ := base64.StdEncoding.DecodeString(str("content"))
			sum := sha1.Sum(data)
			f.files[key] = fakeFile{data: data, sha: hex.EncodeToString(sum[:])}
			f.write(w, r, 201, map[string]any{})
		case http.MethodDelete:
			if !ok || str("sha") != cur.sha {
				f.write(w, r, 409, map[string]string{"message": "sha mismatch"})
				return
			}
			delete(f.files, key)
			f.write(w, r, 200, map[string]any{})
		}
		return
	}
	rp, ok := f.repos[full]
	if !ok {
		f.write(w, r, 404, map[string]string{"message": "Not Found"})
		return
	}
	if f.serveIssueSync(w, r, full, rest, body) {
		return
	}
	switch {
	case len(rest) == 0:
		f.write(w, r, 200, rp)
	case rest[0] == "releases":
		f.write(w, r, 200, f.releases[full])
	case len(rest) == 1 && rest[0] == "issues":
		var out []tether.Issue
		for _, is := range f.issues[full] {
			if !is.UpdatedAt.Before(since(r)) {
				out = append(out, is)
			}
		}
		f.write(w, r, 200, out)
	case len(rest) == 2 && rest[0] == "issues" && rest[1] == "comments":
		var out []tether.Comment
		for _, c := range f.comments[full] {
			if !c.UpdatedAt.Before(since(r)) {
				out = append(out, c)
			}
		}
		f.write(w, r, 200, out)
	case len(rest) == 2 && rest[0] == "pulls" && rest[1] == "comments":
		var out []tether.ReviewComment
		for _, c := range f.reviews[full] {
			if !c.UpdatedAt.Before(since(r)) {
				out = append(out, c)
			}
		}
		f.write(w, r, 200, out)
	case len(rest) == 3 && rest[0] == "issues" && rest[2] == "comments" && r.Method == http.MethodPost:
		n, _ := strconv.ParseInt(rest[1], 10, 64)
		c := tether.Comment{ID: f.id(), Body: str("body"), User: tether.User{Login: f.login}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
			IssueURL: fmt.Sprintf("%s/repos/%s/issues/%d", f.srv.URL, full, n)}
		f.comments[full] = append(f.comments[full], c)
		f.write(w, r, 201, c)
	case len(rest) == 1 && rest[0] == "pulls" && r.Method == http.MethodGet:
		var out []tether.Pull
		for _, pr := range f.pulls[full] {
			if pr.State == "open" {
				out = append(out, *pr)
			}
		}
		f.write(w, r, 200, out)
	case len(rest) == 1 && rest[0] == "pulls" && r.Method == http.MethodPost:
		for _, pr := range f.pulls[full] {
			if pr.State == "open" && pr.Head.Ref == str("head") {
				f.write(w, r, 422, map[string]string{"message": "A pull request already exists"})
				return
			}
		}
		n := f.addPullLocked(full, str("title"), str("head"), str("base"), f.login, rp)
		pr := f.pulls[full][n]
		pr.Body = str("body")
		f.write(w, r, 201, pr)
	case len(rest) == 2 && rest[0] == "pulls":
		n, _ := strconv.ParseInt(rest[1], 10, 64)
		pr, ok := f.pulls[full][n]
		if !ok {
			f.write(w, r, 404, map[string]string{"message": "Not Found"})
			return
		}
		if r.Method == http.MethodPatch {
			for k, v := range body {
				s, _ := v.(string)
				switch k {
				case "title":
					pr.Title = s
				case "body":
					pr.Body = s
				case "state":
					pr.State = s
				}
			}
		}
		f.write(w, r, 200, pr)
	case len(rest) == 3 && rest[0] == "pulls" && rest[2] == "merge" && r.Method == http.MethodPut:
		n, _ := strconv.ParseInt(rest[1], 10, 64)
		pr := f.pulls[full][n]
		head := strings.TrimSpace(runGit(f.t, f.bare(full), "rev-parse", "refs/heads/"+pr.Head.Ref))
		if s := str("sha"); s != "" && s != head {
			f.write(w, r, 409, map[string]string{"message": "Head branch was modified"})
			return
		}
		runGit(f.t, f.bare(full), "update-ref", "refs/heads/"+pr.Base.Ref, head) // a fast-forward "merge"
		pr.State, pr.Merged, pr.MergeCommitSHA = "closed", true, head
		f.write(w, r, 200, map[string]any{"sha": head, "merged": true})
	case len(rest) == 2 && rest[0] == "statuses" && r.Method == http.MethodPost:
		f.statuses = append(f.statuses, fakeStatus{Repo: full, SHA: rest[1], Status: tether.Status{State: str("state"),
			Context: str("context"), Description: str("description"), TargetURL: str("target_url")}})
		f.write(w, r, 201, map[string]any{})
	case len(rest) == 2 && rest[0] == "branches":
		sha := strings.TrimSpace(runGit(f.t, f.bare(full), "rev-parse", "refs/heads/"+rest[1]))
		f.write(w, r, 200, map[string]any{"commit": map[string]string{"sha": sha}})
	default:
		f.write(w, r, 404, map[string]string{"message": "Not Found"})
	}
}

// addPullLocked records an open pull request (f.mu held).
func (f *fakeGH) addPullLocked(full, title, head, base, author string, headRepo *tether.Repo) int64 {
	if f.pulls[full] == nil {
		f.pulls[full] = map[int64]*tether.Pull{}
	}
	n := int64(len(f.pulls[full]) + len(f.issues[full]) + 1)
	for f.pulls[full][n] != nil {
		n++
	}
	sha := ""
	if out, err := gitTry(f.bare(full), "rev-parse", "refs/heads/"+head); err == nil {
		sha = strings.TrimSpace(out)
	}
	pr := &tether.Pull{Number: n, Title: title, State: "open", HTMLURL: fmt.Sprintf("%s/%s/pull/%d", f.srv.URL, full, n),
		User: tether.User{Login: author}, Head: tether.Branch{Ref: head, SHA: sha, Repo: headRepo},
		Base: tether.Branch{Ref: base, Repo: f.repos[full]}, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	f.pulls[full][n] = pr
	f.issues[full] = append(f.issues[full], tether.Issue{Number: n, Title: title, State: "open", User: tether.User{Login: author},
		PullRequest: &struct{}{}, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	return n
}

// ---------- helpers ----------

type tetherFixture struct {
	gitFixture
	gh   *fakeGH
	ck   map[string]string
	work string
}

func newTetherFixture(t *testing.T) *tetherFixture {
	t.Helper()
	f := &tetherFixture{gitFixture: newGitFixture(t)}
	f.gh = newFakeGH(t)
	f.gh.use(f.a)
	f.ck = pullsCookie(t, f.a)
	f.work = pullsWork(t, f.a)
	t.Cleanup(func() { f.a.tether().bg.Wait(); f.a.actions().bg.Wait() })
	return f
}

func (f *tetherFixture) connect(t *testing.T) {
	t.Helper()
	rec := do(t, f.a, "POST", "/settings/tether/token", url.Values{"token": {fakeGHToken}}, f.ck)
	expectStatus(t, rec, 303, "connect")
	f.wait()
}

func (f *tetherFixture) wait() { f.a.tether().bg.Wait(); f.a.actions().bg.Wait() }

func (f *tetherFixture) me(t *testing.T) store.Owner {
	o, err := f.a.db.PersonalOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (f *tetherFixture) link(t *testing.T, gh, home string, syncIssues bool) store.TetherLink {
	t.Helper()
	form := url.Values{"gh_repo": {gh}, "home": {home}, "create": {"1"}}
	if syncIssues {
		form.Set("sync_issues", "1")
	}
	expectStatus(t, do(t, f.a, "POST", "/zoe/demo/settings/tether/link", form, f.ck), 303, "link")
	f.wait()
	l, err := f.a.db.TetherLink(context.Background(), f.repoID(t))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (f *tetherFixture) sync(t *testing.T) store.TetherLink {
	t.Helper()
	f.a.tetherSyncRepo(context.Background(), f.repoID(t), true)
	f.wait()
	l, _ := f.a.db.TetherLink(context.Background(), f.repoID(t))
	return l
}

func ghSHA(t *testing.T, f *fakeGH, full, ref string) string {
	out, err := gitTry(f.bare(full), "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ---------- tests ----------

func TestTetherConnectEncryptsAndIsPrivate(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()

	body := expectStatus(t, do(t, a, "GET", "/settings/tether", nil, f.ck), 200, "settings")
	mustContain(t, body, "settings", "Connect GitHub", "Contents", "Pull requests", "Issues", "Metadata", "Starring")
	expectStatus(t, do(t, a, "POST", "/settings/tether/token", url.Values{"token": {"github_pat_wrong"}}, f.ck), 422, "bad token")
	f.connect(t)

	// Stored sealed: the plaintext isn't in the database file or the table.
	var ct []byte
	if err := a.db.DB().QueryRow(`SELECT token_ct FROM tether_accounts`).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ct), fakeGHToken) || len(ct) < 20 {
		t.Fatal("the token is stored in the clear")
	}
	if tok, err := a.db.TetherToken(ctx, a.secretSealer(), f.me(t).ID); err != nil || tok != fakeGHToken {
		t.Fatalf("TetherToken = %q, %v", tok, err)
	}
	// Bound to its person: the ciphertext doesn't open for anyone else.
	if _, err := a.secretSealer().Open([]byte("atelier-tether\x0099"), ct); err == nil {
		t.Error("the token opened with another person's binding")
	}
	body = expectStatus(t, do(t, a, "GET", "/settings/tether", nil, f.ck), 200, "linked")
	mustContain(t, body, "linked", "zoe-gh", "4990 of 5000", "Disconnect", "badge")
	if strings.Contains(body, fakeGHToken) {
		t.Fatal("the token was rendered")
	}

	// A second person sees none of it.
	sam, err := a.db.CreateOwner(ctx, "sam", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := a.db.CreateSessionFor(ctx, sam.ID, time.Hour)
	samCk := map[string]string{"Cookie": sessionCookie + "=" + sess}
	body = expectStatus(t, do(t, a, "GET", "/settings/tether", nil, samCk), 200, "sam settings")
	if strings.Contains(body, "zoe-gh") || !strings.Contains(body, "Connect GitHub") {
		t.Error("sam saw zoe's Tether account")
	}
	if _, err := a.db.TetherToken(ctx, a.secretSealer(), sam.ID); err == nil {
		t.Error("sam has a token")
	}
	f.gh.search = []tether.Repo{*f.gh.addRepo("octo/thing", false)}
	body = expectStatus(t, do(t, a, "GET", "/search?q=thing", nil, samCk), 200, "sam search")
	if strings.Contains(body, "octo") || !strings.Contains(body, "Connect GitHub in") {
		t.Error("sam's search used zoe's GitHub")
	}
	body = expectStatus(t, do(t, a, "GET", "/", nil, samCk), 200, "sam home")
	if strings.Contains(body, "zoe-gh") {
		t.Error("sam's home shows zoe's Tether")
	}
	if rec := do(t, a, "GET", "/zoe/demo/settings/tether", nil, samCk); rec.Code != 404 && rec.Code != 403 {
		t.Errorf("sam opened zoe's repo Tether page: %d", rec.Code)
	}
	if rec := do(t, a, "GET", "/zoe/demo/tether/status", nil, samCk); rec.Code != 404 {
		t.Errorf("sam read zoe's repo status: %d", rec.Code)
	}

	expectStatus(t, do(t, a, "POST", "/settings/tether/disconnect", url.Values{}, f.ck), 303, "disconnect")
	if _, err := a.db.TetherAccount(ctx, f.me(t).ID); err == nil {
		t.Error("still connected after Disconnect")
	}
}

func TestTetherOAuthConnect(t *testing.T) {
	f := newTetherFixture(t)
	f.a.cfg.GitHubClientID, f.a.cfg.GitHubClientSecret, f.a.cfg.GitHubLogin = "cid", "csecret", "zoe-gh"
	rec := do(t, f.a, "GET", "/settings/tether/oauth?private=1", nil, f.ck)
	loc := rec.Header().Get("Location")
	if rec.Code != 303 || !strings.Contains(loc, "scope=read%3Auser+repo") || !strings.Contains(loc, "callback%2Ftether") {
		t.Fatalf("oauth start = %d %s", rec.Code, loc)
	}
	u, _ := url.Parse(loc)
	state := u.Query().Get("state")
	var stateCookie string
	for _, c := range rec.Result().Cookies() {
		if c.Name == tetherStateCookie {
			stateCookie = c.Value
		}
	}
	ck := map[string]string{"Cookie": f.ck["Cookie"] + "; " + tetherStateCookie + "=" + stateCookie}
	expectStatus(t, do(t, f.a, "GET", "/auth/github/callback/tether?code=abc&state=nope", nil, ck), 400, "bad state")
	expectStatus(t, do(t, f.a, "GET", "/auth/github/callback/tether?code=abc&state="+state, nil, ck), 303, "callback")
	acc, err := f.a.db.TetherAccount(context.Background(), f.me(t).ID)
	if err != nil || acc.Method != "oauth" || !acc.PrivateMirror || acc.Login != "zoe-gh" {
		t.Fatalf("oauth account = %+v, %v", acc, err)
	}
	// Without opting in to private mirroring, only public_repo is asked for.
	rec = do(t, f.a, "GET", "/settings/tether/oauth", nil, f.ck)
	if l := rec.Header().Get("Location"); strings.Contains(l, "+repo&") || !strings.Contains(l, "public_repo") {
		t.Errorf("public oauth scope = %s", l)
	}
}

func TestTetherFeedTrendingOfflineAndBackoff(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	f.connect(t)
	me := f.me(t).ID
	now := time.Now().Add(-time.Hour)
	ev := func(typ, actor, repo, payload string) tether.Event {
		e := tether.Event{ID: strconv.FormatInt(f.gh.id(), 10), Type: typ, Payload: json.RawMessage(payload), CreatedAt: now}
		e.Actor.Login, e.Actor.AvatarURL, e.Repo.Name = actor, "https://avatars.githubusercontent.com/u/5", repo
		return e
	}
	f.gh.mu.Lock()
	f.gh.events = []tether.Event{
		ev("WatchEvent", "alice", "alice/stars", `{}`),
		ev("PullRequestEvent", "bob", "bob/lib", `{"action":"opened","number":7,"pull_request":{"title":"<script>alert(1)</script> Fix it","html_url":"javascript:alert(1)"}}`),
		ev("CreateEvent", "bob", "bob/lib", `{"ref_type":"branch"}`),
	}
	f.gh.starred = []tether.Repo{{FullName: "carol/tool", Name: "tool", Owner: tether.User{Login: "carol"}, PushedAt: time.Now()}}
	f.gh.releases["carol/tool"] = []tether.Release{{TagName: "v2.0", Name: "Tool 2", PublishedAt: time.Now().Add(-2 * time.Hour), HTMLURL: f.gh.srv.URL + "/carol/tool/releases/v2.0"}}
	f.gh.search = []tether.Repo{{FullName: "dave/rocket", Name: "rocket", Stars: 900, Language: "Go", Description: "Fast",
		Owner: tether.User{Login: "dave", AvatarURL: "https://evil.example/x.png"}}}
	f.gh.mu.Unlock()
	f.gh.mu.Lock()
	f.gh.repos["carol/tool"] = &tether.Repo{FullName: "carol/tool"}
	f.gh.mu.Unlock()

	if err := a.tetherRefreshFeed(ctx, me); err != nil {
		t.Fatal(err)
	}
	if err := a.tetherRefreshTrending(ctx, me, ""); err != nil {
		t.Fatal(err)
	}
	body := expectStatus(t, do(t, a, "GET", "/", nil, f.ck), 200, "home")
	mustContain(t, body, "home", "tt-feed", "via Tether", "starred", "alice/stars", "opened pull request #7", "Tool 2",
		"Popular this week", "dave/rocket", "★ 900", "GitHub as of")
	if strings.Contains(body, "<script>alert(1)") || strings.Contains(body, `href="javascript:`) || strings.Contains(body, "evil.example") {
		t.Fatal("untrusted GitHub content reached the page unescaped")
	}
	if strings.Contains(body, "created a branch") {
		t.Error("branch creations should be filtered out")
	}

	// A second refresh revalidates with ETags (304s).
	before := f.gh.notModified
	if err := a.tetherRefreshFeed(ctx, me); err != nil {
		t.Fatal(err)
	}
	if f.gh.notModified <= before {
		t.Error("the refresh didn't use conditional requests")
	}

	// Rate limited: the refresh fails cleanly, the client backs off, the cache stays.
	f.gh.mu.Lock()
	f.gh.limited = true
	f.gh.mu.Unlock()
	err := a.tetherRefreshFeed(ctx, me)
	if err == nil || !strings.Contains(tetherErrText(err), "rate limit") {
		t.Fatalf("limited refresh = %v", err)
	}
	hits := f.gh.hit("GET /users/zoe-gh/received_events")
	if err := a.tetherRefreshFeed(ctx, me); err == nil {
		t.Fatal("blocked refresh succeeded")
	}
	if f.gh.hit("GET /users/zoe-gh/received_events") != hits {
		t.Error("Tether kept calling GitHub while rate limited")
	}
	c, _, _ := a.tetherClient(ctx, me)
	c.Unblock()

	// Offline: GitHub is gone, the home page still shows what's cached.
	f.gh.srv.Close()
	body = expectStatus(t, do(t, a, "GET", "/", nil, f.ck), 200, "offline home")
	mustContain(t, body, "offline home", "alice/stars", "dave/rocket", "as of")
	f.wait()
}

func TestTetherSearchMirrorAndBadges(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	f.connect(t)
	pullsCommit(t, f.work, "main.go", "package main\n// the rocket launcher\n", "Initial")
	runGit(t, f.work, "push", "-q", "origin", "main")
	if _, err := a.db.CreateIssue(ctx, f.repoID(t), "Rocket is slow", "", ActorOwner, "zoe"); err != nil {
		t.Fatal(err)
	}
	pub := f.gh.addRepo("dave/rocket", false)
	priv := f.gh.addRepo("zoe-gh/secret-rocket", true)
	for _, full := range []string{"dave/rocket", "zoe-gh/secret-rocket"} {
		w := t.TempDir()
		runGit(t, w, "init", "-q", "--initial-branch=main", ".")
		pullsCommit(t, w, "README.md", "# "+full+"\n", "first")
		runGit(t, w, "push", "-q", f.gh.bare(full), "main")
	}
	f.gh.mu.Lock()
	f.gh.search = []tether.Repo{*pub, *priv}
	f.gh.searchItems = []tether.Issue{{Number: 3, Title: "Rocket crash", State: "open", User: tether.User{Login: "dave"},
		RepositoryURL: f.gh.srv.URL + "/repos/dave/rocket"}}
	body, _ := json.Marshal(tetherBadgeFileJSON{Atelier: true, Studio: "https://studio.dave.dev/dave"})
	f.gh.files["dave/dave/.atelier"] = fakeFile{data: body, sha: "abc"}
	f.gh.repos["dave/dave"] = &tether.Repo{FullName: "dave/dave"}
	f.gh.mu.Unlock()

	page := expectStatus(t, do(t, a, "GET", "/search?q=rocket", nil, f.ck), 200, "search")
	mustContain(t, page, "search", "In your studio", "zoe/demo", "Rocket is slow", "main.go", "rocket launcher",
		"From GitHub via Tether", "rocket", "Mirror to studio", "Open on GitHub", "Rocket crash")
	f.wait() // badge checks run in the background
	page = expectStatus(t, do(t, a, "GET", "/search?q=rocket&src=github", nil, f.ck), 200, "search again")
	mustContain(t, page, "badges", "uses Atelier", "Visit their studio", "https://studio.dave.dev/dave")
	if strings.Contains(page, "Rocket is slow") {
		t.Error("src=github showed studio results")
	}
	studio := expectStatus(t, do(t, a, "GET", "/search?q=rocket&src=studio", nil, f.ck), 200, "studio only")
	if strings.Contains(studio, "From GitHub via Tether") {
		t.Error("src=studio showed GitHub results")
	}

	// Mirror a public repository: an ordinary import mirror, no credentials kept.
	expectStatus(t, do(t, a, "POST", "/search/mirror", url.Values{"repo": {"dave/rocket"}, "q": {"rocket"}}, f.ck), 303, "mirror public")
	waitFor(t, func() bool { _, err := a.db.RepoByName(ctx, "zoe", "rocket"); return err == nil })
	rp, _ := a.db.RepoByName(ctx, "zoe", "rocket")
	m, err := a.db.ImportMirror(ctx, rp.ID)
	if err != nil || m.Secret != "" {
		t.Fatalf("public mirror = %+v, %v", m, err)
	}
	// Mirror a private one: cloned once with the token, then kept up to date by a Tether link.
	expectStatus(t, do(t, a, "POST", "/search/mirror", url.Values{"repo": {"zoe-gh/secret-rocket"}}, f.ck), 303, "mirror private")
	f.wait()
	sr, err := a.db.RepoByName(ctx, "zoe", "secret-rocket")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ImportMirror(ctx, sr.ID); err == nil {
		t.Error("a private mirror stored the token in import_mirrors")
	}
	if l, err := a.db.TetherLink(ctx, sr.ID); err != nil || l.Home != store.TetherHomeGitHub || l.GHRepo != "zoe-gh/secret-rocket" {
		t.Errorf("private mirror link = %+v, %v", l, err)
	}
	// Already there: the button turns into a link.
	page = expectStatus(t, do(t, a, "GET", "/search?q=rocket&src=github", nil, f.ck), 200, "search after mirror")
	mustContain(t, page, "after mirror", "In your studio")
	if rec := do(t, a, "POST", "/search/mirror", url.Values{"repo": {"../etc"}}, f.ck); rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "err=") {
		t.Error("an invalid repository name was accepted")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("timed out waiting")
}

func TestTetherBadgeWriteAndRead(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	a.cfg.PublicURL = "https://atelier.example"
	f.connect(t)
	f.gh.addRepo("zoe-gh/zoe-gh", false)
	expectStatus(t, do(t, a, "POST", "/settings/tether/badge", url.Values{"on": {"1"}}, f.ck), 303, "badge on")
	f.gh.mu.Lock()
	file, ok := f.gh.files["zoe-gh/zoe-gh/.atelier"]
	f.gh.mu.Unlock()
	var got tetherBadgeFileJSON
	if !ok || json.Unmarshal(file.data, &got) != nil || !got.Atelier || got.Studio != "https://atelier.example/zoe" {
		t.Fatalf("badge file = %q", file.data)
	}
	if acc, _ := a.db.TetherAccount(ctx, f.me(t).ID); !acc.Badge {
		t.Error("badge flag not saved")
	}
	expectStatus(t, do(t, a, "POST", "/settings/tether/badge", url.Values{"on": {"0"}}, f.ck), 303, "badge off")
	f.gh.mu.Lock()
	_, ok = f.gh.files["zoe-gh/zoe-gh/.atelier"]
	f.gh.mu.Unlock()
	if ok {
		t.Error("badge file not removed")
	}

	// Reading other people's: cached, unsafe studio links dropped.
	c, _, _ := a.tetherClient(ctx, f.me(t).ID)
	f.gh.mu.Lock()
	f.gh.files["eve/eve/.atelier"] = fakeFile{data: []byte(`{"atelier":true,"studio":"javascript:alert(1)"}`), sha: "e1"}
	f.gh.mu.Unlock()
	b, err := a.tetherCheckBadge(ctx, c, "eve")
	if err != nil || !b.Present || b.Studio != "" {
		t.Fatalf("eve badge = %+v, %v", b, err)
	}
	if b, err := a.tetherCheckBadge(ctx, c, "nobody"); err != nil || b.Present {
		t.Errorf("no badge = %+v, %v", b, err)
	}
	hits := f.gh.hit("GET /repos/eve/eve/contents/.atelier")
	got2 := a.tetherBadges(ctx, f.me(t).ID, []string{"eve", "eve"})
	f.wait()
	if !got2["eve"].Present || f.gh.hit("GET /repos/eve/eve/contents/.atelier") != hits {
		t.Error("a fresh cached badge was fetched again")
	}
	// Without a profile repository, a clear error.
	f.gh.mu.Lock()
	delete(f.gh.repos, "zoe-gh/zoe-gh")
	f.gh.mu.Unlock()
	body := expectStatus(t, do(t, a, "POST", "/settings/tether/badge", url.Values{"on": {"1"}}, f.ck), 502, "no profile repo")
	mustContain(t, body, "no profile repo", "create your profile repository")
}

func TestTetherPushMirrorAndHome(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	f.connect(t)
	pullsCommit(t, f.work, "README.md", "# demo\n", "Initial")
	runGit(t, f.work, "push", "-q", "origin", "main")
	runGit(t, f.work, "checkout", "-q", "-b", "feature")
	pullsCommit(t, f.work, "f.txt", "f\n", "Feature")
	runGit(t, f.work, "push", "-q", "origin", "feature")
	runGit(t, f.work, "checkout", "-q", "main")

	// Link, creating the twin as a private repository.
	l := f.link(t, "zoe-gh/demo", "atelier", false)
	f.gh.mu.Lock()
	twin := f.gh.repos["zoe-gh/demo"]
	f.gh.mu.Unlock()
	if twin == nil || !twin.Private {
		t.Fatalf("twin = %+v", twin)
	}
	g, _ := a.git.Open("zoe", "demo")
	main, _ := g.BranchSHA(ctx, "main")
	if ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/main") != main || ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/feature") == "" {
		t.Fatalf("push mirror: main=%s twin=%s, link %+v", main, ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/main"), l)
	}
	// Nothing changed: no push.
	before := l.PushedRefs
	if l = f.sync(t); l.PushedRefs != before || l.PushError != "" {
		t.Errorf("idle sync changed things: %+v", l)
	}
	// A deleted branch is deleted on GitHub too.
	runGit(t, f.work, "push", "-q", "origin", ":feature")
	f.sync(t)
	if ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/feature") != "" {
		t.Error("deleted branch still on GitHub")
	}

	// GitHub's main moves on its own: Atelier never force-pushes over it.
	other := t.TempDir()
	runGit(t, other, "clone", "-q", f.gh.bare("zoe-gh/demo"), ".")
	pullsCommit(t, other, "gh.txt", "from github\n", "On GitHub")
	runGit(t, other, "push", "-q", "origin", "main")
	ghMain := ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/main")
	pullsCommit(t, f.work, "here.txt", "here\n", "In Atelier")
	runGit(t, f.work, "push", "-q", "origin", "main")
	l = f.sync(t)
	if ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/main") != ghMain || l.Diverged == "" {
		t.Fatalf("diverged main: twin %s (want %s), link %+v", ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/main"), ghMain, l)
	}
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/settings/tether", nil, f.ck), 200, "repo page")
	mustContain(t, body, "repo page", "The default branches differ", "Keep Atelier", "Keep GitHub", "Protect main on GitHub",
		"Restrict who can push", "Home: Atelier")
	if strings.Contains(body, fakeGHToken) {
		t.Fatal("token on the repo page")
	}

	// Switching home needs identical mains.
	body = expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/tether/home", url.Values{"home": {"github"}}, f.ck), 409, "switch blocked")
	mustContain(t, body, "switch blocked", "must be identical")
	// Keep GitHub's: Atelier's main moves (the old tip is backed up).
	mine, _ := g.BranchSHA(ctx, "main")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/tether/reconcile", url.Values{"keep": {"github"}}, f.ck), 303, "reconcile")
	if now, _ := g.BranchSHA(ctx, "main"); now != ghMain {
		t.Fatalf("after keep=github main = %s, want %s", now, ghMain)
	}
	if refs, _ := a.tetherRefs(ctx, a.git.Path("zoe", "demo"), "refs/tether/backup"); len(refs) != 1 {
		t.Errorf("backup refs = %v (old tip %s)", refs, mine)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/tether/home", url.Values{"home": {"github"}}, f.ck), 303, "switch")
	if l, _ = a.db.TetherLink(ctx, f.repoID(t)); l.Home != store.TetherHomeGitHub {
		t.Fatalf("home = %s", l.Home)
	}

	// Home: GitHub — new GitHub commits are fetched and handed to Actions.
	_, _ = a.db.TakeActionsPushes(ctx, 100)
	runGit(t, other, "pull", "-q", "origin", "main")
	pullsCommit(t, other, "gh2.txt", "more\n", "More on GitHub")
	runGit(t, other, "push", "-q", "origin", "main")
	runGit(t, other, "push", "-q", "origin", "main:gh-only")
	l = f.sync(t)
	want := ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/main")
	if now, _ := g.BranchSHA(ctx, "main"); now != want || l.FetchError != "" {
		t.Fatalf("fetch: main %s want %s (%+v)", now, want, l)
	}
	if sha, _ := g.BranchSHA(ctx, "gh-only"); sha != want {
		t.Error("GitHub-only branch not fetched")
	}
	pushes, _ := a.db.TakeActionsPushes(ctx, 100)
	found := false
	for _, p := range pushes {
		if p.Ref == "refs/heads/main" && p.New == want && p.Pusher == "bot:github" {
			found = true
		}
	}
	if !found {
		t.Errorf("fetched update not queued for Actions: %+v", pushes)
	}
	// Status JSON for the repo card.
	st := expectStatus(t, do(t, a, "GET", "/zoe/demo/tether/status", nil, f.ck), 200, "status")
	mustContain(t, st, "status", `"linked":true`, `"gh_repo":"zoe-gh/demo"`, `"home":"github"`)
	code := expectStatus(t, do(t, a, "GET", "/zoe/demo", nil, f.ck), 200, "code page")
	mustContain(t, code, "code page", "data-tether-card", "/static/tether.js")

	// Keep Atelier's: force-push after confirmation.
	pullsCommit(t, other, "x.txt", "x\n", "X")
	runGit(t, other, "push", "-q", "origin", "main")
	if err := a.tetherReconcile(ctx, f.mustRepo(t), "atelier"); err != nil {
		t.Fatal(err)
	}
	if now, _ := g.BranchSHA(ctx, "main"); ghSHA(t, f.gh, "zoe-gh/demo", "refs/heads/main") != now {
		t.Error("keep=atelier didn't make GitHub match")
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/settings/tether/unlink", url.Values{}, f.ck), 303, "unlink")
	if _, ok, _ := tether.Get().Linked(ctx, f.repoID(t)); ok {
		t.Error("still linked")
	}
}

func (f *tetherFixture) mustRepo(t *testing.T) store.Repo {
	rp, err := f.a.db.RepoByName(context.Background(), "zoe", "demo")
	if err != nil {
		t.Fatal(err)
	}
	return rp
}

func TestTetherGitEnvKeepsTokenOffArgv(t *testing.T) {
	env := tetherGitEnv("tok123")
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_KEY_1=http.extraHeader") || strings.Contains(joined, "tok123") {
		t.Fatalf("env = %v", env)
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:tok123"))
	if s := tetherScrub("fatal: tok123 and "+basic, "tok123"); strings.Contains(s, "tok123") || strings.Contains(s, basic) {
		t.Errorf("scrub = %q", s)
	}
}

func TestTetherIssueAndCommentSync(t *testing.T) {
	f := newTetherFixture(t)
	a, ctx := f.a, context.Background()
	f.connect(t)
	pullsCommit(t, f.work, "README.md", "# demo\n", "Initial")
	runGit(t, f.work, "push", "-q", "origin", "main")
	f.gh.addRepo("zoe-gh/demo", true)
	runGit(t, f.work, "push", "-q", f.gh.bare("zoe-gh/demo"), "main")
	t0 := time.Now().Add(-time.Hour)
	f.gh.mu.Lock()
	f.gh.issues["zoe-gh/demo"] = []tether.Issue{{Number: 3, Title: "Crash on start", Body: "It crashes", State: "open",
		User: tether.User{Login: "alice"}, CreatedAt: t0, UpdatedAt: t0, HTMLURL: f.gh.srv.URL + "/zoe-gh/demo/issues/3"}}
	f.gh.comments["zoe-gh/demo"] = []tether.Comment{{ID: 501, Body: "Same here", User: tether.User{Login: "bob"}, CreatedAt: t0, UpdatedAt: t0,
		IssueURL: f.gh.srv.URL + "/repos/zoe-gh/demo/issues/3"}}
	f.gh.mu.Unlock()

	f.link(t, "zoe-gh/demo", "atelier", true)
	is, err := a.db.IssueByNumber(ctx, f.repoID(t), 3)
	if err != nil || is.AuthorKind != "import" || is.AuthorName != "github:alice" || is.Title != "Crash on start" {
		t.Fatalf("synced issue = %+v, %v", is, err)
	}
	cs, _ := a.db.IssueComments(ctx, is.ID)
	if len(cs) != 1 || cs[0].AuthorName != "github:bob" || cs[0].Body != "Same here" {
		t.Fatalf("synced comments = %+v", cs)
	}
	ns, _ := a.db.Notifications(ctx, store.NotificationQuery{PersonID: f.me(t).ID, SubjectTypes: []string{"tether"}})
	if len(ns) != 1 || ns[0].Kind != EventTetherComment || ns[0].ActorKind != "bot" || ns[0].ActorName != "github:bob" {
		t.Fatalf("tether notifications = %+v", ns)
	}
	body := expectStatus(t, do(t, a, "GET", "/notifications?filter=tether", nil, f.ck), 200, "inbox")
	mustContain(t, body, "inbox", "Crash on start", "github:bob", "nf-tether")

	// Comment in Atelier: it goes to GitHub, and doesn't come back twice.
	expectStatus(t, do(t, a, "POST", "/zoe/demo/issues/3/comment", url.Values{"body": {"Fixed in main"}}, f.ck), 303, "comment")
	f.wait()
	f.gh.mu.Lock()
	gcs := f.gh.comments["zoe-gh/demo"]
	f.gh.mu.Unlock()
	if len(gcs) != 2 || gcs[1].Body != "Fixed in main" {
		t.Fatalf("GitHub comments = %+v", gcs)
	}
	f.sync(t)
	if cs, _ = a.db.IssueComments(ctx, is.ID); len(cs) != 2 {
		t.Fatalf("after round trip, %d comments", len(cs))
	}
	// Closed on GitHub → closed here; a new GitHub comment arrives once.
	f.gh.mu.Lock()
	f.gh.issues["zoe-gh/demo"][0].State, f.gh.issues["zoe-gh/demo"][0].UpdatedAt = "closed", time.Now()
	f.gh.comments["zoe-gh/demo"] = append(f.gh.comments["zoe-gh/demo"], tether.Comment{ID: 502, Body: "Thanks!", User: tether.User{Login: "alice"},
		CreatedAt: time.Now(), UpdatedAt: time.Now(), IssueURL: f.gh.srv.URL + "/repos/zoe-gh/demo/issues/3"})
	f.gh.mu.Unlock()
	f.sync(t)
	f.sync(t)
	is, _ = a.db.IssueByNumber(ctx, f.repoID(t), 3)
	cs, _ = a.db.IssueComments(ctx, is.ID)
	if is.State != "closed" || len(cs) != 3 {
		t.Fatalf("after close: state %s, %d comments", is.State, len(cs))
	}
	// Numbers taken in Atelier don't clash: GitHub #1 lands on a free number.
	if _, err := a.db.CreateIssue(ctx, f.repoID(t), "Local one", "", ActorOwner, "zoe"); err != nil {
		t.Fatal(err)
	}
	f.gh.mu.Lock()
	f.gh.issues["zoe-gh/demo"] = append(f.gh.issues["zoe-gh/demo"], tether.Issue{Number: 4, Title: "Another", State: "open",
		User: tether.User{Login: "alice"}, CreatedAt: time.Now(), UpdatedAt: time.Now().Add(time.Second)})
	f.gh.mu.Unlock()
	f.sync(t)
	it, err := a.db.TetherItemByGH(ctx, f.repoID(t), 4)
	if err != nil || it.Number == 4 {
		t.Fatalf("GitHub #4 = %+v, %v", it, err)
	}
	if got, _ := a.db.IssueByNumber(ctx, f.repoID(t), it.Number); got.Title != "Another" {
		t.Errorf("GitHub #4 landed on #%d = %q", it.Number, got.Title)
	}
}
