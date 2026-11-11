package main

import (
	"context"
	"strings"
	"testing"
)

func TestParseRefUpdates(t *testing.T) {
	z := strings.Repeat("0", 40)
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	ups, err := parseRefUpdates(strings.NewReader(z + " " + a + " refs/heads/new\n" + a + " " + b + " refs/heads/main\n\n" + b + " " + z + " refs/heads/old\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 3 || !ups[0].isCreate() || ups[0].isDelete() || ups[1].isCreate() || ups[1].isDelete() || !ups[2].isDelete() {
		t.Fatalf("updates = %+v", ups)
	}
	if ups[1].Ref != "refs/heads/main" || ups[1].Old != a || ups[1].New != b {
		t.Errorf("update = %+v", ups[1])
	}
	if _, err := parseRefUpdates(strings.NewReader("garbage\n")); err == nil {
		t.Error("garbage input accepted")
	}
	if err := runHook([]string{"pre-receive"}); err == nil {
		t.Error("missing args accepted")
	}
	if err := runHook([]string{"update", "a", "b"}); err == nil {
		t.Error("unknown hook accepted")
	}
}

// Work deferred with onAccept runs only when every check passes (#12: a
// one-time secret-scanning bypass must not be used up by a push that branch
// rules then reject).
func TestPreReceiveAcceptStepsRunOnlyWhenAccepted(t *testing.T) {
	ran := 0
	deferring := func(ctx context.Context, pc pushContext) ([]string, error) {
		pc.onAccept(func(context.Context) (string, error) { ran++; return "", nil })
		return nil, nil
	}
	rejecting := func(context.Context, pushContext) ([]string, error) { return []string{"branch rule says no"}, nil }
	passing := func(context.Context, pushContext) ([]string, error) { return nil, nil }

	// The rejecting check runs before and after the deferring one.
	for _, checks := range [][]preReceiveCheck{{rejecting, deferring}, {deferring, rejecting}} {
		msgs, err := runPreReceiveChecks(t.Context(), pushContext{}, checks)
		if err != nil || len(msgs) != 1 || ran != 0 {
			t.Fatalf("rejected push: msgs=%v err=%v, deferred steps ran %d times", msgs, err, ran)
		}
	}
	msgs, err := runPreReceiveChecks(t.Context(), pushContext{}, []preReceiveCheck{deferring, passing})
	if err != nil || len(msgs) != 0 || ran != 1 {
		t.Fatalf("accepted push: msgs=%v err=%v, deferred steps ran %d times", msgs, err, ran)
	}
	// A step can still refuse the push at the accept point.
	late := func(ctx context.Context, pc pushContext) ([]string, error) {
		pc.onAccept(func(context.Context) (string, error) { return "bypass already used", nil })
		return nil, nil
	}
	if msgs, err := runPreReceiveChecks(t.Context(), pushContext{}, []preReceiveCheck{late}); err != nil || len(msgs) != 1 {
		t.Fatalf("late refusal: msgs=%v err=%v", msgs, err)
	}
}
