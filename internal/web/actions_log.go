package web

// Run log rendering: turns the raw lines a runner sends into readable HTML.
//
//   - ANSI colour and style codes become classed spans (no inline styles: CSP).
//     Other terminal codes (cursor moves, hyperlinks) are dropped, and a line
//     a progress bar rewrote with "\r" keeps only its last version.
//   - Workflow commands are shown as what they mean: ::error:: and friends get
//     a label and their file position, ::group:: becomes a fold, and plumbing
//     like ::set-output:: is dimmed.
//   - Known noise (Docker image pulls, apt package lists) folds away, and
//     lines that look like errors or warnings are tinted so they stand out.
//
// The page renders whole steps with buildLog/renderLog. Live updates send
// one rendered row per line (logStreamRow) and actions.js does the folding,
// so all the markup is made here.

import (
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strconv"
	"strings"
	"time"

	"atelier/internal/actions"
)

// Row kinds. Each becomes an is-<kind> class.
const (
	logError    = "error"
	logWarning  = "warning"
	logNotice   = "notice"
	logDebug    = "debug"
	logCommand  = "command"  // workflow plumbing (::set-output:: etc.), dimmed
	logQuiet    = "quiet"    // harmless chatter (debconf), dimmed
	logPull     = "pull"     // download progress, folded when there's a run of it
	logGroup    = "group"    // ::group::title
	logEndGroup = "endgroup" // ::endgroup::, not shown
)

type logRow struct {
	N      int // line number in the step, from 1; 0 when not known yet (live)
	Time   time.Time
	Stream string
	Kind   string
	Label  string        // "Error", "Warning", "Notice" for workflow commands
	Body   template.HTML // the text, ANSI rendered
	Text   string        // the text, plain (for the MCP log tool)
	Where  string        // file:line:col from an ::error file=…:: command
	Title  string        // group title, or the image a Docker pull line names
	ID     string        // anchor, set on the job's first error
}

// logBlock is a run of plain rows, or a fold (a ::group:: or a pull).
type logBlock struct {
	Fold     string // "", logGroup or logPull
	Head     logRow // the ::group:: line
	Rows     []logRow
	Unclosed bool // a ::group:: with no ::endgroup:: yet (live lines go inside)
}

// logParser classifies lines one at a time. It is stateful only for
// ::stop-commands::, which turns command parsing off until its token comes back.
type logParser struct{ stop string }

var logCommands = map[string]bool{
	"set-output": true, "save-state": true, "add-mask": true, "echo": true, "add-matcher": true,
	"remove-matcher": true, "set-env": true, "add-path": true, "stop-commands": true,
}

var (
	logGHPrefix = regexp.MustCompile(`^##\[(error|warning|notice|debug|group|endgroup)\] ?(.*)$`)
	logErrorRe  = regexp.MustCompile(`(?i)^(?:error|fatal|panic)(?:[:\[\s!]|$)|^npm ERR!|^E: |^Process completed with exit code [1-9]`)
	logWarnRe   = regexp.MustCompile(`(?i)^(?:warning|warn)(?:[:\[\s!]|$)|^npm warn\b|^W: `)
	logQuietRe  = regexp.MustCompile(`^(?:debconf: |dpkg-preconfigure: |\(Reading database \.\.\. )`)
	logPullRe   = regexp.MustCompile(`^(?:[0-9a-f]{12}: (?:Pulling fs layer|Waiting|Verifying Checksum|Download complete|Pull complete|Already exists|Downloading|Extracting)\b|Digest: sha256:[0-9a-f]+$|(?:Get|Hit|Ign):\d+ \S)`)
	logImageRe  = regexp.MustCompile(`^(?:Unable to find image '([^']+)' locally|Status: (?:Downloaded newer image|Image is up to date) for (\S+))$`)
	logFromRe   = regexp.MustCompile(`^([\w.-]+): Pulling from (\S+)$`)
)

// row classifies and renders one line.
func (p *logParser) row(l actions.LogLine) logRow {
	body, plain := ansiHTML(lastCR(l.Text))
	r := logRow{Time: l.Time, Stream: l.Stream, Body: body, Text: plain}
	t := strings.TrimLeft(plain, " \t")
	if p.stop != "" {
		if strings.TrimSpace(plain) == "::"+p.stop+"::" {
			p.stop, r.Kind = "", logCommand
		}
		return r
	}
	name, props, value, ok := parseLogCommand(t)
	if !ok {
		if m := logGHPrefix.FindStringSubmatch(t); m != nil {
			name, value, ok = m[1], m[2], true
		}
	}
	if ok {
		switch name {
		case logError, logWarning, logNotice:
			r.Kind, r.Label, r.Body, r.Text = name, strings.ToUpper(name[:1])+name[1:], escText(value), value
			r.Where = logWhere(props)
		case logDebug:
			r.Kind, r.Body, r.Text = logDebug, escText(value), value
		case logGroup:
			r.Kind, r.Title, r.Body, r.Text = logGroup, value, escText(value), value
		case logEndGroup:
			r.Kind = logEndGroup
		case "stop-commands":
			p.stop, r.Kind = value, logCommand
		default:
			if logCommands[name] {
				r.Kind = logCommand
			}
		}
		return r
	}
	switch {
	case logErrorRe.MatchString(t):
		r.Kind = logError
	case logWarnRe.MatchString(t):
		r.Kind = logWarning
	case logQuietRe.MatchString(t):
		r.Kind = logQuiet
	case logPullRe.MatchString(t):
		r.Kind = logPull
	default:
		if m := logImageRe.FindStringSubmatch(t); m != nil {
			r.Kind, r.Title = logPull, m[1]+m[2]
		} else if m := logFromRe.FindStringSubmatch(t); m != nil {
			r.Kind, r.Title = logPull, strings.TrimPrefix(m[2], "library/")+":"+m[1]
		}
	}
	return r
}

// parseLogCommand parses "::name k=v,k2=v2::value" (as the runner does).
func parseLogCommand(t string) (name string, props map[string]string, value string, ok bool) {
	if !strings.HasPrefix(t, "::") {
		return "", nil, "", false
	}
	head, value, found := strings.Cut(t[2:], "::")
	if !found {
		return "", nil, "", false
	}
	name, propStr, _ := strings.Cut(head, " ")
	if name == "" || strings.ContainsAny(name, " \t") {
		return "", nil, "", false
	}
	props = map[string]string{}
	for _, kv := range strings.Split(propStr, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			props[k] = strings.NewReplacer("%0D", "", "%0A", " ", "%3A", ":", "%2C", ",", "%25", "%").Replace(v)
		}
	}
	value = strings.NewReplacer("%0D", "", "%0A", "\n", "%25", "%").Replace(value)
	return name, props, value, true
}

func logWhere(props map[string]string) string {
	f := props["file"]
	if f == "" {
		return props["title"]
	}
	if l := props["line"]; l != "" {
		f += ":" + l
		if c := props["col"]; c != "" {
			f += ":" + c
		}
	}
	return f
}

// lastCR keeps what a terminal would show after "\r" rewrites.
func lastCR(s string) string {
	s = strings.TrimRight(s, "\r")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func escText(s string) template.HTML { return template.HTML(html.EscapeString(s)) }

// buildLog groups a step's lines into blocks. first is the number of the
// first line (more than 1 when earlier lines were cut).
func buildLog(lines []actions.LogLine, first int) []logBlock {
	var (
		p     logParser
		out   []logBlock
		group = -1 // index of the open ::group:: block
		pulls []logRow
	)
	add := func(r logRow) {
		if group >= 0 {
			out[group].Rows = append(out[group].Rows, r)
			return
		}
		if n := len(out); n > 0 && out[n-1].Fold == "" {
			out[n-1].Rows = append(out[n-1].Rows, r)
			return
		}
		out = append(out, logBlock{Rows: []logRow{r}})
	}
	flush := func() {
		if len(pulls) >= 2 {
			out = append(out, logBlock{Fold: logPull, Rows: pulls})
		} else {
			for _, r := range pulls {
				add(r)
			}
		}
		pulls = nil
	}
	for i, l := range lines {
		r := p.row(l)
		r.N = first + i
		switch {
		case r.Kind == logGroup:
			flush()
			out = append(out, logBlock{Fold: logGroup, Head: r})
			group = len(out) - 1
		case r.Kind == logEndGroup:
			group = -1
		case r.Kind == logPull && group < 0:
			pulls = append(pulls, r)
		default:
			if group < 0 {
				flush()
			}
			add(r)
		}
	}
	flush()
	if group >= 0 {
		out[group].Unclosed = true
	}
	return out
}

// firstLogError returns the first error row, so the page can link to it.
func firstLogError(blocks []logBlock) *logRow {
	for i := range blocks {
		for j := range blocks[i].Rows {
			if blocks[i].Rows[j].Kind == logError {
				return &blocks[i].Rows[j]
			}
		}
	}
	return nil
}

// markFirstError picks where a failed job's "Jump to the error" link goes:
// the first error line of the failed step, else the first error line
// anywhere, else the failed step itself. It returns the anchor.
func markFirstError(jobID int64, steps []actStepView, stepBlocks [][]logBlock, setup []logBlock) string {
	id := fmt.Sprintf("job-%d-error", jobID)
	for i, s := range steps {
		if s.Status == actions.StatusFailure {
			if r := firstLogError(stepBlocks[i]); r != nil {
				r.ID = id
				return id
			}
		}
	}
	for _, bl := range append([][]logBlock{setup}, stepBlocks...) {
		if r := firstLogError(bl); r != nil {
			r.ID = id
			return id
		}
	}
	for _, s := range steps {
		if s.Status == actions.StatusFailure {
			return fmt.Sprintf("step-%d-%d", jobID, s.Index)
		}
	}
	return ""
}

func renderLog(blocks []logBlock) template.HTML {
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Fold == "" {
			for _, r := range bl.Rows {
				b.WriteString(logRowHTML(r))
			}
			continue
		}
		open := false
		for _, r := range bl.Rows {
			open = open || r.Kind == logError
		}
		b.WriteString(logFoldOpen(bl, open))
		for _, r := range bl.Rows {
			b.WriteString(logRowHTML(r))
		}
		b.WriteString("</details>")
	}
	return template.HTML(b.String())
}

func logFoldOpen(bl logBlock, open bool) string {
	var b strings.Builder
	b.WriteString(`<details class="act-fold is-` + bl.Fold + `"`)
	if open {
		b.WriteString(" open")
	}
	if bl.Unclosed {
		b.WriteString(" data-act-open")
	}
	b.WriteString(`><summary`)
	if bl.Head.N > 0 {
		fmt.Fprintf(&b, ` data-ln="%d"`, bl.Head.N)
	}
	b.WriteString(`><span class="act-fold-t">`)
	if bl.Fold == logGroup {
		b.WriteString(string(bl.Head.Body))
	} else {
		b.WriteString(html.EscapeString(pullTitle(bl.Rows)))
	}
	n := len(bl.Rows)
	fmt.Fprintf(&b, `</span><span class="act-fold-n" data-n="%d">%s</span></summary>`, n, logLinesWord(n))
	return b.String()
}

func pullTitle(rows []logRow) string {
	for _, r := range rows {
		if r.Title != "" {
			return "Pulled image " + r.Title
		}
	}
	return "Download progress"
}

func logLinesWord(n int) string {
	if n == 1 {
		return "1 line"
	}
	return strconv.Itoa(n) + " lines"
}

func logRowHTML(r logRow) string {
	var b strings.Builder
	b.WriteString(`<div class="act-ln`)
	if r.Kind != "" {
		b.WriteString(" is-" + r.Kind)
	}
	if r.Stream == "stderr" || r.Stream == "system" {
		b.WriteString(" is-" + r.Stream)
	}
	b.WriteString(`"`)
	if r.N > 0 {
		fmt.Fprintf(&b, ` data-ln="%d"`, r.N)
	}
	if r.ID != "" {
		b.WriteString(` id="` + html.EscapeString(r.ID) + `"`)
	}
	if r.Title != "" {
		b.WriteString(` data-title="` + html.EscapeString(r.Title) + `"`)
	}
	b.WriteString(">")
	if !r.Time.IsZero() {
		t := r.Time.UTC()
		b.WriteString(`<time class="act-ts" datetime="` + t.Format(time.RFC3339) + `">` + t.Format("15:04:05") + `</time>`)
	}
	b.WriteString(`<span class="act-lt">`)
	if r.Label != "" {
		b.WriteString(`<span class="act-lbl">` + r.Label + `</span>`)
	}
	b.WriteString(string(r.Body))
	if r.Where != "" {
		b.WriteString(` <span class="act-where">` + html.EscapeString(r.Where) + `</span>`)
	}
	b.WriteString("</span></div>")
	return b.String()
}

// logStreamRow is one rendered line for the live stream. actions.js places
// it: Kind group opens Fold, endgroup closes it, and pull rows fold together
// (Fold is the empty fold to put them in).
type logStreamRow struct {
	Step int    `json:"step"`
	Kind string `json:"k,omitempty"`
	HTML string `json:"h,omitempty"`
	Fold string `json:"f,omitempty"`
}

func streamRow(p *logParser, l actions.LogLine) logStreamRow {
	r := p.row(l)
	s := logStreamRow{Step: l.Step, Kind: r.Kind}
	switch r.Kind {
	case logGroup:
		s.Fold = logFoldOpen(logBlock{Fold: logGroup, Head: r}, false) + "</details>"
	case logEndGroup:
	case logPull:
		s.HTML = logRowHTML(r)
		s.Fold = logFoldOpen(logBlock{Fold: logPull}, false) + "</details>"
	default:
		s.HTML = logRowHTML(r)
	}
	return s
}

// ---------- ANSI ----------

// sgrState is the current Select Graphic Rendition. Colours are 0–15 (the
// classic palette, which the CSS maps to theme colours) or -1 for default.
type sgrState struct {
	fg, bg                   int
	bold, dim, italic, under bool
	strike, inverse          bool
}

func (s sgrState) class() string {
	var c []string
	fg, bg := s.fg, s.bg
	if s.inverse {
		fg, bg = bg, fg
		if fg < 0 && bg < 0 {
			c = append(c, "a-inv")
		}
	}
	if fg >= 0 {
		c = append(c, "a-f"+strconv.Itoa(fg))
	}
	if bg >= 0 {
		c = append(c, "a-g"+strconv.Itoa(bg))
	}
	for _, f := range []struct {
		on bool
		c  string
	}{{s.bold, "a-b"}, {s.dim, "a-d"}, {s.italic, "a-i"}, {s.under, "a-u"}, {s.strike, "a-s"}} {
		if f.on {
			c = append(c, f.c)
		}
	}
	return strings.Join(c, " ")
}

// ansiHTML renders text with ANSI escape codes as HTML, and also returns the
// text with the codes removed.
func ansiHTML(s string) (template.HTML, string) {
	if !strings.ContainsAny(s, "\x1b\x00\x01\x02\x03\x04\x05\x06\x07\x08\x0b\x0c\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1c\x1d\x1e\x1f\x7f") {
		return escText(s), s
	}
	var (
		out, plain, seg strings.Builder
		st              = sgrState{fg: -1, bg: -1}
		cls             string
	)
	flush := func() {
		if seg.Len() == 0 {
			return
		}
		text := html.EscapeString(seg.String())
		if cls == "" {
			out.WriteString(text)
		} else {
			out.WriteString(`<span class="` + cls + `">` + text + `</span>`)
		}
		seg.Reset()
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b && i+1 < len(s) {
			switch s[i+1] {
			case '[': // CSI: parameters, then a final byte 0x40–0x7e
				j := i + 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j < len(s) && s[j] == 'm' {
					flush()
					st = st.apply(s[i+2 : j])
					cls = st.class()
				}
				i = j
			case ']': // OSC (hyperlinks, titles): ends with BEL or ESC \
				j := i + 2
				for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
					j++
				}
				if j < len(s) && s[j] == 0x1b {
					j++
				}
				i = j
			default:
				i++
			}
			continue
		}
		if (c < 0x20 && c != '\t') || c == 0x7f {
			continue
		}
		seg.WriteByte(c)
		plain.WriteByte(c)
	}
	flush()
	return template.HTML(out.String()), plain.String()
}

func (s sgrState) apply(params string) sgrState {
	if params == "" {
		params = "0"
	}
	ps := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	if len(ps) == 0 {
		ps = []string{"0"}
	}
	num := func(i int) int {
		if i >= len(ps) {
			return -1
		}
		n, err := strconv.Atoi(ps[i])
		if err != nil {
			return -1
		}
		return n
	}
	for i := 0; i < len(ps); i++ {
		n := num(i)
		switch {
		case n == 0:
			s = sgrState{fg: -1, bg: -1}
		case n == 1:
			s.bold = true
		case n == 2:
			s.dim = true
		case n == 3:
			s.italic = true
		case n == 4:
			s.under = true
		case n == 7:
			s.inverse = true
		case n == 9:
			s.strike = true
		case n == 21 || n == 22:
			s.bold, s.dim = false, false
		case n == 23:
			s.italic = false
		case n == 24:
			s.under = false
		case n == 27:
			s.inverse = false
		case n == 29:
			s.strike = false
		case n >= 30 && n <= 37:
			s.fg = n - 30
		case n >= 90 && n <= 97:
			s.fg = n - 90 + 8
		case n == 39:
			s.fg = -1
		case n >= 40 && n <= 47:
			s.bg = n - 40
		case n >= 100 && n <= 107:
			s.bg = n - 100 + 8
		case n == 49:
			s.bg = -1
		case n == 38 || n == 48:
			col := -1
			switch num(i + 1) {
			case 5:
				col = ansi256(num(i + 2))
				i += 2
			case 2:
				col = nearestANSI(num(i+2), num(i+3), num(i+4))
				i += 4
			}
			if n == 38 {
				s.fg = col
			} else {
				s.bg = col
			}
		}
	}
	return s
}

// ansiPalette is xterm's 16 colours, used to pick the nearest one.
var ansiPalette = [16][3]int{
	{0, 0, 0}, {205, 0, 0}, {0, 205, 0}, {205, 205, 0}, {0, 0, 238}, {205, 0, 205}, {0, 205, 205}, {229, 229, 229},
	{127, 127, 127}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0}, {92, 92, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
}

func ansi256(n int) int {
	switch {
	case n < 0 || n > 255:
		return -1
	case n < 16:
		return n
	case n >= 232:
		v := 8 + 10*(n-232)
		return nearestANSI(v, v, v)
	}
	n -= 16
	lv := [6]int{0, 95, 135, 175, 215, 255}
	return nearestANSI(lv[n/36], lv[n/6%6], lv[n%6])
}

func nearestANSI(r, g, b int) int {
	if r < 0 || g < 0 || b < 0 {
		return -1
	}
	best, bestD := -1, 1<<30
	for i, p := range ansiPalette {
		dr, dg, db := r-p[0], g-p[1], b-p[2]
		if d := dr*dr + dg*dg + db*db; d < bestD {
			best, bestD = i, d
		}
	}
	return best
}
