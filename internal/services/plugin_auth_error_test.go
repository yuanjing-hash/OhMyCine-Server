package services

import (
	"fmt"
	"strings"
	"testing"
)

func TestPluginAuthErrorMapsStableCodesWithoutTrustingPluginText(t *testing.T) {
	for _, selection := range []struct {
		name string
		raw  string
		code string
	}{
		{"denied", `{"pluginError":{"code":"permission-denied","message":"fixture-secret https://provider.invalid/private fdfe:dcba:9876::8d"}}`, CodePermissionDenied},
		{"upstream", `{"pluginError":{"code":"upstream-unavailable","message":"fixture-secret"}}`, CodePluginOnlineLibraryUnavailable},
		{"malformed", `{"qrCodeUrl":"fixture-secret"}`, CodePluginResponseInvalid},
	} {
		t.Run(selection.name, func(t *testing.T) {
			err := pluginAuthResponseError([]byte(selection.raw), nil)
			if ErrorCode(err) != selection.code {
				t.Fatalf("unexpected classification: %s", ErrorCode(err))
			}
			message := fmt.Sprint(err)
			if strings.Contains(message, "fixture-secret") || strings.Contains(message, "provider.invalid") || strings.Contains(message, "fdfe:") {
				t.Fatal("plugin diagnostic escaped the Server-owned error boundary")
			}
			if selection.name == "denied" && (!strings.Contains(message, "权限") || !strings.Contains(message, "DNS") || !strings.Contains(message, "fake-IP")) {
				t.Fatal("network/permission denial lost its safe troubleshooting hint")
			}
		})
	}
}
