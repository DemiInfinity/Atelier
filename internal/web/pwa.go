package web

// The installable app (PWA): /manifest.webmanifest, the service worker at
// /sw.js and the offline page.
//
//   pwa.go             manifest, service worker, build version
//   static/sw.js       the service worker (the version is filled in here)
//   static/pwa.js      registers the worker, the compact top-bar menu and the
//                      bottom navigation shown when installed
//   static/mobile.css  the phone and tablet layout pass, loaded after every
//                      other stylesheet
//   static/offline.html what the worker shows when the server can't be reached
//   static/icons/      PNG icons rendered from the "logo" template
//
// Service workers only run in a secure context: HTTPS, or http://localhost.
// On a plain-HTTP LAN address pwa.js doesn't register anything and the site
// works exactly as before; installing to the home screen needs HTTPS.
//
// Web Push: pushHook below is set by push.go. Every event that notifies
// someone is handed to it once per recipient (the same people the inbox
// notifies, never the person who did it); push.go applies the person's
// preferences and sends to their devices in the background. static/sw.js
// shows the notification on "push" and opens its URL on click. Web Push
// needs HTTPS too (or http://localhost).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	"atelier/internal/store"
)

func init() {
	registerFeature(feature{
		name: "pwa",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /sw.js", a.serviceWorker)
			m.HandleFunc("GET /manifest.webmanifest", a.webManifest)
		},
	})
	registerEventHandler(pwaPushHandler)
}

// PushMessage is what a Web Push sender delivers to one person's devices.
// URL is site-relative; the service worker opens it when the push is tapped.
type PushMessage struct {
	PersonID     int64
	Kind         string
	Reason       string // Event.Reason ("mention", "review_requested"…), may be empty
	Title, Body  string
	URL          string
	RepoFullName string
}

// pushHook sends a push to the person's subscribed devices (push.go sets it;
// nil means Web Push is off). It must return quickly.
var pushHook func(ctx context.Context, a *App, m PushMessage)

// pwaPushHandler fans an event out to pushHook, one message per recipient.
func pwaPushHandler(ctx context.Context, a *App, ev Event) {
	if pushHook == nil || ev.Subject() == "" || ev.Title == "" {
		return
	}
	var repo *store.Repo
	if ev.RepoID != 0 {
		rp := ev.Repo
		if rp.ID != ev.RepoID {
			found, err := a.repoByID(ctx, ev.RepoID)
			if err != nil {
				return
			}
			rp = found
		}
		repo = &rp
	}
	people, err := a.notificationRecipients(ctx, ev, repo)
	if err != nil {
		return
	}
	for _, id := range people {
		if ownAction(ev.Actor, id) {
			continue
		}
		m := PushMessage{PersonID: id, Kind: ev.Kind, Reason: ev.Reason, Title: ev.Title, Body: ev.Detail, URL: safeLocalURL(ev.URL)}
		if repo != nil {
			m.RepoFullName = repo.FullName()
		}
		pushHook(ctx, a, m)
	}
}

// pwaVersionPlaceholder is replaced in static/sw.js with buildVersion().
const pwaVersionPlaceholder = "__ATELIER_VERSION__"

var (
	buildVersionOnce sync.Once
	buildVersionStr  string
)

// buildVersion names the service worker's caches. It changes whenever any
// embedded static file changes (so a deploy with new CSS or JS evicts the old
// caches) and includes the VCS revision when the binary was built from git.
func buildVersion() string {
	buildVersionOnce.Do(func() {
		h := sha256.New()
		var files []string
		_ = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				files = append(files, p)
			}
			return nil
		})
		sort.Strings(files)
		for _, f := range files {
			b, _ := staticFS.ReadFile(f)
			h.Write([]byte(f))
			h.Write([]byte{0})
			h.Write(b)
		}
		v := hex.EncodeToString(h.Sum(nil))[:12]
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" && len(s.Value) >= 7 {
					v = s.Value[:7] + "-" + v
				}
			}
		}
		buildVersionStr = v
	})
	return buildVersionStr
}

var (
	swOnce   sync.Once
	swSource []byte
)

// serviceWorker serves static/sw.js from the root so its scope is the whole
// site. It is never cached by the browser's HTTP cache (no-cache), so a new
// deploy is picked up on the next navigation.
func (a *App) serviceWorker(w http.ResponseWriter, r *http.Request) {
	swOnce.Do(func() {
		b, err := staticFS.ReadFile("static/sw.js")
		if err != nil {
			return
		}
		swSource = []byte(strings.ReplaceAll(string(b), pwaVersionPlaceholder, buildVersion()))
	})
	if swSource == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Service-Worker-Allowed", "/")
	_, _ = w.Write(swSource)
}

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose,omitempty"`
}

type manifestShortcut struct {
	Name  string         `json:"name"`
	URL   string         `json:"url"`
	Icons []manifestIcon `json:"icons,omitempty"`
}

type webManifest struct {
	ID              string             `json:"id"`
	Name            string             `json:"name"`
	ShortName       string             `json:"short_name"`
	Description     string             `json:"description"`
	Lang            string             `json:"lang"`
	StartURL        string             `json:"start_url"`
	Scope           string             `json:"scope"`
	Display         string             `json:"display"`
	DisplayOverride []string           `json:"display_override"`
	Orientation     string             `json:"orientation"`
	BackgroundColor string             `json:"background_color"`
	ThemeColor      string             `json:"theme_color"`
	Categories      []string           `json:"categories"`
	Icons           []manifestIcon     `json:"icons"`
	Shortcuts       []manifestShortcut `json:"shortcuts"`
}

// Palette (static/atelier.css): --bg is the page, --surface the top bar.
const (
	pwaBackground = "#131211"
	pwaTheme      = "#1a1917"
)

func pwaManifest() webManifest {
	icon := []manifestIcon{{Src: "/static/icons/icon-192.png", Sizes: "192x192", Type: "image/png"}}
	return webManifest{
		ID: "/", Name: "Atelier", ShortName: "Atelier", Lang: "en",
		Description: "Your personal GitHub, self-hosted: code, pull requests, Actions and notifications.",
		StartURL:    "/?source=pwa", Scope: "/", Display: "standalone", DisplayOverride: []string{"standalone", "minimal-ui"},
		Orientation: "any", BackgroundColor: pwaBackground, ThemeColor: pwaTheme,
		Categories: []string{"developer", "productivity", "utilities"},
		Icons: []manifestIcon{
			{Src: "/static/icons/icon-192.png", Sizes: "192x192", Type: "image/png", Purpose: "any"},
			{Src: "/static/icons/icon-512.png", Sizes: "512x512", Type: "image/png", Purpose: "any"},
			{Src: "/static/icons/maskable-192.png", Sizes: "192x192", Type: "image/png", Purpose: "maskable"},
			{Src: "/static/icons/maskable-512.png", Sizes: "512x512", Type: "image/png", Purpose: "maskable"},
			{Src: "/static/icon.svg", Sizes: "any", Type: "image/svg+xml", Purpose: "any"},
		},
		Shortcuts: []manifestShortcut{
			{Name: "Notifications", URL: "/notifications", Icons: icon},
			{Name: "Insights", URL: "/insights", Icons: icon},
			{Name: "New repository", URL: "/new", Icons: icon},
		},
	}
}

func (a *App) webManifest(w http.ResponseWriter, r *http.Request) {
	b, err := json.MarshalIndent(pwaManifest(), "", "  ")
	if err != nil {
		http.Error(w, "manifest unavailable", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/manifest+json; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(b)
}
