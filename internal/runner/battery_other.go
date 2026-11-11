//go:build !darwin && !linux && !windows

package runner

func deviceOnBattery() bool { return false }
