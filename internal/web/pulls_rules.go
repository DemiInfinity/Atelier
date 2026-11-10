package web

import (
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"atelier/internal/store"
)

// Branch rules: /{owner}/{repo}/settings/branches. Rules are enforced on push
// by the pre-receive hook (cmd/atelier/hook.go) and on merge by pullStatus.

func pullsRulesRoutes(a *App, m *http.ServeMux) {
	m.HandleFunc("GET /{owner}/{repo}/settings/branches", a.authed(a.branchRulesPage))
	m.HandleFunc("POST /{owner}/{repo}/settings/branches", a.authed(a.branchRuleCreate))
	m.HandleFunc("POST /{owner}/{repo}/settings/branches/identity", a.authed(a.branchRulesIdentity))
	m.HandleFunc("POST /{owner}/{repo}/settings/branches/{id}", a.authed(a.branchRuleUpdate))
	m.HandleFunc("POST /{owner}/{repo}/settings/branches/{id}/delete", a.authed(a.branchRuleDelete))
}

type branchRuleRow struct {
	store.BranchRule
	Matches []string // existing branches it protects
	Checks  []string // required checks (actions_checks.go)
}

func (a *App) renderBranchRules(w http.ResponseWriter, r *http.Request, form store.BranchRule, errMsg, notice string, code int) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	rules, err := a.db.BranchRules(ctx, v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rows := make([]branchRuleRow, len(rules))
	var ruleIDs []int64
	for _, rl := range rules {
		ruleIDs = append(ruleIDs, rl.ID)
	}
	checks, err := a.db.BranchRuleChecks(ctx, ruleIDs)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for i, rl := range rules {
		rows[i].BranchRule = rl
		rows[i].Checks = checks[rl.ID]
		for _, b := range v.Branches {
			if store.MatchBranch(rl.Pattern, b) {
				rows[i].Matches = append(rows[i].Matches, b)
			}
		}
	}
	editing := int64(0)
	if id, err := strconv.ParseInt(r.URL.Query().Get("edit"), 10, 64); err == nil && form.ID == 0 && form.Pattern == "" {
		if rl, err := a.db.BranchRuleByID(ctx, v.Repo.ID, id); err == nil {
			form, editing = rl, rl.ID
		}
	} else if form.ID != 0 {
		editing = form.ID
	}
	if form.ID == 0 && form.Pattern == "" && errMsg == "" {
		form = store.BranchRule{Pattern: v.Default, RequirePR: true, BlockForcePush: true}
		if len(rules) > 0 {
			form.Pattern = ""
		}
	}
	// Required checks in the form: what was posted, or the rule being edited.
	formChecks := strings.Join(parseCheckNames(r.FormValue("required_checks")), ", ")
	if r.Method == http.MethodGet && editing != 0 {
		formChecks = strings.Join(checks[editing], ", ")
	}
	email, _ := a.db.Setting(ctx, settingPullsCommitterEmail)
	ident := a.pullIdentity(ctx)
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "pulls_rules", page{Title: "Branch rules · " + v.Repo.FullName(), Nav: "repos", Error: errMsg, Notice: notice,
		Data: map[string]any{"V": v, "Rules": rows, "Form": form, "Editing": editing, "Email": email, "Identity": ident,
			"Checks": formChecks}})
}

func (a *App) branchRulesPage(w http.ResponseWriter, r *http.Request) {
	a.renderBranchRules(w, r, store.BranchRule{}, "", r.URL.Query().Get("saved"), http.StatusOK)
}

func branchRuleFromForm(r *http.Request) store.BranchRule {
	on := func(k string) bool { return r.FormValue(k) != "" }
	return store.BranchRule{
		Pattern:              strings.TrimSpace(r.FormValue("pattern")),
		RequirePR:            on("require_pr"),
		BlockForcePush:       on("block_force_push"),
		RequireResolvedNotes: on("require_resolved_notes"),
		LinearHistory:        on("linear_history"),
	}
}

const branchPatternHelp = "Use a branch name or a pattern like release/* (letters, numbers, . _ - / and *)."

func (a *App) branchRuleCreate(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	rl := branchRuleFromForm(r)
	rl.RepoID = v.Repo.ID
	if !store.ValidBranchPattern(rl.Pattern) {
		a.renderBranchRules(w, r, rl, branchPatternHelp, "", http.StatusUnprocessableEntity)
		return
	}
	created, err := a.db.CreateBranchRule(r.Context(), rl)
	if err != nil {
		if errors.Is(err, store.ErrBranchRuleExists) {
			a.renderBranchRules(w, r, rl, "There's already a rule for "+rl.Pattern+".", "", http.StatusConflict)
			return
		}
		a.fail(w, r, err)
		return
	}
	if err := a.db.SetBranchRuleChecks(r.Context(), created.ID, parseCheckNames(r.FormValue("required_checks"))); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/branches?saved=1", http.StatusSeeOther)
}

func (a *App) branchRuleUpdate(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	rl := branchRuleFromForm(r)
	rl.ID, rl.RepoID = id, v.Repo.ID
	if !store.ValidBranchPattern(rl.Pattern) {
		a.renderBranchRules(w, r, rl, branchPatternHelp, "", http.StatusUnprocessableEntity)
		return
	}
	switch err := a.db.UpdateBranchRule(r.Context(), rl); {
	case errors.Is(err, store.ErrBranchRuleExists):
		a.renderBranchRules(w, r, rl, "There's already a rule for "+rl.Pattern+".", "", http.StatusConflict)
		return
	case err != nil:
		a.fail(w, r, err)
		return
	}
	if err := a.db.SetBranchRuleChecks(r.Context(), rl.ID, parseCheckNames(r.FormValue("required_checks"))); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/branches?saved=1", http.StatusSeeOther)
}

func (a *App) branchRuleDelete(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.db.DeleteBranchRule(r.Context(), v.Repo.ID, id); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/branches?saved=1", http.StatusSeeOther)
}

// branchRulesIdentity sets the email on merge commits Atelier makes (it's
// account-wide; it sits here because this is where merging is configured).
func (a *App) branchRulesIdentity(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	email := strings.TrimSpace(r.FormValue("email"))
	if email != "" {
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email || strings.ContainsAny(email, "<>\n") {
			a.renderBranchRules(w, r, store.BranchRule{}, "That doesn't look like an email address.", "", http.StatusUnprocessableEntity)
			return
		}
	}
	if err := a.db.SetSetting(r.Context(), settingPullsCommitterEmail, email); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/branches?saved=1", http.StatusSeeOther)
}
