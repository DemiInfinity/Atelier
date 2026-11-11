package store

import "context"

// UseSecretBypasses consumes one unused bypass for each fingerprint, all or
// nothing, in a single transaction. ok is false (and nothing is used) if any
// fingerprint has no unused bypass left, e.g. because a concurrent push took
// it. Duplicate fingerprints count once.
func (s *Store) UseSecretBypasses(ctx context.Context, repoID int64, fingerprints []string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	t := now()
	seen := map[string]bool{}
	for _, fp := range fingerprints {
		if seen[fp] {
			continue
		}
		seen[fp] = true
		res, err := tx.ExecContext(ctx, `UPDATE secret_bypasses SET used_at=? WHERE id=(SELECT id FROM secret_bypasses
			WHERE repo_id=? AND fingerprint=? AND used_at=0 AND expires_at>? ORDER BY id LIMIT 1)`, t, repoID, fp, t)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return false, nil
		}
	}
	return true, tx.Commit()
}
