package web

import (
	"strings"

	"atelier/internal/gitrepo"
)

// GitHub's legacy review-comment "position" is an offset into one file's
// patch (the "patch" field of /pulls/{n}/files): the line just below the
// first "@@" header is position 1, and counting carries on through every
// following line, later "@@" headers included, until the file ends.
// Atelier notes are anchored by line and side, so positions are mapped to
// and from those using the same lines appsPatch prints.

// appsPatchLines are the lines of f that appear in its patch, in order.
func appsPatchLines(f *gitrepo.DiffFile) []gitrepo.DiffLine {
	out := make([]gitrepo.DiffLine, 0, len(f.Lines))
	for _, l := range f.Lines {
		switch l.Kind {
		case "hunk", "add", "del", "ctx":
			out = append(out, l)
		}
	}
	return out
}

// appsPositionLine maps a diff position in f to a line and side ("new" for
// added and context lines, numbered on the new side; "old" for deleted
// lines, numbered on the old side). ok is false for positions outside the
// patch or on a hunk header.
func appsPositionLine(f *gitrepo.DiffFile, pos int) (line int, side string, ok bool) {
	ls := appsPatchLines(f)
	if pos < 1 || len(ls) == 0 || ls[0].Kind != "hunk" || pos >= len(ls) {
		return 0, "", false
	}
	l := ls[pos] // ls[0] is the first header, position 0
	switch l.Kind {
	case "add", "ctx":
		return l.New, "new", l.New > 0
	case "del":
		return l.Old, "old", l.Old > 0
	}
	return 0, "", false
}

// appsLinePosition is the reverse: the position of line on side, if that
// line is part of the patch.
func appsLinePosition(f *gitrepo.DiffFile, line int, side string) (int, bool) {
	ls := appsPatchLines(f)
	if len(ls) == 0 || ls[0].Kind != "hunk" {
		return 0, false
	}
	for i, l := range ls {
		switch {
		case side == "old" && l.Kind == "del" && l.Old == line,
			side != "old" && (l.Kind == "add" || l.Kind == "ctx") && l.New == line:
			return i, true
		}
	}
	return 0, false
}

// appsDiffHunk is GitHub's "diff_hunk": the patch from the hunk header that
// contains pos down to the line at pos.
func appsDiffHunk(f *gitrepo.DiffFile, pos int) string {
	ls := appsPatchLines(f)
	if pos < 1 || pos >= len(ls) {
		return ""
	}
	start := pos
	for start > 0 && ls[start].Kind != "hunk" {
		start--
	}
	var b strings.Builder
	for i := start; i <= pos; i++ {
		l := ls[i]
		switch l.Kind {
		case "hunk":
			b.WriteString(l.Text)
		case "add":
			b.WriteString("+" + l.Text)
		case "del":
			b.WriteString("-" + l.Text)
		default:
			b.WriteString(" " + l.Text)
		}
		if i < pos {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// appsDiffFile finds path in a diff (by new path, or old path for renames).
func appsDiffFile(d *gitrepo.Diff, path string) *gitrepo.DiffFile {
	if d == nil {
		return nil
	}
	for i := range d.Files {
		if d.Files[i].Path == path {
			return &d.Files[i]
		}
	}
	for i := range d.Files {
		if d.Files[i].OldPath == path {
			return &d.Files[i]
		}
	}
	return nil
}
