// Package expr evaluates GitHub Actions style ${{ }} expressions.
//
// It supports literals (null, booleans, numbers, 'strings'), contexts with
// property access (a.b, a['b'], a.*.b), the operators ! == != < <= > >= && ||
// and grouping, and the functions contains, startsWith, endsWith, format,
// join, toJSON, fromJSON, success, failure, always, cancelled and hashFiles.
//
// Besides full evaluation, expressions can be evaluated partially: contexts
// the caller knows (for the Atelier server: github, inputs, vars, matrix,
// needs…) are folded into literals and everything else is left in place for
// the runner, which resolves steps.*, env.*, secrets.* and friends at run time.
package expr

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------- lexer ----------

type tokKind int

const (
	tEOF tokKind = iota
	tNull
	tBool
	tNumber
	tString
	tIdent
	tDot
	tStar
	tLBracket
	tRBracket
	tLParen
	tRParen
	tComma
	tNot
	tEq
	tNe
	tLt
	tLe
	tGt
	tGe
	tAnd
	tOr
)

type token struct {
	kind tokKind
	text string
	num  float64
	pos  int
}

// MaxLength bounds the size of one expression.
const MaxLength = 64 * 1024

func lex(src string) ([]token, error) {
	if len(src) > MaxLength {
		return nil, fmt.Errorf("expression is too long")
	}
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
			continue
		case c == '\'':
			var b strings.Builder
			j := i + 1
			closed := false
			for j < len(src) {
				if src[j] == '\'' {
					if j+1 < len(src) && src[j+1] == '\'' {
						b.WriteByte('\'')
						j += 2
						continue
					}
					closed = true
					j++
					break
				}
				b.WriteByte(src[j])
				j++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string at position %d", i)
			}
			out = append(out, token{kind: tString, text: b.String(), pos: i})
			i = j
			continue
		case c >= '0' && c <= '9' || (c == '-' || c == '+' || c == '.') && i+1 < len(src) && isDigit(src[i+1]) && numberAllowed(out):
			j := i + 1
			for j < len(src) && (isIdentChar(src[j]) || src[j] == '.' ||
				((src[j] == '-' || src[j] == '+') && (src[j-1] == 'e' || src[j-1] == 'E') && !strings.HasPrefix(strings.ToLower(src[i:]), "0x"))) {
				j++
			}
			text := src[i:j]
			n, err := parseNumber(text)
			if err != nil {
				return nil, fmt.Errorf("bad number %q at position %d", text, i)
			}
			out = append(out, token{kind: tNumber, text: text, num: n, pos: i})
			i = j
			continue
		case isIdentStart(c):
			j := i + 1
			for j < len(src) && (isIdentChar(src[j]) || src[j] == '-') {
				j++
			}
			word := src[i:j]
			switch word {
			case "null":
				out = append(out, token{kind: tNull, text: word, pos: i})
			case "true", "false":
				out = append(out, token{kind: tBool, text: word, pos: i})
			default:
				out = append(out, token{kind: tIdent, text: word, pos: i})
			}
			i = j
			continue
		}
		two := ""
		if i+1 < len(src) {
			two = src[i : i+2]
		}
		switch two {
		case "==":
			out = append(out, token{kind: tEq, text: two, pos: i})
			i += 2
			continue
		case "!=":
			out = append(out, token{kind: tNe, text: two, pos: i})
			i += 2
			continue
		case "<=":
			out = append(out, token{kind: tLe, text: two, pos: i})
			i += 2
			continue
		case ">=":
			out = append(out, token{kind: tGe, text: two, pos: i})
			i += 2
			continue
		case "&&":
			out = append(out, token{kind: tAnd, text: two, pos: i})
			i += 2
			continue
		case "||":
			out = append(out, token{kind: tOr, text: two, pos: i})
			i += 2
			continue
		}
		k := tEOF
		switch c {
		case '.':
			k = tDot
		case '*':
			k = tStar
		case '[':
			k = tLBracket
		case ']':
			k = tRBracket
		case '(':
			k = tLParen
		case ')':
			k = tRParen
		case ',':
			k = tComma
		case '!':
			k = tNot
		case '<':
			k = tLt
		case '>':
			k = tGt
		default:
			return nil, fmt.Errorf("unexpected character %q at position %d", c, i)
		}
		out = append(out, token{kind: k, text: string(c), pos: i})
		i++
	}
	out = append(out, token{kind: tEOF, pos: len(src)})
	return out, nil
}

// numberAllowed: a sign or dot starts a number only where an operand may start.
func numberAllowed(prev []token) bool {
	if len(prev) == 0 {
		return true
	}
	switch prev[len(prev)-1].kind {
	case tIdent, tNumber, tString, tNull, tBool, tRParen, tRBracket, tStar:
		return false
	}
	return true
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isIdentChar(c byte) bool  { return isIdentStart(c) || isDigit(c) }

func parseNumber(s string) (float64, error) {
	ls := strings.ToLower(s)
	neg := false
	if strings.HasPrefix(ls, "-") {
		neg, ls = true, ls[1:]
	} else if strings.HasPrefix(ls, "+") {
		ls = ls[1:]
	}
	var n float64
	switch {
	case strings.HasPrefix(ls, "0x"):
		v, err := strconv.ParseInt(ls[2:], 16, 64)
		if err != nil {
			return 0, err
		}
		n = float64(v)
	case strings.HasPrefix(ls, "0o"):
		v, err := strconv.ParseInt(ls[2:], 8, 64)
		if err != nil {
			return 0, err
		}
		n = float64(v)
	default:
		v, err := strconv.ParseFloat(ls, 64)
		if err != nil {
			return 0, err
		}
		n = v
	}
	if neg {
		n = -n
	}
	return n, nil
}

// ---------- AST ----------

type node interface{}

type (
	litNode  struct{ v any }
	ctxNode  struct{ name string } // a top-level context (github, matrix…)
	propNode struct {
		obj  node
		name string // property name for .name
	}
	indexNode struct {
		obj, idx node
	}
	starNode struct{ obj node } // obj.* or obj[*]
	notNode  struct{ x node }
	binNode  struct {
		op   tokKind
		l, r node
	}
	callNode struct {
		name string
		args []node
	}
	groupNode struct{ x node }
)

type parser struct {
	toks []token
	i    int
	src  string
}

// maxDepth guards against pathological nesting.
const maxDepth = 100

func parse(src string) (node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, src: src}
	n, err := p.or(0)
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, fmt.Errorf("unexpected %q at position %d", p.peek().text, p.peek().pos)
	}
	return n, nil
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) or(d int) (node, error) {
	if d > maxDepth {
		return nil, fmt.Errorf("expression is nested too deeply")
	}
	l, err := p.and(d)
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tOr {
		p.next()
		r, err := p.and(d)
		if err != nil {
			return nil, err
		}
		l = binNode{op: tOr, l: l, r: r}
	}
	return l, nil
}

func (p *parser) and(d int) (node, error) {
	l, err := p.equality(d)
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tAnd {
		p.next()
		r, err := p.equality(d)
		if err != nil {
			return nil, err
		}
		l = binNode{op: tAnd, l: l, r: r}
	}
	return l, nil
}

func (p *parser) equality(d int) (node, error) {
	l, err := p.comparison(d)
	if err != nil {
		return nil, err
	}
	for k := p.peek().kind; k == tEq || k == tNe; k = p.peek().kind {
		p.next()
		r, err := p.comparison(d)
		if err != nil {
			return nil, err
		}
		l = binNode{op: k, l: l, r: r}
	}
	return l, nil
}

func (p *parser) comparison(d int) (node, error) {
	l, err := p.unary(d)
	if err != nil {
		return nil, err
	}
	for k := p.peek().kind; k == tLt || k == tLe || k == tGt || k == tGe; k = p.peek().kind {
		p.next()
		r, err := p.unary(d)
		if err != nil {
			return nil, err
		}
		l = binNode{op: k, l: l, r: r}
	}
	return l, nil
}

func (p *parser) unary(d int) (node, error) {
	if p.peek().kind == tNot {
		p.next()
		if d > maxDepth {
			return nil, fmt.Errorf("expression is nested too deeply")
		}
		x, err := p.unary(d + 1)
		if err != nil {
			return nil, err
		}
		return notNode{x: x}, nil
	}
	return p.postfix(d)
}

func (p *parser) postfix(d int) (node, error) {
	n, err := p.primary(d)
	if err != nil {
		return nil, err
	}
	for {
		switch p.peek().kind {
		case tDot:
			p.next()
			t := p.next()
			switch t.kind {
			case tStar:
				n = starNode{obj: n}
			case tIdent, tNull, tBool:
				n = propNode{obj: n, name: t.text}
			case tNumber: // e.g. matrix.node.18 (unusual but harmless)
				n = propNode{obj: n, name: t.text}
			default:
				return nil, fmt.Errorf("expected a property name after '.' at position %d", t.pos)
			}
		case tLBracket:
			p.next()
			if p.peek().kind == tStar {
				p.next()
				if p.next().kind != tRBracket {
					return nil, fmt.Errorf("expected ']' at position %d", p.peek().pos)
				}
				n = starNode{obj: n}
				continue
			}
			idx, err := p.or(d + 1)
			if err != nil {
				return nil, err
			}
			if t := p.next(); t.kind != tRBracket {
				return nil, fmt.Errorf("expected ']' at position %d", t.pos)
			}
			n = indexNode{obj: n, idx: idx}
		default:
			return n, nil
		}
	}
}

func (p *parser) primary(d int) (node, error) {
	t := p.next()
	switch t.kind {
	case tNull:
		return litNode{v: nil}, nil
	case tBool:
		return litNode{v: t.text == "true"}, nil
	case tNumber:
		return litNode{v: t.num}, nil
	case tString:
		return litNode{v: t.text}, nil
	case tLParen:
		x, err := p.or(d + 1)
		if err != nil {
			return nil, err
		}
		if c := p.next(); c.kind != tRParen {
			return nil, fmt.Errorf("expected ')' at position %d", c.pos)
		}
		return groupNode{x: x}, nil
	case tIdent:
		if p.peek().kind == tLParen {
			p.next()
			var args []node
			if p.peek().kind != tRParen {
				for {
					a, err := p.or(d + 1)
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.peek().kind == tComma {
						p.next()
						continue
					}
					break
				}
			}
			if c := p.next(); c.kind != tRParen {
				return nil, fmt.Errorf("expected ')' at position %d", c.pos)
			}
			name := strings.ToLower(t.text)
			if _, ok := knownFuncs[name]; !ok {
				return nil, fmt.Errorf("unknown function %s()", t.text)
			}
			return callNode{name: name, args: args}, nil
		}
		return ctxNode{name: strings.ToLower(t.text)}, nil
	case tEOF:
		return nil, fmt.Errorf("unexpected end of expression")
	}
	return nil, fmt.Errorf("unexpected %q at position %d", t.text, t.pos)
}

// knownFuncs maps function names to their allowed argument counts (min, max; -1 = any).
var knownFuncs = map[string][2]int{
	"contains":   {2, 2},
	"startswith": {2, 2},
	"endswith":   {2, 2},
	"format":     {1, -1},
	"join":       {1, 2},
	"tojson":     {1, 1},
	"fromjson":   {1, 1},
	"hashfiles":  {1, -1},
	"success":    {0, 0},
	"failure":    {0, 0},
	"always":     {0, 0},
	"cancelled":  {0, 0},
}

// statusFuncs need the job/step status and are resolved where that is known.
func isStatusFunc(name string) bool {
	switch name {
	case "success", "failure", "always", "cancelled":
		return true
	}
	return false
}

// Check parses an expression and reports syntax errors.
func Check(src string) error {
	_, err := parse(src)
	return err
}

// Roots returns the top-level contexts an expression refers to (lowercase).
func Roots(src string) ([]string, error) {
	n, err := parse(src)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	walk(n, func(x node) {
		if c, ok := x.(ctxNode); ok && !seen[c.name] {
			seen[c.name] = true
			out = append(out, c.name)
		}
	})
	return out, nil
}

// UsesStatusFunc reports whether the expression calls success(), failure(),
// always() or cancelled().
func UsesStatusFunc(src string) bool {
	n, err := parse(src)
	if err != nil {
		return false
	}
	found := false
	walk(n, func(x node) {
		if c, ok := x.(callNode); ok && isStatusFunc(c.name) {
			found = true
		}
	})
	return found
}

func walk(n node, f func(node)) {
	f(n)
	switch x := n.(type) {
	case propNode:
		walk(x.obj, f)
	case indexNode:
		walk(x.obj, f)
		walk(x.idx, f)
	case starNode:
		walk(x.obj, f)
	case notNode:
		walk(x.x, f)
	case binNode:
		walk(x.l, f)
		walk(x.r, f)
	case callNode:
		for _, a := range x.args {
			walk(a, f)
		}
	case groupNode:
		walk(x.x, f)
	}
}
