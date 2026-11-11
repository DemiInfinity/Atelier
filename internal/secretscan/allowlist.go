package secretscan

import (
	"errors"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// AllowlistPath is the repository file that lists ignored paths and secrets:
//
//	# .atelier/secret-scanning.yml
//	paths:
//	  - testdata/**
//	  - "*.example.env"
//	fingerprints:
//	  - 3f2a9c0d1e7b44c2a1f00b9e   # test key used by the fixtures
const AllowlistPath = ".atelier/secret-scanning.yml"

// MaxAllowlistBytes caps the allowlist file.
const MaxAllowlistBytes = 64 << 10

// Allowlist says which paths and fingerprints are never reported. A nil
// Allowlist allows nothing.
type Allowlist struct {
	Paths        []string
	Fingerprints []string
	paths        []*regexp.Regexp
}

type allowFile struct {
	Paths        []string `yaml:"paths"`
	Fingerprints []string `yaml:"fingerprints"`
}

// ParseAllowlist reads .atelier/secret-scanning.yml.
func ParseAllowlist(data []byte) (*Allowlist, error) {
	if len(data) > MaxAllowlistBytes {
		return nil, errors.New("secret-scanning.yml is too large")
	}
	var f allowFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	a := &Allowlist{}
	for _, p := range f.Paths {
		p = strings.TrimSpace(p)
		if p == "" || len(a.Paths) >= 500 {
			continue
		}
		a.Paths = append(a.Paths, p)
		a.paths = append(a.paths, globRe(p))
	}
	for _, fp := range f.Fingerprints {
		fp = strings.ToLower(strings.TrimSpace(fp))
		if len(fp) >= 12 && isHex(fp) && len(a.Fingerprints) < 5000 {
			a.Fingerprints = append(a.Fingerprints, fp)
		}
	}
	return a, nil
}

// PathAllowed reports whether a path is excluded from scanning.
func (a *Allowlist) PathAllowed(p string) bool {
	if a == nil || p == "" {
		return false
	}
	p = strings.TrimPrefix(p, "/")
	for _, re := range a.paths {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// FingerprintAllowed reports whether a secret is allowlisted. Listed
// fingerprints may be shortened to a prefix of at least 12 characters.
func (a *Allowlist) FingerprintAllowed(fp string) bool {
	if a == nil {
		return false
	}
	for _, x := range a.Fingerprints {
		if strings.HasPrefix(fp, x) || strings.HasPrefix(x, fp) && len(fp) >= 12 {
			return true
		}
	}
	return false
}

// globRe turns a gitignore-like pattern into a regular expression: "**"
// crosses directories, "*" and "?" don't, a trailing "/" matches everything
// below, and a pattern without "/" matches the file name anywhere.
func globRe(p string) *regexp.Regexp {
	anywhere := !strings.Contains(strings.TrimSuffix(p, "/"), "/")
	p = strings.TrimPrefix(p, "/")
	if strings.HasSuffix(p, "/") {
		p += "**"
	}
	var b strings.Builder
	b.WriteString("^")
	if anywhere {
		b.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '*' && i+1 < len(p) && p[i+1] == '*':
			i++
			if i+1 < len(p) && p[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile(`^\x00$`) // matches nothing
	}
	return re
}
