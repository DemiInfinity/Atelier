package main

import (
	"context"
	"fmt"
)

// Mirrored repositories are overwritten on every sync, so a push would be
// silently lost. Refuse it with a clear message instead.
func init() { preReceiveChecks = append(preReceiveChecks, checkMirror) }

func checkMirror(ctx context.Context, pc pushContext) ([]string, error) {
	src, err := pc.DB.MirrorSource(ctx, pc.Repo.ID)
	if err != nil || src == "" {
		return nil, err
	}
	return []string{fmt.Sprintf("%s is a mirror of %s and is overwritten on every sync. "+
		"Push to the source instead, or stop mirroring in Settings → Import.", pc.Repo.FullName(), src)}, nil
}
