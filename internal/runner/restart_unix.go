//go:build !windows

package runner

import (
	"os"
	"syscall"
)

// RestartSelf runs the (updated) executable in place of this process: same
// process id, arguments and environment, so launchd, systemd and Docker see
// the same process carry on.
func RestartSelf() error {
	exe, err := selfExecutable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
