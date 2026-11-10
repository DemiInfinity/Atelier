package web

import (
	"context"
	"fmt"
	"strings"

	"atelier/internal/actions"
	"atelier/internal/store"
)

// Checks: every job of every run for a commit is a check on that commit, and
// on any open pull request whose head is that commit. Branch rules can require
// checks by name (the job name, or "Workflow / job"). The newest run wins when
// the same check ran more than once.

type actCheck struct {
	Name     string // job name
	Workflow string
	Status   string // success, failure, pending, skipped
	Detail   string // the job's own status (queued, waiting, cancelled…)
	URL      string
	Required bool
}

type actChecks struct {
	Checks                                    []actCheck
	Total, Passed, Failed, Pending            int
	MissingRequired, FailingRequired, Waiting []string
}

// checkStatus folds a job status into success / failure / pending / skipped.
func checkStatus(s string) string {
	switch s {
	case actions.StatusSuccess:
		return "success"
	case actions.StatusSkipped:
		return "skipped"
	case actions.StatusFailure, actions.StatusCancelled:
		return "failure"
	}
	return "pending"
}

func checkMatches(required string, c actCheck) bool {
	return strings.EqualFold(required, c.Name) || strings.EqualFold(required, c.Workflow+" / "+c.Name)
}

// commitChecks lists the checks for a commit and how they stand against the
// required names.
func (a *App) commitChecks(ctx context.Context, rp store.Repo, sha string, required []string) actChecks {
	var out actChecks
	if sha == "" {
		return out
	}
	runs, err := a.db.ActionsRuns(ctx, store.ActionsRunFilter{RepoID: rp.ID, SHA: sha, Limit: 200})
	if err != nil {
		return out
	}
	byID := map[int64]store.ActionsRun{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	jobs, err := a.db.ActionsJobsForSHA(ctx, rp.ID, sha)
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		run, ok := byID[j.RunID]
		if !ok {
			continue
		}
		key := strings.ToLower(run.WorkflowPath + "\x00" + j.Name)
		if seen[key] {
			continue // an older run of the same check
		}
		seen[key] = true
		c := actCheck{Name: j.Name, Workflow: run.WorkflowName, Status: checkStatus(j.Status), Detail: j.Status,
			URL: actionsRunURL(rp, run)}
		out.Checks = append(out.Checks, c)
	}
	for _, req := range required {
		found := false
		for i := range out.Checks {
			if checkMatches(req, out.Checks[i]) {
				found = true
				out.Checks[i].Required = true
				switch out.Checks[i].Status {
				case "failure":
					out.FailingRequired = append(out.FailingRequired, req)
				case "pending":
					out.Waiting = append(out.Waiting, req)
				}
			}
		}
		if !found {
			out.MissingRequired = append(out.MissingRequired, req)
		}
	}
	for _, c := range out.Checks {
		out.Total++
		switch c.Status {
		case "success", "skipped":
			out.Passed++
		case "failure":
			out.Failed++
		default:
			out.Pending++
		}
	}
	return out
}

// requiredChecks lists the check names a branch's rules require.
func (a *App) requiredChecks(ctx context.Context, p store.Protection) []string {
	var ids []int64
	for _, r := range p.Rules {
		ids = append(ids, r.ID)
	}
	m, err := a.db.BranchRuleChecks(ctx, ids)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		for _, n := range m[id] {
			if !seen[strings.ToLower(n)] {
				seen[strings.ToLower(n)] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// pullChecks is the checks block of a pull request's merge status.
func (a *App) pullChecks(ctx context.Context, rp store.Repo, headSHA string, p store.Protection) actChecks {
	return a.commitChecks(ctx, rp, headSHA, a.requiredChecks(ctx, p))
}

// addToStatus adds the checks pill, next steps and merge block.
func (c actChecks) addToStatus(s *pullMergeStatus, block func(string)) {
	switch {
	case len(c.FailingRequired) > 0:
		s.Pills = append(s.Pills, pullStatusPill{Label: fmt.Sprintf("Checks failing · %d of %d", c.Failed, c.Total), Kind: "bad",
			Title: "Required: " + strings.Join(c.FailingRequired, ", ")})
		s.Steps = append(s.Steps, pullStep{Text: "Fix the failing required " + pluralWord(len(c.FailingRequired), "check") + ": " + strings.Join(c.FailingRequired, ", ")})
		block("Required " + pluralWord(len(c.FailingRequired), "check") + " failed: " + strings.Join(c.FailingRequired, ", ") + ".")
	case len(c.MissingRequired) > 0 || len(c.Waiting) > 0:
		names := append(append([]string{}, c.Waiting...), c.MissingRequired...)
		label := fmt.Sprintf("Checks pending · %d of %d passed", c.Passed, c.Total)
		if c.Total == 0 {
			label = "Waiting for required checks"
		}
		s.Pills = append(s.Pills, pullStatusPill{Label: label, Kind: "warn", Title: "Required: " + strings.Join(names, ", ")})
		s.Steps = append(s.Steps, pullStep{Text: "Wait for the required " + pluralWord(len(names), "check") + ": " + strings.Join(names, ", ")})
		block("Waiting for required " + pluralWord(len(names), "check") + ": " + strings.Join(names, ", ") + ".")
	case c.Total == 0:
		s.Pills = append(s.Pills, pullStatusPill{Label: "No checks", Kind: "info", Title: "No Actions workflow ran on this commit"})
	case c.Failed > 0:
		s.Pills = append(s.Pills, pullStatusPill{Label: fmt.Sprintf("Checks failing · %d of %d", c.Failed, c.Total), Kind: "bad"})
		s.Steps = append(s.Steps, pullStep{Text: "Look at the failing " + pluralWord(c.Failed, "check")})
	case c.Pending > 0:
		s.Pills = append(s.Pills, pullStatusPill{Label: fmt.Sprintf("Checks pending · %d of %d passed", c.Passed, c.Total), Kind: "warn"})
	default:
		s.Pills = append(s.Pills, pullStatusPill{Label: fmt.Sprintf("Checks passed · %d of %d", c.Passed, c.Total), Kind: "ok"})
		s.Steps = append(s.Steps, pullStep{Text: "All checks passed", Done: true})
	}
}

func pluralWord(n int, w string) string {
	if n == 1 {
		return w
	}
	return w + "s"
}

// parseCheckNames reads a comma- or newline-separated list of check names.
func parseCheckNames(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		n := strings.TrimSpace(part)
		if n == "" || len(n) > 200 || seen[strings.ToLower(n)] || len(out) >= 50 {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	return out
}
