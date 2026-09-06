package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

func deletionSnapshotStore(t *testing.T, db *gorm.DB, library models.MediaLibrary, entries []models.MediaLibraryEntry, assets []models.MediaLibrarySourceAsset) *CatalogSnapshotStore {
	t.Helper()
	var location struct{ File string }
	if err := db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	read, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := read.DB()
	t.Cleanup(func() { _ = sql.Close() })
	store := NewCatalogSnapshotStore(db, read)
	if err := db.Transaction(func(tx *gorm.DB) error {
		return StartCatalogConversionTx(tx, models.CatalogHead{LibraryID: library.ID, SourceEpoch: 1, SourceFingerprint: "source-a", ConfigFingerprint: "config-a"}, func(*gorm.DB) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	c, token := catalogCandidate(t, store, library, "base", 0)
	requests := make([]CatalogIdentityRequest, 0, len(entries)+len(assets))
	batch := CatalogFactBatch{}
	for _, e := range entries {
		requests = append(requests, CatalogIdentityRequest{Kind: "entry", SourceKey: e.RelativePath, ExistingID: e.ID})
		batch.Entries = append(batch.Entries, CatalogEntryFromLegacy(e, nil))
	}
	for _, a := range assets {
		requests = append(requests, CatalogIdentityRequest{Kind: "asset", SourceKey: a.RelativePath, ExistingID: a.ID})
		batch.SourceAssets = append(batch.SourceAssets, models.CatalogSourceAssetFact{MediaLibrarySourceAsset: a})
	}
	if _, err := store.ResolveIdentities(context.Background(), c.ID, token, requests); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendBatch(context.Background(), c.ID, token, batch); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	return store
}

func versionedCloudDeletion(t *testing.T, n int) (pan115CatalogDeletionFixture, *CatalogSnapshotStore) {
	t.Helper()
	f := newPan115CatalogDeletionFixture(t, n)
	store := deletionSnapshotStore(t, f.service.db, f.library, f.entries, nil)
	f.service.SetCatalogSnapshotStore(store)
	return f, store
}

func TestCatalogDeletionVersionedPhysicalFirstAndImmutableAnchors(t *testing.T) {
	f, store := versionedCloudDeletion(t, 2)
	preview := f.preview(t)
	var before []models.MediaLibraryEntry
	if err := store.writeDB.Order("id").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	f.driver.recycleFailID = f.entries[1].ProviderID
	if _, err := f.service.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, f.work, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeMediaCatalogDeletionPartial {
		t.Fatalf("physical failure=%v", err)
	}
	if rows := catalogReadEntries(t, store, f.library.ID, ""); len(rows) != 2 {
		t.Fatal("failed physical operation published tombstones")
	}
	f.driver.recycleFailID = ""
	result, err := f.service.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, f.work, preview.ConfirmationToken, RequestContext{})
	if err != nil || !result.Deleted || result.RemovedFiles != 2 {
		t.Fatalf("result=%+v %v", result, err)
	}
	if len(f.driver.recycled) != 2 {
		t.Fatalf("replayed recycle: %v", f.driver.recycled)
	}
	if rows := catalogReadEntries(t, store, f.library.ID, ""); len(rows) != 0 {
		t.Fatal("deleted entries remain effective")
	}
	var after []models.MediaLibraryEntry
	if err := store.writeDB.Order("id").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("versioned deletion mutated anchors")
	}
}

func TestCatalogDeletionCommitFailureRetainsIdentityCheckpoint(t *testing.T) {
	f, store := versionedCloudDeletion(t, 1)
	preview := f.preview(t)
	// Only the final publication marker is rejected, after physical completion.
	if err := store.writeDB.Exec("CREATE TRIGGER reject_delete_finalize BEFORE UPDATE OF consumed_at ON media_catalog_deletion_previews BEGIN SELECT RAISE(ABORT,'fixture'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, f.work, preview.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeMediaCatalogDeletionPartial {
		t.Fatalf("finalize failure=%v", err)
	}
	if len(catalogReadEntries(t, store, f.library.ID, "")) != 1 {
		t.Fatal("head leaked across rollback")
	}
	if err := store.writeDB.Exec("DROP TRIGGER reject_delete_finalize").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, f.work, preview.ConfirmationToken, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if len(f.driver.recycled) != 1 {
		t.Fatal("completed physical delete repeated")
	}
}

func TestCatalogDeletionFenceAndCompaction(t *testing.T) {
	for _, kind := range []string{"logical", "source", "config", "compaction", "legacy_preview"} {
		t.Run(kind, func(t *testing.T) {
			f, store := versionedCloudDeletion(t, 1)
			preview := f.preview(t)
			switch kind {
			case "logical":
				if err := store.writeDB.Model(&models.MediaLibrary{}).Where("id=?", f.library.ID).Update("content_revision", gorm.Expr("content_revision+1")).Error; err != nil {
					t.Fatal(err)
				}
			case "source":
				if err := store.writeDB.Model(&models.MediaLibrary{}).Where("id=?", f.library.ID).Update("relative_root", "another-root").Error; err != nil {
					t.Fatal(err)
				}
			case "config":
				if err := store.writeDB.Model(&models.CatalogHead{}).Where("library_id=?", f.library.ID).Update("config_fingerprint", "new-config").Error; err != nil {
					t.Fatal(err)
				}
			case "compaction":
				if ok, err := store.Compact(context.Background(), CatalogCompactionInput{LibraryID: f.library.ID, Force: true}); err != nil || !ok {
					t.Fatalf("compact %v %v", ok, err)
				}
			case "legacy_preview":
				if err := store.writeDB.Exec("UPDATE media_catalog_deletion_previews SET snapshot_json=json_remove(json_set(snapshot_json,'$.version',2),'$.catalog_fence')").Error; err != nil {
					t.Fatal(err)
				}
			}
			_, err := f.service.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, f.work, preview.ConfirmationToken, RequestContext{})
			if kind == "compaction" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || len(f.driver.recycled) != 0 {
				t.Fatalf("stale preview deleted: %v %v", err, f.driver.recycled)
			}
		})
	}
}

func TestCatalogDeletionBudgetBeforePhysicalMutation(t *testing.T) {
	f, store := versionedCloudDeletion(t, 1)
	c, token := catalogCandidate(t, store, f.library, "delta", 1)
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(f.entries[0], nil)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	if err := store.writeDB.Model(&models.CatalogSnapshot{}).Where("id=?", c.ID).Update("row_count", CatalogMaxDeltaRows).Error; err != nil {
		t.Fatal(err)
	}
	p := f.preview(t)
	if _, err := f.service.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, f.work, p.ConfirmationToken, RequestContext{}); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("budget err=%v", err)
	}
	if len(f.driver.recycled) != 0 {
		t.Fatal("budget refusal occurred after deletion")
	}
}

func TestCatalogDeletionExactIDsAndVersionPreservation(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	catalogConvert(t, store, library, rec, entries)
	svc := &MediaLibraryService{db: store.writeDB, catalogStore: store}
	for _, kind := range []string{"foreign", "missing", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			w, err := svc.captureCatalogDeletion(context.Background(), library.ID, "", []string{entries[0].RelativePath})
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "foreign":
				w.Entries[0].LibraryID++
			case "missing":
				w.Entries[0].ID += 10000
			case "duplicate":
				w.Entries = append(w.Entries, w.Entries[0])
			}
			if err := svc.prepareCatalogDeletion(context.Background(), &w); err == nil {
				t.Fatal("accepted invalid exact identity")
			}
			svc.abandonCatalogDeletion(&w)
		})
	}
	w, err := svc.captureCatalogDeletion(context.Background(), library.ID, "", []string{entries[0].RelativePath})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.abandonCatalogDeletion(&w)
	if err := svc.prepareCatalogDeletion(context.Background(), &w); err != nil {
		t.Fatal(err)
	}
	if err := svc.commitCatalogDeletion(context.Background(), &w, func(*gorm.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rows := catalogReadEntries(t, store, library.ID, "")
	if len(rows) != 1 || rows[0].ID != entries[1].ID || *rows[0].Season != 2 || *rows[0].Episode != 1 {
		t.Fatalf("other version lost: %+v", rows)
	}
	if err := store.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error {
		var count int64
		if err := r.Recognitions().Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			t.Fatal("shared recognition tombstoned")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	last, err := svc.captureCatalogDeletion(context.Background(), library.ID, "", []string{entries[1].RelativePath})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.abandonCatalogDeletion(&last)
	if err := svc.prepareCatalogDeletion(context.Background(), &last); err != nil {
		t.Fatal(err)
	}
	if err := svc.commitCatalogDeletion(context.Background(), &last, func(*gorm.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error {
		var count int64
		if err := r.Recognitions().Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("orphan recognition not tombstoned")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var anchor models.MediaLibraryRecognition
	if err := store.writeDB.First(&anchor, rec.ID).Error; err != nil {
		t.Fatal("recognition anchor deleted", err)
	}
}

func TestTransferDeletionVersionedPreservesSourceAndAnchors(t *testing.T) {
	queue, actor, service, _, transfer, source, destination := completedTransferForDeletion(t)
	var library models.MediaLibrary
	var managed models.MediaManagedItem
	if err := queue.db.First(&library, transfer.LibraryID).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&managed, "transfer_task_id=?", transfer.ID).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: managed.RelativePath, Size: managed.Size, WorkKey: "movie:delete", Title: "Delete", MediaType: "movie", MatchStatus: "unrecognized", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	sidecarPath := filepath.Join(filepath.Dir(destination), "delete.ass")
	if err := os.WriteFile(sidecarPath, []byte("subtitle"), 0600); err != nil {
		t.Fatal(err)
	}
	sidecarRelative := filepath.ToSlash(filepath.Join(filepath.Dir(managed.RelativePath), "delete.ass"))
	asset := models.MediaLibrarySourceAsset{LibraryID: library.ID, RelativePath: sidecarRelative, Name: "delete.ass", Size: 8, Active: true}
	if err := queue.db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	sidecar := managed
	sidecar.ID = 0
	sidecar.OpaqueID = "deletion-sidecar"
	sidecar.RelativePath = sidecarRelative
	sidecar.Kind = models.MediaManagedItemKindSidecar
	sidecar.Size = 8
	if err := queue.db.Create(&sidecar).Error; err != nil {
		t.Fatal(err)
	}
	store := deletionSnapshotStore(t, queue.db, library, []models.MediaLibraryEntry{entry}, []models.MediaLibrarySourceAsset{asset})
	service.SetCatalogSnapshotStore(store)
	service.SetMediaChangeService(NewMediaChangeService(queue.db))
	p, err := service.PreviewDeletion(context.Background(), actor, transfer.ID, TransferDeletionPreviewInput{Scope: models.TransferDeletionScopeRecordAndLibrary}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmDeletion(context.Background(), actor, transfer.ID, p.ConfirmationToken, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("source deleted", err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatal("destination remains", err)
	}
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Fatal("sidecar remains", err)
	}
	if err := store.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error {
		var count int64
		if err := r.SourceAssets().Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("source asset remains")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(catalogReadEntries(t, store, library.ID, "")) != 0 {
		t.Fatal("catalog entry remains")
	}
	var anchor models.MediaLibraryEntry
	if err := queue.db.First(&anchor, entry.ID).Error; err != nil {
		t.Fatal("anchor deleted", err)
	}
	var assetAnchor models.MediaLibrarySourceAsset
	if err := queue.db.First(&assetAnchor, asset.ID).Error; err != nil {
		t.Fatal("asset anchor removed", err)
	}
}

func TestCatalogDeletionPreparedCandidateSurvivesCompaction(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	catalogConvert(t, store, library, rec, entries)
	svc := &MediaLibraryService{db: store.writeDB, catalogStore: store}
	w, err := svc.captureCatalogDeletion(context.Background(), library.ID, "", []string{entries[0].RelativePath})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.abandonCatalogDeletion(&w)
	if err := svc.prepareCatalogDeletion(context.Background(), &w); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Compact(context.Background(), CatalogCompactionInput{LibraryID: library.ID, Force: true}); err != nil {
		t.Fatal(err)
	}
	if err := svc.commitCatalogDeletion(context.Background(), &w, func(*gorm.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if rows := catalogReadEntries(t, store, library.ID, ""); len(rows) != 1 || rows[0].ID != entries[1].ID {
		t.Fatalf("unexpected remainder %+v", rows)
	}
}

func TestCatalogDeletionPreparedCandidateRejectsLogicalAndSourceRaces(t *testing.T) {
	for _, kind := range []string{"source_epoch", "logical", "physical_identity"} {
		t.Run(kind, func(t *testing.T) {
			store, library, rec, entries := catalogFixture(t)
			catalogConvert(t, store, library, rec, entries)
			svc := &MediaLibraryService{db: store.writeDB, catalogStore: store}
			w, err := svc.captureCatalogDeletion(context.Background(), library.ID, "", []string{entries[0].RelativePath})
			if err != nil {
				t.Fatal(err)
			}
			defer svc.abandonCatalogDeletion(&w)
			if err := svc.prepareCatalogDeletion(context.Background(), &w); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "source_epoch":
				err = store.writeDB.Model(&models.CatalogHead{}).Where("library_id=?", library.ID).Update("source_epoch", 2).Error
			case "logical":
				err = store.writeDB.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Update("content_revision", gorm.Expr("content_revision+1")).Error
			case "physical_identity":
				c, token := catalogCandidate(t, store, library, "delta", 1)
				e := entries[0]
				e.ProviderID = "replacement"
				err = store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(e, &rec)}})
				if err == nil {
					catalogPublish(t, store, c, token)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			called := false
			if err := svc.commitCatalogDeletion(context.Background(), &w, func(*gorm.DB) error { called = true; return nil }); err == nil || called {
				t.Fatalf("stale candidate committed %v callback=%v", err, called)
			}
			var count int64
			if err := store.writeDB.Model(&models.MediaLibraryEntry{}).Where("library_id=?", library.ID).Count(&count).Error; err != nil || count != 2 {
				t.Fatalf("anchors lost %d %v", count, err)
			}
		})
	}
}

func TestTransferDeletionVersionedPartialRetainsCompleteManifestForRetry(t *testing.T) {
	f := newCloudTransferFixture(t, models.MediaLibraryTransferCopy, models.MediaLibraryConflictRename, false)
	if result := runCompletedDeletionTransfer(t, f); result.ErrorCode != "" {
		t.Fatal(result)
	}
	var transfer models.TransferTask
	var first models.MediaManagedItem
	if err := f.queue.db.First(&transfer, "download_task_id=?", f.download.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.First(&first, "transfer_task_id=?", transfer.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Model(&models.Job{}).Where("id=?", transfer.JobID).Update("status", models.JobStatusCompleted).Error; err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID = 0
	second.OpaqueID = "second-deletion-version"
	second.RelativePath = "Second.mkv"
	second.ProviderItemID = "second-deletion-version"
	second.ProviderParentID = "library-root"
	second.Size = 20
	f.driver.items[second.ProviderItemID] = cloudpkg.Item{ID: second.ProviderItemID, ParentID: "library-root", Name: "Second.mkv", Size: 20}
	if err := f.queue.db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	entries := []models.MediaLibraryEntry{}
	for _, m := range []models.MediaManagedItem{first, second} {
		entries = append(entries, models.MediaLibraryEntry{LibraryID: f.library.ID, RelativePath: m.RelativePath, ProviderID: m.ProviderItemID, Size: m.Size, WorkKey: "movie:transfer-delete", Title: "Delete", MediaType: "movie", MatchStatus: "unrecognized"})
	}
	if err := f.queue.db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	store := deletionSnapshotStore(t, f.queue.db, f.library, entries, nil)
	f.service.SetCatalogSnapshotStore(store)
	actor := Actor{User: models.User{ID: f.download.OwnerID}, Permissions: map[string]struct{}{authz.PermissionJobsControlAll: {}}}
	p, err := f.service.PreviewDeletion(context.Background(), actor, transfer.ID, TransferDeletionPreviewInput{Scope: models.TransferDeletionScopeRecordAndLibrary}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	f.driver.recycleFailID = second.ProviderItemID
	if _, err := f.service.ConfirmDeletion(context.Background(), actor, transfer.ID, p.ConfirmationToken, RequestContext{}); ErrorCode(err) != CodeTransferDeletionPartial {
		t.Fatalf("partial error %v", err)
	}
	if len(catalogReadEntries(t, store, f.library.ID, "")) != 2 {
		t.Fatal("partial publication")
	}
	var active int64
	if err := f.queue.db.Model(&models.MediaManagedItem{}).Where("transfer_task_id=? AND active=?", transfer.ID, true).Count(&active).Error; err != nil || active != 2 {
		t.Fatalf("lost complete recovery manifest %d %v", active, err)
	}
	f.driver.recycleFailID = ""
	p, err = f.service.PreviewDeletion(context.Background(), actor, transfer.ID, TransferDeletionPreviewInput{Scope: models.TransferDeletionScopeRecordAndLibrary}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if p.LibraryMissing != 1 {
		t.Fatalf("missing progress %+v", p)
	}
	if _, err := f.service.ConfirmDeletion(context.Background(), actor, transfer.ID, p.ConfirmationToken, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if len(catalogReadEntries(t, store, f.library.ID, "")) != 0 {
		t.Fatal("completed earlier file resurrected")
	}
	if _, ok := f.driver.items[f.sourceID]; !ok {
		t.Fatal("source was deleted")
	}
}

func TestCatalogDeletionVersionedKeepsPendingArtifactBindingOnQueueFailure(t *testing.T) {
	f, store := versionedCloudDeletion(t, 1)
	if err := store.writeDB.Model(&models.MediaLibrary{}).Where("id=?", f.library.ID).Updates(map[string]any{"strm_enabled": true, "signed_proxy_enabled": true, "dirty_generation": uint64(9), "artifact_generation": uint64(3)}).Error; err != nil {
		t.Fatal(err)
	}
	artifacts := NewMediaArtifactService(store.writeDB, nil, nil, zerolog.Nop()) // intentional unavailable queue
	artifacts.SetCatalogSnapshotStore(store)
	f.service.SetArtifactService(artifacts)
	f.service.SetMediaChangeService(NewMediaChangeService(store.writeDB))
	p := f.preview(t)
	if _, err := f.service.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, f.work, p.ConfirmationToken, RequestContext{}); err != nil {
		t.Fatal("postcommit enqueue failure made deletion fail", err)
	}
	var library models.MediaLibrary
	var change models.MediaLibraryChange
	var binding models.CatalogArtifactBinding
	if err := store.writeDB.First(&library, f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.First(&change, "library_id=?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.First(&binding, "library_id=?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if library.ArtifactGeneration != 10 || change.Generation != 10 || binding.Generation != 10 || change.State != models.MediaLibraryChangePending || binding.State != "pending" || binding.HeadRevision != 2 {
		t.Fatalf("generation/readiness mismatch library=%+v change=%+v binding=%+v", library, change, binding)
	}
	if len(catalogReadEntries(t, store, f.library.ID, "")) != 0 {
		t.Fatal("pending artifact hid successful logical deletion")
	}
}
