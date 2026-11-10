package gitrepo

// Server-side hooks. Every repository gets a pre-receive hook that runs
// `atelier hook pre-receive <owner> <repo>`, which enforces branch rules on
// pushes over HTTPS (git http-backend) and SSH (git-receive-pack) alike.
// The hook inherits git's environment, so objects still in quarantine
// (GIT_QUARANTINE_PATH / GIT_OBJECT_DIRECTORY) are visible to the checks.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// HookExecutable is the atelier binary the hook runs. Empty means the running
// executable. Inside `go test` binaries hooks are only installed when this is
// set, so a test binary is never run as a hook.
var HookExecutable string

const hookMarker = "# Managed by Atelier"

func init() {
	OnCreate(func(_ context.Context, s *Store, owner, name string) error {
		return s.InstallPreReceiveHook(owner, name)
	})
}

func hookExecutable() (string, error) {
	if HookExecutable != "" {
		return HookExecutable, nil
	}
	if testing.Testing() {
		return "", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// PreReceiveScript is the hook script for one repository.
func (s *Store) PreReceiveScript(exe, owner, name string) string {
	// Data dir: the repos root lives at <ATELIER_DATA>/repos (config.RepoRoot).
	data := filepath.Dir(s.Root)
	return "#!/bin/sh\n" + hookMarker + ". Rewritten at every start; edits are lost.\n" +
		"ATELIER_DATA=" + shellQuote(data) + "\nexport ATELIER_DATA\n" +
		"exec " + shellQuote(exe) + " hook pre-receive " + shellQuote(owner) + " " + shellQuote(name) + "\n"
}

// InstallPreReceiveHook writes (or refreshes) the repository's pre-receive
// hook. A hook not written by Atelier is left alone.
func (s *Store) InstallPreReceiveHook(owner, name string) error {
	exe, err := hookExecutable()
	if err != nil {
		log.Printf("hooks: can't find the atelier executable, branch rules won't be enforced on push: %v", err)
		return nil
	}
	if exe == "" {
		return nil
	}
	dir := filepath.Join(s.Path(owner, name), "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, "pre-receive")
	want := []byte(s.PreReceiveScript(exe, owner, name))
	if cur, err := os.ReadFile(p); err == nil {
		if bytes.Equal(cur, want) {
			return nil
		}
		if !bytes.Contains(cur, []byte(hookMarker)) {
			log.Printf("hooks: %s/%s has its own pre-receive hook; leaving it (branch rules not enforced on push)", owner, name)
			return nil
		}
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, want, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("install hook: %w", err)
	}
	return nil
}
