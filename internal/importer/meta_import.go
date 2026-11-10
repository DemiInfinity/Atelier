package importer

// Importing issues, pull requests, labels and milestones after the git data.
//
// Items keep their source numbers so "#12" still means the same thing, and
// the repo's counter is moved past the highest one. Every item is recorded in
// import_links, so resuming after a rate limit (or importing again later)
// skips what is already here. Source authors are never turned into Atelier
// people: they're stored as author_kind "import", author_name "github:alice".

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"atelier/internal/gitrepo"
	"atelier/internal/store"
)

// AuthorImport is the author kind of everything brought in from elsewhere.
const AuthorImport = "import"

// MetaOptions says what to import besides the git data.
type MetaOptions struct {
	Labels, Milestones, Issues, Pulls bool
}

func (o MetaOptions) Any() bool { return o.Labels || o.Milestones || o.Issues || o.Pulls }

// String is the stored form, e.g. "labels milestones issues pulls".
func (o MetaOptions) String() string {
	var s []string
	for _, x := range []struct {
		on   bool
		name string
	}{{o.Labels, "labels"}, {o.Milestones, "milestones"}, {o.Issues, "issues"}, {o.Pulls, "pulls"}} {
		if x.on {
			s = append(s, x.name)
		}
	}
	return strings.Join(s, " ")
}

func ParseMetaOptions(s string) MetaOptions {
	var o MetaOptions
	for _, f := range strings.Fields(s) {
		switch f {
		case "labels":
			o.Labels = true
		case "milestones":
			o.Milestones = true
		case "issues":
			o.Issues = true
		case "pulls":
			o.Pulls = true
		}
	}
	return o
}

// MetaRequest is one repository's metadata import.
type MetaRequest struct {
	Source  string // github or gitea
	APIBase string // GitHub API base, or the Gitea server address
	Repo    string // owner/name at the source
	Token   string // never logged or stored by this package
	Options MetaOptions
	HTTP    *http.Client
}

// Stage is one step of an import, shown on the status UI.
type Stage struct {
	Key   string `json:"key"`   // code, labels, milestones, issues, pulls
	Label string `json:"label"` // Code, Labels, …
	State string `json:"state"` // pending, running, done, skipped, partial, failed
	Done  int    `json:"done"`
	Total int    `json:"total"`
	Note  string `json:"note,omitempty"`
}

// Stages lists the stages for an import with these options, all pending.
func Stages(o MetaOptions) []Stage {
	st := []Stage{{Key: "code", Label: "Code", State: "pending"}}
	add := func(on bool, key, label string) {
		if on {
			st = append(st, Stage{Key: key, Label: label, State: "pending"})
		}
	}
	add(o.Labels, "labels", "Labels")
	add(o.Milestones, "milestones", "Milestones")
	add(o.Issues, "issues", "Issues")
	add(o.Pulls, "pulls", "Pull requests")
	return st
}

// MetaResult counts what one run brought in.
type MetaResult struct {
	Labels, Milestones, Issues, Pulls, Comments int // new in this run
	Existing                                    int // already imported earlier, skipped
	Problems                                    []string
	Partial                                     bool      // stopped early; run again to continue
	Reset                                       time.Time // when the rate limit resets, if known
}

// Summary is e.g. "42 issues, 13 pull requests, 8 labels, 120 comments".
func (r MetaResult) Summary() string {
	var parts []string
	add := func(n int, one, many string) {
		if n == 1 {
			parts = append(parts, "1 "+one)
		} else if n > 1 {
			parts = append(parts, strconv.Itoa(n)+" "+many)
		}
	}
	add(r.Issues, "issue", "issues")
	add(r.Pulls, "pull request", "pull requests")
	add(r.Labels, "label", "labels")
	add(r.Milestones, "milestone", "milestones")
	add(r.Comments, "comment", "comments")
	if len(parts) == 0 {
		if r.Existing > 0 {
			return "Nothing new: everything was already imported"
		}
		return "No issues, pull requests, labels or milestones found"
	}
	s := strings.Join(parts, ", ")
	if r.Existing > 0 {
		s += fmt.Sprintf(" (%d already here)", r.Existing)
	}
	return s
}

type metaRun struct {
	db     *store.Store
	repo   store.Repo
	gr     *gitrepo.Repo
	gitBin string
	dir    string
	src    metaSource
	req    MetaRequest
	stages []Stage
	notify func([]Stage)
	res    MetaResult

	labels     map[string]int64 // lower name → id
	milestones map[string]int64 // lower title → id
}

func (m *metaRun) stage(key string) *Stage {
	for i := range m.stages {
		if m.stages[i].Key == key {
			return &m.stages[i]
		}
	}
	return &Stage{}
}

func (m *metaRun) report() {
	if m.notify != nil {
		cp := make([]Stage, len(m.stages))
		copy(cp, m.stages)
		m.notify(cp)
	}
}

func (m *metaRun) author(login string) string { return m.src.prefix() + ":" + login }

// ImportMeta imports the selected metadata into repo, whose git data must
// already be in place. stages comes from Stages (the code stage is left to
// the caller); progress is called with a copy after every change. Running out
// of rate limit is not an error: the result is marked Partial.
func ImportMeta(ctx context.Context, db *store.Store, gs *gitrepo.Store, repo store.Repo, req MetaRequest,
	stages []Stage, progress func([]Stage)) (MetaResult, []Stage, error) {
	m := &metaRun{db: db, repo: repo, gitBin: gs.Git, dir: gs.Path(repo.Owner, repo.Name), req: req, stages: stages, notify: progress}
	if len(m.stages) == 0 {
		m.stages = Stages(req.Options)
	}
	src, err := newMetaSource(req.HTTP, req.Source, req.APIBase, req.Repo, req.Token)
	if err != nil {
		return m.res, m.stages, err
	}
	m.src = src
	if m.gr, err = gs.Open(repo.Owner, repo.Name); err != nil {
		return m.res, m.stages, err
	}
	err = m.run(ctx)
	var rl *RateLimitError
	if errors.As(err, &rl) {
		m.res.Partial, m.res.Reset = true, rl.Reset
		for i := range m.stages {
			switch m.stages[i].State {
			case "running":
				m.stages[i].State = "partial"
			}
		}
		m.report()
		return m.res, m.stages, nil
	}
	if err != nil {
		for i := range m.stages {
			if m.stages[i].State == "running" {
				m.stages[i].State = "failed"
				m.stages[i].Note = err.Error()
			}
		}
		m.res.Partial = true
		m.report()
	}
	return m.res, m.stages, err
}

func (m *metaRun) run(ctx context.Context) error {
	o := m.req.Options
	if err := m.loadMaps(ctx); err != nil {
		return err
	}
	if o.Labels {
		if err := m.importLabels(ctx); err != nil {
			return err
		}
	}
	if o.Milestones {
		if err := m.importMilestones(ctx); err != nil {
			return err
		}
	}
	if !o.Issues && !o.Pulls {
		return nil
	}
	first := m.stage("issues")
	if !o.Issues {
		first = m.stage("pulls")
	}
	first.State, first.Note = "running", "Listing…"
	m.report()
	items, err := m.src.issues(ctx, o.Issues, o.Pulls, func(n int) {
		first.Note = fmt.Sprintf("Listing… page %d", n)
		m.report()
	})
	if err != nil {
		return err
	}
	first.Note = ""
	var issues, pulls []srcIssue
	var max int64
	for _, it := range items {
		if it.Number > max {
			max = it.Number
		}
		if it.IsPull {
			pulls = append(pulls, it)
		} else {
			issues = append(issues, it)
		}
	}
	// Move the counter first, so nothing created meanwhile takes a source number.
	if max > 0 {
		if err := m.db.BumpRepoCounter(ctx, m.repo.ID, max+1); err != nil {
			return err
		}
	}
	if o.Issues {
		st := m.stage("issues")
		st.State, st.Total = "running", len(issues)
		m.report()
		for _, it := range issues {
			if err := m.importIssue(ctx, it); err != nil {
				return err
			}
			st.Done++
			m.report()
		}
		st.State = "done"
		m.report()
	}
	if o.Pulls {
		st := m.stage("pulls")
		st.State, st.Total = "running", len(pulls)
		m.report()
		for _, it := range pulls {
			if err := m.importPull(ctx, it); err != nil {
				return err
			}
			st.Done++
			m.report()
		}
		st.State = "done"
		m.report()
	}
	return nil
}

func (m *metaRun) loadMaps(ctx context.Context) error {
	m.labels, m.milestones = map[string]int64{}, map[string]int64{}
	ls, err := m.db.Labels(ctx, m.repo.ID)
	if err != nil {
		return err
	}
	for _, l := range ls {
		m.labels[strings.ToLower(l.Name)] = l.ID
	}
	ms, err := m.db.Milestones(ctx, m.repo.ID, "")
	if err != nil {
		return err
	}
	for _, x := range ms {
		m.milestones[strings.ToLower(x.Title)] = x.ID
	}
	return nil
}

// ---------- labels and milestones ----------

// cleanName makes a source label or milestone name acceptable to Atelier.
func cleanName(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '"':
			return '\''
		case '\n', '\r', '\t':
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max]))
	}
	return s
}

// ensureLabel finds a label by name or creates it with the source colour.
func (m *metaRun) ensureLabel(ctx context.Context, l srcLabel) (int64, bool, error) {
	name := cleanName(l.Name, 50)
	if name == "" {
		return 0, false, nil
	}
	if id, ok := m.labels[strings.ToLower(name)]; ok {
		return id, false, nil
	}
	color, ok := store.NormalizeColor(l.Color)
	if !ok {
		color = "ededed"
	}
	lab, err := m.db.CreateLabel(ctx, m.repo.ID, name, color, l.Description)
	if err != nil {
		return 0, false, fmt.Errorf("label %q: %w", name, err)
	}
	m.labels[strings.ToLower(name)] = lab.ID
	m.res.Labels++
	if err := m.db.SaveImportLink(ctx, store.ImportLink{RepoID: m.repo.ID, Kind: "label", SourceKey: strings.ToLower(name),
		TargetID: lab.ID, Complete: true}); err != nil {
		return 0, false, err
	}
	return lab.ID, true, nil
}

func (m *metaRun) importLabels(ctx context.Context) error {
	st := m.stage("labels")
	st.State = "running"
	m.report()
	// Whatever arrived before a rate limit is still used.
	ls, listErr := m.src.labels(ctx)
	st.Total = len(ls)
	for _, l := range ls {
		if _, created, err := m.ensureLabel(ctx, l); err != nil {
			return err
		} else if !created {
			m.res.Existing++
		}
		st.Done++
	}
	if listErr != nil {
		return listErr
	}
	if len(ls) > 0 {
		// The repo brought its own labels; don't add the defaults on top.
		if err := m.db.MarkLabelsSeeded(ctx, m.repo.ID); err != nil {
			return err
		}
	}
	st.State = "done"
	m.report()
	return nil
}

func (m *metaRun) ensureMilestone(ctx context.Context, x srcMilestone) (int64, bool, error) {
	title := cleanName(x.Title, 100)
	if title == "" {
		return 0, false, nil
	}
	if id, ok := m.milestones[strings.ToLower(title)]; ok {
		return id, false, nil
	}
	created := x.Created
	if created.IsZero() {
		created = time.Now()
	}
	updated := x.Updated
	if updated.IsZero() {
		updated = created
	}
	ms, err := m.db.ImportMilestone(ctx, m.repo.ID, title, x.Description, x.Due, x.State != "closed", created, updated, x.Closed)
	if err != nil {
		return 0, false, fmt.Errorf("milestone %q: %w", title, err)
	}
	m.milestones[strings.ToLower(title)] = ms.ID
	m.res.Milestones++
	if err := m.db.SaveImportLink(ctx, store.ImportLink{RepoID: m.repo.ID, Kind: "milestone", SourceKey: strings.ToLower(title),
		TargetID: ms.ID, Complete: true}); err != nil {
		return 0, false, err
	}
	return ms.ID, true, nil
}

func (m *metaRun) importMilestones(ctx context.Context) error {
	st := m.stage("milestones")
	st.State = "running"
	m.report()
	ms, listErr := m.src.milestones(ctx)
	st.Total = len(ms)
	for _, x := range ms {
		if _, created, err := m.ensureMilestone(ctx, x); err != nil {
			return err
		} else if !created {
			m.res.Existing++
		}
		st.Done++
	}
	if listErr != nil {
		return listErr
	}
	st.State = "done"
	m.report()
	return nil
}

// ---------- issues ----------

var errNumberTaken = errors.New("number taken")

// claim works out whether a source item still needs creating. It returns the
// existing link when the item was imported before.
func (m *metaRun) claim(ctx context.Context, kind string, it srcIssue) (store.ImportLink, bool, error) {
	key := strconv.FormatInt(it.Number, 10)
	link, err := m.db.ImportLinkFor(ctx, m.repo.ID, kind, key)
	if err == nil {
		return link, true, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return link, false, err
	}
	used, id, authorKind, err := m.db.NumberOwner(ctx, m.repo.ID, it.Number)
	if err != nil {
		return link, false, err
	}
	if used == "" {
		return store.ImportLink{}, false, nil
	}
	// An earlier run created the row but stopped before linking it: adopt it.
	if used == kind && authorKind == AuthorImport {
		link = store.ImportLink{RepoID: m.repo.ID, Kind: kind, SourceKey: key, TargetID: id, Number: it.Number,
			SourceURL: it.URL, SourceAuthor: m.author(it.Author)}
		return link, true, m.db.SaveImportLink(ctx, link)
	}
	return link, false, fmt.Errorf("#%d skipped: the number is already used in Atelier (%w)", it.Number, errNumberTaken)
}

func (m *metaRun) importIssue(ctx context.Context, it srcIssue) error {
	link, exists, err := m.claim(ctx, "issue", it)
	if err != nil && !exists {
		if errors.Is(err, errNumberTaken) {
			m.res.Problems = append(m.res.Problems, "issue "+err.Error())
			return nil
		}
		return err
	}
	if exists && link.Complete {
		m.res.Existing++
		return nil
	}
	if !exists {
		var labelIDs []int64
		for _, l := range it.Labels {
			id, _, err := m.ensureLabel(ctx, l)
			if err != nil {
				return err
			}
			if id > 0 {
				labelIDs = append(labelIDs, id)
			}
		}
		var msID int64
		if it.Milestone != nil {
			if msID, _, err = m.ensureMilestone(ctx, *it.Milestone); err != nil {
				return err
			}
		}
		is, err := m.db.ImportIssue(ctx, store.ImportedIssue{RepoID: m.repo.ID, Number: it.Number, Title: it.Title,
			Body: it.Body, State: it.State, StateReason: it.StateReason, AuthorKind: AuthorImport,
			AuthorName: m.author(it.Author), MilestoneID: msID, LabelIDs: labelIDs, CreatedAt: it.Created,
			UpdatedAt: it.Updated, ClosedAt: it.Closed})
		if err != nil {
			return fmt.Errorf("issue #%d: %w", it.Number, err)
		}
		link = store.ImportLink{RepoID: m.repo.ID, Kind: "issue", SourceKey: strconv.FormatInt(it.Number, 10),
			TargetID: is.ID, Number: it.Number, SourceURL: it.URL, SourceAuthor: m.author(it.Author)}
		if err := m.db.SaveImportLink(ctx, link); err != nil {
			return err
		}
		m.res.Issues++
	}
	if it.Comments > 0 {
		cs, listErr := m.src.comments(ctx, it.Number)
		for _, c := range cs {
			if err := m.importComment(ctx, "issue_comment", link.TargetID, c); err != nil {
				return err
			}
		}
		if listErr != nil {
			return listErr
		}
	}
	link.Complete = true
	return m.db.SaveImportLink(ctx, link)
}

func (m *metaRun) importComment(ctx context.Context, kind string, targetID int64, c srcComment) error {
	key := strconv.FormatInt(c.ID, 10)
	if _, err := m.db.ImportLinkFor(ctx, m.repo.ID, kind, key); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if strings.TrimSpace(c.Body) == "" {
		return nil
	}
	var id int64
	if kind == "issue_comment" {
		ic, err := m.db.ImportIssueComment(ctx, targetID, c.Body, AuthorImport, m.author(c.Author), c.Created, c.Updated)
		if err != nil {
			return err
		}
		id = ic.ID
	} else {
		pc, err := m.db.ImportPullComment(ctx, targetID, AuthorImport, m.author(c.Author), c.Body, c.Created, c.Updated)
		if err != nil {
			return err
		}
		id = pc.ID
	}
	m.res.Comments++
	return m.db.SaveImportLink(ctx, store.ImportLink{RepoID: m.repo.ID, Kind: kind, SourceKey: key, TargetID: id,
		SourceAuthor: m.author(c.Author), Complete: true})
}

// ---------- pull requests ----------

// reachable reports whether a commit is in the repository and reachable from
// a branch or tag (so it won't vanish in a gc and break the pull request page).
func (m *metaRun) reachable(ctx context.Context, sha string) bool {
	if len(sha) < 7 || strings.HasPrefix(sha, "-") {
		return false
	}
	for _, c := range sha {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	full, err := m.gr.ResolveCommit(ctx, sha)
	if err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, gitOr(m.gitBin), "for-each-ref", "--count=1", "--contains", full,
		"--format=%(refname)", "refs/heads", "refs/tags")
	cmd.Dir = m.dir
	cmd.Env = gitEnv(Creds{})
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func (m *metaRun) importPull(ctx context.Context, it srcIssue) error {
	link, exists, err := m.claim(ctx, "pull", it)
	if err != nil && !exists {
		if errors.Is(err, errNumberTaken) {
			m.res.Problems = append(m.res.Problems, "pull request "+err.Error())
			return nil
		}
		return err
	}
	if exists && link.Complete {
		m.res.Existing++
		return nil
	}
	var note string
	if !exists {
		sp, err := m.src.pull(ctx, it.Number)
		if errors.Is(err, errAPINotFound) {
			sp = srcPull{} // listed as a pull request but its details are gone
		} else if err != nil {
			return err
		}
		in := store.ImportedPull{RepoID: m.repo.ID, Number: it.Number, Title: it.Title, Body: it.Body,
			Head: sp.Head, Base: sp.Base, AuthorKind: AuthorImport, AuthorName: m.author(it.Author),
			CreatedAt: it.Created, UpdatedAt: it.Updated, ClosedAt: it.Closed}
		sameRepo := sp.HeadRepo != "" && strings.EqualFold(sp.HeadRepo, m.req.Repo)
		if !sameRepo && sp.HeadLabel != "" {
			in.Head = sp.HeadLabel // a fork's branch: owner:branch, never a local branch
		}
		if in.Head == "" {
			in.Head = "unknown"
		}
		if in.Base == "" {
			in.Base = "unknown"
		}
		headSHA, headErr := "", error(nil)
		if sameRepo {
			headSHA, headErr = m.gr.BranchSHA(ctx, sp.Head)
		}
		_, baseErr := m.gr.BranchSHA(ctx, sp.Base)
		branchesHere := sameRepo && headErr == nil && baseErr == nil && sp.Head != "" && sp.Base != ""
		switch {
		case sp.Merged:
			in.State, in.MergedAt, in.MergedSHA = "merged", sp.MergedAt, sp.MergeSHA
			if sp.MergedBy != "" {
				in.MergedBy = m.author(sp.MergedBy)
			}
			if m.reachable(ctx, sp.HeadSHA) && m.reachable(ctx, sp.BaseSHA) {
				in.HeadSHA, in.BaseSHA = sp.HeadSHA, sp.BaseSHA
			}
		case !branchesHere:
			in.State = "closed"
			missing := sp.Head
			if !sameRepo {
				missing = in.Head
			} else if headErr == nil {
				missing = sp.Base
			}
			if it.State == "open" {
				t := time.Now() // closed now, by the import
				in.ClosedAt = &t
				note = fmt.Sprintf("Imported as closed: the branch `%s` no longer exists. It was open at the source.", missing)
			} else {
				note = fmt.Sprintf("The branch `%s` no longer exists.", missing)
			}
		case it.State == "closed":
			in.State, in.HeadSHA = "closed", headSHA
		default:
			in.State, in.HeadSHA = "open", headSHA
		}
		p, err := m.db.ImportPull(ctx, in)
		if err != nil {
			return fmt.Errorf("pull request #%d: %w", it.Number, err)
		}
		link = store.ImportLink{RepoID: m.repo.ID, Kind: "pull", SourceKey: strconv.FormatInt(it.Number, 10),
			TargetID: p.ID, Number: it.Number, SourceURL: it.URL, SourceAuthor: m.author(it.Author)}
		if err := m.db.SaveImportLink(ctx, link); err != nil {
			return err
		}
		m.res.Pulls++
		if note != "" {
			if _, err := m.db.ImportPullComment(ctx, p.ID, AuthorImport, "atelier", note, time.Now(), time.Now()); err != nil {
				return err
			}
		}
	}
	if it.Comments > 0 {
		cs, listErr := m.src.comments(ctx, it.Number)
		for _, c := range cs {
			if err := m.importComment(ctx, "pull_comment", link.TargetID, c); err != nil {
				return err
			}
		}
		if listErr != nil {
			return listErr
		}
	}
	link.Complete = true
	return m.db.SaveImportLink(ctx, link)
}
