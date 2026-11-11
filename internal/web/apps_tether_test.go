package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"atelier/internal/tether"
)

// appsFakeBridge stands in for Tether: it "mirrors" pull requests and
// records every call.
type appsFakeBridge struct {
	mu      sync.Mutex
	linked  map[int64]string
	mirrors []int64
	closes  []string
}

func (b *appsFakeBridge) Linked(_ context.Context, repoID int64) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	gh, ok := b.linked[repoID]
	return gh, ok, nil
}

func (b *appsFakeBridge) MirrorPull(_ context.Context, repoID, number int64) (int, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.linked[repoID]; !ok {
		return 0, "", tether.ErrNotLinked
	}
	b.mirrors = append(b.mirrors, number)
	n := int(number) + 100
	return n, fmt.Sprintf("https://github.com/zoe/demo-twin/pull/%d", n), nil
}

func (b *appsFakeBridge) ClosePull(_ context.Context, repoID, number int64, merged bool, sha string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closes = append(b.closes, fmt.Sprintf("%d merged=%v sha=%d", number, merged, len(sha)))
	return nil
}

func (b *appsFakeBridge) snapshot() ([]int64, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int64(nil), b.mirrors...), append([]string(nil), b.closes...)
}

func TestAppsTetherReviewFlow(t *testing.T) {
	f := newAppsFix(t)
	ctx := context.Background()
	br := &appsFakeBridge{linked: map[int64]string{}}
	tether.RegisterBridge(br) // after New, which registers the real bridge
	t.Cleanup(func() { tether.RegisterBridge(nil) })
	wait := func() { f.a.apps().bg.Wait() }

	// Switched off: nothing is mirrored.
	runGit(t, f.work, "checkout", "-q", "-b", "one")
	pullsCommit(t, f.work, "one.txt", "1\n", "One")
	runGit(t, f.work, "push", "-q", "origin", "one")
	do(t, f.a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"one"}, "title": {"One"}}, f.ck)
	wait()
	if m, _ := br.snapshot(); len(m) != 0 {
		t.Fatalf("mirrored with review off: %v", m)
	}

	// Switched on but not linked: still nothing, and the page says so.
	rec := do(t, f.a, "POST", "/zoe/demo/settings/apps", url.Values{"tether_review": {"1"}}, f.ck)
	if rec.Code != http.StatusSeeOther || !f.a.db.TetherReview(ctx, f.repo.ID) {
		t.Fatalf("toggle = %d", rec.Code)
	}
	page := expectStatus(t, do(t, f.a, "GET", "/zoe/demo/settings/apps", nil, f.ck), 200, "repo apps page")
	mustContain(t, page, "repo apps page", "Review on GitHub", "Not linked", "your code goes to the bots’ servers via GitHub",
		"How GitHub bots review", "Bots review on GitHub", "Comments sync back")
	runGit(t, f.work, "checkout", "-q", "main")
	runGit(t, f.work, "checkout", "-q", "-b", "two")
	pullsCommit(t, f.work, "two.txt", "2\n", "Two")
	runGit(t, f.work, "push", "-q", "origin", "two")
	do(t, f.a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"two"}, "title": {"Two"}}, f.ck)
	wait()
	if m, _ := br.snapshot(); len(m) != 0 {
		t.Fatalf("mirrored while not linked: %v", m)
	}

	// Linked: opening mirrors, the timeline gets an entry, a push updates the mirror.
	br.mu.Lock()
	br.linked[f.repo.ID] = "zoe/demo-twin"
	br.mu.Unlock()
	f.a.appsScanRefs(ctx) // baseline
	runGit(t, f.work, "checkout", "-q", "main")
	runGit(t, f.work, "checkout", "-q", "-b", "three")
	pullsCommit(t, f.work, "three.txt", "3\n", "Three")
	runGit(t, f.work, "push", "-q", "origin", "three")
	rec = do(t, f.a, "POST", "/zoe/demo/pulls", url.Values{"base": {"main"}, "head": {"three"}, "title": {"Three"}}, f.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("open pull = %d", rec.Code)
	}
	wait()
	m, _ := br.snapshot()
	if len(m) != 1 || m[0] != 3 {
		t.Fatalf("mirrors = %v, want [3]", m)
	}
	p3, _ := f.a.db.PullByNumber(ctx, f.repo.ID, 3)
	evs, _ := f.a.db.PullEvents(ctx, p3.ID)
	found := false
	for _, e := range evs {
		if e.Kind == "tether_mirrored" && e.Data == "103 https://github.com/zoe/demo-twin/pull/103" {
			found = true
		}
	}
	if !found {
		t.Errorf("no tether_mirrored timeline event: %+v", evs)
	}
	page = expectStatus(t, do(t, f.a, "GET", "/zoe/demo/settings/apps", nil, f.ck), 200, "repo apps page")
	mustContain(t, page, "mirrors", "Linked to zoe/demo-twin", "Mirrored to GitHub for bot review · #103", "https://github.com/zoe/demo-twin/pull/103")

	f.a.appsScanRefs(ctx) // the branch's creation isn't an update
	wait()
	pullsCommit(t, f.work, "three.txt", "33\n", "Three again")
	runGit(t, f.work, "push", "-q", "origin", "three")
	f.a.appsScanRefs(ctx)
	wait()
	if m, _ := br.snapshot(); len(m) != 2 || m[1] != 3 {
		t.Fatalf("after push, mirrors = %v", m)
	}
	if evs, _ := f.a.db.PullEvents(ctx, p3.ID); len(evs) != 1 {
		t.Errorf("updating the mirror added another timeline entry: %+v", evs)
	}

	// A bot's comment comes back (stored by Tether, announced with an anchor): no duplicate, bot listed.
	c, _ := f.a.db.AddPullComment(ctx, p3.ID, "import", "github:coderabbitai[bot]", "Consider a constant here.")
	f.a.emit(ctx, Event{Kind: EventTetherComment, Repo: f.repo, Number: 3, Title: "Three",
		URL: fmt.Sprintf("/zoe/demo/pulls/3#comment-%d", c.ID), Actor: Actor{Kind: "bot", Name: "github:coderabbitai[bot]"}, Detail: "Consider a constant here."})
	// …and one announced without being stored is stored with the Tether kind.
	f.a.emit(ctx, Event{Kind: EventTetherComment, Repo: f.repo, Number: 3, Actor: Actor{Kind: "bot", Name: "github:cursor[bot]"}, Detail: "Possible nil deref."})
	cs, _ := f.a.db.PullComments(ctx, p3.ID)
	if len(cs) != 2 || cs[1].AuthorKind != "tether" || cs[1].AuthorName != "cursor[bot]" {
		t.Fatalf("pull comments = %+v", cs)
	}
	page = expectStatus(t, do(t, f.a, "GET", "/settings/apps", nil, f.ck), 200, "apps page")
	mustContain(t, page, "installed", "coderabbitai[bot]", "cursor[bot]", "via Tether", "zoe/demo/settings/apps")

	// Merging closes the mirror; closing does too; a PR that was never mirrored isn't touched.
	head, _ := gitBranch(t, f.a, "three")
	rec = do(t, f.a, "POST", "/zoe/demo/pulls/3/merge", url.Values{"method": {"squash"}, "expected_head": {head}}, f.ck)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("merge = %d\n%.600s", rec.Code, rec.Body)
	}
	do(t, f.a, "POST", "/zoe/demo/pulls/1/state", url.Values{"action": {"close"}}, f.ck)
	wait()
	_, closes := br.snapshot()
	if len(closes) != 1 || !strings.HasPrefix(closes[0], "3 merged=true sha=40") {
		t.Errorf("closes = %v", closes)
	}
	if mm, _ := f.a.db.TetherMirror(ctx, p3.ID); !mm.Closed {
		t.Error("mirror not marked closed")
	}
}
