package sshd

import (
	"crypto/rsa"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

// PublicKey is a validated, normalised public key ready to store.
type PublicKey struct {
	Type        string // e.g. "ssh-ed25519"
	Fingerprint string // "SHA256:…"
	Normalized  string // "type base64" (no comment, no options)
	Comment     string
}

var allowedKeyTypes = map[string]bool{
	ssh.KeyAlgoED25519:    true,
	ssh.KeyAlgoSKED25519:  true,
	ssh.KeyAlgoECDSA256:   true,
	ssh.KeyAlgoECDSA384:   true,
	ssh.KeyAlgoECDSA521:   true,
	ssh.KeyAlgoSKECDSA256: true,
	ssh.KeyAlgoRSA:        true,
}

// ParsePublicKey validates one pasted public key in authorized_keys form
// (as found in ~/.ssh/id_ed25519.pub). Options such as `command="…"` are
// refused, as are certificates, DSA and RSA keys shorter than 2048 bits.
func ParsePublicKey(text string) (PublicKey, error) {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\ufeff"))
	if text == "" {
		return PublicKey{}, errors.New("paste a public key")
	}
	if strings.Contains(text, "PRIVATE KEY") {
		return PublicKey{}, errors.New("that looks like a private key. Paste the public key (the .pub file) instead, and keep the private key secret")
	}
	if lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n"); len(lines) > 1 {
		return PublicKey{}, errors.New("paste one key at a time")
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil {
		return PublicKey{}, errors.New("that isn't a valid SSH public key. It should start with ssh-ed25519, ecdsa-sha2-… or ssh-rsa")
	}
	if len(options) > 0 || len(strings.TrimSpace(string(rest))) > 0 {
		return PublicKey{}, errors.New("paste just the key, without authorized_keys options")
	}
	if !allowedKeyTypes[key.Type()] {
		return PublicKey{}, errors.New("key type " + key.Type() + " isn't supported. Use an ed25519 key (ssh-keygen -t ed25519)")
	}
	if key.Type() == ssh.KeyAlgoRSA {
		if ck, ok := key.(ssh.CryptoPublicKey); ok {
			if rk, ok := ck.CryptoPublicKey().(*rsa.PublicKey); ok && rk.N.BitLen() < 2048 {
				return PublicKey{}, errors.New("RSA keys must be at least 2048 bits. An ed25519 key is better (ssh-keygen -t ed25519)")
			}
		}
	}
	return PublicKey{
		Type:        key.Type(),
		Fingerprint: ssh.FingerprintSHA256(key),
		Normalized:  strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))),
		Comment:     strings.TrimSpace(comment),
	}, nil
}
