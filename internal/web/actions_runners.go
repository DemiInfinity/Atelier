package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
)

// Settings → Runners: the devices that run Actions jobs.

func actionsRunnerRoutes(a *App, m *http.ServeMux) {
	m.HandleFunc("GET /settings/runners", a.authed(a.runnersPage))
	m.HandleFunc("POST /settings/runners/token", a.authed(a.runnersToken))
	m.HandleFunc("POST /settings/runners/retention", a.authed(a.runnersRetention))
	m.HandleFunc("POST /settings/runners/{id}/rename", a.authed(a.runnerRename))
	m.HandleFunc("POST /settings/runners/{id}/enabled", a.authed(a.runnerEnabled))
	m.HandleFunc("POST /settings/runners/{id}/delete", a.authed(a.runnerDelete))
}

type actRunnerView struct {
	store.ActionsRunner
	Online     bool
	AutoLabels []string
	LastJob    string
	LastJobURL string
	SeenAgo    string
}

func (a *App) runnersPage(w http.ResponseWriter, r *http.Request) {
	a.renderRunners(w, r, "", "", http.StatusOK)
}

func (a *App) renderRunners(w http.ResponseWriter, r *http.Request, token, errMsg string, code int) {
	ctx := r.Context()
	who := a.actor(r)
	all, err := a.db.ActionsRunners(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	off := a.actions().offlineAfter
	var views []actRunnerView
	for _, rn := range all {
		if !a.ownsRunner(ctx, who, rn) {
			continue
		}
		v := actRunnerView{ActionsRunner: rn, Online: time.Since(rn.LastSeen) < off, SeenAgo: ago(rn.LastSeen)}
		v.AutoLabels = []string{"self-hosted"}
		if rn.OS != "" {
			v.AutoLabels = append(v.AutoLabels, runnerOSLabel(rn.OS))
		}
		if rn.Arch != "" {
			v.AutoLabels = append(v.AutoLabels, runnerArchLabel(rn.Arch))
		}
		if rn.LastJobID != 0 {
			if j, err := a.db.ActionsJob(ctx, rn.LastJobID); err == nil {
				if run, err := a.db.ActionsRunByID(ctx, j.RunID); err == nil {
					if rp, err := a.repoForRun(ctx, run); err == nil && a.can(ctx, who, rp, PermRead) {
						v.LastJob = fmt.Sprintf("%s · %s #%d", j.Name, rp.FullName(), run.RunNumber)
						v.LastJobURL = actionsRunURL(rp, run)
					}
				}
			}
		}
		views = append(views, v)
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Online && !views[j].Online })
	base := a.baseURL(r)
	a.rememberBaseURL(base)
	cmd := ""
	if token != "" {
		cmd = "atelier-runner register --url " + base + " --token " + token
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "actions_runners", page{Title: "Runners", Nav: "tokens", Error: errMsg, Notice: r.URL.Query().Get("saved"),
		Data: map[string]any{"Runners": views, "Command": cmd, "Base": base, "LogDays": a.actionsLogDays(ctx),
			"Expires": time.Now().Add(registrationTTL)}})
}

// ownsRunner: runners belong to the person who registered them.
func (a *App) ownsRunner(ctx context.Context, who Actor, rn store.ActionsRunner) bool {
	if who.Kind != ActorOwner {
		return false
	}
	if rn.PersonID == 0 {
		return a.isOwnerPerson(ctx, who.PersonID)
	}
	return rn.PersonID == who.PersonID
}

func (a *App) runnersToken(w http.ResponseWriter, r *http.Request) {
	who := a.actor(r)
	plain := regTokenPrefix + store.RandomToken(24)
	if err := a.db.CreateRunnerRegistration(r.Context(), store.Hash(plain), who.PersonID, registrationTTL); err != nil {
		a.fail(w, r, err)
		return
	}
	a.renderRunners(w, r, plain, "", http.StatusOK)
}

func (a *App) runnersRetention(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days")))
	if err != nil || n < 0 || n > 3650 {
		a.renderRunners(w, r, "", "Keep logs for 0 (forever) to 3650 days.", http.StatusUnprocessableEntity)
		return
	}
	if err := a.db.SetSetting(r.Context(), settingActionsLogDays, strconv.Itoa(n)); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/runners?saved=1", http.StatusSeeOther)
}

func (a *App) runnerFromPath(w http.ResponseWriter, r *http.Request) (store.ActionsRunner, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return store.ActionsRunner{}, false
	}
	rn, err := a.db.ActionsRunnerByID(r.Context(), id)
	if err != nil || !a.ownsRunner(r.Context(), a.actor(r), rn) {
		a.notFound(w, r)
		return store.ActionsRunner{}, false
	}
	return rn, true
}

func (a *App) runnerRename(w http.ResponseWriter, r *http.Request) {
	rn, ok := a.runnerFromPath(w, r)
	if !ok {
		return
	}
	name := cleanRunnerText(r.FormValue("name"), 80)
	if name == "" {
		a.renderRunners(w, r, "", "Give the runner a name.", http.StatusUnprocessableEntity)
		return
	}
	if err := a.db.RenameActionsRunner(r.Context(), rn.ID, name); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/runners?saved=1", http.StatusSeeOther)
}

func (a *App) runnerEnabled(w http.ResponseWriter, r *http.Request) {
	rn, ok := a.runnerFromPath(w, r)
	if !ok {
		return
	}
	if err := a.db.SetActionsRunnerEnabled(r.Context(), rn.ID, r.FormValue("enabled") == "1"); err != nil {
		a.fail(w, r, err)
		return
	}
	a.actions().signal()
	http.Redirect(w, r, "/settings/runners?saved=1", http.StatusSeeOther)
}

func (a *App) runnerDelete(w http.ResponseWriter, r *http.Request) {
	rn, ok := a.runnerFromPath(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// A job still running there won't report back: fail it now.
	st := a.actions()
	st.mu.Lock()
	running, _ := a.db.ActionsJobsWithStatus(ctx, actions.StatusRunning)
	for _, j := range running {
		if j.RunnerID == rn.ID {
			a.finishJobLocked(ctx, j, actions.StatusFailure, nil, "The runner "+rn.Name+" was removed.")
		}
	}
	err := a.db.DeleteActionsRunner(ctx, rn.ID)
	st.mu.Unlock()
	if err != nil {
		a.fail(w, r, err)
		return
	}
	st.signal()
	http.Redirect(w, r, "/settings/runners?saved=1", http.StatusSeeOther)
}
