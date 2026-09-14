package nodeprotocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStorageSourcePlanRequiresDifferentStableIdentities(t *testing.T) {
	plan := StorageSourcePlan{StorageID: "source-1", ProviderType: StorageProviderPan115, SourceKind: StorageSourceKindPan115OfflineMagnet, TargetKind: StorageSourceTargetPan115, SourceIdentityDigest: strings.Repeat("a", 64), TargetIdentityDigest: strings.Repeat("b", 64), SourceContentDigest: strings.Repeat("c", 64), OfflineDestinationID: "0"}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	plan.TargetIdentityDigest = plan.SourceIdentityDigest
	if err := plan.Validate(); err == nil {
		t.Fatal("same-identity source route was accepted")
	}
}

func TestStorageSourceOperationRejectsUnknownPayloadAndKeepsURIOutOfPlan(t *testing.T) {
	plan := StorageSourcePlan{StorageID: "source-1", ProviderType: StorageProviderPan115, SourceKind: StorageSourceKindPan115OfflineMagnet, TargetKind: StorageSourceTargetPan115, SourceIdentityDigest: strings.Repeat("a", 64), TargetIdentityDigest: strings.Repeat("b", 64), SourceContentDigest: strings.Repeat("c", 64), OfflineDestinationID: "0"}
	payload, _ := json.Marshal(plan)
	if strings.Contains(string(payload), "source_uri") || strings.Contains(string(payload), "cookie") {
		t.Fatalf("source secret leaked into plan: %s", payload)
	}
	now := time.Now().UTC()
	operation := OperationPlan{ProtocolVersion: VersionV1, OperationKey: "source-1", TaskID: "task-1", NodeID: "node-1", Kind: OperationKindStorageSourceMaterialize, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(time.Minute), Payload: payload}
	if err := operation.Validate(now); err != nil {
		t.Fatal(err)
	}
	operation.Payload = append(payload[:len(payload)-1], []byte(`,"source_uri":"secret"}`)...)
	if err := operation.Validate(now); err == nil {
		t.Fatal("source URI was accepted in immutable operation payload")
	}
}

func TestStorageSourceKindsExposePlainURLAsUnsupported(t *testing.T) {
	if !StorageSourceKindSupported(StorageSourceKindPan115OfflineMagnet) || !StorageSourceKindSupported(StorageSourceKindPan115Share) {
		t.Fatal("executable 115 source kinds were not advertised")
	}
	if StorageSourceKindSupported(StorageSourceKindPan115OfflineURL) {
		t.Fatal("plain URL was advertised despite lacking a stable provider task identity")
	}
	plan := StorageSourcePlan{StorageID: "source-1", ProviderType: StorageProviderPan115, SourceKind: StorageSourceKindPan115OfflineURL, TargetKind: StorageSourceTargetPan115, SourceIdentityDigest: strings.Repeat("a", 64), TargetIdentityDigest: strings.Repeat("b", 64), SourceContentDigest: strings.Repeat("c", 64), OfflineDestinationID: "0"}
	if err := plan.Validate(); err == nil {
		t.Fatal("plain URL source was accepted into an executable Node plan")
	}
}

func TestStorageSourcePlanAllowsLocalTargetWithoutFakeStorageIdentity(t *testing.T) {
	plan := StorageSourcePlan{StorageID: "source-1", ProviderType: StorageProviderPan115, SourceKind: StorageSourceKindPan115OfflineMagnet, TargetKind: StorageSourceTargetLocal, SourceIdentityDigest: strings.Repeat("a", 64), SourceContentDigest: strings.Repeat("c", 64), OfflineDestinationID: "0"}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	plan.TargetIdentityDigest = strings.Repeat("b", 64)
	if err := plan.Validate(); err == nil {
		t.Fatal("local target accepted a fabricated storage identity")
	}
}

func TestStorageSourceContentDigestUsesStableProviderIdentity(t *testing.T) {
	hexMagnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=first"
	base32Magnet := "magnet:?dn=second&XT=URN:BTIH:AERUKZ4JVPG66AJDIVTYTK6N54ASGRLH"
	first, err := StorageSourceContentDigest(StorageSourceKindPan115OfflineMagnet, hexMagnet)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StorageSourceContentDigest(StorageSourceKindPan115OfflineMagnet, base32Magnet)
	if err != nil || first != second {
		t.Fatalf("equivalent BTIH identities diverged: %s %s %v", first, second, err)
	}
	shareA, err := StorageSourceContentDigest(StorageSourceKindPan115Share, "https://115.com/s/share-code?password=one")
	if err != nil {
		t.Fatal(err)
	}
	shareB, err := StorageSourceContentDigest(StorageSourceKindPan115Share, "https://share.115.com/s/share-code?pwd=two")
	if err != nil || shareA != shareB {
		t.Fatalf("rotated share receive code changed stable identity: %s %s %v", shareA, shareB, err)
	}
	for _, sample := range []struct{ kind, raw string }{
		{StorageSourceKindPan115OfflineMagnet, "https://example.test/file"},
		{StorageSourceKindPan115OfflineMagnet, "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&xt=urn:btih:ffffffffffffffffffffffffffffffffffffffff"},
		{StorageSourceKindPan115Share, "https://115.com.evil.test/s/share-code?password=one"},
	} {
		if _, err := StorageSourceContentDigest(sample.kind, sample.raw); err == nil {
			t.Fatalf("unsafe source identity accepted: %s", sample.raw)
		}
	}
}

func TestStorageSourceActionDigestAllowsGrantRotation(t *testing.T) {
	request := StorageSourceActionRequest{RequestID: "request-1", TaskID: "task-1", OperationKey: "source-1", PlanDigest: strings.Repeat("a", 64), StorageID: "storage-1", Action: StorageSourceActionMaterialize, GrantID: "grant-1"}
	first, err := request.Digest()
	if err != nil {
		t.Fatal(err)
	}
	request.GrantID = "grant-2"
	second, err := request.Digest()
	if err != nil || first != second {
		t.Fatalf("grant rotation changed request digest: %s %s %v", first, second, err)
	}
}

func TestStorageSourceCleanupDigestBindsOperationAndTask(t *testing.T) {
	request := StorageSourceCleanupRequest{RequestID: "cleanup-1", OperationKey: "source-1", TaskID: "task-1", PlanDigest: strings.Repeat("a", 64)}
	first, err := request.Digest()
	if err != nil {
		t.Fatal(err)
	}
	request.TaskID = "task-2"
	second, err := request.Digest()
	if err != nil || first == second {
		t.Fatalf("cleanup digest did not bind task: %s %s %v", first, second, err)
	}
}
