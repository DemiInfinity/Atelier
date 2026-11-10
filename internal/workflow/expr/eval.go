package expr

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Context holds the top-level contexts by lowercase name (github, matrix…).
// Values are JSON-like: nil, bool, float64 (ints are accepted too), string,
// []any and map[string]any.
type Context map[string]any

// Status is the job/step status the status functions read. A nil *Status
// means it isn't known here, so expressions using them stay unresolved.
type Status struct {
	Success   bool // every previous step/needed job succeeded
	Failure   bool // something failed
	Cancelled bool // the run was cancelled
}

// Evaluator evaluates expressions against a context.
type Evaluator struct {
	Ctx Context
	// Known says which top-level contexts this evaluator may resolve. nil
	// means every context in Ctx (and any other is null).
	Known func(root string) bool
	// Status for success()/failure()/always()/cancelled(); nil leaves them unresolved.
	Status *Status
}

// ErrUnresolved means an expression needs contexts the evaluator doesn't have.
var ErrUnresolved = errors.New("expression needs run-time context")

// Eval evaluates src fully.
func (e *Evaluator) Eval(src string) (any, error) {
	n, err := parse(src)
	if err != nil {
		return nil, err
	}
	v, ok, err := e.eval(n, 0)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrUnresolved
	}
	return v, nil
}

// EvalBool evaluates src and returns its truthiness.
func (e *Evaluator) EvalBool(src string) (bool, error) {
	v, err := e.Eval(src)
	if err != nil {
		return false, err
	}
	return Truthy(v), nil
}

func (e *Evaluator) knows(root string) bool {
	if e.Known == nil {
		return true
	}
	return e.Known(root)
}

// eval returns the value and whether it's known.
func (e *Evaluator) eval(n node, depth int) (any, bool, error) {
	if depth > maxDepth*4 {
		return nil, false, fmt.Errorf("expression is nested too deeply")
	}
	depth++
	switch x := n.(type) {
	case litNode:
		return x.v, true, nil
	case groupNode:
		return e.eval(x.x, depth)
	case ctxNode:
		if !e.knows(x.name) {
			return nil, false, nil
		}
		return normalize(e.Ctx[x.name]), true, nil
	case propNode:
		obj, ok, err := e.eval(x.obj, depth)
		if err != nil || !ok {
			return nil, ok, err
		}
		return property(obj, x.name), true, nil
	case indexNode:
		obj, ok1, err := e.eval(x.obj, depth)
		if err != nil {
			return nil, false, err
		}
		idx, ok2, err := e.eval(x.idx, depth)
		if err != nil || !ok1 || !ok2 {
			return nil, false, err
		}
		return index(obj, idx), true, nil
	case starNode:
		obj, ok, err := e.eval(x.obj, depth)
		if err != nil || !ok {
			return nil, ok, err
		}
		return star(obj), true, nil
	case notNode:
		v, ok, err := e.eval(x.x, depth)
		if err != nil || !ok {
			return nil, ok, err
		}
		return !Truthy(v), true, nil
	case binNode:
		l, okL, err := e.eval(x.l, depth)
		if err != nil {
			return nil, false, err
		}
		switch x.op {
		case tAnd:
			if okL && !Truthy(l) {
				return l, true, nil
			}
			r, okR, err := e.eval(x.r, depth)
			if err != nil {
				return nil, false, err
			}
			if !okL {
				return nil, false, nil // an unknown left side decides the value
			}
			return r, okR, nil
		case tOr:
			if okL && Truthy(l) {
				return l, true, nil
			}
			r, okR, err := e.eval(x.r, depth)
			if err != nil {
				return nil, false, err
			}
			if !okL {
				return nil, false, nil
			}
			return r, okR, nil
		}
		r, okR, err := e.eval(x.r, depth)
		if err != nil || !okL || !okR {
			return nil, false, err
		}
		return compare(x.op, l, r), true, nil
	case callNode:
		return e.call(x, depth)
	}
	return nil, false, fmt.Errorf("bad expression")
}

func (e *Evaluator) call(c callNode, depth int) (any, bool, error) {
	lim := knownFuncs[c.name]
	if len(c.args) < lim[0] || (lim[1] >= 0 && len(c.args) > lim[1]) {
		return nil, false, fmt.Errorf("%s() takes %s", c.name, argCount(lim))
	}
	if isStatusFunc(c.name) {
		if e.Status == nil {
			return nil, false, nil
		}
		switch c.name {
		case "success":
			return e.Status.Success && !e.Status.Cancelled, true, nil
		case "failure":
			return e.Status.Failure, true, nil
		case "always":
			return true, true, nil
		case "cancelled":
			return e.Status.Cancelled, true, nil
		}
	}
	if c.name == "hashfiles" {
		return nil, false, nil // needs the workspace: the runner resolves it
	}
	args := make([]any, len(c.args))
	allKnown := true
	for i, a := range c.args {
		v, ok, err := e.eval(a, depth)
		if err != nil {
			return nil, false, err
		}
		args[i] = v
		allKnown = allKnown && ok
	}
	if !allKnown {
		return nil, false, nil
	}
	v, err := callFunc(c.name, args)
	return v, err == nil, err
}

func argCount(lim [2]int) string {
	switch {
	case lim[1] < 0:
		return fmt.Sprintf("at least %d argument(s)", lim[0])
	case lim[0] == lim[1]:
		return fmt.Sprintf("%d argument(s)", lim[0])
	}
	return fmt.Sprintf("%d to %d arguments", lim[0], lim[1])
}

func callFunc(name string, a []any) (any, error) {
	switch name {
	case "contains":
		if arr, ok := a[0].([]any); ok {
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
		if arr, ok := a[0].([]any); ok {
			parts := make([]string, len(arr))
			for i, x := range arr {
				parts[i] = ToString(x)
			}
			return strings.Join(parts, sep), nil
		}
		return ToString(a[0]), nil
	case "tojson":
		b, err := json.MarshalIndent(a[0], "", "  ")
		if err != nil {
			return nil, err
		}
		return string(b), nil
	case "fromjson":
		s := ToString(a[0])
		if len(s) > MaxLength*16 {
			return nil, fmt.Errorf("fromJSON: input too large")
		}
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("fromJSON: %v", err)
		}
		return normalize(v), nil
	}
	return nil, fmt.Errorf("unknown function %s()", name)
}

func format(f string, args []any) (string, error) {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		c := f[i]
		switch {
		case c == '{' && i+1 < len(f) && f[i+1] == '{':
			b.WriteByte('{')
			i++
		case c == '}' && i+1 < len(f) && f[i+1] == '}':
			b.WriteByte('}')
			i++
		case c == '{':
			j := strings.IndexByte(f[i:], '}')
			if j < 0 {
				return "", fmt.Errorf("format: unclosed '{'")
			}
			n, err := strconv.Atoi(f[i+1 : i+j])
			if err != nil || n < 0 || n >= len(args) {
				return "", fmt.Errorf("format: bad placeholder %q", f[i:i+j+1])
			}
			b.WriteString(ToString(args[n]))
			i += j
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// ---------- values ----------

// normalize turns ints and typed maps/slices into the JSON-like value set.
func normalize(v any) any {
	switch x := v.(type) {
	case nil, bool, float64, string, []any, map[string]any:
		return v
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case float32:
		return float64(x)
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(x))
		for k, s := range x {
			out[k] = s
		}
		return out
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	// Anything else: round-trip through JSON.
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

func property(obj any, name string) any {
	switch o := obj.(type) {
	case map[string]any:
		if v, ok := o[name]; ok {
			return normalize(v)
		}
		for k, v := range o { // property names are case-insensitive
			if strings.EqualFold(k, name) {
				return normalize(v)
			}
		}
	case []any: // filtered array (a.*.b)
		var out []any
		for _, x := range o {
			if m, ok := x.(map[string]any); ok {
				if v := property(m, name); v != nil {
					out = append(out, v)
				}
			}
		}
		return out
	}
	return nil
}

func index(obj, idx any) any {
	switch o := obj.(type) {
	case map[string]any:
		return property(o, ToString(idx))
	case []any:
		f := ToNumber(idx)
		if math.IsNaN(f) || f < 0 || f != math.Trunc(f) || int(f) >= len(o) {
			return nil
		}
		return normalize(o[int(f)])
	}
	return nil
}

func star(obj any) any {
	switch o := obj.(type) {
	case []any:
		return o
	case map[string]any:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(o))
		for _, k := range keys {
			out = append(out, normalize(o[k]))
		}
		return out
	}
	return []any{}
}

// Truthy follows the Actions rules: false, 0, -0, NaN, "" and null are falsy.
func Truthy(v any) bool {
	switch x := normalize(v).(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

// ToNumber coerces a value to a number (NaN when it can't be).
func ToNumber(v any) float64 {
	switch x := normalize(v).(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0
		}
		f, err := parseNumber(s)
		if err != nil {
			return math.NaN()
		}
		return f
	}
	return math.NaN()
}

// ToString renders a value the way interpolation does.
func ToString(v any) string {
	switch x := normalize(v).(type) {
	case nil:
		return ""
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return formatNumber(x)
	case string:
		return x
	default:
		b, err := json.MarshalIndent(x, "", "  ")
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func formatNumber(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
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
	case []any:
		return 4
	case map[string]any:
		return 5
	}
	return 6
}

func looseEqual(a, b any) bool {
	a, b = normalize(a), normalize(b)
	ka, kb := kind(a), kind(b)
	if ka == kb {
		switch x := a.(type) {
		case nil:
			return true
		case bool:
			return x == b.(bool)
		case float64:
			return x == b.(float64)
		case string:
			return strings.EqualFold(x, b.(string))
		}
		return false // objects and arrays compare by identity
	}
	if ka >= 4 || kb >= 4 {
		return false
	}
	na, nb := ToNumber(a), ToNumber(b)
	return !math.IsNaN(na) && !math.IsNaN(nb) && na == nb
}

func compare(op tokKind, a, b any) any {
	switch op {
	case tEq:
		return looseEqual(a, b)
	case tNe:
		return !looseEqual(a, b)
	}
	a, b = normalize(a), normalize(b)
	if sa, ok := a.(string); ok {
		if sb, ok := b.(string); ok {
			c := strings.Compare(strings.ToLower(sa), strings.ToLower(sb))
			switch op {
			case tLt:
				return c < 0
			case tLe:
				return c <= 0
			case tGt:
				return c > 0
			case tGe:
				return c >= 0
			}
		}
	}
	if kind(a) >= 4 || kind(b) >= 4 {
		return false
	}
	na, nb := ToNumber(a), ToNumber(b)
	if math.IsNaN(na) || math.IsNaN(nb) {
		return false
	}
	switch op {
	case tLt:
		return na < nb
	case tLe:
		return na <= nb
	case tGt:
		return na > nb
	case tGe:
		return na >= nb
	}
	return false
}
