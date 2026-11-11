package web

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"atelier/internal/store"
)

// Settings → Devices and the device approval page. Both need a signed-in
// session (never a token), so a device can't list, revoke or approve devices.

func appDeviceRoutes(a *App, m *http.ServeMux) {
	m.HandleFunc("GET /settings/devices", a.authed(a.appDevicesPage))
	// Literal paths only (the id travels in the form), like Settings → SSH keys.
	m.HandleFunc("POST /settings/devices/revoke", a.authed(a.appDeviceRevoke))
	m.HandleFunc("GET /settings/devices/approve", a.authed(a.appApprovePage))
	m.HandleFunc("POST /settings/devices/approve", a.authed(a.appApproveSubmit))
}

func (a *App) appDevicesPage(w http.ResponseWriter, r *http.Request) {
	devices, err := a.db.AppDevices(r.Context(), a.actor(r).PersonID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	notice := ""
	switch r.URL.Query().Get("done") {
	case "revoked":
		notice = "Device revoked. Its token and the SSH keys it added no longer work."
	case "approved":
		notice = "Device approved. It will finish signing in by itself."
	case "denied":
		notice = "Sign-in denied."
	}
	a.render(w, r, "appapi_devices", page{Title: "Devices", Notice: notice, Data: map[string]any{
		"Devices": devices, "Remote": isRemote(r),
	}})
}

func (a *App) appDeviceRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	who := a.actor(r)
	keys, err := a.db.RevokeAppDevice(r.Context(), who.PersonID, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.app().hub.dropDevice(id)
	log.Printf("appapi: %s revoked device %d (%d SSH key(s) removed)", who.Name, id, keys)
	http.Redirect(w, r, "/settings/devices?done=revoked", http.StatusSeeOther)
}

// appApproveRefused answers approval requests that came through the tunnel:
// a device is approved at home only.
func (a *App) appApproveRefused(w http.ResponseWriter, r *http.Request) bool {
	if !isRemote(r) {
		return false
	}
	logErr(r, errString("device approval refused through the tunnel from "+a.clientIP(r)))
	w.WriteHeader(http.StatusForbidden)
	a.render(w, r, "error", page{Title: "Approve at home", Data: map[string]string{
		"Heading": "Approve this device at home",
		"Message": "Devices can only be approved from your home network, not through remote access.",
	}})
	return true
}

type appApproveView struct {
	Code   store.AppDeviceCode
	Asked  string // what was typed, when it didn't match
	Found  bool
	Typing bool // no code yet: show the box to type one
}

func (a *App) appApproveRender(w http.ResponseWriter, r *http.Request, status int, v appApproveView, errMsg string) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	a.render(w, r, "appapi_approve", page{Title: "Approve a device", Error: errMsg, Data: v})
}

// appApproveLookup finds the pending code for the signed-in person, counting
// wrong guesses so a short code can't be found by trying them all.
func (a *App) appApproveLookup(w http.ResponseWriter, r *http.Request, code string) (store.AppDeviceCode, bool) {
	who := a.actor(r)
	key := "approve:" + strconv.FormatInt(who.PersonID, 10)
	lim := a.app().codeLim
	asked := store.NormalizeAppUserCode(code)
	c, err := a.db.ViewAppDeviceCode(r.Context(), asked, who.PersonID)
	if errors.Is(err, store.ErrNotFound) {
		if !lim.allow(key) {
			a.appApproveRender(w, r, http.StatusTooManyRequests, appApproveView{Asked: asked}, "Too many wrong codes. Wait a minute, then try again.")
			return c, false
		}
		a.appApproveRender(w, r, http.StatusNotFound, appApproveView{Asked: asked},
			"That code isn't waiting for approval. It may have expired or been used; start the sign-in again on the device.")
		return c, false
	}
	if err != nil {
		a.fail(w, r, err)
		return c, false
	}
	return c, true
}

func (a *App) appApprovePage(w http.ResponseWriter, r *http.Request) {
	if a.appApproveRefused(w, r) {
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		a.appApproveRender(w, r, http.StatusOK, appApproveView{Typing: true}, "")
		return
	}
	c, ok := a.appApproveLookup(w, r, code)
	if !ok {
		return
	}
	a.appApproveRender(w, r, http.StatusOK, appApproveView{Code: c, Found: true}, "")
}

func (a *App) appApproveSubmit(w http.ResponseWriter, r *http.Request) {
	if a.appApproveRefused(w, r) {
		return
	}
	// ServeHTTP already blocks cross-site posts; checked again here because
	// this form signs a device in.
	if !a.sameOrigin(r) {
		http.Error(w, "cross-site request blocked", http.StatusForbidden)
		return
	}
	decision := r.FormValue("decision")
	if decision != "approve" && decision != "deny" {
		a.appApproveRender(w, r, http.StatusBadRequest, appApproveView{Typing: true}, "Choose Approve or Deny.")
		return
	}
	// A code belongs to the first person who opened it (here or on the page
	// above); nobody else can decide it.
	who := a.actor(r)
	c, err := a.db.DecideAppDeviceCode(r.Context(), r.FormValue("code"), who.PersonID, decision == "approve")
	if errors.Is(err, store.ErrNotFound) {
		if _, ok := a.appApproveLookup(w, r, r.FormValue("code")); ok {
			a.appApproveRender(w, r, http.StatusConflict, appApproveView{Typing: true}, "That code changed while you were deciding. Start the sign-in again on the device.")
		}
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	log.Printf("appapi: %s %s device sign-in %q (%s, %s) from %s", who.Name, map[bool]string{true: "approved", false: "denied"}[decision == "approve"],
		c.Name, c.Kind, c.OS, c.IP)
	done := "denied"
	if decision == "approve" {
		done = "approved"
	}
	http.Redirect(w, r, "/settings/devices?"+url.Values{"done": {done}}.Encode(), http.StatusSeeOther)
}
