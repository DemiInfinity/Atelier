package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// Push to create
//
// Pushing to a repository address that doesn't exist yet creates it (private)
// and accepts the push, over HTTPS and SSH — no trip to the web UI. Only people
// who could create a repository under that account may do it (in core: the
// owner, via a signed-in key or an access token with write access). Clones and
// fetches never create anything. It can be switched off in Settings →
// Repositories.

const settingPushToCreate = "repos.push_to_create" // "on" (default) or "off"

func (a *App) pushToCreateEnabled(ctx context.Context) bool {
	v, err := a.db.Setting(ctx, settingPushToCreate)
	return err != nil || v != "off"
}

var errNoPushCreate = errors.New("push to create not allowed")

// pushCreate creates owner/name for who, if allowed. It returns the new repo.
func (a *App) pushCreate(ctx context.Context, who Actor, ownerName, name string) (store.Repo, error) {
	if !a.pushToCreateEnabled(ctx) || who.Kind == ActorAnonymous {
		return store.Repo{}, errNoPushCreate
	}
	if !gitrepo.ValidName(name) || reserved[strings.ToLower(name)] {
		return store.Repo{}, errNoPushCreate
	}
	owner, err := a.db.OwnerByName(ctx, ownerName)
	if err != nil {
		return store.Repo{}, errNoPushCreate
	}
	// Ask the shared permission check about the repository as it would exist:
	// private, under that account. Addons can extend who may do this.
	standIn := store.Repo{OwnerID: owner.ID, Owner: owner.Name, OwnerKind: owner.Kind, Name: name, Private: true}
	if !a.can(ctx, who, standIn, PermWrite) {
		return store.Repo{}, errNoPushCreate
	}
	if err := a.git.Init(ctx, owner.Name, name); err != nil {
		// Someone may have created it a moment ago.
		if r, err2 := a.db.RepoByName(ctx, owner.Name, name); err2 == nil {
			return r, nil
		}
		return store.Repo{}, err
	}
	repo, err := a.db.CreateRepo(ctx, owner.ID, name, "", true)
	if err != nil {
		_ = a.git.Remove(owner.Name, name)
		if r, err2 := a.db.RepoByName(ctx, owner.Name, name); err2 == nil {
			return r, nil
		}
		return store.Repo{}, err
	}
	log.Printf("push to create: %s created %s (private)", who.Kind+":"+who.Name, repo.FullName())
	a.emit(ctx, Event{Kind: "repo.created", RepoID: repo.ID, Repo: repo, Title: "Created " + repo.FullName() + " by pushing to it",
		URL: "/" + repo.FullName(), Actor: who})
	return repo, nil
}

// gitPushCreate handles a push over HTTPS to a repository that doesn't exist.
// It returns the created repo, or writes the HTTP response itself and ok=false.
func (a *App) gitPushCreate(w http.ResponseWriter, r *http.Request, owner, name string) (store.Repo, bool) {
	ctx := r.Context()
	if !a.pushToCreateEnabled(ctx) {
		http.Error(w, "repository not found", http.StatusNotFound)
		return store.Repo{}, false
	}
	if !a.lim.allow(a.clientIP(r)) {
		http.Error(w, "too many attempts, try again in a minute", http.StatusTooManyRequests)
		return store.Repo{}, false
	}
	who := Actor{Kind: ActorAnonymous}
	if u, p, ok := r.BasicAuth(); ok {
		for _, cand := range []string{p, u} {
			if w2, ok := a.resolveCredential(ctx, cand); ok {
				who = w2
				break
			}
		}
	}
	if who.Kind == ActorAnonymous {
		// Ask git for credentials; it retries with them.
		w.Header().Set("WWW-Authenticate", `Basic realm="Atelier", charset="UTF-8"`)
		http.Error(w, "authentication required: use an Atelier access token as the password", http.StatusUnauthorized)
		return store.Repo{}, false
	}
	repo, err := a.pushCreate(ctx, who, owner, name)
	if err != nil {
		if !errors.Is(err, errNoPushCreate) {
			logErr(r, err)
		}
		http.Error(w, "repository not found", http.StatusNotFound)
		return store.Repo{}, false
	}
	a.lim.reset(a.clientIP(r))
	return repo, true
}

// sshPushCreate is the SSH server's hook for pushes to missing repositories.
func (a *App) sshPushCreate(ctx context.Context, keyID int64, owner, name string) (store.Repo, error) {
	personID, err := a.db.SSHKeyPerson(ctx, keyID)
	if err != nil {
		return store.Repo{}, errNoPushCreate
	}
	p, err := a.db.Person(ctx, personID)
	if err != nil {
		return store.Repo{}, errNoPushCreate
	}
	return a.pushCreate(ctx, Actor{Kind: ActorOwner, Name: p.Name, PersonID: p.ID}, owner, name)
}

// ---------- Settings → Repositories ----------

func init() {
	registerFeature(feature{
		name: "pushcreate",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/repositories", a.authed(a.repoSettingsPage))
			m.HandleFunc("POST /settings/repositories", a.authed(a.repoSettingsSave))
		},
	})
}

func (a *App) repoSettingsPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "pushcreate_settings", page{Title: "Repositories", Notice: r.URL.Query().Get("saved"), Data: map[string]any{
		"Enabled": a.pushToCreateEnabled(r.Context()), "Base": a.baseURL(r), "SSHBase": a.sshCloneBase(r),
		"Me": a.actor(r).Name,
	}})
}

func (a *App) repoSettingsSave(w http.ResponseWriter, r *http.Request) {
	if !a.isOwnerPerson(r.Context(), a.actor(r).PersonID) {
		a.forbidden(w, r)
		return
	}
	v := "off"
	if r.FormValue("push_to_create") == "on" {
		v = "on"
	}
	if err := a.db.SetSetting(r.Context(), settingPushToCreate, v); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/repositories?saved=1", http.StatusSeeOther)
}
