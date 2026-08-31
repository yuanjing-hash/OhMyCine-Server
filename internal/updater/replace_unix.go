//go:build !windows

package updater

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
