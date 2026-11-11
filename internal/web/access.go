package web

import (
	"context"
	"net/http"

	"atelier/internal/store"
)

// Access control
//
// Every decision about who may read, change or administer a repository goes
// through App.can — web pages, git over HTTPS and git over SSH alike. Core
// Atelier has one person, so the core policy is simple; the Duo / Team /
// Company addons will register their own rules with registerAccessPolicy
// instead of adding checks around the codebase.

// Perm is what an action needs.
type Perm int

const (
	PermRead  Perm = iota + 1 // view code, issues, pull requests; clone
	PermWrite                 // push, open/edit issues and pull requests, merge
	PermAdmin                 // repository settings, branch rules, delete
	// PermPush is git push (and LFS uploads) only. For everyone but bots it
	// is the same as PermWrite; a bot with push gets this and not PermWrite,
	// so it can't publish releases or change anything else.
	PermPush
)

func (p Perm) String() string {
	switch p {
	case PermRead:
		return "read"
	case PermWrite:
		return "write"
	case PermAdmin:
		return "admin"
	case PermPush:
		return "push"
	}
	return "none"
}

// An AccessPolicy may decide a request. Return decided=false to leave it to the
// next policy (and finally the core policy). Policies run in registration order.
type AccessPolicy func(ctx context.Context, a *App, who Actor, repo store.Repo, need Perm) (allowed, decided bool)

var accessPolicies []AccessPolicy

// registerAccessPolicy adds a policy. Call it from an addon's init().
func registerAccessPolicy(p AccessPolicy) { accessPolicies = append(accessPolicies, p) }

// can reports whether who may do something needing perm on repo.
func (a *App) can(ctx context.Context, who Actor, repo store.Repo, need Perm) bool {
	if need == PermPush && who.Kind != ActorBot {
		need = PermWrite
	}
	for _, p := range accessPolicies {
		if allowed, decided := p(ctx, a, who, repo, need); decided {
			return allowed
		}
	}
	return a.corePolicy(ctx, who, repo, need)
}

// corePolicy: the owner can do anything; their access tokens are limited by
// scope and never administer; anyone else may only read public repositories.
// Bots and agents get nothing in core: they act through scoped tokens.
func (a *App) corePolicy(ctx context.Context, who Actor, repo store.Repo, need Perm) bool {
	// Public repositories can be read by anyone, signed in or not.
	if need == PermRead && !repo.Private && (who.Kind == ActorOwner || who.Kind == ActorAnonymous) {
		return true
	}
	switch who.Kind {
	case ActorOwner:
		return a.isOwnerPerson(ctx, who.PersonID)
	case ActorToken:
		if !a.isOwnerPerson(ctx, who.PersonID) {
			return false
		}
		switch need {
		case PermRead:
			return who.hasScope("repo:read") || who.hasScope("repo:write")
		case PermWrite:
			return who.hasScope("repo:write")
		}
		return false
	case ActorAnonymous:
		return need == PermRead && !repo.Private
	}
	return false
}

// isOwnerPerson reports whether id is the personal owner (the one person in core).
func (a *App) isOwnerPerson(ctx context.Context, id int64) bool {
	if id == 0 {
		return false
	}
	me, err := a.db.PersonalOwner(ctx)
	return err == nil && me.ID == id
}

// needFor works out what a repository page request needs: settings pages need
// admin, any other change needs write, and looking needs read.
func needFor(r *http.Request, tab string) Perm {
	if tab == "settings" {
		return PermAdmin
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return PermWrite
	}
	return PermRead
}

func (a *App) forbidden(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusForbidden)
	a.render(w, r, "error", page{Title: "Not allowed", Data: map[string]string{
		"Heading": "You can't do that here", "Message": "Your account doesn't have permission for this action.",
	}})
}

// A TokenResolver turns a credential presented over git-over-HTTPS (Basic auth
// password) into an Actor, e.g. a short-lived Actions job token. Return
// ok=false if the credential isn't yours. Resolvers run before ordinary access
// tokens are checked. Register from a feature's init().
type TokenResolver func(ctx context.Context, a *App, plain string) (who Actor, ok bool)

var tokenResolvers []TokenResolver

func registerTokenResolver(f TokenResolver) { tokenResolvers = append(tokenResolvers, f) }

// resolveCredential checks the registered resolvers, then access tokens.
func (a *App) resolveCredential(ctx context.Context, plain string) (Actor, bool) {
	for _, f := range tokenResolvers {
		if who, ok := f(ctx, a, plain); ok {
			return who, true
		}
	}
	tok, err := a.db.CheckToken(ctx, plain)
	if err != nil {
		return Actor{}, false
	}
	person, err := a.db.TokenPerson(ctx, tok.ID)
	if err != nil {
		return Actor{}, false
	}
	return Actor{Kind: ActorToken, Name: tok.Name, PersonID: person, Scopes: tok.Scopes}, true
}
