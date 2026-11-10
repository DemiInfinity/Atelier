package expr

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Reduce evaluates what it can of src and returns an equivalent expression in
// which every known part is a literal. When the whole expression is known,
// known is true and val holds its value.
func (e *Evaluator) Reduce(src string) (out string, val any, known bool, err error) {
	n, err := parse(src)
	if err != nil {
		return "", nil, false, err
	}
	r, err := e.reduce(n, 0)
	if err != nil {
		return "", nil, false, err
	}
	if l, ok := r.(litNode); ok {
		return render(r), l.v, true, nil
	}
	return render(r), nil, false, nil
}

func (e *Evaluator) reduce(n node, depth int) (node, error) {
	if depth > maxDepth*4 {
		return nil, fmt.Errorf("expression is nested too deeply")
	}
	depth++
	// Fully known subtrees fold into a literal.
	if v, ok, err := e.eval(n, depth); err != nil {
		return nil, err
	} else if ok {
		return litNode{v: v}, nil
	}
	switch x := n.(type) {
	case groupNode:
		in, err := e.reduce(x.x, depth)
		if err != nil {
			return nil, err
		}
		return groupNode{x: in}, nil
	case propNode:
		obj, err := e.reduce(x.obj, depth)
		if err != nil {
			return nil, err
		}
		return propNode{obj: obj, name: x.name}, nil
	case indexNode:
		obj, err := e.reduce(x.obj, depth)
		if err != nil {
			return nil, err
		}
		idx, err := e.reduce(x.idx, depth)
		if err != nil {
			return nil, err
		}
		return indexNode{obj: obj, idx: idx}, nil
	case starNode:
		obj, err := e.reduce(x.obj, depth)
		if err != nil {
			return nil, err
		}
		return starNode{obj: obj}, nil
	case notNode:
		in, err := e.reduce(x.x, depth)
		if err != nil {
			return nil, err
		}
		return notNode{x: in}, nil
	case binNode:
		l, err := e.reduce(x.l, depth)
		if err != nil {
			return nil, err
		}
		if lit, ok := l.(litNode); ok && (x.op == tAnd || x.op == tOr) {
			// A known left side decides which way && and || go.
			if (x.op == tAnd) == Truthy(lit.v) {
				return e.reduce(x.r, depth)
			}
			return l, nil
		}
		r, err := e.reduce(x.r, depth)
		if err != nil {
			return nil, err
		}
		return binNode{op: x.op, l: l, r: r}, nil
	case callNode:
		args := make([]node, len(x.args))
		for i, a := range x.args {
			r, err := e.reduce(a, depth)
			if err != nil {
				return nil, err
			}
			args[i] = r
		}
		return callNode{name: x.name, args: args}, nil
	}
	return n, nil
}

var funcNames = map[string]string{
	"contains": "contains", "startswith": "startsWith", "endswith": "endsWith", "format": "format",
	"join": "join", "tojson": "toJSON", "fromjson": "fromJSON", "hashfiles": "hashFiles",
	"success": "success", "failure": "failure", "always": "always", "cancelled": "cancelled",
}

var opText = map[tokKind]string{
	tEq: "==", tNe: "!=", tLt: "<", tLe: "<=", tGt: ">", tGe: ">=", tAnd: "&&", tOr: "||",
}

func render(n node) string {
	switch x := n.(type) {
	case litNode:
		return Literal(x.v)
	case ctxNode:
		return x.name
	case groupNode:
		return "(" + render(x.x) + ")"
	case propNode:
		if isIdentName(x.name) {
			return render(x.obj) + "." + x.name
		}
		return render(x.obj) + "[" + Literal(x.name) + "]"
	case indexNode:
		return render(x.obj) + "[" + render(x.idx) + "]"
	case starNode:
		return render(x.obj) + ".*"
	case notNode:
		return "!" + wrap(x.x)
	case binNode:
		return wrap(x.l) + " " + opText[x.op] + " " + wrap(x.r)
	case callNode:
		parts := make([]string, len(x.args))
		for i, a := range x.args {
			parts[i] = render(a)
		}
		return funcNames[x.name] + "(" + strings.Join(parts, ", ") + ")"
	}
	return ""
}

func wrap(n node) string {
	if _, ok := n.(binNode); ok {
		return "(" + render(n) + ")"
	}
	return render(n)
}

func isIdentName(s string) bool {
	if s == "" || !isIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isIdentChar(s[i]) && s[i] != '-' {
			return false
		}
	}
	return true
}

// Literal renders a value as expression source.
func Literal(v any) string {
	switch x := normalize(v).(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "fromJSON('null')"
		}
		return formatNumber(x)
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return "null"
		}
		return "fromJSON(" + Literal(string(b)) + ")"
	}
}

// ---------- ${{ }} interpolation ----------

// segment is one part of a string: literal text or an expression.
type segment struct {
	text   string
	isExpr bool
}

// split breaks s into text and ${{ }} expression segments.
func split(s string) ([]segment, error) {
	var out []segment
	for {
		i := strings.Index(s, "${{")
		if i < 0 {
			if s != "" {
				out = append(out, segment{text: s})
			}
			return out, nil
		}
		if i > 0 {
			out = append(out, segment{text: s[:i]})
		}
		rest := s[i+3:]
		end := -1
		inStr := false
		for j := 0; j < len(rest); j++ {
			if rest[j] == '\'' {
				inStr = !inStr
				continue
			}
			if !inStr && strings.HasPrefix(rest[j:], "}}") {
				end = j
				break
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("unclosed ${{ in %q", truncate(s, 60))
		}
		out = append(out, segment{text: strings.TrimSpace(rest[:end]), isExpr: true})
		s = rest[end+2:]
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// HasExpr reports whether s contains a ${{ }} expression.
func HasExpr(s string) bool { return strings.Contains(s, "${{") }

// Expressions returns the expressions inside s's ${{ }} blocks.
func Expressions(s string) ([]string, error) {
	segs, err := split(s)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, sg := range segs {
		if sg.isExpr {
			out = append(out, sg.text)
		}
	}
	return out, nil
}

// Interpolate resolves the ${{ }} blocks in s that the evaluator can, and
// reduces the rest. Resolved values never introduce new ${{ }} blocks: a
// value containing "${{" is kept as a quoted literal expression, so text from
// event payloads can't smuggle expressions to the runner.
func (e *Evaluator) Interpolate(s string) (string, error) {
	if !HasExpr(s) {
		return s, nil
	}
	segs, err := split(s)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, sg := range segs {
		if !sg.isExpr {
			b.WriteString(sg.text)
			continue
		}
		out, val, known, err := e.Reduce(sg.text)
		if err != nil {
			return "", fmt.Errorf("in ${{ %s }}: %w", truncate(sg.text, 80), err)
		}
		if known {
			str := ToString(val)
			if strings.Contains(str, "${{") {
				b.WriteString("${{ " + Literal(str) + " }}")
			} else {
				b.WriteString(str)
			}
			continue
		}
		b.WriteString("${{ " + out + " }}")
	}
	return b.String(), nil
}

// Strip removes one ${{ }} wrapper around a whole condition ("${{ x }}" → "x").
func Strip(cond string) string {
	c := strings.TrimSpace(cond)
	if strings.HasPrefix(c, "${{") && strings.HasSuffix(c, "}}") {
		segs, err := split(c)
		if err == nil && len(segs) == 1 && segs[0].isExpr {
			return segs[0].text
		}
	}
	return c
}

// EvalString interpolates s fully; it fails if anything stays unresolved.
func (e *Evaluator) EvalString(s string) (string, error) {
	out, err := e.Interpolate(s)
	if err != nil {
		return "", err
	}
	if HasExpr(out) {
		// A resolved value may legitimately contain ${{ (kept as a literal);
		// check whether what's left is only literals.
		segs, _ := split(out)
		var b strings.Builder
		for _, sg := range segs {
			if !sg.isExpr {
				b.WriteString(sg.text)
				continue
			}
			v, err := (&Evaluator{Ctx: Context{}, Known: func(string) bool { return false }}).Eval(sg.text)
			if err != nil {
				return "", ErrUnresolved
			}
			b.WriteString(ToString(v))
		}
		return b.String(), nil
	}
	return out, nil
}
