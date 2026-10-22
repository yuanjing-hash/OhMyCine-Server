//go:build quark_live && !windows

package handlers_test

import (
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"os"
	"path/filepath"
	"testing"
)

func observationCheckPrivateDirectory(path string) error {
	for p := path; ; p = filepath.Dir(p) {
		info, e := os.Lstat(p)
		if e != nil || !info.IsDir() || storagefs.IsReparsePoint(p, info) {
			return observationError()
		}
		if p == path && info.Mode().Perm()&0077 != 0 {
			return observationError()
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func observationOfflinePrivateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("fixture chmod")
	}
	return dir
}
