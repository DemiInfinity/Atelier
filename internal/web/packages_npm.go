package web

// npm registry at /npm/.
//
// Scoped packages map to owner accounts: @demi/tool belongs to the account
// "demi", @elyndra/ui to "elyndra". .npmrc:
//
//	@demi:registry=https://git.example.com/npm/
//	//git.example.com/npm/:_authToken=atl_…
//
// Supported: publish (PUT with the packument and base64 tarballs), install
// (packument and tarball GETs), dist-tags, deprecate, unpublish of a version
// or a whole package, whoami and ping.
//
// Upstream cache (Settings → Packages, off by default): names that aren't in
// a local scope are fetched from the public registry. Packuments are cached
// for 10 minutes (and served stale when the upstream is down); tarballs are
// kept forever after their integrity is checked. Names in a local scope are
// never looked up upstream, so a public package can't stand in for a private
// one (dependency confusion).

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"atelier/internal/packages"
	"atelier/internal/store"
)

// EventPackagePublished is emitted when a package version is published.
const EventPackagePublished = "package.published"

var npmUpstreamClient = &http.Client{Timeout: 2 * time.Minute}

const npmCacheTTL = 10 * time.Minute

var npmNameRE = regexp.MustCompile(`^(?:@[a-z0-9][a-z0-9._~-]*/)?[a-z0-9._~-][a-z0-9._~-]*$`)

func npmJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func npmError(w http.ResponseWriter, code int, msg string) {
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier npm registry"`)
	}
	npmJSON(w, code, map[string]string{"error": msg})
}

// npmPath splits the part after /npm/ into a package name and the rest.
// "@demi%2ftool/-/tool-1.0.0.tgz" → "@demi/tool", ["-", "tool-1.0.0.tgz"].
func npmPath(escaped string) (name string, rest []string, err error) {
	escaped = strings.NewReplacer("%2f", "/", "%2F", "/").Replace(escaped)
	segs := strings.Split(strings.Trim(escaped, "/"), "/")
	for i, s := range segs {
		if segs[i], err = url.PathUnescape(s); err != nil {
			return "", nil, err
		}
	}
	if len(segs) == 0 || segs[0] == "" {
		return "", nil, errors.New("no package name")
	}
	n := 1
	if strings.HasPrefix(segs[0], "@") {
		if len(segs) < 2 {
			return "", nil, errors.New("scoped name without a package")
		}
		n = 2
	}
	return strings.ToLower(strings.Join(segs[:n], "/")), segs[n:], nil
}

// npmScope returns the scope ("demi") and bare name ("tool") of "@demi/tool".
func npmScope(name string) (scope, bare string) {
	if s, b, ok := strings.Cut(name, "/"); ok && strings.HasPrefix(s, "@") {
		return s[1:], b
	}
	return "", name
}

func (a *App) serveNpm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who, ok := a.pkgActor(r)
	if !ok {
		npmError(w, http.StatusUnauthorized, "invalid token: create one with packages access under Settings → Access tokens")
		return
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.EscapedPath(), "/npm"), "/")
	if rest == "" {
		npmJSON(w, http.StatusOK, map[string]string{"db_name": "atelier"})
		return
	}
	if strings.HasPrefix(rest, "-/") {
		a.npmSpecial(w, r, who, strings.TrimPrefix(rest, "-/"))
		return
	}
	name, tail, err := npmPath(rest)
	if err != nil || !npmNameRE.MatchString(name) {
		npmError(w, http.StatusBadRequest, "invalid package name")
		return
	}
	scope, _ := npmScope(name)
	var owner store.Owner
	local := false
	if scope != "" {
		if o, err := a.db.OwnerByName(ctx, scope); err == nil {
			owner, local = o, true
		}
	}

	switch {
	case len(tail) == 0 && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		if local {
			a.npmPackument(w, r, who, owner, name)
		} else {
			a.npmUpstreamPackument(w, r, who, name)
		}
	case len(tail) == 0 && r.Method == http.MethodPut:
		if !local {
			npmError(w, http.StatusForbidden, fmt.Sprintf("publish under an Atelier account scope, e.g. @%s/%s", a.personalName(ctx), strings.TrimPrefix(name, "@")))
			return
		}
		a.npmPut(w, r, who, owner, name, false)
	case len(tail) == 2 && tail[0] == "-rev" && r.Method == http.MethodPut && local:
		a.npmPut(w, r, who, owner, name, true)
	case len(tail) == 2 && tail[0] == "-rev" && r.Method == http.MethodDelete && local:
		a.npmDeletePackage(w, r, who, owner, name)
	case len(tail) == 2 && tail[0] == "-" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		if local {
			a.npmTarball(w, r, who, owner, name, tail[1])
		} else {
			a.npmUpstreamTarball(w, r, who, name, tail[1])
		}
	case len(tail) >= 2 && tail[0] == "-" && r.Method == http.MethodDelete && local:
		a.npmDeleteTarball(w, r, who, owner, name, tail[1])
	case len(tail) == 1 && r.Method == http.MethodGet && local:
		a.npmVersionDoc(w, r, who, owner, name, tail[0])
	default:
		npmError(w, http.StatusNotFound, "not found")
	}
}

func (a *App) personalName(ctx context.Context) string {
	if me, err := a.db.PersonalOwner(ctx); err == nil {
		return me.Name
	}
	return "you"
}

// npmSpecial handles /npm/-/… endpoints.
func (a *App) npmSpecial(w http.ResponseWriter, r *http.Request, who Actor, rest string) {
	ctx := r.Context()
	switch {
	case rest == "ping":
		npmJSON(w, http.StatusOK, map[string]any{})
	case rest == "whoami":
		if !isPerson(who) {
			npmError(w, http.StatusUnauthorized, "not signed in: add an Atelier access token to .npmrc")
			return
		}
		name := who.Name
		if p, err := a.db.Person(ctx, who.PersonID); err == nil {
			name = p.Name
		}
		npmJSON(w, http.StatusOK, map[string]string{"username": name})
	case strings.HasPrefix(rest, "user/"):
		npmError(w, http.StatusUnauthorized, "npm login isn't supported: create an access token with packages access under Settings → Access tokens and add it to .npmrc as _authToken")
	case strings.HasPrefix(rest, "npm/v1/security/"):
		// npm audit: no advisory database here.
		npmJSON(w, http.StatusOK, map[string]any{})
	case strings.HasPrefix(rest, "v1/search"):
		npmJSON(w, http.StatusOK, map[string]any{"objects": []any{}, "total": 0, "time": time.Now().UTC().Format(time.RFC3339)})
	case strings.HasPrefix(rest, "package/"):
		a.npmDistTags(w, r, who, strings.TrimPrefix(rest, "package/"))
	default:
		npmError(w, http.StatusNotFound, "not found")
	}
}

// npmLoad finds a local package the caller may read (404 otherwise).
func (a *App) npmLoad(w http.ResponseWriter, r *http.Request, who Actor, owner store.Owner, name string) (store.Package, bool) {
	_, bare := npmScope(name)
	p, err := a.db.Package(r.Context(), owner.ID, store.PkgNpm, bare)
	if err != nil || !a.canSeePackage(r.Context(), who, p) {
		npmError(w, http.StatusNotFound, "package not found")
		return p, false
	}
	return p, true
}

func npmRev(p store.Package) string {
	return fmt.Sprintf("%d-%x", p.Versions+1, p.UpdatedAt.Unix())
}

func npmFullName(p store.Package) string { return "@" + strings.ToLower(p.Owner) + "/" + p.Name }

func npmTarballName(bare, version string) string { return bare + "-" + version + ".tgz" }

// npmDoc builds the packument for a local package.
func (a *App) npmDoc(ctx context.Context, r *http.Request, p store.Package) (map[string]any, error) {
	vers, err := a.db.Versions(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	full := npmFullName(p)
	base := a.baseURL(r) + "/npm/"
	versions := map[string]any{}
	times := map[string]string{"created": p.CreatedAt.UTC().Format(time.RFC3339), "modified": p.UpdatedAt.UTC().Format(time.RFC3339)}
	tags := map[string]string{}
	for _, v := range vers {
		m := map[string]any{}
		_ = json.Unmarshal([]byte(v.Metadata), &m)
		m["name"], m["version"], m["_id"] = full, v.Version, full+"@"+v.Version
		dist, _ := m["dist"].(map[string]any)
		if dist == nil {
			dist = map[string]any{}
		}
		dist["tarball"] = base + full + "/-/" + npmTarballName(p.Name, v.Version)
		m["dist"] = dist
		if v.Deprecated != "" {
			m["deprecated"] = v.Deprecated
		} else {
			delete(m, "deprecated")
		}
		versions[v.Version] = m
		times[v.Version] = v.CreatedAt.UTC().Format(time.RFC3339)
		for _, t := range v.Tags {
			tags[t] = v.Version
		}
	}
	doc := map[string]any{
		"_id": full, "name": full, "_rev": npmRev(p), "description": p.Description,
		"dist-tags": tags, "versions": versions, "time": times,
	}
	return doc, nil
}

func (a *App) npmPackument(w http.ResponseWriter, r *http.Request, who Actor, owner store.Owner, name string) {
	p, ok := a.npmLoad(w, r, who, owner, name)
	if !ok {
		return
	}
	doc, err := a.npmDoc(r.Context(), r, p)
	if err != nil {
		logErr(r, err)
		npmError(w, http.StatusInternalServerError, "internal error")
		return
	}
	npmJSON(w, http.StatusOK, doc)
}

func (a *App) npmVersionDoc(w http.ResponseWriter, r *http.Request, who Actor, owner store.Owner, name, ver string) {
	p, ok := a.npmLoad(w, r, who, owner, name)
	if !ok {
		return
	}
	doc, err := a.npmDoc(r.Context(), r, p)
	if err != nil {
		npmError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if v, ok := doc["dist-tags"].(map[string]string)[ver]; ok {
		ver = v
	}
	if m, ok := doc["versions"].(map[string]any)[ver]; ok {
		npmJSON(w, http.StatusOK, m)
		return
	}
	npmError(w, http.StatusNotFound, "version not found")
}

func (a *App) npmTarball(w http.ResponseWriter, r *http.Request, who Actor, owner store.Owner, name, file string) {
	ctx := r.Context()
	p, ok := a.npmLoad(w, r, who, owner, name)
	if !ok {
		return
	}
	file = path.Base(file)
	ver, ok := strings.CutPrefix(strings.TrimSuffix(file, ".tgz"), p.Name+"-")
	if !ok || !strings.HasSuffix(file, ".tgz") {
		npmError(w, http.StatusNotFound, "tarball not found")
		return
	}
	v, err := a.db.Version(ctx, p.ID, ver)
	if err != nil {
		npmError(w, http.StatusNotFound, "tarball not found")
		return
	}
	f, err := a.db.File(ctx, v.ID, file)
	if err != nil {
		npmError(w, http.StatusNotFound, "tarball not found")
		return
	}
	if r.Method == http.MethodGet {
		a.db.CountDownload(context.WithoutCancel(ctx), p.ID, v.ID, f.ID)
	}
	a.serveBlob(w, r, f.Digest, "application/octet-stream", file)
}

// serveBlob streams a blob (with Range support).
func (a *App) serveBlob(w http.ResponseWriter, r *http.Request, digest, contentType, name string) {
	fh, err := a.pkgBlobs().Open(digest)
	if err != nil {
		http.Error(w, "file is missing from storage", http.StatusNotFound)
		return
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("ETag", `"`+digest+`"`)
	h.Set("Cache-Control", "private, max-age=0, must-revalidate")
	if name != "" {
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	}
	http.ServeContent(w, r, "", st.ModTime(), fh)
}

// ---------- publish, deprecate, unpublish ----------

type npmAttachment struct {
	ContentType string `json:"content_type"`
	Data        string `json:"data"`
	Length      int64  `json:"length"`
}

type npmPublishBody struct {
	ID          string                     `json:"_id"`
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	DistTags    map[string]string          `json:"dist-tags"`
	Versions    map[string]json.RawMessage `json:"versions"`
	Access      *string                    `json:"access"`
	Attachments map[string]npmAttachment   `json:"_attachments"`
}

func (a *App) npmPut(w http.ResponseWriter, r *http.Request, who Actor, owner store.Owner, name string, rev bool) {
	ctx := r.Context()
	if who.Kind == ActorAnonymous {
		npmError(w, http.StatusUnauthorized, "authentication required: add an Atelier access token with packages write access to .npmrc")
		return
	}
	_, bare := npmScope(name)
	existing, err := a.db.Package(ctx, owner.ID, store.PkgNpm, bare)
	exists := err == nil
	private := true
	if exists {
		private = existing.Private
		if !a.canSeePackage(ctx, who, existing) {
			npmError(w, http.StatusNotFound, "package not found")
			return
		}
	}
	if !a.canPackage(ctx, who, owner, private, PermWrite) {
		npmError(w, http.StatusForbidden, "this token can't publish packages for "+owner.Name+" (it needs packages:write)")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pkgLimits.Npm)
	var body npmPublishBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			npmError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("package is larger than the %s limit", humanBytes(pkgLimits.Npm)))
			return
		}
		npmError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Name != "" && strings.ToLower(body.Name) != name {
		npmError(w, http.StatusBadRequest, "package name in the body doesn't match the URL")
		return
	}
	if len(body.Attachments) > 0 {
		a.npmPublish(w, r, who, owner, name, bare, existing, exists, body)
		return
	}
	if !exists {
		npmError(w, http.StatusNotFound, "package not found")
		return
	}
	a.npmUpdate(w, r, existing, body, rev)
}

func (a *App) npmPublish(w http.ResponseWriter, r *http.Request, who Actor, owner store.Owner, name, bare string,
	existing store.Package, exists bool, body npmPublishBody) {
	ctx := r.Context()
	// Work out what is new before writing anything.
	type pending struct {
		version string
		meta    map[string]any
		att     npmAttachment
	}
	var todo []pending
	for ver, raw := range body.Versions {
		if !semver.IsValid("v" + ver) {
			npmError(w, http.StatusBadRequest, "invalid version "+ver)
			return
		}
		if exists {
			if _, err := a.db.Version(ctx, existing.ID, ver); err == nil {
				npmError(w, http.StatusConflict, "cannot publish over the previously published version "+ver)
				return
			}
		}
		var meta map[string]any
		if err := json.Unmarshal(raw, &meta); err != nil {
			npmError(w, http.StatusBadRequest, "invalid version metadata")
			return
		}
		att, ok := npmFindAttachment(body.Attachments, bare, ver)
		if !ok {
			npmError(w, http.StatusBadRequest, "missing tarball for version "+ver)
			return
		}
		todo = append(todo, pending{ver, meta, att})
	}
	if len(todo) == 0 {
		npmError(w, http.StatusBadRequest, "nothing to publish")
		return
	}
	public := body.Access != nil && *body.Access == "public"
	p, created, err := a.db.EnsurePackage(ctx, owner.ID, store.PkgNpm, bare, public, pkgActorOf(who))
	if err != nil {
		logErr(r, err)
		npmError(w, http.StatusInternalServerError, "internal error")
		return
	}
	published := false
	defer func() {
		// A brand-new package whose first publish failed shouldn't linger empty.
		if created && !published {
			if cur, err := a.db.PackageByID(context.WithoutCancel(ctx), p.ID); err == nil && cur.Versions == 0 {
				_ = a.db.DeletePackage(context.WithoutCancel(ctx), p.ID)
			}
		}
	}()
	for _, t := range todo {
		data := base64.NewDecoder(base64.StdEncoding, strings.NewReader(t.att.Data))
		wr, err := a.pkgBlobs().Put(data, pkgLimits.Npm, "")
		if err != nil {
			if errors.Is(err, packages.ErrTooLarge) {
				npmError(w, http.StatusRequestEntityTooLarge, "tarball is too large")
				return
			}
			npmError(w, http.StatusBadRequest, "invalid tarball data")
			return
		}
		dist, _ := t.meta["dist"].(map[string]any)
		if dist == nil {
			dist = map[string]any{}
		}
		if s, _ := dist["shasum"].(string); s != "" && !strings.EqualFold(s, wr.SHA1) {
			npmError(w, http.StatusBadRequest, "tarball doesn't match its shasum")
			return
		}
		integrity := "sha512-" + base64.StdEncoding.EncodeToString(wr.SHA512)
		if s, _ := dist["integrity"].(string); s != "" && !npmIntegrityOK(s, wr) {
			npmError(w, http.StatusBadRequest, "tarball doesn't match its integrity")
			return
		}
		dist["shasum"], dist["integrity"] = wr.SHA1, integrity
		delete(dist, "tarball") // rebuilt from the public URL on every read
		t.meta["dist"] = dist
		meta, _ := json.Marshal(t.meta)
		var tags []string
		for tag, v := range body.DistTags {
			if v == t.version {
				tags = append(tags, tag)
			}
		}
		file := npmTarballName(bare, t.version)
		if _, err := a.db.AddVersion(ctx, p.ID, store.NewVersion{
			Version: t.version, Metadata: string(meta), MediaType: "application/gzip", Size: wr.Size, Tags: tags,
			Files: []store.PackageFile{{Name: file, Digest: wr.Digest, Size: wr.Size, MediaType: "application/gzip"}},
		}, pkgActorOf(who)); err != nil {
			if errors.Is(err, store.ErrExists) {
				npmError(w, http.StatusConflict, "cannot publish over the previously published version "+t.version)
				return
			}
			logErr(r, err)
			npmError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if desc := npmString(t.meta["description"]); desc != "" && body.Description == "" {
			body.Description = desc
		}
		published = true
		a.npmAutoLink(ctx, r, p, t.meta)
		a.emitPackage(ctx, p, who, name+"@"+t.version)
	}
	if body.Description != "" {
		_ = a.db.SetPackageDescription(ctx, p.ID, eventExcerpt(body.Description, 300))
	}
	p, _ = a.db.PackageByID(ctx, p.ID)
	npmJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": name, "rev": npmRev(p)})
}

func npmString(v any) string {
	s, _ := v.(string)
	return s
}

// npmFindAttachment picks the tarball for a version: "@scope/name-1.0.0.tgz",
// "name-1.0.0.tgz", or the only attachment there is.
func npmFindAttachment(atts map[string]npmAttachment, bare, ver string) (npmAttachment, bool) {
	want := npmTarballName(bare, ver)
	for k, v := range atts {
		if path.Base(k) == want {
			return v, true
		}
	}
	if len(atts) == 1 {
		for _, v := range atts {
			return v, true
		}
	}
	return npmAttachment{}, false
}

// npmIntegrityOK checks an SRI string ("sha512-… sha1-…") against the content.
func npmIntegrityOK(sri string, wr packages.Written) bool {
	matched := false
	for _, part := range strings.Fields(sri) {
		alg, b64, ok := strings.Cut(part, "-")
		if !ok {
			continue
		}
		if i := strings.IndexByte(b64, '?'); i >= 0 {
			b64 = b64[:i]
		}
		sum, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return false
		}
		switch alg {
		case "sha512":
			if !bytes.Equal(sum, wr.SHA512) {
				return false
			}
			matched = true
		case "sha1":
			if hex.EncodeToString(sum) != wr.SHA1 {
				return false
			}
			matched = true
		}
	}
	return matched
}

// npmAutoLink links a new package to its repository when package.json's
// repository points at this Atelier.
func (a *App) npmAutoLink(ctx context.Context, r *http.Request, p store.Package, meta map[string]any) {
	if p.RepoID != 0 {
		return
	}
	var u string
	switch v := meta["repository"].(type) {
	case string:
		u = v
	case map[string]any:
		u = npmString(v["url"])
	}
	u = strings.TrimPrefix(u, "git+")
	base := a.baseURL(r) + "/"
	rest, ok := strings.CutPrefix(u, base)
	if !ok {
		return
	}
	owner, name, ok := strings.Cut(strings.TrimSuffix(strings.Trim(rest, "/"), ".git"), "/")
	if !ok || !strings.EqualFold(owner, p.Owner) {
		return
	}
	if repo, err := a.db.RepoByName(ctx, owner, name); err == nil {
		_ = a.db.UpdatePackage(ctx, p.ID, p.Private, p.Description, repo.ID)
	}
}

// npmUpdate applies a packument without tarballs: deprecations, dist-tags
// and, through /-rev/, removal of versions (npm unpublish name@version).
func (a *App) npmUpdate(w http.ResponseWriter, r *http.Request, p store.Package, body npmPublishBody, rev bool) {
	ctx := r.Context()
	vers, err := a.db.Versions(ctx, p.ID)
	if err != nil {
		npmError(w, http.StatusInternalServerError, "internal error")
		return
	}
	left := 0
	for _, v := range vers {
		raw, ok := body.Versions[v.Version]
		if !ok {
			if rev && body.Versions != nil {
				if err := a.db.DeleteVersion(ctx, p.ID, v.Version); err != nil {
					npmError(w, http.StatusInternalServerError, "internal error")
					return
				}
				continue
			}
			left++
			continue
		}
		left++
		var m struct {
			Deprecated *string `json:"deprecated"`
		}
		_ = json.Unmarshal(raw, &m)
		msg := ""
		if m.Deprecated != nil {
			msg = *m.Deprecated
		}
		if msg != v.Deprecated {
			_ = a.db.SetDeprecated(ctx, v.ID, msg)
		}
	}
	if left == 0 {
		_ = a.db.DeletePackage(ctx, p.ID)
		npmJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if body.DistTags != nil {
		current, _ := a.db.Tags(ctx, p.ID)
		for tag, ver := range body.DistTags {
			if v, err := a.db.Version(ctx, p.ID, ver); err == nil && current[tag] != v.ID {
				_ = a.db.SetTag(ctx, p.ID, tag, v.ID)
			}
		}
		for tag := range current {
			if _, ok := body.DistTags[tag]; !ok && tag != "latest" {
				_ = a.db.DeleteTag(ctx, p.ID, tag)
			}
		}
	}
	a.npmFixLatest(ctx, p.ID)
	_ = a.db.TouchPackage(ctx, p.ID)
	p, _ = a.db.PackageByID(ctx, p.ID)
	npmJSON(w, http.StatusOK, map[string]any{"ok": true, "id": npmFullName(p), "rev": npmRev(p)})
}

// npmFixLatest keeps a "latest" tag pointing at a version, after unpublishes.
func (a *App) npmFixLatest(ctx context.Context, pkgID int64) {
	tags, _ := a.db.Tags(ctx, pkgID)
	if _, ok := tags["latest"]; ok {
		return
	}
	vers, _ := a.db.Versions(ctx, pkgID)
	sort.Slice(vers, func(i, j int) bool { return semver.Compare("v"+vers[i].Version, "v"+vers[j].Version) > 0 })
	for _, v := range vers {
		if semver.Prerelease("v"+v.Version) == "" {
			_ = a.db.SetTag(ctx, pkgID, "latest", v.ID)
			return
		}
	}
	if len(vers) > 0 {
		_ = a.db.SetTag(ctx, pkgID, "latest", vers[0].ID)
	}
}

func (a *App) npmDeletePackage(w http.ResponseWriter, r *http.Request, who Actor, owner store.Owner, name string) {
	ctx := r.Context()
	p, ok := a.npmLoad(w, r, who, owner, name)
	if !ok {
		return
	}
	if !a.canPackage(ctx, who, owner, p.Private, PermWrite) {
		npmError(w, http.StatusForbidden, "this token can't unpublish packages (it needs packages:write)")
		return
	}
	if err := a.db.DeletePackage(ctx, p.ID); err != nil {
		npmError(w, http.StatusInternalServerError, "internal error")
		return
	}
	npmJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// npmDeleteTarball is the last step of `npm unpublish name@version`; the
// version itself is normally gone already.
func (a *App) npmDeleteTarball(w http.ResponseWriter, r *http.Request, who Actor, owner store.Owner, name, file string) {
	ctx := r.Context()
	p, ok := a.npmLoad(w, r, who, owner, name)
	if !ok {
		// Unpublishing the last version deletes the package; its tarball goes with it.
		if who.Kind != ActorAnonymous {
			npmJSON(w, http.StatusOK, map[string]any{"ok": true})
		}
		return
	}
	if !a.canPackage(ctx, who, owner, p.Private, PermWrite) {
		npmError(w, http.StatusForbidden, "this token can't unpublish packages (it needs packages:write)")
		return
	}
	ver, ok := strings.CutPrefix(strings.TrimSuffix(path.Base(file), ".tgz"), p.Name+"-")
	if ok {
		if err := a.db.DeleteVersion(ctx, p.ID, ver); err == nil {
			a.npmFixLatest(ctx, p.ID)
		}
	}
	npmJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- dist-tags ----------

// npmDistTags: GET -/package/<name>/dist-tags, PUT/DELETE …/dist-tags/<tag>.
func (a *App) npmDistTags(w http.ResponseWriter, r *http.Request, who Actor, rest string) {
	ctx := r.Context()
	name, tail, err := npmPath(rest)
	if err != nil || len(tail) == 0 || tail[0] != "dist-tags" {
		npmError(w, http.StatusNotFound, "not found")
		return
	}
	scope, _ := npmScope(name)
	owner, err := a.db.OwnerByName(ctx, scope)
	if scope == "" || err != nil {
		npmError(w, http.StatusNotFound, "package not found")
		return
	}
	p, ok := a.npmLoad(w, r, who, owner, name)
	if !ok {
		return
	}
	if r.Method == http.MethodGet && len(tail) == 1 {
		vers, _ := a.db.Versions(ctx, p.ID)
		out := map[string]string{}
		for _, v := range vers {
			for _, t := range v.Tags {
				out[t] = v.Version
			}
		}
		npmJSON(w, http.StatusOK, out)
		return
	}
	if len(tail) != 2 || (r.Method != http.MethodPut && r.Method != http.MethodPost && r.Method != http.MethodDelete) {
		npmError(w, http.StatusNotFound, "not found")
		return
	}
	if !a.canPackage(ctx, who, owner, p.Private, PermWrite) {
		if who.Kind == ActorAnonymous {
			npmError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		npmError(w, http.StatusForbidden, "this token can't change dist-tags (it needs packages:write)")
		return
	}
	tag := tail[1]
	if r.Method == http.MethodDelete {
		if tag == "latest" {
			npmError(w, http.StatusBadRequest, "the latest tag can't be removed")
			return
		}
		if err := a.db.DeleteTag(ctx, p.ID, tag); err != nil {
			npmError(w, http.StatusNotFound, "no such tag")
			return
		}
		npmJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var ver string
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&ver); err != nil || semver.IsValid("v"+tag) {
		npmError(w, http.StatusBadRequest, "send the version as a JSON string; tags can't look like versions")
		return
	}
	v, err := a.db.Version(ctx, p.ID, ver)
	if err != nil {
		npmError(w, http.StatusNotFound, "version not found")
		return
	}
	if err := a.db.SetTag(ctx, p.ID, tag, v.ID); err != nil {
		npmError(w, http.StatusInternalServerError, "internal error")
		return
	}
	npmJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

// ---------- upstream cache ----------

// npmUpstreamAllowed: only people (or their tokens with packages access) may
// use the upstream cache, so Atelier isn't an open proxy.
func (a *App) npmUpstreamAllowed(who Actor) bool {
	if !isPerson(who) {
		return false
	}
	if who.Kind == ActorToken {
		return who.hasScope(ScopePackagesRead) || who.hasScope(ScopePackagesWrite)
	}
	return true
}

// npmUpstreamDoc returns the upstream packument (cached for 10 minutes; a
// stale copy is used when the upstream can't be reached).
func (a *App) npmUpstreamDoc(ctx context.Context, upstream, name string) ([]byte, error) {
	body, at, err := a.db.NpmCached(ctx, name)
	if err == nil && time.Since(at) < npmCacheTTL {
		return body, nil
	}
	fresh, ferr := npmFetchDoc(ctx, upstream, name)
	if ferr == nil {
		_ = a.db.SetNpmCached(context.WithoutCancel(ctx), name, fresh)
		return fresh, nil
	}
	if err == nil {
		return body, nil // stale but better than nothing (offline)
	}
	return nil, ferr
}

var errUpstreamNotFound = errors.New("not found upstream")

func npmFetchDoc(ctx context.Context, upstream, name string) ([]byte, error) {
	esc := url.PathEscape(name) // "@scope/name" → "@scope%2Fname"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream+"/"+esc, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := npmUpstreamClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errUpstreamNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream answered %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, errors.New("upstream sent invalid JSON")
	}
	return b, nil
}

func (a *App) npmUpstreamPackument(w http.ResponseWriter, r *http.Request, who Actor, name string) {
	ctx := r.Context()
	upstream, on := a.npmUpstream(ctx)
	if !on || !a.npmUpstreamAllowed(who) {
		npmError(w, http.StatusNotFound, "package not found")
		return
	}
	body, err := a.npmUpstreamDoc(ctx, upstream, name)
	if err != nil {
		if errors.Is(err, errUpstreamNotFound) {
			npmError(w, http.StatusNotFound, "package not found")
			return
		}
		npmError(w, http.StatusBadGateway, "upstream registry: "+err.Error())
		return
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		npmError(w, http.StatusBadGateway, "upstream sent invalid JSON")
		return
	}
	base := a.baseURL(r) + "/npm/" + name + "/-/"
	if vs, ok := doc["versions"].(map[string]any); ok {
		for _, v := range vs {
			m, _ := v.(map[string]any)
			if m == nil {
				continue
			}
			if dist, ok := m["dist"].(map[string]any); ok {
				if t := npmString(dist["tarball"]); t != "" {
					dist["tarball"] = base + path.Base(t)
				}
			}
		}
	}
	npmJSON(w, http.StatusOK, doc)
}

func (a *App) npmUpstreamTarball(w http.ResponseWriter, r *http.Request, who Actor, name, file string) {
	ctx := r.Context()
	upstream, on := a.npmUpstream(ctx)
	file = path.Base(file)
	if !a.npmUpstreamAllowed(who) {
		npmError(w, http.StatusNotFound, "tarball not found")
		return
	}
	if f, err := a.db.NpmCachedFile(ctx, name, file); err == nil {
		if _, ok := a.pkgBlobs().Stat(f.Digest); ok {
			a.serveBlob(w, r, f.Digest, "application/octet-stream", file)
			return
		}
	}
	if !on {
		npmError(w, http.StatusNotFound, "tarball not found")
		return
	}
	body, err := a.npmUpstreamDoc(ctx, upstream, name)
	if err != nil {
		npmError(w, http.StatusNotFound, "tarball not found")
		return
	}
	var doc struct {
		Versions map[string]struct {
			Dist struct {
				Tarball   string `json:"tarball"`
				Shasum    string `json:"shasum"`
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		npmError(w, http.StatusBadGateway, "upstream sent invalid JSON")
		return
	}
	var src, shasum, integrity string
	for _, v := range doc.Versions {
		if path.Base(v.Dist.Tarball) == file {
			src, shasum, integrity = v.Dist.Tarball, v.Dist.Shasum, v.Dist.Integrity
			break
		}
	}
	su, err1 := url.Parse(src)
	uu, err2 := url.Parse(upstream)
	if src == "" || err1 != nil || err2 != nil || !strings.EqualFold(su.Host, uu.Host) || (su.Scheme != "https" && su.Scheme != "http") {
		npmError(w, http.StatusNotFound, "tarball not found")
		return
	}
	if integrity == "" && shasum == "" {
		npmError(w, http.StatusBadGateway, "upstream gave no checksum for this tarball")
		return
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	resp, err := npmUpstreamClient.Do(req)
	if err != nil {
		npmError(w, http.StatusBadGateway, "upstream registry: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		npmError(w, http.StatusBadGateway, "upstream answered "+resp.Status)
		return
	}
	wr, err := a.pkgBlobs().Put(resp.Body, pkgLimits.Blob, "")
	if err != nil {
		npmError(w, http.StatusBadGateway, "couldn't store the upstream tarball")
		return
	}
	ok := false
	if integrity != "" {
		ok = npmIntegrityOK(integrity, wr)
	} else {
		ok = strings.EqualFold(shasum, wr.SHA1)
	}
	if !ok {
		npmError(w, http.StatusBadGateway, "upstream tarball failed its integrity check")
		return
	}
	if err := a.db.SetNpmCachedFile(context.WithoutCancel(ctx), name, file, wr.Digest, wr.Size); err != nil {
		logErr(r, err)
	}
	a.serveBlob(w, r, wr.Digest, "application/octet-stream", file)
}

// npmSRI returns the sha512 SRI string for content (used by tests and the UI).
func npmSRI(b []byte) string {
	s := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}

func npmSHA1(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}
