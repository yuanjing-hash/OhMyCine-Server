//go:build windows

package database

import (
	"golang.org/x/sys/windows"
	"os"
)

func lockRuntimeFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

func runtimeSingleLink(f *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil || info.NumberOfLinks != 1 {
		return ErrExclusiveRuntime
	}
	return nil
}
