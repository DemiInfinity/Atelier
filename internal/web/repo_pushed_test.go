package web

import (
	"context"
	"sync"
	"testing"
)

func TestRepoPushedEvent(t *testing.T) {
	f := newActFixture(t)
	var mu sync.Mutex
	var got []Event
	registerEventHandler(func(ctx context.Context, a *App, ev Event) {
		if a == f.a && ev.Kind == EventRepoPushed {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		}
	})
	pullsCommit(t, f.work, "hello.txt", "hi\n", "Hello")
	sha := f.push(t, "main")
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("repo.pushed events = %d", len(got))
	}
	ev := got[0]
	if ev.Push == nil || ev.Push.Ref != "refs/heads/main" || ev.Push.New != sha || ev.Push.Pusher != "owner:zoe" || ev.Title != "" {
		t.Fatalf("event = %+v push = %+v", ev, ev.Push)
	}
	if ev.RepoID != f.repoID(t) || ev.Actor.Name != "zoe" {
		t.Errorf("repo/actor = %d %+v", ev.RepoID, ev.Actor)
	}
}
