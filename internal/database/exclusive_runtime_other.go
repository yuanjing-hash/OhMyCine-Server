//go:build !windows && !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package database

import "os"

func lockRuntimeFile(*os.File) error   { return ErrExclusiveRuntime }
func runtimeSingleLink(*os.File) error { return ErrExclusiveRuntime }
