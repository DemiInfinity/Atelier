package web

// Generic files: anything that isn't npm, a container or a Go module.
//
//	curl -T app.tar.gz -H "Authorization: Bearer atl_…" \
//	  https://git.example.com/api/v1/packages/demi/generic/app/1.2.0/app.tar.gz
//	curl -O -H "Authorization: Bearer atl_…" \
//	  https://git.example.com/api/v1/packages/demi/generic/app/1.2.0/app.tar.gz
//
// PUT uploads a file (files are immutable: 409 if it exists), GET/HEAD
// downloads it, DELETE removes it. GET …/{version} lists the version's files
// as JSON and DELETE …/{version} removes the whole version.

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"atelier/internal/packages"
	"atelier/internal/store"
)

var genericNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

func apiJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier"`)
	}
	apiJSON(w, code, map[string]string{"message": msg})
}

func (a *App) serveGenericAPI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who, ok := a.pkgActor(r)
	if !ok {
		apiError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/packages/"), "/")
	if len(segs) < 4 || len(segs) > 5 || segs[1] != store.PkgGeneric {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	for _, s := range append([]string{segs[2]}, segs[3:]...) {
		if !genericNameRE.MatchString(s) || strings.Contains(s, "..") {
			apiError(w, http.StatusBadRequest, "names, versions and file names use letters, numbers, dots, dashes, underscores and +")
			return
		}
	}
	name, version := strings.ToLower(segs[2]), segs[3]
	file := ""
	if len(segs) == 5 {
		file = segs[4]
	}
	owner, err := a.db.OwnerByName(ctx, segs[0])
	if err != nil {
		apiError(w, http.StatusNotFound, "not found")
		return
	}
	pkg, perr := a.db.Package(ctx, owner.ID, store.PkgGeneric, name)
	exists := perr == nil && a.canSeePackage(ctx, who, pkg)
	read := r.Method == http.MethodGet || r.Method == http.MethodHead
	if read || r.Method == http.MethodDelete {
		if !exists {
			if who.Kind == ActorAnonymous {
				apiError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			apiError(w, http.StatusNotFound, "not found")
			return
		}
	}
	if !read {
		if who.Kind == ActorAnonymous {
			apiError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		private := true
		if perr == nil {
			private = pkg.Private
		}
		if perr == nil && !exists {
			apiError(w, http.StatusNotFound, "not found")
			return
		}
		if !a.canPackage(ctx, who, owner, private, PermWrite) {
			apiError(w, http.StatusForbidden, "this credential can't change "+owner.Name+"'s packages (tokens need packages:write)")
			return
		}
	}

	switch {
	case read && file == "":
		v, err := a.db.Version(ctx, pkg.ID, version)
		if err != nil {
			apiError(w, http.StatusNotFound, "version not found")
			return
		}
		files, _ := a.db.Files(ctx, v.ID)
		type fileOut struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
			URL    string `json:"url"`
		}
		out := []fileOut{}
		base := a.baseURL(r) + "/api/v1/packages/" + strings.ToLower(owner.Name) + "/generic/" + pkg.Name + "/" + v.Version + "/"
		for _, f := range files {
			out = append(out, fileOut{f.Name, f.Size, strings.TrimPrefix(f.Digest, "sha256:"), base + f.Name})
		}
		apiJSON(w, http.StatusOK, map[string]any{"name": pkg.Name, "version": v.Version, "files": out})
	case read:
		v, err := a.db.Version(ctx, pkg.ID, version)
		if err != nil {
			apiError(w, http.StatusNotFound, "version not found")
			return
		}
		f, err := a.db.File(ctx, v.ID, file)
		if err != nil {
			apiError(w, http.StatusNotFound, "file not found")
			return
		}
		if r.Method == http.MethodGet && r.Header.Get("Range") == "" {
			a.db.CountDownload(ctx, pkg.ID, v.ID, f.ID)
		}
		a.serveBlob(w, r, f.Digest, "application/octet-stream", f.Name)
	case r.Method == http.MethodPut && file != "":
		a.genericUpload(w, r, who, owner, name, version, file)
	case r.Method == http.MethodDelete && file != "":
		v, err := a.db.Version(ctx, pkg.ID, version)
		if err != nil || a.db.DeleteFile(ctx, v.ID, file) != nil {
			apiError(w, http.StatusNotFound, "file not found")
			return
		}
		if files, _ := a.db.Files(ctx, v.ID); len(files) == 0 {
			_ = a.db.DeleteVersion(ctx, pkg.ID, version)
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete:
		if err := a.db.DeleteVersion(ctx, pkg.ID, version); err != nil {
			apiError(w, http.StatusNotFound, "version not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT, DELETE")
		apiError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) genericUpload(w http.ResponseWriter, r *http.Request, who Actor, owner store.Owner, name, version, file string) {
	ctx := r.Context()
	if r.ContentLength > pkgLimits.Blob {
		apiError(w, http.StatusRequestEntityTooLarge, "file is larger than the "+humanBytes(pkgLimits.Blob)+" limit")
		return
	}
	want := ""
	if s := r.Header.Get("X-Checksum-Sha256"); s != "" {
		want = "sha256:" + strings.ToLower(s)
	}
	wr, err := a.pkgBlobs().Put(r.Body, pkgLimits.Blob, want)
	if err != nil {
		switch {
		case errors.Is(err, packages.ErrTooLarge):
			apiError(w, http.StatusRequestEntityTooLarge, "file is larger than the "+humanBytes(pkgLimits.Blob)+" limit")
		case errors.Is(err, packages.ErrDigest):
			apiError(w, http.StatusBadRequest, "content doesn't match X-Checksum-Sha256")
		default:
			logErr(r, err)
			apiError(w, http.StatusInternalServerError, "storage error")
		}
		return
	}
	pkg, _, err := a.db.EnsurePackage(ctx, owner.ID, store.PkgGeneric, name, false, pkgActorOf(who))
	if err != nil {
		logErr(r, err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	v, err := a.db.Version(ctx, pkg.ID, version)
	newVersion := err != nil
	if newVersion {
		v, err = a.db.AddVersion(ctx, pkg.ID, store.NewVersion{Version: version}, pkgActorOf(who))
		if err != nil && !errors.Is(err, store.ErrExists) {
			logErr(r, err)
			apiError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if errors.Is(err, store.ErrExists) {
			v, _ = a.db.Version(ctx, pkg.ID, version)
		}
	}
	if err := a.db.AddFile(ctx, v.ID, store.PackageFile{Name: file, Digest: wr.Digest, Size: wr.Size, MediaType: "application/octet-stream"}); err != nil {
		if errors.Is(err, store.ErrExists) {
			apiError(w, http.StatusConflict, "this version already has a file named "+file+"; files can't be replaced (delete it first)")
			return
		}
		logErr(r, err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	_ = a.db.TouchPackage(ctx, pkg.ID)
	if newVersion {
		a.emitPackage(ctx, pkg, who, strings.ToLower(owner.Name)+"/"+name+"@"+version)
	}
	apiJSON(w, http.StatusCreated, map[string]any{
		"name": name, "version": version, "file": file, "size": wr.Size, "sha256": strings.TrimPrefix(wr.Digest, "sha256:"),
	})
}
