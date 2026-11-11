package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"atelier/internal/importer"
	"atelier/internal/store"
	"atelier/internal/tether"
)

// Fuller issue sync, both ways (when a link has "Sync issues" on).
//
//   - Title and body, labels, milestone and open/closed state of every
//     synced issue are compared with what both sides agreed on after the last
//     sync (tether_issue_sync). Whichever side changed since wins; when both
//     did, the later updated_at wins. Labels merge as sets: an add or a
//     removal on either side is kept. What Tether writes is recorded as the
//     new agreed state, so the next sync sees no change and nothing bounces.
//   - Labels map by name and milestones by title, case-insensitively. One
//     missing on the side that needs it is created there with its colour and
//     description (milestones: description, due date and state). Matching
//     milestones also keep their due date and state in step (later edit wins).
//   - Issues created in Atelier after issue pushing started for the link
//     (numbered above its watermark) go to GitHub (right away, and the sync
//     retries); the numbers may differ, so
//     tether_items maps them and the issue shows "GitHub #N".
//   - The first time an already-linked issue is compared (no agreed state
//     yet), GitHub's version is taken when GitHub sent it this sync; otherwise
//     Atelier's current state is just recorded. Nothing is written to GitHub.
//
// GitHub only answers what changed since the last cursor (since=), so an
// Atelier change is pushed assuming GitHub didn't change: anything GitHub
// changed was seen first, in the same sync. When that inbound pass fails,
// outbound changes wait for the next sync.
//
// Everything from GitHub is untrusted text: it's stored as is and rendered
// through the Markdown renderer only; label colours are used only once they
// validate as six hex digits.

func init() {
	registerTemplateFuncs(func(a *App) template.FuncMap {
		return template.FuncMap{"tetherIssueGH": a.tetherIssueGH}
	})
}

// tetherGHRef is the GitHub side of an Atelier issue, for the issue page.
type tetherGHRef struct {
	Number int64
	URL    string
}

func (a *App) tetherIssueGH(repoID, number int64) *tetherGHRef {
	it, err := a.db.TetherItem(context.Background(), repoID, number)
	if err != nil || it.Kind != "issue" {
		return nil
	}
	u := it.GHURL
	if !strings.HasPrefix(u, strings.TrimRight(a.tether().web, "/")+"/") {
		u = ""
	}
	return &tetherGHRef{Number: it.GHNumber, URL: u}
}

// ---------- normalising for comparison ----------

// tetherTitle is how a GitHub title is stored in Atelier (and compared).
func tetherTitle(s string) string {
	t := eventExcerpt(s, 256)
	if t == "" {
		return "(untitled)"
	}
	return t
}

// tetherFooterRe matches the attribution Tether adds to text written in
// Atelier by someone other than the owner.
var tetherFooterRe = regexp.MustCompile(`\n*_\([^\n]{1,200} via Atelier\)_\s*$`)

// tetherBody strips that attribution and normalises line endings.
func tetherBody(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return tetherFooterRe.ReplaceAllString(s, "")
}

func tetherTextHash(title, body string) string {
	h := sha256.Sum256([]byte(tetherTitle(title) + "\x00" + strings.TrimSpace(tetherBody(body))))
	return hex.EncodeToString(h[:16])
}

func tetherKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// tetherLabelOK: names Atelier can hold (others are left alone on GitHub).
func tetherLabelOK(name string) bool {
	n := len([]rune(strings.TrimSpace(name)))
	return n >= 1 && n <= 50 && !strings.ContainsAny(name, "\"\n\r\t")
}

func tetherMilestoneOK(title string) bool {
	n := len([]rune(strings.TrimSpace(title)))
	return n >= 1 && n <= 100 && !strings.ContainsAny(title, "\"\n\r\t")
}

func tetherLabelSet(names []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		if tetherLabelOK(n) {
			out[tetherKey(n)] = true
		}
	}
	return out
}

func tetherSetKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func tetherSameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func tetherDueKey(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("2006-01-02")
}

func tetherDueDate(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	d := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return &d
}

// tetherFooter is the attribution added on GitHub for text someone other
// than the owner wrote in Atelier.
func tetherFooter(kind, name string) string {
	if kind == ActorOwner || name == "" {
		return ""
	}
	return "\n\n_(" + name + " via Atelier)_"
}

// tetherIssueFooter is the attribution for an Atelier issue's body on
// GitHub: only issues written in Atelier by someone other than the owner
// carry one (GitHub's own issues never do).
func tetherIssueFooter(is store.Issue) string {
	if is.AuthorKind == importer.AuthorImport || strings.HasPrefix(is.AuthorName, tetherAuthorPrefix) {
		return ""
	}
	return tetherFooter(is.AuthorKind, is.AuthorName)
}

// ---------- one sync's view of the labels and milestones ----------

// tetherIssueRun is one sync of a repository's issues: the GitHub client of
// the link's person and, fetched at most once, both sides' labels and milestones.
type tetherIssueRun struct {
	a  *App
	c  *tether.Client
	rp store.Repo
	l  store.TetherLink

	ghLabels     map[string]tether.Label // by key
	ghMilestones map[string]tether.Milestone
	labels       map[string]store.Label // Atelier's, by key
	milestones   map[string]store.Milestone

	done  map[int64]bool // Atelier numbers compared this sync
	inErr error          // the inbound pass failed: don't push changes out
	err   error          // first non-fatal problem, for the link's sync error
}

func (a *App) newTetherIssueRun(c *tether.Client, rp store.Repo, l store.TetherLink) *tetherIssueRun {
	return &tetherIssueRun{a: a, c: c, rp: rp, l: l, done: map[int64]bool{}}
}

func (r *tetherIssueRun) problem(err error) {
	if err != nil && r.err == nil {
		r.err = err
	}
}

func (r *tetherIssueRun) loadGHLabels(ctx context.Context, force bool) error {
	if r.ghLabels != nil && !force {
		return nil
	}
	ls, err := r.c.RepoLabels(ctx, r.l.GHRepo)
	if err != nil {
		return err
	}
	r.ghLabels = map[string]tether.Label{}
	for _, l := range ls {
		r.ghLabels[tetherKey(l.Name)] = l
	}
	return nil
}

func (r *tetherIssueRun) loadGHMilestones(ctx context.Context, force bool) error {
	if r.ghMilestones != nil && !force {
		return nil
	}
	ms, err := r.c.Milestones(ctx, r.l.GHRepo)
	if err != nil {
		return err
	}
	r.ghMilestones = map[string]tether.Milestone{}
	for _, m := range ms {
		r.ghMilestones[tetherKey(m.Title)] = m
	}
	return nil
}

func (r *tetherIssueRun) loadLocal(ctx context.Context) error {
	if r.labels != nil {
		return nil
	}
	ls, err := r.a.db.Labels(ctx, r.rp.ID)
	if err != nil {
		return err
	}
	ms, err := r.a.db.Milestones(ctx, r.rp.ID, "")
	if err != nil {
		return err
	}
	r.labels, r.milestones = map[string]store.Label{}, map[string]store.Milestone{}
	for _, l := range ls {
		r.labels[tetherKey(l.Name)] = l
	}
	for _, m := range ms {
		r.milestones[tetherKey(m.Title)] = m
	}
	return nil
}

// localLabel finds (or creates, from GitHub's colour and description) an Atelier label.
func (r *tetherIssueRun) localLabel(ctx context.Context, key string, from *tether.Label) (store.Label, bool) {
	if err := r.loadLocal(ctx); err != nil {
		return store.Label{}, false
	}
	if l, ok := r.labels[key]; ok {
		return l, true
	}
	if from == nil {
		if gl, ok := r.ghLabels[key]; ok {
			from = &gl
		} else if r.loadGHLabels(ctx, false) == nil {
			if gl, ok := r.ghLabels[key]; ok {
				from = &gl
			}
		}
	}
	if from == nil {
		return store.Label{}, false
	}
	color, ok := store.NormalizeColor(from.Color)
	if !ok {
		color = "ededed"
	}
	l, err := r.a.db.CreateLabel(ctx, r.rp.ID, from.Name, color, eventExcerpt(from.Description, 200))
	if err != nil {
		r.labels = nil // someone else made it: read them again
		if r.loadLocal(ctx) == nil {
			l, ok := r.labels[key]
			return l, ok
		}
		return store.Label{}, false
	}
	r.labels[key] = l
	return l, true
}

// ghLabelName finds (or creates, from Atelier's colour and description) a GitHub label.
func (r *tetherIssueRun) ghLabelName(ctx context.Context, key string) (string, error) {
	if err := r.loadGHLabels(ctx, false); err != nil {
		return "", err
	}
	if gl, ok := r.ghLabels[key]; ok {
		return gl.Name, nil
	}
	if err := r.loadLocal(ctx); err != nil {
		return "", err
	}
	ll, ok := r.labels[key]
	if !ok {
		return "", fmt.Errorf("label %q is gone", key)
	}
	gl, err := r.c.CreateLabel(ctx, r.l.GHRepo, ll.Name, ll.Color, ll.Description)
	if tether.IsStatus(err, http.StatusUnprocessableEntity) {
		if err := r.loadGHLabels(ctx, true); err != nil {
			return "", err
		}
		if gl, ok := r.ghLabels[key]; ok {
			return gl.Name, nil
		}
	}
	if err != nil {
		return "", err
	}
	r.ghLabels[key] = gl
	return gl.Name, nil
}

// localMilestone finds (or creates, from GitHub's) an Atelier milestone.
func (r *tetherIssueRun) localMilestone(ctx context.Context, from tether.Milestone) (store.Milestone, bool) {
	if err := r.loadLocal(ctx); err != nil || !tetherMilestoneOK(from.Title) {
		return store.Milestone{}, false
	}
	key := tetherKey(from.Title)
	if m, ok := r.milestones[key]; ok {
		return m, true
	}
	now := time.Now()
	m, err := r.a.db.ImportMilestone(ctx, r.rp.ID, from.Title, eventExcerpt(from.Description, 1000), tetherDueDate(from.DueOn),
		from.State != "closed", now, now, nil)
	if err != nil {
		r.labels, r.milestones = nil, nil
		if r.loadLocal(ctx) == nil {
			m, ok := r.milestones[key]
			return m, ok
		}
		return store.Milestone{}, false
	}
	r.milestones[key] = m
	return m, true
}

// ghMilestone finds (or creates, from Atelier's) a GitHub milestone.
func (r *tetherIssueRun) ghMilestone(ctx context.Context, key string) (int64, error) {
	if err := r.loadGHMilestones(ctx, false); err != nil {
		return 0, err
	}
	if m, ok := r.ghMilestones[key]; ok {
		return m.Number, nil
	}
	if err := r.loadLocal(ctx); err != nil {
		return 0, err
	}
	lm, ok := r.milestones[key]
	if !ok {
		return 0, fmt.Errorf("milestone %q is gone", key)
	}
	gm, err := r.c.CreateMilestone(ctx, r.l.GHRepo, lm.Title, lm.Description, lm.State, lm.DueOn)
	if tether.IsStatus(err, http.StatusUnprocessableEntity) {
		if err := r.loadGHMilestones(ctx, true); err != nil {
			return 0, err
		}
		if m, ok := r.ghMilestones[key]; ok {
			return m.Number, nil
		}
	}
	if err != nil {
		return 0, err
	}
	r.ghMilestones[key] = gm
	return gm.Number, nil
}

// ---------- comparing one issue ----------

// tetherSide is one side of an issue, normalised for comparison.
type tetherSide struct {
	hash      string
	labels    map[string]bool
	milestone string // key
	state     string
}

func tetherLocalSide(is store.Issue) tetherSide {
	var names []string
	for _, l := range is.Labels {
		names = append(names, l.Name)
	}
	ms := ""
	if tetherMilestoneOK(is.Milestone) {
		ms = tetherKey(is.Milestone)
	}
	return tetherSide{hash: tetherTextHash(is.Title, is.Body), labels: tetherLabelSet(names), milestone: ms, state: is.State}
}

func tetherGHSide(is tether.Issue) tetherSide {
	var names []string
	for _, l := range is.Labels {
		names = append(names, l.Name)
	}
	ms := ""
	if is.Milestone != nil && tetherMilestoneOK(is.Milestone.Title) {
		ms = tetherKey(is.Milestone.Title)
	}
	state := "open"
	if is.State == "closed" {
		state = "closed"
	}
	return tetherSide{hash: tetherTextHash(is.Title, is.Body), labels: tetherLabelSet(names), milestone: ms, state: state}
}

func tetherBaseSide(st store.TetherIssueState) tetherSide {
	return tetherSide{hash: st.TextHash, labels: tetherLabelSet(st.Labels), milestone: st.Milestone, state: st.State}
}

// tetherReconcileIssue brings one synced issue in step on both sides. gh is
// GitHub's current issue when it changed since the cursor, nil otherwise
// (GitHub is then assumed unchanged since the last sync).
func (a *App) tetherReconcileIssue(ctx context.Context, run *tetherIssueRun, it store.TetherItem, gh *tether.Issue) error {
	rp := run.rp
	run.done[it.Number] = true
	local, err := a.db.IssueByNumber(ctx, rp.ID, it.Number)
	if err != nil {
		return nil // gone (or a pull request): nothing to compare
	}
	L := tetherLocalSide(local)
	snap, serr := a.db.TetherIssueSync(ctx, rp.ID, it.Number)
	if serr != nil && !errors.Is(serr, store.ErrNotFound) {
		return serr
	}
	first := serr != nil
	if first && gh == nil {
		// First look and GitHub sent nothing new: record Atelier as agreed.
		return a.tetherSaveSnapshot(ctx, rp.ID, it.Number, L, time.Time{}, local.UpdatedAt)
	}
	var G, B tetherSide
	var ghUpdated time.Time
	if gh != nil {
		G, ghUpdated = tetherGHSide(*gh), gh.UpdatedAt
	} else {
		G, ghUpdated = tetherBaseSide(snap), snap.GHUpdatedAt
	}
	if first {
		B = L // first look: GitHub's version is taken
	} else {
		B = tetherBaseSide(snap)
	}
	ghNewer := gh != nil && !local.UpdatedAt.After(gh.UpdatedAt)
	// pick: which side's value to keep, and where it must be written.
	pick := func(l, g, b string) (toLocal, toGH bool) {
		switch {
		case l == g:
			return false, false
		case l == b:
			return true, false
		case g == b:
			return false, true
		case ghNewer:
			return true, false
		}
		return false, true
	}
	fields := map[string]any{}
	final := L

	// Title and body.
	if toLocal, toGH := pick(L.hash, G.hash, B.hash); toLocal {
		if err := a.db.UpdateIssue(ctx, local.ID, tetherTitle(gh.Title), tetherBody(gh.Body), importer.AuthorImport, "github"); err != nil {
			return err
		}
		final.hash = G.hash
	} else if toGH {
		fields["title"] = local.Title
		fields["body"] = local.Body + tetherIssueFooter(local)
	}

	// State.
	if toLocal, toGH := pick(L.state, G.state, B.state); toLocal {
		if G.state == "closed" {
			reason := store.ReasonCompleted
			if gh.StateReason == "not_planned" {
				reason = store.ReasonNotPlanned
			}
			_ = a.db.CloseIssue(ctx, local.ID, importer.AuthorImport, "github", reason)
		} else {
			_ = a.db.ReopenIssue(ctx, local.ID, importer.AuthorImport, "github")
		}
		final.state = G.state
	} else if toGH {
		fields["state"] = L.state
		if L.state == "closed" {
			fields["state_reason"] = "completed"
			if local.StateReason == store.ReasonNotPlanned {
				fields["state_reason"] = "not_planned"
			}
		}
	}

	// Milestone.
	if toLocal, toGH := pick(L.milestone, G.milestone, B.milestone); toLocal {
		var id int64
		if G.milestone != "" {
			m, ok := run.localMilestone(ctx, *gh.Milestone)
			if !ok {
				return fmt.Errorf("couldn't make the milestone %q", gh.Milestone.Title)
			}
			id = m.ID
		}
		if err := a.db.SetIssueMilestone(ctx, local.ID, id, importer.AuthorImport, "github"); err != nil {
			return err
		}
		final.milestone = G.milestone
	} else if toGH {
		if L.milestone == "" {
			fields["milestone"] = nil
		} else {
			n, err := run.ghMilestone(ctx, L.milestone)
			if err != nil {
				return err
			}
			fields["milestone"] = n
		}
	}

	// Labels: kept on both, or added on either side since; removed on either side goes.
	result := map[string]bool{}
	for k := range L.labels {
		if G.labels[k] || !B.labels[k] {
			result[k] = true
		}
	}
	for k := range G.labels {
		if L.labels[k] || !B.labels[k] {
			result[k] = true
		}
	}
	if !tetherSameSet(result, L.labels) {
		ghByKey := map[string]*tether.Label{}
		if gh != nil {
			for i := range gh.Labels {
				ghByKey[tetherKey(gh.Labels[i].Name)] = &gh.Labels[i]
			}
		}
		var ids []int64
		for _, k := range tetherSetKeys(result) {
			if l, ok := run.localLabel(ctx, k, ghByKey[k]); ok {
				ids = append(ids, l.ID)
			}
		}
		if err := a.db.SetIssueLabels(ctx, local.ID, ids, importer.AuthorImport, "github"); err != nil {
			return err
		}
	}
	if !tetherSameSet(result, G.labels) {
		names := []string{}
		for _, k := range tetherSetKeys(result) {
			n, err := run.ghLabelName(ctx, k)
			if err != nil {
				return err
			}
			names = append(names, n)
		}
		if gh != nil { // labels Atelier can't hold stay on GitHub
			for _, l := range gh.Labels {
				if !tetherLabelOK(l.Name) {
					names = append(names, l.Name)
				}
			}
		}
		fields["labels"] = names
	}
	final.labels = result

	if len(fields) > 0 {
		out, err := run.c.UpdateIssue(ctx, run.l.GHRepo, it.GHNumber, fields)
		if err != nil {
			return err
		}
		ghUpdated = out.UpdatedAt
		if st, ok := fields["state"].(string); ok {
			it.GHState = st
			_ = a.db.SaveTetherItem(ctx, it)
		}
	}
	after, err := a.db.IssueByNumber(ctx, rp.ID, it.Number)
	if err != nil {
		return err
	}
	return a.tetherSaveSnapshot(ctx, rp.ID, it.Number, final, ghUpdated, after.UpdatedAt)
}

func (a *App) tetherSaveSnapshot(ctx context.Context, repoID, number int64, s tetherSide, ghUpdated, localUpdated time.Time) error {
	// Times are whole seconds: an Atelier edit later in this same second
	// would look unchanged, so a fresh one is recorded a second early (the
	// next sync looks once more and finds nothing, or that edit).
	if time.Since(localUpdated) < 2*time.Second {
		localUpdated = localUpdated.Add(-time.Second)
	}
	return a.db.SaveTetherIssueSync(ctx, store.TetherIssueState{RepoID: repoID, Number: number, TextHash: s.hash,
		Labels: tetherSetKeys(s.labels), Milestone: s.milestone, State: s.state, GHUpdatedAt: ghUpdated, LocalUpdatedAt: localUpdated})
}

// ---------- outbound ----------

// tetherIssuesOut pushes Atelier's side after the inbound pass: new issues
// go to GitHub, changed ones are compared (GitHub unchanged since, or the
// inbound pass would have seen it), and milestones keep their due date and
// state in step.
func (a *App) tetherIssuesOut(ctx context.Context, run *tetherIssueRun) {
	rp := run.rp
	after, err := a.db.TetherIssuesOutAfter(ctx, rp.ID)
	if err != nil {
		run.problem(err)
		return
	}
	nums, err := a.db.TetherUnsyncedIssues(ctx, rp.ID, after)
	if err != nil {
		run.problem(err)
		return
	}
	for _, n := range nums {
		if err := a.tetherPushNewIssue(ctx, run, n); err != nil {
			run.problem(fmt.Errorf("issue #%d: %w", n, err))
			if _, blocked := run.c.BlockedUntil(); blocked {
				return
			}
		}
	}
	if run.inErr != nil {
		return // GitHub's changes weren't all seen: don't overwrite them
	}
	changed, err := a.db.TetherIssuesChangedLocally(ctx, rp.ID)
	if err != nil {
		run.problem(err)
		return
	}
	for _, n := range changed {
		if run.done[n] {
			continue
		}
		it, err := a.db.TetherItem(ctx, rp.ID, n)
		if err != nil {
			continue
		}
		if err := a.tetherReconcileIssue(ctx, run, it, nil); err != nil {
			run.problem(fmt.Errorf("issue #%d: %w", n, err))
			if _, blocked := run.c.BlockedUntil(); blocked {
				return
			}
		}
	}
	a.tetherSyncMilestones(ctx, run)
}

// tetherPushNewIssue opens an Atelier issue on GitHub, with its labels,
// milestone, state and comments, and records the number mapping.
func (a *App) tetherPushNewIssue(ctx context.Context, run *tetherIssueRun, number int64) error {
	rp := run.rp
	if _, err := a.db.TetherItem(ctx, rp.ID, number); err == nil {
		return nil // already there
	}
	is, err := a.db.IssueByNumber(ctx, rp.ID, number)
	if err != nil {
		return nil
	}
	if is.AuthorKind == importer.AuthorImport || strings.HasPrefix(is.AuthorName, tetherAuthorPrefix) {
		return nil // it came from elsewhere
	}
	L := tetherLocalSide(is)
	var labels []string
	for _, k := range tetherSetKeys(L.labels) {
		n, err := run.ghLabelName(ctx, k)
		if err != nil {
			return err
		}
		labels = append(labels, n)
	}
	var ms int64
	if L.milestone != "" {
		if ms, err = run.ghMilestone(ctx, L.milestone); err != nil {
			return err
		}
	}
	gi, err := run.c.CreateIssue(ctx, run.l.GHRepo, is.Title, is.Body+tetherIssueFooter(is), labels, ms)
	if err != nil {
		return err
	}
	if gi.Number == 0 {
		return errors.New("GitHub didn't say which number it gave the issue")
	}
	it := store.TetherItem{RepoID: rp.ID, Kind: "issue", Number: is.Number, GHNumber: gi.Number,
		GHURL: a.tetherHTMLURL(gi.HTMLURL, fmt.Sprintf("%s/issues/%d", run.l.GHRepo, gi.Number)), Origin: "atelier", GHState: "open"}
	if err := a.db.SaveTetherItem(ctx, it); err != nil {
		return err
	}
	ghUpdated, localUpdated := gi.UpdatedAt, is.UpdatedAt
	if !is.IsOpen() {
		reason := "completed"
		if is.StateReason == store.ReasonNotPlanned {
			reason = "not_planned"
		}
		if out, err := run.c.UpdateIssue(ctx, run.l.GHRepo, gi.Number, map[string]any{"state": "closed", "state_reason": reason}); err == nil {
			ghUpdated = out.UpdatedAt
			it.GHState = "closed"
			_ = a.db.SaveTetherItem(ctx, it)
		} else {
			L.state, localUpdated = "open", time.Time{} // the next sync compares it again and closes it
		}
	}
	run.done[is.Number] = true
	if err := a.tetherSaveSnapshot(ctx, rp.ID, is.Number, L, ghUpdated, localUpdated); err != nil {
		return err
	}
	// Comments written before it reached GitHub.
	if cs, err := a.db.IssueComments(ctx, is.ID); err == nil {
		for _, c := range cs {
			if c.AuthorKind == importer.AuthorImport || strings.HasPrefix(c.AuthorName, tetherAuthorPrefix) {
				continue
			}
			who := Actor{Kind: c.AuthorKind, Name: c.AuthorName}
			if err := a.tetherPostComment(ctx, rp, run.l, "issue", is.Number, c.ID, who); err != nil {
				run.problem(err)
				break
			}
		}
	}
	return nil
}

// tetherIssueOpenedOut pushes an issue opened in Atelier right away (the sync retries).
func (a *App) tetherIssueOpenedOut(ctx context.Context, ev Event) {
	if ev.Number == 0 || ev.Actor.Kind == importer.AuthorImport || strings.HasPrefix(ev.Actor.Name, tetherAuthorPrefix) {
		return
	}
	l, err := a.db.TetherLink(ctx, ev.RepoID)
	if err != nil || !l.SyncIssues {
		return
	}
	bctx := context.WithoutCancel(ctx)
	a.tether().goBG("", func() {
		unlock := a.tether().lockRepo(ev.RepoID)
		defer unlock()
		l, err := a.db.TetherLink(bctx, ev.RepoID)
		if err != nil || !l.SyncIssues {
			return
		}
		rp, err := a.repoByID(bctx, ev.RepoID)
		if err != nil {
			return
		}
		// Only the link's person's own tokens, for the twin's owner.
		c, _, err := a.tetherLinkClient(bctx, l.PersonID, l.GHRepo)
		if err != nil {
			return
		}
		// Sets the watermark if this is the first push: this issue is above it.
		if after, err := a.db.TetherIssuesOutAfter(bctx, rp.ID); err != nil || ev.Number <= after {
			return
		}
		if err := a.tetherPushNewIssue(bctx, a.newTetherIssueRun(c, rp, l), ev.Number); err != nil {
			log.Printf("tether: pushing issue #%d to %s: %s", ev.Number, l.GHRepo, tetherErrText(err))
		}
	})
}

// tetherSyncMilestones keeps matching milestones' due date and state in
// step (the later edit wins). It only asks GitHub when Atelier has milestones.
func (a *App) tetherSyncMilestones(ctx context.Context, run *tetherIssueRun) {
	if err := run.loadLocal(ctx); err != nil || len(run.milestones) == 0 {
		return
	}
	if err := run.loadGHMilestones(ctx, false); err != nil {
		run.problem(err)
		return
	}
	for key, lm := range run.milestones {
		gm, ok := run.ghMilestones[key]
		if !ok {
			continue
		}
		gState := "open"
		if gm.State == "closed" {
			gState = "closed"
		}
		if tetherDueKey(lm.DueOn) == tetherDueKey(gm.DueOn) && lm.State == gState {
			continue
		}
		if gm.UpdatedAt.After(lm.UpdatedAt) {
			if tetherDueKey(lm.DueOn) != tetherDueKey(gm.DueOn) {
				if err := a.db.UpdateMilestone(ctx, run.rp.ID, lm.ID, lm.Title, lm.Description, tetherDueDate(gm.DueOn)); err != nil {
					run.problem(err)
					continue
				}
			}
			if lm.State != gState {
				if err := a.db.SetMilestoneState(ctx, run.rp.ID, lm.ID, gState == "open"); err != nil {
					run.problem(err)
				}
			}
			continue
		}
		state := ""
		if lm.State != gState {
			state = lm.State
		}
		setDue := tetherDueKey(lm.DueOn) != tetherDueKey(gm.DueOn)
		out, err := run.c.UpdateMilestone(ctx, run.l.GHRepo, gm.Number, state, setDue, lm.DueOn)
		if err != nil {
			run.problem(err)
			continue
		}
		run.ghMilestones[key] = out
	}
}

// ---------- pull request reviews ----------

// tetherReviewKinds maps GitHub review states to timeline event data.
var tetherReviewKinds = map[string]string{"APPROVED": "approved", "CHANGES_REQUESTED": "changes_requested", "COMMENTED": "commented"}

// tetherSyncReviews shows GitHub reviews (approvals, changes requested and
// commented reviews with a body) on the Atelier pull request's timeline.
// A pull request's reviews are only asked for when it changed on GitHub.
// Branch rules have no approval count, so reviews are shown, not counted.
func (a *App) tetherSyncReviews(ctx context.Context, c *tether.Client, rp store.Repo, l store.TetherLink, prs []tether.Pull) error {
	for _, p := range prs {
		it, err := a.db.TetherItemByGH(ctx, rp.ID, p.Number)
		if err != nil || it.Kind != "pull" {
			continue
		}
		st, serr := a.db.TetherIssueSync(ctx, rp.ID, it.Number)
		if serr == nil && p.UpdatedAt.Unix() <= st.GHUpdatedAt.Unix() {
			continue
		}
		pr, err := a.db.PullByNumber(ctx, rp.ID, it.Number)
		if err != nil {
			continue
		}
		reviews, err := c.PullReviews(ctx, l.GHRepo, p.Number)
		if err != nil {
			return err
		}
		for _, rv := range reviews {
			kind, ok := tetherReviewKinds[rv.State]
			if !ok || rv.ID == 0 || rv.User.Login == "" || (kind == "commented" && strings.TrimSpace(rv.Body) == "") {
				continue
			}
			if fresh, err := a.db.MarkTetherReview(ctx, rp.ID, rv.ID, it.Number); err != nil || !fresh {
				if err != nil {
					return err
				}
				continue
			}
			author := tetherAuthor(rv.User.Login)
			if err := a.db.AddPullEvent(ctx, pr.ID, "tether_review", importer.AuthorImport, author, kind); err != nil {
				_ = a.db.UnmarkTetherReview(ctx, rp.ID, rv.ID)
				return err
			}
			url, detail := fmt.Sprintf("/%s/pulls/%d", rp.FullName(), pr.Number), ""
			if body := strings.TrimSpace(rv.Body); body != "" {
				when := rv.SubmittedAt
				if when.IsZero() {
					when = time.Now()
				}
				pc, err := a.db.ImportPullComment(ctx, pr.ID, importer.AuthorImport, author, rv.Body, when, when)
				if err != nil {
					return err
				}
				url, detail = fmt.Sprintf("%s#comment-%d", url, pc.ID), eventExcerpt(rv.Body, 140)
			}
			a.emit(ctx, Event{Kind: EventTetherComment, Repo: rp, RepoID: rp.ID, Number: pr.Number, Title: pr.Title, URL: url,
				Actor: Actor{Kind: "bot", Name: author}, Detail: detail})
		}
		st.RepoID, st.Number, st.GHUpdatedAt = rp.ID, it.Number, p.UpdatedAt
		if err := a.db.SaveTetherIssueSync(ctx, st); err != nil {
			return err
		}
	}
	return nil
}

// tetherHasOpenMirrors reports whether Atelier pull requests are open on
// GitHub as mirrors (Review on GitHub), whose reviews need fetching.
func (a *App) tetherHasOpenMirrors(ctx context.Context, repoID int64) bool {
	items, err := a.db.TetherItems(ctx, repoID, "pull")
	if err != nil {
		return false
	}
	for _, it := range items {
		if it.Origin == "atelier" && it.GHState == "open" {
			return true
		}
	}
	return false
}
