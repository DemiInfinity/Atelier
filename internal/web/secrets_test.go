package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"atelier/internal/actions"
	"atelier/internal/secrets"
	"atelier/internal/store"
)

// Secret values used in these tests. None may ever appear in a page.
const (
	secRepoValue    = "repo-value-7f3a9c1e5d"
	secAccountValue = "account-value-2b8e4d6f0a"
	secEnvValue     = "env-value-91c4e7a2b3"
	secAcmeValue    = "acme-value-5d0f3b8c62"
	secRetryValue   = "retry-value-c0ffee1234"
)

func secretsPost(t *testing.T, f profFixture, path string, cookie map[string]string, form url.Values, want int) string {
	t.Helper()
	return expectStatus(t, do(t, f.a, "POST", path, form, cookie), want, "POST "+path)
}

func TestSecretsPagesNeverShowValues(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	f := newProfFixture(t)
	ctx := context.Background()
	a := f.a

	// Everything is set up through the web forms, like a person would.
	secretsPost(t, f, "/zoe/demo/settings/secrets", f.zc, url.Values{"name": {"deploy_key"}, "value": {secRepoValue}}, http.StatusSeeOther)
	secretsPost(t, f, "/settings/secrets", f.zc, url.Values{"account": {"zoe"}, "name": {"NPM_TOKEN"}, "value": {secAccountValue}}, http.StatusSeeOther)
	secretsPost(t, f, "/settings/secrets", f.zc, url.Values{"account": {"acme"}, "name": {"ACME_TOKEN"}, "value": {secAcmeValue}}, http.StatusSeeOther)
	secretsPost(t, f, "/settings/variables", f.zc, url.Values{"account": {"zoe"}, "name": {"REGION"}, "value": {"eu-west"}}, http.StatusSeeOther)
	secretsPost(t, f, "/zoe/demo/settings/variables", f.zc, url.Values{"name": {"DEPLOY_HOST"}, "value": {"deploy.example.com"}}, http.StatusSeeOther)
	rec := do(t, a, "POST", "/zoe/demo/settings/environments", url.Values{"name": {"production"}, "require_approval": {"1"},
		"labels": {"mac"}, "branches": {"main, release/*"}, "wait_minutes": {"5"}}, f.zc)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create environment = %d\n%s", rec.Code, rec.Body)
	}
	prod, err := a.db.EnvironmentByName(ctx, f.demo.ID, "production")
	if err != nil {
		t.Fatal(err)
	}
	envID := strconv.FormatInt(prod.ID, 10)
	if !strings.HasSuffix(rec.Header().Get("Location"), "#env-"+envID) {
		t.Errorf("redirect = %q", rec.Header().Get("Location"))
	}
	secretsPost(t, f, "/zoe/demo/settings/secrets", f.zc, url.Values{"env": {envID}, "name": {"DEPLOY_KEY"}, "value": {secEnvValue}}, http.StatusSeeOther)
	secretsPost(t, f, "/zoe/demo/settings/variables", f.zc, url.Values{"env": {envID}, "name": {"DEPLOY_HOST"}, "value": {"prod.example.com"}}, http.StatusSeeOther)

	// The key file was created on first use, private to the server user.
	fi, err := os.Stat(filepath.Join(a.cfg.DataDir, secrets.KeyFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}

	// Failed saves come back with the name but never the value.
	for _, c := range []struct{ path, account, name string }{
		{"/zoe/demo/settings/secrets", "", "GITHUB_TOKEN"},
		{"/zoe/demo/settings/secrets", "", "1BAD"},
		{"/settings/secrets", "zoe", "ATELIER_X"},
	} {
		form := url.Values{"name": {c.name}, "value": {secRetryValue}}
		if c.account != "" {
			form.Set("account", c.account)
		}
		body := secretsPost(t, f, c.path, f.zc, form, http.StatusUnprocessableEntity)
		mustContain(t, body, "failed save", "flash-error", c.name)
		if strings.Contains(body, secRetryValue) {
			t.Errorf("failed save of %s echoes the value", c.name)
		}
	}
	big := url.Values{"name": {"BIG"}, "value": {strings.Repeat("x", store.MaxSecretValue+1)}}
	secretsPost(t, f, "/zoe/demo/settings/secrets", f.zc, big, http.StatusUnprocessableEntity)

	repoPage := f.get(t, "/zoe/demo/settings/secrets", f.zc, http.StatusOK)
	mustContain(t, repoPage, "repo secrets page",
		"Repository secrets", "DEPLOY_KEY", "Updated just now by zoe", "Inherited from your account", "NPM_TOKEN",
		"Manage account secrets", `href="/settings/secrets?account=zoe"`, "DEPLOY_HOST", "deploy.example.com",
		"prod.example.com", "REGION", "eu-west", "Environment: <span class=\"mono sec-env-name\">production</span>",
		"Approval required", "Jobs wait for your approval", "only run on a runner labelled", "release/*",
		`name="env" value="`+envID+`"`, "/static/secrets.css")
	if strings.Contains(repoPage, "ACME_TOKEN") {
		t.Error("another account's secrets are listed as inherited")
	}
	acctPage := f.get(t, "/settings/secrets?account=zoe", f.zc, http.StatusOK)
	mustContain(t, acctPage, "account page", "NPM_TOKEN", "REGION", "eu-west", `aria-current="page">Secrets and variables`,
		`href="/settings/secrets?account=acme"`, "secret.key")
	acmePage := f.get(t, "/settings/secrets?account=acme", f.zc, http.StatusOK)
	mustContain(t, acmePage, "acme page", "Acme Ltd secrets", "ACME_TOKEN", `name="account" value="acme"`)
	if strings.Contains(acmePage, "NPM_TOKEN") {
		t.Error("acme page lists zoe's secrets")
	}
	mustContain(t, f.get(t, "/zoe/demo/settings", f.zc, http.StatusOK), "repo settings", `href="/zoe/demo/settings/secrets"`)
	mustContain(t, f.get(t, "/zoe/demo/settings/secrets?update=DEPLOY_KEY&scope=repo", f.zc, http.StatusOK), "update form",
		"Update DEPLOY_KEY", "Update secret", "readonly")
	mustContain(t, f.get(t, "/zoe/demo/settings/secrets?edit_var=DEPLOY_HOST&scope=env-"+envID, f.zc, http.StatusOK), "edit var form",
		"Edit DEPLOY_HOST", ">prod.example.com</textarea>")
	mustContain(t, f.get(t, "/zoe/demo/settings/secrets?edit_env="+envID, f.zc, http.StatusOK), "edit env form", `id="env-`+envID+`-edit" open`)

	// Render every page we can reach and make sure no value shows up.
	pages := []string{
		"/", "/zoe", "/acme", "/zoe/demo", "/zoe/open", "/zoe/demo/settings", "/zoe/demo/settings/secrets",
		"/zoe/demo/settings/secrets?update=DEPLOY_KEY&scope=repo", "/zoe/demo/settings/secrets?update=DEPLOY_KEY&scope=env-" + envID,
		"/zoe/demo/settings/secrets?edit_env=" + envID, "/zoe/demo/settings/branches", "/zoe/demo/issues", "/zoe/demo/pulls",
		"/zoe/demo/commits/main", "/settings/secrets", "/settings/secrets?account=zoe", "/settings/secrets?account=acme",
		"/settings/secrets?account=zoe&update=NPM_TOKEN&scope=account", "/settings/profile", "/settings/tokens",
		"/settings/keys", "/settings/passkeys", "/settings/backups", "/settings/import", "/notifications",
		"/settings/keys/clone.json", "/new", "/accounts/new",
	}
	for _, p := range pages {
		for _, cookie := range []map[string]string{f.zc, f.sc, nil} {
			body := do(t, a, "GET", p, nil, cookie).Body.String()
			for _, v := range []string{secRepoValue, secAccountValue, secEnvValue, secAcmeValue, secRetryValue} {
				if strings.Contains(body, v) {
					t.Errorf("GET %s shows a secret value", p)
				}
			}
		}
	}
	// Nor is any value stored in plain text on disk.
	for _, name := range []string{"atelier.db", "atelier.db-wal"} {
		data, err := os.ReadFile(filepath.Join(a.cfg.DataDir, name))
		if err != nil {
			continue
		}
		for _, v := range []string{secRepoValue, secAccountValue, secEnvValue, secAcmeValue} {
			if strings.Contains(string(data), v) {
				t.Errorf("%s holds a secret value in plain text", name)
			}
		}
	}

	// The Actions hook resolves them, with precedence.
	src := actions.Secrets()
	jobCtx := actions.WithEnvironmentRef(ctx, "refs/heads/main")
	got, err := src.Resolve(jobCtx, f.demo.ID, "production", []string{"DEPLOY_KEY", "NPM_TOKEN", "ACME_TOKEN", "NOPE"})
	if err != nil || len(got) != 2 || got["DEPLOY_KEY"] != secEnvValue || got["NPM_TOKEN"] != secAccountValue {
		t.Errorf("Resolve(production) = %v, %v", got, err)
	}
	got, err = src.Resolve(ctx, f.demo.ID, "", []string{"DEPLOY_KEY"})
	if err != nil || len(got) != 1 || got["DEPLOY_KEY"] != secRepoValue {
		t.Errorf("Resolve('') = %v, %v", got, err)
	}
	vars, err := src.Variables(jobCtx, f.demo.ID, "production")
	if err != nil || vars["DEPLOY_HOST"] != "prod.example.com" || vars["REGION"] != "eu-west" {
		t.Errorf("Variables = %v, %v", vars, err)
	}

	// Delete through the forms.
	secretsPost(t, f, "/zoe/demo/settings/secrets/delete", f.zc, url.Values{"env": {envID}, "name": {"DEPLOY_KEY"}}, http.StatusSeeOther)
	secretsPost(t, f, "/zoe/demo/settings/variables/delete", f.zc, url.Values{"name": {"DEPLOY_HOST"}}, http.StatusSeeOther)
	secretsPost(t, f, "/settings/variables/delete", f.zc, url.Values{"account": {"zoe"}, "name": {"REGION"}}, http.StatusSeeOther)
	secretsPost(t, f, "/settings/secrets/delete", f.zc, url.Values{"account": {"acme"}, "name": {"ACME_TOKEN"}}, http.StatusSeeOther)
	got, _ = src.Resolve(jobCtx, f.demo.ID, "production", []string{"DEPLOY_KEY", "ACME_TOKEN"})
	if got["DEPLOY_KEY"] != secRepoValue || len(got) != 1 {
		t.Errorf("after deleting the environment secret: %v", got)
	}
	if v, _ := a.db.Secrets(ctx, store.AccountScope(f.acme.ID)); len(v) != 0 {
		t.Errorf("acme secrets after delete: %v", v)
	}
	secretsPost(t, f, "/zoe/demo/settings/environments/delete", f.zc, url.Values{"id": {envID}}, http.StatusSeeOther)
	if _, err := a.db.EnvironmentByName(ctx, f.demo.ID, "production"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("environment still there: %v", err)
	}
}

func TestSecretsPermissions(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	f := newProfFixture(t)
	ctx := context.Background()
	a := f.a
	// sam can read the public repo but administer nothing of zoe's.
	f.get(t, "/zoe/demo/settings/secrets", f.sc, http.StatusNotFound)
	f.get(t, "/zoe/open/settings/secrets", f.sc, http.StatusForbidden)
	f.get(t, "/settings/secrets?account=zoe", f.sc, http.StatusForbidden)
	f.get(t, "/settings/secrets?account=acme", f.sc, http.StatusForbidden)
	f.get(t, "/settings/secrets?account=nobody", f.zc, http.StatusNotFound)
	// sam manages his own account only; the switcher offers nothing else.
	own := f.get(t, "/settings/secrets", f.sc, http.StatusOK)
	if strings.Contains(own, "account=acme") || strings.Contains(own, "account=zoe") {
		t.Error("sam's switcher offers accounts he can't edit")
	}

	for _, c := range []struct {
		path string
		form url.Values
		want int
	}{
		{"/zoe/demo/settings/secrets", url.Values{"name": {"X"}, "value": {"v"}}, http.StatusNotFound},
		{"/zoe/open/settings/secrets", url.Values{"name": {"X"}, "value": {"v"}}, http.StatusForbidden},
		{"/zoe/open/settings/variables", url.Values{"name": {"X"}, "value": {"v"}}, http.StatusForbidden},
		{"/zoe/open/settings/environments", url.Values{"name": {"prod"}}, http.StatusForbidden},
		{"/settings/secrets", url.Values{"account": {"zoe"}, "name": {"X"}, "value": {"v"}}, http.StatusForbidden},
		{"/settings/secrets", url.Values{"account": {"acme"}, "name": {"X"}, "value": {"v"}}, http.StatusForbidden},
		{"/settings/variables", url.Values{"account": {"acme"}, "name": {"X"}, "value": {"v"}}, http.StatusForbidden},
	} {
		secretsPost(t, f, c.path, f.sc, c.form, c.want)
	}
	for _, sc := range []store.SecretScope{store.RepoScope(f.pub.ID), store.RepoScope(f.demo.ID), store.AccountScope(f.zoe.ID), store.AccountScope(f.acme.ID)} {
		if s, _ := a.db.Secrets(ctx, sc); len(s) != 0 {
			t.Errorf("sam managed to add a secret to %v", sc)
		}
		if v, _ := a.db.Variables(ctx, sc); len(v) != 0 {
			t.Errorf("sam managed to add a variable to %v", sc)
		}
	}
	if envs, _ := a.db.Environments(ctx, f.pub.ID); len(envs) != 0 {
		t.Error("sam created an environment")
	}
	// An environment id from another repository can't be targeted.
	other, err := a.db.SaveEnvironment(ctx, store.Environment{RepoID: f.pub.ID, Name: "production"}, store.SecretsBy{Name: "zoe"})
	if err != nil {
		t.Fatal(err)
	}
	secretsPost(t, f, "/zoe/demo/settings/secrets", f.zc, url.Values{"env": {strconv.FormatInt(other.ID, 10)}, "name": {"X"}, "value": {"v"}}, http.StatusNotFound)
	secretsPost(t, f, "/zoe/demo/settings/environments/delete", f.zc, url.Values{"id": {strconv.FormatInt(other.ID, 10)}}, http.StatusNotFound)
	// Signed-out visitors are sent to sign in.
	if rec := do(t, a, "GET", "/zoe/open/settings/secrets", nil, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("anonymous = %d", rec.Code)
	}
	if rec := do(t, a, "POST", "/settings/secrets", url.Values{"account": {"zoe"}, "name": {"X"}, "value": {"v"}}, nil); rec.Code != http.StatusSeeOther {
		t.Errorf("anonymous post = %d", rec.Code)
	}
}

func TestSecretsEnvironmentGate(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	f := newProfFixture(t)
	ctx := context.Background()
	a := f.a
	src := actions.Secrets()
	by := store.SecretsBy{Name: "zoe", Kind: ActorOwner, PersonID: f.zoe.ID}
	prod, err := a.db.SaveEnvironment(ctx, store.Environment{RepoID: f.demo.ID, Name: "production", RequireApproval: true,
		RunnerLabels: []string{"mac"}, Branches: []string{"main", "release/*"}, WaitMinutes: 10}, by)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.SaveEnvironment(ctx, store.Environment{RepoID: f.demo.ID, Name: "staging"}, by); err != nil {
		t.Fatal(err)
	}
	if err := a.db.SetSecret(ctx, a.secretSealer(), store.EnvScope(prod.ID), "DEPLOY_KEY", "k", by); err != nil {
		t.Fatal(err)
	}

	main := actions.WithEnvironmentRef(ctx, "refs/heads/main")
	approve, labels, err := src.EnvironmentGate(main, f.demo.ID, "production")
	if err != nil || !approve || len(labels) != 1 || labels[0] != "mac" {
		t.Errorf("gate(main) = %v %v %v", approve, labels, err)
	}
	if _, _, err := src.EnvironmentGate(actions.WithEnvironmentRef(ctx, "refs/heads/release/2.0"), f.demo.ID, "production"); err != nil {
		t.Errorf("gate(release/2.0) = %v", err)
	}
	for _, ref := range []string{"refs/heads/feature", "refs/tags/v1.0", ""} {
		c := ctx
		if ref != "" {
			c = actions.WithEnvironmentRef(ctx, ref)
		}
		_, _, err := src.EnvironmentGate(c, f.demo.ID, "production")
		var be *actions.EnvironmentBranchError
		if !errors.As(err, &be) || be.Environment != "production" {
			t.Errorf("gate(%q) = %v, want a branch error", ref, err)
			continue
		}
		if !strings.Contains(err.Error(), "production") || !strings.Contains(err.Error(), "main, release/*") {
			t.Errorf("message = %q", err)
		}
		// Its secrets aren't released to that ref either.
		if got, err := src.Resolve(c, f.demo.ID, "production", []string{"DEPLOY_KEY"}); err == nil || len(got) != 0 {
			t.Errorf("Resolve(%q) = %v, %v", ref, got, err)
		}
	}
	if got, err := src.Resolve(main, f.demo.ID, "production", []string{"DEPLOY_KEY"}); err != nil || got["DEPLOY_KEY"] != "k" {
		t.Errorf("Resolve(main) = %v %v", got, err)
	}
	// No rules, no environment, unknown environment: nothing to wait for.
	for _, env := range []string{"staging", "", "nope"} {
		if approve, labels, err := src.EnvironmentGate(ctx, f.demo.ID, env); err != nil || approve || labels != nil {
			t.Errorf("gate(%q) = %v %v %v", env, approve, labels, err)
		}
	}
	w, ok := src.(actions.EnvironmentWaiter)
	if !ok {
		t.Fatal("source has no wait timer")
	}
	if d, err := w.EnvironmentWait(main, f.demo.ID, "production"); err != nil || d.Minutes() != 10 {
		t.Errorf("wait = %v %v", d, err)
	}

	// Editing through the form: bad input is refused and shown again.
	body := secretsPost(t, f, "/zoe/demo/settings/environments", f.zc, url.Values{"id": {strconv.FormatInt(prod.ID, 10)},
		"name": {"production"}, "branches": {"../evil"}}, http.StatusUnprocessableEntity)
	mustContain(t, body, "bad env edit", "flash-error", "../evil")
	secretsPost(t, f, "/zoe/demo/settings/environments", f.zc, url.Values{"name": {"staging"}}, http.StatusUnprocessableEntity)
	secretsPost(t, f, "/zoe/demo/settings/environments", f.zc, url.Values{"id": {strconv.FormatInt(prod.ID, 10)},
		"name": {"production"}, "labels": {"linux, docker"}}, http.StatusSeeOther)
	approve, labels, err = src.EnvironmentGate(actions.WithEnvironmentRef(ctx, "refs/heads/anything"), f.demo.ID, "production")
	if err != nil || approve || strings.Join(labels, ",") != "linux,docker" {
		t.Errorf("after edit: %v %v %v", approve, labels, err)
	}
}

func TestSecretsKeyRotationWhileRunning(t *testing.T) {
	t.Setenv(secrets.EnvKey, "")
	f := newProfFixture(t)
	ctx := context.Background()
	a := f.a
	by := store.SecretsBy{Name: "zoe", Kind: ActorOwner, PersonID: f.zoe.ID}
	if err := a.db.SetSecret(ctx, a.secretSealer(), store.RepoScope(f.demo.ID), "ONE", "1", by); err != nil {
		t.Fatal(err)
	}
	// Rotate the way `atelier secrets rotate-key` does, behind the server's back.
	dir := a.cfg.DataDir
	cur, _, err := secrets.Load(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	raw := secrets.GenerateKey()
	next, _ := secrets.NewKeyring(raw)
	if _, err := a.db.RotateSecrets(ctx, cur, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, secrets.KeyFile), filepath.Join(dir, secrets.OldKeyFile)); err != nil {
		t.Fatal(err)
	}
	if err := secrets.WriteKeyFile(filepath.Join(dir, secrets.KeyFile), raw); err != nil {
		t.Fatal(err)
	}
	// The server picks the new key up without a restart.
	got, err := actions.Secrets().Resolve(ctx, f.demo.ID, "", []string{"ONE"})
	if err != nil || got["ONE"] != "1" {
		t.Fatalf("after rotation: %v %v", got, err)
	}
	if err := a.db.SetSecret(ctx, a.secretSealer(), store.RepoScope(f.demo.ID), "TWO", "2", by); err != nil {
		t.Fatal(err)
	}
	only, _ := secrets.NewKeyring(raw)
	if got, err := a.db.ResolveSecrets(ctx, only, f.demo.ID, "", []string{"TWO"}); err != nil || got["TWO"] != "2" {
		t.Errorf("new secrets aren't sealed with the new key: %v %v", got, err)
	}
}
