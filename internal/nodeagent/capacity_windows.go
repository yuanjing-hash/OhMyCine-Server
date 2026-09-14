//go:build windows

package nodeagent

import (
	"golang.org/x/sys/windows"
	"path/filepath"
)

func managedFreeBytes(path string) *int64 {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	pointer, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return nil
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &available, nil, nil); err != nil {
		return nil
	}
	if available > uint64(^uint64(0)>>1) {
		return nil
	}
	value := int64(available)
	return &value
}
