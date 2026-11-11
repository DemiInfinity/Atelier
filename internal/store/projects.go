package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Projects: GitHub-Projects-style planning boards. A project belongs to an
// account (personal or company) and holds issues and pull requests from any
// repository plus draft items. Each project has custom fields (Status is a
// built-in single select), saved views and simple automation. The logic and
// all permission checks live in internal/web/projects*.go: this file never
// decides who may see an item.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS proj_projects (
  id           INTEGER PRIMARY KEY,
  owner_id     INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  title        TEXT NOT NULL,
  description  TEXT NOT NULL DEFAULT '',
  private      INTEGER NOT NULL DEFAULT 1,
  closed       INTEGER NOT NULL DEFAULT 0,
  auto_add     INTEGER NOT NULL DEFAULT 0,
  auto_done    INTEGER NOT NULL DEFAULT 1,
  auto_reopen  INTEGER NOT NULL DEFAULT 1,
  created_kind TEXT NOT NULL DEFAULT '',
  created_by   TEXT NOT NULL DEFAULT '',
  person_id    INTEGER REFERENCES owners(id) ON DELETE SET NULL,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS proj_projects_owner ON proj_projects (owner_id);

CREATE TABLE IF NOT EXISTS proj_fields (
  id         INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES proj_projects(id) ON DELETE CASCADE,
  name       TEXT NOT NULL COLLATE NOCASE,
  kind       TEXT NOT NULL CHECK (kind IN ('single_select','text','number','date')),
  builtin    TEXT NOT NULL DEFAULT '',
  position   INTEGER NOT NULL DEFAULT 0,
  UNIQUE (project_id, name)
);

CREATE TABLE IF NOT EXISTS proj_options (
  id       INTEGER PRIMARY KEY,
  field_id INTEGER NOT NULL REFERENCES proj_fields(id) ON DELETE CASCADE,
  name     TEXT NOT NULL COLLATE NOCASE,
  color    TEXT NOT NULL DEFAULT 'gray',
  position INTEGER NOT NULL DEFAULT 0,
  UNIQUE (field_id, name)
);

CREATE TABLE IF NOT EXISTS proj_items (
  id         INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES proj_projects(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL CHECK (kind IN ('issue','pull','draft')),
  repo_id    INTEGER REFERENCES repos(id) ON DELETE CASCADE,
  number     INTEGER NOT NULL DEFAULT 0,
  title      TEXT NOT NULL DEFAULT '',
  body       TEXT NOT NULL DEFAULT '',
  position   REAL NOT NULL DEFAULT 0,
  added_kind TEXT NOT NULL DEFAULT '',
  added_by   TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  CHECK ((kind = 'draft') = (repo_id IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS proj_items_content ON proj_items (project_id, kind, repo_id, number) WHERE kind <> 'draft';
CREATE INDEX IF NOT EXISTS proj_items_lookup ON proj_items (repo_id, number);

CREATE TABLE IF NOT EXISTS proj_values (
  item_id  INTEGER NOT NULL REFERENCES proj_items(id) ON DELETE CASCADE,
  field_id INTEGER NOT NULL REFERENCES proj_fields(id) ON DELETE CASCADE,
  value    TEXT NOT NULL,
  PRIMARY KEY (item_id, field_id)
);

CREATE TABLE IF NOT EXISTS proj_views (
  id         INTEGER PRIMARY KEY,
  project_id INTEGER NOT NULL REFERENCES proj_projects(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  layout     TEXT NOT NULL DEFAULT 'board' CHECK (layout IN ('board','table','roadmap')),
  filter     TEXT NOT NULL DEFAULT '',
  sort       TEXT NOT NULL DEFAULT '',
  position   INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS proj_links (
  project_id INTEGER NOT NULL REFERENCES proj_projects(id) ON DELETE CASCADE,
  repo_id    INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  PRIMARY KEY (project_id, repo_id)
);
CREATE INDEX IF NOT EXISTS proj_links_repo ON proj_links (repo_id);`)
}

// Field kinds.
const (
	ProjFieldSelect = "single_select"
	ProjFieldText   = "text"
	ProjFieldNumber = "number"
	ProjFieldDate   = "date"
)

// ProjFieldKinds lists the kinds a person can create, in menu order.
var ProjFieldKinds = []string{ProjFieldSelect, ProjFieldText, ProjFieldNumber, ProjFieldDate}

// ProjColors are the colours a single-select option may use.
var ProjColors = []string{"gray", "blue", "amber", "green", "violet", "red"}

// ErrProjInvalid is returned for a bad name, kind or value.
var ErrProjInvalid = errors.New("invalid project input")

type Project struct {
	ID          int64
	OwnerID     int64
	Owner       string
	OwnerKind   string
	Title       string
	Description string
	Private     bool
	Closed      bool
	AutoAdd     bool // add issues and pull requests opened in linked repositories
	AutoDone    bool // Status → Done when closed or merged
	AutoReopen  bool // Status → Todo when reopened
	CreatedKind string
	CreatedBy   string
	PersonID    int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Items       int // number of items (filled by list queries)
}

type ProjOption struct {
	ID       int64
	FieldID  int64
	Name     string
	Color    string
	Position int
}

type ProjField struct {
	ID        int64
	ProjectID int64
	Name      string
	Kind      string
	Builtin   string // "status" for the built-in Status field
	Position  int
	Options   []ProjOption
}

func (f ProjField) IsStatus() bool { return f.Builtin == "status" }
func (f ProjField) IsSelect() bool { return f.Kind == ProjFieldSelect }

// KindLabel names a field kind for people.
func (f ProjField) KindLabel() string { return ProjKindLabel(f.Kind) }

// ProjKindLabel names a field kind for people.
func ProjKindLabel(k string) string {
	switch k {
	case ProjFieldSelect:
		return "Single select"
	case ProjFieldText:
		return "Text"
	case ProjFieldNumber:
		return "Number"
	case ProjFieldDate:
		return "Date"
	}
	return k
}

// Option finds an option by id (as stored in a value).
func (f ProjField) Option(value string) (ProjOption, bool) {
	for _, o := range f.Options {
		if strconv.FormatInt(o.ID, 10) == value {
			return o, true
		}
	}
	return ProjOption{}, false
}

// OptionNamed finds an option by name, ignoring case.
func (f ProjField) OptionNamed(name string) (ProjOption, bool) {
	for _, o := range f.Options {
		if strings.EqualFold(o.Name, name) {
			return o, true
		}
	}
	return ProjOption{}, false
}

// ProjItem is one card / row. For issues and pull requests the title, state
// and labels come from the item itself; the caller must check the item's
// repository is readable before showing any of them.
type ProjItem struct {
	ID          int64
	ProjectID   int64
	Kind        string // issue, pull, draft
	RepoID      int64
	RepoOwnerID int64
	RepoOwner   string
	RepoOwnerK  string
	RepoName    string
	RepoPrivate bool
	Number      int64
	Title       string
	Body        string // drafts only
	State       string // open, closed, merged, draft; "" if the issue or PR is gone
	Labels      []Label
	Position    float64
	Values      map[int64]string // field id → value
	AddedKind   string
	AddedBy     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (it ProjItem) IsDraft() bool { return it.Kind == "draft" }

// Missing reports an issue or pull request that no longer exists.
func (it ProjItem) Missing() bool { return !it.IsDraft() && it.State == "" }

// Repo is the item's repository, for permission checks. Drafts have none.
func (it ProjItem) Repo() Repo {
	return Repo{ID: it.RepoID, OwnerID: it.RepoOwnerID, Owner: it.RepoOwner, OwnerKind: it.RepoOwnerK, Name: it.RepoName, Private: it.RepoPrivate}
}

// RepoFullName is "owner/name" ("" for drafts).
func (it ProjItem) RepoFullName() string {
	if it.IsDraft() {
		return ""
	}
	return it.RepoOwner + "/" + it.RepoName
}

type ProjView struct {
	ID        int64
	ProjectID int64
	Name      string
	Layout    string
	Filter    string
	Sort      string
	Position  int
	CreatedAt time.Time
}

// ProjItemRef points at an item in a project.
type ProjItemRef struct {
	ProjectID int64
	ItemID    int64
}

// ---------- projects ----------

const projSelect = `SELECT p.id, p.owner_id, o.name, o.kind, p.title, p.description, p.private, p.closed,
p.auto_add, p.auto_done, p.auto_reopen, p.created_kind, p.created_by, COALESCE(p.person_id, 0), p.created_at, p.updated_at,
(SELECT COUNT(*) FROM proj_items it WHERE it.project_id = p.id)
FROM proj_projects p JOIN owners o ON o.id = p.owner_id`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var c, u int64
	err := row.Scan(&p.ID, &p.OwnerID, &p.Owner, &p.OwnerKind, &p.Title, &p.Description, &p.Private, &p.Closed,
		&p.AutoAdd, &p.AutoDone, &p.AutoReopen, &p.CreatedKind, &p.CreatedBy, &p.PersonID, &c, &u, &p.Items)
	p.CreatedAt, p.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return p, err
}

func (s *Store) queryProjects(ctx context.Context, where string, args ...any) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, projSelect+" "+where+" ORDER BY p.closed, p.updated_at DESC, p.id DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ValidProjTitle reports whether t is a usable project, view or field name.
func ValidProjTitle(t string, max int) bool {
	n := len([]rune(strings.TrimSpace(t)))
	return n >= 1 && n <= max
}

// NewProject is what CreateProject needs.
type NewProject struct {
	OwnerID     int64
	Title       string
	Description string
	Private     bool
	ActorKind   string
	ActorName   string
	PersonID    int64
}

// DefaultStatuses are the Status options a new project starts with.
var DefaultStatuses = []ProjOption{{Name: "Todo", Color: "gray"}, {Name: "In progress", Color: "amber"}, {Name: "Done", Color: "green"}}

// CreateProject makes a project with the built-in Status field (Todo / In
// progress / Done) and a Board view.
func (s *Store) CreateProject(ctx context.Context, in NewProject) (Project, error) {
	title := strings.TrimSpace(in.Title)
	if !ValidProjTitle(title, 120) || in.OwnerID == 0 {
		return Project{}, ErrProjInvalid
	}
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		t := now()
		var person any
		if in.PersonID != 0 {
			person = in.PersonID
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO proj_projects (owner_id, title, description, private, created_kind, created_by, person_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, in.OwnerID, title, strings.TrimSpace(in.Description), in.Private, in.ActorKind, in.ActorName, person, t, t)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		res, err = tx.ExecContext(ctx, `INSERT INTO proj_fields (project_id, name, kind, builtin, position) VALUES (?, 'Status', 'single_select', 'status', 0)`, id)
		if err != nil {
			return err
		}
		fid, _ := res.LastInsertId()
		for i, o := range DefaultStatuses {
			if _, err := tx.ExecContext(ctx, `INSERT INTO proj_options (field_id, name, color, position) VALUES (?, ?, ?, ?)`, fid, o.Name, o.Color, i); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO proj_views (project_id, name, layout, position, created_at) VALUES (?, 'Board', 'board', 0, ?)`, id, t)
		return err
	})
	if err != nil {
		return Project{}, err
	}
	return s.ProjectByID(ctx, id)
}

func (s *Store) ProjectByID(ctx context.Context, id int64) (Project, error) {
	p, err := scanProject(s.db.QueryRowContext(ctx, projSelect+` WHERE p.id=?`, id))
	return p, notFound(err)
}

// Projects lists every project, open ones first, most recently updated first.
func (s *Store) Projects(ctx context.Context) ([]Project, error) { return s.queryProjects(ctx, "") }

// ProjectsByOwner lists an account's projects.
func (s *Store) ProjectsByOwner(ctx context.Context, ownerID int64) ([]Project, error) {
	return s.queryProjects(ctx, "WHERE p.owner_id=?", ownerID)
}

// UpdateProject saves title, description, visibility, closed and automation.
func (s *Store) UpdateProject(ctx context.Context, p Project) error {
	title := strings.TrimSpace(p.Title)
	if !ValidProjTitle(title, 120) {
		return ErrProjInvalid
	}
	res, err := s.db.ExecContext(ctx, `UPDATE proj_projects SET title=?, description=?, private=?, closed=?, auto_add=?, auto_done=?, auto_reopen=?, updated_at=? WHERE id=?`,
		title, strings.TrimSpace(p.Description), p.Private, p.Closed, p.AutoAdd, p.AutoDone, p.AutoReopen, now(), p.ID)
	return projExpectRow(res, err)
}

func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	return projExpectRow(s.db.ExecContext(ctx, `DELETE FROM proj_projects WHERE id=?`, id))
}

func (s *Store) touchProject(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE proj_projects SET updated_at=? WHERE id=?`, now(), id)
	return err
}

func projExpectRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------- fields ----------

// ProjectFields returns the fields in order, Status first, with their options.
func (s *Store) ProjectFields(ctx context.Context, projectID int64) ([]ProjField, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, name, kind, builtin, position FROM proj_fields
		WHERE project_id=? ORDER BY builtin <> 'status', position, id`, projectID)
	if err != nil {
		return nil, err
	}
	var out []ProjField
	idx := map[int64]int{}
	for rows.Next() {
		var f ProjField
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.Name, &f.Kind, &f.Builtin, &f.Position); err != nil {
			rows.Close()
			return nil, err
		}
		idx[f.ID] = len(out)
		out = append(out, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	orows, err := s.db.QueryContext(ctx, `SELECT o.id, o.field_id, o.name, o.color, o.position FROM proj_options o
		JOIN proj_fields f ON f.id = o.field_id WHERE f.project_id=? ORDER BY o.position, o.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer orows.Close()
	for orows.Next() {
		var o ProjOption
		if err := orows.Scan(&o.ID, &o.FieldID, &o.Name, &o.Color, &o.Position); err != nil {
			return nil, err
		}
		if i, ok := idx[o.FieldID]; ok {
			out[i].Options = append(out[i].Options, o)
		}
	}
	return out, orows.Err()
}

// ProjectField finds one field of a project.
func (s *Store) ProjectField(ctx context.Context, projectID, fieldID int64) (ProjField, error) {
	fields, err := s.ProjectFields(ctx, projectID)
	if err != nil {
		return ProjField{}, err
	}
	for _, f := range fields {
		if f.ID == fieldID {
			return f, nil
		}
	}
	return ProjField{}, ErrNotFound
}

// StatusField returns the project's built-in Status field.
func (s *Store) StatusField(ctx context.Context, projectID int64) (ProjField, error) {
	fields, err := s.ProjectFields(ctx, projectID)
	if err != nil {
		return ProjField{}, err
	}
	for _, f := range fields {
		if f.IsStatus() {
			return f, nil
		}
	}
	return ProjField{}, ErrNotFound
}

func validProjKind(k string) bool {
	for _, x := range ProjFieldKinds {
		if x == k {
			return true
		}
	}
	return false
}

// ValidProjColor reports whether c is one of ProjColors.
func ValidProjColor(c string) bool {
	for _, x := range ProjColors {
		if x == c {
			return true
		}
	}
	return false
}

// projReservedNames clash with built-in columns and filter qualifiers.
var projReservedNames = map[string]bool{"title": true, "repo": true, "repository": true, "is": true, "label": true, "labels": true, "no": true, "number": true, "state": true, "type": true}

// CreateField adds a custom field. Options apply to single selects.
func (s *Store) CreateField(ctx context.Context, projectID int64, name, kind string, options []string) (ProjField, error) {
	name = strings.TrimSpace(name)
	if !ValidProjTitle(name, 60) || !validProjKind(kind) || projReservedNames[strings.ToLower(name)] || strings.ContainsAny(name, `":`) {
		return ProjField{}, ErrProjInvalid
	}
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO proj_fields (project_id, name, kind, position)
			VALUES (?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM proj_fields WHERE project_id=?))`, projectID, name, kind, projectID)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		if kind == ProjFieldSelect {
			seen := map[string]bool{}
			pos := 0
			for _, o := range options {
				o = strings.TrimSpace(o)
				if !ValidProjTitle(o, 60) || seen[strings.ToLower(o)] {
					continue
				}
				seen[strings.ToLower(o)] = true
				if _, err := tx.ExecContext(ctx, `INSERT INTO proj_options (field_id, name, color, position) VALUES (?, ?, ?, ?)`,
					id, o, ProjColors[pos%len(ProjColors)], pos); err != nil {
					return err
				}
				pos++
			}
		}
		return s.touchProject(ctx, tx, projectID)
	})
	if err != nil {
		if projUniqueErr(err) {
			return ProjField{}, ErrProjInvalid
		}
		return ProjField{}, err
	}
	return s.ProjectField(ctx, projectID, id)
}

func projUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE")
}

// RenameField renames a custom field (Status keeps its name).
func (s *Store) RenameField(ctx context.Context, projectID, fieldID int64, name string) error {
	name = strings.TrimSpace(name)
	if !ValidProjTitle(name, 60) || projReservedNames[strings.ToLower(name)] || strings.ContainsAny(name, `":`) {
		return ErrProjInvalid
	}
	err := projExpectRow(s.db.ExecContext(ctx, `UPDATE proj_fields SET name=? WHERE id=? AND project_id=? AND builtin=''`, name, fieldID, projectID))
	if projUniqueErr(err) {
		return ErrProjInvalid
	}
	return err
}

// DeleteField removes a custom field and its values. Status can't be deleted.
func (s *Store) DeleteField(ctx context.Context, projectID, fieldID int64) error {
	return projExpectRow(s.db.ExecContext(ctx, `DELETE FROM proj_fields WHERE id=? AND project_id=? AND builtin=''`, fieldID, projectID))
}

// OptionEdit is one change to a single select's options.
type OptionEdit struct {
	ID     int64 // 0 = add a new option
	Name   string
	Color  string
	Delete bool
}

// EditOptions renames, recolours, adds and deletes options of a single
// select. Values pointing at a deleted option are cleared. Order follows
// edits (existing options not mentioned keep their place at the end).
func (s *Store) EditOptions(ctx context.Context, projectID, fieldID int64, edits []OptionEdit) error {
	f, err := s.ProjectField(ctx, projectID, fieldID)
	if err != nil {
		return err
	}
	if !f.IsSelect() {
		return ErrProjInvalid
	}
	have := map[int64]bool{}
	for _, o := range f.Options {
		have[o.ID] = true
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		pos := 0
		for _, e := range edits {
			name := strings.TrimSpace(e.Name)
			color := e.Color
			if !ValidProjColor(color) {
				color = "gray"
			}
			switch {
			case e.ID != 0 && !have[e.ID]:
				continue
			case e.ID != 0 && e.Delete:
				if _, err := tx.ExecContext(ctx, `DELETE FROM proj_values WHERE field_id=? AND value=?`, fieldID, strconv.FormatInt(e.ID, 10)); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM proj_options WHERE id=?`, e.ID); err != nil {
					return err
				}
			case e.ID != 0:
				if !ValidProjTitle(name, 60) {
					return ErrProjInvalid
				}
				if _, err := tx.ExecContext(ctx, `UPDATE proj_options SET name=?, color=?, position=? WHERE id=?`, name, color, pos, e.ID); err != nil {
					return err
				}
				pos++
			case name != "" && !e.Delete:
				if !ValidProjTitle(name, 60) {
					return ErrProjInvalid
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO proj_options (field_id, name, color, position) VALUES (?, ?, ?, ?)`, fieldID, name, color, pos); err != nil {
					return err
				}
				pos++
			}
		}
		return s.touchProject(ctx, tx, projectID)
	})
	if projUniqueErr(err) {
		return ErrProjInvalid
	}
	return err
}

// ---------- items ----------

const projItemSelect = `SELECT it.id, it.project_id, it.kind, COALESCE(it.repo_id, 0), COALESCE(r.owner_id, 0), COALESCE(o.name, ''), COALESCE(o.kind, ''),
COALESCE(r.name, ''), COALESCE(r.private, 1), it.number,
CASE it.kind WHEN 'issue' THEN COALESCE(i.title, '') WHEN 'pull' THEN COALESCE(pl.title, '') ELSE it.title END,
CASE it.kind WHEN 'draft' THEN it.body ELSE '' END,
CASE it.kind WHEN 'issue' THEN COALESCE(i.state, '') WHEN 'pull' THEN COALESCE(pl.state, '') ELSE 'draft' END,
COALESCE(i.id, 0), it.position, it.added_kind, it.added_by, it.created_at, it.updated_at
FROM proj_items it
LEFT JOIN repos r ON r.id = it.repo_id
LEFT JOIN owners o ON o.id = r.owner_id
LEFT JOIN issues i ON it.kind = 'issue' AND i.repo_id = it.repo_id AND i.number = it.number
LEFT JOIN pulls pl ON it.kind = 'pull' AND pl.repo_id = it.repo_id AND pl.number = it.number`

func (s *Store) queryItems(ctx context.Context, where string, args ...any) ([]ProjItem, error) {
	rows, err := s.db.QueryContext(ctx, projItemSelect+" "+where+" ORDER BY it.position, it.id", args...)
	if err != nil {
		return nil, err
	}
	var out []ProjItem
	issueOf := map[int64]int{}
	var issueIDs []int64
	for rows.Next() {
		var it ProjItem
		var issueID, c, u int64
		if err := rows.Scan(&it.ID, &it.ProjectID, &it.Kind, &it.RepoID, &it.RepoOwnerID, &it.RepoOwner, &it.RepoOwnerK,
			&it.RepoName, &it.RepoPrivate, &it.Number, &it.Title, &it.Body, &it.State, &issueID, &it.Position,
			&it.AddedKind, &it.AddedBy, &c, &u); err != nil {
			rows.Close()
			return nil, err
		}
		it.CreatedAt, it.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
		it.Values = map[int64]string{}
		if issueID != 0 {
			issueOf[issueID] = len(out)
			issueIDs = append(issueIDs, issueID)
		}
		out = append(out, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	labels, err := s.issueLabels(ctx, issueIDs)
	if err != nil {
		return nil, err
	}
	for iid, k := range issueOf {
		out[k].Labels = labels[iid]
	}
	byID := map[int64]int{}
	ids := make([]any, len(out))
	for k, it := range out {
		byID[it.ID] = k
		ids[k] = it.ID
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	vrows, err := s.db.QueryContext(ctx, `SELECT item_id, field_id, value FROM proj_values WHERE item_id IN (`+ph+`)`, ids...)
	if err != nil {
		return nil, err
	}
	defer vrows.Close()
	for vrows.Next() {
		var iid, fid int64
		var v string
		if err := vrows.Scan(&iid, &fid, &v); err != nil {
			return nil, err
		}
		out[byID[iid]].Values[fid] = v
	}
	return out, vrows.Err()
}

// ProjectItems returns every item of a project in board order.
func (s *Store) ProjectItems(ctx context.Context, projectID int64) ([]ProjItem, error) {
	return s.queryItems(ctx, "WHERE it.project_id=?", projectID)
}

// ProjectItem finds one item of a project.
func (s *Store) ProjectItem(ctx context.Context, projectID, itemID int64) (ProjItem, error) {
	items, err := s.queryItems(ctx, "WHERE it.project_id=? AND it.id=?", projectID, itemID)
	if err != nil {
		return ProjItem{}, err
	}
	if len(items) == 0 {
		return ProjItem{}, ErrNotFound
	}
	return items[0], nil
}

// ItemsFor lists the project items holding an issue or pull request.
func (s *Store) ItemsFor(ctx context.Context, kind string, repoID, number int64) ([]ProjItemRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, id FROM proj_items WHERE kind=? AND repo_id=? AND number=? ORDER BY project_id`, kind, repoID, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjItemRef
	for rows.Next() {
		var r ProjItemRef
		if err := rows.Scan(&r.ProjectID, &r.ItemID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func projNextPosition(ctx context.Context, tx *sql.Tx, projectID int64) (float64, error) {
	var p float64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM proj_items WHERE project_id=?`, projectID).Scan(&p)
	return p, err
}

// AddItem adds an issue or pull request to the end of a project. If it is
// already there, the existing item is returned with added=false.
func (s *Store) AddItem(ctx context.Context, projectID int64, kind string, repoID, number int64, actorKind, actorName string) (item ProjItem, added bool, err error) {
	if (kind != "issue" && kind != "pull") || repoID == 0 || number <= 0 {
		return ProjItem{}, false, ErrProjInvalid
	}
	var id int64
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT id FROM proj_items WHERE project_id=? AND kind=? AND repo_id=? AND number=?`, projectID, kind, repoID, number).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		pos, err := projNextPosition(ctx, tx, projectID)
		if err != nil {
			return err
		}
		t := now()
		res, err := tx.ExecContext(ctx, `INSERT INTO proj_items (project_id, kind, repo_id, number, position, added_kind, added_by, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, projectID, kind, repoID, number, pos, actorKind, actorName, t, t)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		added = true
		return s.touchProject(ctx, tx, projectID)
	})
	if err != nil {
		return ProjItem{}, false, err
	}
	item, err = s.ProjectItem(ctx, projectID, id)
	return item, added, err
}

// AddDraft adds a draft item (a title and a body) to the end of a project.
func (s *Store) AddDraft(ctx context.Context, projectID int64, title, body, actorKind, actorName string) (ProjItem, error) {
	title = strings.TrimSpace(title)
	if !ValidProjTitle(title, 256) {
		return ProjItem{}, ErrProjInvalid
	}
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		pos, err := projNextPosition(ctx, tx, projectID)
		if err != nil {
			return err
		}
		t := now()
		res, err := tx.ExecContext(ctx, `INSERT INTO proj_items (project_id, kind, title, body, position, added_kind, added_by, created_at, updated_at)
			VALUES (?, 'draft', ?, ?, ?, ?, ?, ?, ?)`, projectID, title, body, pos, actorKind, actorName, t, t)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		return s.touchProject(ctx, tx, projectID)
	})
	if err != nil {
		return ProjItem{}, err
	}
	return s.ProjectItem(ctx, projectID, id)
}

// UpdateDraft changes a draft's title and body.
func (s *Store) UpdateDraft(ctx context.Context, projectID, itemID int64, title, body string) error {
	title = strings.TrimSpace(title)
	if !ValidProjTitle(title, 256) {
		return ErrProjInvalid
	}
	return projExpectRow(s.db.ExecContext(ctx, `UPDATE proj_items SET title=?, body=?, updated_at=? WHERE id=? AND project_id=? AND kind='draft'`,
		title, body, now(), itemID, projectID))
}

// ConvertDraft turns a draft into the issue it became, keeping its place and
// field values.
func (s *Store) ConvertDraft(ctx context.Context, projectID, itemID, repoID, number int64) error {
	return projExpectRow(s.db.ExecContext(ctx, `UPDATE proj_items SET kind='issue', repo_id=?, number=?, title='', body='', updated_at=?
		WHERE id=? AND project_id=? AND kind='draft'`, repoID, number, now(), itemID, projectID))
}

// RemoveItem takes an item off a project (the issue or PR itself stays).
func (s *Store) RemoveItem(ctx context.Context, projectID, itemID int64) error {
	return projExpectRow(s.db.ExecContext(ctx, `DELETE FROM proj_items WHERE id=? AND project_id=?`, itemID, projectID))
}

// SetItemValue sets (or, with "", clears) one field of an item. Values are
// checked against the field kind; single selects store the option id.
func (s *Store) SetItemValue(ctx context.Context, projectID, itemID, fieldID int64, value string) error {
	f, err := s.ProjectField(ctx, projectID, fieldID)
	if err != nil {
		return err
	}
	value, err = NormalizeProjValue(f, value)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proj_items WHERE id=? AND project_id=?`, itemID, projectID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if err := projSetValueTx(ctx, tx, itemID, fieldID, value); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proj_items SET updated_at=? WHERE id=?`, now(), itemID); err != nil {
			return err
		}
		return s.touchProject(ctx, tx, projectID)
	})
}

func projSetValueTx(ctx context.Context, tx *sql.Tx, itemID, fieldID int64, value string) error {
	if value == "" {
		_, err := tx.ExecContext(ctx, `DELETE FROM proj_values WHERE item_id=? AND field_id=?`, itemID, fieldID)
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO proj_values (item_id, field_id, value) VALUES (?, ?, ?)
		ON CONFLICT(item_id, field_id) DO UPDATE SET value=excluded.value`, itemID, fieldID, value)
	return err
}

// NormalizeProjValue checks a value for a field and returns its stored form.
func NormalizeProjValue(f ProjField, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	switch f.Kind {
	case ProjFieldSelect:
		if _, ok := f.Option(value); !ok {
			if o, ok := f.OptionNamed(value); ok {
				return strconv.FormatInt(o.ID, 10), nil
			}
			return "", ErrProjInvalid
		}
	case ProjFieldText:
		if len([]rune(value)) > 1000 {
			return "", ErrProjInvalid
		}
	case ProjFieldNumber:
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || n != n {
			return "", ErrProjInvalid
		}
		return strconv.FormatFloat(n, 'f', -1, 64), nil
	case ProjFieldDate:
		d, err := time.Parse("2006-01-02", value)
		if err != nil {
			return "", ErrProjInvalid
		}
		return d.Format("2006-01-02"), nil
	}
	return value, nil
}

// MoveItem puts an item before another (beforeID) or at the end (beforeID 0),
// renumbering the project's positions. If statusField is non-zero the item's
// value for that field is set to status ("" clears it) in the same step.
func (s *Store) MoveItem(ctx context.Context, projectID, itemID, beforeID, statusField int64, status string) error {
	if statusField != 0 {
		f, err := s.ProjectField(ctx, projectID, statusField)
		if err != nil {
			return err
		}
		if status, err = NormalizeProjValue(f, status); err != nil {
			return err
		}
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM proj_items WHERE project_id=? ORDER BY position, id`, projectID)
		if err != nil {
			return err
		}
		var order []int64
		found := false
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			if id == itemID {
				found = true
				continue
			}
			order = append(order, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !found {
			return ErrNotFound
		}
		at := len(order)
		if beforeID != 0 && beforeID != itemID {
			for k, id := range order {
				if id == beforeID {
					at = k
					break
				}
			}
		}
		order = append(order[:at], append([]int64{itemID}, order[at:]...)...)
		for k, id := range order {
			if _, err := tx.ExecContext(ctx, `UPDATE proj_items SET position=? WHERE id=?`, float64(k+1), id); err != nil {
				return err
			}
		}
		if statusField != 0 {
			if err := projSetValueTx(ctx, tx, itemID, statusField, status); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE proj_items SET updated_at=? WHERE id=?`, now(), itemID); err != nil {
			return err
		}
		return s.touchProject(ctx, tx, projectID)
	})
}

// ---------- views ----------

func (s *Store) ProjectViews(ctx context.Context, projectID int64) ([]ProjView, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, project_id, name, layout, filter, sort, position, created_at FROM proj_views
		WHERE project_id=? ORDER BY position, id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjView
	for rows.Next() {
		var v ProjView
		var c int64
		if err := rows.Scan(&v.ID, &v.ProjectID, &v.Name, &v.Layout, &v.Filter, &v.Sort, &v.Position, &c); err != nil {
			return nil, err
		}
		v.CreatedAt = time.Unix(c, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ValidProjLayout reports whether l is board, table or roadmap.
func ValidProjLayout(l string) bool { return l == "board" || l == "table" || l == "roadmap" }

// SaveView creates a view, or updates it when v.ID is set.
func (s *Store) SaveView(ctx context.Context, v ProjView) (ProjView, error) {
	v.Name = strings.TrimSpace(v.Name)
	if !ValidProjTitle(v.Name, 60) || !ValidProjLayout(v.Layout) || len(v.Filter) > 500 || len(v.Sort) > 60 {
		return ProjView{}, ErrProjInvalid
	}
	if v.ID != 0 {
		err := projExpectRow(s.db.ExecContext(ctx, `UPDATE proj_views SET name=?, layout=?, filter=?, sort=? WHERE id=? AND project_id=?`,
			v.Name, v.Layout, v.Filter, v.Sort, v.ID, v.ProjectID))
		return v, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO proj_views (project_id, name, layout, filter, sort, position, created_at)
		VALUES (?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM proj_views WHERE project_id=?), ?)`,
		v.ProjectID, v.Name, v.Layout, v.Filter, v.Sort, v.ProjectID, now())
	if err != nil {
		return ProjView{}, err
	}
	v.ID, _ = res.LastInsertId()
	return v, nil
}

func (s *Store) DeleteView(ctx context.Context, projectID, viewID int64) error {
	return projExpectRow(s.db.ExecContext(ctx, `DELETE FROM proj_views WHERE id=? AND project_id=?`, viewID, projectID))
}

// ---------- linked repositories (automation) ----------

func (s *Store) LinkProjectRepo(ctx context.Context, projectID, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO proj_links (project_id, repo_id) VALUES (?, ?)`, projectID, repoID)
	return err
}

func (s *Store) UnlinkProjectRepo(ctx context.Context, projectID, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM proj_links WHERE project_id=? AND repo_id=?`, projectID, repoID)
	return err
}

// ProjectLinks returns the repository ids linked to a project.
func (s *Store) ProjectLinks(ctx context.Context, projectID int64) ([]int64, error) {
	return s.projIDs(ctx, `SELECT repo_id FROM proj_links WHERE project_id=? ORDER BY repo_id`, projectID)
}

// ProjectsLinkedTo returns the ids of projects linked to a repository.
func (s *Store) ProjectsLinkedTo(ctx context.Context, repoID int64) ([]int64, error) {
	return s.projIDs(ctx, `SELECT project_id FROM proj_links WHERE repo_id=? ORDER BY project_id`, repoID)
}

func (s *Store) projIDs(ctx context.Context, q string, args ...any) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, rows.Err()
}
