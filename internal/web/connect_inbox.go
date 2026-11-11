package web

// Connect's inbox: everything other studios send. Every POST must carry a
// valid HTTP Signature by the actor it claims to come from, with a Digest of
// the body and a Date within connectSkew; a signature is accepted once
// (replays are refused) and an activity id once. Bodies are capped at 1 MB
// and parsed strictly. Blocked studios (and, with the allowlist on, studios
// not on it) are refused before any work, and each studio is rate-limited.
//
// Nothing here grants anything: a follow request only records who asked,
// a Like only counts a star, and an offered branch becomes a pull request
// whose Actions wait for the owner's approval.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"atelier/internal/connect"
	"atelier/internal/store"
)

// EventConnectFollowed tells the owner someone followed them (or asked to).
const EventConnectFollowed = "connect.followed"

type inboxErr struct {
	status int
	msg    string
}

func (e *inboxErr) Error() string { return e.msg }

func inboxFail(status int, msg string) error { return &inboxErr{status: status, msg: msg} }

func (a *App) connectInbox(w http.ResponseWriter, r *http.Request, base, local string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st := a.connect()
	if !st.ipRate.allow(a.clientIP(r)) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	if ct := strings.ToLower(r.Header.Get("Content-Type")); !strings.Contains(ct, "json") {
		http.Error(w, "expected application/activity+json", http.StatusUnsupportedMediaType)
		return
	}
	if r.ContentLength > connect.MaxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, connect.MaxBody))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	status, msg := http.StatusAccepted, "accepted"
	if err := a.connectReceive(r.Context(), r, base, body); err != nil {
		var ie *inboxErr
		if errors.As(err, &ie) {
			status, msg = ie.status, ie.msg
		} else {
			logErr(r, err)
			status, msg = http.StatusInternalServerError, "internal error"
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

// connectHostAllowed applies the block list and, when on, the allowlist.
func (a *App) connectHostAllowed(ctx context.Context, host string) bool {
	if host == "" {
		return false
	}
	rule := a.db.ConnectRuleFor(ctx, host)
	if rule == "block" {
		return false
	}
	if c, err := a.db.ConnectSettings(ctx); err == nil && c.AllowlistOnly && rule != "allow" {
		return false
	}
	return true
}

// connectReceive verifies and handles one inbound activity.
func (a *App) connectReceive(ctx context.Context, r *http.Request, base string, body []byte) error {
	st := a.connect()
	now := st.now()
	sig, err := connect.ParseSignature(r.Header.Get("Signature"))
	if err != nil {
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Signature ") {
			sig, err = connect.ParseSignature(auth)
		}
	}
	if err != nil {
		return inboxFail(http.StatusUnauthorized, "signature required: "+err.Error())
	}
	actorURI := connect.StripFragment(sig.KeyID)
	host := connect.HostOf(actorURI)
	if host == "" || !connect.SafeLink(actorURI) {
		return inboxFail(http.StatusUnauthorized, "keyId is not a URL")
	}
	if host == connect.HostOf(base) {
		return inboxFail(http.StatusForbidden, "that's this studio")
	}
	if !a.connectHostAllowed(ctx, host) {
		return inboxFail(http.StatusForbidden, "this studio doesn't accept activities from "+host)
	}
	// Cheap checks before any network work: the clock and the digest.
	if t, err := http.ParseTime(r.Header.Get("Date")); err != nil || now.Sub(t) > connectSkew || t.Sub(now) > connectSkew {
		return inboxFail(http.StatusUnauthorized, connect.ErrSigClock.Error())
	}
	if !connect.DigestMatches(r.Header.Get("Digest"), body) {
		return inboxFail(http.StatusUnauthorized, connect.ErrSigDigest.Error())
	}

	// Verify against the signer's key, refreshing the actor once if the
	// cached key doesn't match (keys can rotate).
	vreq := *r
	_, vreq.Host = a.external(r)
	actor, fresh, err := a.connectRemoteActor(ctx, base, actorURI, false)
	if err != nil {
		return inboxFail(http.StatusUnauthorized, "couldn't fetch the signer: "+err.Error())
	}
	verify := func(ac store.ConnectActor) error {
		if ac.KeyID != sig.KeyID {
			return connect.ErrSigInvalid
		}
		pub, err := connect.ParsePublicKeyPEM(ac.KeyPEM)
		if err != nil {
			return err
		}
		return connect.VerifyRequest(&vreq, body, pub, sig, now, connectSkew)
	}
	err = verify(actor)
	if err != nil && !fresh && (errors.Is(err, connect.ErrSigInvalid) || actor.KeyID != sig.KeyID) {
		if actor, _, err = a.connectRemoteActor(ctx, base, actorURI, true); err == nil {
			err = verify(actor)
		}
	}
	if err != nil {
		return inboxFail(http.StatusUnauthorized, "signature: "+err.Error())
	}
	// Counted per studio only once verified, so nobody can use up another studio's allowance.
	if !st.hostRate.allow(host) {
		return inboxFail(http.StatusTooManyRequests, "too many activities from "+host)
	}
	// Each signature only once.
	sum := sha256.Sum256([]byte(r.Header.Get("Signature") + "\x00" + r.Header.Get("Authorization")))
	if first, err := a.db.ConnectFirstSeen(ctx, "sig:"+hex.EncodeToString(sum[:]), 2*connectSkew); err != nil {
		return err
	} else if !first {
		return inboxFail(http.StatusUnauthorized, "replayed request")
	}

	doc, err := connect.DecodeJSON(body, connect.MaxBody)
	if err != nil {
		return inboxFail(http.StatusBadRequest, err.Error())
	}
	if doc.ID("actor") != actor.URI {
		return inboxFail(http.StatusForbidden, "the activity's actor isn't the signer")
	}
	id := doc.Str("id")
	if id == "" || connect.HostOf(id) != host || len(id) > 2048 {
		return inboxFail(http.StatusBadRequest, "the activity needs an id on the sender's host")
	}
	if first, err := a.db.ConnectFirstSeen(ctx, "act:"+id, 30*24*time.Hour); err != nil {
		return err
	} else if !first {
		return nil // already handled: accept quietly
	}
	return a.connectDispatch(ctx, base, actor, doc)
}

// connectRemoteActor returns a remote actor, from the cache while it's fresh.
// fresh says whether it was just fetched.
func (a *App) connectRemoteActor(ctx context.Context, base, uri string, force bool) (store.ConnectActor, bool, error) {
	if !force {
		if c, err := a.db.ConnectActor(ctx, uri); err == nil && time.Since(c.FetchedAt) < connectActorTTL {
			return c, false, nil
		}
	}
	host := connect.HostOf(uri)
	if !a.connectHostAllowed(ctx, host) {
		return store.ConnectActor{}, false, errors.New("that studio is blocked")
	}
	cl, err := a.connectClient(ctx, base)
	if err != nil {
		return store.ConnectActor{}, false, err
	}
	d, err := cl.Get(ctx, uri, connect.ContentType+", "+connect.LDContentType)
	if err != nil {
		return store.ConnectActor{}, false, err
	}
	ra, err := connect.ParseActor(d, uri)
	if err != nil {
		return store.ConnectActor{}, false, err
	}
	if ra.CloneURI != "" && connect.HostOf(ra.CloneURI) != host {
		ra.CloneURI = ""
	}
	c, err := a.db.SaveConnectActor(ctx, store.ConnectActor{URI: ra.ID, Host: host, Type: ra.Type, Username: ra.Username,
		Name: ra.Name, Summary: ra.Summary, URL: ra.URL, Inbox: ra.Inbox, SharedInbox: ra.SharedInbox, Outbox: ra.Outbox,
		KeyID: ra.KeyID, KeyPEM: ra.KeyPEM, CloneURI: ra.CloneURI, AttributedTo: ra.AttributedTo})
	return c, true, err
}

// connectLocalFromURI is the local actor a URI names ("" if none).
func (a *App) connectLocalFromURI(ctx context.Context, base, uri string) (store.Owner, bool) {
	name, ok := strings.CutPrefix(uri, base+"/connect/users/")
	if !ok || strings.Contains(name, "/") {
		return store.Owner{}, false
	}
	return a.connectLocal(ctx, name)
}

// connectRepoFromURI is the public repository a URI names.
func (a *App) connectRepoFromURI(ctx context.Context, base, uri string) (store.Repo, bool) {
	rest, ok := strings.CutPrefix(uri, base+"/connect/repos/")
	if !ok {
		return store.Repo{}, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return store.Repo{}, false
	}
	return a.connectPublicRepo(ctx, parts[0], parts[1])
}

func (a *App) connectDispatch(ctx context.Context, base string, actor store.ConnectActor, doc connect.Doc) error {
	id := doc.Str("id")
	switch doc.Type() {
	case "Follow":
		o, ok := a.connectLocalFromURI(ctx, base, doc.ID("object"))
		if !ok {
			return inboxFail(http.StatusNotFound, "no such actor here")
		}
		c, _ := a.db.ConnectSettings(ctx)
		state := "pending"
		if c.AutoAccept {
			state = "accepted"
		}
		f, err := a.db.SetConnectFollower(ctx, o.Name, actor.URI, state, id)
		if err != nil {
			return err
		}
		if f.State == "accepted" {
			if err := a.connectSendAccept(ctx, base, o, f, "Accept"); err != nil {
				return err
			}
		}
		if me, err := a.connectMe(ctx); err == nil {
			title := actor.Handle() + " followed " + o.Name
			if f.State == "pending" {
				title = actor.Handle() + " asked to follow " + o.Name
			}
			a.emit(ctx, Event{Kind: EventConnectFollowed, Title: title, URL: "/settings/connect",
				Actor: Actor{Kind: "connect", Name: actor.Handle()}, Recipients: []int64{me.ID}})
		}
		return nil

	case "Undo":
		inner := doc.Obj("object")
		innerID := doc.ID("object")
		if inner != nil {
			if inner.ID("actor") != "" && inner.ID("actor") != actor.URI {
				return inboxFail(http.StatusForbidden, "can't undo someone else's activity")
			}
			switch inner.Type() {
			case "Follow":
				if o, ok := a.connectLocalFromURI(ctx, base, inner.ID("object")); ok {
					_, err := a.db.DeleteConnectFollowerActor(ctx, o.Name, actor.URI)
					return err
				}
				return a.db.DeleteConnectFollowerByActivity(ctx, actor.URI, innerID)
			case "Like":
				if rp, ok := a.connectRepoFromURI(ctx, base, inner.ID("object")); ok {
					if err := a.db.RemoveConnectStar(ctx, rp.ID, actor.URI); err != nil {
						return err
					}
				}
			}
		}
		if err := a.db.DeleteConnectFollowerByActivity(ctx, actor.URI, innerID); err != nil {
			return err
		}
		if err := a.db.RemoveConnectStarByActivity(ctx, actor.URI, innerID); err != nil {
			return err
		}
		return a.db.DeleteConnectFeedActivity(ctx, actor.URI, innerID)

	case "Accept", "Reject":
		f, err := a.db.ConnectFollowingByActivity(ctx, doc.ID("object"))
		if err != nil || f.ActorURI != actor.URI {
			return nil // not a follow of ours, or not theirs to answer
		}
		if doc.Type() == "Accept" {
			return a.db.SetConnectFollowingState(ctx, f.ID, "accepted")
		}
		return a.db.SetConnectFollowingState(ctx, f.ID, "rejected")

	case "Like":
		if rp, ok := a.connectRepoFromURI(ctx, base, doc.ID("object")); ok {
			_, err := a.db.AddConnectStar(ctx, rp.ID, actor.URI, id)
			return err
		}
		return a.connectFeedFrom(ctx, actor, doc)

	case "Create", "Announce", "Push":
		return a.connectFeedFrom(ctx, actor, doc)

	case "Offer":
		return a.connectReceiveOffer(ctx, base, actor, doc)

	case "Delete":
		if doc.ID("object") == actor.URI { // the actor is gone
			for _, l := range a.connectLocalNames(ctx) {
				_, _ = a.db.DeleteConnectFollowerActor(ctx, l, actor.URI)
			}
			return a.db.DeleteConnectFeedFrom(ctx, actor.URI)
		}
		return a.db.DeleteConnectFeedObject(ctx, actor.URI, doc.ID("object"))

	case "Update":
		if doc.ID("object") == actor.URI {
			_, _, err := a.connectRemoteActor(ctx, base, actor.URI, true)
			return err
		}
	}
	return nil // anything else is accepted and ignored
}

// connectLocalNames lists the local actors.
func (a *App) connectLocalNames(ctx context.Context) []string {
	owners, _ := a.db.Owners(ctx)
	var out []string
	for _, o := range owners {
		if _, ok := a.connectLocal(ctx, o.Name); ok {
			out = append(out, o.Name)
		}
	}
	return out
}

// connectFeedFrom keeps an activity from someone a local account follows.
// Everything in it is untrusted: it's reduced to plain text, links must be
// http(s) on the sender's own host, and objects must be the sender's own.
func (a *App) connectFeedFrom(ctx context.Context, actor store.ConnectActor, doc connect.Doc) error {
	if !a.db.ConnectFollowsAccepted(ctx, actor.URI) {
		return nil // not someone we follow: ignore quietly
	}
	host := actor.Host
	link := func(u string) string {
		if u != "" && connect.SafeLink(u) && connect.HostOf(u) == host {
			return u
		}
		return ""
	}
	it := store.ConnectFeedItem{ActivityID: doc.Str("id"), ActorURI: actor.URI, Published: doc.Time("published")}
	if now := time.Now(); it.Published.IsZero() || it.Published.After(now) {
		it.Published = now
	}
	summary := connect.Clip(connect.PlainText(doc.Str("summary")), 200)
	obj := doc.Obj("object")
	objID := doc.ID("object")
	own := func(o connect.Doc) bool {
		if o == nil || connect.HostOf(o.Str("id")) != host {
			return false
		}
		at := o.ID("attributedTo")
		return at == "" || connect.HostOf(at) == host
	}
	switch doc.Type() {
	case "Create":
		if !own(obj) {
			return nil
		}
		name := connect.Clip(connect.PlainText(obj.Str("name")), 120)
		it.Link = link(obj.ID("url"))
		switch obj.Type() {
		case "Repository":
			it.Kind, it.Text = "repo", "created a repository"
			it.ObjectURI, it.ObjectName = obj.Str("id"), name
			it.Detail = connect.Clip(connect.PlainText(obj.Str("summary")), 280)
		case "Note", "Article":
			it.Kind, it.Text = "note", "wrote"
			if obj.Str("atelierKind") == "release" {
				it.Kind, it.Text = "release", "published a release "+name
				it.ObjectURI = obj.ID("context")
			}
			it.Detail = connect.Clip(connect.PlainText(obj.Str("content")), 280)
		default:
			return nil
		}
	case "Push":
		it.Kind, it.Text = "push", "pushed"
		it.ObjectURI = doc.ID("context")
		if connect.HostOf(it.ObjectURI) != host {
			return nil
		}
		it.ObjectName = connect.Clip(connect.PlainText(doc.Str("atelierRepo")), 120)
		it.Link = link(doc.ID("url"))
		if obj != nil {
			var commits []connect.Doc
			_ = json.Unmarshal(obj["orderedItems"], &commits)
			var lines []string
			for i, c := range commits {
				if i == 3 {
					break
				}
				if s := connect.Clip(connect.PlainText(c.Str("summary")), 90); s != "" {
					lines = append(lines, s)
				}
			}
			it.Detail = strings.Join(lines, " · ")
		}
	case "Like":
		it.Kind, it.Text = "star", "starred"
		it.ObjectURI = objID
		if obj != nil {
			it.ObjectName = connect.Clip(connect.PlainText(obj.Str("name")), 120)
			if u := obj.ID("url"); u != "" && connect.SafeLink(u) && connect.HostOf(u) == connect.HostOf(objID) {
				it.Link = u
			}
		}
	case "Announce":
		it.Kind, it.Text = "share", "shared"
		it.ObjectURI = objID
		if connect.SafeLink(objID) {
			it.Link = objID
		}
	default:
		return nil
	}
	if summary != "" {
		// The sender's own wording, without the object's name at the end
		// (it's shown next to it as a link).
		if it.ObjectName != "" && strings.HasSuffix(summary, it.ObjectName) {
			summary = strings.TrimRight(strings.TrimSuffix(summary, it.ObjectName), ": ")
		}
		if summary != "" {
			it.Text = summary
		}
	}
	if it.ObjectURI != "" && !connect.SafeLink(it.ObjectURI) {
		it.ObjectURI = ""
	}
	_, err := a.db.AddConnectFeed(ctx, it)
	return err
}
