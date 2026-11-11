package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
)

// Device workspaces ("Open on <device>"). The Code page lists the person's
// online runners that switched on the workspaces capability. Choosing one
// queues a request; the runner picks it up over its own long-poll
// (workspaces_api.go), clones or updates the repository in its workspaces
// folder with a short-lived token, checks out the branch and, if asked, runs
// one of its own open presets. The server only ever names a preset: commands
// live in the runner's config.
//
// Runners belong to a person (ownsRunner), and so do requests: nobody sees or
// uses another person's devices, which keeps the future multi-person addons
// safe by default.

func init() {
	registerFeature(feature{name: "workspaces", routes: func(a *App, m *http.ServeMux) {
		m.HandleFunc("GET /{owner}/{repo}/workspaces", a.authed(a.wsOpenPage))
		m.HandleFunc("POST /{owner}/{repo}/workspaces", a.authed(a.wsOpenSubmit))
		m.HandleFunc("GET /{owner}/{repo}/workspaces/devices", a.authed(a.wsDevicesJSON))
		m.HandleFunc("GET /{owner}/{repo}/workspaces/requests/{id}", a.authed(a.wsRequestJSON))
		m.HandleFunc("GET /settings/workspaces", a.authed(a.wsDevicePage))
		wsAPIRoutes(a, m)
	}})
}

type wsState struct {
	mu         sync.Mutex
	wake       chan struct{}
	pollWait   time.Duration // longest a runner's workspace poll is held open
	tokenTTL   time.Duration // how long a request's repository token works
	pickupTime time.Duration // a request nobody picks up within this expires
}

var wsStates sync.Map // *App → *wsState

func (a *App) ws() *wsState {
	if st, ok := wsStates.Load(a); ok {
		return st.(*wsState)
	}
	st, _ := wsStates.LoadOrStore(a, &wsState{wake: make(chan struct{}), pollWait: actions.PollWait,
		tokenTTL: 10 * time.Minute, pickupTime: 2 * time.Minute})
	return st.(*wsState)
}

func (st *wsState) signal() {
	st.mu.Lock()
	close(st.wake)
	st.wake = make(chan struct{})
	st.mu.Unlock()
}

func (st *wsState) waitCh() <-chan struct{} {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.wake
}

// wsDevice is one of the person's runners that can take workspace requests.
type wsDevice struct {
	ID      int64    `json:"id"`
	Name    string   `json:"name"`
	OS      string   `json:"os"`
	Online  bool     `json:"online"`
	Presets []string `json:"presets"`
	Root    string   `json:"root"`
	Seen    string   `json:"seen"`
}

// wsDevices lists who's runners with the workspaces capability, online first.
func (a *App) wsDevices(ctx context.Context, who Actor) []wsDevice {
	all, err := a.db.ActionsRunners(ctx)
	if err != nil {
		return nil
	}
	off := a.actions().offlineAfter
	var out []wsDevice
	for _, rn := range all {
		if !rn.Enabled || !a.ownsRunner(ctx, who, rn) {
			continue
		}
		d, err := a.db.WorkspaceDevice(ctx, rn.ID)
		if err != nil {
			continue // never polled for workspaces: the capability is off on that device
		}
		out = append(out, wsDevice{ID: rn.ID, Name: rn.Name, OS: rn.OS, Online: time.Since(d.SeenAt) < off,
			Presets: d.Presets, Root: d.Root, Seen: ago(d.SeenAt)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Online && !out[j].Online })
	return out
}

// wsLoad opens a repository for workspace actions: the person needs write
// access (the device gets a read/write token for it).
func (a *App) wsLoad(w http.ResponseWriter, r *http.Request) (*repoView, bool) {
	v, _, ok := a.loadRepo(w, r, "code")
	if !ok {
		return nil, false
	}
	if !a.can(r.Context(), a.actor(r), v.Repo, PermWrite) {
		a.forbidden(w, r)
		return nil, false
	}
	return v, true
}

type wsPageData struct {
	V       *repoView
	Branch  string
	Devices []wsDevice
	Request *wsRequestView
}

type wsRequestView struct {
	ID      int64  `json:"id"`
	Device  string `json:"device"`
	Branch  string `json:"branch"`
	Preset  string `json:"preset"`
	State   string `json:"state"`
	Label   string `json:"label"`
	Message string `json:"message"`
	Path    string `json:"path"`
	Dirty   bool   `json:"dirty"`
	Done    bool   `json:"done"`
	Failed  bool   `json:"failed"`
	When    string `json:"when"`
}

func wsStateLabel(s string) (label string, done, failed bool) {
	switch s {
	case store.WorkspacePending:
		return "Waiting for the device…", false, false
	case store.WorkspaceDelivered:
		return "The device picked it up…", false, false
	case actions.WorkspaceCloning:
		return "Cloning…", false, false
	case "fetching":
		return "Fetching…", false, false
	case actions.WorkspaceCloned:
		return "Cloned", true, false
	case actions.WorkspaceUpdated:
		return "Updated", true, false
	case actions.WorkspaceOpened:
		return "Opened", true, false
	case store.WorkspaceExpired:
		return "The device didn't answer", true, true
	case actions.WorkspaceFailed:
		return "Failed", true, true
	}
	return s, false, false
}

func (a *App) wsRequestView(ctx context.Context, req store.WorkspaceRequest) *wsRequestView {
	rv := &wsRequestView{ID: req.ID, Branch: req.Branch, Preset: req.Preset, State: req.State, Message: req.Message,
		Path: req.Path, Dirty: req.Dirty, When: ago(req.CreatedAt)}
	// A pending request whose device never asked for it expires on its own.
	if req.State == store.WorkspacePending && time.Since(req.CreatedAt) > a.ws().pickupTime {
		rv.State, rv.Message = store.WorkspaceExpired, "The device didn't pick this up. Is atelier-runner running there?"
	}
	rv.Label, rv.Done, rv.Failed = wsStateLabel(rv.State)
	if rn, err := a.db.ActionsRunnerByID(ctx, req.RunnerID); err == nil {
		rv.Device = rn.Name
	}
	return rv
}

func (a *App) wsOpenPage(w http.ResponseWriter, r *http.Request) {
	v, ok := a.wsLoad(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	d := &wsPageData{V: v, Branch: r.URL.Query().Get("ref"), Devices: a.wsDevices(ctx, who)}
	if d.Branch == "" {
		d.Branch = v.Default
	}
	if id, err := strconv.ParseInt(r.URL.Query().Get("request"), 10, 64); err == nil {
		if req, err := a.db.WorkspaceRequest(ctx, id); err == nil && req.PersonID == who.PersonID && req.RepoID == v.Repo.ID {
			d.Request = a.wsRequestView(ctx, req)
		}
	}
	a.render(w, r, "workspaces_open", page{Title: "Open on a device · " + v.Repo.FullName(), Nav: "repos", Data: d})
}

func (a *App) wsOpenSubmit(w http.ResponseWriter, r *http.Request) {
	v, ok := a.wsLoad(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	fail := func(msg string) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		a.render(w, r, "workspaces_open", page{Title: "Open on a device · " + v.Repo.FullName(), Nav: "repos", Error: msg,
			Data: &wsPageData{V: v, Branch: r.FormValue("branch"), Devices: a.wsDevices(ctx, who)}})
	}
	if v.Empty {
		fail("This repository is empty: push something first.")
		return
	}
	g, err := a.git.Open(v.Repo.Owner, v.Repo.Name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	branch := r.FormValue("branch")
	if _, err := g.BranchSHA(ctx, branch); err != nil {
		fail("Pick an existing branch.")
		return
	}
	id, _ := strconv.ParseInt(r.FormValue("runner"), 10, 64)
	var dev *wsDevice
	for _, d := range a.wsDevices(ctx, who) {
		if d.ID == id {
			dev = &d
			break
		}
	}
	if dev == nil {
		fail("Pick one of your devices that has workspaces switched on.")
		return
	}
	if !dev.Online {
		fail(dev.Name + " is offline. Start atelier-runner there and try again.")
		return
	}
	preset := r.FormValue("preset")
	if preset != "" {
		found := false
		for _, p := range dev.Presets {
			found = found || p == preset
		}
		if !found {
			fail(dev.Name + " has no open preset called " + preset + ".")
			return
		}
	}
	req, err := a.db.CreateWorkspaceRequest(ctx, store.WorkspaceRequest{RunnerID: dev.ID, PersonID: who.PersonID,
		RepoID: v.Repo.ID, Branch: branch, Preset: preset})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.ws().signal()
	http.Redirect(w, r, fmt.Sprintf("%s/workspaces?ref=%s&request=%d", v.Base, branch, req.ID), http.StatusSeeOther)
}

func wsWriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// wsDevicesJSON feeds the "Open on…" menu on the Code page.
func (a *App) wsDevicesJSON(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "code")
	if !ok {
		return
	}
	who := a.actor(r)
	if !a.can(r.Context(), who, v.Repo, PermWrite) {
		wsWriteJSON(w, map[string]any{"devices": []wsDevice{}})
		return
	}
	devs := a.wsDevices(r.Context(), who)
	if devs == nil {
		devs = []wsDevice{}
	}
	wsWriteJSON(w, map[string]any{"devices": devs, "action": v.Base + "/workspaces"})
}

func (a *App) wsRequestJSON(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "code")
	if !ok {
		return
	}
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	req, err := a.db.WorkspaceRequest(ctx, id)
	if err != nil || req.PersonID != a.actor(r).PersonID || req.RepoID != v.Repo.ID {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	wsWriteJSON(w, a.wsRequestView(ctx, req))
}

// ---------- Settings → Runners → <device> → Workspaces ----------
//
// /settings/workspaces?runner=<id>: a /settings/runners/{id}/… GET pattern
// would clash with the /{owner}/{repo}/tree/{rest...} routes in ServeMux.

type wsWorkspaceView struct {
	store.Workspace
	RepoURL string
	Opened  string
}

func (a *App) wsDevicePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	id, _ := strconv.ParseInt(r.URL.Query().Get("runner"), 10, 64)
	rn, err := a.db.ActionsRunnerByID(ctx, id)
	if err != nil || !a.ownsRunner(ctx, who, rn) {
		a.notFound(w, r)
		return
	}
	list, err := a.db.Workspaces(ctx, rn.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var views []wsWorkspaceView
	for _, ws := range list {
		wv := wsWorkspaceView{Workspace: ws}
		if rp, err := a.db.RepoByName(ctx, ws.OwnerName, ws.RepoName); err == nil && a.can(ctx, who, rp, PermRead) {
			wv.RepoURL = "/" + rp.FullName()
		}
		if !ws.LastOpened.IsZero() {
			wv.Opened = ago(ws.LastOpened)
		}
		views = append(views, wv)
	}
	reqs, _ := a.db.RecentWorkspaceRequests(ctx, rn.ID, 10)
	var recent []*wsRequestView
	for _, q := range reqs {
		if q.PersonID == who.PersonID {
			rv := a.wsRequestView(ctx, q)
			if rp, err := a.repoByIDForActions(ctx, q.RepoID); err == nil {
				rv.Message = strings.TrimSpace(rp.FullName() + " · " + rv.Message)
			}
			recent = append(recent, rv)
		}
	}
	dev, derr := a.db.WorkspaceDevice(ctx, rn.ID)
	a.render(w, r, "workspaces_device", page{Title: "Workspaces · " + rn.Name, Nav: "tokens", Data: map[string]any{
		"Runner": rn, "Workspaces": views, "Recent": recent, "Device": dev, "HasDevice": derr == nil,
		"Online": derr == nil && time.Since(dev.SeenAt) < a.actions().offlineAfter,
	}})
}
