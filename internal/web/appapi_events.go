package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
)

// Live events for the app: GET /api/app/v1/events is a Server-Sent Events
// stream fed from the event bus (events.go). Each connection only gets what
// its caller may see (App.can for anything about a repository, the personal
// owner for instance-wide events), and a device is never told about what it
// did itself. A person can hold a handful of streams at once.

var (
	appPingEvery    = 25 * time.Second
	appRecheckEvery = time.Minute // how often a stream checks its token still works
)

const (
	appMaxStreams = 8  // per person
	appStreamBuf  = 64 // events a slow client may fall behind before it's dropped
)

type appMessage struct {
	event string
	data  []byte
}

type appConn struct {
	who      Actor
	deviceID int64
	ch       chan appMessage
	gone     chan struct{} // closed when the hub drops the connection
	once     sync.Once
}

func (c *appConn) drop() { c.once.Do(func() { close(c.gone) }) }

type appHub struct {
	mu    sync.Mutex
	conns map[*appConn]struct{}
	seq   atomic.Int64
	// merged remembers commits Atelier just made by merging ("repoID:sha"),
	// so the push they cause isn't announced as a second moment.
	merged map[string]time.Time
}

func newAppHub() *appHub {
	h := &appHub{conns: map[*appConn]struct{}{}, merged: map[string]time.Time{}}
	h.seq.Store(time.Now().UnixMilli())
	return h
}

func (h *appHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// add registers a connection, unless the person already has too many.
func (h *appHub) add(c *appConn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for o := range h.conns {
		if o.who.PersonID == c.who.PersonID {
			n++
		}
	}
	if n >= appMaxStreams {
		return false
	}
	h.conns[c] = struct{}{}
	return true
}

func (h *appHub) remove(c *appConn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

func (h *appHub) snapshot() []*appConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*appConn, 0, len(h.conns))
	for c := range h.conns {
		out = append(out, c)
	}
	return out
}

// dropDevice ends a revoked device's streams.
func (h *appHub) dropDevice(id int64) {
	for _, c := range h.snapshot() {
		if c.deviceID == id {
			c.drop()
		}
	}
}

// send queues a message; a client that has fallen too far behind is dropped
// (it reconnects and reads /status again).
func (c *appConn) send(m appMessage) {
	select {
	case c.ch <- m:
	default:
		c.drop()
	}
}

// own reports whether who is this connection's own device.
func (c *appConn) own(who Actor) bool {
	return c.deviceID != 0 && who.Kind == ActorToken && who.PersonID == c.who.PersonID && who.Name == c.who.Name
}

// appMaySee reports whether a connection may hear about an event: repository
// events need read access; instance-wide events go to the personal owner
// (or to the people the event names).
func (a *App) appMaySee(ctx context.Context, c *appConn, ev Event, repo *store.Repo) bool {
	if len(ev.Recipients) > 0 {
		named := false
		for _, id := range ev.Recipients {
			named = named || id == c.who.PersonID
		}
		if !named {
			return false
		}
	}
	if repo != nil {
		return a.can(ctx, c.who, *repo, PermRead)
	}
	return len(ev.Recipients) > 0 || a.isOwnerPerson(ctx, c.who.PersonID)
}

// statusChanged tells everyone who can read rp (except the device that did
// it) that parts of /status changed.
func (h *appHub) statusChanged(ctx context.Context, a *App, rp store.Repo, by Actor, parts ...string) {
	data, _ := json.Marshal(map[string]any{"changed": parts})
	for _, c := range h.snapshot() {
		if c.own(by) || !a.can(ctx, c.who, rp, PermRead) {
			continue
		}
		c.send(appMessage{"status", data})
	}
}

// appBroadcastRemote sends the remote event to the owner's streams.
func (a *App) appBroadcastRemote(ctx context.Context) {
	conns := a.app().hub.snapshot()
	if len(conns) == 0 {
		return
	}
	st := a.appRemoteState(ctx)
	data, _ := json.Marshal(map[string]any{"on": st.On, "address": appStrOrNil(st.Address), "connected": st.Connected})
	for _, c := range conns {
		if a.isOwnerPerson(ctx, c.who.PersonID) {
			c.send(appMessage{"remote", data})
		}
	}
}

// ---------- event bus → streams ----------

func appEventHandler(ctx context.Context, a *App, ev Event) {
	a.autoMergeOnEvent(ctx, ev)
	h := a.app().hub
	if ev.Kind == EventPullMerged && ev.RepoID != 0 {
		// Remember the merge commit: its push to the base branch follows.
		if p, err := a.db.PullByNumber(ctx, ev.RepoID, ev.Number); err == nil && p.MergedSHA != "" {
			h.mu.Lock()
			for k, t := range h.merged {
				if time.Since(t) > 5*time.Minute {
					delete(h.merged, k)
				}
			}
			h.merged[fmt.Sprintf("%d:%s", ev.RepoID, p.MergedSHA)] = time.Now()
			h.mu.Unlock()
		}
	}
	conns := h.snapshot()
	if len(conns) == 0 {
		return
	}
	var repo *store.Repo
	if ev.RepoID != 0 {
		rp := ev.Repo
		if rp.ID != ev.RepoID {
			found, err := a.repoByID(ctx, ev.RepoID)
			if err != nil {
				return
			}
			rp = found
		}
		repo = &rp
	}
	note := a.appNotify(ctx, ev, repo)
	changed := appChanged(ev)
	if note == nil && len(changed) == 0 {
		return
	}
	var noteData, statusData []byte
	if note != nil {
		note["id"] = fmt.Sprintf("n:%d", h.seq.Add(1))
		noteData, _ = json.Marshal(note)
	}
	if len(changed) > 0 {
		statusData, _ = json.Marshal(map[string]any{"changed": changed})
	}
	for _, c := range conns {
		if c.own(ev.Actor) || !a.appMaySee(ctx, c, ev, repo) {
			continue
		}
		if noteData != nil {
			c.send(appMessage{"notify", noteData})
		}
		if statusData != nil {
			c.send(appMessage{"status", statusData})
		}
	}
}

// appChanged says which parts of /status an event touches.
func appChanged(ev Event) []string {
	switch ev.Subject() {
	case "run", "job":
		return []string{"needs", "running", "unread"}
	case "pull", "tether":
		return []string{"needs", "unread"}
	case "repo":
		if ev.Kind == EventRepoPushed {
			return []string{"needs", "feed"}
		}
		return []string{"feed", "unread"}
	case "secret":
		if ev.Kind == EventSecretDetected {
			return []string{"needs", "unread"}
		}
		return []string{"unread"}
	}
	if ev.Title != "" {
		return []string{"unread"}
	}
	return nil
}

// appNotify builds the notify payload of an event (without its id), or nil
// when the event isn't worth a pop-up.
func (a *App) appNotify(ctx context.Context, ev Event, repo *store.Repo) map[string]any {
	thread := appThread(ev.Actor.Kind, ev.Actor.Name)
	if ev.Subject() == "connect" {
		thread = "connect"
	}
	kind := strings.ReplaceAll(strings.ReplaceAll(ev.Kind, ".", "_"), "-", "_")
	var moment any
	title, detail := ev.Title, ev.Detail
	fromGitHub := thread == "github"
	if fromGitHub {
		moment = "github"
	}
	switch ev.Kind {
	case EventRepoPushed:
		if ev.Push == nil || repo == nil || actZeroSHA(ev.Push.New) {
			return nil
		}
		branch, ok := strings.CutPrefix(ev.Push.Ref, "refs/heads/")
		if !ok {
			return nil // tags are announced by releases
		}
		h := a.app().hub
		h.mu.Lock()
		_, byMerge := h.merged[fmt.Sprintf("%d:%s", ev.RepoID, ev.Push.New)]
		h.mu.Unlock()
		if byMerge {
			return nil
		}
		kind, title, detail = "push", "Pushed "+branch+" to "+repo.Name, appJoin(ev.Actor.Name, shortSHA(ev.Push.New))
		if !fromGitHub {
			moment = "push"
		}
	case EventRunCompleted, EventRunFailed:
		agent := false
		if run, err := a.db.ActionsRunBySeq(ctx, ev.RepoID, ev.Number); err == nil {
			agent = appIsAgentRun(run)
			thread = "yours"
			if agent {
				thread = "agents"
			}
			switch {
			case ev.Kind == EventRunFailed:
				kind, moment = "run_failed", "fail"
				title = run.WorkflowName + " failed on " + run.Branch()
			case run.Conclusion == actions.StatusSuccess && agent:
				kind, moment = "agent_finished", "agent"
				title = run.WorkflowName + " finished"
			case run.Conclusion == actions.StatusSuccess:
				kind, moment = "run_passed", "pass"
				title = run.WorkflowName + " passed on " + run.Branch()
			default:
				kind, moment = "run_cancelled", nil
			}
			if repo != nil {
				detail = appJoin(repo.Name, detail)
			}
		} else if ev.Kind == EventRunFailed {
			kind, moment = "run_failed", "fail"
		}
	case EventJobWaiting:
		kind, moment, thread = "job_waiting", nil, "yours"
		if run, err := a.db.ActionsRunBySeq(ctx, ev.RepoID, ev.Number); err == nil && appIsAgentRun(run) {
			kind, moment, thread = "agent_approval", "agent", "agents"
			title = "Approve " + run.WorkflowName + " before it runs"
		}
	case EventPullMerged:
		kind = "pull_merged"
		if !fromGitHub {
			moment = "merge"
		}
		title = "Merged “" + ev.Title + "”"
	case EventPullCommented, EventPullNote, EventTetherComment:
		kind = "pull_review"
		if ev.Kind == EventTetherComment {
			thread, moment = "github", "github"
		}
	case EventSecretDetected:
		kind, moment = "security_alert", "security"
	case EventPullAutoMergeCancelled:
		kind = "auto_merge_cancelled"
	}
	if title == "" {
		return nil
	}
	out := map[string]any{"kind": kind, "thread": thread, "title": title, "detail": detail, "repo": nil,
		"web_path": appStrOrNil(safeLocalURL(ev.URL)), "moment": moment, "at": appTime(time.Now())}
	if repo != nil {
		out["repo"] = repo.FullName()
	}
	return out
}

// ---------- the stream ----------

func (a *App) appEvents(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rc := http.NewResponseController(w)
	conn := &appConn{who: c.Who, ch: make(chan appMessage, appStreamBuf), gone: make(chan struct{})}
	if c.Device != nil {
		conn.deviceID = c.Device.ID
	}
	h := a.app().hub
	if !h.add(conn) {
		w.Header().Set("Retry-After", "30")
		appError(w, http.StatusTooManyRequests, "too_many_streams", "Too many live connections for this account. Close one and try again.")
		return
	}
	defer h.remove(conn)

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no") // nginx: don't buffer
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
		return
	}
	ctx := r.Context()
	ping := time.NewTicker(appPingEvery)
	defer ping.Stop()
	recheck := time.NewTicker(appRecheckEvery)
	defer recheck.Stop()
	plain, _ := bearerToken(r)
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.gone:
			return
		case <-recheck.C:
			// A revoked or expired credential stops hearing things.
			if who, ok := a.resolveCredential(ctx, plain); !ok || who.PersonID != c.Who.PersonID || !who.hasScope(appScope) {
				return
			}
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case m := <-conn.ch:
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.event, m.data); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
