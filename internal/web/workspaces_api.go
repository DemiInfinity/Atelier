package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atelier/internal/actions"
	"atelier/internal/store"
)

// The runner side of device workspaces (see internal/actions/contract.go):
//
//	POST /api/runner/v1/workspaces/poll          long-poll for a request
//	POST /api/runner/v1/workspaces/{id}/status   progress and result
//	POST /api/runner/v1/workspaces/sync          the workspaces on the device
//
// Each request delivered to a runner carries a fresh repository token
// ("atw_…", only its hash is stored). It reads and writes that one
// repository, as the person who asked, for tokenTTL or until the runner
// reports the final state, whichever comes first.

const (
	wsTokenPrefix  = "atw_"
	ActorWorkspace = "workspace"
	maxWsBody      = 256 << 10
)

func init() {
	registerTokenResolver(func(ctx context.Context, a *App, plain string) (Actor, bool) { return a.wsActor(ctx, plain) })
	registerAccessPolicy(wsAccessPolicy)
}

func wsAPIRoutes(a *App, m *http.ServeMux) {
	p := actions.APIPrefix
	m.HandleFunc("POST "+p+"/workspaces/poll", a.runnerAuthed(a.wsPoll))
	m.HandleFunc("POST "+p+"/workspaces/{id}/status", a.runnerAuthed(a.wsStatus))
	m.HandleFunc("POST "+p+"/workspaces/sync", a.runnerAuthed(a.wsSync))
}

// wsActor resolves a workspace token to an actor that may use only its repository.
func (a *App) wsActor(ctx context.Context, plain string) (Actor, bool) {
	if !strings.HasPrefix(plain, wsTokenPrefix) || len(plain) > 200 {
		return Actor{}, false
	}
	req, err := a.db.WorkspaceRequestByToken(ctx, store.Hash(plain))
	if err != nil {
		return Actor{}, false
	}
	return Actor{Kind: ActorWorkspace, Name: fmt.Sprintf("workspace-%d", req.ID), PersonID: req.PersonID,
		Scopes: []string{fmt.Sprintf("workspace:repo:%d", req.RepoID)}}, true
}

// wsAccessPolicy: a workspace token reads and writes its own repository with
// the access its person has there; nothing else, and never admin.
func wsAccessPolicy(ctx context.Context, a *App, who Actor, repo store.Repo, need Perm) (bool, bool) {
	if who.Kind != ActorWorkspace {
		return false, false
	}
	if repo.ID == 0 || !who.hasScope(fmt.Sprintf("workspace:repo:%d", repo.ID)) || need > PermWrite {
		return false, true
	}
	person := Actor{Kind: ActorOwner, Name: who.Name, PersonID: who.PersonID}
	return a.can(ctx, person, repo, need), true
}

func (a *App) wsPoll(w http.ResponseWriter, r *http.Request, rn store.ActionsRunner) {
	var req actions.WorkspacePollRequest
	if !runnerReadJSON(w, r, maxWsBody, &req) {
		return
	}
	ctx := r.Context()
	if !runnerHasCap(req.Capabilities, actions.CapWorkspaces) {
		runnerError(w, http.StatusForbidden, "this runner hasn't switched on the workspaces capability")
		return
	}
	var presets []string
	for _, p := range req.Presets {
		if p = cleanRunnerText(p, 40); p != "" && len(presets) < 20 {
			presets = append(presets, p)
		}
	}
	if err := a.db.SaveWorkspaceDevice(ctx, store.WorkspaceDevice{RunnerID: rn.ID, Presets: presets,
		Root: cleanRunnerText(req.Root, 500), SSH: req.SSH}); err != nil {
		runnerError(w, http.StatusInternalServerError, "server error")
		return
	}
	_ = a.db.SeenActionsRunner(ctx, rn.ID)
	st := a.ws()
	deadline := time.NewTimer(st.pollWait)
	defer deadline.Stop()
	for {
		wake := st.waitCh()
		if fresh, err := a.db.ActionsRunnerByID(ctx, rn.ID); err != nil || !fresh.Enabled {
			if err != nil {
				runnerError(w, http.StatusUnauthorized, "this runner was removed")
				return
			}
		} else if out := a.wsTake(ctx, r, rn); out != nil {
			runnerJSON(w, http.StatusOK, actions.WorkspacePollResponse{Request: out})
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			_ = a.db.SaveWorkspaceDevice(ctx, store.WorkspaceDevice{RunnerID: rn.ID, Presets: presets,
				Root: cleanRunnerText(req.Root, 500), SSH: req.SSH})
			runnerJSON(w, http.StatusOK, actions.WorkspacePollResponse{})
			return
		case <-ctx.Done():
			return
		}
	}
}

// wsTake hands the runner its next request, with a fresh token.
func (a *App) wsTake(ctx context.Context, r *http.Request, rn store.ActionsRunner) *actions.WorkspaceRequest {
	st := a.ws()
	plain := wsTokenPrefix + store.RandomToken(32)
	req, err := a.db.TakeWorkspaceRequest(ctx, rn.ID, store.Hash(plain), st.tokenTTL, st.pickupTime)
	if err != nil {
		return nil
	}
	rp, err := a.repoByIDForActions(ctx, req.RepoID)
	// The person must still be able to write the repository, and still own the device.
	if err != nil || !a.can(ctx, Actor{Kind: ActorOwner, PersonID: req.PersonID}, rp, PermWrite) ||
		!a.ownsRunner(ctx, Actor{Kind: ActorOwner, PersonID: req.PersonID}, rn) {
		_ = a.db.UpdateWorkspaceRequest(ctx, req.ID, actions.WorkspaceFailed, "Not allowed any more.", "", false, true)
		return nil
	}
	out := &actions.WorkspaceRequest{ID: req.ID, Repo: actions.RepoRef{Owner: rp.Owner, Name: rp.Name}, Branch: req.Branch,
		CloneURL: a.baseURL(r) + "/" + rp.FullName() + ".git", Token: plain, Preset: req.Preset}
	if base := a.sshCloneBase(r); base != "" {
		out.SSHURL = base + "/" + rp.FullName() + ".git"
	}
	return out
}

func (a *App) wsStatus(w http.ResponseWriter, r *http.Request, rn store.ActionsRunner) {
	var in actions.WorkspaceStatus
	if !runnerReadJSON(w, r, maxWsBody, &in) {
		return
	}
	ctx := r.Context()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	req, err := a.db.WorkspaceRequest(ctx, id)
	if err != nil || req.RunnerID != rn.ID {
		runnerError(w, http.StatusNotFound, "no such workspace request for this runner")
		return
	}
	state := in.State
	switch state {
	case actions.WorkspaceCloning, "fetching", actions.WorkspaceCloned, actions.WorkspaceUpdated,
		actions.WorkspaceOpened, actions.WorkspaceFailed:
	default:
		runnerError(w, http.StatusBadRequest, "unknown state")
		return
	}
	final := in.Final || state == actions.WorkspaceFailed || state == actions.WorkspaceOpened
	if err := a.db.UpdateWorkspaceRequest(ctx, id, state, cleanRunnerText(in.Message, 500), cleanRunnerText(in.Path, 1000),
		in.Dirty, final); err != nil {
		runnerError(w, http.StatusInternalServerError, "server error")
		return
	}
	_ = a.db.SeenActionsRunner(ctx, rn.ID)
	runnerJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) wsSync(w http.ResponseWriter, r *http.Request, rn store.ActionsRunner) {
	var in actions.WorkspaceSyncRequest
	if !runnerReadJSON(w, r, maxWsBody, &in) {
		return
	}
	var list []store.Workspace
	for _, ws := range in.Workspaces {
		if len(list) >= 500 {
			break
		}
		p := cleanRunnerText(ws.Path, 1000)
		if p == "" {
			continue
		}
		list = append(list, store.Workspace{OwnerName: cleanRunnerText(ws.Owner, 100), RepoName: cleanRunnerText(ws.Repo, 100),
			Path: p, Branch: cleanRunnerText(ws.Branch, 250), Head: cleanRunnerText(ws.Head, 64), Dirty: ws.Dirty,
			LastOpened: ws.LastOpened})
	}
	if err := a.db.ReplaceWorkspaces(r.Context(), rn.ID, list); err != nil {
		runnerError(w, http.StatusInternalServerError, "server error")
		return
	}
	_ = a.db.SeenActionsRunner(r.Context(), rn.ID)
	runnerJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
