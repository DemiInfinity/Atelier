package backup

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Usage is disk usage by type, for the Storage section.
type Usage struct {
	Repos       int64
	Database    int64
	Backups     int64
	FreeData    int64 // free space where the data lives; -1 if unknown
	FreeBackups int64 // free space where the backups live; -1 if unknown
	SameDisk    bool  // backups share a filesystem with the data
}

// DiskUsage measures the repositories, database (with WAL) and backups.
func DiskUsage(repoRoot, dbPath, backupDir string) Usage {
	u := Usage{
		Repos:   dirSize(repoRoot),
		Backups: dirSize(backupDir),
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if st, err := os.Stat(p); err == nil {
			u.Database += st.Size()
		}
	}
	var dataDev, backupDev uint64
	u.FreeData, dataDev = freeSpace(filepath.Dir(dbPath))
	u.FreeBackups, backupDev = freeSpace(existingParent(backupDir))
	u.SameDisk = dataDev != 0 && dataDev == backupDev
	return u
}

func existingParent(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
