package store

// Tether: what was last written to both sides of a synced issue, so the next
// sync can tell which side changed (and never bounces a change back), plus
// GitHub pull request reviews already shown in Atelier.
//
// tether_issue_sync holds, per linked item, the state both sides agreed on
// after the last sync: a hash of the title and body, the label names and
// milestone title (lower case), and the open/closed state. A side whose
// current value differs from it changed since; when both did, the later
// edit wins. For pull requests only gh_updated_at is used (to fetch reviews
// only when the pull request changed on GitHub).

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS tether_issue_sync (
  repo_id          INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  number           INTEGER NOT NULL,
  text_hash        TEXT NOT NULL DEFAULT '',
  labels           TEXT NOT NULL DEFAULT '',
  milestone        TEXT NOT NULL DEFAULT '',
  state            TEXT NOT NULL DEFAULT '',
  gh_updated_at    INTEGER NOT NULL DEFAULT 0,
  local_updated_at INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (repo_id, number)
);
CREATE TABLE IF NOT EXISTS tether_reviews (
  repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  gh_id   INTEGER NOT NULL,
  number  INTEGER NOT NULL,
  PRIMARY KEY (repo_id, gh_id)
);
CREATE TABLE IF NOT EXISTS tether_link_sync (
  repo_id          INTEGER PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
  issues_out_after INTEGER NOT NULL
);`)
}

// TetherIssueState is what both sides of a synced item agreed on last.
type TetherIssueState struct {
	RepoID         int64
	Number         int64 // Atelier number
	TextHash       string
	Labels         []string // lower case, sorted
	Milestone      string   // lower case title, "" = none
	State          string   // open or closed
	GHUpdatedAt    time.Time
	LocalUpdatedAt time.Time
}

// TetherIssueSync returns an item's last agreed state (ErrNotFound if it was never synced this way).
func (s *Store) TetherIssueSync(ctx context.Context, repoID, number int64) (TetherIssueState, error) {
	var st TetherIssueState
	var labels string
	var gu, lu int64
	err := s.db.QueryRowContext(ctx, `SELECT repo_id, number, text_hash, labels, milestone, state, gh_updated_at, local_updated_at
		FROM tether_issue_sync WHERE repo_id=? AND number=?`, repoID, number).
		Scan(&st.RepoID, &st.Number, &st.TextHash, &labels, &st.Milestone, &st.State, &gu, &lu)
	if err != nil {
		return st, notFound(err)
	}
	if labels != "" {
		st.Labels = strings.Split(labels, "\n")
	}
	if gu > 0 {
		st.GHUpdatedAt = time.Unix(gu, 0)
	}
	if lu > 0 {
		st.LocalUpdatedAt = time.Unix(lu, 0)
	}
	return st, nil
}

// SaveTetherIssueSync records an item's agreed state.
func (s *Store) SaveTetherIssueSync(ctx context.Context, st TetherIssueState) error {
	labels := append([]string(nil), st.Labels...)
	sort.Strings(labels)
	var gu, lu int64
	if !st.GHUpdatedAt.IsZero() {
		gu = st.GHUpdatedAt.Unix()
	}
	if !st.LocalUpdatedAt.IsZero() {
		lu = st.LocalUpdatedAt.Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tether_issue_sync (repo_id, number, text_hash, labels, milestone, state,
		gh_updated_at, local_updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, number) DO UPDATE SET text_hash=excluded.text_hash, labels=excluded.labels,
		milestone=excluded.milestone, state=excluded.state, gh_updated_at=excluded.gh_updated_at,
		local_updated_at=excluded.local_updated_at`,
		st.RepoID, st.Number, st.TextHash, strings.Join(labels, "\n"), st.Milestone, st.State, gu, lu)
	return err
}

// TetherIssuesChangedLocally lists the Atelier numbers of synced issues that
// changed in Atelier since their last sync (or were never synced this way).
func (s *Store) TetherIssuesChangedLocally(ctx context.Context, repoID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.number FROM tether_items t
		JOIN issues i ON i.repo_id = t.repo_id AND i.number = t.number
		LEFT JOIN tether_issue_sync s ON s.repo_id = t.repo_id AND s.number = t.number
		WHERE t.repo_id=? AND t.kind='issue' AND (s.number IS NULL OR i.updated_at > s.local_updated_at)
		ORDER BY t.number`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// TetherUnsyncedIssues lists Atelier issues numbered above after that
// aren't on GitHub yet (imported issues are left alone).
func (s *Store) TetherUnsyncedIssues(ctx context.Context, repoID, after int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.number FROM issues i WHERE i.repo_id=? AND i.number > ?
		AND i.author_kind <> 'import'
		AND NOT EXISTS (SELECT 1 FROM tether_items t WHERE t.repo_id = i.repo_id AND t.number = i.number)
		ORDER BY i.number`, repoID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// TetherIssuesOutAfter is the watermark for pushing Atelier issues to
// GitHub: only issues numbered above it are pushed. The first call sets it
// to the repository's highest issue or pull request number, so issues
// written before issue pushing started (or before the link) stay in Atelier.
func (s *Store) TetherIssuesOutAfter(ctx context.Context, repoID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT issues_out_after FROM tether_link_sync WHERE repo_id=?`, repoID).Scan(&n)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO tether_link_sync (repo_id, issues_out_after) VALUES (?,
		MAX((SELECT COALESCE(MAX(number), 0) FROM issues WHERE repo_id=?), (SELECT COALESCE(MAX(number), 0) FROM pulls WHERE repo_id=?)))`,
		repoID, repoID, repoID); err != nil {
		return 0, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT issues_out_after FROM tether_link_sync WHERE repo_id=?`, repoID).Scan(&n)
	return n, err
}

// MarkTetherReview records that a GitHub review is shown on Atelier pull request number.
// It reports false if it already was.
func (s *Store) MarkTetherReview(ctx context.Context, repoID, ghID, number int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO tether_reviews (repo_id, gh_id, number) VALUES (?, ?, ?)`, repoID, ghID, number)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UnmarkTetherReview forgets a review (when showing it failed).
func (s *Store) UnmarkTetherReview(ctx context.Context, repoID, ghID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tether_reviews WHERE repo_id=? AND gh_id=?`, repoID, ghID)
	return err
}
