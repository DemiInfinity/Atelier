package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"atelier/internal/store"
)

// Items: adding issues, pull requests and drafts, moving cards, editing
// field values, converting drafts and removing items.

// projItem loads /projects/{id}/items/{item}. When content is true the
// viewer must be able to read the item's repository too (anything that
// shows or changes the item itself); otherwise it's reported as not found.
func (a *App) projItem(w http.ResponseWriter, r *http.Request, p store.Project, content bool) (store.ProjItem, bool) {
	id, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return store.ProjItem{}, false
	}
	it, err := a.db.ProjectItem(r.Context(), p.ID, id)
	if err != nil {
		a.fail(w, r, err)
		return store.ProjItem{}, false
	}
	if content && !a.projItemVisible(r.Context(), a.actor(r), it) {
		a.notFound(w, r)
		return store.ProjItem{}, false
	}
	return it, true
}

// projRefRe matches "owner/repo#12" and issue / pull request paths or URLs.
var (
	projRefShort = regexp.MustCompile(`^([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)#(\d+)$`)
	projRefPath  = regexp.MustCompile(`(?:^|/)([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+)/(issues|pulls?)/(\d+)/?(?:[?#].*)?$`)
)

var errProjRef = errors.New("not an issue or pull request you can see")

// projResolveRef turns what someone typed into an issue or pull request the
// viewer can read.
func (a *App) projResolveRef(r *http.Request, ref string) (store.Repo, string, int64, error) {
	return a.projResolveRefAs(r.Context(), a.actor(r), ref)
}

// projResolveRefAs is projResolveRef for any actor (the MCP tools use it too).
func (a *App) projResolveRefAs(ctx context.Context, who Actor, ref string) (store.Repo, string, int64, error) {
	ref = strings.TrimSpace(ref)
	var owner, name, kind string
	var num int64
	if m := projRefShort.FindStringSubmatch(ref); m != nil {
		owner, name = m[1], m[2]
		num, _ = strconv.ParseInt(m[3], 10, 64)
	} else if m := projRefPath.FindStringSubmatch(ref); m != nil {
		owner, name = m[1], m[2]
		num, _ = strconv.ParseInt(m[4], 10, 64)
		kind = "issue"
		if strings.HasPrefix(m[3], "pull") {
			kind = "pull"
		}
	} else {
		return store.Repo{}, "", 0, errProjRef
	}
	rp, err := a.db.RepoByName(ctx, owner, name)
	if err != nil || !a.can(ctx, who, rp, PermRead) {
		return store.Repo{}, "", 0, errProjRef
	}
	return a.projResolveNumber(ctx, rp, kind, num)
}

// projResolveNumber works out whether #num is an issue or a pull request
// (kind may be given or "").
func (a *App) projResolveNumber(ctx context.Context, rp store.Repo, kind string, num int64) (store.Repo, string, int64, error) {
	if kind != "pull" {
		if _, err := a.db.IssueByNumber(ctx, rp.ID, num); err == nil {
			return rp, "issue", num, nil
		}
	}
	if kind != "issue" {
		if _, err := a.db.PullByNumber(ctx, rp.ID, num); err == nil {
			return rp, "pull", num, nil
		}
	}
	return store.Repo{}, "", 0, errProjRef
}

// projItemAdd adds an item. One of:
//
//	ref=owner/repo#12 (or an issue / pull request URL)
//	repo=<id>&number=<n>[&kind=issue|pull]   (from the sidebar panel)
//	title=…[&body=…]                         a draft
//
// status=<option id> optionally sets its Status. return= says where to go.
func (a *App) projItemAdd(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	back := projBack(r, projURL(p))
	var it store.ProjItem
	var err error
	switch {
	case strings.TrimSpace(r.FormValue("ref")) != "":
		rp, kind, num, rerr := a.projResolveRef(r, r.FormValue("ref"))
		if rerr != nil {
			a.projAddFailed(w, r, p, "Couldn’t find that issue or pull request. Use owner/repo#12 or paste its link.")
			return
		}
		it, _, err = a.db.AddItem(ctx, p.ID, kind, rp.ID, num, who.Kind, who.Name)
	case r.FormValue("repo") != "":
		repoID, _ := strconv.ParseInt(r.FormValue("repo"), 10, 64)
		num, _ := strconv.ParseInt(r.FormValue("number"), 10, 64)
		rp, rerr := a.db.RepoByID(ctx, repoID)
		if rerr != nil || !a.can(ctx, who, rp, PermRead) {
			a.notFound(w, r)
			return
		}
		kind := r.FormValue("kind")
		if kind != "issue" && kind != "pull" {
			kind = ""
		}
		var rerr2 error
		if rp, kind, num, rerr2 = a.projResolveNumber(ctx, rp, kind, num); rerr2 != nil {
			a.notFound(w, r)
			return
		}
		it, _, err = a.db.AddItem(ctx, p.ID, kind, rp.ID, num, who.Kind, who.Name)
	default:
		title := strings.TrimSpace(r.FormValue("title"))
		if !store.ValidProjTitle(title, 256) {
			a.projAddFailed(w, r, p, "Give the draft a title (up to 256 characters).")
			return
		}
		it, err = a.db.AddDraft(ctx, p.ID, title, r.FormValue("body"), who.Kind, who.Name)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if st := r.FormValue("status"); st != "" {
		if sf, err := a.db.StatusField(ctx, p.ID); err == nil {
			if err := a.db.SetItemValue(ctx, p.ID, it.ID, sf.ID, st); err != nil && !errors.Is(err, store.ErrProjInvalid) {
				a.fail(w, r, err)
				return
			}
		}
	}
	if projWantsJSON(r) {
		projJSON(w, http.StatusOK, fmt.Sprintf(`{"ok":true,"item":%d}`, it.ID))
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (a *App) projAddFailed(w http.ResponseWriter, r *http.Request, p store.Project, msg string) {
	if projWantsJSON(r) {
		projJSON(w, http.StatusUnprocessableEntity, `{"ok":false}`)
		return
	}
	w.WriteHeader(http.StatusUnprocessableEntity)
	a.render(w, r, "error", page{Title: "Couldn’t add item", Nav: "projects", Data: map[string]string{
		"Heading": "Couldn’t add that item", "Message": msg,
	}})
}

// projWantsJSON: the board's script asks for a small JSON reply.
func projWantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func projJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// projItemMove moves a card: before=<item id> or after=<item id> (0 or
// neither = the end), and optionally status=<option id> ("" = No status;
// only when the form has a status field at all).
func (a *App) projItemMove(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermWrite)
	if !ok {
		return
	}
	it, ok := a.projItem(w, r, p, true)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	before, _ := strconv.ParseInt(r.PostForm.Get("before"), 10, 64)
	if after, _ := strconv.ParseInt(r.PostForm.Get("after"), 10, 64); after != 0 && before == 0 {
		items, err := a.db.ProjectItems(ctx, p.ID)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		before = projBeforeFromAfter(items, it.ID, after)
	}
	var statusField int64
	status := ""
	if _, has := r.PostForm["status"]; has {
		sf, err := a.db.StatusField(ctx, p.ID)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		statusField, status = sf.ID, r.PostForm.Get("status")
	}
	err := a.db.MoveItem(ctx, p.ID, it.ID, before, statusField, status)
	if errors.Is(err, store.ErrProjInvalid) {
		http.Error(w, "unknown status", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if projWantsJSON(r) {
		projJSON(w, http.StatusOK, `{"ok":true}`)
		return
	}
	http.Redirect(w, r, projBack(r, projURL(p))+"#item-"+strconv.FormatInt(it.ID, 10), http.StatusSeeOther)
}

// projBeforeFromAfter turns "put itemID after after" into the item it goes
// before (0 = the end), for MoveItem.
func projBeforeFromAfter(items []store.ProjItem, itemID, after int64) int64 {
	var before int64
	for k, x := range items {
		if x.ID == after && k+1 < len(items) {
			before = items[k+1].ID
			if before == itemID && k+2 < len(items) {
				before = items[k+2].ID
			} else if before == itemID {
				before = 0
			}
		}
	}
	return before
}

// projItemField sets one field: field=<id>&value=….
func (a *App) projItemField(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermWrite)
	if !ok {
		return
	}
	it, ok := a.projItem(w, r, p, true)
	if !ok {
		return
	}
	fid, _ := strconv.ParseInt(r.FormValue("field"), 10, 64)
	err := a.db.SetItemValue(r.Context(), p.ID, it.ID, fid, r.FormValue("value"))
	switch {
	case errors.Is(err, store.ErrProjInvalid):
		if projWantsJSON(r) {
			projJSON(w, http.StatusUnprocessableEntity, `{"ok":false}`)
			return
		}
		http.Error(w, "That value doesn’t fit the field (numbers, dates as YYYY-MM-DD, or one of the options).", http.StatusUnprocessableEntity)
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	if projWantsJSON(r) {
		projJSON(w, http.StatusOK, `{"ok":true}`)
		return
	}
	http.Redirect(w, r, projBack(r, projURL(p)), http.StatusSeeOther)
}

// ---------- the item page ----------

type projItemData struct {
	BodyHTML template.HTML // a draft's body
	P        store.Project
	CanAdmin bool
	URL      string
	Item     store.ProjItem
	Card     projCard
	CanWrite bool
	Repos    []store.Repo // where a draft can become an issue
	Back     string
}

func (a *App) projItemPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermRead)
	if !ok {
		return
	}
	it, ok := a.projItem(w, r, p, true)
	if !ok {
		return
	}
	a.renderProjItem(w, r, p, it, "")
}

func (a *App) renderProjItem(w http.ResponseWriter, r *http.Request, p store.Project, it store.ProjItem, errMsg string) {
	ctx := r.Context()
	who := a.actor(r)
	fields, err := a.db.ProjectFields(ctx, p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d := projItemData{P: p, URL: projURL(p), Item: it, Card: projCardFor(p, it, true, fields),
		CanWrite: a.canProject(ctx, who, p, PermWrite), CanAdmin: a.canProject(ctx, who, p, PermAdmin), Back: projURL(p)}
	if it.IsDraft() && strings.TrimSpace(it.Body) != "" {
		d.BodyHTML = renderMarkdown([]byte(it.Body))
	}
	if it.IsDraft() && d.CanWrite {
		repos, err := a.db.Repos(ctx)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		for _, rp := range repos {
			if a.can(ctx, who, rp, PermWrite) {
				d.Repos = append(d.Repos, rp)
			}
		}
	}
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	a.render(w, r, "projects_item", page{Title: d.Card.Title + " · " + p.Title, Nav: "projects", Error: errMsg, Data: d})
}

func (a *App) projItemDraft(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermWrite)
	if !ok {
		return
	}
	it, ok := a.projItem(w, r, p, true)
	if !ok {
		return
	}
	if !it.IsDraft() {
		a.notFound(w, r)
		return
	}
	err := a.db.UpdateDraft(r.Context(), p.ID, it.ID, r.FormValue("title"), r.FormValue("body"))
	if errors.Is(err, store.ErrProjInvalid) {
		a.renderProjItem(w, r, p, it, "Give the draft a title (up to 256 characters).")
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, projURL(p)+"/items/"+strconv.FormatInt(it.ID, 10), http.StatusSeeOther)
}

// projItemConvert turns a draft into an issue in a repository the person
// can write to, keeping its place and field values.
func (a *App) projItemConvert(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermWrite)
	if !ok {
		return
	}
	it, ok := a.projItem(w, r, p, true)
	if !ok {
		return
	}
	if !it.IsDraft() {
		a.notFound(w, r)
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	repoID, _ := strconv.ParseInt(r.FormValue("repo"), 10, 64)
	rp, err := a.db.RepoByID(ctx, repoID)
	if err != nil || !a.can(ctx, who, rp, PermWrite) {
		a.renderProjItem(w, r, p, it, "Pick a repository you can open issues in.")
		return
	}
	is, err := a.db.CreateIssue(ctx, rp.ID, it.Title, it.Body, who.Kind, who.Name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	// Convert first, so automation that reacts to issue.opened finds the
	// item already here instead of adding a second one.
	if err := a.db.ConvertDraft(ctx, p.ID, it.ID, rp.ID, is.Number); err != nil {
		a.fail(w, r, err)
		return
	}
	issueURL := "/" + rp.FullName() + "/issues/" + strconv.FormatInt(is.Number, 10)
	a.emit(ctx, Event{Kind: EventIssueOpened, Repo: rp, RepoID: rp.ID, Number: is.Number, Title: is.Title,
		URL: issueURL, Actor: who, Detail: eventExcerpt(is.Body, 140)})
	http.Redirect(w, r, projBack(r, projURL(p)), http.StatusSeeOther)
}

func (a *App) projItemRemove(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermWrite)
	if !ok {
		return
	}
	// Removing doesn't show anything, but keep it to items you can see so a
	// card you can't read isn't silently dropped by mistake.
	it, ok := a.projItem(w, r, p, true)
	if !ok {
		return
	}
	if err := a.db.RemoveItem(r.Context(), p.ID, it.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	back := projBack(r, projURL(p))
	if strings.HasPrefix(back, projURL(p)+"/items/") {
		back = projURL(p)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}
