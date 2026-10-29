package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type MediaVersionUpgradeWorker struct{ service *MediaVersionUpgradeService }

func NewMediaVersionUpgradeWorker(service *MediaVersionUpgradeService) *MediaVersionUpgradeWorker {
	return &MediaVersionUpgradeWorker{service: service}
}
func (w *MediaVersionUpgradeWorker) Run(ctx context.Context, runtime JobRuntime, job ClaimedJob) WorkerResult {
	var payload struct {
		ID string `json:"upgrade_id"`
	}
	if json.Unmarshal([]byte(job.Job.PayloadJSON), &payload) != nil || payload.ID == "" {
		return WorkerResult{ErrorCode: CodeInvalidRequest, ErrorMessage: "洗版任务参数无效"}
	}
	return w.service.run(ctx, runtime, job, payload.ID)
}
func (s *MediaVersionUpgradeService) deferUpgrade(id, stage, code string) WorkerResult {
	next := s.now().Add(30 * time.Second)
	_ = s.db.Model(&models.MediaVersionUpgrade{}).Where("id=? AND state NOT IN ?", id, []string{"completed", "failed", "cancelled"}).Updates(map[string]any{"stage": stage, "error_code": code, "next_attempt_at": next, "updated_at": s.now()}).Error
	return WorkerResult{RetryAt: &next, ErrorCode: code, ErrorMessage: mediaUpgradePublicError(code)}
}
func (s *MediaVersionUpgradeService) failUpgrade(id, code string) WorkerResult {
	_ = s.db.Model(&models.MediaVersionUpgrade{}).Where("id=?", id).Updates(map[string]any{"state": "failed", "error_code": code, "next_attempt_at": nil, "revision": gorm.Expr("revision+1"), "updated_at": s.now()}).Error
	return WorkerResult{ErrorCode: code, ErrorMessage: mediaUpgradePublicError(code)}
}
func (s *MediaVersionUpgradeService) run(ctx context.Context, runtime JobRuntime, job ClaimedJob, id string) WorkerResult {
	var row models.MediaVersionUpgrade
	if err := s.db.WithContext(ctx).First(&row, "id=?", id).Error; err != nil {
		return WorkerResult{ErrorCode: CodeNotFound, ErrorMessage: "洗版任务不存在"}
	}
	if row.JobID != job.Job.ID || job.Job.OwnerID == nil || *job.Job.OwnerID != row.OwnerID {
		return WorkerResult{ErrorCode: CodePermissionDenied, ErrorMessage: "洗版任务身份不匹配"}
	}
	if row.State == "completed" {
		return WorkerResult{}
	}
	if row.State == "failed" || row.State == "cancelled" {
		return WorkerResult{ErrorCode: row.ErrorCode, ErrorMessage: mediaUpgradePublicError(row.ErrorCode)}
	}
	snapshot, err := decodeMediaUpgradeSnapshot(row.SnapshotJSON)
	if err != nil {
		return s.failUpgrade(id, CodeMediaUpgradeChanged)
	}
	if err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := catalogCheckJob(tx, models.CatalogSnapshot{JobID: &job.Job.ID, JobLeaseHash: leaseHash(job.LeaseToken)}, s.now()); err != nil {
			return err
		}
		return s.validateSnapshotTx(tx, row, snapshot)
	}); err != nil {
		return s.failUpgrade(id, CodeMediaUpgradeChanged)
	}
	if err = runtime.Heartbeat(nil, nil, nil, nil, nil); err != nil {
		return WorkerResult{ErrorCode: CodeQueueLeaseInvalid, ErrorMessage: "洗版任务租约已失效"}
	}
	var download models.DownloadTask
	if err = s.db.First(&download, "id=? AND owner_id=?", row.DownloadTaskID, row.OwnerID).Error; err != nil {
		return s.failUpgrade(id, "media_upgrade_replacement_failed")
	}
	if download.Phase == models.DownloadTaskStatusCancelled || download.Phase == models.DownloadTaskStatusFailed {
		return s.failUpgrade(id, "media_upgrade_replacement_failed")
	}
	var transfer models.TransferTask
	err = s.db.Where("download_task_id=? AND library_id=?", download.ID, row.LibraryID).First(&transfer).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.deferUpgrade(id, "download_import", "")
	}
	if err != nil {
		return s.deferUpgrade(id, "download_import", "media_upgrade_state_unavailable")
	}
	if transfer.Phase == models.TransferTaskStatusFailed || transfer.Phase == "cancelled" {
		return s.failUpgrade(id, "media_upgrade_replacement_failed")
	}
	if transfer.Phase != models.TransferTaskStatusCompleted || transfer.CatalogPublishedAt == nil {
		return s.deferUpgrade(id, "catalog_publish", "")
	}
	if row.TransferTaskID != "" && row.TransferTaskID != transfer.ID {
		return s.failUpgrade(id, CodeMediaUpgradeChanged)
	}
	if err = s.db.Model(&row).Update("transfer_task_id", transfer.ID).Error; err != nil {
		return s.deferUpgrade(id, "catalog_publish", "media_upgrade_state_unavailable")
	}
	row.TransferTaskID = transfer.ID
	output, ready, err := s.replacementReady(ctx, row, snapshot, transfer)
	if err != nil {
		if ErrorCode(err) == CodeMediaUpgradeChanged || ErrorCode(err) == CodeMediaUpgradeSelection {
			return s.failUpgrade(id, ErrorCode(err))
		}
		return s.deferUpgrade(id, "replacement_artifacts", "media_upgrade_artifacts_pending")
	}
	if !ready {
		return s.deferUpgrade(id, "replacement_artifacts", "media_upgrade_artifacts_pending")
	}
	raw, _ := json.Marshal(output)
	if err = s.db.Model(&row).Updates(map[string]any{"state": "replacement_ready", "stage": "recycle_old", "output_json": string(raw), "error_code": "", "updated_at": s.now()}).Error; err != nil {
		return s.deferUpgrade(id, "replacement_artifacts", "media_upgrade_state_unavailable")
	}
	row.State, row.OutputJSON = "replacement_ready", string(raw)
	if err = s.recycleOld(ctx, runtime, job, row, snapshot); err != nil {
		_ = s.db.Model(&row).Updates(map[string]any{"state": "recycle_pending", "updated_at": s.now()}).Error
		code := CodeMediaUpgradePending
		if ErrorCode(err) == CodeMediaUpgradeSeedingBusy {
			code = CodeMediaUpgradeSeedingBusy
		}
		return s.deferUpgrade(id, "recycle_old", code)
	}
	return WorkerResult{}
}

// Exact receipt -> exact managed output -> exact visible catalog rows -> exact
// generation binding. A different work/version or a globally-ready library is
// never evidence that THIS replacement is ready.
func (s *MediaVersionUpgradeService) replacementReady(ctx context.Context, row models.MediaVersionUpgrade, snapshot mediaUpgradeSnapshot, transfer models.TransferTask) (mediaUpgradeOutput, bool, error) {
	var output mediaUpgradeOutput
	output.PhysicalIdentities = map[uint]string{}
	var scan models.MediaLibraryScanRun
	err := s.db.WithContext(ctx).Where("library_id=? AND kind='transfer_batch' AND status='success' AND CASE WHEN json_valid(checkpoint_json) THEN json_extract(checkpoint_json,'$.transfer_batch_id') ELSE '' END=?", row.LibraryID, transfer.ID).Order("id DESC").First(&scan).Error
	if err != nil {
		return output, false, err
	}
	var managed []models.MediaManagedItem
	if err = s.db.Where("transfer_task_id=? AND download_task_id=? AND library_id=? AND active=? AND managed=?", transfer.ID, row.DownloadTaskID, row.LibraryID, true, true).Find(&managed).Error; err != nil {
		return output, false, err
	}
	expected, err := transferBatchExpectedFiles(transfer, snapshotStorageType(s.db, row.LibraryID))
	if err != nil || expected != len(managed) {
		return output, false, mediaUpgradeSelectionError()
	}
	paths := []string{}
	identities := map[string]string{}
	for _, item := range managed {
		if isVideoFile(item.RelativePath) {
			paths = append(paths, item.RelativePath)
			identities[item.RelativePath] = item.ProviderItemID
		}
	}
	if len(paths) != 1 {
		return output, false, mediaUpgradeSelectionError()
	}
	var entries []models.MediaLibraryEntry
	var library models.MediaLibrary
	var storage models.Storage
	var head models.CatalogHead
	err = s.libraries.withCatalogRead(ctx, []uint{row.LibraryID}, func(tx *gorm.DB, reader *CatalogReader) error {
		var err error
		library, storage, head, _, err = catalogDeletionFenceTx(tx, reader, row.LibraryID)
		if err != nil {
			return err
		}
		return reader.Entries().Where("library_id=? AND relative_path IN ?", row.LibraryID, paths).Order("id").Find(&entries).Error
	})
	if err != nil {
		return output, false, err
	}
	if len(entries) != len(paths) {
		return output, false, nil
	}
	root := ""
	if storage.Type == models.StorageTypeLocal {
		root, err = catalogLocalDeletionRoot(storage, library)
		if err != nil {
			return output, false, err
		}
	}
	for _, entry := range entries {
		if entry.ID == snapshot.Entry.ID || entry.RelativePath == snapshot.Entry.RelativePath || entry.WorkKey != snapshot.Entry.WorkKey || entry.MediaType != snapshot.Entry.MediaType || entry.TMDBID == nil || snapshot.Entry.TMDBID == nil || *entry.TMDBID != *snapshot.Entry.TMDBID || !equalInt(entry.Season, snapshot.Entry.Season) || !equalInt(entry.Episode, snapshot.Entry.Episode) {
			return output, false, mediaUpgradeSelectionError()
		}
		if storage.Type != models.StorageTypeLocal && (entry.ProviderID == snapshot.Entry.ProviderID || identities[entry.RelativePath] == "" || identities[entry.RelativePath] != entry.ProviderID) {
			return output, false, mediaUpgradeChanged()
		}
		if root != "" {
			oldPath, _ := catalogLocalDeletionTarget(root, snapshot.Entry.RelativePath)
			newPath, _ := catalogLocalDeletionTarget(root, entry.RelativePath)
			oldInfo, oldErr := os.Stat(oldPath)
			newInfo, newErr := os.Stat(newPath)
			if newErr != nil || !newInfo.Mode().IsRegular() || newInfo.Size() != entry.Size || !newInfo.ModTime().Equal(entry.ModifiedAt) || ensureSafeDirectoryPath(root, filepath.Dir(newPath), false) != nil {
				return output, false, mediaUpgradeChanged()
			}
			// Equal byte hashes are allowed (e.g. a new separate copy + new subtitles).
			// A link to the SAME physical file is not a replacement to recycle itself.
			identity, identityErr := managedMediaFileIdentity(newPath)
			if identityErr != nil || identity == snapshot.Items[0].FileIdentity {
				return output, false, mediaUpgradeChanged()
			}
			output.PhysicalIdentities[entry.ID] = identity
			if oldErr == nil && os.SameFile(oldInfo, newInfo) {
				return output, false, mediaUpgradeChanged()
			}
		}
		if storage.Type != models.StorageTypeLocal {
			driver, _, err := s.recycleDriver(storage)
			if err != nil {
				return output, false, err
			}
			actual, err := providerItemWithinRoot(ctx, driver, entry.ProviderID, library.ProviderRootID)
			if err != nil || actual.ID != entry.ProviderID || actual.IsDir || actual.Name != path.Base(entry.RelativePath) || actual.Size != entry.Size || (!actual.ModifiedAt.IsZero() && !entry.ModifiedAt.IsZero() && !actual.ModifiedAt.Equal(entry.ModifiedAt)) {
				return output, false, mediaUpgradeChanged()
			}
			if err := s.rejectUnchangedCloudReplacement(ctx, snapshot, actual, managed, driver, library); err != nil {
				return output, false, err
			}
			raw, _ := json.Marshal([]any{actual.ID, actual.ParentID, actual.Name, actual.Size, actual.ModifiedAt.UTC(), strings.ToUpper(actual.SHA1), actual.Revision})
			digest := sha256.Sum256(raw)
			output.PhysicalIdentities[entry.ID] = hex.EncodeToString(digest[:])
		}
		output.EntryIDs = append(output.EntryIDs, entry.ID)
	}
	output.Version, output.Generation, output.EntryDigest = 1, scan.Generation, catalogDeletionDigest(entries)
	if row.OutputJSON != "" && row.OutputJSON != "{}" {
		var previous mediaUpgradeOutput
		if json.Unmarshal([]byte(row.OutputJSON), &previous) != nil || previous.EntryDigest != output.EntryDigest || previous.Generation != output.Generation || !sameUpgradePhysicalIdentities(previous.PhysicalIdentities, output.PhysicalIdentities) {
			return output, false, mediaUpgradeChanged()
		}
	}
	artifactsRequired := library.MetadataArtifactsEnabled
	if storage.Type != models.StorageTypeLocal {
		artifactsRequired = artifactsRequired || library.STRMEnabled && library.SignedProxyEnabled
	}
	if !artifactsRequired {
		return output, true, nil
	}
	var runs []models.MediaArtifactRun
	if err = s.db.Where("library_id=? AND generation=? AND status=?", row.LibraryID, scan.Generation, models.MediaArtifactStatusCompleted).Find(&runs).Error; err != nil {
		return output, false, err
	}
	validRuns := []models.MediaArtifactRun{}
	for _, run := range runs {
		var policy mediaArtifactPolicy
		if json.Unmarshal([]byte(run.PolicyJSON), &policy) != nil || policy.LibraryID != row.LibraryID || policy.Generation != scan.Generation || policy.Metadata != library.MetadataArtifactsEnabled || policy.STRMEnabled != (storage.Type != models.StorageTypeLocal && library.STRMEnabled && library.SignedProxyEnabled) {
			continue
		}
		validRuns = append(validRuns, run)
	}
	runs = validRuns
	if len(runs) == 0 {
		return output, false, nil
	}
	if head.Mode == "versioned" {
		matched := false
		for _, run := range runs {
			if run.CatalogBindingID == "" {
				continue
			}
			var binding models.CatalogArtifactBinding
			if err := s.db.First(&binding, "id=? AND library_id=? AND generation=?", run.CatalogBindingID, row.LibraryID, scan.Generation).Error; err != nil {
				continue
			}
			if binding.State != "completed" || binding.SourceEpoch != snapshot.Fence.SourceEpoch || binding.SourceFingerprint != snapshot.Fence.SourceFingerprint || binding.ConfigFingerprint != snapshot.Fence.ConfigFingerprint {
				continue
			}
			var count int64
			if err := s.db.Model(&models.CatalogArtifactBindingItem{}).Where("binding_id=? AND entity_kind='entry' AND entity_id IN ? AND status='completed'", binding.ID, output.EntryIDs).Count(&count).Error; err != nil {
				return output, false, err
			}
			if count == int64(len(output.EntryIDs)) {
				matched = true
				output.BindingID = binding.ID
				break
			}
		}
		if !matched {
			return output, false, nil
		}
	}
	for _, entry := range entries {
		sources := []string{fmt.Sprintf("entry:%d", entry.ID)}
		if storage.Type == models.StorageTypeLocal && entry.RecognitionID != nil {
			sources = append(sources, fmt.Sprintf("recognition:%d", *entry.RecognitionID))
		}
		query := s.db.Model(&models.MediaArtifact{}).Where("library_id=? AND source_identity IN ? AND active=? AND managed=? AND status=?", row.LibraryID, sources, true, true, models.MediaArtifactStatusCompleted)
		if storage.Type != models.StorageTypeLocal {
			query = query.Where("kind=?", models.MediaArtifactKindSTRM)
		} else {
			query = query.Where("kind=?", models.MediaArtifactKindNFO)
		}
		if output.BindingID != "" {
			query = query.Where("catalog_binding_id=?", output.BindingID)
		} else {
			ids := []string{}
			for _, run := range runs {
				ids = append(ids, run.ID)
			}
			query = query.Where("run_id IN ?", ids)
		}
		var count int64
		if err := query.Count(&count).Error; err != nil {
			return output, false, err
		}
		if count == 0 {
			return output, false, nil
		}
	}
	return output, true, nil
}
func snapshotStorageType(db *gorm.DB, libraryID uint) string {
	var out string
	_ = db.Table("storages").Select("storages.type").Joins("JOIN media_libraries ON media_libraries.storage_id=storages.id").Where("media_libraries.id=?", libraryID).Scan(&out).Error
	return out
}
func equalInt(a, b *int) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

func (s *MediaVersionUpgradeService) recycleOld(ctx context.Context, runtime JobRuntime, claim ClaimedJob, row models.MediaVersionUpgrade, snapshot mediaUpgradeSnapshot) error {
	paths := []string{snapshot.Entry.RelativePath}
	for _, asset := range snapshot.Assets {
		paths = append(paths, asset.RelativePath)
	}
	write, err := s.libraries.captureCatalogDeletion(ctx, row.LibraryID, "", paths)
	if err != nil {
		return err
	}
	if len(write.Entries) != 1 || catalogDeletionDigest(write.Entries) != catalogDeletionDigest([]models.MediaLibraryEntry{snapshot.Entry}) || catalogDeletionAssetDigest(write.Assets) != catalogDeletionAssetDigest(snapshot.Assets) {
		return mediaUpgradeChanged()
	}
	if err = s.libraries.prepareCatalogDeletion(ctx, &write); err != nil {
		return err
	}
	defer s.libraries.abandonCatalogDeletion(&write)
	// Same source fence, exact old ID, fresh artifact proof and live lease before
	// every external operation. New publications may change content/head only.
	check := func() error {
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := catalogCheckJob(tx, models.CatalogSnapshot{JobID: &claim.Job.ID, JobLeaseHash: leaseHash(claim.LeaseToken)}, s.now()); err != nil {
				return err
			}
			return s.validateSnapshotTx(tx, row, snapshot)
		})
	}
	if err = check(); err != nil {
		return err
	}
	var receipt models.MediaCatalogDeletionPreview
	now := s.now()
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := s.validateSnapshotTx(tx, row, snapshot); err != nil {
			return err
		}
		if row.RecycleReceiptID != "" {
			return tx.First(&receipt, "id=?", row.RecycleReceiptID).Error
		}
		receiptID := uuid.NewString()
		digest := sha256.Sum256([]byte(uuid.NewString()))
		// Version FOUR/purpose upgrade is intentionally rejected by the ordinary
		// catalog-deletion confirmation/recovery path (which accepts only 2/3).
		raw, _ := json.Marshal(map[string]any{"version": 4, "purpose": "media_version_upgrade", "upgrade_id": row.ID})
		receipt = models.MediaCatalogDeletionPreview{ID: receiptID, TokenHash: hex.EncodeToString(digest[:]), ActorID: row.OwnerID, LibraryID: row.LibraryID, WorkKey: "upgrade:" + row.ID, EntryDigest: catalogDeletionDigest(write.Entries), StorageType: write.Storage.Type, SnapshotJSON: string(raw), StateJSON: `{"version":1,"completed":{}}`, ExpiresAt: now.Add(365 * 24 * time.Hour), CreatedAt: now, UpdatedAt: now}
		if err := RegisterCatalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: row.LibraryID, OwnerKind: CatalogPhysicalDeletion, OwnerID: receipt.ID, ActorID: row.OwnerID}, func(tx *gorm.DB) error { return tx.Create(&receipt).Error }); err != nil {
			return err
		}
		return tx.Model(&models.MediaVersionUpgrade{}).Where("id=? AND recycle_receipt_id=''", row.ID).Update("recycle_receipt_id", receipt.ID).Error
	})
	if err != nil {
		return err
	}
	var permit CatalogPhysicalWritePermit
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := s.validateSnapshotTx(tx, row, snapshot); err != nil {
			return err
		}
		if err := tx.Model(&receipt).Updates(map[string]any{"started_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		var err error
		permit, err = EnterCatalogPhysicalWriteTx(tx, CatalogPhysicalWriteInput{LibraryID: row.LibraryID, OwnerKind: CatalogPhysicalDeletion, OwnerID: receipt.ID, ActorID: row.OwnerID, ClaimAt: now})
		return err
	})
	if err != nil {
		return err
	}
	defer quiesceCatalogPhysicalWrite(s.db, permit, s.libraries.log)
	var state mediaUpgradeRecycleState
	if json.Unmarshal([]byte(row.RecycleJSON), &state) != nil || state.Version != 1 {
		return mediaUpgradeChanged()
	}
	if state.Items == nil {
		state.Items = map[string]string{}
	}
	for _, item := range snapshot.Items {
		if state.Items[item.Key] == "done" {
			continue
		}
		if err = runtime.Heartbeat(nil, nil, nil, nil, nil); err != nil {
			return err
		}
		if err = check(); err != nil {
			return err
		}
		// Recheck the precise new output immediately before touching old bytes.
		var transfer models.TransferTask
		if err = s.db.First(&transfer, "id=?", row.TransferTaskID).Error; err != nil {
			return err
		}
		if _, ready, err := s.replacementReady(ctx, row, snapshot, transfer); err != nil || !ready {
			if err != nil {
				return err
			}
			return mediaUpgradeChanged()
		}
		if write.Storage.Type == models.StorageTypeLocal {
			if err = s.recycleLocalItem(ctx, row, item, write.Library, write.Storage); err != nil {
				return err
			}
			state.Items[item.Key] = "done"
			if err = s.persistUpgradeRecycle(row.ID, state); err != nil {
				return err
			}
		} else if err = s.recycleCloudItem(ctx, row, item, write.Storage, &state); err != nil {
			return err
		}
	}
	return s.publishUpgradeRemoval(ctx, row, snapshot, &write, receipt, permit)
}

func sameUpgradePhysicalIdentities(a, b map[uint]string) bool {
	if len(a) != len(b) {
		return false
	}
	for id, value := range a {
		if b[id] != value || value == "" {
			return false
		}
	}
	return true
}
