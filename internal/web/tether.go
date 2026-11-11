package web

// Tether: Atelier's link to GitHub.
//
//	tether.go           the feature, its per-App state, GitHub clients and git plumbing
//	tether_settings.go  Settings → Tether: connect with a token or OAuth, badge, disconnect
//	tether_search.go    /search: your studio first, then GitHub; Mirror to studio
//	tether_feed.go      GitHub activity and "Popular this week" for the home page
//	tether_repo.go      a repository's GitHub twin: link, home, push mirroring and fetching
//	tether_sync.go      issues, pull requests and comments both ways; Merge on GitHub
//	tether_checks.go    Actions results reported to GitHub as commit statuses
//	tether_badge.go     the opt-in "I use Atelier" badge, written and read
//	tether_bridge.go    the tether.Bridge the apps & bots feature calls
//
// Rules: everything from GitHub is untrusted (templates escape it, nothing is
// executed); the token is sealed at rest (internal/secrets), passed to git
// only through GIT_CONFIG_* environment variables and never logged or shown;
// GET requests go through an ETag cache and rate limits are respected.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"atelier/internal/store"
	"atelier/internal/tether"
)

func init() {
	registerFeature(feature{
		name: "tether",
		routes: func(a *App, m *http.ServeMux) {
			tetherBridgeImpl.app.Store(a)
			tether.RegisterBridge(tetherBridgeImpl)
			tetherSettingsRoutes(a, m)
			tetherSearchRoutes(a, m)
			tetherRepoRoutes(a, m)
		},
		start: func(ctx context.Context, a *App) { a.tetherLoop(ctx) },
	})
	registerEventHandler(tetherEventHandler)
	registerPathHandler(tetherMergePathHandler)
}

// Event kinds.
const (
	// EventTetherComment: a comment synced back from GitHub onto an Atelier
	// issue or pull request. Actor is {Kind: "bot", Name: "github:<login>"}.
	EventTetherComment = "tether.comment"
)

// Cache lifetimes.
const (
	tetherFeedTTL     = 15 * time.Minute
	tetherTrendingTTL = 30 * time.Minute
	tetherBadgeTTL    = 24 * time.Hour
	tetherSyncEvery   = 5 * time.Minute
)

// tetherState is the feature's per-App state.
type tetherState struct {
	api     string // GitHub REST API
	web     string // github.com, for OAuth and links
	gitBase string // where twins are cloned from and pushed to: <gitBase>/<owner>/<repo>.git
	hc      *http.Client

	mu        sync.Mutex
	repoLocks map[int64]*sync.Mutex
	busy      map[string]bool // background refreshes in flight
	kicks     chan int64      // repositories to sync now

	bg sync.WaitGroup // background work (tests wait on it)
}

var tetherStates sync.Map // *App → *tetherState

func (a *App) tether() *tetherState {
	if st, ok := tetherStates.Load(a); ok {
		return st.(*tetherState)
	}
	// ATELIER_TETHER_API / _WEB / _GIT point Tether elsewhere (GitHub
	// Enterprise, or a local fake while developing).
	env := func(k, def string) string {
		if v := strings.TrimRight(strings.TrimSpace(os.Getenv(k)), "/"); v != "" {
			return v
		}
		return def
	}
	st, _ := tetherStates.LoadOrStore(a, &tetherState{
		api: env("ATELIER_TETHER_API", tether.DefaultAPI), web: env("ATELIER_TETHER_WEB", "https://github.com"),
		gitBase:   env("ATELIER_TETHER_GIT", "https://github.com"),
		hc:        &http.Client{Timeout: 20 * time.Second},
		repoLocks: map[int64]*sync.Mutex{}, busy: map[string]bool{}, kicks: make(chan int64, 64),
	})
	return st.(*tetherState)
}

// goBG runs f in the background once per key (empty key: always).
func (st *tetherState) goBG(key string, f func()) {
	if key != "" {
		st.mu.Lock()
		if st.busy[key] {
			st.mu.Unlock()
			return
		}
		st.busy[key] = true
		st.mu.Unlock()
	}
	st.bg.Add(1)
	go func() {
		defer st.bg.Done()
		defer func() {
			if key != "" {
				st.mu.Lock()
				delete(st.busy, key)
				st.mu.Unlock()
			}
			if p := recover(); p != nil {
				log.Printf("tether: background work panicked: %v", p)
			}
		}()
		f()
	}()
}

// lockRepo serialises git and sync work on one repository.
func (st *tetherState) lockRepo(id int64) func() {
	st.mu.Lock()
	l := st.repoLocks[id]
	if l == nil {
		l = &sync.Mutex{}
		st.repoLocks[id] = l
	}
	st.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (st *tetherState) kick(repoID int64) {
	select {
	case st.kicks <- repoID:
	default:
	}
}

// ---------- GitHub clients ----------

var errTetherNotConnected = errors.New("connect GitHub in Settings → Tether first")

// tetherCache stores a person's conditional-request validators.
type tetherCache struct {
	db     *store.Store
	person int64
}

func (c tetherCache) Get(ctx context.Context, key string) (string, []byte, bool) {
	etag, body, _, err := c.db.TetherCacheGet(ctx, c.person, key)
	return etag, body, err == nil
}

func (c tetherCache) Put(ctx context.Context, key, etag string, body []byte) {
	if len(body) > 2<<20 {
		return
	}
	_ = c.db.TetherCachePut(context.WithoutCancel(ctx), c.person, key, etag, body)
}

// tetherClient returns a GitHub client with a person's token.
func (a *App) tetherClient(ctx context.Context, personID int64) (*tether.Client, store.TetherAccount, error) {
	acc, err := a.db.TetherAccount(ctx, personID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, acc, errTetherNotConnected
	}
	if err != nil {
		return nil, acc, err
	}
	tok, err := a.db.TetherToken(ctx, a.secretSealer(), personID)
	if err != nil {
		return nil, acc, fmt.Errorf("the saved GitHub token can't be read (was the secret key replaced?): reconnect GitHub in Settings → Tether")
	}
	st := a.tether()
	db := a.db
	c := &tether.Client{API: st.api, Token: tok, HTTP: st.hc, Cache: tetherCache{db: db, person: personID},
		OnRate: func(r tether.Rate) {
			if r.Resource == "" || r.Resource == "core" {
				_ = db.SetTetherRate(context.WithoutCancel(ctx), personID, r.Limit, r.Remaining, r.Reset)
			}
		}}
	return c, acc, nil
}

// tetherErrText turns an error into words for a page (never the token).
func tetherErrText(err error) string {
	var rl *tether.RateLimitError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &rl):
		return "GitHub's rate limit ran out. Tether will try again after " + rl.Reset.UTC().Format("15:04") + " UTC."
	case tether.IsStatus(err, http.StatusUnauthorized):
		return "GitHub didn't accept the token. It may have expired or been revoked: reconnect in Settings → Tether."
	case tether.IsStatus(err, http.StatusNotFound):
		return "GitHub says that doesn't exist, or the token can't see it."
	case tether.IsStatus(err, http.StatusForbidden):
		return "The token doesn't have permission for that. Check its scopes in Settings → Tether."
	}
	return err.Error()
}

// ---------- safe values from GitHub ----------

// tetherAvatar keeps only GitHub avatar URLs (the CSP allows nothing else).
func tetherAvatar(u string) string {
	if strings.HasPrefix(u, "https://avatars.githubusercontent.com/") && !strings.ContainsAny(u, "\"'<> ") {
		return u
	}
	return ""
}

// tetherWebURL is a link on github.com for path (an owner/repo, a login…).
func (a *App) tetherWebURL(path string) string {
	return strings.TrimRight(a.tether().web, "/") + "/" + strings.TrimLeft(path, "/")
}

// tetherHTMLURL keeps a URL GitHub sent only if it points at GitHub's web UI.
func (a *App) tetherHTMLURL(u, fallback string) string {
	web := strings.TrimRight(a.tether().web, "/") + "/"
	if strings.HasPrefix(u, web) && !strings.ContainsAny(u, "\"'<> \n") {
		return u
	}
	return a.tetherWebURL(fallback)
}

// ---------- git ----------

// tetherRemote is the twin's git URL. Credentials never go in it.
func (a *App) tetherRemote(gh string) string {
	base := a.tether().gitBase
	if filepath.IsAbs(base) {
		return filepath.Join(base, filepath.FromSlash(gh)+".git")
	}
	return strings.TrimRight(base, "/") + "/" + gh + ".git"
}

// tetherGitEnv is a clean environment for git, with the token passed as an
// HTTP header through GIT_CONFIG_* (never on the command line).
func tetherGitEnv(token string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") || strings.HasPrefix(kv, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") || strings.HasPrefix(kv, "GIT_ASKPASS=") ||
			strings.HasPrefix(kv, "SSH_ASKPASS=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GCM_INTERACTIVE=never")
	cfg := [][2]string{{"credential.helper", ""}}
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		cfg = append(cfg, [2]string{"http.extraHeader", "Authorization: Basic " + basic})
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// tetherScrub removes the token (raw or as a Basic header) from text.
func tetherScrub(s, token string) string {
	if token == "" {
		return s
	}
	s = strings.ReplaceAll(s, token, "***")
	return strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)), "***")
}

// tetherGit runs git in a repository. stdout is returned even on failure
// (git push --porcelain reports per-ref results there).
func (a *App) tetherGit(ctx context.Context, dir, token string, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.git.Git, args...)
	cmd.Dir = dir
	cmd.Env = tetherGitEnv(token)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := stdout.String()
	if err != nil {
		msg := strings.TrimSpace(tetherScrub(stderr.String(), token))
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out, nil
}

// tetherRefs lists refs under the prefixes as ref → sha.
func (a *App) tetherRefs(ctx context.Context, dir string, prefixes ...string) (map[string]string, error) {
	out, err := a.tetherGit(ctx, dir, "", "", append([]string{"for-each-ref", "--format=%(refname) %(objectname)"}, prefixes...)...)
	if err != nil {
		return nil, err
	}
	return parseRefList(out), nil
}

func parseRefList(s string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if ref, sha, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			m[ref] = sha
		}
	}
	return m
}

func formatRefList(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %s\n", k, m[k])
	}
	return b.String()
}

// tetherIsAncestor reports whether a is an ancestor of (or equal to) b.
func (a *App) tetherIsAncestor(ctx context.Context, dir, anc, desc string) bool {
	_, err := a.tetherGit(ctx, dir, "", "", "merge-base", "--is-ancestor", anc, desc)
	return err == nil
}

// tetherUpdateRef moves ref to sha if it still points at old ("" = must not exist).
func (a *App) tetherUpdateRef(ctx context.Context, dir, ref, sha, old string) error {
	if old == "" {
		old = strings.Repeat("0", 40)
	}
	_, err := a.tetherGit(ctx, dir, "", "", "update-ref", ref, sha, old)
	return err
}

// ---------- the background loop ----------

// tetherLoop refreshes feeds, mirrors pushes, fetches and syncs linked repositories.
func (a *App) tetherLoop(ctx context.Context) {
	st := a.tether()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	lastFull := map[int64]time.Time{}
	var lastPrune time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-st.kicks:
			a.tetherSyncRepo(ctx, id, true)
			lastFull[id] = time.Now()
		case now := <-tick.C:
			links, err := a.db.TetherLinks(ctx)
			if err != nil {
				continue
			}
			for _, l := range links {
				full := now.Sub(lastFull[l.RepoID]) >= tetherSyncEvery
				if full {
					lastFull[l.RepoID] = now
				}
				a.tetherSyncRepo(ctx, l.RepoID, full)
			}
			if accs, err := a.db.TetherAccounts(ctx); err == nil {
				for _, acc := range accs {
					a.tetherRefreshIfStale(ctx, acc.PersonID, "")
				}
			}
			if now.Sub(lastPrune) > 24*time.Hour {
				lastPrune = now
				_ = a.db.PruneTetherCache(ctx, now.Add(-14*24*time.Hour))
			}
		}
	}
}

// tetherEventHandler reacts to Atelier activity on linked repositories.
func tetherEventHandler(ctx context.Context, a *App, ev Event) {
	if ev.RepoID == 0 {
		return
	}
	switch ev.Kind {
	case EventIssueCommented, EventPullCommented:
		a.tetherCommentOut(ctx, ev)
	case EventPullMerged, EventPullClosed:
		if _, err := a.db.TetherLink(ctx, ev.RepoID); err == nil {
			st := a.tether()
			st.kick(ev.RepoID)
			if it, err := a.db.TetherItem(ctx, ev.RepoID, ev.Number); err == nil && it.Origin == "atelier" && it.GHState == "open" {
				bctx := context.WithoutCancel(ctx)
				merged := ev.Kind == EventPullMerged
				st.goBG("", func() {
					sha := ""
					if p, err := a.db.PullByNumber(bctx, ev.RepoID, ev.Number); err == nil {
						sha = p.MergedSHA
					}
					if err := a.tetherClosePull(bctx, ev.RepoID, ev.Number, merged, sha); err != nil {
						log.Printf("tether: closing the GitHub side of #%d: %s", ev.Number, tetherErrText(err))
					}
				})
			}
		}
	case EventRunCompleted, EventRunFailed, EventJobWaiting:
		if l, err := a.db.TetherLink(ctx, ev.RepoID); err == nil && l.Home == store.TetherHomeGitHub {
			st := a.tether()
			st.goBG(fmt.Sprintf("status:%d", ev.RepoID), func() {
				a.tetherReportStatuses(context.WithoutCancel(ctx), l)
			})
		}
	}
}
