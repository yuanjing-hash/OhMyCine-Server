package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

type retryRenewRemoteUploadClient struct {
	fail            bool
	operation       *nodeprotocol.OperationResponse
	renewals        []nodeprotocol.LeaseRequest
	beforeRenew     func(nodeprotocol.LeaseRequest)
	completeOnRenew bool
}

func (c *retryRenewRemoteUploadClient) PutOperation(context.Context, nodeprotocol.PutOperationRequest) (nodeprotocol.OperationResponse, error) {
	if c.operation != nil {
		return *c.operation, nil
	}
	panic("unexpected PutOperation")
}
func (c *retryRenewRemoteUploadClient) RenewOperationLease(_ context.Context, key string, input nodeprotocol.LeaseRequest) (nodeprotocol.OperationResponse, error) {
	c.renewals = append(c.renewals, input)
	if c.beforeRenew != nil {
		c.beforeRenew(input)
	}
	if c.fail {
		c.fail = false
		return nodeprotocol.OperationResponse{}, errors.New("temporary network failure")
	}
	status, phase := nodeprotocol.OperationRunning, nodeprotocol.PhaseUploadingTarget
	if c.completeOnRenew {
		status, phase = nodeprotocol.OperationCompleted, nodeprotocol.PhaseCompleted
	}
	return nodeprotocol.OperationResponse{OperationKey: key, PlanDigest: input.PlanDigest, Status: status, Phase: phase, LeaseEpoch: input.LeaseEpoch, LeaseExpiresAt: input.LeaseExpiresAt, Revision: 2}, nil
}
func (c *retryRenewRemoteUploadClient) PutCredentialGrant(context.Context, nodeprotocol.CredentialGrantEnvelope) error {
	panic("unexpected PutCredentialGrant")
}
func (c *retryRenewRemoteUploadClient) StorageAction(context.Context, nodeprotocol.StorageActionRequest) (nodeprotocol.StorageActionResponse, error) {
	panic("unexpected StorageAction")
}
func (c *retryRenewRemoteUploadClient) StorageActionResult(context.Context, string, string, string) (nodeprotocol.StorageActionResponse, error) {
	panic("unexpected StorageActionResult")
}

func TestRemoteUploadBatchesHaveNoWholeTaskFileLimit(t *testing.T) {
	rows := make([]models.RemoteUploadFile, 1201)
	for index := range rows {
		rows[index] = models.RemoteUploadFile{
			Ordinal: index, SourceFileToken: "file:" + uuid.NewString(),
			TargetRelative: fmt.Sprintf("电视剧/Example/Season 01/Example - S01E%05d.mkv", index+1),
			Size:           1, SHA256: strings.Repeat("a", 64), ConflictAction: nodeprotocol.StorageConflictFailIfExists,
		}
	}
	manifest := downloadpkg.Manifest{RemoteExportOperationKey: "qb:file_export:batch-test", RemoteExportDigest: strings.Repeat("b", 64)}
	storage := models.Storage{ID: 9, Type: models.StorageTypeQuark, RootPath: "storage-root"}
	task := models.DownloadTask{ID: uuid.NewString(), TargetProviderRootID: "library-root", TargetRelativeRoot: "/nested/library"}
	batches, err := buildRemoteUploadBatches(rows, storage, task, manifest)
	if err != nil {
		t.Fatal(err)
	}
	covered := 0
	for index, batch := range batches {
		if batch.plan.TargetRootRelativePath != task.TargetRelativeRoot || batch.plan.TargetRootID != task.TargetProviderRootID {
			t.Fatal("frozen Quark target locator changed")
		}
		if batch.index != index || len(batch.rows) == 0 || len(batch.rows) > nodeprotocol.MaxStorageUploadFiles || len(batch.plan.Files) != len(batch.rows) {
			t.Fatalf("invalid batch %d: rows=%d plan=%d", index, len(batch.rows), len(batch.plan.Files))
		}
		raw, err := json.Marshal(batch.plan)
		if err != nil || len(raw) > nodeprotocol.MaxWireBodyBytes {
			t.Fatalf("batch %d wire bytes=%d err=%v", index, len(raw), err)
		}
		covered += len(batch.rows)
	}
	if covered != len(rows) || len(batches) != 3 {
		t.Fatalf("covered=%d batches=%d, want files=%d batches=3", covered, len(batches), len(rows))
	}
	task.TargetRelativeRoot = ""
	if _, err = buildRemoteUploadBatches(rows[:1], storage, task, manifest); err == nil {
		t.Fatal("upload child accepted without frozen path")
	}
	task.TargetProviderRootID = storage.RootPath
	if batches, err = buildRemoteUploadBatches(rows[:1], storage, task, manifest); err != nil || batches[0].plan.TargetRootRelativePath != "/" {
		t.Fatalf("upload root compatibility failed: %v", err)
	}
}

func TestRemoteUploadOperationIsStableAndOnlyRenewsLease(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	service := NewTransferService(queue.db, queue.audit, queue, zerolog.Nop())
	worker := NewTransferWorker(service)
	nodeID, taskID, operationKey := uuid.NewString(), uuid.NewString(), remoteUploadOperationKey(uuid.NewString(), 0)
	now := time.Now().UTC()
	if err := queue.db.Create(&models.TransferNode{ID: nodeID, OwnerID: actor.User.ID, Name: "Operation Node", NameNormalized: "operation-node-" + nodeID, APIURL: "https://node.example.test", Status: models.NodeStatusOnline, ProtocolMin: 1, ProtocolMax: 1, CapabilitiesJSON: `{}`, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	payload := nodeprotocol.StorageUploadPlan{
		StorageID: "1", ProviderType: nodeprotocol.StorageProviderPan115,
		SourceExportOperationKey: "qb:file_export:operation-test", SourceManifestDigest: strings.Repeat("a", 64), TargetRootID: "library-root",
		Files: []nodeprotocol.StorageUploadFile{{SourceFileToken: "file:operation-test", TargetRelativePath: "电影/Movie.mkv", Size: 10, SHA256: strings.Repeat("b", 64), ConflictAction: nodeprotocol.StorageConflictFailIfExists}},
	}
	firstPlan, first, completed, err := worker.prepareRemoteUploadOperation(context.Background(), nodeID, taskID, operationKey, payload)
	if err != nil || completed {
		t.Fatalf("first record=%+v completed=%v err=%v", first, completed, err)
	}
	secondPlan, second, completed, err := worker.prepareRemoteUploadOperation(context.Background(), nodeID, taskID, operationKey, payload)
	if err != nil || completed {
		t.Fatalf("second record=%+v completed=%v err=%v", second, completed, err)
	}
	if first.PlanDigest != second.PlanDigest || firstPlan.LeaseEpoch+1 != secondPlan.LeaseEpoch || second.LeaseEpoch != secondPlan.LeaseEpoch {
		t.Fatalf("first=%+v second=%+v firstPlan=%+v secondPlan=%+v", first, second, firstPlan, secondPlan)
	}
	changed := payload
	changed.Files = append([]nodeprotocol.StorageUploadFile(nil), payload.Files...)
	changed.Files[0].TargetRelativePath = "电影/Changed.mkv"
	if _, _, _, err := worker.prepareRemoteUploadOperation(context.Background(), nodeID, taskID, operationKey, changed); err == nil {
		t.Fatal("changed immutable payload was accepted")
	} else {
		var failure *cloudTransferFailure
		if !errors.As(err, &failure) || failure.code != nodeprotocol.ErrorPlanConflict {
			t.Fatalf("changed immutable payload err=%v failure=%+v", err, failure)
		}
	}
	late := nodeprotocol.OperationResponse{OperationKey: second.OperationKey, PlanDigest: second.PlanDigest, Status: nodeprotocol.OperationCompleted, Phase: nodeprotocol.PhaseCompleted, LeaseEpoch: first.LeaseEpoch, Revision: 4}
	if err := validateRemoteOperationResponse(late, second); err != nil {
		t.Fatalf("completed lost acknowledgement was rejected: %v", err)
	}
	late.Status = nodeprotocol.OperationRunning
	if err := validateRemoteOperationResponse(late, second); err == nil {
		t.Fatal("running operation with stale lease was accepted")
	}
}

func TestRemoteUploadLeaseRenewalCanRetryAfterNetworkFailure(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	service := NewTransferService(queue.db, queue.audit, queue, zerolog.Nop())
	worker := NewTransferWorker(service)
	nodeID := uuid.NewString()
	now := time.Now().UTC()
	if err := queue.db.Create(&models.TransferNode{ID: nodeID, OwnerID: actor.User.ID, Name: "Renew Node", NameNormalized: "renew-node-" + nodeID, APIURL: "https://node.example.test", Status: models.NodeStatusOnline, ProtocolMin: 1, ProtocolMax: 1, CapabilitiesJSON: `{}`, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	payload := nodeprotocol.StorageUploadPlan{StorageID: "1", ProviderType: nodeprotocol.StorageProviderPan115, SourceExportOperationKey: "qb:file_export:renew-test", SourceManifestDigest: strings.Repeat("a", 64), TargetRootID: "library-root", Files: []nodeprotocol.StorageUploadFile{{SourceFileToken: "file:renew-test", TargetRelativePath: "电影/Movie.mkv", Size: 10, SHA256: strings.Repeat("b", 64), ConflictAction: nodeprotocol.StorageConflictFailIfExists}}}
	_, record, _, err := worker.prepareRemoteUploadOperation(context.Background(), nodeID, uuid.NewString(), remoteUploadOperationKey(uuid.NewString(), 0), payload)
	if err != nil {
		t.Fatal(err)
	}
	before := record.LeaseEpoch
	client := &retryRenewRemoteUploadClient{fail: true}
	client.beforeRenew = func(input nodeprotocol.LeaseRequest) {
		var issued models.RemoteOperation
		if err := queue.db.First(&issued, record.ID).Error; err != nil || issued.LeaseEpoch != input.LeaseEpoch {
			t.Fatalf("renewal reached Node before durable reservation: %+v err=%v", issued, err)
		}
	}
	if _, err := worker.renewRemoteUploadOperation(context.Background(), client, &record); err == nil {
		t.Fatal("first renewal unexpectedly succeeded")
	}
	var afterFailure models.RemoteOperation
	if err := queue.db.First(&afterFailure, record.ID).Error; err != nil || afterFailure.LeaseEpoch != before+1 || record.LeaseEpoch != before+1 {
		t.Fatalf("after failure db=%+v record=%+v err=%v", afterFailure, record, err)
	}
	if _, err := worker.renewRemoteUploadOperation(context.Background(), client, &record); err != nil {
		t.Fatal(err)
	}
	var afterSuccess models.RemoteOperation
	if err := queue.db.First(&afterSuccess, record.ID).Error; err != nil || afterSuccess.LeaseEpoch != before+2 || record.LeaseEpoch != before+2 || len(client.renewals) != 2 || client.renewals[0].LeaseEpoch == client.renewals[1].LeaseEpoch {
		t.Fatalf("after success db=%+v record=%+v err=%v", afterSuccess, record, err)
	}
	client.beforeRenew = func(input nodeprotocol.LeaseRequest) {
		if err := queue.db.Model(&models.RemoteOperation{}).Where("id=? AND lease_epoch=?", record.ID, input.LeaseEpoch).Update("lease_epoch", input.LeaseEpoch+1).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := worker.renewRemoteUploadOperation(context.Background(), client, &record); err == nil {
		t.Fatal("late A renewal overwrote B's reservation")
	}
	if err := queue.db.First(&afterSuccess, record.ID).Error; err != nil || afterSuccess.LeaseEpoch != before+4 {
		t.Fatalf("successor epoch was changed: %+v err=%v", afterSuccess, err)
	}
	transferID, downloadID := uuid.NewString(), uuid.NewString()
	_, existing, _, err := worker.prepareRemoteUploadOperation(context.Background(), nodeID, downloadID, remoteUploadOperationKey(transferID, 0), payload)
	if err != nil {
		t.Fatal(err)
	}
	execution := &retryRenewRemoteUploadClient{fail: true, operation: &nodeprotocol.OperationResponse{OperationKey: existing.OperationKey, PlanDigest: existing.PlanDigest, LeaseEpoch: existing.LeaseEpoch, Revision: 1, Status: nodeprotocol.OperationRunning}}
	retainedJob, err := queue.Enqueue(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "transfer", DisplayName: "retained upload", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	err = worker.executeRemoteUploadBatch(context.Background(), nil, execution, models.TransferNode{ID: nodeID}, models.Storage{}, &models.TransferTask{ID: transferID, JobID: retainedJob.ID}, models.DownloadTask{ID: downloadID}, nil, remoteUploadBatch{plan: payload}, nil)
	if err == nil || len(execution.renewals) != 1 || execution.renewals[0].LeaseEpoch != existing.LeaseEpoch+1 {
		t.Fatalf("reserved successor rejected the existing Node epoch before renewal: renewals=%+v err=%v", execution.renewals, err)
	}
	t.Run("completed_original_without_source", func(t *testing.T) {
		for _, completedOnRenew := range []bool{false, true} {
			f := newRemoteBatchFixture(t, nodeprotocol.StorageProviderPan115)
			task := f.task
			task.Phase = models.TransferTaskStatusTransferring
			if err := f.queue.db.Model(&task).Updates(map[string]any{"phase": task.Phase, "finished_at": nil}).Error; err != nil {
				t.Fatal(err)
			}
			rows := append([]models.RemoteUploadFile(nil), f.rows...)
			rows[0].Status = nodeprotocol.StorageFilePending
			if err := f.queue.db.Model(&models.RemoteUploadFile{}).Where("id=?", rows[0].ID).Update("status", nodeprotocol.StorageFilePending).Error; err != nil {
				t.Fatal(err)
			}
			var storage models.Storage
			if err := f.queue.db.First(&storage, f.download.TargetStorageID).Error; err != nil {
				t.Fatal(err)
			}
			manifest := f.manifest
			expired := time.Now().Add(-time.Hour)
			manifest.RemoteExportExpiresAt = &expired
			batches, err := buildRemoteUploadBatches(rows, storage, f.download, manifest)
			if err != nil {
				t.Fatal(err)
			}
			existing, err := remoteUploadBatchRecordTx(f.queue.db, task, f.download, batches[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := f.queue.db.Model(&models.RemoteOperation{}).Where("id=?", existing.ID).Updates(map[string]any{"status": nodeprotocol.OperationRunning, "phase": nodeprotocol.PhaseUploadingTarget}).Error; err != nil {
				t.Fatal(err)
			}
			putStatus, expectedRenewals := nodeprotocol.OperationCompleted, 0
			if completedOnRenew {
				putStatus, expectedRenewals = nodeprotocol.OperationRunning, 1
			}
			remote := &terminalRemoteResultClient{key: existing.OperationKey, task: f.download.ID,
				retryRenewRemoteUploadClient: retryRenewRemoteUploadClient{completeOnRenew: completedOnRenew, operation: &nodeprotocol.OperationResponse{OperationKey: existing.OperationKey, PlanDigest: existing.PlanDigest, LeaseEpoch: existing.LeaseEpoch, Revision: 1, Status: putStatus}},
				response:                     nodeprotocol.StorageActionResponse{RequestID: existing.OperationKey, OperationKey: existing.OperationKey, PlanDigest: existing.PlanDigest, Status: nodeprotocol.StorageActionCompleted, CompletedFiles: 1, TotalFiles: 1, Files: []nodeprotocol.StorageUploadFileResult{{SourceFileToken: rows[0].SourceFileToken, TargetParentID: rows[0].TargetParentID, TargetItemID: rows[0].TargetItemID, TargetName: path.Base(rows[0].TargetRelative), Size: rows[0].Size, SHA1: rows[0].TargetSHA1, Status: nodeprotocol.StorageFileCompleted}}}}
			f.queue = NewQueueService(f.queue.db, f.queue.audit)
			claimed, err := f.queue.Claim([]string{"transfer"})
			if err != nil || claimed == nil {
				t.Fatalf("original transfer claim=%+v err=%v", claimed, err)
			}
			ctx := context.WithValue(context.Background(), remoteUploadClaimKey{}, *claimed)
			f.service.downloader = nil // A completed original result cannot need source access.
			if err := NewTransferWorker(f.service).executeRemoteUploadBatch(ctx, workerRuntime{queue: f.queue, job: *claimed}, remote, models.TransferNode{ID: *f.download.NodeID}, storage, &task, f.download, rows, batches[0], &manifest); err != nil {
				t.Fatal(err)
			}
			if remote.queries != 1 || len(remote.renewals) != expectedRenewals || !manifest.RemoteExportExpiresAt.Equal(expired) || !remoteUploadRowsTerminal(rows) {
				t.Fatal("completed original required renewal/source or failed to reconcile pending file receipts")
			}
		}
	})
}

func TestRemoteUploadResponsePersistsTerminalReceiptsAndSummary(t *testing.T) {
	fixture := newUploadTransferFixture(t, models.MediaLibraryConflictOverwrite, false)
	if err := fixture.service.EnqueuePackage(fixture.download, fixture.manifest, fixture.manifest); err != nil {
		t.Fatal(err)
	}
	var task models.TransferTask
	if err := fixture.queue.db.Where("download_task_id = ?", fixture.download.ID).First(&task).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := []models.RemoteUploadFile{
		{TransferTaskID: task.ID, DownloadTaskID: fixture.download.ID, Ordinal: 0, SourceFileToken: "file:complete", SourceRelative: "Movie.mkv", TargetRelative: "电影/Movie.mkv", Size: 10, SHA256: strings.Repeat("a", 64), ConflictAction: nodeprotocol.StorageConflictFailIfExists, Status: nodeprotocol.StorageFilePending, CreatedAt: now, UpdatedAt: now},
		{TransferTaskID: task.ID, DownloadTaskID: fixture.download.ID, Ordinal: 1, SourceFileToken: "file:skip", SourceRelative: "Movie.zh.ass", TargetRelative: "电影/Movie.zh.ass", Size: 2, SHA256: strings.Repeat("b", 64), ConflictAction: nodeprotocol.StorageConflictSkipIfExists, Status: nodeprotocol.StorageFilePending, CreatedAt: now, UpdatedAt: now},
	}
	if err := fixture.queue.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	response := nodeprotocol.StorageActionResponse{RequestID: "request", OperationKey: "operation", PlanDigest: strings.Repeat("c", 64), Status: nodeprotocol.StorageActionCompleted, TotalFiles: 2, CompletedFiles: 2, Files: []nodeprotocol.StorageUploadFileResult{
		{SourceFileToken: rows[0].SourceFileToken, TargetParentID: "movie-dir", TargetName: "Movie.mkv", TargetItemID: "uploaded-movie", Status: nodeprotocol.StorageFileCompleted, Size: 10, SHA1: strings.Repeat("d", 40)},
		{SourceFileToken: rows[1].SourceFileToken, TargetParentID: "movie-dir", TargetName: "Movie.zh.ass", Status: nodeprotocol.StorageFileSkipped, Size: 2},
	}}
	if err := NewTransferWorker(fixture.service).applyRemoteUploadResponse(context.Background(), &task, rows, rows, response); err != nil {
		t.Fatal(err)
	}
	var saved []models.RemoteUploadFile
	if err := fixture.queue.db.Where("transfer_task_id = ?", task.ID).Order("ordinal").Find(&saved).Error; err != nil {
		t.Fatal(err)
	}
	if saved[0].Status != nodeprotocol.StorageFileCompleted || saved[0].TargetItemID != "uploaded-movie" || saved[1].Status != nodeprotocol.StorageFileSkipped {
		t.Fatalf("saved=%+v", saved)
	}
	var summary TransferPlanSummary
	if err := json.Unmarshal([]byte(task.PlanSummaryJSON), &summary); err != nil || summary.CompletedFiles != 1 || summary.SkippedFiles != 1 || task.ProcessedFiles != 2 {
		t.Fatalf("task=%+v summary=%+v err=%v", task, summary, err)
	}
	if err := fixture.queue.db.Transaction(func(tx *gorm.DB) error {
		return captureRemoteUploadManagedItems(tx, task, fixture.download, saved)
	}); err != nil {
		t.Fatal(err)
	}
	var managed []models.MediaManagedItem
	if err := fixture.queue.db.Where("transfer_task_id = ?", task.ID).Find(&managed).Error; err != nil || len(managed) != 1 || managed[0].ProviderItemID != "uploaded-movie" {
		t.Fatalf("managed=%+v err=%v", managed, err)
	}
}
