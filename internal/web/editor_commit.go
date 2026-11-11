package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// editorCommit is one commit from the web editor.
//
// Why the pre-receive hook, not a copy of its checks: branch rules, secret
// scanning push protection, mirrors and Tether all live in the hook binary
// (cmd/atelier, `atelier hook pre-receive`), which other features extend from
// their own files. Web commits run that same hook, fed the proposed ref update
// on stdin with the new objects in a quarantine, exactly as git receive-pack
// does for a push. So there's one source of truth: a check added to the hook
// applies to pushes and web commits alike, and a rejected commit never lands
// in the object store. The editor only checks "require a pull request" itself
// beforehand, to steer the form to "new branch" with a clear message (and as
// a backstop for a repository whose hook isn't installed).
//
// After the branch moves, the update is queued exactly like the
// post-receive hook queues a push (QueueActionsPush), so Actions, repo.pushed
// listeners (webhooks, Pages, Tether) and pull request synchronization fire the
// same way.
type editorCommit struct {
	Branch         string // the branch the person was on
	Base           string // the commit the form was based on ("" for an empty repository)
	Changes        []gitrepo.FileChange
	Touched        []string // paths that must not have changed since Base
	Message        string
	Description    string
	DefaultMessage string
	Choice         string // direct, branch
	NewBranch      string
	CreateBranch   bool // the first commit of an empty repository
}

type editorResult struct {
	SHA          string
	Branch       string // where the commit went
	PullNumber   int64
	Problem      string
	ProtectedMsg string
	Rejected     []string
	Stale        *editorStale
}

// editorStale explains a refused commit: the branch moved and the file changed.
type editorStale struct {
	Branch string
	Tip    string
	Paths  []string
	Diff   string
	Last   *gitrepo.Commit
	URL    string // history of the (first) file on the branch
}

// editorIdentity is who a web commit is authored by: the person's profile
// name, and the commit email set in Settings → Branches for the owner.
func (a *App) editorIdentity(ctx context.Context, who Actor) gitrepo.Signature {
	sig := a.pullIdentity(ctx)
	if a.isOwnerPerson(ctx, who.PersonID) {
		return sig
	}
	if p, err := a.db.Person(ctx, who.PersonID); err == nil {
		sig.Name, sig.Email = p.Label(), strings.ToLower(p.Name)+"@atelier.local"
	}
	return sig
}

func (a *App) editorCommit(ctx context.Context, who Actor, v *repoView, g *gitrepo.Repo, ec editorCommit) (editorResult, error) {
	var res editorResult
	msg := strings.TrimSpace(ec.Message)
	if msg == "" {
		msg = ec.DefaultMessage
	}
	if len(msg) > 500 {
		res.Problem = "Keep the commit message under 500 characters; put the rest in the description."
		return res, nil
	}
	if ec.Description != "" {
		msg += "\n\n" + ec.Description
	}

	tip, err := g.BranchSHA(ctx, ec.Branch)
	switch {
	case err != nil && !ec.CreateBranch:
		res.Problem = "The branch " + ec.Branch + " no longer exists."
		return res, nil
	case err == nil && ec.Base == "":
		// Someone pushed the first commit while this form was open.
		res.Stale = &editorStale{Branch: ec.Branch, Tip: tip, Paths: ec.Touched, Last: g.LastCommit(ctx, tip, "")}
		return res, nil
	case ec.Base != "" && !g.IsCommit(ctx, ec.Base):
		res.Problem = "Atelier lost track of which version you started from. Reload the page and try again."
		return res, nil
	}

	// Conflict safety: building on the tip is fine as long as none of the
	// touched files changed since the form's base; otherwise refuse.
	parent := tip
	if ec.Base != "" && tip != ec.Base {
		changed, err := g.ChangedPaths(ctx, ec.Base, tip, ec.Touched)
		if err != nil {
			return res, err
		}
		if len(changed) > 0 {
			if ec.Choice != "branch" {
				diff, _ := g.PathDiff(ctx, ec.Base, tip, changed, 64<<10)
				res.Stale = &editorStale{Branch: ec.Branch, Tip: tip, Paths: changed, Diff: diff,
					Last: g.LastCommit(ctx, tip, changed[0]), URL: v.Base + "/commits/" + ec.Branch + "/" + changed[0]}
				return res, nil
			}
			// A new branch starts where the person started; the pull request
			// then shows the conflict instead of anything being overwritten.
			parent = ec.Base
		}
	}

	target, old := ec.Branch, tip
	if ec.Choice == "branch" {
		nb := strings.TrimSpace(ec.NewBranch)
		if !gitrepo.ValidBranchName(nb) {
			res.Problem = "Choose a valid name for the new branch (letters, numbers, - _ . /)."
			return res, nil
		}
		if _, err := g.BranchSHA(ctx, nb); err == nil {
			res.Problem = "A branch called " + nb + " already exists. Choose another name."
			return res, nil
		}
		if ec.CreateBranch {
			res.Problem = "This repository is empty: the first commit has to go to " + ec.Branch + "."
			return res, nil
		}
		target, old = nb, ""
	} else if prot := a.editProtection(ctx, v, ec.Branch); prot != "" && !ec.CreateBranch {
		res.ProtectedMsg = prot
		return res, nil
	}

	sig := a.editorIdentity(ctx, who)
	staged, err := g.StageEdit(ctx, gitrepo.Edit{Parent: parent, Changes: ec.Changes, Message: msg, Author: sig, Committer: sig})
	if err != nil {
		switch {
		case errors.Is(err, gitrepo.ErrNoChanges):
			res.Problem = "Nothing changed, so there's nothing to commit."
		case errors.Is(err, gitrepo.ErrPathConflict):
			res.Problem = "There's already a file or folder at " + strings.TrimSuffix(err.Error(), ": "+gitrepo.ErrPathConflict.Error()) + ". Choose another path."
		case errors.Is(err, gitrepo.ErrPathMissing):
			res.Problem = strings.TrimSuffix(err.Error(), ": "+gitrepo.ErrPathMissing.Error()) + " isn't on " + ec.Branch + " any more."
		case strings.Contains(err.Error(), "invalid file path"):
			res.Problem = "One of the file paths isn't allowed."
		default:
			return res, err
		}
		return res, nil
	}
	defer staged.Discard()

	ref := "refs/heads/" + target
	oldOrZero := old
	if oldOrZero == "" {
		oldOrZero = gitrepo.ZeroSHA
	}
	pusher := who.Kind + ":" + who.Name
	if _, err := staged.RunPreReceive(ctx, []gitrepo.RefUpdate{{Old: oldOrZero, New: staged.SHA, Ref: ref}}, pusher); err != nil {
		var hr *gitrepo.HookRejected
		if errors.As(err, &hr) {
			res.Rejected = hr.Messages
			return res, nil
		}
		return res, err
	}
	if err := staged.Publish(ctx, target, old, sig); err != nil {
		switch {
		case errors.Is(err, gitrepo.ErrRefMoved):
			now, _ := g.BranchSHA(ctx, target)
			res.Stale = &editorStale{Branch: target, Tip: now, Paths: ec.Touched, Last: g.LastCommit(ctx, now, "")}
			return res, nil
		case errors.Is(err, gitrepo.ErrBranchExists):
			res.Problem = "A branch called " + target + " was just created. Choose another name."
			return res, nil
		}
		return res, err
	}
	res.SHA, res.Branch = staged.SHA, target

	_ = a.db.TouchRepo(ctx, v.Repo.ID)
	if err := a.db.QueueActionsPush(ctx, store.ActionsPush{Owner: v.Repo.Owner, Repo: v.Repo.Name,
		Old: oldOrZero, New: staged.SHA, Ref: ref, Pusher: pusher}); err != nil {
		log.Printf("editor: Actions won't see %s on %s: %v", staged.SHA, ref, err)
	}

	if ec.Choice == "branch" {
		subject, _, _ := strings.Cut(msg, "\n")
		p, err := a.db.CreatePull(ctx, store.NewPull{RepoID: v.Repo.ID, Title: subject, Body: ec.Description,
			Head: target, Base: ec.Branch, HeadSHA: staged.SHA, AuthorKind: who.Kind, AuthorName: who.Name})
		if err != nil {
			return res, fmt.Errorf("commit %s is on %s, but opening the pull request failed: %w", staged.SHA, target, err)
		}
		a.emit(ctx, pullEvent(EventPullOpened, v, p, who, ec.Description))
		res.PullNumber = p.Number
	}
	return res, nil
}
