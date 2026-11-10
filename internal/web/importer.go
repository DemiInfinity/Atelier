package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"atelier/internal/importer"
	"atelier/internal/store"
)

// TODO(issues): import issues, pull requests, releases and wikis once the
// issues feature lands. The UI already says "coming soon".

func init() {
	registerFeature(feature{
		name: "import",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /settings/import", a.authed(a.importPage))
			m.HandleFunc("POST /settings/import/list", a.authed(a.importList))
			m.HandleFunc("GET /settings/import/pick/{sid}", a.authed(a.importPick))
			m.HandleFunc("POST /settings/import/pick/{sid}", a.authed(a.importPickSubmit))
			m.HandleFunc("POST /settings/import/url", a.authed(a.importURLSubmit))
			m.HandleFunc("GET /settings/import/jobs", a.authed(a.importJobsJSON))
			m.HandleFunc("POST /settings/import/mirrors/{id}/sync", a.authed(a.importMirrorSync))
			m.HandleFunc("POST /settings/import/mirrors/{id}/stop", a.authed(a.importMirrorStop))
		},
		start: func(ctx context.Context, a *App) {
			st := a.importState()
			st.mu.Lock()
			st.ctx = ctx
			st.mu.Unlock()
			// Queued jobs' tokens only lived in memory, so they can't resume.
			if err := a.db.FailUnfinishedImports(ctx, "Interrupted by a restart. Start the import again."); err != nil {
				log.Printf("import: %v", err)
			}
			a.mirrorLoop(ctx)
		},
	})
}

// githubAPI is GitHub's REST API; tests replace it.
var githubAPI = importer.GitHubAPI

const importSessionTTL = 30 * time.Minute

// importSession holds a listing between "list repositories" and "import
// selected", including the token, in memory only.
type importSession struct {
	Source  string // gitea or github
	Server  string // Gitea base URL; empty for GitHub
	Login   string
	token   string
	Repos   []importer.Remote
	created time.Time
}

type importState struct {
	mu       sync.Mutex
	ctx      context.Context
	sessions map[string]*importSession
	creds    map[int64]importer.Creds // per queued job, dropped once it runs
	queue    chan int64
	worker   sync.Once
	syncing  map[int64]bool
}

var importStates sync.Map // *App → *importState

func (a *App) importState() *importState {
	v, _ := importStates.LoadOrStore(a, &importState{
		ctx:      context.Background(),
		sessions: map[string]*importSession{},
		creds:    map[int64]importer.Creds{},
		queue:    make(chan int64, 1024),
		syncing:  map[int64]bool{},
	})
	return v.(*importState)
}

func (st *importState) session(id string) *importSession {
	st.mu.Lock()
	defer st.mu.Unlock()
	for k, s := range st.sessions {
		if time.Since(s.created) > importSessionTTL {
			delete(st.sessions, k)
		}
	}
	return st.sessions[id]
}

func (a *App) mirrorInterval() time.Duration {
	if a.cfg.MirrorIntervalMins > 0 {
		return time.Duration(a.cfg.MirrorIntervalMins) * time.Minute
	}
	return 30 * time.Minute
}

// ---------- background work ----------

// enqueue records jobs and hands them to the single import worker.
func (a *App) enqueueImports(ctx context.Context, reqs []importer.Request, who Actor) error {
	st := a.importState()
	st.worker.Do(func() { go a.importWorker() })
	for _, req := range reqs {
		id, err := a.db.CreateImportJob(ctx, store.ImportJob{
			Source: req.Source, SourceURL: req.URL, Owner: req.Owner, Name: req.Name, Description: req.Description,
			Private: req.Private, Mirror: req.Mirror, ActorKind: who.Kind, ActorName: who.Name,
		})
		if err != nil {
			return err
		}
		st.mu.Lock()
		st.creds[id] = req.Creds
		st.mu.Unlock()
		select {
		case st.queue <- id:
		default:
			_ = a.db.SetImportJobStatus(ctx, id, "failed", "Too many imports queued; try again shortly.")
		}
	}
	return nil
}

func (a *App) importWorker() {
	st := a.importState()
	for id := range st.queue {
		st.mu.Lock()
		ctx := st.ctx
		creds := st.creds[id]
		delete(st.creds, id)
		st.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		a.runImport(ctx, id, creds)
	}
}

func (a *App) runImport(ctx context.Context, id int64, creds importer.Creds) {
	j, err := a.db.ImportJob(ctx, id)
	if err != nil {
		log.Printf("import job %d: %v", id, err)
		return
	}
	_ = a.db.SetImportJobStatus(ctx, id, "running", "Cloning…")
	start := time.Now()
	_, err = importer.Import(ctx, a.db, a.git, importer.Request{
		Source: j.Source, URL: j.SourceURL, Creds: creds, Owner: j.Owner, Name: j.Name,
		Description: j.Description, Private: j.Private, Mirror: j.Mirror, MirrorInterval: a.mirrorInterval(),
	})
	bg := context.WithoutCancel(ctx)
	if err != nil {
		log.Printf("import %s from %s: %v", j.FullName(), j.SourceURL, err)
		_ = a.db.SetImportJobStatus(bg, id, "failed", err.Error())
		return
	}
	msg := fmt.Sprintf("Imported in %s.", shortDuration(time.Since(start)))
	if j.Mirror {
		msg += " Mirroring every " + every(a.mirrorInterval()) + "."
	}
	_ = a.db.SetImportJobStatus(bg, id, "done", msg)
}

func (a *App) mirrorLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		ms, err := a.db.ImportMirrors(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("mirrors: %v", err)
		}
		for _, m := range ms {
			if ctx.Err() != nil {
				return
			}
			if importer.Due(m, time.Now()) {
				a.syncMirror(ctx, m)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// syncMirror fetches one mirror unless it's already being fetched.
func (a *App) syncMirror(ctx context.Context, m store.ImportMirror) {
	st := a.importState()
	st.mu.Lock()
	if st.syncing[m.RepoID] {
		st.mu.Unlock()
		return
	}
	st.syncing[m.RepoID] = true
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		delete(st.syncing, m.RepoID)
		st.mu.Unlock()
	}()
	if err := importer.SyncMirror(ctx, a.db, a.git, m); err != nil {
		log.Printf("mirror %s: %v", m.Repo, err)
	}
}

// ---------- pages ----------

// mirrorView is a mirror for the page; Secret is never rendered, only HasSecret.
type mirrorView struct {
	store.ImportMirror
	Every string
}

// every formats an interval as "30 minutes", "2 hours".
func every(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return plural(int(d/time.Hour), "hour")
	}
	return plural(int(d.Round(time.Minute)/time.Minute), "minute")
}

type importForm struct {
	Source, Server, URL, Username, Owner, Name, Description string
	Private, Mirror                                         bool
}

func (a *App) importPage(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("source")
	if src != "github" && src != "git" {
		src = "gitea"
	}
	a.renderImport(w, r, importForm{Source: src, Private: true}, "", r.URL.Query().Get("msg"))
}

func (a *App) renderImport(w http.ResponseWriter, r *http.Request, f importForm, errMsg, msg string) {
	ctx := r.Context()
	owners, err := a.db.Owners(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	jobs, err := a.db.ImportJobs(ctx, 30)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	mirrors, err := a.db.ImportMirrors(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	notice := map[string]string{
		"queued":  "Import started. Progress shows below.",
		"stopped": "Mirroring stopped and its stored credentials deleted. The repository stays.",
		"syncing": "Fetching from the source now.",
	}[msg]
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	views := make([]mirrorView, len(mirrors))
	for i, m := range mirrors {
		views[i] = mirrorView{ImportMirror: m, Every: every(m.Interval)}
	}
	a.render(w, r, "importer_settings", page{Title: "Import", Error: errMsg, Data: map[string]any{
		"Form": f, "Owners": owners, "Jobs": jobs, "Mirrors": views, "Notice": notice,
		"Interval": every(a.mirrorInterval()),
	}})
}

func formOwner(r *http.Request, owners []store.Owner) string {
	o := r.FormValue("owner")
	if o == "" && len(owners) > 0 {
		o = owners[0].Name
	}
	return o
}

func (a *App) importList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := importForm{Source: r.FormValue("source"), Server: strings.TrimSpace(r.FormValue("server")), Private: true}
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		a.renderImport(w, r, f, "Paste an access token so Atelier can list your repositories.", "")
		return
	}
	lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var (
		repos  []importer.Remote
		login  string
		err    error
		server string
	)
	switch f.Source {
	case "gitea":
		server, err = importer.NormalizeServer(f.Server)
		if err == nil {
			repos, login, err = importer.ListGitea(lctx, nil, server, token)
		}
	case "github":
		repos, login, err = importer.ListGitHub(lctx, nil, githubAPI, token)
	default:
		a.renderImport(w, r, importForm{Source: "gitea", Private: true}, "Pick a source.", "")
		return
	}
	if err != nil {
		a.renderImport(w, r, f, "Couldn't list repositories: "+err.Error(), "")
		return
	}
	sid := store.RandomToken(16)
	st := a.importState()
	st.mu.Lock()
	st.sessions[sid] = &importSession{Source: f.Source, Server: server, Login: login, token: token, Repos: repos, created: time.Now()}
	st.mu.Unlock()
	http.Redirect(w, r, "/settings/import/pick/"+sid, http.StatusSeeOther)
}

type pickRow struct {
	importer.Remote
	Index   int
	Checked bool
	Target  string
}

func (a *App) importPick(w http.ResponseWriter, r *http.Request) {
	s := a.importState().session(r.PathValue("sid"))
	if s == nil {
		http.Redirect(w, r, "/settings/import", http.StatusSeeOther)
		return
	}
	rows := make([]pickRow, len(s.Repos))
	for i, rm := range s.Repos {
		rows[i] = pickRow{Remote: rm, Index: i, Target: rm.Name}
	}
	a.renderPick(w, r, s, rows, "", false, "")
}

func (a *App) renderPick(w http.ResponseWriter, r *http.Request, s *importSession, rows []pickRow, owner string, mirror bool, errMsg string) {
	owners, err := a.db.Owners(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if owner == "" && len(owners) > 0 {
		owner = owners[0].Name
	}
	label := "GitHub"
	if s.Source == "gitea" {
		label = s.Server
	}
	if errMsg != "" {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	a.render(w, r, "importer_pick", page{Title: "Choose repositories to import", Error: errMsg, Data: map[string]any{
		"SID": r.PathValue("sid"), "Source": s.Source, "SourceLabel": label, "Login": s.Login, "Rows": rows,
		"Owners": owners, "Owner": owner, "Mirror": mirror, "Interval": every(a.mirrorInterval()),
	}})
}

func (a *App) cloneURL(s *importSession, rm importer.Remote) string {
	// Build the URL from the address the owner typed rather than trusting the
	// API's clone_url, so the token only ever goes to that host.
	if s.Source == "gitea" {
		return s.Server + "/" + rm.FullName + ".git"
	}
	if githubAPI == importer.GitHubAPI {
		return "https://github.com/" + rm.FullName + ".git"
	}
	return rm.CloneURL // tests
}

func (a *App) importPickSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := a.importState()
	sid := r.PathValue("sid")
	s := st.session(sid)
	if s == nil {
		http.Redirect(w, r, "/settings/import", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	owner := r.FormValue("owner")
	mirror := r.FormValue("mirror") == "on"
	selected := map[int]bool{}
	for _, v := range r.Form["repo"] {
		if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(s.Repos) {
			selected[i] = true
		}
	}
	rows := make([]pickRow, len(s.Repos))
	var reqs []importer.Request
	var problems []string
	names := map[string]bool{}
	user := s.Login
	if s.Source == "github" {
		user = "x-access-token"
	}
	for i, rm := range s.Repos {
		target := strings.TrimSpace(r.FormValue("name_" + strconv.Itoa(i)))
		if target == "" {
			target = rm.Name
		}
		rows[i] = pickRow{Remote: rm, Index: i, Checked: selected[i], Target: target}
		if !selected[i] {
			continue
		}
		req := importer.Request{
			Source: s.Source, URL: a.cloneURL(s, rm), Creds: importer.Creds{Username: user, Token: s.token},
			Owner: owner, Name: target, Description: rm.Description, Private: rm.Private, Mirror: mirror,
			MirrorInterval: a.mirrorInterval(),
		}
		if _, err := importer.Check(ctx, a.db, req); err != nil {
			problems = append(problems, rm.FullName+": "+err.Error())
			continue
		}
		if names[strings.ToLower(target)] {
			problems = append(problems, rm.FullName+": another selected repository is also called "+target)
			continue
		}
		names[strings.ToLower(target)] = true
		reqs = append(reqs, req)
	}
	switch {
	case len(selected) == 0:
		a.renderPick(w, r, s, rows, owner, mirror, "Select at least one repository.")
		return
	case len(problems) > 0:
		a.renderPick(w, r, s, rows, owner, mirror, strings.Join(problems, " · "))
		return
	}
	if err := a.enqueueImports(ctx, reqs, a.actor(r)); err != nil {
		a.fail(w, r, err)
		return
	}
	st.mu.Lock()
	delete(st.sessions, sid) // the token now lives only with the queued jobs
	st.mu.Unlock()
	http.Redirect(w, r, "/settings/import?msg=queued", http.StatusSeeOther)
}

func (a *App) importURLSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := importForm{
		Source: "git", URL: strings.TrimSpace(r.FormValue("url")), Username: strings.TrimSpace(r.FormValue("username")),
		Owner: r.FormValue("owner"), Name: strings.TrimSpace(r.FormValue("name")),
		Description: strings.TrimSpace(r.FormValue("description")), Private: r.FormValue("visibility") != "public",
		Mirror: r.FormValue("mirror") == "on",
	}
	clean, creds, err := importer.CleanURL(f.URL)
	if err != nil {
		a.renderImport(w, r, f, err.Error(), "")
		return
	}
	f.URL = clean // never echo credentials back into the form
	if tok := strings.TrimSpace(r.FormValue("token")); tok != "" {
		creds.Token = tok
	}
	if f.Username != "" {
		creds.Username = f.Username
	}
	if f.Name == "" {
		f.Name = importer.NameFromURL(clean)
	}
	req := importer.Request{
		Source: "git", URL: clean, Creds: creds, Owner: f.Owner, Name: f.Name, Description: f.Description,
		Private: f.Private, Mirror: f.Mirror, MirrorInterval: a.mirrorInterval(),
	}
	if _, err := importer.Check(ctx, a.db, req); err != nil {
		a.renderImport(w, r, f, err.Error(), "")
		return
	}
	if err := a.enqueueImports(ctx, []importer.Request{req}, a.actor(r)); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/import?msg=queued", http.StatusSeeOther)
}

type jobJSON struct {
	ID      int64  `json:"id"`
	Repo    string `json:"repo"`
	Status  string `json:"status"`
	Message string `json:"message"`
	URL     string `json:"url,omitempty"`
}

func (a *App) importJobsJSON(w http.ResponseWriter, r *http.Request) {
	jobs, err := a.db.ImportJobs(r.Context(), 30)
	if err != nil {
		logErr(r, err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	out := make([]jobJSON, 0, len(jobs))
	for _, j := range jobs {
		jj := jobJSON{ID: j.ID, Repo: j.FullName(), Status: j.Status, Message: j.Message}
		if j.Status == "done" {
			jj.URL = "/" + j.FullName()
		}
		out = append(out, jj)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (a *App) mirrorFromPath(w http.ResponseWriter, r *http.Request) (store.ImportMirror, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return store.ImportMirror{}, false
	}
	m, err := a.db.ImportMirror(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		a.notFound(w, r)
		return m, false
	}
	if err != nil {
		a.fail(w, r, err)
		return m, false
	}
	return m, true
}

func (a *App) importMirrorSync(w http.ResponseWriter, r *http.Request) {
	m, ok := a.mirrorFromPath(w, r)
	if !ok {
		return
	}
	st := a.importState()
	st.mu.Lock()
	ctx := st.ctx
	st.mu.Unlock()
	go a.syncMirror(ctx, m)
	http.Redirect(w, r, "/settings/import?msg=syncing", http.StatusSeeOther)
}

func (a *App) importMirrorStop(w http.ResponseWriter, r *http.Request) {
	m, ok := a.mirrorFromPath(w, r)
	if !ok {
		return
	}
	if err := a.db.DeleteImportMirror(r.Context(), m.RepoID); err != nil {
		a.fail(w, r, err)
		return
	}
	// Drop the remote so the repository no longer points at the source.
	_ = importer.Unmirror(r.Context(), a.git.Git, a.git.Path(m.Owner, m.Name))
	http.Redirect(w, r, "/settings/import?msg=stopped", http.StatusSeeOther)
}
