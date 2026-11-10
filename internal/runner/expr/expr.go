// Package expr evaluates GitHub Actions `${{ }}` expressions for atelier-runner.
//
// Values are nil, bool, float64, string, map[string]any and []any. Other Go
// values found in contexts are converted with Normalize when they are read.
package expr

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Env is the evaluation environment.
type Env struct {
	Contexts  map[string]any // named values (keys lower-case)
	JobStatus string         // "success" or "failure"
	Cancelled bool           // for cancelled(); when true success() is false
	HashFiles func(patterns []string) (string, error)
}

// filtered is the result of an object filter (`*`); derefs map over it.
type filtered []any

// ---------------------------------------------------------------- lexer

type tokKind int

const (
	tEOF tokKind = iota
	tNum
	tStr
	tIdent
	tPunct
)

type token struct {
	kind tokKind
	text string // ident name / punct / raw number
	str  string // decoded string literal
	num  float64
	pos  int
}

type syntaxError struct {
	src string
	pos int
	msg string
}

func (e *syntaxError) Error() string {
	return fmt.Sprintf("expression %q: %s at position %d", e.src, e.msg, e.pos)
}

func isIdentStart(c byte) bool { return c == '_' || unicode.IsLetter(rune(c)) }
func isIdentChar(c byte) bool {
	return c == '_' || c == '-' || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))
}
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func lex(src string) ([]token, error) {
	var toks []token
	errAt := func(pos int, f string, a ...any) error {
		return &syntaxError{src, pos, fmt.Sprintf(f, a...)}
	}
	for i := 0; i < len(src); {
		c := src[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		start := i
		prevDot := len(toks) > 0 && toks[len(toks)-1].kind == tPunct && toks[len(toks)-1].text == "."
		switch {
		case c == '\'':
			var sb strings.Builder
			i++
			for {
				if i >= len(src) {
					return nil, errAt(start, "unterminated string literal")
				}
				if src[i] == '\'' {
					if i+1 < len(src) && src[i+1] == '\'' {
						sb.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				sb.WriteByte(src[i])
				i++
			}
			toks = append(toks, token{kind: tStr, str: sb.String(), pos: start})
		case prevDot && isIdentChar(c), isIdentStart(c):
			for i < len(src) && isIdentChar(src[i]) {
				i++
			}
			toks = append(toks, token{kind: tIdent, text: src[start:i], pos: start})
		case isDigit(c) || (c == '-' && i+1 < len(src) && (isDigit(src[i+1]) || src[i+1] == '.')) ||
			(c == '.' && i+1 < len(src) && isDigit(src[i+1]) && !prevValue(toks)):
			if c == '-' {
				i++
			}
			hex := strings.HasPrefix(strings.ToLower(src[i:]), "0x")
			for i < len(src) {
				d := src[i]
				if isDigit(d) || d == '.' || unicode.IsLetter(rune(d)) {
					i++
				} else if (d == '+' || d == '-') && !hex && (src[i-1] == 'e' || src[i-1] == 'E') {
					i++
				} else {
					break
				}
			}
			raw := src[start:i]
			n, ok := parseNumber(raw)
			if !ok {
				return nil, errAt(start, "invalid number %q", raw)
			}
			toks = append(toks, token{kind: tNum, text: raw, num: n, pos: start})
		default:
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "<=", ">=", "==", "!=", "&&", "||":
				toks = append(toks, token{kind: tPunct, text: two, pos: start})
				i += 2
				continue
			}
			if strings.IndexByte("()[].!<>,*", c) < 0 {
				return nil, errAt(start, "unexpected character %q", c)
			}
			toks = append(toks, token{kind: tPunct, text: string(c), pos: start})
			i++
		}
	}
	return append(toks, token{kind: tEOF, pos: len(src)}), nil
}

// prevValue reports whether the previous token ends a value (so '.' is a deref).
func prevValue(toks []token) bool {
	if len(toks) == 0 {
		return false
	}
	t := toks[len(toks)-1]
	return t.kind != tPunct || t.text == ")" || t.text == "]" || t.text == "*"
}

// parseNumber parses a numeric literal or numeric string (decimal, hex 0x, exponent).
func parseNumber(s string) (float64, bool) {
	neg := false
	body := s
	if strings.HasPrefix(body, "-") {
		neg, body = true, body[1:]
	} else if strings.HasPrefix(body, "+") {
		body = body[1:]
	}
	if body == "" {
		return 0, false
	}
	var f float64
	if l := strings.ToLower(body); strings.HasPrefix(l, "0x") {
		u, err := strconv.ParseUint(l[2:], 16, 64)
		if err != nil {
			return 0, false
		}
		f = float64(u)
	} else if body == "Infinity" {
		f = math.Inf(1)
	} else {
		for i := 0; i < len(body); i++ {
			if c := body[i]; !isDigit(c) && strings.IndexByte(".eE+-", c) < 0 {
				return 0, false
			}
		}
		v, err := strconv.ParseFloat(body, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return 0, false
		}
		f = v
	}
	if neg {
		f = -f
	}
	return f, true
}

// ---------------------------------------------------------------- parser

type node interface{}

type (
	litNode  struct{ v any }
	nameNode struct{ name string }
	notNode  struct{ x node }
	binNode  struct {
		op   string
		l, r node
		pos  int
	}
	propNode struct {
		x    node
		name string // "" with star => filter
		star bool
	}
	indexNode struct {
		x, idx node
	}
	callNode struct {
		name string
		args []node
		pos  int
	}
)

type parser struct {
	src  string
	toks []token
	i    int
}

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token { t := p.toks[p.i]; p.i++; return t }
func (p *parser) is(s string) bool {
	t := p.peek()
	return t.kind == tPunct && t.text == s
}
func (p *parser) errf(pos int, f string, a ...any) error {
	return &syntaxError{p.src, pos, fmt.Sprintf(f, a...)}
}
func (p *parser) expect(s string) error {
	if !p.is(s) {
		t := p.peek()
		return p.errf(t.pos, "expected %q, found %s", s, describe(t))
	}
	p.i++
	return nil
}

func describe(t token) string {
	switch t.kind {
	case tEOF:
		return "end of expression"
	case tStr:
		return fmt.Sprintf("string '%s'", t.str)
	}
	return fmt.Sprintf("%q", t.text)
}

func parse(src string) (node, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	if p.peek().kind == tEOF {
		return nil, p.errf(0, "empty expression")
	}
	n, err := p.binary(0)
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tEOF {
		return nil, p.errf(t.pos, "unexpected %s", describe(t))
	}
	return n, nil
}

var levels = [][]string{{"||"}, {"&&"}, {"==", "!="}, {"<", "<=", ">", ">="}}

func (p *parser) binary(level int) (node, error) {
	if level == len(levels) {
		return p.unary()
	}
	l, err := p.binary(level + 1)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		match := false
		for _, op := range levels[level] {
			if t.kind == tPunct && t.text == op {
				match = true
			}
		}
		if !match {
			return l, nil
		}
		p.i++
		r, err := p.binary(level + 1)
		if err != nil {
			return nil, err
		}
		l = &binNode{op: t.text, l: l, r: r, pos: t.pos}
	}
}

func (p *parser) unary() (node, error) {
	if p.is("!") {
		p.i++
		x, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &notNode{x}, nil
	}
	return p.postfix()
}

func (p *parser) postfix() (node, error) {
	x, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.is("."):
			p.i++
			t := p.next()
			switch {
			case t.kind == tPunct && t.text == "*":
				x = &propNode{x: x, star: true}
			case t.kind == tIdent:
				x = &propNode{x: x, name: t.text}
			case t.kind == tNum && !strings.ContainsAny(t.text, ".-"):
				x = &propNode{x: x, name: t.text}
			default:
				return nil, p.errf(t.pos, "expected property name after '.', found %s", describe(t))
			}
		case p.is("["):
			p.i++
			if p.is("*") {
				p.i++
				x = &propNode{x: x, star: true}
			} else {
				idx, err := p.binary(0)
				if err != nil {
					return nil, err
				}
				x = &indexNode{x: x, idx: idx}
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
		default:
			return x, nil
		}
	}
}

func (p *parser) primary() (node, error) {
	t := p.next()
	switch t.kind {
	case tNum:
		return &litNode{t.num}, nil
	case tStr:
		return &litNode{t.str}, nil
	case tIdent:
		switch t.text {
		case "null":
			return &litNode{nil}, nil
		case "true":
			return &litNode{true}, nil
		case "false":
			return &litNode{false}, nil
		}
		if p.is("(") {
			p.i++
			call := &callNode{name: strings.ToLower(t.text), pos: t.pos}
			if !p.is(")") {
				for {
					a, err := p.binary(0)
					if err != nil {
						return nil, err
					}
					call.args = append(call.args, a)
					if !p.is(",") {
						break
					}
					p.i++
				}
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			if err := p.checkCall(call); err != nil {
				return nil, err
			}
			return call, nil
		}
		if !isIdentStart(t.text[0]) {
			return nil, p.errf(t.pos, "unexpected %s", describe(t))
		}
		return &nameNode{strings.ToLower(t.text)}, nil
	case tPunct:
		if t.text == "(" {
			x, err := p.binary(0)
			if err != nil {
				return nil, err
			}
			return x, p.expect(")")
		}
	}
	return nil, p.errf(t.pos, "unexpected %s", describe(t))
}

var arity = map[string][2]int{ // min, max (-1 = unbounded)
	"contains": {2, 2}, "startswith": {2, 2}, "endswith": {2, 2},
	"format": {1, -1}, "join": {1, 2}, "tojson": {1, 1}, "fromjson": {1, 1},
	"hashfiles": {1, -1}, "success": {0, 0}, "failure": {0, 0},
	"always": {0, 0}, "cancelled": {0, 0},
}

func (p *parser) checkCall(c *callNode) error {
	a, ok := arity[c.name]
	if !ok {
		return p.errf(c.pos, "unknown function %s()", c.name)
	}
	if n := len(c.args); n < a[0] || (a[1] >= 0 && n > a[1]) {
		return p.errf(c.pos, "wrong number of arguments to %s(): got %d", c.name, n)
	}
	return nil
}

// ---------------------------------------------------------------- evaluation

type evalError struct {
	src string
	pos int
	err error
}

func (e *evalError) Error() string {
	return fmt.Sprintf("expression %q: %v at position %d", e.src, e.err, e.pos)
}
func (e *evalError) Unwrap() error { return e.err }

type evaluator struct {
	env *Env
	src string
}

// unwrap strips a single surrounding ${{ }} if it spans the whole string.
func unwrap(src string) string {
	s := strings.TrimSpace(src)
	if strings.HasPrefix(s, "${{") {
		if end, ok := findClose(s, 3); ok && end+2 == len(s) {
			return strings.TrimSpace(s[3:end])
		}
	}
	return s
}

// Eval evaluates a bare expression (optionally wrapped in ${{ }}).
func (e *Env) Eval(src string) (any, error) {
	src = unwrap(src)
	n, err := parse(src)
	if err != nil {
		return nil, err
	}
	return e.run(src, n)
}

func (e *Env) run(src string, n node) (any, error) {
	ev := &evaluator{env: e, src: src}
	v, err := ev.eval(n)
	if err != nil {
		return nil, err
	}
	return final(v), nil
}

func final(v any) any {
	if f, ok := v.(filtered); ok {
		return []any(f)
	}
	return v
}

func (ev *evaluator) eval(n node) (any, error) {
	switch n := n.(type) {
	case *litNode:
		return n.v, nil
	case *nameNode:
		if ev.env == nil || ev.env.Contexts == nil {
			return nil, nil
		}
		return lookup(ev.env.Contexts, n.name), nil
	case *notNode:
		v, err := ev.eval(n.x)
		if err != nil {
			return nil, err
		}
		return !Truthy(v), nil
	case *propNode:
		x, err := ev.eval(n.x)
		if err != nil {
			return nil, err
		}
		if n.star {
			return star(x), nil
		}
		return deref(x, n.name), nil
	case *indexNode:
		x, err := ev.eval(n.x)
		if err != nil {
			return nil, err
		}
		idx, err := ev.eval(n.idx)
		if err != nil {
			return nil, err
		}
		return index(x, idx), nil
	case *binNode:
		l, err := ev.eval(n.l)
		if err != nil {
			return nil, err
		}
		switch n.op {
		case "&&":
			if !Truthy(l) {
				return l, nil
			}
			return ev.eval(n.r)
		case "||":
			if Truthy(l) {
				return l, nil
			}
			return ev.eval(n.r)
		}
		r, err := ev.eval(n.r)
		if err != nil {
			return nil, err
		}
		switch n.op {
		case "==":
			return looseEqual(l, r), nil
		case "!=":
			return !looseEqual(l, r), nil
		}
		return compare(n.op, l, r), nil
	case *callNode:
		args := make([]any, len(n.args))
		for i, a := range n.args {
			v, err := ev.eval(a)
			if err != nil {
				return nil, err
			}
			args[i] = v
		}
		v, err := ev.call(n.name, args)
		if err != nil {
			return nil, &evalError{ev.src, n.pos, fmt.Errorf("%s(): %w", n.name, err)}
		}
		return v, nil
	}
	return nil, fmt.Errorf("expression %q: internal error: unknown node %T", ev.src, n)
}

func canonical(v any) bool {
	switch v.(type) {
	case nil, bool, float64, string, map[string]any, []any, filtered:
		return true
	}
	return false
}

func norm(v any) any {
	if canonical(v) {
		return v
	}
	return Normalize(v)
}

// lookup is a case-insensitive map lookup; missing keys give nil.
func lookup(m map[string]any, key string) any {
	if v, ok := m[key]; ok {
		return norm(v)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if strings.EqualFold(k, key) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Strings(keys)
	return norm(m[keys[0]])
}

func deref(x any, name string) any {
	switch x := x.(type) {
	case map[string]any:
		return lookup(x, name)
	case filtered:
		var out filtered
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				if v := lookup(m, name); v != nil {
					out = append(out, v)
				}
			}
		}
		if out == nil {
			out = filtered{}
		}
		return out
	}
	return nil
}

func values(x any) []any {
	switch x := x.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, norm(x[k]))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = norm(v)
		}
		return out
	}
	return nil
}

func star(x any) any {
	out := filtered{}
	if f, ok := x.(filtered); ok {
		for _, item := range f {
			out = append(out, values(item)...)
		}
		return out
	}
	switch x.(type) {
	case map[string]any, []any:
		return append(out, values(x)...)
	}
	return out
}

func index(x, idx any) any {
	switch xv := x.(type) {
	case map[string]any:
		return lookup(xv, ToString(idx))
	case []any, filtered:
		arr, _ := asArray(xv)
		if s, ok := idx.(string); ok {
			if f, isF := xv.(filtered); isF {
				return deref(f, s)
			}
			if _, isNum := parseNumber(strings.TrimSpace(s)); !isNum {
				return nil
			}
		}
		n := toNumber(idx)
		if math.IsNaN(n) || n < 0 || n != math.Trunc(n) || n >= float64(len(arr)) {
			return nil
		}
		return norm(arr[int(n)])
	}
	return nil
}

func asArray(v any) ([]any, bool) {
	switch v := v.(type) {
	case []any:
		return v, true
	case filtered:
		return []any(v), true
	}
	return nil, false
}

// ---------------------------------------------------------------- coercion

// Truthy reports GitHub expression truthiness.
func Truthy(v any) bool {
	switch v := norm(v).(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0 && !math.IsNaN(v)
	case string:
		return v != ""
	}
	return true
}

func toNumber(v any) float64 {
	switch v := norm(v).(type) {
	case nil:
		return 0
	case bool:
		if v {
			return 1
		}
		return 0
	case float64:
		return v
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0
		}
		if n, ok := parseNumber(s); ok {
			return n
		}
	}
	return math.NaN()
}

func kind(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case bool:
		return 1
	case float64:
		return 2
	case string:
		return 3
	}
	return 4 // object / array
}

func sameInstance(a, b any) bool {
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	if ra.Kind() != rb.Kind() {
		return false
	}
	if ra.Kind() == reflect.Slice && (ra.Len() != rb.Len() || ra.Len() == 0) {
		return ra.Len() == 0 && rb.Len() == 0 && ra.Pointer() == rb.Pointer() && ra.Pointer() != 0
	}
	return ra.Pointer() == rb.Pointer()
}

func looseEqual(a, b any) bool {
	a, b = norm(a), norm(b)
	ka, kb := kind(a), kind(b)
	if ka != kb {
		x, y := toNumber(a), toNumber(b)
		return x == y // NaN never equal
	}
	switch ka {
	case 0:
		return true
	case 1:
		return a.(bool) == b.(bool)
	case 2:
		return a.(float64) == b.(float64)
	case 3:
		return strings.EqualFold(a.(string), b.(string))
	}
	return sameInstance(a, b)
}

func compare(op string, a, b any) bool {
	a, b = norm(a), norm(b)
	var c int
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			c = strings.Compare(strings.ToUpper(sa), strings.ToUpper(sb))
			goto done
		}
	}
	{
		x, y := toNumber(a), toNumber(b)
		if math.IsNaN(x) || math.IsNaN(y) {
			return false
		}
		switch {
		case x < y:
			c = -1
		case x > y:
			c = 1
		}
	}
done:
	switch op {
	case "<":
		return c < 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	}
	return c >= 0
}

// ToString converts a value to its GitHub string form.
func ToString(v any) string {
	switch v := norm(v).(type) {
	case nil:
		return ""
	case bool:
		if v {
			return "true"
		}
		return "false"
	case float64:
		return formatNumber(v)
	case string:
		return v
	default:
		s, _ := toJSON(v)
		return s
	}
}

func formatNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func toJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(jsonable(final(v))); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// jsonable replaces non-finite numbers (unsupported by encoding/json) with strings.
func jsonable(v any) any {
	switch v := norm(v).(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return formatNumber(v)
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = jsonable(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = jsonable(x)
		}
		return out
	case filtered:
		return jsonable([]any(v))
	default:
		return v
	}
}

// Normalize converts arbitrary Go values into expression values.
func Normalize(v any) any {
	switch v := v.(type) {
	case nil, bool, float64, string:
		return v
	case filtered:
		return []any(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return v.String()
		}
		return f
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = Normalize(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = Normalize(x)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = x
		}
		return out
	case []string:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = x
		}
		return out
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(rv.Uint())
	case reflect.Float32:
		return rv.Float()
	}
	if (rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice) && rv.IsNil() {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Sprint(v)
	}
	return out
}

// ---------------------------------------------------------------- functions

func (ev *evaluator) call(name string, a []any) (any, error) {
	e := ev.env
	switch name {
	case "contains":
		if arr, ok := asArray(norm(a[0])); ok {
			for _, x := range arr {
				if looseEqual(x, a[1]) {
					return true, nil
				}
			}
			return false, nil
		}
		return strings.Contains(strings.ToLower(ToString(a[0])), strings.ToLower(ToString(a[1]))), nil
	case "startswith":
		return strings.HasPrefix(strings.ToLower(ToString(a[0])), strings.ToLower(ToString(a[1]))), nil
	case "endswith":
		return strings.HasSuffix(strings.ToLower(ToString(a[0])), strings.ToLower(ToString(a[1]))), nil
	case "format":
		return format(ToString(a[0]), a[1:])
	case "join":
		sep := ","
		if len(a) > 1 {
			sep = ToString(a[1])
		}
		arr, ok := asArray(norm(a[0]))
		if !ok {
			return ToString(a[0]), nil
		}
		parts := make([]string, len(arr))
		for i, x := range arr {
			parts[i] = ToString(x)
		}
		return strings.Join(parts, sep), nil
	case "tojson":
		return toJSON(a[0])
	case "fromjson":
		s := ToString(a[0])
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("empty input")
		}
		var out any
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return nil, fmt.Errorf("invalid JSON: %v", err)
		}
		return out, nil
	case "hashfiles":
		if e == nil || e.HashFiles == nil {
			return nil, fmt.Errorf("hashFiles is not available in this context")
		}
		pats := make([]string, len(a))
		for i, x := range a {
			pats[i] = ToString(x)
		}
		return e.HashFiles(pats)
	case "success":
		return e != nil && e.JobStatus == "success" && !e.Cancelled, nil
	case "failure":
		return e != nil && e.JobStatus == "failure", nil
	case "cancelled":
		return e != nil && e.Cancelled, nil
	case "always":
		return true, nil
	}
	return nil, fmt.Errorf("unknown function")
}

func format(f string, args []any) (string, error) {
	var sb strings.Builder
	for i := 0; i < len(f); i++ {
		c := f[i]
		switch {
		case c == '{' && i+1 < len(f) && f[i+1] == '{':
			sb.WriteByte('{')
			i++
		case c == '}' && i+1 < len(f) && f[i+1] == '}':
			sb.WriteByte('}')
			i++
		case c == '{':
			end := strings.IndexByte(f[i:], '}')
			if end < 0 {
				return "", fmt.Errorf("unclosed '{' at %d in format string %q", i, f)
			}
			n, err := strconv.Atoi(f[i+1 : i+end])
			if err != nil || n < 0 || strings.ContainsAny(f[i+1:i+end], "+- ") {
				return "", fmt.Errorf("invalid placeholder %q in format string %q", f[i:i+end+1], f)
			}
			if n >= len(args) {
				return "", fmt.Errorf("placeholder {%d} out of range (%d argument(s)) in format string %q", n, len(args), f)
			}
			sb.WriteString(ToString(args[n]))
			i += end
		case c == '}':
			return "", fmt.Errorf("unmatched '}' at %d in format string %q", i, f)
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String(), nil
}

// ---------------------------------------------------------------- templates

// findClose returns the index of the "}}" closing a ${{ opened before from,
// ignoring "}}" inside single-quoted string literals.
func findClose(s string, from int) (int, bool) {
	inStr := false
	for i := from; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			inStr = !inStr // '' escapes toggle twice
		case !inStr && s[i] == '}' && i+1 < len(s) && s[i+1] == '}':
			return i, true
		}
	}
	return 0, false
}

type segment struct {
	lit  string
	expr string // non-empty for expression segments
	pos  int
	isEx bool
}

func split(s string) ([]segment, error) {
	var segs []segment
	for {
		i := strings.Index(s, "${{")
		if i < 0 {
			if s != "" {
				segs = append(segs, segment{lit: s})
			}
			return segs, nil
		}
		end, ok := findClose(s, i+3)
		if !ok {
			return nil, fmt.Errorf("template %q: unterminated '${{' at position %d", s, i)
		}
		if i > 0 {
			segs = append(segs, segment{lit: s[:i]})
		}
		segs = append(segs, segment{expr: strings.TrimSpace(s[i+3 : end]), pos: i, isEx: true})
		s = s[end+2:]
	}
}

// Interpolate replaces every ${{ expr }} in s with ToString(value).
func (e *Env) Interpolate(s string) (string, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	segs, err := split(s)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, seg := range segs {
		if !seg.isEx {
			sb.WriteString(seg.lit)
			continue
		}
		v, err := e.Eval(seg.expr)
		if err != nil {
			return "", err
		}
		sb.WriteString(ToString(v))
	}
	return sb.String(), nil
}

// HasStatusFunc reports whether the expression calls success/failure/always/cancelled.
func HasStatusFunc(src string) bool {
	src = strings.TrimSpace(src)
	if strings.Contains(src, "${{") && unwrap(src) == src {
		segs, err := split(src)
		if err != nil {
			return false
		}
		for _, s := range segs {
			if s.isEx && HasStatusFunc(s.expr) {
				return true
			}
		}
		return false
	}
	n, err := parse(unwrap(src))
	return err == nil && hasStatus(n)
}

func hasStatus(n node) bool {
	switch n := n.(type) {
	case *notNode:
		return hasStatus(n.x)
	case *binNode:
		return hasStatus(n.l) || hasStatus(n.r)
	case *propNode:
		return hasStatus(n.x)
	case *indexNode:
		return hasStatus(n.x) || hasStatus(n.idx)
	case *callNode:
		switch n.name {
		case "success", "failure", "always", "cancelled":
			return true
		}
		for _, a := range n.args {
			if hasStatus(a) {
				return true
			}
		}
	}
	return false
}

// EvalCondition evaluates an `if:` condition with GitHub semantics: an empty
// condition means success(), and conditions without a status function are
// evaluated as success() && (expr).
func (e *Env) EvalCondition(src string) (bool, error) {
	s := strings.TrimSpace(src)
	if s == "" {
		s = "success()"
	}
	implicit := !HasStatusFunc(s)
	if inner := unwrap(s); inner != s || !strings.Contains(s, "${{") {
		n, err := parse(inner)
		if err != nil {
			return false, err
		}
		if implicit && !(e.JobStatus == "success" && !e.Cancelled) {
			return false, nil
		}
		v, err := e.run(inner, n)
		if err != nil {
			return false, err
		}
		return Truthy(v), nil
	}
	// Mixed literal text and ${{ }}: the result is a string, truthy when non-empty.
	out, err := e.Interpolate(s)
	if err != nil {
		return false, err
	}
	if implicit && !(e.JobStatus == "success" && !e.Cancelled) {
		return false, nil
	}
	return out != "", nil
}
