package web

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"atelier/internal/store"
)

// Issues for the app API: thin JSON over the issue store, with the same
// events as the web pages and the MCP tools (issue.opened, issue.commented,
// issue.closed, issue.reopened), so notifications, agents and Connect hear
// about a change made from the app like any other.

const (
	appIssuesPer    = 30
	appIssuesMaxPer = 100
)

func appIssueRoutes(a *App, m *http.ServeMux) {
	const p = appAPIPrefix
	m.HandleFunc("GET "+p+"/issues", a.appAuthed(a.appIssues))
	m.HandleFunc("POST "+p+"/repos/{owner}/{repo}/issues", a.appAuthed(a.appIssueCreate))
	m.HandleFunc("GET "+p+"/repos/{owner}/{repo}/issues/{n}", a.appAuthed(a.appIssue))
	m.HandleFunc("PATCH "+p+"/repos/{owner}/{repo}/issues/{n}", a.appAuthed(a.appIssueState))
	m.HandleFunc("POST "+p+"/repos/{owner}/{repo}/issues/{n}/comments", a.appAuthed(a.appIssueComment))
}

// appIssueJSON is the contract's issue summary.
func (a *App) appIssueJSON(r *http.Request, rp store.Repo, is store.Issue) map[string]any {
	labels := []map[string]any{}
	for _, l := range is.Labels {
		labels = append(labels, map[string]any{"name": l.Name, "color": "#" + l.Color})
	}
	path := fmt.Sprintf("/%s/issues/%d", rp.FullName(), is.Number)
	return map[string]any{
		"repo": rp.FullName(), "number": is.Number, "title": is.Title, "state": is.State,
		"author": is.AuthorKind + ":" + is.AuthorName, "via": appVia(is.AuthorKind, is.AuthorName),
		"labels": labels, "comments": is.Comments,
		"created_at": appTime(is.CreatedAt), "updated_at": appTime(is.UpdatedAt),
		"web_path": path, "web_url": a.baseURL(r) + path,
	}
}

// appCommentJSON is a comment as pull requests show theirs, with created_at.
func appCommentJSON(c store.IssueComment) map[string]any {
	author := strings.TrimPrefix(c.AuthorName, tetherAuthorPrefix)
	kind := "person"
	switch {
	case c.AuthorKind == ActorBot, c.AuthorKind == "tether", strings.HasSuffix(author, "[bot]"):
		kind = "bot"
	case c.AuthorKind == "agent", c.AuthorKind == ActorJob:
		kind = "agent"
	}
	return map[string]any{"id": c.ID, "author": author, "via": appVia(c.AuthorKind, c.AuthorName), "kind": kind,
		"body": c.Body, "created_at": appTime(c.CreatedAt)}
}

// appIssueFull is the issue with its Markdown body and its comments.
func (a *App) appIssueFull(r *http.Request, rp store.Repo, is store.Issue) (map[string]any, error) {
	cs, err := a.db.IssueComments(r.Context(), is.ID)
	if err != nil {
		return nil, err
	}
	out := a.appIssueJSON(r, rp, is)
	comments := []map[string]any{}
	for _, c := range cs {
		comments = append(comments, appCommentJSON(c))
	}
	out["body"], out["comments"] = is.Body, comments
	return out, nil
}

func appPageParams(r *http.Request) (page, per int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	per, _ = strconv.Atoi(r.URL.Query().Get("per_page"))
	if page < 1 {
		page = 1
	}
	if per < 1 {
		per = appIssuesPer
	}
	if per > appIssuesMaxPer {
		per = appIssuesMaxPer
	}
	if page > 1000 {
		page = 1000
	}
	return page, per
}

func (a *App) appIssues(w http.ResponseWriter, r *http.Request, c *appCaller) {
	ctx := r.Context()
	q := store.IssueQuery{}
	switch s := r.URL.Query().Get("state"); s {
	case "", "open":
		q.State = "open"
	case "closed":
		q.State = "closed"
	case "all":
	default:
		appError(w, http.StatusUnprocessableEntity, "invalid_state", "state must be open, closed or all.")
		return
	}
	page, per := appPageParams(r)
	out := []map[string]any{}
	if name := r.URL.Query().Get("repo"); name != "" { // one repository, like /runs?repo=
		owner, repo, _ := strings.Cut(name, "/")
		rp, ok := a.appRepoNamed(w, r, c, owner, repo, PermRead)
		if !ok {
			return
		}
		issues, more, err := a.db.Issues(ctx, rp.ID, q, (page-1)*per, per)
		if err != nil {
			a.appFail(w, r, err)
			return
		}
		for _, is := range issues {
			out = append(out, a.appIssueJSON(r, rp, is))
		}
		writeJSON(w, http.StatusOK, map[string]any{"issues": out, "has_more": more})
		return
	}
	// Every readable repository, newest first: the first page*per_page of
	// each is enough to know the page asked for.
	repos, byID, err := a.appReadable(ctx, c.Who)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	var all []store.Issue
	for _, rp := range repos {
		issues, _, err := a.db.Issues(ctx, rp.ID, q, 0, page*per+1)
		if err != nil {
			a.appFail(w, r, err)
			return
		}
		all = append(all, issues...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].CreatedAt.After(all[j].CreatedAt)
		}
		return all[i].ID > all[j].ID
	})
	start := min((page-1)*per, len(all))
	end := min(start+per, len(all))
	for _, is := range all[start:end] {
		out = append(out, a.appIssueJSON(r, byID[is.RepoID], is))
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": out, "has_more": end < len(all)})
}

// appLoadIssue loads the repository (checking need) and the issue in the path.
func (a *App) appLoadIssue(w http.ResponseWriter, r *http.Request, c *appCaller, need Perm) (store.Repo, store.Issue, bool) {
	rp, ok := a.appRepo(w, r, c, need)
	if !ok {
		return rp, store.Issue{}, false
	}
	n, ok := appPathID(r, "n")
	if !ok {
		appError(w, http.StatusNotFound, "not_found", "There's no such issue.")
		return rp, store.Issue{}, false
	}
	is, err := a.db.IssueByNumber(r.Context(), rp.ID, n)
	if err != nil {
		appError(w, http.StatusNotFound, "not_found", "There's no such issue.")
		return rp, store.Issue{}, false
	}
	return rp, is, true
}

// appIssueReply answers with the issue as it stands now.
func (a *App) appIssueReply(w http.ResponseWriter, r *http.Request, status int, rp store.Repo, number int64) {
	is, err := a.db.IssueByNumber(r.Context(), rp.ID, number)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	out, err := a.appIssueFull(r, rp, is)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, status, out)
}

func (a *App) appIssue(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, is, ok := a.appLoadIssue(w, r, c, PermRead)
	if !ok {
		return
	}
	out, err := a.appIssueFull(r, rp, is)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// appIssueBody normalises a Markdown body like the web forms do.
func appIssueBody(w http.ResponseWriter, s string, required bool) (string, bool) {
	body := mcpNormalize(s)
	if required && body == "" {
		appError(w, http.StatusUnprocessableEntity, "empty_body", "Write something first.")
		return "", false
	}
	if len(body) > pullBodyMax {
		appError(w, http.StatusUnprocessableEntity, "body_too_long", "That's too long (at most "+strconv.Itoa(pullBodyMax)+" bytes).")
		return "", false
	}
	return body, true
}

func (a *App) appIssueCreate(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, ok := a.appRepo(w, r, c, PermWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	invalid := func() {
		appError(w, http.StatusUnprocessableEntity, "invalid_title", "Give the issue a title: one line, up to 256 characters.")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" || strings.ContainsAny(title, "\n\r") {
		invalid()
		return
	}
	body, ok := appIssueBody(w, in.Body, false)
	if !ok {
		return
	}
	if err := a.db.EnsureDefaultLabels(ctx, rp.ID); err != nil { // as opening the Issues tab does
		a.appFail(w, r, err)
		return
	}
	is, err := a.db.CreateIssue(ctx, rp.ID, title, body, c.Who.Kind, c.Who.Name)
	if errors.Is(err, store.ErrInvalidIssue) {
		invalid()
		return
	}
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	a.emit(ctx, issueEvent(EventIssueOpened, appsView(rp), is, c.Who, body))
	a.appIssueReply(w, r, http.StatusCreated, rp, is.Number)
}

func (a *App) appIssueComment(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, is, ok := a.appLoadIssue(w, r, c, PermWrite)
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	body, ok := appIssueBody(w, in.Body, true)
	if !ok {
		return
	}
	cm, err := a.db.AddIssueComment(r.Context(), is.ID, body, c.Who.Kind, c.Who.Name)
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	a.emit(r.Context(), issueEvent(EventIssueCommented, appsView(rp), is, c.Who, body, fmt.Sprintf("#comment-%d", cm.ID)))
	writeJSON(w, http.StatusCreated, appCommentJSON(cm))
}

func (a *App) appIssueState(w http.ResponseWriter, r *http.Request, c *appCaller) {
	rp, is, ok := a.appLoadIssue(w, r, c, PermWrite)
	if !ok {
		return
	}
	ctx := r.Context()
	var in struct {
		State string `json:"state"`
	}
	if !appDecode(w, r, &in) {
		return
	}
	var err error
	action := ""
	switch {
	case in.State != "open" && in.State != "closed":
		appError(w, http.StatusUnprocessableEntity, "invalid_state", "state must be open or closed.")
		return
	case in.State == is.State: // nothing to do
	case in.State == "closed":
		action, err = "close", a.db.CloseIssue(ctx, is.ID, c.Who.Kind, c.Who.Name, store.ReasonCompleted)
	default:
		action, err = "reopen", a.db.ReopenIssue(ctx, is.ID, c.Who.Kind, c.Who.Name)
	}
	if err != nil {
		a.appFail(w, r, err)
		return
	}
	a.emitIssueState(ctx, appsView(rp), is, c.Who, action)
	a.appIssueReply(w, r, http.StatusOK, rp, is.Number)
}
