package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"atelier/internal/store"
)

// Webhook settings: per repository (/{owner}/{repo}/settings/webhooks) and
// per account (/settings/webhooks?account=…, firing for all its
// repositories). The same templates serve both.

func appsHookRoutes(a *App, m *http.ServeMux) {
	// Account hook pages sit one segment deeper (/settings/webhooks/account/{id})
	// so they can't overlap /{owner}/{repo}/… routes.
	for _, b := range [][2]string{{"/{owner}/{repo}/settings/webhooks", "/{owner}/{repo}/settings/webhooks"}, {"/settings/webhooks", appsAccountHookBase}} {
		list, base := b[0], b[1]
		m.HandleFunc("GET "+list, a.authed(a.appsHooksPage))
		m.HandleFunc("POST "+list, a.authed(a.appsHookCreate))
		m.HandleFunc("GET "+base+"/{id}", a.authed(a.appsHookPage))
		m.HandleFunc("POST "+base+"/{id}", a.authed(a.appsHookUpdate))
		m.HandleFunc("POST "+base+"/{id}/delete", a.authed(a.appsHookDelete))
		m.HandleFunc("POST "+base+"/{id}/ping", a.authed(a.appsHookPing))
		m.HandleFunc("GET "+base+"/{id}/deliveries/{did}", a.authed(a.appsDeliveryPage))
		m.HandleFunc("POST "+base+"/{id}/deliveries/{did}/redeliver", a.authed(a.appsRedeliverPost))
	}
}

const appsAccountHookBase = "/settings/webhooks/account"

// appsHookScope is where a webhook page lives.
type appsHookScope struct {
	V        *repoView // repository hooks
	Owner    store.Owner
	Accounts []store.Owner // account hooks: accounts the viewer can switch between
	Base     string        // URL of the list
	ItemBase string        // hook pages live at ItemBase/{id}
	Query    string        // "?account=…" for account lists
	Title    string
}

func (s appsHookScope) IsRepo() bool { return s.V != nil }

// HookURL is a hook's page.
func (s appsHookScope) HookURL(id int64) string {
	if s.ItemBase != "" {
		return fmt.Sprintf("%s/%d", s.ItemBase, id)
	}
	return fmt.Sprintf("%s/%d", s.Base, id)
}

func (a *App) appsScope(w http.ResponseWriter, r *http.Request) (appsHookScope, bool) {
	ctx := r.Context()
	if r.PathValue("owner") != "" {
		v, _, ok := a.loadRepo(w, r, "settings")
		if !ok {
			return appsHookScope{}, false
		}
		return appsHookScope{V: v, Base: v.Base + "/settings/webhooks", Title: v.Repo.FullName()}, true
	}
	who := a.actor(r)
	var o store.Owner
	var err error
	if name := r.URL.Query().Get("account"); name != "" {
		o, err = a.db.OwnerByName(ctx, name)
	} else {
		o, err = a.db.Person(ctx, who.PersonID)
	}
	if err != nil {
		a.fail(w, r, err)
		return appsHookScope{}, false
	}
	if !a.canEditAccount(ctx, who, o) {
		a.forbidden(w, r)
		return appsHookScope{}, false
	}
	return a.appsAccountScope(ctx, who, o), true
}

func (a *App) appsAccountScope(ctx context.Context, who Actor, o store.Owner) appsHookScope {
	s := appsHookScope{Owner: o, Base: "/settings/webhooks", ItemBase: appsAccountHookBase, Query: "?account=" + url.QueryEscape(o.Name), Title: o.Label()}
	if owners, err := a.db.Owners(ctx); err == nil {
		for _, x := range owners {
			if a.canEditAccount(ctx, who, x) {
				s.Accounts = append(s.Accounts, x)
			}
		}
	}
	return s
}

// appsLoadHook loads a hook in its scope (404 when it's someone else's).
func (a *App) appsLoadHook(w http.ResponseWriter, r *http.Request) (appsHookScope, store.Webhook, bool) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return appsHookScope{}, store.Webhook{}, false
	}
	h, err := a.db.WebhookByID(ctx, id)
	if err != nil {
		a.notFound(w, r)
		return appsHookScope{}, store.Webhook{}, false
	}
	if r.PathValue("owner") != "" {
		v, _, ok := a.loadRepo(w, r, "settings")
		if !ok {
			return appsHookScope{}, h, false
		}
		if h.RepoID != v.Repo.ID {
			a.notFound(w, r)
			return appsHookScope{}, h, false
		}
		return appsHookScope{V: v, Base: v.Base + "/settings/webhooks", Title: v.Repo.FullName()}, h, true
	}
	who := a.actor(r)
	o, err := a.db.OwnerByID(ctx, h.OwnerID)
	if h.OwnerID == 0 || err != nil || !a.canEditAccount(ctx, who, o) {
		a.notFound(w, r)
		return appsHookScope{}, h, false
	}
	return a.appsAccountScope(ctx, who, o), h, true
}

type appsHookForm struct {
	URL          string
	Events       map[string]bool
	Active       bool
	AllowLocal   bool
	HasSecret    bool
	RemoveSecret bool
	secret       string
}

func appsHookFormFrom(r *http.Request) appsHookForm {
	_ = r.ParseForm()
	f := appsHookForm{URL: strings.TrimSpace(r.FormValue("url")), Events: map[string]bool{}, Active: r.FormValue("active") != "",
		AllowLocal: r.FormValue("allow_local") != "", RemoveSecret: r.FormValue("remove_secret") != "", secret: r.FormValue("secret")}
	for _, e := range r.Form["events"] {
		for _, k := range store.WebhookEvents {
			if e == k {
				f.Events[e] = true
			}
		}
	}
	return f
}

func appsFormOfHook(h store.Webhook) appsHookForm {
	f := appsHookForm{URL: h.URL, Events: map[string]bool{}, Active: h.Active, AllowLocal: h.AllowLocal, HasSecret: h.HasSecret}
	for _, e := range h.Events {
		f.Events[e] = true
	}
	return f
}

func (f appsHookForm) eventList() []string {
	var out []string
	for _, e := range store.WebhookEvents {
		if f.Events[e] && e != "ping" {
			out = append(out, e)
		}
	}
	return out
}

func (f appsHookForm) validate() string {
	if _, err := appsCheckURL(f.URL, f.AllowLocal); err != nil {
		return err.Error()
	}
	if len(f.eventList()) == 0 {
		return "Pick at least one event."
	}
	if len(f.secret) > 256 {
		return "Keep the secret under 256 characters."
	}
	return ""
}

type appsHooksData struct {
	Scope  appsHookScope
	Hooks  []store.Webhook
	Form   appsHookForm
	Events []appsEventInfo
}

type appsEventInfo struct{ Name, Label, Hint string }

var appsEventInfos = []appsEventInfo{
	{"push", "Pushes", "Branches and tags, including merges"},
	{"pull_request", "Pull requests", "Opened, synchronize, closed (merged or not), reopened"},
	{"issues", "Issues", "Opened, closed, reopened"},
	{"issue_comment", "Comments", "On issues and pull requests"},
	{"release", "Releases", "Published"},
	{"workflow_run", "Workflow runs", "Actions runs that completed"},
}

func (a *App) appsHooksPage(w http.ResponseWriter, r *http.Request) {
	s, ok := a.appsScope(w, r)
	if !ok {
		return
	}
	a.renderHooks(w, r, s, appsHookForm{Active: true, Events: map[string]bool{"push": true}}, "", http.StatusOK)
}

func (a *App) renderHooks(w http.ResponseWriter, r *http.Request, s appsHookScope, f appsHookForm, errMsg string, code int) {
	ctx := r.Context()
	d := &appsHooksData{Scope: s, Form: f, Events: appsEventInfos}
	if s.V != nil {
		d.Hooks, _ = a.db.RepoWebhooks(ctx, s.V.Repo.ID)
	} else {
		d.Hooks, _ = a.db.AccountWebhooks(ctx, s.Owner.ID)
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "apps_webhooks", page{Title: "Webhooks · " + s.Title, Nav: appsNav(s), Error: errMsg, Data: d})
}

func appsNav(s appsHookScope) string {
	if s.V != nil {
		return "repos"
	}
	return "tokens"
}

func (a *App) appsHookCreate(w http.ResponseWriter, r *http.Request) {
	s, ok := a.appsScope(w, r)
	if !ok {
		return
	}
	f := appsHookFormFrom(r)
	if msg := f.validate(); msg != "" {
		a.renderHooks(w, r, s, f, msg, http.StatusUnprocessableEntity)
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	h := store.Webhook{URL: f.URL, Events: f.eventList(), Active: f.Active, AllowLocal: f.AllowLocal, CreatedBy: who.Name, PersonID: who.PersonID}
	if s.V != nil {
		h.RepoID = s.V.Repo.ID
	} else {
		h.OwnerID = s.Owner.ID
	}
	h, err := a.db.SaveWebhook(ctx, h)
	if err == nil && f.secret != "" {
		err = a.db.SetWebhookSecret(ctx, a.secretSealer(), h.ID, f.secret)
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.rememberBaseURL(a.baseURL(r))
	if h.Active {
		_ = a.appsPing(ctx, h, who)
	}
	http.Redirect(w, r, s.HookURL(h.ID)+"?created=1", http.StatusSeeOther)
}

type appsHookData struct {
	Scope      appsHookScope
	Hook       store.Webhook
	Form       appsHookForm
	Events     []appsEventInfo
	Deliveries []store.Delivery
	Created    bool
}

func (a *App) renderHook(w http.ResponseWriter, r *http.Request, s appsHookScope, h store.Webhook, f appsHookForm, errMsg, notice string, code int) {
	d := &appsHookData{Scope: s, Hook: h, Form: f, Events: appsEventInfos, Created: r.URL.Query().Get("created") != ""}
	d.Deliveries, _ = a.db.Deliveries(r.Context(), h.ID, 50)
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "apps_webhook", page{Title: appsHookHost(h.URL) + " · Webhooks", Nav: appsNav(s), Error: errMsg, Notice: notice, Data: d})
}

func (a *App) appsHookPage(w http.ResponseWriter, r *http.Request) {
	s, h, ok := a.appsLoadHook(w, r)
	if !ok {
		return
	}
	notice := ""
	if r.URL.Query().Get("saved") != "" {
		notice = "saved"
	}
	a.renderHook(w, r, s, h, appsFormOfHook(h), "", notice, http.StatusOK)
}

func (a *App) appsHookUpdate(w http.ResponseWriter, r *http.Request) {
	s, h, ok := a.appsLoadHook(w, r)
	if !ok {
		return
	}
	f := appsHookFormFrom(r)
	f.HasSecret = h.HasSecret
	if msg := f.validate(); msg != "" {
		a.renderHook(w, r, s, h, f, msg, "", http.StatusUnprocessableEntity)
		return
	}
	ctx := r.Context()
	h.URL, h.Events, h.Active, h.AllowLocal = f.URL, f.eventList(), f.Active, f.AllowLocal
	if _, err := a.db.SaveWebhook(ctx, h); err != nil {
		a.fail(w, r, err)
		return
	}
	switch {
	case f.secret != "":
		if err := a.db.SetWebhookSecret(ctx, a.secretSealer(), h.ID, f.secret); err != nil {
			a.fail(w, r, err)
			return
		}
	case f.RemoveSecret:
		_ = a.db.SetWebhookSecret(ctx, a.secretSealer(), h.ID, "")
	}
	http.Redirect(w, r, s.HookURL(h.ID)+"?saved=1", http.StatusSeeOther)
}

func (a *App) appsHookDelete(w http.ResponseWriter, r *http.Request) {
	s, h, ok := a.appsLoadHook(w, r)
	if !ok {
		return
	}
	if err := a.db.DeleteWebhook(r.Context(), h.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, s.Base+s.Query, http.StatusSeeOther)
}

func (a *App) appsHookPing(w http.ResponseWriter, r *http.Request) {
	s, h, ok := a.appsLoadHook(w, r)
	if !ok {
		return
	}
	a.rememberBaseURL(a.baseURL(r))
	if err := a.appsPing(r.Context(), h, a.actor(r)); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, s.HookURL(h.ID)+"#deliveries", http.StatusSeeOther)
}

func (a *App) appsLoadDelivery(w http.ResponseWriter, r *http.Request) (appsHookScope, store.Webhook, store.Delivery, bool) {
	s, h, ok := a.appsLoadHook(w, r)
	if !ok {
		return s, h, store.Delivery{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("did"), 10, 64)
	var d store.Delivery
	if err == nil {
		d, err = a.db.DeliveryByID(r.Context(), id)
	}
	if err != nil || d.HookID != h.ID {
		a.notFound(w, r)
		return s, h, d, false
	}
	return s, h, d, true
}

type appsDeliveryData struct {
	Scope       appsHookScope
	Hook        store.Webhook
	D           store.Delivery
	Payload     string
	ReqHeaders  []appsHeader
	RespHeaders []appsHeader
}

func (a *App) appsDeliveryPage(w http.ResponseWriter, r *http.Request) {
	s, h, d, ok := a.appsLoadDelivery(w, r)
	if !ok {
		return
	}
	a.render(w, r, "apps_delivery", page{Title: d.Event + " delivery · Webhooks", Nav: appsNav(s), Data: &appsDeliveryData{
		Scope: s, Hook: h, D: d, Payload: appsPretty(d.Payload), ReqHeaders: appsHeaderList(d.ReqHeaders), RespHeaders: appsHeaderList(d.RespHeaders),
	}})
}

func (a *App) appsRedeliverPost(w http.ResponseWriter, r *http.Request) {
	s, h, d, ok := a.appsLoadDelivery(w, r)
	if !ok {
		return
	}
	if err := a.appsRedeliver(r.Context(), h, d); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, s.HookURL(h.ID)+"#deliveries", http.StatusSeeOther)
}
