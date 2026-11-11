// Package tether is Atelier's link to GitHub.
//
// This file is the contract other features use (the apps & bots bridge):
// they call Get() and never import the web package. The web package
// registers the real bridge when it starts; until then, and whenever Tether
// isn't set up, Get returns a bridge that reports nothing linked.
package tether

import (
	"context"
	"errors"
	"sync/atomic"
)

// Bridge mirrors Atelier pull requests to a repository's GitHub twin.
type Bridge interface {
	// Linked returns the GitHub "owner/repo" twin of an Atelier repo, if linked.
	Linked(ctx context.Context, repoID int64) (ghRepo string, ok bool, err error)
	// MirrorPull pushes the PR's head and base to the linked (private) GitHub twin and opens or updates a matching PR there; returns its GitHub number and URL.
	MirrorPull(ctx context.Context, repoID int64, number int64) (ghNumber int, url string, err error)
	// ClosePull closes the mirrored PR (after a merge/close in Atelier).
	ClosePull(ctx context.Context, repoID int64, number int64, merged bool, sha string) error
}

// ErrNotLinked is returned by the no-op bridge (and by the real one for a
// repository without a GitHub twin).
var ErrNotLinked = errors.New("tether: this repository isn't linked to GitHub")

type holder struct{ b Bridge }

var current atomic.Pointer[holder]

// RegisterBridge installs the bridge. A nil bridge restores the no-op one.
func RegisterBridge(b Bridge) {
	if b == nil {
		current.Store(nil)
		return
	}
	current.Store(&holder{b: b})
}

// Get returns the registered bridge, or a no-op bridge (ok=false) when
// Tether isn't set up.
func Get() Bridge {
	if h := current.Load(); h != nil && h.b != nil {
		return h.b
	}
	return noop{}
}

type noop struct{}

func (noop) Linked(context.Context, int64) (string, bool, error) { return "", false, nil }
func (noop) MirrorPull(context.Context, int64, int64) (int, string, error) {
	return 0, "", ErrNotLinked
}
func (noop) ClosePull(context.Context, int64, int64, bool, string) error { return ErrNotLinked }
