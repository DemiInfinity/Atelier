package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// ---------- home ----------

type activity struct {
	Repo   store.Repo
	Commit *gitrepo.Commit
	Branch string
}

type emberCell struct {
	Level int
	Title string
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owners, err := a.db.Owners(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	active := owners[0]
	if q := r.URL.Query().Get("account"); q != "" {
		for _, o := range owners {
			if strings.EqualFold(o.Name, q) {
				active = o
			}
		}
	}
	repos, err := a.db.ReposByOwner(ctx, active.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	all, err := a.db.Repos(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}

	// Recent activity: the latest commit in each recently updated repo.
	var acts []activity
	for _, rp := range all {
		if len(acts) == 8 {
			break
		}
		g, err := a.git.Open(rp.Owner, rp.Name)
		if err != nil || g.IsEmpty(ctx) {
			continue
		}
		br := g.DefaultBranch(ctx)
		if c := g.LastCommit(ctx, br, ""); c != nil {
			acts = append(acts, activity{Repo: rp, Commit: c, Branch: br})
		}
	}
	sort.SliceStable(acts, func(i, j int) bool { return acts[i].Commit.When.After(acts[j].Commit.When) })

	cells, total := a.embers(ctx, all)
	a.render(w, r, "home", page{Title: "Home", Nav: "home", Data: map[string]any{
		"Owners": owners, "Active": active, "Repos": repos, "Activity": acts,
		"Embers": cells, "EmberTotal": total,
	}})
}

// embers builds a 53-week contribution grid (column = week, Sunday first).
func (a *App) embers(ctx context.Context, repos []store.Repo) ([]emberCell, int) {
	today := time.Now().Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, -(52*7 + int(today.Weekday())))
	counts := map[string]int{}
	total := 0
	for _, rp := range repos {
		g, err := a.git.Open(rp.Owner, rp.Name)
		if err != nil {
			continue
		}
		for _, t := range g.CommitTimes(ctx, start) {
			counts[t.Format("2006-01-02")]++
			total++
		}
	}
	var cells []emberCell
	for d := start; !d.After(today); d = d.AddDate(0, 0, 1) {
		n := counts[d.Format("2006-01-02")]
		lv := 0
		switch {
		case n >= 10:
			lv = 4
		case n >= 6:
			lv = 3
		case n >= 3:
			lv = 2
		case n >= 1:
			lv = 1
		}
		cells = append(cells, emberCell{Level: lv, Title: plural(n, "commit") + " on " + d.Format("Mon 2 Jan 2006")})
	}
	return cells, total
}

// ---------- accounts ----------

func (a *App) ownerPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	o, err := a.db.OwnerByName(ctx, r.PathValue("owner"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	repos, err := a.db.ReposByOwner(ctx, o.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "owner", page{Title: o.Label(), Data: map[string]any{"Owner": o, "Repos": repos}})
}

func (a *App) newAccountPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "account_new", page{Title: "New company account", Data: map[string]string{}})
}

func (a *App) newAccountSubmit(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	display := strings.TrimSpace(r.FormValue("display_name"))
	form := map[string]string{"Name": name, "DisplayName": display}
	if !gitrepo.ValidName(name) || reserved[strings.ToLower(name)] {
		a.render(w, r, "account_new", page{Title: "New company account", Error: "Use letters, numbers, dots, dashes or underscores for the account name.", Data: form})
		return
	}
	if _, err := a.db.CreateOwner(r.Context(), name, display, "company"); err != nil {
		a.render(w, r, "account_new", page{Title: "New company account", Error: "That account name is already taken.", Data: form})
		return
	}
	http.Redirect(w, r, "/?account="+url.QueryEscape(name), http.StatusSeeOther)
}

// ---------- new repository ----------

func (a *App) newRepoPage(w http.ResponseWriter, r *http.Request) {
	owners, err := a.db.Owners(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "new", page{Title: "New repository", Data: map[string]any{
		"Owners": owners, "Owner": r.URL.Query().Get("owner"), "Private": true,
	}})
}

func (a *App) newRepoSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owners, err := a.db.Owners(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	ownerName := r.FormValue("owner")
	name := strings.TrimSpace(r.FormValue("name"))
	desc := strings.TrimSpace(r.FormValue("description"))
	private := r.FormValue("visibility") != "public"
	data := map[string]any{"Owners": owners, "Owner": ownerName, "Name": name, "Description": desc, "Private": private}
	fail := func(msg string) {
		a.render(w, r, "new", page{Title: "New repository", Error: msg, Data: data})
	}

	owner, err := a.db.OwnerByName(ctx, ownerName)
	if err != nil {
		fail("Pick an owner.")
		return
	}
	if !gitrepo.ValidName(name) {
		fail("Use letters, numbers, dots, dashes or underscores, and don't end the name with .git.")
		return
	}
	if _, err := a.db.RepoByName(ctx, owner.Name, name); err == nil {
		fail("You already have a repository with that name.")
		return
	}
	if err := a.git.Init(ctx, owner.Name, name); err != nil {
		a.fail(w, r, err)
		return
	}
	repo, err := a.db.CreateRepo(ctx, owner.ID, name, desc, private)
	if err != nil {
		_ = a.git.Remove(owner.Name, name)
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/"+repo.FullName(), http.StatusSeeOther)
}

// ---------- repository views ----------

type crumb struct{ Name, URL string }

type repoView struct {
	Repo       store.Repo
	Tab        string
	Empty      bool
	Ref        string
	Commit     string
	Path       string
	Crumbs     []crumb
	Branches   []string
	Tags       []string
	CloneHTTPS string
	Commits    int
	Base       string // "/owner/repo"
	Default    string // default branch
}

func (a *App) loadRepo(w http.ResponseWriter, r *http.Request, tab string) (*repoView, *gitrepo.Repo, bool) {
	ctx := r.Context()
	rp, err := a.db.RepoByName(ctx, r.PathValue("owner"), r.PathValue("repo"))
	if err != nil {
		a.fail(w, r, err)
		return nil, nil, false
	}
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		a.fail(w, r, err)
		return nil, nil, false
	}
	v := &repoView{
		Repo: rp, Tab: tab, Empty: g.IsEmpty(ctx), Base: "/" + rp.FullName(),
		CloneHTTPS: a.baseURL(r) + "/" + rp.FullName() + ".git",
	}
	v.Default = "main"
	if !v.Empty {
		v.Branches = g.Branches(ctx)
		v.Tags = g.Tags(ctx)
		v.Default = g.DefaultBranch(ctx)
	}
	return v, g, true
}

func (v *repoView) setCrumbs(kind string) {
	v.Crumbs = nil
	if v.Path == "" {
		return
	}
	acc := ""
	for _, seg := range strings.Split(v.Path, "/") {
		acc = strings.TrimPrefix(acc+"/"+seg, "/")
		v.Crumbs = append(v.Crumbs, crumb{Name: seg, URL: v.Base + "/" + kind + "/" + v.Ref + "/" + acc})
	}
}

func (a *App) repoCode(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "code")
	if !ok {
		return
	}
	if v.Empty {
		a.render(w, r, "repo_code", page{Title: v.Repo.FullName(), Nav: "repos", Data: map[string]any{"V": v}})
		return
	}
	v.Ref = g.DefaultBranch(r.Context())
	a.showTree(w, r, v, g)
}

func (a *App) repoTree(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "code")
	if !ok {
		return
	}
	ref, p, found := g.SplitRefPath(r.Context(), r.PathValue("rest"))
	if !found {
		a.notFound(w, r)
		return
	}
	v.Ref, v.Path = ref, p
	a.showTree(w, r, v, g)
}

func (a *App) showTree(w http.ResponseWriter, r *http.Request, v *repoView, g *gitrepo.Repo) {
	ctx := r.Context()
	commit, err := g.ResolveCommit(ctx, v.Ref)
	if err != nil {
		a.notFound(w, r)
		return
	}
	v.Commit = commit
	switch g.ObjectType(ctx, commit, v.Path) {
	case "tree":
	case "blob":
		http.Redirect(w, r, v.Base+"/blob/"+v.Ref+"/"+v.Path, http.StatusSeeOther)
		return
	default:
		a.notFound(w, r)
		return
	}
	entries, err := g.Tree(ctx, commit, v.Path)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v.setCrumbs("tree")
	v.Commits = g.CommitCount(ctx, commit)

	type row struct {
		gitrepo.Entry
		Last *gitrepo.Commit
		URL  string
	}
	rows := make([]row, 0, len(entries))
	var readme []byte
	readmeName := ""
	for _, e := range entries {
		kind := "blob"
		if e.IsDir() {
			kind = "tree"
		}
		rows = append(rows, row{Entry: e, Last: g.LastCommit(ctx, commit, e.Path), URL: v.Base + "/" + kind + "/" + v.Ref + "/" + e.Path})
		if !e.IsDir() && readme == nil && strings.HasPrefix(strings.ToLower(e.Name), "readme") {
			if b, err := g.Blob(ctx, commit, e.Path, gitrepo.MaxBlob); err == nil && !b.Binary && !b.TooLarge {
				readme, readmeName = b.Data, e.Name
			}
		}
	}
	a.render(w, r, "repo_code", page{Title: v.Repo.FullName(), Nav: "repos", Data: map[string]any{
		"V": v, "Rows": rows, "Last": g.LastCommit(ctx, commit, v.Path),
		"Readme": readme, "ReadmeName": readmeName,
	}})
}

func (a *App) repoBlob(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "code")
	if !ok {
		return
	}
	ctx := r.Context()
	ref, p, found := g.SplitRefPath(ctx, r.PathValue("rest"))
	if !found || p == "" {
		a.notFound(w, r)
		return
	}
	v.Ref, v.Path = ref, p
	commit, _ := g.ResolveCommit(ctx, ref)
	v.Commit = commit
	if g.ObjectType(ctx, commit, p) == "tree" {
		http.Redirect(w, r, v.Base+"/tree/"+ref+"/"+p, http.StatusSeeOther)
		return
	}
	b, err := g.Blob(ctx, commit, p, gitrepo.MaxBlob)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v.setCrumbs("tree")
	if n := len(v.Crumbs); n > 0 { // the file itself links to its blob view, not a tree redirect
		v.Crumbs[n-1].URL = v.Base + "/blob/" + v.Ref + "/" + v.Path
	}
	name := p[strings.LastIndex(p, "/")+1:]
	a.render(w, r, "repo_blob", page{Title: name + " · " + v.Repo.FullName(), Nav: "repos", Data: map[string]any{
		"V": v, "Blob": b, "Name": name, "Last": g.LastCommit(ctx, commit, p),
		"Markdown": isMarkdown(name) && r.URL.Query().Get("plain") == "",
	}})
}

func (a *App) repoRaw(w http.ResponseWriter, r *http.Request) {
	_, g, ok := a.loadRepo(w, r, "code")
	if !ok {
		return
	}
	ctx := r.Context()
	ref, p, found := g.SplitRefPath(ctx, r.PathValue("rest"))
	if !found || p == "" {
		a.notFound(w, r)
		return
	}
	commit, _ := g.ResolveCommit(ctx, ref)
	b, err := g.Blob(ctx, commit, p, 0)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	// Never let raw files render as HTML in Atelier's origin.
	if b.Binary {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	w.Header().Set("Content-Security-Policy", "sandbox")
	_, _ = w.Write(b.Data)
}

func (a *App) repoCommits(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "commits")
	if !ok {
		return
	}
	ctx := r.Context()
	if v.Empty {
		a.render(w, r, "repo_commits", page{Title: "Commits · " + v.Repo.FullName(), Nav: "repos", Data: map[string]any{"V": v}})
		return
	}
	ref, p, found := g.SplitRefPath(ctx, r.PathValue("rest"))
	if !found {
		a.notFound(w, r)
		return
	}
	v.Ref, v.Path = ref, p
	commit, _ := g.ResolveCommit(ctx, ref)
	pageNum, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if pageNum < 1 {
		pageNum = 1
	}
	const per = 40
	commits, err := g.Log(ctx, commit, p, (pageNum-1)*per, per+1)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	more := len(commits) > per
	if more {
		commits = commits[:per]
	}
	type day struct {
		Label   string
		Commits []gitrepo.Commit
	}
	var days []day
	for _, c := range commits {
		label := c.When.Format("Monday 2 January 2006")
		if len(days) == 0 || days[len(days)-1].Label != label {
			days = append(days, day{Label: label})
		}
		days[len(days)-1].Commits = append(days[len(days)-1].Commits, c)
	}
	a.render(w, r, "repo_commits", page{Title: "Commits · " + v.Repo.FullName(), Nav: "repos", Data: map[string]any{
		"V": v, "Days": days, "Page": pageNum, "More": more, "Prev": pageNum - 1, "Next": pageNum + 1,
	}})
}

func (a *App) repoCommit(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "commits")
	if !ok {
		return
	}
	ctx := r.Context()
	sha, err := g.ResolveCommit(ctx, r.PathValue("sha"))
	if err != nil {
		a.notFound(w, r)
		return
	}
	c, patch, err := g.Show(ctx, sha)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	v.Ref = sha
	a.render(w, r, "repo_commit", page{Title: c.Subject + " · " + v.Repo.FullName(), Nav: "repos", Data: map[string]any{
		"V": v, "C": c, "Patch": patch,
	}})
}

// ---------- repository settings ----------

func (a *App) repoSettings(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	a.render(w, r, "repo_settings", page{Title: "Settings · " + v.Repo.FullName(), Nav: "repos", Notice: r.URL.Query().Get("saved"), Data: map[string]any{"V": v}})
}

func (a *App) repoSettingsSubmit(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	desc := strings.TrimSpace(r.FormValue("description"))
	private := r.FormValue("visibility") != "public"
	if err := a.db.UpdateRepo(r.Context(), v.Repo.ID, desc, private); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/settings?saved=1", http.StatusSeeOther)
}

func (a *App) repoDelete(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "settings")
	if !ok {
		return
	}
	if r.FormValue("confirm") != v.Repo.FullName() {
		a.render(w, r, "repo_settings", page{Title: "Settings · " + v.Repo.FullName(), Nav: "repos",
			Error: "Type the full repository name to confirm deletion.", Data: map[string]any{"V": v}})
		return
	}
	if err := a.db.DeleteRepo(r.Context(), v.Repo.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.git.Remove(v.Repo.Owner, v.Repo.Name); err != nil {
		logErr(r, err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- access tokens ----------

var tokenScopes = map[string][]string{
	"read":  {"repo:read"},
	"write": {"repo:read", "repo:write"},
}

var tokenExpiry = map[string]time.Duration{
	"30": 30 * 24 * time.Hour, "90": 90 * 24 * time.Hour, "365": 365 * 24 * time.Hour, "never": 0,
}

func (a *App) tokensPage(w http.ResponseWriter, r *http.Request) {
	a.renderTokens(w, r, "", "")
}

func (a *App) renderTokens(w http.ResponseWriter, r *http.Request, created, errMsg string) {
	toks, err := a.db.Tokens(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	me, _ := a.db.PersonalOwner(r.Context())
	a.render(w, r, "tokens", page{Title: "Access tokens", Error: errMsg, Data: map[string]any{
		"Tokens": toks, "Created": created, "Base": a.baseURL(r), "User": me.Name,
	}})
}

func (a *App) tokensCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	scopes, ok1 := tokenScopes[r.FormValue("access")]
	ttl, ok2 := tokenExpiry[r.FormValue("expires")]
	if name == "" || len(name) > 80 || !ok1 || !ok2 {
		a.renderTokens(w, r, "", "Give the token a name, access level and expiry.")
		return
	}
	plain, err := a.db.CreateToken(r.Context(), name, scopes, ttl)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.renderTokens(w, r, plain, "")
}

func (a *App) tokensRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.db.DeleteToken(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/tokens", http.StatusSeeOther)
}
