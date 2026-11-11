package pagesdeploy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	typ        byte
	link       string
}

func mkTar(t *testing.T, gz bool, es ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var tw *tar.Writer
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		tw = tar.NewWriter(zw)
	} else {
		tw = tar.NewWriter(&buf)
	}
	for _, e := range es {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	if zw != nil {
		zw.Close()
	}
	return &buf
}

func TestExtractKeepsInside(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "pwned-"+filepath.Base(root))
	s := &Store{Root: root, Limits: Limits{MaxBytes: 1 << 20, MaxFiles: 100}}
	buf := mkTar(t, true,
		entry{name: "index.html", body: "<h1>hi</h1>"},
		entry{name: "css/", typ: tar.TypeDir},
		entry{name: "css/site.css", body: "body{}"},
		entry{name: "../../pwned-" + filepath.Base(root), body: "x"},
		entry{name: "/etc/evil", body: "x"},
		entry{name: "a/../../escape", body: "x"},
		entry{name: `..\win`, body: "x"},
		entry{name: "link", typ: tar.TypeSymlink, link: "/etc/passwd"},
		entry{name: "hard", typ: tar.TypeLink, link: "index.html"},
		entry{name: ".git/config", body: "x"},
	)
	res, err := s.Extract(7, 1, buf, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 {
		t.Errorf("files = %d, want 2", res.Files)
	}
	dir := s.Dir(7, 1)
	if b, _ := os.ReadFile(filepath.Join(dir, "css", "site.css")); string(b) != "body{}" {
		t.Errorf("css = %q", b)
	}
	for _, p := range []string{outside, filepath.Join(dir, "link"), filepath.Join(dir, "hard"), filepath.Join(dir, ".git"), "/etc/evil"} {
		if _, err := os.Lstat(p); err == nil && p != "/etc/evil" {
			t.Errorf("%s exists", p)
		}
	}
	// Nothing outside the snapshot directory besides it.
	ents, _ := os.ReadDir(filepath.Join(root, "sites", "7"))
	if len(ents) != 1 {
		t.Errorf("site dir has %d entries", len(ents))
	}
}

func TestExtractLimitsAndStrip(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: root, Limits: Limits{MaxBytes: 10, MaxFiles: 2}}
	_, err := s.Extract(1, 1, mkTar(t, false, entry{name: "a", body: "1"}, entry{name: "b", body: "2"}, entry{name: "c", body: "3"}), "")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("3 files: err = %v", err)
	}
	_, err = s.Extract(1, 2, mkTar(t, false, entry{name: "a", body: strings.Repeat("x", 11)}), "")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("11 bytes: err = %v", err)
	}
	if snaps := s.Snapshots(1); len(snaps) != 0 {
		t.Errorf("failed deploys left snapshots %v", snaps)
	}
	if m, _ := filepath.Glob(filepath.Join(root, "sites", "1", ".tmp-*")); len(m) != 0 {
		t.Errorf("temp dirs left: %v", m)
	}
	res, err := s.Extract(1, 3, mkTar(t, false, entry{name: "docs/index.html", body: "d"}, entry{name: "README", body: "r"}), "/docs/")
	if err != nil || res.Files != 1 {
		t.Fatalf("strip: %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(1, 3), "index.html")); err != nil {
		t.Error(err)
	}
	if _, ok := CleanFolder("../x"); ok {
		t.Error("CleanFolder accepted ../x")
	}
	if f, ok := CleanFolder("/docs/"); !ok || f != "docs" {
		t.Errorf("CleanFolder(/docs/) = %q %v", f, ok)
	}
	if HumanBytes(1<<30) != "1 GB" {
		t.Errorf("HumanBytes = %s", HumanBytes(1<<30))
	}
}
