package web

import (
	"context"
	"strings"
	"testing"
)

// #12: home/profile Embers and account Insights count the same commits:
// those on each repository's default branch, not other branches.
func TestContributionsDefaultBranchOnlyEverywhere(t *testing.T) {
	f, work, now := insFixture(t) // five commits on main
	a := f.a
	ctx := context.Background()
	ck := pullsCookie(t, a)

	// Two commits on a feature branch, today: counted by neither.
	runGit(t, work, "checkout", "-q", "-b", "feature")
	insCommit(t, work, "Ann Lee <ann@example.com>", now, "wip.txt", "wip\n", "WIP")
	insCommit(t, work, "Ann Lee <ann@example.com>", now, "wip.txt", "wip 2\n", "WIP 2")
	runGit(t, work, "push", "-q", "origin", "feature")

	all, _ := a.db.Repos(ctx)
	if _, total := a.embers(ctx, all); total != 5 {
		t.Errorf("embers total = %d, want 5 (default branch only)", total)
	}
	body := expectStatus(t, do(t, a, "GET", "/insights", nil, ck), 200, "account")
	mustContain(t, body, "account", `ins-ink-commits">5<`, "5 commits in the last year")
	home := expectStatus(t, do(t, a, "GET", "/", nil, ck), 200, "home")
	if !strings.Contains(home, "5 commits") {
		t.Errorf("home doesn't show 5 commits in its Embers")
	}

	// Merged into main, the branch's commits count in both, on their dates.
	runGit(t, work, "checkout", "-q", "main")
	runGit(t, work, "merge", "-q", "--ff-only", "feature")
	runGit(t, work, "push", "-q", "origin", "main")
	cells, total := a.embers(ctx, all)
	if total != 7 {
		t.Errorf("embers total after merge = %d, want 7", total)
	}
	if last := cells[len(cells)-1]; !strings.HasPrefix(last.Title, "2 commits on "+now.Format("Mon 2 Jan 2006")) {
		t.Errorf("today's ember = %+v", last)
	}
	body = expectStatus(t, do(t, a, "GET", "/insights", nil, ck), 200, "account after merge")
	mustContain(t, body, "account after merge", `ins-ink-commits">7<`)
}
