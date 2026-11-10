// Package backup takes and restores Atelier snapshots.
//
// A snapshot is one timestamped .tar.gz holding:
//
//	manifest.json                 version, time, files and repos with sizes and sha256
//	atelier.db                    a consistent SQLite copy made with VACUUM INTO
//	repos/<owner>/<name>.bundle   every bare repository as `git bundle create --all`
//
// Bundles are self-consistent even while pushes are happening, which a plain
// copy of a bare repository is not.
package backup

import (
	"archive/tar"
	"bytes"
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
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ManifestVersion is bumped when the archive layout changes incompatibly.
const ManifestVersion = 1

const (
	namePrefix = "atelier-"
	nameSuffix = ".tar.gz"
	nameTime   = "20060102-150405"
)

type Manifest struct {
	Version int         `json:"version"`
	Created time.Time   `json:"created"`
	Files   []FileEntry `json:"files"`
	Repos   []RepoEntry `json:"repos"`
}

type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type RepoEntry struct {
	Owner  string `json:"owner"`
	Name   string `json:"name"`
	Head   string `json:"head,omitempty"`   // e.g. refs/heads/main
	Bundle string `json:"bundle,omitempty"` // archive path; empty for a repo with no refs
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256,omitempty"`
}

func (r RepoEntry) FullName() string { return r.Owner + "/" + r.Name }

// Options says what to snapshot and where to write it.
type Options struct {
	DB       *sql.DB // live database; copied with VACUUM INTO
	RepoRoot string  // holds <owner>/<name>.git
	Git      string  // git binary; "git" when empty
	Dir      string  // output directory
	Now      time.Time
}

// Snapshot is a finished archive, local or remote.
type Snapshot struct {
	Name string
	Path string // local path; empty for remote-only
	Size int64
	Time time.Time
}

// Result describes a snapshot that was just written.
type Result struct {
	Snapshot
	SHA256   string
	Manifest Manifest
}

// Create writes a new snapshot into opts.Dir.
func Create(ctx context.Context, opts Options) (Result, error) {
	if opts.Git == "" {
		opts.Git = "git"
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
		return Result{}, err
	}
	work, err := os.MkdirTemp(opts.Dir, ".work-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(work)

	m := Manifest{Version: ManifestVersion, Created: opts.Now.UTC().Truncate(time.Second)}

	// 1. Database.
	dbCopy := filepath.Join(work, "atelier.db")
	if _, err := opts.DB.ExecContext(ctx, `VACUUM INTO ?`, dbCopy); err != nil {
		return Result{}, fmt.Errorf("copy database: %w", err)
	}
	fe, err := fileEntry(dbCopy, "atelier.db")
	if err != nil {
		return Result{}, err
	}
	m.Files = append(m.Files, fe)

	// 2. Repositories.
	repos, err := findRepos(opts.RepoRoot)
	if err != nil {
		return Result{}, fmt.Errorf("list repositories: %w", err)
	}
	for _, rp := range repos {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		dir := filepath.Join(opts.RepoRoot, rp.Owner, rp.Name+".git")
		entry := RepoEntry{Owner: rp.Owner, Name: rp.Name}
		if out, err := git(ctx, opts.Git, dir, "symbolic-ref", "-q", "HEAD"); err == nil {
			entry.Head = strings.TrimSpace(string(out))
		}
		refs, err := git(ctx, opts.Git, dir, "for-each-ref", "--count=1", "--format=%(refname)")
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", entry.FullName(), err)
		}
		if len(bytes.TrimSpace(refs)) > 0 {
			entry.Bundle = "repos/" + rp.Owner + "/" + rp.Name + ".bundle"
			out := filepath.Join(work, filepath.FromSlash(entry.Bundle))
			if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
				return Result{}, err
			}
			if _, err := git(ctx, opts.Git, dir, "bundle", "create", "--quiet", out, "--all"); err != nil {
				return Result{}, fmt.Errorf("bundle %s: %w", entry.FullName(), err)
			}
			fe, err := fileEntry(out, entry.Bundle)
			if err != nil {
				return Result{}, err
			}
			entry.Size, entry.SHA256 = fe.Size, fe.SHA256
		}
		m.Repos = append(m.Repos, entry)
	}

	// 3. Archive: manifest first so readers can check everything that follows.
	name := namePrefix + opts.Now.Format(nameTime) + nameSuffix
	for i := 2; fileExists(filepath.Join(opts.Dir, name)); i++ {
		name = fmt.Sprintf("%s%s-%d%s", namePrefix, opts.Now.Format(nameTime), i, nameSuffix)
	}
	final := filepath.Join(opts.Dir, name)
	partial := final + ".partial"
	sum, size, err := writeArchive(partial, work, m)
	if err != nil {
		os.Remove(partial)
		return Result{}, err
	}
	if err := os.Rename(partial, final); err != nil {
		os.Remove(partial)
		return Result{}, err
	}
	return Result{Snapshot: Snapshot{Name: name, Path: final, Size: size, Time: opts.Now}, SHA256: sum, Manifest: m}, nil
}

func writeArchive(path, work string, m Manifest) (string, int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	gz := gzip.NewWriter(cw)
	tw := tar.NewWriter(gz)

	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", 0, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o640, Size: int64(len(mj)), ModTime: m.Created}); err != nil {
		return "", 0, err
	}
	if _, err := tw.Write(mj); err != nil {
		return "", 0, err
	}
	paths := make([]string, 0, len(m.Files)+len(m.Repos))
	for _, fe := range m.Files {
		paths = append(paths, fe.Path)
	}
	for _, r := range m.Repos {
		if r.Bundle != "" {
			paths = append(paths, r.Bundle)
		}
	}
	for _, p := range paths {
		if err := addFile(tw, filepath.Join(work, filepath.FromSlash(p)), p, m.Created); err != nil {
			return "", 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	if err := gz.Close(); err != nil {
		return "", 0, err
	}
	if err := f.Sync(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

func addFile(tw *tar.Writer, src, name string, mod time.Time) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o640, Size: st.Size(), ModTime: mod}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func fileEntry(path, name string) (FileEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileEntry{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return FileEntry{}, err
	}
	return FileEntry{Path: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

type repoRef struct{ Owner, Name string }

// findRepos lists <root>/<owner>/<name>.git directories.
func findRepos(root string) ([]repoRef, error) {
	owners, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []repoRef
	for _, o := range owners {
		if !o.IsDir() || strings.HasPrefix(o.Name(), ".") {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, o.Name()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() && strings.HasSuffix(e.Name(), ".git") && !strings.HasPrefix(e.Name(), ".") {
				out = append(out, repoRef{Owner: o.Name(), Name: strings.TrimSuffix(e.Name(), ".git")})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func git(ctx context.Context, bin, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ParseName returns the time encoded in a snapshot file name, in local time.
func ParseName(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, nameSuffix) {
		return time.Time{}, false
	}
	core := strings.TrimSuffix(strings.TrimPrefix(name, namePrefix), nameSuffix)
	if len(core) < len(nameTime) {
		return time.Time{}, false
	}
	if rest := core[len(nameTime):]; rest != "" && !validSuffix(rest) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(nameTime, core[:len(nameTime)], time.Local)
	return t, err == nil
}

func validSuffix(s string) bool {
	if len(s) < 2 || s[0] != '-' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ValidName reports whether name is a snapshot file name (safe to join to a dir).
func ValidName(name string) bool {
	_, ok := ParseName(name)
	return ok && !strings.ContainsAny(name, `/\`)
}

// List returns the snapshots in dir, newest first.
func List(dir string) ([]Snapshot, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, e := range entries {
		t, ok := ParseName(e.Name())
		if !ok || e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Snapshot{Name: e.Name(), Path: filepath.Join(dir, e.Name()), Size: info.Size(), Time: t})
	}
	sortSnapshots(out)
	return out, nil
}

func sortSnapshots(s []Snapshot) {
	sort.Slice(s, func(i, j int) bool {
		if !s[i].Time.Equal(s[j].Time) {
			return s[i].Time.After(s[j].Time)
		}
		return s[i].Name > s[j].Name
	})
}
