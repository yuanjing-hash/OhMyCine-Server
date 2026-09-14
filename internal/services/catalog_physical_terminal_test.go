package services

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type terminalTransferFixture struct {
	queue    *QueueService
	actor    Actor
	library  models.MediaLibrary
	job      models.Job
	transfer models.TransferTask
}

func newTerminalTransferFixture(t *testing.T, suffix string) terminalTransferFixture {
	t.Helper()
	queue, actor, _ := queueFixture(t)
	actor = historyAdmin(actor)
	now := queue.clock.Now()
	library := historyLibrary(t, queue, "terminal-transfer-"+suffix)
	transferID := "terminal-transfer-" + suffix
	job := models.Job{
		ID: transferID + "-job", OwnerID: &actor.User.ID, CreatedByKind: "user",
		JobType: "transfer", Status: models.JobStatusCancelled, Revision: 1,
		PayloadJSON: fmt.Sprintf(`{"transfer_task_id":%q}`, transferID), CheckpointJSON: `{}`,
		DisplayName: "test transfer", FinishedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := queue.db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	downloadJob := historyJob(t, queue, actor.User.ID, "download-"+suffix+"-job", models.JobStatusCompleted)
	if err := queue.db.Model(&models.Job{}).Where("id = ?", downloadJob.ID).Update("history_cleared_at", now).Error; err != nil {
		t.Fatal(err)
	}
	download := models.DownloadTask{
		ID: "download-" + suffix, OwnerID: actor.User.ID, JobID: downloadJob.ID,
		DownloaderName: "test", ProviderType: "test", SourceCiphertext: "encrypted",
		DisplayName: "test download", Phase: models.DownloadTaskStatusCompleted,
		CreatedAt: now, UpdatedAt: now, FinishedAt: &now,
	}
	if err := queue.db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	transfer := models.TransferTask{
		ID: transferID, OwnerID: actor.User.ID, JobID: job.ID,
		DownloadTaskID: download.ID, LibraryID: library.ID, LibraryName: library.Name,
		ManifestJSON: `{}`, SourceManifestJSON: `{}`, Phase: models.TransferTaskStatusFailed,
		TotalFiles: 1, CleanupStatus: models.TransferCleanupPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := queue.db.Create(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	return terminalTransferFixture{queue: queue, actor: actor, library: library, job: job, transfer: transfer}
}

func (f terminalTransferFixture) drain() error {
	return f.queue.db.Transaction(func(tx *gorm.DB) error {
		return AssertCatalogPhysicalDrainedTx(tx, f.library.ID)
	})
}

func TestCatalogPhysicalCancelledLegacyTransferWithPositiveNoIOEvidenceCloses(t *testing.T) {
	f := newTerminalTransferFixture(t, "no-io")
	if err := f.queue.db.Model(&models.Job{}).Where("id = ?", f.job.ID).Update("history_cleared_at", f.queue.clock.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.drain(); err != nil {
		t.Fatalf("cancelled preflight-only transfer blocked its library: %v", err)
	}
	var transfer models.TransferTask
	if err := f.queue.db.First(&transfer, "id = ?", f.transfer.ID).Error; err != nil {
		t.Fatal(err)
	}
	if transfer.FinishedAt == nil {
		t.Fatal("terminal preflight-only transfer was not lifecycle-closed")
	}
	f.actor.Permissions[authz.PermissionMediaLibrariesDelete] = struct{}{}
	libraries := NewMediaLibraryService(f.queue.db, NewAuditService(f.queue.db), zerolog.Nop())
	t.Cleanup(libraries.Close)
	libraries.SetQueueService(f.queue)
	if result, err := libraries.DeleteRequest(context.Background(), f.actor, f.library.ID, RequestContext{}); err != nil || result.Deleted || result.JobID == "" {
		t.Fatalf("proven no-I/O history still blocked durable library deletion: result=%+v err=%v", result, err)
	}
}

func TestManagementHistoryCannotHideAmbiguousTransferBlocker(t *testing.T) {
	f := newTerminalTransferFixture(t, "ambiguous")
	if err := f.queue.db.Model(&models.TransferTask{}).Where("id = ?", f.transfer.ID).Update("plan_summary_json", `{"items":[{"relative_path":"movie.mkv","result":"planned"}]}`).Error; err != nil {
		t.Fatal(err)
	}
	preview, err := f.queue.PreviewHistoryPurge(f.actor, HistoryPurgeInput{Scope: HistoryScopeTasks})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Deleted != 0 || preview.Skipped != 1 {
		t.Fatalf("ambiguous physical owner was offered for hiding: %+v", preview)
	}
	result, err := f.queue.PurgeHistory(f.actor, HistoryPurgeInput{Scope: HistoryScopeTasks}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 0 || result.Skipped != 1 {
		t.Fatalf("ambiguous physical owner was hidden: %+v", result)
	}
	var job models.Job
	if err := f.queue.db.First(&job, "id = ?", f.job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if job.HistoryClearedAt != nil {
		t.Fatal("actual library blocker disappeared from task history")
	}
	if err := f.drain(); err == nil {
		t.Fatal("planned transfer without entered/settled evidence did not fail closed")
	}
}

func TestQueueCancellationSettlesRegisteredNoIOOwner(t *testing.T) {
	f := newTerminalTransferFixture(t, "registered")
	if err := f.queue.db.Model(&models.TransferTask{}).Where("id = ?", f.transfer.ID).Updates(map[string]any{"phase": models.TransferTaskStatusQueued, "last_error_code": ""}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Model(&models.Job{}).Where("id = ?", f.job.ID).Updates(map[string]any{"status": models.JobStatusQueued, "finished_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	proof := models.CatalogPhysicalWrite{
		LibraryID: f.library.ID, OwnerKind: CatalogPhysicalTransfer, OwnerID: f.transfer.ID,
		Revision: 1, State: "admitted", JobID: f.job.ID, EnteredAt: now, UpdatedAt: now,
	}
	if err := f.queue.db.Create(&proof).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.queue.Control(f.actor, f.job.ID, "cancel", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var current models.CatalogPhysicalWrite
	if err := f.queue.db.First(&current, proof.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.State != "settled" || current.SettledAt == nil {
		t.Fatalf("cancelled admitted owner=%+v, want settled no-I/O proof", current)
	}
	var transfer models.TransferTask
	if err := f.queue.db.First(&transfer, "id = ?", f.transfer.ID).Error; err != nil {
		t.Fatal(err)
	}
	if transfer.Phase != models.TransferTaskStatusFailed || transfer.FinishedAt == nil || transfer.LastErrorCode != "transfer_cancelled" {
		t.Fatalf("cancelled domain lifecycle not closed: %+v", transfer)
	}
	if err := f.drain(); err != nil {
		t.Fatalf("settled cancellation blocked retirement: %v", err)
	}
}

func TestCatalogPhysicalSupersededArtifactTerminalityAndAmbiguity(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, false)
	if err := db.Delete(&repair).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := models.MediaArtifactRun{
		ID: "terminal-superseded-artifact", LibraryID: input.LibraryID, Generation: 1,
		PolicyJSON: `{}`, Status: models.MediaArtifactStatusSuperseded,
		ErrorCode: catalogGenerationSupersededCode, CleanupStatus: models.MediaArtifactCleanupSkipped,
		FinishedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", input.LibraryID).Updates(map[string]any{
		"artifact_generation": 2, "artifact_applied_generation": 2, "artifact_status": models.MediaArtifactStatusCompleted,
	}).Error; err != nil {
		t.Fatal(err)
	}
	drain := func() error {
		return db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, input.LibraryID) })
	}
	if err := drain(); err != nil {
		t.Fatalf("proven superseded artifact blocked retirement: %v", err)
	}
	if err := db.Model(&run).Updates(map[string]any{"status": models.MediaArtifactStatusFailed, "error_code": "artifact_write_failed"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := drain(); err == nil {
		t.Fatal("ambiguous failed artifact was treated as terminal no-I/O")
	}
}

func TestCatalogPhysicalSupersededArtifactWithHistoricalProgressRequiresNewerAppliedGeneration(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, false)
	if err := db.Delete(&repair).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := models.MediaArtifactRun{
		ID: "terminal-superseded-positive-progress", LibraryID: input.LibraryID, Generation: 7,
		PolicyJSON: `{}`, Status: models.MediaArtifactStatusSuperseded, ErrorCode: "historical_reason",
		CleanupStatus: models.MediaArtifactCleanupSkipped, FinishedAt: &now,
		ExpectedCount: 900, ProcessedCount: 900, SucceededCount: 900, UpdatedCount: 900,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	drain := func() error {
		return db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, input.LibraryID) })
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", input.LibraryID).Updates(map[string]any{
		"artifact_generation": 7, "artifact_applied_generation": 7, "artifact_status": models.MediaArtifactStatusCompleted,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := drain(); err == nil {
		t.Fatal("current positive-progress run was accepted without a newer applied generation")
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", input.LibraryID).Updates(map[string]any{
		"artifact_generation": 8, "artifact_applied_generation": 8, "artifact_status": models.MediaArtifactStatusCompleted,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := drain(); err != nil {
		t.Fatalf("fully reconciled superseded run still blocked retirement: %v", err)
	}

	for _, test := range []struct {
		name string
		add  func(*gorm.DB) error
	}{
		{name: "physical owner", add: func(tx *gorm.DB) error {
			return tx.Create(&models.CatalogPhysicalWrite{LibraryID: input.LibraryID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, Revision: 1, State: "quiescent", EnteredAt: now, UpdatedAt: now}).Error
		}},
		{name: "managed artifact", add: func(tx *gorm.DB) error {
			return tx.Create(&models.MediaArtifact{OpaqueID: "superseded-owned-artifact", RunID: run.ID, LibraryID: input.LibraryID, Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/owned.nfo", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}).Error
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			if err := test.add(tx); err != nil {
				t.Fatal(err)
			}
			if err := tx.Transaction(func(inner *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(inner, input.LibraryID) }); err == nil {
				t.Fatal("superseded run with recovery/ownership evidence did not block")
			}
		})
	}
}

func TestCatalogPhysicalFailedZeroItemArtifactUsesPositiveNoIOEvidence(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, false)
	if err := db.Delete(&repair).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := models.Job{
		ID: "terminal-failed-artifact-job", CreatedByKind: "system", JobType: JobTypeMediaArtifact,
		Status: models.JobStatusFailed, Revision: 1,
		PayloadJSON: `{"artifact_run_id":"terminal-failed-artifact"}`, CheckpointJSON: `{}`,
		DisplayName: "failed before artifact I/O", FinishedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaArtifactRun{
		ID: "terminal-failed-artifact", LibraryID: input.LibraryID, Generation: 1, JobID: &job.ID,
		PolicyJSON: `{}`, Status: models.MediaArtifactStatusFailed, ErrorCode: CodeQueueLeaseInvalid,
		CleanupStatus: models.MediaArtifactCleanupSkipped, CleanupAt: &now, FinishedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	drain := func() error {
		return db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, input.LibraryID) })
	}
	if err := drain(); err != nil {
		t.Fatalf("proven failed zero-item artifact blocked retirement: %v", err)
	}
	for _, test := range []struct {
		name    string
		updates map[string]any
	}{
		{name: "active job", updates: map[string]any{"status": models.JobStatusRunning, "finished_at": nil}},
		{name: "malformed payload", updates: map[string]any{"payload_json": `{"artifact_run_id":"another-run"}`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			if err := tx.Model(&models.Job{}).Where("id = ?", job.ID).Updates(test.updates).Error; err != nil {
				t.Fatal(err)
			}
			if err := tx.Transaction(func(inner *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(inner, input.LibraryID) }); err == nil {
				t.Fatal("artifact without exact terminal queue identity did not block retirement")
			}
		})
	}

	for _, test := range []struct {
		name string
		add  func(*gorm.DB) error
	}{
		{name: "physical entered", add: func(tx *gorm.DB) error {
			return tx.Create(&models.CatalogPhysicalWrite{LibraryID: input.LibraryID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, Revision: 1, State: "entered", JobID: job.ID, JobLeaseHash: "historical", OwnerDigest: "owner", SourceFingerprint: "source", ConfigFingerprint: "config", EnteredAt: now, UpdatedAt: now}).Error
		}},
		{name: "physical quiescent", add: func(tx *gorm.DB) error {
			return tx.Create(&models.CatalogPhysicalWrite{LibraryID: input.LibraryID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, Revision: 1, State: "quiescent", JobID: job.ID, JobLeaseHash: "historical", OwnerDigest: "owner", SourceFingerprint: "source", ConfigFingerprint: "config", EnteredAt: now, UpdatedAt: now}).Error
		}},
		{
			name: "write receipt",
			add: func(tx *gorm.DB) error {
				physical := models.CatalogPhysicalWrite{LibraryID: input.LibraryID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, Revision: 1, State: "settled", JobID: job.ID, OwnerDigest: "owner", SourceFingerprint: "source", ConfigFingerprint: "config", EnteredAt: now, SettledAt: &now, UpdatedAt: now}
				if err := tx.Create(&physical).Error; err != nil {
					return err
				}
				return tx.Create(&models.CatalogArtifactWriteReceipt{RunID: run.ID, LibraryID: input.LibraryID, ArtifactID: 1, PhysicalWriteID: physical.ID, PermitRevision: 1, Revision: 1, TargetKind: models.MediaArtifactTargetLocalAdjacent, RelativePath: "/receipt.nfo", RootIdentity: "root", PolicyDigest: "policy", SourceFingerprint: "source", ConfigFingerprint: "config", BeforeArtifactJSON: `{}`, AfterArtifactJSON: `{}`, Phase: "prepared", CreatedAt: now, UpdatedAt: now}).Error
			},
		},
		{
			name: "owned artifact",
			add: func(tx *gorm.DB) error {
				return tx.Create(&models.MediaArtifact{OpaqueID: "failed-no-io-owned", RunID: run.ID, LibraryID: input.LibraryID, Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalAdjacent, RelativePath: "/owned.nfo", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}).Error
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			if err := test.add(tx); err != nil {
				t.Fatal(err)
			}
			if err := tx.Transaction(func(inner *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(inner, input.LibraryID) }); err == nil {
				t.Fatal("ambiguous artifact evidence did not block retirement")
			}
		})
	}
}

func TestCatalogPhysicalDrainIsLibraryScoped(t *testing.T) {
	db, repair, input := catalogPhysicalRepairFixture(t, false)
	if err := db.Delete(&repair).Error; err != nil {
		t.Fatal(err)
	}
	var source models.MediaLibrary
	if err := db.First(&source, input.LibraryID).Error; err != nil {
		t.Fatal(err)
	}
	other := source
	other.ID = 0
	other.Name = "other physical blocker"
	other.NameNormalized = "other physical blocker"
	other.RelativeRoot = "/other-physical-blocker"
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Create(&models.CatalogPhysicalWrite{LibraryID: other.ID, OwnerKind: CatalogPhysicalRepair, OwnerID: "other-only", Revision: 1, State: "entered", EnteredAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, input.LibraryID) }); err != nil {
		t.Fatalf("another library's physical blocker leaked into selected library: %v", err)
	}
}

func TestSTRMHistoryCannotHideAmbiguousArtifactRun(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	actor = historyAdmin(actor)
	library := historyLibrary(t, queue, "ambiguous-artifact-history")
	now := time.Now().UTC()
	run := models.MediaArtifactRun{
		ID: "ambiguous-artifact-history", LibraryID: library.ID, Generation: 1,
		PolicyJSON: `{}`, Status: models.MediaArtifactStatusFailed,
		ErrorCode: "artifact_write_failed", CleanupStatus: models.MediaArtifactCleanupSkipped,
		FinishedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	if err := queue.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	input := HistoryPurgeInput{Scope: HistoryScopeSTRMRuns, ResourceID: fmt.Sprint(library.ID)}
	preview, err := queue.PreviewHistoryPurge(actor, input)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Deleted != 0 || preview.Skipped != 1 {
		t.Fatalf("ambiguous artifact run was offered for hiding: %+v", preview)
	}
	if err := queue.db.Model(&run).Updates(map[string]any{"status": models.MediaArtifactStatusSuperseded, "error_code": catalogGenerationSupersededCode}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{
		"artifact_generation": 2, "artifact_applied_generation": 2, "artifact_status": models.MediaArtifactStatusCompleted,
	}).Error; err != nil {
		t.Fatal(err)
	}
	preview, err = queue.PreviewHistoryPurge(actor, input)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Deleted != 1 || preview.Skipped != 0 {
		t.Fatalf("terminal superseded run remained uncleareable: %+v", preview)
	}
}
