package contract

import (
	"encoding/json"
	"os"
	"testing"
)

func TestGoContractConsumesSharedOfflineFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/offline-download-plan.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var plan OfflineDownloadPlan
	if err := json.Unmarshal(body, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Version != 1 || plan.Format != "hls" || plan.VariantID != "fixture-1080p" || len(plan.Assets) != 2 || plan.Assets[0].Kind != "video" || plan.Assets[1].ExpectedContentType != "text/vtt" {
		t.Fatalf("offline ABI drift: %+v", plan)
	}
	if _, ok := knownCapabilities[CapabilityMediaOffline]; !ok {
		t.Fatal("offline capability missing")
	}
	if _, ok := knownCapabilities[CapabilitySiteAuth]; !ok {
		t.Fatal("read-only auth capability missing")
	}
}
