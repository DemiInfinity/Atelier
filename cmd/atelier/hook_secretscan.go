package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"atelier/internal/config"
	"atelier/internal/gitrepo"
	"atelier/internal/secretscan"
	"atelier/internal/store"
)

// Push protection: the pre-receive hook scans the lines added by the pushed
// commits that aren't in the repository yet and rejects the push when they
// contain a secret. Only the new commits are read, within the limits below;
// past them the rest of the push isn't scanned (a huge import shouldn't be
// stuck behind the scanner). A secret can be let through once with a bypass
// created on the repository's Security page.

func init() { preReceiveChecks = append(preReceiveChecks, checkSecretPush) }

// Limits on how much of a push is scanned.
var (
	secretPushMaxCommits       = 1000
	secretPushMaxBytes   int64 = 32 << 20
	secretPushTimeout          = 30 * time.Second
)

// settingSecurityBaseURL is remembered by the web server (see internal/web/security.go).
const settingSecurityBaseURL = "security.base_url"

func checkSecretPush(ctx context.Context, pc pushContext) ([]string, error) {
	settings, err := pc.DB.SecuritySettings(ctx, pc.Repo.ID)
	if err != nil {
		return nil, err
	}
	type hit struct {
		f   secretscan.Finding
		ref string
	}
	var hits []hit
	seen := map[string]bool{}
	for _, u := range pc.Updates {
		if u.isDelete() || !strings.HasPrefix(u.Ref, "refs/") {
			continue
		}
		sc := secretscan.New(loadPushAllowlist(ctx, pc.Git, u.New))
		var found []secretscan.Finding
		args := append(append([]string{}, secretscan.PatchLogArgs...),
			fmt.Sprintf("--max-count=%d", secretPushMaxCommits+1), u.New, "--not", "--all")
		err := pc.Git.StreamGit(ctx, secretPushTimeout, func(r io.Reader) error {
			var err error
			found, _, err = sc.ScanPatchLog(r, secretPushMaxCommits, secretPushMaxBytes)
			return err
		}, args...)
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		for _, f := range found {
			if !seen[f.Fingerprint] {
				seen[f.Fingerprint] = true
				hits = append(hits, hit{f, u.Ref})
			}
		}
	}
	if len(hits) == 0 {
		return nil, nil
	}

	alert := func(f secretscan.Finding, source string) error {
		_, _, err := pc.DB.RecordSecretAlert(ctx, store.SecretAlert{RepoID: pc.Repo.ID, Kind: "code", Source: source,
			RuleID: f.RuleID, RuleName: f.RuleName, Fingerprint: f.Fingerprint, Redacted: f.Redacted,
			Path: f.Path, Line: f.Line, CommitSHA: f.Commit})
		return err
	}
	if !settings.PushProtection {
		// Not blocking: the secrets land, so they become alerts, but only
		// once another check can no longer reject the push.
		pc.onAccept(func(ctx context.Context) (string, error) {
			for _, h := range hits {
				if err := alert(h.f, "push"); err != nil {
					return "", err
				}
			}
			return "", nil
		})
		return nil, nil
	}

	var blocked, bypassed []hit
	for _, h := range hits {
		ok, err := pc.DB.HasSecretBypass(ctx, pc.Repo.ID, h.f.Fingerprint)
		if err != nil {
			return nil, err
		}
		if ok {
			bypassed = append(bypassed, h)
		} else {
			blocked = append(blocked, h)
		}
	}
	if len(blocked) == 0 {
		// Every secret has a one-time bypass. They are used up only when the
		// whole push is accepted: if branch rules or another check reject it,
		// the bypasses stay available for the next try. Using them is one
		// transaction, so two pushes racing for the same bypass can't both
		// get through; the loser is rejected here.
		pc.onAccept(func(ctx context.Context) (string, error) {
			fps := make([]string, len(bypassed))
			for i, h := range bypassed {
				fps[i] = h.f.Fingerprint
			}
			ok, err := pc.DB.UseSecretBypasses(ctx, pc.Repo.ID, fps)
			if err != nil {
				return "", err
			}
			if !ok {
				return "secret scanning: the one-time bypass for this secret was already used by another push. " +
					"Choose \"Allow once\" again on the repository's Security page and push again.", nil
			}
			// Keep an alert so the secret is still rotated later.
			for _, h := range bypassed {
				if err := alert(h.f, "bypass"); err != nil {
					return "", err
				}
			}
			return "", nil
		})
		return nil, nil
	}

	var msgs []string
	const show = 10
	for i, h := range blocked {
		f := h.f
		if err := pc.DB.RecordSecretPushBlock(ctx, store.SecretPushBlock{RepoID: pc.Repo.ID, Fingerprint: f.Fingerprint,
			RuleName: f.RuleName, Redacted: f.Redacted, Path: f.Path, Line: f.Line, CommitSHA: f.Commit, Ref: h.ref}); err != nil {
			return nil, err
		}
		if i < show {
			msgs = append(msgs, fmt.Sprintf("secret scanning: %s in %s:%d (commit %s): %s, fingerprint %s",
				f.RuleName, f.Path, f.Line, short(f.Commit), f.Redacted, f.Fingerprint))
		}
	}
	if len(blocked) > show {
		msgs = append(msgs, fmt.Sprintf("secret scanning: … and %d more", len(blocked)-show))
	}
	base := config.FromEnv().PublicURL
	if base == "" {
		base, _ = pc.DB.Setting(ctx, settingSecurityBaseURL)
	}
	page := strings.TrimRight(base, "/") + "/" + pc.Repo.FullName() + "/security#push-blocks"
	msgs = append(msgs,
		"Push protection blocked this push. Remove the secret from these commits (and rotate it if it was ever real), then push again.",
		"To push it anyway, choose \"Allow once\" for it at "+page+" and push again within an hour.",
		"To stop flagging a test value, add its fingerprint to "+secretscan.AllowlistPath+".")
	return msgs, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// loadPushAllowlist reads .atelier/secret-scanning.yml from the pushed commit.
// A missing or broken file means nothing is allowlisted.
func loadPushAllowlist(ctx context.Context, g *gitrepo.Repo, rev string) *secretscan.Allowlist {
	b, err := g.Blob(ctx, rev, secretscan.AllowlistPath, secretscan.MaxAllowlistBytes)
	if err != nil || b.TooLarge || b.Binary {
		return nil
	}
	al, err := secretscan.ParseAllowlist(b.Data)
	if err != nil {
		return nil
	}
	return al
}
