package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
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

type fencedRemoteResultClient struct {
	terminalRemoteResultClient
	operation  nodeprotocol.OperationResponse
	beforeStop func()
	stops      int
	nodeID     string
	seals      int
}

func (c *fencedRemoteResultClient) Health(context.Context) (nodeprotocol.HealthResponse, error) {
	return nodeprotocol.HealthResponse{NodeID: c.nodeID, ProtocolMin: 1, ProtocolMax: 1, Capabilities: nodeprotocol.Capabilities{Codes: []string{nodeprotocol.CapabilityStorageUploadRetryV1}}}, nil
}

func (c *fencedRemoteResultClient) Operation(context.Context, string) (nodeprotocol.OperationResponse, error) {
	return c.operation, nil
}
func (c *fencedRemoteResultClient) RenewOperationLease(_ context.Context, key string, input nodeprotocol.LeaseRequest) (nodeprotocol.OperationResponse, error) {
	if key != c.operation.OperationKey || input.PlanDigest != c.operation.PlanDigest || input.LeaseEpoch < c.operation.LeaseEpoch {
		return nodeprotocol.OperationResponse{}, errors.New("node_lease_conflict")
	}
	c.renewals = append(c.renewals, input)
	if c.beforeRenew != nil {
		c.beforeRenew(input)
	}
	c.operation.LeaseEpoch, c.operation.LeaseExpiresAt = input.LeaseEpoch, input.LeaseExpiresAt
	c.operation.Revision++
	return c.operation, nil
}
func (c *fencedRemoteResultClient) StopOperation(_ context.Context, key string, input nodeprotocol.OperationStopRequest) (nodeprotocol.OperationResponse, error) {
	c.stops++
	if input.SealUploadRetry {
		c.seals++
	}
	if c.beforeStop != nil {
		c.beforeStop()
	}
	if key != c.operation.OperationKey || input.PlanDigest != c.operation.PlanDigest || input.LeaseEpoch != c.operation.LeaseEpoch {
		return nodeprotocol.OperationResponse{}, errors.New("node_lease_conflict")
	}
	return c.operation, nil
}

func TestRemoteUploadTerminalRowsRecoverOriginalResultAfterRestart(t *testing.T) {
	t.Run("explicit_linked_retry_preserves_original", func(t *testing.T) {
		f := newRemoteBatchFixture(t, cloud.ProviderPan115)
		f.queue = NewQueueService(f.queue.db, f.queue.audit)
		f.service.queue = f.queue
		worker := NewTransferWorker(f.service)
		var user models.User
		if err := f.queue.db.First(&user, f.download.OwnerID).Error; err != nil {
			t.Fatal(err)
		}
		actor := Actor{User: user, Permissions: map[string]struct{}{authz.PermissionJobsRespond: {}, authz.PermissionJobsReadAll: {}, authz.PermissionJobsControlAll: {}, authz.PermissionTransfersReadAll: {}, authz.PermissionDownloadsCreate: {}, authz.PermissionMediaLibrariesRead: {}, authz.PermissionDiscoveryRead: {}}}
		persistFixtureAuthority(t, f.queue.db, actor)
		task := f.task
		task.Phase = models.TransferTaskStatusTransferring
		task.FinishedAt = nil
		if err := f.queue.db.Model(&task).Updates(map[string]any{"phase": task.Phase, "finished_at": nil}).Error; err != nil {
			t.Fatal(err)
		}
		var storage models.Storage
		if err := f.queue.db.First(&storage, f.library.StorageID).Error; err != nil {
			t.Fatal(err)
		}
		batches, err := buildRemoteUploadBatches(f.rows, storage, f.download, f.manifest)
		if err != nil {
			t.Fatal(err)
		}
		record, err := remoteUploadBatchRecordTx(f.queue.db, task, f.download, batches[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := f.queue.db.Model(&record).Updates(map[string]any{"status": nodeprotocol.OperationRunning, "phase": nodeprotocol.PhaseVerifyingTarget}).Error; err != nil {
			t.Fatal(err)
		}
		claimed, err := f.queue.Claim([]string{"transfer"})
		if err != nil || claimed == nil {
			t.Fatalf("claim=%+v err=%v", claimed, err)
		}
		ctx := context.WithValue(context.Background(), remoteUploadClaimKey{}, *claimed)
		var proof models.CatalogPhysicalWrite
		if err := f.queue.db.First(&proof, "owner_kind=? AND owner_id=?", CatalogPhysicalTransfer, task.ID).Error; err != nil {
			t.Fatal(err)
		}
		proof.JobLeaseHash, proof.State = leaseHash(claimed.LeaseToken), "entered"
		if err := f.queue.db.Save(&proof).Error; err != nil {
			t.Fatal(err)
		}
		zero := 0
		inactive := false
		check := &nodeprotocol.StorageTargetCheck{Version: 1, Reason: "target_missing", Complete: true, NameMatches: &zero, SizeMatches: &zero, IdenticalMatches: &zero}
		response := nodeprotocol.StorageActionResponse{RequestID: record.OperationKey, OperationKey: record.OperationKey, PlanDigest: record.PlanDigest, Status: nodeprotocol.StorageActionReconciliation, Phase: nodeprotocol.PhaseVerifyingTarget, TotalFiles: len(f.rows), ExecutionActive: &inactive, ErrorCode: nodeprotocol.ErrorReconciliationNeeded, TargetCheck: check}
		client := &fencedRemoteResultClient{nodeID: *task.NodeID, operation: nodeprotocol.OperationResponse{OperationKey: record.OperationKey, PlanDigest: record.PlanDigest, LeaseEpoch: record.LeaseEpoch, Status: nodeprotocol.OperationRunning}, terminalRemoteResultClient: terminalRemoteResultClient{key: record.OperationKey, task: f.download.ID, response: response}}
		if err := worker.persistMeasuredUpload(ctx, &task, f.rows, f.rows, response); err != nil {
			t.Fatal(err)
		}
		// A completed Node receipt may have an older epoch than a reserved,
		// unacknowledged renewal. It needs a read, never a higher-epoch seal.
		if err := f.queue.db.Model(&record).Update("lease_epoch", record.LeaseEpoch+1).Error; err != nil {
			t.Fatal(err)
		}
		client.operation.Status = nodeprotocol.OperationCompleted
		client.response.Status = nodeprotocol.StorageActionCompleted
		client.response.CompletedFiles = len(f.rows)
		if wait, err := worker.waitForRemoteUploadRetry(ctx, client, task, f.download, batches, batches[0], remoteUploadStopProof{physical: proof}); err != nil || wait != nil || client.seals != 0 {
			t.Fatal("late completion required a higher-epoch seal", err)
		}
		if err := f.queue.db.Model(&record).Update("lease_epoch", client.operation.LeaseEpoch).Error; err != nil {
			t.Fatal(err)
		}
		client.operation.Status = nodeprotocol.OperationRunning
		client.response = response
		check.Complete = false
		if err := worker.persistMeasuredUpload(ctx, &task, f.rows, f.rows, response); err != nil {
			t.Fatal(err)
		}
		if wait, err := worker.waitForRemoteUploadRetry(ctx, client, task, f.download, batches, batches[0], remoteUploadStopProof{physical: proof}); err != nil || wait != nil || client.stops != 0 {
			t.Fatal("partial listing authorized a retry")
		}
		check.Complete = true
		if err := worker.persistMeasuredUpload(ctx, &task, f.rows, f.rows, response); err != nil {
			t.Fatal(err)
		}
		wait, err := worker.waitForRemoteUploadRetry(ctx, client, task, f.download, batches, batches[0], remoteUploadStopProof{physical: proof})
		if err != nil || wait == nil || wait.ActionType != "upload_retry" || client.seals != 1 {
			t.Fatalf("wait=%+v err=%v seals=%d", wait, err, client.seals)
		}
		if err := f.queue.Wait(task.JobID, claimed.LeaseToken, *wait); err != nil {
			t.Fatal(err)
		}
		page, err := f.service.List(actor, TransferListFilter{Scope: "active"})
		if err != nil || len(page.List) != 1 || !page.List[0].UploadRetryAllowed || page.List[0].UploadRetryVersion == 0 || page.List[0].RemoteTargetCheck == nil || page.List[0].RemoteTransferredBytes != nil {
			t.Fatalf("action projection=%+v err=%v", page, err)
		}
		version := page.List[0].UploadRetryVersion
		denied := actor
		denied.Permissions = map[string]struct{}{authz.PermissionTransfersReadAll: {}, authz.PermissionMediaLibrariesRead: {}, authz.PermissionJobsReadAll: {}}
		readOnly, err := f.service.List(denied, TransferListFilter{Scope: "active"})
		if err != nil || len(readOnly.List) != 1 || readOnly.List[0].UploadRetryAllowed || readOnly.List[0].UploadRetryVersion != 0 {
			t.Fatal("read-only actor obtained retry authority")
		}
		if _, err := f.queue.Respond(denied, task.JobID, version, "retry_upload", RequestContext{}); err == nil {
			t.Fatal("read-only actor responded")
		}
		if _, err := f.queue.Respond(actor, task.JobID, version, "retry_upload", RequestContext{}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.queue.Respond(actor, task.JobID, version, "retry_upload", RequestContext{}); err == nil {
			t.Fatal("duplicate response accepted")
		}
		claimed, err = f.queue.Claim([]string{"transfer"})
		if err != nil || claimed == nil {
			t.Fatalf("resume=%+v err=%v", claimed, err)
		}
		ctx = context.WithValue(context.Background(), remoteUploadClaimKey{}, *claimed)
		if err := worker.activateRemoteUploadRetry(ctx, client, task, f.download, batches); err != nil {
			t.Fatal(err)
		}
		if err := worker.activateRemoteUploadRetry(ctx, client, task, f.download, batches); err != nil {
			t.Fatal(err)
		}
		childKey, payload, err := remoteUploadBatchIdentityTx(f.queue.db, task, batches[0])
		if err != nil || payload.RetryOf == nil || payload.RetryOf.OperationKey != record.OperationKey || payload.RetryOf.ActionVersion != version || childKey == record.OperationKey {
			t.Fatalf("child=%s payload=%+v err=%v", childKey, payload, err)
		}
		_, child, _, err := worker.prepareRemoteUploadOperation(ctx, *task.NodeID, f.download.ID, childKey, payload)
		if err != nil {
			t.Fatal(err)
		}
		retained, err := remoteUploadBatchRecordTx(f.queue.db, task, f.download, batches[0])
		if err != nil || retained.ID != child.ID {
			t.Fatal("publication resolved a different retry generation")
		}
		var count int64
		if err := f.queue.db.Model(&models.RemoteOperation{}).Where("operation_key IN ?", []string{record.OperationKey, childKey}).Count(&count).Error; err != nil || count != 2 {
			t.Fatal("original operation was removed")
		}
		if err := worker.persistMeasuredUpload(ctx, &task, f.rows, f.rows, nodeprotocol.StorageActionResponse{Status: nodeprotocol.StorageActionCompleted}); err != nil {
			t.Fatal(err)
		}
		var projected transferProjectionRow
		projected.ExecutionLocation, projected.Phase, projected.RemoteErrorCode = models.NodeLocationRemote, models.TransferTaskStatusCompleted, nodeprotocol.ErrorReconciliationNeeded
		projected.UploadProgressJSON = task.UploadProgressJSON
		finalizeTransferProjection(&projected)
		if projected.RemoteTargetCheck != nil || projected.RemoteErrorCode != "" {
			t.Fatal("old reconciliation obscured completed output")
		}
	})
	for _, provider := range []string{cloud.ProviderQuark, cloud.ProviderPan115} {
		t.Run(provider, func(t *testing.T) {
			f := newRemoteBatchFixture(t, provider)
			var user models.User
			if err := f.queue.db.First(&user, f.download.OwnerID).Error; err != nil {
				t.Fatal(err)
			}
			persistFixtureAuthority(t, f.queue.db, Actor{User: user, Permissions: map[string]struct{}{authz.PermissionJobsControlAll: {}, authz.PermissionDownloadsCreate: {}, authz.PermissionMediaLibrariesRead: {}, authz.PermissionDiscoveryRead: {}}})
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
			runtime, err := database.AcquireExclusiveRuntime(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			db, err = runtime.Bind(db)
			if err != nil {
				t.Fatal(err)
			}
			service.db = db
			var task models.TransferTask
			var rows []models.RemoteUploadFile
			var storage models.Storage
			var record models.RemoteOperation
			for _, err := range []error{db.First(&task, "id = ?", f.task.ID).Error, db.Where("transfer_task_id = ?", f.task.ID).Order("ordinal").Find(&rows).Error, db.First(&storage, f.library.StorageID).Error, db.First(&record, "operation_key = ?", key).Error} {
				if err != nil {
					t.Fatal(err)
				}
			}
			// A control request deadline is independent of the live worker parent.
			requestError := remoteUploadClientError(context.DeadlineExceeded)
			if !errors.Is(requestError, context.DeadlineExceeded) {
				t.Fatal("request deadline lost its cause")
			}
			if pending := worker.completedTransferResult(task); pending.RetryAt == nil || pending.ErrorCode != "transfer_reconciliation_pending" {
				t.Fatalf("unfinished original was eligible for queue completion: %+v", pending)
			}
			var originalJob models.Job
			if err := db.First(&originalJob, "id=?", task.JobID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&models.Job{}).Where("id=?", task.JobID).Updates(map[string]any{"status": models.JobStatusCompleted, "finished_at": time.Now().UTC()}).Error; err != nil {
				t.Fatal(err)
			}
			service.queue = NewQueueService(db, service.audit)
			if _, err := worker.RecoverStoppedWork(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			var resumed models.Job
			if err := db.First(&resumed, "id=?", originalJob.ID).Error; err != nil || resumed.Status != models.JobStatusQueued || resumed.PayloadJSON != originalJob.PayloadJSON || resumed.ResourceKey != originalJob.ResourceKey || resumed.Revision != originalJob.Revision+1 || resumed.FinishedAt != nil {
				t.Fatalf("original false-completed job did not recover: %+v %v", resumed, err)
			}
			if _, err := worker.RecoverStoppedWork(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			// Recheck cancellation at the actual queue write, after a recovery
			// candidate and its frozen route have already been read.
			if err := db.Model(&models.Job{}).Where("id=?", task.JobID).Updates(map[string]any{"status": models.JobStatusCompleted, "finished_at": time.Now().UTC()}).Error; err != nil {
				t.Fatal(err)
			}
			callbackName := "fixture-cancel-recovery-before-cas"
			if err := db.Callback().Update().Before("gorm:update").Register(callbackName, func(tx *gorm.DB) {
				values, ok := tx.Statement.Dest.(map[string]interface{})
				if ok && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "jobs" && values["status"] == models.JobStatusQueued {
					tx.AddError(tx.Session(&gorm.Session{NewDB: true}).Model(&models.DownloadTask{}).Where("id=?", f.download.ID).Update("phase", models.DownloadTaskStatusCancelled).Error)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := worker.RecoverStoppedWork(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			db.Callback().Update().Remove(callbackName)
			var refused models.Job
			if err := db.First(&refused, "id=?", task.JobID).Error; err != nil || refused.Status != models.JobStatusCompleted {
				t.Fatal("late cancellation was overwritten by automatic recovery")
			}
			if err := db.Model(&models.DownloadTask{}).Where("id=?", f.download.ID).Update("phase", f.download.Phase).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&models.Job{}).Where("id=?", task.JobID).Updates(map[string]any{"status": models.JobStatusQueued, "finished_at": nil}).Error; err != nil {
				t.Fatal(err)
			}
			manifest := f.manifest
			expired := time.Now().Add(-time.Hour)
			manifest.RemoteExportExpiresAt = &expired // completion must not refresh it
			batches, err := buildRemoteUploadBatches(rows, storage, f.download, manifest)
			if err != nil || len(batches) != 1 {
				t.Fatal(err)
			}
			// Restart must not infer that an asynchronous Node writer exited just
			// because the original Server process/queue lease disappeared.
			expiry := time.Now().UTC().Add(time.Minute)
			ownerDigest := catalogPhysicalDigest(task.DownloadTaskID, task.SourceDataSourceJSON, task.TargetDataSourceJSON, task.RouteKind, "1")
			ownerDigest = catalogPhysicalDigest(ownerDigest, uintID(task.LibraryID), uintID(task.OwnerID))
			oldProof := models.CatalogPhysicalWrite{LibraryID: task.LibraryID, OwnerKind: CatalogPhysicalTransfer, OwnerID: task.ID, Revision: 1, State: "entered", JobID: task.JobID, JobLeaseHash: leaseHash("lease-A"), RuntimeID: "prior-process", OwnerDigest: ownerDigest, SourceFingerprint: "source", ConfigFingerprint: "config", EnteredAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			var admitted models.CatalogPhysicalWrite
			if err := db.First(&admitted, "owner_kind=? AND owner_id=?", CatalogPhysicalTransfer, task.ID).Error; err != nil {
				t.Fatal(err)
			}
			oldProof.ID = admitted.ID
			if err := db.Save(&oldProof).Error; err != nil {
				t.Fatal(err)
			}
			orphan := oldProof
			orphan.ID, orphan.OwnerID = 0, "orphan-"+task.ID
			if err := db.Create(&orphan).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := RecoverCatalogPhysicalRuntimeBatch(context.Background(), db); err != nil {
				t.Fatal(err)
			}
			if err := db.First(&orphan, orphan.ID).Error; err != nil || orphan.State != "entered" {
				t.Fatal("missing Transfer domain was mistaken for local process exit")
			}
			if err := db.Delete(&orphan).Error; err != nil {
				t.Fatal(err)
			}
			var physical models.CatalogPhysicalWrite
			if err := db.First(&physical, oldProof.ID).Error; err != nil || physical.State != "entered" {
				t.Fatal("Server restart falsely joined Node writer")
			}
			if err := db.Model(&models.Job{}).Where("id=?", task.JobID).Updates(map[string]any{"status": models.JobStatusRunning, "lease_token_hash": leaseHash("lease-A"), "lease_expires_at": expiry}).Error; err != nil {
				t.Fatal(err)
			}
			claimedA := ClaimedJob{Job: resumed, LeaseToken: "lease-A"}
			stoppedClient := &fencedRemoteResultClient{terminalRemoteResultClient: terminalRemoteResultClient{key: key, task: f.download.ID, response: nodeprotocol.StorageActionResponse{RequestID: key, OperationKey: key, PlanDigest: record.PlanDigest, Status: nodeprotocol.StorageActionRunning, TotalFiles: len(rows)}}, operation: nodeprotocol.OperationResponse{OperationKey: key, PlanDigest: record.PlanDigest, Status: nodeprotocol.OperationRunning, LeaseEpoch: record.LeaseEpoch}}
			if err := worker.stopRemoteUploadExecution(context.Background(), stoppedClient, task, f.download, batches, remoteUploadStopProof{physical: oldProof}); err == nil {
				t.Fatal("old Node without execution-exit proof quiesced")
			}
			// The old A stop was admitted, then B acquired a newer physical/Node epoch.
			// The Node CAS rejects the delayed control RPC before it can affect B.
			stoppedClient.beforeStop = func() {
				stoppedClient.operation.LeaseEpoch = record.LeaseEpoch + 1
				if err := db.Model(&models.CatalogPhysicalWrite{}).Where("id=?", oldProof.ID).Updates(map[string]any{"revision": 2, "job_lease_hash": leaseHash("lease-B")}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&models.RemoteOperation{}).Where("id=?", record.ID).Update("lease_epoch", record.LeaseEpoch+1).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&models.Job{}).Where("id=?", task.JobID).Update("lease_token_hash", leaseHash("lease-B")).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := worker.stopRemoteUploadExecution(context.Background(), stoppedClient, task, f.download, batches, remoteUploadStopProof{physical: oldProof}); err == nil {
				t.Fatal("late A stop affected B")
			}
			if _, err := worker.captureRemoteUploadStopProof(context.Background(), task, &claimedA, false); err == nil {
				t.Fatal("late A interrupt adopted B witness")
			}
			if _, err := worker.captureRemoteUploadStopProof(context.Background(), task, &claimedA, true); err == nil {
				t.Fatal("late A recovery adopted B witness")
			}
			if err := db.Transaction(func(tx *gorm.DB) error { return quiesceRemoteUploadProofTx(tx, oldProof) }); err == nil {
				t.Fatal("late A exit ACK overwrote B")
			}
			staleContext := context.WithValue(context.Background(), remoteUploadClaimKey{}, claimedA)
			staleManifest := manifest
			if err := worker.refreshRemoteUploadExport(staleContext, &task, f.download, &staleManifest); ErrorCode(err) != CodeQueueLeaseInvalid {
				t.Fatalf("late A refreshed the original export after B claimed it: %v", err)
			}
			if err := worker.persistRemoteOperationState(staleContext, record, nodeprotocol.OperationCompleted, nodeprotocol.PhaseCompleted, "", nil); err == nil {
				t.Fatal("old receipt crossed new operation epoch/claim")
			}
			if err := worker.persistRemoteUploadProgress(staleContext, &task, rows, models.TransferTaskStatusPlanning); err == nil {
				t.Fatal("old progress overwrote new claimant")
			}
			worker.cloudFailure(task, cloudTransferError("old_request_failed", false, nil), staleContext)
			var preserved models.TransferTask
			if err := db.First(&preserved, "id=?", task.ID).Error; err != nil || preserved.Phase != models.TransferTaskStatusTransferring {
				t.Fatal("old failure regressed successor transfer")
			}

			stoppedClient.beforeStop = nil
			active := false
			stoppedClient.response.ExecutionActive = &active
			// A durable queued pause is claimed with a new token C. It must be able to
			// join retained predecessor B, without allowing obsolete A to control C.
			if err := db.Model(&models.Job{}).Where("id=?", task.JobID).Updates(map[string]any{"lease_token_hash": leaseHash("lease-C"), "interrupt_status": models.JobStatusPaused}).Error; err != nil {
				t.Fatal(err)
			}
			claimedB := ClaimedJob{Job: resumed, LeaseToken: "lease-C"}
			controlProof, err := worker.captureRemoteUploadStopProof(context.Background(), task, &claimedB, false)
			if err != nil || controlProof.physical.JobLeaseHash != leaseHash("lease-B") {
				t.Fatalf("new durable control could not join its retained predecessor: %v", err)
			}
			// A renewal was reserved but never reached the Node. Seal all issued
			// epochs before treating the old Node's stop ACK as an execution exit.
			issued := stoppedClient.operation.LeaseEpoch + 1
			if err := db.Model(&models.RemoteOperation{}).Where("id=?", record.ID).Update("lease_epoch", issued).Error; err != nil {
				t.Fatal(err)
			}
			stoppedClient.beforeRenew = func(input nodeprotocol.LeaseRequest) {
				var retained models.CatalogPhysicalWrite
				var operation models.RemoteOperation
				if input.LeaseEpoch != issued+1 || input.LeaseExpiresAt.After(time.Now()) || db.First(&retained, controlProof.physical.ID).Error != nil || retained.State != "entered" || db.First(&operation, record.ID).Error != nil || operation.LeaseEpoch != input.LeaseEpoch {
					t.Fatal("stop barrier was not reserved under retained entered ownership")
				}
			}
			stopsBeforeGap := stoppedClient.stops
			if err := worker.stopRemoteUploadExecution(context.Background(), stoppedClient, task, f.download, batches, controlProof); err != nil {
				t.Fatal(err)
			}
			if len(stoppedClient.renewals) != 1 || stoppedClient.stops-stopsBeforeGap != 2 || stoppedClient.operation.LeaseEpoch != issued+1 {
				t.Fatal("unknown renewal did not converge through a unique stop barrier")
			}
			stoppedClient.beforeRenew = nil
			if _, err := stoppedClient.RenewOperationLease(context.Background(), key, nodeprotocol.LeaseRequest{PlanDigest: record.PlanDigest, LeaseEpoch: issued, LeaseExpiresAt: time.Now().Add(time.Minute)}); err == nil {
				t.Fatal("late unknown renewal reopened the stopped operation")
			}
			if err := db.Transaction(func(tx *gorm.DB) error { return quiesceRemoteUploadProofTx(tx, controlProof.physical) }); err != nil {
				t.Fatal(err)
			}
			if _, err := worker.captureRemoteUploadStopProof(context.Background(), task, &claimedA, false); err == nil {
				t.Fatal("old A adopted durable control claim C")
			}
			if err := db.Model(&models.Job{}).Where("id=?", task.JobID).Update("interrupt_status", "").Error; err != nil {
				t.Fatal(err)
			}
			recoveryContext := context.WithValue(context.Background(), remoteUploadClaimKey{}, claimedB)
			if err := worker.recoverRemoteUploadPermit(recoveryContext, stoppedClient, task, f.download, batches); err != nil {
				t.Fatal(err)
			}
			if err := db.First(&physical, oldProof.ID).Error; err != nil || physical.State != "quiescent" || physical.Revision != 2 {
				t.Fatal("exact predecessor Node exit was not recovered")
			}
			if err := db.Model(&models.Job{}).Where("id=?", task.JobID).Updates(map[string]any{"status": models.JobStatusQueued, "lease_token_hash": "", "lease_expires_at": nil}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&models.RemoteOperation{}).Where("id=?", record.ID).Update("lease_epoch", record.LeaseEpoch).Error; err != nil {
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
			now := time.Now().UTC()
			task.FinishedAt = &now
			if err := db.Model(&models.TransferTask{}).Where("id=?", task.ID).Update("finished_at", now).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&models.Job{}).Where("id=?", task.JobID).Updates(map[string]any{"status": models.JobStatusCompleted, "finished_at": now}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Transaction(func(tx *gorm.DB) error {
				return settleStoppedRemoteUploadProofTx(tx, physical, task, f.download, batches)
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.First(&physical, physical.ID).Error; err != nil || physical.State != "settled" {
				t.Fatal("verified terminal upload left old physical ownership stuck")
			}
		})
	}
}
