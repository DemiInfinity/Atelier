package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testGitDir = "gitdata-test"

func init() { RegisterGitDataDir(testGitDir) }

func initBare(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, filepath.Dir(p), "init", "-q", "--bare", "--initial-branch=main", p)
}

// Repositories in a git data directory (snippets) are bundled like repos, not
// copied file by file, and come back from a full restore.
func TestGitDataDirBundledAndRestored(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	src := newEnv(t, t.TempDir())
	full := filepath.Join(src.data, testGitDir, "ab", "ab01.git")
	empty := filepath.Join(src.data, testGitDir, "cd", "cd02.git")
	initBare(t, full)
	initBare(t, empty)
	one := pushCommit(t, full, "main", "a.txt", "first")
	// A loose file a plain copy would have picked up must not be in the archive.
	if err := os.WriteFile(filepath.Join(src.data, testGitDir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Create(ctx, Options{DB: src.db.DB(), RepoRoot: src.git.Root, Dir: filepath.Join(src.data, "backups"),
		Now: time.Date(2026, 11, 11, 3, 0, 0, 0, time.Local)})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ReadManifest(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != ManifestVersion || len(m.DataRepos) != 2 {
		t.Fatalf("manifest = %+v", m)
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, testGitDir+"/") {
			t.Errorf("git data dir file copied as a plain file: %s", f.Path)
		}
	}
	for _, r := range m.DataRepos {
		if r.Dir != testGitDir {
			t.Errorf("data repo dir = %q", r.Dir)
		}
		switch r.Name {
		case "ab01":
			if r.Bundle != "data-repos/"+testGitDir+"/ab/ab01.bundle" || r.Head != "refs/heads/main" || r.SHA256 == "" {
				t.Errorf("full entry = %+v", r)
			}
		case "cd02":
			if r.Bundle != "" {
				t.Errorf("empty repo bundled: %+v", r)
			}
		}
	}

	// Restore into an existing data directory twice: the second run moves the
	// first restore aside and must still place the bundles.
	dst := t.TempDir()
	o := RestoreOptions{Archive: res.Path, DataDir: dst, DBPath: filepath.Join(dst, "atelier.db"), RepoRoot: filepath.Join(dst, "repos")}
	for i := range 2 {
		if _, err := RestoreAll(ctx, o); err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
		got := filepath.Join(dst, testGitDir, "ab", "ab01.git")
		if sha := run(t, got, "rev-parse", "main"); sha != one {
			t.Errorf("restore %d: main = %s, want %s", i, sha, one)
		}
		if err := exec.Command("git", "-C", got, "fsck", "--no-progress").Run(); err != nil {
			t.Errorf("restore %d: fsck: %v", i, err)
		}
		if _, err := os.Stat(filepath.Join(dst, testGitDir, "cd", "cd02.git", "HEAD")); err != nil {
			t.Errorf("restore %d: empty repo missing: %v", i, err)
		}
	}
}

// Snapshots taken before git data directories were bundled carried them as
// plain files (manifest version 1); those must still restore.
func TestGitDataDirVersion1ArchiveRestores(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	src := newEnv(t, t.TempDir())
	full := filepath.Join(src.data, testGitDir, "ab", "ab01.git")
	initBare(t, full)
	one := pushCommit(t, full, "main", "a.txt", "first")
	run(t, full, "gc", "-q") // pack everything, like an old plain copy would hold

	work := t.TempDir()
	if _, err := src.db.DB().ExecContext(ctx, `VACUUM INTO ?`, filepath.Join(work, "atelier.db")); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: 1, Created: time.Now().UTC().Truncate(time.Second)}
	fe, err := fileEntry(filepath.Join(work, "atelier.db"), "atelier.db")
	if err != nil {
		t.Fatal(err)
	}
	m.Files = append(m.Files, fe)
	// The old way: every file under the data directory, read in place.
	isGit := gitDataDirs
	gitDataDirs = nil
	extra, err := collectDataDirs(src.data, &m)
	gitDataDirs = isGit
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "atelier-20261101-030000.tar.gz")
	if _, _, err := writeArchive(archive, work, m, extra); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	o := RestoreOptions{Archive: archive, DataDir: dst, DBPath: filepath.Join(dst, "atelier.db"), RepoRoot: filepath.Join(dst, "repos")}
	if _, err := RestoreAll(ctx, o); err != nil {
		t.Fatal(err)
	}
	if sha := run(t, filepath.Join(dst, testGitDir, "ab", "ab01.git"), "rev-parse", "main"); sha != one {
		t.Errorf("main = %s, want %s", sha, one)
	}
}
