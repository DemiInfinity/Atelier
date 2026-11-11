package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"atelier/internal/secrets"
)

func secretsFixture(t *testing.T) (*Store, *secrets.Keyring, Owner, Repo) {
	t.Helper()
	s := openTest(t)
	ctx := context.Background()
	me, err := s.CreateOwner(ctx, "zoe", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	rp, err := s.CreateRepo(ctx, me.ID, "demo", "", true)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := secrets.NewKeyring(secrets.GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	return s, kr, me, rp
}

var zoeBy = SecretsBy{Name: "zoe", Kind: "owner", PersonID: 1}

func TestSecretNameValidation(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"TOKEN", true}, {"_X", true}, {"A1_B2", true}, {"deploy_key", true}, {" NPM_TOKEN ", true},
		{"", false}, {"1ABC", false}, {"A-B", false}, {"A B", false}, {"A.B", false}, {"ÄBC", false},
		{"GITHUB_TOKEN", false}, {"github_x", false}, {"ATELIER_TOKEN", false}, {"atelier_", false},
		{"GITHUB", true}, {"MY_GITHUB_TOKEN", true}, {strings.Repeat("A", 200), true}, {strings.Repeat("A", 201), false},
	} {
		if got := ValidSecretName(NormalizeSecretName(c.in)); got != c.want {
			t.Errorf("ValidSecretName(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if NormalizeSecretName(" deploy_key ") != "DEPLOY_KEY" {
		t.Error("names aren't upper-cased")
	}
	for _, c := range []struct {
		in   string
		want bool
	}{{"production", true}, {"staging-eu.1", true}, {"Prod_2", true}, {"", false}, {"has space", false}, {"a/b", false}, {strings.Repeat("e", 101), false}} {
		if got := ValidEnvironmentName(c.in); got != c.want {
			t.Errorf("ValidEnvironmentName(%q) = %v", c.in, got)
		}
	}
}

func TestSecretsStoredEncrypted(t *testing.T) {
	s, kr, _, rp := secretsFixture(t)
	ctx := context.Background()
	const value = "s3cr3t-value-do-not-leak"
	if err := s.SetSecret(ctx, kr, RepoScope(rp.ID), "TOKEN", value, zoeBy); err != nil {
		t.Fatal(err)
	}
	var ct []byte
	if err := s.DB().QueryRow(`SELECT ciphertext FROM secrets_values WHERE name='TOKEN'`).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte(value)) {
		t.Fatal("value stored in plain text")
	}
	// Listing shows names and who changed them, never values.
	list, err := s.Secrets(ctx, RepoScope(rp.ID))
	if err != nil || len(list) != 1 || list[0].Name != "TOKEN" || list[0].UpdatedBy != "zoe" || list[0].CreatedAt.IsZero() {
		t.Fatalf("Secrets = %+v, %v", list, err)
	}
	// Limits.
	if err := s.SetSecret(ctx, kr, RepoScope(rp.ID), "EMPTY", "", zoeBy); !errors.Is(err, ErrSecretValue) {
		t.Errorf("empty value: %v", err)
	}
	if err := s.SetSecret(ctx, kr, RepoScope(rp.ID), "BIG", strings.Repeat("x", MaxSecretValue+1), zoeBy); !errors.Is(err, ErrSecretValue) {
		t.Errorf("too big: %v", err)
	}
	if err := s.SetSecret(ctx, kr, RepoScope(rp.ID), "MAX", strings.Repeat("x", MaxSecretValue), zoeBy); err != nil {
		t.Errorf("64 KB value: %v", err)
	}
	if err := s.SetSecret(ctx, kr, RepoScope(rp.ID), "GITHUB_TOKEN", "x", zoeBy); !errors.Is(err, ErrSecretName) {
		t.Errorf("reserved name: %v", err)
	}
	if err := s.SetVariable(ctx, RepoScope(rp.ID), "V", strings.Repeat("x", MaxVariableValue+1), zoeBy); !errors.Is(err, ErrVariableValue) {
		t.Errorf("big variable: %v", err)
	}

	// A ciphertext moved onto another secret's row doesn't open.
	if err := s.SetSecret(ctx, kr, RepoScope(rp.ID), "OTHER", "other", zoeBy); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE secrets_values SET ciphertext=(SELECT ciphertext FROM secrets_values WHERE name='TOKEN') WHERE name='OTHER'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveSecrets(ctx, kr, rp.ID, "", []string{"OTHER"}); !errors.Is(err, secrets.ErrTampered) {
		t.Errorf("moved ciphertext: %v", err)
	}

	// Update keeps created_*, changes the value.
	if err := s.SetSecret(ctx, kr, RepoScope(rp.ID), "TOKEN", "v2", SecretsBy{Name: "bot", Kind: "token"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ResolveSecrets(ctx, kr, rp.ID, "", []string{"TOKEN"})
	list, _ = s.Secrets(ctx, RepoScope(rp.ID))
	if got["TOKEN"] != "v2" || list[len(list)-1].Name != "TOKEN" || list[len(list)-1].UpdatedBy != "bot" || list[len(list)-1].CreatedBy != "zoe" {
		t.Errorf("after update: %v %+v", got, list)
	}
	if err := s.DeleteSecret(ctx, RepoScope(rp.ID), "TOKEN"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSecret(ctx, RepoScope(rp.ID), "TOKEN"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestSecretsPrecedenceAndResolve(t *testing.T) {
	s, kr, me, rp := secretsFixture(t)
	ctx := context.Background()
	other, _ := s.CreateRepo(ctx, me.ID, "other", "", true)
	acme, _ := s.CreateOwner(ctx, "acme", "", "company")
	acmeRepo, _ := s.CreateRepo(ctx, acme.ID, "site", "", true)
	prod, err := s.SaveEnvironment(ctx, Environment{RepoID: rp.ID, Name: "production"}, zoeBy)
	if err != nil {
		t.Fatal(err)
	}
	set := func(sc SecretScope, name, v string) {
		t.Helper()
		if err := s.SetSecret(ctx, kr, sc, name, v, zoeBy); err != nil {
			t.Fatal(err)
		}
		if err := s.SetVariable(ctx, sc, name, v, zoeBy); err != nil {
			t.Fatal(err)
		}
	}
	set(AccountScope(me.ID), "A", "account")
	set(AccountScope(me.ID), "B", "account")
	set(AccountScope(me.ID), "C", "account")
	set(RepoScope(rp.ID), "B", "repo")
	set(RepoScope(rp.ID), "C", "repo")
	set(EnvScope(prod.ID), "C", "env")
	set(RepoScope(other.ID), "D", "other-repo")
	set(AccountScope(acme.ID), "E", "acme")

	names := []string{"A", "B", "C", "D", "E", "MISSING", "bad-name"}
	got, err := s.ResolveSecrets(ctx, kr, rp.ID, "production", names)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "account", "B": "repo", "C": "env"}
	if len(got) != len(want) {
		t.Errorf("Resolve(production) = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Resolve(production)[%s] = %q, want %q", k, got[k], v)
		}
	}
	// No environment (or an unknown one): repository beats account.
	for _, env := range []string{"", "staging"} {
		got, _ = s.ResolveSecrets(ctx, kr, rp.ID, env, names)
		if got["C"] != "repo" || got["B"] != "repo" || got["A"] != "account" || len(got) != 3 {
			t.Errorf("Resolve(%q) = %v", env, got)
		}
	}
	// Only requested names come back, keyed as asked (case-insensitive).
	got, _ = s.ResolveSecrets(ctx, kr, rp.ID, "production", []string{"c"})
	if len(got) != 1 || got["c"] != "env" {
		t.Errorf("Resolve([c]) = %v", got)
	}
	if got, _ := s.ResolveSecrets(ctx, kr, rp.ID, "", nil); len(got) != 0 {
		t.Errorf("Resolve(nil) = %v", got)
	}
	// Account secrets go only to that account's repositories.
	got, _ = s.ResolveSecrets(ctx, kr, acmeRepo.ID, "", names)
	if len(got) != 1 || got["E"] != "acme" {
		t.Errorf("acme Resolve = %v", got)
	}
	// Environment names match case-insensitively.
	if got, _ := s.ResolveSecrets(ctx, kr, rp.ID, "Production", []string{"C"}); got["C"] != "env" {
		t.Errorf("Resolve(Production) = %v", got)
	}

	vars, err := s.ResolveVariables(ctx, rp.ID, "production")
	if err != nil || len(vars) != 3 || vars["A"] != "account" || vars["B"] != "repo" || vars["C"] != "env" {
		t.Errorf("ResolveVariables = %v %v", vars, err)
	}
	vars, _ = s.ResolveVariables(ctx, rp.ID, "")
	if vars["C"] != "repo" || vars["D"] != "" {
		t.Errorf("ResolveVariables('') = %v", vars)
	}

	// Deleting the environment deletes its secrets; deleting the repo its own.
	if err := s.DeleteEnvironment(ctx, rp.ID, prod.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	s.DB().QueryRow(`SELECT COUNT(*) FROM secrets_values WHERE scope='env'`).Scan(&n)
	if n != 0 {
		t.Errorf("%d environment secrets left after delete", n)
	}
	if err := s.DeleteRepo(ctx, rp.ID); err != nil {
		t.Fatal(err)
	}
	s.DB().QueryRow(`SELECT COUNT(*) FROM secrets_values WHERE scope='repo' AND scope_id=?`, rp.ID).Scan(&n)
	if n != 0 {
		t.Errorf("%d repo secrets left after repo delete", n)
	}
	if _, err := s.ResolveSecrets(ctx, kr, rp.ID, "", []string{"A"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted repo: %v", err)
	}
}

func TestEnvironments(t *testing.T) {
	s, _, _, rp := secretsFixture(t)
	ctx := context.Background()
	e, err := s.SaveEnvironment(ctx, Environment{RepoID: rp.ID, Name: "production", RequireApproval: true,
		RunnerLabels: ParseList("mac, linux mac"), Branches: ParseList("main,release/*"), WaitMinutes: 5}, zoeBy)
	if err != nil {
		t.Fatal(err)
	}
	if !e.RequireApproval || strings.Join(e.RunnerLabels, ",") != "mac,linux" || strings.Join(e.Branches, ",") != "main,release/*" ||
		e.WaitMinutes != 5 || e.CreatedBy != "zoe" {
		t.Fatalf("saved = %+v", e)
	}
	if _, err := s.SaveEnvironment(ctx, Environment{RepoID: rp.ID, Name: "PRODUCTION"}, zoeBy); !errors.Is(err, ErrEnvExists) {
		t.Errorf("duplicate: %v", err)
	}
	for _, bad := range []Environment{
		{RepoID: rp.ID, Name: "bad name"},
		{RepoID: rp.ID, Name: "x", RunnerLabels: []string{"a;b"}},
		{RepoID: rp.ID, Name: "x", Branches: []string{"../x"}},
		{RepoID: rp.ID, Name: "x", WaitMinutes: -1},
		{RepoID: rp.ID, Name: "x", WaitMinutes: MaxWaitMinutes + 1},
	} {
		if _, err := s.SaveEnvironment(ctx, bad, zoeBy); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	e.RequireApproval, e.Name = false, "prod"
	e2, err := s.SaveEnvironment(ctx, e, SecretsBy{Name: "sam", Kind: "owner"})
	if err != nil || e2.Name != "prod" || e2.RequireApproval || e2.UpdatedBy != "sam" || e2.CreatedBy != "zoe" {
		t.Fatalf("update = %+v %v", e2, err)
	}
	if _, err := s.SaveEnvironment(ctx, Environment{ID: 999, RepoID: rp.ID, Name: "x"}, zoeBy); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}

	for _, c := range []struct {
		ref  string
		want bool
	}{
		{"refs/heads/main", true}, {"main", true}, {"refs/heads/release/1.0", true}, {"refs/heads/release/1/x", false},
		{"refs/heads/feature", false}, {"refs/tags/v1", false}, {"refs/pull/1/head", false}, {"", false},
	} {
		if got := e2.AllowsRef(c.ref); got != c.want {
			t.Errorf("AllowsRef(%q) = %v", c.ref, got)
		}
	}
	if !(Environment{}).AllowsRef("refs/tags/v1") {
		t.Error("no branch rule should allow everything")
	}
}

func TestRotateSecrets(t *testing.T) {
	s, kr, me, rp := secretsFixture(t)
	ctx := context.Background()
	s.SetSecret(ctx, kr, RepoScope(rp.ID), "ONE", "1", zoeBy)
	s.SetSecret(ctx, kr, AccountScope(me.ID), "TWO", "2", zoeBy)
	to, _ := secrets.NewKeyring(secrets.GenerateKey())
	n, err := s.RotateSecrets(ctx, kr, to)
	if err != nil || n != 2 {
		t.Fatalf("Rotate = %d, %v", n, err)
	}
	got, err := s.ResolveSecrets(ctx, to, rp.ID, "", []string{"ONE", "TWO"})
	if err != nil || got["ONE"] != "1" || got["TWO"] != "2" {
		t.Fatalf("after rotation: %v %v", got, err)
	}
	if _, err := s.ResolveSecrets(ctx, kr, rp.ID, "", []string{"ONE"}); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Errorf("old key still opens: %v", err)
	}
	// A failed rotation (wrong "from" key) changes nothing.
	wrong, _ := secrets.NewKeyring(secrets.GenerateKey())
	if _, err := s.RotateSecrets(ctx, wrong, kr); err == nil {
		t.Fatal("rotation with the wrong key succeeded")
	}
	if got, err := s.ResolveSecrets(ctx, to, rp.ID, "", []string{"ONE"}); err != nil || got["ONE"] != "1" {
		t.Errorf("after failed rotation: %v %v", got, err)
	}
}

func TestRotateSecretsAlsoResealsWebhooksAndTether(t *testing.T) {
	s, kr, me, rp := secretsFixture(t)
	ctx := context.Background()
	h, err := s.SaveWebhook(ctx, Webhook{RepoID: rp.ID, URL: "https://example.com/hook", ContentType: "json", Events: []string{"push"}, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetWebhookSecret(ctx, kr, h.ID, "hook-secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTetherAccount(ctx, kr, TetherAccount{PersonID: me.ID, Login: "zoe", Method: "pat", RateRemaining: -1}, "ghp_token"); err != nil {
		t.Fatal(err)
	}
	to, _ := secrets.NewKeyring(secrets.GenerateKey())
	n, err := s.RotateSecrets(ctx, kr, to)
	if err != nil || n != 2 {
		t.Fatalf("Rotate = %d, %v", n, err)
	}
	if got, err := s.WebhookSecret(ctx, to, h.ID); err != nil || got != "hook-secret" {
		t.Errorf("webhook secret after rotation = %q, %v", got, err)
	}
	if got, err := s.TetherToken(ctx, to, me.ID); err != nil || got != "ghp_token" {
		t.Errorf("tether token after rotation = %q, %v", got, err)
	}
	if _, err := s.WebhookSecret(ctx, kr, h.ID); err == nil {
		t.Error("old key still opens the webhook secret")
	}
}
