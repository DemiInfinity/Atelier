package web

// Go module proxy at /go/ (the GOPROXY protocol) for modules that live in
// Atelier repositories:
//
//	GOPROXY=https://git.example.com/go,https://proxy.golang.org,direct
//	GONOSUMDB=git.example.com   (private modules aren't in the public checksum database)
//
// A module path is <host>/<owner>/<repo>, or <host>/<owner>/<repo>/vN for
// major version N ≥ 2. Versions are the repository's semver tags (v1.2.3);
// branch names and commit hashes resolve to pseudo-versions. Zips are built
// from the repository at the tag following the module zip rules
// (golang.org/x/mod/zip). Modules in subdirectories aren't served.
//
// Private repositories need credentials: put an access token (packages:read or
// repo:read) in ~/.netrc for the host. The go command also sends ?go-get=1
// requests to repository pages; serveGoGet answers them with the go-import
// meta tag so `GOPRIVATE=git.example.com go get git.example.com/owner/repo`
// works over git as well.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	modzip "golang.org/x/mod/zip"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

var majorSuffixRE = regexp.MustCompile(`^v([2-9]|[1-9][0-9]+)$`)

// goModule is a resolved module path.
type goModule struct {
	path  string // full module path, e.g. git.example.com/demi/tool/v2
	repo  store.Repo
	git   *gitrepo.Repo
	major string // "" or "v2", "v3"…
}

// goReader is who as the module proxy sees them: tokens with packages access
// may read repositories' code here.
func goReader(who Actor) Actor {
	if who.Kind == ActorToken && (who.hasScope(ScopePackagesRead) || who.hasScope(ScopePackagesWrite)) && !who.hasScope("repo:read") {
		who.Scopes = append(append([]string{}, who.Scopes...), "repo:read")
	}
	return who
}

func goNotFound(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, msg+"\n")
}

func (a *App) serveGoProxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	who, ok := a.pkgActor(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="Atelier Go proxy"`)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/go/")
	var escMod, op string
	if i := strings.Index(rest, "/@v/"); i >= 0 {
		escMod, op = rest[:i], rest[i+4:]
	} else if m, ok := strings.CutSuffix(rest, "/@latest"); ok {
		escMod, op = m, "@latest"
	} else {
		goNotFound(w, "not found")
		return
	}
	modPath, err := module.UnescapePath(escMod)
	if err != nil {
		goNotFound(w, "invalid module path")
		return
	}
	m, ok := a.goResolve(ctx, goReader(who), modPath)
	if !ok {
		goNotFound(w, "not found: "+modPath+" (private modules need an access token in ~/.netrc)")
		return
	}
	switch {
	case op == "list":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for _, v := range a.goVersions(ctx, m) {
			fmt.Fprintln(w, v)
		}
	case op == "@latest":
		vers := a.goVersions(ctx, m)
		query := ""
		if len(vers) > 0 {
			query = vers[0]
			for _, v := range vers { // prefer the highest release over pre-releases
				if semver.Prerelease(v) == "" {
					query = v
					break
				}
			}
		} else {
			query = m.git.DefaultBranch(ctx)
		}
		a.goInfo(w, r, m, query)
	case strings.HasSuffix(op, ".info"):
		ver, err := module.UnescapeVersion(strings.TrimSuffix(op, ".info"))
		if err != nil {
			goNotFound(w, "invalid version")
			return
		}
		a.goInfo(w, r, m, ver)
	case strings.HasSuffix(op, ".mod"):
		ver, err := module.UnescapeVersion(strings.TrimSuffix(op, ".mod"))
		if err != nil {
			goNotFound(w, "invalid version")
			return
		}
		commit, ok := a.goCommit(ctx, m, ver)
		if !ok {
			goNotFound(w, "unknown version "+ver)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(a.goModFile(ctx, m, commit))
	case strings.HasSuffix(op, ".zip"):
		ver, err := module.UnescapeVersion(strings.TrimSuffix(op, ".zip"))
		if err != nil {
			goNotFound(w, "invalid version")
			return
		}
		a.goZip(w, r, who, m, ver)
	default:
		goNotFound(w, "not found")
	}
}

// goResolve maps a module path to a repository the caller may read.
func (a *App) goResolve(ctx context.Context, who Actor, modPath string) (goModule, bool) {
	if module.CheckPath(modPath) != nil {
		return goModule{}, false
	}
	elems := strings.Split(modPath, "/")
	m := goModule{path: modPath}
	switch {
	case len(elems) == 3:
	case len(elems) == 4 && majorSuffixRE.MatchString(elems[3]):
		m.major = elems[3]
	default:
		return goModule{}, false
	}
	repo, err := a.db.RepoByName(ctx, elems[1], elems[2])
	if err != nil || !a.can(ctx, who, repo, PermRead) {
		return goModule{}, false
	}
	g, err := a.git.Open(repo.Owner, repo.Name)
	if err != nil || g.IsEmpty(ctx) {
		return goModule{}, false
	}
	m.repo, m.git = repo, g
	return m, true
}

// goMajorOK reports whether a semver version belongs to the module's major.
func (m goModule) majorOK(v string) bool {
	maj := semver.Major(v)
	if m.major == "" {
		return maj == "v0" || maj == "v1"
	}
	return maj == m.major
}

// goVersions lists release tags for the module, highest first.
func (a *App) goVersions(ctx context.Context, m goModule) []string {
	var out []string
	for _, t := range m.git.Tags(ctx) {
		if semver.IsValid(t) && semver.Canonical(t) == t && m.majorOK(t) && !module.IsPseudoVersion(t) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return semver.Compare(out[i], out[j]) > 0 })
	return out
}

// goCommit resolves a canonical version (a tag or a pseudo-version) to a commit.
func (a *App) goCommit(ctx context.Context, m goModule, ver string) (string, bool) {
	if !semver.IsValid(ver) || !m.majorOK(ver) {
		return "", false
	}
	if module.IsPseudoVersion(ver) {
		rev, err := module.PseudoVersionRev(ver)
		if err != nil {
			return "", false
		}
		c, err := m.git.ResolveCommit(ctx, rev)
		if err != nil || !strings.HasPrefix(c, rev) {
			return "", false
		}
		return c, true
	}
	if semver.Canonical(ver) != ver {
		return "", false
	}
	c, err := m.git.ResolveCommit(ctx, "refs/tags/"+ver)
	return c, err == nil
}

// goInfo answers .info / @latest. A query that isn't a known version (a
// branch, a commit) is answered with a pseudo-version.
func (a *App) goInfo(w http.ResponseWriter, r *http.Request, m goModule, query string) {
	ctx := r.Context()
	ver := query
	commit, ok := a.goCommit(ctx, m, query)
	if !ok {
		if semver.IsValid(query) { // a version this module doesn't have
			goNotFound(w, "unknown version "+query)
			return
		}
		c, err := m.git.ResolveCommit(ctx, query)
		if err != nil || strings.HasPrefix(query, "-") {
			goNotFound(w, "unknown revision "+query)
			return
		}
		commit = c
		last := m.git.LastCommit(ctx, c, "")
		if last == nil {
			goNotFound(w, "unknown revision "+query)
			return
		}
		major := m.major
		if major == "" {
			major = "v0"
		}
		ver = module.PseudoVersion(major, "", last.When, c[:12])
	}
	last := m.git.LastCommit(ctx, commit, "")
	if last == nil {
		goNotFound(w, "unknown revision "+query)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"Version": ver, "Time": last.When.UTC().Format(time.RFC3339),
		"Origin": map[string]string{"VCS": "git", "URL": a.baseURL(r) + "/" + m.repo.FullName() + ".git", "Hash": commit},
	})
}

// goModFile returns the go.mod at commit, or a synthesized one.
func (a *App) goModFile(ctx context.Context, m goModule, commit string) []byte {
	if b, err := m.git.Blob(ctx, commit, "go.mod", 16<<20); err == nil && !b.TooLarge {
		if f, err := modfile.ParseLax("go.mod", b.Data, nil); err == nil && f.Module != nil && f.Module.Mod.Path == m.path {
			return b.Data
		}
	}
	return []byte("module " + modfile.AutoQuote(m.path) + "\n")
}

// goZip builds and serves the module zip for a version.
func (a *App) goZip(w http.ResponseWriter, r *http.Request, who Actor, m goModule, ver string) {
	ctx := r.Context()
	commit, ok := a.goCommit(ctx, m, ver)
	if !ok {
		goNotFound(w, "unknown version "+ver)
		return
	}
	// go.mod must declare this module path; otherwise there's no module here.
	if b, err := m.git.Blob(ctx, commit, "go.mod", 16<<20); err == nil && !b.TooLarge {
		if f, err := modfile.ParseLax("go.mod", b.Data, nil); err == nil && f.Module != nil && f.Module.Mod.Path != m.path {
			goNotFound(w, fmt.Sprintf("go.mod at %s declares %s, not %s", ver, f.Module.Mod.Path, m.path))
			return
		}
	}
	tmp, err := os.CreateTemp(filepath.Join(a.pkgBlobs().Root, ".tmp"), "gozip-")
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := a.goBuildZip(ctx, tmp, m, ver, commit); err != nil {
		logErr(r, err)
		http.Error(w, "couldn't build the module zip: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodGet {
		a.goCountDownload(context.WithoutCancel(ctx), who, m, ver)
	}
	w.Header().Set("Content-Type", "application/zip")
	st, _ := tmp.Stat()
	http.ServeContent(w, r, "", st.ModTime(), tmp)
}

// goBuildZip writes the module zip: the files of `git archive` at commit,
// filtered and laid out by golang.org/x/mod/zip.
func (a *App) goBuildZip(ctx context.Context, out io.Writer, m goModule, ver, commit string) error {
	cmd := exec.CommandContext(ctx, a.git.Git, "-c", "core.autocrlf=input", "-c", "core.eol=lf", "archive", "--format=zip", commit)
	cmd.Dir = a.git.Path(m.repo.Owner, m.repo.Name)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git archive: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	zr, err := zip.NewReader(bytes.NewReader(stdout.Bytes()), int64(stdout.Len()))
	if err != nil {
		return err
	}
	var files []modzip.File
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") || f.FileInfo().IsDir() {
			continue
		}
		files = append(files, goZipFile{f})
	}
	return modzip.Create(out, module.Version{Path: m.path, Version: ver}, files)
}

type goZipFile struct{ f *zip.File }

func (z goZipFile) Path() string                 { return z.f.Name }
func (z goZipFile) Lstat() (os.FileInfo, error)  { return z.f.FileInfo(), nil }
func (z goZipFile) Open() (io.ReadCloser, error) { return z.f.Open() }

// goCountDownload records a zip download against a "go" package for the
// repository (created on first download so the Packages page can show counts).
func (a *App) goCountDownload(ctx context.Context, who Actor, m goModule, ver string) {
	o, err := a.db.OwnerByID(ctx, m.repo.OwnerID)
	if err != nil {
		return
	}
	name := m.repo.Name
	if m.major != "" {
		name += "/" + m.major
	}
	p, created, err := a.db.EnsurePackage(ctx, o.ID, store.PkgGo, strings.ToLower(name), !m.repo.Private, store.PkgActor{Kind: ActorSystem, Name: "go-proxy"})
	if err != nil {
		return
	}
	if created || p.RepoID == 0 {
		_ = a.db.UpdatePackage(ctx, p.ID, m.repo.Private, m.repo.Description, m.repo.ID)
	}
	v, err := a.db.Version(ctx, p.ID, ver)
	if err != nil {
		v, err = a.db.AddVersion(ctx, p.ID, store.NewVersion{Version: ver, MediaType: "application/zip"}, store.PkgActor{Kind: ActorSystem, Name: "go-proxy"})
		if err != nil {
			return
		}
	}
	a.db.CountDownload(ctx, p.ID, v.ID, 0)
}

// ---------- go get meta tags ----------

// serveGoGet answers `go get` discovery (?go-get=1) for repository paths:
// /{owner}/{repo}[/subpackage…]. It reports whether it answered.
func (a *App) serveGoGet(w http.ResponseWriter, r *http.Request) bool {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) < 2 || reserved[strings.ToLower(segs[0])] {
		return false
	}
	ctx := r.Context()
	repo, err := a.db.RepoByName(ctx, segs[0], strings.TrimSuffix(segs[1], ".git"))
	if err != nil {
		return false
	}
	who, ok := a.pkgActor(r)
	if !ok || !a.can(ctx, goReader(who), repo, PermRead) {
		return false // the normal page (sign-in or 404) answers
	}
	base := a.baseURL(r)
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	importPath := host + "/" + strings.ToLower(repo.Owner) + "/" + strings.ToLower(repo.Name)
	repoURL := base + "/" + repo.FullName()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html>
<html><head>
<meta name="go-import" content="%s git %s.git">
<meta name="go-source" content="%s %s %s/tree/HEAD{/dir} %s/blob/HEAD{/dir}/{file}#L{line}">
</head><body>go get %s</body></html>
`, html.EscapeString(importPath), html.EscapeString(repoURL), html.EscapeString(importPath), html.EscapeString(repoURL),
		html.EscapeString(repoURL), html.EscapeString(repoURL), html.EscapeString(importPath))
	return true
}
