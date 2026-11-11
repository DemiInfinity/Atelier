package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"atelier/internal/store"
)

// Bots pushing over HTTPS (REMOTE_USER "bot:<name>", set by the web server)
// may only update existing or new branches that no branch rule protects,
// never the default branch, never tags, and never delete refs. The web
// access policy already refuses pushes from bots without the push permission;
// this check keeps them off protected branches.

func init() { preReceiveChecks = append(preReceiveChecks, checkBotPush) }

func checkBotPush(ctx context.Context, pc pushContext) ([]string, error) {
	return botPushViolations(ctx, pc, os.Getenv("REMOTE_USER"))
}

func botPushViolations(ctx context.Context, pc pushContext, remoteUser string) ([]string, error) {
	kind, name, _ := strings.Cut(remoteUser, ":")
	if kind != "bot" {
		return nil, nil
	}
	bot, err := pc.DB.BotByName(ctx, name)
	if err != nil || !bot.CanPush || !bot.HasRepo(pc.Repo.ID) {
		return []string{fmt.Sprintf("bot %s isn't allowed to push to %s.", name, pc.Repo.FullName())}, nil
	}
	rules, err := pc.DB.BranchRules(ctx, pc.Repo.ID)
	if err != nil {
		return nil, err
	}
	def := pc.Git.DefaultBranch(ctx)
	var msgs []string
	for _, u := range pc.Updates {
		branch, ok := strings.CutPrefix(u.Ref, "refs/heads/")
		switch {
		case !ok:
			msgs = append(msgs, fmt.Sprintf("bots can only push branches, not %s.", u.Ref))
		case strings.Trim(u.New, "0") == "":
			msgs = append(msgs, fmt.Sprintf("bots can't delete branches (%s).", branch))
		case branch == def:
			msgs = append(msgs, fmt.Sprintf("bots can't push to the default branch %s; open a pull request instead.", branch))
		case store.ProtectionFrom(rules, branch).Protected():
			msgs = append(msgs, fmt.Sprintf("bots can't push to protected branch %s.", branch))
		}
	}
	return msgs, nil
}
