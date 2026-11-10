package main

import (
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
	if err := runHook([]string{"post-receive", "a", "b"}); err == nil {
		t.Error("unknown hook accepted")
	}
}
