package highlight

import (
	"html"
	"html/template"
)

// DiffLine is one line of a diff for Diff: Kind is "ctx", "add" or "del" for
// code; "hunk" and "file" start a new hunk; anything else (meta, "\ No
// newline…") is shown as plain text.
type DiffLine struct {
	Kind string
	Text string // without the +/-/space marker
}

// Diff highlights a file's diff lines. Each hunk is coloured as two texts,
// the old side (context + removed lines) and the new side (context + added
// lines), so a comment or string opened on one line carries on to the next
// line of the same side; context lines take the new side's colours. It never
// changes which lines there are or their kinds: out[i] is lines[i].Text.
func Diff(name string, lines []DiffLine) []template.HTML {
	out := make([]template.HTML, len(lines))
	for i, l := range lines {
		out[i] = template.HTML(html.EscapeString(l.Text))
	}
	lexer := Detect(name, nil).Lexer
	if lexer == "" {
		return out
	}
	total := 0
	for _, l := range lines {
		total += len(l.Text) + 1
		if len(l.Text) > MaxLineLen {
			return out
		}
	}
	if total > MaxBytes {
		return out
	}
	flush := func(idx []int) {
		var oldI, newI []int
		var oldT, newT []string
		for _, i := range idx {
			switch lines[i].Kind {
			case "ctx":
				oldI, oldT = append(oldI, -1), append(oldT, lines[i].Text) // context: coloured from the new side
				newI, newT = append(newI, i), append(newT, lines[i].Text)
			case "del":
				oldI, oldT = append(oldI, i), append(oldT, lines[i].Text)
			case "add":
				newI, newT = append(newI, i), append(newT, lines[i].Text)
			}
		}
		for _, side := range []struct {
			idx []int
			txt []string
		}{{oldI, oldT}, {newI, newT}} {
			if len(side.txt) == 0 {
				continue
			}
			hl := Lines(lexer, side.txt)
			for k, i := range side.idx {
				if i >= 0 && k < len(hl) {
					out[i] = hl[k]
				}
			}
		}
	}
	var hunk []int
	for i, l := range lines {
		switch l.Kind {
		case "ctx", "add", "del":
			hunk = append(hunk, i)
		case "hunk", "file":
			flush(hunk)
			hunk = hunk[:0]
		}
	}
	flush(hunk)
	return out
}
