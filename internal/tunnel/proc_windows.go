package tunnel

import (
	"os"
	"os/exec"
)

// Atelier's server runs on Linux (Docker) and macOS; on Windows the helper
// is killed on its own, without a process group.
func setGroup(cmd *exec.Cmd) {}

func killGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func commandLine(pid int) (string, bool) { return "", false }
