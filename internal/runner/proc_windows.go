//go:build windows

package runner

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// setProcGroup kills the whole process tree on cancel (taskkill /T).
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		return cmd.Process.Kill()
	}
}

// setCmdLine passes a raw command line (needed for cmd.exe's own quoting rules).
func setCmdLine(cmd *exec.Cmd, line string) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CmdLine = line
}

// Windows has no process groups to check; cancelling a step kills its whole
// tree (setProcGroup), but programs a step leaves running are not tracked.
func groupAlive(pgid int) bool { return false }

func stopGroups(pgids []int, grace time.Duration) int { return 0 }

// pidAlive reports whether a process with this id exists.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid) // opens the process on Windows
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
