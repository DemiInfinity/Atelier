package main

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"atelier/internal/depupdates"
)

// Actions jobs pushing over HTTPS (REMOTE_USER "job:actions-job-<id>") get
// push access only for lockfile regeneration on update branches (see
// internal/web/security_deps_lockfiles.go). This check makes sure such a
// push is a fast-forward of the job's own atelier-updates/* branch, which
// the updates bot opened a pull request from, touching only lockfiles.

func init() { preReceiveChecks = append(preReceiveChecks, checkLockfileJobPush) }

func checkLockfileJobPush(ctx context.Context, pc pushContext) ([]string, error) {
	return lockfileJobViolations(ctx, pc, os.Getenv("REMOTE_USER"))
}

func lockfileJobViolations(ctx context.Context, pc pushContext, remoteUser string) ([]string, error) {
	kind, name, _ := strings.Cut(remoteUser, ":")
	if kind != "job" {
		return nil, nil
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(name, "actions-job-"), 10, 64)
	if err != nil || id <= 0 {
		return []string{"Actions jobs can't push here."}, nil
	}
	st, err := pc.DB.LockfileJobState(ctx, pc.Repo.ID, id, "atelier-updates")
	if err != nil {
		return nil, err
	}
	if !st.Running {
		return []string{"this Actions job isn't running, so it can't push."}, nil
	}
	ups := make([]depupdates.RefUpdate, len(pc.Updates))
	for i, u := range pc.Updates {
		ups[i] = depupdates.RefUpdate{Old: u.Old, New: u.New, Ref: u.Ref}
	}
	msg, err := depupdates.LockfilePushAllowed(depupdates.LockfilePush{
		Branch: st.Branch, BotPR: st.BotPR, Enabled: st.Enabled, Updates: ups,
		FastFwd: func(old, new string) (bool, error) { return pc.Git.IsAncestor(ctx, old, new) },
		Merges:  func(old, new string) (bool, error) { return pc.Git.HasMergeCommits(ctx, old, new) },
		ChangedIn: func(old, new string) ([]string, error) {
			var out []string
			err := pc.Git.StreamGit(ctx, time.Minute, func(r io.Reader) error {
				b, err := io.ReadAll(io.LimitReader(r, 4<<20))
				for _, p := range strings.Split(string(b), "\x00") {
					if p = strings.TrimSpace(p); p != "" {
						out = append(out, p)
					}
				}
				return err
			}, "-c", "core.quotePath=false", "log", "--no-renames", "--format=", "--name-only", "-z", old+".."+new)
			return out, err
		},
	})
	if err != nil || msg == "" {
		return nil, err
	}
	return []string{msg}, nil
}
