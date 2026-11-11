package web

import (
	"context"
	"log"
	"regexp"
	"strconv"

	"atelier/internal/store"
)

// closesRe matches GitHub-style closing keywords: "closes #12", "Fixes #3",
// "resolved #40". Only issues in the same repository are closed.
var closesRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+#(\d+)\b`)

// closingRefs returns the distinct issue numbers a pull request says it closes.
func closingRefs(texts ...string) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, t := range texts {
		for _, m := range closesRe.FindAllStringSubmatch(t, -1) {
			n, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil || n <= 0 || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// closeReferencedIssues closes "closes #N" issues once a pull request merges.
// Numbers that belong to pull requests or don't exist are skipped quietly.
func (a *App) closeReferencedIssues(ctx context.Context, repoID int64, p store.Pull, who Actor) {
	for _, n := range closingRefs(p.Title, p.Body) {
		if n == p.Number {
			continue
		}
		iss, err := a.db.IssueByNumber(ctx, repoID, n)
		if err != nil || iss.State != "open" {
			continue
		}
		if err := a.db.CloseIssueByNumber(ctx, repoID, n, who.Kind, who.Name, store.ReasonCompleted); err != nil {
			log.Printf("closing issue #%d after merging #%d: %v", n, p.Number, err)
			continue
		}
		_ = a.db.AddPullEvent(ctx, p.ID, "closed_issue", who.Kind, who.Name, "#"+strconv.FormatInt(n, 10))
		// Tell everyone else too (notifications, webhooks, projects).
		if rp, err := a.repoByID(ctx, repoID); err == nil {
			if closed, err := a.db.IssueByNumber(ctx, repoID, n); err == nil {
				a.emit(ctx, issueEvent(EventIssueClosed, appsView(rp), closed, who, "closed by #"+strconv.FormatInt(p.Number, 10)))
			}
		}
	}
}
