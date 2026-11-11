package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"atelier/internal/gitrepo"
	"atelier/internal/secretscan"
	"atelier/internal/store"
)

// #12: a one-time secret-scanning bypass is only used up when the push is
// accepted. A push that a branch rule rejects leaves it for the next try.
func TestSecretBypassKeptWhenBranchRuleRejects(t *testing.T) {
	bin := buildAtelier(t)
	gitrepo.HookExecutable = bin
	t.Cleanup(func() { gitrepo.HookExecutable = "" })

	f := newGitFixture(t)
	a := f.a
	ctx := context.Background()
	srv := httptest.NewServer(a)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	remote := "http://zoe:" + f.write + "@" + u.Host + "/zoe/demo.git"
	rp := secRepo(t, a, "demo")
	ck := pullsCookie(t, a)

	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", ".")
	runGit(t, work, "remote", "add", "origin", remote)
	pullsCommit(t, work, "README.md", "# Demo\n", "First")
	runGit(t, work, "push", "-q", "origin", "main")

	gh := secFakeGitHub()
	fp := secretscan.Fingerprint(gh)
	pullsCommit(t, work, "app.env", "TOKEN="+gh+"\n", "Add config")
	if out, err := gitTry(work, "push", "origin", "main"); err == nil {
		t.Fatalf("push with a secret accepted:\n%s", out)
	}
	blocks, _ := a.db.SecretPushBlocks(ctx, rp.ID, 10)
	if len(blocks) != 1 {
		t.Fatalf("blocks = %+v", blocks)
	}
	expectStatus(t, do(t, a, "POST", "/zoe/demo/security/push-blocks/"+strconv.FormatInt(blocks[0].ID, 10)+"/allow", nil, ck), 303, "allow once")

	// main now requires pull requests: the push is rejected by the branch
	// rule, after secret scanning has already passed it on the bypass.
	rule, err := a.db.CreateBranchRule(ctx, store.BranchRule{RepoID: rp.ID, Pattern: "main", RequirePR: true})
	if err != nil {
		t.Fatal(err)
	}
	out, err := gitTry(work, "push", "origin", "main")
	if err == nil {
		t.Fatalf("push to a require-PR branch accepted:\n%s", out)
	}
	mustContain(t, out, "branch rule", "pull request")
	if ok, _ := a.db.HasSecretBypass(ctx, rp.ID, fp); !ok {
		t.Fatal("a push rejected by branch rules used up the bypass")
	}
	if alerts, _ := a.db.SecretAlerts(ctx, rp.ID, 0); len(alerts) != 0 {
		t.Fatalf("a rejected push recorded alerts: %+v", alerts)
	}

	// Without the rule the same push goes through and uses the bypass.
	if err := a.db.DeleteBranchRule(ctx, rp.ID, rule.ID); err != nil {
		t.Fatal(err)
	}
	if out, err := gitTry(work, "push", "origin", "main"); err != nil {
		t.Fatalf("push after removing the rule rejected: %v\n%s", err, out)
	}
	if ok, _ := a.db.HasSecretBypass(ctx, rp.ID, fp); ok {
		t.Fatal("accepted push didn't use up the bypass")
	}
	if alerts, _ := a.db.SecretAlerts(ctx, rp.ID, 0); len(alerts) != 1 || alerts[0].Source != "bypass" {
		t.Fatalf("alerts after accepted bypass = %+v", alerts)
	}
}
