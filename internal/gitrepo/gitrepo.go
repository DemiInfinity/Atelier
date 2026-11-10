// Package gitrepo manages bare repositories on disk and reads them for the
// web UI. Everything goes through the real git binary so behaviour matches
// what git clients see.
package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ValidName reports whether s is usable as an owner or repository name.
func ValidName(s string) bool {
	return nameRe.MatchString(s) && !strings.HasSuffix(strings.ToLower(s), ".git") && !strings.Contains(s, "..")
}

type Store struct {
	Root string
	Git  string // path to the git binary
}

func New(root string) (*Store, error) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("git not found in PATH: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{Root: root, Git: gitBin}, nil
}

// Path is where a repo lives on disk. Names are lowercased so URLs are
// case-insensitive, matching the database's NOCASE collation.
func (s *Store) Path(owner, name string) string {
	return filepath.Join(s.Root, strings.ToLower(owner), strings.ToLower(name)+".git")
}

// RelPath is Path relative to Root, with forward slashes (for git http-backend).
func (s *Store) RelPath(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name) + ".git"
}

func (s *Store) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Git, args...)
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

func (s *Store) Init(ctx context.Context, owner, name string) error {
	p := s.Path(owner, name)
	if _, err := os.Stat(p); err == nil {
		return errors.New("a repository already exists at that path")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if _, err := s.run(ctx, s.Root, "init", "--bare", "--initial-branch=main", p); err != nil {
		return err
	}
	// receive-pack over HTTP is gated by Atelier's own auth, so allow it here.
	if _, err := s.run(ctx, p, "config", "http.receivepack", "true"); err != nil {
		return err
	}
	return s.AfterCreate(ctx, owner, name)
}

// afterCreate holds setup steps other features need on every new repository
// (e.g. installing server-side hooks). Register them from an init() function.
var afterCreate []func(ctx context.Context, s *Store, owner, name string) error

// OnCreate registers a step to run whenever a repository is created or imported.
func OnCreate(f func(ctx context.Context, s *Store, owner, name string) error) {
	afterCreate = append(afterCreate, f)
}

// AfterCreate runs the registered steps. Init calls it; anything that creates a
// bare repo another way (imports, mirrors) must call it too.
func (s *Store) AfterCreate(ctx context.Context, owner, name string) error {
	for _, f := range afterCreate {
		if err := f(ctx, s, owner, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Remove(owner, name string) error {
	return os.RemoveAll(s.Path(owner, name))
}

// ---------- reading ----------

type Repo struct {
	s   *Store
	dir string
}

func (s *Store) Open(owner, name string) (*Repo, error) {
	p := s.Path(owner, name)
	if _, err := os.Stat(p); err != nil {
		return nil, ErrNotFound
	}
	return &Repo{s: s, dir: p}, nil
}

func (r *Repo) run(ctx context.Context, args ...string) ([]byte, error) {
	return r.s.run(ctx, r.dir, args...)
}

func (r *Repo) IsEmpty(ctx context.Context) bool {
	out, err := r.run(ctx, "for-each-ref", "--count=1", "refs/heads")
	return err != nil || len(bytes.TrimSpace(out)) == 0
}

func (r *Repo) DefaultBranch(ctx context.Context) string {
	out, err := r.run(ctx, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "main"
	}
	b := strings.TrimSpace(string(out))
	// If HEAD points at a branch that doesn't exist (e.g. pushed only "master"), fall back.
	if _, err := r.ResolveCommit(ctx, b); err != nil {
		if bs := r.Branches(ctx); len(bs) > 0 {
			return bs[0]
		}
	}
	return b
}

func (r *Repo) Branches(ctx context.Context) []string {
	out, err := r.run(ctx, "for-each-ref", "--format=%(refname:short)", "--sort=-committerdate", "refs/heads")
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

func (r *Repo) Tags(ctx context.Context) []string {
	out, err := r.run(ctx, "for-each-ref", "--format=%(refname:short)", "--sort=-creatordate", "refs/tags")
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// ResolveCommit turns a branch, tag or sha into a full commit hash.
func (r *Repo) ResolveCommit(ctx context.Context, ref string) (string, error) {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\n:") {
		return "", ErrNotFound
	}
	out, err := r.run(ctx, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", ErrNotFound
	}
	return strings.TrimSpace(string(out)), nil
}

// SplitRefPath splits "feature/x/docs/readme.md" into a ref and a path by
// trying the longest branch or tag name first, so refs may contain slashes.
func (r *Repo) SplitRefPath(ctx context.Context, rest string) (ref, path string, ok bool) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	for i := len(parts); i >= 1; i-- {
		cand := strings.Join(parts[:i], "/")
		if _, err := r.ResolveCommit(ctx, cand); err == nil {
			return cand, strings.Join(parts[i:], "/"), true
		}
	}
	return "", "", false
}

type Entry struct {
	Mode, Type, Hash, Name, Path string
	Size                         int64
}

func (e Entry) IsDir() bool { return e.Type == "tree" }

func cleanPath(p string) (string, bool) {
	p = strings.Trim(p, "/")
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", false
		}
	}
	return p, true
}

func (r *Repo) Tree(ctx context.Context, commit, path string) ([]Entry, error) {
	path, ok := cleanPath(path)
	if !ok {
		return nil, ErrNotFound
	}
	out, err := r.run(ctx, "ls-tree", "-z", "-l", commit+":"+path)
	if err != nil {
		return nil, ErrNotFound
	}
	var entries []Entry
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, name, found := strings.Cut(rec, "\t")
		if !found {
			continue
		}
		f := strings.Fields(meta)
		if len(f) < 4 {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64)
		full := name
		if path != "" {
			full = path + "/" + name
		}
		entries = append(entries, Entry{Mode: f[0], Type: f[1], Hash: f[2], Size: size, Name: name, Path: full})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

func (r *Repo) ObjectType(ctx context.Context, commit, path string) string {
	path, ok := cleanPath(path)
	if !ok {
		return ""
	}
	if path == "" {
		return "tree"
	}
	out, err := r.run(ctx, "cat-file", "-t", commit+":"+path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

const MaxBlob = 1 << 20

type Blob struct {
	Data     []byte
	Size     int64
	Binary   bool
	TooLarge bool
}

func (r *Repo) Blob(ctx context.Context, commit, path string, limit int64) (*Blob, error) {
	path, ok := cleanPath(path)
	if !ok || path == "" {
		return nil, ErrNotFound
	}
	sizeOut, err := r.run(ctx, "cat-file", "-s", commit+":"+path)
	if err != nil {
		return nil, ErrNotFound
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(string(sizeOut)), 10, 64)
	b := &Blob{Size: size}
	if limit > 0 && size > limit {
		b.TooLarge = true
		return b, nil
	}
	data, err := r.run(ctx, "cat-file", "blob", commit+":"+path)
	if err != nil {
		return nil, err
	}
	b.Data = data
	probe := data
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	b.Binary = bytes.IndexByte(probe, 0) >= 0
	return b, nil
}

type Commit struct {
	Hash, Short, Author, Email, Subject, Body string
	When                                      time.Time
	Parents                                   []string
}

const logFormat = "%H%x1f%P%x1f%an%x1f%ae%x1f%at%x1f%s%x1f%b%x1e"

func parseCommits(out []byte) []Commit {
	var commits []Commit
	for _, rec := range strings.Split(string(out), "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 7)
		if len(f) < 7 {
			continue
		}
		ts, _ := strconv.ParseInt(f[4], 10, 64)
		short := f[0]
		if len(short) > 7 {
			short = short[:7]
		}
		commits = append(commits, Commit{
			Hash: f[0], Short: short, Parents: strings.Fields(f[1]), Author: f[2], Email: f[3],
			When: time.Unix(ts, 0), Subject: f[5], Body: strings.TrimSpace(f[6]),
		})
	}
	return commits
}

func (r *Repo) Log(ctx context.Context, commit, path string, skip, limit int) ([]Commit, error) {
	args := []string{"log", "--format=" + logFormat, "--skip=" + strconv.Itoa(skip), "-n", strconv.Itoa(limit), commit}
	if p, ok := cleanPath(path); ok && p != "" {
		args = append(args, "--", p)
	}
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return parseCommits(out), nil
}

func (r *Repo) LastCommit(ctx context.Context, commit, path string) *Commit {
	cs, err := r.Log(ctx, commit, path, 0, 1)
	if err != nil || len(cs) == 0 {
		return nil
	}
	return &cs[0]
}

func (r *Repo) CommitCount(ctx context.Context, commit string) int {
	out, err := r.run(ctx, "rev-list", "--count", commit)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// Show returns a commit and its patch (capped in size).
func (r *Repo) Show(ctx context.Context, sha string) (*Commit, string, error) {
	out, err := r.run(ctx, "show", "-s", "--format="+logFormat, sha)
	if err != nil {
		return nil, "", ErrNotFound
	}
	cs := parseCommits(out)
	if len(cs) == 0 {
		return nil, "", ErrNotFound
	}
	patch, err := r.run(ctx, "show", "--format=", "--patch", "--stat", "--no-color", "--find-renames", sha)
	if err != nil {
		return &cs[0], "", nil
	}
	const maxPatch = 2 << 20
	if len(patch) > maxPatch {
		patch = append(patch[:maxPatch], []byte("\n… diff truncated …\n")...)
	}
	return &cs[0], string(patch), nil
}

// CommitDays returns commit timestamps across all branches since the given
// time, used for the Embers contribution graph.
func (r *Repo) CommitTimes(ctx context.Context, since time.Time) []time.Time {
	out, err := r.run(ctx, "log", "--all", "--format=%at", "--since="+strconv.FormatInt(since.Unix(), 10))
	if err != nil {
		return nil
	}
	var ts []time.Time
	for _, l := range strings.Fields(string(out)) {
		if n, err := strconv.ParseInt(l, 10, 64); err == nil {
			ts = append(ts, time.Unix(n, 0))
		}
	}
	return ts
}
