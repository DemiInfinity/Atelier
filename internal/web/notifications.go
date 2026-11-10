package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"atelier/internal/store"
)

// Notifications inbox
//
// The default event handler turns events into notifications for every person
// who may read the repository (in core: the owner), except the person who did
// it in person. Actions taken through an access token, a bot or an agent are
// never "your own" — you want to hear what Claude did with your token.
//
// Background work that doesn't emit events itself yet (backups, imports) is
// watched by a small poller that emits on its behalf; see watchSystemSources.

func init() {
	registerFeature(feature{
		name: "notifications",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /notifications", a.authed(a.notificationsPage))
			m.HandleFunc("GET /notifications/count", a.authed(a.notificationsCount))
			m.HandleFunc("GET /notifications/{id}", a.authed(a.notificationOpen))
			m.HandleFunc("POST /notifications/{id}/done", a.authed(a.notificationDone))
			m.HandleFunc("POST /notifications/done", a.authed(a.notificationsAllDone))
		},
		start: func(ctx context.Context, a *App) { a.watchSystemSources(ctx, notifyPollEvery) },
	})
	registerEventHandler(notifyHandler)
}

// ---------- event → notifications ----------

const (
	ReasonSubscribed = "subscribed" // activity on a repository you can read
	ReasonSystem     = "system"     // backups, imports, other instance events
)

// notifyHandler is the default handler: one notification per recipient.
func notifyHandler(ctx context.Context, a *App, ev Event) {
	subject := ev.Subject()
	if subject == "" || ev.Title == "" {
		return
	}
	var repo *store.Repo
	if ev.RepoID != 0 {
		rp := ev.Repo
		if rp.ID != ev.RepoID {
			found, err := a.repoByID(ctx, ev.RepoID)
			if err != nil {
				log.Printf("notifications: %s: repo %d: %v", ev.Kind, ev.RepoID, err)
				return
			}
			rp = found
		}
		repo = &rp
	}
	people, err := a.notificationRecipients(ctx, ev, repo)
	if err != nil {
		log.Printf("notifications: %s: recipients: %v", ev.Kind, err)
		return
	}
	reason := ev.Reason
	if reason == "" {
		reason = ReasonSystem
		if repo != nil {
			reason = ReasonSubscribed
		}
	}
	for _, p := range people {
		if ownAction(ev.Actor, p) {
			continue
		}
		if _, err := a.db.Notify(ctx, store.NewNotification{
			PersonID: p, Kind: ev.Kind, RepoID: ev.RepoID, SubjectType: subject, Number: ev.Number,
			Title: eventExcerpt(ev.Title, 256), Detail: eventExcerpt(ev.Detail, 280), URL: safeLocalURL(ev.URL),
			ActorKind: ev.Actor.Kind, ActorName: ev.Actor.Name, Reason: reason,
		}); err != nil {
			log.Printf("notifications: %s: notify %d: %v", ev.Kind, p, err)
		}
	}
}

// ownAction: a person isn't told about what they did themselves, in person.
// Tokens, bots and agents acting for them don't count as "themselves".
func ownAction(who Actor, person int64) bool {
	return who.PersonID != 0 && who.PersonID == person && who.Kind == ActorOwner
}

// notificationRecipients is everyone who should hear about ev: people who can
// read its repository, or for instance-wide events the personal owner (the
// one administrator in core). ev.Recipients narrows the list.
func (a *App) notificationRecipients(ctx context.Context, ev Event, repo *store.Repo) ([]int64, error) {
	owners, err := a.db.Owners(ctx)
	if err != nil {
		return nil, err
	}
	var want map[int64]bool
	if len(ev.Recipients) > 0 {
		want = map[int64]bool{}
		for _, id := range ev.Recipients {
			want[id] = true
		}
	}
	var out []int64
	for _, o := range owners {
		if o.Kind != "user" || (want != nil && !want[o.ID]) {
			continue
		}
		if repo != nil {
			if !a.can(ctx, personActor(o), *repo, PermRead) {
				continue
			}
		} else if !a.isOwnerPerson(ctx, o.ID) && want == nil {
			continue
		}
		out = append(out, o.ID)
	}
	return out, nil
}

// personActor is a person acting in person (used to ask a.can on their behalf).
func personActor(o store.Owner) Actor {
	return Actor{Kind: ActorOwner, Name: o.Name, PersonID: o.ID}
}

func (a *App) repoByID(ctx context.Context, id int64) (store.Repo, error) {
	repos, err := a.db.Repos(ctx)
	if err != nil {
		return store.Repo{}, err
	}
	for _, r := range repos {
		if r.ID == id {
			return r, nil
		}
	}
	return store.Repo{}, store.ErrNotFound
}

// safeLocalURL keeps only site-relative links ("/x"), never "//host" or schemes.
func safeLocalURL(u string) string {
	if !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") || strings.HasPrefix(u, "/\\") {
		return ""
	}
	return u
}

// ---------- watching backups and imports ----------

var notifyPollEvery = 15 * time.Second

const settingNotifySeeded = "notifications.sources_seeded"

// watchSystemSources turns finished backup runs and import jobs into events.
// The first run only records what already exists, so history isn't announced.
//
// TODO(shared): once backups.go and importer.go emit EventBackup* / EventImport*
// themselves, delete this poller.
func (a *App) watchSystemSources(ctx context.Context, every time.Duration) {
	_, err := a.db.Setting(ctx, settingNotifySeeded)
	seeding := err != nil
	a.pollSystemSources(ctx, seeding)
	if seeding {
		_ = a.db.SetSetting(ctx, settingNotifySeeded, "1")
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.pollSystemSources(ctx, false)
		}
	}
}

// pollSystemSources looks at recent backup runs and import jobs. With silent
// set it only marks them seen.
func (a *App) pollSystemSources(ctx context.Context, silent bool) {
	if runs, err := a.db.BackupRuns(ctx, 20); err == nil {
		for i := len(runs) - 1; i >= 0; i-- { // oldest first
			run := runs[i]
			first, err := a.db.MarkNotificationSourceSeen(ctx, "backup_run", run.ID)
			if err != nil || !first || silent {
				continue
			}
			if ev, ok := backupEvent(run); ok {
				a.emit(ctx, ev)
			}
		}
	}
	if jobs, err := a.db.ImportJobs(ctx, 50); err == nil {
		for i := len(jobs) - 1; i >= 0; i-- {
			j := jobs[i]
			if j.Status != "done" && j.Status != "failed" {
				continue
			}
			first, err := a.db.MarkNotificationSourceSeen(ctx, "import_job", j.ID)
			if err != nil || !first || silent {
				continue
			}
			a.emit(ctx, a.importEvent(ctx, j))
		}
	}
}

// backupEvent: failures always notify; successes only for runs someone asked
// for (a nightly "it worked" every day would just be noise).
func backupEvent(run store.BackupRun) (Event, bool) {
	ev := Event{Number: 0, URL: "/settings/backups", Actor: Actor{Kind: ActorSystem, Name: "backups"}}
	if run.OK {
		if run.Trigger == "schedule" {
			return Event{}, false
		}
		ev.Kind, ev.Title = EventBackupSucceeded, "Backup finished"
		ev.Detail = fmt.Sprintf("%s · %s", humanBytes(run.Size), run.Destinations)
	} else {
		ev.Kind, ev.Title = EventBackupFailed, "Backup failed"
		ev.Detail = run.Error
	}
	if run.ActorName != "" && run.Trigger != "schedule" {
		ev.Detail = strings.TrimSpace(ev.Detail + " · started by " + run.ActorName)
	}
	return ev, true
}

func (a *App) importEvent(ctx context.Context, j store.ImportJob) Event {
	ev := Event{Number: j.ID, Actor: Actor{Kind: ActorSystem, Name: "import"}, Detail: j.Message}
	if j.Status == "done" {
		ev.Kind, ev.Title, ev.URL = EventImportDone, "Imported "+j.FullName(), "/"+j.FullName()
		if rp, err := a.db.RepoByName(ctx, j.Owner, j.Name); err == nil {
			ev.Repo, ev.RepoID = rp, rp.ID
		}
	} else {
		ev.Kind, ev.Title, ev.URL = EventImportFailed, "Import of "+j.FullName()+" failed", "/settings/import"
	}
	return ev
}

// ---------- inbox page ----------

const notificationsPerPage = 25

// notifFilter is one entry of the filter sidebar.
type notifFilter struct {
	Key, Label, Soon string // Soon: placeholder text for features that haven't arrived
	Subjects, Actors []string
	Class            string
}

var notifFilters = []notifFilter{
	{Key: "all", Label: "All"},
	{Key: "issues", Label: "Issues", Subjects: []string{"issue"}, Class: "nf-issue"},
	{Key: "pulls", Label: "Pull requests", Subjects: []string{"pull"}, Class: "nf-pull"},
	{Key: "agents", Label: "Agents & bots", Actors: []string{ActorToken, "bot", "agent"}, Class: "nf-agent"},
	{Key: "system", Label: "System", Subjects: []string{"backup", "import"}, Class: "nf-system"},
	{Key: "actions", Label: "Actions", Subjects: []string{"action"}, Soon: "Actions notifications arrive with Phase 3.", Class: "nf-action"},
	{Key: "runners", Label: "Runners", Subjects: []string{"runner"}, Soon: "Runner notifications arrive with Phase 3.", Class: "nf-runner"},
	{Key: "releases", Label: "Releases", Subjects: []string{"release"}, Soon: "Release notifications arrive with Phase 3.", Class: "nf-release"},
	{Key: "tether", Label: "Tether", Subjects: []string{"tether"}, Soon: "Tether notifications arrive with Phase 4.", Class: "nf-tether"},
}

func findNotifFilter(key string) notifFilter {
	for _, f := range notifFilters {
		if f.Key == key {
			return f
		}
	}
	return notifFilters[0]
}

func (f notifFilter) matches(g store.NotificationGroup) bool {
	return (len(f.Subjects) == 0 || notifHas(f.Subjects, g.SubjectType)) &&
		(len(f.Actors) == 0 || notifHas(f.Actors, g.ActorKind))
}

func notifHas(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// notifState is the page's current view, from the query string or form.
type notifState struct {
	Filter string
	RepoID int64
	Unread bool
	Done   bool
	Page   int
}

func readNotifState(v url.Values) notifState {
	s := notifState{Filter: findNotifFilter(v.Get("filter")).Key, Unread: v.Get("unread") == "1",
		Done: v.Get("view") == "done", Page: 1}
	s.RepoID, _ = strconv.ParseInt(v.Get("repo"), 10, 64)
	if p, err := strconv.Atoi(v.Get("page")); err == nil && p > 1 {
		s.Page = p
	}
	return s
}

// URL returns the page link for this state with changes applied.
func (s notifState) URL() string {
	v := url.Values{}
	if s.Filter != "all" {
		v.Set("filter", s.Filter)
	}
	if s.RepoID != 0 {
		v.Set("repo", strconv.FormatInt(s.RepoID, 10))
	}
	if s.Unread {
		v.Set("unread", "1")
	}
	if s.Done {
		v.Set("view", "done")
	}
	if s.Page > 1 {
		v.Set("page", strconv.Itoa(s.Page))
	}
	if len(v) == 0 {
		return "/notifications"
	}
	return "/notifications?" + v.Encode()
}

func (s notifState) with(f func(*notifState)) string {
	f(&s)
	return s.URL()
}

// readableRepoIDs lists repositories who may read (notifications about other
// repositories stay hidden even if they exist, e.g. after access changed).
func (a *App) readableRepoIDs(ctx context.Context, who Actor) ([]int64, error) {
	repos, err := a.db.Repos(ctx)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, r := range repos {
		if a.can(ctx, who, r, PermRead) {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

func (a *App) notifQuery(ctx context.Context, who Actor, s notifState) (store.NotificationQuery, error) {
	readable, err := a.readableRepoIDs(ctx, who)
	if err != nil {
		return store.NotificationQuery{}, err
	}
	f := findNotifFilter(s.Filter)
	return store.NotificationQuery{PersonID: who.PersonID, Done: s.Done, UnreadOnly: s.Unread,
		SubjectTypes: f.Subjects, ActorKinds: f.Actors, RepoID: s.RepoID, ReadableRepos: readable}, nil
}

type notifFilterView struct {
	notifFilter
	URL    string
	Active bool
	Count  int
}

type notifRepoView struct {
	ID     int64
	Name   string
	URL    string
	Active bool
	Count  int
}

type notifRowView struct {
	store.Notification
	Class, Icon, What, ReasonText, ActorText string
	ActorClass, Where                        string
}

func (a *App) notificationsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	s := readNotifState(r.URL.Query())
	q, err := a.notifQuery(ctx, who, s)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	q.Limit, q.Offset = notificationsPerPage+1, (s.Page-1)*notificationsPerPage
	list, err := a.db.Notifications(ctx, q)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	more := len(list) > notificationsPerPage
	if more {
		list = list[:notificationsPerPage]
	}
	groups, err := a.db.NotificationGroups(ctx, q)
	if err != nil {
		a.fail(w, r, err)
		return
	}

	// Sidebar counts: unread in the inbox, everything in the done list.
	count := func(g store.NotificationGroup) int {
		if s.Done {
			return g.Count
		}
		return g.Unread
	}
	var filters []notifFilterView
	for _, f := range notifFilters {
		fv := notifFilterView{notifFilter: f, Active: f.Key == s.Filter,
			URL: s.with(func(x *notifState) { x.Filter, x.Page = f.Key, 1 })}
		for _, g := range groups {
			if f.matches(g) {
				fv.Count += count(g)
			}
		}
		filters = append(filters, fv)
	}
	byRepo := map[int64]*notifRepoView{}
	for _, g := range groups {
		if g.RepoID == 0 {
			continue
		}
		rv := byRepo[g.RepoID]
		if rv == nil {
			id := g.RepoID
			rv = &notifRepoView{ID: id, Name: g.Repo, Active: id == s.RepoID,
				URL: s.with(func(x *notifState) { x.RepoID, x.Page = id, 1 })}
			byRepo[id] = rv
		}
		rv.Count += count(g)
	}
	var repos []notifRepoView
	for _, rv := range byRepo {
		repos = append(repos, *rv)
	}
	sort.Slice(repos, func(i, j int) bool { return strings.ToLower(repos[i].Name) < strings.ToLower(repos[j].Name) })
	repoName := ""
	for _, rv := range repos {
		if rv.Active {
			repoName = rv.Name
		}
	}

	rows := make([]notifRowView, 0, len(list))
	for _, n := range list {
		rows = append(rows, notifRow(n))
	}
	active := findNotifFilter(s.Filter)
	data := map[string]any{
		"State": s, "Filters": filters, "Repos": repos, "RepoName": repoName, "Rows": rows,
		"Active": active, "More": more,
		"InboxURL":    s.with(func(x *notifState) { x.Done, x.Page = false, 1 }),
		"DoneURL":     s.with(func(x *notifState) { x.Done, x.Unread, x.Page = true, false, 1 }),
		"AllURL":      s.with(func(x *notifState) { x.Unread, x.Page = false, 1 }),
		"UnreadURL":   s.with(func(x *notifState) { x.Unread, x.Page = true, 1 }),
		"ClearRepo":   s.with(func(x *notifState) { x.RepoID, x.Page = 0, 1 }),
		"PrevURL":     s.with(func(x *notifState) { x.Page-- }),
		"NextURL":     s.with(func(x *notifState) { x.Page++ }),
		"Back":        s.URL(),
		"Filtered":    s.Filter != "all" || s.RepoID != 0 || s.Unread,
		"ClearFilter": (notifState{Done: s.Done}).URL(),
	}
	errMsg := ""
	if r.URL.Query().Get("msg") == "missing" {
		errMsg = "That notification no longer exists."
	}
	a.render(w, r, "notifications_inbox", page{Title: "Notifications", Nav: "notifications", Error: errMsg, Data: data})
}

// notifRow works out how a row looks: its colour (left border), icon and words.
func notifRow(n store.Notification) notifRowView {
	v := notifRowView{Notification: n, ActorText: n.ActorName}
	_, verb, _ := strings.Cut(n.Kind, ".")
	switch n.SubjectType {
	case "issue":
		v.Class, v.Icon = "nf-issue", "issue"
	case "pull":
		v.Class, v.Icon = "nf-pull", "pull"
	case "backup", "import":
		v.Class, v.Icon = "nf-system", n.SubjectType
	case "action":
		v.Class, v.Icon = "nf-action", "action"
	case "runner":
		v.Class, v.Icon = "nf-runner", "runner"
	case "tether":
		v.Class, v.Icon = "nf-tether", "tether"
	case "release":
		v.Class, v.Icon = "nf-release", "release"
	default:
		v.Class, v.Icon = "nf-system", "dot"
	}
	switch n.ActorKind {
	case ActorToken:
		v.Class, v.ActorClass, v.ActorText = "nf-agent", "nf-actor-agent", n.ActorName+" (token)"
	case "bot", "agent":
		v.Class, v.ActorClass = "nf-agent", "nf-actor-agent"
	case ActorSystem:
		v.ActorClass, v.ActorText = "nf-actor-system", "Atelier"
	}
	if verb == "failed" {
		v.Class = "nf-failed"
	}
	switch {
	case n.Repo != "" && n.Number != 0 && (n.SubjectType == "issue" || n.SubjectType == "pull"):
		v.Where = fmt.Sprintf("%s · #%d", n.Repo, n.Number)
	case n.Repo != "":
		v.Where = n.Repo
	case n.SubjectType == "backup":
		v.Where = "Backups"
	case n.SubjectType == "import":
		v.Where = "Import"
	default:
		v.Where = strings.ToUpper(n.SubjectType[:1]) + n.SubjectType[1:]
	}
	v.What = notifVerb(n.Kind)
	switch n.Reason {
	case ReasonSubscribed:
		v.ReasonText = "Repository activity"
	case ReasonSystem:
		v.ReasonText = "System"
	default:
		v.ReasonText = strings.ReplaceAll(n.Reason, "_", " ")
	}
	return v
}

func notifVerb(kind string) string {
	switch kind {
	case EventIssueOpened, EventPullOpened:
		return "opened"
	case EventIssueCommented, EventPullCommented:
		return "commented"
	case EventIssueClosed, EventPullClosed:
		return "closed"
	case EventIssueReopened, EventPullReopened:
		return "reopened"
	case EventPullNote:
		return "added a note"
	case EventPullMerged:
		return "merged"
	case EventBackupSucceeded, EventImportDone:
		return "finished"
	case EventBackupFailed, EventImportFailed:
		return "failed"
	}
	_, verb, _ := strings.Cut(kind, ".")
	return strings.ReplaceAll(verb, "_", " ")
}

// notificationsCount feeds the top-bar bell (static/notifications_bell.js).
func (a *App) notificationsCount(w http.ResponseWriter, r *http.Request) {
	n, err := a.db.UnreadNotificationCount(r.Context(), a.actor(r).PersonID)
	if err != nil {
		logErr(r, err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"unread": n, "label": bellLabel(n)})
}

// bellLabel is the badge text: "" for none, "99+" above 99.
func bellLabel(n int) string {
	switch {
	case n <= 0:
		return ""
	case n > 99:
		return "99+"
	}
	return strconv.Itoa(n)
}

// notificationOpen marks a notification read and goes to its subject.
func (a *App) notificationOpen(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	n, err := a.db.Notification(ctx, who.PersonID, id)
	if err != nil {
		http.Redirect(w, r, "/notifications?msg=missing", http.StatusSeeOther)
		return
	}
	if n.RepoID != 0 {
		if rp, err := a.repoByID(ctx, n.RepoID); err != nil || !a.can(ctx, who, rp, PermRead) {
			a.notFound(w, r)
			return
		}
	}
	if err := a.db.MarkNotificationRead(ctx, who.PersonID, id); err != nil {
		logErr(r, err)
	}
	dest := safeLocalURL(n.URL)
	if dest == "" {
		dest = "/notifications"
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// back returns to the inbox view the form came from.
func notifBack(r *http.Request) string {
	if b := safeLocalURL(r.FormValue("back")); strings.HasPrefix(b, "/notifications") {
		return b
	}
	return "/notifications"
}

func (a *App) notificationDone(w http.ResponseWriter, r *http.Request) {
	who := a.actor(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.db.MarkNotificationDone(r.Context(), who.PersonID, id); err != nil && err != store.ErrNotFound {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, notifBack(r), http.StatusSeeOther)
}

// notificationsAllDone marks everything in the current view done (the filter,
// repository and unread choice come from the form).
func (a *App) notificationsAllDone(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	s := readNotifState(r.PostForm)
	s.Done, s.Page = false, 1
	q, err := a.notifQuery(ctx, who, s)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if _, err := a.db.MarkNotificationsDone(ctx, q); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, s.URL(), http.StatusSeeOther)
}
