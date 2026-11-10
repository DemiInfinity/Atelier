package web

// Import of issues, pull requests, labels and milestones: job progress per
// stage, the status UI's view of it, and resuming a partial import.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"atelier/internal/importer"
	"atelier/internal/store"
)

// importHTTP is the client for source APIs; nil means importer.DefaultHTTP.
var importHTTP *http.Client

// metaFromForm reads the "Also import" checkboxes.
func metaFromForm(r *http.Request) importer.MetaOptions {
	on := func(k string) bool { return r.FormValue(k) == "on" }
	return importer.MetaOptions{Issues: on("meta_issues"), Pulls: on("meta_pulls"), Labels: on("meta_labels"),
		Milestones: on("meta_milestones")}
}

func (a *App) saveImportMeta(ctx context.Context, jobID int64, req importer.Request, who Actor) error {
	stages := importer.Stages(req.Meta)
	if req.ResumeRepoID > 0 {
		setStage(stages, "code", "skipped", "Already here")
	}
	return a.db.SaveImportJobMeta(ctx, store.ImportJobMeta{
		JobID: jobID, RepoID: req.ResumeRepoID, PersonID: who.PersonID, SourceRepo: req.MetaRepo, APIBase: req.MetaAPI,
		Options: req.Meta.String(), Resume: req.ResumeRepoID > 0, Stages: encodeStages(stages),
	})
}

func encodeStages(st []importer.Stage) string {
	b, _ := json.Marshal(st)
	return string(b)
}

func decodeStages(s string, o importer.MetaOptions) []importer.Stage {
	var st []importer.Stage
	if s == "" || json.Unmarshal([]byte(s), &st) != nil || len(st) == 0 {
		return importer.Stages(o)
	}
	return st
}

func setStage(st []importer.Stage, key, state, note string) {
	for i := range st {
		if st[i].Key == key {
			st[i].State, st[i].Note = state, note
		}
	}
}

// progressMessage describes the running stage, e.g. "Importing issues… 12 of 40".
func progressMessage(st []importer.Stage) string {
	for _, s := range st {
		if s.State != "running" {
			continue
		}
		if s.Key == "code" {
			return "Cloning…"
		}
		if s.Note != "" {
			return s.Label + ": " + strings.ToLower(s.Note)
		}
		m := "Importing " + strings.ToLower(s.Label) + "…"
		if s.Total > 0 {
			m += fmt.Sprintf(" %d of %d", s.Done, s.Total)
		}
		return strings.TrimSpace(m)
	}
	return "Importing…"
}

func scrubToken(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "***")
	}
	return s
}

// importProgress saves stage progress. Writes are throttled a little: the
// stage list changes after every item.
func (a *App) importProgress(ctx context.Context, jobID, repoID int64, st []importer.Stage, msg string) {
	bg := context.WithoutCancel(ctx)
	_ = a.db.SetImportJobProgress(bg, jobID, repoID, encodeStages(st))
	_ = a.db.SetImportJobStatus(bg, jobID, "running", msg)
}

// failInterruptedStages marks the stages of jobs a restart cut short as
// failed, so their progress doesn't look like it's still moving.
func (a *App) failInterruptedStages(ctx context.Context) {
	jobs, err := a.db.ImportJobs(ctx, 200)
	if err != nil {
		return
	}
	var ids []int64
	for _, j := range jobs {
		if j.Status == "queued" || j.Status == "running" {
			ids = append(ids, j.ID)
		}
	}
	metas, err := a.db.ImportJobMetas(ctx, ids)
	if err != nil {
		return
	}
	for id, m := range metas {
		st := decodeStages(m.Stages, importer.ParseMetaOptions(m.Options))
		for i := range st {
			if st[i].State == "running" {
				st[i].State, st[i].Note = "failed", "Interrupted by a restart"
			}
		}
		_ = a.db.FinishImportJobMeta(ctx, id, encodeStages(st), m.Summary, nil)
	}
}

// ---------- status UI ----------

type stageView struct {
	importer.Stage
	Detail string // "8", "12 / 40", "✓", ""
}

func stageViews(st []importer.Stage) []stageView {
	out := make([]stageView, len(st))
	for i, s := range st {
		v := stageView{Stage: s}
		switch {
		case s.Key == "code" && s.State == "done":
			v.Detail = "✓"
		case s.State == "skipped" || s.State == "pending":
		case s.State == "done":
			v.Detail = strconv.Itoa(s.Total)
		case s.Total > 0:
			v.Detail = fmt.Sprintf("%d / %d", s.Done, s.Total)
		}
		out[i] = v
	}
	return out
}

// jobView is one row of "Recent imports".
type jobView struct {
	store.ImportJob
	Stages    []stageView
	Summary   string
	Resumable bool   // offer "Resume" / "Import again"
	ResumeBtn string // its label
	NeedToken bool   // no stored mirror token to resume with
}

// importJobViews adds metadata progress to jobs and drops jobs for
// repositories the viewer can't see.
func (a *App) importJobViews(ctx context.Context, who Actor, jobs []store.ImportJob) ([]jobView, error) {
	ids := make([]int64, len(jobs))
	for i, j := range jobs {
		ids[i] = j.ID
	}
	metas, err := a.db.ImportJobMetas(ctx, ids)
	if err != nil {
		return nil, err
	}
	mirrors, err := a.db.ImportMirrors(ctx)
	if err != nil {
		return nil, err
	}
	stored := map[int64]bool{}
	for _, m := range mirrors {
		stored[m.RepoID] = m.HasSecret()
	}
	latest := map[int64]bool{} // jobs are newest first: only the newest per repo offers resume
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		v := jobView{ImportJob: j}
		m, ok := metas[j.ID]
		if ok && m.RepoID > 0 {
			repo, err := a.db.ImportedRepo(ctx, m.RepoID)
			switch {
			case err == nil && !a.can(ctx, who, repo, PermRead):
				continue
			case err == nil && !latest[m.RepoID] && repo.FullName() == j.FullName():
				latest[m.RepoID] = true
				if (j.Status == "partial" || j.Status == "done" || j.Status == "failed") && a.can(ctx, who, repo, PermAdmin) &&
					m.SourceRepo != "" {
					v.Resumable = true
					v.ResumeBtn = "Import new issues and pull requests"
					if j.Status != "done" {
						v.ResumeBtn = "Resume"
					}
					v.NeedToken = !stored[m.RepoID]
				}
			}
		}
		if ok {
			v.Stages = stageViews(decodeStages(m.Stages, importer.ParseMetaOptions(m.Options)))
			v.Summary = m.Summary
		}
		out = append(out, v)
	}
	return out, nil
}

// ---------- resume ----------

func (a *App) importResume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	j, err := a.db.ImportJob(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		a.notFound(w, r)
		return
	} else if err != nil {
		a.fail(w, r, err)
		return
	}
	m, err := a.db.ImportJobMeta(ctx, id)
	if err != nil || m.RepoID == 0 || m.SourceRepo == "" {
		a.notFound(w, r)
		return
	}
	repo, err := a.db.ImportedRepo(ctx, m.RepoID)
	if err != nil || repo.FullName() != j.FullName() {
		a.notFound(w, r)
		return
	}
	who := a.actor(r)
	if !a.can(ctx, who, repo, PermAdmin) {
		a.forbidden(w, r)
		return
	}
	if j.Status == "queued" || j.Status == "running" {
		http.Redirect(w, r, "/settings/import", http.StatusSeeOther)
		return
	}
	token := strings.TrimSpace(r.FormValue("token"))
	if token == "" {
		// A mirror's stored token can be reused; one-off imports kept none.
		if mm, err := a.db.ImportMirror(ctx, repo.ID); err == nil {
			token = mm.Secret
		}
	}
	if token == "" {
		a.renderImport(w, r, importForm{Source: j.Source, Private: true},
			"Paste an access token for "+m.SourceRepo+" to resume: one-off imports don't keep theirs.", "")
		return
	}
	req := importer.Request{
		Source: j.Source, URL: j.SourceURL, Creds: importer.Creds{Token: token}, Owner: repo.Owner, Name: repo.Name,
		Description: j.Description, Private: j.Private, Meta: importer.ParseMetaOptions(m.Options),
		MetaAPI: m.APIBase, MetaRepo: m.SourceRepo, ResumeRepoID: repo.ID,
	}
	if !req.Meta.Any() {
		a.notFound(w, r)
		return
	}
	if m.ResumeAfter != nil && time.Now().Before(*m.ResumeAfter) && j.Status == "partial" {
		a.renderImport(w, r, importForm{Source: j.Source, Private: true},
			"The source's rate limit resets at "+m.ResumeAfter.UTC().Format("15:04")+" UTC. Resume after that.", "")
		return
	}
	if err := a.enqueueImports(ctx, []importer.Request{req}, who); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/settings/import?msg=resumed", http.StatusSeeOther)
}
