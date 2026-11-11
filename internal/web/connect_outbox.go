package web

// What Connect sends: follows and their answers, stars, and the public
// activity of your accounts (new public repositories, pushes, releases).
// Activities are queued in connect_deliveries, one row per inbox, and a
// loop sends them signed, retrying with backoff. While Connect is off (or
// has no public address) nothing is queued and nothing queued is sent.
// A delivery about a repository is re-checked just before sending: if the
// repository has gone private or away, it's dropped.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"atelier/internal/connect"
	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

func init() {
	registerEventHandler(func(ctx context.Context, a *App, ev Event) {
		switch ev.Kind {
		case EventRepoPushed:
			a.connectOnPush(ctx, ev)
		case EventReleasePublished:
			a.connectOnRelease(ctx, ev)
		}
	})
}

var errConnectOff = errors.New("Connect is off or has no public address, so nothing was sent")

func connectNewID(base string) string { return base + "/connect/activities/" + store.RandomToken(12) }

// connectQueue queues an activity for inboxes. repoID, when set, is the
// public repository the activity is about.
func (a *App) connectQueue(ctx context.Context, act map[string]any, inboxes []string, repoID int64) error {
	if a.connectActive(ctx) == "" {
		return errConnectOff
	}
	body, err := json.Marshal(act)
	if err != nil {
		return err
	}
	id, _ := act["id"].(string)
	kind, _ := act["type"].(string)
	seen := map[string]bool{}
	for _, in := range inboxes {
		host := connect.HostOf(in)
		if in == "" || seen[in] || !connect.SafeLink(in) || !a.connectHostAllowed(ctx, host) {
			continue
		}
		seen[in] = true
		if err := a.db.QueueConnectDelivery(ctx, store.ConnectDelivery{Inbox: in, Host: host, ActivityID: id, Kind: kind,
			Body: body, RepoID: repoID}); err != nil {
			return err
		}
	}
	a.connect().signal()
	return nil
}

// connectFollowerInboxes are the inboxes of an account's accepted followers;
// a company account's activity also reaches the personal owner's followers.
func (a *App) connectFollowerInboxes(ctx context.Context, owner string) []string {
	locals := []string{owner}
	if me, err := a.connectMe(ctx); err == nil && !strings.EqualFold(me.Name, owner) {
		locals = append(locals, me.Name)
	}
	in, err := a.db.ConnectAcceptedFollowerInboxes(ctx, locals)
	if err != nil {
		log.Printf("connect: follower inboxes: %v", err)
	}
	return in
}

// ---------- the delivery loop ----------

func (a *App) connectLoop(ctx context.Context) {
	st := a.connect()
	_ = a.db.PruneConnect(ctx, time.Now())
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var lastAnnounce, lastPrune time.Time
	for {
		select {
		case <-ctx.Done():
			st.bg.Wait()
			return
		case <-tick.C:
		case <-st.wake:
		}
		if time.Since(lastAnnounce) > 15*time.Second {
			lastAnnounce = time.Now()
			a.connectAnnounceRepos(ctx)
		}
		for a.connectDeliverDue(ctx) == 20 {
		}
		if time.Since(lastPrune) > time.Hour {
			lastPrune = time.Now()
			_ = a.db.PruneConnect(ctx, time.Now())
		}
	}
}

// connectDeliverDue sends what's due and returns how many it tried.
func (a *App) connectDeliverDue(ctx context.Context) int {
	st := a.connect()
	st.deliverMu.Lock()
	defer st.deliverMu.Unlock()
	base := a.connectActive(ctx)
	if base == "" {
		return 0 // off: nothing leaves
	}
	cl, err := a.connectClient(ctx, base)
	if err != nil {
		log.Printf("connect: %v", err)
		return 0
	}
	due, err := a.db.DueConnectDeliveries(ctx, st.now(), 20)
	if err != nil {
		log.Printf("connect: deliveries: %v", err)
		return 0
	}
	for _, d := range due {
		finish := func(status string, attempts int, next time.Time, code int, msg string) {
			if err := a.db.FinishConnectDelivery(ctx, d.ID, status, attempts, next, code, msg); err != nil {
				log.Printf("connect: delivery %d: %v", d.ID, err)
			}
		}
		if d.RepoID != 0 {
			if rp, err := a.db.RepoByID(ctx, d.RepoID); err != nil || !a.connectFederates(ctx, rp) {
				finish("dropped", d.Attempts, st.now(), 0, "the repository is private or gone")
				continue
			}
		}
		if !a.connectHostAllowed(ctx, d.Host) {
			finish("dropped", d.Attempts, st.now(), 0, "studio blocked")
			continue
		}
		// Sign as the activity's own actor (every local actor shares the studio key).
		var head struct {
			Actor string `json:"actor"`
		}
		_ = json.Unmarshal(d.Body, &head)
		if !strings.HasPrefix(head.Actor, base+"/connect/") {
			finish("dropped", d.Attempts, st.now(), 0, "the studio address changed since this was queued")
			continue
		}
		c := *cl
		c.KeyID = head.Actor + "#main-key"
		code, err := c.Post(ctx, d.Inbox, d.Body)
		attempts := d.Attempts + 1
		switch {
		case err == nil:
			finish("sent", attempts, st.now(), code, "")
		case errors.Is(err, connect.ErrBlocked):
			finish("failed", attempts, st.now(), 0, err.Error())
		case code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests:
			finish("failed", attempts, st.now(), code, err.Error())
		case attempts >= st.maxAttempts:
			finish("failed", attempts, st.now(), code, err.Error())
		default:
			finish("queued", attempts, st.now().Add(st.backoff(attempts)), code, err.Error())
		}
	}
	return len(due)
}

// ---------- public activity ----------

// connectAnnounceRepos announces public repositories that haven't been yet
// (new ones, and ones made public). Private repositories are skipped.
func (a *App) connectAnnounceRepos(ctx context.Context) {
	base := a.connectActive(ctx)
	if base == "" {
		return
	}
	done, err := a.db.ConnectAnnounced(ctx)
	if err != nil {
		return
	}
	repos, err := a.db.Repos(ctx)
	if err != nil {
		return
	}
	for _, rp := range repos {
		if done[rp.ID] || !a.connectFederates(ctx, rp) {
			continue
		}
		doc, err := a.connectRepoDoc(ctx, base, rp)
		if err != nil {
			return
		}
		doc.Context = nil
		actor := connectActorURI(base, rp.Owner)
		act := connect.Activity(connectNewID(base), "Create", actor, doc, []string{connect.Public},
			[]string{actor + "/followers"}, time.Now())
		act["summary"] = "created a repository: " + rp.FullName()
		if err := a.connectQueue(ctx, act, a.connectFollowerInboxes(ctx, rp.Owner), rp.ID); err != nil {
			return
		}
		_ = a.db.MarkConnectAnnounced(ctx, rp.ID)
	}
}

// connectRepoNow re-reads a repository so visibility is current.
func (a *App) connectRepoNow(ctx context.Context, ev Event) (store.Repo, bool) {
	id := ev.RepoID
	if id == 0 {
		id = ev.Repo.ID
	}
	rp, err := a.db.RepoByID(ctx, id)
	if err != nil || !a.connectFederates(ctx, rp) {
		return store.Repo{}, false
	}
	return rp, true
}

const zeroSHA = "0000000000000000000000000000000000000000"

// connectOnPush summarises a branch update of a public repository as a
// ForgeFed Push: the number of commits and the first few subjects.
func (a *App) connectOnPush(ctx context.Context, ev Event) {
	if ev.Push == nil || !strings.HasPrefix(ev.Push.Ref, "refs/heads/") || ev.Push.New == "" || ev.Push.New == zeroSHA {
		return
	}
	base := a.connectActive(ctx)
	if base == "" {
		return
	}
	rp, ok := a.connectRepoNow(ctx, ev)
	if !ok {
		return // private repositories never federate
	}
	push := *ev.Push
	st := a.connect()
	st.bg.Add(1)
	go func() {
		defer st.bg.Done()
		ctx := context.WithoutCancel(ctx)
		g, err := a.git.Open(rp.Owner, rp.Name)
		if err != nil {
			return
		}
		var commits []gitrepo.Commit
		count := 0
		if push.Old == "" || push.Old == zeroSHA {
			commits, _ = g.Log(ctx, push.New, "", 0, 5)
			count = len(commits)
		} else {
			commits, _ = g.CommitsBetween(ctx, push.Old, push.New, 50) // oldest first
			count = len(commits)
			for i, j := 0, len(commits)-1; i < j; i, j = i+1, j-1 {
				commits[i], commits[j] = commits[j], commits[i]
			}
			if len(commits) > 5 {
				commits = commits[:5]
			}
		}
		if count == 0 {
			return
		}
		branch := strings.TrimPrefix(push.Ref, "refs/heads/")
		repoURI := connectRepoURI(base, rp)
		var items []any
		for _, c := range commits {
			items = append(items, map[string]any{"type": "Commit", "id": base + "/" + rp.FullName() + "/commit/" + c.Hash,
				"hash": c.Hash, "summary": connect.Clip(c.Subject, 200), "context": repoURI})
		}
		actor := connectActorURI(base, rp.Owner)
		act := connect.Activity(connectNewID(base), "Push", actor, map[string]any{
			"type": "OrderedCollection", "totalItems": count, "orderedItems": items,
		}, []string{connect.Public}, []string{actor + "/followers"}, time.Now())
		act["target"] = repoURI + "/branches/" + url.PathEscape(branch)
		act["context"] = repoURI
		act["url"] = base + "/" + rp.FullName() + "/commits/" + branch
		act["atelierRepo"] = rp.FullName()
		act["summary"] = "pushed " + plural(count, "commit") + " to " + branch + " in " + rp.FullName()
		if err := a.connectQueue(ctx, act, a.connectFollowerInboxes(ctx, rp.Owner), rp.ID); err != nil && !errors.Is(err, errConnectOff) {
			log.Printf("connect: push: %v", err)
		}
	}()
}

// connectOnRelease announces a published release of a public repository.
func (a *App) connectOnRelease(ctx context.Context, ev Event) {
	base := a.connectActive(ctx)
	if base == "" {
		return
	}
	rp, ok := a.connectRepoNow(ctx, ev)
	if !ok {
		return
	}
	actor := connectActorURI(base, rp.Owner)
	repoURI := connectRepoURI(base, rp)
	note := map[string]any{
		"id": repoURI + "/releases/" + strconv.FormatInt(ev.Number, 10), "type": "Note", "attributedTo": actor, "context": repoURI,
		"name": rp.FullName() + " " + ev.Title, "content": "Released " + ev.Title + " of " + rp.FullName(),
		"url": base + ev.URL, "published": time.Now().UTC().Format(time.RFC3339), "atelierKind": "release",
		"to": []string{connect.Public},
	}
	act := connect.Activity(connectNewID(base), "Create", actor, note, []string{connect.Public}, []string{actor + "/followers"}, time.Now())
	act["summary"] = "released " + ev.Title + " of " + rp.FullName()
	if err := a.connectQueue(ctx, act, a.connectFollowerInboxes(ctx, rp.Owner), rp.ID); err != nil && !errors.Is(err, errConnectOff) {
		log.Printf("connect: release: %v", err)
	}
}

// ---------- follows ----------

// connectResolve turns what someone typed (@name@host, name@host, an actor
// URL, a profile URL or a studio address from a badge) into a remote actor.
func (a *App) connectResolve(ctx context.Context, base, input string) (store.ConnectActor, error) {
	input = strings.TrimSpace(input)
	if input == "" || len(input) > 2048 {
		return store.ConnectActor{}, errors.New("Enter @name@studio.example or a studio address.")
	}
	cl, err := a.connectClient(ctx, base)
	if err != nil {
		return store.ConnectActor{}, err
	}
	finger := func(scheme, user, host string) (store.ConnectActor, error) {
		d, err := cl.Get(ctx, connect.WebFingerURL(scheme, user, host), connect.JRDType+", application/json")
		if err != nil {
			return store.ConnectActor{}, err
		}
		var links []connect.JRDLink
		_ = json.Unmarshal(d["links"], &links)
		self := connect.JRD{Links: links}.SelfLink()
		if self == "" || connect.HostOf(self) != strings.ToLower(host) {
			return store.ConnectActor{}, errors.New("that studio didn't name an actor for " + user)
		}
		c, _, err := a.connectRemoteActor(ctx, base, self, true)
		return c, err
	}
	if !strings.Contains(input, "://") {
		user, host, err := connect.ParseHandle(input)
		if err != nil {
			return store.ConnectActor{}, errors.New("Enter @name@studio.example or a studio address.")
		}
		c, err := finger("https", user, host)
		if err != nil && a.connect().allowLocal {
			c, err = finger("http", user, host)
		}
		return c, err
	}
	u, err := url.Parse(input)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return store.ConnectActor{}, errors.New("That doesn't look like a studio address.")
	}
	// An actor URL?
	if c, _, err := a.connectRemoteActor(ctx, base, input, true); err == nil {
		return c, nil
	}
	// A profile or studio address (https://studio.example/name): WebFinger
	// for its last path segment.
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	user := segs[len(segs)-1]
	if len(segs) >= 3 && segs[0] == "connect" && segs[1] == "users" {
		user = segs[2]
	}
	if user == "" {
		return store.ConnectActor{}, errors.New("Add the person's name to the address, like https://studio.example/name.")
	}
	return finger(u.Scheme, user, u.Host)
}

// connectFollow sends a Follow from the personal owner.
func (a *App) connectFollow(ctx context.Context, input string) (store.ConnectActor, error) {
	base := a.connectActive(ctx)
	if base == "" {
		return store.ConnectActor{}, errConnectOff
	}
	me, err := a.connectMe(ctx)
	if err != nil {
		return store.ConnectActor{}, err
	}
	them, err := a.connectResolve(ctx, base, input)
	if err != nil {
		return store.ConnectActor{}, err
	}
	if them.Host == connect.HostOf(base) {
		return them, errors.New("That's this studio.")
	}
	id := connectNewID(base)
	act := connect.Activity(id, "Follow", connectActorURI(base, me.Name), them.URI, []string{them.URI}, nil, time.Time{})
	if err := a.db.SetConnectFollowing(ctx, me.Name, them.URI, "pending", id); err != nil {
		return them, err
	}
	return them, a.connectQueue(ctx, act, []string{them.Inbox}, 0)
}

// connectUnfollow sends Undo(Follow) and forgets the follow and its feed.
func (a *App) connectUnfollow(ctx context.Context, actorURI string) error {
	me, err := a.connectMe(ctx)
	if err != nil {
		return err
	}
	f, err := a.db.ConnectFollowingOf(ctx, me.Name, actorURI)
	if err != nil {
		return err
	}
	if base := a.connectActive(ctx); base != "" && f.Actor.Inbox != "" {
		meURI := connectActorURI(base, me.Name)
		act := connect.Activity(connectNewID(base), "Undo", meURI,
			map[string]any{"id": f.ActivityID, "type": "Follow", "actor": meURI, "object": f.ActorURI}, []string{f.ActorURI}, nil, time.Time{})
		_ = a.connectQueue(ctx, act, []string{f.Actor.Inbox}, 0)
	}
	if err := a.db.DeleteConnectFollowing(ctx, f.ID); err != nil {
		return err
	}
	return a.db.DeleteConnectFeedFrom(ctx, actorURI)
}

// connectSendAccept answers a follow request with Accept or Reject.
func (a *App) connectSendAccept(ctx context.Context, base string, o store.Owner, f store.ConnectFollow, kind string) error {
	them, err := a.db.ConnectActor(ctx, f.ActorURI)
	if err != nil {
		return err
	}
	meURI := connectActorURI(base, o.Name)
	act := connect.Activity(connectNewID(base), kind, meURI,
		map[string]any{"id": f.ActivityID, "type": "Follow", "actor": f.ActorURI, "object": meURI}, []string{f.ActorURI}, nil, time.Time{})
	return a.connectQueue(ctx, act, []string{them.Inbox}, 0)
}

// ---------- stars ----------

// connectStar stars a public repository on another studio: a Like to the
// repository, which your followers see too.
func (a *App) connectStar(ctx context.Context, repoURI string) (store.ConnectActor, error) {
	base := a.connectActive(ctx)
	if base == "" {
		return store.ConnectActor{}, errConnectOff
	}
	me, err := a.connectMe(ctx)
	if err != nil {
		return store.ConnectActor{}, err
	}
	rp, err := a.connectResolve(ctx, base, repoURI)
	if err != nil {
		return rp, err
	}
	if rp.Type != "Repository" {
		return rp, errors.New("That address isn't a repository.")
	}
	if rp.Host == connect.HostOf(base) {
		return rp, errors.New("That repository is on this studio.")
	}
	meURI := connectActorURI(base, me.Name)
	id := connectNewID(base)
	act := connect.Activity(id, "Like", meURI, map[string]any{"id": rp.URI, "type": "Repository", "name": rp.Name, "url": rp.URL},
		[]string{rp.URI}, []string{meURI + "/followers"}, time.Now())
	act["summary"] = "starred " + rp.Label()
	if err := a.db.AddConnectStarred(ctx, store.ConnectStarred{ObjectURI: rp.URI, Name: rp.Label(), URL: rp.URL,
		Inbox: rp.Inbox, ActivityID: id}); err != nil {
		return rp, err
	}
	return rp, a.connectQueue(ctx, act, append([]string{rp.Inbox}, a.connectFollowerInboxes(ctx, me.Name)...), 0)
}

// connectUnstar undoes a star.
func (a *App) connectUnstar(ctx context.Context, repoURI string) error {
	s, err := a.db.ConnectStarredOne(ctx, repoURI)
	if err != nil {
		return err
	}
	if base := a.connectActive(ctx); base != "" {
		if me, err := a.connectMe(ctx); err == nil {
			meURI := connectActorURI(base, me.Name)
			act := connect.Activity(connectNewID(base), "Undo", meURI,
				map[string]any{"id": s.ActivityID, "type": "Like", "actor": meURI, "object": s.ObjectURI}, []string{s.ObjectURI}, nil, time.Time{})
			_ = a.connectQueue(ctx, act, append([]string{s.Inbox}, a.connectFollowerInboxes(ctx, me.Name)...), 0)
		}
	}
	return a.db.DeleteConnectStarred(ctx, repoURI)
}
