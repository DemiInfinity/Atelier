package web

// Web Push wire format, standard library only:
//
//   RFC 8291  Message Encryption for Web Push (aes128gcm, RFC 8188)
//   RFC 8292  VAPID: the server proves who it is with an ES256 JWT
//
// Written here rather than pulled in as a module: it is ~150 lines over
// crypto/ecdh, crypto/hkdf, crypto/aes and crypto/ecdsa, it keeps the HTTP
// client (and so the SSRF guard) entirely ours, and the tests decrypt and
// verify independently of this code.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

var b64url = base64.RawURLEncoding

// pushB64 decodes base64url with or without padding (browsers send unpadded,
// some libraries pad).
func pushB64(s string) ([]byte, error) {
	for len(s)%4 != 0 && len(s) > 0 && s[len(s)-1] == '=' {
		s = s[:len(s)-1]
	}
	if b, err := b64url.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// pushRecordSize is the RFC 8188 record size we advertise. Payloads are far
// smaller, so there is exactly one record.
const pushRecordSize = 4096

// pushMaxPlaintext keeps the encrypted body under the 4 KB every push
// service accepts (header 86 bytes + padding delimiter + 16-byte tag).
const pushMaxPlaintext = 3000

// pushEncrypt encrypts plaintext for a subscription (RFC 8291 §3.4) and
// returns the aes128gcm body. uaPublic is the browser's P-256 key (65 bytes,
// uncompressed) and authSecret its 16-byte secret.
func pushEncrypt(plaintext, uaPublic, authSecret []byte) ([]byte, error) {
	return pushEncryptWith(rand.Reader, plaintext, uaPublic, authSecret)
}

func pushEncryptWith(rnd io.Reader, plaintext, uaPublic, authSecret []byte) ([]byte, error) {
	if len(plaintext) > pushMaxPlaintext {
		return nil, errors.New("push payload too large")
	}
	if len(authSecret) != 16 {
		return nil, errors.New("auth secret must be 16 bytes")
	}
	curve := ecdh.P256()
	ua, err := curve.NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("browser key: %w", err)
	}
	as, err := curve.GenerateKey(rnd)
	if err != nil {
		return nil, err
	}
	asPublic := as.PublicKey().Bytes()
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rnd, salt); err != nil {
		return nil, err
	}
	cek, nonce, err := pushKeys(shared, authSecret, uaPublic, asPublic, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// One record, so it is the last: the delimiter is 0x02, no padding.
	record := append(append([]byte(nil), plaintext...), 0x02)
	header := make([]byte, 0, 16+4+1+len(asPublic))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, pushRecordSize)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)
	return gcm.Seal(header, nonce, record, nil), nil
}

// pushKeys derives the content key and nonce (RFC 8291 §3.3–3.4, RFC 8188 §2.2).
func pushKeys(shared, authSecret, uaPublic, asPublic, salt []byte) (cek, nonce []byte, err error) {
	keyInfo := "WebPush: info\x00" + string(uaPublic) + string(asPublic)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, keyInfo, 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}

// ---------- VAPID ----------

// vapidKeys is the server's VAPID identity. The private key never leaves
// this struct: not in pages, logs or errors.
type vapidKeys struct {
	priv *ecdsa.PrivateKey
	pub  []byte // uncompressed P-256 point, 65 bytes
}

// PublicKey is the applicationServerKey the browser subscribes with.
func (k *vapidKeys) PublicKey() string { return b64url.EncodeToString(k.pub) }

// String never prints the private key.
func (k *vapidKeys) String() string { return "vapid(" + k.PublicKey() + ")" }

const vapidFile = "vapid.pem"

// loadOrCreateVAPID reads <dir>/vapid.pem, creating a fresh P-256 key (mode
// 0600, written atomically) the first time. A restored backup leaves the file
// group-readable, so the mode is tightened on every load.
func loadOrCreateVAPID(dir string) (*vapidKeys, error) {
	path := filepath.Join(dir, vapidFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return nil, err
		}
		b = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		tmp, err := os.CreateTemp(dir, ".vapid-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		if err := tmp.Chmod(0o600); err != nil {
			tmp.Close()
			return nil, err
		}
		if _, err := tmp.Write(b); err != nil {
			tmp.Close()
			return nil, err
		}
		if err := tmp.Close(); err != nil {
			return nil, err
		}
		// Link fails if another process created the key meanwhile: use theirs.
		if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if b, err = os.ReadFile(path); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm() != 0o600 {
		_ = os.Chmod(path, 0o600)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("vapid key file is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, errors.New("vapid key file is unreadable")
	}
	priv, ok := k.(*ecdsa.PrivateKey)
	if !ok || priv.Curve != elliptic.P256() {
		return nil, errors.New("vapid key is not a P-256 key")
	}
	pk, err := priv.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	return &vapidKeys{priv: priv, pub: pk.Bytes()}, nil
}

// vapidJWTTTL is how long a VAPID token is valid (RFC 8292 allows ≤ 24h).
const vapidJWTTTL = 12 * time.Hour

// authorization is the VAPID Authorization header for a push service origin
// ("https://host"), RFC 8292 §3.
func (k *vapidKeys) authorization(audience, subject string, now time.Time) (string, error) {
	hdr, _ := json.Marshal(map[string]string{"typ": "JWT", "alg": "ES256"})
	claims, _ := json.Marshal(map[string]any{"aud": audience, "exp": now.Add(vapidJWTTTL).Unix(), "sub": subject})
	signing := b64url.EncodeToString(hdr) + "." + b64url.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + b64url.EncodeToString(sig) + ", k=" + k.PublicKey(), nil
}
