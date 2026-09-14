//go:build !windows

package nodeagent

import "golang.org/x/sys/unix"

func managedFreeBytes(path string) *int64 {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return nil
	}
	available := uint64(stat.Bavail) * uint64(stat.Bsize)
	if available > uint64(^uint64(0)>>1) {
		return nil
	}
	value := int64(available)
	return &value
}
