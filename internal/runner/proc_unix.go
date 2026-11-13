//go:build !windows

package runner

import (
	"os/exec"
	"syscall"
	"time"
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

// groupAlive reports whether anything is still running in process group pgid.
// A group that still has members keeps its number, so it can't be mistaken
// for someone else's later.
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || err == syscall.EPERM
}

// stopGroups stops the process groups that steps left running (programs
// started with & or nohup): SIGTERM, then SIGKILL after grace. It returns
// how many groups were still running.
func stopGroups(pgids []int, grace time.Duration) int {
	var live []int
	for _, g := range pgids {
		if groupAlive(g) {
			live = append(live, g)
			_ = syscall.Kill(-g, syscall.SIGTERM)
		}
	}
	deadline := time.Now().Add(grace)
	for _, g := range live {
		for groupAlive(g) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if groupAlive(g) {
			_ = syscall.Kill(-g, syscall.SIGKILL)
		}
	}
	return len(live)
}

// pidAlive reports whether a process with this id exists.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
