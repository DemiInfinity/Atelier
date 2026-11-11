package web

import (
	"context"
	"fmt"
	"strings"

	"atelier/internal/store"
)

// Bot accounts. A bot is a named actor with its own access token (an
// ordinary atl_ token row linked from apps_bots), limited to the
// repositories and permissions chosen for it:
//
//	bot:<id>          which bot
//	bot:repo:<id>     a repository it may work in
//	bot:comment       comment on issues and pull requests, set labels
//	bot:suggest       leave inline notes on pull request lines
//	bot:checks        set commit statuses and check runs
//	bot:push          push to branches that no branch rule protects (opt-in)
//
// Bots never merge, never administer, and can't push to protected or
// default branches (the pre-receive hook in cmd/atelier/hook_apps.go
// enforces that for git pushes).

const ActorBot = "bot"

func init() {
	registerTokenResolver(func(ctx context.Context, a *App, plain string) (Actor, bool) { return a.botActor(ctx, plain) })
	registerAccessPolicy(botAccessPolicy)
}

// botActor turns a bot's token into its actor.
func (a *App) botActor(ctx context.Context, plain string) (Actor, bool) {
	if !strings.HasPrefix(plain, store.TokenPrefix) || len(plain) > 200 {
		return Actor{}, false
	}
	tok, err := a.db.CheckToken(ctx, plain)
	if err != nil {
		return Actor{}, false
	}
	b, err := a.db.BotByToken(ctx, tok.ID)
	if err != nil {
		return Actor{}, false
	}
	return botActorFor(b), true
}

func botActorFor(b store.Bot) Actor {
	scopes := []string{fmt.Sprintf("bot:%d", b.ID)}
	for _, id := range b.RepoIDs {
		scopes = append(scopes, fmt.Sprintf("bot:repo:%d", id))
	}
	for _, p := range []struct {
		on   bool
		name string
	}{{b.CanComment, "bot:comment"}, {b.CanSuggest, "bot:suggest"}, {b.CanChecks, "bot:checks"}, {b.CanPush, "bot:push"}} {
		if p.on {
			scopes = append(scopes, p.name)
		}
	}
	return Actor{Kind: ActorBot, Name: b.Name, PersonID: b.PersonID, Scopes: scopes}
}

// botInRepo reports whether a bot actor was given a repository.
func botInRepo(who Actor, repoID int64) bool {
	return who.Kind == ActorBot && who.hasScope(fmt.Sprintf("bot:repo:%d", repoID))
}

// botAccessPolicy: a bot reads its repositories (and public ones), writes
// (git push) only when it has bot:push, and never administers. The bot's
// person must still be the owner.
func botAccessPolicy(ctx context.Context, a *App, who Actor, repo store.Repo, need Perm) (bool, bool) {
	if who.Kind != ActorBot {
		return false, false
	}
	if repo.ID == 0 { // packages: bots have none
		return need == PermRead && !repo.Private, true
	}
	if !botInRepo(who, repo.ID) || !a.isOwnerPerson(ctx, who.PersonID) {
		return need == PermRead && !repo.Private, true
	}
	switch need {
	case PermRead:
		return true, true
	case PermPush:
		return who.hasScope("bot:push"), true
	}
	return false, true // PermWrite, PermAdmin: never
}

// appsMayDo reports whether who may perform an API operation on repo:
// "comment", "suggest" or "checks". Bots need the matching permission and
// the repository; everyone else needs write access.
func (a *App) appsMayDo(ctx context.Context, who Actor, repo store.Repo, op string) bool {
	if who.Kind == ActorBot {
		return botInRepo(who, repo.ID) && a.can(ctx, who, repo, PermRead) && who.hasScope("bot:"+op)
	}
	return a.can(ctx, who, repo, PermWrite)
}

// ---------- statuses and check runs as checks ----------

// appsExternalChecks lists statuses and check runs posted through /api/v3
// for a commit, in the shape the merge status bar uses. actions_checks.go's
// commitChecks should append these (see the apps report).
func (a *App) appsExternalChecks(ctx context.Context, rp store.Repo, sha string) []actCheck {
	if sha == "" {
		return nil
	}
	sts, err := a.db.CommitStatuses(ctx, rp.ID, sha)
	if err != nil {
		return nil
	}
	var out []actCheck
	for _, s := range sts {
		wf := "Status"
		if s.Kind == "check_run" {
			wf = "Check"
		}
		if s.CreatorKind == ActorBot {
			wf = s.CreatorName
		}
		out = append(out, actCheck{Name: s.Context, Workflow: wf, Status: appsCheckStatus(s), Detail: s.Description, URL: s.TargetURL})
	}
	return out
}

// appsCheckStatus folds a status or check run into success / failure / pending / skipped.
func appsCheckStatus(s store.CommitStatus) string {
	switch s.State {
	case "success":
		return "success"
	case "skipped":
		return "skipped"
	case "failure", "error":
		return "failure"
	}
	return "pending"
}
