package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LockName is the file a running server keeps fresh in its data directory so
// a full restore can tell the data is in use. It works across containers
// because it relies on the file's age, not on process ids.
const LockName = "atelier.lock"

const (
	lockHeartbeat = 30 * time.Second
	lockStale     = 3 * time.Minute
)

// HoldLock writes the lock file and refreshes it until ctx is done, then
// removes it. It returns once the file is written.
func HoldLock(ctx context.Context, dataDir string) error {
	p := filepath.Join(dataDir, LockName)
	host, _ := os.Hostname()
	body := fmt.Sprintf("pid %d on %s since %s\n", os.Getpid(), host, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(lockHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				os.Remove(p)
				return
			case now := <-t.C:
				_ = os.Chtimes(p, now, now)
			}
		}
	}()
	return nil
}

// LockHeld reports whether a server appears to be running against dataDir,
// with the lock's contents for the message.
func LockHeld(dataDir string) (bool, string) {
	p := filepath.Join(dataDir, LockName)
	st, err := os.Stat(p)
	if err != nil {
		return false, ""
	}
	if time.Since(st.ModTime()) > lockStale {
		return false, ""
	}
	b, _ := os.ReadFile(p)
	return true, string(b)
}
