// Package connect is the protocol side of Atelier Connect: studios (separate
// Atelier instances) following each other, starring and offering branches,
// with no central server. It speaks the ActivityPub / ForgeFed shapes where
// practical so other software can interoperate later:
//
//   - identity: one Ed25519 key pair per studio (keys.go), actor documents
//     (activity.go) and WebFinger (webfinger.go);
//   - transport: every POST is signed with an HTTP Signature (draft-cavage,
//     hs2019 with Ed25519; rsa-sha256 is verified too) over (request-target),
//     host, date and digest (httpsig.go);
//   - fetching: an SSRF-guarded client that refuses private and loopback
//     addresses unless told otherwise, HTTPS only, with timeouts and size
//     caps (client.go).
//
// The web package holds the studio's state, inbox and delivery queue.
package connect

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// KeyFile is where the studio key lives, relative to the data directory.
// The directory is included in backups (backup.RegisterDataDir).
const (
	KeyDir  = "connect"
	KeyFile = "studio-key.pem"
)

// Key is a studio's signing key.
type Key struct {
	Priv ed25519.PrivateKey
	Pub  ed25519.PublicKey
}

// LoadOrCreateKey reads the PEM (PKCS #8) private key at path, creating a new
// Ed25519 key with mode 0600 if there is none. A key file readable by others
// (a restore writes 0640) is tightened back to 0600.
func LoadOrCreateKey(path string) (*Key, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return createKey(path)
	}
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(path, 0o600)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("connect: %s is not a PEM private key", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("connect: %s: %w", path, err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("connect: %s is not an Ed25519 key", path)
	}
	return &Key{Priv: priv, Pub: priv.Public().(ed25519.PublicKey)}, nil
}

func createKey(path string) (*Key, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	// Link rather than rename: if another process created the key first, keep theirs.
	if err := os.Link(tmp, path); err != nil {
		os.Remove(tmp)
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreateKey(path)
		}
		return nil, err
	}
	os.Remove(tmp)
	return &Key{Priv: priv, Pub: pub}, nil
}

// PublicPEM is the public key as a PKIX PEM block (publicKeyPem).
func (k *Key) PublicPEM() string {
	der, _ := x509.MarshalPKIXPublicKey(k.Pub)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// ParsePublicKeyPEM reads a remote actor's publicKeyPem: Ed25519 or RSA
// (2048 bits or more), PKIX or PKCS #1.
func ParsePublicKeyPEM(s string) (crypto.PublicKey, error) {
	if len(s) > 16<<10 {
		return nil, errors.New("public key too large")
	}
	blk, _ := pem.Decode([]byte(s))
	if blk == nil {
		return nil, errors.New("no PEM block in publicKeyPem")
	}
	var k any
	var err error
	switch blk.Type {
	case "PUBLIC KEY":
		k, err = x509.ParsePKIXPublicKey(blk.Bytes)
	case "RSA PUBLIC KEY":
		k, err = x509.ParsePKCS1PublicKey(blk.Bytes)
	default:
		return nil, fmt.Errorf("unsupported PEM block %q", blk.Type)
	}
	if err != nil {
		return nil, err
	}
	switch pk := k.(type) {
	case ed25519.PublicKey:
		return pk, nil
	case *rsa.PublicKey:
		if pk.N.BitLen() < 2048 {
			return nil, errors.New("RSA key shorter than 2048 bits")
		}
		return pk, nil
	}
	return nil, errors.New("unsupported key type")
}
