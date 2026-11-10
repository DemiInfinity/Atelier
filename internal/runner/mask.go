package runner

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Masker replaces secret values (and common encodings of them) with "***".
type Masker struct {
	mu     sync.RWMutex
	values map[string]bool
	rep    *strings.Replacer
}

func NewMasker() *Masker { return &Masker{values: map[string]bool{}} }

// Add registers a secret value plus its base64 forms (whole and as part of a
// longer base64 string at each byte alignment), JSON-escaped and URL-encoded forms,
// and each line of a multi-line value.
func (m *Masker) Add(v string) {
	if strings.TrimSpace(v) == "" {
		return
	}
	forms := []string{v}
	if strings.ContainsAny(v, "\r\n") {
		for _, l := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if l = strings.TrimSpace(l); l != "" {
				forms = append(forms, l)
			}
		}
	}
	b := []byte(v)
	forms = append(forms,
		base64.StdEncoding.EncodeToString(b),
		base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b),
		base64.RawURLEncoding.EncodeToString(b),
		url.QueryEscape(v), url.PathEscape(v))
	if j, err := json.Marshal(v); err == nil {
		forms = append(forms, string(j[1:len(j)-1]))
	}
	// base64 of the value embedded in a longer string (e.g. "user:secret" in a
	// basic-auth header): keep only the characters fully determined by v.
	for off := 0; off < 3; off++ {
		enc := base64.RawStdEncoding.EncodeToString(append(make([]byte, off), b...))
		start := (8*off + 5) / 6
		end := 8 * (off + len(b)) / 6
		if end-start >= 6 && end <= len(enc) {
			s := enc[start:end]
			forms = append(forms, s, strings.NewReplacer("+", "-", "/", "_").Replace(s))
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for _, f := range forms {
		if f != "" && !m.values[f] {
			m.values[f] = true
			changed = true
		}
	}
	if changed {
		vals := make([]string, 0, len(m.values))
		for k := range m.values {
			vals = append(vals, k)
		}
		sort.Slice(vals, func(i, j int) bool {
			if len(vals[i]) != len(vals[j]) {
				return len(vals[i]) > len(vals[j])
			}
			return vals[i] < vals[j]
		})
		args := make([]string, 0, 2*len(vals))
		for _, k := range vals {
			args = append(args, k, "***")
		}
		m.rep = strings.NewReplacer(args...)
	}
}

// Mask returns s with every registered value replaced.
func (m *Masker) Mask(s string) string {
	m.mu.RLock()
	rep := m.rep
	m.mu.RUnlock()
	if rep == nil {
		return s
	}
	return rep.Replace(s)
}

// MaskMap masks every value of a map (nil stays nil).
func (m *Masker) MaskMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = m.Mask(v)
	}
	return out
}
