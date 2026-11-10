package expr

import (
	"strings"
	"testing"
)

func testCtx() Context {
	return Context{
		"github": map[string]any{
			"ref": "refs/heads/main", "event_name": "push", "actor": "zoe",
			"event": map[string]any{"pull_request": map[string]any{"title": "Fix ${{ secrets.TOKEN }}", "number": 7}},
		},
		"matrix": map[string]any{"os": "linux", "go": "1.22", "n": 3},
		"needs": map[string]any{
			"build": map[string]any{"result": "success", "outputs": map[string]any{"version": "1.2.3", "list": `["a","b"]`}},
			"lint":  map[string]any{"result": "failure", "outputs": map[string]any{}},
		},
		"inputs": map[string]any{"debug": true, "level": "warn"},
	}
}

func TestEval(t *testing.T) {
	e := &Evaluator{Ctx: testCtx()}
	cases := []struct {
		src  string
		want string
	}{
		{"1 == 1", "true"},
		{"'abc' == 'ABC'", "true"},
		{"github.ref == 'refs/heads/main'", "true"},
		{"github['event_name']", "push"},
		{"github.EVENT_NAME", "push"},
		{"matrix.n > 2 && matrix.n < 4", "true"},
		{"matrix.n >= 4 || 'fallback'", "fallback"},
		{"!inputs.debug", "false"},
		{"null", ""},
		{"0x10 == 16", "true"},
		{"-1.5 < 0", "true"},
		{"'1' == 1", "true"},
		{"true == 1", "true"},
		{"null == 0", "true"},
		{"'' == 0", "true"},
		{"'abc' == 0", "false"},
		{"contains('Hello world', 'WORLD')", "true"},
		{"contains(fromJSON('[\"a\",\"b\"]'), 'b')", "true"},
		{"contains(fromJSON(needs.build.outputs.list), 'c')", "false"},
		{"startsWith(github.ref, 'refs/heads/')", "true"},
		{"endsWith('file.go', '.GO')", "true"},
		{"format('{0}-{1} {{x}}', matrix.os, matrix.go)", "linux-1.22 {x}"},
		{"join(fromJSON('[1,2,3]'), '+')", "1+2+3"},
		{"join('solo')", "solo"},
		{"toJSON(matrix.n)", "3"},
		{"fromJSON('{\"a\":{\"b\":2}}').a.b", "2"},
		{"needs.*.result", "[\n  \"success\",\n  \"failure\"\n]"},
		{"contains(needs.*.result, 'failure')", "true"},
		{"needs.build.outputs.version", "1.2.3"},
		{"needs.missing.outputs.x", ""},
		{"(1 == 2) || (2 == 2)", "true"},
		{"'it''s'", "it's"},
		{"1.0", "1"},
		{"fromJSON('[10,20]')[1]", "20"},
	}
	for _, c := range cases {
		v, err := e.Eval(c.src)
		if err != nil {
			t.Errorf("Eval(%q): %v", c.src, err)
			continue
		}
		if got := ToString(v); got != c.want {
			t.Errorf("Eval(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestEvalErrors(t *testing.T) {
	e := &Evaluator{Ctx: testCtx()}
	for _, src := range []string{"", "1 ==", "foo(", "nope()", "'open", "a.", "(1", "contains(1)", "format('{2}', 1)", "fromJSON('{')", "1 = 2", "a $ b"} {
		if _, err := e.Eval(src); err == nil {
			t.Errorf("Eval(%q) should fail", src)
		}
	}
	deep := strings.Repeat("(", 500) + "1" + strings.Repeat(")", 500)
	if _, err := e.Eval(deep); err == nil {
		t.Error("deeply nested expression should fail")
	}
}

func TestStatusFunctions(t *testing.T) {
	e := &Evaluator{Ctx: testCtx(), Status: &Status{Success: false, Failure: true}}
	for src, want := range map[string]bool{
		"success()": false, "failure()": true, "always()": true, "cancelled()": false,
		"failure() && needs.lint.result == 'failure'": true,
	} {
		got, err := e.EvalBool(src)
		if err != nil || got != want {
			t.Errorf("%s = %v, %v; want %v", src, got, err, want)
		}
	}
	if !UsesStatusFunc("always() && x") || UsesStatusFunc("github.ref") {
		t.Error("UsesStatusFunc")
	}
	// Without a status they're unresolved.
	if _, err := (&Evaluator{Ctx: testCtx()}).Eval("success()"); err != ErrUnresolved {
		t.Errorf("success() without status: %v", err)
	}
}

func serverKnown(root string) bool {
	switch root {
	case "github", "matrix", "needs", "inputs", "vars", "strategy", "atelier":
		return true
	}
	return false
}

func TestInterpolatePartial(t *testing.T) {
	e := &Evaluator{Ctx: testCtx(), Known: serverKnown}
	cases := map[string]string{
		"go test ./... # ${{ matrix.os }}/${{ matrix.go }}":    "go test ./... # linux/1.22",
		"token=${{ secrets.TOKEN }}":                           "token=${{ secrets.TOKEN }}",
		"${{ steps.build.outputs.v }}-${{ matrix.os }}":        "${{ steps.build.outputs.v }}-linux",
		"${{ matrix.os == 'linux' && env.FOO }}":               "${{ env.FOO }}",
		"${{ matrix.os == 'mac' && env.FOO }}":                 "false",
		"${{ hashFiles('**/go.sum') }}":                        "${{ hashFiles('**/go.sum') }}",
		"${{ format('{0}-{1}', matrix.os, runner.os) }}":       "${{ format('{0}-{1}', 'linux', runner.os) }}",
		"${{ github.event.pull_request.title }}":               "${{ 'Fix ${{ secrets.TOKEN }}' }}",
		"no expressions":                                       "no expressions",
		"${{ toJSON(fromJSON('{\"a\":1}')) }}":                 "{\n  \"a\": 1\n}",
		"${{ contains(steps.*.outcome, 'failure') || false }}": "${{ contains(steps.*.outcome, 'failure') || false }}",
	}
	for in, want := range cases {
		got, err := e.Interpolate(in)
		if err != nil {
			t.Errorf("Interpolate(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Interpolate(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := e.Interpolate("${{ unclosed"); err == nil {
		t.Error("unclosed ${{ should fail")
	}

	out, _, known, err := e.Reduce("matrix.os == 'linux' && success()")
	if err != nil || known || out != "success()" {
		t.Errorf("Reduce = %q %v %v", out, known, err)
	}
	out, _, _, _ = e.Reduce("steps.a.outputs[matrix.os] != needs.build.outputs.version")
	if out != "steps.a.outputs['linux'] != '1.2.3'" {
		t.Errorf("Reduce index = %q", out)
	}
	roots, _ := Roots("steps.x.outputs.y == matrix.os || env.A")
	if strings.Join(roots, ",") != "steps,matrix,env" {
		t.Errorf("Roots = %v", roots)
	}
	if Strip("${{ github.ref == 'x' }}") != "github.ref == 'x'" || Strip("a == b") != "a == b" {
		t.Error("Strip")
	}
	if s, err := e.EvalString("v${{ needs.build.outputs.version }}"); err != nil || s != "v1.2.3" {
		t.Errorf("EvalString = %q %v", s, err)
	}
	if _, err := e.EvalString("${{ env.X }}"); err == nil {
		t.Error("EvalString with runtime context should fail")
	}
}
