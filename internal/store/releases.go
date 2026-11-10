package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Releases and their downloadable assets. Asset bytes live on disk,
// content-addressed by sha256 under <data>/releases/ (see web/releases_blobs.go);
// the database only records names, sizes and checksums.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS releases (
  id            INTEGER PRIMARY KEY,
  repo_id       INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  tag           TEXT NOT NULL,
  target        TEXT NOT NULL DEFAULT '',
  commit_sha    TEXT NOT NULL DEFAULT '',
  title         TEXT NOT NULL DEFAULT '',
  body          TEXT NOT NULL DEFAULT '',
  draft         INTEGER NOT NULL DEFAULT 0,
  prerelease    INTEGER NOT NULL DEFAULT 0,
  author_kind   TEXT NOT NULL DEFAULT 'owner',
  author_name   TEXT NOT NULL DEFAULT '',
  author_person INTEGER,
  built_by      TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  published_at  INTEGER,
  UNIQUE (repo_id, tag)
);
CREATE INDEX IF NOT EXISTS releases_repo ON releases (repo_id, draft, published_at);

CREATE TABLE IF NOT EXISTS release_assets (
  id              INTEGER PRIMARY KEY,
  release_id      INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
  name            TEXT NOT NULL,
  size            INTEGER NOT NULL,
  content_type    TEXT NOT NULL DEFAULT 'application/octet-stream',
  sha256          TEXT NOT NULL,
  downloads       INTEGER NOT NULL DEFAULT 0,
  uploader_kind   TEXT NOT NULL DEFAULT 'owner',
  uploader_name   TEXT NOT NULL DEFAULT '',
  uploader_person INTEGER,
  created_at      INTEGER NOT NULL,
  UNIQUE (release_id, name)
);
CREATE INDEX IF NOT EXISTS release_assets_sha ON release_assets (sha256);`)
}

// ErrReleaseExists is returned when a release for the tag already exists.
var ErrReleaseExists = errors.New("a release for that tag already exists")

// ErrAssetExists is returned when the release already has an asset with that name.
var ErrAssetExists = errors.New("an asset with that name already exists")

type Release struct {
	ID           int64
	RepoID       int64
	Tag          string
	Target       string // branch or commit the tag was (or will be) made from
	CommitSHA    string // commit the tag points at, once it exists
	Title        string
	Body         string
	Draft        bool
	Prerelease   bool
	AuthorKind   string
	AuthorName   string
	AuthorPerson int64
	BuiltBy      string // e.g. the runner an Actions job ran on
	CreatedAt    time.Time
	PublishedAt  *time.Time
	Assets       []ReleaseAsset // filled by the *WithAssets helpers
}

// Name is the title, or the tag when the release has no title.
func (r Release) Name() string {
	if strings.TrimSpace(r.Title) != "" {
		return r.Title
	}
	return r.Tag
}

// When is the publish time, or the creation time for drafts.
func (r Release) When() time.Time {
	if r.PublishedAt != nil {
		return *r.PublishedAt
	}
	return r.CreatedAt
}

type ReleaseAsset struct {
	ID             int64
	ReleaseID      int64
	Name           string
	Size           int64
	ContentType    string
	SHA256         string
	Downloads      int64
	UploaderKind   string
	UploaderName   string
	UploaderPerson int64
	CreatedAt      time.Time
}

// NewRelease is what CreateRelease stores.
type NewRelease struct {
	RepoID       int64
	Tag          string
	Target       string
	CommitSHA    string
	Title        string
	Body         string
	Draft        bool
	Prerelease   bool
	AuthorKind   string
	AuthorName   string
	AuthorPerson int64
	BuiltBy      string
}

const releaseCols = `id, repo_id, tag, target, commit_sha, title, body, draft, prerelease, author_kind, author_name,
COALESCE(author_person, 0), built_by, created_at, published_at`

func scanRelease(row interface{ Scan(...any) error }) (Release, error) {
	var r Release
	var created int64
	var pub sql.NullInt64
	err := row.Scan(&r.ID, &r.RepoID, &r.Tag, &r.Target, &r.CommitSHA, &r.Title, &r.Body, &r.Draft, &r.Prerelease,
		&r.AuthorKind, &r.AuthorName, &r.AuthorPerson, &r.BuiltBy, &created, &pub)
	r.CreatedAt = time.Unix(created, 0)
	if pub.Valid {
		t := time.Unix(pub.Int64, 0)
		r.PublishedAt = &t
	}
	return r, err
}

func relNullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func relUniqueErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// CreateRelease stores a release. A non-draft is published now.
func (s *Store) CreateRelease(ctx context.Context, in NewRelease) (Release, error) {
	t := now()
	var pub any
	if !in.Draft {
		pub = t
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO releases (repo_id, tag, target, commit_sha, title, body, draft, prerelease,
		author_kind, author_name, author_person, built_by, created_at, published_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.RepoID, in.Tag, in.Target, in.CommitSHA, in.Title, in.Body, in.Draft, in.Prerelease,
		in.AuthorKind, in.AuthorName, relNullID(in.AuthorPerson), in.BuiltBy, t, pub)
	if relUniqueErr(err) {
		return Release{}, ErrReleaseExists
	}
	if err != nil {
		return Release{}, err
	}
	id, _ := res.LastInsertId()
	return s.ReleaseByID(ctx, in.RepoID, id)
}

// UpdateRelease saves edits. Publishing a draft (Draft going false) sets
// published_at; it is never cleared again.
func (s *Store) UpdateRelease(ctx context.Context, r Release) error {
	var pub any
	if !r.Draft {
		pub = now()
		if r.PublishedAt != nil {
			pub = r.PublishedAt.Unix()
		}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE releases SET tag=?, target=?, commit_sha=?, title=?, body=?, draft=?, prerelease=?,
		built_by=?, published_at=? WHERE id=? AND repo_id=?`,
		r.Tag, r.Target, r.CommitSHA, r.Title, r.Body, r.Draft, r.Prerelease, r.BuiltBy, pub, r.ID, r.RepoID)
	if relUniqueErr(err) {
		return ErrReleaseExists
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRelease removes a release and its asset rows, returning the asset
// checksums it held so the caller can remove unreferenced files.
func (s *Store) DeleteRelease(ctx context.Context, repoID, id int64) ([]string, error) {
	assets, err := s.ReleaseAssets(ctx, id)
	if err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM releases WHERE id=? AND repo_id=?`, id, repoID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	var shas []string
	for _, a := range assets {
		shas = append(shas, a.SHA256)
	}
	return shas, nil
}

func (s *Store) ReleaseByID(ctx context.Context, repoID, id int64) (Release, error) {
	r, err := scanRelease(s.db.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM releases WHERE id=? AND repo_id=?`, id, repoID))
	return r, notFound(err)
}

func (s *Store) ReleaseByTag(ctx context.Context, repoID int64, tag string) (Release, error) {
	r, err := scanRelease(s.db.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM releases WHERE repo_id=? AND tag=?`, repoID, tag))
	return r, notFound(err)
}

// LatestRelease is the newest published release that isn't a prerelease.
func (s *Store) LatestRelease(ctx context.Context, repoID int64) (Release, error) {
	r, err := scanRelease(s.db.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM releases
		WHERE repo_id=? AND draft=0 AND prerelease=0 ORDER BY published_at DESC, id DESC LIMIT 1`, repoID))
	return r, notFound(err)
}

// Releases lists a repository's releases: drafts first (when included), then
// published ones newest first.
func (s *Store) Releases(ctx context.Context, repoID int64, includeDrafts bool) ([]Release, error) {
	q := `SELECT ` + releaseCols + ` FROM releases WHERE repo_id=?`
	if !includeDrafts {
		q += ` AND draft=0`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY draft DESC, COALESCE(published_at, created_at) DESC, id DESC`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReleaseCount counts published releases.
func (s *Store) ReleaseCount(ctx context.Context, repoID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM releases WHERE repo_id=? AND draft=0`, repoID).Scan(&n)
	return n, err
}

// ---------- assets ----------

const assetCols = `id, release_id, name, size, content_type, sha256, downloads, uploader_kind, uploader_name,
COALESCE(uploader_person, 0), created_at`

func scanAsset(row interface{ Scan(...any) error }) (ReleaseAsset, error) {
	var a ReleaseAsset
	var created int64
	err := row.Scan(&a.ID, &a.ReleaseID, &a.Name, &a.Size, &a.ContentType, &a.SHA256, &a.Downloads,
		&a.UploaderKind, &a.UploaderName, &a.UploaderPerson, &created)
	a.CreatedAt = time.Unix(created, 0)
	return a, err
}

// AddReleaseAsset records an uploaded file.
func (s *Store) AddReleaseAsset(ctx context.Context, a ReleaseAsset) (ReleaseAsset, error) {
	if a.ContentType == "" {
		a.ContentType = "application/octet-stream"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO release_assets (release_id, name, size, content_type, sha256,
		uploader_kind, uploader_name, uploader_person, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		a.ReleaseID, a.Name, a.Size, a.ContentType, a.SHA256, a.UploaderKind, a.UploaderName, relNullID(a.UploaderPerson), now())
	if relUniqueErr(err) {
		return ReleaseAsset{}, ErrAssetExists
	}
	if err != nil {
		return ReleaseAsset{}, err
	}
	id, _ := res.LastInsertId()
	return s.ReleaseAssetByID(ctx, a.ReleaseID, id)
}

func (s *Store) ReleaseAssetByID(ctx context.Context, releaseID, id int64) (ReleaseAsset, error) {
	a, err := scanAsset(s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM release_assets WHERE id=? AND release_id=?`, id, releaseID))
	return a, notFound(err)
}

func (s *Store) ReleaseAssetByName(ctx context.Context, releaseID int64, name string) (ReleaseAsset, error) {
	a, err := scanAsset(s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM release_assets WHERE release_id=? AND name=?`, releaseID, name))
	return a, notFound(err)
}

// ReleaseAssetInRepo finds an asset by id, making sure it belongs to the repository.
func (s *Store) ReleaseAssetInRepo(ctx context.Context, repoID, id int64) (ReleaseAsset, error) {
	a, err := scanAsset(s.db.QueryRowContext(ctx, `SELECT `+assetCols+` FROM release_assets
		WHERE id=? AND release_id IN (SELECT id FROM releases WHERE repo_id=?)`, id, repoID))
	return a, notFound(err)
}

func (s *Store) ReleaseAssets(ctx context.Context, releaseID int64) ([]ReleaseAsset, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+assetCols+` FROM release_assets WHERE release_id=? ORDER BY name`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseAsset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteReleaseAsset removes an asset row.
func (s *Store) DeleteReleaseAsset(ctx context.Context, releaseID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM release_assets WHERE id=? AND release_id=?`, id, releaseID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountReleaseDownload adds one to an asset's download count.
func (s *Store) CountReleaseDownload(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE release_assets SET downloads = downloads + 1 WHERE id=?`, id)
	return err
}

// ReleaseBlobInUse reports whether any asset still refers to the checksum.
func (s *Store) ReleaseBlobInUse(ctx context.Context, sha string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_assets WHERE sha256=?`, sha).Scan(&n)
	return n > 0, err
}
