//go:build !windows && !linux

package services

import "errors"

func managedMediaMoveNoReplace(source, destination string) error {
	// No portable atomic no-replace rename is available here. Never fall back to
	// os.Rename (which may overwrite) or a copy/delete masquerading as recycle.
	return errors.New("safe local recycle is unavailable on this platform")
}

func managedMediaRecycleSupported() bool { return false }
