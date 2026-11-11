package web

import (
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atelier/internal/tether"
)

// Issues and pull requests on GitHub, read-only: lists (open/closed) and the
// conversation, plus a pull request's changed files.

type ghItemRow struct {
	Number   int64
	Title    string
	URL, GH  string
	State    string // open, closed, merged
	Draft    bool
	Author   tetherPerson
	Created  time.Time
	Comments int
	Labels   []string
}

type ghListData struct {
	V         *ghRepoView
	Pulls     bool
	State     string
	OpenURL   string
	ClosedURL string
	Rows      []ghItemRow
	Pager     ghPager
}

func ghStateParam(r *http.Request) string {
	if r.URL.Query().Get("state") == "closed" {
		return "closed"
	}
	return "open"
}

func ghLabelNames(ls []tether.Label) []string {
	var out []string
	for i, l := range ls {
		if i >= 8 {
			break
		}
		if n := eventExcerpt(l.Name, 50); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func (a *App) ghIssues(w http.ResponseWriter, r *http.Request, g *ghReq) {
	ctx := r.Context()
	v, ok := a.ghLoadRepo(w, r, g, "issues", false)
	if !ok {
		return
	}
	state, page := ghStateParam(r), ghPageNum(r)
	items, err := g.c.Issues(ctx, v.Full, state, page, 30)
	if err != nil {
		a.ghFail(w, r, g, err, v.GH+"/issues")
		return
	}
	var logins []string
	for _, it := range items {
		logins = append(logins, it.User.Login)
	}
	badges := a.ghBadges(ctx, g, logins)
	d := &ghListData{V: v, State: state, OpenURL: v.Base + "/issues", ClosedURL: v.Base + "/issues?state=closed",
		Pager: pagerFor(r, page, len(items) == 30)}
	for _, it := range items {
		if it.IsPull() || it.Number <= 0 {
			continue // pull requests have their own tab
		}
		n := strconv.FormatInt(it.Number, 10)
		d.Rows = append(d.Rows, ghItemRow{Number: it.Number, Title: eventExcerpt(it.Title, 300), State: it.State,
			URL: v.Base + "/issues/" + n, GH: a.ghWebURL(v.Full + "/issues/" + n), Author: a.ghPerson(it.User, badges),
			Created: it.CreatedAt, Comments: it.Comments, Labels: ghLabelNames(it.Labels)})
	}
	v.ghBase = g.base()
	a.render(w, r, "gh_issues", page2("Issues · "+v.Full, d))
}

func (a *App) ghPulls(w http.ResponseWriter, r *http.Request, g *ghReq) {
	ctx := r.Context()
	v, ok := a.ghLoadRepo(w, r, g, "pulls", false)
	if !ok {
		return
	}
	state, page := ghStateParam(r), ghPageNum(r)
	prs, err := g.c.Pulls(ctx, v.Full, state, page, 30)
	if err != nil {
		a.ghFail(w, r, g, err, v.GH+"/pulls")
		return
	}
	var logins []string
	for _, p := range prs {
		logins = append(logins, p.User.Login)
	}
	badges := a.ghBadges(ctx, g, logins)
	d := &ghListData{V: v, Pulls: true, State: state, OpenURL: v.Base + "/pulls", ClosedURL: v.Base + "/pulls?state=closed",
		Pager: pagerFor(r, page, len(prs) == 30)}
	for _, p := range prs {
		if p.Number <= 0 {
			continue
		}
		n := strconv.FormatInt(p.Number, 10)
		st := p.State
		if p.MergedAt != nil || p.Merged {
			st = "merged"
		}
		d.Rows = append(d.Rows, ghItemRow{Number: p.Number, Title: eventExcerpt(p.Title, 300), State: st, Draft: p.Draft,
			URL: v.Base + "/pull/" + n, GH: a.ghWebURL(v.Full + "/pull/" + n), Author: a.ghPerson(p.User, badges),
			Created: p.CreatedAt, Labels: ghLabelNames(p.Labels)})
	}
	v.ghBase = g.base()
	a.render(w, r, "gh_issues", page2("Pull requests · "+v.Full, d))
}

type ghCommentView struct {
	ID     int64
	Author tetherPerson
	When   time.Time
	Body   template.HTML
	GH     string
}

type ghItemData struct {
	V        *ghRepoView
	Pull     bool
	Files    bool // the "Files changed" tab
	Number   int64
	Title    string
	State    string
	Draft    bool
	Author   tetherPerson
	Created  time.Time
	Body     template.HTML
	Comments []ghCommentView
	Labels   []string
	GH       string
	URL      string
	// Pull requests
	Head, BaseRef string
	HeadURL       string
	Commits       int
	Add, Del      int
	Changed       int
	Diffs         []ghFileDiff
	MergedAt      *time.Time
}

func (a *App) ghIssuePage(w http.ResponseWriter, r *http.Request, g *ghReq) {
	ctx := r.Context()
	v, ok := a.ghLoadRepo(w, r, g, "issues", false)
	if !ok {
		return
	}
	ns := r.PathValue("n")
	if !ghNumberRe.MatchString(ns) {
		a.notFound(w, r)
		return
	}
	n, _ := strconv.ParseInt(ns, 10, 64)
	is, err := g.c.Issue(ctx, v.Full, n)
	if err != nil {
		a.ghFail(w, r, g, err, a.ghWebURL(v.Full+"/issues/"+ns))
		return
	}
	if is.IsPull() {
		http.Redirect(w, r, v.Base+"/pull/"+ns, http.StatusSeeOther)
		return
	}
	d := &ghItemData{V: v, Number: n, Title: eventExcerpt(is.Title, 400), State: is.State, Created: is.CreatedAt,
		Labels: ghLabelNames(is.Labels), GH: a.ghWebURL(v.Full + "/issues/" + ns), URL: v.Base + "/issues/" + ns}
	if is.StateReason == "not_planned" {
		d.State = "not_planned"
	}
	a.ghConversation(r, g, v, d, is.User, is.Body, n)
	v.ghBase = g.base()
	a.render(w, r, "gh_issue", page2(d.Title+" · Issue #"+ns, d))
}

// ghConversation fills the author, body and comments.
func (a *App) ghConversation(r *http.Request, g *ghReq, v *ghRepoView, d *ghItemData, author tether.User, body string, n int64) {
	ctx := r.Context()
	comments, _ := g.c.IssueComments(ctx, v.Full, n)
	logins := []string{author.Login}
	for _, c := range comments {
		logins = append(logins, c.User.Login)
	}
	badges := a.ghBadges(ctx, g, logins)
	d.Author = a.ghPerson(author, badges)
	mc := ghMD{Repo: v.Full, Ref: v.Default, Issues: true}
	if strings.TrimSpace(body) != "" {
		d.Body = a.ghMarkdown(body, mc)
	}
	for _, c := range comments {
		d.Comments = append(d.Comments, ghCommentView{ID: c.ID, Author: a.ghPerson(c.User, badges), When: c.CreatedAt,
			Body: a.ghMarkdown(c.Body, mc), GH: a.tetherHTMLURL(c.HTMLURL, v.Full+"/issues/"+strconv.FormatInt(n, 10))})
	}
}

func (a *App) ghPullPage(w http.ResponseWriter, r *http.Request, g *ghReq) {
	ctx := r.Context()
	v, ok := a.ghLoadRepo(w, r, g, "pulls", false)
	if !ok {
		return
	}
	ns := r.PathValue("n")
	if !ghNumberRe.MatchString(ns) {
		a.notFound(w, r)
		return
	}
	n, _ := strconv.ParseInt(ns, 10, 64)
	p, err := g.c.PullCached(ctx, v.Full, n)
	if err != nil {
		if tether.IsStatus(err, http.StatusNotFound) {
			// Maybe it's an issue.
			if is, ierr := g.c.Issue(ctx, v.Full, n); ierr == nil && !is.IsPull() {
				http.Redirect(w, r, v.Base+"/issues/"+ns, http.StatusSeeOther)
				return
			}
		}
		a.ghFail(w, r, g, err, a.ghWebURL(v.Full+"/pull/"+ns))
		return
	}
	d := &ghItemData{V: v, Pull: true, Files: strings.HasSuffix(r.URL.Path, "/files"), Number: n,
		Title: eventExcerpt(p.Title, 400), State: p.State, Draft: p.Draft, Created: p.CreatedAt, Labels: ghLabelNames(p.Labels),
		GH: a.ghWebURL(v.Full + "/pull/" + ns), URL: v.Base + "/pull/" + ns, Head: eventExcerpt(p.Head.Ref, 200),
		BaseRef: eventExcerpt(p.Base.Ref, 200), Commits: p.Commits, Add: p.Additions, Del: p.Deletions, Changed: p.ChangedFiles,
		MergedAt: p.MergedAt}
	if p.MergedAt != nil || p.Merged {
		d.State = "merged"
	}
	if p.Head.Repo != nil && tether.ValidRepo(p.Head.Repo.FullName) && p.Head.Ref != "" {
		d.HeadURL = ghLocalURL(p.Head.Repo.FullName) + "/tree/" + ghEscPath(p.Head.Ref)
	}
	if d.Files {
		files, err := g.c.PullFiles(ctx, v.Full, n)
		if err != nil {
			a.ghFail(w, r, g, err, d.GH+"/files")
			return
		}
		ref := ""
		if ghFullSHARe.MatchString(p.Head.SHA) && p.Head.Repo != nil && strings.EqualFold(p.Head.Repo.FullName, v.Full) {
			ref = p.Head.SHA
		}
		d.Diffs = ghDiffs(v, ref, files)
		d.Author = a.ghPerson(p.User, a.ghBadges(ctx, g, []string{p.User.Login}))
	} else {
		a.ghConversation(r, g, v, d, p.User, p.Body, n)
	}
	v.ghBase = g.base()
	a.render(w, r, "gh_issue", page2(d.Title+" · Pull request #"+ns, d))
}
