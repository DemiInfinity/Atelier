package web

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"atelier/internal/store"
)

// The project page: /projects/{id}?view=…&layout=board|table|roadmap&q=…&sort=…

type projCell struct {
	Field   store.ProjField
	Value   string // stored value (option id for single selects)
	Display string
	Color   string // single selects
	Range   string // iterations: their dates, e.g. "3 Nov – 16 Nov 2026"
	Current bool   // iterations: running today
}

type projCard struct {
	ID      int64
	Hidden  bool // the viewer can't read the item's repository
	Kind    string
	Title   string
	URL     string // the issue / PR / item page
	ItemURL string // /projects/{id}/items/{item}
	Repo    string // owner/name
	Number  int64
	State   string
	Labels  []store.Label
	Status  projCell
	Cells   []projCell // every field except Status, in order
	Prev    int64      // card above in its board column (0 = first)
	Next    int64      // card below in its board column (0 = last)
}

// StateLabel is a short word for the item's state.
func (c projCard) StateLabel() string {
	switch {
	case c.Hidden:
		return ""
	case c.Kind == "draft":
		return "Draft"
	case c.State == "":
		return "Deleted"
	case c.State == "merged":
		return "Merged"
	case c.State == "closed":
		return "Closed"
	}
	return "Open"
}

// AllCells is Status followed by the other fields.
func (c projCard) AllCells() []projCell { return append([]projCell{c.Status}, c.Cells...) }

type projColumn struct {
	Value string // option id, "" for No status
	Name  string
	Color string
	Cards []projCard
}

type projViewTab struct {
	View   store.ProjView
	URL    string
	Active bool
}

type projTableCol struct {
	Key, Label, URL string
	Sorted, Desc    bool
}

type projTick struct {
	X     float64
	Label string
}

type projRoadRow struct {
	Card       projCard
	Start, End string
	X, W       float64 // bar position on a 0–1000 scale
	HasBar     bool
}

type projRoadmap struct {
	DateFields   []store.ProjField
	Start, End   store.ProjField
	From, To     string
	Ticks        []projTick
	Rows         []projRoadRow
	Unscheduled  []projCard
	NeedsFields  bool
	TodayX       float64
	ShowToday    bool
	StartFieldID int64
	EndFieldID   int64
}

type projViewData struct {
	P          store.Project
	URL        string
	Self       string // this page's URL, for return=
	CanWrite   bool
	CanAdmin   bool
	Layout     string
	Q          string
	Sort       string
	Views      []projViewTab
	Active     *store.ProjView
	Modified   bool
	Fields     []store.ProjField
	Status     store.ProjField
	Columns    []projColumn
	Rows       []projCard
	TableCols  []projTableCol
	Road       projRoadmap
	Total      int
	Shown      int
	Hidden     int
	LabelCSS   []string
	LayoutURLs map[string]string
	ClearURL   string
}

func (a *App) projView(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermRead)
	if !ok {
		return
	}
	ctx := r.Context()
	who := a.actor(r)
	q := r.URL.Query()

	views, err := a.db.ProjectViews(ctx, p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	fields, err := a.db.ProjectFields(ctx, p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := a.db.ProjectItems(ctx, p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}

	d := projViewData{P: p, URL: projURL(p), Fields: fields,
		CanWrite: a.canProject(ctx, who, p, PermWrite), CanAdmin: a.canProject(ctx, who, p, PermAdmin)}
	for _, f := range fields {
		if f.IsStatus() {
			d.Status = f
		}
	}

	// Which view, and what the URL overrides.
	var active *store.ProjView
	if vid, err := strconv.ParseInt(q.Get("view"), 10, 64); err == nil {
		for i := range views {
			if views[i].ID == vid {
				active = &views[i]
			}
		}
	} else if !q.Has("layout") && !q.Has("q") && !q.Has("sort") && len(views) > 0 {
		active = &views[0]
	}
	d.Layout, d.Q, d.Sort = "board", "", ""
	if active != nil {
		d.Layout, d.Q, d.Sort = active.Layout, active.Filter, active.Sort
	}
	if l := q.Get("layout"); store.ValidProjLayout(l) {
		d.Layout = l
	}
	if q.Has("q") {
		d.Q = strings.TrimSpace(q.Get("q"))
	}
	if q.Has("sort") {
		d.Sort = q.Get("sort")
	}
	d.Active = active
	if active != nil {
		d.Modified = d.Layout != active.Layout || d.Q != active.Filter || d.Sort != active.Sort
	}
	d.Self = projSelfURL(p, active, d.Layout, d.Q, d.Sort)
	d.ClearURL = projSelfURL(p, active, d.Layout, "", d.Sort)
	d.LayoutURLs = map[string]string{}
	for _, l := range []string{"board", "table", "roadmap"} {
		d.LayoutURLs[l] = projSelfURL(p, active, l, d.Q, d.Sort)
	}
	for _, v := range views {
		d.Views = append(d.Views, projViewTab{View: v, URL: d.URL + "?view=" + strconv.FormatInt(v.ID, 10), Active: active != nil && active.ID == v.ID})
	}

	// Cards the viewer may see, filtered.
	filter := parseProjFilter(d.Q)
	labelRepos := map[string]bool{}
	var cards []projCard
	for _, it := range items {
		visible := a.projItemVisible(ctx, who, it)
		d.Total++
		if !filter.match(it, visible, fields) {
			continue
		}
		c := projCardFor(p, it, visible, fields)
		if c.Hidden {
			d.Hidden++
		} else if len(c.Labels) > 0 {
			labelRepos[c.Repo] = true
		}
		cards = append(cards, c)
	}
	d.Shown = len(cards)
	for rp := range labelRepos {
		d.LabelCSS = append(d.LabelCSS, "/"+rp+"/labels.css")
	}
	sort.Strings(d.LabelCSS)

	switch d.Layout {
	case "table":
		d.Rows, d.TableCols = projTable(d, cards)
	case "roadmap":
		d.Road = projBuildRoadmap(q, fields, cards)
	default:
		d.Columns = projBoard(d.Status, cards)
	}
	a.render(w, r, "projects_view", page{Title: p.Title, Nav: "projects", Data: d})
}

func projSelfURL(p store.Project, active *store.ProjView, layout, q, sortKey string) string {
	v := url.Values{}
	if active != nil {
		v.Set("view", strconv.FormatInt(active.ID, 10))
	}
	v.Set("layout", layout)
	if q != "" {
		v.Set("q", q)
	}
	if sortKey != "" {
		v.Set("sort", sortKey)
	}
	return projURL(p) + "?" + v.Encode()
}

func projCardFor(p store.Project, it store.ProjItem, visible bool, fields []store.ProjField) projCard {
	c := projCard{ID: it.ID, Kind: it.Kind, ItemURL: projURL(p) + "/items/" + strconv.FormatInt(it.ID, 10)}
	if !visible {
		c.Hidden = true
		c.Title = "Private item"
	} else {
		c.Title, c.Repo, c.Number, c.State, c.Labels = it.Title, it.RepoFullName(), it.Number, it.State, it.Labels
		switch it.Kind {
		case "issue":
			c.URL = "/" + c.Repo + "/issues/" + strconv.FormatInt(it.Number, 10)
		case "pull":
			c.URL = "/" + c.Repo + "/pulls/" + strconv.FormatInt(it.Number, 10)
		default:
			c.URL = c.ItemURL
		}
		if it.Missing() {
			c.Title = "Deleted " + it.Kind
		}
	}
	for _, f := range fields {
		cell := projCellFor(f, it.Values[f.ID])
		if f.IsStatus() {
			c.Status = cell
		} else {
			c.Cells = append(c.Cells, cell)
		}
	}
	return c
}

func projCellFor(f store.ProjField, v string) projCell {
	cell := projCell{Field: f, Value: v, Display: v}
	if f.IsSelect() {
		cell.Display = ""
		if o, ok := f.Option(v); ok {
			cell.Display, cell.Color = o.Name, o.Color
		} else {
			cell.Value = ""
		}
	}
	if f.Kind == store.ProjFieldDate && v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			cell.Display = t.Format("2 Jan 2006")
		}
	}
	if f.IsIteration() {
		cell.Display = ""
		if it, ok := f.Iteration(v); ok {
			cell.Display, cell.Range, cell.Current = it.Name, it.Range(), it.Current()
		} else {
			cell.Value = ""
		}
	}
	return cell
}

// projBoard groups cards by Status, in option order. "No status" comes first
// and only when something is in it.
func projBoard(status store.ProjField, cards []projCard) []projColumn {
	cols := []projColumn{{Value: "", Name: "No status", Color: "gray"}}
	idx := map[string]int{"": 0}
	for _, o := range status.Options {
		k := strconv.FormatInt(o.ID, 10)
		idx[k] = len(cols)
		cols = append(cols, projColumn{Value: k, Name: o.Name, Color: o.Color})
	}
	for _, c := range cards {
		i, ok := idx[c.Status.Value]
		if !ok {
			i = 0
		}
		cols[i].Cards = append(cols[i].Cards, c)
	}
	for i := range cols {
		cs := cols[i].Cards
		for k := range cs {
			if k > 0 {
				cs[k].Prev = cs[k-1].ID
			}
			if k+1 < len(cs) {
				cs[k].Next = cs[k+1].ID
			}
		}
	}
	if len(cols[0].Cards) == 0 {
		cols = cols[1:]
	}
	return cols
}

// projTable sorts rows by ?sort=key[:desc] (title, repo, state, number or
// f<field id>). Without a sort, rows keep board order. Private items sort last.
func projTable(d projViewData, cards []projCard) ([]projCard, []projTableCol) {
	key, desc := d.Sort, false
	if k, dir, ok := strings.Cut(d.Sort, ":"); ok {
		key, desc = k, dir == "desc"
	}
	colURL := func(k string) (string, bool, bool) {
		next := k
		sorted := key == k
		if sorted && !desc {
			next = k + ":desc"
		}
		v := url.Values{}
		if d.Active != nil {
			v.Set("view", strconv.FormatInt(d.Active.ID, 10))
		}
		v.Set("layout", "table")
		if d.Q != "" {
			v.Set("q", d.Q)
		}
		v.Set("sort", next)
		return d.URL + "?" + v.Encode(), sorted, sorted && desc
	}
	var cols []projTableCol
	add := func(k, label string) {
		u, s, ds := colURL(k)
		cols = append(cols, projTableCol{Key: k, Label: label, URL: u, Sorted: s, Desc: ds})
	}
	add("title", "Title")
	add("repo", "Repository")
	add("state", "State")
	for _, f := range d.Fields {
		add("f"+strconv.FormatInt(f.ID, 10), f.Name)
	}

	val := func(c projCard) (string, float64, bool) { // text, number, isNumber
		switch key {
		case "title":
			return strings.ToLower(c.Title), 0, false
		case "repo":
			return strings.ToLower(c.Repo) + "#" + strconv.FormatInt(1e9+c.Number, 10), 0, false
		case "state":
			return c.StateLabel(), 0, false
		case "number":
			return "", float64(c.Number), true
		}
		if id, err := strconv.ParseInt(strings.TrimPrefix(key, "f"), 10, 64); err == nil && strings.HasPrefix(key, "f") {
			cells := append([]projCell{c.Status}, c.Cells...)
			for _, cell := range cells {
				if cell.Field.ID != id {
					continue
				}
				switch cell.Field.Kind {
				case store.ProjFieldNumber:
					n, err := strconv.ParseFloat(cell.Value, 64)
					if err != nil {
						return "", 0, false
					}
					return "x", n, true
				case store.ProjFieldIteration:
					if it, ok := cell.Field.Iteration(cell.Value); ok {
						return it.StartDate(), 0, false
					}
					return "", 0, false
				case store.ProjFieldSelect:
					for i, o := range cell.Field.Options {
						if strconv.FormatInt(o.ID, 10) == cell.Value {
							return strconv.Itoa(1000 + i), 0, false
						}
					}
					return "", 0, false
				}
				return strings.ToLower(cell.Value), 0, false
			}
		}
		return "", 0, false
	}
	if key != "" {
		sort.SliceStable(cards, func(i, j int) bool {
			ci, cj := cards[i], cards[j]
			if ci.Hidden != cj.Hidden {
				return !ci.Hidden
			}
			si, ni, isNi := val(ci)
			sj, nj, isNj := val(cj)
			emptyI, emptyJ := si == "" && !isNi, sj == "" && !isNj
			if emptyI != emptyJ {
				return !emptyI // empty values last, either direction
			}
			var less, greater bool
			if isNi && isNj {
				less, greater = ni < nj, ni > nj
			} else {
				less, greater = si < sj, si > sj
			}
			if desc {
				return greater
			}
			return less
		})
	}
	return cards, cols
}

// projBuildRoadmap lays dated items on a timeline. The start and target
// fields are ?start= and ?target= (date field ids); by default a field
// named like "start" and one like "target", "end" or "due", else the first
// two date fields.
func projBuildRoadmap(q url.Values, fields []store.ProjField, cards []projCard) projRoadmap {
	var rm projRoadmap
	for _, f := range fields {
		if f.Kind == store.ProjFieldDate {
			rm.DateFields = append(rm.DateFields, f)
		}
	}
	if len(rm.DateFields) == 0 {
		rm.NeedsFields = true
		rm.Unscheduled = cards
		return rm
	}
	pick := func(param string, names ...string) (store.ProjField, bool) {
		if id, err := strconv.ParseInt(q.Get(param), 10, 64); err == nil {
			for _, f := range rm.DateFields {
				if f.ID == id {
					return f, true
				}
			}
		}
		for _, n := range names {
			for _, f := range rm.DateFields {
				if strings.Contains(strings.ToLower(f.Name), n) {
					return f, true
				}
			}
		}
		return store.ProjField{}, false
	}
	start, okS := pick("start", "start", "begin")
	end, okE := pick("target", "target", "end", "due", "finish")
	if !okS {
		start = rm.DateFields[0]
	}
	if !okE {
		end = rm.DateFields[len(rm.DateFields)-1]
	}
	rm.Start, rm.End, rm.StartFieldID, rm.EndFieldID = start, end, start.ID, end.ID

	value := func(c projCard, f store.ProjField) string {
		for _, cell := range c.Cells {
			if cell.Field.ID == f.ID {
				return cell.Value
			}
		}
		return ""
	}
	type span struct {
		c    projCard
		s, e time.Time
	}
	var spans []span
	var from, to time.Time
	for _, c := range cards {
		s, e := value(c, start), value(c, end)
		if s == "" && e == "" {
			rm.Unscheduled = append(rm.Unscheduled, c)
			continue
		}
		if s == "" {
			s = e
		}
		if e == "" {
			e = s
		}
		ts, err1 := time.Parse("2006-01-02", s)
		te, err2 := time.Parse("2006-01-02", e)
		if err1 != nil || err2 != nil {
			rm.Unscheduled = append(rm.Unscheduled, c)
			continue
		}
		if te.Before(ts) {
			ts, te = te, ts
		}
		spans = append(spans, span{c, ts, te})
		if from.IsZero() || ts.Before(from) {
			from = ts
		}
		if to.IsZero() || te.After(to) {
			to = te
		}
	}
	if len(spans) == 0 {
		return rm
	}
	// Whole weeks from the Monday on or before the first date, at least four.
	from = from.AddDate(0, 0, -((int(from.Weekday()) + 6) % 7))
	to = to.AddDate(0, 0, 1)
	if min := from.AddDate(0, 0, 28); to.Before(min) {
		to = min
	}
	days := to.Sub(from).Hours() / 24
	x := func(t time.Time) float64 { return 1000 * (t.Sub(from).Hours() / 24) / days }
	step := 7
	if days > 7*16 {
		step = 28
	}
	for t := from; t.Before(to); t = t.AddDate(0, 0, step) {
		rm.Ticks = append(rm.Ticks, projTick{X: x(t), Label: t.Format("2 Jan")})
	}
	rm.From, rm.To = from.Format("2 Jan 2006"), to.AddDate(0, 0, -1).Format("2 Jan 2006")
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if !today.Before(from) && today.Before(to) {
		rm.ShowToday, rm.TodayX = true, x(today)
	}
	for _, sp := range spans {
		x0 := x(sp.s)
		w := x(sp.e.AddDate(0, 0, 1)) - x0
		if w < 4 {
			w = 4
		}
		rm.Rows = append(rm.Rows, projRoadRow{Card: sp.c, Start: sp.s.Format("2 Jan 2006"), End: sp.e.Format("2 Jan 2006"), X: x0, W: w, HasBar: true})
	}
	sort.SliceStable(rm.Rows, func(i, j int) bool { return rm.Rows[i].X < rm.Rows[j].X })
	return rm
}

// ---------- saved views ----------

func (a *App) projViewSave(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermWrite)
	if !ok {
		return
	}
	v := store.ProjView{ProjectID: p.ID, Name: r.FormValue("name"), Layout: r.FormValue("layout"),
		Filter: strings.TrimSpace(r.FormValue("q")), Sort: r.FormValue("sort")}
	if id, err := strconv.ParseInt(r.FormValue("view"), 10, 64); err == nil && r.FormValue("update") == "1" {
		v.ID = id
		if v.Name == "" {
			views, err := a.db.ProjectViews(r.Context(), p.ID)
			if err != nil {
				a.fail(w, r, err)
				return
			}
			for _, x := range views {
				if x.ID == id {
					v.Name = x.Name
				}
			}
		}
	}
	saved, err := a.db.SaveView(r.Context(), v)
	if err != nil {
		if err == store.ErrProjInvalid {
			http.Error(w, "Give the view a name (up to 60 characters) and pick board, table or roadmap.", http.StatusUnprocessableEntity)
			return
		}
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, projURL(p)+"?view="+strconv.FormatInt(saved.ID, 10), http.StatusSeeOther)
}

func (a *App) projViewDelete(w http.ResponseWriter, r *http.Request) {
	p, ok := a.loadProject(w, r, PermWrite)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("view"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	if err := a.db.DeleteView(r.Context(), p.ID, id); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, projURL(p), http.StatusSeeOther)
}
