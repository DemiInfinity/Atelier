package web

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"atelier/internal/store"
	"atelier/internal/tether"
)

// GitHub bots through the Tether bridge. With "Review on GitHub" on for a
// repository (and the repository linked by Tether), opening or updating a
// pull request mirrors it to the private GitHub twin, where GitHub-only bots
// (CodeRabbit, Cursor Bugbot, any GitHub App) review it. Tether syncs their
// comments back as tether.comment events; merging or closing here closes the
// mirror.
//
// The timeline entry is a pull event of kind "tether_mirrored" with data
// "<github number> <url>"; synced comments are pull comments by kind
// "tether" (see the report for the template lines that render them).

const (
	appsPullEventMirror  = "tether_mirrored"
	appsCommentKindTethr = "tether"
	appsTetherTimeout    = 2 * time.Minute
)

// appsBG runs slow bridge work off the request, tracked so tests can wait.
func (a *App) appsBG(f func(ctx context.Context)) {
	st := a.apps()
	st.bg.Add(1)
	go func() {
		defer st.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), appsTetherTimeout)
		defer cancel()
		f(ctx)
	}()
}

func (a *App) appsTetherOnEvent(ctx context.Context, rp store.Repo, ev Event) {
	switch ev.Kind {
	case EventPullOpened, EventPullReopened:
		if !a.db.TetherReview(ctx, rp.ID) {
			return
		}
		number := ev.Number
		a.appsBG(func(ctx context.Context) {
			p, err := a.db.PullByNumber(ctx, rp.ID, number)
			if err == nil {
				a.appsTetherMirror(ctx, rp, p)
			}
		})
	case EventPullMerged, EventPullClosed:
		p, err := a.db.PullByNumber(ctx, rp.ID, ev.Number)
		if err != nil {
			return
		}
		m, err := a.db.TetherMirror(ctx, p.ID)
		if err != nil || m.Closed {
			return
		}
		merged := ev.Kind == EventPullMerged
		a.appsBG(func(ctx context.Context) {
			if err := tether.Get().ClosePull(ctx, rp.ID, p.Number, merged, p.MergedSHA); err != nil {
				log.Printf("apps: closing GitHub mirror of %s#%d: %v", rp.FullName(), p.Number, err)
				return
			}
			m.Closed = true
			_ = a.db.SaveTetherMirror(ctx, m)
		})
	case EventTetherComment:
		a.appsTetherComment(ctx, rp, ev)
	}
}

// appsTetherSync refreshes the mirror after the head branch moved.
func (a *App) appsTetherSync(ctx context.Context, rp store.Repo, p store.Pull, head string) {
	if !a.db.TetherReview(ctx, rp.ID) {
		return
	}
	if m, err := a.db.TetherMirror(ctx, p.ID); err == nil && m.HeadSHA == head && !m.Closed {
		return
	}
	a.appsBG(func(ctx context.Context) { a.appsTetherMirror(ctx, rp, p) })
}

// appsTetherMirror mirrors (or updates) one pull request on GitHub.
func (a *App) appsTetherMirror(ctx context.Context, rp store.Repo, p store.Pull) {
	br := tether.Get()
	if _, ok, err := br.Linked(ctx, rp.ID); err != nil || !ok {
		if err != nil {
			log.Printf("apps: Tether link for %s: %v", rp.FullName(), err)
		}
		return
	}
	n, url, err := br.MirrorPull(ctx, rp.ID, p.Number)
	if err != nil {
		log.Printf("apps: mirroring %s#%d to GitHub: %v", rp.FullName(), p.Number, err)
		return
	}
	prev, perr := a.db.TetherMirror(ctx, p.ID)
	head, _ := a.appsPullSHAs(ctx, rp, p)
	if err := a.db.SaveTetherMirror(ctx, store.TetherMirror{PullID: p.ID, RepoID: rp.ID, GHNumber: n, URL: url, HeadSHA: head}); err != nil {
		log.Printf("apps: saving mirror of %s#%d: %v", rp.FullName(), p.Number, err)
		return
	}
	if perr != nil || prev.GHNumber != n || prev.Closed {
		_ = a.db.AddPullEvent(ctx, p.ID, appsPullEventMirror, "tether", "Tether", fmt.Sprintf("%d %s", n, url))
	}
}

// appsTetherComment shows a comment synced back from GitHub on the pull
// request, and remembers the bot for the Installed list. If Tether already
// stored the comment (its event links to "#comment-N"), it isn't added again.
func (a *App) appsTetherComment(ctx context.Context, rp store.Repo, ev Event) {
	// Tether's actor is {Kind: "bot", Name: "github:<login>"}.
	name := strings.TrimPrefix(strings.TrimSpace(ev.Actor.Name), "github:")
	if name == "" {
		return
	}
	_ = a.db.NoteTetherBot(ctx, rp.ID, name)
	if ev.Number == 0 || appsCommentID(ev.URL) != 0 || strings.TrimSpace(ev.Detail) == "" {
		return
	}
	p, err := a.db.PullByNumber(ctx, rp.ID, ev.Number)
	if err != nil {
		return
	}
	if _, err := a.db.AddPullComment(ctx, p.ID, appsCommentKindTethr, name, ev.Detail); err != nil {
		log.Printf("apps: storing Tether comment on %s#%d: %v", rp.FullName(), p.Number, err)
	}
}

// appsTetherLinked reports the GitHub twin of a repository, if any.
func (a *App) appsTetherLinked(ctx context.Context, repoID int64) (string, bool) {
	gh, ok, err := tether.Get().Linked(ctx, repoID)
	return gh, ok && err == nil
}
