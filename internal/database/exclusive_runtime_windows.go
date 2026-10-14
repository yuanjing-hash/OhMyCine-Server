//go:build windows

package database

import (
	"os"

	"golang.org/x/sys/windows"
)

func runtimeVerifyOrdinaryFile(path string, expected os.FileInfo) error {
	f, err := os.Open(path)
	if err != nil {
		return ErrExclusiveRuntime
	}
	defer func() { _ = f.Close() }()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(expected, actual) {
		return ErrExclusiveRuntime
	}
	return runtimeSingleLink(f)
}

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
