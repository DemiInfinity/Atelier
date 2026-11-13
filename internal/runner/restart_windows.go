//go:build windows

package runner

import (
	"os"
	"os/exec"
)

// RestartSelf starts the (updated) executable with the same arguments and
// leaves this process to exit. A Windows service never gets here: it exits
// with an error and the service's recovery actions restart it.
func RestartSelf() error {
	exe, err := selfExecutable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Start()
}
