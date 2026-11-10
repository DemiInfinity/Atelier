package web

// Container registry at /v2/ (OCI Distribution v2, what `docker push/pull`,
// Portainer, podman and crane speak).
//
// Image names are <host>/<owner>/<name>[:tag]; <name> may contain slashes.
// Authentication is HTTP Basic: `docker login <host>` with any username and an
// Atelier access token (packages:read to pull private images, packages:write to
// push) as the password. Registries often add a Bearer token service
// (/v2/token) on top, but that only matters when the registry and the auth
// server are separate; with one server Basic is simpler and Docker, Podman,
// containerd and crane all support it. Docker only sends credentials over
// HTTPS unless the host is listed in "insecure-registries".
//
// Pushes: monolithic (POST ?digest=) and chunked (POST, PATCH…, PUT ?digest=)
// blob uploads, cross-repository mounts (POST ?mount=&from=), image manifests
// and indexes (multi-arch). Pulls, tags list (with n/last paging), catalog, and
// deletes of manifests, tags and blob links.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"atelier/internal/packages"
	"atelier/internal/store"
)

var ociNameRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)
var ociTagRE = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

const (
	mtOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mtOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mtDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	mtDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
)

func ociError(w http.ResponseWriter, status int, code, msg string) {
	h := w.Header()
	if status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", `Basic realm="Atelier container registry", charset="UTF-8"`)
	}
	h.Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

// ociRoute is a parsed /v2/ path.
type ociRoute struct {
	owner, name string // "demi", "atelier" (name may contain slashes)
	kind        string // "blob", "upload", "manifest", "tags", "referrers"
	ref         string // digest, tag or upload id
}

func parseOCIPath(p string) (ociRoute, bool) {
	segs := strings.Split(strings.TrimPrefix(p, "/v2/"), "/")
	n := len(segs)
	var rt ociRoute
	var k int
	switch {
	case n >= 4 && segs[n-3] == "blobs" && segs[n-2] == "uploads":
		rt.kind, rt.ref, k = "upload", segs[n-1], n-3
	case n >= 3 && segs[n-2] == "blobs":
		rt.kind, rt.ref, k = "blob", segs[n-1], n-2
	case n >= 3 && segs[n-2] == "manifests":
		rt.kind, rt.ref, k = "manifest", segs[n-1], n-2
	case n >= 3 && segs[n-2] == "tags" && segs[n-1] == "list":
		rt.kind, k = "tags", n-2
	case n >= 3 && segs[n-2] == "referrers":
		rt.kind, rt.ref, k = "referrers", segs[n-1], n-2
	default:
		return rt, false
	}
	if k < 2 {
		return rt, false
	}
	rt.owner = strings.ToLower(segs[0])
	rt.name = strings.ToLower(strings.Join(segs[1:k], "/"))
	return rt, ociNameRE.MatchString(rt.name)
}

func (a *App) serveOCI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	who, ok := a.pkgActor(r)
	if !ok {
		ociError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid credentials: use an Atelier access token as the password")
		return
	}
	p := r.URL.Path
	if p == "/v2" || p == "/v2/" {
		if who.Kind == ActorAnonymous {
			ociError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
		return
	}
	if p == "/v2/_catalog" {
		a.ociCatalog(w, r, who)
		return
	}
	if p == "/v2/token" {
		// Some clients ask for a token even after a Basic challenge; hand back
		// the credential they already proved (it is checked on every request).
		if who.Kind == ActorAnonymous {
			ociError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		ociError(w, http.StatusNotFound, "UNSUPPORTED", "this registry uses Basic authentication")
		return
	}
	rt, ok := parseOCIPath(p)
	if !ok {
		ociError(w, http.StatusNotFound, "NAME_INVALID", "invalid repository name")
		return
	}
	owner, err := a.db.OwnerByName(ctx, rt.owner)
	if err != nil {
		if who.Kind == ActorAnonymous {
			ociError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		ociError(w, http.StatusNotFound, "NAME_UNKNOWN", "no Atelier account named "+rt.owner)
		return
	}
	pkg, perr := a.db.Package(ctx, owner.ID, store.PkgContainer, rt.name)
	exists := perr == nil
	write := r.Method != http.MethodGet && r.Method != http.MethodHead

	if exists && !a.canSeePackage(ctx, who, pkg) {
		exists = false // hide private images
		if who.Kind == ActorAnonymous {
			ociError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		if write {
			ociError(w, http.StatusForbidden, "DENIED", "you can't push to "+rt.owner+"/"+rt.name)
			return
		}
	}
	if write {
		private := true
		if exists {
			private = pkg.Private
		}
		if who.Kind == ActorAnonymous {
			ociError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		if !a.canPackage(ctx, who, owner, private, PermWrite) {
			ociError(w, http.StatusForbidden, "DENIED", "this credential can't push to "+rt.owner+" (tokens need packages:write)")
			return
		}
	}
	if !exists && !(write && (rt.kind == "upload" && r.Method == http.MethodPost)) {
		if who.Kind == ActorAnonymous {
			ociError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		ociError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository "+rt.owner+"/"+rt.name+" not found")
		return
	}
	if !exists {
		pkg, _, err = a.db.EnsurePackage(ctx, owner.ID, store.PkgContainer, rt.name, false, pkgActorOf(who))
		if err != nil {
			logErr(r, err)
			ociError(w, http.StatusInternalServerError, "UNKNOWN", "internal error")
			return
		}
	}
	base := "/v2/" + rt.owner + "/" + rt.name

	switch rt.kind {
	case "blob":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			a.ociGetBlob(w, r, pkg, rt.ref)
		case http.MethodDelete:
			if !pkgDigestOK(rt.ref) {
				ociError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest")
				return
			}
			_ = a.db.UnlinkBlob(ctx, pkg.ID, rt.ref)
			w.WriteHeader(http.StatusAccepted)
		default:
			ociError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		}
	case "upload":
		a.ociUpload(w, r, who, pkg, base, rt.ref)
	case "manifest":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			a.ociGetManifest(w, r, pkg, rt.ref)
		case http.MethodPut:
			a.ociPutManifest(w, r, who, pkg, base, rt.ref)
		case http.MethodDelete:
			a.ociDeleteManifest(w, r, pkg, rt.ref)
		default:
			ociError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
		}
	case "tags":
		a.ociTags(w, r, pkg, rt.owner+"/"+rt.name)
	default:
		ociError(w, http.StatusNotFound, "UNSUPPORTED", "not supported")
	}
}

func pkgDigestOK(d string) bool { _, ok := packages.ParseDigest(d); return ok }

func (a *App) ociGetBlob(w http.ResponseWriter, r *http.Request, pkg store.Package, digest string) {
	if !pkgDigestOK(digest) {
		ociError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid digest")
		return
	}
	if !a.db.BlobInPackage(r.Context(), pkg.ID, digest) {
		ociError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
		return
	}
	if _, ok := a.pkgBlobs().Stat(digest); !ok {
		ociError(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
		return
	}
	w.Header().Set("Docker-Content-Digest", digest)
	a.serveBlob(w, r, digest, "application/octet-stream", "")
}

// ---------- uploads ----------

func ociRange(size int64) string {
	if size <= 0 {
		return "0-0"
	}
	return fmt.Sprintf("0-%d", size-1)
}

func (a *App) ociUpload(w http.ResponseWriter, r *http.Request, who Actor, pkg store.Package, base, id string) {
	ctx := r.Context()
	b := a.pkgBlobs()
	q := r.URL.Query()
	created := func(digest string) {
		if err := a.db.LinkBlob(ctx, pkg.ID, digest); err != nil {
			logErr(r, err)
			ociError(w, http.StatusInternalServerError, "UNKNOWN", "internal error")
			return
		}
		h := w.Header()
		h.Set("Location", base+"/blobs/"+digest)
		h.Set("Docker-Content-Digest", digest)
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)
	}
	putErr := func(err error) {
		switch {
		case errors.Is(err, packages.ErrTooLarge):
			ociError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "blob is larger than the "+humanBytes(pkgLimits.Blob)+" limit")
		case errors.Is(err, packages.ErrDigest):
			ociError(w, http.StatusBadRequest, "DIGEST_INVALID", "content doesn't match the digest")
		case errors.Is(err, packages.ErrNotFound):
			ociError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload unknown")
		default:
			logErr(r, err)
			ociError(w, http.StatusInternalServerError, "UNKNOWN", "storage error")
		}
	}

	if r.Method == http.MethodPost && id == "" {
		// Cross-repository mount.
		if mount := q.Get("mount"); mount != "" && pkgDigestOK(mount) {
			if a.ociCanMount(ctx, who, q.Get("from"), mount) {
				created(mount)
				return
			}
		}
		// Monolithic upload.
		if d := q.Get("digest"); d != "" {
			wr, err := b.Put(r.Body, pkgLimits.Blob, d)
			if err != nil {
				putErr(err)
				return
			}
			created(wr.Digest)
			return
		}
		id = store.RandomToken(16)
		if err := b.StartUpload(id); err != nil {
			putErr(err)
			return
		}
		if err := a.db.StartUpload(ctx, id, pkg.ID, who.PersonID); err != nil {
			putErr(err)
			return
		}
		h := w.Header()
		h.Set("Location", base+"/blobs/uploads/"+id)
		h.Set("Docker-Upload-UUID", id)
		h.Set("Range", "0-0")
		h.Set("Content-Length", "0")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if id == "" {
		ociError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload unknown")
		return
	}
	if owner, err := a.db.Upload(ctx, id); err != nil || owner != pkg.ID {
		ociError(w, http.StatusNotFound, "BLOB_UPLOAD_UNKNOWN", "upload unknown")
		return
	}
	size, err := b.UploadSize(id)
	if err != nil {
		putErr(err)
		return
	}
	progress := func(code int, size int64) {
		h := w.Header()
		h.Set("Location", base+"/blobs/uploads/"+id)
		h.Set("Docker-Upload-UUID", id)
		h.Set("Range", ociRange(size))
		h.Set("Content-Length", "0")
		w.WriteHeader(code)
	}
	switch r.Method {
	case http.MethodGet:
		progress(http.StatusNoContent, size)
	case http.MethodDelete:
		_ = b.CancelUpload(id)
		_ = a.db.EndUpload(ctx, id)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPatch:
		if cr := r.Header.Get("Content-Range"); cr != "" {
			start, _, _ := strings.Cut(strings.TrimPrefix(cr, "bytes "), "-")
			if n, err := strconv.ParseInt(start, 10, 64); err != nil || n != size {
				w.Header().Set("Range", ociRange(size))
				ociError(w, http.StatusRequestedRangeNotSatisfiable, "BLOB_UPLOAD_INVALID", "chunk out of order")
				return
			}
		}
		n, err := b.AppendUpload(id, r.Body, pkgLimits.Blob)
		if err != nil {
			putErr(err)
			return
		}
		progress(http.StatusAccepted, n)
	case http.MethodPut:
		d := q.Get("digest")
		if !pkgDigestOK(d) {
			ociError(w, http.StatusBadRequest, "DIGEST_INVALID", "a digest is required to finish an upload")
			return
		}
		if _, err := b.AppendUpload(id, r.Body, pkgLimits.Blob); err != nil {
			putErr(err)
			return
		}
		wr, err := b.FinishUpload(id, d)
		if err != nil {
			putErr(err)
			return
		}
		_ = a.db.EndUpload(ctx, id)
		created(wr.Digest)
	default:
		ociError(w, http.StatusMethodNotAllowed, "UNSUPPORTED", "method not allowed")
	}
}

// ociCanMount reports whether who can read blob digest in repository from.
func (a *App) ociCanMount(ctx context.Context, who Actor, from, digest string) bool {
	owner, name, ok := strings.Cut(strings.ToLower(from), "/")
	if !ok {
		return false
	}
	o, err := a.db.OwnerByName(ctx, owner)
	if err != nil {
		return false
	}
	src, err := a.db.Package(ctx, o.ID, store.PkgContainer, name)
	if err != nil || !a.canSeePackage(ctx, who, src) {
		return false
	}
	_, onDisk := a.pkgBlobs().Stat(digest)
	return onDisk && a.db.BlobInPackage(ctx, src.ID, digest)
}

// ---------- manifests ----------

type ociDescriptor struct {
	MediaType string         `json:"mediaType"`
	Digest    string         `json:"digest"`
	Size      int64          `json:"size"`
	Platform  map[string]any `json:"platform,omitempty"`
}

type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	ArtifactType  string          `json:"artifactType"`
	Config        *ociDescriptor  `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
	Manifests     []ociDescriptor `json:"manifests"`
	Subject       *ociDescriptor  `json:"subject"`
}

func (m ociManifest) isIndex() bool {
	return m.MediaType == mtOCIIndex || m.MediaType == mtDockerList || (m.Config == nil && len(m.Manifests) > 0)
}

func (a *App) ociPutManifest(w http.ResponseWriter, r *http.Request, who Actor, pkg store.Package, base, ref string) {
	ctx := r.Context()
	body, err := io.ReadAll(io.LimitReader(r.Body, pkgLimits.Manifest+1))
	if err != nil {
		ociError(w, http.StatusBadRequest, "MANIFEST_INVALID", "couldn't read the manifest")
		return
	}
	if int64(len(body)) > pkgLimits.Manifest {
		ociError(w, http.StatusRequestEntityTooLarge, "SIZE_INVALID", "manifest is too large")
		return
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	isDigest := strings.HasPrefix(ref, "sha256:")
	if isDigest && ref != digest {
		ociError(w, http.StatusBadRequest, "DIGEST_INVALID", "manifest doesn't match its digest")
		return
	}
	if !isDigest && !ociTagRE.MatchString(ref) {
		ociError(w, http.StatusBadRequest, "TAG_INVALID", "invalid tag")
		return
	}
	var m ociManifest
	if err := json.Unmarshal(body, &m); err != nil || m.SchemaVersion != 2 {
		ociError(w, http.StatusBadRequest, "MANIFEST_INVALID", "not a schema 2 manifest")
		return
	}
	mt := m.MediaType
	if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && ct != "" && ct != "application/json" {
		mt = ct
	}
	if mt == "" {
		mt = mtOCIManifest
		if m.isIndex() {
			mt = mtOCIIndex
		}
	}

	files := []store.PackageFile{{Name: "manifest", Digest: digest, Size: int64(len(body)), MediaType: mt}}
	var size int64
	if m.isIndex() {
		for _, c := range m.Manifests {
			v, err := a.db.Version(ctx, pkg.ID, c.Digest)
			if err != nil {
				ociError(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "index refers to unknown manifest "+c.Digest)
				return
			}
			size += v.Size
			files = append(files, store.PackageFile{Name: c.Digest, Digest: c.Digest, Size: c.Size, MediaType: c.MediaType})
		}
	} else {
		descs := append([]ociDescriptor{}, m.Layers...)
		if m.Config != nil {
			descs = append(descs, *m.Config)
		}
		for _, d := range descs {
			n, onDisk := a.pkgBlobs().Stat(d.Digest)
			if !pkgDigestOK(d.Digest) || !onDisk || !a.db.BlobInPackage(ctx, pkg.ID, d.Digest) {
				ociError(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", "blob unknown to registry: "+d.Digest)
				return
			}
			size += n
			files = append(files, store.PackageFile{Name: d.Digest, Digest: d.Digest, Size: n, MediaType: d.MediaType})
		}
	}
	if _, err := a.pkgBlobs().Put(bytes.NewReader(body), pkgLimits.Manifest, digest); err != nil {
		logErr(r, err)
		ociError(w, http.StatusInternalServerError, "UNKNOWN", "storage error")
		return
	}
	var tags []string
	if !isDigest {
		tags = []string{ref}
	}
	if _, err := a.db.AddVersion(ctx, pkg.ID, store.NewVersion{
		Version: digest, Metadata: string(body), MediaType: mt, Size: size, Files: files, Tags: tags, Replace: true,
	}, pkgActorOf(who)); err != nil {
		logErr(r, err)
		ociError(w, http.StatusInternalServerError, "UNKNOWN", "internal error")
		return
	}
	if !isDigest {
		a.emitPackage(ctx, pkg, who, strings.ToLower(pkg.Owner)+"/"+pkg.Name+":"+ref)
	}
	h := w.Header()
	h.Set("Location", base+"/manifests/"+digest)
	h.Set("Docker-Content-Digest", digest)
	if m.Subject != nil {
		h.Set("OCI-Subject", m.Subject.Digest)
	}
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func (a *App) ociVersion(ctx context.Context, pkg store.Package, ref string) (store.PackageVersion, error) {
	if strings.HasPrefix(ref, "sha256:") {
		return a.db.Version(ctx, pkg.ID, ref)
	}
	return a.db.TagVersion(ctx, pkg.ID, ref)
}

func (a *App) ociGetManifest(w http.ResponseWriter, r *http.Request, pkg store.Package, ref string) {
	ctx := r.Context()
	v, err := a.ociVersion(ctx, pkg, ref)
	if err != nil {
		ociError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	f, err := a.db.File(ctx, v.ID, "manifest")
	if err != nil {
		ociError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	if r.Method == http.MethodGet {
		a.db.CountDownload(context.WithoutCancel(ctx), pkg.ID, v.ID, f.ID)
	}
	w.Header().Set("Docker-Content-Digest", f.Digest)
	a.serveBlob(w, r, f.Digest, v.MediaType, "")
}

func (a *App) ociDeleteManifest(w http.ResponseWriter, r *http.Request, pkg store.Package, ref string) {
	ctx := r.Context()
	var err error
	if strings.HasPrefix(ref, "sha256:") {
		err = a.db.DeleteVersion(ctx, pkg.ID, ref)
	} else {
		err = a.db.DeleteTag(ctx, pkg.ID, ref)
	}
	if err != nil {
		ociError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ---------- tags and catalog ----------

// ociPage applies ?n= and ?last= paging and sets the Link header.
func ociPage(w http.ResponseWriter, r *http.Request, all []string, path string) []string {
	q := r.URL.Query()
	if last := q.Get("last"); last != "" {
		i := sort.SearchStrings(all, last)
		if i < len(all) && all[i] == last {
			i++
		}
		all = all[i:]
	}
	if n, err := strconv.Atoi(q.Get("n")); err == nil && n >= 0 && n < len(all) {
		all = all[:n]
		if n > 0 {
			w.Header().Set("Link", fmt.Sprintf(`<%s?n=%d&last=%s>; rel="next"`, path, n, url.QueryEscape(all[n-1])))
		}
	}
	if all == nil {
		all = []string{}
	}
	return all
}

func (a *App) ociTags(w http.ResponseWriter, r *http.Request, pkg store.Package, name string) {
	tags, err := a.db.TagNames(r.Context(), pkg.ID)
	if err != nil {
		ociError(w, http.StatusInternalServerError, "UNKNOWN", "internal error")
		return
	}
	sort.Strings(tags)
	tags = ociPage(w, r, tags, "/v2/"+name+"/tags/list")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "tags": tags})
}

func (a *App) ociCatalog(w http.ResponseWriter, r *http.Request, who Actor) {
	ctx := r.Context()
	if who.Kind == ActorAnonymous {
		ociError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	pkgs, err := a.db.PackagesByType(ctx, 0, store.PkgContainer)
	if err != nil {
		ociError(w, http.StatusInternalServerError, "UNKNOWN", "internal error")
		return
	}
	var names []string
	for _, p := range pkgs {
		if a.canSeePackage(ctx, who, p) {
			names = append(names, strings.ToLower(p.Owner)+"/"+p.Name)
		}
	}
	sort.Strings(names)
	names = ociPage(w, r, names, "/v2/_catalog")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"repositories": names})
}
