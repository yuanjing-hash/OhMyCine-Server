package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestPan115DirectoryTreeTombstoneFilterIsStrict(t *testing.T) {
	base := providerEventPayload{Kind: cloudpkg.ChangeDeleted, ItemID: "ep", ParentID: "0", Name: "共享20260914020547_目录树.txt"}
	if !isPan115DirectoryTreeTombstone(models.StorageTypePan115, base, map[string]struct{}{}, nil) {
		t.Fatal("expected bookkeeping tombstone")
	}
	cases := []struct {
		name  string
		typ   string
		p     providerEventPayload
		known map[string]struct{}
		ext   []string
		want  bool
	}{
		{"video", models.StorageTypePan115, providerEventPayload{Kind: cloudpkg.ChangeDeleted, ItemID: "v", ParentID: "0", Name: "共享20260914020547_目录树.mkv"}, nil, nil, false},
		{"known", models.StorageTypePan115, base, map[string]struct{}{"ep": {}}, nil, false},
		{"asset", models.StorageTypePan115, base, nil, []string{".txt"}, false},
		{"parent", models.StorageTypePan115, providerEventPayload{Kind: cloudpkg.ChangeDeleted, ItemID: "x", ParentID: "root", Name: base.Name}, nil, nil, false},
		{"shape", models.StorageTypePan115, providerEventPayload{Kind: cloudpkg.ChangeDeleted, ItemID: "x", ParentID: "0", Name: "共享123_目录树.txt"}, nil, nil, false},
		{"storage", models.StorageTypeLocal, base, nil, nil, false},
	}
	for _, tc := range cases {
		if got := isPan115DirectoryTreeTombstone(tc.typ, tc.p, tc.known, tc.ext); got != tc.want {
			t.Errorf("%s got %v", tc.name, got)
		}
	}
}

func TestProviderEventReconcileDefersBeforeScanAllocationWhilePhysicalWriteEntered(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("event barrier", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.Storage{}).Where("id = ?", storage.ID).Update("type", models.StorageTypePan115).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	proof := models.CatalogPhysicalWrite{LibraryID: library.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: "bulk-repair", Revision: 1, State: "entered", OwnerDigest: "test", EnteredAt: now, UpdatedAt: now}
	if err := db.Create(&proof).Error; err != nil {
		t.Fatal(err)
	}
	scope := providerChangeScope{EventCount: 4, DeliveryMaxID: 9, Blocked: true, BlockCode: "move_scope_unknown"}
	if _, err := service.reconcile(withProviderChangeScope(context.Background(), scope), library.ID, "event"); !errors.Is(err, errMediaLibraryEventReconcileDeferred) {
		t.Fatalf("entered physical write did not defer event scan: %v", err)
	}
	var runs int64
	if err := db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", library.ID).Count(&runs).Error; err != nil || runs != 0 {
		t.Fatalf("deferred event allocated scan runs=%d err=%v", runs, err)
	}
	for _, jobType := range []string{JobTypeMediaLibraryRecognition, JobTypeMediaArtifact} {
		var jobs int64
		if err := db.Model(&models.Job{}).Where("job_type = ?", jobType).Count(&jobs).Error; err != nil || jobs != 0 {
			t.Fatalf("deferred event allocated %s jobs=%d err=%v", jobType, jobs, err)
		}
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
	if err := service.ackProviderChangeScope(context.Background(), library.ID, scope.DeliveryIDs); err != nil {
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

func TestProviderChangeAccumulatorCoalescesStormAndRetainsInvalidScope(t *testing.T) {
	accumulator := newProviderChangeAccumulator()
	rows := make([]models.ProviderEvent, 1000)
	for index := range rows {
		rows[index] = persistedProviderEvent(t, cloudpkg.ChangeCreated, "same-file", "parent")
	}
	accumulator.add(rows)
	scope := accumulator.take()
	if scope.Blocked || scope.EventCount != len(rows) || len(scope.Events) != 1 || len(scope.ParentIDs) != 1 {
		t.Fatalf("coalesced scope=%+v", scope)
	}
	accumulator.add([]models.ProviderEvent{persistedProviderEvent(t, cloudpkg.ChangeMoved, "moved", "target")})
	if scope = accumulator.take(); scope.Blocked || len(scope.Events) != 1 || scope.Events[0].Kind != cloudpkg.ChangeMoved {
		t.Fatalf("move scope=%+v", scope)
	}
	accumulator.add([]models.ProviderEvent{{PayloadJSON: `{"kind":"created","item_id":"broken\nidentity"}`}})
	if scope = accumulator.take(); !scope.Blocked || scope.BlockCode != "invalid_event_payload" {
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

func TestPan115ScopedFilesAvoidFullTreeAndUnknownScopeFails(t *testing.T) {
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
	if err != nil || !result.Scoped || driver.streamCalls != 0 {
		t.Fatalf("directory bounded result=%+v stream_calls=%d err=%v", result, driver.streamCalls, err)
	}
	request.providerScope = &providerChangeScope{Events: []providerChangeEvent{{Kind: cloudpkg.ChangeDeleted, ItemID: "unknown-deleted-directory", ParentID: "root"}}, ParentIDs: []string{"root"}}
	result, err = backend.Scan(context.Background(), request)
	if !errors.Is(err, errProviderChangeScopeUnproven) || driver.streamCalls != 0 {
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

func TestPan115EventScopeCannotEscalateToFullScan(t *testing.T) {
	driver := &scopedTreeDriver{fakeCloudDriver: &fakeCloudDriver{
		items: map[string]cloudpkg.Item{
			"root":      {ID: "root", Name: "Library", IsDir: true},
			"target":    {ID: "target", ParentID: "root", Name: "Target", IsDir: true},
			"moved":     {ID: "moved", ParentID: "target", Name: "Show.S01E01.mkv", Size: 12},
			"unrelated": {ID: "unrelated", ParentID: "target", Name: "Other.mkv", Size: 20},
		}, children: map[string][]cloudpkg.Item{"target": {{ID: "unrelated", ParentID: "target", Name: "Other.mkv", Size: 20}}},
	}}
	backend := pan115MediaLibraryBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }}
	connectionID := uint(1)
	request := MediaLibraryScanRequest{Library: models.MediaLibrary{ProviderRootID: "root", Recursive: true}, Storage: models.Storage{Type: models.StorageTypePan115, ConnectionID: &connectionID}, VideoExtensions: []string{".mkv"}}
	for _, scope := range []providerChangeScope{
		{Blocked: true, BlockCode: "scope_overflow"},
		{Blocked: true, BlockCode: "cursor_gap"},
		{},
		{Events: []providerChangeEvent{{Kind: cloudpkg.ChangeMoved, ItemID: "missing"}}},
		{Events: []providerChangeEvent{{Kind: cloudpkg.ChangeCreated, ItemID: "root"}}},
	} {
		request.providerScope = &scope
		if _, err := backend.Scan(context.Background(), request); !errors.Is(err, errProviderChangeScopeUnproven) {
			t.Fatalf("scope=%+v err=%v", scope, err)
		}
		if driver.streamCalls != 0 {
			t.Fatal("event escalated to full scan")
		}
	}
	request.providerScope = &providerChangeScope{Events: []providerChangeEvent{{Kind: cloudpkg.ChangeMoved, ItemID: "moved", ParentID: "target", PreviousParentID: "old"}}, ParentIDs: []string{"target", "old"}}
	for range 2 {
		result, err := backend.Scan(context.Background(), request)
		if err != nil || len(result.Files) != 1 || result.Files[0].ProviderID != "moved" || result.Files[0].RelativePath != "/Target/Show.S01E01.mkv" || driver.streamCalls != 0 {
			t.Fatalf("exact move result=%+v err=%v", result, err)
		}
	}
}

func TestProviderEventMissingScopeAllocatesNoScan(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("bounded", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.Storage{}).Where("id = ?", storage.ID).Update("type", models.StorageTypePan115).Error; err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"event", "incremental", "catch_up"} {
		_, err := service.reconcile(context.Background(), library.ID, kind)
		if !errors.Is(err, errProviderChangeScopeUnproven) {
			t.Fatalf("kind=%s err=%v", kind, err)
		}
	}
	var count int64
	if err := db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", library.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("runs=%d err=%v", count, err)
	}
}

func TestProviderEventHydrationDrainsBoundedPages(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("pages", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxProviderChangeScopeItems+2; i++ {
		payload, _ := json.Marshal(providerEventPayload{Kind: cloudpkg.ChangeCreated, ItemID: fmt.Sprintf("file-%d", i), ParentID: fmt.Sprintf("parent-%d", i)})
		row := models.MediaLibraryProviderEvent{LibraryID: library.ID, InboxEventID: uint(i + 1), PayloadJSON: string(payload)}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	processed := 0
	for {
		pending := newProviderChangeAccumulator()
		if err := service.hydratePendingProviderChanges(context.Background(), library.ID, pending, make(chan struct{}, 1)); err != nil {
			t.Fatal(err)
		}
		scope := pending.take()
		if scope.empty() {
			break
		}
		if scope.Blocked || len(scope.Events) > maxProviderChangeScopeItems/3 {
			t.Fatalf("scope=%+v", scope)
		}
		processed += len(scope.Events)
		if err := service.ackProviderChangeScope(context.Background(), library.ID, scope.DeliveryIDs); err != nil {
			t.Fatal(err)
		}
	}
	if processed != maxProviderChangeScopeItems+2 {
		t.Fatalf("processed=%d", processed)
	}
}

func TestProviderInvalidDeliveryDoesNotStarveValidDelivery(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("invalid isolation", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	bad := models.MediaLibraryProviderEvent{LibraryID: library.ID, InboxEventID: 1, PayloadJSON: `{"kind":"fallback"}`}
	if err := db.Create(&bad).Error; err != nil {
		t.Fatal(err)
	}
	good := models.MediaLibraryProviderEvent{LibraryID: library.ID, InboxEventID: 2, PayloadJSON: `{"kind":"created","item_id":"good","parent_id":"root"}`}
	if err := db.Create(&good).Error; err != nil {
		t.Fatal(err)
	}
	pending := newProviderChangeAccumulator()
	if err := service.hydratePendingProviderChanges(context.Background(), library.ID, pending, make(chan struct{}, 1)); err != nil {
		t.Fatal(err)
	}
	scope := pending.take()
	if len(scope.Events) != 1 || scope.Events[0].ItemID != "good" || scope.Blocked {
		t.Fatalf("scope=%+v", scope)
	}
	if err := service.ackProviderChangeScope(context.Background(), library.ID, scope.DeliveryIDs); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&bad, bad.ID).Error; err != nil || bad.ProcessedAt != nil {
		t.Fatalf("invalid event discarded: %+v err=%v", bad, err)
	}
}

func TestProviderDeltaDuplicateChecksOnlyAffectedIdentities(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("duplicate batch", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	entry := models.MediaLibraryEntry{LibraryID: library.ID, ProviderID: "affected", RelativePath: "/Show.S01E01.mkv", Size: 12, ModifiedAt: now}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	unrelated := models.MediaLibraryEntry{LibraryID: library.ID, ProviderID: "unrelated", RelativePath: "/Other.mkv", Size: 99, ModifiedAt: now}
	if err := db.Create(&unrelated).Error; err != nil {
		t.Fatal(err)
	}
	delta := medialibrary.Result{Partial: true, Scoped: true, Files: []medialibrary.File{{ProviderID: entry.ProviderID, RelativePath: entry.RelativePath, Size: entry.Size, ModifiedAt: now}}}
	if applied, err := service.providerDeltaAlreadyApplied(context.Background(), library.ID, delta); err != nil || !applied {
		t.Fatalf("duplicate applied=%v err=%v", applied, err)
	}
	delta.Files[0].RelativePath = "/Moved/Show.S01E01.mkv"
	if applied, err := service.providerDeltaAlreadyApplied(context.Background(), library.ID, delta); err != nil || applied {
		t.Fatalf("move applied=%v err=%v", applied, err)
	}
}

func TestProviderPageIsolatesUnresolvedDelivery(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("scope isolation", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	connection := models.Connection{Name: "isolated", NameNormalized: "isolated", Provider: cloudpkg.ProviderPan115, CredentialCiphertext: "encrypted", Enabled: true, Revision: 1}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	connectionID := connection.ID
	if err := db.Model(&models.Storage{}).Where("id = ?", storage.ID).Updates(map[string]any{"type": models.StorageTypePan115, "connection_id": connectionID, "root_path": "root"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("provider_root_id", "root").Error; err != nil {
		t.Fatal(err)
	}
	driver := &scopedTreeDriver{fakeCloudDriver: &fakeCloudDriver{items: map[string]cloudpkg.Item{"root": {ID: "root", Name: "Library", IsDir: true}, "good": {ID: "good", ParentID: "root", Name: "Show.S01E01.mkv", Size: 12}}}}
	service.backends.Register(pan115MediaLibraryBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }})
	rows := []models.MediaLibraryProviderEvent{
		{LibraryID: library.ID, InboxEventID: 1, PayloadJSON: `{"kind":"deleted","item_id":"missing-directory","parent_id":"root"}`},
		{LibraryID: library.ID, InboxEventID: 2, PayloadJSON: `{"kind":"created","item_id":"good","parent_id":"root"}`},
		{LibraryID: library.ID, InboxEventID: 1739, PayloadJSON: `{"item_id":"3517398426054559245","kind":"deleted","name":"共享20260914020247_目录树.txt","parent_id":"0","previous_parent_id":""}`},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	pending := newProviderChangeAccumulator()
	pending.addDeliveries(rows, rows[2].ID)
	prepared, err := service.prepareProviderDeliveryPage(context.Background(), library.ID, pending.take())
	if err != nil || len(prepared.DeliveryIDs) != 2 || prepared.DeliveryIDs[0] != rows[1].ID || prepared.DeliveryIDs[1] != rows[2].ID || prepared.VerifiedResult == nil || len(prepared.VerifiedResult.Files) != 1 || driver.streamCalls != 0 {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	if err := service.ackProviderChangeScope(context.Background(), library.ID, prepared.DeliveryIDs); err != nil {
		t.Fatal(err)
	}
	var filtered models.MediaLibraryProviderEvent
	if err := db.First(&filtered, rows[2].ID).Error; err != nil || filtered.ProcessedAt == nil {
		t.Fatalf("historical directory export not acknowledged: %+v err=%v", filtered, err)
	}
	var unresolved models.MediaLibraryProviderEvent
	if err := db.First(&unresolved, rows[0].ID).Error; err != nil || unresolved.ProcessedAt != nil {
		t.Fatalf("unresolved=%+v err=%v", unresolved, err)
	}
}

func TestLocalWatcherScopeIncludesOnlyNamedFilesAndExactDeletes(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "Show.S01E01.mkv")
	unrelated := filepath.Join(root, "Other.mkv")
	if err := os.WriteFile(target, []byte("episode"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unrelated, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	request := MediaLibraryScanRequest{Storage: models.Storage{RootPath: root}, Library: models.MediaLibrary{Recursive: true}, VideoExtensions: []string{".mkv"}, providerScope: &providerChangeScope{LocalPaths: []string{target}}}
	result, err := (localMediaLibraryBackend{}).Scan(context.Background(), request)
	if err != nil || len(result.Files) != 1 || result.Files[0].RelativePath != "/Show.S01E01.mkv" || !result.Partial {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	id := result.Files[0].ProviderID
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	request.knownLocalPaths = map[string][]string{"/Show.S01E01.mkv": {id}}
	result, err = (localMediaLibraryBackend{}).Scan(context.Background(), request)
	if err != nil || len(result.Files) != 0 || len(result.DeletedProviderIDs) != 1 || result.DeletedProviderIDs[0] != id {
		t.Fatalf("delete=%+v err=%v", result, err)
	}
	request.providerScope = &providerChangeScope{LocalPaths: []string{root}}
	if _, err := (localMediaLibraryBackend{}).Scan(context.Background(), request); !errors.Is(err, errProviderChangeScopeUnproven) {
		t.Fatalf("root event allowed full scan: %v", err)
	}
	request.providerScope = &providerChangeScope{LocalPaths: []string{filepath.Join(root, "..", "escape.mkv")}}
	if _, err := (localMediaLibraryBackend{}).Scan(context.Background(), request); err == nil {
		t.Fatal("outside-root path allowed")
	}
}

func TestLocalWatcherDeliversPathAndRetriesFailure(t *testing.T) {
	root := t.TempDir()
	watcher, err := newRecursiveWatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	listener := localMediaLibraryListener{watcher: watcher, incremental: time.Hour}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target := filepath.Join(root, "Show.S01E01.mkv")
	go func() { time.Sleep(100 * time.Millisecond); _ = os.WriteFile(target, []byte("episode"), 0600) }()
	calls := 0
	err = listener.Run(ctx, func(ctx context.Context, kind string) error {
		scope, ok := providerChangeScopeFromContext(ctx)
		if !ok || len(scope.LocalPaths) != 1 || scope.LocalPaths[0] != target || kind != "event" {
			t.Fatalf("scope=%+v kind=%s", scope, kind)
		}
		calls++
		if calls == 1 {
			return errors.New("temporary failure")
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestLocalWatcherUnresolvablePathDoesNotBlockValidFile(t *testing.T) {
	root := t.TempDir()
	watcher, err := newRecursiveWatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	listener := localMediaLibraryListener{watcher: watcher, incremental: time.Hour}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bad := filepath.Join(root, "a-unresolved.mkv")
	good := filepath.Join(root, "z-valid.mkv")
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.WriteFile(bad, []byte("bad"), 0600)
		_ = os.WriteFile(good, []byte("good"), 0600)
	}()
	validHandled := false
	badCalls := 0
	err = listener.Run(ctx, func(ctx context.Context, kind string) error {
		scope, ok := providerChangeScopeFromContext(ctx)
		if !ok {
			t.Fatal("missing local scope")
		}
		for _, name := range scope.LocalPaths {
			if name == bad {
				badCalls++
				return errProviderChangeScopeUnproven
			}
		}
		if len(scope.LocalPaths) == 1 && scope.LocalPaths[0] == good {
			validHandled = true
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || !validHandled || badCalls < 2 {
		t.Fatalf("valid=%v badCalls=%d err=%v", validHandled, badCalls, err)
	}
}
