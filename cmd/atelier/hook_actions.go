package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"atelier/internal/config"
	"atelier/internal/store"
)

// `atelier hook post-receive <owner> <repo>` runs after git has updated refs.
// It records each update in the actions_push_queue table, which the server's
// Actions loop drains within about a second to start workflow runs. It never
// fails the push: the refs are already updated, so problems are only reported.
func postReceive(owner, name, pusher string, stdin io.Reader, stderr io.Writer) error {
	ups, err := parseRefUpdates(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "atelier: couldn't read the pushed refs for Actions:", err)
		return nil
	}
	if len(ups) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := store.Open(config.FromEnv().DBPath())
	if err != nil {
		fmt.Fprintln(stderr, "atelier: Actions won't see this push:", err)
		return nil
	}
	defer db.Close()
	if len(pusher) > 200 {
		pusher = pusher[:200]
	}
	for _, u := range ups {
		if err := db.QueueActionsPush(ctx, store.ActionsPush{Owner: owner, Repo: name, Old: u.Old, New: u.New, Ref: u.Ref, Pusher: pusher}); err != nil {
			fmt.Fprintln(stderr, "atelier: Actions won't see this push:", err)
			return nil
		}
	}
	return nil
}
