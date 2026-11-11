package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"atelier/internal/gitrepo"
	"atelier/internal/pagesdeploy"
	"atelier/internal/store"
)

// /{owner}/{repo}/settings/pages. Every handler goes through loadRepo with the
// "settings" tab, which needs PermAdmin.

type pagesSettingsData struct {
	V          *repoView
	Site       store.PagesSite
	On         bool // ATELIER_PAGES_URL is set and usable
	SiteURL    string
	PagesBase  string
	Private    bool
	UserSite   string // the <owner>.pages repository name
	IsUserSite bool
	Live       *store.PagesDeploy
	Deploys    []pagesDeployView
	MaxSize    string
	MaxFiles   int
	Keep       int
	Workflow   string
	Folder     string
	BadBranch  bool
}

type pagesDeployView struct {
	store.PagesDeploy
	Live     bool
	Size     string
	Rollback bool
}

const pagesWorkflowExample = `name: Pages
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: [linux]
    steps:
      - uses: actions/checkout@v4
      - run: npm ci && npm run build
      - uses: atelier/deploy-pages@v1
        with:
          path: dist`

func (a *App) pagesSettings(w http.ResponseWriter, r *http.Request) {
	a.pagesRender(w, r, "", http.StatusOK)
}

func (a *App) pagesRender(w http.ResponseWriter, r *http.Request, errMsg string, code int) {
	v, g, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	ps, err := a.db.PagesSite(ctx, v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if ps.UpdatedAt.IsZero() && v.Default != "" {
		ps.Branch = v.Default
	}
	files := a.pagesFiles()
	d := &pagesSettingsData{
		V: v, Site: ps, On: a.pagesEnabled(), SiteURL: a.pagesSiteURL(v.Repo), PagesBase: a.pagesBase(),
		Private: v.Repo.Private, UserSite: pagesUserSiteName(v.Repo.Owner),
		IsUserSite: strings.EqualFold(v.Repo.Name, pagesUserSiteName(v.Repo.Owner)),
		MaxSize:    pagesdeploy.HumanBytes(files.Limits.MaxBytes), MaxFiles: files.Limits.MaxFiles, Keep: a.pagesKeep(),
		Workflow: pagesWorkflowExample, Folder: "/" + ps.Folder,
	}
	if ps.Enabled && ps.Source == store.PagesFromBranch && !v.Empty && !pagesBranchExists(ctx, g, ps.Branch) {
		d.BadBranch = true
	}
	ds, _ := a.db.PagesDeploys(ctx, v.Repo.ID, 30)
	for _, dp := range ds {
		dv := pagesDeployView{PagesDeploy: dp, Live: dp.ID == ps.LiveID, Size: pagesdeploy.HumanBytes(dp.Bytes)}
		dv.Rollback = !dv.Live && dp.Status == "ready" && !v.Repo.Private
		if dv.Live {
			live := dp
			d.Live = &live
		}
		d.Deploys = append(d.Deploys, dv)
	}
	notice := ""
	if r.URL.Query().Get("saved") != "" {
		notice = "saved"
	}
	if errMsg == "" && r.URL.Query().Get("failed") != "" {
		errMsg = "The deploy failed. The deploy history below says why."
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "pages_settings", page{Title: "Pages · " + v.Repo.FullName(), Nav: "repos", Notice: notice, Error: errMsg, Data: d})
}

func (a *App) pagesSettingsSave(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	if v.Repo.Private {
		a.pagesRender(w, r, errPagesPrivate.Error()+". Make the repository public first.", http.StatusBadRequest)
		return
	}
	ps, err := a.db.PagesSite(ctx, v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	source := r.FormValue("source")
	if source != store.PagesFromBranch && source != store.PagesFromActions {
		a.pagesRender(w, r, "Choose where the site comes from.", http.StatusBadRequest)
		return
	}
	branch := strings.TrimSpace(r.FormValue("branch"))
	if branch == "" {
		branch = v.Default
	}
	if !gitrepo.ValidBranchName(branch) {
		a.pagesRender(w, r, "That isn't a valid branch name.", http.StatusBadRequest)
		return
	}
	folder, ok := pagesdeploy.CleanFolder(r.FormValue("folder"))
	if !ok {
		a.pagesRender(w, r, "The folder must be a path inside the repository, such as /docs.", http.StatusBadRequest)
		return
	}
	if source == store.PagesFromActions {
		folder = ""
	}
	ps.Enabled, ps.Source, ps.Branch, ps.Folder = true, source, branch, folder
	if err := a.db.SavePagesSite(ctx, ps); err != nil {
		a.fail(w, r, err)
		return
	}
	if source == store.PagesFromBranch && !v.Empty && pagesBranchExists(ctx, g, branch) {
		if _, err := a.pagesDeployBranch(ctx, v.Repo, ps, "", a.actor(r).Name); err != nil {
			http.Redirect(w, r, v.Base+"/settings/pages?failed=1", http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, v.Base+"/settings/pages?saved=1", http.StatusSeeOther)
}

func (a *App) pagesDeployNow(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	ctx := r.Context()
	ps, err := a.db.PagesSite(ctx, v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !ps.Enabled || ps.Source != store.PagesFromBranch {
		a.pagesRender(w, r, "Only sites published from a branch can be deployed from here; workflows deploy the others.", http.StatusBadRequest)
		return
	}
	if _, err := a.pagesDeployBranch(ctx, v.Repo, ps, "", a.actor(r).Name); err != nil {
		if errors.Is(err, errPagesPrivate) {
			a.pagesRender(w, r, err.Error()+".", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, v.Base+"/settings/pages?failed=1", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/pages?saved=1", http.StatusSeeOther)
}

func (a *App) pagesRollback(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.FormValue("deploy"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.pagesRollbackTo(r.Context(), v.Repo, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			a.notFound(w, r)
			return
		}
		a.pagesRender(w, r, "Couldn't roll back: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/pages?saved=1", http.StatusSeeOther)
}

func (a *App) pagesUnpublish(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	if err := a.pagesUnpublishRepo(r.Context(), v.Repo); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/settings/pages?saved=1", http.StatusSeeOther)
}
