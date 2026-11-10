//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
)

// setProcGroup runs the command in its own process group so cancelling a step
// also stops everything it started.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		go func(pid int) {
			// escalate if the group ignores SIGTERM
			<-afterGrace()
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}(cmd.Process.Pid)
		return nil
	}
}

// setCmdLine is only needed on Windows (cmd.exe quoting).
func setCmdLine(cmd *exec.Cmd, line string) {}
