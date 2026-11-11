package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"atelier/internal/store"
	"atelier/internal/tether"
)

// The bridge the apps & bots feature uses (internal/tether/contract.go): a
// pull request is mirrored to the repository's GitHub twin, GitHub-only bots
// review it there, and their comments come back through the comment sync.

var tetherBridgeImpl = &tetherBridge{}

type tetherBridge struct{ app atomic.Pointer[App] }

func (b *tetherBridge) get() (*App, error) {
	a := b.app.Load()
	if a == nil {
		return nil, errors.New("tether: server not started")
	}
	return a, nil
}

func (b *tetherBridge) Linked(ctx context.Context, repoID int64) (string, bool, error) {
	a, err := b.get()
	if err != nil {
		return "", false, nil
	}
	l, err := a.db.TetherLink(ctx, repoID)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return l.GHRepo, true, nil
}

func (b *tetherBridge) MirrorPull(ctx context.Context, repoID, number int64) (int, string, error) {
	a, err := b.get()
	if err != nil {
		return 0, "", err
	}
	return a.tetherMirrorPull(ctx, repoID, number)
}

func (b *tetherBridge) ClosePull(ctx context.Context, repoID, number int64, merged bool, sha string) error {
	a, err := b.get()
	if err != nil {
		return err
	}
	return a.tetherClosePull(ctx, repoID, number, merged, sha)
}

const tetherMirrorFooter = "\n\n---\n_Mirrored from Atelier by Tether. Review here; merging happens in Atelier._"

// tetherMirrorPull pushes the pull request's branches to the twin and opens
// (or updates) the matching GitHub pull request.
func (a *App) tetherMirrorPull(ctx context.Context, repoID, number int64) (int, string, error) {
	unlock := a.tether().lockRepo(repoID)
	defer unlock()
	l, err := a.db.TetherLink(ctx, repoID)
	if errors.Is(err, store.ErrNotFound) {
		return 0, "", tether.ErrNotLinked
	}
	if err != nil {
		return 0, "", err
	}
	rp, err := a.repoByID(ctx, repoID)
	if err != nil {
		return 0, "", err
	}
	p, err := a.db.PullByNumber(ctx, repoID, number)
	if err != nil {
		return 0, "", err
	}
	it, itErr := a.db.TetherItem(ctx, repoID, number)
	if itErr == nil && it.Origin == "github" {
		return int(it.GHNumber), it.GHURL, nil // it came from GitHub: it's already there
	}
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return 0, "", err
	}
	head, err := g.BranchSHA(ctx, p.Head)
	if err != nil {
		return 0, "", fmt.Errorf("the head branch %s is gone", p.Head)
	}
	if _, err := g.BranchSHA(ctx, p.Base); err != nil {
		return 0, "", fmt.Errorf("the base branch %s is gone", p.Base)
	}
	c, _, err := a.tetherLinkClient(ctx, l.PersonID, l.GHRepo)
	if err != nil {
		return 0, "", err
	}
	tok, err := a.tetherLinkToken(ctx, l.PersonID, l.GHRepo)
	if err != nil {
		return 0, "", errTetherNotConnected
	}
	hr, br := "refs/heads/"+p.Head, "refs/heads/"+p.Base
	out, perr := a.tetherGit(ctx, a.git.Path(rp.Owner, rp.Name), tok, "", "push", "--porcelain", "--",
		a.tetherRemote(l.GHRepo), "+"+hr+":"+hr, br+":"+br)
	if perr != nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "!\t") && strings.Contains(line, ":"+br+"\t") {
				return 0, "", fmt.Errorf("GitHub's %s has commits Atelier doesn't: reconcile the twin first", p.Base)
			}
		}
		return 0, "", perr
	}
	body := p.Body + tetherMirrorFooter
	var gp tether.Pull
	if itErr == nil {
		gp, err = c.UpdatePull(ctx, l.GHRepo, it.GHNumber, map[string]string{"title": p.Title, "body": body, "base": p.Base, "state": "open"})
	} else {
		gp, err = c.CreatePull(ctx, l.GHRepo, p.Title, p.Head, p.Base, body)
		if tether.IsStatus(err, http.StatusUnprocessableEntity) {
			// Probably already open for this branch: adopt it.
			if open, lerr := c.OpenPulls(ctx, l.GHRepo); lerr == nil {
				for _, o := range open {
					if o.Head.Ref == p.Head && o.Base.Ref == p.Base {
						gp, err = c.UpdatePull(ctx, l.GHRepo, o.Number, map[string]string{"title": p.Title, "body": body})
						break
					}
				}
			}
		}
	}
	if err != nil {
		return 0, "", err
	}
	url := a.tetherHTMLURL(gp.HTMLURL, fmt.Sprintf("%s/pull/%d", l.GHRepo, gp.Number))
	if err := a.db.SaveTetherItem(ctx, store.TetherItem{RepoID: repoID, Kind: "pull", Number: number, GHNumber: gp.Number,
		GHURL: url, Origin: "atelier", HeadSHA: head, GHState: "open"}); err != nil {
		return 0, "", err
	}
	return int(gp.Number), url, nil
}

// tetherClosePull closes the GitHub side of a mirrored pull request.
func (a *App) tetherClosePull(ctx context.Context, repoID, number int64, merged bool, sha string) error {
	unlock := a.tether().lockRepo(repoID)
	defer unlock()
	l, err := a.db.TetherLink(ctx, repoID)
	if errors.Is(err, store.ErrNotFound) {
		return tether.ErrNotLinked
	}
	if err != nil {
		return err
	}
	it, err := a.db.TetherItem(ctx, repoID, number)
	if errors.Is(err, store.ErrNotFound) {
		return nil // never mirrored: nothing to close
	}
	if err != nil {
		return err
	}
	if it.GHState != "open" {
		return nil
	}
	c, _, err := a.tetherLinkClient(ctx, l.PersonID, l.GHRepo)
	if err != nil {
		return err
	}
	if merged {
		msg := "Merged in Atelier."
		if pullSHARe.MatchString(sha) {
			msg = "Merged in Atelier as " + sha + "."
		}
		if cm, err := c.CreateComment(ctx, l.GHRepo, it.GHNumber, msg); err == nil {
			_ = a.db.MarkTetherComment(ctx, repoID, cm.ID, "pull_comment", 0, "out")
		}
	}
	if _, err := c.UpdatePull(ctx, l.GHRepo, it.GHNumber, map[string]string{"state": "closed"}); err != nil {
		return err
	}
	it.GHState = "closed"
	if merged {
		it.GHState = "merged"
	}
	return a.db.SaveTetherItem(ctx, it)
}
