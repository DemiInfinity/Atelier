//go:build !unix

package backup

func freeSpace(string) (int64, uint64) { return -1, 0 }
