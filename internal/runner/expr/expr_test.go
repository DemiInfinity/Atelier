package expr

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func testEnv() *Env {
	shared := map[string]any{"k": "v"}
	return &Env{
		JobStatus: "success",
		Contexts: map[string]any{
			"github": map[string]any{
				"event_name": "push",
				"ref":        "refs/heads/main",
				"Actor":      "Demi",
				"event": map[string]any{
					"commits": []any{
						map[string]any{"message": "first", "id": 1},
						map[string]any{"message": "second", "id": 2},
						map[string]any{"id": 3},
					},
				},
			},
			"env":     map[string]string{"FOO": "bar", "EMPTY": ""},
			"steps":   map[string]any{"a": map[string]any{"outcome": "failure", "outputs": map[string]any{"n": "5"}}, "my-step": map[string]any{"outputs": map[string]any{"x": "y"}}},
			"matrix":  map[string]any{"os": "linux", "n": 3, "list": []string{"a", "b"}},
			"shared":  map[string]any{"a": shared, "b": shared, "c": map[string]any{"k": "v"}},
			"nums":    []any{1.0, 2.0, 3.0},
			"labels":  []any{"Bug", "feature"},
			"objmap":  map[string]any{"x": map[string]any{"v": 1.0}, "y": map[string]any{"v": 2.0}},
			"secrets": map[string]any{"TOKEN": "s3cr3t"},
		},
	}
}

func TestEval(t *testing.T) {
	env := testEnv()
	tests := []struct {
		src  string
		want any
	}{
		// literals
		{"null", nil},
		{"true", true},
		{"false", false},
		{"42", 42.0},
		{"-7", -7.0},
		{"1.5", 1.5},
		{"0xff", 255.0},
		{"-0x10", -16.0},
		{"1e3", 1000.0},
		{"2.5E-1", 0.25},
		{"'hello'", "hello"},
		{"'it''s'", "it's"},
		{"''", ""},
		{"${{ 'wrapped' }}", "wrapped"},
		{"  ${{1}}  ", 1.0},
		// context access, case-insensitive
		{"github.event_name", "push"},
		{"GITHUB.EVENT_NAME", "push"},
		{"github.actor", "Demi"},
		{"github['ref']", "refs/heads/main"},
		{"github['REF']", "refs/heads/main"},
		{"env.FOO", "bar"},
		{"env.foo", "bar"},
		{"steps.my-step.outputs.x", "y"},
		{"steps['my-step'].outputs['x']", "y"},
		{"matrix.n", 3.0},
		{"matrix.list[1]", "b"},
		{"nums[0]", 1.0},
		{"nums['2']", 3.0},
		{"nums[3]", nil},
		{"nums[-1]", nil},
		{"nums['x']", nil},
		{"github.missing", nil},
		{"github.missing.deeper", nil},
		{"unknownctx.foo", nil},
		{"null.foo", nil},
		{"github.event.commits[1].message", "second"},
		// filters
		{"github.event.commits.*.message", []any{"first", "second"}},
		{"github.event.commits.*.id", []any{1.0, 2.0, 3.0}},
		{"github.event.commits[*].id", []any{1.0, 2.0, 3.0}},
		{"objmap.*.v", []any{1.0, 2.0}},
		{"nums.*", []any{1.0, 2.0, 3.0}},
		{"github.event_name.*", []any{}},
		{"contains(github.event.commits.*.message, 'SECOND')", true},
		{"join(github.event.commits.*.message, '|')", "first|second"},
		// operators & precedence
		{"!true", false},
		{"!!'x'", true},
		{"!0", true},
		{"1 < 2", true},
		{"2 <= 2", true},
		{"3 > 4", false},
		{"3 >= 4", false},
		{"1 == 1 && 2 == 2", true},
		{"true || false && false", true},
		{"(true || false) && false", false},
		{"!false == true", true},
		{"1 < 2 == true", true},
		{"!(1 == 2)", true},
		// short-circuit value return
		{"'a' && 'b'", "b"},
		{"'' && 'b'", ""},
		{"null || 'default'", "default"},
		{"'x' || 'y'", "x"},
		{"0 || null", nil},
		{"env.MISSING || 'fallback'", "fallback"},
		{"github.event_name == 'push' && 'yes' || 'no'", "yes"},
		{"github.event_name == 'pr' && 'yes' || 'no'", "no"},
		{"false && hashFiles('x')", false}, // RHS not evaluated
		// loose equality
		{"'ABC' == 'abc'", true},
		{"'abc' != 'abd'", true},
		{"null == 0", true},
		{"null == ''", true},
		{"null == null", true},
		{"true == 1", true},
		{"false == 0", true},
		{"true == 'true'", false}, // 1 vs NaN
		{"'1' == 1", true},
		{"' 1 ' == 1", true},
		{"'0x10' == 16", true},
		{"'' == 0", true},
		{"'abc' == 0", false},
		{"steps.a.outputs.n == 5", true},
		{"nums == nums", true},
		{"nums == 1", false},
		{"shared.a == shared.b", true},
		{"shared.a == shared.c", false},
		{"shared.a != shared.c", true},
		{"1.0 == 1", true},
		// comparisons
		{"'a' < 'B'", true},
		{"'b' > 'A'", true},
		{"'abc' <= 'ABC'", true},
		{"'10' > 9", true},
		{"'abc' < 1", false},
		{"'abc' >= 1", false},
		{"null < 1", true},
		{"true > false", true},
		{"nums < 1", false},
		// functions
		{"contains('Hello World', 'WORLD')", true},
		{"contains('abc', 'x')", false},
		{"contains(labels, 'bug')", true},
		{"contains(labels, 'nope')", false},
		{"contains(nums, '2')", true},
		{"contains(github.event_name, 'us')", true},
		{"startsWith('Hello', 'he')", true},
		{"STARTSWITH(github.ref, 'refs/heads/')", true},
		{"startsWith('Hello', 'lo')", false},
		{"endsWith('Hello', 'LO')", true},
		{"endsWith('Hello', 'he')", false},
		{"format('{0} {1}', 'a', 'b')", "a b"},
		{"format('{1}{0}{1}', 'a', 'b')", "bab"},
		{"format('{{0}} is {0}', 5)", "{0} is 5"},
		{"format('}}{{')", "}{"},
		{"format('x={0}', null)", "x="},
		{"format('{0}', true)", "true"},
		{"join(labels)", "Bug,feature"},
		{"join(labels, ', ')", "Bug, feature"},
		{"join('solo', '-')", "solo"},
		{"join(nums, '+')", "1+2+3"},
		{"join(matrix.list)", "a,b"},
		{"toJSON('x')", `"x"`},
		{"toJSON(1)", "1"},
		{"toJSON(null)", "null"},
		{"toJSON(labels)", "[\n  \"Bug\",\n  \"feature\"\n]"},
		{"toJSON(secrets)", "{\n  \"TOKEN\": \"s3cr3t\"\n}"},
		{"fromJSON('[1, 2]')", []any{1.0, 2.0}},
		{"fromJSON('{\"a\": {\"b\": true}}').a.b", true},
		{"fromJSON('3') == 3", true},
		{"fromJSON(toJSON(github.event.commits))[0].message", "first"},
		{"fromJSON(toJSON(matrix)).list[0]", "a"},
		{"fromJSON('\"a<b>\"')", "a<b>"},
		{"toJSON('a<b>&')", `"a<b>&"`},
		// status functions
		{"success()", true},
		{"failure()", false},
		{"always()", true},
		{"cancelled()", false},
		{"Success()", true},
	}
	for _, tt := range tests {
		got, err := env.Eval(tt.src)
		if err != nil {
			t.Errorf("Eval(%q) error: %v", tt.src, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Eval(%q) = %#v, want %#v", tt.src, got, tt.want)
		}
	}
}

func TestEvalErrors(t *testing.T) {
	env := testEnv()
	tests := []struct{ src, want string }{
		{"", "empty expression"},
		{"1 +", "unexpected character"},
		{"'abc", "unterminated string"},
		{"(1", `expected ")"`},
		{"a[1", `expected "]"`},
		{"a.", "expected property name"},
		{"1 2", "unexpected"},
		{"nope()", "unknown function"},
		{"contains('a')", "wrong number of arguments"},
		{"success(1)", "wrong number of arguments"},
		{"join()", "wrong number of arguments"},
		{"toJSON(1, 2)", "wrong number of arguments"},
		{"format()", "wrong number of arguments"},
		{"format('{1}', 'a')", "out of range"},
		{"format('{x}', 'a')", "invalid placeholder"},
		{"format('{0', 'a')", "unclosed"},
		{"format('a}b')", "unmatched"},
		{"fromJSON('{bad')", "invalid JSON"},
		{"fromJSON('')", "empty input"},
		{"hashFiles('**/go.sum')", "not available"},
		{"1 = 2", "unexpected character"},
		{"0xZZ", "invalid number"},
		{"1 && && 2", "unexpected"},
	}
	for _, tt := range tests {
		_, err := env.Eval(tt.src)
		if err == nil {
			t.Errorf("Eval(%q) succeeded, want error containing %q", tt.src, tt.want)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Eval(%q) error = %q, want it to contain %q", tt.src, err, tt.want)
		}
		if !strings.Contains(err.Error(), "position") && !strings.Contains(err.Error(), "expression") {
			t.Errorf("Eval(%q) error %q lacks context", tt.src, err)
		}
	}
	_, err := env.Eval("1 == (2")
	if err == nil || !strings.Contains(err.Error(), `"1 == (2"`) || !strings.Contains(err.Error(), "position 7") {
		t.Errorf("error should include expression and position: %v", err)
	}
}

func TestHashFiles(t *testing.T) {
	env := testEnv()
	var got []string
	env.HashFiles = func(p []string) (string, error) {
		got = p
		return "abc123", nil
	}
	v, err := env.Eval("hashFiles('**/go.sum', format('{0}.lock', 'x'))")
	if err != nil || v != "abc123" {
		t.Fatalf("hashFiles = %v, %v", v, err)
	}
	if !reflect.DeepEqual(got, []string{"**/go.sum", "x.lock"}) {
		t.Fatalf("patterns = %v", got)
	}
	sentinel := errors.New("boom")
	env.HashFiles = func([]string) (string, error) { return "", sentinel }
	if _, err := env.Eval("hashFiles('x')"); !errors.Is(err, sentinel) {
		t.Fatalf("want wrapped sentinel, got %v", err)
	}
}

func TestStatusFunctions(t *testing.T) {
	cases := []struct {
		status                           string
		cancelled                        bool
		success, failure, cancel, always bool
	}{
		{"success", false, true, false, false, true},
		{"failure", false, false, true, false, true},
		{"success", true, false, false, true, true},
		{"failure", true, false, true, true, true},
	}
	for _, c := range cases {
		env := &Env{JobStatus: c.status, Cancelled: c.cancelled}
		for src, want := range map[string]bool{"success()": c.success, "failure()": c.failure, "cancelled()": c.cancel, "always()": c.always} {
			v, err := env.Eval(src)
			if err != nil || v != want {
				t.Errorf("%s (status=%s cancelled=%v) = %v, %v; want %v", src, c.status, c.cancelled, v, err, want)
			}
		}
	}
}

func TestHasStatusFunc(t *testing.T) {
	tests := map[string]bool{
		"success()":                            true,
		"${{ failure() }}":                     true,
		"always() && x":                        true,
		"!cancelled()":                         true,
		"contains(toJSON(x), 'a') || ALWAYS()": true,
		"steps.a.outcome == 'failure'":         false,
		"'success()'":                          false,
		"github.success":                       false,
		"x ${{ failure() }}":                   true,
		"x ${{ y }}":                           false,
		"bad (":                                false,
	}
	for src, want := range tests {
		if got := HasStatusFunc(src); got != want {
			t.Errorf("HasStatusFunc(%q) = %v, want %v", src, got, want)
		}
	}
}

func TestEvalCondition(t *testing.T) {
	tests := []struct {
		status    string
		cancelled bool
		src       string
		want      bool
	}{
		{"success", false, "", true},
		{"failure", false, "", false},
		{"success", true, "", false},
		{"success", false, "   ", true},
		{"success", false, "true", true},
		{"success", false, "false", false},
		{"success", false, "${{ github.event_name == 'push' }}", true},
		{"success", false, "github.event_name == 'PUSH'", true},
		{"success", false, "github.event_name != 'push'", false},
		{"failure", false, "steps.a.outcome == 'failure'", false}, // implicit success()
		{"success", false, "steps.a.outcome == 'failure'", true},
		{"failure", false, "failure()", true},
		{"failure", false, "failure() && steps.a.outcome == 'failure'", true},
		{"failure", false, "${{ failure() && env.FOO }}", true},
		{"success", false, "failure()", false},
		{"failure", false, "always()", true},
		{"success", true, "always()", true},
		{"success", true, "cancelled()", true},
		{"success", true, "!cancelled()", false},
		{"failure", false, "!cancelled()", true},
		{"success", false, "env.MISSING", false},
		{"success", false, "env.FOO", true},
		{"success", false, "env.EMPTY", false},
		{"success", false, "0", false},
		{"success", false, "'0'", true},
		{"success", false, "nums", true},
		{"failure", false, "false && hashFiles('x')", false},
		// mixed literal text: non-empty string is truthy
		{"success", false, "x ${{ false }}", true},
		{"failure", false, "x ${{ false }}", false},
		{"failure", false, "x ${{ failure() }}", true},
		{"success", false, "${{ false }} ${{ false }}", true},
	}
	for _, tt := range tests {
		env := testEnv()
		env.JobStatus, env.Cancelled = tt.status, tt.cancelled
		got, err := env.EvalCondition(tt.src)
		if err != nil {
			t.Errorf("EvalCondition(%q) error: %v", tt.src, err)
			continue
		}
		if got != tt.want {
			t.Errorf("EvalCondition(%q) [status=%s cancelled=%v] = %v, want %v", tt.src, tt.status, tt.cancelled, got, tt.want)
		}
	}
	for _, src := range []string{"${{ (", "1 ==", "nope()", "x ${{ 'unterminated"} {
		if _, err := testEnv().EvalCondition(src); err == nil {
			t.Errorf("EvalCondition(%q) want error", src)
		}
	}
	// syntax errors surface even when the implicit success() is false
	env := testEnv()
	env.JobStatus = "failure"
	if _, err := env.EvalCondition("1 =="); err == nil {
		t.Error("want syntax error under failure status")
	}
}

func TestInterpolate(t *testing.T) {
	env := testEnv()
	tests := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"", ""},
		{"no } braces {{ here }}", "no } braces {{ here }}"},
		{"${{ github.event_name }}", "push"},
		{"event=${{github.event_name}} ref=${{ github.ref }}!", "event=push ref=refs/heads/main!"},
		{"${{ 'a}}b' }}", "a}}b"},
		{"${{ format('{{{0}}}', 'x') }}", "{x}"},
		{"${{ 'it''s }} fine' }} ok", "it's }} fine ok"},
		{"n=${{ matrix.n }} f=${{ 1.5 }} b=${{ true }} z=${{ null }}.", "n=3 f=1.5 b=true z=."},
		{"${{ labels }}", "[\n  \"Bug\",\n  \"feature\"\n]"},
		{"${{1}}${{2}}${{3}}", "123"},
		{"$${{ 'x' }}", "$x"},
		{"${{ env.MISSING }}", ""},
	}
	for _, tt := range tests {
		got, err := env.Interpolate(tt.in)
		if err != nil {
			t.Errorf("Interpolate(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Interpolate(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	for _, in := range []string{"a ${{ b", "${{ 'x }}", "${{ 1 + }}", "ok ${{ nope() }}"} {
		if _, err := env.Interpolate(in); err == nil {
			t.Errorf("Interpolate(%q) want error", in)
		}
	}
}

func TestToStringTruthy(t *testing.T) {
	strs := []struct {
		v    any
		want string
	}{
		{nil, ""}, {true, "true"}, {false, "false"}, {3.0, "3"}, {-0.0, "0"}, {1.5, "1.5"},
		{1e21, "1000000000000000000000"}, {0.1, "0.1"}, {math.NaN(), "NaN"}, {math.Inf(1), "Infinity"},
		{"s", "s"}, {7, "7"}, {[]any{}, "[]"}, {map[string]any{}, "{}"},
		{map[string]any{"a": 1.0}, "{\n  \"a\": 1\n}"},
	}
	for _, tt := range strs {
		if got := ToString(tt.v); got != tt.want {
			t.Errorf("ToString(%#v) = %q, want %q", tt.v, got, tt.want)
		}
	}
	truthy := []struct {
		v    any
		want bool
	}{
		{nil, false}, {false, false}, {true, true}, {0.0, false}, {math.Copysign(0, -1), false},
		{math.NaN(), false}, {2.0, true}, {"", false}, {"0", true}, {"false", true},
		{[]any{}, true}, {map[string]any{}, true}, {0, false}, {1, true},
	}
	for _, tt := range truthy {
		if got := Truthy(tt.v); got != tt.want {
			t.Errorf("Truthy(%#v) = %v, want %v", tt.v, got, tt.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	type S struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	var nilMap map[string]int
	tests := []struct {
		in, want any
	}{
		{nil, nil},
		{5, 5.0},
		{int64(-2), -2.0},
		{uint8(9), 9.0},
		{float32(1.5), 1.5},
		{"x", "x"},
		{map[string]string{"a": "b"}, map[string]any{"a": "b"}},
		{[]string{"a"}, []any{"a"}},
		{[]int{1, 2}, []any{1.0, 2.0}},
		{map[string]any{"n": 1, "l": []string{"z"}}, map[string]any{"n": 1.0, "l": []any{"z"}}},
		{S{"a", 2}, map[string]any{"name": "a", "n": 2.0}},
		{&S{"b", 3}, map[string]any{"name": "b", "n": 3.0}},
		{nilMap, nil},
	}
	for _, tt := range tests {
		if got := Normalize(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Normalize(%#v) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
	// structs in contexts are normalized lazily on access
	env := &Env{Contexts: map[string]any{"job": S{"j", 4}}}
	if v, err := env.Eval("job.NAME"); err != nil || v != "j" {
		t.Errorf("struct context access = %v, %v", v, err)
	}
}
