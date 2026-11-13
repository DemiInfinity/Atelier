package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"atelier/internal/actions"
)

// fakeBuild is a "new atelier-runner": a script that answers `version`.
func fakeBuild(version string) []byte {
	return []byte("#!/bin/sh\necho \"atelier-runner " + version + " (test/test, protocol 1)\"\n")
}

func offerFor(version string, body []byte) *actions.UpdateOffer {
	sum := sha256.Sum256(body)
	return &actions.UpdateOffer{Version: version, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}
}

// useFakeExecutable points selfExecutable at a file standing in for the
// running binary.
func useFakeExecutable(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "atelier-runner")
	if err := os.WriteFile(exe, []byte("old build"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := selfExecutable
	selfExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { selfExecutable = old })
	return exe
}

// An offered update waits for the running job, then replaces the
// executable, and Run asks to be restarted.
func TestSelfUpdateAfterJob(t *testing.T) {
	skipNoUnixShell(t)
	exe := useFakeExecutable(t)
	f := newFakeServer(t)
	body := fakeBuild("v2")
	f.mu.Lock()
	f.update, f.updateBody, f.updateAfter = offerFor("v2", body), body, 1
	f.mu.Unlock()
	job := baseJob(51, nil)
	job.Steps = []actions.Step{{Index: 0, Run: `sleep 1; echo job-done`}}
	f.enqueue(job)
	r := f.testRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	err := r.Run(ctx)
	if !errors.Is(err, ErrRestart) {
		t.Fatalf("Run = %v, want ErrRestart", err)
	}
	if st := f.finish(51).Status; st != actions.StatusSuccess {
		t.Errorf("job = %s: the update didn't wait for it", st)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(body) {
		t.Errorf("executable not replaced: %q", b)
	}
	// While the update waited, the runner asked for no new job.
	f.mu.Lock()
	defer f.mu.Unlock()
	last := f.polls[len(f.polls)-1]
	if !last.Busy {
		t.Errorf("last poll before updating wasn't busy: %+v", last)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".atelier-runner-update-*"))
	if len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// A build that fails a check is refused, reported, and the runner carries on.
func TestSelfUpdateRefusesBadBuild(t *testing.T) {
	skipNoUnixShell(t)
	cases := map[string]struct {
		body  []byte
		offer *actions.UpdateOffer
	}{
		"checksum": {fakeBuild("v2"), func() *actions.UpdateOffer {
			o := offerFor("v2", fakeBuild("v2"))
			o.SHA256 = "00" + o.SHA256[2:]
			return o
		}()},
		"wrong version": {fakeBuild("v3"), offerFor("v2", fakeBuild("v3"))},
		"won't start":   {[]byte("#!/bin/sh\nexit 3\n"), offerFor("v2", []byte("#!/bin/sh\nexit 3\n"))},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			exe := useFakeExecutable(t)
			f := newFakeServer(t)
			f.mu.Lock()
			f.update, f.updateBody = c.offer, c.body
			f.mu.Unlock()
			r := f.testRunner(t)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				f.mu.Lock()
				reported := len(f.polls) > 0 && f.polls[len(f.polls)-1].UpdateFailed == "v2"
				f.mu.Unlock()
				if reported || time.Now().After(deadline) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run = %v", err)
			}
			f.mu.Lock()
			last := f.polls[len(f.polls)-1]
			f.mu.Unlock()
			if last.UpdateFailed != "v2" || last.Busy {
				t.Errorf("after a failed update the runner polls %+v", last)
			}
			if b, _ := os.ReadFile(exe); string(b) != "old build" {
				t.Errorf("executable changed: %q", b)
			}
		})
	}
}

func TestSwapExecutable(t *testing.T) {
	dir := t.TempDir()
	exe, next := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(exe, []byte("old"), 0o755)
	os.WriteFile(next, []byte("new"), 0o755)
	if err := swapExecutable(exe, next); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Errorf("exe = %q", b)
	}
}
