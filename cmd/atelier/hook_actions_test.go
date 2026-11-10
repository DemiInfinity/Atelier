package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"atelier/internal/store"
)

func TestPostReceiveQueuesPushes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ATELIER_DATA", dir)
	old, nw := strings.Repeat("a", 40), strings.Repeat("b", 40)
	in := old + " " + nw + " refs/heads/main\n" + strings.Repeat("0", 40) + " " + nw + " refs/tags/v1\n"
	var stderr bytes.Buffer
	if err := runHookWith([]string{"post-receive", "zoe", "demo"}, "owner:zoe", strings.NewReader(in), &stderr); err != nil {
		t.Fatalf("post-receive: %v (%s)", err, stderr.String())
	}
	db, err := store.Open(filepath.Join(dir, "atelier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := db.TakeActionsPushes(context.Background(), 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("queued = %+v, %v", got, err)
	}
	if got[0].Owner != "zoe" || got[0].Ref != "refs/heads/main" || got[0].Old != old || got[0].Pusher != "owner:zoe" || got[1].Ref != "refs/tags/v1" {
		t.Errorf("queued = %+v", got)
	}
	if again, _ := db.TakeActionsPushes(context.Background(), 10); len(again) != 0 {
		t.Errorf("pushes should be taken once, got %d", len(again))
	}
	// Bad input is reported but never fails the push.
	if err := runHookWith([]string{"post-receive", "zoe", "demo"}, "", strings.NewReader("garbage\n"), &stderr); err != nil {
		t.Errorf("bad input failed the hook: %v", err)
	}
}

func runHookWith(args []string, pusher string, stdin *strings.Reader, stderr *bytes.Buffer) error {
	return postReceive(args[1], args[2], pusher, stdin, stderr)
}
