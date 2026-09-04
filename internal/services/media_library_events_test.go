package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

func TestProviderListenerRequeuesScopeAfterReconcileFailure(t *testing.T) {
	payload, err := json.Marshal(providerEventPayload{Kind: cloudpkg.ChangeCreated, ItemID: "file", ParentID: "root", Name: "safe.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	pending := newProviderChangeAccumulator()
	pending.addDeliveries([]models.MediaLibraryProviderEvent{{ID: 7, PayloadJSON: string(payload)}}, 7)
	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener := &providerMediaLibraryListener{wake: wake, pending: pending, incremental: time.Hour, debounce: time.Millisecond}
	calls := 0
	err = listener.Run(ctx, func(reconcileCtx context.Context, _ string) error {
		calls++
		scope, ok := providerChangeScopeFromContext(reconcileCtx)
		if !ok || scope.DeliveryMaxID != 7 || len(scope.Events) != 1 {
			t.Fatalf("retry scope=%+v ok=%v", scope, ok)
		}
		if calls == 1 {
			return errors.New("temporary reconcile failure")
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatalf("listener err=%v calls=%d", err, calls)
	}
}

func TestProviderInboxAcknowledgesAfterDurableLibraryFanout(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("durable delivery", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	connection := models.Connection{Name: "delivery", NameNormalized: "delivery", Provider: cloudpkg.ProviderPan115, CredentialCiphertext: "encrypted", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.Storage{}).Where("id = ?", storage.ID).Updates(map[string]any{"type": models.StorageTypePan115, "connection_id": connection.ID, "root_path": "root", "root_path_normalized": "root"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	event, valid := normalizeProviderEvent(connection.ID, providerLifeStream, cloudpkg.ChangeEvent{ID: "1", Time: now, Kind: cloudpkg.ChangeCreated, ItemID: "file", ParentID: "root", Name: "safe.mkv"}, now)
	if !valid || db.Create(&event).Error != nil {
		t.Fatalf("event valid=%v id=%d", valid, event.ID)
	}
	inbox := NewProviderEventService(db, service)
	if processed, err := inbox.ProcessPending(context.Background(), connection.ID); err != nil || processed != 1 {
		t.Fatalf("processed=%d err=%v", processed, err)
	}
	var delivery models.MediaLibraryProviderEvent
	if err := db.Where("library_id = ? AND inbox_event_id = ?", library.ID, event.ID).First(&delivery).Error; err != nil || delivery.ProcessedAt != nil {
		t.Fatalf("durable delivery=%+v err=%v", delivery, err)
	}
	pending := newProviderChangeAccumulator()
	wake := make(chan struct{}, 1)
	if err := service.hydratePendingProviderChanges(context.Background(), library.ID, pending, wake); err != nil {
		t.Fatal(err)
	}
	scope := pending.take()
	if scope.DeliveryMaxID != delivery.ID || len(scope.Events) != 1 {
		t.Fatalf("hydrated scope=%+v", scope)
	}
	if err := service.ackPendingProviderChanges(context.Background(), library.ID, scope.DeliveryMaxID); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&delivery, delivery.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("acknowledged delivery was not cleaned up: delivery=%+v err=%v", delivery, err)
	}
}

type scopedTreeDriver struct {
	*fakeCloudDriver
	streamCalls int
}

func (d *scopedTreeDriver) StreamTree(_ context.Context, _ string, _ int, emit func(cloudpkg.TreeBatch) error) error {
	d.streamCalls++
	return emit(cloudpkg.TreeBatch{})
}

func persistedProviderEvent(t *testing.T, kind, itemID, parentID string) models.ProviderEvent {
	t.Helper()
	payload, err := json.Marshal(providerEventPayload{Kind: kind, ItemID: itemID, ParentID: parentID, Name: "safe.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	return models.ProviderEvent{PayloadJSON: string(payload)}
}

func TestProviderChangeAccumulatorCoalescesStormAndFallsBackConservatively(t *testing.T) {
	accumulator := newProviderChangeAccumulator()
	rows := make([]models.ProviderEvent, 1000)
	for index := range rows {
		rows[index] = persistedProviderEvent(t, cloudpkg.ChangeCreated, "same-file", "parent")
	}
	accumulator.add(rows)
	scope := accumulator.take()
	if scope.FullFallback || scope.EventCount != len(rows) || len(scope.Events) != 1 || len(scope.ParentIDs) != 1 {
		t.Fatalf("coalesced scope=%+v", scope)
	}
	accumulator.add([]models.ProviderEvent{persistedProviderEvent(t, cloudpkg.ChangeMoved, "moved", "target")})
	if scope = accumulator.take(); !scope.FullFallback || scope.FallbackCode != "move_scope_unknown" || len(scope.Events) != 0 {
		t.Fatalf("move scope=%+v", scope)
	}
	accumulator.add([]models.ProviderEvent{{PayloadJSON: `{"kind":"created","item_id":"broken\nidentity"}`}})
	if scope = accumulator.take(); !scope.FullFallback || scope.FallbackCode != "invalid_event_payload" {
		t.Fatalf("invalid scope=%+v", scope)
	}
	deliveryPayload, err := json.Marshal(providerEventPayload{Kind: cloudpkg.ChangeCreated, ItemID: "delivery-file", ParentID: "parent", Name: "safe.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	deliveries := []models.MediaLibraryProviderEvent{{ID: 9, PayloadJSON: string(deliveryPayload)}}
	accumulator.addDeliveries(deliveries, 9)
	accumulator.addDeliveries(deliveries, 9)
	if scope = accumulator.take(); scope.EventCount != 1 || len(scope.DeliveryIDs) != 1 || scope.DeliveryMaxID != 9 {
		t.Fatalf("replayed delivery expanded scope=%+v", scope)
	}
}

func TestPan115ScopedFilesAvoidFullTreeAndDirectoryFallsBack(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	driver := &scopedTreeDriver{fakeCloudDriver: &fakeCloudDriver{
		items: map[string]cloudpkg.Item{
			"root":    {ID: "root", ParentID: "0", Name: "媒体", IsDir: true},
			"created": {ID: "created", ParentID: "root", Name: "Created.2026.mkv", Size: 10, ModifiedAt: now},
			"updated": {ID: "updated", ParentID: "root", Name: "Updated.2026.mkv", Size: 20, ModifiedAt: now},
			"folder":  {ID: "folder", ParentID: "root", Name: "Series", IsDir: true, ModifiedAt: now},
		},
		children: map[string][]cloudpkg.Item{
			"root": {
				{ID: "created", ParentID: "root", Name: "Created.2026.mkv", Size: 10, ModifiedAt: now},
				{ID: "updated", ParentID: "root", Name: "Updated.2026.mkv", Size: 20, ModifiedAt: now},
				{ID: "folder", ParentID: "root", Name: "Series", IsDir: true, ModifiedAt: now},
			},
		},
	}}
	backend := pan115MediaLibraryBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
	connectionID := uint(1)
	request := MediaLibraryScanRequest{
		Library:         models.MediaLibrary{ProviderRootID: "root", Recursive: true},
		Storage:         models.Storage{Type: models.StorageTypePan115, RootPath: "root", ConnectionID: &connectionID},
		VideoExtensions: []string{".mkv"},
		providerScope: &providerChangeScope{Events: []providerChangeEvent{
			{Kind: cloudpkg.ChangeCreated, ItemID: "created", ParentID: "root"},
			{Kind: cloudpkg.ChangeCreated, ItemID: "updated", ParentID: "root"},
			{Kind: cloudpkg.ChangeDeleted, ItemID: "deleted", ParentID: "root"},
		}, ParentIDs: []string{"root"}},
		knownProviderIDs: map[string]struct{}{"deleted": {}},
	}
	result, err := backend.Scan(context.Background(), request)
	if err != nil || !result.Scoped || !result.Partial || len(result.Files) != 2 || len(result.DeletedProviderIDs) != 1 || result.DeletedProviderIDs[0] != "deleted" || driver.streamCalls != 0 {
		t.Fatalf("scoped result=%+v stream_calls=%d err=%v", result, driver.streamCalls, err)
	}
	request.providerScope = &providerChangeScope{Events: []providerChangeEvent{{Kind: cloudpkg.ChangeCreated, ItemID: "folder", ParentID: "root"}}, ParentIDs: []string{"root"}}
	result, err = backend.Scan(context.Background(), request)
	if err != nil || result.Scoped || driver.streamCalls != 1 {
		t.Fatalf("directory fallback result=%+v stream_calls=%d err=%v", result, driver.streamCalls, err)
	}
	request.providerScope = &providerChangeScope{Events: []providerChangeEvent{{Kind: cloudpkg.ChangeDeleted, ItemID: "unknown-deleted-directory", ParentID: "root"}}, ParentIDs: []string{"root"}}
	result, err = backend.Scan(context.Background(), request)
	if err != nil || result.Scoped || driver.streamCalls != 2 {
		t.Fatalf("unknown deleted directory must fall back: result=%+v stream_calls=%d err=%v", result, driver.streamCalls, err)
	}
}

func TestScopedPan115PublishPrunesOnlyExplicitIdentity(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	service.SetQueueService(NewQueueService(db, NewAuditService(db)))
	created, err := service.Create(context.Background(), actor, testLibraryInput("Scoped publish", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", created.ID).Updates(map[string]any{"dirty_generation": 1, "baseline_generation": 1}).Error; err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	targetFile := medialibrary.File{RelativePath: "/Delete.2025.mkv", ProviderID: "delete-provider", ProviderIDStable: true, ModifiedAt: now}
	unrelatedFile := medialibrary.File{RelativePath: "/Keep.2025.mkv", ProviderID: "keep-provider", ProviderIDStable: true, ModifiedAt: now}
	targetUnit := medialibrary.GroupRecognitionUnits([]medialibrary.File{targetFile})[0]
	unrelatedUnit := medialibrary.GroupRecognitionUnits([]medialibrary.File{unrelatedFile})[0]
	recognitions := []models.MediaLibraryRecognition{
		{LibraryID: library.ID, SourceKey: targetUnit.SourceKey, InputFingerprint: targetUnit.InputFingerprint, ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "删除目标", MetadataJSON: currentRecognitionMetadataJSON(t), LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
		{LibraryID: library.ID, SourceKey: unrelatedUnit.SourceKey, InputFingerprint: unrelatedUnit.InputFingerprint, ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "保留目标", MetadataJSON: currentRecognitionMetadataJSON(t), LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&recognitions).Error; err != nil {
		t.Fatal(err)
	}
	entries := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RelativePath: "/Delete.2025.mkv", ProviderID: "delete-provider", RecognitionID: &recognitions[0].ID, MediaType: "movie", Title: "删除目标", WorkKey: "movie:delete", MatchStatus: mediaRecognitionStatusMatched, LastGeneration: 1, ModifiedAt: now, CreatedAt: now, UpdatedAt: now},
		{LibraryID: library.ID, RelativePath: "/Keep.2025.mkv", ProviderID: "keep-provider", RecognitionID: &recognitions[1].ID, MediaType: "movie", Title: "保留目标", WorkKey: "movie:keep", MatchStatus: mediaRecognitionStatusMatched, LastGeneration: 1, ModifiedAt: now, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	artifactRun := models.MediaArtifactRun{ID: "00000000-0000-0000-0000-000000000001", LibraryID: library.ID, Generation: 1, PolicyJSON: "{}", Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&artifactRun).Error; err != nil {
		t.Fatal(err)
	}
	artifact := models.MediaArtifact{OpaqueID: "unrelated-artifact", RunID: artifactRun.ID, LibraryID: library.ID, SourceIdentity: "keep-provider", Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "Keep.2025.strm", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	delta := medialibrary.Result{
		Scoped: true, Partial: true, AuthoritativeParentPaths: []string{"/"},
		Files: []medialibrary.File{{RelativePath: unrelatedFile.RelativePath, ProviderID: unrelatedFile.ProviderID, ProviderIDStable: true, ModifiedAt: now}},
	}
	merged, err := service.mergeScopedPan115Catalog(context.Background(), library.ID, delta)
	if err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "event", Status: "running", Phase: "enumerating", Generation: 2, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: now}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	published, err := service.publishFastPan115Scan(context.Background(), library, storage, profile, run, merged, time.Now(), serverlog.OperationLibraryEventScan)
	if err != nil || published.Removed != 1 || !published.Partial {
		t.Fatalf("published=%+v err=%v", published, err)
	}
	var targetEntries, targetRecognitions, unrelatedEntries, unrelatedRecognitions, unrelatedArtifacts int64
	_ = db.Model(&models.MediaLibraryEntry{}).Where("id = ?", entries[0].ID).Count(&targetEntries).Error
	_ = db.Model(&models.MediaLibraryRecognition{}).Where("id = ?", recognitions[0].ID).Count(&targetRecognitions).Error
	_ = db.Model(&models.MediaLibraryEntry{}).Where("id = ?", entries[1].ID).Count(&unrelatedEntries).Error
	_ = db.Model(&models.MediaLibraryRecognition{}).Where("id = ?", recognitions[1].ID).Count(&unrelatedRecognitions).Error
	_ = db.Model(&models.MediaArtifact{}).Where("id = ?", artifact.ID).Count(&unrelatedArtifacts).Error
	if targetEntries != 0 || targetRecognitions != 0 || unrelatedEntries != 1 || unrelatedRecognitions != 1 || unrelatedArtifacts != 1 {
		t.Fatalf("target=(%d,%d) unrelated=(%d,%d,%d)", targetEntries, targetRecognitions, unrelatedEntries, unrelatedRecognitions, unrelatedArtifacts)
	}
}
