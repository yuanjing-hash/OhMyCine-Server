package site

import "testing"

func TestNormalizePayloadKindKeepsOnlySafeContractValues(t *testing.T) {
	for _, known := range []PayloadKind{PayloadTorrent, PayloadMagnet, PayloadShare} {
		if got := NormalizePayloadKind(known); got != known {
			t.Fatalf("known payload %q normalized to %q", known, got)
		}
	}
	for _, unknown := range []PayloadKind{"", PayloadUnknown, "bt", "pt", "magnet:?xt=private", "https://private.example.test/torrent"} {
		if got := NormalizePayloadKind(unknown); got != PayloadUnknown {
			t.Fatalf("unsupported payload metadata became %q", got)
		}
	}
}
