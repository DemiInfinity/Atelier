//go:build unix

package backup

import "syscall"

// freeSpace returns bytes available to unprivileged users and a filesystem id.
func freeSpace(path string) (int64, uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1, 0
	}
	var dev syscall.Stat_t
	var id uint64
	if syscall.Stat(path, &dev) == nil {
		id = uint64(dev.Dev) + 1 // +1 so a real device 0 isn't "unknown"
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), id
}
