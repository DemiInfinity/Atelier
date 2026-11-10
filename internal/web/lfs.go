package web

// Git LFS server: the batch API with the basic transfer adapter, and the
// file locking API, under /{owner}/{repo}.git/info/lfs/.
//
//	POST /{owner}/{repo}.git/info/lfs/objects/batch
//	PUT  /{owner}/{repo}.git/info/lfs/objects/{oid}      upload (write)
//	GET  /{owner}/{repo}.git/info/lfs/objects/{oid}      download (read)
//	POST /{owner}/{repo}.git/info/lfs/objects/verify     verify an upload (write)
//	GET  /{owner}/{repo}.git/info/lfs/locks              list locks (read)
//	POST /{owner}/{repo}.git/info/lfs/locks              lock a path (write)
//	POST /{owner}/{repo}.git/info/lfs/locks/verify       ours / theirs (write)
//	POST /{owner}/{repo}.git/info/lfs/locks/{id}/unlock  unlock (write; force for others' locks)
//
// Authentication follows git over HTTPS exactly: anonymous reads of public
// repositories, otherwise an access token or Actions job token as the Basic
// auth password, checked with a.can. Objects are stored content-addressed
// under <data>/lfs/ (shared between repositories, so forks don't duplicate
// data) and each repository records which objects it holds, so one
// repository can never download another's objects by guessing an oid.

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

	"atelier/internal/store"
)

const lfsMediaType = "application/vnd.git-lfs+json"

func init() {
	registerPathHandler(func(a *App, w http.ResponseWriter, r *http.Request) bool {
		owner, repo, rest, ok := lfsPath(r.URL.Path)
		if !ok {
			return false
		}
		a.serveLFS(w, r, owner, repo, rest)
		return true
	})
}

// lfsPath matches /{owner}/{repo}.git/info/lfs/<rest>.
func lfsPath(p string) (owner, repo, rest string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 3)
	if len(parts) < 3 || !strings.HasSuffix(strings.ToLower(parts[1]), ".git") {
		return "", "", "", false
	}
	if !strings.HasPrefix(parts[2], "info/lfs/") {
		return "", "", "", false
	}
	return parts[0], parts[1][:len(parts[1])-4], strings.TrimPrefix(parts[2], "info/lfs/"), true
}

func lfsJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", lfsMediaType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func lfsError(w http.ResponseWriter, status int, msg string) {
	lfsJSON(w, status, map[string]string{"message": msg})
}

// lfsAuth decides who the caller is and whether they may do need, answering
// 401/403/404 itself (with the same rate limit as git over HTTPS).
func (a *App) lfsAuth(w http.ResponseWriter, r *http.Request, repo store.Repo, need Perm) (Actor, bool) {
	ctx := r.Context()
	who := Actor{Kind: ActorAnonymous, Name: "anonymous"}
	if a.can(ctx, who, repo, need) && r.Header.Get("Authorization") == "" {
		return who, true
	}
	if !a.lim.allow(a.clientIP(r)) {
		lfsError(w, http.StatusTooManyRequests, "too many attempts, try again in a minute")
		return who, false
	}
	if u, p, ok := r.BasicAuth(); ok {
		for _, cand := range []string{p, u} {
			if got, ok := a.resolveCredential(ctx, cand); ok {
				who = got
				break
			}
		}
	} else if h := r.Header.Get("Authorization"); h != "" {
		scheme, cred, _ := strings.Cut(h, " ")
		if s := strings.ToLower(scheme); s == "bearer" || s == "token" {
			if got, ok := a.resolveCredential(ctx, strings.TrimSpace(cred)); ok {
				who = got
			}
		}
	}
	if who.Kind == ActorAnonymous {
		if a.can(ctx, who, repo, need) {
			a.lim.reset(a.clientIP(r))
			return who, true
		}
		w.Header().Set("LFS-Authenticate", `Basic realm="Atelier"`)
		w.Header().Set("WWW-Authenticate", `Basic realm="Atelier", charset="UTF-8"`)
		lfsError(w, http.StatusUnauthorized, "authentication required: use an Atelier access token as the password")
		return who, false
	}
	if !a.can(ctx, who, repo, need) {
		if !a.can(ctx, who, repo, PermRead) {
			lfsError(w, http.StatusNotFound, "repository not found")
		} else {
			lfsError(w, http.StatusForbidden, "this credential can't write to this repository")
		}
		return who, false
	}
	a.lim.reset(a.clientIP(r))
	return who, true
}

func (a *App) serveLFS(w http.ResponseWriter, r *http.Request, owner, name, rest string) {
	ctx := r.Context()
	repo, err := a.db.RepoByName(ctx, owner, name)
	if err != nil {
		lfsError(w, http.StatusNotFound, "repository not found")
		return
	}
	switch {
	case rest == "objects/batch" && r.Method == http.MethodPost:
		a.lfsBatch(w, r, repo)
	case rest == "objects/verify" && r.Method == http.MethodPost:
		a.lfsVerify(w, r, repo)
	case strings.HasPrefix(rest, "objects/") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		a.lfsDownload(w, r, repo, strings.TrimPrefix(rest, "objects/"))
	case strings.HasPrefix(rest, "objects/") && r.Method == http.MethodPut:
		a.lfsUpload(w, r, repo, strings.TrimPrefix(rest, "objects/"))
	case rest == "locks" && r.Method == http.MethodGet:
		a.lfsLocksList(w, r, repo)
	case rest == "locks" && r.Method == http.MethodPost:
		a.lfsLockCreate(w, r, repo)
	case rest == "locks/verify" && r.Method == http.MethodPost:
		a.lfsLocksVerify(w, r, repo)
	case strings.HasPrefix(rest, "locks/") && strings.HasSuffix(rest, "/unlock") && r.Method == http.MethodPost:
		a.lfsUnlock(w, r, repo, strings.TrimSuffix(strings.TrimPrefix(rest, "locks/"), "/unlock"))
	default:
		lfsError(w, http.StatusNotFound, "not found")
	}
}

// ---------- batch ----------

type lfsPointer struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type lfsBatchRequest struct {
	Operation string       `json:"operation"`
	Transfers []string     `json:"transfers"`
	Objects   []lfsPointer `json:"objects"`
	HashAlgo  string       `json:"hash_algo"`
}

type lfsAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int               `json:"expires_in,omitempty"`
}

type lfsObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type lfsBatchObject struct {
	OID           string               `json:"oid"`
	Size          int64                `json:"size"`
	Authenticated bool                 `json:"authenticated,omitempty"`
	Actions       map[string]lfsAction `json:"actions,omitempty"`
	Error         *lfsObjectError      `json:"error,omitempty"`
}

func (a *App) lfsBatch(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	ctx := r.Context()
	var req lfsBatchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		lfsError(w, http.StatusUnprocessableEntity, "invalid batch request")
		return
	}
	need := PermRead
	switch req.Operation {
	case "download":
	case "upload":
		need = PermWrite
	default:
		lfsError(w, http.StatusUnprocessableEntity, "operation must be upload or download")
		return
	}
	if _, ok := a.lfsAuth(w, r, repo, need); !ok {
		return
	}
	if req.HashAlgo != "" && req.HashAlgo != "sha256" {
		lfsError(w, http.StatusConflict, "only sha256 is supported")
		return
	}
	if len(req.Transfers) > 0 && !containsStr(req.Transfers, "basic") {
		lfsError(w, http.StatusUnprocessableEntity, "only the basic transfer adapter is supported")
		return
	}
	if len(req.Objects) > 1000 {
		lfsError(w, http.StatusRequestEntityTooLarge, "send at most 1000 objects per batch")
		return
	}
	base := a.baseURL(r) + "/" + repo.FullName() + ".git/info/lfs/objects/"
	header := map[string]string{}
	if h := r.Header.Get("Authorization"); h != "" {
		header["Authorization"] = h
	}
	limit := lfsObjectLimit()
	out := make([]lfsBatchObject, 0, len(req.Objects))
	for _, p := range req.Objects {
		o := lfsBatchObject{OID: p.OID, Size: p.Size}
		switch {
		case !validSHA256(p.OID):
			o.Error = &lfsObjectError{Code: 422, Message: "invalid object id"}
		case p.Size < 0:
			o.Error = &lfsObjectError{Code: 422, Message: "invalid size"}
		case req.Operation == "download":
			size, err := a.db.LFSObject(ctx, repo.ID, p.OID)
			if _, onDisk := a.lfsBlobs().has(p.OID); err != nil || !onDisk {
				o.Error = &lfsObjectError{Code: 404, Message: "object does not exist"}
				break
			}
			o.Size, o.Authenticated = size, true
			o.Actions = map[string]lfsAction{"download": {Href: base + p.OID, Header: header, ExpiresIn: 3600}}
		default: // upload
			if p.Size > limit {
				o.Error = &lfsObjectError{Code: 422, Message: fmt.Sprintf("object is larger than the %s limit", limitText(limit))}
				break
			}
			if _, err := a.db.LFSObject(ctx, repo.ID, p.OID); err == nil {
				if _, onDisk := a.lfsBlobs().has(p.OID); onDisk {
					break // already here: no actions means nothing to send
				}
			}
			o.Authenticated = true
			o.Actions = map[string]lfsAction{
				"upload": {Href: base + p.OID, Header: header, ExpiresIn: 3600},
				"verify": {Href: base + "verify", Header: header, ExpiresIn: 3600},
			}
		}
		out = append(out, o)
	}
	lfsJSON(w, http.StatusOK, map[string]any{"transfer": "basic", "objects": out, "hash_algo": "sha256"})
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ---------- objects ----------

func (a *App) lfsUpload(w http.ResponseWriter, r *http.Request, repo store.Repo, oid string) {
	ctx := r.Context()
	if _, ok := a.lfsAuth(w, r, repo, PermWrite); !ok {
		return
	}
	if !validSHA256(oid) {
		lfsError(w, http.StatusUnprocessableEntity, "invalid object id")
		return
	}
	limit := lfsObjectLimit()
	if r.ContentLength > limit {
		lfsError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("object is larger than the %s limit", limitText(limit)))
		return
	}
	// If the bytes are already stored (another repository has them), check
	// the upload anyway: the client must prove it has the content.
	_, size, err := a.lfsBlobs().put(r.Body, limit, oid, r.ContentLength)
	switch {
	case errors.Is(err, errBlobTooLarge):
		lfsError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("object is larger than the %s limit", limitText(limit)))
		return
	case errors.Is(err, errBlobChecksum):
		lfsError(w, http.StatusUnprocessableEntity, "content does not match the object id")
		return
	case errors.Is(err, errBlobSize):
		lfsError(w, http.StatusUnprocessableEntity, "content does not match the declared size")
		return
	case err != nil:
		logErr(r, err)
		lfsError(w, http.StatusInternalServerError, "could not store the object")
		return
	}
	if err := a.db.AddLFSObject(context.WithoutCancel(ctx), repo.ID, oid, size); err != nil {
		logErr(r, err)
		lfsError(w, http.StatusInternalServerError, "could not record the object")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *App) lfsDownload(w http.ResponseWriter, r *http.Request, repo store.Repo, oid string) {
	ctx := r.Context()
	if _, ok := a.lfsAuth(w, r, repo, PermRead); !ok {
		return
	}
	if !validSHA256(oid) {
		lfsError(w, http.StatusUnprocessableEntity, "invalid object id")
		return
	}
	if _, err := a.db.LFSObject(ctx, repo.ID, oid); err != nil {
		lfsError(w, http.StatusNotFound, "object does not exist")
		return
	}
	f, err := a.lfsBlobs().open(oid)
	if err != nil {
		lfsError(w, http.StatusNotFound, "object does not exist")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		logErr(r, err)
		lfsError(w, http.StatusInternalServerError, "could not read the object")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("ETag", `"`+oid+`"`)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func (a *App) lfsVerify(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	ctx := r.Context()
	if _, ok := a.lfsAuth(w, r, repo, PermWrite); !ok {
		return
	}
	var p lfsPointer
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&p); err != nil || !validSHA256(p.OID) {
		lfsError(w, http.StatusUnprocessableEntity, "invalid verify request")
		return
	}
	size, err := a.db.LFSObject(ctx, repo.ID, p.OID)
	disk, onDisk := a.lfsBlobs().has(p.OID)
	if err != nil || !onDisk {
		lfsError(w, http.StatusNotFound, "object does not exist")
		return
	}
	if size != p.Size || disk != p.Size {
		lfsError(w, http.StatusUnprocessableEntity, "object size does not match")
		return
	}
	lfsJSON(w, http.StatusOK, map[string]string{"message": "ok"})
}

// ---------- locks ----------

type lfsLockOwner struct {
	Name string `json:"name"`
}

type lfsLockJSON struct {
	ID       string       `json:"id"`
	Path     string       `json:"path"`
	LockedAt string       `json:"locked_at"`
	Owner    lfsLockOwner `json:"owner"`
}

func lfsLockOut(l store.LFSLock) lfsLockJSON {
	return lfsLockJSON{ID: strconv.FormatInt(l.ID, 10), Path: l.Path, LockedAt: l.LockedAt.UTC().Format(time.RFC3339),
		Owner: lfsLockOwner{Name: l.OwnerName}}
}

// lockIsOurs: the same person (or, for actors without one, the same actor).
func lockIsOurs(l store.LFSLock, who Actor) bool {
	if l.OwnerPerson != 0 || who.PersonID != 0 {
		return l.OwnerPerson == who.PersonID
	}
	return l.OwnerKind == who.Kind && l.OwnerName == who.Name
}

func lfsCursor(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	if n < 0 {
		return 0
	}
	return n
}

func (a *App) lfsLocksList(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	if _, ok := a.lfsAuth(w, r, repo, PermRead); !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	locks, err := a.db.LFSLocks(r.Context(), repo.ID, q.Get("path"), lfsCursor(q.Get("id")), lfsCursor(q.Get("cursor")), limit+1)
	if err != nil {
		logErr(r, err)
		lfsError(w, http.StatusInternalServerError, "could not list locks")
		return
	}
	next := ""
	if len(locks) > limit {
		locks = locks[:limit]
		next = strconv.FormatInt(locks[len(locks)-1].ID, 10)
	}
	out := make([]lfsLockJSON, 0, len(locks))
	for _, l := range locks {
		out = append(out, lfsLockOut(l))
	}
	resp := map[string]any{"locks": out}
	if next != "" {
		resp["next_cursor"] = next
	}
	lfsJSON(w, http.StatusOK, resp)
}

type lfsRef struct {
	Name string `json:"name"`
}

func (a *App) lfsLockCreate(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	who, ok := a.lfsAuth(w, r, repo, PermWrite)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
		Ref  lfsRef `json:"ref"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || strings.TrimSpace(req.Path) == "" || len(req.Path) > 4096 {
		lfsError(w, http.StatusUnprocessableEntity, "give the path to lock")
		return
	}
	l, err := a.db.CreateLFSLock(r.Context(), store.LFSLock{RepoID: repo.ID, Path: req.Path, Ref: req.Ref.Name,
		OwnerKind: who.Kind, OwnerName: who.Name, OwnerPerson: who.PersonID})
	if errors.Is(err, store.ErrLocked) {
		lfsJSON(w, http.StatusConflict, map[string]any{"lock": lfsLockOut(l), "message": "already created lock"})
		return
	}
	if err != nil {
		logErr(r, err)
		lfsError(w, http.StatusInternalServerError, "could not create the lock")
		return
	}
	lfsJSON(w, http.StatusCreated, map[string]any{"lock": lfsLockOut(l)})
}

func (a *App) lfsLocksVerify(w http.ResponseWriter, r *http.Request, repo store.Repo) {
	who, ok := a.lfsAuth(w, r, repo, PermWrite)
	if !ok {
		return
	}
	var req struct {
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	locks, err := a.db.LFSLocks(r.Context(), repo.ID, "", 0, lfsCursor(req.Cursor), limit+1)
	if err != nil {
		logErr(r, err)
		lfsError(w, http.StatusInternalServerError, "could not list locks")
		return
	}
	next := ""
	if len(locks) > limit {
		locks = locks[:limit]
		next = strconv.FormatInt(locks[len(locks)-1].ID, 10)
	}
	ours, theirs := []lfsLockJSON{}, []lfsLockJSON{}
	for _, l := range locks {
		if lockIsOurs(l, who) {
			ours = append(ours, lfsLockOut(l))
		} else {
			theirs = append(theirs, lfsLockOut(l))
		}
	}
	resp := map[string]any{"ours": ours, "theirs": theirs}
	if next != "" {
		resp["next_cursor"] = next
	}
	lfsJSON(w, http.StatusOK, resp)
}

func (a *App) lfsUnlock(w http.ResponseWriter, r *http.Request, repo store.Repo, idStr string) {
	who, ok := a.lfsAuth(w, r, repo, PermWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	var req struct {
		Force bool   `json:"force"`
		Ref   lfsRef `json:"ref"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req)
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		lfsError(w, http.StatusNotFound, "lock not found")
		return
	}
	l, err := a.db.LFSLockByID(ctx, repo.ID, id)
	if err != nil {
		lfsError(w, http.StatusNotFound, "lock not found")
		return
	}
	if !lockIsOurs(l, who) && !req.Force {
		lfsError(w, http.StatusForbidden, "this lock belongs to "+l.OwnerName+"; use --force to remove it")
		return
	}
	if err := a.db.DeleteLFSLock(ctx, repo.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		logErr(r, err)
		lfsError(w, http.StatusInternalServerError, "could not remove the lock")
		return
	}
	lfsJSON(w, http.StatusOK, map[string]any{"lock": lfsLockOut(l)})
}

// ---------- usage ----------

type lfsUsage struct {
	Objects int
	Bytes   int64
}

// LFSUsage is the repository's Git LFS storage (repository settings page).
func (v *repoView) LFSUsage() lfsUsage {
	a := releaseApp.Load()
	if a == nil || v == nil {
		return lfsUsage{}
	}
	n, b, _ := a.db.LFSUsage(context.Background(), v.Repo.ID)
	return lfsUsage{Objects: n, Bytes: b}
}
