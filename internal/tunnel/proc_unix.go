//go:build !windows

package tunnel

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// setGroup puts the helper in its own process group, so killGroup takes
// down anything it started as well.
func setGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup SIGKILLs the process group led by pid (and pid itself, in case
// it isn't a group leader).
func killGroup(pid int) {
	if pid <= 1 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// commandLine returns a running process's command line.
func commandLine(pid int) (string, bool) {
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
		return strings.ReplaceAll(string(b), "\x00", " "), true
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(out))
	return s, s != ""
}
