package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"atelier/internal/actions"
	"atelier/internal/secrets"
	"atelier/internal/store"
)

// Secrets and variables for Actions, and environments.
//
//	/{owner}/{repo}/settings/secrets   repository secrets, variables and environments (admin)
//	/settings/secrets?account=<name>   account secrets and variables, inherited by every repo
//
// Values are encrypted at rest (internal/secrets) and are write-only: no page
// or API shows a secret's value again. Only the SecretSource registered with
// the Actions engine reads them, to put them in a job for a runner.

func init() {
	actions.RegisterSecretSource(secretSrc)
	registerFeature(feature{name: "secrets", routes: secretsRoutes})
}

func secretsRoutes(a *App, m *http.ServeMux) {
	secretSrc.app.Store(a)

	m.HandleFunc("GET /{owner}/{repo}/settings/secrets", a.authed(a.secretsRepoPage))
	m.HandleFunc("POST /{owner}/{repo}/settings/secrets", a.authed(a.secretsRepoSet))
	m.HandleFunc("POST /{owner}/{repo}/settings/secrets/delete", a.authed(a.secretsRepoDelete))
	m.HandleFunc("POST /{owner}/{repo}/settings/variables", a.authed(a.secretsRepoVarSet))
	m.HandleFunc("POST /{owner}/{repo}/settings/variables/delete", a.authed(a.secretsRepoVarDelete))
	m.HandleFunc("POST /{owner}/{repo}/settings/environments", a.authed(a.secretsEnvSave))
	m.HandleFunc("POST /{owner}/{repo}/settings/environments/delete", a.authed(a.secretsEnvDelete))

	m.HandleFunc("GET /settings/secrets", a.authed(a.secretsAccountPage))
	m.HandleFunc("POST /settings/secrets", a.authed(a.secretsAccountSet))
	m.HandleFunc("POST /settings/secrets/delete", a.authed(a.secretsAccountDelete))
	m.HandleFunc("POST /settings/variables", a.authed(a.secretsAccountVarSet))
	m.HandleFunc("POST /settings/variables/delete", a.authed(a.secretsAccountVarDelete))
}

// Event kinds. Recipients are limited to people who administer the
// repository (or account), so secret names never reach mere readers.
const (
	EventSecretUpdated      = "secret.updated"
	EventSecretDeleted      = "secret.deleted"
	EventVariableUpdated    = "variable.updated"
	EventVariableDeleted    = "variable.deleted"
	EventEnvironmentUpdated = "environment.updated"
	EventEnvironmentDeleted = "environment.deleted"
)

// maxSecretsForm bounds a secrets form post: a 64 KB value plus the rest.
const maxSecretsForm = store.MaxSecretValue*3 + 16*1024

// ---------- the key ----------

// secretKeys caches the keyring for a data directory and reloads it when a
// key file changes (e.g. after `atelier secrets rotate-key`).
type secretKeys struct {
	mu    sync.Mutex
	dir   string
	kr    *secrets.Keyring
	stamp string
}

var secretKeyCache sync.Map // data dir → *secretKeys

func keysFor(dir string) *secretKeys {
	v, _ := secretKeyCache.LoadOrStore(dir, &secretKeys{dir: dir})
	return v.(*secretKeys)
}

func keyStamp(dir string) string {
	var b strings.Builder
	b.WriteString(os.Getenv(secrets.EnvKey))
	for _, n := range []string{secrets.KeyFile, secrets.NextKeyFile, secrets.OldKeyFile} {
		if fi, err := os.Stat(filepath.Join(dir, n)); err == nil {
			fmt.Fprintf(&b, "|%s:%d:%d", n, fi.ModTime().UnixNano(), fi.Size())
		}
	}
	return b.String()
}

func (k *secretKeys) keyring(create, force bool) (*secrets.Keyring, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := keyStamp(k.dir)
	if k.kr != nil && !force && st == k.stamp {
		return k.kr, nil
	}
	kr, _, err := secrets.Load(k.dir, create)
	if errors.Is(err, os.ErrNotExist) {
		return nil, secrets.ErrUnknownKey
	}
	if err != nil {
		return nil, err
	}
	k.kr, k.stamp = kr, keyStamp(k.dir)
	return kr, nil
}

// Seal and Open make secretKeys a store.SecretSealer.
func (k *secretKeys) Seal(aad, pt []byte) ([]byte, error) {
	kr, err := k.keyring(true, false)
	if err != nil {
		return nil, err
	}
	return kr.Seal(aad, pt)
}

func (k *secretKeys) Open(aad, ct []byte) ([]byte, error) {
	kr, err := k.keyring(false, false)
	if err == nil {
		var pt []byte
		if pt, err = kr.Open(aad, ct); !errors.Is(err, secrets.ErrUnknownKey) {
			return pt, err
		}
	}
	if kr, err = k.keyring(false, true); err != nil {
		return nil, err
	}
	return kr.Open(aad, ct)
}

func (a *App) secretSealer() store.SecretSealer { return keysFor(a.cfg.DataDir) }

// ---------- the Actions hook ----------

var secretSrc = &secretSource{}

// secretSource implements actions.SecretSource for the running App.
type secretSource struct{ app atomic.Pointer[App] }

func (s *secretSource) get() (*App, error) {
	a := s.app.Load()
	if a == nil {
		return nil, errors.New("secrets: server not started")
	}
	return a, nil
}

// checkBranch applies an environment's branch rule to the job's ref (from
// actions.WithEnvironmentRef). It returns the environment, if it exists.
func (a *App) secretsCheckBranch(ctx context.Context, repoID int64, env string) (store.Environment, bool, error) {
	if env == "" {
		return store.Environment{}, false, nil
	}
	e, err := a.db.EnvironmentByName(ctx, repoID, env)
	if errors.Is(err, store.ErrNotFound) {
		return store.Environment{}, false, nil
	}
	if err != nil {
		return store.Environment{}, false, err
	}
	ref := actions.EnvironmentRef(ctx)
	if len(e.Branches) > 0 && (ref == "" || !e.AllowsRef(ref)) {
		return e, true, &actions.EnvironmentBranchError{Environment: e.Name, Ref: ref, Allowed: e.Branches}
	}
	return e, true, nil
}

func (s *secretSource) Resolve(ctx context.Context, repoID int64, env string, names []string) (map[string]string, error) {
	a, err := s.get()
	if err != nil {
		return nil, err
	}
	if _, _, err := a.secretsCheckBranch(ctx, repoID, env); err != nil {
		return nil, err
	}
	return a.db.ResolveSecrets(ctx, a.secretSealer(), repoID, env, names)
}

func (s *secretSource) Variables(ctx context.Context, repoID int64, env string) (map[string]string, error) {
	a, err := s.get()
	if err != nil {
		return nil, err
	}
	return a.db.ResolveVariables(ctx, repoID, env)
}

func (s *secretSource) EnvironmentGate(ctx context.Context, repoID int64, env string) (bool, []string, error) {
	a, err := s.get()
	if err != nil {
		return false, nil, err
	}
	e, ok, err := a.secretsCheckBranch(ctx, repoID, env)
	if err != nil || !ok {
		return false, nil, err
	}
	return e.RequireApproval, append([]string(nil), e.RunnerLabels...), nil
}

// EnvironmentWait implements actions.EnvironmentWaiter.
func (s *secretSource) EnvironmentWait(ctx context.Context, repoID int64, env string) (time.Duration, error) {
	a, err := s.get()
	if err != nil {
		return 0, err
	}
	e, ok, err := a.secretsCheckBranch(ctx, repoID, env)
	if err != nil || !ok {
		return 0, err
	}
	return time.Duration(e.WaitMinutes) * time.Minute, nil
}

var _ actions.EnvironmentWaiter = (*secretSource)(nil)

// ---------- shared helpers ----------

func secretsBy(who Actor) store.SecretsBy {
	return store.SecretsBy{Name: who.Name, Kind: who.Kind, PersonID: who.PersonID}
}

// secretsValue reads a value from a form, turning browser CRLFs into LFs.
func secretsValue(r *http.Request) string {
	return strings.ReplaceAll(r.PostFormValue("value"), "\r\n", "\n")
}

func secretsParse(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxSecretsForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "that form is too large: values can be at most 64 KB", http.StatusRequestEntityTooLarge)
		return false
	}
	return true
}

func secretsErrText(err error) string {
	for _, e := range []error{store.ErrSecretName, store.ErrSecretValue, store.ErrVariableValue, store.ErrEnvName,
		store.ErrEnvExists, store.ErrEnvLabels, store.ErrEnvBranches, store.ErrEnvWait} {
		if errors.Is(err, e) {
			s := e.Error()
			return strings.ToUpper(s[:1]) + s[1:] + "."
		}
	}
	return ""
}

// secretsRetry is which form to open, and what to put back in it: after a
// failed post, or for an Update / Edit link. It never holds a secret value.
type secretsRetry struct {
	Kind  string // "secret", "var", "env"
	Scope string // secretsScope.ID ("repo", "env-3", "account"); for env forms "env-<id>" or "new-env"
	Name  string
	Value string // variables only
	Edit  bool   // editing an existing entry (the name is fixed)
	Env   envForm
}

// retryFromQuery opens a form for an Update / Edit link.
func retryFromQuery(q url.Values) secretsRetry {
	switch {
	case q.Get("update") != "":
		return secretsRetry{Kind: "secret", Scope: q.Get("scope"), Name: store.NormalizeSecretName(q.Get("update")), Edit: true}
	case q.Get("edit_var") != "":
		return secretsRetry{Kind: "var", Scope: q.Get("scope"), Name: store.NormalizeSecretName(q.Get("edit_var")), Edit: true}
	case q.Get("edit_env") != "":
		return secretsRetry{Kind: "env", Scope: "env-" + q.Get("edit_env"), Edit: true}
	}
	return secretsRetry{}
}

// scopeForm is the state of one add / update form.
type scopeForm struct {
	Open, Edit  bool
	Name, Value string
}

// secretsScope is one place secrets and variables live, as shown on a page.
type secretsScope struct {
	ID         string // "repo", "env-3", "account": anchors and form ids
	PostBase   string // forms post to PostBase+"/secrets", PostBase+"/variables"…
	Field      string // hidden field naming the scope: "env", "account" or ""
	FieldValue string
	PageURL    string // this page, ending in "?" or "&"
	Secrets    []store.SecretMeta
	Variables  []store.Variable
	Secret     scopeForm
	Var        scopeForm
}

func (s secretsScope) UpdateURL(name string) string {
	return s.PageURL + "update=" + url.QueryEscape(name) + "&scope=" + s.ID + "#" + s.ID + "-secret-form"
}

func (s secretsScope) EditVarURL(name string) string {
	return s.PageURL + "edit_var=" + url.QueryEscape(name) + "&scope=" + s.ID + "#" + s.ID + "-var-form"
}

// fill opens the scope's forms that rt points at.
func (s *secretsScope) fill(rt secretsRetry) {
	if rt.Scope != s.ID {
		return
	}
	switch rt.Kind {
	case "secret":
		s.Secret = scopeForm{Open: true, Edit: rt.Edit, Name: rt.Name}
	case "var":
		f := scopeForm{Open: true, Edit: rt.Edit, Name: rt.Name, Value: rt.Value}
		if rt.Edit {
			for _, v := range s.Variables {
				if v.Name == rt.Name {
					f.Value = v.Value
				}
			}
		}
		s.Var = f
	}
}

// adminRecipients is every person who may administer repo (for events).
func (a *App) secretsRecipients(ctx context.Context, who Actor, ok func(store.Owner) bool) []int64 {
	out := []int64{}
	if who.PersonID != 0 {
		out = append(out, who.PersonID)
	}
	owners, err := a.db.Owners(ctx)
	if err != nil {
		return out
	}
	for _, o := range owners {
		if o.Kind == "user" && o.ID != who.PersonID && ok(o) {
			out = append(out, o.ID)
		}
	}
	return out
}

func (a *App) secretsEmitRepo(ctx context.Context, v *repoView, who Actor, kind, title, detail string) {
	rcpt := a.secretsRecipients(ctx, who, func(o store.Owner) bool { return a.can(ctx, personActor(o), v.Repo, PermAdmin) })
	a.emit(ctx, Event{Kind: kind, Repo: v.Repo, RepoID: v.Repo.ID, Title: title, URL: v.Base + "/settings/secrets",
		Actor: who, Detail: detail, Recipients: rcpt})
}

func (a *App) secretsEmitAccount(ctx context.Context, acct store.Owner, who Actor, kind, title, detail string) {
	rcpt := a.secretsRecipients(ctx, who, func(o store.Owner) bool { return a.canEditAccount(ctx, personActor(o), acct) })
	a.emit(ctx, Event{Kind: kind, Title: title, URL: "/settings/secrets?account=" + url.QueryEscape(acct.Name),
		Actor: who, Detail: detail, Recipients: rcpt})
}

// ---------- repository page ----------

type inheritedSecret struct {
	store.SecretMeta
	Overridden bool
}

type inheritedVar struct {
	store.Variable
	Overridden bool
}

type envForm struct {
	ID              int64
	Name            string
	RequireApproval bool
	Labels          string
	Branches        string
	WaitMinutes     int
}

func envFormOf(e store.Environment) envForm {
	return envForm{ID: e.ID, Name: e.Name, RequireApproval: e.RequireApproval,
		Labels: strings.Join(e.RunnerLabels, ", "), Branches: strings.Join(e.Branches, ", "), WaitMinutes: e.WaitMinutes}
}

type envView struct {
	store.Environment
	Scope    secretsScope
	Form     envForm
	EditOpen bool
}

func (a *App) secretsRepoPage(w http.ResponseWriter, r *http.Request) {
	notice := ""
	if r.URL.Query().Get("saved") != "" {
		notice = "saved"
	}
	a.renderSecretsRepo(w, r, http.StatusOK, "", notice, retryFromQuery(r.URL.Query()))
}

func (a *App) renderSecretsRepo(w http.ResponseWriter, r *http.Request, status int, errMsg, notice string, retry secretsRetry) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	pageURL := v.Base + "/settings/secrets?"
	repo := secretsScope{ID: "repo", PostBase: v.Base + "/settings", PageURL: pageURL}
	var err error
	if repo.Secrets, err = a.db.Secrets(ctx, store.RepoScope(v.Repo.ID)); err != nil {
		a.fail(w, r, err)
		return
	}
	if repo.Variables, err = a.db.Variables(ctx, store.RepoScope(v.Repo.ID)); err != nil {
		a.fail(w, r, err)
		return
	}
	repo.fill(retry)
	acct, err := a.db.OwnerByID(ctx, v.Repo.OwnerID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	acctSecrets, err := a.db.Secrets(ctx, store.AccountScope(acct.ID))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	acctVars, err := a.db.Variables(ctx, store.AccountScope(acct.ID))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	have := map[string]bool{}
	for _, s := range repo.Secrets {
		have[s.Name] = true
	}
	inh := make([]inheritedSecret, len(acctSecrets))
	for i, s := range acctSecrets {
		inh[i] = inheritedSecret{SecretMeta: s, Overridden: have[s.Name]}
	}
	haveVar := map[string]bool{}
	for _, x := range repo.Variables {
		haveVar[x.Name] = true
	}
	inhVars := make([]inheritedVar, len(acctVars))
	for i, x := range acctVars {
		inhVars[i] = inheritedVar{Variable: x, Overridden: haveVar[x.Name]}
	}
	envs, err := a.db.Environments(ctx, v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	views := make([]envView, len(envs))
	for i, e := range envs {
		id := strconv.FormatInt(e.ID, 10)
		ev := envView{Environment: e, Form: envFormOf(e), Scope: secretsScope{
			ID: "env-" + id, PostBase: v.Base + "/settings", Field: "env", FieldValue: id, PageURL: pageURL}}
		if ev.Scope.Secrets, err = a.db.Secrets(ctx, store.EnvScope(e.ID)); err != nil {
			a.fail(w, r, err)
			return
		}
		if ev.Scope.Variables, err = a.db.Variables(ctx, store.EnvScope(e.ID)); err != nil {
			a.fail(w, r, err)
			return
		}
		ev.Scope.fill(retry)
		if retry.Kind == "env" && retry.Scope == ev.Scope.ID {
			ev.EditOpen = true
			if !retry.Edit {
				ev.Form = retry.Env
			}
		}
		views[i] = ev
	}
	newEnv := envForm{}
	newEnvOpen := retry.Kind == "env" && retry.Scope == "new-env"
	if newEnvOpen {
		newEnv = retry.Env
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	a.render(w, r, "secrets_repo", page{Title: "Secrets and variables · " + v.Repo.FullName(), Nav: "repos",
		Error: errMsg, Notice: notice, Data: map[string]any{
			"V": v, "Repo": repo, "Account": acct,
			"AccountSecrets": inh, "AccountVars": inhVars,
			"CanManageAccount": a.canEditAccount(ctx, who, acct),
			"AccountURL":       "/settings/secrets?account=" + url.QueryEscape(acct.Name),
			"Envs":             views, "NewEnv": newEnv, "NewEnvOpen": newEnvOpen,
		}})
}

// secretsRepoScope works out the scope a repository form targets: the
// repository, or one of its environments (field "env" = environment id).
func (a *App) secretsRepoScope(r *http.Request, v *repoView) (store.SecretScope, store.Environment, error) {
	envID := strings.TrimSpace(r.PostFormValue("env"))
	if envID == "" {
		return store.RepoScope(v.Repo.ID), store.Environment{}, nil
	}
	id, err := strconv.ParseInt(envID, 10, 64)
	if err != nil {
		return store.SecretScope{}, store.Environment{}, store.ErrNotFound
	}
	e, err := a.db.EnvironmentByID(r.Context(), v.Repo.ID, id)
	if err != nil {
		return store.SecretScope{}, store.Environment{}, err
	}
	return store.EnvScope(e.ID), e, nil
}

func scopeLabel(e store.Environment, base string) string {
	if e.ID != 0 {
		return "Environment " + e.Name + " " + base
	}
	return "Repository " + base
}

// repoSecretsForm loads the repository (admin) and parses the form.
func (a *App) repoSecretsForm(w http.ResponseWriter, r *http.Request) (*repoView, store.SecretScope, store.Environment, bool) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok || !secretsParse(w, r) {
		return nil, store.SecretScope{}, store.Environment{}, false
	}
	sc, env, err := a.secretsRepoScope(r, v)
	if err != nil {
		a.fail(w, r, err)
		return nil, store.SecretScope{}, store.Environment{}, false
	}
	return v, sc, env, true
}

func (a *App) secretsRepoSet(w http.ResponseWriter, r *http.Request) {
	v, sc, env, ok := a.repoSecretsForm(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	name := store.NormalizeSecretName(r.PostFormValue("name"))
	if err := a.db.SetSecret(ctx, a.secretSealer(), sc, name, secretsValue(r), secretsBy(who)); err != nil {
		if msg := secretsErrText(err); msg != "" {
			a.renderSecretsRepo(w, r, http.StatusUnprocessableEntity, msg, "", secretsRetry{Kind: "secret", Scope: scopeID(env), Name: name, Edit: r.PostFormValue("edit") != ""})
			return
		}
		a.fail(w, r, err)
		return
	}
	a.secretsEmitRepo(ctx, v, who, EventSecretUpdated, name, scopeLabel(env, "secret updated"))
	http.Redirect(w, r, v.Base+"/settings/secrets?saved=1#"+secretsAnchor(env), http.StatusSeeOther)
}

func scopeID(e store.Environment) string {
	if e.ID != 0 {
		return "env-" + strconv.FormatInt(e.ID, 10)
	}
	return "repo"
}

func secretsAnchor(e store.Environment) string {
	if e.ID != 0 {
		return scopeID(e)
	}
	return "repo-secrets"
}

func (a *App) secretsRepoDelete(w http.ResponseWriter, r *http.Request) {
	v, sc, env, ok := a.repoSecretsForm(w, r)
	if !ok {
		return
	}
	name := store.NormalizeSecretName(r.PostFormValue("name"))
	if err := a.db.DeleteSecret(r.Context(), sc, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.fail(w, r, err)
		return
	}
	a.secretsEmitRepo(r.Context(), v, a.actor(r), EventSecretDeleted, name, scopeLabel(env, "secret deleted"))
	http.Redirect(w, r, v.Base+"/settings/secrets?saved=1#"+secretsAnchor(env), http.StatusSeeOther)
}

func (a *App) secretsRepoVarSet(w http.ResponseWriter, r *http.Request) {
	v, sc, env, ok := a.repoSecretsForm(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	name := store.NormalizeSecretName(r.PostFormValue("name"))
	value := secretsValue(r)
	if err := a.db.SetVariable(ctx, sc, name, value, secretsBy(who)); err != nil {
		if msg := secretsErrText(err); msg != "" {
			a.renderSecretsRepo(w, r, http.StatusUnprocessableEntity, msg, "", secretsRetry{Kind: "var", Scope: scopeID(env), Name: name, Value: value, Edit: r.PostFormValue("edit") != ""})
			return
		}
		a.fail(w, r, err)
		return
	}
	a.secretsEmitRepo(ctx, v, who, EventVariableUpdated, name, scopeLabel(env, "variable updated"))
	anchor := "repo-vars"
	if env.ID != 0 {
		anchor = secretsAnchor(env)
	}
	http.Redirect(w, r, v.Base+"/settings/secrets?saved=1#"+anchor, http.StatusSeeOther)
}

func (a *App) secretsRepoVarDelete(w http.ResponseWriter, r *http.Request) {
	v, sc, env, ok := a.repoSecretsForm(w, r)
	if !ok {
		return
	}
	name := store.NormalizeSecretName(r.PostFormValue("name"))
	if err := a.db.DeleteVariable(r.Context(), sc, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.fail(w, r, err)
		return
	}
	a.secretsEmitRepo(r.Context(), v, a.actor(r), EventVariableDeleted, name, scopeLabel(env, "variable deleted"))
	anchor := "repo-vars"
	if env.ID != 0 {
		anchor = secretsAnchor(env)
	}
	http.Redirect(w, r, v.Base+"/settings/secrets?saved=1#"+anchor, http.StatusSeeOther)
}

func (a *App) secretsEnvSave(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok || !secretsParse(w, r) {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	f := envForm{
		Name:            strings.TrimSpace(r.PostFormValue("name")),
		RequireApproval: r.PostFormValue("require_approval") != "",
		Labels:          r.PostFormValue("labels"),
		Branches:        r.PostFormValue("branches"),
	}
	if s := strings.TrimSpace(r.PostFormValue("id")); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			a.notFound(w, r)
			return
		}
		f.ID = id
	}
	retry := func(msg string) {
		sc := "new-env"
		if f.ID != 0 {
			sc = "env-" + strconv.FormatInt(f.ID, 10)
		}
		a.renderSecretsRepo(w, r, http.StatusUnprocessableEntity, msg, "", secretsRetry{Kind: "env", Scope: sc, Env: f})
	}
	if s := strings.TrimSpace(r.PostFormValue("wait_minutes")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			retry(secretsErrText(store.ErrEnvWait))
			return
		}
		f.WaitMinutes = n
	}
	e := store.Environment{ID: f.ID, RepoID: v.Repo.ID, Name: f.Name, RequireApproval: f.RequireApproval,
		RunnerLabels: store.ParseList(f.Labels), Branches: store.ParseList(f.Branches), WaitMinutes: f.WaitMinutes}
	saved, err := a.db.SaveEnvironment(ctx, e, secretsBy(who))
	if err != nil {
		if msg := secretsErrText(err); msg != "" {
			retry(msg)
			return
		}
		a.fail(w, r, err)
		return
	}
	a.secretsEmitRepo(ctx, v, who, EventEnvironmentUpdated, saved.Name, "Environment saved")
	http.Redirect(w, r, v.Base+"/settings/secrets?saved=1#"+secretsAnchor(saved), http.StatusSeeOther)
}

func (a *App) secretsEnvDelete(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok || !secretsParse(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	e, err := a.db.EnvironmentByID(r.Context(), v.Repo.ID, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.db.DeleteEnvironment(r.Context(), v.Repo.ID, id); err != nil {
		a.fail(w, r, err)
		return
	}
	a.secretsEmitRepo(r.Context(), v, a.actor(r), EventEnvironmentDeleted, e.Name, "Environment deleted")
	http.Redirect(w, r, v.Base+"/settings/secrets?saved=1#environments", http.StatusSeeOther)
}

// ---------- account page ----------

// secretsAccount resolves ?account= (or the form's account field) to an
// account the signed-in person may manage; it writes the error otherwise.
func (a *App) secretsAccount(w http.ResponseWriter, r *http.Request, name string) (store.Owner, bool) {
	ctx := r.Context()
	who := a.actor(r)
	var o store.Owner
	var err error
	if name != "" {
		o, err = a.db.OwnerByName(ctx, name)
	} else {
		o, err = a.db.Person(ctx, who.PersonID)
	}
	if err != nil {
		a.fail(w, r, err)
		return store.Owner{}, false
	}
	if !a.canEditAccount(ctx, who, o) {
		a.forbidden(w, r)
		return store.Owner{}, false
	}
	return o, true
}

func (a *App) secretsAccountPage(w http.ResponseWriter, r *http.Request) {
	o, ok := a.secretsAccount(w, r, r.URL.Query().Get("account"))
	if !ok {
		return
	}
	notice := ""
	if r.URL.Query().Get("saved") != "" {
		notice = "saved"
	}
	a.renderSecretsAccount(w, r, o, http.StatusOK, "", notice, retryFromQuery(r.URL.Query()))
}

func (a *App) renderSecretsAccount(w http.ResponseWriter, r *http.Request, o store.Owner, status int, errMsg, notice string, retry secretsRetry) {
	ctx := r.Context()
	who := a.actor(r)
	owners, err := a.db.Owners(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var accounts []store.Owner
	for _, x := range owners {
		if a.canEditAccount(ctx, who, x) {
			accounts = append(accounts, x)
		}
	}
	sc := secretsScope{ID: "account", PostBase: "/settings", Field: "account", FieldValue: o.Name,
		PageURL: "/settings/secrets?account=" + url.QueryEscape(o.Name) + "&"}
	if sc.Secrets, err = a.db.Secrets(ctx, store.AccountScope(o.ID)); err != nil {
		a.fail(w, r, err)
		return
	}
	if sc.Variables, err = a.db.Variables(ctx, store.AccountScope(o.ID)); err != nil {
		a.fail(w, r, err)
		return
	}
	sc.fill(retry)
	repos, err := a.db.ReposByOwner(ctx, o.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	envKey := strings.TrimSpace(os.Getenv(secrets.EnvKey)) != ""
	a.render(w, r, "secrets_account", page{Title: "Secrets and variables", Nav: "tokens", Error: errMsg, Notice: notice,
		Data: map[string]any{
			"Account": o, "Accounts": accounts, "Scope": sc, "RepoCount": len(repos), "KeyFromEnv": envKey,
		}})
}

func (a *App) accountSecretsForm(w http.ResponseWriter, r *http.Request) (store.Owner, bool) {
	if !secretsParse(w, r) {
		return store.Owner{}, false
	}
	return a.secretsAccount(w, r, r.PostFormValue("account"))
}

func accountSecretsURL(o store.Owner, anchor string) string {
	return "/settings/secrets?account=" + url.QueryEscape(o.Name) + "&saved=1#" + anchor
}

func (a *App) secretsAccountSet(w http.ResponseWriter, r *http.Request) {
	o, ok := a.accountSecretsForm(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	name := store.NormalizeSecretName(r.PostFormValue("name"))
	if err := a.db.SetSecret(ctx, a.secretSealer(), store.AccountScope(o.ID), name, secretsValue(r), secretsBy(who)); err != nil {
		if msg := secretsErrText(err); msg != "" {
			a.renderSecretsAccount(w, r, o, http.StatusUnprocessableEntity, msg, "", secretsRetry{Kind: "secret", Scope: "account", Name: name, Edit: r.PostFormValue("edit") != ""})
			return
		}
		a.fail(w, r, err)
		return
	}
	a.secretsEmitAccount(ctx, o, who, EventSecretUpdated, name, "Account secret updated for "+o.Name)
	http.Redirect(w, r, accountSecretsURL(o, "account-secrets"), http.StatusSeeOther)
}

func (a *App) secretsAccountDelete(w http.ResponseWriter, r *http.Request) {
	o, ok := a.accountSecretsForm(w, r)
	if !ok {
		return
	}
	name := store.NormalizeSecretName(r.PostFormValue("name"))
	if err := a.db.DeleteSecret(r.Context(), store.AccountScope(o.ID), name); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.fail(w, r, err)
		return
	}
	a.secretsEmitAccount(r.Context(), o, a.actor(r), EventSecretDeleted, name, "Account secret deleted for "+o.Name)
	http.Redirect(w, r, accountSecretsURL(o, "account-secrets"), http.StatusSeeOther)
}

func (a *App) secretsAccountVarSet(w http.ResponseWriter, r *http.Request) {
	o, ok := a.accountSecretsForm(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	name := store.NormalizeSecretName(r.PostFormValue("name"))
	value := secretsValue(r)
	if err := a.db.SetVariable(ctx, store.AccountScope(o.ID), name, value, secretsBy(who)); err != nil {
		if msg := secretsErrText(err); msg != "" {
			a.renderSecretsAccount(w, r, o, http.StatusUnprocessableEntity, msg, "", secretsRetry{Kind: "var", Scope: "account", Name: name, Value: value, Edit: r.PostFormValue("edit") != ""})
			return
		}
		a.fail(w, r, err)
		return
	}
	a.secretsEmitAccount(ctx, o, who, EventVariableUpdated, name, "Account variable updated for "+o.Name)
	http.Redirect(w, r, accountSecretsURL(o, "account-vars"), http.StatusSeeOther)
}

func (a *App) secretsAccountVarDelete(w http.ResponseWriter, r *http.Request) {
	o, ok := a.accountSecretsForm(w, r)
	if !ok {
		return
	}
	name := store.NormalizeSecretName(r.PostFormValue("name"))
	if err := a.db.DeleteVariable(r.Context(), store.AccountScope(o.ID), name); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.fail(w, r, err)
		return
	}
	a.secretsEmitAccount(r.Context(), o, a.actor(r), EventVariableDeleted, name, "Account variable deleted for "+o.Name)
	http.Redirect(w, r, accountSecretsURL(o, "account-vars"), http.StatusSeeOther)
}
