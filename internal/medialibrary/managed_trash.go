package medialibrary

import "strings"

// ManagedTrashDirectory is reserved for reversible, receipt-owned local trash.
// It must never become a media source, sidecar source or auto-intake candidate.
const ManagedTrashDirectory = ".omc-managed-trash"

func IsManagedTrashPath(relative string) bool {
	for _, segment := range strings.Split(strings.ReplaceAll(relative, "\\", "/"), "/") {
		if strings.EqualFold(segment, ManagedTrashDirectory) {
			return true
		}
	}
	return false
}
