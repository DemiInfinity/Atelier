package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Iteration fields (issue #12): a project field whose values are named
// iterations (sprints), each with a start date and a length in days, like
// GitHub Projects' iteration field.
//
// proj_fields.kind has a CHECK constraint listing the original four kinds, and
// SQLite can't change a CHECK without rebuilding the table (which would
// cascade-delete every option and value). So an iteration field is stored as
// a proj_fields row of kind 'text' plus a row in proj_iteration_fields, and
// ProjectFields reports it as kind "iteration". Values hold the iteration id,
// like single selects hold an option id.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS proj_iteration_fields (
  field_id      INTEGER PRIMARY KEY REFERENCES proj_fields(id) ON DELETE CASCADE,
  duration_days INTEGER NOT NULL DEFAULT 14 CHECK (duration_days BETWEEN 1 AND 365)
);

CREATE TABLE IF NOT EXISTS proj_iterations (
  id            INTEGER PRIMARY KEY,
  field_id      INTEGER NOT NULL REFERENCES proj_fields(id) ON DELETE CASCADE,
  name          TEXT NOT NULL COLLATE NOCASE,
  start_date    TEXT NOT NULL,
  duration_days INTEGER NOT NULL CHECK (duration_days BETWEEN 1 AND 365),
  position      INTEGER NOT NULL DEFAULT 0,
  UNIQUE (field_id, name)
);
CREATE INDEX IF NOT EXISTS proj_iterations_field ON proj_iterations (field_id, start_date);`)
}

// ProjFieldIteration is the iteration field kind.
const ProjFieldIteration = "iteration"

// ProjIteration is one iteration of an iteration field.
type ProjIteration struct {
	ID       int64
	FieldID  int64
	Name     string
	Start    time.Time // a date (UTC midnight)
	Days     int
	Position int
}

const projDate = "2006-01-02"

// End is the first day after the iteration.
func (it ProjIteration) End() time.Time { return it.Start.AddDate(0, 0, it.Days) }

// LastDay is the iteration's final day.
func (it ProjIteration) LastDay() time.Time { return it.Start.AddDate(0, 0, it.Days-1) }

// StartDate is the start as YYYY-MM-DD.
func (it ProjIteration) StartDate() string { return it.Start.Format(projDate) }

// EndDate is the last day as YYYY-MM-DD.
func (it ProjIteration) EndDate() string { return it.LastDay().Format(projDate) }

// Contains reports whether the day of t falls inside the iteration.
func (it ProjIteration) Contains(t time.Time) bool {
	d := projDay(t)
	return !d.Before(it.Start) && d.Before(it.End())
}

// Current reports whether the iteration is running today.
func (it ProjIteration) Current() bool { return it.Contains(time.Now()) }

// Range is a short date range for people, e.g. "3 Nov – 16 Nov 2026".
func (it ProjIteration) Range() string {
	last := it.LastDay()
	if it.Start.Year() == last.Year() {
		return it.Start.Format("2 Jan") + " – " + last.Format("2 Jan 2006")
	}
	return it.Start.Format("2 Jan 2006") + " – " + last.Format("2 Jan 2006")
}

// projDay is the calendar day of t, as UTC midnight (iteration dates are days,
// not instants).
func projDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func (f ProjField) IsIteration() bool { return f.Kind == ProjFieldIteration }

// Iteration finds an iteration by id (as stored in a value).
func (f ProjField) Iteration(value string) (ProjIteration, bool) {
	for _, it := range f.Iterations {
		if strconv.FormatInt(it.ID, 10) == value {
			return it, true
		}
	}
	return ProjIteration{}, false
}

// IterationNamed finds an iteration by name, ignoring case.
func (f ProjField) IterationNamed(name string) (ProjIteration, bool) {
	for _, it := range f.Iterations {
		if strings.EqualFold(it.Name, name) {
			return it, true
		}
	}
	return ProjIteration{}, false
}

// IterationAt resolves @current, @next and @previous for the day of now.
// @next is the first iteration starting after now; @previous the last one
// ending on or before it.
func (f ProjField) IterationAt(ref string, now time.Time) (ProjIteration, bool) {
	day := projDay(now)
	switch strings.ToLower(ref) {
	case "@current":
		for _, it := range f.Iterations {
			if it.Contains(day) {
				return it, true
			}
		}
	case "@next":
		var best ProjIteration
		found := false
		for _, it := range f.Iterations {
			if it.Start.After(day) && (!found || it.Start.Before(best.Start)) {
				best, found = it, true
			}
		}
		return best, found
	case "@previous":
		var best ProjIteration
		found := false
		for _, it := range f.Iterations {
			if !it.End().After(day) && (!found || it.Start.After(best.Start)) {
				best, found = it, true
			}
		}
		return best, found
	}
	return ProjIteration{}, false
}

// loadIterations fills in iteration kinds, durations and iterations for the
// fields of one project (out is indexed by field id via idx).
func (s *Store) loadIterations(ctx context.Context, projectID int64, out []ProjField, idx map[int64]int) error {
	rows, err := s.db.QueryContext(ctx, `SELECT x.field_id, x.duration_days FROM proj_iteration_fields x
		JOIN proj_fields f ON f.id = x.field_id WHERE f.project_id=?`, projectID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var days int
		if err := rows.Scan(&id, &days); err != nil {
			rows.Close()
			return err
		}
		if i, ok := idx[id]; ok {
			out[i].Kind = ProjFieldIteration
			out[i].Duration = days
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	irows, err := s.db.QueryContext(ctx, `SELECT i.id, i.field_id, i.name, i.start_date, i.duration_days, i.position FROM proj_iterations i
		JOIN proj_fields f ON f.id = i.field_id WHERE f.project_id=? ORDER BY i.start_date, i.position, i.id`, projectID)
	if err != nil {
		return err
	}
	defer irows.Close()
	for irows.Next() {
		var it ProjIteration
		var start string
		if err := irows.Scan(&it.ID, &it.FieldID, &it.Name, &start, &it.Days, &it.Position); err != nil {
			return err
		}
		t, err := time.Parse(projDate, start)
		if err != nil {
			continue
		}
		it.Start = t
		if i, ok := idx[it.FieldID]; ok && out[i].IsIteration() {
			out[i].Iterations = append(out[i].Iterations, it)
		}
	}
	return irows.Err()
}

// ValidIterationDays reports whether n is an allowed iteration length.
func ValidIterationDays(n int) bool { return n >= 1 && n <= 365 }

// ParseIterationLine reads one line of a new iteration field's list:
// "Sprint 1" or "Sprint 1 2026-11-02" (a trailing start date).
func ParseIterationLine(line string) (name string, start time.Time, hasStart bool) {
	line = strings.TrimSpace(line)
	if i := strings.LastIndexAny(line, " \t"); i > 0 {
		if t, err := time.Parse(projDate, strings.TrimSpace(line[i+1:])); err == nil {
			return strings.TrimSpace(line[:i]), t, true
		}
	}
	return line, time.Time{}, false
}

// NewIterationField describes an iteration field to create.
type NewIterationField struct {
	Name  string
	Days  int       // default length of each iteration (14 when 0)
	Start time.Time // the first iteration's start (today when zero)
	// Lines name the first iterations ("Sprint 1", optionally followed by a
	// YYYY-MM-DD start); without dates they follow one another. With no lines,
	// three iterations named "Iteration 1"–"Iteration 3" are created.
	Lines []string
}

// CreateIterationField adds an iteration field and its first iterations.
func (s *Store) CreateIterationField(ctx context.Context, projectID int64, in NewIterationField) (ProjField, error) {
	name := strings.TrimSpace(in.Name)
	if !ValidProjTitle(name, 60) || projReservedNames[strings.ToLower(name)] || strings.ContainsAny(name, `":`) {
		return ProjField{}, ErrProjInvalid
	}
	days := in.Days
	if days == 0 {
		days = 14
	}
	if !ValidIterationDays(days) {
		return ProjField{}, ErrProjInvalid
	}
	start := in.Start
	if start.IsZero() {
		start = time.Now()
	}
	start = projDay(start)
	lines := in.Lines
	if len(lines) == 0 {
		lines = []string{"Iteration 1", "Iteration 2", "Iteration 3"}
	}
	if len(lines) > 100 {
		return ProjField{}, ErrProjInvalid
	}
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO proj_fields (project_id, name, kind, position)
			VALUES (?, ?, 'text', (SELECT COALESCE(MAX(position), 0) + 1 FROM proj_fields WHERE project_id=?))`, projectID, name, projectID)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `INSERT INTO proj_iteration_fields (field_id, duration_days) VALUES (?, ?)`, id, days); err != nil {
			return err
		}
		next := start
		seen := map[string]bool{}
		for pos, line := range lines {
			n, st, ok := ParseIterationLine(line)
			if n == "" {
				continue
			}
			if !ValidProjTitle(n, 60) || seen[strings.ToLower(n)] {
				return ErrProjInvalid
			}
			seen[strings.ToLower(n)] = true
			if ok {
				next = st
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO proj_iterations (field_id, name, start_date, duration_days, position) VALUES (?, ?, ?, ?, ?)`,
				id, n, next.Format(projDate), days, pos); err != nil {
				return err
			}
			next = next.AddDate(0, 0, days)
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

// IterationEdit is one change to an iteration field's iterations.
type IterationEdit struct {
	ID     int64  // 0 = add a new iteration
	Name   string // "" for a new one: "Iteration N"
	Start  string // YYYY-MM-DD; "" for a new one: right after the last iteration
	Days   int    // 0: the field's default length
	Delete bool
}

// EditIterations renames, re-dates, adds and deletes iterations, and sets the
// field's default length (days > 0). Values pointing at a deleted iteration
// are cleared.
func (s *Store) EditIterations(ctx context.Context, projectID, fieldID int64, days int, edits []IterationEdit) error {
	f, err := s.ProjectField(ctx, projectID, fieldID)
	if err != nil {
		return err
	}
	if !f.IsIteration() {
		return ErrProjInvalid
	}
	if days == 0 {
		days = f.Duration
	}
	if !ValidIterationDays(days) {
		return ErrProjInvalid
	}
	have := map[int64]ProjIteration{}
	var last time.Time
	for _, it := range f.Iterations {
		have[it.ID] = it
		if it.End().After(last) {
			last = it.End()
		}
	}
	if last.IsZero() {
		last = projDay(time.Now())
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE proj_iteration_fields SET duration_days=? WHERE field_id=?`, days, fieldID); err != nil {
			return err
		}
		// New iterations without a name are numbered after the ones kept.
		count := len(f.Iterations)
		taken := map[string]bool{}
		for _, it := range f.Iterations {
			taken[strings.ToLower(it.Name)] = true
		}
		for _, e := range edits {
			if e.ID != 0 && e.Delete {
				if it, ok := have[e.ID]; ok {
					count--
					delete(taken, strings.ToLower(it.Name))
				}
			}
		}
		for pos, e := range edits {
			name := strings.TrimSpace(e.Name)
			length := e.Days
			if length == 0 {
				length = days
			}
			if !ValidIterationDays(length) {
				return ErrProjInvalid
			}
			switch {
			case e.ID != 0:
				cur, ok := have[e.ID]
				if !ok {
					continue
				}
				if e.Delete {
					if _, err := tx.ExecContext(ctx, `DELETE FROM proj_values WHERE field_id=? AND value=?`, fieldID, strconv.FormatInt(e.ID, 10)); err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `DELETE FROM proj_iterations WHERE id=?`, e.ID); err != nil {
						return err
					}
					continue
				}
				if name == "" {
					name = cur.Name
				}
				start := cur.Start
				if e.Start != "" {
					t, err := time.Parse(projDate, strings.TrimSpace(e.Start))
					if err != nil {
						return ErrProjInvalid
					}
					start = t
				}
				if e.Days == 0 {
					length = cur.Days
				}
				if !ValidProjTitle(name, 60) {
					return ErrProjInvalid
				}
				if _, err := tx.ExecContext(ctx, `UPDATE proj_iterations SET name=?, start_date=?, duration_days=?, position=? WHERE id=?`,
					name, start.Format(projDate), length, pos, e.ID); err != nil {
					return err
				}
			case !e.Delete:
				count++
				for name == "" {
					name = fmt.Sprintf("Iteration %d", count)
					if taken[strings.ToLower(name)] {
						name, count = "", count+1
					}
				}
				taken[strings.ToLower(name)] = true
				start := last
				if e.Start != "" {
					t, err := time.Parse(projDate, strings.TrimSpace(e.Start))
					if err != nil {
						return ErrProjInvalid
					}
					start = t
				}
				if !ValidProjTitle(name, 60) {
					return ErrProjInvalid
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO proj_iterations (field_id, name, start_date, duration_days, position) VALUES (?, ?, ?, ?, ?)`,
					fieldID, name, start.Format(projDate), length, pos); err != nil {
					return err
				}
				last = start.AddDate(0, 0, length)
			}
		}
		return s.touchProject(ctx, tx, projectID)
	})
	if projUniqueErr(err) {
		return ErrProjInvalid
	}
	return err
}

// normalizeIteration turns an iteration id, name or @current / @next /
// @previous into the stored id.
func normalizeIteration(f ProjField, value string) (string, error) {
	if _, ok := f.Iteration(value); ok {
		return value, nil
	}
	if strings.HasPrefix(value, "@") {
		if it, ok := f.IterationAt(value, time.Now()); ok {
			return strconv.FormatInt(it.ID, 10), nil
		}
		return "", ErrProjInvalid
	}
	if it, ok := f.IterationNamed(value); ok {
		return strconv.FormatInt(it.ID, 10), nil
	}
	return "", ErrProjInvalid
}
