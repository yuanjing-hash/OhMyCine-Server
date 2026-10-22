//go:build quark_live && windows

package handlers_test

import "golang.org/x/sys/windows"

func liveTVCopySecurity(source, temporary string) error {
	sd, err := windows.GetNamedSecurityInfo(source, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return liveTVError("tv_state_save_failed")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return liveTVError("tv_state_save_failed")
	}
	control, _, err := sd.Control()
	if err != nil {
		return liveTVError("tv_state_save_failed")
	}
	flags := windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	if control&windows.SE_DACL_PROTECTED != 0 {
		flags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	return windows.SetNamedSecurityInfo(temporary, windows.SE_FILE_OBJECT, windows.SECURITY_INFORMATION(flags), nil, nil, dacl, nil)
}
