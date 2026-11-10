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

// Actor is who performed an action. Record Actor.Name and Kind on anything you
// store (issue authors, comments, merges) so history stays right when more
// kinds of actor arrive. PersonID says which person is ultimately responsible:
// the signed-in person, or the person who owns the token / key in use.
type Actor struct {
	Kind     string // ActorOwner, ActorToken, ActorAnonymous, "bot", "agent"
	Name     string
	PersonID int64
	Scopes   []string // for tokens
}

const (
	ActorOwner     = "owner" // a signed-in person (in core, always the owner)
	ActorToken     = "token"
	ActorAnonymous = "anonymous"
)

func (who Actor) hasScope(s string) bool {
	for _, x := range who.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

type actorKey struct{}

func withActor(r *http.Request, who Actor) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), actorKey{}, who))
}

// actor returns who is making a request: the signed-in person on routes wrapped
// in a.authed (or passkeyAPI), anonymous otherwise.
func (a *App) actor(r *http.Request) Actor {
	if who, ok := r.Context().Value(actorKey{}).(Actor); ok {
		return who
	}
	// Not signed in (or a route that doesn't sign people in): never assume
	// it's the owner.
	return Actor{Kind: ActorAnonymous, Name: "anonymous"}
}
