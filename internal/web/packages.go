package web

// Packages: Atelier's package registries.
//
//	/npm/                         npm registry (packages_npm.go)
//	/v2/                          container registry, OCI Distribution v2 (packages_oci.go)
//	/go/                          Go module proxy for repositories (packages_goproxy.go)
//	/api/v1/packages/{owner}/generic/{name}/{version}/{file}   generic files (packages_generic.go)
//	/packages, /packages/{owner}/{type}/{name}                  UI (packages_ui.go)
//	/settings/packages            npm upstream setting
//
// Files are content-addressed blobs (internal/packages); metadata lives in the
// packages tables (internal/store/packages.go). A package belongs to an owner
// account. Access goes through a.can with a stand-in repository for the owner
// (see canPackage), so the Duo / Team / Company addons decide package access
// the same way they decide repository access. Access tokens need the
// packages:read / packages:write scopes; repository-only tokens are refused.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"atelier/internal/packages"
	"atelier/internal/store"
)

func init() {
	registerFeature(feature{
		name: "packages",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/packages", a.authed(a.packagesSettings))
			m.HandleFunc("POST /settings/packages", a.authed(a.packagesSettingsSubmit))
			m.HandleFunc("GET /packages", a.authed(a.packagesList))
		},
		start: func(ctx context.Context, a *App) {
			// Weekly garbage collection of blobs nothing refers to any more.
			t := time.NewTimer(10 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				if res, err := a.PackagesGC(ctx); err != nil {
					log.Printf("packages: gc: %v", err)
				} else if res.Removed > 0 || res.Uploads > 0 {
					log.Printf("packages: gc: %s", res)
				}
				t.Reset(7 * 24 * time.Hour)
			}
		},
	})
}

// Scopes for access tokens.
const (
	ScopePackagesRead  = "packages:read"
	ScopePackagesWrite = "packages:write"
)

// Size limits. ATELIER_PACKAGES_MAX_BYTES (e.g. "2G", "500M") overrides the
// per-blob limit for container layers and generic files.
var pkgLimits = struct {
	Blob     int64 // one container blob or generic file
	Npm      int64 // one npm publish request (JSON with base64 tarball)
	Manifest int64 // one container manifest
}{Blob: 10 << 30, Npm: 256 << 20, Manifest: 4 << 20}

func init() {
	if n := sizeFromEnvPkg("ATELIER_PACKAGES_MAX_BYTES"); n > 0 {
		pkgLimits.Blob = n
		if n < pkgLimits.Npm {
			pkgLimits.Npm = n
		}
	}
}

func sizeFromEnvPkg(key string) int64 {
	v := strings.ToUpper(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return 0
	}
	mult := int64(1)
	for suf, m := range map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40} {
		if strings.HasSuffix(v, suf) || strings.HasSuffix(v, suf+"B") {
			v = strings.TrimSuffix(strings.TrimSuffix(v, "B"), suf)
			mult = m
			break
		}
	}
	var n int64
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n * mult
}

// ---------- blob store ----------

var (
	pkgBlobsMu sync.Mutex
	pkgBlobsBy = map[string]*packages.Blobs{}
)

// pkgBlobs is the blob store under <data>/packages.
func (a *App) pkgBlobs() *packages.Blobs {
	root := filepath.Join(a.cfg.DataDir, "packages")
	pkgBlobsMu.Lock()
	defer pkgBlobsMu.Unlock()
	if b, ok := pkgBlobsBy[root]; ok {
		return b
	}
	b, err := packages.New(root)
	if err != nil {
		log.Printf("packages: %v", err)
		b = &packages.Blobs{Root: root}
	}
	pkgBlobsBy[root] = b
	return b
}

// PackagesGC removes blobs nothing refers to. Blobs written in the last hour
// are kept (their database rows may not exist yet), as are container blobs
// uploaded in the last day that no manifest uses yet.
func (a *App) PackagesGC(ctx context.Context) (packages.GCResult, error) {
	return PackagesGC(ctx, a.db, a.pkgBlobs())
}

// PackagesGC is the collection itself, shared with `atelier packages gc`.
func PackagesGC(ctx context.Context, db *store.Store, b *packages.Blobs) (packages.GCResult, error) {
	refs, err := db.ReferencedBlobs(ctx, 24*time.Hour)
	if err != nil {
		return packages.GCResult{}, err
	}
	return b.GC(func(d string) bool { return refs[d] }, time.Hour, 24*time.Hour)
}

// ---------- dispatch ----------

// servePackages handles the registry paths. It runs before the main router
// (see ServeHTTP): their URL shapes (/npm/@scope%2fname, /v2/owner/name/…)
// would clash with the repository routes. It reports whether it handled r.
func (a *App) servePackages(w http.ResponseWriter, r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/npm" || strings.HasPrefix(p, "/npm/"):
		a.serveNpm(w, r)
	case p == "/v2" || strings.HasPrefix(p, "/v2/"):
		a.serveOCI(w, r)
	case strings.HasPrefix(p, "/go/"):
		a.serveGoProxy(w, r)
	case strings.HasPrefix(p, "/api/v1/packages/"):
		a.serveGenericAPI(w, r)
	case strings.HasPrefix(p, "/packages/"):
		a.servePackagesUI(w, r)
	case r.URL.Query().Get("go-get") == "1" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		return a.serveGoGet(w, r)
	default:
		return false
	}
	return true
}

// ---------- who is calling ----------

// pkgActor works out who is calling a registry API: a signed-in browser (for
// downloads only), a Bearer token, or Basic auth with a token as the password
// (or username). ok=false means a credential was sent but isn't valid.
func (a *App) pkgActor(r *http.Request) (Actor, bool) {
	anon := Actor{Kind: ActorAnonymous, Name: "anonymous"}
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz == "" {
		// Cookies only for reads: a cross-site page must not be able to
		// publish or delete with someone's session.
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if who, ok := a.sessionActor(r); ok {
				return who, true
			}
		}
		return anon, true
	}
	ip := a.clientIP(r)
	if !a.lim.allow(ip) {
		return anon, false
	}
	var cands []string
	scheme, rest, _ := strings.Cut(authz, " ")
	switch strings.ToLower(scheme) {
	case "bearer", "token":
		cands = []string{strings.TrimSpace(rest)}
	case "basic":
		if u, p, ok := r.BasicAuth(); ok {
			cands = []string{p, u}
		}
	}
	for _, c := range cands {
		if c == "" {
			continue
		}
		if who, ok := a.resolveCredential(r.Context(), c); ok {
			a.lim.reset(ip)
			return who, true
		}
	}
	return anon, false
}

// pkgRepo is the stand-in repository a.can judges package access by: the
// owner's account, with the package's visibility.
func pkgRepo(o store.Owner, private bool) store.Repo {
	return store.Repo{OwnerID: o.ID, Owner: o.Name, OwnerKind: o.Kind, Private: private}
}

// canPackage reports whether who may do something needing perm to a package
// of owner o (private or public). Tokens need the packages scopes: they are
// translated to the repository scopes the access policies understand, so a
// token with only repo:* scopes gets nothing here.
func (a *App) canPackage(ctx context.Context, who Actor, o store.Owner, private bool, need Perm) bool {
	if who.Kind == ActorToken {
		read, write := who.hasScope(ScopePackagesRead), who.hasScope(ScopePackagesWrite)
		switch {
		case need == PermRead && !read && !write:
			return false
		case need >= PermWrite && !write:
			return false
		}
		var sc []string
		if read || write {
			sc = append(sc, "repo:read")
		}
		if write {
			sc = append(sc, "repo:write")
		}
		who.Scopes = sc
	}
	return a.can(ctx, who, pkgRepo(o, private), need)
}

// canSeePackage is canPackage(PermRead) for a stored package.
func (a *App) canSeePackage(ctx context.Context, who Actor, p store.Package) bool {
	o := store.Owner{ID: p.OwnerID, Name: p.Owner, Kind: p.OwnerKind}
	return a.canPackage(ctx, who, o, p.Private, PermRead)
}

func pkgActorOf(who Actor) store.PkgActor {
	return store.PkgActor{Kind: who.Kind, Name: who.Name, PersonID: who.PersonID}
}

// isPerson reports whether who is a known person (signed in, or their token).
func isPerson(who Actor) bool {
	return who.PersonID != 0 && who.Kind != ActorAnonymous
}

// ---------- settings ----------

const (
	settingNpmUpstream    = "packages.npm.upstream"     // "on" to proxy the public registry
	settingNpmUpstreamURL = "packages.npm.upstream_url" // default https://registry.npmjs.org
	defaultNpmUpstream    = "https://registry.npmjs.org"
)

func (a *App) npmUpstream(ctx context.Context) (string, bool) {
	on, _ := a.db.Setting(ctx, settingNpmUpstream)
	u, _ := a.db.Setting(ctx, settingNpmUpstreamURL)
	if u == "" {
		u = defaultNpmUpstream
	}
	return strings.TrimRight(u, "/"), on == "on"
}

func (a *App) packagesSettings(w http.ResponseWriter, r *http.Request) {
	a.renderPackagesSettings(w, r, r.URL.Query().Get("saved"), "", "")
}

func (a *App) renderPackagesSettings(w http.ResponseWriter, r *http.Request, notice, errMsg, gc string) {
	ctx := r.Context()
	up, on := a.npmUpstream(ctx)
	cachedPkgs, cachedFiles, cachedSize := a.db.NpmCacheStats(ctx)
	a.render(w, r, "packages_settings", page{Title: "Packages", Nav: "tokens", Notice: notice, Error: errMsg, Data: map[string]any{
		"Upstream": on, "UpstreamURL": up, "CachedPackages": cachedPkgs, "CachedFiles": cachedFiles, "CachedSize": cachedSize,
		"DiskUsage": a.pkgBlobs().DiskUsage(), "Registry": a.baseURL(r) + "/npm/",
		"MaxBlob": pkgLimits.Blob, "MaxNpm": pkgLimits.Npm, "GC": gc,
	}})
}

func (a *App) packagesSettingsSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	me, err := a.db.PersonalOwner(ctx)
	if err != nil || !a.canPackage(ctx, a.actor(r), me, true, PermAdmin) {
		a.forbidden(w, r)
		return
	}
	if r.FormValue("action") == "gc" {
		res, err := a.PackagesGC(ctx)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		log.Printf("packages: gc from settings: %s", res)
		a.renderPackagesSettings(w, r, "", "", res.String())
		return
	}
	on := ""
	if r.FormValue("upstream") == "on" {
		on = "on"
	}
	u := strings.TrimRight(strings.TrimSpace(r.FormValue("upstream_url")), "/")
	if u == "" {
		u = defaultNpmUpstream
	}
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		a.renderPackagesSettings(w, r, "", "The upstream registry must be an http(s) URL.", "")
		return
	}
	if err := errors.Join(a.db.SetSetting(ctx, settingNpmUpstream, on), a.db.SetSetting(ctx, settingNpmUpstreamURL, u)); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/packages?saved=1", http.StatusSeeOther)
}

// jsonUnmarshalString decodes JSON held in a string.
func jsonUnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// emitPackage announces a published version to the people who can see the
// package (not everyone who can read a linked repository: the package may be
// private while the repository is public).
func (a *App) emitPackage(ctx context.Context, p store.Package, who Actor, title string) {
	owners, err := a.db.Owners(ctx)
	if err != nil {
		return
	}
	var to []int64
	for _, o := range owners {
		if o.Kind == "user" && a.canSeePackage(ctx, Actor{Kind: ActorOwner, Name: o.Name, PersonID: o.ID}, p) {
			to = append(to, o.ID)
		}
	}
	if len(to) == 0 {
		return
	}
	a.emit(ctx, Event{Kind: EventPackagePublished, Title: title, URL: pkgPageURL(p), Actor: who,
		Detail: pkgTypeLabel(p.Type) + " package", Recipients: to})
}
