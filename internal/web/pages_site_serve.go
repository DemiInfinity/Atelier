package web

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"

	"atelier/internal/store"
)

// The Pages host. App.ServeHTTP hands every request whose Host is the host
// of ATELIER_PAGES_URL to servePagesHost before anything else runs, so on
// that host there are no app routes, no git, no static UI files and no
// cookies; and since nothing else calls servePages, the app's host never
// serves a site.

// pagesCSP sandboxes every site: scripts run, but in an opaque origin, so one
// site can't read another's pages (they share the Pages origin) or touch
// storage on it. Resources may come from the site itself or any https URL.
const pagesCSP = "sandbox allow-scripts allow-forms allow-popups allow-popups-to-escape-sandbox allow-downloads allow-modals; " +
	"default-src 'self' https: data: blob: 'unsafe-inline'; object-src 'none'; base-uri 'self'; frame-ancestors 'self'"

// servePagesHost answers the request if it's for the Pages host.
func (a *App) servePagesHost(w http.ResponseWriter, r *http.Request) bool {
	base := a.pagesBase()
	if base == "" {
		return false
	}
	scheme, host, _ := strings.Cut(base, "://")
	_, reqHost := a.external(r)
	if !pagesSameHost(reqHost, scheme, host, scheme) {
		return false
	}
	a.servePages(w, r)
	return true
}

func (a *App) servePages(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Content-Security-Policy", pagesCSP)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/" {
		pagesWelcome(w, r)
		return
	}
	segs, ok := pagesSplitPath(r.URL.Path)
	if !ok || len(segs) == 0 || segs[0] == "" {
		pagesPlain404(w, r)
		return
	}
	ctx := r.Context()
	owner := segs[0]
	// <pages>/<owner>/<repo>/… first, then the owner's site <owner>.pages.
	if len(segs) >= 2 && segs[1] != "" && !strings.EqualFold(segs[1], pagesUserSiteName(owner)) {
		if rp, ps, ok := a.pagesLiveSite(ctx, owner, segs[1]); ok {
			if len(segs) == 2 {
				pagesRedirectSlash(w, r)
				return
			}
			a.pagesServeFile(w, r, rp, ps, strings.Join(segs[2:], "/"))
			return
		}
	}
	if rp, ps, ok := a.pagesLiveSite(ctx, owner, pagesUserSiteName(owner)); ok {
		if len(segs) == 1 {
			pagesRedirectSlash(w, r)
			return
		}
		a.pagesServeFile(w, r, rp, ps, strings.Join(segs[1:], "/"))
		return
	}
	pagesPlain404(w, r)
}

// pagesSplitPath splits a URL path into segments, refusing anything that
// could climb out of a site ("..", ".", backslashes, NUL). A trailing slash
// leaves a final "" segment.
func pagesSplitPath(p string) ([]string, bool) {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return nil, false
	}
	segs := strings.Split(p[1:], "/")
	for i, s := range segs {
		if s == ".." || s == "." || (s == "" && i != len(segs)-1) {
			return nil, false
		}
	}
	return segs, true
}

// pagesLiveSite finds a published, public site.
func (a *App) pagesLiveSite(ctx context.Context, owner, name string) (store.Repo, store.PagesSite, bool) {
	rp, err := a.db.RepoByName(ctx, owner, name)
	if err != nil || rp.Private {
		return rp, store.PagesSite{}, false
	}
	ps, err := a.db.PagesSite(ctx, rp.ID)
	if err != nil || !ps.Enabled || ps.LiveID == 0 {
		return rp, ps, false
	}
	return rp, ps, true
}

func pagesRedirectSlash(w http.ResponseWriter, r *http.Request) {
	target := r.URL.EscapedPath() + "/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// pagesServeFile serves name (relative, may end in "/") from the live deploy.
func (a *App) pagesServeFile(w http.ResponseWriter, r *http.Request, rp store.Repo, ps store.PagesSite, name string) {
	root, err := os.OpenRoot(a.pagesFiles().Dir(rp.ID, ps.LiveID))
	if err != nil {
		pagesPlain404(w, r)
		return
	}
	defer root.Close()
	dirURL := name == "" || strings.HasSuffix(name, "/")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		name = "."
	}
	fi, err := root.Stat(name)
	switch {
	case err == nil && fi.IsDir():
		if !dirURL {
			pagesRedirectSlash(w, r)
			return
		}
		name = path.Join(name, "index.html")
		fi, err = root.Stat(name)
	case err == nil && dirURL:
		err = fs.ErrNotExist // "/page.html/" is not a file
	case errors.Is(err, fs.ErrNotExist) && !dirURL && path.Ext(name) == "":
		// pretty URLs: /about → about.html
		if fi2, err2 := root.Stat(name + ".html"); err2 == nil && fi2.Mode().IsRegular() {
			name, fi, err = name+".html", fi2, nil
		}
	}
	if err != nil || !fi.Mode().IsRegular() {
		a.pagesNotFound(w, r, root, ps.LiveID)
		return
	}
	f, err := root.Open(name)
	if err != nil {
		a.pagesNotFound(w, r, root, ps.LiveID)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", pagesContentType(name))
	h.Set("ETag", pagesETag(ps.LiveID, name, fi))
	h.Set("Cache-Control", "public, max-age=0, must-revalidate")
	h.Set("Access-Control-Allow-Origin", "*") // public content; sandboxed pages fetch from an opaque origin
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// pagesNotFound serves the site's 404.html (with status 404) or a plain 404.
func (a *App) pagesNotFound(w http.ResponseWriter, r *http.Request, root *os.Root, deployID int64) {
	f, err := root.Open("404.html")
	if err != nil {
		pagesPlain404(w, r)
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		pagesPlain404(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

func pagesETag(deployID int64, name string, fi fs.FileInfo) string {
	hs := fnv.New64a()
	_, _ = fmt.Fprintf(hs, "%s\x00%d\x00%d", name, fi.Size(), fi.ModTime().UnixNano())
	return fmt.Sprintf(`"%d-%x"`, deployID, hs.Sum64())
}

// pagesTypes pins the types that matter for sites, so they don't depend on
// the host's mime database.
var pagesTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".mjs": "text/javascript; charset=utf-8",
	".json": "application/json", ".map": "application/json", ".webmanifest": "application/manifest+json",
	".xml": "application/xml", ".rss": "application/rss+xml", ".atom": "application/atom+xml",
	".txt": "text/plain; charset=utf-8", ".md": "text/plain; charset=utf-8", ".csv": "text/csv; charset=utf-8",
	".svg": "image/svg+xml", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".avif": "image/avif", ".ico": "image/x-icon",
	".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".otf": "font/otf",
	".wasm": "application/wasm", ".pdf": "application/pdf", ".zip": "application/zip",
	".mp4": "video/mp4", ".webm": "video/webm", ".mp3": "audio/mpeg", ".ogg": "audio/ogg", ".wav": "audio/wav",
}

func pagesContentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := pagesTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}
