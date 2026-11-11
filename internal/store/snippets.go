package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Snippets: small versioned files, like GitHub gists. Each snippet's content
// lives in its own bare git repository under <data>/snippets/; this table holds
// what the UI needs to list and search them without opening git: the owner
// account, visibility, description and a cache of the file names at the tip.

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS snippets (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  uid         TEXT NOT NULL UNIQUE,
  owner_id    INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  description TEXT NOT NULL DEFAULT '',
  private     INTEGER NOT NULL DEFAULT 1,
  files       TEXT NOT NULL DEFAULT '',
  actor_kind  TEXT NOT NULL DEFAULT '',
  actor_name  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS snippets_owner ON snippets(owner_id, updated_at);
CREATE TABLE IF NOT EXISTS snippet_stars (
  person_id  INTEGER NOT NULL,
  snippet_id INTEGER NOT NULL REFERENCES snippets(id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (person_id, snippet_id)
);`)
}

// Snippet is one snippet's metadata.
type Snippet struct {
	ID          int64
	UID         string // the id in URLs and on disk
	OwnerID     int64
	Owner       string
	OwnerKind   string
	Description string
	Private     bool
	Files       []string // file names at the tip, in tree order
	ActorKind   string
	ActorName   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Title is the first file name, or the id for an empty snippet.
func (s Snippet) Title() string {
	if len(s.Files) > 0 {
		return s.Files[0]
	}
	return s.UID
}

const snippetSelect = `SELECT s.id, s.uid, s.owner_id, o.name, o.kind, s.description, s.private, s.files,
	s.actor_kind, s.actor_name, s.created_at, s.updated_at
FROM snippets s JOIN owners o ON o.id = s.owner_id`

func scanSnippets(rows *sql.Rows) ([]Snippet, error) {
	defer rows.Close()
	var out []Snippet
	for rows.Next() {
		var sn Snippet
		var priv int
		var files string
		var c, u int64
		if err := rows.Scan(&sn.ID, &sn.UID, &sn.OwnerID, &sn.Owner, &sn.OwnerKind, &sn.Description, &priv, &files,
			&sn.ActorKind, &sn.ActorName, &c, &u); err != nil {
			return nil, err
		}
		sn.Private = priv != 0
		if files != "" {
			sn.Files = strings.Split(files, "\n")
		}
		sn.CreatedAt, sn.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
		out = append(out, sn)
	}
	return out, rows.Err()
}

func joinFiles(files []string) string { return strings.Join(files, "\n") }

// CreateSnippet records a new snippet. The caller creates its git repository.
func (s *Store) CreateSnippet(ctx context.Context, uid string, ownerID int64, desc string, private bool,
	files []string, actorKind, actorName string) (Snippet, error) {
	t := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO snippets (uid, owner_id, description, private, files, actor_kind,
		actor_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uid, ownerID, desc, private, joinFiles(files), actorKind, actorName, t, t)
	if err != nil {
		return Snippet{}, err
	}
	return s.SnippetByUID(ctx, uid)
}

// SnippetByUID loads one snippet.
func (s *Store) SnippetByUID(ctx context.Context, uid string) (Snippet, error) {
	rows, err := s.db.QueryContext(ctx, snippetSelect+` WHERE s.uid=?`, strings.ToLower(uid))
	if err != nil {
		return Snippet{}, err
	}
	out, err := scanSnippets(rows)
	if err != nil {
		return Snippet{}, err
	}
	if len(out) == 0 {
		return Snippet{}, ErrNotFound
	}
	return out[0], nil
}

// UpdateSnippet changes the description and visibility.
func (s *Store) UpdateSnippet(ctx context.Context, id int64, desc string, private bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE snippets SET description=?, private=?, updated_at=? WHERE id=?`,
		desc, private, now(), id)
	return err
}

// SetSnippetFiles refreshes the cached file names (after a web edit or a push).
func (s *Store) SetSnippetFiles(ctx context.Context, id int64, files []string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE snippets SET files=?, updated_at=? WHERE id=?`, joinFiles(files), now(), id)
	return err
}

// DeleteSnippet removes a snippet's row and its stars.
func (s *Store) DeleteSnippet(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM snippet_stars WHERE snippet_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM snippets WHERE id=?`, id)
	return err
}

// Snippets lists every snippet, newest change first. Callers filter by access.
// q, when set, keeps snippets whose file names or description contain every
// word (case-insensitive).
func (s *Store) Snippets(ctx context.Context, q string, limit int) ([]Snippet, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	where, args := snippetWhere(q)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, snippetSelect+where+` ORDER BY s.updated_at DESC, s.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return scanSnippets(rows)
}

// StarredSnippets lists the snippets a person starred, newest star first.
func (s *Store) StarredSnippets(ctx context.Context, personID int64, q string) ([]Snippet, error) {
	where, args := snippetWhere(q)
	if where == "" {
		where = " WHERE "
	} else {
		where += " AND "
	}
	where += `s.id IN (SELECT snippet_id FROM snippet_stars WHERE person_id=?)`
	args = append(args, personID)
	rows, err := s.db.QueryContext(ctx, snippetSelect+where+` ORDER BY s.updated_at DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	return scanSnippets(rows)
}

func snippetWhere(q string) (string, []any) {
	words := strings.Fields(q)
	if len(words) > 8 {
		words = words[:8]
	}
	var conds []string
	var args []any
	for _, w := range words {
		conds = append(conds, `(s.files LIKE ? ESCAPE '\' OR s.description LIKE ? ESCAPE '\')`)
		args = append(args, likeEscape(w), likeEscape(w))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// SetSnippetStar stars or unstars a snippet for a person.
func (s *Store) SetSnippetStar(ctx context.Context, personID, snippetID int64, on bool) error {
	if on {
		_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO snippet_stars (person_id, snippet_id, created_at)
			VALUES (?, ?, ?)`, personID, snippetID, now())
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM snippet_stars WHERE person_id=? AND snippet_id=?`, personID, snippetID)
	return err
}

// SnippetStarred reports whether a person starred a snippet.
func (s *Store) SnippetStarred(ctx context.Context, personID, snippetID int64) bool {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snippet_stars WHERE person_id=? AND snippet_id=?`,
		personID, snippetID).Scan(&n)
	return n > 0
}
