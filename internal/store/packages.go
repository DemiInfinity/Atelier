package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Packages
//
// A package belongs to an owner account and has a type (npm, container, go,
// generic). Each version has files; every file points at a content-addressed
// blob (internal/packages). Tags name versions: npm dist-tags and container
// tags. Container blobs uploaded to a repository but not yet used by a
// manifest are tracked in package_links so pulls can check they belong there.

func init() {
	RegisterSchema(`
CREATE TABLE IF NOT EXISTS packages (
  id          INTEGER PRIMARY KEY,
  owner_id    INTEGER NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  type        TEXT NOT NULL CHECK (type IN ('npm','container','go','generic')),
  name        TEXT NOT NULL COLLATE NOCASE,
  private     INTEGER NOT NULL DEFAULT 1,
  repo_id     INTEGER REFERENCES repos(id) ON DELETE SET NULL,
  description TEXT NOT NULL DEFAULT '',
  downloads   INTEGER NOT NULL DEFAULT 0,
  actor_kind  TEXT NOT NULL DEFAULT '',
  actor_name  TEXT NOT NULL DEFAULT '',
  person_id   INTEGER,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  UNIQUE (owner_id, type, name)
);
CREATE TABLE IF NOT EXISTS package_versions (
  id          INTEGER PRIMARY KEY,
  package_id  INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
  version     TEXT NOT NULL,
  metadata    TEXT NOT NULL DEFAULT '',
  media_type  TEXT NOT NULL DEFAULT '',
  size        INTEGER NOT NULL DEFAULT 0,
  downloads   INTEGER NOT NULL DEFAULT 0,
  deprecated  TEXT NOT NULL DEFAULT '',
  actor_kind  TEXT NOT NULL DEFAULT '',
  actor_name  TEXT NOT NULL DEFAULT '',
  person_id   INTEGER,
  created_at  INTEGER NOT NULL,
  UNIQUE (package_id, version)
);
CREATE TABLE IF NOT EXISTS package_files (
  id          INTEGER PRIMARY KEY,
  version_id  INTEGER NOT NULL REFERENCES package_versions(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  digest      TEXT NOT NULL,
  size        INTEGER NOT NULL,
  media_type  TEXT NOT NULL DEFAULT '',
  downloads   INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  UNIQUE (version_id, name)
);
CREATE INDEX IF NOT EXISTS package_files_digest ON package_files(digest);
CREATE TABLE IF NOT EXISTS package_tags (
  package_id  INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
  tag         TEXT NOT NULL,
  version_id  INTEGER NOT NULL REFERENCES package_versions(id) ON DELETE CASCADE,
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (package_id, tag)
);
CREATE TABLE IF NOT EXISTS package_links (
  package_id  INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
  digest      TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (package_id, digest)
);
CREATE TABLE IF NOT EXISTS package_uploads (
  id          TEXT PRIMARY KEY,
  package_id  INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
  person_id   INTEGER,
  created_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS package_npm_cache (
  name        TEXT PRIMARY KEY,
  body        BLOB NOT NULL,
  fetched_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS package_npm_cache_files (
  name        TEXT NOT NULL,
  file        TEXT NOT NULL,
  digest      TEXT NOT NULL,
  size        INTEGER NOT NULL,
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (name, file)
);
`)
}

// Package types.
const (
	PkgNpm       = "npm"
	PkgContainer = "container"
	PkgGo        = "go"
	PkgGeneric   = "generic"
)

// PackageTypes lists the types in display order.
var PackageTypes = []string{PkgNpm, PkgContainer, PkgGo, PkgGeneric}

// PkgActor is who did something to a package.
type PkgActor struct {
	Kind, Name string
	PersonID   int64
}

type Package struct {
	ID          int64
	OwnerID     int64
	Owner       string
	OwnerKind   string
	Type        string
	Name        string
	Private     bool
	RepoID      int64
	Repo        string // "owner/name" of the linked repository, if any
	Description string
	Downloads   int64
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Summary fields filled by listings.
	Latest   string
	Versions int
	Size     int64
}

type PackageVersion struct {
	ID         int64
	PackageID  int64
	Version    string
	Metadata   string
	MediaType  string
	Size       int64
	Downloads  int64
	Deprecated string
	ActorKind  string
	ActorName  string
	CreatedAt  time.Time
	Tags       []string
}

type PackageFile struct {
	ID        int64
	VersionID int64
	Name      string
	Digest    string
	Size      int64
	MediaType string
	Downloads int64
	CreatedAt time.Time
}

const pkgSelect = `SELECT p.id, p.owner_id, o.name, o.kind, p.type, p.name, p.private, COALESCE(p.repo_id, 0),
  COALESCE((SELECT ro.name || '/' || r.name FROM repos r JOIN owners ro ON ro.id = r.owner_id WHERE r.id = p.repo_id), ''),
  p.description, p.downloads, p.actor_name, p.created_at, p.updated_at,
  COALESCE((SELECT v.version FROM package_tags t JOIN package_versions v ON v.id = t.version_id WHERE t.package_id = p.id AND t.tag = 'latest'),
           (SELECT v.version FROM package_versions v WHERE v.package_id = p.id ORDER BY v.created_at DESC, v.id DESC LIMIT 1), ''),
  (SELECT COUNT(*) FROM package_versions v WHERE v.package_id = p.id),
  COALESCE((SELECT SUM(v.size) FROM package_versions v WHERE v.package_id = p.id), 0)
FROM packages p JOIN owners o ON o.id = p.owner_id`

func scanPackage(row interface{ Scan(...any) error }) (Package, error) {
	var p Package
	var priv int
	var c, u int64
	err := row.Scan(&p.ID, &p.OwnerID, &p.Owner, &p.OwnerKind, &p.Type, &p.Name, &priv, &p.RepoID, &p.Repo,
		&p.Description, &p.Downloads, &p.CreatedBy, &c, &u, &p.Latest, &p.Versions, &p.Size)
	p.Private = priv != 0
	p.CreatedAt, p.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return p, err
}

func (s *Store) scanPackages(ctx context.Context, q string, args ...any) ([]Package, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Package
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Package finds one package.
func (s *Store) Package(ctx context.Context, ownerID int64, typ, name string) (Package, error) {
	p, err := scanPackage(s.db.QueryRowContext(ctx, pkgSelect+` WHERE p.owner_id=? AND p.type=? AND p.name=?`, ownerID, typ, name))
	return p, notFound(err)
}

// PackageByID finds a package by id.
func (s *Store) PackageByID(ctx context.Context, id int64) (Package, error) {
	p, err := scanPackage(s.db.QueryRowContext(ctx, pkgSelect+` WHERE p.id=?`, id))
	return p, notFound(err)
}

// Packages lists every package, most recently updated first. Callers filter
// by access.
func (s *Store) Packages(ctx context.Context) ([]Package, error) {
	return s.scanPackages(ctx, pkgSelect+` ORDER BY p.updated_at DESC, p.id DESC`)
}

// PackagesByType lists packages of one type for an owner (ownerID 0 = all).
func (s *Store) PackagesByType(ctx context.Context, ownerID int64, typ string) ([]Package, error) {
	if ownerID == 0 {
		return s.scanPackages(ctx, pkgSelect+` WHERE p.type=? ORDER BY p.name`, typ)
	}
	return s.scanPackages(ctx, pkgSelect+` WHERE p.owner_id=? AND p.type=? ORDER BY p.name`, ownerID, typ)
}

// EnsurePackage returns the package, creating it (private unless public is
// set) if it doesn't exist. created reports whether it was new.
func (s *Store) EnsurePackage(ctx context.Context, ownerID int64, typ, name string, public bool, who PkgActor) (Package, bool, error) {
	if p, err := s.Package(ctx, ownerID, typ, name); err == nil {
		return p, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Package{}, false, err
	}
	t := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO packages (owner_id, type, name, private, actor_kind, actor_name, person_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (owner_id, type, name) DO NOTHING`,
		ownerID, typ, name, !public, who.Kind, who.Name, nullID(who.PersonID), t, t)
	if err != nil {
		return Package{}, false, err
	}
	p, err := s.Package(ctx, ownerID, typ, name)
	return p, err == nil, err
}

// UpdatePackage changes visibility, description and linked repository (0 = none).
func (s *Store) UpdatePackage(ctx context.Context, id int64, private bool, desc string, repoID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE packages SET private=?, description=?, repo_id=?, updated_at=? WHERE id=?`,
		private, desc, nullID(repoID), now(), id)
	return err
}

// SetPackageDescription updates just the description.
func (s *Store) SetPackageDescription(ctx context.Context, id int64, desc string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE packages SET description=? WHERE id=?`, desc, id)
	return err
}

// TouchPackage marks a package as updated.
func (s *Store) TouchPackage(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE packages SET updated_at=? WHERE id=?`, now(), id)
	return err
}

// DeletePackage removes a package with its versions, files and tags. Blobs
// are left for garbage collection.
func (s *Store) DeletePackage(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM packages WHERE id=?`, id)
	return err
}

// ---------- versions ----------

const versionSelect = `SELECT id, package_id, version, metadata, media_type, size, downloads, deprecated, actor_kind, actor_name, created_at FROM package_versions`

func scanVersion(row interface{ Scan(...any) error }) (PackageVersion, error) {
	var v PackageVersion
	var c int64
	err := row.Scan(&v.ID, &v.PackageID, &v.Version, &v.Metadata, &v.MediaType, &v.Size, &v.Downloads, &v.Deprecated,
		&v.ActorKind, &v.ActorName, &c)
	v.CreatedAt = time.Unix(c, 0)
	return v, err
}

// NewVersion is a version with its files, created in one transaction.
type NewVersion struct {
	Version   string
	Metadata  string
	MediaType string
	Size      int64
	Files     []PackageFile
	Tags      []string // tags to point at this version
	Replace   bool     // replace an existing version with the same name (containers)
}

// ErrExists is returned when a version or file already exists.
var ErrExists = errors.New("already exists")

// AddVersion stores a version, its files and tags.
func (s *Store) AddVersion(ctx context.Context, pkgID int64, nv NewVersion, who PkgActor) (PackageVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PackageVersion{}, err
	}
	defer tx.Rollback()
	t := now()
	var vid int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM package_versions WHERE package_id=? AND version=?`, pkgID, nv.Version).Scan(&vid)
	switch {
	case err == nil && !nv.Replace:
		return PackageVersion{}, ErrExists
	case err == nil:
		if _, err := tx.ExecContext(ctx, `UPDATE package_versions SET metadata=?, media_type=?, size=? WHERE id=?`,
			nv.Metadata, nv.MediaType, nv.Size, vid); err != nil {
			return PackageVersion{}, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM package_files WHERE version_id=?`, vid); err != nil {
			return PackageVersion{}, err
		}
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx, `INSERT INTO package_versions (package_id, version, metadata, media_type, size, actor_kind, actor_name, person_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, pkgID, nv.Version, nv.Metadata, nv.MediaType, nv.Size, who.Kind, who.Name, nullID(who.PersonID), t)
		if err != nil {
			return PackageVersion{}, err
		}
		vid, _ = res.LastInsertId()
	default:
		return PackageVersion{}, err
	}
	for _, f := range nv.Files {
		if _, err := tx.ExecContext(ctx, `INSERT INTO package_files (version_id, name, digest, size, media_type, created_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (version_id, name) DO NOTHING`, vid, f.Name, f.Digest, f.Size, f.MediaType, t); err != nil {
			return PackageVersion{}, err
		}
	}
	for _, tag := range nv.Tags {
		if err := setTag(ctx, tx, pkgID, tag, vid); err != nil {
			return PackageVersion{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE packages SET updated_at=? WHERE id=?`, t, pkgID); err != nil {
		return PackageVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return PackageVersion{}, err
	}
	return s.Version(ctx, pkgID, nv.Version)
}

// AddFile adds a file to an existing version (generic packages).
func (s *Store) AddFile(ctx context.Context, versionID int64, f PackageFile) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO package_files (version_id, name, digest, size, media_type, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (version_id, name) DO NOTHING`, versionID, f.Name, f.Digest, f.Size, f.MediaType, now())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExists
	}
	_, err = s.db.ExecContext(ctx, `UPDATE package_versions SET size = (SELECT COALESCE(SUM(size),0) FROM package_files WHERE version_id=?) WHERE id=?`, versionID, versionID)
	return err
}

// DeleteFile removes one file from a version.
func (s *Store) DeleteFile(ctx context.Context, versionID int64, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM package_files WHERE version_id=? AND name=?`, versionID, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `UPDATE package_versions SET size = (SELECT COALESCE(SUM(size),0) FROM package_files WHERE version_id=?) WHERE id=?`, versionID, versionID)
	return err
}

// Version finds one version by name.
func (s *Store) Version(ctx context.Context, pkgID int64, version string) (PackageVersion, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx, versionSelect+` WHERE package_id=? AND version=?`, pkgID, version))
	if err != nil {
		return v, notFound(err)
	}
	v.Tags, _ = s.versionTags(ctx, v.ID)
	return v, nil
}

func (s *Store) versionTags(ctx context.Context, vid int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tag FROM package_tags WHERE version_id=? ORDER BY tag`, vid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Versions lists a package's versions, newest first, with their tags.
func (s *Store) Versions(ctx context.Context, pkgID int64) ([]PackageVersion, error) {
	rows, err := s.db.QueryContext(ctx, versionSelect+` WHERE package_id=? ORDER BY created_at DESC, id DESC`, pkgID)
	if err != nil {
		return nil, err
	}
	var out []PackageVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	tags, err := s.Tags(ctx, pkgID)
	if err != nil {
		return nil, err
	}
	byID := map[int64]int{}
	for i, v := range out {
		byID[v.ID] = i
	}
	for tag, vid := range tags {
		if i, ok := byID[vid]; ok {
			out[i].Tags = append(out[i].Tags, tag)
		}
	}
	for i := range out {
		sortStrings(out[i].Tags)
	}
	return out, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// DeleteVersion removes a version (its files and tags go with it).
func (s *Store) DeleteVersion(ctx context.Context, pkgID int64, version string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM package_versions WHERE package_id=? AND version=?`, pkgID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `UPDATE packages SET updated_at=? WHERE id=?`, now(), pkgID)
	return err
}

// SetDeprecated sets (or clears, with "") a version's deprecation message.
func (s *Store) SetDeprecated(ctx context.Context, versionID int64, msg string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE package_versions SET deprecated=? WHERE id=?`, msg, versionID)
	return err
}

// Files lists a version's files by name.
func (s *Store) Files(ctx context.Context, versionID int64) ([]PackageFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, version_id, name, digest, size, media_type, downloads, created_at FROM package_files WHERE version_id=? ORDER BY name`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PackageFile
	for rows.Next() {
		var f PackageFile
		var c int64
		if err := rows.Scan(&f.ID, &f.VersionID, &f.Name, &f.Digest, &f.Size, &f.MediaType, &f.Downloads, &c); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(c, 0)
		out = append(out, f)
	}
	return out, rows.Err()
}

// File finds one file of a version.
func (s *Store) File(ctx context.Context, versionID int64, name string) (PackageFile, error) {
	var f PackageFile
	var c int64
	err := s.db.QueryRowContext(ctx, `SELECT id, version_id, name, digest, size, media_type, downloads, created_at FROM package_files WHERE version_id=? AND name=?`,
		versionID, name).Scan(&f.ID, &f.VersionID, &f.Name, &f.Digest, &f.Size, &f.MediaType, &f.Downloads, &c)
	f.CreatedAt = time.Unix(c, 0)
	return f, notFound(err)
}

// CountDownload adds one download to a package, a version and optionally a file.
func (s *Store) CountDownload(ctx context.Context, pkgID, versionID, fileID int64) {
	_, _ = s.db.ExecContext(ctx, `UPDATE packages SET downloads = downloads + 1 WHERE id=?`, pkgID)
	if versionID != 0 {
		_, _ = s.db.ExecContext(ctx, `UPDATE package_versions SET downloads = downloads + 1 WHERE id=?`, versionID)
	}
	if fileID != 0 {
		_, _ = s.db.ExecContext(ctx, `UPDATE package_files SET downloads = downloads + 1 WHERE id=?`, fileID)
	}
}

// ---------- tags ----------

func setTag(ctx context.Context, tx *sql.Tx, pkgID int64, tag string, vid int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO package_tags (package_id, tag, version_id, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (package_id, tag) DO UPDATE SET version_id=excluded.version_id, updated_at=excluded.updated_at`, pkgID, tag, vid, now())
	return err
}

// SetTag points a tag at a version.
func (s *Store) SetTag(ctx context.Context, pkgID int64, tag string, versionID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setTag(ctx, tx, pkgID, tag, versionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE packages SET updated_at=? WHERE id=?`, now(), pkgID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteTag removes a tag.
func (s *Store) DeleteTag(ctx context.Context, pkgID int64, tag string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM package_tags WHERE package_id=? AND tag=?`, pkgID, tag)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Tags maps each tag of a package to its version id.
func (s *Store) Tags(ctx context.Context, pkgID int64) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tag, version_id FROM package_tags WHERE package_id=?`, pkgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var t string
		var v int64
		if err := rows.Scan(&t, &v); err != nil {
			return nil, err
		}
		out[t] = v
	}
	return out, rows.Err()
}

// TagNames lists a package's tags in order.
func (s *Store) TagNames(ctx context.Context, pkgID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tag FROM package_tags WHERE package_id=? ORDER BY tag`, pkgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TagVersion resolves a tag to its version.
func (s *Store) TagVersion(ctx context.Context, pkgID int64, tag string) (PackageVersion, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx, `SELECT v.id, v.package_id, v.version, v.metadata, v.media_type, v.size, v.downloads, v.deprecated,
		v.actor_kind, v.actor_name, v.created_at FROM package_tags t JOIN package_versions v ON v.id = t.version_id WHERE t.package_id=? AND t.tag=?`, pkgID, tag))
	return v, notFound(err)
}

// ---------- container blob links and uploads ----------

// LinkBlob records that a blob was uploaded to (or mounted into) a package.
func (s *Store) LinkBlob(ctx context.Context, pkgID int64, digest string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO package_links (package_id, digest, created_at) VALUES (?, ?, ?)
		ON CONFLICT (package_id, digest) DO UPDATE SET created_at=excluded.created_at`, pkgID, digest, now())
	return err
}

// UnlinkBlob forgets a blob link.
func (s *Store) UnlinkBlob(ctx context.Context, pkgID int64, digest string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM package_links WHERE package_id=? AND digest=?`, pkgID, digest)
	return err
}

// BlobInPackage reports whether a blob belongs to a package: linked by an
// upload, or used by one of its versions.
func (s *Store) BlobInPackage(ctx context.Context, pkgID int64, digest string) bool {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM package_links WHERE package_id=? AND digest=?
		UNION ALL SELECT 1 FROM package_files f JOIN package_versions v ON v.id = f.version_id WHERE v.package_id=? AND f.digest=? LIMIT 1`,
		pkgID, digest, pkgID, digest).Scan(&one)
	return err == nil
}

// StartUpload records an upload session.
func (s *Store) StartUpload(ctx context.Context, id string, pkgID, personID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO package_uploads (id, package_id, person_id, created_at) VALUES (?, ?, ?, ?)`,
		id, pkgID, nullID(personID), now())
	return err
}

// Upload returns the package an upload session belongs to.
func (s *Store) Upload(ctx context.Context, id string) (pkgID int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT package_id FROM package_uploads WHERE id=?`, id).Scan(&pkgID)
	return pkgID, notFound(err)
}

// EndUpload forgets an upload session.
func (s *Store) EndUpload(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM package_uploads WHERE id=?`, id)
	return err
}

// ---------- npm upstream cache ----------

// NpmCached returns a cached upstream packument and when it was fetched.
func (s *Store) NpmCached(ctx context.Context, name string) ([]byte, time.Time, error) {
	var body []byte
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT body, fetched_at FROM package_npm_cache WHERE name=?`, name).Scan(&body, &at)
	return body, time.Unix(at, 0), notFound(err)
}

// SetNpmCached stores an upstream packument.
func (s *Store) SetNpmCached(ctx context.Context, name string, body []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO package_npm_cache (name, body, fetched_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET body=excluded.body, fetched_at=excluded.fetched_at`, name, body, now())
	return err
}

// NpmCachedFile returns a cached upstream tarball's blob.
func (s *Store) NpmCachedFile(ctx context.Context, name, file string) (PackageFile, error) {
	var f PackageFile
	err := s.db.QueryRowContext(ctx, `SELECT file, digest, size FROM package_npm_cache_files WHERE name=? AND file=?`, name, file).
		Scan(&f.Name, &f.Digest, &f.Size)
	return f, notFound(err)
}

// SetNpmCachedFile records a cached upstream tarball.
func (s *Store) SetNpmCachedFile(ctx context.Context, name, file, digest string, size int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO package_npm_cache_files (name, file, digest, size, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (name, file) DO UPDATE SET digest=excluded.digest, size=excluded.size`, name, file, digest, size, now())
	return err
}

// NpmCacheStats counts cached upstream packages and tarball bytes.
func (s *Store) NpmCacheStats(ctx context.Context) (packages, tarballs int, size int64) {
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM package_npm_cache`).Scan(&packages)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(size),0) FROM package_npm_cache_files`).Scan(&tarballs, &size)
	return
}

// ---------- garbage collection ----------

// ReferencedBlobs returns every digest still in use: package files, container
// blob links younger than linkTTL (older links that no manifest uses are
// dropped first) and cached npm tarballs.
func (s *Store) ReferencedBlobs(ctx context.Context, linkTTL time.Duration) (map[string]bool, error) {
	if linkTTL > 0 {
		cut := time.Now().Add(-linkTTL).Unix()
		if _, err := s.db.ExecContext(ctx, `DELETE FROM package_links WHERE created_at < ? AND NOT EXISTS (
			SELECT 1 FROM package_files f JOIN package_versions v ON v.id = f.version_id
			WHERE v.package_id = package_links.package_id AND f.digest = package_links.digest)`, cut); err != nil {
			return nil, err
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM package_uploads WHERE created_at < ?`, cut); err != nil {
			return nil, err
		}
	}
	out := map[string]bool{}
	for _, q := range []string{
		`SELECT DISTINCT digest FROM package_files`,
		`SELECT DISTINCT digest FROM package_links`,
		`SELECT DISTINCT digest FROM package_npm_cache_files`,
	} {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				rows.Close()
				return nil, err
			}
			out[strings.TrimSpace(d)] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
