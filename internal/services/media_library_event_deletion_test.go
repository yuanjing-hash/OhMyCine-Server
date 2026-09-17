package services

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

func deletionFixture(t *testing.T) (*MediaLibraryService, *gorm.DB, models.MediaLibrary, models.Storage, models.MediaLibraryProviderEvent, models.ProviderEvent) {
	t.Helper()
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := s.Create(context.Background(), actor, testLibraryInput("deletion", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	connection := models.Connection{Name: "deletion", NameNormalized: "deletion", Provider: cloudpkg.ProviderPan115, CredentialCiphertext: "encrypted", Enabled: true, Revision: 1}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	storage.Type, storage.ConnectionID, storage.RootPath = models.StorageTypePan115, &connection.ID, "root"
	if err := db.Save(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library.ProviderRootID = "root"
	if err := db.Save(&library.MediaLibrary).Error; err != nil {
		t.Fatal(err)
	}
	payload := providerEventPayload{Kind: cloudpkg.ChangeDeleted, ItemID: "file", ParentID: "", Name: "Any.S02E01.mkv"}
	encoded, _ := json.Marshal(payload)
	inbox := models.ProviderEvent{ConnectionID: connection.ID, Stream: "life", ProviderEventID: "100", EventTime: time.Now().UTC().Add(time.Second), Kind: payload.Kind, ItemID: payload.ItemID, ParentID: payload.ParentID, PayloadJSON: string(encoded)}
	if err := db.Create(&inbox).Error; err != nil {
		t.Fatal(err)
	}
	row := models.MediaLibraryProviderEvent{LibraryID: library.ID, InboxEventID: inbox.ID, PayloadJSON: string(encoded), SourceFingerprint: catalogSourceFingerprint(library.MediaLibrary, storage)}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return s, db, library.MediaLibrary, storage, row, inbox
}

func TestProviderDeletionLocalFileNoNetworkAndDuplicate(t *testing.T) {
	s, db, lib, _, row, _ := deletionFixture(t)
	entry := models.MediaLibraryEntry{LibraryID: lib.ID, ProviderID: "file", RelativePath: "/Any/S02/01.mkv", Size: 12}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	// No usable provider factory: any Stat/Scan would fail the test.
	s.backends.Register(pan115MediaLibraryBackend{driver: func(uint) (cloudpkg.Driver, error) { t.Fatal("deleted file must not query provider"); return nil, nil }})
	scope := providerChangeScope{DeliveryIDs: []uint{row.ID}}
	prepared, err := s.prepareProviderDeliveryPage(context.Background(), lib.ID, scope)
	if err != nil || len(prepared.VerifiedResult.DeletedProviderIDs) != 1 {
		t.Fatalf("%+v %v", prepared, err)
	}
	if err := db.Delete(&entry).Error; err != nil {
		t.Fatal(err)
	}
	again, err := s.prepareProviderDeliveryPage(context.Background(), lib.ID, scope)
	if err != nil || !again.empty() {
		t.Fatalf("duplicate=%+v %v", again, err)
	}
}

func TestProviderDeletionUnknownCompletesWithoutHotLoopOrNameExceptions(t *testing.T) {
	s, db, lib, _, row, _ := deletionFixture(t)
	s.backends.Register(pan115MediaLibraryBackend{driver: func(uint) (cloudpkg.Driver, error) {
		t.Fatal("unknown deleted identity must not be Stat repeatedly")
		return nil, nil
	}})
	prepared, err := s.prepareProviderDeliveryPage(context.Background(), lib.ID, providerChangeScope{DeliveryIDs: []uint{row.ID}})
	if err != nil || !prepared.empty() {
		t.Fatalf("%+v %v", prepared, err)
	}
	if err := db.First(&row, row.ID).Error; err != nil || row.ProcessedAt == nil || row.ResolutionReason != providerDeletionNoResiduals {
		t.Fatalf("row=%+v %v", row, err)
	}
	acc := newProviderChangeAccumulator()
	if err := s.hydratePendingProviderChanges(context.Background(), lib.ID, acc, nil); err != nil || !acc.take().empty() {
		t.Fatal("parked event retried", err)
	}
}

func TestProviderDeletionLegacyUnknownNoOpPreservesSameName(t *testing.T) {
	s, db, lib, _, row, _ := deletionFixture(t)
	if err := db.Model(&row).Update("source_fingerprint", "").Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{LibraryID: lib.ID, ProviderID: "another-stable-id", RelativePath: "/Any.S02E01.mkv"}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	prepared, err := s.prepareProviderDeliveryPage(context.Background(), lib.ID, providerChangeScope{DeliveryIDs: []uint{row.ID}})
	if err != nil || !prepared.empty() {
		t.Fatalf("%+v %v", prepared, err)
	}
	if err := db.First(&row, row.ID).Error; err != nil || row.ProcessedAt == nil || row.ResolutionCode != providerDeletionNoResiduals {
		t.Fatalf("%+v %v", row, err)
	}
	if err := db.First(&entry, entry.ID).Error; err != nil {
		t.Fatal("same-name identity deleted", err)
	}
}

func TestProviderDeletionLegacyTrackedIdentityRecoversSource(t *testing.T) {
	s, db, lib, storage, row, _ := deletionFixture(t)
	if err := db.Model(&row).Update("source_fingerprint", "").Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{LibraryID: lib.ID, ProviderID: "file", RelativePath: "/Any.S02E01.mkv"}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	prepared, err := s.prepareProviderDeliveryPage(context.Background(), lib.ID, providerChangeScope{DeliveryIDs: []uint{row.ID}})
	if err != nil || len(prepared.VerifiedResult.DeletedProviderIDs) != 1 {
		t.Fatalf("%+v %v", prepared, err)
	}
	if err := db.First(&row, row.ID).Error; err != nil || row.SourceFingerprint != catalogSourceFingerprint(lib, storage) || row.ProcessedAt != nil {
		t.Fatalf("%+v %v", row, err)
	}
}

func TestProviderDeletionPausedNoResidualsRecheckedLocally(t *testing.T) {
	s, db, lib, _, row, _ := deletionFixture(t)
	if err := db.Model(&row).Updates(map[string]any{"source_fingerprint": "", "resolution_code": providerEventNeedsReview, "resolution_reason": "deletion_source_unproven"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.hydratePendingProviderChanges(context.Background(), lib.ID, newProviderChangeAccumulator(), nil); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row, row.ID).Error; err != nil || row.ProcessedAt == nil || row.ResolutionCode != providerDeletionNoResiduals {
		t.Fatalf("%+v %v", row, err)
	}
}

func TestProviderDeletionRemovedManifestIsNotResidual(t *testing.T) {
	s, db, lib, _, row, _ := deletionFixture(t)
	owner := models.MediaArtifactRun{ID: "removed-owner", LibraryID: lib.ID, Generation: 1, PolicyJSON: "{}", Status: models.MediaArtifactStatusCompleted}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	artifact := models.MediaArtifact{OpaqueID: "removed-manifest", RunID: owner.ID, LibraryID: lib.ID, ProviderItemID: "file", Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/Any.strm", Managed: true, Status: "removed"}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	complete, err := s.completeAbsentProviderDeletion(context.Background(), lib.ID, row.ID, "file")
	if err != nil || !complete {
		t.Fatalf("complete=%v err=%v", complete, err)
	}
}

func TestProviderDeletionLiveSidecarBlocksNoOp(t *testing.T) {
	s, db, lib, _, row, _ := deletionFixture(t)
	owner := models.MediaArtifactRun{ID: "sidecar-owner", LibraryID: lib.ID, Generation: 1, PolicyJSON: "{}", Status: models.MediaArtifactStatusCompleted}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	artifact := models.MediaArtifact{OpaqueID: "sidecar-manifest", RunID: owner.ID, LibraryID: lib.ID, ProviderItemID: "file", Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/Any.nfo", Managed: true, Status: models.MediaArtifactStatusCompleted}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	complete, err := s.completeAbsentProviderDeletion(context.Background(), lib.ID, row.ID, "file")
	if err != nil || complete {
		t.Fatalf("complete=%v err=%v", complete, err)
	}
}

func TestProviderDeletionPausedPagesDoNotStarve(t *testing.T) {
	s, db, lib, _, row, inbox := deletionFixture(t)
	if err := db.Model(&row).Update("resolution_code", providerEventNeedsReview).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		event := inbox
		event.ID = 0
		event.ProviderEventID = fmt.Sprint(200 + i)
		if err := db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
		delivery := row
		delivery.ID = 0
		delivery.InboxEventID = event.ID
		delivery.ResolutionCode = providerEventNeedsReview
		if err := db.Create(&delivery).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []int64{1, 0} {
		if err := s.recheckPausedProviderDeletions(context.Background(), lib.ID); err != nil {
			t.Fatal(err)
		}
		var remaining int64
		if err := db.Model(&models.MediaLibraryProviderEvent{}).Where("library_id = ? AND processed_at IS NULL", lib.ID).Count(&remaining).Error; err != nil || remaining != want {
			t.Fatalf("remaining=%d want=%d err=%v", remaining, want, err)
		}
	}
}

func TestProviderDeletionHistoricalPathWithoutLiveResidualCompletes(t *testing.T) {
	s, db, lib, storage, row, inbox := deletionFixture(t)
	run := models.MediaLibraryScanRun{ID: 123, LibraryID: lib.ID, StartedAt: inbox.EventTime.Add(-time.Hour)}
	result := medialibrary.Result{Files: []medialibrary.File{{ProviderID: "file", RelativePath: "/Old/01.mkv"}}}
	if err := stageProviderPaths(context.Background(), db, lib, storage, result, run); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return persistProviderPathsTx(tx, lib, storage, result, run.StartedAt, run.ID)
	}); err != nil {
		t.Fatal(err)
	}
	complete, err := s.completeAbsentProviderDeletion(context.Background(), lib.ID, row.ID, "file")
	if err != nil || !complete {
		t.Fatalf("complete=%v err=%v", complete, err)
	}
}

func TestProviderDeletionPausedCurrentIdentityResumesWithoutScan(t *testing.T) {
	s, db, lib, _, row, _ := deletionFixture(t)
	if err := db.Model(&row).Updates(map[string]any{"source_fingerprint": "", "resolution_code": providerEventNeedsReview, "resolution_reason": "deletion_source_unproven"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.MediaLibraryEntry{LibraryID: lib.ID, ProviderID: "file", RelativePath: "/Any.mkv"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.recheckPausedProviderDeletions(context.Background(), lib.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row, row.ID).Error; err != nil || row.ResolutionCode != "" || row.ProcessedAt != nil || row.SourceFingerprint == "" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
	var scans int64
	if err := db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", lib.ID).Count(&scans).Error; err != nil || scans != 0 {
		t.Fatalf("scans=%d err=%v", scans, err)
	}
}

func TestProviderDeletionCommitRejectsRestoreAndRootReplacement(t *testing.T) {
	s, db, lib, storage, row, inbox := deletionFixture(t)
	entry := models.MediaLibraryEntry{LibraryID: lib.ID, ProviderID: "file", RelativePath: "/Any.mkv"}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	prepared, err := s.prepareProviderDeliveryPage(context.Background(), lib.ID, providerChangeScope{DeliveryIDs: []uint{row.ID}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := withProviderChangeScope(context.Background(), prepared)
	if err := db.Transaction(func(tx *gorm.DB) error { return validateProviderDeletionScopeTx(ctx, tx, lib, storage) }); err != nil {
		t.Fatal(err)
	}
	older := inbox
	older.ID = 0
	older.ProviderEventID = "99"
	older.Kind = cloudpkg.ChangeMoved
	if err := db.Create(&older).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return validateProviderDeletionScopeTx(ctx, tx, lib, storage) }); err != nil {
		t.Fatal("receive order overrode provider order", err)
	}
	restore := inbox
	restore.ID = 0
	restore.ProviderEventID = "101"
	restore.Kind = cloudpkg.ChangeCreated
	if err := db.Create(&restore).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return validateProviderDeletionScopeTx(ctx, tx, lib, storage) }); err == nil {
		t.Fatal("same-second restore raced commit")
	}
	if err := db.Delete(&restore).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&lib).Update("provider_root_id", "replacement").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return validateProviderDeletionScopeTx(ctx, tx, lib, storage) }); err == nil {
		t.Fatal("source replacement raced commit")
	}
}

func TestProviderDeletionDirectoryPreservesMovedAndOtherVersions(t *testing.T) {
	s, db, lib, storage, row, inbox := deletionFixture(t)
	observed := inbox.EventTime.Add(-time.Hour)
	result := medialibrary.Result{Directories: []cloudpkg.TreeEntry{{Item: cloudpkg.Item{ID: "file", ParentID: "root", IsDir: true}, RelativePath: "/TV"}}, Files: []medialibrary.File{{ProviderID: "a", RelativePath: "/TV/S02/01.mkv"}, {ProviderID: "moved", RelativePath: "/TV/S09/01.mkv"}, {ProviderID: "other-version", RelativePath: "/Other/01.mkv"}}}
	run := models.MediaLibraryScanRun{ID: 100, LibraryID: lib.ID, StartedAt: observed}
	if err := stageProviderPaths(context.Background(), db, lib, storage, result, run); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return persistProviderPathsTx(tx, lib, storage, result, observed, run.ID) }); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []models.MediaLibraryEntry{{LibraryID: lib.ID, ProviderID: "a", RelativePath: "/TV/S02/01.mkv"}, {LibraryID: lib.ID, ProviderID: "moved", RelativePath: "/Outside/01.mkv"}, {LibraryID: lib.ID, ProviderID: "other-version", RelativePath: "/Other/01.mkv"}} {
		if err := db.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
	}
	payload, _ := decodeProviderEventPayload(row.PayloadJSON)
	delta, reason, err := s.prepareLocalProviderDeletion(context.Background(), lib, storage, row, payload, nil)
	if err != nil || reason != "" || len(delta.DeletedProviderIDs) != 1 || delta.DeletedProviderIDs[0] != "a" {
		t.Fatalf("%+v %s %v", delta, reason, err)
	}
}

func TestProviderPathsPublicationDoesNotFalselyResolveParkedHints(t *testing.T) {
	_, db, lib, storage, row, _ := deletionFixture(t)
	if err := db.Model(&row).Update("resolution_code", providerEventNeedsReview).Error; err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Add(time.Second)
	run := models.MediaLibraryScanRun{ID: 100, LibraryID: lib.ID, StartedAt: observed}
	if err := stageProviderPaths(context.Background(), db, lib, storage, medialibrary.Result{}, run); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return persistProviderPathsTx(tx, lib, storage, medialibrary.Result{Partial: true}, observed, run.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row, row.ID).Error; err != nil || row.ProcessedAt != nil {
		t.Fatal("partial scan acknowledged event", err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return persistProviderPathsTx(tx, lib, storage, medialibrary.Result{}, observed, run.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row, row.ID).Error; err != nil || row.ProcessedAt != nil || row.ResolutionCode != providerEventNeedsReview {
		t.Fatalf("%+v %v", row, err)
	}
}

func TestProviderDeletionAmbiguousSameSecondOrderParks(t *testing.T) {
	s, db, lib, _, row, inbox := deletionFixture(t)
	if err := db.Create(&models.MediaLibraryEntry{LibraryID: lib.ID, ProviderID: "file", RelativePath: "/Any.mkv"}).Error; err != nil {
		t.Fatal(err)
	}
	other := inbox
	other.ID = 0
	other.ProviderEventID = "unordered"
	other.Kind = cloudpkg.ChangeCreated
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	prepared, err := s.prepareProviderDeliveryPage(context.Background(), lib.ID, providerChangeScope{DeliveryIDs: []uint{row.ID}})
	if err != nil || !prepared.empty() {
		t.Fatal("ambiguous event acknowledged", err)
	}
	if err := db.First(&row, row.ID).Error; err != nil || row.ProcessedAt != nil || row.ResolutionReason != "deletion_event_order_unproven" {
		t.Fatalf("%+v %v", row, err)
	}
}

type deletionEvidenceTreeDriver struct{ *fakeCloudDriver }

func (d *deletionEvidenceTreeDriver) StreamTree(_ context.Context, _ string, _ int, emit func(cloudpkg.TreeBatch) error) error {
	return emit(cloudpkg.TreeBatch{Total: 1, Directories: []cloudpkg.TreeEntry{{Item: cloudpkg.Item{ID: "show", ParentID: "root", Name: "Show", IsDir: true}, RelativePath: "/Show"}, {Item: cloudpkg.Item{ID: "s9", ParentID: "show", Name: "Season09", IsDir: true}, RelativePath: "/Show/Season09"}}, Entries: []cloudpkg.TreeEntry{{Item: cloudpkg.Item{ID: "episode", ParentID: "s9", Name: "Show.S09E01.mkv", Size: 12}, RelativePath: "/Show/Season09/Show.S09E01.mkv"}}})
}

func TestProviderPathsRealScanPublicationStoresDirectories(t *testing.T) {
	s, db, lib, _, _, _ := deletionFixture(t)
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", lib.ID).Update("recursive", true).Error; err != nil {
		t.Fatal(err)
	}
	driver := &deletionEvidenceTreeDriver{fakeCloudDriver: &fakeCloudDriver{}}
	s.backends.Register(pan115MediaLibraryBackend{driver: func(uint) (cloudpkg.Driver, error) { return driver, nil }})
	run, err := s.reconcile(context.Background(), lib.ID, "full")
	if err != nil || run.CatalogPublishedAt == nil {
		t.Fatalf("scan=%+v err=%v", run, err)
	}
	var paths []models.MediaLibraryProviderPath
	if err := db.Where("library_id = ?", lib.ID).Order("provider_id").Find(&paths).Error; err != nil || len(paths) != 3 {
		t.Fatalf("paths=%+v err=%v", paths, err)
	}
	if paths[0].ProviderID != "episode" || paths[0].IsDir || paths[1].ProviderID != "s9" || !paths[1].IsDir || paths[1].RelativePath != "/Show/Season09" {
		t.Fatalf("paths=%+v", paths)
	}
}
