//go:build windows

package services

import "golang.org/x/sys/windows"

func managedMediaMoveNoReplace(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	// Deliberately omit MOVEFILE_REPLACE_EXISTING and COPY_ALLOWED. Trash is
	// reversible on the same volume and an occupied destination is never replaced.
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}

func managedMediaRecycleSupported() bool { return true }
