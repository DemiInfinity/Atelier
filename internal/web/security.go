package web

// Security: /{owner}/{repo}/security.
//
//   security.go          the page, settings, alert actions, one-time bypasses
//   security_scan.go     Actions log scanning and the full-history scan
//   security_deps.go     dependency updates (Go modules, npm) and update PRs
//
// Secret detection lives in internal/secretscan, push protection in
// cmd/atelier/hook_secretscan.go, version lookups and manifest edits in
// internal/depupdates. Anyone who can read the repository sees the three
// counters; details (where a secret is, its partial value, dependency names)
// need admin.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"atelier/internal/store"
)

const (
	// EventSecretDetected is emitted when scanning finds a new secret.
	EventSecretDetected = "secret.detected"

	settingSecurityBaseURL = "security.base_url"
	secBypassTTL           = time.Hour
)

func init() {
	registerFeature(feature{
		name: "security",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /{owner}/{repo}/security", a.authed(a.securityPage))
			m.HandleFunc("POST /{owner}/{repo}/security/settings", a.authed(a.securitySettingsSave))
			m.HandleFunc("POST /{owner}/{repo}/security/alerts/{id}", a.authed(a.securityAlertStatus))
			m.HandleFunc("POST /{owner}/{repo}/security/push-blocks/{id}/allow", a.authed(a.securityAllowOnce))
			m.HandleFunc("POST /{owner}/{repo}/security/scan", a.authed(a.securityScanNow))
			m.HandleFunc("POST /{owner}/{repo}/security/dependencies/check", a.authed(a.securityDepsCheckNow))
		},
		start: func(ctx context.Context, a *App) { a.secDepsScheduler(ctx) },
	})
	registerEventHandler(securityEventHandler)
}

type secAlertView struct {
	store.SecretAlert
	Where, WhereURL string
	Open            bool
}

type secBlockView struct {
	store.SecretPushBlock
	Allowed bool // an unused bypass is waiting
}

type secDepView struct {
	store.DepUpdate
	PRURL string
}

type secPageData struct {
	V                           *repoView
	CanAdmin                    bool
	SecretsOpen, Updates, Vulns int
	Alerts                      []secAlertView
	Blocks                      []secBlockView
	History, DepsScan           *store.SecurityScan
	Deps                        []secDepView
	Settings                    store.SecuritySettings
	GoProxy, NpmRegistry        string
	NpmViaAtelier               bool
	SecretsURL                  string
	Flash                       string
	ScanMaxCommits              int
}

// secFlashes are the fixed messages ?done= can show (never reflected text).
var secFlashes = map[string]string{
	"settings": "Security settings saved.",
	"alert":    "Alert updated.",
	"allow":    "Allowed once: push again within an hour.",
	"scan":     "History scan started. Refresh in a moment to see the results.",
	"scanning": "A history scan is already running.",
	"deps":     "Dependency check started. Refresh in a moment to see the results.",
	"checking": "A dependency check is already running.",
	"nodeps":   "There's no go.mod or package.json on the default branch.",
}

func (a *App) securityPage(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "security")
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	d := &secPageData{V: v, CanAdmin: a.can(ctx, who, v.Repo, PermAdmin), Flash: secFlashes[r.URL.Query().Get("done")],
		ScanMaxCommits: secHistoryMaxCommits}
	var err error
	if d.SecretsOpen, err = a.db.OpenSecretAlertCount(ctx, v.Repo.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	if d.Updates, d.Vulns, err = a.db.DepCounts(ctx, v.Repo.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	if d.CanAdmin {
		if err := a.secLoadDetails(ctx, r, d); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	a.render(w, r, "security_repo", page{Title: "Security · " + v.Repo.FullName(), Nav: "repos", Data: d})
}

func (a *App) secLoadDetails(ctx context.Context, r *http.Request, d *secPageData) error {
	v := d.V
	// Remember how this server is reached, for push protection's messages.
	if base := a.baseURL(r); base != "" {
		if cur, _ := a.db.Setting(ctx, settingSecurityBaseURL); cur != base {
			_ = a.db.SetSetting(ctx, settingSecurityBaseURL, base)
		}
	}
	alerts, err := a.db.SecretAlerts(ctx, v.Repo.ID, 200)
	if err != nil {
		return err
	}
	for _, al := range alerts {
		av := secAlertView{SecretAlert: al, Open: al.Status == store.AlertOpen}
		if al.Kind == "log" {
			av.Where = fmt.Sprintf("Actions run #%d · %s", al.RunSeq, al.JobName)
			if al.StepName != "" {
				av.Where += " · step “" + al.StepName + "”"
			}
			av.Where += fmt.Sprintf(" · line %d", al.Line)
			av.WhereURL = fmt.Sprintf("%s/actions/runs/%d", v.Base, al.RunSeq)
		} else {
			av.Where = fmt.Sprintf("%s:%d", al.Path, al.Line)
			if al.CommitSHA != "" {
				av.WhereURL = fmt.Sprintf("%s/blob/%s/%s#L%d", v.Base, al.CommitSHA, secEscapePath(al.Path), al.Line)
			}
		}
		d.Alerts = append(d.Alerts, av)
	}
	blocks, err := a.db.SecretPushBlocks(ctx, v.Repo.ID, 20)
	if err != nil {
		return err
	}
	pending, err := a.db.PendingSecretBypasses(ctx, v.Repo.ID)
	if err != nil {
		return err
	}
	for _, b := range blocks {
		if time.Since(b.CreatedAt) > 30*24*time.Hour {
			continue
		}
		d.Blocks = append(d.Blocks, secBlockView{SecretPushBlock: b, Allowed: pending[b.Fingerprint]})
	}
	if sc, err := a.db.SecurityScanState(ctx, v.Repo.ID, "history"); err == nil {
		d.History = &sc
	}
	if sc, err := a.db.SecurityScanState(ctx, v.Repo.ID, "deps"); err == nil {
		d.DepsScan = &sc
	}
	deps, err := a.db.DepUpdates(ctx, v.Repo.ID)
	if err != nil {
		return err
	}
	for _, u := range deps {
		dv := secDepView{DepUpdate: u}
		if u.PRNumber > 0 {
			dv.PRURL = fmt.Sprintf("%s/pulls/%d", v.Base, u.PRNumber)
		}
		d.Deps = append(d.Deps, dv)
	}
	if d.Settings, err = a.db.SecuritySettings(ctx, v.Repo.ID); err != nil {
		return err
	}
	d.GoProxy = a.secGoProxySetting(ctx)
	d.NpmRegistry = a.secNpmRegistrySetting(ctx)
	_, d.NpmViaAtelier = a.npmUpstream(ctx)
	d.SecretsURL = v.Base + "/settings/secrets#repo-secrets"
	return nil
}

func secEscapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// secAdminRepo loads the repository for a security change: admins only.
func (a *App) secAdminRepo(w http.ResponseWriter, r *http.Request) (*repoView, bool) {
	v, _, ok := a.loadRepo(w, r, "security")
	if !ok {
		return nil, false
	}
	if !a.can(r.Context(), a.actor(r), v.Repo, PermAdmin) {
		a.forbidden(w, r)
		return nil, false
	}
	return v, true
}

func (a *App) secDone(w http.ResponseWriter, r *http.Request, v *repoView, done, anchor string) {
	http.Redirect(w, r, v.Base+"/security?done="+done+anchor, http.StatusSeeOther)
}

func (a *App) securitySettingsSave(w http.ResponseWriter, r *http.Request) {
	v, ok := a.secAdminRepo(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	st := store.SecuritySettings{
		PushProtection: r.FormValue("push_protection") == "on",
		ScanLogs:       r.FormValue("scan_logs") == "on",
		SecurityPRs:    r.FormValue("security_prs") == "on",
		AllPRs:         r.FormValue("all_prs") == "on",
	}
	if err := a.db.SetSecuritySettings(ctx, v.Repo.ID, st); err != nil {
		a.fail(w, r, err)
		return
	}
	// Registry settings apply to every repository.
	if _, ok := r.Form["goproxy"]; ok {
		if err := a.secSaveRegistries(ctx, r.FormValue("goproxy"), r.FormValue("npm_registry")); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	a.secDone(w, r, v, "settings", "#settings")
}

func (a *App) securityAlertStatus(w http.ResponseWriter, r *http.Request) {
	v, ok := a.secAdminRepo(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	status := r.FormValue("status")
	switch status {
	case store.AlertOpen, store.AlertResolved, store.AlertFalsePositive:
	default:
		http.Error(w, "unknown status", http.StatusBadRequest)
		return
	}
	if err := a.db.SetSecretAlertStatus(r.Context(), v.Repo.ID, id, status, a.actor(r).Name); err != nil {
		a.fail(w, r, err)
		return
	}
	a.secDone(w, r, v, "alert", "#secret-scanning")
}

func (a *App) securityAllowOnce(w http.ResponseWriter, r *http.Request) {
	v, ok := a.secAdminRepo(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	b, err := a.db.SecretPushBlockByID(ctx, v.Repo.ID, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.db.CreateSecretBypass(ctx, v.Repo.ID, b.Fingerprint, a.actor(r).Name, secBypassTTL); err != nil {
		a.fail(w, r, err)
		return
	}
	a.secDone(w, r, v, "allow", "#push-blocks")
}

func (a *App) securityScanNow(w http.ResponseWriter, r *http.Request) {
	v, ok := a.secAdminRepo(w, r)
	if !ok {
		return
	}
	started, err := a.secStartHistoryScan(r.Context(), v.Repo)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	done := "scan"
	if !started {
		done = "scanning"
	}
	a.secDone(w, r, v, done, "#secret-scanning")
}

func (a *App) securityDepsCheckNow(w http.ResponseWriter, r *http.Request) {
	v, ok := a.secAdminRepo(w, r)
	if !ok {
		return
	}
	done, err := a.secStartDepsCheck(r.Context(), v.Repo)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.secDone(w, r, v, done, "#dependencies")
}

// secAdmins are the people who may see a repository's security details: the
// only ones told about new alerts.
func (a *App) secAdmins(ctx context.Context, repo store.Repo) []int64 {
	owners, err := a.db.Owners(ctx)
	if err != nil {
		return nil
	}
	var out []int64
	for _, o := range owners {
		if o.Kind == "user" && a.can(ctx, personActor(o), repo, PermAdmin) {
			out = append(out, o.ID)
		}
	}
	return out
}

// secNotify tells the repository's admins about newly found secrets.
func (a *App) secNotify(ctx context.Context, repo store.Repo, n int, where, rule string) {
	if n == 0 {
		return
	}
	admins := a.secAdmins(ctx, repo)
	if len(admins) == 0 {
		return
	}
	title := "Possible " + rule + " found in " + where
	if n > 1 {
		title = fmt.Sprintf("%d possible secrets found in %s", n, where)
	}
	a.emit(ctx, Event{Kind: EventSecretDetected, Repo: repo, RepoID: repo.ID, Title: title,
		URL: "/" + repo.FullName() + "/security#secret-scanning", Actor: Actor{Kind: ActorSystem, Name: "secret scanning"},
		Detail: "Rotate it, then store the new value as an Actions secret.", Recipients: admins})
}
