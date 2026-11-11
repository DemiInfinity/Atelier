package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	kr, err := NewKeyring(GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	aad := []byte("repo:1:TOKEN")
	for _, pt := range [][]byte{[]byte("hunter2"), []byte("x"), bytes.Repeat([]byte("ab\n"), 20000)} {
		ct, err := kr.Seal(aad, pt)
		if err != nil {
			t.Fatal(err)
		}
		// Random ciphertext can contain a very short plaintext by chance
		// (one byte appears in ~30 random bytes about 1 time in 9).
		if len(pt) >= 4 && bytes.Contains(ct, pt) {
			t.Fatal("ciphertext contains the plaintext")
		}
		got, err := kr.Open(aad, ct)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("Open = %q, %v", got, err)
		}
	}
	// Same plaintext seals differently each time (random nonce).
	a, _ := kr.Seal(aad, []byte("same"))
	b, _ := kr.Seal(aad, []byte("same"))
	if bytes.Equal(a, b) {
		t.Error("two seals of the same value are identical")
	}
}

func TestTamperDetection(t *testing.T) {
	kr, _ := NewKeyring(GenerateKey())
	aad := []byte("repo:1:TOKEN")
	ct, _ := kr.Seal(aad, []byte("top secret value"))

	for i := range ct {
		bad := append([]byte(nil), ct...)
		bad[i] ^= 0x01
		if _, err := kr.Open(aad, bad); err == nil {
			t.Fatalf("flipping byte %d went unnoticed", i)
		}
	}
	if _, err := kr.Open(aad, ct[:len(ct)-1]); !errors.Is(err, ErrTampered) {
		t.Errorf("truncated: %v", err)
	}
	if _, err := kr.Open(aad, ct[:5]); !errors.Is(err, ErrTampered) {
		t.Errorf("short: %v", err)
	}
	if _, err := kr.Open(nil, nil); !errors.Is(err, ErrTampered) {
		t.Errorf("empty: %v", err)
	}
	// Moving a ciphertext to another secret (different aad) fails.
	if _, err := kr.Open([]byte("repo:1:OTHER"), ct); !errors.Is(err, ErrTampered) {
		t.Errorf("moved: %v", err)
	}
	// A different key can't open it, and says so.
	other, _ := NewKeyring(GenerateKey())
	if _, err := other.Open(aad, ct); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("other key: %v", err)
	}
	// A keyring holding the old key as a secondary one can.
	raw := GenerateKey()
	k1, _ := NewKeyring(raw)
	ct1, _ := k1.Seal(aad, []byte("v"))
	k2, _ := NewKeyring(GenerateKey(), raw)
	if got, err := k2.Open(aad, ct1); err != nil || string(got) != "v" {
		t.Errorf("secondary key: %q %v", got, err)
	}
	if k2.PrimaryID() == k1.PrimaryID() || k1.PrimaryID() != KeyID(raw) {
		t.Error("primary ids")
	}
}

func TestBadKeys(t *testing.T) {
	if _, err := NewKeyring(make([]byte, 16)); err == nil {
		t.Error("16-byte key accepted")
	}
	if _, err := DecodeKey("not base64!!"); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := DecodeKey(EncodeKey(make([]byte, 31))); err == nil {
		t.Error("31-byte key accepted")
	}
	raw := GenerateKey()
	for _, s := range []string{EncodeKey(raw), " " + EncodeKey(raw) + "\n", strings.TrimRight(EncodeKey(raw), "=")} {
		if got, err := DecodeKey(s); err != nil || !bytes.Equal(got, raw) {
			t.Errorf("DecodeKey(%q) = %v", s, err)
		}
	}
}

func TestLoadCreatesKeyFileOnce(t *testing.T) {
	t.Setenv(EnvKey, "")
	dir := t.TempDir()
	if _, _, err := Load(dir, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load without key, create=false: %v", err)
	}
	if _, err := os.Stat(KeyPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("create=false wrote a key")
	}
	kr, src, err := Load(dir, true)
	if err != nil || src != FromFile {
		t.Fatalf("Load: %v %v", src, err)
	}
	fi, err := os.Stat(KeyPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", fi.Mode().Perm())
	}
	data, _ := os.ReadFile(KeyPath(dir))
	raw, err := DecodeKey(string(data))
	if err != nil || KeyID(raw) != kr.PrimaryID() {
		t.Fatalf("key file doesn't hold the primary key: %v", err)
	}
	kr2, _, err := Load(dir, true)
	if err != nil || kr2.PrimaryID() != kr.PrimaryID() {
		t.Fatalf("second Load made a new key: %v", err)
	}
	if err := WriteKeyFile(KeyPath(dir), GenerateKey()); !errors.Is(err, os.ErrExist) {
		t.Errorf("WriteKeyFile overwrote: %v", err)
	}
}

func TestLoadEnvAndExtraKeys(t *testing.T) {
	dir := t.TempDir()
	envKey, fileKey, nextKey, oldKey := GenerateKey(), GenerateKey(), GenerateKey(), GenerateKey()
	if err := WriteKeyFile(KeyPath(dir), fileKey); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeyFile(filepath.Join(dir, NextKeyFile), nextKey); err != nil {
		t.Fatal(err)
	}
	if err := WriteKeyFile(filepath.Join(dir, OldKeyFile), oldKey); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvKey, EncodeKey(envKey))
	kr, src, err := Load(dir, true)
	if err != nil || src != FromEnv || kr.PrimaryID() != KeyID(envKey) {
		t.Fatalf("env key not primary: %v %v", src, err)
	}
	for _, raw := range [][]byte{fileKey, nextKey, oldKey} {
		k, _ := NewKeyring(raw)
		ct, _ := k.Seal([]byte("a"), []byte("v"))
		if _, err := kr.Open([]byte("a"), ct); err != nil {
			t.Errorf("can't open with extra key: %v", err)
		}
	}
	t.Setenv(EnvKey, "bad")
	if _, _, err := Load(dir, true); err == nil {
		t.Error("bad env key accepted")
	}
}
