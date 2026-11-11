package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"atelier/internal/gitrepo"
	"atelier/internal/secretscan"
	"atelier/internal/store"
)

// TestSecretPushProtectionOverHTTP pushes over git's smart HTTP protocol to a
// real server whose pre-receive hook runs the real atelier binary.
func TestSecretPushProtectionOverHTTP(t *testing.T) {
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
	if err := a.db.SetSetting(ctx, settingSecurityBaseURL, srv.URL); err != nil {
		t.Fatal(err)
	}
	rp := secRepo(t, a, "demo")
	ck := pullsCookie(t, a)

	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", ".")
	runGit(t, work, "remote", "add", "origin", remote)
	pullsCommit(t, work, "README.md", "# Demo\n", "First")
	runGit(t, work, "push", "-q", "origin", "main")

	// A token in a new commit is rejected, with file:line, the rule and how to bypass.
	gh := secFakeGitHub()
	fp := secretscan.Fingerprint(gh)
	before, _ := gitBranch(t, a, "main")
	pullsCommit(t, work, "config/app.env", "NAME=demo\nTOKEN="+gh+"\n", "Add config")
	out, err := gitTry(work, "push", "origin", "main")
	if err == nil {
		t.Fatalf("push with a secret accepted:\n%s", out)
	}
	mustContain(t, out, "remote rejected", "secret scanning: GitHub personal access token in config/app.env:2", secretscan.Redact(gh),
		"fingerprint "+fp, "Allow once", srv.URL+"/zoe/demo/security#push-blocks", ".atelier/secret-scanning.yml", "pre-receive hook declined")
	if strings.Contains(out, gh) {
		t.Fatal("the hook printed the full secret")
	}
	if got, _ := gitBranch(t, a, "main"); got != before {
		t.Fatal("rejected push moved main")
	}
	blocks, _ := a.db.SecretPushBlocks(ctx, rp.ID, 10)
	if len(blocks) != 1 || blocks[0].Fingerprint != fp || blocks[0].Path != "config/app.env" || blocks[0].Ref != "refs/heads/main" {
		t.Fatalf("blocks = %+v", blocks)
	}
	// Pushing it to another branch is blocked too.
	if out, err := gitTry(work, "push", "origin", "HEAD:refs/heads/elsewhere"); err == nil {
		t.Fatalf("push to a new branch accepted:\n%s", out)
	}

	// Allow once from the Security page, then push again.
	body := expectStatus(t, do(t, a, "GET", "/zoe/demo/security", nil, ck), 200, "page")
	mustContain(t, body, "page", "Blocked pushes", "config/app.env:2", "Allow once")
	expectStatus(t, do(t, a, "POST", "/zoe/demo/security/push-blocks/"+strconv.FormatInt(blocks[0].ID, 10)+"/allow", nil, ck), 303, "allow once")
	if out, err := gitTry(work, "push", "origin", "main"); err != nil {
		t.Fatalf("push after allow once rejected: %v\n%s", err, out)
	}
	alerts, _ := a.db.SecretAlerts(ctx, rp.ID, 0)
	if len(alerts) != 1 || alerts[0].Source != "bypass" || alerts[0].Fingerprint != fp || alerts[0].Path != "config/app.env" || alerts[0].Line != 2 {
		t.Fatalf("alerts after bypass = %+v", alerts)
	}
	if ok, _ := a.db.HasSecretBypass(ctx, rp.ID, fp); ok {
		t.Error("bypass not used up")
	}
	// The bypass was one-time: the same secret in a new commit is blocked again.
	pullsCommit(t, work, "other.env", "T="+gh+"\n", "Again")
	if out, err := gitTry(work, "push", "origin", "main"); err == nil || !strings.Contains(out, "other.env:1") {
		t.Fatalf("second push of the same secret: err=%v\n%s", err, out)
	}

	// Allowlisting its fingerprint lets it through.
	pullsCommit(t, work, ".atelier/secret-scanning.yml", "fingerprints:\n  - "+fp+"  # demo token\n", "Allow the demo token")
	if out, err := gitTry(work, "push", "origin", "main"); err != nil {
		t.Fatalf("allowlisted push rejected: %v\n%s", err, out)
	}

	// With push protection off, secrets land as alerts.
	if err := a.db.SetSecuritySettings(ctx, rp.ID, store.SecuritySettings{}); err != nil {
		t.Fatal(err)
	}
	gh2 := secFakeGitHub()
	pullsCommit(t, work, "late.env", "T="+gh2+"\n", "Late")
	if out, err := gitTry(work, "push", "origin", "main"); err != nil {
		t.Fatalf("push with protection off rejected: %v\n%s", err, out)
	}
	alerts, _ = a.db.SecretAlerts(ctx, rp.ID, 0)
	if len(alerts) != 2 || alerts[0].Fingerprint != secretscan.Fingerprint(gh2) || alerts[0].Source != "push" {
		t.Errorf("alerts with protection off = %+v", alerts)
	}
}
