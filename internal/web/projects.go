package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"atelier/internal/store"
)

// Projects: GitHub-Projects-style planning boards (/projects).
//
//	projects.go         feature, permissions, project list, new, settings, fields, links
//	projects_view.go    the project page (board, table, roadmap) and saved views
//	projects_items.go   adding, moving, editing, converting and removing items
//	projects_filter.go  the filter language (status:"In progress" repo:x is:open …)
//	projects_panel.go   the sidebar panel on issues and pull requests, and automation
//
// A project belongs to an account. Who may see or change it is decided by
// a.can against a stand-in repository for the account with the project's
// visibility (see projRepo), so the Duo / Team / Company addons extend
// projects through the same access policies as repositories. Each issue or
// pull request on a project is shown only to people who can read its
// repository: everyone else sees "Private item".

func init() {
	registerFeature(feature{
		name: "projects",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /projects", a.authed(a.projList))
			m.HandleFunc("GET /projects/new", a.authed(a.projNewPage))
			m.HandleFunc("POST /projects/new", a.authed(a.projCreate))
			m.HandleFunc("GET /projects/panel", a.authed(a.projPanel))
			m.HandleFunc("POST /projects/panel", a.authed(a.projPanelAdd))
			m.HandleFunc("GET /projects/{id}", a.authed(a.projView))
			m.HandleFunc("GET /projects/{id}/settings", a.authed(a.projSettingsPage))
			m.HandleFunc("POST /projects/{id}/settings", a.authed(a.projSettingsSave))
			m.HandleFunc("POST /projects/{id}/delete", a.authed(a.projDelete))
			m.HandleFunc("POST /projects/{id}/links", a.authed(a.projLinks))
			m.HandleFunc("POST /projects/{id}/fields", a.authed(a.projFieldCreate))
			m.HandleFunc("POST /projects/{id}/fields/{field}/edit", a.authed(a.projFieldEdit))
			m.HandleFunc("POST /projects/{id}/fields/{field}/delete", a.authed(a.projFieldDelete))
			m.HandleFunc("POST /projects/{id}/views", a.authed(a.projViewSave))
			m.HandleFunc("POST /projects/{id}/views/{view}/delete", a.authed(a.projViewDelete))
			m.HandleFunc("POST /projects/{id}/items", a.authed(a.projItemAdd))
			m.HandleFunc("GET /projects/{id}/items/{item}", a.authed(a.projItemPage))
			m.HandleFunc("POST /projects/{id}/items/{item}/move", a.authed(a.projItemMove))
			m.HandleFunc("POST /projects/{id}/items/{item}/field", a.authed(a.projItemField))
			m.HandleFunc("POST /projects/{id}/items/{item}/draft", a.authed(a.projItemDraft))
			m.HandleFunc("POST /projects/{id}/items/{item}/convert", a.authed(a.projItemConvert))
			m.HandleFunc("POST /projects/{id}/items/{item}/remove", a.authed(a.projItemRemove))
		},
	})
}

// ---------- permissions ----------

// projRepo is the stand-in repository a.can judges a project by: the
// project's account, with the project's visibility.
func projRepo(p store.Project) store.Repo {
	return store.Repo{OwnerID: p.OwnerID, Owner: p.Owner, OwnerKind: p.OwnerKind, Name: "", Private: p.Private}
}

// canProject: PermRead to see a project, PermWrite to change its items and
// field values, PermAdmin for settings, fields, links and deletion. Projects
// are managed in the browser only: tokens get nothing (there's no scope).
func (a *App) canProject(ctx context.Context, who Actor, p store.Project, need Perm) bool {
	if who.Kind == ActorToken || who.Kind == ActorAnonymous {
		return false
	}
	return a.can(ctx, who, projRepo(p), need)
}

// canCreateProject reports whether who may make projects for account.
func (a *App) canCreateProject(ctx context.Context, who Actor, account store.Owner) bool {
	p := store.Project{OwnerID: account.ID, Owner: account.Name, OwnerKind: account.Kind, Private: true}
	return a.canProject(ctx, who, p, PermAdmin)
}

// projItemVisible reports whether who may see an item's title, repository,
// labels and state. Drafts belong to the project, so its readers see them.
func (a *App) projItemVisible(ctx context.Context, who Actor, it store.ProjItem) bool {
	if it.IsDraft() {
		return true
	}
	return a.can(ctx, who, it.Repo(), PermRead)
}

// loadProject finds /projects/{id} and checks need. Projects the viewer
// can't read are reported as not found.
func (a *App) loadProject(w http.ResponseWriter, r *http.Request, need Perm) (store.Project, bool) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return store.Project{}, false
	}
	p, err := a.db.ProjectByID(ctx, id)
	if err != nil {
		a.fail(w, r, err)
		return store.Project{}, false
	}
	who := a.actor(r)
	if !a.canProject(ctx, who, p, PermRead) {
		a.notFound(w, r)
		return store.Project{}, false
	}
	if need > PermRead && !a.canProject(ctx, who, p, need) {
		a.forbidden(w, r)
		return store.Project{}, false
	}
	return p, true
}

func projURL(p store.Project) string { return "/projects/" + strconv.FormatInt(p.ID, 10) }

// projBack is where a form goes afterwards: its local "return" value, or def.
func projBack(r *http.Request, def string) string {
	if ret := r.FormValue("return"); ret != "" {
		if s := safeNext(ret); s != "/" || ret == "/" {
			return s
		}
	}
	return def
}

// ---------- the project list ----------

type projListRow struct {
	P        store.Project
	URL      string
	CanAdmin bool
}

type projListData struct {
	Account   *store.Owner // set on an account's Projects tab
	Rows      []projListRow
	Closed    []projListRow
	Query     string
	State     string // open (default) or closed
	CanCreate bool
	NewURL    string
	Accounts  []store.Owner
}

// projList is /projects: every project you can see, or one account's
// (?account=name, the Projects tab of a profile).
func (a *App) projList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	q := r.URL.Query()
	d := projListData{Query: strings.TrimSpace(q.Get("q")), State: q.Get("state"), NewURL: "/projects/new"}
	if d.State != "closed" {
		d.State = "open"
	}
	var all []store.Project
	var err error
	if name := q.Get("account"); name != "" {
		acct, err := a.db.OwnerByName(ctx, name)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		d.Account = &acct
		d.CanCreate = a.canCreateProject(ctx, who, acct)
		d.NewURL = "/projects/new?account=" + url.QueryEscape(acct.Name)
		all, err = a.db.ProjectsByOwner(ctx, acct.ID)
		if err != nil {
			a.fail(w, r, err)
			return
		}
	} else {
		if all, err = a.db.Projects(ctx); err != nil {
			a.fail(w, r, err)
			return
		}
		owners, err := a.db.Owners(ctx)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		for _, o := range owners {
			if a.canCreateProject(ctx, who, o) {
				d.CanCreate = true
				break
			}
		}
	}
	for _, p := range all {
		if !a.canProject(ctx, who, p, PermRead) {
			continue
		}
		if d.Query != "" && !strings.Contains(strings.ToLower(p.Title+" "+p.Description), strings.ToLower(d.Query)) {
			continue
		}
		row := projListRow{P: p, URL: projURL(p), CanAdmin: a.canProject(ctx, who, p, PermAdmin)}
		if p.Closed {
			d.Closed = append(d.Closed, row)
		} else {
			d.Rows = append(d.Rows, row)
		}
	}
	title := "Projects"
	nav := "projects"
	if d.Account != nil {
		title = "Projects · " + d.Account.Label()
		nav = "repos"
	}
	a.render(w, r, "projects_list", page{Title: title, Nav: nav, Data: d})
}

// ---------- new project ----------

type projNewData struct {
	Accounts    []store.Owner
	Account     string
	Title       string
	Description string
	Private     bool
}

func (a *App) projCreatable(ctx context.Context, who Actor) ([]store.Owner, error) {
	owners, err := a.db.Owners(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.Owner
	for _, o := range owners {
		if a.canCreateProject(ctx, who, o) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (a *App) projNewPage(w http.ResponseWriter, r *http.Request) {
	accts, err := a.projCreatable(r.Context(), a.actor(r))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if len(accts) == 0 {
		a.forbidden(w, r)
		return
	}
	a.render(w, r, "projects_new", page{Title: "New project", Nav: "projects", Data: projNewData{
		Accounts: accts, Account: r.URL.Query().Get("account"), Private: true,
	}})
}

func (a *App) projCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	accts, err := a.projCreatable(ctx, who)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d := projNewData{Accounts: accts, Account: r.FormValue("account"), Title: strings.TrimSpace(r.FormValue("title")),
		Description: strings.TrimSpace(r.FormValue("description")), Private: r.FormValue("visibility") != "public"}
	var acct *store.Owner
	for i := range accts {
		if strings.EqualFold(accts[i].Name, d.Account) {
			acct = &accts[i]
		}
	}
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		a.render(w, r, "projects_new", page{Title: "New project", Nav: "projects", Error: msg, Data: d})
	}
	switch {
	case len(accts) == 0:
		a.forbidden(w, r)
		return
	case acct == nil:
		fail(http.StatusUnprocessableEntity, "Pick an account you can create projects in.")
		return
	case !store.ValidProjTitle(d.Title, 120):
		fail(http.StatusUnprocessableEntity, "Give the project a name (up to 120 characters).")
		return
	case len(d.Description) > 1000:
		fail(http.StatusUnprocessableEntity, "Keep the description under 1000 characters.")
		return
	}
	p, err := a.db.CreateProject(ctx, store.NewProject{OwnerID: acct.ID, Title: d.Title, Description: d.Description,
		Private: d.Private, ActorKind: who.Kind, ActorName: who.Name, PersonID: who.PersonID})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, projURL(p), http.StatusSeeOther)
}

// ---------- settings ----------

type projLinkRow struct {
	Repo   store.Repo
	Linked bool
}

type projSettingsData struct {
	P        store.Project
	CanAdmin bool
	URL      string
	Fields   []store.ProjField
	Kinds    []struct{ Value, Label string }
	Colors   []string
	Links    []projLinkRow
	Section  string
}

func (a *App) projSettingsPage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermAdmin)
	if !ok {
		return
	}
	a.renderProjSettings(w, r, p, "", r.URL.Query().Get("saved"))
}

func (a *App) renderProjSettings(w http.ResponseWriter, r *http.Request, p store.Project, errMsg, notice string) {
	ctx := r.Context()
	who := a.actor(r)
	fields, err := a.db.ProjectFields(ctx, p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	links, err := a.db.ProjectLinks(ctx, p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	linked := map[int64]bool{}
	for _, id := range links {
		linked[id] = true
	}
	repos, err := a.db.Repos(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d := projSettingsData{P: p, CanAdmin: true, URL: projURL(p), Fields: fields, Colors: store.ProjColors}
	for _, k := range store.ProjFieldKinds {
		d.Kinds = append(d.Kinds, struct{ Value, Label string }{k, store.ProjKindLabel(k)})
	}
	for _, rp := range repos {
		if !a.can(ctx, who, rp, PermRead) {
			continue
		}
		d.Links = append(d.Links, projLinkRow{Repo: rp, Linked: linked[rp.ID]})
	}
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	a.render(w, r, "projects_settings", page{Title: "Settings · " + p.Title, Nav: "projects", Error: errMsg, Notice: notice, Data: d})
}

func (a *App) projSettingsSave(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermAdmin)
	if !ok {
		return
	}
	p.Title = strings.TrimSpace(r.FormValue("title"))
	p.Description = strings.TrimSpace(r.FormValue("description"))
	if !store.ValidProjTitle(p.Title, 120) || len(p.Description) > 1000 {
		a.renderProjSettings(w, r, p, "Give the project a name (up to 120 characters) and keep the description under 1000 characters.", "")
		return
	}
	p.Private = r.FormValue("visibility") != "public"
	p.Closed = r.FormValue("closed") == "1"
	p.AutoAdd = r.FormValue("auto_add") == "1"
	p.AutoDone = r.FormValue("auto_done") == "1"
	p.AutoReopen = r.FormValue("auto_reopen") == "1"
	if err := a.db.UpdateProject(r.Context(), p); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, projURL(p)+"/settings?saved=1", http.StatusSeeOther)
}

func (a *App) projDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermAdmin)
	if !ok {
		return
	}
	if r.FormValue("confirm") != p.Title {
		a.renderProjSettings(w, r, p, "Type the project's name to delete it.", "")
		return
	}
	if err := a.db.DeleteProject(r.Context(), p.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/projects?account="+url.QueryEscape(p.Owner), http.StatusSeeOther)
}

// projLinks saves which repositories automation watches. Only repositories
// the person can read can be linked.
func (a *App) projLinks(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermAdmin)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	want := map[int64]bool{}
	for _, v := range r.PostForm["repo"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			want[id] = true
		}
	}
	repos, err := a.db.Repos(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for _, rp := range repos {
		if !a.can(ctx, who, rp, PermRead) {
			continue // never touch links you can't see
		}
		if want[rp.ID] {
			err = a.db.LinkProjectRepo(ctx, p.ID, rp.ID)
		} else {
			err = a.db.UnlinkProjectRepo(ctx, p.ID, rp.ID)
		}
		if err != nil {
			a.fail(w, r, err)
			return
		}
	}
	http.Redirect(w, r, projURL(p)+"/settings?saved=1#automation", http.StatusSeeOther)
}

// ---------- fields ----------

func (a *App) projFieldCreate(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermAdmin)
	if !ok {
		return
	}
	var opts []string
	for _, line := range strings.Split(r.FormValue("options"), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			opts = append(opts, s)
		}
	}
	_, err := a.db.CreateField(r.Context(), p.ID, r.FormValue("name"), r.FormValue("kind"), opts)
	if errors.Is(err, store.ErrProjInvalid) {
		a.renderProjSettings(w, r, p, "Use a new field name (up to 60 characters, no quotes or colons) that isn't a built-in column such as Title, Repository or Labels.", "")
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, projURL(p)+"/settings?saved=1#fields", http.StatusSeeOther)
}

func (a *App) projField(w http.ResponseWriter, r *http.Request, p store.Project) (store.ProjField, bool) {
	id, err := strconv.ParseInt(r.PathValue("field"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return store.ProjField{}, false
	}
	f, err := a.db.ProjectField(r.Context(), p.ID, id)
	if err != nil {
		a.fail(w, r, err)
		return store.ProjField{}, false
	}
	return f, true
}

// projFieldEdit renames a field and edits a single select's options:
// opt_<id> (name), color_<id>, del_<id>, and new / new_color for one more.
func (a *App) projFieldEdit(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermAdmin)
	if !ok {
		return
	}
	f, ok := a.projField(w, r, p)
	if !ok {
		return
	}
	ctx := r.Context()
	if !f.IsStatus() {
		if name := strings.TrimSpace(r.FormValue("name")); name != "" && name != f.Name {
			if err := a.db.RenameField(ctx, p.ID, f.ID, name); err != nil {
				if errors.Is(err, store.ErrProjInvalid) {
					a.renderProjSettings(w, r, p, "That field name is taken or not allowed.", "")
					return
				}
				a.fail(w, r, err)
				return
			}
		}
	}
	if f.IsSelect() {
		var edits []store.OptionEdit
		for _, o := range f.Options {
			k := strconv.FormatInt(o.ID, 10)
			name := r.FormValue("opt_" + k)
			if name == "" {
				name = o.Name
			}
			color := r.FormValue("color_" + k)
			if color == "" {
				color = o.Color
			}
			edits = append(edits, store.OptionEdit{ID: o.ID, Name: name, Color: color, Delete: r.FormValue("del_"+k) == "1"})
		}
		if n := strings.TrimSpace(r.FormValue("new")); n != "" {
			edits = append(edits, store.OptionEdit{Name: n, Color: r.FormValue("new_color")})
		}
		if err := a.db.EditOptions(ctx, p.ID, f.ID, edits); err != nil {
			if errors.Is(err, store.ErrProjInvalid) {
				a.renderProjSettings(w, r, p, "Each option needs a different name (up to 60 characters).", "")
				return
			}
			a.fail(w, r, err)
			return
		}
	}
	http.Redirect(w, r, projURL(p)+"/settings?saved=1#field-"+strconv.FormatInt(f.ID, 10), http.StatusSeeOther)
}

func (a *App) projFieldDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermAdmin)
	if !ok {
		return
	}
	f, ok := a.projField(w, r, p)
	if !ok {
		return
	}
	if f.IsStatus() {
		a.renderProjSettings(w, r, p, "Status is built in and can't be deleted. Edit its options instead.", "")
		return
	}
	if err := a.db.DeleteField(r.Context(), p.ID, f.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, projURL(p)+"/settings?saved=1#fields", http.StatusSeeOther)
}
