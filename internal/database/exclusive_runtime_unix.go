//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package database

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockRuntimeFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }

func runtimeSingleLink(f *os.File) error {
	var info unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &info); err != nil || info.Nlink != 1 {
		return ErrExclusiveRuntime
	}
	return nil
}
