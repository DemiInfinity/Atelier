// Package packages stores the files behind Atelier's package registries (npm,
// containers, Go module zips, generic files) as content-addressed blobs:
//
//	<data>/packages/blobs/sha256/<ab>/<abcdef…>   one file per blob, named by its sha256
//	<data>/packages/.uploads/<id>                  container uploads in progress
//	<data>/packages/.tmp/                          blobs being written
//
// The dot directories are skipped by backups (only finished blobs matter).
//
// Blobs are streamed to disk, hashed on the way and renamed into place, so a
// blob file is always complete. Metadata (which package version uses which
// blob) lives in the database; GC removes blob files nothing refers to.
package packages

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrTooLarge is returned when a blob or upload exceeds its size limit.
var ErrTooLarge = errors.New("too large")

// ErrDigest is returned when content doesn't match the digest it was sent with.
var ErrDigest = errors.New("digest mismatch")

// ErrNotFound is returned for a missing blob or upload.
var ErrNotFound = errors.New("not found")

// Blobs is the on-disk blob store rooted at <data>/packages.
type Blobs struct {
	Root string
}

// New prepares the directories under root.
func New(root string) (*Blobs, error) {
	for _, d := range []string{"blobs/sha256", ".uploads", ".tmp"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o750); err != nil {
			return nil, err
		}
	}
	return &Blobs{Root: root}, nil
}

// ParseDigest checks a "sha256:<64 hex>" digest and returns its hex part.
func ParseDigest(d string) (string, bool) {
	hexPart, ok := strings.CutPrefix(d, "sha256:")
	if !ok || len(hexPart) != 64 {
		return "", false
	}
	for _, c := range hexPart {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return hexPart, true
}

// Path is where a blob lives on disk ("" for an invalid digest).
func (b *Blobs) Path(digest string) string {
	h, ok := ParseDigest(digest)
	if !ok {
		return ""
	}
	return filepath.Join(b.Root, "blobs", "sha256", h[:2], h)
}

// Stat reports a blob's size, if it exists.
func (b *Blobs) Stat(digest string) (int64, bool) {
	p := b.Path(digest)
	if p == "" {
		return 0, false
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	return st.Size(), true
}

// Open opens a blob for reading.
func (b *Blobs) Open(digest string) (*os.File, error) {
	p := b.Path(digest)
	if p == "" {
		return nil, ErrNotFound
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Written describes a stored blob.
type Written struct {
	Digest string // sha256:<hex>
	Size   int64
	SHA1   string // hex, for npm's shasum
	SHA512 []byte // raw, for npm's integrity
}

// Put streams r into the store. max (>0) limits the size; want, if set, is the
// digest the content must have. Extra hashes (sha1, sha512) are computed for
// npm. An existing identical blob is kept as is.
func (b *Blobs) Put(r io.Reader, max int64, want string) (Written, error) {
	if want != "" {
		if _, ok := ParseDigest(want); !ok {
			return Written{}, ErrDigest
		}
	}
	tmp, err := os.CreateTemp(filepath.Join(b.Root, ".tmp"), "blob-")
	if err != nil {
		return Written{}, err
	}
	defer os.Remove(tmp.Name())
	hs := newHashes()
	n, err := copyLimited(io.MultiWriter(tmp, hs), r, max)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Written{}, err
	}
	w := hs.result(n)
	if want != "" && w.Digest != want {
		return Written{}, ErrDigest
	}
	if err := b.place(tmp.Name(), w.Digest); err != nil {
		return Written{}, err
	}
	return w, nil
}

// place renames a finished temp file to its digest path.
func (b *Blobs) place(tmp, digest string) error {
	dst := b.Path(digest)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	if st, err := os.Stat(dst); err == nil && st.Mode().IsRegular() {
		// Already stored: refresh its time so GC's grace period protects it
		// until the caller records a reference.
		now := time.Now()
		_ = os.Chtimes(dst, now, now)
		return nil
	}
	_ = os.Chmod(tmp, 0o640)
	return os.Rename(tmp, dst)
}

// copyLimited copies at most max bytes (max <= 0: no limit), failing with
// ErrTooLarge if r has more.
func copyLimited(dst io.Writer, r io.Reader, max int64) (int64, error) {
	if max <= 0 {
		return io.Copy(dst, r)
	}
	n, err := io.Copy(dst, io.LimitReader(r, max+1))
	if err != nil {
		return n, err
	}
	if n > max {
		return n, ErrTooLarge
	}
	return n, nil
}

type hashes struct {
	sha256, sha1, sha512 hash.Hash
}

func newHashes() *hashes {
	return &hashes{sha256: sha256.New(), sha1: sha1.New(), sha512: sha512.New()}
}

func (h *hashes) Write(p []byte) (int, error) {
	h.sha256.Write(p)
	h.sha1.Write(p)
	h.sha512.Write(p)
	return len(p), nil
}

func (h *hashes) result(n int64) Written {
	return Written{
		Digest: "sha256:" + hex.EncodeToString(h.sha256.Sum(nil)),
		Size:   n,
		SHA1:   hex.EncodeToString(h.sha1.Sum(nil)),
		SHA512: h.sha512.Sum(nil),
	}
}

// ---------- uploads (container registry) ----------

func (b *Blobs) uploadPath(id string) (string, bool) {
	if id == "" || len(id) > 64 || strings.ContainsAny(id, `/\.`) {
		return "", false
	}
	return filepath.Join(b.Root, ".uploads", id), true
}

// StartUpload creates an empty upload file.
func (b *Blobs) StartUpload(id string) error {
	p, ok := b.uploadPath(id)
	if !ok {
		return ErrNotFound
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	return f.Close()
}

// UploadSize is how many bytes an upload holds so far.
func (b *Blobs) UploadSize(id string) (int64, error) {
	p, ok := b.uploadPath(id)
	if !ok {
		return 0, ErrNotFound
	}
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// AppendUpload adds r to an upload, keeping the total at most max bytes. On
// error the upload is truncated back to its previous size.
func (b *Blobs) AppendUpload(id string, r io.Reader, max int64) (int64, error) {
	p, ok := b.uploadPath(id)
	if !ok {
		return 0, ErrNotFound
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	before := st.Size()
	room := int64(0)
	if max > 0 {
		room = max - before
		if room < 0 {
			room = 0
		}
		if room == 0 {
			room = -1 // any byte at all is too many
		}
	}
	var n int64
	if room < 0 {
		var one [1]byte
		k, _ := io.ReadFull(r, one[:])
		if k > 0 {
			err = ErrTooLarge
		}
	} else {
		n, err = copyLimited(f, r, room)
	}
	if err != nil {
		_ = f.Truncate(before)
		return before, err
	}
	return before + n, nil
}

// FinishUpload checks an upload against digest and moves it into the store.
func (b *Blobs) FinishUpload(id, digest string) (Written, error) {
	p, ok := b.uploadPath(id)
	if !ok {
		return Written{}, ErrNotFound
	}
	if _, ok := ParseDigest(digest); !ok {
		return Written{}, ErrDigest
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return Written{}, ErrNotFound
	}
	if err != nil {
		return Written{}, err
	}
	hs := newHashes()
	n, err := io.Copy(hs, f)
	f.Close()
	if err != nil {
		return Written{}, err
	}
	w := hs.result(n)
	if w.Digest != digest {
		return Written{}, ErrDigest
	}
	if err := b.place(p, digest); err != nil {
		return Written{}, err
	}
	_ = os.Remove(p) // when the blob already existed, place leaves the upload file behind
	return w, nil
}

// CancelUpload deletes an upload.
func (b *Blobs) CancelUpload(id string) error {
	p, ok := b.uploadPath(id)
	if !ok {
		return ErrNotFound
	}
	err := os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// ---------- garbage collection ----------

// GCResult says what a collection did.
type GCResult struct {
	Kept, Removed int
	Freed         int64
	Uploads       int // stale uploads removed
}

func (g GCResult) String() string {
	return fmt.Sprintf("kept %d blobs, removed %d (%s freed), %d stale uploads", g.Kept, g.Removed, human(g.Freed), g.Uploads)
}

// GC deletes blob files whose digest isn't referenced, if they are older than
// grace (which protects blobs written moments ago whose database rows don't
// exist yet). Uploads and temp files untouched for staleUploads are removed too.
func (b *Blobs) GC(referenced func(digest string) bool, grace, staleUploads time.Duration) (GCResult, error) {
	var res GCResult
	cut := time.Now().Add(-grace)
	root := filepath.Join(b.Root, "blobs", "sha256")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		digest := "sha256:" + d.Name()
		if _, ok := ParseDigest(digest); !ok {
			return nil // not ours; leave it alone
		}
		if referenced(digest) {
			res.Kept++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(cut) {
			res.Kept++
			return nil
		}
		if err := os.Remove(p); err == nil {
			res.Removed++
			res.Freed += info.Size()
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	// Temp files left by crashes and stale uploads.
	stale := time.Now().Add(-staleUploads)
	for _, dir := range []string{".tmp", ".uploads"} {
		entries, _ := os.ReadDir(filepath.Join(b.Root, dir))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || info.ModTime().After(stale) {
				continue
			}
			if os.Remove(filepath.Join(b.Root, dir, e.Name())) == nil && dir == ".uploads" {
				res.Uploads++
			}
		}
	}
	return res, nil
}

func human(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/(k*k))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
}

// DiskUsage is the total size of all blobs.
func (b *Blobs) DiskUsage() int64 {
	var total int64
	_ = filepath.WalkDir(filepath.Join(b.Root, "blobs"), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
