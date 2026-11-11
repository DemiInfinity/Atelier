package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// Pull requests: list, compare/new, the PR page (Overview + Files changed)
// with its merge status bar, merging (merge commit or squash) and notes to
// self. Branch rules live in pulls_rules.go.

func init() {
	registerFeature(feature{
		name: "pulls",
		routes: func(a *App, m *http.ServeMux) {
			m.HandleFunc("GET /{owner}/{repo}/pulls", a.authed(a.pullsList))
			m.HandleFunc("POST /{owner}/{repo}/pulls", a.authed(a.pullCreate))
			m.HandleFunc("GET /{owner}/{repo}/pulls/new", a.authed(a.pullCompare))
			m.HandleFunc("GET /{owner}/{repo}/compare", a.authed(a.pullCompare))
			m.HandleFunc("GET /{owner}/{repo}/pulls/{number}", a.authed(a.pullOverview))
			m.HandleFunc("GET /{owner}/{repo}/pulls/{number}/files", a.authed(a.pullFiles))
			m.HandleFunc("POST /{owner}/{repo}/pulls/{number}/merge", a.authed(a.pullMerge))
			m.HandleFunc("POST /{owner}/{repo}/pulls/{number}/state", a.authed(a.pullState))
			m.HandleFunc("POST /{owner}/{repo}/pulls/{number}/edit", a.authed(a.pullEdit))
			m.HandleFunc("POST /{owner}/{repo}/pulls/{number}/comments", a.authed(a.pullComment))
			m.HandleFunc("POST /{owner}/{repo}/pulls/{number}/notes", a.authed(a.pullAddNote))
			m.HandleFunc("POST /{owner}/{repo}/pulls/{number}/notes/{id}/resolve", a.authed(a.pullResolveNote))
			m.HandleFunc("POST /{owner}/{repo}/pulls/{number}/delete-branch", a.authed(a.pullDeleteBranch))
			pullsRulesRoutes(a, m)
		},
		start: func(ctx context.Context, a *App) { a.ensurePullHooks(ctx) },
	})
}

// ensurePullHooks installs or refreshes the pre-receive hook on every
// repository, so branch rules are enforced on pushes to repos created before
// this feature (and so the hook points at the current binary).
func (a *App) ensurePullHooks(ctx context.Context) {
	repos, err := a.db.Repos(ctx)
	if err != nil {
		log.Printf("pulls: listing repositories for hooks: %v", err)
		return
	}
	for _, rp := range repos {
		if ctx.Err() != nil {
			return
		}
		if _, err := a.git.Open(rp.Owner, rp.Name); err != nil {
			continue
		}
		if err := a.git.InstallPreReceiveHook(rp.Owner, rp.Name); err != nil {
			log.Printf("pulls: installing hook for %s: %v", rp.FullName(), err)
		}
	}
}

const (
	pullTitleMax = 256
	pullBodyMax  = 65536
	// SettingCommitterEmail is the email on merge commits Atelier makes.
	settingPullsCommitterEmail = "pulls.committer_email"
)

// ---------- list ----------

func (a *App) pullsList(w http.ResponseWriter, r *http.Request) {
	v, _, ok := a.loadRepo(w, r, "pulls")
	if !ok {
		return
	}
	ctx := r.Context()
	state := r.URL.Query().Get("state")
	if state != "merged" && state != "closed" {
		state = "open"
	}
	pulls, err := a.db.Pulls(ctx, v.Repo.ID, state)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	counts, err := a.db.PullCounts(ctx, v.Repo.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "pulls_list", page{Title: "Pull requests · " + v.Repo.FullName(), Nav: "repos", Data: map[string]any{
		"V": v, "Pulls": pulls, "State": state, "Counts": counts,
	}})
}

// ---------- compare / new ----------

type pullCompareData struct {
	V           *repoView
	Base, Head  string
	Cmp         *gitrepo.Comparison
	Merge       *gitrepo.MergeCheck
	Files       []pullFileView
	Existing    *store.Pull
	Title, Body string
	Problem     string   // why there's nothing to open
	Recent      []string // branches to suggest when no head is picked
	CommitList  pullCommitList
}

func (a *App) pullCompare(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "pulls")
	if !ok {
		return
	}
	q := r.URL.Query()
	a.renderCompare(w, r, v, g, q.Get("base"), q.Get("head"), "", "", "")
}

func (a *App) renderCompare(w http.ResponseWriter, r *http.Request, v *repoView, g *gitrepo.Repo, base, head, title, body, errMsg string) {
	ctx := r.Context()
	d := &pullCompareData{V: v, Base: base, Head: head, Title: title, Body: body}
	if d.Base == "" {
		d.Base = v.Default
	}
	render := func() {
		if errMsg != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
		a.render(w, r, "pulls_compare", page{Title: "Compare changes · " + v.Repo.FullName(), Nav: "repos", Error: errMsg, Data: d})
	}
	if v.Empty {
		d.Problem = "This repository is empty. Push a branch first."
		render()
		return
	}
	if d.Head == "" {
		for _, b := range v.Branches {
			if b != d.Base && len(d.Recent) < 5 {
				d.Recent = append(d.Recent, b)
			}
		}
		render()
		return
	}
	baseSHA, err1 := g.BranchSHA(ctx, d.Base)
	headSHA, err2 := g.BranchSHA(ctx, d.Head)
	switch {
	case err1 != nil:
		d.Problem = "The base branch " + d.Base + " doesn't exist."
	case err2 != nil:
		d.Problem = "The head branch " + d.Head + " doesn't exist."
	case d.Base == d.Head:
		d.Problem = "Choose two different branches to compare."
	}
	if d.Problem != "" {
		render()
		return
	}
	if p, err := a.db.OpenPullFor(ctx, v.Repo.ID, d.Base, d.Head); err == nil {
		d.Existing = &p
	}
	cmp, err := g.Compare(ctx, baseSHA, headSHA)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d.Cmp = cmp
	d.CommitList = pullCommitList{Base: v.Base, Commits: cmp.Commits}
	switch {
	case cmp.Unrelated:
		d.Problem = d.Head + " and " + d.Base + " have entirely different histories."
	case cmp.Ahead == 0:
		d.Problem = d.Base + " is up to date with all commits from " + d.Head + "."
	}
	if d.Problem == "" {
		if mc, err := g.CheckMerge(ctx, baseSHA, headSHA); err == nil {
			d.Merge = mc
		}
		d.Files = pullFileViews(cmp.Diff, nil, headSHA, nil, "", v.Base)
		if d.Title == "" && errMsg == "" {
			d.Title, d.Body = defaultPullText(cmp.Commits, d.Head)
		}
	}
	render()
}

// defaultPullText suggests a title and body: the commit's own message for a
// single commit, otherwise the branch name made readable.
func defaultPullText(commits []gitrepo.Commit, head string) (string, string) {
	if len(commits) == 1 {
		return commits[0].Subject, commits[0].Body
	}
	name := head
	if i := strings.LastIndex(name, "/"); i >= 0 && i < len(name)-1 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ").Replace(name))
	if name == "" {
		return head, ""
	}
	rs := []rune(name)
	rs[0] = unicode.ToUpper(rs[0])
	return string(rs), ""
}

func (a *App) pullCreate(w http.ResponseWriter, r *http.Request) {
	v, g, ok := a.loadRepo(w, r, "pulls")
	if !ok {
		return
	}
	ctx := r.Context()
	base, head := r.FormValue("base"), r.FormValue("head")
	title := strings.TrimSpace(r.FormValue("title"))
	body := normalizePullText(r.FormValue("body"))
	fail := func(msg string) { a.renderCompare(w, r, v, g, base, head, title, body, msg) }

	if v.Empty {
		fail("This repository is empty.")
		return
	}
	baseSHA, err1 := g.BranchSHA(ctx, base)
	headSHA, err2 := g.BranchSHA(ctx, head)
	switch {
	case err1 != nil || err2 != nil:
		fail("Pick an existing base and head branch.")
		return
	case base == head:
		fail("Choose two different branches.")
		return
	case title == "":
		fail("Give the pull request a title.")
		return
	case len(title) > pullTitleMax:
		fail(fmt.Sprintf("Keep the title under %d characters.", pullTitleMax))
		return
	case len(body) > pullBodyMax:
		fail("The description is too long.")
		return
	}
	if p, err := a.db.OpenPullFor(ctx, v.Repo.ID, base, head); err == nil {
		http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d", v.Base, p.Number), http.StatusSeeOther)
		return
	}
	ahead, _, err := g.AheadBehind(ctx, baseSHA, headSHA)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if ahead == 0 {
		fail(base + " already has every commit from " + head + ". There's nothing to merge.")
		return
	}
	who := a.actor(r)
	p, err := a.db.CreatePull(ctx, store.NewPull{RepoID: v.Repo.ID, Title: title, Body: body, Head: head, Base: base,
		HeadSHA: headSHA, AuthorKind: who.Kind, AuthorName: who.Name})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.emit(ctx, pullEvent(EventPullOpened, v, p, who, body))
	http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d", v.Base, p.Number), http.StatusSeeOther)
}

func normalizePullText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

// ---------- the pull request page ----------

type pullStatusPill struct {
	Label string
	Kind  string // ok, warn, bad, info
	Title string
}

type pullStep struct {
	Text string
	Done bool
}

type pullMergeMethod struct {
	Value, Label, Hint string
	Allowed            bool
}

// pullMergeStatus is everything the merge status bar shows.
type pullMergeStatus struct {
	Pills     []pullStatusPill
	Steps     []pullStep
	Methods   []pullMergeMethod
	Primary   pullMergeMethod
	CanMerge  bool
	Reason    string // why merging is blocked
	Conflicts []string
}

type pullTimelineItem struct {
	When    time.Time
	Kind    string // commits, comment, event
	Commits pullCommitList
	Comment *store.PullComment
	HTML    template.HTML
	Event   *store.PullEvent
	From    string // renamed: old title
	To      string // renamed: new title
	SHA     string // merged: commit
	Method  string // merged: merge or squash
	URL     string // tether_mirrored: the GitHub pull request
}

type pullNoteView struct {
	store.PullNote
	Current bool   // pinned to the head commit being shown
	Anchor  string // id of the diff line, when current
	HTML    template.HTML
	URL     string // the pull request
	Back    string // tab to return to after resolving
}

// pullNoteForm is the add-a-note form for one line (Line 0 for the JS template).
type pullNoteForm struct {
	URL, Commit, Path, Side string
	Line                    int
	ID, Cancel              string
}

type pullCommitList struct {
	Base    string
	Commits []gitrepo.Commit
}

type pullLineView struct {
	gitrepo.DiffLine
	Side  string // side a note on this line uses
	Num   int    // line number on that side
	ID    string
	Notes []*pullNoteView
	Form  *pullNoteForm // the no-JS note form, rendered after this line
}

type pullFileView struct {
	*gitrepo.DiffFile
	Index     int
	Anchor    string
	Notes     int
	Open      int // unresolved notes on this file
	LineViews []pullLineView
	URL       string // the pull request; empty turns notes off (compare page)
	Commit    string // head commit shown
	RepoBase  string
}

type pullTreeRow struct {
	Name  string
	Depth int
	Dir   bool
	File  *pullFileView
}

type pullPageData struct {
	V          *repoView
	P          store.Pull
	URL        string
	Tab        string
	BodyHTML   template.HTML
	HeadSHA    string
	BaseSHA    string
	HeadExists bool
	BaseExists bool
	Cmp        *gitrepo.Comparison
	Status     pullMergeStatus
	Protection store.Protection
	Checks     actChecks // Actions jobs on the head commit (actions_checks.go)
	Timeline   []pullTimelineItem
	Comments   int
	Notes      []*pullNoteView
	Unresolved int
	Files      []pullFileView
	Tree       []pullTreeRow
	NoteForm   bool
	CanDelete  bool // head branch can be deleted now
	Commits    int
	CommitList pullCommitList
	Editing    bool
	NoteTpl    pullNoteForm
}

func (a *App) loadPull(w http.ResponseWriter, r *http.Request) (*repoView, *gitrepo.Repo, store.Pull, bool) {
	v, g, ok := a.loadRepo(w, r, "pulls")
	if !ok {
		return nil, nil, store.Pull{}, false
	}
	n, err := strconv.ParseInt(r.PathValue("number"), 10, 64)
	if err != nil || n < 1 {
		a.notFound(w, r)
		return nil, nil, store.Pull{}, false
	}
	p, err := a.db.PullByNumber(r.Context(), v.Repo.ID, n)
	if err != nil {
		a.fail(w, r, err)
		return nil, nil, store.Pull{}, false
	}
	return v, g, p, true
}

func (a *App) pullOverview(w http.ResponseWriter, r *http.Request) {
	v, g, p, ok := a.loadPull(w, r)
	if !ok {
		return
	}
	a.renderPull(w, r, v, g, p, "overview", "", http.StatusOK)
}

func (a *App) pullFiles(w http.ResponseWriter, r *http.Request) {
	v, g, p, ok := a.loadPull(w, r)
	if !ok {
		return
	}
	a.renderPull(w, r, v, g, p, "files", "", http.StatusOK)
}

func (a *App) renderPull(w http.ResponseWriter, r *http.Request, v *repoView, g *gitrepo.Repo, p store.Pull, tab, errMsg string, code int) {
	d, err := a.buildPull(r, v, g, p, tab)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
	a.render(w, r, "pulls_view", page{
		Title: p.Title + " · Pull request #" + strconv.FormatInt(p.Number, 10) + " · " + v.Repo.FullName(),
		Nav:   "repos", Error: errMsg, Data: d,
	})
}

func (a *App) buildPull(r *http.Request, v *repoView, g *gitrepo.Repo, p store.Pull, tab string) (*pullPageData, error) {
	ctx := r.Context()
	d := &pullPageData{V: v, P: p, Tab: tab, URL: fmt.Sprintf("%s/pulls/%d", v.Base, p.Number),
		BodyHTML: renderMarkdown([]byte(p.Body)), Editing: r.URL.Query().Get("edit") == "1"}

	// Which commits to compare.
	curHead, errH := g.BranchSHA(ctx, p.Head)
	curBase, errB := g.BranchSHA(ctx, p.Base)
	d.HeadExists, d.BaseExists = errH == nil, errB == nil
	if p.IsMerged() {
		d.HeadSHA, d.BaseSHA = p.HeadSHA, p.BaseSHA
	} else {
		d.HeadSHA, d.BaseSHA = curHead, curBase
		if !d.HeadExists {
			d.HeadSHA = p.HeadSHA
		}
		if p.IsOpen() && d.HeadExists && curHead != p.HeadSHA {
			a.notePullPush(ctx, g, &p, curHead)
			d.P = p
		}
	}
	var err error
	if d.HeadSHA != "" && d.BaseSHA != "" {
		if d.Cmp, err = g.Compare(ctx, d.BaseSHA, d.HeadSHA); err != nil {
			return nil, err
		}
		d.Commits = len(d.Cmp.Commits)
		d.CommitList = pullCommitList{Base: v.Base, Commits: d.Cmp.Commits}
	}
	if d.Protection, err = a.db.ProtectionFor(ctx, v.Repo.ID, p.Base); err != nil {
		return nil, err
	}
	d.Checks = a.pullChecks(ctx, v.Repo, d.HeadSHA, d.Protection)

	// Notes.
	notes, err := a.db.PullNotes(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for i := range notes {
		n := &pullNoteView{PullNote: notes[i], Current: notes[i].CommitSHA == d.HeadSHA, HTML: renderMarkdown([]byte(notes[i].Body)),
			URL: d.URL, Back: tab}
		d.Notes = append(d.Notes, n)
		if !n.Resolved {
			d.Unresolved++
		}
	}

	// Timeline: commits, comments and events in time order.
	comments, err := a.db.PullComments(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	events, err := a.db.PullEvents(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	d.Comments = len(comments)
	d.Timeline = pullTimeline(v.Base, d.Cmp, comments, events)

	if p.IsOpen() {
		d.Status = pullStatus(ctx, g, d)
		// Home: GitHub — the merge happens on GitHub (tether_sync.go takes the POST).
		if l, err := a.db.TetherLink(ctx, v.Repo.ID); err == nil && l.Home == "github" {
			for i := range d.Status.Methods {
				d.Status.Methods[i].Label += " on GitHub"
			}
			d.Status.Primary = d.Status.Methods[0]
			for i := range d.Status.Steps {
				d.Status.Steps[i].Text += " on GitHub"
			}
		}
	}
	if !p.IsOpen() && d.HeadExists && p.Head != v.Default && !a.headProtected(ctx, v.Repo.ID, p.Head) &&
		(p.IsClosed() || curHead == p.HeadSHA) {
		d.CanDelete = true
	}

	if tab == "files" && d.Cmp != nil && d.Cmp.Diff != nil {
		q := r.URL.Query()
		form := &pullNoteTarget{Path: q.Get("path"), Side: q.Get("side")}
		form.Line, _ = strconv.Atoi(q.Get("line"))
		if form.Path == "" || form.Line < 1 {
			form = nil
		}
		d.Files = pullFileViews(d.Cmp.Diff, d.Notes, d.HeadSHA, form, d.URL, v.Base)
		d.NoteForm = form != nil
		d.NoteTpl = pullNoteForm{URL: d.URL, Commit: d.HeadSHA, ID: "pr-note-js", Cancel: d.URL + "/files"}
		d.Tree = pullTree(d.Files)
	}
	return d, nil
}

// notePullPush records that the head branch moved; a non-fast-forward move
// shows up in the timeline as a force push.
func (a *App) notePullPush(ctx context.Context, g *gitrepo.Repo, p *store.Pull, head string) {
	if p.HeadSHA != "" {
		if ff, err := g.IsAncestor(ctx, p.HeadSHA, head); err == nil && !ff {
			_ = a.db.AddPullEvent(ctx, p.ID, "force_pushed", "owner", "", p.HeadSHA+" "+head)
		}
	}
	if err := a.db.SetPullHeadSHA(ctx, p.ID, head); err == nil {
		p.HeadSHA = head
	}
}

func (a *App) headProtected(ctx context.Context, repoID int64, branch string) bool {
	pr, err := a.db.ProtectionFor(ctx, repoID, branch)
	return err != nil || pr.Protected()
}

func pullTimeline(base string, cmp *gitrepo.Comparison, comments []store.PullComment, events []store.PullEvent) []pullTimelineItem {
	var items []pullTimelineItem
	if cmp != nil {
		for _, c := range cmp.Commits {
			items = append(items, pullTimelineItem{When: c.When, Kind: "commits", Commits: pullCommitList{Base: base, Commits: []gitrepo.Commit{c}}})
		}
	}
	for i := range comments {
		c := &comments[i]
		items = append(items, pullTimelineItem{When: c.CreatedAt, Kind: "comment", Comment: c, HTML: renderMarkdown([]byte(c.Body))})
	}
	for i := range events {
		e := &events[i]
		it := pullTimelineItem{When: e.CreatedAt, Kind: "event", Event: e}
		switch e.Kind {
		case "renamed":
			it.From, it.To, _ = strings.Cut(e.Data, "\n")
		case "merged", "force_pushed":
			it.Method, it.SHA, _ = strings.Cut(e.Data, " ")
		case "tether_mirrored": // "<GitHub number> <url>"
			it.SHA, it.URL, _ = strings.Cut(e.Data, " ")
			if !strings.HasPrefix(it.URL, "https://") && !strings.HasPrefix(it.URL, "http://") {
				it.URL = ""
			}
		}
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].When.Before(items[j].When) })
	// Group runs of commits into one entry.
	var out []pullTimelineItem
	for _, it := range items {
		if it.Kind == "commits" && len(out) > 0 && out[len(out)-1].Kind == "commits" {
			last := &out[len(out)-1].Commits
			last.Commits = append(last.Commits, it.Commits.Commits...)
			continue
		}
		out = append(out, it)
	}
	return out
}

// pullStatus works out whether an open pull request can be merged, and why not.
func pullStatus(ctx context.Context, g *gitrepo.Repo, d *pullPageData) pullMergeStatus {
	var s pullMergeStatus
	pr := d.Protection
	block := func(reason string) {
		if s.Reason == "" {
			s.Reason = reason
		}
	}

	// Conflicts.
	switch {
	case !d.BaseExists:
		s.Pills = append(s.Pills, pullStatusPill{Label: "Base branch missing", Kind: "bad"})
		s.Steps = append(s.Steps, pullStep{Text: "Restore the base branch " + d.P.Base})
		block("The base branch " + d.P.Base + " no longer exists.")
	case !d.HeadExists:
		s.Pills = append(s.Pills, pullStatusPill{Label: "Head branch deleted", Kind: "bad"})
		s.Steps = append(s.Steps, pullStep{Text: "Push " + d.P.Head + " again, or close this pull request"})
		block("The head branch " + d.P.Head + " was deleted.")
	case d.Cmp == nil || d.Cmp.Unrelated:
		s.Pills = append(s.Pills, pullStatusPill{Label: "No common history", Kind: "bad"})
		block("The branches have no common history.")
	case d.Cmp.Ahead == 0:
		s.Pills = append(s.Pills, pullStatusPill{Label: "Nothing to merge", Kind: "info"})
		s.Steps = append(s.Steps, pullStep{Text: "Push commits to " + d.P.Head + ", or close this pull request"})
		block(d.P.Base + " already contains every commit from " + d.P.Head + ".")
	default:
		mc, err := g.CheckMerge(ctx, d.BaseSHA, d.HeadSHA)
		switch {
		case err != nil:
			s.Pills = append(s.Pills, pullStatusPill{Label: "Mergeability unknown", Kind: "warn", Title: err.Error()})
			block("Atelier couldn't check this merge.")
		case mc.Clean:
			s.Pills = append(s.Pills, pullStatusPill{Label: "No conflicts", Kind: "ok"})
			s.Steps = append(s.Steps, pullStep{Text: "No conflicts with " + d.P.Base, Done: true})
		default:
			s.Conflicts = mc.Conflicts
			s.Pills = append(s.Pills, pullStatusPill{Label: "Conflicts in " + plural(len(mc.Conflicts), "file"), Kind: "bad"})
			s.Steps = append(s.Steps, pullStep{Text: "Resolve conflicts: merge " + d.P.Base + " into " + d.P.Head + " locally and push"})
			block("Resolve the conflicts first.")
		}
		if d.Cmp.Behind > 0 {
			s.Pills = append(s.Pills, pullStatusPill{Label: plural(d.Cmp.Behind, "commit") + " behind " + d.P.Base, Kind: "info"})
		}
	}

	// Notes to self.
	switch {
	case d.Unresolved > 0 && pr.RequireResolvedNotes:
		s.Pills = append(s.Pills, pullStatusPill{Label: plural(d.Unresolved, "note") + " to self unresolved", Kind: "bad", Title: "Required by branch rule " + pr.Patterns()})
		s.Steps = append(s.Steps, pullStep{Text: "Resolve " + plural(d.Unresolved, "note") + " to self (required by a branch rule)"})
		block("Resolve every note to self first (branch rule " + pr.Patterns() + ").")
	case d.Unresolved > 0:
		s.Pills = append(s.Pills, pullStatusPill{Label: plural(d.Unresolved, "note") + " to self unresolved", Kind: "warn"})
		s.Steps = append(s.Steps, pullStep{Text: "Resolve " + plural(d.Unresolved, "note") + " to self"})
	case len(d.Notes) > 0:
		s.Pills = append(s.Pills, pullStatusPill{Label: "Notes resolved", Kind: "ok"})
		s.Steps = append(s.Steps, pullStep{Text: "All notes to self resolved", Done: true})
	}

	// Branch rules.
	if pr.Protected() {
		s.Pills = append(s.Pills, pullStatusPill{Label: d.P.Base + " is protected", Kind: "info", Title: "Branch rule " + pr.Patterns()})
		if pr.LinearHistory {
			s.Pills = append(s.Pills, pullStatusPill{Label: "Linear history: squash only", Kind: "info"})
		}
	}

	// Checks from Actions; failing or missing required checks block the merge.
	d.Checks.addToStatus(&s, block)

	s.Methods = []pullMergeMethod{
		{Value: "squash", Label: "Squash and merge", Hint: "Combine every commit into one commit on " + d.P.Base, Allowed: true},
		{Value: "merge", Label: "Create a merge commit", Hint: "Keep every commit and add a merge commit", Allowed: !pr.LinearHistory},
	}
	if pr.LinearHistory {
		s.Methods[1].Hint = "Not allowed: " + d.P.Base + " requires linear history"
	}
	s.Primary = s.Methods[0]
	s.CanMerge = s.Reason == ""
	if s.CanMerge {
		s.Steps = append(s.Steps, pullStep{Text: "Squash and merge into " + d.P.Base})
	}
	return s
}

// ---------- files changed ----------

type pullNoteTarget struct {
	Path, Side string
	Line       int
}

// pullFileViews prepares diffs for rendering, attaching notes made on the
// commit being shown to their lines.
func pullFileViews(diff *gitrepo.Diff, notes []*pullNoteView, headSHA string, form *pullNoteTarget, pullURL, repoBase string) []pullFileView {
	if diff == nil {
		return nil
	}
	byLine := map[string][]*pullNoteView{}
	perFile := map[string][2]int{}
	for _, n := range notes {
		c := perFile[n.Path]
		c[0]++
		if !n.Resolved {
			c[1]++
		}
		perFile[n.Path] = c
		if n.CommitSHA == headSHA {
			byLine[fmt.Sprintf("%s\x00%s\x00%d", n.Path, n.Side, n.Line)] = append(byLine[fmt.Sprintf("%s\x00%s\x00%d", n.Path, n.Side, n.Line)], n)
		}
	}
	out := make([]pullFileView, len(diff.Files))
	for i := range diff.Files {
		f := &diff.Files[i]
		fv := pullFileView{DiffFile: f, Index: i, Anchor: "diff-" + strconv.Itoa(i), URL: pullURL, Commit: headSHA, RepoBase: repoBase}
		fv.Notes, fv.Open = perFile[f.Path][0], perFile[f.Path][1]
		for _, l := range f.Lines {
			lv := pullLineView{DiffLine: l}
			switch l.Kind {
			case "add", "ctx":
				lv.Side, lv.Num = "new", l.New
			case "del":
				lv.Side, lv.Num = "old", l.Old
			}
			if lv.Side != "" {
				lv.ID = fmt.Sprintf("f%d-%s%d", i, lv.Side[:1], lv.Num)
				lv.Notes = byLine[fmt.Sprintf("%s\x00%s\x00%d", f.Path, lv.Side, lv.Num)]
				for _, n := range lv.Notes {
					n.Anchor = lv.ID
				}
				if pullURL != "" && form != nil && form.Path == f.Path && form.Side == lv.Side && form.Line == lv.Num {
					lv.Form = &pullNoteForm{URL: pullURL, Commit: headSHA, Path: f.Path, Side: lv.Side, Line: lv.Num,
						ID: "note-body-" + lv.ID, Cancel: pullURL + "/files#" + lv.ID}
				}
			}
			fv.LineViews = append(fv.LineViews, lv)
		}
		out[i] = fv
	}
	return out
}

// pullTree turns the changed files into an indented tree with directory rows.
func pullTree(files []pullFileView) []pullTreeRow {
	idx := make([]int, len(files))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return files[idx[i]].Path < files[idx[j]].Path })
	var rows []pullTreeRow
	var prev []string
	for _, i := range idx {
		f := &files[i]
		dir, name := path.Split(f.Path)
		var parts []string
		if dir = strings.Trim(dir, "/"); dir != "" {
			parts = strings.Split(dir, "/")
		}
		common := 0
		for common < len(parts) && common < len(prev) && parts[common] == prev[common] {
			common++
		}
		for k := common; k < len(parts); k++ {
			rows = append(rows, pullTreeRow{Name: parts[k], Depth: min(k, 8), Dir: true})
		}
		rows = append(rows, pullTreeRow{Name: name, Depth: min(len(parts), 8), File: f})
		prev = parts
	}
	return rows
}

// ---------- actions ----------

func (a *App) pullIdentity(ctx context.Context) gitrepo.Signature {
	sig := gitrepo.Signature{Name: "Atelier", Email: "atelier@atelier.local"}
	if me, err := a.db.PersonalOwner(ctx); err == nil {
		sig.Name, sig.Email = me.Label(), strings.ToLower(me.Name)+"@atelier.local"
	}
	if e, err := a.db.Setting(ctx, settingPullsCommitterEmail); err == nil && e != "" {
		sig.Email = e
	}
	return sig
}

func (a *App) pullMerge(w http.ResponseWriter, r *http.Request) {
	v, g, p, ok := a.loadPull(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if !p.IsOpen() {
		a.renderPull(w, r, v, g, p, "overview", "This pull request isn't open.", http.StatusConflict)
		return
	}
	d, err := a.buildPull(r, v, g, p, "overview")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if !d.Status.CanMerge {
		a.renderPull(w, r, v, g, d.P, "overview", "Can't merge yet: "+d.Status.Reason, http.StatusConflict)
		return
	}
	method := r.FormValue("method")
	var allowed bool
	for _, m := range d.Status.Methods {
		if m.Value == method {
			allowed = m.Allowed
		}
	}
	if !allowed {
		a.renderPull(w, r, v, g, d.P, "overview", "That merge method isn't allowed here.", http.StatusUnprocessableEntity)
		return
	}

	who := a.actor(r)
	committer := a.pullIdentity(ctx)
	req := gitrepo.MergeRequest{Base: p.Base, Head: p.Head, ExpectedHead: r.FormValue("expected_head"),
		Method: method, Committer: committer, Author: committer}
	if method == "squash" {
		var b strings.Builder
		fmt.Fprintf(&b, "%s (#%d)\n\n", p.Title, p.Number)
		for _, c := range d.Cmp.Commits {
			fmt.Fprintf(&b, "* %s\n", c.Subject)
		}
		req.Message = b.String()
		// Keep authorship when one person (or agent) wrote every commit.
		if cs := d.Cmp.Commits; len(cs) > 0 {
			same := true
			for _, c := range cs {
				same = same && c.Author == cs[0].Author && c.Email == cs[0].Email
			}
			if same {
				req.Author = gitrepo.Signature{Name: cs[0].Author, Email: cs[0].Email}
			}
		}
	} else {
		req.Message = fmt.Sprintf("Merge pull request #%d from %s\n\n%s", p.Number, p.Head, p.Title)
	}
	res, err := g.MergeBranches(ctx, req)
	if err != nil {
		msg := ""
		switch {
		case errors.Is(err, gitrepo.ErrRefMoved):
			msg = "A branch changed while merging (probably a new push). Check the latest changes and try again."
		case errors.Is(err, gitrepo.ErrMergeConflict):
			msg = "The branches now have conflicts."
		case errors.Is(err, gitrepo.ErrNothingToMerge):
			msg = "There's nothing left to merge."
		default:
			a.fail(w, r, err)
			return
		}
		a.renderPull(w, r, v, g, d.P, "overview", msg, http.StatusConflict)
		return
	}
	if err := a.db.MarkPullMerged(ctx, p.ID, method, res.SHA, res.BaseBefore, res.Head, who.Name); err != nil {
		a.fail(w, r, err)
		return
	}
	_ = a.db.AddPullEvent(ctx, p.ID, "merged", who.Kind, who.Name, method+" "+res.SHA)
	a.emit(ctx, pullEvent(EventPullMerged, v, p, who, "into "+p.Base))
	_ = a.db.TouchRepo(ctx, v.Repo.ID)
	a.closeReferencedIssues(ctx, v.Repo.ID, p, who)

	if r.FormValue("delete_branch") != "" && p.Head != v.Default && !a.headProtected(ctx, v.Repo.ID, p.Head) {
		if err := g.DeleteBranch(ctx, p.Head, res.Head, committer); err == nil {
			_ = a.db.AddPullEvent(ctx, p.ID, "branch_deleted", who.Kind, who.Name, p.Head)
		}
	}
	http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d", v.Base, p.Number), http.StatusSeeOther)
}

func (a *App) pullState(w http.ResponseWriter, r *http.Request) {
	v, g, p, ok := a.loadPull(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	var err error
	switch r.FormValue("action") {
	case "close":
		if err = a.db.ClosePull(ctx, p.ID); err == nil {
			_ = a.db.AddPullEvent(ctx, p.ID, "closed", who.Kind, who.Name, "")
			a.emit(ctx, pullEvent(EventPullClosed, v, p, who, ""))
		}
	case "reopen":
		if err = a.db.ReopenPull(ctx, p.ID); err == nil {
			_ = a.db.AddPullEvent(ctx, p.ID, "reopened", who.Kind, who.Name, "")
			a.emit(ctx, pullEvent(EventPullReopened, v, p, who, ""))
		}
	default:
		err = store.ErrPullStateChanged
	}
	if errors.Is(err, store.ErrPullStateChanged) {
		a.renderPull(w, r, v, g, p, "overview", "That can't be done to this pull request now.", http.StatusConflict)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d", v.Base, p.Number), http.StatusSeeOther)
}

func (a *App) pullEdit(w http.ResponseWriter, r *http.Request) {
	v, g, p, ok := a.loadPull(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	title := strings.TrimSpace(r.FormValue("title"))
	body := normalizePullText(r.FormValue("body"))
	if title == "" || len(title) > pullTitleMax || len(body) > pullBodyMax {
		a.renderPull(w, r, v, g, p, "overview", fmt.Sprintf("Give the pull request a title under %d characters.", pullTitleMax), http.StatusUnprocessableEntity)
		return
	}
	if err := a.db.UpdatePullText(ctx, p.ID, title, body); err != nil {
		a.fail(w, r, err)
		return
	}
	who := a.actor(r)
	if title != p.Title {
		_ = a.db.AddPullEvent(ctx, p.ID, "renamed", who.Kind, who.Name, p.Title+"\n"+title)
	}
	http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d", v.Base, p.Number), http.StatusSeeOther)
}

func (a *App) pullComment(w http.ResponseWriter, r *http.Request) {
	v, g, p, ok := a.loadPull(w, r)
	if !ok {
		return
	}
	body := normalizePullText(r.FormValue("body"))
	if body == "" || len(body) > pullBodyMax {
		a.renderPull(w, r, v, g, p, "overview", "Write a comment first.", http.StatusUnprocessableEntity)
		return
	}
	who := a.actor(r)
	c, err := a.db.AddPullComment(r.Context(), p.ID, who.Kind, who.Name, body)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.emit(r.Context(), pullEvent(EventPullCommented, v, p, who, body, fmt.Sprintf("#comment-%d", c.ID)))
	http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d#comment-%d", v.Base, p.Number, c.ID), http.StatusSeeOther)
}

var pullSHARe = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func (a *App) pullAddNote(w http.ResponseWriter, r *http.Request) {
	v, g, p, ok := a.loadPull(w, r)
	if !ok {
		return
	}
	in := store.NewPullNote{PullID: p.ID, CommitSHA: r.FormValue("commit"), Path: r.FormValue("path"),
		Side: r.FormValue("side"), Body: normalizePullText(r.FormValue("body"))}
	in.Line, _ = strconv.Atoi(r.FormValue("line"))
	if !pullSHARe.MatchString(in.CommitSHA) || in.Path == "" || len(in.Path) > 4096 || in.Line < 1 ||
		(in.Side != "old" && in.Side != "new") {
		a.renderPull(w, r, v, g, p, "files", "That line can't take a note.", http.StatusUnprocessableEntity)
		return
	}
	if in.Body == "" || len(in.Body) > pullBodyMax {
		a.renderPull(w, r, v, g, p, "files", "Write the note first.", http.StatusUnprocessableEntity)
		return
	}
	who := a.actor(r)
	in.AuthorKind, in.AuthorName = who.Kind, who.Name
	n, err := a.db.AddPullNote(r.Context(), in)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.emit(r.Context(), pullEvent(EventPullNote, v, p, who, in.Body, fmt.Sprintf("/files#note-%d", n.ID)))
	http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d/files#note-%d", v.Base, p.Number, n.ID), http.StatusSeeOther)
}

func (a *App) pullResolveNote(w http.ResponseWriter, r *http.Request) {
	v, _, p, ok := a.loadPull(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	resolved := r.FormValue("resolved") == "1"
	if err := a.db.SetPullNoteResolved(r.Context(), p.ID, id, resolved, a.actor(r).Name); err != nil {
		a.fail(w, r, err)
		return
	}
	back := fmt.Sprintf("%s/pulls/%d", v.Base, p.Number)
	if r.FormValue("back") == "files" {
		back += "/files"
	}
	http.Redirect(w, r, back+"#note-"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (a *App) pullDeleteBranch(w http.ResponseWriter, r *http.Request) {
	v, g, p, ok := a.loadPull(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	fail := func(msg string) { a.renderPull(w, r, v, g, p, "overview", msg, http.StatusConflict) }
	switch {
	case p.IsOpen():
		fail("Close or merge the pull request before deleting its branch.")
		return
	case p.Head == v.Default:
		fail("The default branch can't be deleted.")
		return
	case a.headProtected(ctx, v.Repo.ID, p.Head):
		fail(p.Head + " is protected by a branch rule.")
		return
	}
	expected := ""
	if p.IsMerged() {
		expected = p.HeadSHA // don't throw away commits pushed after the merge
	}
	err := g.DeleteBranch(ctx, p.Head, expected, a.pullIdentity(ctx))
	switch {
	case errors.Is(err, gitrepo.ErrRefMoved):
		fail(p.Head + " has new commits since the merge, so it wasn't deleted.")
		return
	case errors.Is(err, gitrepo.ErrNotFound):
	case err != nil:
		a.fail(w, r, err)
		return
	default:
		who := a.actor(r)
		_ = a.db.AddPullEvent(ctx, p.ID, "branch_deleted", who.Kind, who.Name, p.Head)
	}
	http.Redirect(w, r, fmt.Sprintf("%s/pulls/%d", v.Base, p.Number), http.StatusSeeOther)
}
