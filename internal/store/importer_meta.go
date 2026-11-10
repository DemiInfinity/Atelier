package store

// Issue, pull request, label and milestone imports.
//
// import_links remembers which source item became which Atelier row, so an
// import that is resumed or run again never duplicates anything.
// import_job_meta keeps a job's metadata options and per-stage progress next
// to the import_jobs row (which belongs to the plain git import).

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func init() {
	RegisterSchema(`CREATE TABLE IF NOT EXISTS import_links (
  repo_id       INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  kind          TEXT NOT NULL,              -- label, milestone, issue, pull, issue_comment, pull_comment
  source_key    TEXT NOT NULL,              -- number, comment id or lower-cased name at the source
  target_id     INTEGER NOT NULL,           -- the Atelier row
  number        INTEGER NOT NULL DEFAULT 0, -- issue / pull request number (kept from the source)
  source_url    TEXT NOT NULL DEFAULT '',
  source_author TEXT NOT NULL DEFAULT '',   -- e.g. github:alice
  complete      INTEGER NOT NULL DEFAULT 0, -- 1 once comments etc. are all in
  created_at    INTEGER NOT NULL,
  PRIMARY KEY (repo_id, kind, source_key)
);
CREATE TABLE IF NOT EXISTS import_job_meta (
  job_id       INTEGER PRIMARY KEY REFERENCES import_jobs(id) ON DELETE CASCADE,
  repo_id      INTEGER NOT NULL DEFAULT 0,  -- set once the repository exists (or up front when resuming)
  person_id    INTEGER NOT NULL DEFAULT 0,  -- who started it
  source_repo  TEXT NOT NULL DEFAULT '',    -- owner/name at the source
  api_base     TEXT NOT NULL DEFAULT '',
  options      TEXT NOT NULL DEFAULT '',    -- e.g. "labels milestones issues pulls"
  resume       INTEGER NOT NULL DEFAULT 0,  -- 1: the code is already here, only fetch metadata
  stages       TEXT NOT NULL DEFAULT '',    -- JSON progress per stage
  summary      TEXT NOT NULL DEFAULT '',
  resume_after INTEGER,                     -- when a rate limit resets
  updated_at   INTEGER NOT NULL
);`)
}

// ---------- links ----------

// ImportLink ties a source item to the Atelier row it became.
type ImportLink struct {
	RepoID       int64
	Kind         string
	SourceKey    string
	TargetID     int64
	Number       int64
	SourceURL    string
	SourceAuthor string
	Complete     bool
}

// ImportLinkFor returns the link for one source item, or ErrNotFound.
func (s *Store) ImportLinkFor(ctx context.Context, repoID int64, kind, key string) (ImportLink, error) {
	l := ImportLink{RepoID: repoID, Kind: kind, SourceKey: key}
	var complete int
	err := s.db.QueryRowContext(ctx, `SELECT target_id, number, source_url, source_author, complete FROM import_links
		WHERE repo_id=? AND kind=? AND source_key=?`, repoID, kind, key).
		Scan(&l.TargetID, &l.Number, &l.SourceURL, &l.SourceAuthor, &complete)
	l.Complete = complete != 0
	return l, notFound(err)
}

// SaveImportLink records (or updates) a link.
func (s *Store) SaveImportLink(ctx context.Context, l ImportLink) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO import_links
		(repo_id, kind, source_key, target_id, number, source_url, source_author, complete, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, kind, source_key) DO UPDATE SET target_id=excluded.target_id, number=excluded.number,
		source_url=excluded.source_url, source_author=excluded.source_author, complete=excluded.complete`,
		l.RepoID, l.Kind, l.SourceKey, l.TargetID, l.Number, l.SourceURL, l.SourceAuthor, l.Complete, now())
	return err
}

// ImportLinkCounts returns how many items of each kind were imported into a repo.
func (s *Store) ImportLinkCounts(ctx context.Context, repoID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM import_links WHERE repo_id=? GROUP BY kind`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// NumberOwner says what already uses #number in a repo: "issue", "pull" or ""
// (free), with the row id and its author kind.
func (s *Store) NumberOwner(ctx context.Context, repoID, number int64) (kind string, id int64, authorKind string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT 'issue', id, author_kind FROM issues WHERE repo_id=? AND number=?
		UNION ALL SELECT 'pull', id, author_kind FROM pulls WHERE repo_id=? AND number=? LIMIT 1`,
		repoID, number, repoID, number).Scan(&kind, &id, &authorKind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, "", nil
	}
	return kind, id, authorKind, err
}

// BumpRepoCounter makes sure the repo's next #number is at least next, so new
// issues and pull requests continue after imported ones. It never lowers it.
func (s *Store) BumpRepoCounter(ctx context.Context, repoID, next int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO repo_counters (repo_id, next) VALUES (?, ?)
		ON CONFLICT(repo_id) DO UPDATE SET next = MAX(next, excluded.next)`, repoID, next)
	return err
}

// RepoCounter returns the next #number a repo will hand out (1 if none yet).
func (s *Store) RepoCounter(ctx context.Context, repoID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT next FROM repo_counters WHERE repo_id=?`, repoID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 1, nil
	}
	return n, err
}

// ---------- job metadata ----------

// ImportJobMeta is the metadata part of an import job.
type ImportJobMeta struct {
	JobID       int64
	RepoID      int64
	PersonID    int64
	SourceRepo  string
	APIBase     string
	Options     string
	Resume      bool
	Stages      string // JSON, owned by the importer
	Summary     string
	ResumeAfter *time.Time
	UpdatedAt   time.Time
}

func (s *Store) SaveImportJobMeta(ctx context.Context, m ImportJobMeta) error {
	var after any
	if m.ResumeAfter != nil {
		after = m.ResumeAfter.Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO import_job_meta
		(job_id, repo_id, person_id, source_repo, api_base, options, resume, stages, summary, resume_after, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET repo_id=excluded.repo_id, person_id=excluded.person_id,
		source_repo=excluded.source_repo, api_base=excluded.api_base, options=excluded.options, resume=excluded.resume,
		stages=excluded.stages, summary=excluded.summary, resume_after=excluded.resume_after, updated_at=excluded.updated_at`,
		m.JobID, m.RepoID, m.PersonID, m.SourceRepo, m.APIBase, m.Options, m.Resume, m.Stages, m.Summary, after, now())
	return err
}

// SetImportJobProgress updates a job's stage JSON (and repo id once known).
func (s *Store) SetImportJobProgress(ctx context.Context, jobID, repoID int64, stages string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE import_job_meta SET repo_id=CASE WHEN ?>0 THEN ? ELSE repo_id END,
		stages=?, updated_at=? WHERE job_id=?`, repoID, repoID, stages, now(), jobID)
	return err
}

// FinishImportJobMeta records the summary and, for a partial import, when it can resume.
func (s *Store) FinishImportJobMeta(ctx context.Context, jobID int64, stages, summary string, resumeAfter *time.Time) error {
	var after any
	if resumeAfter != nil {
		after = resumeAfter.Unix()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE import_job_meta SET stages=?, summary=?, resume_after=?, updated_at=? WHERE job_id=?`,
		stages, summary, after, now(), jobID)
	return err
}

func (s *Store) ImportJobMeta(ctx context.Context, jobID int64) (ImportJobMeta, error) {
	ms, err := s.importJobMetas(ctx, `WHERE job_id=?`, jobID)
	if err != nil {
		return ImportJobMeta{}, err
	}
	if len(ms) == 0 {
		return ImportJobMeta{}, ErrNotFound
	}
	return ms[0], nil
}

// ImportJobMetas returns the metadata of the given jobs, by job id.
func (s *Store) ImportJobMetas(ctx context.Context, jobIDs []int64) (map[int64]ImportJobMeta, error) {
	out := map[int64]ImportJobMeta{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	ph := "?"
	args := []any{jobIDs[0]}
	for _, id := range jobIDs[1:] {
		ph += ",?"
		args = append(args, id)
	}
	ms, err := s.importJobMetas(ctx, `WHERE job_id IN (`+ph+`)`, args...)
	for _, m := range ms {
		out[m.JobID] = m
	}
	return out, err
}

func (s *Store) importJobMetas(ctx context.Context, where string, args ...any) ([]ImportJobMeta, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, repo_id, person_id, source_repo, api_base, options, resume, stages,
		summary, resume_after, updated_at FROM import_job_meta `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ImportJobMeta
	for rows.Next() {
		var m ImportJobMeta
		var resume int
		var after sql.NullInt64
		var up int64
		if err := rows.Scan(&m.JobID, &m.RepoID, &m.PersonID, &m.SourceRepo, &m.APIBase, &m.Options, &resume, &m.Stages,
			&m.Summary, &after, &up); err != nil {
			return nil, err
		}
		m.Resume = resume != 0
		m.ResumeAfter = unixPtr(after)
		m.UpdatedAt = time.Unix(up, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ImportedRepo finds a repository by id (named for the importer to avoid clashing with core).
func (s *Store) ImportedRepo(ctx context.Context, id int64) (Repo, error) {
	rows, err := s.db.QueryContext(ctx, repoSelect+` WHERE r.id=?`, id)
	if err != nil {
		return Repo{}, err
	}
	repos, err := scanRepos(rows)
	if err != nil {
		return Repo{}, err
	}
	if len(repos) == 0 {
		return Repo{}, ErrNotFound
	}
	return repos[0], nil
}
