package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestDeployPagesPack(t *testing.T) {
	if !isDeployPages("atelier/deploy-pages@v1") || !isDeployPages("Atelier/Deploy-Pages@main") || isDeployPages("atelier/deploy-pages-x@v1") {
		t.Error("isDeployPages")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "assets"), 0o755)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>x</h1>"), 0o644)
	os.WriteFile(filepath.Join(dir, "assets", "a.js"), []byte("go()"), 0o644)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("secret"), 0o644)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "passwd")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	files, size, skipped, err := packSite(dir, &buf)
	if err != nil || files != 2 || size != 14 || skipped != 1 {
		t.Fatalf("packSite = %d %d %d %v", files, size, skipped, err)
	}
	zr, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	sort.Strings(names)
	want := []string{"assets/", "assets/a.js", "index.html"}
	if len(names) != len(want) {
		t.Fatalf("names = %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names = %v", names)
		}
	}
}
