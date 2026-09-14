package nodeprotocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validStoragePlanForTest() StorageUploadPlan {
	return StorageUploadPlan{
		StorageID:                "storage-1",
		ProviderType:             StorageProviderPan115,
		SourceExportOperationKey: "export-1",
		SourceManifestDigest:     strings.Repeat("a", 64),
		TargetRootID:             "0",
		Files: []StorageUploadFile{{
			SourceFileToken:    "file:source-1",
			TargetRelativePath: "TV/Season 01/Episode 01.mkv",
			Size:               1024,
			SHA256:             strings.Repeat("b", 64),
			ConflictAction:     StorageConflictFailIfExists,
		}},
	}
}

func TestStorageUploadPlanRejectsTraversalDuplicateTargetsAndUnknownConflict(t *testing.T) {
	for name, mutate := range map[string]func(*StorageUploadPlan){
		"traversal": func(plan *StorageUploadPlan) { plan.Files[0].TargetRelativePath = "../escape.mkv" },
		"absolute":  func(plan *StorageUploadPlan) { plan.Files[0].TargetRelativePath = "/escape.mkv" },
		"duplicate": func(plan *StorageUploadPlan) { plan.Files = append(plan.Files, plan.Files[0]) },
		"conflict":  func(plan *StorageUploadPlan) { plan.Files[0].ConflictAction = "node_decides" },
	} {
		t.Run(name, func(t *testing.T) {
			plan := validStoragePlanForTest()
			mutate(&plan)
			if err := plan.Validate(); err == nil {
				t.Fatal("invalid storage plan was accepted")
			}
		})
	}
}

func TestOperationPlanValidatesTypedStoragePayload(t *testing.T) {
	storage := validStoragePlanForTest()
	payload, _ := json.Marshal(storage)
	now := time.Now().UTC()
	plan := OperationPlan{ProtocolVersion: VersionV1, OperationKey: "upload-1", TaskID: "task-1", NodeID: "node-1", Kind: OperationKindStorageUpload, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(time.Minute), Payload: payload}
	if err := plan.Validate(now); err != nil {
		t.Fatal(err)
	}
	plan.Payload = json.RawMessage(`{"storage_id":"storage-1","unexpected":true}`)
	if err := plan.Validate(now); err == nil {
		t.Fatal("unknown storage payload field was accepted")
	}
}

func TestStorageActionDigestDoesNotDependOnRotatingGrant(t *testing.T) {
	request := StorageActionRequest{RequestID: "request-1", TaskID: "task-1", OperationKey: "upload-1", PlanDigest: strings.Repeat("a", 64), StorageID: "storage-1", Action: StorageActionUpload, GrantID: "grant-1"}
	first, err := request.Digest()
	if err != nil {
		t.Fatal(err)
	}
	request.GrantID = "grant-2"
	second, err := request.Digest()
	if err != nil || first != second {
		t.Fatalf("rotating grant changed request digest: first=%s second=%s err=%v", first, second, err)
	}
}

func TestOperationPlanDigestExcludesLeaseButIncludesPayload(t *testing.T) {
	now := time.Now().UTC()
	plan := OperationPlan{ProtocolVersion: VersionV1, OperationKey: "operation-1", TaskID: "task-1", NodeID: "node-1", Kind: "test", PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(time.Minute), Payload: json.RawMessage(`{"value":1}`)}
	first, err := plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	plan.LeaseEpoch = 9
	plan.LeaseExpiresAt = now.Add(10 * time.Minute)
	second, err := plan.Digest()
	if err != nil || first != second {
		t.Fatalf("lease changed immutable digest: first=%s second=%s err=%v", first, second, err)
	}
	plan.Payload = json.RawMessage(`{"value":2}`)
	changed, err := plan.Digest()
	if err != nil || changed == second {
		t.Fatalf("payload did not change digest: before=%s after=%s err=%v", second, changed, err)
	}
}
