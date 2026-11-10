package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"atelier/internal/store"
)

// RestoreOptions says where a snapshot is restored to.
type RestoreOptions struct {
	Archive  string
	DataDir  string
	DBPath   string
	RepoRoot string
	Git      string // "git" when empty
	Force    bool   // restore even if a server seems to be running
	// AfterRepo runs after each repository is restored (gitrepo.Store.AfterCreate).
	AfterRepo func(ctx context.Context, owner, name string) error
	Log       func(format string, args ...any)
}

func (o *RestoreOptions) defaults() {
	if o.Git == "" {
		o.Git = "git"
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
}

// ReadManifest returns an archive's manifest without extracting anything else.
func ReadManifest(archive string) (Manifest, error) {
	var m Manifest
	err := walkArchive(archive, func(name string, r io.Reader) (bool, error) {
		if name != "manifest.json" {
			return false, errors.New("archive does not start with manifest.json")
		}
		return false, json.NewDecoder(r).Decode(&m)
	})
	return m, err
}

// walkArchive calls fn for each regular file; fn returns false to stop.
func walkArchive(archive string, fn func(name string, r io.Reader) (bool, error)) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a snapshot (gzip): %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read snapshot: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		more, err := fn(h.Name, tr)
		if err != nil || !more {
			return err
		}
	}
}

// extract unpacks the archive into dir, checking every file against the
// manifest. want filters which files to unpack (nil = all).
func extract(archive, dir string, want func(name string) bool) (Manifest, error) {
	var m Manifest
	sums := map[string]FileEntry{}
	got := map[string]bool{}
	first := true
	err := walkArchive(archive, func(name string, r io.Reader) (bool, error) {
		if first {
			first = false
			if name != "manifest.json" {
				return false, errors.New("archive does not start with manifest.json")
			}
			if err := json.NewDecoder(r).Decode(&m); err != nil {
				return false, fmt.Errorf("manifest: %w", err)
			}
			if m.Version > ManifestVersion {
				return false, fmt.Errorf("snapshot format %d is newer than this Atelier understands (%d)", m.Version, ManifestVersion)
			}
			for _, fe := range m.Files {
				sums[fe.Path] = fe
			}
			for _, re := range m.Repos {
				if re.Bundle != "" {
					sums[re.Bundle] = FileEntry{Path: re.Bundle, Size: re.Size, SHA256: re.SHA256}
				}
			}
			return true, nil
		}
		fe, ok := sums[name]
		if !ok {
			return false, fmt.Errorf("unexpected file %q in snapshot", name)
		}
		if want != nil && !want(name) {
			return true, nil
		}
		clean := path.Clean(name)
		if clean != name || path.IsAbs(clean) || strings.HasPrefix(clean, "../") || clean == ".." {
			return false, fmt.Errorf("unsafe path %q in snapshot", name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return false, err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
		if err != nil {
			return false, err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(out, h), r)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return false, err
		}
		if n != fe.Size || hex.EncodeToString(h.Sum(nil)) != fe.SHA256 {
			return false, fmt.Errorf("%s is damaged (size or checksum mismatch)", name)
		}
		got[name] = true
		return true, nil
	})
	if err != nil {
		return m, err
	}
	if first {
		return m, errors.New("empty snapshot")
	}
	for name := range sums {
		if (want == nil || want(name)) && !got[name] {
			return m, fmt.Errorf("snapshot is missing %s", name)
		}
	}
	return m, nil
}

// RestoreAll replaces the database and every repository with the snapshot's.
// The current data is moved aside to <data>/pre-restore-<time>/, not deleted.
func RestoreAll(ctx context.Context, o RestoreOptions) (Manifest, error) {
	o.defaults()
	if held, who := LockHeld(o.DataDir); held && !o.Force {
		return Manifest{}, fmt.Errorf("Atelier looks like it is running against %s (%s). Stop the server first, or pass --force",
			o.DataDir, strings.TrimSpace(who))
	}
	if err := os.MkdirAll(o.DataDir, 0o750); err != nil {
		return Manifest{}, err
	}
	stamp := time.Now().Format(nameTime)
	work, err := os.MkdirTemp(o.DataDir, ".restore-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(work)

	o.Log("checking and unpacking %s", o.Archive)
	m, err := extract(o.Archive, work, nil)
	if err != nil {
		return m, err
	}

	// Move the current data aside.
	aside := filepath.Join(o.DataDir, "pre-restore-"+stamp)
	moved := false
	moveAside := func(src, name string) error {
		if !fileExists(src) {
			return nil
		}
		if err := os.MkdirAll(aside, 0o750); err != nil {
			return err
		}
		moved = true
		return os.Rename(src, filepath.Join(aside, name))
	}
	for _, sfx := range []string{"", "-wal", "-shm"} {
		if err := moveAside(o.DBPath+sfx, filepath.Base(o.DBPath)+sfx); err != nil {
			return m, err
		}
	}
	if err := moveAside(o.RepoRoot, "repos"); err != nil {
		return m, err
	}
	if moved {
		o.Log("moved the current data to %s", aside)
	}

	if err := os.Rename(filepath.Join(work, "atelier.db"), o.DBPath); err != nil {
		return m, fmt.Errorf("place database: %w", err)
	}
	if err := os.MkdirAll(o.RepoRoot, 0o755); err != nil {
		return m, err
	}
	for _, re := range m.Repos {
		dst := filepath.Join(o.RepoRoot, strings.ToLower(re.Owner), strings.ToLower(re.Name)+".git")
		bundle := ""
		if re.Bundle != "" {
			bundle = filepath.Join(work, filepath.FromSlash(re.Bundle))
		}
		if err := restoreBundle(ctx, o.Git, bundle, dst, re.Head); err != nil {
			return m, fmt.Errorf("%s: %w", re.FullName(), err)
		}
		if o.AfterRepo != nil {
			if err := o.AfterRepo(ctx, strings.ToLower(re.Owner), strings.ToLower(re.Name)); err != nil {
				return m, fmt.Errorf("%s: %w", re.FullName(), err)
			}
		}
		o.Log("restored %s", re.FullName())
	}
	return m, nil
}

// RestoreRepo restores one repository (owner/name) from a snapshot into a
// running or stopped server. An existing copy is moved aside, and the
// repository's row is created or updated from the snapshot's database.
func RestoreRepo(ctx context.Context, o RestoreOptions, db *store.Store, fullName string) error {
	o.defaults()
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || owner == "" || name == "" {
		return errors.New("use --repo owner/name")
	}
	m, err := ReadManifest(o.Archive)
	if err != nil {
		return err
	}
	var entry *RepoEntry
	for i := range m.Repos {
		if strings.EqualFold(m.Repos[i].Owner, owner) && strings.EqualFold(m.Repos[i].Name, name) {
			entry = &m.Repos[i]
		}
	}
	if entry == nil {
		return fmt.Errorf("%s is not in this snapshot", fullName)
	}
	if err := os.MkdirAll(o.DataDir, 0o750); err != nil {
		return err
	}
	work, err := os.MkdirTemp(o.DataDir, ".restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if _, err := extract(o.Archive, work, func(n string) bool { return n == "atelier.db" || n == entry.Bundle }); err != nil {
		return err
	}

	// Metadata from the snapshot's database.
	snap, err := sql.Open("sqlite", "file:"+filepath.Join(work, "atelier.db")+"?mode=ro")
	if err != nil {
		return err
	}
	defer snap.Close()
	var desc, ownerKind, ownerDisplay string
	var private bool
	err = snap.QueryRowContext(ctx, `SELECT r.description, r.private, o.kind, o.display_name
		FROM repos r JOIN owners o ON o.id = r.owner_id WHERE o.name = ? AND r.name = ?`, entry.Owner, entry.Name).
		Scan(&desc, &private, &ownerKind, &ownerDisplay)
	if errors.Is(err, sql.ErrNoRows) {
		desc, private, ownerKind = "", true, "" // bundle without a row: only restore into an existing account
	} else if err != nil {
		return fmt.Errorf("read snapshot database: %w", err)
	}

	liveOwner, err := db.OwnerByName(ctx, entry.Owner)
	if errors.Is(err, store.ErrNotFound) {
		if ownerKind != "company" {
			return fmt.Errorf("the account %q doesn't exist here; restore everything instead, or create it first", entry.Owner)
		}
		if liveOwner, err = db.CreateOwner(ctx, entry.Owner, ownerDisplay, "company"); err != nil {
			return err
		}
		o.Log("created company account %s", liveOwner.Name)
	} else if err != nil {
		return err
	}

	dst := filepath.Join(o.RepoRoot, strings.ToLower(liveOwner.Name), strings.ToLower(entry.Name)+".git")
	if fileExists(dst) {
		aside := dst + ".pre-restore-" + time.Now().Format(nameTime)
		if err := os.Rename(dst, aside); err != nil {
			return err
		}
		o.Log("moved the current copy to %s", aside)
	}
	bundle := ""
	if entry.Bundle != "" {
		bundle = filepath.Join(work, filepath.FromSlash(entry.Bundle))
	}
	if err := restoreBundle(ctx, o.Git, bundle, dst, entry.Head); err != nil {
		return err
	}
	if o.AfterRepo != nil {
		if err := o.AfterRepo(ctx, strings.ToLower(liveOwner.Name), strings.ToLower(entry.Name)); err != nil {
			return err
		}
	}
	if r, err := db.RepoByName(ctx, liveOwner.Name, entry.Name); err == nil {
		err = db.UpdateRepo(ctx, r.ID, desc, private)
		if err != nil {
			return err
		}
	} else if errors.Is(err, store.ErrNotFound) {
		if _, err := db.CreateRepo(ctx, liveOwner.ID, entry.Name, desc, private); err != nil {
			return err
		}
	} else {
		return err
	}
	o.Log("restored %s", entry.FullName())
	return nil
}

// restoreBundle creates a bare repository at dst from a bundle (or an empty
// one when bundle is "").
func restoreBundle(ctx context.Context, gitBin, bundle, dst, head string) error {
	if fileExists(dst) {
		return fmt.Errorf("%s already exists", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := git(ctx, gitBin, filepath.Dir(dst), "init", "--bare", "--quiet", "--initial-branch=main", dst); err != nil {
		return err
	}
	if bundle != "" {
		if _, err := git(ctx, gitBin, dst, "fetch", "--quiet", "--update-head-ok", bundle, "+refs/*:refs/*"); err != nil {
			return err
		}
	}
	if strings.HasPrefix(head, "refs/heads/") {
		if _, err := git(ctx, gitBin, dst, "symbolic-ref", "HEAD", head); err != nil {
			return err
		}
	}
	_, err := git(ctx, gitBin, dst, "config", "http.receivepack", "true")
	return err
}
