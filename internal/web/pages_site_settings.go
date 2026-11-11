package web

import (
	"context"
	"errors"
	"fmt"
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
	Private    bool   // private and can't publish (no verified custom domain)
	PrivSite   bool   // private, served only on its verified custom domain
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

	Domain       *pagesDomainView
	Previews     bool
	PreviewRows  []pagesPreviewView
	AppHost      string
	TunnelListen string
}

type pagesDomainView struct {
	store.PagesDomain
	URL     string
	TXTName string
	FileURL string
	Forward string // where the proxy or tunnel forwards to
}

type pagesPreviewView struct {
	store.PagesPreview
	URL     string
	Title   string
	PullURL string
	Short   string
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
	mayPublish := a.pagesMayPublish(ctx, v.Repo)
	d := &pagesSettingsData{
		V: v, Site: ps, On: a.pagesEnabled(), SiteURL: a.pagesPublicURL(ctx, v.Repo), PagesBase: a.pagesBase(),
		Private: !mayPublish, PrivSite: v.Repo.Private && mayPublish, UserSite: pagesUserSiteName(v.Repo.Owner),
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
		dv.Rollback = !dv.Live && dp.Status == "ready" && mayPublish
		if dv.Live {
			live := dp
			d.Live = &live
		}
		d.Deploys = append(d.Deploys, dv)
	}
	a.pagesSettingsExtras(ctx, r, v, d)
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
	if !a.pagesMayPublish(ctx, v.Repo) {
		a.pagesRender(w, r, errPagesPrivate.Error()+". Make the repository public, or verify a custom domain first.", http.StatusBadRequest)
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

// pagesSettingsExtras fills the custom domain and previews cards.
func (a *App) pagesSettingsExtras(ctx context.Context, r *http.Request, v *repoView, d *pagesSettingsData) {
	d.AppHost = a.pagesAppHostName()
	d.TunnelListen = a.cfg.TunnelListen
	if dom, err := a.db.PagesDomain(ctx, v.Repo.ID); err == nil {
		dv := &pagesDomainView{PagesDomain: dom, URL: a.pagesDomainURL(dom), TXTName: pagesTXTPrefix + dom.Host,
			FileURL: "https://" + dom.Host + pagesWellKnown + dom.Token}
		if dom.Route == pagesRouteTunnel {
			dv.Forward = "http://" + a.cfg.TunnelListen
		} else {
			dv.Forward = pagesForwardHint(a.cfg.PublicURL)
		}
		d.Domain = dv
	}
	d.Previews, _ = a.db.PagesPreviewsOn(ctx, v.Repo.ID)
	pvs, _ := a.db.PagesPreviews(ctx, v.Repo.ID)
	for _, pv := range pvs {
		row := pagesPreviewView{PagesPreview: pv, PullURL: fmt.Sprintf("%s/pulls/%d", v.Base, pv.Number), Short: pv.SHA}
		if len(row.Short) > 7 {
			row.Short = row.Short[:7]
		}
		if pv.LiveID != 0 {
			row.URL = a.pagesPreviewURL(v.Repo, pv.Number)
		}
		if pr, err := a.db.PullByNumber(ctx, v.Repo.ID, pv.Number); err == nil {
			row.Title = pr.Title
		}
		d.PreviewRows = append(d.PreviewRows, row)
	}
}

// pagesForwardHint is where a proxy host should forward: the same place the
// app's proxy host forwards to.
func pagesForwardHint(publicURL string) string {
	if publicURL == "" {
		return "the Atelier container and port"
	}
	return "the same forward host and port as " + strings.TrimRight(publicURL, "/")
}
