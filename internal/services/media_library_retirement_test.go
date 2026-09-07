package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func retirementFixture(t *testing.T) (*MediaLibraryService, models.MediaLibrary, Actor) {
	t.Helper()
	store, library, recognition, entries := catalogFixture(t)
	catalogConvert(t, store, library, recognition, entries)
	s := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
	s.SetCatalogSnapshotStore(store)
	s.SetQueueService(NewQueueService(store.writeDB, s.audit))
	s.SetRetirementPhysicalGuard(AssertCatalogPhysicalDrainedTx)
	var user models.User
	if err := s.db.Where("username=?", "library-test").First(&user).Error; err != nil {
		t.Fatal(err)
	}
	actor := Actor{User: user, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesDelete: {}, authz.PermissionMediaLibrariesRead: {}, authz.PermissionMediaLibrariesUpdate: {}, authz.PermissionMediaLibrariesScan: {}}}
	return s, library, actor
}

func claimRetirement(t *testing.T, s *MediaLibraryService, library models.MediaLibrary, actor Actor) (ClaimedJob, models.MediaLibraryRetirement) {
	t.Helper()
	result, err := s.DeleteRequest(context.Background(), actor, library.ID, RequestContext{})
	if err != nil || result.Deleted || result.Status != "deleting" || result.JobID == "" {
		t.Fatalf("accept: %+v %v", result, err)
	}
	again, err := s.DeleteRequest(context.Background(), actor, library.ID, RequestContext{})
	if err != nil || again != result {
		t.Fatalf("idempotence: %+v %v", again, err)
	}
	claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRetirement})
	if err != nil || claim == nil {
		t.Fatalf("claim %v", err)
	}
	var row models.MediaLibraryRetirement
	if err := s.db.Where("job_id=?", claim.Job.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	return *claim, row
}

func TestLibraryRetirementPreservesFilesHistoryAndOtherLibraryAcrossRestart(t *testing.T) {
	s, library, actor := retirementFixture(t)
	var storage models.Storage
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(storage.RootPath, "source.mkv")
	if err := os.WriteFile(file, []byte("unchanged-source"), 0600); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(storage.RootPath, "source.nfo")
	if err := os.WriteFile(artifact, []byte("unchanged-artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	other := library
	other.ID = 0
	other.Name = "Other library"
	other.NameNormalized = "other library"
	other.RelativeRoot = "/other"
	if err := s.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint{library.ID, other.ID} {
		if err := s.db.Create(&models.PlayerMediaFavorite{UserID: actor.User.ID, LibraryID: id, WorkKey: "work"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	collection := models.PlayerMediaCollection{ID: "manual-keep", OwnerID: &actor.User.ID, Source: "manual", Kind: "collection", Name: "Keep collection"}
	if err := s.db.Create(&collection).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint{library.ID, other.ID} {
		if err := s.db.Create(&models.PlayerMediaCollectionItem{CollectionID: collection.ID, LibraryID: id, WorkKey: "work", Origin: "manual"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for i, kind := range []string{"server", "emby"} {
		if err := s.db.Create(&models.PlayerPlaybackHistory{UserID: actor.User.ID, SyncKey: fmt.Sprintf("history-%d", i), SourceKind: kind, SourceID: "source", MediaIdentity: "identity", Title: "history", ClientUpdatedAt: 1}).Error; err != nil {
			t.Fatal(err)
		}
	}
	claim, row := claimRetirement(t, s, library, actor)
	if err := s.Delete(actor, library.ID, RequestContext{}); err == nil {
		t.Fatal("legacy Delete must not claim retired library deleted")
	}
	detail, err := s.Get(actor, library.ID)
	if err != nil || detail.Retirement == nil || detail.Enabled {
		t.Fatalf("deleting detail: %+v %v", detail, err)
	}
	if err := s.catalogStore.Read(context.Background(), []uint{library.ID}, func(*CatalogReader) error { return nil }); err == nil {
		t.Fatal("retired catalog still readable")
	}
	for step := 0; step < 200; step++ {
		// Recreate worker every step: all state belongs to durable rows.
		w := NewMediaLibraryRetirementWorker(s)
		waiting, done, err := w.step(context.Background(), claim, &row)
		if err != nil || waiting {
			t.Fatalf("step %d phase %s: %v waiting=%v", step, row.Phase, err, waiting)
		}
		if done {
			break
		}
		if step == 199 {
			t.Fatal("did not converge")
		}
	}
	if row.Phase != "completed" {
		t.Fatalf("phase %s", row.Phase)
	}
	result, err := s.DeleteRequest(context.Background(), actor, library.ID, RequestContext{})
	if err != nil || !result.Deleted {
		t.Fatalf("completed receipt %+v %v", result, err)
	}
	for _, table := range []string{"player_media_favorites", "player_media_collection_items", "player_media_collections"} {
		var n int64
		if err := s.db.Table(table).Count(&n).Error; err != nil || n != 1 {
			t.Fatalf("%s=%d %v", table, n, err)
		}
	}
	var n int64
	if err := s.db.Model(&models.PlayerPlaybackHistory{}).Count(&n).Error; err != nil || n != 2 {
		t.Fatalf("history %d %v", n, err)
	}
	for path, want := range map[string]string{file: "unchanged-source", artifact: "unchanged-artifact"} {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != want {
			t.Fatalf("file changed %v", err)
		}
	}
	var fk []map[string]any
	if err := s.db.Raw("PRAGMA foreign_key_check").Scan(&fk).Error; err != nil || len(fk) != 0 {
		t.Fatalf("fk %v %v", fk, err)
	}
}

func TestLibraryRetirementRefusalRollsBackEnabledHeadAndQueue(t *testing.T) {
	s, library, actor := retirementFixture(t)
	if err := s.db.Model(&library).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	var before models.CatalogHead
	s.db.First(&before, "library_id=?", library.ID)
	s.SetRetirementPhysicalGuard(func(*gorm.DB, uint) error { return appError(CodeConflict, "unfinished physical owner", nil) })
	_, err := s.DeleteRequest(context.Background(), actor, library.ID, RequestContext{})
	if err == nil {
		t.Fatal("unsafe owner admitted")
	}
	var after models.MediaLibrary
	s.db.First(&after, library.ID)
	if !after.Enabled {
		t.Fatal("refusal disabled original owner")
	}
	var head models.CatalogHead
	s.db.First(&head, "library_id=?", library.ID)
	if head != before {
		t.Fatal("refusal changed head")
	}
	var n int64
	s.db.Model(&models.MediaLibraryRetirement{}).Count(&n)
	if n != 0 {
		t.Fatal("gate survived rollback")
	}
	s.db.Model(&models.Job{}).Where("job_type=?", JobTypeMediaLibraryRetirement).Count(&n)
	if n != 0 {
		t.Fatal("job survived rollback")
	}
}

func TestLibraryRetirementLegacySynchronousAndUnknownReferenceRefused(t *testing.T) {
	legacy, library, actor := createCatalogTestLibrary(t)
	result, err := legacy.DeleteRequest(context.Background(), actor, library.ID, RequestContext{})
	if err != nil || !result.Deleted || result.JobID != "" {
		t.Fatalf("legacy %+v %v", result, err)
	}
	s, library, actor := retirementFixture(t)
	var layer models.CatalogHeadLayer
	s.db.First(&layer, "library_id=?", library.ID)
	if err := s.db.Create(&models.CatalogSnapshotReference{SnapshotID: layer.SnapshotID, OwnerKind: "diagnosis", OwnerID: "unknown-owner", CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	claim, row := claimRetirement(t, s, library, actor)
	w := NewMediaLibraryRetirementWorker(s)
	for i := 0; i < 4; i++ {
		_, _, err = w.step(context.Background(), claim, &row)
		if err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("unproven ref removed")
	}
	var n int64
	s.db.Model(&models.CatalogSnapshotReference{}).Where("owner_id=?", "unknown-owner").Count(&n)
	if n != 1 {
		t.Fatal("unproven ref lost")
	}
	raw, _ := json.Marshal(retirementSummary(row))
	for _, private := range []string{"source_epoch", "fingerprint", "cursor", "owner"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private receipt exposed")
		}
	}
}

func TestLibraryRetirementCleanupSchemaInventory(t *testing.T) {
	s, _, _ := createCatalogTestLibrary(t)
	// Negative stages are exact reference release and shared snapshot GC before
	// derived cleanup. Positive stages use the actual bounded cleanup plan.
	order := map[string]int{"catalog_snapshot_references": -4, "catalog_head_layers": -3, "catalog_entry_facts": -2, "catalog_recognition_facts": -2, "catalog_source_asset_facts": -2, "catalog_collection_member_facts": -2, "catalog_collection_preparations": -2, "catalog_conversion_manifests": -2, "catalog_snapshots": -1, "catalog_heads": 1000, "media_libraries": 1001}
	for i, step := range libraryRetirementCleanup {
		if step.update == "" {
			order[step.table] = i
		}
	}
	var tables []string
	if err := s.db.Raw("SELECT name FROM sqlite_master WHERE type='table'").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		var refs []struct {
			Table string
			From  string
		}
		if err := s.db.Raw("PRAGMA foreign_key_list(\"" + strings.ReplaceAll(table, "\"", "\"\"") + "\")").Scan(&refs).Error; err != nil {
			t.Fatal(err)
		}
		for _, ref := range refs {
			parentOrder, removedParent := order[ref.Table]
			if !removedParent {
				continue
			}
			childOrder, reviewedChild := order[table]
			if table == "media_acquisitions" && ref.Table == "media_libraries" {
				continue
			} // explicitly bounded SET NULL
			if !reviewedChild {
				t.Errorf("unreviewed retirement FK %s.%s -> %s", table, ref.From, ref.Table)
			} else if childOrder >= parentOrder {
				t.Errorf("retirement child %s must precede parent %s", table, ref.Table)
			}
		}
	}
}

func TestLibraryRetirementBatchBoundAndLeaseFence(t *testing.T) {
	s, library, actor := retirementFixture(t)
	issue := models.MediaLibraryStructureIssue{Token: "large-group", LibraryID: library.ID, DiagnosisJobID: "test", Generation: 1, Code: "duplicate_target", Kind: "video", State: "pending"}
	if err := s.db.Create(&issue).Error; err != nil {
		t.Fatal(err)
	}
	// One issue with many members must not be removed using hidden CASCADE.
	for offset := 0; offset < 1000; offset += 250 {
		members := make([]models.MediaLibraryStructureIssueMember, 250)
		for i := range members {
			members[i] = models.MediaLibraryStructureIssueMember{IssueID: issue.ID, Token: fmt.Sprintf("member-%d", offset+i), SourcePath: "sample"}
		}
		if err := s.db.CreateInBatches(members, 250).Error; err != nil {
			t.Fatal(err)
		}
	}
	claim, row := claimRetirement(t, s, library, actor)
	w := NewMediaLibraryRetirementWorker(s)
	for i := 0; i < len(libraryRetirementCleanup)*2+16 && row.Phase != "completed"; i++ {
		var before, after int64
		s.db.Model(&models.MediaLibraryStructureIssueMember{}).Count(&before)
		_, _, err := w.step(context.Background(), claim, &row)
		if err != nil {
			t.Fatal(err)
		}
		s.db.Model(&models.MediaLibraryStructureIssueMember{}).Count(&after)
		if before-after > 250 {
			t.Fatalf("unbounded member deletion %d", before-after)
		}
	}
	if row.Phase != "completed" {
		t.Fatalf("phase %s", row.Phase)
	}
	bad := claim
	bad.LeaseToken = "wrong-lease"
	_, _, err := w.step(context.Background(), bad, &row)
	if err == nil || errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("missing lease fence %v", err)
	}
}

func TestLibraryRetirementCancelsOnlyProvenUnenteredRepairAndBlocksNewAdmission(t *testing.T) {
	s, library, actor := retirementFixture(t)
	var layer models.CatalogHeadLayer
	if err := s.db.First(&layer, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	job, err := s.queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: JobTypeMediaLibraryRepair, DisplayName: "unentered repair", ResourceKey: "library:" + uintID(library.ID), Payload: map[string]any{"repair_id": "retirement-safe-repair"}}, func(tx *gorm.DB, job models.Job) error {
		repair := models.MediaLibraryStructureRepair{ID: "retirement-safe-repair", LibraryID: library.ID, OwnerID: actor.User.ID, JobID: &job.ID, Scope: models.MediaLibraryStructureScopeFull, Phase: "queued", PlanJSON: "{}", RuleFingerprint: "test", Generation: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		if err := RegisterCatalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: repair.ID}, func(tx *gorm.DB) error { return tx.Create(&repair).Error }); err != nil {
			return err
		}
		return AcquireCatalogReferenceTx(tx, layer.SnapshotID, "repair", repair.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, row := claimRetirement(t, s, library, actor)
	if err := s.db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalAdmissionTx(tx, library.ID) }); err == nil {
		t.Fatal("new physical admission survived retirement gate")
	}
	w := NewMediaLibraryRetirementWorker(s)
	for i := 0; i < 80 && row.Phase != "completed"; i++ {
		if _, _, err := w.step(context.Background(), claim, &row); err != nil {
			t.Fatal(err)
		}
	}
	if row.Phase != "completed" {
		t.Fatal("retirement did not converge")
	}
	var kept models.Job
	if err := s.db.First(&kept, "id=?", job.ID).Error; err != nil || kept.Status != models.JobStatusCancelled || kept.LeaseTokenHash != "" {
		t.Fatalf("safe queued job not cancelled or history lost %+v %v", kept, err)
	}
}
