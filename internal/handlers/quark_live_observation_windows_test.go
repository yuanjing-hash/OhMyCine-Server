//go:build quark_live && windows

package handlers_test

import (
	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func observationCheckPrivateDirectory(path string) error {
	for p := path; ; p = filepath.Dir(p) {
		info, e := os.Lstat(p)
		if e != nil || !info.IsDir() || storagefs.IsReparsePoint(p, info) {
			return observationError()
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	sd, e := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if e != nil {
		return observationError()
	}
	acl, _, e := sd.DACL()
	if e != nil || acl == nil {
		return observationError()
	}
	user, e := windows.GetCurrentProcessToken().GetTokenUser()
	if e != nil {
		return observationError()
	}
	// Only current user may be granted access. Do not reset an existing ACL.
	for i := uint32(0); ; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ace) != nil {
			if i == 0 {
				return observationError()
			}
			break
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return observationError()
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(user.User.Sid) {
			return observationError()
		}
	}
	return nil
}

func observationOfflinePrivateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil) != nil {
		t.Fatal("fixture ACL")
	}
	return dir
}
