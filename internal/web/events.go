package web

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"strings"

	"atelier/internal/store"
)

// Events
//
// Features announce that something happened by emitting an Event; other
// features listen with an event handler. Neither side imports the other, so
// Actions, runners, Tether, releases and backups can join later without
// editing shared code.
//
// Emitting, from wherever the thing happens (usually right after it's stored):
//
//	a.emit(ctx, Event{Kind: EventIssueCommented, Repo: v.Repo, Number: is.Number,
//		Title: is.Title, URL: v.Base + "/issues/12#comment-3", Actor: a.actor(r)})
//
// or with the helpers below for the common subjects:
//
//	a.emit(ctx, issueEvent(EventIssueOpened, v, is, act, ""))
//
// Listening, from a feature's init():
//
//	func init() {
//		registerEventHandler(func(ctx context.Context, a *App, ev Event) {
//			if ev.Kind == EventPullMerged { … }
//		})
//	}
//
// Rules:
//   - Kind is "<subject>.<verb>" (issue.opened, pull.merged, backup.failed…).
//     The part before the dot is the subject type notifications group by.
//   - Always set Actor to who did it (a.actor(r) in a handler; a system actor
//     such as Actor{Kind: ActorSystem, Name: "backups"} for background work).
//   - Set Repo (or at least RepoID) for anything about a repository: listeners
//     must check repository access with a.can before showing it to anyone.
//   - Handlers run synchronously, in registration order, after the change is
//     stored, with a context that isn't cancelled when the request ends. A
//     handler that panics is logged and skipped. Keep handlers quick; start a
//     goroutine for slow work (webhooks, network calls).
//   - emit never fails the caller: handlers log their own errors.

// Event is something that happened in Atelier.
type Event struct {
	Kind   string     // "<subject>.<verb>", see the Event* constants
	RepoID int64      // repository it's about, 0 if none
	Repo   store.Repo // optional: the repository itself (saves a lookup)
	Number int64      // issue / pull request number, job id… (0 = the subject as a whole)
	Title  string     // subject title, e.g. the issue title
	URL    string     // site-relative link to the subject ("/owner/repo/issues/3#comment-9")
	Actor  Actor      // who did it
	Detail string     // optional one-line summary (a comment excerpt, an error message)
	// Reason overrides why recipients are told (default: "subscribed" for
	// repository activity, "system" otherwise). Future: "mention", "review_requested".
	Reason string
	// Recipients, if set, limits who is notified to these people. Repository
	// access and the no-self-notification rule still apply.
	Recipients []int64
}

// Subject is the subject type: the part of Kind before the dot.
func (ev Event) Subject() string {
	s, _, _ := strings.Cut(ev.Kind, ".")
	return s
}

// Event kinds emitted by core features. Later features add their own
// (action.failed, runner.offline, tether.synced, release.published…).
const (
	EventIssueOpened    = "issue.opened"
	EventIssueCommented = "issue.commented"
	EventIssueClosed    = "issue.closed"
	EventIssueReopened  = "issue.reopened"

	EventPullOpened    = "pull.opened"
	EventPullCommented = "pull.commented"
	EventPullNote      = "pull.noted"
	EventPullMerged    = "pull.merged"
	EventPullClosed    = "pull.closed"
	EventPullReopened  = "pull.reopened"

	EventBackupSucceeded = "backup.succeeded"
	EventBackupFailed    = "backup.failed"
	EventImportDone      = "import.done"
	EventImportFailed    = "import.failed"
)

// ActorSystem is Atelier itself (schedulers, background jobs).
const ActorSystem = "system"

// EventHandler reacts to an event.
type EventHandler func(ctx context.Context, a *App, ev Event)

var eventHandlers []EventHandler

// registerEventHandler adds a listener. Call it from a feature's init().
func registerEventHandler(h EventHandler) { eventHandlers = append(eventHandlers, h) }

// emit tells every registered handler about ev.
func (a *App) emit(ctx context.Context, ev Event) {
	ctx = context.WithoutCancel(ctx)
	if ev.RepoID == 0 {
		ev.RepoID = ev.Repo.ID
	}
	for _, h := range eventHandlers {
		func() {
			defer func() {
				if p := recover(); p != nil {
					log.Printf("events: handler panicked on %s: %v\n%s", ev.Kind, p, debug.Stack())
				}
			}()
			h(ctx, a, ev)
		}()
	}
}

// ---------- helpers for one-line emits ----------

// eventExcerpt shortens text to one line of at most n runes.
func eventExcerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

// issueEvent builds an event about an issue; anchor ("#comment-4") and detail are optional.
func issueEvent(kind string, v *repoView, is store.Issue, who Actor, detail string, anchor ...string) Event {
	url := fmt.Sprintf("%s/issues/%d", v.Base, is.Number)
	if len(anchor) > 0 {
		url += anchor[0]
	}
	return Event{Kind: kind, Repo: v.Repo, RepoID: v.Repo.ID, Number: is.Number, Title: is.Title, URL: url,
		Actor: who, Detail: eventExcerpt(detail, 140)}
}

// pullEvent builds an event about a pull request; suffix ("/files#note-3") and detail are optional.
func pullEvent(kind string, v *repoView, p store.Pull, who Actor, detail string, suffix ...string) Event {
	url := fmt.Sprintf("%s/pulls/%d", v.Base, p.Number)
	if len(suffix) > 0 {
		url += suffix[0]
	}
	return Event{Kind: kind, Repo: v.Repo, RepoID: v.Repo.ID, Number: p.Number, Title: p.Title, URL: url,
		Actor: who, Detail: eventExcerpt(detail, 140)}
}

// issueStateEvent maps an issue state action ("close", "reopen"…) to its event kind.
func issueStateKind(action string) string {
	switch action {
	case "close", "close_completed", "close_not_planned":
		return EventIssueClosed
	case "reopen":
		return EventIssueReopened
	}
	return ""
}

// emitIssueState emits the event for an applied issue state change, if any.
func (a *App) emitIssueState(ctx context.Context, v *repoView, is store.Issue, who Actor, action string) {
	if kind := issueStateKind(action); kind != "" {
		a.emit(ctx, issueEvent(kind, v, is, who, ""))
	}
}
