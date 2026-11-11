package runner

func deviceOnBattery() bool { return sysfsOnBattery("/sys/class/power_supply") }
