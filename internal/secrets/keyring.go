// Package secrets encrypts Actions secrets at rest with AES-256-GCM.
//
// # The key
//
// The 32-byte key comes from ATELIER_SECRET_KEY (standard base64) when it is
// set. Otherwise Atelier generates one the first time it needs it and keeps it
// in <data>/secret.key (mode 0600).
//
// Backups copy the database, which holds the secrets only in encrypted form.
// Back the key up as well, somewhere other than next to the backups: without
// it, the secrets in a restored database can't be read (everything else
// restores fine; you'd just enter the secrets again).
//
// # Rotation
//
// `atelier secrets rotate-key` writes a new key to secret.key.next, re-encrypts
// every secret with it in one database transaction, then moves the old key to
// secret.key.old and the new one into place. Load reads all three files, and
// every ciphertext names the key that sealed it, so a rotation interrupted at
// any point can still decrypt everything (run it again to finish).
//
// # Ciphertext format
//
//	"A1" | key id (8 bytes) | nonce (12 bytes) | AES-GCM ciphertext and tag
//
// The caller passes additional data naming what the value belongs to (scope and
// name), so a ciphertext copied onto another row fails to open.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the AES-256 key length.
const KeySize = 32

// EnvKey is the environment variable that supplies the key.
const EnvKey = "ATELIER_SECRET_KEY"

// File names inside the data directory.
const (
	KeyFile     = "secret.key"
	NextKeyFile = "secret.key.next" // written first during rotation
	OldKeyFile  = "secret.key.old"  // the key before the last rotation
)

var (
	// ErrTampered means a ciphertext failed authentication: it was changed,
	// truncated, or moved to a different secret.
	ErrTampered = errors.New("secret ciphertext failed authentication (tampered or moved)")
	// ErrUnknownKey means the ciphertext was sealed with a key this keyring
	// doesn't have (e.g. the key file was replaced, or a backup restored
	// without its key).
	ErrUnknownKey = errors.New("secret was encrypted with a key this server doesn't have")
)

var magic = []byte{'A', '1'}

const idLen = 8

type key struct {
	id   [idLen]byte
	aead cipher.AEAD
}

// Keyring seals with its primary key and opens with any key it holds.
type Keyring struct {
	primary key
	byID    map[[idLen]byte]key
}

// GenerateKey returns a new random key.
func GenerateKey() []byte {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}

// KeyID is a short public identifier of a key (never the key itself).
func KeyID(raw []byte) string {
	id := keyID(raw)
	return hex.EncodeToString(id[:])
}

func keyID(raw []byte) [idLen]byte {
	h := sha256.Sum256(append([]byte("atelier secret key id\x00"), raw...))
	var id [idLen]byte
	copy(id[:], h[:idLen])
	return id
}

func newKey(raw []byte) (key, error) {
	if len(raw) != KeySize {
		return key{}, fmt.Errorf("secret key must be %d bytes, got %d", KeySize, len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return key{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return key{}, err
	}
	return key{id: keyID(raw), aead: aead}, nil
}

// NewKeyring builds a keyring that seals with primary and can also open
// anything sealed with one of the others.
func NewKeyring(primary []byte, others ...[]byte) (*Keyring, error) {
	p, err := newKey(primary)
	if err != nil {
		return nil, err
	}
	kr := &Keyring{primary: p, byID: map[[idLen]byte]key{p.id: p}}
	for _, raw := range others {
		k, err := newKey(raw)
		if err != nil {
			return nil, err
		}
		if _, ok := kr.byID[k.id]; !ok {
			kr.byID[k.id] = k
		}
	}
	return kr, nil
}

// PrimaryID identifies the key new values are sealed with.
func (kr *Keyring) PrimaryID() string { return hex.EncodeToString(kr.primary.id[:]) }

// Seal encrypts plaintext, binding it to aad.
func (kr *Keyring) Seal(aad, plaintext []byte) ([]byte, error) {
	k := kr.primary
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(magic)+idLen+len(nonce)+len(plaintext)+k.aead.Overhead())
	out = append(out, magic...)
	out = append(out, k.id[:]...)
	out = append(out, nonce...)
	return k.aead.Seal(out, nonce, plaintext, aad), nil
}

// Open decrypts a value sealed by Seal with the same aad.
func (kr *Keyring) Open(aad, blob []byte) ([]byte, error) {
	hdr := len(magic) + idLen
	if len(blob) < hdr || string(blob[:len(magic)]) != string(magic) {
		return nil, ErrTampered
	}
	var id [idLen]byte
	copy(id[:], blob[len(magic):hdr])
	k, ok := kr.byID[id]
	if !ok {
		return nil, ErrUnknownKey
	}
	ns := k.aead.NonceSize()
	if len(blob) < hdr+ns+k.aead.Overhead() {
		return nil, ErrTampered
	}
	pt, err := k.aead.Open(nil, blob[hdr:hdr+ns], blob[hdr+ns:], aad)
	if err != nil {
		return nil, ErrTampered
	}
	return pt, nil
}

// Source says where the primary key came from.
type Source string

const (
	FromEnv  Source = "env"
	FromFile Source = "file"
)

// DecodeKey parses a base64 (standard or URL, padded or not) 32-byte key.
func DecodeKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != KeySize {
				return nil, fmt.Errorf("%s must decode to %d bytes, got %d", EnvKey, KeySize, len(b))
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("%s isn't valid base64", EnvKey)
}

// EncodeKey is the form used in key files and ATELIER_SECRET_KEY.
func EncodeKey(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

// KeyPath is <dataDir>/secret.key.
func KeyPath(dataDir string) string { return filepath.Join(dataDir, KeyFile) }

// ReadKeyFile reads a key file. A missing file returns os.ErrNotExist.
func ReadKeyFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := DecodeKey(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

// WriteKeyFile writes a key with mode 0600, refusing to overwrite.
func WriteKeyFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(EncodeKey(raw) + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

// Load returns the keyring for a data directory: the primary key from
// ATELIER_SECRET_KEY, else secret.key (created on first use when create is
// true), plus secret.key.next and secret.key.old for decryption only.
// With create false and no key anywhere it returns os.ErrNotExist.
func Load(dataDir string, create bool) (*Keyring, Source, error) {
	var primary []byte
	src := FromFile
	var extra [][]byte
	if v := os.Getenv(EnvKey); strings.TrimSpace(v) != "" {
		k, err := DecodeKey(v)
		if err != nil {
			return nil, "", err
		}
		primary, src = k, FromEnv
	}
	fileKey, err := ReadKeyFile(KeyPath(dataDir))
	switch {
	case err == nil:
		if primary == nil {
			primary = fileKey
		} else {
			extra = append(extra, fileKey)
		}
	case errors.Is(err, os.ErrNotExist):
		if primary == nil {
			if !create {
				return nil, "", os.ErrNotExist
			}
			k := GenerateKey()
			if err := WriteKeyFile(KeyPath(dataDir), k); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return nil, "", fmt.Errorf("create secret key: %w", err)
				}
				// Someone else created it first: use theirs.
				if k, err = ReadKeyFile(KeyPath(dataDir)); err != nil {
					return nil, "", err
				}
			}
			primary = k
		}
	default:
		return nil, "", err
	}
	for _, name := range []string{NextKeyFile, OldKeyFile} {
		k, err := ReadKeyFile(filepath.Join(dataDir, name))
		if err == nil {
			extra = append(extra, k)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
	}
	kr, err := NewKeyring(primary, extra...)
	return kr, src, err
}
