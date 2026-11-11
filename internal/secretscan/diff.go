package secretscan

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// PatchLogArgs are the `git log` arguments ScanPatchLog expects, before the
// revisions: each commit starts with a NUL-prefixed hash line, and only added
// lines are shown (no context).
var PatchLogArgs = []string{"-c", "core.quotePath=false", "log", "-p", "--no-color", "--no-ext-diff", "--no-renames",
	"--unified=0", "--diff-filter=d", "--format=%x00%H"}

// PatchStats says how much a patch scan read.
type PatchStats struct {
	Commits   int
	Bytes     int64
	Truncated bool // a limit stopped the scan early
}

// ScanPatchLog scans the added lines in `git log` output (see PatchLogArgs),
// stopping after maxCommits commits or maxBytes bytes. Findings carry the
// commit, path and new-file line number.
func (s *Scanner) ScanPatchLog(r io.Reader, maxCommits int, maxBytes int64) ([]Finding, PatchStats, error) {
	var out []Finding
	var st PatchStats
	lim := s.limits()
	br := bufio.NewReaderSize(r, 64<<10)

	var commit, path string
	var lines []Line
	var fileBytes int64
	skipFile, inHeader := false, false
	newLine := 0
	flush := func() {
		if path != "" && !skipFile && len(lines) > 0 {
			for _, f := range s.ScanLines(path, lines) {
				f.Commit = commit
				out = append(out, f)
			}
		}
		lines, fileBytes, skipFile = nil, 0, false
	}
	for {
		raw, tooLong, err := readLine(br, lim.MaxLineBytes)
		st.Bytes += int64(len(raw))
		if maxBytes > 0 && st.Bytes > maxBytes {
			st.Truncated = true
			break
		}
		if len(raw) > 0 || tooLong {
			line := raw
			switch {
			case strings.HasPrefix(line, "\x00"):
				flush()
				path = ""
				if maxCommits > 0 && st.Commits >= maxCommits {
					st.Truncated = true
					return out, st, nil
				}
				st.Commits++
				commit = strings.TrimSpace(line[1:])
			case strings.HasPrefix(line, "diff --git "):
				flush()
				path, inHeader = "", true
			case inHeader && strings.HasPrefix(line, "+++ "):
				p := strings.TrimPrefix(line, "+++ ")
				if p == "/dev/null" {
					path = ""
					continue
				}
				if strings.HasPrefix(p, `"`) {
					if u, err := strconv.Unquote(p); err == nil {
						p = u
					}
				}
				path = strings.TrimPrefix(p, "b/")
				skipFile = s.SkipPath(path)
			case strings.HasPrefix(line, "@@ "):
				newLine, inHeader = hunkNewStart(line), false
			case !inHeader && strings.HasPrefix(line, "+") && path != "":
				if !skipFile {
					fileBytes += int64(len(line))
					if fileBytes > lim.MaxFileBytes {
						skipFile = true // too much added text in one file: treat as generated
						lines = nil
					} else {
						lines = append(lines, Line{No: newLine, Text: line[1:]})
					}
				}
				newLine++
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, st, err
		}
	}
	flush()
	return out, st, nil
}

// readLine reads one line without its newline, keeping at most max bytes.
func readLine(br *bufio.Reader, max int) (string, bool, error) {
	var b []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(b) < max {
			room := max - len(b)
			if len(chunk) > room {
				b = append(b, chunk[:room]...)
				tooLong = true
			} else {
				b = append(b, chunk...)
			}
		} else if len(chunk) > 0 {
			tooLong = true
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		s := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		return s, tooLong, err
	}
}

// hunkNewStart reads "+c" from "@@ -a,b +c,d @@".
func hunkNewStart(h string) int {
	i := strings.Index(h, " +")
	if i < 0 {
		return 1
	}
	rest := h[i+2:]
	end := strings.IndexAny(rest, ", ")
	if end >= 0 {
		rest = rest[:end]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 1
	}
	return n
}
