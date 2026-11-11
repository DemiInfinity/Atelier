package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"atelier/internal/sshd"
	"atelier/internal/store"
)

// The App API (docs/app-api.md): what the desktop app and the atl CLI talk
// to, under /api/app/v1.
//
//	appapi.go           routes, bearer auth, the device flow, /me, device SSH keys
//	appapi_devices.go   Settings → Devices and the approval page (sessions only)
//	appapi_status.go    "Today": needs, running, feed, unread
//	appapi_repos.go     repositories: list, search, create, mirror from GitHub
//	appapi_pulls.go     pull requests, merging, merge when green
//	appapi_runs.go      Actions runs, logs, agent approvals, cancelling
//	appapi_remote.go    the remote access switch
//	appapi_events.go    live events (Server-Sent Events)
//	appapi_messages.go  friendly messages
//
// Every request carries a bearer credential: a device token ("atd_…", made
// by the device flow) or an ordinary access token with the "app" scope.
// Session cookies are never accepted here, so there's no CSRF surface.
// Device tokens resolve to Actor{Kind: token, Name: "device:<name>"} with the
// scopes app, repo:read and repo:write, so they also work for git over HTTPS
// and are limited by App.can like any read/write token: no repository
// administration, no settings. Through the remote-access tunnel /api/… is
// the "api" class and is served only when the owner exposed it.

const (
	appAPIPrefix   = "/api/app/v1"
	appScope       = "app"
	appCodeTTL     = 10 * time.Minute
	appPollSeconds = 2
	appMaxBody     = 64 << 10
	appStudioName  = "Atelier"
)

func init() {
	registerFeature(feature{
		name: "appapi",
		routes: func(a *App, m *http.ServeMux) {
			const p = appAPIPrefix
			m.HandleFunc("POST "+p+"/device/start", a.appDeviceStart)
			m.HandleFunc("POST "+p+"/device/poll", a.appDevicePoll)
			m.HandleFunc("POST "+p+"/device/ssh-key", a.appAuthed(a.appDeviceSSHKey))
			m.HandleFunc("GET "+p+"/me", a.appAuthed(a.appMe))
			m.HandleFunc("GET "+p+"/status", a.appAuthed(a.appStatus))

			m.HandleFunc("GET "+p+"/repos", a.appAuthed(a.appRepos))
			m.HandleFunc("POST "+p+"/repos", a.appAuthed(a.appRepoCreate))
			m.HandleFunc("POST "+p+"/repos/mirror", a.appAuthed(a.appRepoMirror))

			m.HandleFunc("GET "+p+"/pulls", a.appAuthed(a.appPullsAll))
			m.HandleFunc("GET "+p+"/repos/{owner}/{repo}/pulls", a.appAuthed(a.appPullsRepo))
			m.HandleFunc("GET "+p+"/repos/{owner}/{repo}/pulls/{n}", a.appAuthed(a.appPull))
			m.HandleFunc("POST "+p+"/repos/{owner}/{repo}/pulls/{n}/merge", a.appAuthed(a.appPullMerge))
			m.HandleFunc("DELETE "+p+"/repos/{owner}/{repo}/pulls/{n}/merge", a.appAuthed(a.appPullMergeCancel))

			m.HandleFunc("GET "+p+"/runs", a.appAuthed(a.appRuns))
			m.HandleFunc("GET "+p+"/jobs/{id}/log", a.appAuthed(a.appJobLog))
			m.HandleFunc("POST "+p+"/jobs/{id}/cancel", a.appAuthed(a.appJobCancel))
			m.HandleFunc("GET "+p+"/agents/waiting", a.appAuthed(a.appAgentsWaiting))
			m.HandleFunc("POST "+p+"/agents/jobs/{id}/approve", a.appAuthed(a.appAgentApprove))

			m.HandleFunc("GET "+p+"/remote", a.appAuthed(a.appRemoteGet))
			m.HandleFunc("POST "+p+"/remote", a.appAuthed(a.appRemoteSet))
			m.HandleFunc("GET "+p+"/remote/who", a.appAuthed(a.appRemoteWho))
			m.HandleFunc("POST "+p+"/remote/link", a.appAuthed(a.appRemoteLink))

			m.HandleFunc("GET "+p+"/events", a.appAuthed(a.appEvents))

			m.HandleFunc("GET "+p+"/messages", a.appAuthed(a.appMessagesGet))
			m.HandleFunc("PUT "+p+"/messages", a.appAuthed(a.appMessagesPut))

			// Anything else under the prefix is a JSON 404, not the HTML page.
			m.HandleFunc(p+"/", func(w http.ResponseWriter, r *http.Request) {
				appError(w, http.StatusNotFound, "not_found", "There's no such endpoint in the app API.")
			})

			appDeviceRoutes(a, m)
			appAutoMergeRoutes(a, m)
		},
		start: func(ctx context.Context, a *App) { a.appBackground(ctx) },
	})
	registerTokenResolver(appDeviceResolver)
	registerEventHandler(appEventHandler)
}

// ---------- per-App state ----------

type appState struct {
	startLim *limiter // device/start, per address
	pollLim  *limiter // device/poll, per address
	codeLim  *limiter // wrong codes on the approval page, per person
	hub      *appHub
	mergeMu  sync.Mutex // serialises merge-when-green decisions
	// headMoved remembers when a pending merge's head branch was first seen
	// ahead of the commit it was asked for ("repoID:number").
	headMoved map[string]time.Time
	bg        sync.WaitGroup // background work (tests wait on it)
}

// goBG runs f in the background.
func (s *appState) goBG(f func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		f()
	}()
}

var appStates sync.Map // *App → *appState

func (a *App) app() *appState {
	if s, ok := appStates.Load(a); ok {
		return s.(*appState)
	}
	s := &appState{
		startLim:  newLimiter(10, time.Minute),
		pollLim:   newLimiter(120, time.Minute),
		codeLim:   newLimiter(10, time.Minute),
		hub:       newAppHub(),
		headMoved: map[string]time.Time{},
	}
	actual, _ := appStates.LoadOrStore(a, s)
	return actual.(*appState)
}

// appBackground runs until ctx ends: it re-checks merges waiting for green
// (bots' checks and web approvals don't announce themselves with an event)
// and tells live clients when remote access changes.
func (a *App) appBackground(ctx context.Context) {
	sweep := time.NewTicker(appAutoMergeSweep)
	defer sweep.Stop()
	remote := time.NewTicker(2 * time.Second)
	defer remote.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			a.autoMergeSweep(ctx)
		case <-remote.C:
			if a.app().hub.count() == 0 {
				last = ""
				continue
			}
			st := a.appRemoteState(ctx)
			b, _ := json.Marshal(map[string]any{"on": st.On, "address": st.Address, "connected": st.Connected})
			if last != "" && string(b) != last {
				a.appBroadcastRemote(ctx)
			}
			last = string(b)
		}
	}
}

// ---------- JSON helpers ----------

// appError writes the app API's error shape.
func appError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func (a *App) appFail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		appError(w, http.StatusNotFound, "not_found", "That doesn't exist, or you can't see it.")
		return
	}
	logErr(r, err)
	appError(w, http.StatusInternalServerError, "internal", "Something went wrong on the server; the error has been logged.")
}

// appDecode reads a JSON body into v. An empty body is an empty object.
func appDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, appMaxBody))
	if err != nil {
		appError(w, http.StatusRequestEntityTooLarge, "too_large", "The request body is too large.")
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return true
	}
	if err := json.Unmarshal(body, v); err != nil {
		appError(w, http.StatusBadRequest, "bad_json", "The request body isn't valid JSON for this endpoint.")
		return false
	}
	return true
}

// appTime is an RFC 3339 UTC time, or nil for the zero time.
func appTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func appStrOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// appCleanText trims s, drops control characters and cuts it to n runes.
func appCleanText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > n {
		s = strings.TrimSpace(string([]rune(s)[:n]))
	}
	return s
}

func appPathID(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n, err == nil && n > 0
}

// ---------- who is calling ----------

// appCaller is an authenticated app API request.
type appCaller struct {
	Who    Actor
	Person store.Owner
	Device *store.AppDevice // nil for an ordinary access token with the app scope
}

// owner reports whether the caller acts for the personal owner (the one
// person who runs the studio).
func (c *appCaller) owner(ctx context.Context, a *App) bool {
	return a.isOwnerPerson(ctx, c.Who.PersonID)
}

// appDeviceActor is the Actor a device token resolves to.
func appDeviceActor(d store.AppDevice) Actor {
	return Actor{Kind: ActorToken, Name: "device:" + d.Name, PersonID: d.PersonID, Scopes: []string{appScope, "repo:read", "repo:write"}}
}

// appDeviceResolver makes device tokens work wherever access tokens do (git
// over HTTPS, the REST APIs), with the scopes above.
func appDeviceResolver(ctx context.Context, a *App, plain string) (Actor, bool) {
	if !strings.HasPrefix(plain, store.AppDeviceTokenPrefix) {
		return Actor{}, false
	}
	d, err := a.db.AppDeviceByToken(ctx, plain)
	if err != nil || d.Revoked() {
		return Actor{}, false
	}
	if _, err := a.db.Person(ctx, d.PersonID); err != nil {
		return Actor{}, false
	}
	_ = a.db.TouchAppDevice(context.WithoutCancel(ctx), d, "")
	return appDeviceActor(d), true
}

// appAuthenticate resolves the request's bearer credential. It answers 401,
// 403 or 429 itself and returns ok=false when the caller may not go on.
func (a *App) appAuthenticate(w http.ResponseWriter, r *http.Request) (*appCaller, bool) {
	ctx := r.Context()
	w.Header().Set("Cache-Control", "no-store")
	unauthorized := func(code, msg string) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier"`)
		appError(w, http.StatusUnauthorized, code, msg)
	}
	plain, ok := bearerToken(r)
	if !ok {
		unauthorized("unauthorized", "Send a device token as Authorization: Bearer atd_….")
		return nil, false
	}
	ip := a.clientIP(r)
	bad := func() {
		if !a.lim.allow(ip) {
			appError(w, http.StatusTooManyRequests, "rate_limited", "Too many attempts, try again in a minute.")
			return
		}
		unauthorized("unauthorized", "That token isn't valid.")
	}
	c := &appCaller{}
	if strings.HasPrefix(plain, store.AppDeviceTokenPrefix) {
		d, err := a.db.AppDeviceByToken(ctx, plain)
		if err != nil {
			bad()
			return nil, false
		}
		if d.Revoked() {
			unauthorized("revoked", "This device was revoked. Sign in again to keep using it.")
			return nil, false
		}
		_ = a.db.TouchAppDevice(context.WithoutCancel(ctx), d, ip)
		c.Device, c.Who = &d, appDeviceActor(d)
	} else {
		who, found := a.resolveCredential(ctx, plain)
		if !found {
			bad()
			return nil, false
		}
		if who.Kind != ActorToken || !who.hasScope(appScope) {
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="app"`)
			appError(w, http.StatusForbidden, "forbidden", "This token doesn't have the app scope.")
			return nil, false
		}
		c.Who = who
	}
	p, err := a.db.Person(ctx, c.Who.PersonID)
	if err != nil {
		unauthorized("unauthorized", "That token isn't valid.")
		return nil, false
	}
	c.Person = p
	if !isRemote(r) {
		a.rememberBaseURL(a.baseURL(r))
	}
	return c, true
}

func (a *App) appAuthed(h func(http.ResponseWriter, *http.Request, *appCaller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := a.appAuthenticate(w, r)
		if !ok {
			return
		}
		h(w, withActor(r, c.Who), c)
	}
}

// appRepo loads the repository named in the path and checks need with
// App.can. A repository the caller can't read is a 404, like on the web.
func (a *App) appRepo(w http.ResponseWriter, r *http.Request, c *appCaller, need Perm) (store.Repo, bool) {
	return a.appRepoNamed(w, r, c, r.PathValue("owner"), r.PathValue("repo"), need)
}

func (a *App) appRepoNamed(w http.ResponseWriter, r *http.Request, c *appCaller, owner, name string, need Perm) (store.Repo, bool) {
	ctx := r.Context()
	rp, err := a.db.RepoByName(ctx, owner, name)
	if err != nil || !a.can(ctx, c.Who, rp, PermRead) {
		appError(w, http.StatusNotFound, "not_found", "That repository doesn't exist, or you can't see it.")
		return store.Repo{}, false
	}
	if need != PermRead && !a.can(ctx, c.Who, rp, need) {
		appError(w, http.StatusForbidden, "forbidden", "You don't have "+need.String()+" access to this repository.")
		return store.Repo{}, false
	}
	return rp, true
}

// appReadable lists every repository the caller may read, most recently
// updated first, with a lookup by id.
func (a *App) appReadable(ctx context.Context, who Actor) ([]store.Repo, map[int64]store.Repo, error) {
	all, err := a.db.Repos(ctx)
	if err != nil {
		return nil, nil, err
	}
	repos := a.readableRepos(ctx, who, all)
	byID := make(map[int64]store.Repo, len(repos))
	for _, rp := range repos {
		byID[rp.ID] = rp
	}
	return repos, byID, nil
}

// ---------- the device flow ----------

func (a *App) appStudio(r *http.Request) map[string]any {
	return map[string]any{"name": appStudioName, "url": a.appHomeURL(r), "version": buildVersion()}
}

// appHomeURL is the studio's address for a client. Through the tunnel that
// is the tunnel address (what the client can reach).
func (a *App) appHomeURL(r *http.Request) string { return a.baseURL(r) }

// appApproveBase is where the owner approves devices: always the home
// address, since the approval page is refused through the tunnel.
func (a *App) appApproveBase(r *http.Request) string {
	if !isRemote(r) {
		return a.baseURL(r)
	}
	if b := a.actionsBaseURL(); b != "" {
		return strings.TrimRight(b, "/")
	}
	return a.baseURL(r)
}

func (a *App) appDeviceStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !a.app().startLim.allow(a.clientIP(r)) {
		w.Header().Set("Retry-After", "60")
		appError(w, http.StatusTooManyRequests, "rate_limited", "Too many sign-in attempts from this address. Try again in a minute.")
		return
	}
	if a.needsSetup(r.Context()) {
		appError(w, http.StatusConflict, "not_set_up", "This studio hasn't been set up yet. Open it in a browser first.")
		return
	}
	var in struct {
		Name    string `json:"name"`
		Kind    string `json:"kind"`
		OS      string `json:"os"`
		Version string `json:"version"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	in.Name, in.OS, in.Version = appCleanText(in.Name, 60), appCleanText(in.OS, 32), appCleanText(in.Version, 32)
	if in.Name == "" {
		appError(w, http.StatusUnprocessableEntity, "invalid_name", "Give the device a name.")
		return
	}
	if in.Kind != "desktop" && in.Kind != "cli" {
		appError(w, http.StatusUnprocessableEntity, "invalid_kind", "kind must be desktop or cli.")
		return
	}
	deviceCode, userCode, err := a.db.StartAppDeviceCode(r.Context(), store.AppDeviceCode{Name: in.Name, Kind: in.Kind, OS: in.OS,
		Version: in.Version, IP: a.clientIP(r)}, appCodeTTL)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": deviceCode, "user_code": userCode,
		"verify_url": a.appApproveBase(r) + "/settings/devices/approve?code=" + userCode,
		"expires_in": int(appCodeTTL.Seconds()), "interval": appPollSeconds,
	})
}

func (a *App) appDevicePoll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !a.app().pollLim.allow(a.clientIP(r)) {
		w.Header().Set("Retry-After", "60")
		appError(w, http.StatusTooManyRequests, "rate_limited", "Polling too fast. Wait a minute, then poll every 2 seconds.")
		return
	}
	var in struct {
		DeviceCode string `json:"device_code"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	if len(in.DeviceCode) != 64 {
		appError(w, http.StatusGone, "expired", "This sign-in code has expired. Start again.")
		return
	}
	token, d, err := a.db.ClaimAppDeviceCode(r.Context(), in.DeviceCode)
	switch {
	case errors.Is(err, store.ErrAppCodePending):
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
		return
	case errors.Is(err, store.ErrAppCodeDenied):
		appError(w, http.StatusGone, "denied", "The sign-in was denied.")
		return
	case errors.Is(err, store.ErrAppCodeExpired):
		appError(w, http.StatusGone, "expired", "This sign-in code has expired or was already used. Start again.")
		return
	case err != nil:
		a.appFail(w, r, err)
		return
	}
	p, err := a.db.Person(r.Context(), d.PersonID)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "device_id": d.ID,
		"person": map[string]any{"name": p.Label(), "login": p.Name},
		"studio": a.appStudio(r),
	})
}

// ---------- who and where ----------

func (a *App) appMe(w http.ResponseWriter, r *http.Request, c *appCaller) {
	ctx := r.Context()
	// Accounts are the ones the person may switch between on the web.
	accounts := []map[string]any{}
	for _, o := range a.switchAccounts(ctx, personActor(c.Person)) {
		sig := a.editorIdentity(ctx, c.Who)
		accounts = append(accounts, map[string]any{"name": o.Name, "display": o.Label(), "kind": o.Kind,
			"commit_name": sig.Name, "commit_email": sig.Email})
	}
	studio := a.appStudio(r)
	studio["ssh"] = nil
	if a.cfg.SSHEnabled() {
		studio["ssh"] = map[string]any{"host": a.sshHost(r), "port": a.cfg.SSHPort()}
	}
	_, terr := a.db.TetherAccount(ctx, c.Who.PersonID)
	studio["tether"] = terr == nil
	studio["connect"] = c.owner(ctx, a) && a.connectActive(ctx) != ""
	var device any
	if c.Device != nil {
		device = map[string]any{"id": c.Device.ID, "name": c.Device.Name, "kind": c.Device.Kind}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"person": map[string]any{"name": c.Person.Label(), "login": c.Person.Name,
			"avatar_url": appStrOrNil(a.avatarURLFor(c.Person.Name, 160))},
		"accounts": accounts, "studio": studio, "device": device,
	})
}

// ---------- this device's SSH key ----------

func (a *App) appDeviceSSHKey(w http.ResponseWriter, r *http.Request, c *appCaller) {
	ctx := r.Context()
	if c.Device == nil {
		appError(w, http.StatusForbidden, "device_only", "Only a signed-in device can add its own SSH key. Add other keys in Settings → SSH keys.")
		return
	}
	var in struct {
		Title string `json:"title"`
		Key   string `json:"key"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	pk, err := sshd.ParsePublicKey(in.Key)
	if err != nil {
		appError(w, http.StatusUnprocessableEntity, "invalid_key", sshCapitalise(err.Error())+".")
		return
	}
	title := appCleanText(in.Title, 80)
	if title == "" {
		title = appCleanText(pk.Comment, 80)
	}
	if title == "" {
		title = c.Device.Name
	}
	k, err := a.db.AddSSHKey(ctx, title, pk.Type, pk.Fingerprint, pk.Normalized, c.Who.Name, "device")
	if errors.Is(err, store.ErrKeyExists) {
		old, err := a.db.SSHKeyByFingerprint(ctx, pk.Fingerprint)
		if err != nil {
			a.appFail(w, r, err)
			return
		}
		if owner, err := a.db.SSHKeyPerson(ctx, old.ID); err != nil || owner != c.Who.PersonID {
			appError(w, http.StatusConflict, "key_taken", "That key is already in use by someone else.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": old.ID, "fingerprint": old.Fingerprint})
		return
	}
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	if err := a.db.LinkAppDeviceKey(ctx, *c.Device, k.ID); err != nil {
		_ = a.db.DeleteSSHKey(ctx, k.ID)
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": k.ID, "fingerprint": k.Fingerprint})
}
