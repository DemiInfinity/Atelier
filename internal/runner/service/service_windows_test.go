//go:build windows

package service

import (
	"syscall"
	"testing"
)

// TestWindowsEscapeArgMatchesSyscall cross-checks the portable
// implementation against the real syscall.EscapeArg.
func TestWindowsEscapeArgMatchesSyscall(t *testing.T) {
	for _, c := range escapeArgCases {
		if got, want := windowsEscapeArg(c.in), syscall.EscapeArg(c.in); got != want {
			t.Errorf("windowsEscapeArg(%q) = %s, syscall.EscapeArg = %s", c.in, got, want)
		}
	}
}
