//go:build linux

package services

import "golang.org/x/sys/unix"

func managedMediaMoveNoReplace(source, destination string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE)
}

func managedMediaRecycleSupported() bool { return true }
