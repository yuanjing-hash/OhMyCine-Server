package services

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/nodeclient"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

type remoteUploadControlClient interface {
	remoteUploadNodeClient
	Operation(context.Context, string) (nodeprotocol.OperationResponse, error)
	StopOperation(context.Context, string, nodeprotocol.OperationStopRequest) (nodeprotocol.OperationResponse, error)
}

type remoteUploadClaimKey struct{}

// A control witness is obtained from the original process permit or by a
// transaction that authorizes this caller's claim. Cleanup never adopts a new
// owner's witness merely because it is the current row.
type remoteUploadStopProof struct{ physical models.CatalogPhysicalWrite }

func (w *TransferWorker) captureRemoteUploadStopProof(ctx context.Context, task models.TransferTask, claim *ClaimedJob, predecessor bool) (remoteUploadStopProof, error) {
	var proof remoteUploadStopProof
	err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("owner_kind=? AND owner_id=? AND state IN ('entered','quiescent')", CatalogPhysicalTransfer, task.ID).First(&proof.physical).Error; err != nil {
			return err
		}
		if proof.physical.JobID != task.JobID || proof.physical.LibraryID != task.LibraryID {
			return ErrCatalogFence
		}
		var job models.Job
		if err := tx.First(&job, "id=?", task.JobID).Error; err != nil {
			return err
		}
		if claim == nil {
			if job.LeaseTokenHash != "" || job.LeaseExpiresAt != nil || job.InterruptStatus != "" || !containsString([]string{models.JobStatusPaused, models.JobStatusCancelled, models.JobStatusFailed, models.JobStatusCompleted}, job.Status) {
				return ErrCatalogFence
			}
			return nil
		}
		if claim.Job.ID != job.ID || claim.Job.Generation != job.Generation || claim.LeaseToken == "" {
			return ErrCatalogFence
		}
		hash := leaseHash(claim.LeaseToken)
		if predecessor {
			if job.Status != models.JobStatusRunning || job.InterruptStatus != "" || job.CancellationAsked || job.LeaseTokenHash != hash || job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(time.Now().UTC()) {
				return ErrCatalogFence
			}
		} else {
			liveClaim := job.Status == models.JobStatusRunning && job.LeaseTokenHash == hash && job.LeaseExpiresAt != nil && job.LeaseExpiresAt.After(time.Now().UTC())
			durableControl := job.InterruptStatus == models.JobStatusPaused || job.InterruptStatus == models.JobStatusCancelled
			if proof.physical.JobLeaseHash != hash {
				// A newly claimed durable pause/cancel is allowed to stop the retained
				// predecessor, while the obsolete claimant cannot adopt the new claim.
				if !liveClaim || !durableControl {
					return ErrCatalogFence
				}
			} else if job.LeaseTokenHash != "" && job.LeaseTokenHash != hash {
				return ErrCatalogFence
			}
		}
		return nil
	})
	return proof, err
}

func (w *TransferWorker) remoteUploadJobActive(ctx context.Context, jobID string) error {
	var job models.Job
	if err := w.service.db.WithContext(ctx).First(&job, "id=?", jobID).Error; err != nil {
		return err
	}
	if claim, ok := ctx.Value(remoteUploadClaimKey{}).(ClaimedJob); ok {
		if job.ID != claim.Job.ID || job.LeaseTokenHash != leaseHash(claim.LeaseToken) || job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(time.Now().UTC()) || job.Generation != claim.Job.Generation {
			return context.Canceled
		}
	}
	if job.Status != models.JobStatusRunning || job.InterruptStatus != "" || job.CancellationAsked {
		return context.Canceled
	}
	return ctx.Err()
}

func assertRemoteUploadStopProofTx(tx *gorm.DB, proof models.CatalogPhysicalWrite) error {
	if proof.ID == 0 {
		return ErrCatalogFence
	}
	var retained int64
	if err := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND revision=? AND job_id=? AND job_lease_hash=? AND runtime_id=? AND claim_digest=? AND owner_digest=? AND state IN ('entered','quiescent')", proof.ID, proof.Revision, proof.JobID, proof.JobLeaseHash, proof.RuntimeID, proof.ClaimDigest, proof.OwnerDigest).Count(&retained).Error; err != nil {
		return err
	}
	if retained != 1 {
		return ErrCatalogFence
	}
	return nil
}

// Joining the HTTP call alone does not join an asynchronous Node upload. Stop
// its original authority and require a current-process execution-exit receipt.
func (w *TransferWorker) stopRemoteUploadExecution(ctx context.Context, raw remoteUploadNodeClient, task models.TransferTask, download models.DownloadTask, batches []remoteUploadBatch, witness remoteUploadStopProof) error {
	proof := witness.physical
	client, ok := raw.(remoteUploadControlClient)
	if !ok {
		return cloudTransferError(nodeprotocol.ErrorCapabilityMissing, true, nil)
	}
	var job models.Job
	if err := w.service.db.WithContext(ctx).First(&job, "id=?", task.JobID).Error; err != nil {
		return err
	}
	var currentDownload models.DownloadTask
	if err := w.service.db.WithContext(ctx).First(&currentDownload, "id=?", download.ID).Error; err != nil {
		return err
	}
	cancelling := job.Status == models.JobStatusCancelled || job.InterruptStatus == models.JobStatusCancelled || currentDownload.Phase == models.DownloadTaskStatusCancelled
	for _, batch := range batches {
		var record models.RemoteOperation
		// Capture the issued epoch while the exact original physical owner is still
		// current. A later handoff invalidates this witness before any control RPC.
		err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := assertRemoteUploadStopProofTx(tx, proof); err != nil {
				return err
			}
			var err error
			record, err = remoteUploadBatchRecordTx(tx, task, download, batch)
			return err
		})
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				continue
			}
			var count int64
			key, _, identityErr := remoteUploadBatchIdentityTx(w.service.db.WithContext(ctx), task, batch)
			if !errors.Is(err, ErrCatalogFence) && identityErr == nil && w.service.db.WithContext(ctx).Model(&models.RemoteOperation{}).Where("operation_key=?", key).Count(&count).Error == nil && count == 0 {
				continue
			}
			return err
		}
		remote, err := client.Operation(ctx, record.OperationKey)
		var unavailable *nodeclient.RemoteError
		if errors.As(err, &unavailable) && unavailable.Code == "node_operation_not_found" {
			continue
		}
		if err != nil || remote.OperationKey != record.OperationKey || remote.PlanDigest != record.PlanDigest {
			return firstNonNil(err, errors.New(nodeprotocol.ErrorPlanConflict))
		}
		if remote.Status == nodeprotocol.OperationCompleted {
			continue
		}
		// Never adopt a newer Node owner. A lower epoch can instead be an issued
		// renewal whose request/ACK was lost before it reached the Node.
		if remote.LeaseEpoch == 0 || remote.LeaseEpoch > record.LeaseEpoch {
			return ErrCatalogFence
		}
		stopEpoch := record.LeaseEpoch
		if remote.LeaseEpoch < record.LeaseEpoch {
			// This first CAS checks the live capability, including when the cached
			// enrollment still describes an older Node. It is not an exit ACK.
			probe, err := client.StopOperation(ctx, record.OperationKey, nodeprotocol.OperationStopRequest{PlanDigest: record.PlanDigest, LeaseEpoch: remote.LeaseEpoch})
			if err != nil {
				return err
			}
			if probe.OperationKey != record.OperationKey || probe.PlanDigest != record.PlanDigest || probe.LeaseEpoch != remote.LeaseEpoch {
				return ErrCatalogFence
			}
			if probe.Status == nodeprotocol.OperationCompleted {
				continue
			}
			if probe.Status == nodeprotocol.OperationCancelled || probe.Status == nodeprotocol.OperationFailed {
				stopEpoch = probe.LeaseEpoch // terminal status already seals renewals
			} else {
				if record.LeaseEpoch == ^uint64(0) {
					return ErrCatalogFence
				}
				stopEpoch = record.LeaseEpoch + 1
				if err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					if err := assertRemoteUploadStopProofTx(tx, proof); err != nil {
						return err
					}
					result := tx.Model(&models.RemoteOperation{}).Where("id=? AND plan_digest=? AND lease_epoch=? AND status NOT IN ?", record.ID, record.PlanDigest, record.LeaseEpoch, []string{nodeprotocol.OperationCompleted, nodeprotocol.OperationFailed, nodeprotocol.OperationCancelled}).Updates(map[string]any{"lease_epoch": stopEpoch, "updated_at": time.Now().UTC()})
					if result.Error != nil || result.RowsAffected != 1 {
						return firstNonNil(result.Error, ErrCatalogFence)
					}
					return nil
				}); err != nil {
					return err
				}
				record.LeaseEpoch = stopEpoch
				barrier, err := client.RenewOperationLease(ctx, record.OperationKey, nodeprotocol.LeaseRequest{PlanDigest: record.PlanDigest, LeaseEpoch: stopEpoch, LeaseExpiresAt: time.Now().UTC()})
				if err != nil {
					return err
				}
				if err := validateRemoteOperationResponse(barrier, record); err != nil {
					return err
				}
				if barrier.Status == nodeprotocol.OperationCompleted {
					continue
				}
			}
		}
		if err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := assertRemoteUploadStopProofTx(tx, proof); err != nil {
				return err
			}
			var retained int64
			if err := tx.Model(&models.RemoteOperation{}).Where("id=? AND plan_digest=? AND lease_epoch=?", record.ID, record.PlanDigest, record.LeaseEpoch).Count(&retained).Error; err != nil {
				return err
			}
			if retained != 1 {
				return ErrCatalogFence
			}
			return nil
		}); err != nil {
			return err
		}
		stopped, err := client.StopOperation(ctx, record.OperationKey, nodeprotocol.OperationStopRequest{PlanDigest: record.PlanDigest, LeaseEpoch: stopEpoch, Cancel: cancelling})
		if err != nil {
			return err
		}
		if stopped.OperationKey != record.OperationKey || stopped.PlanDigest != record.PlanDigest || stopped.LeaseEpoch != stopEpoch {
			return ErrCatalogFence
		}
		for {
			response, err := client.StorageActionResult(ctx, record.OperationKey, download.ID, record.OperationKey)
			var missing *nodeclient.RemoteError
			if errors.As(err, &missing) && missing.Code == "node_storage_result_not_found" {
				break // Receipt creation precedes every storage execution.
			}
			if err != nil {
				return err
			}
			request := nodeprotocol.StorageActionRequest{RequestID: record.OperationKey, OperationKey: record.OperationKey, PlanDigest: record.PlanDigest}
			if err := validateStorageActionResponse(response, request, len(batch.rows)); err != nil {
				return err
			}
			if response.Status == nodeprotocol.StorageActionCompleted || response.ExecutionActive != nil && !*response.ExecutionActive {
				break
			}
			if response.ExecutionActive == nil {
				return cloudTransferError("node_execution_exit_unknown", true, nil)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return nil
}

func (w *TransferWorker) recoverRemoteUploadPermit(ctx context.Context, client remoteUploadNodeClient, task models.TransferTask, download models.DownloadTask, batches []remoteUploadBatch) error {
	claim, ok := ctx.Value(remoteUploadClaimKey{}).(ClaimedJob)
	if !ok {
		return ErrCatalogFence
	}
	previous, err := w.captureRemoteUploadStopProof(ctx, task, &claim, true)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := w.stopRemoteUploadExecution(ctx, client, task, download, batches, previous); err != nil {
		return err
	}
	// A fresh, same-operation Node exit plus the exact retained physical row
	// supplies the missing external-call join. The ledger still verifies runtime,
	// revision and original owner before acknowledging the predecessor.
	return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return quiesceRemoteUploadProofTx(tx, previous.physical)
	})
}

func (w *TransferWorker) interruptRemoteUpload(ctx context.Context, task models.TransferTask, download models.DownloadTask, claim *ClaimedJob) error {
	var rows []models.RemoteUploadFile
	if err := w.service.db.WithContext(ctx).Where("transfer_task_id=?", task.ID).Order("ordinal").Find(&rows).Error; err != nil || len(rows) == 0 {
		return err
	}
	if w.service.downloader == nil || w.service.downloader.transferNodes == nil || task.NodeID == nil || download.TargetStorageID == nil {
		return cloudTransferError(nodeprotocol.ErrorNodeOffline, true, nil)
	}
	client, _, err := w.service.downloader.transferNodes.nodeClient(*task.NodeID)
	if err != nil {
		return err
	}
	var manifest downloadpkg.Manifest
	var storage models.Storage
	if json.Unmarshal([]byte(task.ManifestJSON), &manifest) != nil {
		return ErrCatalogInvalid
	}
	if err := w.service.db.WithContext(ctx).First(&storage, *download.TargetStorageID).Error; err != nil {
		return err
	}
	batches, err := buildRemoteUploadBatches(rows, storage, download, manifest)
	if err != nil {
		return err
	}
	proof, err := w.captureRemoteUploadStopProof(ctx, task, claim, false)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := w.stopRemoteUploadExecution(ctx, client, task, download, batches, proof); err != nil {
		return err
	}
	if task.Phase == models.TransferTaskStatusCompleted {
		if err := w.recoverCompletedRemoteUploadOperations(ctx, &task, download); err != nil {
			return err
		}
	}
	return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := quiesceRemoteUploadProofTx(tx, proof.physical); err != nil {
			return err
		}
		if task.Phase != models.TransferTaskStatusCompleted {
			return nil
		}
		return settleStoppedRemoteUploadProofTx(tx, proof.physical, task, download, batches)
	})
}

// Only a fresh exact-operation Node exit proof may acknowledge a predecessor
// runtime. Generic physical admission continues to reject reconstructed permits.
func quiesceRemoteUploadProofTx(tx *gorm.DB, proof models.CatalogPhysicalWrite) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	runtimeID, err := database.ExclusiveRuntimeID(tx)
	if err != nil {
		return err
	}
	if runtimeID == "" {
		return ErrCatalogInvalid
	}
	if proof.ID == 0 || proof.OwnerKind != CatalogPhysicalTransfer || proof.JobID == "" || proof.RuntimeID == "" {
		return ErrCatalogFence
	}
	result := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND owner_kind=? AND owner_id=? AND library_id=? AND revision=? AND job_id=? AND job_lease_hash=? AND runtime_id=? AND claim_digest=? AND owner_digest=? AND state IN ('entered','quiescent')", proof.ID, proof.OwnerKind, proof.OwnerID, proof.LibraryID, proof.Revision, proof.JobID, proof.JobLeaseHash, proof.RuntimeID, proof.ClaimDigest, proof.OwnerDigest).Updates(map[string]any{"state": "quiescent", "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCatalogFence
	}
	return nil
}

// Terminal queue state alone is not completion. This path is available only
// after the exact asynchronous writer joined and every original file/batch has
// its verified completion receipt, including crash-before-final-ledger-ACK.
func settleStoppedRemoteUploadProofTx(tx *gorm.DB, proof models.CatalogPhysicalWrite, task models.TransferTask, download models.DownloadTask, batches []remoteUploadBatch) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	var current models.TransferTask
	var job models.Job
	if tx.First(&current, "id=?", task.ID).Error != nil || tx.First(&job, "id=?", task.JobID).Error != nil || !terminalLeaseFreeCatalogJob(job) || current.JobID != proof.JobID || current.DownloadTaskID != download.ID || current.ManifestJSON != task.ManifestJSON || current.Phase != models.TransferTaskStatusCompleted || current.FinishedAt == nil {
		return ErrCatalogFence
	}
	owner, err := catalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: proof.LibraryID, OwnerKind: proof.OwnerKind, OwnerID: proof.OwnerID}, true, false)
	if err != nil || owner.digest != proof.OwnerDigest || owner.jobID != proof.JobID {
		return ErrCatalogFence
	}
	for _, batch := range batches {
		if !remoteUploadRowsTerminal(batch.rows) {
			return ErrCatalogFence
		}
		record, err := remoteUploadBatchRecordTx(tx, current, download, batch)
		if err != nil || record.Status != nodeprotocol.OperationCompleted || record.Phase != nodeprotocol.PhaseCompleted || record.ErrorCode != "" {
			return ErrCatalogFence
		}
	}
	now := time.Now().UTC()
	result := tx.Model(&models.CatalogPhysicalWrite{}).Where("id=? AND revision=? AND state='quiescent' AND runtime_id=? AND job_lease_hash=? AND owner_digest=?", proof.ID, proof.Revision, proof.RuntimeID, proof.JobLeaseHash, proof.OwnerDigest).Updates(map[string]any{"state": "settled", "settled_at": now, "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrCatalogFence
	}
	return nil
}

// Every write from a live upload worker is bound to the same queue claimant.
// Recovery without a live claimant uses its stricter frozen result transaction.
func fenceRemoteUploadClaim(query *gorm.DB, ctx context.Context) *gorm.DB {
	if ctx == nil {
		return query
	}
	claim, ok := ctx.Value(remoteUploadClaimKey{}).(ClaimedJob)
	if !ok {
		return query
	}
	return query.Where("EXISTS(SELECT 1 FROM jobs upload_owner WHERE upload_owner.id=? AND upload_owner.status=? AND upload_owner.lease_token_hash=? AND upload_owner.generation=? AND upload_owner.lease_expires_at>? AND upload_owner.interrupt_status='' AND upload_owner.cancellation_asked=0)", claim.Job.ID, models.JobStatusRunning, leaseHash(claim.LeaseToken), claim.Job.Generation, time.Now().UTC())
}
func assertRemoteUploadClaimTx(tx *gorm.DB, ctx context.Context) error {
	claim, ok := ctx.Value(remoteUploadClaimKey{}).(ClaimedJob)
	if !ok {
		return nil
	}
	var count int64
	if err := fenceRemoteUploadClaim(tx.Model(&models.Job{}).Where("id=?", claim.Job.ID), ctx).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return appError(CodeQueueLeaseInvalid, "原上传执行租约已变更", nil)
	}
	return nil
}
