package web

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"

	"atelier/internal/store"
)

// The Projects panel in the issue and pull request sidebars. The sidebar
// include (partials_projects.html) only knows the repository id and number;
// projects.js loads this panel into it (?fragment=1), and without scripts
// its link opens the same panel as a page.
//
//	GET /projects/panel?kind=issue|pull&repo=<id>&number=<n>

type projPanelRow struct {
	P        store.Project
	URL      string
	ItemID   int64
	Status   projCell
	FieldID  int64
	Options  []store.ProjOption
	CanWrite bool
}

type projPanelData struct {
	Kind     string
	Repo     store.Repo
	Number   int64
	Title    string
	Return   string // the issue or pull request page
	In       []projPanelRow
	Addable  []store.Project
	Fragment bool
}

func (a *App) projPanel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who := a.actor(r)
	q := r.URL.Query()
	kind := q.Get("kind")
	repoID, _ := strconv.ParseInt(q.Get("repo"), 10, 64)
	num, _ := strconv.ParseInt(q.Get("number"), 10, 64)
	if kind != "issue" && kind != "pull" {
		a.notFound(w, r)
		return
	}
	rp, err := a.db.RepoByID(ctx, repoID)
	if err != nil || !a.can(ctx, who, rp, PermRead) {
		a.notFound(w, r)
		return
	}
	d := projPanelData{Kind: kind, Repo: rp, Number: num, Fragment: q.Get("fragment") == "1"}
	if kind == "issue" {
		is, err := a.db.IssueByNumber(ctx, rp.ID, num)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		d.Title = is.Title
		d.Return = "/" + rp.FullName() + "/issues/" + strconv.FormatInt(num, 10)
	} else {
		pl, err := a.db.PullByNumber(ctx, rp.ID, num)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		d.Title = pl.Title
		d.Return = "/" + rp.FullName() + "/pulls/" + strconv.FormatInt(num, 10)
	}
	refs, err := a.db.ItemsFor(ctx, kind, rp.ID, num)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	in := map[int64]bool{}
	for _, ref := range refs {
		p, err := a.db.ProjectByID(ctx, ref.ProjectID)
		if err != nil || !a.canProject(ctx, who, p, PermRead) {
			continue
		}
		in[p.ID] = true
		sf, err := a.db.StatusField(ctx, p.ID)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		it, err := a.db.ProjectItem(ctx, p.ID, ref.ItemID)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		d.In = append(d.In, projPanelRow{P: p, URL: projURL(p), ItemID: it.ID, Status: projCellFor(sf, it.Values[sf.ID]),
			FieldID: sf.ID, Options: sf.Options, CanWrite: a.canProject(ctx, who, p, PermWrite)})
	}
	all, err := a.db.Projects(ctx)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	for _, p := range all {
		if !p.Closed && !in[p.ID] && a.canProject(ctx, who, p, PermWrite) {
			d.Addable = append(d.Addable, p)
		}
	}
	if d.Fragment {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := a.pages["projects_panel"].ExecuteTemplate(w, "proj-panel-body", d); err != nil {
			logErr(r, err)
		}
		return
	}
	a.render(w, r, "projects_panel", page{Title: "Projects · " + d.Title, Nav: "projects", Data: d})
}

// ---------- automation ----------

// Automation, per project (Settings → Automation):
//   - issue.opened / pull.opened in a linked repository: add the item (Status Todo)
//   - issue.closed / pull.closed / pull.merged: Status → Done
//     (a merged pull request's "closes #N" issues too)
//   - issue.reopened / pull.reopened: Status → Todo

func init() {
	registerEventHandler(func(ctx context.Context, a *App, ev Event) { a.projAutomate(ctx, ev) })
}

const projAutoActor = "automation"

func (a *App) projAutomate(ctx context.Context, ev Event) {
	kind := ev.Subject()
	if kind != "issue" && kind != "pull" || ev.RepoID == 0 || ev.Number == 0 {
		return
	}
	switch ev.Kind {
	case EventIssueOpened, EventPullOpened:
		a.projAutoAdd(ctx, kind, ev.RepoID, ev.Number)
	case EventIssueClosed, EventPullClosed:
		a.projAutoStatus(ctx, kind, ev.RepoID, ev.Number, true)
	case EventPullMerged:
		a.projAutoStatus(ctx, kind, ev.RepoID, ev.Number, true)
		if pl, err := a.db.PullByNumber(ctx, ev.RepoID, ev.Number); err == nil {
			for _, n := range closingRefs(pl.Title, pl.Body) {
				if n != pl.Number {
					a.projAutoStatus(ctx, "issue", ev.RepoID, n, true)
				}
			}
		}
	case EventIssueReopened, EventPullReopened:
		a.projAutoStatus(ctx, kind, ev.RepoID, ev.Number, false)
	}
}

// projStatusOption finds the Done option (named "Done", else the last) or
// the Todo option (named "Todo", else the first).
func projStatusOption(sf store.ProjField, done bool) (store.ProjOption, bool) {
	if len(sf.Options) == 0 {
		return store.ProjOption{}, false
	}
	name, fallback := "todo", sf.Options[0]
	if done {
		name, fallback = "done", sf.Options[len(sf.Options)-1]
	}
	for _, o := range sf.Options {
		if strings.EqualFold(strings.Join(strings.Fields(o.Name), ""), name) {
			return o, true
		}
	}
	return fallback, true
}

func (a *App) projAutoAdd(ctx context.Context, kind string, repoID, number int64) {
	ids, err := a.db.ProjectsLinkedTo(ctx, repoID)
	if err != nil {
		log.Printf("projects: linked projects for repo %d: %v", repoID, err)
		return
	}
	for _, pid := range ids {
		p, err := a.db.ProjectByID(ctx, pid)
		if err != nil || !p.AutoAdd || p.Closed {
			continue
		}
		it, added, err := a.db.AddItem(ctx, p.ID, kind, repoID, number, ActorSystem, projAutoActor)
		if err != nil {
			log.Printf("projects: auto-add to project %d: %v", p.ID, err)
			continue
		}
		if !added {
			continue
		}
		if sf, err := a.db.StatusField(ctx, p.ID); err == nil {
			if o, ok := projStatusOption(sf, false); ok {
				if err := a.db.SetItemValue(ctx, p.ID, it.ID, sf.ID, strconv.FormatInt(o.ID, 10)); err != nil {
					log.Printf("projects: auto-add status: %v", err)
				}
			}
		}
	}
}

func (a *App) projAutoStatus(ctx context.Context, kind string, repoID, number int64, done bool) {
	refs, err := a.db.ItemsFor(ctx, kind, repoID, number)
	if err != nil {
		log.Printf("projects: items for %s %d#%d: %v", kind, repoID, number, err)
		return
	}
	for _, ref := range refs {
		p, err := a.db.ProjectByID(ctx, ref.ProjectID)
		if err != nil || (done && !p.AutoDone) || (!done && !p.AutoReopen) {
			continue
		}
		sf, err := a.db.StatusField(ctx, p.ID)
		if err != nil {
			continue
		}
		o, ok := projStatusOption(sf, done)
		if !ok {
			continue
		}
		if err := a.db.SetItemValue(ctx, p.ID, ref.ItemID, sf.ID, strconv.FormatInt(o.ID, 10)); err != nil {
			log.Printf("projects: automation status on project %d: %v", p.ID, err)
		}
	}
}

// projPanelAdd adds an issue or pull request to the project picked in the
// panel (project=<id>); the rest is projItemAdd.
func (a *App) projPanelAdd(w http.ResponseWriter, r *http.Request) {
	r.SetPathValue("id", r.FormValue("project"))
	a.projItemAdd(w, r)
}
