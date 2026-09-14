package nodeclient

import (
	"bytes"
	"net"
	"strings"
	"testing"
)

func TestNormalizePublicNodeURLRejectsLocalAndNonOriginTargets(t *testing.T) {
	for _, raw := range []string{
		"https://localhost:4433",
		"https://127.0.0.1:4433",
		"https://10.0.0.1:4433",
		"https://node.example.com/path",
		"https://node.example.com?target=internal",
		"https://user@node.example.com",
	} {
		if _, err := NormalizePublicURL(raw); err == nil {
			t.Fatalf("unsafe node URL accepted: %s", raw)
		}
	}
	got, err := NormalizePublicURL("https://node.example.com:4433/")
	if err != nil || got != "https://node.example.com:4433" {
		t.Fatalf("normalized URL=%q err=%v", got, err)
	}
}

func TestNormalizePublicNodeURLAcceptsHTTP(t *testing.T) {
	got, err := NormalizePublicURL("http://node.example.com:80/")
	if err != nil || got != "http://node.example.com:80" {
		t.Fatalf("normalized HTTP URL=%q err=%v", got, err)
	}
}

func TestPublicIPClassification(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.1.1", "::1", "fc00::1", "fe80::1"} {
		if isPublicIP(net.ParseIP(raw)) {
			t.Fatalf("private address classified as public: %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !isPublicIP(net.ParseIP(raw)) {
			t.Fatalf("public address rejected: %s", raw)
		}
	}
}

func TestDecodeBoundedJSONRejectsOversizedResponses(t *testing.T) {
	payload := `{"value":"` + strings.Repeat("x", 1<<20) + `"}`
	var output map[string]string
	if err := decodeBoundedJSON(bytes.NewBufferString(payload), &output); err == nil {
		t.Fatal("oversized node response accepted")
	}
}
