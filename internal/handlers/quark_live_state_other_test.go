//go:build quark_live && !windows

package handlers_test

import "os"

func liveTVCopySecurity(source, temporary string) error {
	info, err := os.Stat(source)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return liveTVError("tv_state_save_failed")
	}
	return os.Chmod(temporary, info.Mode().Perm())
}
