package web

// A GitHub-compatible REST subset at /api/v3, for bots and tools that speak
// GitHub's API (the runner sets GITHUB_API_URL to <server>/api/v3).
// Credentials: bot tokens, access tokens and Actions job tokens, as
// Authorization: Bearer …, "token …" or Basic. No session cookies, so there
// is no CSRF surface. Writes need the matching bot permission, or write
// access for other credentials. Nothing here merges.
//
//	GET    /api/v3/user
//	GET    /api/v3/repos/{owner}/{repo}
//	GET    /api/v3/repos/{owner}/{repo}/issues/{n}
//	GET    /api/v3/repos/{owner}/{repo}/issues/{n}/comments
//	POST   /api/v3/repos/{owner}/{repo}/issues/{n}/comments          {body}            (issues and pull requests)
//	GET    /api/v3/repos/{owner}/{repo}/issues/{n}/labels
//	POST   /api/v3/repos/{owner}/{repo}/issues/{n}/labels            {labels: [names]} or [names]
//	DELETE /api/v3/repos/{owner}/{repo}/issues/{n}/labels/{name}
//	GET    /api/v3/repos/{owner}/{repo}/pulls/{n}
//	GET    /api/v3/repos/{owner}/{repo}/pulls/{n}/files
//	GET    /api/v3/repos/{owner}/{repo}/pulls/{n}/comments
//	POST   /api/v3/repos/{owner}/{repo}/pulls/{n}/comments           {body, commit_id, path, line, side} or {…, position}  → an Atelier note
//	PUT    /api/v3/repos/{owner}/{repo}/pulls/{n}/merge              always 403: merge in Atelier
//	POST   /api/v3/repos/{owner}/{repo}/statuses/{sha}               {state, context, description, target_url}
//	GET    /api/v3/repos/{owner}/{repo}/commits/{ref}/status         combined status
//	GET    /api/v3/repos/{owner}/{repo}/commits/{ref}/statuses
//	POST   /api/v3/repos/{owner}/{repo}/check-runs                   {name, head_sha, status, conclusion, details_url, output}
//	PATCH  /api/v3/repos/{owner}/{repo}/check-runs/{id}
//	GET    /api/v3/repos/{owner}/{repo}/commits/{ref}/check-runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

func appsAPIRoutes(a *App, m *http.ServeMux) {
	const p = "/api/v3/repos/{owner}/{repo}"
	m.HandleFunc("GET /api/v3/user", a.apiV3User)
	m.HandleFunc("GET "+p, a.apiV3Repo)
	m.HandleFunc("GET "+p+"/issues/{n}", a.apiV3Issue)
	m.HandleFunc("GET "+p+"/issues/{n}/comments", a.apiV3IssueComments)
	m.HandleFunc("POST "+p+"/issues/{n}/comments", a.apiV3IssueCommentCreate)
	m.HandleFunc("GET "+p+"/issues/{n}/labels", a.apiV3Labels)
	m.HandleFunc("POST "+p+"/issues/{n}/labels", a.apiV3LabelsAdd)
	m.HandleFunc("DELETE "+p+"/issues/{n}/labels/{name}", a.apiV3LabelRemove)
	m.HandleFunc("GET "+p+"/pulls/{n}", a.apiV3Pull)
	m.HandleFunc("GET "+p+"/pulls/{n}/files", a.apiV3PullFiles)
	m.HandleFunc("GET "+p+"/pulls/{n}/comments", a.apiV3ReviewComments)
	m.HandleFunc("POST "+p+"/pulls/{n}/comments", a.apiV3ReviewCommentCreate)
	m.HandleFunc("PUT "+p+"/pulls/{n}/merge", a.apiV3Merge)
	m.HandleFunc("POST "+p+"/statuses/{sha}", a.apiV3StatusCreate)
	m.HandleFunc("GET "+p+"/commits/{ref}/status", a.apiV3CombinedStatus)
	m.HandleFunc("GET "+p+"/commits/{ref}/statuses", a.apiV3Statuses)
	m.HandleFunc("POST "+p+"/check-runs", a.apiV3CheckRunCreate)
	m.HandleFunc("PATCH "+p+"/check-runs/{id}", a.apiV3CheckRunUpdate)
	m.HandleFunc("GET "+p+"/commits/{ref}/check-runs", a.apiV3CheckRuns)
}

// apiV3Auth authenticates the caller (anonymous is allowed for reads of public repositories).
func (a *App) apiV3Auth(w http.ResponseWriter, r *http.Request) (Actor, bool) {
	who, valid := a.requestActor(r, false)
	if !valid {
		if !a.lim.allow(a.clientIP(r)) {
			writeAPIError(w, http.StatusTooManyRequests, "Too many attempts, try again in a minute.")
			return who, false
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier"`)
		writeAPIError(w, http.StatusUnauthorized, "Bad credentials")
		return who, false
	}
	return who, true
}

// apiV3Repo loads the repository and checks op: "read", or a write
// operation ("comment", "suggest", "checks").
func (a *App) apiV3RepoFor(w http.ResponseWriter, r *http.Request, op string) (store.Repo, Actor, bool) {
	ctx := r.Context()
	who, ok := a.apiV3Auth(w, r)
	if !ok {
		return store.Repo{}, who, false
	}
	repo, err := a.db.RepoByName(ctx, r.PathValue("owner"), r.PathValue("repo"))
	if err != nil || !a.can(ctx, who, repo, PermRead) {
		if err == nil && who.Kind == ActorAnonymous {
			w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier"`)
			writeAPIError(w, http.StatusUnauthorized, "Requires authentication")
			return repo, who, false
		}
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return repo, who, false
	}
	if op != "read" && !a.appsMayDo(ctx, who, repo, op) {
		if who.Kind == ActorAnonymous {
			w.Header().Set("WWW-Authenticate", `Bearer realm="Atelier"`)
			writeAPIError(w, http.StatusUnauthorized, "Requires authentication")
		} else {
			writeAPIError(w, http.StatusForbidden, appsDeniedText(who, op))
		}
		return repo, who, false
	}
	a.rememberBaseURL(a.baseURL(r))
	return repo, who, true
}

func appsDeniedText(who Actor, op string) string {
	if who.Kind == ActorBot {
		what := map[string]string{"comment": "comment", "suggest": "suggest changes", "checks": "set checks"}[op]
		return "Resource not accessible by integration: this bot isn't allowed to " + what + " in this repository."
	}
	return "Resource not accessible by this credential: it needs write access to this repository."
}

func apiV3Number(w http.ResponseWriter, r *http.Request) (int64, bool) {
	n, err := strconv.ParseInt(r.PathValue("n"), 10, 64)
	if err != nil || n < 1 {
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return 0, false
	}
	return n, true
}

func apiV3Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeAPIError(w, http.StatusBadRequest, "Problems parsing JSON")
		return false
	}
	return true
}

func apiV3Invalid(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed", "errors": []map[string]string{{"message": msg}}})
}

func appsView(rp store.Repo) *repoView { return &repoView{Repo: rp, Base: "/" + rp.FullName()} }

// ---------- user and repository ----------

func (a *App) apiV3User(w http.ResponseWriter, r *http.Request) {
	who, ok := a.apiV3Auth(w, r)
	if !ok {
		return
	}
	if who.Kind == ActorAnonymous {
		writeAPIError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	writeJSON(w, http.StatusOK, a.ghUser(r.Context(), who.Kind, who.Name))
}

func (a *App) apiV3Repo(w http.ResponseWriter, r *http.Request) {
	rp, who, ok := a.apiV3RepoFor(w, r, "read")
	if !ok {
		return
	}
	ctx := r.Context()
	out := a.ghRepo(ctx, rp)
	write := a.can(ctx, who, rp, PermWrite)
	out["permissions"] = map[string]bool{"admin": a.can(ctx, who, rp, PermAdmin), "maintain": false, "push": write,
		"triage": write || a.appsMayDo(ctx, who, rp, "comment"), "pull": true}
	writeJSON(w, http.StatusOK, out)
}

// ---------- issues and comments ----------

func (a *App) apiV3Issue(w http.ResponseWriter, r *http.Request) {
	rp, _, ok := a.apiV3RepoFor(w, r, "read")
	if !ok {
		return
	}
	n, ok := apiV3Number(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if is, err := a.db.IssueByNumber(ctx, rp.ID, n); err == nil {
		writeJSON(w, http.StatusOK, a.ghIssue(ctx, rp, is))
		return
	}
	if p, err := a.db.PullByNumber(ctx, rp.ID, n); err == nil {
		writeJSON(w, http.StatusOK, a.ghPullAsIssue(ctx, rp, p))
		return
	}
	writeAPIError(w, http.StatusNotFound, "Not Found")
}

func (a *App) apiV3IssueComments(w http.ResponseWriter, r *http.Request) {
	rp, _, ok := a.apiV3RepoFor(w, r, "read")
	if !ok {
		return
	}
	n, ok := apiV3Number(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out := []map[string]any{}
	if is, err := a.db.IssueByNumber(ctx, rp.ID, n); err == nil {
		cs, err := a.db.IssueComments(ctx, is.ID)
		if err != nil {
			a.apiFail(w, r, err)
			return
		}
		for _, c := range cs {
			out = append(out, a.ghComment(ctx, rp, n, c.ID, c.AuthorKind, c.AuthorName, c.Body, c.CreatedAt, c.UpdatedAt, false))
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	p, err := a.db.PullByNumber(ctx, rp.ID, n)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return
	}
	cs, err := a.db.PullComments(ctx, p.ID)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	for _, c := range cs {
		out = append(out, a.ghComment(ctx, rp, n, c.ID, c.AuthorKind, c.AuthorName, c.Body, c.CreatedAt, c.UpdatedAt, true))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) apiV3IssueCommentCreate(w http.ResponseWriter, r *http.Request) {
	rp, who, ok := a.apiV3RepoFor(w, r, "comment")
	if !ok {
		return
	}
	n, ok := apiV3Number(w, r)
	if !ok {
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if !apiV3Decode(w, r, &in) {
		return
	}
	body := normalizePullText(in.Body)
	if strings.TrimSpace(body) == "" || len(body) > pullBodyMax {
		apiV3Invalid(w, "body is missing or too long")
		return
	}
	ctx := r.Context()
	v := appsView(rp)
	if is, err := a.db.IssueByNumber(ctx, rp.ID, n); err == nil {
		c, err := a.db.AddIssueComment(ctx, is.ID, body, who.Kind, who.Name)
		if err != nil {
			a.apiFail(w, r, err)
			return
		}
		a.emit(ctx, issueEvent(EventIssueCommented, v, is, who, body, fmt.Sprintf("#comment-%d", c.ID)))
		writeJSON(w, http.StatusCreated, a.ghComment(ctx, rp, n, c.ID, c.AuthorKind, c.AuthorName, c.Body, c.CreatedAt, c.UpdatedAt, false))
		return
	}
	p, err := a.db.PullByNumber(ctx, rp.ID, n)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return
	}
	c, err := a.db.AddPullComment(ctx, p.ID, who.Kind, who.Name, body)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	a.emit(ctx, pullEvent(EventPullCommented, v, p, who, body, fmt.Sprintf("#comment-%d", c.ID)))
	writeJSON(w, http.StatusCreated, a.ghComment(ctx, rp, n, c.ID, c.AuthorKind, c.AuthorName, c.Body, c.CreatedAt, c.UpdatedAt, true))
}

// ---------- labels ----------

// apiV3LabelTarget is the issue or pull request an issues/{n}/labels call
// names: on GitHub every pull request is also an issue.
type apiV3LabelTarget struct {
	repo   store.Repo
	issue  *store.Issue
	pull   *store.Pull
	labels []store.Label
}

func (a *App) apiV3LabelTargetFor(w http.ResponseWriter, r *http.Request, op string) (apiV3LabelTarget, Actor, bool) {
	rp, who, ok := a.apiV3RepoFor(w, r, op)
	if !ok {
		return apiV3LabelTarget{}, who, false
	}
	n, ok := apiV3Number(w, r)
	if !ok {
		return apiV3LabelTarget{}, who, false
	}
	t := apiV3LabelTarget{repo: rp}
	if err := a.apiV3ReloadLabels(r.Context(), &t, n); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "Not Found")
		} else {
			a.apiFail(w, r, err)
		}
		return t, who, false
	}
	return t, who, true
}

// apiV3ReloadLabels (re)reads the issue or pull request numbered n.
func (a *App) apiV3ReloadLabels(ctx context.Context, t *apiV3LabelTarget, n int64) error {
	if is, err := a.db.IssueByNumber(ctx, t.repo.ID, n); err == nil {
		t.issue, t.labels = &is, is.Labels
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	p, err := a.db.PullByNumber(ctx, t.repo.ID, n)
	if err != nil {
		return err
	}
	ls, err := a.db.PullLabels(ctx, p.ID)
	if err != nil {
		return err
	}
	t.pull, t.labels = &p, ls
	return nil
}

func (t apiV3LabelTarget) number() int64 {
	if t.issue != nil {
		return t.issue.Number
	}
	return t.pull.Number
}

// set replaces the labels, then reloads them.
func (a *App) apiV3SetLabels(ctx context.Context, t *apiV3LabelTarget, ids []int64, who Actor) error {
	var err error
	if t.issue != nil {
		err = a.db.SetIssueLabels(ctx, t.issue.ID, ids, who.Kind, who.Name)
	} else {
		err = a.db.SetPullLabels(ctx, t.pull.ID, ids)
	}
	if err != nil {
		return err
	}
	return a.apiV3ReloadLabels(ctx, t, t.number())
}

func (a *App) apiV3Labels(w http.ResponseWriter, r *http.Request) {
	t, _, ok := a.apiV3LabelTargetFor(w, r, "read")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, ghLabels(t.labels))
}

func (a *App) apiV3LabelsAdd(w http.ResponseWriter, r *http.Request) {
	t, who, ok := a.apiV3LabelTargetFor(w, r, "comment")
	if !ok {
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		var obj struct {
			Labels []string `json:"labels"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			writeAPIError(w, http.StatusBadRequest, "Problems parsing JSON")
			return
		}
		names = obj.Labels
	}
	ctx := r.Context()
	all, err := a.db.Labels(ctx, t.repo.ID)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	byName := map[string]store.Label{}
	for _, l := range all {
		byName[strings.ToLower(l.Name)] = l
	}
	ids := []int64{}
	have := map[int64]bool{}
	for _, l := range t.labels {
		ids, have[l.ID] = append(ids, l.ID), true
	}
	before := maps.Clone(have)
	for _, n := range names {
		n = strings.TrimSpace(n)
		l, ok := byName[strings.ToLower(n)]
		if !ok {
			apiV3Invalid(w, fmt.Sprintf("label %q doesn't exist in this repository", n))
			return
		}
		if !have[l.ID] {
			ids, have[l.ID] = append(ids, l.ID), true
		}
	}
	if err := a.apiV3SetLabels(ctx, &t, ids, who); err != nil {
		a.apiFail(w, r, err)
		return
	}
	if t.issue != nil {
		for _, l := range t.labels {
			if !before[l.ID] {
				a.emit(ctx, issueEvent(EventIssueLabeled, appsView(t.repo), *t.issue, who, l.Name))
			}
		}
	}
	writeJSON(w, http.StatusOK, ghLabels(t.labels))
}

func (a *App) apiV3LabelRemove(w http.ResponseWriter, r *http.Request) {
	t, who, ok := a.apiV3LabelTargetFor(w, r, "comment")
	if !ok {
		return
	}
	name := r.PathValue("name")
	ids := []int64{}
	found := false
	for _, l := range t.labels {
		if strings.EqualFold(l.Name, name) {
			found = true
			continue
		}
		ids = append(ids, l.ID)
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "Label does not exist")
		return
	}
	if err := a.apiV3SetLabels(r.Context(), &t, ids, who); err != nil {
		a.apiFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ghLabels(t.labels))
}

// ---------- pull requests ----------

func (a *App) apiV3LoadPull(w http.ResponseWriter, r *http.Request, op string) (store.Repo, store.Pull, Actor, bool) {
	rp, who, ok := a.apiV3RepoFor(w, r, op)
	if !ok {
		return rp, store.Pull{}, who, false
	}
	n, ok := apiV3Number(w, r)
	if !ok {
		return rp, store.Pull{}, who, false
	}
	p, err := a.db.PullByNumber(r.Context(), rp.ID, n)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return rp, p, who, false
	}
	return rp, p, who, true
}

func (a *App) apiV3Pull(w http.ResponseWriter, r *http.Request) {
	rp, p, _, ok := a.apiV3LoadPull(w, r, "read")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, a.ghPull(r.Context(), rp, p))
}

func (a *App) apiV3PullFiles(w http.ResponseWriter, r *http.Request) {
	rp, p, _, ok := a.apiV3LoadPull(w, r, "read")
	if !ok {
		return
	}
	ctx := r.Context()
	head, base := a.appsPullSHAs(ctx, rp, p)
	out := []map[string]any{}
	if head == "" || base == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	cmp, err := g.Compare(ctx, base, head)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	if cmp.Diff == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	web := a.appsBase() + "/" + rp.FullName()
	for _, f := range cmp.Diff.Files {
		status := f.Status
		if status == "" {
			status = "modified"
		}
		m := map[string]any{
			"sha": "", "filename": f.Path, "status": status, "additions": f.Additions, "deletions": f.Deletions,
			"changes": f.Additions + f.Deletions, "blob_url": web + "/blob/" + head + "/" + f.Path,
			"raw_url": web + "/raw/" + head + "/" + f.Path,
		}
		if f.OldPath != "" {
			m["previous_filename"] = f.OldPath
		}
		if !f.Binary {
			m["patch"] = appsPatch(&f)
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, out)
}

// appsPatch rebuilds a unified diff patch (GitHub's "patch" field).
func appsPatch(f *gitrepo.DiffFile) string {
	var b strings.Builder
	for _, l := range f.Lines {
		switch l.Kind {
		case "hunk":
			b.WriteString(l.Text)
		case "add":
			b.WriteString("+" + l.Text)
		case "del":
			b.WriteString("-" + l.Text)
		case "ctx":
			b.WriteString(" " + l.Text)
		default:
			continue
		}
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (a *App) apiV3ReviewComments(w http.ResponseWriter, r *http.Request) {
	rp, p, _, ok := a.apiV3LoadPull(w, r, "read")
	if !ok {
		return
	}
	ctx := r.Context()
	notes, err := a.db.PullNotes(ctx, p.ID)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	out := []map[string]any{}
	diffs := apiV3Diffs{}
	for _, n := range notes {
		out = append(out, a.apiV3ReviewCommentJSON(ctx, rp, p, n, diffs))
	}
	writeJSON(w, http.StatusOK, out)
}

// apiV3Diffs caches pull request diffs by commit for one request.
type apiV3Diffs map[string]*gitrepo.Diff

// apiV3PullDiff is the pull request's diff as of commit (its base to commit).
func (a *App) apiV3PullDiff(ctx context.Context, rp store.Repo, p store.Pull, commit string, cache apiV3Diffs) *gitrepo.Diff {
	if d, ok := cache[commit]; ok {
		return d
	}
	cache[commit] = nil
	_, base := a.appsPullSHAs(ctx, rp, p)
	if base == "" || commit == "" {
		return nil
	}
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil {
		return nil
	}
	cmp, err := g.Compare(ctx, base, commit)
	if err != nil {
		return nil
	}
	cache[commit] = cmp.Diff
	return cmp.Diff
}

// apiV3ReviewCommentJSON adds the diff position and hunk to a review comment
// when its line is part of the diff at its commit.
func (a *App) apiV3ReviewCommentJSON(ctx context.Context, rp store.Repo, p store.Pull, n store.PullNote, diffs apiV3Diffs) map[string]any {
	m := a.ghReviewComment(ctx, rp, p, n)
	if f := appsDiffFile(a.apiV3PullDiff(ctx, rp, p, n.CommitSHA, diffs), n.Path); f != nil {
		if pos, ok := appsLinePosition(f, n.Line, n.Side); ok {
			m["position"], m["original_position"], m["diff_hunk"] = pos, pos, appsDiffHunk(f, pos)
		}
	}
	return m
}

var appsSHARe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func (a *App) apiV3ReviewCommentCreate(w http.ResponseWriter, r *http.Request) {
	rp, p, who, ok := a.apiV3LoadPull(w, r, "suggest")
	if !ok {
		return
	}
	var in struct {
		Body     string `json:"body"`
		CommitID string `json:"commit_id"`
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Side     string `json:"side"`
		Position *int   `json:"position"`
	}
	if !apiV3Decode(w, r, &in) {
		return
	}
	ctx := r.Context()
	if in.CommitID == "" {
		in.CommitID, _ = a.appsPullSHAs(ctx, rp, p)
	}
	side := "new"
	if strings.EqualFold(in.Side, "LEFT") {
		side = "old"
	}
	body := normalizePullText(in.Body)
	switch {
	case strings.TrimSpace(body) == "" || len(body) > pullBodyMax:
		apiV3Invalid(w, "body is missing or too long")
		return
	case !appsSHARe.MatchString(in.CommitID):
		apiV3Invalid(w, "commit_id must be a full commit SHA")
		return
	case in.Path == "" || len(in.Path) > 4096:
		apiV3Invalid(w, "path is required")
		return
	}
	diffs := apiV3Diffs{}
	if in.Line < 1 && in.Position != nil {
		// GitHub's legacy diff position: map it to a line and side.
		f := appsDiffFile(a.apiV3PullDiff(ctx, rp, p, in.CommitID, diffs), in.Path)
		if f == nil {
			apiV3Invalid(w, "path isn't part of the pull request's diff at commit_id")
			return
		}
		line, s, ok := appsPositionLine(f, *in.Position)
		if !ok {
			apiV3Invalid(w, "position isn't a line of the diff for path")
			return
		}
		in.Line, side = line, s
	}
	if in.Line < 1 {
		apiV3Invalid(w, "line is required (or a diff position)")
		return
	}
	n, err := a.db.AddPullNote(ctx, store.NewPullNote{PullID: p.ID, CommitSHA: in.CommitID, Path: in.Path, Side: side,
		Line: in.Line, Body: body, AuthorKind: who.Kind, AuthorName: who.Name})
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	a.emit(ctx, pullEvent(EventPullNote, appsView(rp), p, who, body, fmt.Sprintf("/files#note-%d", n.ID)))
	writeJSON(w, http.StatusCreated, a.apiV3ReviewCommentJSON(ctx, rp, p, n, diffs))
}

func (a *App) apiV3Merge(w http.ResponseWriter, r *http.Request) {
	if _, _, _, ok := a.apiV3LoadPull(w, r, "read"); !ok {
		return
	}
	writeAPIError(w, http.StatusForbidden, "Merging isn't available through the API: bots and tokens can't merge in Atelier. Merge from the pull request page.")
}

// ---------- statuses and check runs ----------

func (a *App) apiV3StatusJSON(rp store.Repo, s store.CommitStatus) map[string]any {
	ctx := context.Background()
	return map[string]any{
		"id": s.ID, "node_id": "", "state": s.State, "context": s.Context, "description": nilIfEmpty(s.Description),
		"target_url": nilIfEmpty(s.TargetURL), "created_at": ghTime(s.CreatedAt), "updated_at": ghTime(s.UpdatedAt),
		"creator": a.ghUser(ctx, s.CreatorKind, s.CreatorName),
		"url":     fmt.Sprintf("%s/api/v3/repos/%s/statuses/%s", a.appsBase(), rp.FullName(), s.SHA),
	}
}

func (a *App) apiV3CheckRunJSON(rp store.Repo, s store.CommitStatus) map[string]any {
	var concl, done any
	if s.Conclusion != "" {
		concl = s.Conclusion
	}
	if s.RunStatus == "completed" {
		done = ghTime(s.UpdatedAt)
	}
	return map[string]any{
		"id": s.ID, "node_id": "", "name": s.Context, "head_sha": s.SHA, "status": s.RunStatus, "conclusion": concl,
		"details_url": nilIfEmpty(s.TargetURL), "html_url": nilIfEmpty(s.TargetURL), "external_id": "",
		"started_at": ghTime(s.CreatedAt), "completed_at": done,
		"output": map[string]any{"title": nilIfEmpty(s.Description), "summary": nilIfEmpty(s.Description)},
		"app":    map[string]any{"slug": s.CreatorName, "name": s.CreatorName},
		"url":    fmt.Sprintf("%s/api/v3/repos/%s/check-runs/%d", a.appsBase(), rp.FullName(), s.ID),
	}
}

// apiV3Commit resolves a SHA or ref to a commit in the repository.
func (a *App) apiV3Commit(ctx context.Context, rp store.Repo, ref string) (string, bool) {
	g, err := a.git.Open(rp.Owner, rp.Name)
	if err != nil || ref == "" || strings.HasPrefix(ref, "-") {
		return "", false
	}
	sha, err := g.ResolveCommit(ctx, ref)
	return sha, err == nil
}

func appsTrim(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func appsSafeURL(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://") {
		return appsTrim(s, 2048)
	}
	return "" // never store javascript: and friends
}

func (a *App) apiV3StatusCreate(w http.ResponseWriter, r *http.Request) {
	rp, who, ok := a.apiV3RepoFor(w, r, "checks")
	if !ok {
		return
	}
	var in struct {
		State       string `json:"state"`
		TargetURL   string `json:"target_url"`
		Description string `json:"description"`
		Context     string `json:"context"`
	}
	if !apiV3Decode(w, r, &in) {
		return
	}
	switch in.State {
	case "error", "failure", "pending", "success":
	default:
		apiV3Invalid(w, "state must be error, failure, pending or success")
		return
	}
	ctx := r.Context()
	sha, ok := a.apiV3Commit(ctx, rp, r.PathValue("sha"))
	if !ok {
		apiV3Invalid(w, "No commit found for SHA: "+r.PathValue("sha"))
		return
	}
	if in.Context == "" {
		in.Context = "default"
	}
	st, err := a.db.SetCommitStatus(ctx, store.CommitStatus{RepoID: rp.ID, SHA: sha, Context: appsTrim(in.Context, 200), Kind: "status",
		State: in.State, Description: appsTrim(in.Description, 1000), TargetURL: appsSafeURL(in.TargetURL),
		CreatorKind: who.Kind, CreatorName: who.Name})
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.apiV3StatusJSON(rp, st))
}

func (a *App) apiV3Statuses(w http.ResponseWriter, r *http.Request) {
	rp, _, ok := a.apiV3RepoFor(w, r, "read")
	if !ok {
		return
	}
	ctx := r.Context()
	sha, ok := a.apiV3Commit(ctx, rp, r.PathValue("ref"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "No commit found for SHA: "+r.PathValue("ref"))
		return
	}
	sts, err := a.db.CommitStatuses(ctx, rp.ID, sha)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	out := []map[string]any{}
	for _, s := range sts {
		if s.Kind == "status" {
			out = append(out, a.apiV3StatusJSON(rp, s))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) apiV3CombinedStatus(w http.ResponseWriter, r *http.Request) {
	rp, _, ok := a.apiV3RepoFor(w, r, "read")
	if !ok {
		return
	}
	ctx := r.Context()
	sha, ok := a.apiV3Commit(ctx, rp, r.PathValue("ref"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "No commit found for SHA: "+r.PathValue("ref"))
		return
	}
	sts, err := a.db.CommitStatuses(ctx, rp.ID, sha)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	state := "success"
	list := []map[string]any{}
	n := 0
	for _, s := range sts {
		if s.Kind != "status" {
			continue
		}
		n++
		list = append(list, a.apiV3StatusJSON(rp, s))
		switch {
		case s.State == "failure" || s.State == "error":
			state = "failure"
		case s.State == "pending" && state != "failure":
			state = "pending"
		}
	}
	if n == 0 {
		state = "pending"
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": state, "sha": sha, "total_count": n, "statuses": list,
		"repository": a.ghRepo(ctx, rp)})
}

type apiV3CheckRunIn struct {
	Name       *string `json:"name"`
	HeadSHA    string  `json:"head_sha"`
	Status     *string `json:"status"`
	Conclusion *string `json:"conclusion"`
	DetailsURL *string `json:"details_url"`
	Output     *struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

// apply validates and folds a check-run body into s.
func (in apiV3CheckRunIn) apply(s *store.CommitStatus) string {
	if in.Name != nil {
		s.Context = appsTrim(*in.Name, 200)
	}
	if in.Status != nil {
		s.RunStatus = *in.Status
	}
	if in.Conclusion != nil {
		s.Conclusion = *in.Conclusion
		if s.Conclusion != "" {
			s.RunStatus = "completed"
		}
	}
	if s.RunStatus == "" {
		s.RunStatus = "queued"
	}
	switch s.RunStatus {
	case "queued", "in_progress", "completed", "waiting", "requested", "pending":
	default:
		return "status must be queued, in_progress or completed"
	}
	if in.DetailsURL != nil {
		s.TargetURL = appsSafeURL(*in.DetailsURL)
	}
	if in.Output != nil {
		d := in.Output.Title
		if d == "" {
			d = in.Output.Summary
		}
		s.Description = appsTrim(d, 1000)
	}
	if s.RunStatus != "completed" {
		s.State = "pending"
		return ""
	}
	switch s.Conclusion {
	case "success", "neutral":
		s.State = "success"
	case "skipped":
		s.State = "skipped"
	case "failure", "cancelled", "timed_out", "action_required", "stale", "startup_failure":
		s.State = "failure"
	case "":
		return "conclusion is required when status is completed"
	default:
		return "unknown conclusion " + strconv.Quote(s.Conclusion)
	}
	return ""
}

func (a *App) apiV3CheckRunCreate(w http.ResponseWriter, r *http.Request) {
	rp, who, ok := a.apiV3RepoFor(w, r, "checks")
	if !ok {
		return
	}
	var in apiV3CheckRunIn
	if !apiV3Decode(w, r, &in) {
		return
	}
	if in.Name == nil || strings.TrimSpace(*in.Name) == "" {
		apiV3Invalid(w, "name is required")
		return
	}
	ctx := r.Context()
	sha, ok := a.apiV3Commit(ctx, rp, in.HeadSHA)
	if !ok || !appsSHARe.MatchString(in.HeadSHA) {
		apiV3Invalid(w, "head_sha must be a commit in this repository")
		return
	}
	s := store.CommitStatus{RepoID: rp.ID, SHA: sha, Kind: "check_run", CreatorKind: who.Kind, CreatorName: who.Name}
	if msg := in.apply(&s); msg != "" {
		apiV3Invalid(w, msg)
		return
	}
	st, err := a.db.SetCommitStatus(ctx, s)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.apiV3CheckRunJSON(rp, st))
}

func (a *App) apiV3CheckRunUpdate(w http.ResponseWriter, r *http.Request) {
	rp, who, ok := a.apiV3RepoFor(w, r, "checks")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ctx := r.Context()
	var s store.CommitStatus
	if err == nil {
		s, err = a.db.CommitStatusByID(ctx, rp.ID, id)
	}
	if err != nil || s.Kind != "check_run" {
		writeAPIError(w, http.StatusNotFound, "Not Found")
		return
	}
	var in apiV3CheckRunIn
	if !apiV3Decode(w, r, &in) {
		return
	}
	in.Name = nil // renaming would orphan the old check
	s.CreatorKind, s.CreatorName = who.Kind, who.Name
	if msg := in.apply(&s); msg != "" {
		apiV3Invalid(w, msg)
		return
	}
	st, err := a.db.SetCommitStatus(ctx, s)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a.apiV3CheckRunJSON(rp, st))
}

func (a *App) apiV3CheckRuns(w http.ResponseWriter, r *http.Request) {
	rp, _, ok := a.apiV3RepoFor(w, r, "read")
	if !ok {
		return
	}
	ctx := r.Context()
	sha, ok := a.apiV3Commit(ctx, rp, r.PathValue("ref"))
	if !ok {
		writeAPIError(w, http.StatusNotFound, "No commit found for SHA: "+r.PathValue("ref"))
		return
	}
	sts, err := a.db.CommitStatuses(ctx, rp.ID, sha)
	if err != nil {
		a.apiFail(w, r, err)
		return
	}
	runs := []map[string]any{}
	for _, s := range sts {
		if s.Kind == "check_run" {
			runs = append(runs, a.apiV3CheckRunJSON(rp, s))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(runs), "check_runs": runs})
}
