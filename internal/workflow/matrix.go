package workflow

import (
	"encoding/json"
	"fmt"
	"strings"

	"atelier/internal/workflow/expr"
)

// Combo is one matrix combination, keys in matrix order.
type Combo struct {
	Keys   []string
	Values map[string]any
}

// Label is the combination's values joined for job names: "linux, 1.22".
func (c Combo) Label() string {
	parts := make([]string, 0, len(c.Keys))
	for _, k := range c.Keys {
		v := c.Values[k]
		switch v.(type) {
		case map[string]any, []any:
			b, _ := json.Marshal(v)
			parts = append(parts, string(b))
		default:
			parts = append(parts, expr.ToString(v))
		}
	}
	return strings.Join(parts, ", ")
}

// Map returns the values as a plain map (for the matrix context).
func (c Combo) Map() map[string]any {
	out := make(map[string]any, len(c.Values))
	for k, v := range c.Values {
		out[k] = v
	}
	return out
}

// Expand returns the matrix's combinations. e resolves expressions in a
// dynamic matrix (it may be nil for a static one).
func (m *Matrix) Expand(e *expr.Evaluator) ([]Combo, error) {
	if m == nil {
		return nil, nil
	}
	mm := *m
	if m.Dynamic() {
		if e == nil {
			return nil, fmt.Errorf("the matrix uses expressions that can't be resolved here")
		}
		r, err := m.resolve(e)
		if err != nil {
			return nil, err
		}
		mm = *r
	}
	return mm.expandStatic()
}

func evalValue(e *expr.Evaluator, s string) (any, error) {
	inner := expr.Strip(s)
	if inner != strings.TrimSpace(s) { // exactly one ${{ }}: keep the value's type
		v, err := e.Eval(inner)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	return e.EvalString(s)
}

func (m *Matrix) resolve(e *expr.Evaluator) (*Matrix, error) {
	if m.Expr != "" {
		v, err := evalValue(e, m.Expr)
		if err != nil {
			return nil, fmt.Errorf("matrix: %w", err)
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("matrix: the expression must give an object, got %s", expr.ToString(v))
		}
		return matrixFromObject(obj)
	}
	out := &Matrix{}
	for _, ax := range m.Axes {
		nx := Axis{Name: ax.Name}
		if ax.Expr != "" {
			v, err := evalValue(e, ax.Expr)
			if err != nil {
				return nil, fmt.Errorf("matrix.%s: %w", ax.Name, err)
			}
			arr, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("matrix.%s: the expression must give a list", ax.Name)
			}
			nx.Values = arr
		} else {
			for _, val := range ax.Values {
				rv, err := resolveAny(e, val)
				if err != nil {
					return nil, fmt.Errorf("matrix.%s: %w", ax.Name, err)
				}
				nx.Values = append(nx.Values, rv)
			}
		}
		out.Axes = append(out.Axes, nx)
	}
	var err error
	if out.Include, err = resolveList(e, m.Include, m.IncludeExpr, "include"); err != nil {
		return nil, err
	}
	if out.Exclude, err = resolveList(e, m.Exclude, m.ExcludeExpr, "exclude"); err != nil {
		return nil, err
	}
	return out, nil
}

func resolveAny(e *expr.Evaluator, v any) (any, error) {
	s, ok := v.(string)
	if !ok || !expr.HasExpr(s) {
		return v, nil
	}
	return evalValue(e, s)
}

func resolveList(e *expr.Evaluator, list []map[string]any, exprStr, what string) ([]map[string]any, error) {
	if exprStr != "" {
		v, err := evalValue(e, exprStr)
		if err != nil {
			return nil, fmt.Errorf("matrix.%s: %w", what, err)
		}
		arr, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("matrix.%s: the expression must give a list", what)
		}
		var out []map[string]any
		for _, x := range arr {
			obj, ok := x.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("matrix.%s: every entry must be an object", what)
			}
			out = append(out, obj)
		}
		return out, nil
	}
	var out []map[string]any
	for _, m := range list {
		nm := map[string]any{}
		for k, v := range m {
			rv, err := resolveAny(e, v)
			if err != nil {
				return nil, fmt.Errorf("matrix.%s.%s: %w", what, k, err)
			}
			nm[k] = rv
		}
		out = append(out, nm)
	}
	return out, nil
}

func matrixFromObject(obj map[string]any) (*Matrix, error) {
	m := &Matrix{}
	keys := sortedKeys(obj)
	for _, k := range keys {
		v := obj[k]
		switch k {
		case "include", "exclude":
			arr, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("matrix.%s must be a list", k)
			}
			var list []map[string]any
			for _, x := range arr {
				o, ok := x.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("matrix.%s entries must be objects", k)
				}
				list = append(list, o)
			}
			if k == "include" {
				m.Include = list
			} else {
				m.Exclude = list
			}
		default:
			arr, ok := v.([]any)
			if !ok {
				return nil, fmt.Errorf("matrix.%s must be a list", k)
			}
			m.Axes = append(m.Axes, Axis{Name: k, Values: arr})
		}
	}
	return m, nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ { // tiny insertion sort keeps imports small
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func valEqual(a, b any) bool {
	ja, err1 := json.Marshal(normalizeYAML(a))
	jb, err2 := json.Marshal(normalizeYAML(b))
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}

// normalizeYAML turns yaml-decoded values (ints, map[string]interface{}) into JSON-ish ones.
func normalizeYAML(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = normalizeYAML(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = normalizeYAML(v)
		}
		return out
	}
	return v
}

func (m Matrix) expandStatic() ([]Combo, error) {
	var combos []Combo
	if len(m.Axes) > 0 {
		combos = []Combo{{Values: map[string]any{}}}
		for _, ax := range m.Axes {
			var next []Combo
			for _, c := range combos {
				for _, v := range ax.Values {
					nc := Combo{Keys: append(append([]string{}, c.Keys...), ax.Name), Values: c.Map()}
					nc.Values[ax.Name] = normalizeYAML(v)
					next = append(next, nc)
					if len(next) > MaxMatrix*4 {
						return nil, fmt.Errorf("the matrix has more than %d combinations", MaxMatrix)
					}
				}
			}
			combos = next
		}
	}
	// Exclude: drop combinations matching every key of an exclude entry.
	if len(m.Exclude) > 0 {
		var kept []Combo
		for _, c := range combos {
			drop := false
			for _, ex := range m.Exclude {
				all := len(ex) > 0
				for k, v := range ex {
					if cv, ok := c.Values[k]; !ok || !valEqual(cv, v) {
						all = false
						break
					}
				}
				if all {
					drop = true
					break
				}
			}
			if !drop {
				kept = append(kept, c)
			}
		}
		combos = kept
	}
	// Include: extend matching combinations without overwriting original
	// values, or add a new combination when none match.
	orig := map[string]bool{}
	for _, ax := range m.Axes {
		orig[ax.Name] = true
	}
	for _, inc := range m.Include {
		keys := sortedKeys(inc)
		matched := false
		for i := range combos {
			c := &combos[i]
			ok := true
			for _, k := range keys {
				if orig[k] && !valEqual(c.Values[k], inc[k]) {
					ok = false
					break
				}
			}
			if !ok || len(m.Axes) == 0 {
				continue
			}
			matched = true
			for _, k := range keys {
				if orig[k] {
					continue
				}
				if _, had := c.Values[k]; !had {
					c.Keys = append(c.Keys, k)
				}
				c.Values[k] = normalizeYAML(inc[k])
			}
		}
		if !matched {
			nc := Combo{Values: map[string]any{}}
			for _, ax := range m.Axes { // matrix keys first, in matrix order
				if v, ok := inc[ax.Name]; ok {
					nc.Keys = append(nc.Keys, ax.Name)
					nc.Values[ax.Name] = normalizeYAML(v)
				}
			}
			for _, k := range keys {
				if !orig[k] {
					nc.Keys = append(nc.Keys, k)
					nc.Values[k] = normalizeYAML(inc[k])
				}
			}
			combos = append(combos, nc)
		}
	}
	if len(combos) > MaxMatrix {
		return nil, fmt.Errorf("the matrix has %d combinations; the limit is %d", len(combos), MaxMatrix)
	}
	if len(combos) == 0 {
		return nil, fmt.Errorf("the matrix has no combinations left")
	}
	return combos, nil
}
