package web

import (
	"bytes"
	"html/template"
	"strconv"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// renderIssueMarkdown renders Markdown like the shared `markdown` template func
// and also turns #123 into a link to that issue in the same repo. It works on
// the parsed tree, so references inside code, links and autolinks are left
// alone, and raw HTML stays escaped exactly as in renderMarkdown.
func renderIssueMarkdown(src, base string, exists func(int64) bool) template.HTML {
	source := []byte(src)
	doc := md.Parser().Parse(text.NewReader(source))
	linkIssueRefs(doc, source, base, exists)
	var buf bytes.Buffer
	if err := md.Renderer().Render(&buf, source, doc); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(buf.String())
}

func linkIssueRefs(doc ast.Node, source []byte, base string, exists func(int64) bool) {
	var texts []*ast.Text
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case ast.KindCodeSpan, ast.KindLink, ast.KindAutoLink, ast.KindImage, ast.KindRawHTML,
			ast.KindCodeBlock, ast.KindFencedCodeBlock, ast.KindHTMLBlock:
			return ast.WalkSkipChildren, nil
		case ast.KindText:
			texts = append(texts, n.(*ast.Text))
		}
		return ast.WalkContinue, nil
	})
	for _, t := range texts {
		splitIssueRefs(t, source, base, exists)
	}
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// splitIssueRefs replaces one text node with text / link / text … pieces.
func splitIssueRefs(t *ast.Text, source []byte, base string, exists func(int64) bool) {
	seg := t.Segment
	val := seg.Value(source)
	parent := t.Parent()
	if parent == nil {
		return
	}
	start := 0 // start of the not-yet-emitted text, relative to seg.Start
	var pieces []ast.Node
	for i := 0; i < len(val); i++ {
		if val[i] != '#' {
			continue
		}
		// "#" must not follow a word character (so a#1 and &#123; stay as they are).
		if i > 0 && (isWordByte(val[i-1]) || val[i-1] == '&') {
			continue
		}
		if i == 0 && seg.Start > 0 && (isWordByte(source[seg.Start-1]) || source[seg.Start-1] == '&') {
			continue
		}
		j := i + 1
		for j < len(val) && val[j] >= '0' && val[j] <= '9' {
			j++
		}
		if j == i+1 || j-i > 10 || (j < len(val) && isWordByte(val[j])) {
			continue
		}
		n, err := strconv.ParseInt(string(val[i+1:j]), 10, 64)
		if err != nil || n <= 0 || (exists != nil && !exists(n)) {
			continue
		}
		if i > start {
			pieces = append(pieces, ast.NewTextSegment(text.NewSegment(seg.Start+start, seg.Start+i)))
		}
		link := ast.NewLink()
		link.Destination = []byte(base + "/issues/" + strconv.FormatInt(n, 10))
		link.SetAttributeString("class", []byte("issue-ref"))
		link.AppendChild(link, ast.NewTextSegment(text.NewSegment(seg.Start+i, seg.Start+j)))
		pieces = append(pieces, link)
		start = j
		i = j - 1
	}
	if len(pieces) == 0 {
		return
	}
	rest := ast.NewTextSegment(text.NewSegment(seg.Start+start, seg.Stop))
	rest.SetSoftLineBreak(t.SoftLineBreak())
	rest.SetHardLineBreak(t.HardLineBreak())
	if start < len(val) || t.SoftLineBreak() || t.HardLineBreak() {
		pieces = append(pieces, rest)
	}
	for _, p := range pieces {
		parent.InsertBefore(parent, t, p)
	}
	parent.RemoveChild(parent, t)
}
