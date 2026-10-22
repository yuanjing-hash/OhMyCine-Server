package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
)

func TestQuarkProofCleanupChecksAllWritePermissionsBeforeAccess(t *testing.T) {
	permissions := []string{authz.PermissionConnectionsRead, authz.PermissionConnectionsUpdate, authz.PermissionStoragesBrowse, authz.PermissionStoragesCreate}
	for _, missing := range permissions {
		t.Run(missing, func(t *testing.T) {
			actor := Actor{Permissions: map[string]struct{}{}}
			for _, permission := range permissions {
				if permission != missing {
					actor.Permissions[permission] = struct{}{}
				}
			}
			// No database: an unauthorized call must stop before any connection,
			// credential, receipt or provider access.
			_, err := (&ConnectionService{}).RetryQuarkProofCleanup(context.Background(), actor, 1, RequestContext{})
			if ErrorCode(err) != CodePermissionDenied {
				t.Fatalf("missing %s was accepted: %v", missing, err)
			}
		})
	}
}
