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

func TestDeleteRequestAcceptsHistoricalBlockerWithoutStartingFollowupWork(t *testing.T) {
	s, library, actor := createCatalogTestLibrary(t)
	s.SetQueueService(NewQueueService(s.db, s.audit))
	if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	blocker := models.MediaArtifactRun{ID: "legacy-delete-blocker", LibraryID: library.ID, Generation: 1, PolicyJSON: `{}`, Status: models.MediaArtifactStatusFailed, CleanupStatus: models.MediaArtifactCleanupPending, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&blocker).Error; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { <-ctx.Done(); close(done) }()
	s.mu.Lock()
	s.supervisors[library.ID] = supervisorHandle{cancel: cancel, done: done, wake: make(chan struct{}, 1), pending: newProviderChangeAccumulator()}
	s.mu.Unlock()
	defer func() {
		cancel()
		<-done
	}()

	result, err := s.DeleteRequest(context.Background(), actor, library.ID, RequestContext{})
	if err != nil || result.Deleted || result.JobID == "" {
		t.Fatalf("durable retirement was not accepted: result=%+v err=%v", result, err)
	}
	select {
	case <-ctx.Done():
		t.Fatal("predictable delete conflict stopped the supervisor")
	default:
	}
	var jobs int64
	if err := s.db.Model(&models.Job{}).Where("job_type <> ?", JobTypeMediaLibraryRetirement).Count(&jobs).Error; err != nil || jobs != 0 {
		t.Fatalf("delete acceptance created follow-up work: count=%d err=%v", jobs, err)
	}
}

func TestHeadlessLibraryWithLargeHistoryUsesBoundedRetirement(t *testing.T) {
	s, library, actor := createCatalogTestLibrary(t)
	queue := NewQueueService(s.db, s.audit)
	s.SetQueueService(queue)
	now := time.Now().UTC()
	if err := s.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{
		"artifact_generation": 3, "artifact_applied_generation": 3, "artifact_status": models.MediaArtifactStatusCompleted,
	}).Error; err != nil {
		t.Fatal(err)
	}
	runs := make([]models.MediaArtifactRun, 0, CatalogBatchRows+7)
	for i := 0; i < CatalogBatchRows+7; i++ {
		runs = append(runs, models.MediaArtifactRun{
			ID: fmt.Sprintf("headless-history-%03d", i), LibraryID: library.ID, Generation: 2,
			PolicyJSON: `{}`, Status: models.MediaArtifactStatusSuperseded, ErrorCode: "old_reason",
			CleanupStatus: models.MediaArtifactCleanupSkipped, FinishedAt: &now,
			ExpectedCount: 10, ProcessedCount: 10, SucceededCount: 10, UpdatedCount: 10,
			CreatedAt: now, UpdatedAt: now,
		})
	}
	if err := s.db.CreateInBatches(&runs, 100).Error; err != nil {
		t.Fatal(err)
	}
	result, err := s.DeleteRequest(context.Background(), actor, library.ID, RequestContext{})
	if err != nil || result.Deleted || result.Status != "deleting" || result.JobID == "" {
		t.Fatalf("headless bounded retirement result=%+v err=%v", result, err)
	}
	again, err := s.DeleteRequest(context.Background(), actor, library.ID, RequestContext{})
	if err != nil || again != result {
		t.Fatalf("headless retirement was not idempotent: %+v err=%v", again, err)
	}
	claim, err := queue.Claim([]string{JobTypeMediaLibraryRetirement})
	if err != nil || claim == nil {
		t.Fatalf("claim headless retirement: %v", err)
	}
	var row models.MediaLibraryRetirement
	if err := s.db.Where("job_id = ?", claim.Job.ID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.SourceEpoch != 0 || row.SourceFingerprint != "" || row.ConfigFingerprint != "" {
		t.Fatalf("headless retirement invented a catalog fence: %+v", row)
	}
	worker := NewMediaLibraryRetirementWorker(s)
	for step := 0; step < 200 && row.Phase != "completed"; step++ {
		waiting, _, stepErr := worker.step(context.Background(), *claim, &row)
		if stepErr != nil || waiting {
			t.Fatalf("headless step=%d phase=%s waiting=%v err=%v", step, row.Phase, waiting, stepErr)
		}
	}
	if row.Phase != "completed" {
		t.Fatalf("headless retirement did not complete: phase=%s", row.Phase)
	}
	var remaining int64
	if err := s.db.Model(&models.MediaArtifactRun{}).Where("library_id = ?", library.ID).Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("headless history remaining=%d err=%v", remaining, err)
	}
	if err := s.db.First(&models.MediaLibrary{}, library.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("headless library still exists: %v", err)
	}
}

func retirementFixture(t *testing.T) (*MediaLibraryService, models.MediaLibrary, Actor) {
	t.Helper()
	store, library, recognition, entries := catalogFixture(t)
	catalogConvert(t, store, library, recognition, entries)
	s := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
	s.SetCatalogSnapshotStore(store)
	s.SetQueueService(NewQueueService(store.writeDB, s.audit))
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

func TestLibraryRetirementAcceptanceCreatesGateBeforePhysicalDrain(t *testing.T) {
	s, library, actor := retirementFixture(t)
	if err := s.db.Model(&library).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	var before models.CatalogHead
	s.db.First(&before, "library_id=?", library.ID)
	result, err := s.DeleteRequest(context.Background(), actor, library.ID, RequestContext{})
	if err != nil || result.JobID == "" || result.Deleted {
		t.Fatalf("retirement was not accepted: result=%+v err=%v", result, err)
	}
	var after models.MediaLibrary
	s.db.First(&after, library.ID)
	if after.Enabled {
		t.Fatal("retirement gate did not disable producers")
	}
	var head models.CatalogHead
	s.db.First(&head, "library_id=?", library.ID)
	if head != before {
		t.Fatal("acceptance changed head")
	}
	var n int64
	s.db.Model(&models.MediaLibraryRetirement{}).Count(&n)
	if n != 1 {
		t.Fatalf("retirement receipt count=%d", n)
	}
	s.db.Model(&models.Job{}).Where("job_type=?", JobTypeMediaLibraryRetirement).Count(&n)
	if n != 1 {
		t.Fatalf("retirement job count=%d", n)
	}
}

func TestLibraryRetirementCanClaimWhileLibraryWorkerOwnsOrdinaryResource(t *testing.T) {
	s, library, actor := retirementFixture(t)
	now := time.Now().UTC()
	expires := now.Add(time.Minute)
	ownerID := actor.User.ID
	blocking := models.Job{
		ID: "running-library-worker", OwnerID: &ownerID, CreatedByKind: "user",
		JobType: JobTypeMediaArtifact, Priority: 100, LanePosition: 1, Revision: 1,
		Status: models.JobStatusRunning, DisplayName: "running library worker",
		ResourceKey: mediaArtifactResourceKey(library.ID), PayloadJSON: `{}`, CheckpointJSON: `{}`,
		Generation: 1, StartedGeneration: 1, LeaseTokenHash: "active-lease", LeaseExpiresAt: &expires,
		HeartbeatAt: &now, AttemptCount: 1, CreatedAt: now, UpdatedAt: now, StartedAt: &now,
	}
	if err := s.db.Create(&blocking).Error; err != nil {
		t.Fatal(err)
	}
	result, err := s.DeleteRequest(context.Background(), actor, library.ID, RequestContext{})
	if err != nil || result.JobID == "" {
		t.Fatalf("accept result=%+v err=%v", result, err)
	}
	var retirementJob models.Job
	if err := s.db.First(&retirementJob, "id = ?", result.JobID).Error; err != nil {
		t.Fatal(err)
	}
	if retirementJob.ResourceKey != mediaLibraryRetirementResourceKey(library.ID) || retirementJob.ResourceKey == blocking.ResourceKey {
		t.Fatalf("retirement resource cannot preempt library worker: retirement=%q worker=%q", retirementJob.ResourceKey, blocking.ResourceKey)
	}
	claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRetirement})
	if err != nil || claim == nil || claim.Job.ID != result.JobID {
		t.Fatalf("retirement was blocked behind worker: claim=%+v err=%v", claim, err)
	}
}

func TestLibraryRetirementStopsAndRemovesExactLibraryTransferOnly(t *testing.T) {
	s, library, actor := retirementFixture(t)
	other := library
	other.ID = 0
	other.Name = "Other transfer library"
	other.NameNormalized = "other transfer library"
	other.RelativeRoot = "/other-transfer"
	if err := s.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	expires := now.Add(time.Minute)
	ownerID := actor.User.ID
	createPipeline := func(prefix string, target models.MediaLibrary, running bool) (models.Job, models.TransferTask, models.CatalogPhysicalWrite) {
		downloadJob := models.Job{ID: prefix + "-download-job", OwnerID: &ownerID, CreatedByKind: "user", JobType: "download", Priority: 1, LanePosition: 1, Revision: 1, Status: models.JobStatusCompleted, DisplayName: prefix + " download", PayloadJSON: `{}`, CheckpointJSON: `{}`, Generation: 1, FinishedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := s.db.Create(&downloadJob).Error; err != nil {
			t.Fatal(err)
		}
		targetID := target.ID
		download := models.DownloadTask{ID: prefix + "-download", OwnerID: ownerID, JobID: downloadJob.ID, DownloaderName: "test", ProviderType: models.DownloaderTypeFake, SourceCiphertext: "test", DisplayName: prefix, Phase: models.DownloadTaskStatusCompleted, TargetLibraryID: &targetID, TargetLibraryName: target.Name, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
		if err := s.db.Create(&download).Error; err != nil {
			t.Fatal(err)
		}
		status := models.JobStatusCompleted
		finished := &now
		job := models.Job{ID: prefix + "-transfer-job", OwnerID: &ownerID, CreatedByKind: "user", JobType: "transfer", Priority: 1, LanePosition: 1, Revision: 1, Status: status, DisplayName: prefix + " transfer", ResourceKey: "library:" + uintID(target.ID), PayloadJSON: `{"transfer_task_id":"` + prefix + `-transfer"}`, CheckpointJSON: `{}`, Generation: 1, FinishedAt: finished, CreatedAt: now, UpdatedAt: now}
		if running {
			job.Status, job.FinishedAt = models.JobStatusRunning, nil
			job.StartedGeneration, job.LeaseTokenHash, job.LeaseExpiresAt, job.HeartbeatAt, job.StartedAt, job.AttemptCount = 1, "transfer-lease", &expires, &now, &now, 1
		}
		if err := s.db.Create(&job).Error; err != nil {
			t.Fatal(err)
		}
		transfer := models.TransferTask{ID: prefix + "-transfer", OwnerID: ownerID, JobID: job.ID, DownloadTaskID: download.ID, LibraryID: target.ID, LibraryName: target.Name, ManifestJSON: `{}`, SourceManifestJSON: `{}`, SourceDataSourceJSON: `{}`, TargetDataSourceJSON: `{}`, Phase: models.TransferTaskStatusCompleted, CleanupStatus: models.TransferCleanupCompleted, CreatedAt: now, UpdatedAt: now, FinishedAt: &now}
		if running {
			transfer.Phase, transfer.CleanupStatus, transfer.FinishedAt = models.TransferTaskStatusTransferring, models.TransferCleanupPending, nil
		}
		if err := s.db.Create(&transfer).Error; err != nil {
			t.Fatal(err)
		}
		state := "settled"
		if running {
			state = "entered"
		}
		proof := models.CatalogPhysicalWrite{LibraryID: target.ID, OwnerKind: CatalogPhysicalTransfer, OwnerID: transfer.ID, State: state, Revision: 1, OwnerDigest: prefix, JobID: job.ID, JobLeaseHash: job.LeaseTokenHash, EnteredAt: now, UpdatedAt: now}
		if err := s.db.Create(&proof).Error; err != nil {
			t.Fatal(err)
		}
		return job, transfer, proof
	}
	job, transfer, proof := createPipeline("selected", library, true)
	otherJob, otherTransfer, _ := createPipeline("other", other, false)
	for _, task := range []models.TransferTask{transfer, otherTransfer} {
		for i := 0; i < CatalogBatchRows+1; i++ {
			fileToken := fmt.Sprintf("file-%d", i)
			if err := s.db.Create(&models.RemoteTransferFile{TransferTaskID: task.ID, DownloadTaskID: task.DownloadTaskID, FileToken: fileToken, RelativePath: fileToken, CompletedBitmap: []byte{}, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.db.Create(&models.RemoteUploadFile{TransferTaskID: task.ID, DownloadTaskID: task.DownloadTaskID, Ordinal: i, SourceFileToken: fileToken, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	claim, row := claimRetirement(t, s, library, actor)
	var frozen int64
	if err := s.db.Model(&models.MediaLibraryRetirementJob{}).Where("retirement_id = ? AND job_id = ?", row.ID, job.ID).Count(&frozen).Error; err != nil || frozen != 1 {
		t.Fatalf("selected-library transfer was not frozen: count=%d err=%v", frozen, err)
	}
	worker := NewMediaLibraryRetirementWorker(s)
	waiting := false
	for i := 0; i < 10 && !waiting; i++ {
		var err error
		waiting, _, err = worker.step(context.Background(), claim, &row)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !waiting {
		var got models.Job
		_ = s.db.First(&got, "id = ?", job.ID).Error
		t.Fatalf("running transfer was not interrupted: phase=%s job=%+v", row.Phase, got)
	}
	var interrupted models.Job
	if err := s.db.First(&interrupted, "id = ?", job.ID).Error; err != nil || !interrupted.CancellationAsked || interrupted.InterruptStatus != models.JobStatusCancelled {
		t.Fatalf("transfer interrupt not persisted: job=%+v err=%v", interrupted, err)
	}
	if err := s.db.First(&proof, proof.ID).Error; err != nil || proof.State != "entered" {
		t.Fatalf("entered evidence removed before exit: proof=%+v err=%v", proof, err)
	}
	if err := s.db.Model(&models.CatalogPhysicalWrite{}).Where("id = ?", proof.ID).Updates(map[string]any{"state": "quiescent", "updated_at": time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.Job{}).Where("id = ?", job.ID).Updates(map[string]any{"status": models.JobStatusCancelled, "interrupt_status": "", "lease_token_hash": "", "lease_expires_at": nil, "heartbeat_at": nil, "finished_at": time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 160 && row.Phase != "completed"; i++ {
		if _, _, err := worker.step(context.Background(), claim, &row); err != nil {
			t.Fatal(err)
		}
	}
	if row.Phase != "completed" {
		t.Fatalf("retirement did not complete: %s", row.Phase)
	}
	for label, model := range map[string]any{"selected transfer": &models.TransferTask{}, "selected job": &models.Job{}, "selected proof": &models.CatalogPhysicalWrite{}} {
		id := any(transfer.ID)
		switch label {
		case "selected job":
			id = job.ID
		case "selected proof":
			id = proof.ID
		}
		if err := s.db.First(model, "id = ?", id).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("%s survived: %v", label, err)
		}
	}
	if err := s.db.First(&models.TransferTask{}, "id = ?", otherTransfer.ID).Error; err != nil {
		t.Fatalf("other-library transfer removed: %v", err)
	}
	if err := s.db.First(&models.Job{}, "id = ?", otherJob.ID).Error; err != nil {
		t.Fatalf("other-library transfer job removed: %v", err)
	}
	if err := s.db.First(&models.DownloadTask{}, "id = ?", "selected-download").Error; err != nil {
		t.Fatalf("upstream download history removed: %v", err)
	}
	for _, model := range []any{&models.RemoteTransferFile{}, &models.RemoteUploadFile{}} {
		for _, task := range []models.TransferTask{transfer, otherTransfer} {
			var count int64
			want := int64(0)
			if task.ID == otherTransfer.ID {
				want = CatalogBatchRows + 1
			}
			if err := s.db.Model(model).Where("transfer_task_id = ?", task.ID).Count(&count).Error; err != nil || count != want {
				t.Fatalf("remote checkpoint retirement scope: model=%T task=%s count=%d want=%d err=%v", model, task.ID, count, want, err)
			}
		}
	}
}

func TestLibraryRetirementDiscardsUnknownLibraryReferenceAfterDrain(t *testing.T) {
	s, library, actor := retirementFixture(t)
	var layer models.CatalogHeadLayer
	s.db.First(&layer, "library_id=?", library.ID)
	if err := s.db.Create(&models.CatalogSnapshotReference{SnapshotID: layer.SnapshotID, OwnerKind: "diagnosis", OwnerID: "unknown-owner", CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatal(err)
	}
	claim, row := claimRetirement(t, s, library, actor)
	w := NewMediaLibraryRetirementWorker(s)
	for i := 0; i < 100 && row.Phase != "completed"; i++ {
		if _, _, err := w.step(context.Background(), claim, &row); err != nil {
			t.Fatal(err)
		}
	}
	if row.Phase != "completed" {
		t.Fatalf("retirement did not complete: %s", row.Phase)
	}
	var n int64
	s.db.Model(&models.CatalogSnapshotReference{}).Where("owner_id=?", "unknown-owner").Count(&n)
	if n != 0 {
		t.Fatal("retired library reference survived")
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
	// A stale expiry without a lease hash is not an active worker and must not
	// keep retirement in an endless draining loop.
	staleExpiry := time.Now().UTC().Add(-time.Minute)
	if err := s.db.Model(&models.Job{}).Where("id = ?", job.ID).Update("lease_expires_at", staleExpiry).Error; err != nil {
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
	if err := s.db.First(&models.Job{}, "id=?", job.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("retired library job history survived: %v", err)
	}
	if err := s.db.First(&models.MediaLibraryStructureRepair{}, "id=?", "retirement-safe-repair").Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("retired library repair history survived: %v", err)
	}
}
