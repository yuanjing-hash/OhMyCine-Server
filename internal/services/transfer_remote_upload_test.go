package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	fail bool
}

func (c *retryRenewRemoteUploadClient) PutOperation(context.Context, nodeprotocol.PutOperationRequest) (nodeprotocol.OperationResponse, error) {
	panic("unexpected PutOperation")
}
func (c *retryRenewRemoteUploadClient) RenewOperationLease(_ context.Context, key string, input nodeprotocol.LeaseRequest) (nodeprotocol.OperationResponse, error) {
	if c.fail {
		c.fail = false
		return nodeprotocol.OperationResponse{}, errors.New("temporary network failure")
	}
	return nodeprotocol.OperationResponse{OperationKey: key, PlanDigest: input.PlanDigest, Status: nodeprotocol.OperationRunning, Phase: nodeprotocol.PhaseUploadingTarget, LeaseEpoch: input.LeaseEpoch, LeaseExpiresAt: input.LeaseExpiresAt, Revision: 2}, nil
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
	batches, err := buildRemoteUploadBatches(rows, models.Storage{ID: 9}, models.DownloadTask{ID: uuid.NewString(), TargetProviderRootID: "library-root"}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	covered := 0
	for index, batch := range batches {
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
	if _, err := worker.renewRemoteUploadOperation(context.Background(), client, &record); err == nil {
		t.Fatal("first renewal unexpectedly succeeded")
	}
	var afterFailure models.RemoteOperation
	if err := queue.db.First(&afterFailure, record.ID).Error; err != nil || afterFailure.LeaseEpoch != before || record.LeaseEpoch != before {
		t.Fatalf("after failure db=%+v record=%+v err=%v", afterFailure, record, err)
	}
	if _, err := worker.renewRemoteUploadOperation(context.Background(), client, &record); err != nil {
		t.Fatal(err)
	}
	var afterSuccess models.RemoteOperation
	if err := queue.db.First(&afterSuccess, record.ID).Error; err != nil || afterSuccess.LeaseEpoch != before+1 || record.LeaseEpoch != before+1 {
		t.Fatalf("after success db=%+v record=%+v err=%v", afterSuccess, record, err)
	}
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

func TestRemoteUploadOperationKeyIsStablePerBatch(t *testing.T) {
	taskID := uuid.NewString()
	firstKey := remoteUploadOperationKey(taskID, 0)
	if firstKey != remoteUploadOperationKey(taskID, 0) || firstKey == remoteUploadOperationKey(taskID, 1) {
		t.Fatal("remote upload operation key is not stable per batch")
	}
}

func TestRemoteUploadAcceptsCompletedLostAckWithOlderLeaseOnly(t *testing.T) {
	record := models.RemoteOperation{OperationKey: "storage:upload:lost-ack", PlanDigest: strings.Repeat("a", 64), LeaseEpoch: 3}
	completed := nodeprotocol.OperationResponse{OperationKey: record.OperationKey, PlanDigest: record.PlanDigest, Status: nodeprotocol.OperationCompleted, Phase: nodeprotocol.PhaseCompleted, LeaseEpoch: 2, Revision: 4}
	if err := validateRemoteOperationResponse(completed, record); err != nil {
		t.Fatalf("completed lost acknowledgement was rejected: %v", err)
	}
	running := completed
	running.Status = nodeprotocol.OperationRunning
	if err := validateRemoteOperationResponse(running, record); err == nil {
		t.Fatal("running operation with stale lease was accepted")
	}
}
