package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// hashFiles implements hashFiles(): the SHA-256 of the SHA-256 of every
// matching file under the workspace, in path order; "" when nothing matches.
// Patterns are globs relative to the workspace with *, ?, ** and !negation.
func hashFiles(ws string, patterns []string) (string, error) {
	type pat struct {
		re  *regexp.Regexp
		neg bool
	}
	var pats []pat
	for _, p := range patterns {
		for _, line := range strings.Split(p, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			neg := strings.HasPrefix(line, "!")
			line = strings.TrimPrefix(line, "!")
			line = filepath.ToSlash(line)
			if filepath.IsAbs(line) || strings.HasPrefix(line, "/") {
				if rel, err := filepath.Rel(ws, filepath.FromSlash(line)); err == nil {
					line = filepath.ToSlash(rel)
				}
			}
			line = strings.TrimPrefix(line, "./")
			pats = append(pats, pat{re: globRegexp(line), neg: neg})
		}
	}
	var files []string
	err := filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" && p != ws {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(ws, p)
		rel = filepath.ToSlash(rel)
		match := false
		for _, pt := range pats {
			if pt.re.MatchString(rel) {
				match = !pt.neg
			}
		}
		if match {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", nil
	}
	sort.Strings(files)
	outer := sha256.New()
	for _, f := range files {
		h := sha256.New()
		fh, err := os.Open(f)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, fh)
		fh.Close()
		if err != nil {
			return "", err
		}
		outer.Write(h.Sum(nil))
	}
	return hex.EncodeToString(outer.Sum(nil)), nil
}

// globRegexp converts a glob (with **) to an anchored regexp.
func globRegexp(g string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
