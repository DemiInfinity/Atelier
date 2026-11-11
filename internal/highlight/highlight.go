// Package highlight colours source code for Atelier's pages with chroma.
//
// Output is HTML with CSS classes only (hl-k, hl-s, …; see static/highlight.css),
// never inline styles, so it works under the app's CSP. Every piece of text is
// HTML-escaped by chroma's formatter; when a file is too large, has very long
// (minified) lines or takes too long, it comes back as escaped plain text with
// a note saying why.
package highlight

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"html"
	"html/template"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/chroma/v2"
	chtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
)

// Limits: past these a file is shown as plain text.
const (
	MaxBytes   = 512 << 10              // whole file
	MaxLineLen = 5000                   // any one line (minified code)
	Budget     = 750 * time.Millisecond // tokenising time per file
	analyseMax = 16 << 10               // chroma's content analysers only run on files this small
)

// Result is a highlighted file.
type Result struct {
	Lang  string          // language shown to people ("Kotlin"); "" for plain text
	Lines []template.HTML // one entry per line, without the newline
	Note  string          // why highlighting was skipped, if it was
}

var formatter = chtml.New(chtml.WithClasses(true), chtml.ClassPrefix("hl-"), chtml.PreventSurroundingPre(true))

// File highlights a whole file. The name picks the language (see Detect).
func File(name string, data []byte) Result {
	lines := splitLines(data)
	lang := Detect(name, data)
	res := Result{Lang: lang.Name}
	if lang.Lexer == "" {
		res.Lines = plain(lines)
		return res
	}
	if note := tooBig(len(data), lines); note != "" {
		res.Lines, res.Note = plain(lines), note
		return res
	}
	key := cacheKey(lang.Lexer, data)
	if hl, ok := cache.get(key); ok {
		res.Lines = hl
		return res
	}
	hl, ok := highlightLines(lang.Lexer, lines, time.Now().Add(Budget))
	if !ok {
		res.Lines, res.Note = plain(lines), "Shown without highlighting: it took too long to colour."
		return res
	}
	cache.put(key, hl)
	res.Lines = hl
	return res
}

// Lines highlights lines of code in a language given by lexer name, as
// returned in Lang.Lexer. It returns escaped plain text when it can't.
func Lines(lexer string, lines []string) []template.HTML {
	if lexer == "" {
		return plain(lines)
	}
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	if tooBig(n, lines) != "" {
		return plain(lines)
	}
	if hl, ok := highlightLines(lexer, lines, time.Now().Add(Budget)); ok {
		return hl
	}
	return plain(lines)
}

// Code highlights a block of code (a Markdown fence) in the language named by
// hint: a chroma name or alias ("go", "kotlin", "ts") or a file extension.
// ok is false when the language isn't known.
func Code(hint, code string) (template.HTML, bool) {
	lexer := LexerForHint(hint)
	if lexer == "" {
		return "", false
	}
	lines := strings.Split(strings.TrimSuffix(code, "\n"), "\n")
	hl := Lines(lexer, lines)
	var b strings.Builder
	for _, l := range hl {
		b.WriteString(string(l))
		b.WriteByte('\n')
	}
	return template.HTML(b.String()), true
}

func tooBig(size int, lines []string) string {
	if size > MaxBytes {
		return "Shown without highlighting: the file is larger than 512 KB."
	}
	long := 0
	for _, l := range lines {
		if len(l) > MaxLineLen {
			return "Shown without highlighting: it has very long lines (minified?)."
		}
		if len(l) > 500 {
			long++
		}
	}
	// Mostly long lines: minified or generated.
	if size > 4096 && long*2 > len(lines) {
		return "Shown without highlighting: it looks minified."
	}
	return ""
}

func splitLines(data []byte) []string {
	s := strings.TrimSuffix(string(data), "\n")
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(p, "\r")
	}
	return parts
}

func plain(lines []string) []template.HTML {
	out := make([]template.HTML, len(lines))
	for i, l := range lines {
		out[i] = template.HTML(html.EscapeString(l))
	}
	return out
}

// highlightLines lexes the lines as one text, so state such as an open
// comment or string carries from line to line, then formats each line
// separately. A line whose tokens don't spell exactly its input is shown
// plain, so highlighting never changes the text.
func highlightLines(lexerName string, lines []string, deadline time.Time) (out []template.HTML, ok bool) {
	lexer := lexers.Get(lexerName)
	if lexer == nil {
		return nil, false
	}
	defer func() {
		if recover() != nil { // a lexer bug must not take the page down
			out, ok = nil, false
		}
	}()
	src := strings.Join(lines, "\n") + "\n"
	it, err := chroma.Coalesce(lexer).Tokenise(nil, src)
	if err != nil {
		return nil, false
	}
	var toks []chroma.Token
	for i := 0; ; i++ {
		t := it()
		if t == chroma.EOF {
			break
		}
		toks = append(toks, t)
		if i%256 == 0 && time.Now().After(deadline) {
			return nil, false
		}
	}
	split := chroma.SplitTokensIntoLines(toks)
	out = make([]template.HTML, len(lines))
	style := Style()
	var buf bytes.Buffer
	for i, l := range lines {
		if i >= len(split) {
			out[i] = template.HTML(html.EscapeString(l))
			continue
		}
		var sb strings.Builder
		lt := split[i][:0]
		for _, tok := range split[i] {
			tok.Value = strings.TrimSuffix(tok.Value, "\n")
			if tok.Value == "" {
				continue
			}
			if tok.Type == chroma.TextWhitespace {
				tok.Type = chroma.Text // no span around plain spaces
			}
			sb.WriteString(tok.Value)
			lt = append(lt, tok)
		}
		if sb.String() != l {
			out[i] = template.HTML(html.EscapeString(l))
			continue
		}
		buf.Reset()
		if err := formatter.Format(&buf, style, chroma.Literator(lt...)); err != nil {
			out[i] = template.HTML(html.EscapeString(l))
			continue
		}
		out[i] = template.HTML(buf.String())
	}
	return out, true
}

// ---------- cache ----------

type lru struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[[32]byte]*list.Element
}

type lruEntry struct {
	key   [32]byte
	lines []template.HTML
}

var cache = &lru{max: 64, ll: list.New(), items: map[[32]byte]*list.Element{}}

// cacheKey is per (content, language): the content hash is the blob, wherever
// it was read from.
func cacheKey(lexer string, data []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(lexer))
	h.Write([]byte{0})
	h.Write(data)
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

func (c *lru) get(k [32]byte) ([]template.HTML, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*lruEntry).lines, true
	}
	return nil, false
}

func (c *lru) put(k [32]byte, lines []template.HTML) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.ll.MoveToFront(e)
		return
	}
	c.items[k] = c.ll.PushFront(&lruEntry{key: k, lines: lines})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*lruEntry).key)
	}
}
