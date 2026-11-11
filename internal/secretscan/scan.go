package secretscan

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Finding is one detected secret.
type Finding struct {
	RuleID      string
	RuleName    string
	Path        string // file path ("" for logs)
	Line        int    // 1-based line number (in the file, or in the log)
	Commit      string // commit that added it (diff scans)
	Fingerprint string // stable hash of the secret value, used by allowlists and bypasses
	Redacted    string // first 4 and last 2 characters, never the whole value
	Encoded     string // "base64" when found inside a base64 string, "joined" for split lines
	secret      string
}

// Secret returns the full value. Use it only to mask the value (e.g. in
// stored logs); never store or display it.
func (f Finding) Secret() string { return f.secret }

// Fingerprint is the stable identifier of a secret value: a truncated SHA-256.
func Fingerprint(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])[:24]
}

// Redact keeps the first 4 and last 2 characters of a secret.
func Redact(secret string) string {
	if len(secret) <= 8 {
		if len(secret) <= 2 {
			return "…"
		}
		return secret[:2] + "…"
	}
	return secret[:4] + "…" + secret[len(secret)-2:]
}

// Line is one line of text to scan, with its 1-based number.
type Line struct {
	No   int
	Text string
}

// Limits bound the work done per file and per line.
type Limits struct {
	MaxFileBytes int64 // files (or added text per file in a diff) above this are skipped
	MaxLineBytes int   // longer lines are only scanned up to this many bytes
	MaxFindings  int   // per file
}

// DefaultLimits are used when a Scanner's limits are zero.
var DefaultLimits = Limits{MaxFileBytes: 2 << 20, MaxLineBytes: 8 << 10, MaxFindings: 50}

// Scanner applies rules and an allowlist.
type Scanner struct {
	Rules  []*Rule
	Allow  *Allowlist
	Limits Limits
}

// New returns a scanner with the default rules and limits.
func New(allow *Allowlist) *Scanner {
	return &Scanner{Rules: DefaultRules, Allow: allow, Limits: DefaultLimits}
}

func (s *Scanner) limits() Limits {
	l := s.Limits
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = DefaultLimits.MaxFileBytes
	}
	if l.MaxLineBytes <= 0 {
		l.MaxLineBytes = DefaultLimits.MaxLineBytes
	}
	if l.MaxFindings <= 0 {
		l.MaxFindings = DefaultLimits.MaxFindings
	}
	return l
}

// IsBinary reports whether data looks binary (a NUL byte near the start).
func IsBinary(data []byte) bool {
	probe := data
	if len(probe) > 8000 {
		probe = probe[:8000]
	}
	return bytes.IndexByte(probe, 0) >= 0
}

// skippedPaths are never scanned: vendored code, lockfiles full of hashes, images.
var skippedSuffixes = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".pdf", ".zip", ".gz", ".tgz", ".jar",
	".woff", ".woff2", ".ttf", ".otf", ".mp4", ".mov", ".mp3", ".wasm", ".so", ".dylib", ".dll", ".exe",
	"go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", ".min.js.map", ".js.map"}

// SkipPath reports whether a path is never scanned (binary types, lockfiles,
// allowlisted paths).
func (s *Scanner) SkipPath(path string) bool {
	l := strings.ToLower(path)
	for _, suf := range skippedSuffixes {
		if strings.HasSuffix(l, suf) {
			return true
		}
	}
	return s.Allow.PathAllowed(path)
}

// ScanFile scans a whole file. Binary and over-large files are skipped.
func (s *Scanner) ScanFile(path string, data []byte) []Finding {
	if s.SkipPath(path) || int64(len(data)) > s.limits().MaxFileBytes || IsBinary(data) {
		return nil
	}
	text := string(data)
	lines := make([]Line, 0, strings.Count(text, "\n")+1)
	for i, t := range strings.Split(text, "\n") {
		lines = append(lines, Line{No: i + 1, Text: strings.TrimSuffix(t, "\r")})
	}
	return s.ScanLines(path, lines)
}

// ScanText scans one line of free text (a log line). Path is left empty.
func (s *Scanner) ScanText(text string) []Finding {
	return s.ScanLines("", []Line{{No: 1, Text: text}})
}

// ScanLines scans lines from one file (all of it, or the lines a commit
// added). Consecutive line numbers are treated as adjacent, so secrets split
// over several lines are found too.
func (s *Scanner) ScanLines(path string, lines []Line) []Finding {
	if path != "" && s.SkipPath(path) {
		return nil
	}
	lim := s.limits()
	var out []Finding
	seen := map[string]bool{}
	add := func(f Finding) bool {
		if seen[f.Fingerprint] || s.Allow.FingerprintAllowed(f.Fingerprint) {
			return true
		}
		seen[f.Fingerprint] = true
		f.Path = path
		out = append(out, f)
		return len(out) < lim.MaxFindings
	}
	var total int64
	for i, ln := range lines {
		total += int64(len(ln.Text))
		if total > lim.MaxFileBytes {
			break
		}
		text := ln.Text
		if len(text) > lim.MaxLineBytes {
			text = text[:lim.MaxLineBytes]
		}
		var next string
		if i+1 < len(lines) && lines[i+1].No == ln.No+1 {
			next = lines[i+1].Text
		}
		for _, f := range s.scanLine(text, next, true) {
			f.Line = ln.No
			if !add(f) {
				return out
			}
		}
		// Secrets split over several lines.
		if joined, ok := joinSplit(lines, i, lim.MaxLineBytes); ok {
			for _, f := range s.scanLine(joined, "", false) {
				f.Line, f.Encoded = ln.No, "joined"
				if !add(f) {
					return out
				}
			}
		}
	}
	return out
}

// scanLine runs every rule over one line. next is the following line (for
// private keys). plain is false for decoded or joined text, where only the
// specific rules run.
func (s *Scanner) scanLine(text, next string, plain bool) []Finding {
	if len(text) < 8 {
		return nil
	}
	lower := strings.ToLower(text)
	var out []Finding
	for _, r := range s.Rules {
		if r.generic && !plain {
			continue
		}
		if !hasAny(lower, r.keywords) || r.generic && !hasRun(text, 24, quotedRunByte) {
			continue
		}
		for _, ix := range r.re.FindAllStringSubmatchIndex(text, 8) {
			secret := text[ix[2*r.group]:ix[2*r.group+1]]
			if !r.plausible(secret) {
				continue
			}
			f := Finding{RuleID: r.ID, RuleName: r.Name, secret: secret}
			if r.pem {
				// A header alone (docs, code that checks for one) is noise:
				// the key body must follow, on the next line or after "\n".
				body := pemBody(text[ix[1]:], next)
				if body == "" {
					continue
				}
				f.Fingerprint = Fingerprint(secret + "\n" + body)
				f.Redacted = secret + " " + Redact(body)
				f.secret = body
			} else {
				f.Fingerprint = Fingerprint(secret)
				f.Redacted = Redact(secret)
			}
			out = append(out, f)
		}
	}
	if plain && hasRun(text, 28, b64Byte) {
		out = append(out, s.scanBase64(text)...)
	}
	return out
}

var b64RunRe = regexp.MustCompile(`^[A-Za-z0-9+/=]+`)

// pemBody returns the start of a private key's body: the rest of the line
// after a literal "\n" (keys embedded in JSON or env strings), or the next line.
func pemBody(rest, next string) string {
	rest = strings.TrimLeft(rest, ` "'`)
	if b, ok := strings.CutPrefix(rest, `\n`); ok {
		if run := b64RunRe.FindString(b); len(run) >= 16 {
			return run
		}
	}
	n := strings.TrimLeft(strings.TrimSpace(next), `"'`)
	if strings.HasPrefix(n, "Proc-Type:") {
		return n
	}
	if run := b64RunRe.FindString(n); len(run) >= 16 {
		return run
	}
	return ""
}

func b64Byte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '_' || c == '-'
}

func quotedRunByte(c byte) bool { return b64Byte(c) || c == '=' || c == '.' || c == '~' }

// hasRun reports whether text has n consecutive bytes accepted by ok: a cheap
// test before running the expensive expressions.
func hasRun(text string, n int, ok func(byte) bool) bool {
	run := 0
	for i := 0; i < len(text); i++ {
		if ok(text[i]) {
			run++
			if run >= n {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

func hasAny(lower string, kws []string) bool {
	if len(kws) == 0 {
		return true
	}
	for _, k := range kws {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

var b64Re = regexp.MustCompile(`[A-Za-z0-9+/_\-]{28,}={0,2}`)

// scanBase64 decodes long base64 runs and scans what's inside.
func (s *Scanner) scanBase64(text string) []Finding {
	var out []Finding
	for i, cand := range b64Re.FindAllString(text, 8) {
		if i >= 8 {
			break
		}
		dec, ok := decodeB64(cand)
		if !ok {
			continue
		}
		for _, f := range s.scanLine(dec, "", false) {
			f.Encoded = "base64"
			f.RuleName += " (base64-encoded)"
			// Mask the encoded form: that's what appears in the text.
			f.secret = cand
			out = append(out, f)
		}
	}
	return out
}

func decodeB64(s string) (string, bool) {
	var b []byte
	var err error
	trimmed := strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "-_") {
		b, err = base64.RawURLEncoding.DecodeString(trimmed)
	} else {
		b, err = base64.RawStdEncoding.DecodeString(trimmed)
	}
	if err != nil || len(b) < 16 || !utf8.Valid(b) {
		return "", false
	}
	printable := 0
	for _, c := range b {
		if c >= 0x20 && c < 0x7f || c == '\n' || c == '\t' {
			printable++
		}
	}
	if printable*10 < len(b)*9 {
		return "", false
	}
	return string(b), true
}

var (
	// a line ending in a string literal, optionally followed by a continuation operator
	litEndRe = regexp.MustCompile(`"([A-Za-z0-9+/=_\-.:~]*)"\s*(?:\+|\.|\\|&|\|\|)?\s*$`)
	// a line that is only a string literal (a continuation)
	litOnlyRe = regexp.MustCompile(`^\s*(?:\+|\.|&|\|\|)?\s*"([A-Za-z0-9+/=_\-.:~]*)"\s*(?:\+|\.|\\|&|\|\||;|,|\))*\s*$`)
)

// joinSplit joins a secret that starts on lines[i] and continues on the
// following lines, either as concatenated string literals
//
//	key := "sk-ant-api03-abc" +
//		"def…"
//
// or with a trailing backslash. It returns the joined text when line i
// starts such a group.
func joinSplit(lines []Line, i, maxBytes int) (string, bool) {
	const maxLines = 12
	cur := lines[i].Text
	contiguous := func(j int) bool { return j < len(lines) && lines[j].No == lines[j-1].No+1 }
	if !contiguous(i + 1) {
		return "", false
	}
	// Trailing backslash: join raw text.
	if strings.HasSuffix(cur, "\\") && !strings.HasSuffix(cur, "\\\\") {
		var b strings.Builder
		b.WriteString(strings.TrimSuffix(cur, "\\"))
		for j := i + 1; j < len(lines) && j <= i+maxLines && contiguous(j); j++ {
			t := strings.TrimLeft(lines[j].Text, " \t")
			cont := strings.HasSuffix(t, "\\")
			b.WriteString(strings.TrimSuffix(t, "\\"))
			if !cont || b.Len() > maxBytes {
				break
			}
		}
		return b.String(), true
	}
	m := litEndRe.FindStringSubmatch(cur)
	if m == nil || !litOnlyRe.MatchString(lines[i+1].Text) {
		return "", false
	}
	// Keep the start of the line (it may hold the variable name the generic
	// rule would want, but joined text only runs specific rules).
	var b strings.Builder
	b.WriteString(m[1])
	for j := i + 1; j < len(lines) && j <= i+maxLines && contiguous(j); j++ {
		mm := litOnlyRe.FindStringSubmatch(lines[j].Text)
		if mm == nil {
			break
		}
		b.WriteString(mm[1])
		if b.Len() > maxBytes {
			break
		}
	}
	return b.String(), true
}
