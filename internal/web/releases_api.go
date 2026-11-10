package web

// REST API for releases, shaped like GitHub's so workflow steps can create
// releases and upload build artefacts with curl. Every call authenticates
// with an access token or an Actions job token (Authorization: Bearer …,
// "token …" or Basic); session cookies are not accepted, so there is no CSRF
// surface. Anonymous callers can read public repositories.
//
//	GET    /api/v1/repos/{owner}/{repo}/releases
//	POST   /api/v1/repos/{owner}/{repo}/releases                 {tag_name, target_commitish, name, body, draft, prerelease, generate_release_notes, built_by}
//	POST   /api/v1/repos/{owner}/{repo}/releases/generate-notes  {tag_name, target_commitish, previous_tag_name}
//	GET    /api/v1/repos/{owner}/{repo}/releases/latest
//	GET    /api/v1/repos/{owner}/{repo}/releases/tags/{tag}
//	GET    /api/v1/repos/{owner}/{repo}/releases/{id}
//	PATCH  /api/v1/repos/{owner}/{repo}/releases/{id}            any of the create fields
//	DELETE /api/v1/repos/{owner}/{repo}/releases/{id}
//	POST   /api/v1/repos/{owner}/{repo}/releases/{id}/assets?name=…   raw body, streamed
//	PUT    /api/v1/repos/{owner}/{repo}/releases/{id}/assets/{name}   raw body, streamed (curl -T)
//	DELETE /api/v1/repos/{owner}/{repo}/releases/assets/{asset_id}

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

func init() {
	registerFeature(feature{
		name: "releases-api",
		routes: func(a *App, m *http.ServeMux) {
			const p = "/api/v1/repos/{owner}/{repo}/releases"
			m.HandleFunc("GET "+p, a.apiReleasesList)
			m.HandleFunc("POST "+p, a.apiReleasesCreate)
			m.HandleFunc("POST "+p+"/generate-notes", a.apiReleasesNotes)
			m.HandleFunc("GET "+p+"/latest", a.apiReleasesLatest)
			m.HandleFunc("GET "+p+"/tags/{tag}", a.apiReleasesByTag)
			m.HandleFunc("GET "+p+"/{id}", a.apiReleasesGet)
			m.HandleFunc("PATCH "+p+"/{id}", a.apiReleasesUpdate)
			m.HandleFunc("DELETE "+p+"/{id}", a.apiReleasesDelete)
			m.HandleFunc("POST "+p+"/{id}/assets", a.apiReleasesUpload)
			m.HandleFunc("PUT "+p+"/{id}/assets/{name}", a.apiReleasesUpload)
			m.HandleFunc("DELETE "+p+"/assets/{asset}", a.apiReleasesAssetDelete)
		},
	})
}

// ---------- JSON shapes ----------

type apiUser struct {
	Login string `json:"login"`
	Type  string `json:"type"` // "User" for people and their tokens, else the actor kind
}

type apiAsset struct {
	ID                 int64     `json:"id"`
	Name               string    `json:"name"`
	ContentType        string    `json:"content_type"`
	Size               int64     `json:"size"`
	Digest             string    `json:"digest"`
	DownloadCount      int64     `json:"download_count"`
	State              string    `json:"state"`
	CreatedAt          time.Time `json:"created_at"`
	Uploader           apiUser   `json:"uploader"`
	BrowserDownloadURL string    `json:"browser_download_url"`
	URL                string    `json:"url"`
}

type apiRelease struct {
	ID              int64      `json:"id"`
	TagName         string     `json:"tag_name"`
	TargetCommitish string     `json:"target_commitish"`
	Name            string     `json:"name"`
	Body            string     `json:"body"`
	Draft           bool       `json:"draft"`
	Prerelease      bool       `json:"prerelease"`
	CreatedAt       time.Time  `json:"created_at"`
	PublishedAt     *time.Time `json:"published_at"`
	Author          apiUser    `json:"author"`
	BuiltBy         string     `json:"built_by,omitempty"`
	CommitSHA       string     `json:"commit_sha,omitempty"`
	URL             string     `json:"url"`
	HTMLURL         string     `json:"html_url"`
	UploadURL       string     `json:"upload_url"`
	TarballURL      string     `json:"tarball_url,omitempty"`
	ZipballURL      string     `json:"zipball_url,omitempty"`
	Assets          []apiAsset `json:"assets"`
}

func apiUserFor(kind, name string) apiUser {
	t := "User"
	switch kind {
	case ActorOwner, ActorToken, "":
	default:
		t = kind
	}
	return apiUser{Login: name, Type: t}
}

func (a *App) apiReleaseJSON(ctx context.Context, r *http.Request, repo store.Repo, g *gitrepo.Repo, rel store.Release) (apiRelease, error) {
	base := a.baseURL(r)
	api := fmt.Sprintf("%s/api/v1/repos/%s/releases/%d", base, repo.FullName(), rel.ID)
	out := apiRelease{
		ID: rel.ID, TagName: rel.Tag, TargetCommitish: rel.Target, Name: rel.Title, Body: rel.Body,
		Draft: rel.Draft, Prerelease: rel.Prerelease, CreatedAt: rel.CreatedAt.UTC(), Author: apiUserFor(rel.AuthorKind, rel.AuthorName),
		BuiltBy: rel.BuiltBy, CommitSHA: rel.CommitSHA, URL: api, HTMLURL: base + releaseTagURL(repo, rel.Tag),
		UploadURL: api + "/assets{?name,label}", Assets: []apiAsset{},
	}
	if rel.PublishedAt != nil {
		t := rel.PublishedAt.UTC()
		out.PublishedAt = &t
	}
	if g != nil {
		if _, ok := g.TagCommit(ctx, rel.Tag); ok {
			file := base + "/" + repo.FullName() + "/archive/" + pathEscapeTag(rel.Tag)
			out.TarballURL, out.ZipballURL = file+".tar.gz", file+".zip"
		}
	}
	assets, err := a.db.ReleaseAssets(ctx, rel.ID)
	if err != nil {
		return out, err
	}
	for _, as := range assets {
		out.Assets = append(out.Assets, apiAssetJSON(base, repo, rel, as))
	}
	return out, nil
}

func apiAssetJSON(base string, repo store.Repo, rel store.Release, as store.ReleaseAsset) apiAsset {
	return apiAsset{
		ID: as.ID, Name: as.Name, ContentType: as.ContentType, Size: as.Size, Digest: "sha256:" + as.SHA256,
		DownloadCount: as.Downloads, State: "uploaded", CreatedAt: as.CreatedAt.UTC(),
		Uploader:           apiUserFor(as.UploaderKind, as.UploaderName),
		BrowserDownloadURL: base + releaseDownloadURL(repo, rel.Tag, as.Name),
		URL:                fmt.Sprintf("%s/api/v1/repos/%s/releases/assets/%d", base, repo.FullName(), as.ID),
	}
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// apiFail answers with a relError's status and message, or a logged 500.
func (a *App) apiFail(w http.ResponseWriter, r *http.Request, err error) {
	var re *relError
	switch {
	case errors.As(err, &re):
		writeAPIError(w, re.Code, re.Msg)
	case errors.Is(err, store.ErrNotFound), errors.Is(err, gitrepo.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "Not Found")
	default:
		logErr(r, err)
		writeAPIError(w, http.StatusInternalServerError, "Something went wrong; the error has been logged on the server.")
	}
}

// apiRepo authenticates the caller and loads the repository, checking need.
// Private repositories the caller can't read are reported as not found.
func (a *App) apiRepo(w http.ResponseWriter, r *http.Request, need Perm) (store.Repo, *gitrepo.Repo, Actor, bool) {
	ctx := r.Context()
	who, valid := a.requestActor(r, false)
	if !valid {
		if !a.lim.allow(a.clientIP(r)) {
			writeAPIError(w, http.StatusTooManyRequests, "Too many attempts, try again in a minute.")
			return store.Repo{}, nil, who, false
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier"`)
		writeAPIError(w, http.StatusUnauthorized, "Bad credentials")
		return store.Repo{}, nil, who, false
	}
	repo, err := a.db.RepoByName(ctx, r.PathValue("owner"), r.PathValue("repo"))
	if err != nil || !a.can(ctx, who, repo, PermRead) {
		if err == nil && who.Kind == ActorAnonymous {
			w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier"`)
			writeAPIError(w, http.StatusUnauthorized, "Requires authentication")
			return repo, nil, who, false
		}
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return repo, nil, who, false
	}
	if need != PermRead && !a.can(ctx, who, repo, need) {
		if who.Kind == ActorAnonymous {
			w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier"`)
			writeAPIError(w, http.StatusUnauthorized, "Requires authentication")
		} else {
			writeAPIError(w, http.StatusForbidden, "This credential can't change releases in this repository.")
		}
		return repo, nil, who, false
	}
	g, err := a.git.Open(repo.Owner, repo.Name)
	if err != nil {
		a.apiFail(w, r, err)
		return repo, nil, who, false
	}
	return repo, g, who, true
}

func (a *App) apiReleaseByID(w http.ResponseWriter, r *http.Request, repo store.Repo, who Actor) (store.Release, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return store.Release{}, false
	}
	rel, err := a.db.ReleaseByID(r.Context(), repo.ID, id)
	if err != nil || (rel.Draft && !a.can(r.Context(), who, repo, PermWrite)) {
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return store.Release{}, false
	}
	return rel, true
}

func (a *App) writeRelease(w http.ResponseWriter, r *http.Request, status int, repo store.Repo, g *gitrepo.Repo, rel store.Release) {
	out, err := a.apiReleaseJSON(r.Context(), r, repo, g, rel)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	writeJSON(w, status, out)
}

func decodeJSONBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(v); err != nil && err != io.EOF {
		return relErr(http.StatusBadRequest, "Problems parsing JSON: %v", err)
	}
	return nil
}

// ---------- handlers ----------

func (a *App) apiReleasesList(w http.ResponseWriter, r *http.Request) {
	repo, g, who, ok := a.apiRepo(w, r, PermRead)
	if !ok {
		return
	}
	ctx := r.Context()
	rels, err := a.db.Releases(ctx, repo.ID, a.can(ctx, who, repo, PermWrite))
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	pageNum, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if perPage <= 0 || perPage > 100 {
		perPage = 30
	}
	if pageNum < 1 {
		pageNum = 1
	}
	start := (pageNum - 1) * perPage
	out := []apiRelease{}
	for i := start; i < len(rels) && i < start+perPage; i++ {
		j, err := a.apiReleaseJSON(ctx, r, repo, g, rels[i])
		if err != nil {
			a.apiFail(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

type apiReleaseBody struct {
	TagName              *string `json:"tag_name"`
	TargetCommitish      *string `json:"target_commitish"`
	Name                 *string `json:"name"`
	Body                 *string `json:"body"`
	Draft                *bool   `json:"draft"`
	Prerelease           *bool   `json:"prerelease"`
	GenerateReleaseNotes bool    `json:"generate_release_notes"`
	BuiltBy              *string `json:"built_by"`
	PreviousTagName      string  `json:"previous_tag_name"`
}

func (b apiReleaseBody) apply(in *releaseInput) {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	set(&in.Tag, b.TagName)
	set(&in.Target, b.TargetCommitish)
	set(&in.Title, b.Name)
	set(&in.Body, b.Body)
	set(&in.BuiltBy, b.BuiltBy)
	if b.Draft != nil {
		in.Draft = *b.Draft
	}
	if b.Prerelease != nil {
		in.Prerelease = *b.Prerelease
	}
}

func (a *App) apiReleasesCreate(w http.ResponseWriter, r *http.Request) {
	repo, g, who, ok := a.apiRepo(w, r, PermWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	var body apiReleaseBody
	if err := decodeJSONBody(r, &body); err != nil {
		a.apiFail(w, r, err)
		return
	}
	var in releaseInput
	body.apply(&in)
	if body.GenerateReleaseNotes {
		notes, _, err := a.generateReleaseNotes(ctx, repo, g, strings.TrimSpace(in.Tag), strings.TrimSpace(in.Target), body.PreviousTagName)
		if err != nil {
			a.apiFail(w, r, err)
			return
		}
		if strings.TrimSpace(in.Body) != "" {
			in.Body = strings.TrimRight(in.Body, "\n") + "\n\n" + notes
		} else {
			in.Body = notes
		}
	}
	rel, err := a.createRelease(ctx, repo, g, who, in)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	a.writeRelease(w, r, http.StatusCreated, repo, g, rel)
}

func (a *App) apiReleasesNotes(w http.ResponseWriter, r *http.Request) {
	repo, g, _, ok := a.apiRepo(w, r, PermWrite)
	if !ok {
		return
	}
	var body apiReleaseBody
	if err := decodeJSONBody(r, &body); err != nil {
		a.apiFail(w, r, err)
		return
	}
	var in releaseInput
	body.apply(&in)
	notes, _, err := a.generateReleaseNotes(r.Context(), repo, g, strings.TrimSpace(in.Tag), strings.TrimSpace(in.Target), body.PreviousTagName)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": in.Tag, "body": notes})
}

func (a *App) apiReleasesLatest(w http.ResponseWriter, r *http.Request) {
	repo, g, _, ok := a.apiRepo(w, r, PermRead)
	if !ok {
		return
	}
	rel, err := a.db.LatestRelease(r.Context(), repo.ID)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	a.writeRelease(w, r, http.StatusOK, repo, g, rel)
}

func (a *App) apiReleasesByTag(w http.ResponseWriter, r *http.Request) {
	repo, g, who, ok := a.apiRepo(w, r, PermRead)
	if !ok {
		return
	}
	rel, err := a.db.ReleaseByTag(r.Context(), repo.ID, r.PathValue("tag"))
	if err != nil || (rel.Draft && !a.can(r.Context(), who, repo, PermWrite)) {
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return
	}
	a.writeRelease(w, r, http.StatusOK, repo, g, rel)
}

func (a *App) apiReleasesGet(w http.ResponseWriter, r *http.Request) {
	repo, g, who, ok := a.apiRepo(w, r, PermRead)
	if !ok {
		return
	}
	rel, ok := a.apiReleaseByID(w, r, repo, who)
	if !ok {
		return
	}
	a.writeRelease(w, r, http.StatusOK, repo, g, rel)
}

func (a *App) apiReleasesUpdate(w http.ResponseWriter, r *http.Request) {
	repo, g, who, ok := a.apiRepo(w, r, PermWrite)
	if !ok {
		return
	}
	rel, ok := a.apiReleaseByID(w, r, repo, who)
	if !ok {
		return
	}
	var body apiReleaseBody
	if err := decodeJSONBody(r, &body); err != nil {
		a.apiFail(w, r, err)
		return
	}
	in := releaseInput{Tag: rel.Tag, Target: rel.Target, Title: rel.Title, Body: rel.Body, Draft: rel.Draft, Prerelease: rel.Prerelease}
	body.apply(&in)
	rel, err := a.updateRelease(r.Context(), repo, g, who, rel, in)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	a.writeRelease(w, r, http.StatusOK, repo, g, rel)
}

func (a *App) apiReleasesDelete(w http.ResponseWriter, r *http.Request) {
	repo, _, who, ok := a.apiRepo(w, r, PermWrite)
	if !ok {
		return
	}
	rel, ok := a.apiReleaseByID(w, r, repo, who)
	if !ok {
		return
	}
	if err := a.deleteRelease(r.Context(), repo, rel); err != nil {
		a.apiFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiReleasesUpload streams the request body into a new asset.
func (a *App) apiReleasesUpload(w http.ResponseWriter, r *http.Request) {
	repo, _, who, ok := a.apiRepo(w, r, PermWrite)
	if !ok {
		return
	}
	rel, ok := a.apiReleaseByID(w, r, repo, who)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	if name == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "Say what the file is called with ?name=<file name>.")
		return
	}
	as, err := a.addReleaseAsset(r.Context(), rel, who, name, r.Header.Get("Content-Type"), r.Body, r.ContentLength)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, apiAssetJSON(a.baseURL(r), repo, rel, as))
}

func (a *App) apiReleasesAssetDelete(w http.ResponseWriter, r *http.Request) {
	repo, _, _, ok := a.apiRepo(w, r, PermWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("asset"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return
	}
	as, err := a.db.ReleaseAssetInRepo(ctx, repo.ID, id)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	if err := a.db.DeleteReleaseAsset(ctx, as.ReleaseID, as.ID); err != nil {
		a.apiFail(w, r, err)
		return
	}
	a.dropUnusedReleaseBlobs(ctx, as.SHA256)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- help on the releases page ----------

type releasesAPIHelp struct {
	Create, Upload, Publish, List, Download string
	Limit                                   string
}

// releasesAPIHelp builds the curl examples shown on the releases page.
func (a *App) releasesAPIHelp(r *http.Request, repo store.Repo) releasesAPIHelp {
	base := a.baseURL(r)
	api := base + "/api/v1/repos/" + repo.FullName() + "/releases"
	return releasesAPIHelp{
		Limit: limitText(releaseAssetLimit()),
		Create: `curl -sf -X POST -H "Authorization: Bearer $ATELIER_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"tag_name":"v1.2.0","target_commitish":"main","name":"v1.2.0","draft":true,"generate_release_notes":true}' \
  ` + api,
		Upload: `curl -sf -X POST -H "Authorization: Bearer $ATELIER_TOKEN" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @dist/app-linux-amd64 \
  "` + api + `/$RELEASE_ID/assets?name=app-linux-amd64"`,
		Publish: `curl -sf -X PATCH -H "Authorization: Bearer $ATELIER_TOKEN" \
  -d '{"draft":false}' ` + api + `/$RELEASE_ID`,
		List: `curl -sf -H "Authorization: Bearer $ATELIER_TOKEN" ` + api + `/latest`,
		Download: `curl -fLO -H "Authorization: Bearer $ATELIER_TOKEN" \
  ` + base + "/" + repo.FullName() + `/releases/download/v1.2.0/app-linux-amd64`,
	}
}
