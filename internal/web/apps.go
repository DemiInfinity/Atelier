package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"atelier/internal/store"
)

// Apps and bots (Settings → Apps and bots, /settings/apps):
//
//	apps.go           feature, Apps page, bot accounts, the repository Apps page
//	apps_hooks.go     webhook settings pages (repository and account)
//	apps_deliver.go   delivery queue, signing, SSRF guard, retries
//	apps_payloads.go  GitHub-shaped payloads, event → webhook mapping, push detection
//	apps_bots.go      bot tokens, access policy, statuses as checks
//	apps_api.go       the GitHub-compatible /api/v3 subset
//	apps_tether.go    GitHub bots through the Tether bridge

func init() {
	registerFeature(feature{
		name: "apps",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/apps", a.authed(a.appsPage))
			m.HandleFunc("POST /settings/apps/bots", a.authed(a.appsBotCreate))
			m.HandleFunc("GET /settings/apps/bots/{id}", a.authed(a.appsBotPage))
			m.HandleFunc("POST /settings/apps/bots/{id}", a.authed(a.appsBotUpdate))
			m.HandleFunc("POST /settings/apps/bots/{id}/token", a.authed(a.appsBotToken))
			m.HandleFunc("POST /settings/apps/bots/{id}/delete", a.authed(a.appsBotDelete))
			m.HandleFunc("GET /{owner}/{repo}/settings/apps", a.authed(a.appsRepoPage))
			m.HandleFunc("POST /{owner}/{repo}/settings/apps", a.authed(a.appsRepoSave))
			appsHookRoutes(a, m)
			appsAPIRoutes(a, m)
		},
		start: func(ctx context.Context, a *App) {
			go a.appsRefLoop(ctx, 5*time.Second)
			a.appsDeliveryLoop(ctx)
		},
	})
}

// ---------- the Apps page ----------

// appsInstalled is one row of the Installed list.
type appsInstalled struct {
	Name      string
	Initial   string
	Kind      string // tether, native, custom, webhook
	Detail    string
	Where     []string
	Configure string
	Last      time.Time
}

// AppsNativeSource lets another feature (agentic workflows) list its native
// agent bots on the Apps page. Register from an init() with
// registerAppsNativeSource.
type AppsNativeSource func(ctx context.Context, a *App, who Actor) []appsInstalled

var appsNativeSources []AppsNativeSource

func registerAppsNativeSource(f AppsNativeSource) { appsNativeSources = append(appsNativeSources, f) }

type appsBotView struct {
	store.Bot
	Repos []store.Repo
}

type appsPageData struct {
	Installed  []appsInstalled
	Bots       []appsBotView
	Repos      []store.Repo // repositories the viewer administers
	Accounts   []store.Owner
	APIBase    string
	Created    string // a new bot token, shown once
	CreatedBot string
	Form       appsBotForm
	Endpoints  []appsEndpoint
	Tethered   bool // some repository is linked through Tether
}

type appsBotForm struct {
	Name, Initial, Description     string
	Comment, Suggest, Checks, Push bool
	RepoIDs                        map[int64]bool
}

type appsEndpoint struct{ Method, Path, What string }

var appsEndpoints = []appsEndpoint{
	{"GET", "/repos/{owner}/{repo}", "Repository"},
	{"GET", "/repos/{owner}/{repo}/pulls/{n}", "Pull request"},
	{"GET", "/repos/{owner}/{repo}/pulls/{n}/files", "Changed files with patches"},
	{"GET POST", "/repos/{owner}/{repo}/issues/{n}/comments", "Comments on issues and pull requests"},
	{"GET POST", "/repos/{owner}/{repo}/pulls/{n}/comments", "Review comments (Atelier notes on lines)"},
	{"POST", "/repos/{owner}/{repo}/statuses/{sha}", "Commit status (shows as a check)"},
	{"POST PATCH", "/repos/{owner}/{repo}/check-runs", "Check runs (shows as a check)"},
	{"GET POST", "/repos/{owner}/{repo}/issues/{n}/labels", "Labels on issues"},
	{"GET", "/user", "Who the token belongs to"},
}

func (a *App) appsAdminRepos(ctx context.Context, who Actor) []store.Repo {
	all, err := a.db.Repos(ctx)
	if err != nil {
		return nil
	}
	var out []store.Repo
	for _, rp := range all {
		if a.can(ctx, who, rp, PermAdmin) {
			out = append(out, rp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName() < out[j].FullName() })
	return out
}

func (a *App) appsPage(w http.ResponseWriter, r *http.Request) {
	a.renderApps(w, r, appsBotForm{Comment: true, Suggest: true, Checks: true}, "", "", "", http.StatusOK)
}

func (a *App) renderApps(w http.ResponseWriter, r *http.Request, form appsBotForm, created, createdBot, errMsg string, code int) {
	ctx := r.Context()
	who := a.actor(r)
	a.rememberBaseURL(a.baseURL(r))
	d := &appsPageData{Form: form, Created: created, CreatedBot: createdBot, APIBase: a.baseURL(r) + "/api/v3", Endpoints: appsEndpoints}
	d.Repos = a.appsAdminRepos(ctx, who)
	byID := map[int64]store.Repo{}
	for _, rp := range d.Repos {
		byID[rp.ID] = rp
	}
	if owners, err := a.db.Owners(ctx); err == nil {
		for _, o := range owners {
			if a.canEditAccount(ctx, who, o) {
				d.Accounts = append(d.Accounts, o)
			}
		}
	}
	for _, rp := range d.Repos {
		if _, ok := a.appsTetherLinked(ctx, rp.ID); ok {
			d.Tethered = true
			break
		}
	}

	// Bots.
	bots, _ := a.db.Bots(ctx, who.PersonID)
	for _, b := range bots {
		v := appsBotView{Bot: b}
		var where []string
		for _, id := range b.RepoIDs {
			if rp, ok := byID[id]; ok {
				v.Repos = append(v.Repos, rp)
				where = append(where, rp.FullName())
			}
		}
		d.Bots = append(d.Bots, v)
		d.Installed = append(d.Installed, appsInstalled{Name: b.Name, Initial: b.Avatar(), Kind: "custom", Detail: b.Description,
			Where: where, Configure: fmt.Sprintf("/settings/apps/bots/%d", b.ID), Last: b.UpdatedAt})
	}

	// GitHub bots seen through Tether, and native agents seen in comments.
	group := func(kind string, list []store.BotAuthor) {
		idx := map[string]int{}
		for _, b := range list {
			rp, ok := byID[b.RepoID]
			if !ok {
				continue
			}
			key := strings.ToLower(b.Name)
			if i, ok := idx[key]; ok {
				d.Installed[i].Where = appendUnique(d.Installed[i].Where, rp.FullName())
				continue
			}
			it := appsInstalled{Name: b.Name, Initial: appsInitial(b.Name), Kind: kind, Where: []string{rp.FullName()}, Last: b.Last}
			if kind == "tether" {
				it.Detail = "GitHub App reviewing mirrored pull requests"
				it.Configure = "/" + rp.FullName() + "/settings/apps"
			} else {
				it.Detail = "Agent working in your repositories"
				it.Configure = "/" + rp.FullName() + "/actions"
			}
			idx[key] = len(d.Installed)
			d.Installed = append(d.Installed, it)
		}
	}
	tb, _ := a.db.TetherBots(ctx)
	tc, _ := a.db.BotAuthors(ctx, appsCommentKindTethr)
	group("tether", append(tb, tc...))
	ag, _ := a.db.BotAuthors(ctx, "agent")
	group("native", ag)
	for _, src := range appsNativeSources {
		d.Installed = append(d.Installed, src(ctx, a, who)...)
	}

	// Webhooks.
	hooks, _ := a.db.AllWebhooks(ctx)
	for _, h := range hooks {
		it := appsInstalled{Name: appsHookHost(h.URL), Initial: "↗", Kind: "webhook", Detail: strings.Join(h.Events, ", ")}
		if !h.Active {
			it.Detail = "Inactive · " + it.Detail
		}
		if h.RepoID != 0 {
			rp, ok := byID[h.RepoID]
			if !ok {
				continue
			}
			it.Where, it.Configure = []string{rp.FullName()}, fmt.Sprintf("/%s/settings/webhooks/%d", rp.FullName(), h.ID)
		} else {
			o, err := a.db.OwnerByID(ctx, h.OwnerID)
			if err != nil || !a.canEditAccount(ctx, who, o) {
				continue
			}
			it.Where, it.Configure = []string{"All of " + o.Name}, fmt.Sprintf("%s/%d", appsAccountHookBase, h.ID)
		}
		d.Installed = append(d.Installed, it)
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "apps_index", page{Title: "Apps and bots", Nav: "tokens", Error: errMsg, Data: d})
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func appsInitial(name string) string {
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return strings.ToUpper(string(r))
		}
	}
	return "?"
}

func appsHookHost(raw string) string {
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return raw
	}
	return s
}

// ---------- bots ----------

var appsBotNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

func (a *App) appsBotFormFrom(r *http.Request) appsBotForm {
	f := appsBotForm{
		Name: strings.TrimSpace(r.FormValue("name")), Initial: strings.TrimSpace(r.FormValue("initial")),
		Description: strings.TrimSpace(r.FormValue("description")),
		Comment:     r.FormValue("comment") != "", Suggest: r.FormValue("suggest") != "",
		Checks: r.FormValue("checks") != "", Push: r.FormValue("push") != "", RepoIDs: map[int64]bool{},
	}
	_ = r.ParseForm()
	for _, s := range r.Form["repos"] {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			f.RepoIDs[id] = true
		}
	}
	return f
}

// appsBotValidate checks a bot form and returns the bot to save.
func (a *App) appsBotValidate(ctx context.Context, who Actor, f appsBotForm, creating bool) (store.Bot, error) {
	b := store.Bot{PersonID: who.PersonID, Name: f.Name, Description: f.Description,
		CanComment: f.Comment, CanSuggest: f.Suggest, CanChecks: f.Checks, CanPush: f.Push}
	if creating {
		if !appsBotNameRe.MatchString(f.Name) {
			return b, errors.New("Use letters, digits and dashes for the bot's name (up to 39).")
		}
		if _, err := a.db.OwnerByName(ctx, f.Name); err == nil {
			return b, errors.New("That name belongs to an account. Pick another name for the bot.")
		}
	}
	if utf8.RuneCountInString(f.Description) > 200 {
		return b, errors.New("Keep the description under 200 characters.")
	}
	if f.Initial != "" {
		if utf8.RuneCountInString(f.Initial) != 1 || unicode.IsSpace([]rune(f.Initial)[0]) {
			return b, errors.New("The avatar initial is a single character.")
		}
		b.Initial = strings.ToUpper(f.Initial)
	}
	for id := range f.RepoIDs {
		rp, err := a.db.RepoByIDApps(ctx, id)
		if err != nil || !a.can(ctx, who, rp, PermAdmin) {
			return b, errors.New("Pick repositories you administer.")
		}
		b.RepoIDs = append(b.RepoIDs, id)
	}
	if len(b.RepoIDs) == 0 {
		return b, errors.New("Choose at least one repository the bot can work in.")
	}
	sort.Slice(b.RepoIDs, func(i, j int) bool { return b.RepoIDs[i] < b.RepoIDs[j] })
	return b, nil
}

// appsBotIssueToken makes a new token for a bot (revoking any old one).
func (a *App) appsBotIssueToken(ctx context.Context, b store.Bot) (string, error) {
	plain, err := a.db.CreateToken(ctx, "bot: "+b.Name, []string{"bot"}, 0)
	if err != nil {
		return "", err
	}
	if b.PersonID != 0 {
		if err := a.db.AssignTokenPerson(ctx, plain, b.PersonID); err != nil {
			return "", err
		}
	}
	return plain, a.db.SetBotToken(ctx, b.ID, plain)
}

func (a *App) appsBotCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	f := a.appsBotFormFrom(r)
	b, err := a.appsBotValidate(ctx, who, f, true)
	if err == nil {
		b, err = a.db.SaveBot(ctx, b)
	}
	if err != nil {
		msg := err.Error()
		if !errors.Is(err, store.ErrBotNameTaken) && !strings.HasSuffix(msg, ".") {
			a.fail(w, r, err)
			return
		}
		a.renderApps(w, r, f, "", "", strings.ToUpper(msg[:1])+msg[1:], http.StatusUnprocessableEntity)
		return
	}
	plain, err := a.appsBotIssueToken(ctx, b)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.renderApps(w, r, appsBotForm{Comment: true, Suggest: true, Checks: true}, plain, b.Name, "", http.StatusOK)
}

func (a *App) appsLoadBot(w http.ResponseWriter, r *http.Request) (store.Bot, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return store.Bot{}, false
	}
	b, err := a.db.BotByID(r.Context(), id)
	if err != nil || b.PersonID != a.actor(r).PersonID {
		a.notFound(w, r)
		return store.Bot{}, false
	}
	return b, true
}

type appsBotPageData struct {
	Bot     store.Bot
	Form    appsBotForm
	Repos   []store.Repo
	Created string
	APIBase string
}

func (a *App) renderBot(w http.ResponseWriter, r *http.Request, b store.Bot, f appsBotForm, created, errMsg, notice string, code int) {
	d := &appsBotPageData{Bot: b, Form: f, Repos: a.appsAdminRepos(r.Context(), a.actor(r)), Created: created,
		APIBase: a.baseURL(r) + "/api/v3"}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "apps_bot", page{Title: b.Name + " · Apps and bots", Nav: "tokens", Error: errMsg, Notice: notice, Data: d})
}

func appsFormOf(b store.Bot) appsBotForm {
	f := appsBotForm{Name: b.Name, Initial: b.Initial, Description: b.Description, Comment: b.CanComment, Suggest: b.CanSuggest,
		Checks: b.CanChecks, Push: b.CanPush, RepoIDs: map[int64]bool{}}
	for _, id := range b.RepoIDs {
		f.RepoIDs[id] = true
	}
	return f
}

func (a *App) appsBotPage(w http.ResponseWriter, r *http.Request) {
	b, ok := a.appsLoadBot(w, r)
	if !ok {
		return
	}
	notice := ""
	if r.URL.Query().Get("saved") != "" {
		notice = "saved"
	}
	a.renderBot(w, r, b, appsFormOf(b), "", "", notice, http.StatusOK)
}

func (a *App) appsBotUpdate(w http.ResponseWriter, r *http.Request) {
	b, ok := a.appsLoadBot(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	f := a.appsBotFormFrom(r)
	f.Name = b.Name
	nb, err := a.appsBotValidate(ctx, a.actor(r), f, false)
	if err != nil {
		a.renderBot(w, r, b, f, "", err.Error(), "", http.StatusUnprocessableEntity)
		return
	}
	nb.ID, nb.PersonID = b.ID, b.PersonID
	if _, err := a.db.SaveBot(ctx, nb); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/settings/apps/bots/%d?saved=1", b.ID), http.StatusSeeOther)
}

func (a *App) appsBotToken(w http.ResponseWriter, r *http.Request) {
	b, ok := a.appsLoadBot(w, r)
	if !ok {
		return
	}
	plain, err := a.appsBotIssueToken(r.Context(), b)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.renderBot(w, r, b, appsFormOf(b), plain, "", "", http.StatusOK)
}

func (a *App) appsBotDelete(w http.ResponseWriter, r *http.Request) {
	b, ok := a.appsLoadBot(w, r)
	if !ok {
		return
	}
	if err := a.db.DeleteBot(r.Context(), b.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/apps", http.StatusSeeOther)
}

// ---------- the repository Apps page ----------

type appsMirrorView struct {
	store.TetherMirror
	Number int64
	Title  string
}

type appsRepoData struct {
	V        *repoView
	TetherOn bool
	Linked   string
	IsLinked bool
	Mirrors  []appsMirrorView
	Hooks    []store.Webhook
	Bots     []store.Bot
}

func (a *App) appsRepoPage(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	d := &appsRepoData{V: v, TetherOn: a.db.TetherReview(ctx, v.Repo.ID)}
	d.Linked, d.IsLinked = a.appsTetherLinked(ctx, v.Repo.ID)
	ms, _ := a.db.TetherMirrors(ctx, v.Repo.ID)
	for _, m := range ms {
		mv := appsMirrorView{TetherMirror: m}
		if p, err := a.db.PullByIDApps(ctx, m.PullID); err == nil {
			mv.Number, mv.Title = p.Number, p.Title
		}
		d.Mirrors = append(d.Mirrors, mv)
	}
	d.Hooks, _ = a.db.RepoWebhooks(ctx, v.Repo.ID)
	bots, _ := a.db.Bots(ctx, a.actor(r).PersonID)
	for _, b := range bots {
		if b.HasRepo(v.Repo.ID) {
			d.Bots = append(d.Bots, b)
		}
	}
	notice := ""
	if r.URL.Query().Get("saved") != "" {
		notice = "saved"
	}
	a.render(w, r, "apps_repo", page{Title: "Apps and bots · " + v.Repo.FullName(), Nav: "repos", Notice: notice, Data: d})
}

func (a *App) appsRepoSave(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	if err := a.db.SetTetherReview(r.Context(), v.Repo.ID, r.FormValue("tether_review") != ""); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/apps?saved=1", http.StatusSeeOther)
}
