package web

// Settings → Connect, the "via Connect" items on Home, followers and
// following on profiles, Connect stars on repositories and "Follow on
// Atelier" on GitHub profiles whose badge names a studio actor.

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"atelier/internal/connect"
	"atelier/internal/store"
)

func connectRoutes(a *App, m *http.ServeMux) {
	own := func(h http.HandlerFunc) http.HandlerFunc { return a.authed(a.tunnelOwner(h)) }
	m.HandleFunc("GET /settings/connect", own(a.connectPage))
	m.HandleFunc("POST /settings/connect", own(a.connectSave))
	m.HandleFunc("POST /settings/connect/follow", own(a.connectFollowSubmit))
	m.HandleFunc("POST /settings/connect/unfollow", own(a.connectUnfollowSubmit))
	m.HandleFunc("POST /settings/connect/followers/{id}/{action}", own(a.connectFollowerSubmit))
	m.HandleFunc("POST /settings/connect/rules", own(a.connectRuleSubmit))
	m.HandleFunc("POST /settings/connect/star", own(a.connectStarSubmit))
	m.HandleFunc("POST /settings/connect/unstar", own(a.connectUnstarSubmit))
	m.HandleFunc("POST /settings/connect/offer", own(a.connectOfferSubmit))
}

func init() {
	registerTemplateFuncs(func(a *App) template.FuncMap {
		return template.FuncMap{
			"connectProfile":     a.connectProfileData,
			"connectStars":       a.connectStarsFor,
			"connectBadgeFollow": a.connectBadgeFollow,
		}
	})
}

// ---------- the settings page ----------

type connectRepoStars struct {
	Repo  store.Repo
	Count int
}

type connectView struct {
	Settings    store.ConnectSettings
	Base        string
	Why         string
	Active      bool
	Handle      string
	ActorURL    string
	Fingerprint string
	KeyErr      string
	AllowLocal  bool
	Following   []store.ConnectFollow
	Followers   []store.ConnectFollow
	Pending     []store.ConnectFollow
	Rules       []store.ConnectRule
	Starred     []store.ConnectStarred
	Stars       []connectRepoStars
	Log         []store.ConnectDelivery
	PublicRepos []store.Repo
	Saved       bool
	Notice      string
	Error       string
}

func (a *App) connectData(r *http.Request) (connectView, error) {
	ctx := r.Context()
	c, err := a.db.ConnectSettings(ctx)
	if err != nil {
		return connectView{}, err
	}
	me, err := a.connectMe(ctx)
	if err != nil {
		return connectView{}, err
	}
	v := connectView{Settings: c, AllowLocal: a.connect().allowLocal}
	v.Base, v.Why = a.connectBaseFor(c)
	v.Active = c.Enabled && v.Base != ""
	if k, err := a.connectKey(); err != nil {
		v.KeyErr = err.Error()
	} else {
		v.Fingerprint = connectFingerprint(k)
	}
	if v.Base != "" {
		v.Handle = "@" + strings.ToLower(me.Name) + "@" + connect.HostOf(v.Base)
		v.ActorURL = connectActorURI(v.Base, me.Name)
	}
	v.Following, _ = a.db.ConnectFollowing(ctx, me.Name)
	all, _ := a.db.ConnectFollowers(ctx, me.Name, "")
	for _, f := range all {
		if f.State == "pending" {
			v.Pending = append(v.Pending, f)
		} else {
			v.Followers = append(v.Followers, f)
		}
	}
	v.Rules, _ = a.db.ConnectRules(ctx)
	v.Starred, _ = a.db.ConnectStarredList(ctx)
	counts, _ := a.db.ConnectStarCounts(ctx)
	repos, _ := a.db.Repos(ctx)
	for _, rp := range repos {
		if !a.connectFederates(ctx, rp) {
			continue
		}
		v.PublicRepos = append(v.PublicRepos, rp)
		if n := counts[rp.ID]; n > 0 {
			v.Stars = append(v.Stars, connectRepoStars{Repo: rp, Count: n})
		}
	}
	sort.Slice(v.PublicRepos, func(i, j int) bool { return v.PublicRepos[i].FullName() < v.PublicRepos[j].FullName() })
	v.Log, _ = a.db.ConnectDeliveryLog(ctx, connectLogShown)
	return v, nil
}

func (a *App) connectRender(w http.ResponseWriter, r *http.Request, status int, v connectView) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	a.render(w, r, "connect_settings", page{Title: "Connect", Nav: "tokens", Error: v.Error, Data: v})
}

func (a *App) connectPage(w http.ResponseWriter, r *http.Request) {
	v, err := a.connectData(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v.Saved = r.URL.Query().Get("saved") != ""
	v.Notice = connectNotices[r.URL.Query().Get("done")]
	a.connectRender(w, r, http.StatusOK, v)
}

var connectNotices = map[string]string{
	"follow":   "Follow request sent. It shows as pending until their studio accepts.",
	"unfollow": "Unfollowed.",
	"accept":   "Accepted.",
	"reject":   "Declined.",
	"remove":   "Removed.",
	"rule":     "Saved.",
	"star":     "Starred.",
	"unstar":   "Star removed.",
	"offer":    "Branch offered. Their studio opens a pull request once it has fetched it.",
}

// connectFail re-renders the page with an error.
func (a *App) connectFail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	v, err := a.connectData(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v.Error = msg
	a.connectRender(w, r, status, v)
}

func connectDone(w http.ResponseWriter, r *http.Request, what string) {
	next := safeNext(r.FormValue("next"))
	if next == "/" {
		next = "/settings/connect"
	}
	sep := "?"
	if strings.Contains(next, "?") {
		sep = "&"
	}
	http.Redirect(w, r, next+sep+"done="+what, http.StatusSeeOther)
}

func connectErrText(err error) string {
	if errors.Is(err, errConnectOff) {
		return "Connect is off or has no public address, so nothing was sent."
	}
	var se *connect.StatusError
	if errors.As(err, &se) {
		return "The other studio answered " + strconv.Itoa(se.Code) + "."
	}
	if errors.Is(err, connect.ErrBlocked) {
		return "That address is on a private or local network, which Connect never contacts."
	}
	msg := err.Error()
	if msg != "" && strings.ToUpper(msg[:1]) == msg[:1] {
		return msg
	}
	return "Couldn't reach that studio: " + msg
}

func (a *App) connectSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := a.db.ConnectSettings(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	switch r.FormValue("action") {
	case "on":
		c.Enabled = true
		if _, err := a.connectKey(); err != nil {
			a.connectFail(w, r, http.StatusInternalServerError, "The studio key couldn't be created: "+err.Error())
			return
		}
	case "off":
		c.Enabled = false
	default:
		c.AutoAccept = r.FormValue("auto_accept") != ""
		c.AllowlistOnly = r.FormValue("allowlist_only") != ""
	}
	if err := a.db.SaveConnectSettings(ctx, c, a.actor(r).Name); err != nil {
		a.fail(w, r, err)
		return
	}
	a.connect().signal()
	http.Redirect(w, r, "/settings/connect?saved=1", http.StatusSeeOther)
}

func (a *App) connectFollowSubmit(w http.ResponseWriter, r *http.Request) {
	if _, err := a.connectFollow(r.Context(), r.FormValue("target")); err != nil {
		a.connectFail(w, r, http.StatusBadRequest, connectErrText(err))
		return
	}
	connectDone(w, r, "follow")
}

func (a *App) connectUnfollowSubmit(w http.ResponseWriter, r *http.Request) {
	if err := a.connectUnfollow(r.Context(), r.FormValue("actor")); err != nil {
		a.connectFail(w, r, http.StatusBadRequest, connectErrText(err))
		return
	}
	connectDone(w, r, "unfollow")
}

func (a *App) connectFollowerSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f, err := a.db.ConnectFollowerByID(ctx, id)
	if err != nil {
		a.notFound(w, r)
		return
	}
	o, ok := a.connectLocal(ctx, f.Local)
	if !ok {
		a.notFound(w, r)
		return
	}
	base := a.connectActive(ctx)
	action := r.PathValue("action")
	switch action {
	case "accept":
		if base == "" {
			a.connectFail(w, r, http.StatusConflict, connectErrText(errConnectOff))
			return
		}
		if err := a.db.SetConnectFollowerState(ctx, f.ID, "accepted"); err != nil {
			a.fail(w, r, err)
			return
		}
		if err := a.connectSendAccept(ctx, base, o, f, "Accept"); err != nil {
			a.connectFail(w, r, http.StatusBadGateway, connectErrText(err))
			return
		}
	case "reject", "remove":
		if base != "" {
			_ = a.connectSendAccept(ctx, base, o, f, "Reject")
		}
		if err := a.db.DeleteConnectFollower(ctx, f.ID); err != nil {
			a.fail(w, r, err)
			return
		}
	default:
		a.notFound(w, r)
		return
	}
	connectDone(w, r, action)
}

// connectRuleHost reads a studio host from a host name, handle or URL.
func connectRuleHost(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if strings.Contains(s, "://") {
		return connect.HostOf(s)
	}
	if _, h, err := connect.ParseHandle(s); err == nil {
		return h
	}
	if _, h, err := connect.ParseHandle("x@" + s); err == nil {
		return h
	}
	return ""
}

func (a *App) connectRuleSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	host := connectRuleHost(r.FormValue("host"))
	if host == "" {
		a.connectFail(w, r, http.StatusBadRequest, "Enter a studio's host name, like studio.example.")
		return
	}
	switch kind := r.FormValue("kind"); kind {
	case "block":
		if err := a.db.SetConnectRule(ctx, host, "block"); err != nil {
			a.fail(w, r, err)
			return
		}
		if err := a.db.ForgetConnectHost(ctx, host); err != nil {
			a.fail(w, r, err)
			return
		}
	case "allow":
		if err := a.db.SetConnectRule(ctx, host, "allow"); err != nil {
			a.fail(w, r, err)
			return
		}
	case "remove":
		if err := a.db.DeleteConnectRule(ctx, host); err != nil {
			a.fail(w, r, err)
			return
		}
	default:
		a.connectFail(w, r, http.StatusBadRequest, "Choose block or allow.")
		return
	}
	connectDone(w, r, "rule")
}

func (a *App) connectStarSubmit(w http.ResponseWriter, r *http.Request) {
	if _, err := a.connectStar(r.Context(), r.FormValue("repo")); err != nil {
		a.connectFail(w, r, http.StatusBadRequest, connectErrText(err))
		return
	}
	connectDone(w, r, "star")
}

func (a *App) connectUnstarSubmit(w http.ResponseWriter, r *http.Request) {
	if err := a.connectUnstar(r.Context(), r.FormValue("repo")); err != nil {
		a.connectFail(w, r, http.StatusBadRequest, connectErrText(err))
		return
	}
	connectDone(w, r, "unstar")
}

// ---------- Home ----------

// connectFeedView is one "via Connect" item on Home.
type connectFeedView struct {
	store.ConnectFeedItem
	Who     string // display name
	Handle  string
	Starred bool // you've starred the object (a repository)
	CanStar bool
}

// connectHomeFeed adds what people you follow did to Home, for the owner.
func (a *App) connectHomeFeed(ctx context.Context, who Actor, feed []homeItem, max int) []homeItem {
	if !a.isOwnerPerson(ctx, who.PersonID) {
		return feed
	}
	items, err := a.db.ConnectFeed(ctx, connectFeedShown)
	if err != nil || len(items) == 0 {
		return feed
	}
	starred, _ := a.db.ConnectStarredList(ctx)
	have := map[string]bool{}
	for _, s := range starred {
		have[s.ObjectURI] = true
	}
	for _, it := range items {
		v := &connectFeedView{ConnectFeedItem: it, Who: it.Actor.Name, Handle: store.ConnectActor{Host: it.Actor.Host, Username: it.Actor.Username}.Handle()}
		if v.Who == "" {
			v.Who = v.Handle
		}
		v.CanStar = it.ObjectURI != "" && (it.Kind == "repo" || it.Kind == "push" || it.Kind == "release" || it.Kind == "star")
		v.Starred = have[it.ObjectURI]
		feed = append(feed, homeItem{When: it.Published, Connect: v})
	}
	sort.SliceStable(feed, func(i, j int) bool { return feed[i].When.After(feed[j].When) })
	if len(feed) > max {
		feed = feed[:max]
	}
	return feed
}

// ---------- profiles ----------

type connectProfileView struct {
	Handle    string
	Active    bool
	Owner     bool // the viewer is the owner: show the lists
	Followers []store.ConnectFollow
	Following []store.ConnectFollow
}

// connectProfileData is the Connect section of a profile: counts for
// everyone, the lists for the owner. Nil when there's nothing to show.
func (a *App) connectProfileData(o store.Owner, canEdit bool) *connectProfileView {
	ctx := context.Background()
	if _, ok := a.connectLocal(ctx, o.Name); !ok {
		return nil
	}
	v := &connectProfileView{Owner: canEdit}
	if base := a.connectActive(ctx); base != "" {
		v.Active = true
		v.Handle = "@" + strings.ToLower(o.Name) + "@" + connect.HostOf(base)
	}
	v.Followers, _ = a.db.ConnectFollowers(ctx, o.Name, "accepted")
	fs, _ := a.db.ConnectFollowing(ctx, o.Name)
	for _, f := range fs {
		if f.State == "accepted" {
			v.Following = append(v.Following, f)
		}
	}
	if !v.Active && len(v.Followers) == 0 && len(v.Following) == 0 {
		return nil
	}
	return v
}

// connectStarsFor is a repository's star count from other studios.
func (a *App) connectStarsFor(rp store.Repo) int {
	if rp.Private {
		return 0
	}
	return a.db.ConnectStarCount(context.Background(), rp.ID)
}

// ---------- badges ----------

// ConnectBadgeActor is what the .atelier badge publishes as "connect": the
// personal owner's actor URL, when Connect is on and the studio has a public
// address; "" otherwise. tether_badge.go calls it.
func (a *App) ConnectBadgeActor(ctx context.Context, personID int64) string {
	base := a.connectActive(ctx)
	if base == "" || !a.isOwnerPerson(ctx, personID) {
		return ""
	}
	me, err := a.connectMe(ctx)
	if err != nil {
		return ""
	}
	return connectActorURI(base, me.Name)
}

// connectNoteBadge remembers the actor URL a GitHub badge names.
func (a *App) connectNoteBadge(ctx context.Context, login, actor string) {
	if actor != "" && (!connect.SafeLink(actor) || len(actor) > 300) {
		actor = ""
	}
	_ = a.db.SaveConnectBadge(ctx, login, actor)
}

type connectBadgeView struct {
	Actor     string
	Following bool
	Pending   bool
}

// connectBadgeFollow is what /gh/<login> needs for "Follow on Atelier".
func (a *App) connectBadgeFollow(login string) *connectBadgeView {
	ctx := context.Background()
	actor := a.db.ConnectBadge(ctx, login)
	if actor == "" || a.connectActive(ctx) == "" {
		return nil
	}
	v := &connectBadgeView{Actor: actor}
	if me, err := a.connectMe(ctx); err == nil {
		if f, err := a.db.ConnectFollowingOf(ctx, me.Name, actor); err == nil {
			v.Following, v.Pending = f.State == "accepted", f.State == "pending"
		}
	}
	return v
}
