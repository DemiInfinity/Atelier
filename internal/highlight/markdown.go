package highlight

import (
	"html"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// Markdown is a goldmark extension that colours fenced code blocks whose
// info string names a known language (```go, ```kotlin, ```ts …). Other
// blocks render exactly as goldmark's default: escaped, uncoloured.
var Markdown goldmark.Extender = mdExt{}

type mdExt struct{}

func (mdExt) Extend(m goldmark.Markdown) {
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(fenceRenderer{}, 100)))
}

type fenceRenderer struct{}

func (fenceRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, renderFence)
}

var safeLang = regexp.MustCompile(`^[A-Za-z0-9_+#.-]{1,40}$`)

func renderFence(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.FencedCodeBlock)
	lang := ""
	if l := n.Language(source); l != nil {
		lang = string(l)
	}
	var code strings.Builder
	for i := 0; i < n.Lines().Len(); i++ {
		seg := n.Lines().At(i)
		code.Write(seg.Value(source))
	}
	hl, ok := Code(lang, code.String())
	_, _ = w.WriteString("<pre")
	if ok {
		_, _ = w.WriteString(` class="hl-pre"`)
	}
	_, _ = w.WriteString("><code")
	if safeLang.MatchString(lang) {
		_, _ = w.WriteString(` class="language-` + html.EscapeString(lang) + `"`)
	}
	_, _ = w.WriteString(">")
	if ok {
		_, _ = w.WriteString(string(hl))
	} else {
		_, _ = w.WriteString(html.EscapeString(code.String()))
	}
	_, _ = w.WriteString("</code></pre>\n")
	return ast.WalkSkipChildren, nil
}
