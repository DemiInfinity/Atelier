// Package pagesdeploy stores Pages sites on disk: each deploy is an immutable
// snapshot directory, filled from a tar stream with path and size checks.
//
// Layout under Root (usually <data>/pages):
//
//	sites/<repoID>/<deployID>/…   one snapshot per deploy, never changed
//	sites/<repoID>/.tmp-*         snapshots being written
//
// Which snapshot is live is recorded in the database (pages_sites), so the
// switch to a new deploy is a single atomic update; this package only writes
// and removes snapshot directories.
package pagesdeploy

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Limits bound one deploy.
type Limits struct {
	MaxBytes int64 // total size of all files
	MaxFiles int   // number of files (directories don't count)
}

// ErrTooLarge is returned (wrapped) when a deploy breaks a limit.
var ErrTooLarge = errors.New("site is too large")

// Store manages snapshot directories under Root.
type Store struct {
	Root   string
	Limits Limits
}

func (s *Store) siteDir(repoID int64) string {
	return filepath.Join(s.Root, "sites", strconv.FormatInt(repoID, 10))
}

// Dir is where deploy deployID of repository repoID lives.
func (s *Store) Dir(repoID, deployID int64) string {
	return filepath.Join(s.siteDir(repoID), strconv.FormatInt(deployID, 10))
}

// Result describes an extracted snapshot.
type Result struct {
	Files int
	Bytes int64
}

// Extract reads a tar (optionally gzip-compressed) stream into a new snapshot
// for deployID. Only regular files and directories are kept: symlinks, hard
// links, devices and anything whose name leaves the snapshot are skipped (and
// counted in skipped). The snapshot appears at Dir(repoID, deployID) only once
// it's complete; on error nothing is left behind. strip is a folder inside the
// archive to publish (e.g. "docs"); "" publishes the whole archive.
func (s *Store) Extract(repoID, deployID int64, r io.Reader, strip string) (Result, error) {
	var res Result
	strip, ok := CleanFolder(strip)
	if !ok {
		return res, fmt.Errorf("invalid folder %q", strip)
	}
	site := s.siteDir(repoID)
	if err := os.MkdirAll(site, 0o755); err != nil {
		return res, err
	}
	tmp, err := os.MkdirTemp(site, ".tmp-")
	if err != nil {
		return res, err
	}
	done := false
	defer func() {
		if !done {
			_ = os.RemoveAll(tmp)
		}
	}()
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return res, err
	}
	defer root.Close()

	br := bufio.NewReader(r)
	var in io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return res, fmt.Errorf("reading gzip: %w", err)
		}
		defer gz.Close()
		in = gz
	}
	tr := tar.NewReader(in)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return res, fmt.Errorf("reading archive: %w", err)
		}
		name, ok := entryName(h.Name, strip)
		if !ok {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return res, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if h.Size < 0 {
				continue
			}
			res.Files++
			res.Bytes += h.Size
			if s.Limits.MaxFiles > 0 && res.Files > s.Limits.MaxFiles {
				return res, fmt.Errorf("%w: more than %d files", ErrTooLarge, s.Limits.MaxFiles)
			}
			if s.Limits.MaxBytes > 0 && res.Bytes > s.Limits.MaxBytes {
				return res, fmt.Errorf("%w: more than %s", ErrTooLarge, HumanBytes(s.Limits.MaxBytes))
			}
			if dir := path.Dir(name); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return res, err
				}
			}
			f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return res, err
			}
			n, err := io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return res, err
			}
			if n != h.Size {
				return res, errors.New("archive ended inside a file")
			}
		default:
			// symlinks, hard links, devices, fifos: never published
		}
	}
	dest := s.Dir(repoID, deployID)
	_ = os.RemoveAll(dest)
	if err := os.Rename(tmp, dest); err != nil {
		return res, err
	}
	done = true
	return res, nil
}

// entryName cleans an archive path, applies strip, and reports whether the
// entry belongs in the snapshot.
func entryName(name, strip string) (string, bool) {
	name = strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(name, "/") {
		return "", false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", false
		}
	}
	name = path.Clean(name)
	if name == "." || name == "" {
		return "", false
	}
	if strip != "" {
		rest, ok := strings.CutPrefix(name, strip+"/")
		if !ok {
			return "", false
		}
		name = rest
	}
	if strings.HasPrefix(name, ".git/") || name == ".git" {
		return "", false
	}
	if !fs.ValidPath(name) {
		return "", false
	}
	return name, true
}

// CleanFolder normalises a publishing folder: "", "/" and "." mean the root;
// "/docs/" becomes "docs". ok is false for anything climbing out.
func CleanFolder(f string) (string, bool) {
	f = strings.Trim(strings.TrimSpace(strings.ReplaceAll(f, `\`, "/")), "/")
	if f == "" || f == "." {
		return "", true
	}
	for _, seg := range strings.Split(f, "/") {
		if seg == ".." || seg == "" {
			return "", false
		}
	}
	c := path.Clean(f)
	if !fs.ValidPath(c) || len(c) > 200 {
		return "", false
	}
	return c, true
}

// Remove deletes one snapshot.
func (s *Store) Remove(repoID, deployID int64) error {
	return os.RemoveAll(s.Dir(repoID, deployID))
}

// RemoveSite deletes every snapshot of a repository.
func (s *Store) RemoveSite(repoID int64) error {
	return os.RemoveAll(s.siteDir(repoID))
}

// Sites lists the repository IDs that have a directory on disk.
func (s *Store) Sites() []int64 {
	ents, err := os.ReadDir(filepath.Join(s.Root, "sites"))
	if err != nil {
		return nil
	}
	var out []int64
	for _, e := range ents {
		if id, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && e.IsDir() {
			out = append(out, id)
		}
	}
	return out
}

// Snapshots lists the deploy IDs present on disk for a repository.
func (s *Store) Snapshots(repoID int64) []int64 {
	ents, err := os.ReadDir(s.siteDir(repoID))
	if err != nil {
		return nil
	}
	var out []int64
	for _, e := range ents {
		if id, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && e.IsDir() {
			out = append(out, id)
		}
	}
	return out
}

// HumanBytes formats n as "12 KB", "1.5 GB".
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s + " " + string("KMGTPE"[exp]) + "B"
}
