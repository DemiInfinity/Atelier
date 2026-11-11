package store

import (
	"context"
	"time"
)

// Snippet comments: plain Markdown comments under a snippet, like gist
// comments. Who may read or write them is decided in the web package.

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS snippet_comments (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  snippet_id  INTEGER NOT NULL REFERENCES snippets(id) ON DELETE CASCADE,
  author_kind TEXT NOT NULL,
  author_name TEXT NOT NULL,
  person_id   INTEGER REFERENCES owners(id) ON DELETE SET NULL,
  body        TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS snippet_comments_snippet ON snippet_comments(snippet_id, id);`)
}

// SnippetComment is one comment on a snippet.
type SnippetComment struct {
	ID         int64
	SnippetID  int64
	AuthorKind string
	AuthorName string
	PersonID   int64
	Body       string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (c SnippetComment) Edited() bool { return c.UpdatedAt.After(c.CreatedAt) }

const snippetCommentCols = `id, snippet_id, author_kind, author_name, COALESCE(person_id, 0), body, created_at, updated_at`

func scanSnippetComment(row interface{ Scan(...any) error }) (SnippetComment, error) {
	var c SnippetComment
	var created, updated int64
	err := row.Scan(&c.ID, &c.SnippetID, &c.AuthorKind, &c.AuthorName, &c.PersonID, &c.Body, &created, &updated)
	c.CreatedAt, c.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return c, err
}

// AddSnippetComment adds a comment. personID may be 0 (a token or bot).
func (s *Store) AddSnippetComment(ctx context.Context, snippetID int64, body, authorKind, authorName string, personID int64) (SnippetComment, error) {
	var person any
	if personID != 0 {
		person = personID
	}
	t := now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO snippet_comments (snippet_id, author_kind, author_name, person_id, body, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, snippetID, authorKind, authorName, person, body, t, t)
	if err != nil {
		return SnippetComment{}, err
	}
	id, _ := res.LastInsertId()
	return s.SnippetCommentByID(ctx, id)
}

// SnippetComments lists a snippet's comments, oldest first.
func (s *Store) SnippetComments(ctx context.Context, snippetID int64) ([]SnippetComment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+snippetCommentCols+` FROM snippet_comments WHERE snippet_id=? ORDER BY id`, snippetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnippetComment
	for rows.Next() {
		c, err := scanSnippetComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SnippetCommentByID(ctx context.Context, id int64) (SnippetComment, error) {
	c, err := scanSnippetComment(s.db.QueryRowContext(ctx, `SELECT `+snippetCommentCols+` FROM snippet_comments WHERE id=?`, id))
	return c, notFound(err)
}

func (s *Store) UpdateSnippetComment(ctx context.Context, id int64, body string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE snippet_comments SET body=?, updated_at=? WHERE id=?`, body, now(), id)
	return err
}

func (s *Store) DeleteSnippetComment(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM snippet_comments WHERE id=?`, id)
	return err
}
