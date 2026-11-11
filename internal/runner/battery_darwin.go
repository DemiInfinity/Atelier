package runner

import (
	"context"
	"os/exec"
	"time"
)

func deviceOnBattery() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// the full path: services start with a minimal PATH
	out, err := exec.CommandContext(ctx, "/usr/bin/pmset", "-g", "batt").Output()
	if err != nil {
		return false
	}
	return pmsetOnBattery(string(out))
}
