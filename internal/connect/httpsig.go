package connect

// HTTP Signatures (draft-cavage-http-signatures-12), the way ActivityPub
// servers use them. Outgoing requests are signed with the studio's Ed25519
// key as algorithm "hs2019" over (request-target) host date digest (POST) or
// (request-target) host date (GET). Incoming signatures may be hs2019 with an
// Ed25519 or RSA key, "ed25519", or "rsa-sha256".

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Digest is the Digest header for a body: SHA-256=<base64>.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "SHA-256=" + base64.StdEncoding.EncodeToString(sum[:])
}

// DigestMatches reports whether a Digest header carries the body's SHA-256.
func DigestMatches(header string, body []byte) bool {
	want := Digest(body)
	for _, d := range strings.Split(header, ",") {
		d = strings.TrimSpace(d)
		if len(d) > 8 && strings.EqualFold(d[:8], "SHA-256=") &&
			subtle.ConstantTimeCompare([]byte(d[8:]), []byte(want[8:])) == 1 {
			return true
		}
	}
	return false
}

// SignRequest adds Date, Digest (for requests with a body) and Signature to req.
func SignRequest(req *http.Request, keyID string, k *Key, body []byte, now time.Time) error {
	if req.Host == "" {
		req.Host = req.URL.Host
	}
	req.Header.Set("Date", now.UTC().Format(http.TimeFormat))
	headers := []string{"(request-target)", "host", "date"}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		req.Header.Set("Digest", Digest(body))
		headers = append(headers, "digest")
	}
	str, err := signingString(req, headers, 0, 0)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(k.Priv, []byte(str))
	req.Header.Set("Signature", fmt.Sprintf(`keyId="%s",algorithm="hs2019",headers="%s",signature="%s"`,
		keyID, strings.Join(headers, " "), base64.StdEncoding.EncodeToString(sig)))
	return nil
}

// SigParams is a parsed Signature header.
type SigParams struct {
	KeyID     string
	Algorithm string
	Headers   []string
	Signature []byte
	Created   int64
	Expires   int64
}

// ParseSignature parses a Signature header (or the value of an Authorization:
// Signature header).
func ParseSignature(h string) (SigParams, error) {
	var p SigParams
	h = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "Signature "))
	if h == "" || len(h) > 4096 {
		return p, errors.New("missing or oversized Signature header")
	}
	seen := map[string]bool{}
	for len(h) > 0 {
		eq := strings.IndexByte(h, '=')
		if eq <= 0 {
			return p, errors.New("malformed Signature header")
		}
		name := strings.ToLower(strings.TrimSpace(h[:eq]))
		h = h[eq+1:]
		var val string
		if strings.HasPrefix(h, `"`) {
			end := strings.IndexByte(h[1:], '"')
			if end < 0 {
				return p, errors.New("unterminated value in Signature header")
			}
			val, h = h[1:1+end], h[2+end:]
		} else {
			end := strings.IndexByte(h, ',')
			if end < 0 {
				end = len(h)
			}
			val, h = strings.TrimSpace(h[:end]), h[end:]
		}
		h = strings.TrimLeft(strings.TrimSpace(h), ",")
		h = strings.TrimSpace(h)
		if seen[name] {
			return p, errors.New("repeated parameter in Signature header")
		}
		seen[name] = true
		switch name {
		case "keyid":
			p.KeyID = val
		case "algorithm":
			p.Algorithm = strings.ToLower(val)
		case "headers":
			p.Headers = strings.Fields(strings.ToLower(val))
		case "signature":
			b, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				return p, errors.New("signature is not base64")
			}
			p.Signature = b
		case "created":
			p.Created, _ = strconv.ParseInt(val, 10, 64)
		case "expires":
			p.Expires, _ = strconv.ParseInt(val, 10, 64)
		}
	}
	if p.KeyID == "" || len(p.Signature) == 0 {
		return p, errors.New("Signature header needs keyId and signature")
	}
	if len(p.Headers) == 0 {
		p.Headers = []string{"date"} // the draft's default
	}
	return p, nil
}

// signingString builds the string that is signed.
func signingString(req *http.Request, headers []string, created, expires int64) (string, error) {
	var b strings.Builder
	for i, h := range headers {
		if i > 0 {
			b.WriteByte('\n')
		}
		var v string
		switch h {
		case "(request-target)":
			v = strings.ToLower(req.Method) + " " + req.URL.RequestURI()
		case "(created)":
			if created == 0 {
				return "", errors.New("(created) signed without a created parameter")
			}
			v = strconv.FormatInt(created, 10)
		case "(expires)":
			if expires == 0 {
				return "", errors.New("(expires) signed without an expires parameter")
			}
			v = strconv.FormatInt(expires, 10)
		case "host":
			v = req.Host
			if v == "" {
				v = req.URL.Host
			}
		default:
			raw := req.Header.Values(h)
			if len(raw) == 0 {
				return "", fmt.Errorf("signed header %q is missing", h)
			}
			vals := make([]string, len(raw))
			for j := range raw {
				vals[j] = strings.TrimSpace(raw[j])
			}
			v = strings.Join(vals, ", ")
		}
		b.WriteString(h)
		b.WriteString(": ")
		b.WriteString(v)
	}
	return b.String(), nil
}

// Verification errors.
var (
	ErrSigMissing  = errors.New("request is not signed")
	ErrSigHeaders  = errors.New("signature doesn't cover the required headers")
	ErrSigClock    = errors.New("request date is outside the allowed clock skew")
	ErrSigDigest   = errors.New("digest doesn't match the body")
	ErrSigInvalid  = errors.New("signature doesn't verify")
	ErrSigAlgorith = errors.New("unsupported signature algorithm")
)

// VerifyRequest checks a parsed signature against req (and body, for
// requests with one) and the signer's public key. It requires the signature
// to cover (request-target), host and date, plus digest for requests with a
// body; the Date (or created) must be within skew of now; and the Digest must
// match the body.
func VerifyRequest(req *http.Request, body []byte, pub crypto.PublicKey, p SigParams, now time.Time, skew time.Duration) error {
	covered := map[string]bool{}
	for _, h := range p.Headers {
		covered[h] = true
	}
	hasBody := req.Method != http.MethodGet && req.Method != http.MethodHead
	if !covered["(request-target)"] || !covered["host"] || (!covered["date"] && !covered["(created)"]) ||
		(hasBody && !covered["digest"]) {
		return ErrSigHeaders
	}
	// Clock: the Date header, or (created)/(expires).
	if covered["date"] {
		t, err := http.ParseTime(req.Header.Get("Date"))
		if err != nil {
			return ErrSigClock
		}
		if d := now.Sub(t); d > skew || d < -skew {
			return ErrSigClock
		}
	}
	if covered["(created)"] {
		if d := now.Sub(time.Unix(p.Created, 0)); d > skew || d < -skew {
			return ErrSigClock
		}
	}
	if p.Expires != 0 && now.After(time.Unix(p.Expires, 0)) {
		return ErrSigClock
	}
	if hasBody {
		if !DigestMatches(req.Header.Get("Digest"), body) {
			return ErrSigDigest
		}
	}
	str, err := signingString(req, p.Headers, p.Created, p.Expires)
	if err != nil {
		return ErrSigHeaders
	}
	switch k := pub.(type) {
	case ed25519.PublicKey:
		if p.Algorithm != "" && p.Algorithm != "hs2019" && p.Algorithm != "ed25519" {
			return ErrSigAlgorith
		}
		if !ed25519.Verify(k, []byte(str), p.Signature) {
			return ErrSigInvalid
		}
	case *rsa.PublicKey:
		if p.Algorithm != "" && p.Algorithm != "hs2019" && p.Algorithm != "rsa-sha256" {
			return ErrSigAlgorith
		}
		sum := sha256.Sum256([]byte(str))
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], p.Signature) != nil {
			return ErrSigInvalid
		}
	default:
		return ErrSigAlgorith
	}
	return nil
}
