package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"atelier/internal/pagesdeploy"
	"atelier/internal/store"
)

// POST /api/pages/deploy uploads a site built by a workflow. The body is a
// tar archive (gzip optional) of the site's files; ?repo=owner/name is
// optional and must be the job's own repository.
//
// Only Actions job tokens are accepted, and only for the job's own
// repository (its actions:repo:<id> scope and jobRepoID must both match).
// The site must be set to publish from Actions. A job from a push to the
// site's branch deploys the site; a job from a pull_request run uploads that
// pull request's preview (pages_site_preview.go), if previews are on and the
// job isn't still waiting for approval. Other branches can't deploy.
// Agentic jobs never deploy.

// pagesJobInfo is what the upload needs to know about the job's run.
type pagesJobInfo struct {
	Event, Ref string
	Pull       int64 // pull request number of a pull_request run
	Waiting    bool  // the job needed approval and hasn't had it
}

// pagesJobRun describes the run a job belongs to. Tests replace it.
var pagesJobRun = func(ctx context.Context, a *App, jobID int64) (pagesJobInfo, error) {
	j, err := a.db.ActionsJob(ctx, jobID)
	if err != nil {
		return pagesJobInfo{}, err
	}
	run, err := a.db.ActionsRunByID(ctx, j.RunID)
	if err != nil {
		return pagesJobInfo{}, err
	}
	return pagesJobInfo{Event: run.Event, Ref: run.Ref, Pull: run.PullNumber, Waiting: j.Approval != "" && j.ApprovedBy == ""}, nil
}

func pagesAPIError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (a *App) pagesUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !a.pagesEnabled() {
		pagesAPIError(w, http.StatusServiceUnavailable, "Pages is off on this server: set ATELIER_PAGES_URL")
		return
	}
	plain, ok := bearerToken(r)
	if !ok {
		if _, p, basic := r.BasicAuth(); basic {
			plain, ok = p, p != ""
		}
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier Pages"`)
		pagesAPIError(w, http.StatusUnauthorized, "a job token is required")
		return
	}
	if !a.lim.allow("pages:" + a.clientIP(r)) {
		pagesAPIError(w, http.StatusTooManyRequests, "too many attempts, try again in a minute")
		return
	}
	who, ok := a.resolveCredential(ctx, plain)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier Pages"`)
		pagesAPIError(w, http.StatusUnauthorized, "invalid or expired token")
		return
	}
	a.lim.reset("pages:" + a.clientIP(r))
	if who.Kind != ActorJob || strings.HasPrefix(who.Name, "agent:") {
		pagesAPIError(w, http.StatusForbidden, "only Actions job tokens can deploy Pages")
		return
	}
	jobRepo := jobRepoID(who)
	var rp store.Repo
	var err error
	if full := strings.TrimSpace(r.URL.Query().Get("repo")); full != "" {
		owner, name, _ := strings.Cut(full, "/")
		rp, err = a.db.RepoByName(ctx, owner, name)
	} else {
		rp, err = a.db.RepoByID(ctx, jobRepo)
	}
	if err != nil || jobRepo == 0 || rp.ID != jobRepo || !who.hasScope(fmt.Sprintf("actions:repo:%d", rp.ID)) {
		pagesAPIError(w, http.StatusForbidden, "this job token can only deploy its own repository's site")
		return
	}
	if !a.pagesMayPublish(ctx, rp) {
		pagesAPIError(w, http.StatusForbidden, errPagesPrivate.Error())
		return
	}
	ps, err := a.db.PagesSite(ctx, rp.ID)
	if err != nil {
		pagesAPIError(w, http.StatusInternalServerError, "server error")
		return
	}
	if !ps.Enabled || ps.Source != store.PagesFromActions {
		pagesAPIError(w, http.StatusConflict, "Pages for "+rp.FullName()+" isn't set to deploy from Actions (Settings → Pages)")
		return
	}
	jobID := pagesJobID(who)
	info, err := pagesJobRun(ctx, a, jobID)
	if err != nil {
		pagesAPIError(w, http.StatusForbidden, "unknown job")
		return
	}
	if info.Waiting {
		pagesAPIError(w, http.StatusForbidden, "this job is waiting for approval")
		return
	}
	var pr store.Pull
	preview := strings.HasPrefix(info.Event, "pull_request")
	if preview {
		if _, ok := a.pagesPreviewsWanted(ctx, rp); !ok {
			pagesAPIError(w, http.StatusConflict, "pull request previews are off for "+rp.FullName()+" (Settings → Pages)")
			return
		}
		if pr, err = a.db.PullByNumber(ctx, rp.ID, info.Pull); err != nil || info.Pull == 0 || !pr.IsOpen() {
			pagesAPIError(w, http.StatusConflict, "the pull request isn't open")
			return
		}
	} else if info.Ref != "refs/heads/"+ps.Branch {
		pagesAPIError(w, http.StatusForbidden, "only workflows run from a push to "+ps.Branch+" (or a pull request, for a preview) can deploy this site")
		return
	}
	files := a.pagesFiles()
	// Tar headers and padding add up to ~1 KB a file on top of the content.
	r.Body = http.MaxBytesReader(w, r.Body, files.Limits.MaxBytes+int64(files.Limits.MaxFiles)*1536+1<<20)
	sha := strings.TrimSpace(r.URL.Query().Get("sha"))
	if len(sha) > 64 {
		sha = sha[:64]
	}
	feed := func(extract func(io.Reader) error) error { return extract(r.Body) }
	var d store.PagesDeploy
	siteURL := a.pagesPublicURL(ctx, rp)
	if preview {
		unlock := pagesPreviewLock(rp.ID)
		d, err = a.pagesPublishPreview(ctx, rp, pr, sha, who.Name, feed)
		unlock()
		siteURL = a.pagesPreviewURL(rp, pr.Number)
	} else {
		d = store.PagesDeploy{RepoID: rp.ID, Source: store.PagesFromActions, Ref: info.Ref, SHA: sha, Actor: who.Name}
		d, err = a.pagesPublish(ctx, rp, d, feed)
	}
	if err != nil {
		code := http.StatusBadRequest
		var mbe *http.MaxBytesError
		if errors.Is(err, pagesdeploy.ErrTooLarge) || errors.As(err, &mbe) {
			code = http.StatusRequestEntityTooLarge
		}
		pagesAPIError(w, code, "deploy failed: "+pagesErrText(err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": d.ID, "url": siteURL, "files": d.Files, "bytes": d.Bytes, "preview": preview,
	})
}

// pagesJobID reads actions:job:<id> from a job actor's scopes.
func pagesJobID(who Actor) int64 {
	for _, s := range who.Scopes {
		if v, ok := strings.CutPrefix(s, "actions:job:"); ok {
			id, _ := strconv.ParseInt(v, 10, 64)
			return id
		}
	}
	return 0
}
