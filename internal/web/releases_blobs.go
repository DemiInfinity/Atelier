package web

// Content-addressed file storage for release assets (<data>/releases/) and
// Git LFS objects (<data>/lfs/). Files are named by their sha256 and sharded
// as ab/cd/<sha>. Uploads stream through a hash into a temporary file inside
// the store (so the final rename never crosses filesystems) and are never
// held in memory.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	errBlobTooLarge = errors.New("file is larger than the limit")
	errBlobChecksum = errors.New("content does not match its sha256")
	errBlobSize     = errors.New("content does not match the expected size")
)

type blobStore struct{ root string }

func (a *App) releaseBlobs() blobStore {
	return blobStore{root: filepath.Join(a.cfg.DataDir, "releases")}
}
func (a *App) lfsBlobs() blobStore { return blobStore{root: filepath.Join(a.cfg.DataDir, "lfs")} }

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (b blobStore) path(sha string) string {
	return filepath.Join(b.root, sha[:2], sha[2:4], sha)
}

// has reports whether the blob exists, and its size.
func (b blobStore) has(sha string) (int64, bool) {
	if !validSHA256(sha) {
		return 0, false
	}
	st, err := os.Stat(b.path(sha))
	if err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	return st.Size(), true
}

func (b blobStore) open(sha string) (*os.File, error) {
	if !validSHA256(sha) {
		return nil, os.ErrNotExist
	}
	return os.Open(b.path(sha))
}

func (b blobStore) remove(sha string) error {
	if !validSHA256(sha) {
		return nil
	}
	err := os.Remove(b.path(sha))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// put streams r into the store. limit caps the size (<= 0: no cap). wantSHA
// and wantSize, when set ("" / < 0 to skip), must match what arrives; on any
// mismatch nothing is stored.
func (b blobStore) put(r io.Reader, limit int64, wantSHA string, wantSize int64) (sha string, size int64, err error) {
	tmpDir := filepath.Join(b.root, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", 0, err
	}
	f, err := os.CreateTemp(tmpDir, "upload-*")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	done := false
	defer func() {
		if !done {
			f.Close()
			os.Remove(tmp)
		}
	}()
	h := sha256.New()
	src := r
	if limit > 0 {
		src = io.LimitReader(r, limit+1)
	}
	n, err := io.Copy(io.MultiWriter(f, h), src)
	if err != nil {
		return "", 0, err
	}
	if limit > 0 && n > limit {
		return "", 0, errBlobTooLarge
	}
	sha = hex.EncodeToString(h.Sum(nil))
	if wantSize >= 0 && n != wantSize {
		return "", 0, errBlobSize
	}
	if wantSHA != "" && sha != wantSHA {
		return "", 0, errBlobChecksum
	}
	if err := f.Sync(); err != nil {
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		return "", 0, err
	}
	done = true
	dst := b.path(sha)
	if _, ok := b.has(sha); ok {
		os.Remove(tmp) // identical content is already stored
		return sha, n, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	if err := os.Chmod(tmp, 0o640); err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	return sha, n, nil
}

// ---------- size limits ----------

const defaultUploadLimit = 2 << 30 // 2 GiB

// parseSize reads "2G", "500M", "64K" or a plain byte count.
func parseSize(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "IB"), "B")
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "T"):
		mult, s = 1<<40, strings.TrimSuffix(s, "T")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n * mult, true
}

func sizeFromEnv(key string) int64 {
	if n, ok := parseSize(os.Getenv(key)); ok {
		return n
	}
	return defaultUploadLimit
}

// releaseAssetLimit is the largest release asset accepted
// (ATELIER_RELEASE_ASSET_MAX, default 2 GB).
func releaseAssetLimit() int64 { return sizeFromEnv("ATELIER_RELEASE_ASSET_MAX") }

// lfsObjectLimit is the largest LFS object accepted (ATELIER_LFS_OBJECT_MAX, default 2 GB).
func lfsObjectLimit() int64 { return sizeFromEnv("ATELIER_LFS_OBJECT_MAX") }

func limitText(n int64) string { return strings.Replace(humanBytes(n), ".0 ", " ", 1) }
