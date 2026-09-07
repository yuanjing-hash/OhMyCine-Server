package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
)

func TestMediaLibraryStructureResponsesDoNotExposePrivatePlanIdentity(t *testing.T) {
	repair := models.MediaLibraryStructureRepair{
		ID: "repair-safe", LibraryID: 7, OwnerID: 42, JobID: stringPointer("job-safe"),
		Scope: models.MediaLibraryStructureScopeWork, WorkKey: "series:tmdb:100",
		RuleFingerprint: "private-rule", PlanJSON: `{"provider_id":"private-provider"}`,
		StateJSON: `{"absolute_path":"D:\\private"}`, Phase: "failed", TotalItems: 2, SucceededItems: 1, FailedItems: 1,
	}
	payload, err := json.Marshal(mediaLibraryStructureRepairDTO(repair))
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(payload)
	if !strings.Contains(serialized, `"phase":"partial_failed"`) || !strings.Contains(serialized, `"failed_items":1`) {
		t.Fatalf("partial repair projection missing: %s", serialized)
	}
	for _, private := range []string{"owner_id", "work_key", "series:tmdb:100", "private-rule", "private-provider", "absolute_path"} {
		if strings.Contains(serialized, private) {
			t.Fatalf("private repair identity leaked: %s", serialized)
		}
	}

	diagnostics, err := json.Marshal(services.MediaLibraryStructureDiagnostics{Issues: []services.StructureIssue{{Code: "path_mismatch", Kind: "video", WorkKey: "movie:tmdb:346", CurrentPath: "old/movie.mkv", ExpectedPath: "电影/movie.mkv", Repairable: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(diagnostics), "work_key") || strings.Contains(string(diagnostics), "movie:tmdb:346") {
		t.Fatalf("private diagnostic work key leaked: %s", diagnostics)
	}
}

func stringPointer(value string) *string { return &value }
