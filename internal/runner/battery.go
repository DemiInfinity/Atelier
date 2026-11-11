package runner

import (
	"os"
	"path/filepath"
	"strings"
)

// Battery state for the pause_on_battery rule. deviceOnBattery (one per OS,
// battery_*.go) answers false when it can't tell, so a desktop or an odd
// machine keeps taking jobs.

// pmsetOnBattery reads `pmset -g batt`, whose first line is
// "Now drawing from 'Battery Power'" or "... 'AC Power'".
func pmsetOnBattery(out string) bool {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return strings.Contains(first, "'Battery Power'")
}

// sysfsOnBattery reads Linux's /sys/class/power_supply: on battery when a
// battery is discharging and no mains or USB supply is online.
func sysfsOnBattery(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	read := func(name, file string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name, file))
		return strings.ToLower(strings.TrimSpace(string(b)))
	}
	discharging := false
	for _, e := range entries {
		switch typ := read(e.Name(), "type"); {
		case typ == "battery":
			// a mouse or headset battery (scope "device") says nothing about this machine
			if read(e.Name(), "scope") != "device" && read(e.Name(), "status") == "discharging" {
				discharging = true
			}
		case typ == "mains" || strings.HasPrefix(typ, "usb"):
			if read(e.Name(), "online") == "1" {
				return false
			}
		}
	}
	return discharging
}
