//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package database

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func lockRuntimeFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }

func runtimeVerifyOrdinaryFile(path string, expected os.FileInfo) error {
	// Lstat preserves symlink and inode checks without disturbing SQLite's
	// process-wide locks, even when Acquire is called after a pool is open.
	actual, err := os.Lstat(path)
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
		return ErrExclusiveRuntime
	}
	info, ok := actual.Sys().(*syscall.Stat_t)
	if !ok || info.Nlink != 1 {
		return ErrExclusiveRuntime
	}
	return nil
}

func runtimeSingleLink(f *os.File) error {
	var info unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &info); err != nil || info.Nlink != 1 {
		return ErrExclusiveRuntime
	}
	return nil
}
