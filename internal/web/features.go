package web

import (
	"context"
	"net/http"

	"atelier/internal/store"
)

// A feature is a self-contained part of Atelier (issues, pull requests, SSH,
// backups…). Each feature lives in its own files and registers itself from an
// init() function, so adding one never means editing a shared list:
//
//	func init() {
//		registerFeature(feature{
//			name:   "issues",
//			routes: func(a *App, m *http.ServeMux) { m.HandleFunc("GET /{owner}/{repo}/issues", a.authed(a.issuesList)) },
//		})
//	}
//
// Templates are picked up automatically: any templates/*.html file becomes a
// page named after the file (without .html), and templates/partials*.html files
// are shared by every page. Database tables are registered with
// store.RegisterSchema from the store package.
type feature struct {
	name string
	// routes adds the feature's HTTP handlers. Wrap UI handlers in a.authed.
	routes func(a *App, m *http.ServeMux)
	// start runs background work (schedulers, listeners) until ctx is done.
	// It is called once from App.Start in its own goroutine.
	start func(ctx context.Context, a *App)
}

var features []feature

func registerFeature(f feature) { features = append(features, f) }

// Start launches every feature's background work. It returns immediately.
func (a *App) Start(ctx context.Context) {
	for _, f := range features {
		if f.start != nil {
			go f.start(ctx, a)
		}
	}
}

// Accessors for code outside the web package (e.g. the SSH server).
func (a *App) Store() *store.Store { return a.db }

// Actor is who performed an action. Today it's always the owner, a token or a
// bot/agent; a future Teams addon adds "user". Record Actor.Name and Kind on
// anything you store (issue authors, comments, merges) so history stays right
// when more kinds of actor arrive.
type Actor struct {
	Kind string // "owner", "token", "bot", "agent"
	Name string
}

// actor returns who is making a UI request. UI routes are always the signed-in owner.
func (a *App) actor(r *http.Request) Actor {
	if me, err := a.db.PersonalOwner(r.Context()); err == nil {
		return Actor{Kind: "owner", Name: me.Name}
	}
	return Actor{Kind: "owner", Name: "owner"}
}
