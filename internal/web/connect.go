package web

// Atelier Connect (ticket #6): independent studios following each other,
// starring and offering branches, with no central server. The protocol
// pieces (keys, HTTP Signatures, actor documents, the SSRF-guarded client)
// are in internal/connect; this file holds the studio's identity and its
// public endpoints, connect_inbox.go what arrives, connect_outbox.go what
// leaves, connect_ui.go Settings → Connect and the feed, connect_pull.go
// branches offered by other studios.
//
// Rules:
//   - Off by default. Nothing is served (404) and nothing is sent until the
//     owner turns Connect on AND the studio has a public address: the
//     tunnel (with the Connect class exposed) or a public ATELIER_PUBLIC_URL.
//   - Only the personal owner and company accounts are actors. A second
//     local person is not, and only the personal owner can change Connect.
//   - Only public repositories federate. Private ones are never announced,
//     never served to other studios, and a queued delivery about a
//     repository that has since gone private is dropped.
//   - Federation never grants anything on your repositories: remote people
//     are not Atelier people and can't sign in. Remote pull requests wait for
//     the owner's approval before any Actions run.
//
// URL layout (all under the studio's public address):
//
//	/.well-known/webfinger?resource=acct:<name>@<host>
//	/connect/users/<name>                Person (you) or Organization (a company account)
//	/connect/users/<name>/inbox|outbox|followers|following
//	/connect/repos/<owner>/<repo>        ForgeFed Repository (public repositories only)
//	/connect/repos/<owner>/<repo>/inbox
//	/connect/inbox                       shared inbox
//	/connect/git/<owner>/<repo>.git/…    read-only clone of public repositories, for offers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"atelier/internal/backup"
	"atelier/internal/connect"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

func init() {
	backup.RegisterDataDir(connect.KeyDir) // the studio key travels with backups
	registerPathHandler(func(a *App, w http.ResponseWriter, r *http.Request) bool { return a.connectServe(w, r) })
	registerFeature(feature{
		name:   "connect",
		routes: connectRoutes,
		start:  func(ctx context.Context, a *App) { a.connectLoop(ctx) },
	})
}

const (
	connectSkew       = 5 * time.Minute // allowed clock difference on signed requests
	connectActorTTL   = 24 * time.Hour  // how long a remote actor document is trusted
	connectRatePerMin = 120             // inbound activities per remote studio per minute
	connectUserAgent  = "Atelier-Connect/1.0"
	connectFeedShown  = 40
	connectLogShown   = 60
)

// connectState is Connect's per-App state.
type connectState struct {
	keyMu sync.Mutex
	key   *connect.Key

	// allowLocal lets two studios on one machine (tests, local development)
	// talk over http:// and loopback. ATELIER_CONNECT_ALLOW_LOCAL=1 sets it.
	allowLocal bool
	now        func() time.Time

	wake        chan struct{}
	hostRate    *limiter // per remote studio
	ipRate      *limiter // per client address, before any signature work
	backoff     func(n int) time.Duration
	maxAttempts int

	deliverMu sync.Mutex
	bg        sync.WaitGroup
}

var connectStates sync.Map // *App → *connectState

func (a *App) connect() *connectState {
	if st, ok := connectStates.Load(a); ok {
		return st.(*connectState)
	}
	st := &connectState{
		allowLocal: os.Getenv("ATELIER_CONNECT_ALLOW_LOCAL") == "1",
		now:        time.Now,
		wake:       make(chan struct{}, 1),
		hostRate:   newLimiter(connectRatePerMin, time.Minute),
		ipRate:     newLimiter(5*connectRatePerMin, time.Minute),
		backoff: func(n int) time.Duration {
			d := 30 * time.Second // 30s, 2m, 8m, 32m, ~2h, then every 6h
			for i := 1; i < n && d < 6*time.Hour; i++ {
				d *= 4
			}
			return d
		},
		maxAttempts: 8,
	}
	got, _ := connectStates.LoadOrStore(a, st)
	return got.(*connectState)
}

func (st *connectState) signal() {
	select {
	case st.wake <- struct{}{}:
	default:
	}
}

// connectKey loads (or creates) the studio key.
func (a *App) connectKey() (*connect.Key, error) {
	st := a.connect()
	st.keyMu.Lock()
	defer st.keyMu.Unlock()
	if st.key != nil {
		return st.key, nil
	}
	k, err := connect.LoadOrCreateKey(filepath.Join(a.cfg.DataDir, connect.KeyDir, connect.KeyFile))
	if err != nil {
		return nil, err
	}
	st.key = k
	return k, nil
}

// connectFingerprint is a short, readable fingerprint of the studio key.
func connectFingerprint(k *connect.Key) string {
	sum := sha256.Sum256(k.Pub)
	h := hex.EncodeToString(sum[:])[:32]
	var parts []string
	for i := 0; i < len(h); i += 4 {
		parts = append(parts, h[i:i+4])
	}
	return strings.Join(parts, ":")
}

// ---------- address ----------

// connectBase is the studio's public address, and why there is none: a
// public ATELIER_PUBLIC_URL (stable) or the tunnel with Connect exposed.
func (a *App) connectBase(ctx context.Context) (base string, why string) {
	c, err := a.db.ConnectSettings(ctx)
	if err != nil {
		return "", "Connect's settings couldn't be read."
	}
	return a.connectBaseFor(c)
}

func (a *App) connectBaseFor(c store.ConnectSettings) (string, string) {
	st := a.connect()
	if pu := strings.TrimRight(a.cfg.PublicURL, "/"); pu != "" {
		if st.allowLocal || (strings.HasPrefix(pu, "https://") && tetherPublicAddress(pu)) {
			return pu, ""
		}
	}
	m := a.tunnel()
	if host := m.remoteHost(); host != "" && m.on.Load() {
		if tc := m.cfg.Load(); tc != nil && tc.TailnetOnly() {
			return "", "Remote access is on in Tailscale's private mode, which only your own devices can reach. Connect needs an address other studios can reach: switch Tailscale to Public (Funnel) under Settings → Remote access."
		}
		if c.ExposeTunnel {
			return tunnelURL(host), ""
		}
		return "", "Remote access is on, but Connect isn't one of the things it exposes. Tick Connect under Settings → Remote access."
	}
	if a.cfg.PublicURL != "" {
		return "", "Your ATELIER_PUBLIC_URL (" + a.cfg.PublicURL + ") only works on your own network. Connect needs an address other studios can reach: turn on remote access, or set a public https:// ATELIER_PUBLIC_URL."
	}
	return "", "Connect needs an address other studios can reach: turn on remote access (Settings → Remote access) or set a public https:// ATELIER_PUBLIC_URL."
}

// connectActive is the studio's base address when Connect is on and has a
// public address, else "".
func (a *App) connectActive(ctx context.Context) string {
	c, err := a.db.ConnectSettings(ctx)
	if err != nil || !c.Enabled {
		return ""
	}
	base, _ := a.connectBaseFor(c)
	return base
}

// connectTunnelExposed says whether the tunnel should pass Connect's paths.
func (a *App) connectTunnelExposed(ctx context.Context) bool {
	c, err := a.db.ConnectSettings(ctx)
	return err == nil && c.Enabled && c.ExposeTunnel
}

// connectClassPath reports whether a path belongs to Connect (the tunnel's
// "connect" exposure class).
func connectClassPath(p string) bool {
	return p == "/.well-known/webfinger" || p == "/connect" || strings.HasPrefix(p, "/connect/")
}

// ---------- local actors ----------

// connectLocal is the account behind a local actor name: the personal owner
// or a company account. A second local person is never an actor.
func (a *App) connectLocal(ctx context.Context, name string) (store.Owner, bool) {
	if !gitrepo.ValidName(name) {
		return store.Owner{}, false
	}
	o, err := a.db.OwnerByName(ctx, name)
	if err != nil {
		return store.Owner{}, false
	}
	if o.IsCompany() || a.isOwnerPerson(ctx, o.ID) {
		return o, true
	}
	return store.Owner{}, false
}

// connectMe is the personal owner, who acts for the studio.
func (a *App) connectMe(ctx context.Context) (store.Owner, error) { return a.db.PersonalOwner(ctx) }

func connectActorURI(base, name string) string {
	return base + "/connect/users/" + url.PathEscape(strings.ToLower(name))
}

func connectRepoURI(base string, rp store.Repo) string {
	return base + "/connect/repos/" + url.PathEscape(strings.ToLower(rp.Owner)) + "/" + url.PathEscape(strings.ToLower(rp.Name))
}

func connectKeyID(base, name string) string { return connectActorURI(base, name) + "#main-key" }

// connectFederates says whether a repository may appear in Connect: public,
// and owned by an actor (not by a second local person).
func (a *App) connectFederates(ctx context.Context, rp store.Repo) bool {
	if rp.Private {
		return false
	}
	_, ok := a.connectLocal(ctx, rp.Owner)
	return ok
}

// connectClient is the guarded client, signing as the personal owner.
func (a *App) connectClient(ctx context.Context, base string) (*connect.Client, error) {
	k, err := a.connectKey()
	if err != nil {
		return nil, err
	}
	st := a.connect()
	c := &connect.Client{AllowLocal: st.allowLocal, Key: k, UserAgent: connectUserAgent, Timeout: 10 * time.Second, Now: st.now}
	if me, err := a.connectMe(ctx); err == nil && base != "" {
		c.KeyID = connectKeyID(base, me.Name)
	}
	return c, nil
}

// ---------- serving ----------

func writeAP(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", connect.ContentType+"; charset=utf-8")
	w.Header().Set("Cache-Control", "max-age=60")
	w.Header().Set("Vary", "Accept")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// connectServe answers Connect's public endpoints. Everything is a 404 while
// Connect is off or has no public address.
func (a *App) connectServe(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	if !connectClassPath(p) {
		return false
	}
	ctx := r.Context()
	base := a.connectActive(ctx)
	if base == "" {
		http.NotFound(w, r)
		return true
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if p == "/.well-known/webfinger" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return true
		}
		a.connectWebFinger(w, r, base)
		return true
	}
	if p == "/connect/inbox" {
		a.connectInbox(w, r, base, "")
		return true
	}
	parts := strings.Split(strings.TrimPrefix(p, "/connect/"), "/")
	switch {
	case len(parts) >= 2 && parts[0] == "users":
		a.connectServeUser(w, r, base, parts[1:])
	case len(parts) >= 3 && parts[0] == "repos":
		a.connectServeRepo(w, r, base, parts[1:])
	case len(parts) >= 4 && parts[0] == "git":
		a.connectServeGit(w, r, parts[1:])
	default:
		http.NotFound(w, r)
	}
	return true
}

func (a *App) connectWebFinger(w http.ResponseWriter, r *http.Request, base string) {
	ctx := r.Context()
	res := r.URL.Query().Get("resource")
	host := connect.HostOf(base)
	var name string
	if strings.HasPrefix(res, "acct:") {
		user, h, err := connect.ParseHandle(res)
		if err != nil || h != host {
			http.NotFound(w, r)
			return
		}
		name = user
	} else if strings.HasPrefix(res, base+"/connect/users/") {
		name = strings.TrimPrefix(res, base+"/connect/users/")
	} else {
		http.NotFound(w, r)
		return
	}
	o, ok := a.connectLocal(ctx, name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	actor := connectActorURI(base, o.Name)
	j := connect.JRD{
		Subject: "acct:" + strings.ToLower(o.Name) + "@" + host,
		Aliases: []string{actor, base + "/" + url.PathEscape(o.Name)},
		Links: []connect.JRDLink{
			{Rel: "self", Type: connect.ContentType, Href: actor},
			{Rel: "http://webfinger.net/rel/profile-page", Type: "text/html", Href: base + "/" + url.PathEscape(o.Name)},
		},
	}
	b, _ := json.Marshal(j)
	w.Header().Set("Content-Type", connect.JRDType)
	w.Header().Set("Cache-Control", "max-age=300")
	_, _ = w.Write(b)
}

// connectActorDoc is a local account's actor document.
func (a *App) connectActorDoc(ctx context.Context, base string, o store.Owner) (connect.Actor, error) {
	k, err := a.connectKey()
	if err != nil {
		return connect.Actor{}, err
	}
	c, _ := a.db.ConnectSettings(ctx)
	id := connectActorURI(base, o.Name)
	typ := "Person"
	if o.IsCompany() {
		typ = "Organization"
	}
	summary := ""
	if pr, err := a.db.Profile(ctx, o.ID); err == nil {
		summary = pr.Bio
	}
	return connect.Actor{
		Context: connect.Context, ID: id, Type: typ, PreferredUsername: strings.ToLower(o.Name), Name: o.Label(),
		Summary: summary, URL: base + "/" + url.PathEscape(o.Name),
		Inbox: id + "/inbox", Outbox: id + "/outbox", Followers: id + "/followers", Following: id + "/following",
		Endpoints: &connect.Endpoints{SharedInbox: base + "/connect/inbox"},
		PublicKey: &connect.PublicKey{ID: id + "#main-key", Owner: id, PublicKeyPem: k.PublicPEM()},
		Published: o.CreatedAt.UTC().Format(time.RFC3339), ManuallyApprovesFollowers: !c.AutoAccept, Discoverable: true,
	}, nil
}

func (a *App) connectServeUser(w http.ResponseWriter, r *http.Request, base string, parts []string) {
	ctx := r.Context()
	name, err := url.PathUnescape(parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	o, ok := a.connectLocal(ctx, name)
	if !ok || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	if sub == "inbox" {
		a.connectInbox(w, r, base, o.Name)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := connectActorURI(base, o.Name)
	switch sub {
	case "":
		doc, err := a.connectActorDoc(ctx, base, o)
		if err != nil {
			logErr(r, err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeAP(w, http.StatusOK, doc)
	case "followers", "following":
		// Counts only: who follows whom is nobody else's business.
		var n int
		if sub == "followers" {
			fs, _ := a.db.ConnectFollowers(ctx, o.Name, "accepted")
			n = len(fs)
		} else {
			fs, _ := a.db.ConnectFollowing(ctx, o.Name)
			for _, f := range fs {
				if f.State == "accepted" {
					n++
				}
			}
		}
		writeAP(w, http.StatusOK, connect.Collection{Context: connect.Context, ID: id + "/" + sub, Type: "OrderedCollection",
			TotalItems: n, OrderedItems: []any{}})
	case "outbox":
		writeAP(w, http.StatusOK, connect.Collection{Context: connect.Context, ID: id + "/outbox", Type: "OrderedCollection",
			OrderedItems: []any{}})
	default:
		http.NotFound(w, r)
	}
}

// connectPublicRepo finds a repository that may be shown to other studios.
func (a *App) connectPublicRepo(ctx context.Context, owner, name string) (store.Repo, bool) {
	owner, err1 := url.PathUnescape(owner)
	name, err2 := url.PathUnescape(name)
	if err1 != nil || err2 != nil || !gitrepo.ValidName(owner) || !gitrepo.ValidName(name) {
		return store.Repo{}, false
	}
	rp, err := a.db.RepoByName(ctx, owner, name)
	if err != nil || !a.connectFederates(ctx, rp) {
		return store.Repo{}, false
	}
	return rp, true
}

// connectRepoDoc is a public repository's ForgeFed Repository actor.
func (a *App) connectRepoDoc(ctx context.Context, base string, rp store.Repo) (connect.Actor, error) {
	k, err := a.connectKey()
	if err != nil {
		return connect.Actor{}, err
	}
	id := connectRepoURI(base, rp)
	return connect.Actor{
		Context: connect.Context, ID: id, Type: "Repository", PreferredUsername: strings.ToLower(rp.Name), Name: rp.FullName(),
		Summary: rp.Description, URL: base + "/" + rp.FullName(), Inbox: id + "/inbox", Outbox: id + "/outbox",
		Followers: id + "/followers", Endpoints: &connect.Endpoints{SharedInbox: base + "/connect/inbox"},
		PublicKey:    &connect.PublicKey{ID: id + "#main-key", Owner: id, PublicKeyPem: k.PublicPEM()},
		AttributedTo: connectActorURI(base, rp.Owner),
		CloneURI:     []string{base + "/connect/git/" + strings.ToLower(rp.Owner) + "/" + strings.ToLower(rp.Name) + ".git"},
		Published:    rp.CreatedAt.UTC().Format(time.RFC3339),
	}, nil
}

func (a *App) connectServeRepo(w http.ResponseWriter, r *http.Request, base string, parts []string) {
	ctx := r.Context()
	rp, ok := a.connectPublicRepo(ctx, parts[0], parts[1])
	if !ok || len(parts) > 3 {
		http.NotFound(w, r)
		return
	}
	sub := ""
	if len(parts) == 3 {
		sub = parts[2]
	}
	if sub == "inbox" {
		a.connectInbox(w, r, base, "")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := connectRepoURI(base, rp)
	switch sub {
	case "":
		doc, err := a.connectRepoDoc(ctx, base, rp)
		if err != nil {
			logErr(r, err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeAP(w, http.StatusOK, doc)
	case "followers", "outbox":
		n := 0
		if sub == "followers" {
			n = a.db.ConnectStarCount(ctx, rp.ID)
		}
		writeAP(w, http.StatusOK, connect.Collection{Context: connect.Context, ID: id + "/" + sub, Type: "OrderedCollection",
			TotalItems: n, OrderedItems: []any{}})
	default:
		http.NotFound(w, r)
	}
}

// connectServeGit serves read-only clones of public repositories, so a
// studio you offer a branch to can fetch it. Pushes are never accepted here.
func (a *App) connectServeGit(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) < 3 || !strings.HasSuffix(strings.ToLower(parts[1]), ".git") {
		http.NotFound(w, r)
		return
	}
	rest := strings.Join(parts[2:], "/")
	if gitrepo.IsWrite(r, rest) || (rest != "info/refs" && rest != "git-upload-pack" && rest != "HEAD") {
		http.Error(w, "read-only", http.StatusForbidden)
		return
	}
	if rest == "info/refs" && r.URL.Query().Get("service") != "git-upload-pack" {
		http.Error(w, "smart HTTP only", http.StatusForbidden)
		return
	}
	name := parts[1][:len(parts[1])-4]
	rp, ok := a.connectPublicRepo(r.Context(), parts[0], name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ip := a.clientIP(r)
	if !a.connect().ipRate.allow("git:" + ip) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	a.git.ServeHTTP(w, r, rp.Owner, rp.Name, rest, "connect:anonymous")
}
