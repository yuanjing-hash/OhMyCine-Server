package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

// Every mutation/lease/grant method inherited here panics. Recovery is allowed
// to read exactly the original request result and nothing else.
type terminalRemoteResultClient struct {
	retryRenewRemoteUploadClient
	key, task  string
	response   nodeprotocol.StorageActionResponse
	err        error
	queries    int
	afterQuery func()
}

func (c *terminalRemoteResultClient) StorageActionResult(_ context.Context, requestID, taskID, operationKey string) (nodeprotocol.StorageActionResponse, error) {
	if requestID != c.key || operationKey != c.key || taskID != c.task {
		panic("recovery changed original operation identity")
	}
	c.queries++
	if c.afterQuery != nil {
		c.afterQuery()
	}
	return c.response, c.err
}

func TestRemoteUploadTerminalRowsRecoverOriginalResultAfterRestart(t *testing.T) {
	for _, provider := range []string{cloud.ProviderQuark, cloud.ProviderPan115} {
		t.Run(provider, func(t *testing.T) {
			f := newRemoteBatchFixture(t, provider)
			key := remoteUploadOperationKey(f.task.ID, 0)
			// Exact crash boundary: applyRemoteUploadResponse has committed all
			// file rows, persistRemoteOperationState has not committed completion.
			if err := f.queue.db.Model(&models.RemoteOperation{}).Where("operation_key = ?", key).Updates(map[string]any{"status": nodeprotocol.OperationRunning, "phase": nodeprotocol.PhaseUploadingTarget}).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.queue.db.Model(&models.TransferTask{}).Where("id = ?", f.task.ID).Update("phase", models.TransferTaskStatusTransferring).Error; err != nil {
				t.Fatal(err)
			}
			readPool, _ := f.catalog.readDB.DB()
			writePool, _ := f.queue.db.DB()
			if err := readPool.Close(); err != nil {
				t.Fatal(err)
			}
			if err := writePool.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := database.Open(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			pool, _ := db.DB()
			t.Cleanup(func() { _ = pool.Close() })
			service := NewTransferService(db, NewAuditService(db), nil, f.service.log)
			worker := NewTransferWorker(service)
			var task models.TransferTask
			var rows []models.RemoteUploadFile
			var storage models.Storage
			var record models.RemoteOperation
			for _, err := range []error{db.First(&task, "id = ?", f.task.ID).Error, db.Where("transfer_task_id = ?", f.task.ID).Order("ordinal").Find(&rows).Error, db.First(&storage, f.library.StorageID).Error, db.First(&record, "operation_key = ?", key).Error} {
				if err != nil {
					t.Fatal(err)
				}
			}
			manifest := f.manifest
			expired := time.Now().Add(-time.Hour)
			manifest.RemoteExportExpiresAt = &expired // completion must not refresh it
			batches, err := buildRemoteUploadBatches(rows, storage, f.download, manifest)
			if err != nil || len(batches) != 1 {
				t.Fatal(err)
			}
			row := rows[0]
			file := nodeprotocol.StorageUploadFileResult{SourceFileToken: row.SourceFileToken, TargetName: "Movie (2024).mkv", TargetItemID: row.TargetItemID, TargetParentID: row.TargetParentID, Status: row.Status, Size: row.Size, SHA1: row.TargetSHA1}
			if row.TargetDigestJSON != "" {
				if err := json.Unmarshal([]byte(row.TargetDigestJSON), &file.ContentDigest); err != nil {
					t.Fatal(err)
				}
			}
			response := nodeprotocol.StorageActionResponse{RequestID: key, OperationKey: key, PlanDigest: record.PlanDigest, Status: nodeprotocol.StorageActionCompleted, TotalFiles: len(rows), CompletedFiles: len(rows), Files: []nodeprotocol.StorageUploadFileResult{file}}
			for _, tc := range []struct {
				name string
				edit func(*terminalRemoteResultClient)
			}{
				{"network_unknown", func(c *terminalRemoteResultClient) { c.err = errors.New("fixture unavailable") }},
				{"not_completed", func(c *terminalRemoteResultClient) { c.response.Status = nodeprotocol.StorageActionReconciliation }},
				{"missing_file", func(c *terminalRemoteResultClient) { c.response.Files = nil }},
				{"file_not_terminal", func(c *terminalRemoteResultClient) { c.response.Files[0].Status = nodeprotocol.StorageFilePending }},
				{"error_on_completed", func(c *terminalRemoteResultClient) { c.response.ErrorCode = nodeprotocol.ErrorReconciliationNeeded }},
				{"wrong_target", func(c *terminalRemoteResultClient) { c.response.Files[0].TargetItemID = "another-item" }},
				{"wrong_operation", func(c *terminalRemoteResultClient) { c.response.OperationKey = "foreign" }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					client := &terminalRemoteResultClient{key: key, task: f.download.ID, response: response}
					client.response.Files = append([]nodeprotocol.StorageUploadFileResult(nil), response.Files...)
					tc.edit(client)
					if err := worker.recoverTerminalRemoteUploadBatch(context.Background(), client, &task, f.download, rows, batches[0]); err == nil {
						t.Fatal("unconfirmed result completed")
					}
					var after models.RemoteOperation
					if err := db.First(&after, record.ID).Error; err != nil || after.Status != nodeprotocol.OperationRunning || after.LeaseEpoch != record.LeaseEpoch || after.PlanDigest != record.PlanDigest {
						t.Fatal("unknown result changed original operation")
					}
					if client.queries != 1 {
						t.Fatal("recovery did not perform exactly one original query")
					}
				})
			}
			cancelled := &terminalRemoteResultClient{key: key, task: f.download.ID, response: response, afterQuery: func() {
				if err := db.Model(&models.RemoteOperation{}).Where("id = ?", record.ID).Update("status", nodeprotocol.OperationCancelled).Error; err != nil {
					t.Fatal(err)
				}
			}}
			if err := worker.recoverTerminalRemoteUploadBatch(context.Background(), cancelled, &task, f.download, rows, batches[0]); err == nil {
				t.Fatal("late response revived cancelled operation")
			}
			var kept models.RemoteOperation
			if err := db.First(&kept, record.ID).Error; err != nil || kept.Status != nodeprotocol.OperationCancelled {
				t.Fatal("cancelled state overwritten")
			}
			if err := db.Model(&models.RemoteOperation{}).Where("id = ?", record.ID).Update("status", nodeprotocol.OperationRunning).Error; err != nil {
				t.Fatal(err)
			}
			changedLease := &terminalRemoteResultClient{key: key, task: f.download.ID, response: response, afterQuery: func() {
				if err := db.Model(&models.RemoteOperation{}).Where("id = ?", record.ID).Update("lease_epoch", record.LeaseEpoch+1).Error; err != nil {
					t.Fatal(err)
				}
			}}
			if err := worker.recoverTerminalRemoteUploadBatch(context.Background(), changedLease, &task, f.download, rows, batches[0]); err == nil {
				t.Fatal("late response crossed operation lease generation")
			}
			if err := db.First(&kept, record.ID).Error; err != nil || kept.LeaseEpoch != record.LeaseEpoch+1 || kept.Status != nodeprotocol.OperationRunning {
				t.Fatal("lease generation was overwritten")
			}
			if err := db.Model(&models.RemoteOperation{}).Where("id = ?", record.ID).Update("lease_epoch", record.LeaseEpoch).Error; err != nil {
				t.Fatal(err)
			}
			cancelledDownload := &terminalRemoteResultClient{key: key, task: f.download.ID, response: response, afterQuery: func() {
				if err := db.Model(&models.DownloadTask{}).Where("id = ?", f.download.ID).Update("phase", models.DownloadTaskStatusCancelled).Error; err != nil {
					t.Fatal(err)
				}
			}}
			if err := worker.recoverTerminalRemoteUploadBatch(context.Background(), cancelledDownload, &task, f.download, rows, batches[0]); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled download revived: %v", err)
			}
			if err := db.First(&kept, record.ID).Error; err != nil || kept.Status != nodeprotocol.OperationRunning {
				t.Fatal("cancelled download completed operation")
			}
			if err := db.Model(&models.DownloadTask{}).Where("id = ?", f.download.ID).Update("phase", f.download.Phase).Error; err != nil {
				t.Fatal(err)
			}
			changedManifest := &terminalRemoteResultClient{key: key, task: f.download.ID, response: response, afterQuery: func() {
				if err := db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Update("manifest_json", "{}").Error; err != nil {
					t.Fatal(err)
				}
			}}
			if err := worker.recoverTerminalRemoteUploadBatch(context.Background(), changedManifest, &task, f.download, rows, batches[0]); err == nil {
				t.Fatal("late result crossed frozen manifest")
			}
			if err := db.First(&kept, record.ID).Error; err != nil || kept.Status != nodeprotocol.OperationRunning {
				t.Fatal("changed manifest completed operation")
			}
			if err := db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Update("manifest_json", task.ManifestJSON).Error; err != nil {
				t.Fatal(err)
			}
			client := &terminalRemoteResultClient{key: key, task: f.download.ID, response: response}
			if err := worker.recoverTerminalRemoteUploadBatch(context.Background(), client, &task, f.download, rows, batches[0]); err != nil {
				t.Fatal(err)
			}
			if err := worker.recoverTerminalRemoteUploadBatch(context.Background(), client, &task, f.download, rows, batches[0]); err != nil {
				t.Fatal(err)
			}
			var after models.RemoteOperation
			if err := db.First(&after, record.ID).Error; err != nil || after.Status != nodeprotocol.OperationCompleted || after.Phase != nodeprotocol.PhaseCompleted || after.LeaseEpoch != record.LeaseEpoch || client.queries != 1 {
				t.Fatal("recovery repeated work or changed lease")
			}
			if task.CloudStateJSON != "" {
				t.Fatal("recovery fabricated Server cloud checkpoint")
			}
			// The old skip-terminal path could already have marked the transfer
			// completed. Original-result recovery must preserve that phase.
			if err := db.Model(&models.RemoteOperation{}).Where("id = ?", record.ID).Update("status", nodeprotocol.OperationRunning).Error; err != nil {
				t.Fatal(err)
			}
			task.Phase = models.TransferTaskStatusCompleted
			if err := db.Model(&models.TransferTask{}).Where("id = ?", task.ID).Update("phase", task.Phase).Error; err != nil {
				t.Fatal(err)
			}
			if err := worker.recoverCompletedRemoteUploadOperations(context.Background(), &task, f.download); err == nil {
				t.Fatal("missing original Node client silently completed")
			}
			if err := worker.recoverTerminalRemoteUploadBatch(context.Background(), client, &task, f.download, rows, batches[0]); err != nil {
				t.Fatal(err)
			}
			var saved models.TransferTask
			if err := db.First(&saved, "id = ?", task.ID).Error; err != nil || saved.Phase != models.TransferTaskStatusCompleted || task.Phase != saved.Phase || client.queries != 2 {
				t.Fatal("completion recovery regressed completed transfer phase")
			}
			if err := worker.recoverCompletedRemoteUploadOperations(context.Background(), &task, f.download); err != nil {
				t.Fatalf("confirmed completion tried to open a Node connection: %v", err)
			}
		})
	}
}
