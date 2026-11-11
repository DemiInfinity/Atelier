package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"atelier/internal/actions"
	"atelier/internal/gitrepo"
	"atelier/internal/secretscan"
	"atelier/internal/store"
)

// Bounds for background scans.
var (
	secHistoryMaxCommits       = 5000
	secHistoryMaxBytes   int64 = 256 << 20
	secHistoryTimeout          = 5 * time.Minute
	secLogMaxBytes       int64 = 64 << 20 // per run
)

// secBackground tracks background scans (tests wait on it).
var secBackground sync.WaitGroup

func secGo(f func()) {
	secBackground.Add(1)
	go func() {
		defer secBackground.Done()
		defer func() {
			if p := recover(); p != nil {
				log.Printf("security: background scan panicked: %v", p)
			}
		}()
		f()
	}()
}

// securityEventHandler scans a run's logs once it has finished.
func securityEventHandler(ctx context.Context, a *App, ev Event) {
	if ev.Kind != EventRunCompleted && ev.Kind != EventRunFailed {
		return
	}
	ctx = context.WithoutCancel(ctx)
	secGo(func() {
		if _, err := a.secScanRunLogs(ctx, ev.RepoID, ev.Number); err != nil {
			log.Printf("security: scanning logs of run %d in repo %d: %v", ev.Number, ev.RepoID, err)
		}
	})
}

// secScanRunLogs scans the stored logs of every job in a run for secrets the
// runner's masking missed. Found values are masked in the stored logs and
// recorded as alerts. It returns the number of new alerts.
func (a *App) secScanRunLogs(ctx context.Context, repoID, seq int64) (int, error) {
	repo, err := a.repoByID(ctx, repoID)
	if err != nil {
		return 0, err
	}
	settings, err := a.db.SecuritySettings(ctx, repo.ID)
	if err != nil || !settings.ScanLogs {
		return 0, err
	}
	run, err := a.db.ActionsRunBySeq(ctx, repo.ID, seq)
	if err != nil {
		return 0, err
	}
	jobs, err := a.db.ActionsJobs(ctx, run.ID)
	if err != nil {
		return 0, err
	}
	sc := secretscan.New(nil)
	var budget = secLogMaxBytes
	created, firstRule := 0, ""
	for _, job := range jobs {
		steps, _ := a.db.ActionsSteps(ctx, job.ID)
		stepName := map[int]string{-1: "Set up job"}
		for _, s := range steps {
			stepName[s.Index] = s.Name
		}
		chunks, err := a.db.ActionsLogs(ctx, job.ID, 0, 0)
		if err != nil {
			return created, err
		}
		lineNo := 0
		for _, ch := range chunks {
			budget -= int64(len(ch.Lines))
			if budget < 0 {
				break
			}
			var lines []actions.LogLine
			if json.Unmarshal([]byte(ch.Lines), &lines) != nil {
				lineNo++
				continue
			}
			scan := make([]secretscan.Line, len(lines))
			for i, l := range lines {
				scan[i] = secretscan.Line{No: lineNo + i + 1, Text: l.Text}
			}
			found := sc.ScanLines("", scan)
			if len(found) > 0 {
				masked := false
				for _, f := range found {
					idx := f.Line - lineNo - 1
					step := lines[idx].Step
					al, isNew, err := a.db.RecordSecretAlert(ctx, store.SecretAlert{RepoID: repo.ID, Kind: "log", Source: "log",
						RuleID: f.RuleID, RuleName: f.RuleName, Fingerprint: f.Fingerprint, Redacted: f.Redacted, Line: f.Line,
						CommitSHA: run.SHA, RunSeq: run.Seq, JobID: job.ID, JobName: job.Name, StepName: stepName[step]})
					if err != nil {
						return created, err
					}
					if isNew {
						created++
						if firstRule == "" {
							firstRule = al.RuleName
						}
					}
					for i := range lines {
						if s := f.Secret(); s != "" && strings.Contains(lines[i].Text, s) {
							lines[i].Text = strings.ReplaceAll(lines[i].Text, s, "***")
							masked = true
						}
					}
				}
				if masked {
					if b, err := json.Marshal(lines); err == nil {
						if err := a.db.RewriteActionsLogChunk(ctx, ch.ID, string(b)); err != nil {
							return created, err
						}
					}
				}
			}
			lineNo += len(lines)
		}
	}
	a.secNotify(ctx, repo, created, fmt.Sprintf("the logs of Actions run #%d", run.Seq), firstRule)
	return created, nil
}

// ---------- history ----------

// secStartHistoryScan starts a background scan of the default branch's
// history. It returns false if one is already running.
func (a *App) secStartHistoryScan(ctx context.Context, repo store.Repo) (bool, error) {
	ok, err := a.db.StartSecurityScan(ctx, repo.ID, "history", secHistoryTimeout+time.Minute)
	if err != nil || !ok {
		return false, err
	}
	ctx = context.WithoutCancel(ctx)
	secGo(func() {
		sc := a.secScanHistory(ctx, repo)
		if err := a.db.FinishSecurityScan(ctx, sc); err != nil {
			log.Printf("security: history scan of %s: %v", repo.FullName(), err)
		}
	})
	return true, nil
}

// secScanHistory scans the lines every commit on the default branch added,
// newest first, within the history limits.
func (a *App) secScanHistory(ctx context.Context, repo store.Repo) store.SecurityScan {
	res := store.SecurityScan{RepoID: repo.ID, Kind: "history"}
	g, err := a.git.Open(repo.Owner, repo.Name)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if g.IsEmpty(ctx) {
		return res
	}
	branch := g.DefaultBranch(ctx)
	tip, err := g.BranchSHA(ctx, branch)
	if err != nil {
		res.Error = "the default branch " + branch + " doesn't exist"
		return res
	}
	sc := secretscan.New(secAllowlist(ctx, g, tip))
	var found []secretscan.Finding
	var stats secretscan.PatchStats
	args := append(append([]string{}, secretscan.PatchLogArgs...), fmt.Sprintf("--max-count=%d", secHistoryMaxCommits+1), tip)
	err = g.StreamGit(ctx, secHistoryTimeout, func(r io.Reader) error {
		var err error
		found, stats, err = sc.ScanPatchLog(r, secHistoryMaxCommits, secHistoryMaxBytes)
		return err
	}, args...)
	res.Commits, res.Truncated = stats.Commits, stats.Truncated
	if errors.Is(err, context.DeadlineExceeded) {
		res.Truncated, err = true, nil
	}
	if err != nil {
		res.Error = err.Error()
	}
	// git log is newest first: record the oldest sighting of each secret.
	created, firstRule := 0, ""
	for i := len(found) - 1; i >= 0; i-- {
		f := found[i]
		_, isNew, err := a.db.RecordSecretAlert(ctx, store.SecretAlert{RepoID: repo.ID, Kind: "code", Source: "history",
			RuleID: f.RuleID, RuleName: f.RuleName, Fingerprint: f.Fingerprint, Redacted: f.Redacted, Path: f.Path, Line: f.Line, CommitSHA: f.Commit})
		if err != nil {
			res.Error = err.Error()
			break
		}
		if isNew {
			created++
			if firstRule == "" {
				firstRule = f.RuleName
			}
		}
	}
	res.Findings = len(found)
	a.secNotify(ctx, repo, created, "the history of "+repo.FullName(), firstRule)
	return res
}

// secAllowlist reads .atelier/secret-scanning.yml at a commit (nil if absent or broken).
func secAllowlist(ctx context.Context, g *gitrepo.Repo, rev string) *secretscan.Allowlist {
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
