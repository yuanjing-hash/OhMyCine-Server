//go:build quark_live && windows

package handlers_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func liveTVTestSecurity(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal("fixture security descriptor is unavailable")
	}
	return sd.String()
}

func TestQuarkLiveTVWindowsPreservesDACLAndRecoversCleanupFailure(t *testing.T) {
	f := newLiveTVFixture(t)
	s := f.open(t)
	mainPath := filepath.Join(f.dir, liveTVSessionName)
	keyPath := filepath.Join(f.dir, "tv-credentials.key")
	// Give the existing state an explicitly protected DACL. Atomic replacement
	// must preserve that file ACL rather than merely inherit directory defaults.
	sd, err := windows.GetNamedSecurityInfo(mainPath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(mainPath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	mainACL, dirACL, keyACL := liveTVTestSecurity(t, mainPath), liveTVTestSecurity(t, f.dir), liveTVTestSecurity(t, keyPath)
	keyBefore, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	var held windows.Handle
	t.Cleanup(func() {
		if held != 0 {
			_ = windows.CloseHandle(held)
		}
	})
	s.write = func(name string, raw []byte) error {
		if err := s.atomicWrite(name, raw); err != nil {
			return err
		}
		if name == liveTVCheckpointName {
			if liveTVTestSecurity(t, filepath.Join(f.dir, name)) != mainACL {
				t.Error("new encrypted checkpoint lost the original state DACL")
			}
			path, err := windows.UTF16PtrFromString(filepath.Join(f.dir, name))
			if err != nil {
				return err
			}
			// A real Windows open handle permits reads but not delete/rename:
			// promotion can commit, while subsequent checkpoint cleanup fails.
			held, err = windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
			return err
		}
		return nil
	}
	if _, _, err := readyLiveTV(s); err == nil || s.session.Generation != 8 || s.session.RefreshToken != "fixture-new-refresh" || f.brokerCalls.Load() != 1 {
		t.Fatalf("did not retain committed replacement after actual cleanup failure: %v", err)
	}
	if liveTVTestSecurity(t, mainPath) != mainACL || liveTVTestSecurity(t, f.dir) != dirACL || liveTVTestSecurity(t, keyPath) != keyACL {
		t.Fatal("renewal changed original state, key or directory ACL")
	}
	keyAfter, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("renewal changed the original encryption key")
	}
	if err := windows.CloseHandle(held); err != nil {
		t.Fatal(err)
	}
	held = 0
	_ = s.lock.Close()
	reloaded := f.open(t)
	if c, renewed, err := readyLiveTV(reloaded); err != nil || renewed || c.TV == nil || c.TV.AccessToken != "fixture-new-access" || c.TV.RefreshToken != "" || f.brokerCalls.Load() != 1 {
		t.Fatalf("cleanup recovery repeated renewal or lost the committed pair: %v", err)
	}
	for _, name := range []string{liveTVCheckpointName, liveTVAttemptName} {
		if _, err := os.Stat(filepath.Join(f.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("committed recovery file was not retired on restart")
		}
	}
}
