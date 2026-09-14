package nodeprotocol

import "testing"

func TestFileExportPagingAndOpaqueTokenValidation(t *testing.T) {
	page, pageSize, err := NormalizeManifestPage(0, 0)
	if err != nil || page != 1 || pageSize != DefaultManifestPageSize {
		t.Fatalf("default manifest page = %d/%d err=%v", page, pageSize, err)
	}
	if _, _, err := NormalizeManifestPage(1, MaxManifestPageSize+1); err == nil {
		t.Fatal("oversized manifest page was accepted")
	}
	if _, _, err := NormalizeChunkPage(-1, 1); err == nil {
		t.Fatal("negative chunk page was accepted")
	}
	if err := ValidateFileToken("file:0123456789abcdef"); err != nil {
		t.Fatalf("valid opaque token rejected: %v", err)
	}
	for _, value := range []string{"", "0123456789abcdef", "file:../../secret", "file token"} {
		if err := ValidateFileToken(value); err == nil {
			t.Fatalf("invalid token %q was accepted", value)
		}
	}
}
