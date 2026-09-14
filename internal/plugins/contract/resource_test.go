package contract

import (
	"strings"
	"testing"
)

func TestNormalizeMagnetRejectsUnsafeAndCanonicalizes(t *testing.T) {
	magnet, ok := NormalizeMagnet("MAGNET:?xt=urn:btih:42b8a6e4f45924b949389b4c01b777686b40fb58&dn=秒速5厘米&dn=秒速5厘米&tr=https%3A%2F%2Ftracker.example%2Fannounce&tr=file%3A%2F%2Fprivate")
	if !ok {
		t.Fatal("expected valid magnet")
	}
	if magnet == "" || magnet[:8] != "magnet:?" {
		t.Fatalf("unexpected canonical magnet %q", magnet)
	}
	if _, ok := NormalizeMagnet("magnet:?xt=urn:btih:not-a-hash"); ok {
		t.Fatal("invalid BTIH accepted")
	}
	unsafeTracker, ok := NormalizeMagnet("magnet:?xt=urn:btih:42b8a6e4f45924b949389b4c01b777686b40fb58&tr=https://user:pass@example.com/a")
	if !ok || strings.Contains(unsafeTracker, "user") {
		t.Fatal("unsafe tracker was not discarded")
	}
}

func TestFormatSizeBytesAcceptsCompactAndBinaryUnits(t *testing.T) {
	for _, test := range []struct {
		input string
		want  int64
	}{
		{"4.6GB", 4939212390},
		{"700 MB", 734003200},
		{"1.5 GiB", 1610612736},
		{"unknown", 0},
	} {
		if got := FormatSizeBytes(test.input); got != test.want {
			t.Fatalf("FormatSizeBytes(%q)=%d, want %d", test.input, got, test.want)
		}
	}
}

func TestResourceRequestValidationBoundsCredentialsAndCaptcha(t *testing.T) {
	if (ResourceLoginRequest{ConnectionID: "c", Username: "u", Password: "short"}).Validate() {
		t.Fatal("short password accepted")
	}
	if (ResourceCookieRequest{ConnectionID: "c", Cookie: "a\nb"}).Validate() {
		t.Fatal("multiline cookie accepted")
	}
	if (ResourceCaptchaRequest{ConnectionID: "c", ChallengeID: "x", Points: nil}).Validate() {
		t.Fatal("empty captcha points accepted")
	}
}
