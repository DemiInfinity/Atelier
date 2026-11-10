package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Release assets and LFS objects travel with every snapshot and come back
// on a full restore; uploads in progress (hidden .tmp files) don't.
func TestSnapshotIncludesDataDirs(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()
	src := newEnv(t, t.TempDir())
	if _, err := src.db.CreateOwner(ctx, "demi", "", "user"); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"releases/ab/cd/abcd1234": "runner binary",
		"lfs/12/34/12345678":      "large design file",
	}
	for p, content := range files {
		full := filepath.Join(src.data, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(src.data, "lfs", ".tmp", "upload-1")
	os.MkdirAll(filepath.Dir(tmp), 0o750)
	os.WriteFile(tmp, []byte("half an upload"), 0o640)

	// DataDir defaults to the parent of RepoRoot.
	res, err := Create(ctx, Options{DB: src.db.DB(), RepoRoot: src.git.Root, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m, err := ReadManifest(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, fe := range m.Files {
		got[fe.Path] = true
		if strings.Contains(fe.Path, ".tmp") {
			t.Errorf("snapshot includes an upload in progress: %s", fe.Path)
		}
	}
	for p := range files {
		if !got[p] {
			t.Errorf("snapshot lacks %s (files %v)", p, m.Files)
		}
	}

	// Full restore into a data dir that already has other LFS data: it's
	// moved aside, and the snapshot's files take its place.
	dst := t.TempDir()
	stale := filepath.Join(dst, "lfs", "ff", "ff", "stale")
	os.MkdirAll(filepath.Dir(stale), 0o750)
	os.WriteFile(stale, []byte("stale"), 0o640)
	e := newEnv(t, dst)
	e.db.Close()
	if _, err := RestoreAll(ctx, e.restoreOpts(res.Path)); err != nil {
		t.Fatal(err)
	}
	for p, content := range files {
		b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(p)))
		if err != nil || string(b) != content {
			t.Errorf("restored %s = %q, %v", p, b, err)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale LFS object still in place: %v", err)
	}
	asides, _ := filepath.Glob(filepath.Join(dst, "pre-restore-*", "lfs", "ff", "ff", "stale"))
	if len(asides) != 1 {
		t.Errorf("stale data wasn't moved aside: %v", asides)
	}

	// Usage reports both stores.
	u := DiskUsage(filepath.Join(dst, "repos"), filepath.Join(dst, "atelier.db"), t.TempDir())
	if u.LFS != int64(len("large design file")) || u.Releases != int64(len("runner binary")) {
		t.Errorf("usage = %+v", u)
	}
}
