package store

import (
	"context"
	"database/sql"
)

// Pull request labels use the repository's issue labels, as on GitHub, where a
// pull request is also an issue. They are set through /api/v3 (bots label pull
// requests) and appear in pull request payloads.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS pull_labels (
  pull_id  INTEGER NOT NULL REFERENCES pulls(id) ON DELETE CASCADE,
  label_id INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
  PRIMARY KEY (pull_id, label_id)
);
CREATE INDEX IF NOT EXISTS pull_labels_label ON pull_labels (label_id);`)
}

// PullLabels returns a pull request's labels, by name.
func (s *Store) PullLabels(ctx context.Context, pullID int64) ([]Label, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.id, l.repo_id, l.name, l.color, l.description
		FROM pull_labels pl JOIN labels l ON l.id = pl.label_id WHERE pl.pull_id = ? ORDER BY l.name`, pullID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Label
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.ID, &l.RepoID, &l.Name, &l.Color, &l.Description); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetPullLabels replaces a pull request's labels. Label IDs from another
// repository are ignored.
func (s *Store) SetPullLabels(ctx context.Context, pullID int64, labelIDs []int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var repoID int64
		if err := tx.QueryRowContext(ctx, `SELECT repo_id FROM pulls WHERE id=?`, pullID).Scan(&repoID); err != nil {
			return notFound(err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pull_labels WHERE pull_id=?`, pullID); err != nil {
			return err
		}
		for _, id := range labelIDs {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pull_labels (pull_id, label_id)
				SELECT ?, id FROM labels WHERE id=? AND repo_id=?`, pullID, id, repoID); err != nil {
				return err
			}
		}
		return nil
	})
}
