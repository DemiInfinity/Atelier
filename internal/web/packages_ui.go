package web

// Packages UI:
//
//	/packages                          every package you can see (filter by ?type=, ?q=)
//	/packages/{owner}                  one account's packages
//	/packages/{owner}/{type}/{name}    a package: versions, install snippet, settings
//	/settings/packages                 npm upstream cache, storage, clean-up

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"atelier/internal/store"
)

func init() {
	registerPathHandler(func(a *App, w http.ResponseWriter, r *http.Request) bool { return a.servePackages(w, r) })
}

// pkgUIHeaders sets the headers ServeHTTP gives UI pages (the packages path
// handler runs before it sets them).
func pkgUIHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' https://fonts.googleapis.com; "+
		"font-src https://fonts.gstatic.com; img-src 'self' data: https://avatars.githubusercontent.com; "+
		"script-src 'self'; form-action 'self' https://github.com; frame-ancestors 'none'; base-uri 'none'")
}

// pkgPageURL is a package's page.
func pkgPageURL(p store.Package) string {
	return "/packages/" + url.PathEscape(strings.ToLower(p.Owner)) + "/" + p.Type + "/" + p.Name
}

// pkgHost is the registry host people type: git.example.com (with a port if any).
func (a *App) pkgHost(r *http.Request) string {
	b := a.baseURL(r)
	b = strings.TrimPrefix(strings.TrimPrefix(b, "https://"), "http://")
	return strings.TrimRight(b, "/")
}

// pkgTypeLabel names a type for people.
func pkgTypeLabel(t string) string {
	switch t {
	case store.PkgNpm:
		return "npm"
	case store.PkgContainer:
		return "Container"
	case store.PkgGo:
		return "Go"
	case store.PkgGeneric:
		return "Generic"
	}
	return t
}

// pkgRow is one line in a package list.
type pkgRow struct {
	Type, TypeLabel string
	Owner           string
	OwnerKind       string
	Name            string // stored name
	Display         string // "@demi/tool", "demi/atelier", "git.example.com/demi/lib"
	Version         string
	Repo            string
	Private         bool
	Downloads       int64
	Versions        int
	Size            int64
	Updated         time.Time
	URL             string
	Description     string
}

func (a *App) pkgDisplay(r *http.Request, p store.Package) string {
	switch p.Type {
	case store.PkgNpm:
		return npmFullName(p)
	case store.PkgGo:
		return a.pkgHost(r) + "/" + strings.ToLower(p.Owner) + "/" + p.Name
	}
	return strings.ToLower(p.Owner) + "/" + p.Name
}

// pkgRows lists what who may see: stored packages plus Go modules (repositories
// with semver tags). owner limits it to one account (nil = all).
func (a *App) pkgRows(ctx context.Context, r *http.Request, who Actor, owner *store.Owner) ([]pkgRow, error) {
	pkgs, err := a.db.Packages(ctx)
	if err != nil {
		return nil, err
	}
	var rows []pkgRow
	goDownloads := map[string]store.Package{}
	for _, p := range pkgs {
		if owner != nil && p.OwnerID != owner.ID {
			continue
		}
		if !a.canSeePackage(ctx, who, p) {
			continue
		}
		if p.Type == store.PkgGo {
			goDownloads[strings.ToLower(p.Owner+"/"+p.Name)] = p
			continue
		}
		version := p.Latest
		if p.Type == store.PkgContainer {
			version = a.ociLatestTag(ctx, p)
		}
		rows = append(rows, pkgRow{
			Type: p.Type, TypeLabel: pkgTypeLabel(p.Type), Owner: p.Owner, OwnerKind: p.OwnerKind, Name: p.Name,
			Display: a.pkgDisplay(r, p), Version: version, Repo: p.Repo, Private: p.Private, Downloads: p.Downloads,
			Versions: p.Versions, Size: p.Size, Updated: p.UpdatedAt, URL: pkgPageURL(p), Description: p.Description,
		})
	}
	// Go modules come straight from repositories.
	repos, err := a.db.Repos(ctx)
	if err != nil {
		return nil, err
	}
	for _, rp := range repos {
		if owner != nil && rp.OwnerID != owner.ID {
			continue
		}
		if !a.can(ctx, who, rp, PermRead) {
			continue
		}
		g, err := a.git.Open(rp.Owner, rp.Name)
		if err != nil {
			continue
		}
		m := goModule{repo: rp, git: g}
		vers := a.goVersions(ctx, m)
		if len(vers) == 0 {
			continue
		}
		name := strings.ToLower(rp.Name)
		dp := goDownloads[strings.ToLower(rp.Owner+"/"+name)]
		rows = append(rows, pkgRow{
			Type: store.PkgGo, TypeLabel: "Go", Owner: rp.Owner, OwnerKind: rp.OwnerKind, Name: name,
			Display: a.pkgHost(r) + "/" + strings.ToLower(rp.Owner) + "/" + name, Version: vers[0], Repo: rp.FullName(),
			Private: rp.Private, Downloads: dp.Downloads, Versions: len(vers), Updated: rp.UpdatedAt,
			URL: "/packages/" + strings.ToLower(rp.Owner) + "/go/" + name, Description: rp.Description,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Updated.After(rows[j].Updated) })
	return rows, nil
}

// ociLatestTag picks the tag to show for an image: "latest", else the most
// recently updated tag, else the newest digest.
func (a *App) ociLatestTag(ctx context.Context, p store.Package) string {
	tags, _ := a.db.TagNames(ctx, p.ID)
	for _, t := range tags {
		if t == "latest" {
			return t
		}
	}
	if len(tags) > 0 {
		vers, _ := a.db.Versions(ctx, p.ID)
		for _, v := range vers {
			if len(v.Tags) > 0 {
				return v.Tags[0]
			}
		}
		return tags[0]
	}
	if len(p.Latest) > 19 {
		return p.Latest[:19]
	}
	return p.Latest
}

// packagesList is /packages (all accounts) and /packages/{owner}.
func (a *App) packagesList(w http.ResponseWriter, r *http.Request) {
	a.renderPackagesList(w, r, nil)
}

func (a *App) renderPackagesList(w http.ResponseWriter, r *http.Request, owner *store.Owner) {
	ctx := r.Context()
	who := a.actor(r)
	all, err := a.pkgRows(ctx, r, who, owner)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	typ := r.URL.Query().Get("type")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	counts := map[string]int{}
	var rows []pkgRow
	for _, row := range all {
		counts[row.Type]++
		if typ != "" && row.Type != typ {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(row.Display+" "+row.Description), q) {
			continue
		}
		rows = append(rows, row)
	}
	type typeCount struct {
		Key, Label string
		Count      int
		URL        string
		Active     bool
	}
	base := "/packages"
	if owner != nil {
		base += "/" + url.PathEscape(strings.ToLower(owner.Name))
	}
	withQ := func(t string) string {
		v := url.Values{}
		if t != "" {
			v.Set("type", t)
		}
		if q != "" {
			v.Set("q", q)
		}
		if len(v) == 0 {
			return base
		}
		return base + "?" + v.Encode()
	}
	types := []typeCount{{Key: "", Label: "All packages", Count: len(all), URL: withQ(""), Active: typ == ""}}
	for _, t := range store.PackageTypes {
		types = append(types, typeCount{Key: t, Label: pkgTypeLabel(t), Count: counts[t], URL: withQ(t), Active: typ == t})
	}
	owners, _ := a.db.Owners(ctx)
	upstreamURL, upstream := a.npmUpstream(ctx)
	title := "Packages"
	if owner != nil {
		title = "Packages · " + owner.Label()
	}
	a.render(w, r, "packages_list", page{Title: title, Nav: "packages", Data: map[string]any{
		"Rows": rows, "Types": types, "Type": typ, "Q": q, "Owner": owner, "Owners": owners, "Base": base,
		"Registry": a.baseURL(r) + "/npm/", "RegistryHost": a.pkgHost(r), "BaseURL": a.baseURL(r),
		"Upstream": upstream, "UpstreamURL": upstreamURL, "HTTPS": a.dockerReachable(r), "Total": len(all),
	}})
}

// servePackagesUI handles /packages/… pages (the main router can't hold
// /packages/{owner}/{type}/{name...} next to the repository routes).
func (a *App) servePackagesUI(w http.ResponseWriter, r *http.Request) {
	pkgUIHeaders(w)
	if r.Method == http.MethodPost && !a.sameOrigin(r) {
		http.Error(w, "cross-site request blocked", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.authed(a.packagesPage)(w, r)
}

func (a *App) packagesPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/packages/"), "/")
	segs := strings.SplitN(rest, "/", 3)
	owner, err := a.db.OwnerByName(ctx, segs[0])
	if err != nil {
		a.notFound(w, r)
		return
	}
	if len(segs) == 1 {
		if r.Method == http.MethodPost {
			a.notFound(w, r)
			return
		}
		a.renderPackagesList(w, r, &owner)
		return
	}
	if len(segs) != 3 {
		a.notFound(w, r)
		return
	}
	typ, name := segs[1], strings.ToLower(segs[2])
	if typ == store.PkgGo {
		a.goPackagePage(w, r, owner, name)
		return
	}
	p, err := a.db.Package(ctx, owner.ID, typ, name)
	who := a.actor(r)
	if err != nil || !a.canSeePackage(ctx, who, p) {
		a.notFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		a.packagesPost(w, r, owner, p)
		return
	}
	a.renderPackagePage(w, r, owner, p, "")
}

// pkgVersionView is a version row on a package page.
type pkgVersionView struct {
	store.PackageVersion
	Label     string // version, or short digest
	Files     []store.PackageFile
	Platforms []string
	Pull      string // install / pull reference
	FileBase  string // generic: download URL prefix
}

func (a *App) renderPackagePage(w http.ResponseWriter, r *http.Request, owner store.Owner, p store.Package, errMsg string) {
	ctx := r.Context()
	who := a.actor(r)
	vers, err := a.db.Versions(ctx, p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	host := a.pkgHost(r)
	display := a.pkgDisplay(r, p)
	// Container: manifests that only exist as part of an index are shown as
	// that index's platforms, not as versions of their own.
	children := map[string]bool{}
	if p.Type == store.PkgContainer {
		for _, v := range vers {
			if strings.Contains(v.MediaType, "index") || strings.Contains(v.MediaType, "list") {
				var m ociManifest
				_ = jsonUnmarshalString(v.Metadata, &m)
				for _, c := range m.Manifests {
					children[c.Digest] = true
				}
			}
		}
	}
	var views []pkgVersionView
	for _, v := range vers {
		if children[v.Version] && len(v.Tags) == 0 {
			continue
		}
		vv := pkgVersionView{PackageVersion: v, Label: v.Version}
		switch p.Type {
		case store.PkgNpm:
			vv.Pull = "npm install " + display + "@" + v.Version
		case store.PkgContainer:
			vv.Label = v.Version
			if len(vv.Label) > 19 {
				vv.Label = vv.Label[:19]
			}
			ref := "@" + v.Version
			if len(v.Tags) > 0 {
				ref = ":" + v.Tags[0]
			}
			vv.Pull = "docker pull " + host + "/" + display + ref
			var m ociManifest
			_ = jsonUnmarshalString(v.Metadata, &m)
			for _, c := range m.Manifests {
				if c.Platform != nil {
					pl := strings.Trim(stringOf(c.Platform["os"])+"/"+stringOf(c.Platform["architecture"])+"/"+stringOf(c.Platform["variant"]), "/")
					if pl != "unknown/unknown" && pl != "" {
						vv.Platforms = append(vv.Platforms, pl)
					}
				}
			}
		case store.PkgGeneric:
			vv.Files, _ = a.db.Files(ctx, v.ID)
			vv.FileBase = "/api/v1/packages/" + url.PathEscape(strings.ToLower(owner.Name)) + "/generic/" + p.Name + "/" + v.Version + "/"
		}
		views = append(views, vv)
	}
	repos, _ := a.db.ReposByOwner(ctx, owner.ID)
	latest := ""
	if len(views) > 0 {
		latest = views[0].Version
		if p.Type == store.PkgContainer {
			latest = a.ociLatestTag(ctx, p)
		}
	}
	a.render(w, r, "packages_view", page{Title: display + " · Packages", Nav: "packages", Error: errMsg,
		Notice: r.URL.Query().Get("saved"), Data: map[string]any{
			"P": p, "Owner": owner, "Display": display, "TypeLabel": pkgTypeLabel(p.Type), "Versions": views,
			"Latest": latest, "Host": host, "BaseURL": a.baseURL(r), "Repos": repos, "HTTPS": a.dockerReachable(r),
			"CanAdmin": a.canPackage(ctx, who, owner, p.Private, PermAdmin), "URL": pkgPageURL(p),
			"Registry": a.baseURL(r) + "/npm/", "Scope": strings.ToLower(owner.Name),
		}})
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// packagesPost: delete a version, delete the package, or change settings.
// Everything here needs admin on the package.
func (a *App) packagesPost(w http.ResponseWriter, r *http.Request, owner store.Owner, p store.Package) {
	ctx := r.Context()
	if !a.canPackage(ctx, a.actor(r), owner, p.Private, PermAdmin) {
		a.forbidden(w, r)
		return
	}
	switch r.FormValue("action") {
	case "delete-version":
		v := r.FormValue("version")
		if err := a.db.DeleteVersion(ctx, p.ID, v); err != nil {
			a.renderPackagePage(w, r, owner, p, "That version doesn't exist any more.")
			return
		}
		if p.Type == store.PkgNpm {
			a.npmFixLatest(ctx, p.ID)
		}
		if p, err := a.db.PackageByID(ctx, p.ID); err == nil && p.Versions == 0 {
			_ = a.db.DeletePackage(ctx, p.ID)
			http.Redirect(w, r, "/packages/"+url.PathEscape(strings.ToLower(owner.Name)), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, pkgPageURL(p)+"?saved=1", http.StatusSeeOther)
	case "delete-package":
		if strings.TrimSpace(r.FormValue("confirm")) != p.Name {
			a.renderPackagePage(w, r, owner, p, "Type the package name to confirm deletion.")
			return
		}
		if err := a.db.DeletePackage(ctx, p.ID); err != nil {
			a.fail(w, r, err)
			return
		}
		http.Redirect(w, r, "/packages/"+url.PathEscape(strings.ToLower(owner.Name)), http.StatusSeeOther)
	case "settings":
		private := r.FormValue("visibility") != "public"
		var repoID int64
		if name := r.FormValue("repo"); name != "" {
			repo, err := a.db.RepoByName(ctx, owner.Name, name)
			if err != nil || !a.can(ctx, a.actor(r), repo, PermRead) {
				a.renderPackagePage(w, r, owner, p, "Pick one of "+owner.Name+"'s repositories.")
				return
			}
			repoID = repo.ID
		}
		desc := strings.TrimSpace(r.FormValue("description"))
		if len(desc) > 300 {
			desc = desc[:300]
		}
		if err := a.db.UpdatePackage(ctx, p.ID, private, desc, repoID); err != nil {
			a.fail(w, r, err)
			return
		}
		http.Redirect(w, r, pkgPageURL(p)+"?saved=1", http.StatusSeeOther)
	default:
		a.notFound(w, r)
	}
}

// goPackagePage shows a Go module (a repository with semver tags).
func (a *App) goPackagePage(w http.ResponseWriter, r *http.Request, owner store.Owner, name string) {
	ctx := r.Context()
	if r.Method == http.MethodPost {
		a.notFound(w, r)
		return
	}
	base, major, _ := strings.Cut(name, "/")
	repo, err := a.db.RepoByName(ctx, owner.Name, base)
	who := a.actor(r)
	if err != nil || !a.can(ctx, who, repo, PermRead) {
		a.notFound(w, r)
		return
	}
	g, err := a.git.Open(repo.Owner, repo.Name)
	if err != nil {
		a.notFound(w, r)
		return
	}
	m := goModule{repo: repo, git: g, major: major}
	if major != "" && !majorSuffixRE.MatchString(major) {
		a.notFound(w, r)
		return
	}
	modPath := a.pkgHost(r) + "/" + strings.ToLower(owner.Name) + "/" + strings.ToLower(repo.Name)
	if major != "" {
		modPath += "/" + major
	}
	m.path = modPath
	downloads := map[string]int64{}
	var total int64
	if p, err := a.db.Package(ctx, owner.ID, store.PkgGo, name); err == nil {
		total = p.Downloads
		if vs, err := a.db.Versions(ctx, p.ID); err == nil {
			for _, v := range vs {
				downloads[v.Version] = v.Downloads
			}
		}
	}
	type goVer struct {
		Version   string
		When      time.Time
		Downloads int64
		Pre       bool
	}
	var vers []goVer
	for _, v := range a.goVersions(ctx, m) {
		gv := goVer{Version: v, Downloads: downloads[v], Pre: semver.Prerelease(v) != ""}
		if c, ok := a.goCommit(ctx, m, v); ok {
			if lc := g.LastCommit(ctx, c, ""); lc != nil {
				gv.When = lc.When
			}
		}
		vers = append(vers, gv)
	}
	if len(vers) == 0 {
		a.notFound(w, r)
		return
	}
	a.render(w, r, "packages_go", page{Title: modPath + " · Packages", Nav: "packages", Data: map[string]any{
		"Owner": owner, "Repo": repo, "Module": modPath, "Versions": vers, "Latest": vers[0].Version,
		"Downloads": total, "Host": a.pkgHost(r), "BaseURL": a.baseURL(r),
	}})
}

// dockerReachable reports whether Docker will talk to this registry without
// an insecure-registries entry: over HTTPS, or on localhost.
func (a *App) dockerReachable(r *http.Request) bool {
	if a.isHTTPS(r) {
		return true
	}
	host := a.pkgHost(r)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return host == "localhost" || strings.HasPrefix(host, "127.") || host == "[::1]" || host == "::1"
}
