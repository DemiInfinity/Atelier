package web

import (
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"atelier/internal/store"
)

// Issues, labels and milestones for a repository.

func init() {
	registerFeature(feature{
		name: "issues",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /{owner}/{repo}/issues", a.authed(a.issuesList))
			m.HandleFunc("GET /{owner}/{repo}/issues/new", a.authed(a.issueNewPage))
			m.HandleFunc("POST /{owner}/{repo}/issues/new", a.authed(a.issueCreate))
			m.HandleFunc("GET /{owner}/{repo}/issues/{number}", a.authed(a.issueView))
			m.HandleFunc("POST /{owner}/{repo}/issues/{number}/comment", a.authed(a.issueComment))
			m.HandleFunc("POST /{owner}/{repo}/issues/{number}/edit", a.authed(a.issueEdit))
			m.HandleFunc("POST /{owner}/{repo}/issues/{number}/state", a.authed(a.issueState))
			m.HandleFunc("POST /{owner}/{repo}/issues/{number}/labels", a.authed(a.issueSetLabels))
			m.HandleFunc("POST /{owner}/{repo}/issues/{number}/milestone", a.authed(a.issueSetMilestone))
			m.HandleFunc("POST /{owner}/{repo}/issues/{number}/comments/{id}/edit", a.authed(a.issueCommentEdit))
			m.HandleFunc("POST /{owner}/{repo}/issues/{number}/comments/{id}/delete", a.authed(a.issueCommentDelete))

			m.HandleFunc("GET /{owner}/{repo}/labels", a.authed(a.labelsPage))
			m.HandleFunc("GET /{owner}/{repo}/labels.css", a.authed(a.labelsCSS))
			m.HandleFunc("POST /{owner}/{repo}/labels", a.authed(a.labelCreate))
			m.HandleFunc("POST /{owner}/{repo}/labels/{id}/edit", a.authed(a.labelUpdate))
			m.HandleFunc("POST /{owner}/{repo}/labels/{id}/delete", a.authed(a.labelDelete))

			m.HandleFunc("GET /{owner}/{repo}/milestones", a.authed(a.milestonesPage))
			m.HandleFunc("GET /{owner}/{repo}/milestones/new", a.authed(a.milestoneNewPage))
			m.HandleFunc("POST /{owner}/{repo}/milestones/new", a.authed(a.milestoneCreate))
			m.HandleFunc("GET /{owner}/{repo}/milestones/{id}/edit", a.authed(a.milestoneEditPage))
			m.HandleFunc("POST /{owner}/{repo}/milestones/{id}/edit", a.authed(a.milestoneUpdate))
			m.HandleFunc("POST /{owner}/{repo}/milestones/{id}/state", a.authed(a.milestoneState))
			m.HandleFunc("POST /{owner}/{repo}/milestones/{id}/delete", a.authed(a.milestoneDelete))
		},
	})
}

const issuesPerPage = 25

// loadIssuesRepo loads the repo for an issues page and seeds its default labels.
func (a *App) loadIssuesRepo(w http.ResponseWriter, r *http.Request) (*repoView, bool) {
	v, _, ok := a.loadRepo(w, r, "issues")
	if !ok {
		return nil, false
	}
	if err := a.db.EnsureDefaultLabels(r.Context(), v.Repo.ID); err != nil {
		a.fail(w, r, err)
		return nil, false
	}
	return v, true
}

func pathInt(r *http.Request, name string) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n, err == nil && n > 0
}

// ---------- list ----------

type issueLink struct {
	Label  string
	URL    string
	Active bool
	Label2 *store.Label // for label filters
	Desc   string
}

// issuesURL builds a list URL for a query, sort and page.
func issuesURL(base string, q store.IssueQuery, sortKey string, page int) string {
	sortKey = strings.TrimSpace(sortKey)
	q.Sort = ""
	vals := url.Values{"q": {q.String()}}
	if sortKey != "" && sortKey != "newest" {
		vals.Set("sort", sortKey)
	}
	if page > 1 {
		vals.Set("page", strconv.Itoa(page))
	}
	return base + "/issues?" + vals.Encode()
}

func (a *App) issuesList(w http.ResponseWriter, r *http.Request) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	qv := r.URL.Query()
	raw := "is:open"
	if qv.Has("q") {
		raw = qv.Get("q")
	}
	q := store.ParseIssueQuery(raw)
	sortKey := qv.Get("sort")
	if sortKey == "" {
		sortKey = q.Sort
	}
	switch sortKey {
	case "oldest", "updated":
	default:
		sortKey = "newest"
	}
	q.Sort = sortKey
	pageNum, _ := strconv.Atoi(qv.Get("page"))
	if pageNum < 1 {
		pageNum = 1
	}

	issues, more, err := a.db.Issues(ctx, v.Repo.ID, q, (pageNum-1)*issuesPerPage, issuesPerPage)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	open, closed, err := a.db.IssueCounts(ctx, v.Repo.ID, q)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	labels, err := a.db.Labels(ctx, v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	milestones, err := a.db.Milestones(ctx, v.Repo.ID, "open")
	if err != nil {
		a.fail(w, r, err)
		return
	}

	withState := func(s string) string { c := q; c.State = s; return issuesURL(v.Base, c, sortKey, 1) }

	var labelLinks []issueLink
	for k := range labels {
		l := labels[k]
		c := q
		c.NoLabel = false
		active := q.HasLabel(l.Name)
		c.Labels = nil
		for _, x := range q.Labels {
			if !strings.EqualFold(x, l.Name) {
				c.Labels = append(c.Labels, x)
			}
		}
		if !active {
			c.Labels = append(c.Labels, l.Name)
		}
		labelLinks = append(labelLinks, issueLink{Label: l.Name, URL: issuesURL(v.Base, c, sortKey, 1), Active: active, Label2: &labels[k], Desc: l.Description})
	}
	noLabel := q
	noLabel.Labels, noLabel.NoLabel = nil, !q.NoLabel

	var msLinks []issueLink
	{
		c := q
		c.Milestone, c.NoMilestone = "", !q.NoMilestone
		msLinks = append(msLinks, issueLink{Label: "Issues with no milestone", URL: issuesURL(v.Base, c, sortKey, 1), Active: q.NoMilestone})
	}
	seen := false
	for _, m := range milestones {
		c := q
		c.NoMilestone = false
		active := strings.EqualFold(q.Milestone, m.Title)
		seen = seen || active
		c.Milestone = m.Title
		if active {
			c.Milestone = ""
		}
		msLinks = append(msLinks, issueLink{Label: m.Title, URL: issuesURL(v.Base, c, sortKey, 1), Active: active})
	}
	if q.Milestone != "" && !seen { // filtering on a closed or unknown milestone
		c := q
		c.Milestone = ""
		msLinks = append(msLinks, issueLink{Label: q.Milestone, URL: issuesURL(v.Base, c, sortKey, 1), Active: true})
	}

	sorts := []issueLink{}
	for _, s := range [][2]string{{"newest", "Newest"}, {"oldest", "Oldest"}, {"updated", "Recently updated"}} {
		sorts = append(sorts, issueLink{Label: s[1], URL: issuesURL(v.Base, q, s[0], 1), Active: s[0] == sortKey})
	}
	sortLabel := map[string]string{"newest": "Newest", "oldest": "Oldest", "updated": "Recently updated"}[sortKey]

	searchText := q
	searchText.Sort = ""
	data := map[string]any{
		"V": v, "Active": "issues", "Q": searchText.String(), "Query": q, "Sort": sortKey, "SortLabel": sortLabel, "Sorts": sorts,
		"Issues": issues, "Open": open, "Closed": closed,
		"OpenURL": withState("open"), "ClosedURL": withState("closed"),
		"Labels": labelLinks, "NoLabelURL": issuesURL(v.Base, noLabel, sortKey, 1),
		"Milestones": msLinks, "Page": pageNum, "More": more,
		"PrevURL": issuesURL(v.Base, q, sortKey, pageNum-1), "NextURL": issuesURL(v.Base, q, sortKey, pageNum+1),
		"Filtered":   searchText.String() != "is:open" || sortKey != "newest",
		"ClearURL":   v.Base + "/issues",
		"LabelCount": len(labels), "MilestoneCount": len(milestones),
	}
	a.render(w, r, "issues_list", page{Title: "Issues · " + v.Repo.FullName(), Nav: "repos", Data: data})
}

// ---------- new ----------

func (a *App) issueNewPage(w http.ResponseWriter, r *http.Request) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return
	}
	a.renderIssueNew(w, r, v, "", "", nil, 0, "")
}

func (a *App) renderIssueNew(w http.ResponseWriter, r *http.Request, v *repoView, title, body string, picked []int64, ms int64, errMsg string) {
	ctx := r.Context()
	labels, err := a.db.Labels(ctx, v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	milestones, err := a.db.Milestones(ctx, v.Repo.ID, "open")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	a.render(w, r, "issues_new", page{Title: "New issue · " + v.Repo.FullName(), Nav: "repos", Error: errMsg, Data: map[string]any{
		"V": v, "Title": title, "Body": body, "Labels": pickLabels(labels, picked), "Milestones": milestones, "MilestoneID": ms,
	}})
}

type pickedLabel struct {
	store.Label
	Checked bool
}

func pickLabels(all []store.Label, picked []int64) []pickedLabel {
	on := map[int64]bool{}
	for _, id := range picked {
		on[id] = true
	}
	out := make([]pickedLabel, len(all))
	for k, l := range all {
		out[k] = pickedLabel{Label: l, Checked: on[l.ID]}
	}
	return out
}

func formIDs(r *http.Request, key string) []int64 {
	_ = r.ParseForm()
	var out []int64
	for _, s := range r.PostForm[key] {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (a *App) issueCreate(w http.ResponseWriter, r *http.Request) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	act := a.actor(r)
	title, body := r.FormValue("title"), normalizeNewlines(r.FormValue("body"))
	labels := formIDs(r, "label")
	ms, _ := strconv.ParseInt(r.FormValue("milestone"), 10, 64)
	is, err := a.db.CreateIssue(ctx, v.Repo.ID, title, body, act.Kind, act.Name)
	if errors.Is(err, store.ErrInvalidIssue) {
		a.renderIssueNew(w, r, v, title, body, labels, ms, "Give the issue a title (up to 256 characters).")
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if len(labels) > 0 {
		if err := a.db.SetIssueLabels(ctx, is.ID, labels, act.Kind, act.Name); err != nil {
			logErr(r, err)
		}
	}
	if ms > 0 {
		if err := a.db.SetIssueMilestone(ctx, is.ID, ms, act.Kind, act.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			logErr(r, err)
		}
	}
	a.emit(ctx, issueEvent(EventIssueOpened, v, is, act, body))
	http.Redirect(w, r, fmt.Sprintf("%s/issues/%d", v.Base, is.Number), http.StatusSeeOther)
}

func normalizeNewlines(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// ---------- view ----------

type commentView struct {
	store.IssueComment
	HTML template.HTML
	Own  bool
}

type eventView struct {
	store.IssueEvent
	LabelID int64 // current label with the same name, for its colour (0 if deleted)
}

type timelineItem struct {
	When    time.Time
	Comment *commentView
	Event   *eventView
}

// loadIssue loads the repo and issue #number, 404ing for unknown numbers and pull requests.
func (a *App) loadIssue(w http.ResponseWriter, r *http.Request) (*repoView, store.Issue, bool) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return nil, store.Issue{}, false
	}
	n, ok := pathInt(r, "number")
	if !ok {
		a.notFound(w, r)
		return nil, store.Issue{}, false
	}
	is, err := a.db.IssueByNumber(r.Context(), v.Repo.ID, n)
	if err != nil {
		a.fail(w, r, err)
		return nil, store.Issue{}, false
	}
	return v, is, true
}

func (a *App) issueMarkdown(r *http.Request, repoID int64, base string) func(string) template.HTML {
	nums, err := a.db.IssueNumbers(r.Context(), repoID)
	if err != nil {
		logErr(r, err)
	}
	exists := func(n int64) bool { return nums[n] }
	return func(src string) template.HTML { return renderIssueMarkdown(src, base, exists) }
}

func (a *App) issueView(w http.ResponseWriter, r *http.Request) {
	v, is, ok := a.loadIssue(w, r)
	if !ok {
		return
	}
	a.renderIssue(w, r, v, is, "", "")
}

func (a *App) renderIssue(w http.ResponseWriter, r *http.Request, v *repoView, is store.Issue, draft, errMsg string) {
	ctx := r.Context()
	act := a.actor(r)
	render := a.issueMarkdown(r, v.Repo.ID, v.Base)

	comments, err := a.db.IssueComments(ctx, is.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	events, err := a.db.IssueEvents(ctx, is.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	labels, err := a.db.Labels(ctx, v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	milestones, err := a.db.Milestones(ctx, v.Repo.ID, "")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	byName := map[string]int64{}
	for _, l := range labels {
		byName[strings.ToLower(l.Name)] = l.ID
	}

	var tl []timelineItem
	for _, c := range comments {
		tl = append(tl, timelineItem{When: c.CreatedAt, Comment: &commentView{IssueComment: c, HTML: render(c.Body),
			Own: c.AuthorKind == act.Kind && c.AuthorName == act.Name}})
	}
	for _, e := range events {
		tl = append(tl, timelineItem{When: e.CreatedAt, Event: &eventView{IssueEvent: e, LabelID: byName[strings.ToLower(e.LabelName)]}})
	}
	// Oldest first; in the same second a comment comes before the events it triggered.
	sort.SliceStable(tl, func(i, j int) bool {
		if !tl[i].When.Equal(tl[j].When) {
			return tl[i].When.Before(tl[j].When)
		}
		return tl[i].Comment != nil && tl[j].Comment == nil
	})

	cur := map[int64]bool{}
	var ids []int64
	for _, l := range is.Labels {
		cur[l.ID] = true
		ids = append(ids, l.ID)
	}
	var msOpts []store.Milestone
	var current *store.Milestone
	for k, m := range milestones {
		if m.ID == is.MilestoneID {
			current = &milestones[k]
		}
		if m.State == "open" || m.ID == is.MilestoneID {
			msOpts = append(msOpts, m)
		}
	}
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	a.render(w, r, "issues_view", page{Title: is.Title + " · Issue #" + strconv.FormatInt(is.Number, 10) + " · " + v.Repo.FullName(),
		Nav: "repos", Error: errMsg, Data: map[string]any{
			"V": v, "Issue": is, "BodyHTML": render(is.Body), "Timeline": tl, "Draft": draft,
			"OwnIssue": is.AuthorKind == act.Kind && is.AuthorName == act.Name,
			"Labels":   pickLabels(labels, ids), "Milestones": msOpts, "Milestone": current,
			"IssueURL":     fmt.Sprintf("%s/issues/%d", v.Base, is.Number),
			"Participants": participants(is, comments),
		}})
}

func participants(is store.Issue, cs []store.IssueComment) []string {
	seen := map[string]bool{is.AuthorName: true}
	out := []string{is.AuthorName}
	for _, c := range cs {
		if !seen[c.AuthorName] {
			seen[c.AuthorName] = true
			out = append(out, c.AuthorName)
		}
	}
	return out
}

func (a *App) issueBack(w http.ResponseWriter, r *http.Request, v *repoView, is store.Issue, anchor string) {
	http.Redirect(w, r, fmt.Sprintf("%s/issues/%d%s", v.Base, is.Number, anchor), http.StatusSeeOther)
}

// issueComment adds a comment and/or closes or reopens, like GitHub's comment box buttons.
func (a *App) issueComment(w http.ResponseWriter, r *http.Request) {
	v, is, ok := a.loadIssue(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	act := a.actor(r)
	body := normalizeNewlines(r.FormValue("body"))
	action := r.FormValue("action")
	anchor := ""
	if strings.TrimSpace(body) != "" {
		c, err := a.db.AddIssueComment(ctx, is.ID, body, act.Kind, act.Name)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		anchor = fmt.Sprintf("#comment-%d", c.ID)
		a.emit(ctx, issueEvent(EventIssueCommented, v, is, act, body, anchor))
	} else if action == "" || action == "comment" {
		a.renderIssue(w, r, v, is, body, "Write something before commenting.")
		return
	}
	if err := a.applyIssueState(r, is, action); err != nil {
		a.fail(w, r, err)
		return
	}
	a.emitIssueState(ctx, v, is, act, action)
	a.issueBack(w, r, v, is, anchor)
}

func (a *App) applyIssueState(r *http.Request, is store.Issue, action string) error {
	act := a.actor(r)
	switch action {
	case "close", "close_completed":
		return a.db.CloseIssue(r.Context(), is.ID, act.Kind, act.Name, store.ReasonCompleted)
	case "close_not_planned":
		return a.db.CloseIssue(r.Context(), is.ID, act.Kind, act.Name, store.ReasonNotPlanned)
	case "reopen":
		return a.db.ReopenIssue(r.Context(), is.ID, act.Kind, act.Name)
	}
	return nil
}

func (a *App) issueState(w http.ResponseWriter, r *http.Request) {
	v, is, ok := a.loadIssue(w, r)
	if !ok {
		return
	}
	if err := a.applyIssueState(r, is, r.FormValue("action")); err != nil {
		a.fail(w, r, err)
		return
	}
	a.emitIssueState(r.Context(), v, is, a.actor(r), r.FormValue("action"))
	a.issueBack(w, r, v, is, "")
}

func (a *App) issueEdit(w http.ResponseWriter, r *http.Request) {
	v, is, ok := a.loadIssue(w, r)
	if !ok {
		return
	}
	act := a.actor(r)
	_ = r.ParseForm()
	title, body := is.Title, is.Body
	if _, set := r.PostForm["title"]; set {
		title = r.PostForm.Get("title")
	}
	if _, set := r.PostForm["body"]; set {
		body = normalizeNewlines(r.PostForm.Get("body"))
	}
	if err := a.db.UpdateIssue(r.Context(), is.ID, title, body, act.Kind, act.Name); err != nil {
		if errors.Is(err, store.ErrInvalidIssue) {
			a.renderIssue(w, r, v, is, "", "Give the issue a title (up to 256 characters).")
			return
		}
		a.fail(w, r, err)
		return
	}
	a.issueBack(w, r, v, is, "")
}

func (a *App) issueSetLabels(w http.ResponseWriter, r *http.Request) {
	v, is, ok := a.loadIssue(w, r)
	if !ok {
		return
	}
	act := a.actor(r)
	if err := a.db.SetIssueLabels(r.Context(), is.ID, formIDs(r, "label"), act.Kind, act.Name); err != nil {
		a.fail(w, r, err)
		return
	}
	a.issueBack(w, r, v, is, "")
}

func (a *App) issueSetMilestone(w http.ResponseWriter, r *http.Request) {
	v, is, ok := a.loadIssue(w, r)
	if !ok {
		return
	}
	act := a.actor(r)
	id, _ := strconv.ParseInt(r.FormValue("milestone"), 10, 64)
	if err := a.db.SetIssueMilestone(r.Context(), is.ID, id, act.Kind, act.Name); err != nil {
		a.fail(w, r, err)
		return
	}
	a.issueBack(w, r, v, is, "")
}

// ownComment loads comment {id} on the issue and checks the current actor wrote it.
func (a *App) ownComment(w http.ResponseWriter, r *http.Request, is store.Issue) (store.IssueComment, bool) {
	id, ok := pathInt(r, "id")
	if !ok {
		a.notFound(w, r)
		return store.IssueComment{}, false
	}
	c, err := a.db.IssueCommentByID(r.Context(), id)
	if err != nil || c.IssueID != is.ID {
		a.notFound(w, r)
		return store.IssueComment{}, false
	}
	act := a.actor(r)
	if c.AuthorKind != act.Kind || c.AuthorName != act.Name {
		http.Error(w, "you can only change your own comments", http.StatusForbidden)
		return store.IssueComment{}, false
	}
	return c, true
}

func (a *App) issueCommentEdit(w http.ResponseWriter, r *http.Request) {
	v, is, ok := a.loadIssue(w, r)
	if !ok {
		return
	}
	c, ok := a.ownComment(w, r, is)
	if !ok {
		return
	}
	body := normalizeNewlines(r.FormValue("body"))
	if strings.TrimSpace(body) == "" {
		a.renderIssue(w, r, v, is, "", "A comment can't be empty. Delete it instead.")
		return
	}
	if err := a.db.UpdateIssueComment(r.Context(), c.ID, body); err != nil {
		a.fail(w, r, err)
		return
	}
	a.issueBack(w, r, v, is, fmt.Sprintf("#comment-%d", c.ID))
}

func (a *App) issueCommentDelete(w http.ResponseWriter, r *http.Request) {
	v, is, ok := a.loadIssue(w, r)
	if !ok {
		return
	}
	c, ok := a.ownComment(w, r, is)
	if !ok {
		return
	}
	if err := a.db.DeleteIssueComment(r.Context(), c.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	a.issueBack(w, r, v, is, "")
}

// ---------- labels ----------

func (a *App) labelsPage(w http.ResponseWriter, r *http.Request) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return
	}
	a.renderLabels(w, r, v, "", r.URL.Query().Get("saved"))
}

func (a *App) renderLabels(w http.ResponseWriter, r *http.Request, v *repoView, errMsg, notice string) {
	labels, err := a.db.Labels(r.Context(), v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	a.render(w, r, "issues_labels", page{Title: "Labels · " + v.Repo.FullName(), Nav: "repos", Error: errMsg, Notice: notice, Data: map[string]any{
		"V": v, "Active": "labels", "Labels": labels, "NewColor": suggestColor(len(labels)),
		"IssuesFor": func(name string) string {
			return issuesURL(v.Base, store.IssueQuery{State: "open", Labels: []string{name}}, "", 1)
		},
	}})
}

var labelPalette = []string{"f2b85c", "7cc0ff", "c9b6ff", "7fd16b", "f08a7a", "5fd4c4", "e8a1d0", "b8b36b"}

func suggestColor(n int) string { return labelPalette[n%len(labelPalette)] }

func labelErr(err error) string {
	if errors.Is(err, store.ErrInvalidLabel) {
		return "Label names need 1–50 characters (no quotes) and a colour like #f2b85c."
	}
	return err.Error()
}

func (a *App) labelCreate(w http.ResponseWriter, r *http.Request) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return
	}
	_, err := a.db.CreateLabel(r.Context(), v.Repo.ID, r.FormValue("name"), r.FormValue("color"), r.FormValue("description"))
	if err != nil {
		a.renderLabels(w, r, v, labelErr(err), "")
		return
	}
	http.Redirect(w, r, v.Base+"/labels?saved=1", http.StatusSeeOther)
}

func (a *App) labelUpdate(w http.ResponseWriter, r *http.Request) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		a.notFound(w, r)
		return
	}
	err := a.db.UpdateLabel(r.Context(), v.Repo.ID, id, r.FormValue("name"), r.FormValue("color"), r.FormValue("description"))
	if errors.Is(err, store.ErrNotFound) {
		a.notFound(w, r)
		return
	}
	if err != nil {
		a.renderLabels(w, r, v, labelErr(err), "")
		return
	}
	http.Redirect(w, r, v.Base+"/labels?saved=1", http.StatusSeeOther)
}

func (a *App) labelDelete(w http.ResponseWriter, r *http.Request) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		a.notFound(w, r)
		return
	}
	if err := a.db.DeleteLabel(r.Context(), v.Repo.ID, id); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/labels", http.StatusSeeOther)
}

// labelsCSS serves one class per label (.lbl-<id>) so pills can carry the
// label's own colour without inline styles (which the CSP forbids).
func (a *App) labelsCSS(w http.ResponseWriter, r *http.Request) {
	rp, err := a.db.RepoByName(r.Context(), r.PathValue("owner"), r.PathValue("repo"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	labels, err := a.db.Labels(r.Context(), rp.ID)
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	var b strings.Builder
	for _, l := range labels {
		b.WriteString(labelRule(".lbl-"+strconv.FormatInt(l.ID, 10), l.Color))
	}
	_, _ = w.Write([]byte(b.String()))
}

// ---------- label colours ----------

type rgb struct{ r, g, b float64 }

func parseHex(c string) rgb {
	c, ok := store.NormalizeColor(c)
	if !ok {
		c = "a8a299"
	}
	v, _ := strconv.ParseUint(c, 16, 32)
	return rgb{float64(v >> 16 & 0xff), float64(v >> 8 & 0xff), float64(v & 0xff)}
}

func (c rgb) hex() string {
	cl := func(x float64) int { return int(math.Round(math.Max(0, math.Min(255, x)))) }
	return fmt.Sprintf("#%02x%02x%02x", cl(c.r), cl(c.g), cl(c.b))
}

func mix(a, b rgb, t float64) rgb {
	return rgb{a.r + (b.r-a.r)*t, a.g + (b.g-a.g)*t, a.b + (b.b-a.b)*t}
}

func luminance(c rgb) float64 {
	ch := func(x float64) float64 {
		x /= 255
		if x <= 0.03928 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(c.r) + 0.7152*ch(c.g) + 0.0722*ch(c.b)
}

func contrast(a, b rgb) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

var pillSurface = rgb{0x1a, 0x19, 0x17} // --surface

// labelColors returns a pill's background, border and text colours: a tint of
// the label colour on Atelier's dark surface, with the text lightened until it
// reaches WCAG AA (4.5:1) against that tint.
func labelColors(hex string) (bg, border, fg string) {
	c := parseHex(hex)
	bgc := mix(pillSurface, c, 0.22)
	fgc := c
	for t := 0.0; t <= 1.0001; t += 0.05 {
		fgc = mix(c, rgb{255, 255, 255}, t)
		if contrast(fgc, bgc) >= 4.5 {
			break
		}
	}
	return bgc.hex(), mix(pillSurface, c, 0.6).hex(), fgc.hex()
}

func labelRule(sel, hex string) string {
	bg, border, fg := labelColors(hex)
	dot := parseHex(hex).hex()
	return fmt.Sprintf("%s{background:%s;border-color:%s;color:%s}%s .lbl-dot,%s.lbl-dot{background:%s}\n", sel, bg, border, fg, sel, sel, dot)
}

// ---------- milestones ----------

type milestoneView struct {
	store.Milestone
	HTML      template.HTML
	IssuesURL string
	OpenURL   string
	ClosedURL string
}

func (a *App) milestonesPage(w http.ResponseWriter, r *http.Request) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	state := "open"
	if r.URL.Query().Get("state") == "closed" {
		state = "closed"
	}
	ms, err := a.db.Milestones(ctx, v.Repo.ID, state)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	open, closed, err := a.db.MilestoneCounts(ctx, v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	render := a.issueMarkdown(r, v.Repo.ID, v.Base)
	views := make([]milestoneView, len(ms))
	for k, m := range ms {
		views[k] = milestoneView{Milestone: m, HTML: render(m.Description),
			IssuesURL: issuesURL(v.Base, store.IssueQuery{Milestone: m.Title}, "", 1),
			OpenURL:   issuesURL(v.Base, store.IssueQuery{State: "open", Milestone: m.Title}, "", 1),
			ClosedURL: issuesURL(v.Base, store.IssueQuery{State: "closed", Milestone: m.Title}, "", 1),
		}
	}
	a.render(w, r, "issues_milestones", page{Title: "Milestones · " + v.Repo.FullName(), Nav: "repos", Notice: r.URL.Query().Get("saved"), Data: map[string]any{
		"V": v, "Active": "milestones", "State": state, "Milestones": views, "Open": open, "Closed": closed,
	}})
}

func (a *App) renderMilestoneForm(w http.ResponseWriter, r *http.Request, v *repoView, m *store.Milestone, title, desc, due, errMsg string) {
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	name := "New milestone"
	if m != nil {
		name = "Edit milestone"
	}
	a.render(w, r, "issues_milestone_form", page{Title: name + " · " + v.Repo.FullName(), Nav: "repos", Error: errMsg, Data: map[string]any{
		"V": v, "M": m, "Heading": name, "MTitle": title, "Desc": desc, "Due": due,
	}})
}

func parseDue(s string) (*time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, true
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, false
	}
	return &t, true
}

func (a *App) milestoneNewPage(w http.ResponseWriter, r *http.Request) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return
	}
	a.renderMilestoneForm(w, r, v, nil, "", "", "", "")
}

func (a *App) milestoneCreate(w http.ResponseWriter, r *http.Request) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return
	}
	title, desc, dueS := r.FormValue("title"), normalizeNewlines(r.FormValue("description")), r.FormValue("due")
	due, ok := parseDue(dueS)
	if !ok {
		a.renderMilestoneForm(w, r, v, nil, title, desc, dueS, "Use a due date like 2026-12-31, or leave it empty.")
		return
	}
	if _, err := a.db.CreateMilestone(r.Context(), v.Repo.ID, title, desc, due); err != nil {
		a.renderMilestoneForm(w, r, v, nil, title, desc, dueS, milestoneErr(err))
		return
	}
	http.Redirect(w, r, v.Base+"/milestones?saved=1", http.StatusSeeOther)
}

func milestoneErr(err error) string {
	if errors.Is(err, store.ErrInvalidMilestone) {
		return "Milestone titles need 1–100 characters and no quotes."
	}
	return err.Error()
}

func (a *App) loadMilestone(w http.ResponseWriter, r *http.Request) (*repoView, store.Milestone, bool) {
	v, ok := a.loadIssuesRepo(w, r)
	if !ok {
		return nil, store.Milestone{}, false
	}
	id, ok := pathInt(r, "id")
	if !ok {
		a.notFound(w, r)
		return nil, store.Milestone{}, false
	}
	m, err := a.db.MilestoneByID(r.Context(), v.Repo.ID, id)
	if err != nil {
		a.fail(w, r, err)
		return nil, store.Milestone{}, false
	}
	return v, m, true
}

func (a *App) milestoneEditPage(w http.ResponseWriter, r *http.Request) {
	v, m, ok := a.loadMilestone(w, r)
	if !ok {
		return
	}
	due := ""
	if m.DueOn != nil {
		due = m.DueOn.Format("2006-01-02")
	}
	a.renderMilestoneForm(w, r, v, &m, m.Title, m.Description, due, "")
}

func (a *App) milestoneUpdate(w http.ResponseWriter, r *http.Request) {
	v, m, ok := a.loadMilestone(w, r)
	if !ok {
		return
	}
	title, desc, dueS := r.FormValue("title"), normalizeNewlines(r.FormValue("description")), r.FormValue("due")
	due, ok := parseDue(dueS)
	if !ok {
		a.renderMilestoneForm(w, r, v, &m, title, desc, dueS, "Use a due date like 2026-12-31, or leave it empty.")
		return
	}
	if err := a.db.UpdateMilestone(r.Context(), v.Repo.ID, m.ID, title, desc, due); err != nil {
		a.renderMilestoneForm(w, r, v, &m, title, desc, dueS, milestoneErr(err))
		return
	}
	http.Redirect(w, r, v.Base+"/milestones?saved=1", http.StatusSeeOther)
}

func (a *App) milestoneState(w http.ResponseWriter, r *http.Request) {
	v, m, ok := a.loadMilestone(w, r)
	if !ok {
		return
	}
	open := r.FormValue("action") == "reopen"
	if err := a.db.SetMilestoneState(r.Context(), v.Repo.ID, m.ID, open); err != nil {
		a.fail(w, r, err)
		return
	}
	back := v.Base + "/milestones"
	if open {
		back += "?state=closed"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (a *App) milestoneDelete(w http.ResponseWriter, r *http.Request) {
	v, m, ok := a.loadMilestone(w, r)
	if !ok {
		return
	}
	if err := a.db.DeleteMilestone(r.Context(), v.Repo.ID, m.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, v.Base+"/milestones", http.StatusSeeOther)
}
