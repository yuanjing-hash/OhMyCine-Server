package services

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Publish exact tombstones and normal artifact-cleanup work together. Shared
// artwork/NFO and logical history rows are deliberately outside this write set.
func (s *MediaVersionUpgradeService) publishUpgradeRemoval(ctx context.Context, row models.MediaVersionUpgrade, snapshot mediaUpgradeSnapshot, write *catalogDeletionWrite, receipt models.MediaCatalogDeletionPreview, permit CatalogPhysicalWritePermit) error {
	libraries := s.libraries
	library, storage := write.Library, write.Storage
	required := mediaLibraryRequiresArtifacts(storage.Type, library, libraries.artifacts != nil)
	var generation, changeRevision uint64
	var artifactScanID uint
	changeReady := false
	err := libraries.commitCatalogDeletion(ctx, write, func(tx *gorm.DB) error {
		current, err := NewAuthorizationService(tx).Resolve(row.OwnerID)
		if err != nil || !mediaUpgradeAllowed(current, row.LibraryID) {
			return mediaUpgradeChanged()
		}
		var output mediaUpgradeOutput
		if json.Unmarshal([]byte(row.OutputJSON), &output) != nil || output.Version != 1 || len(output.EntryIDs) != 1 {
			return mediaUpgradeChanged()
		}
		reader, err := PinCatalogTx(tx, []uint{row.LibraryID})
		if err != nil {
			return err
		}
		var replacement []models.MediaLibraryEntry
		if err := reader.Entries().Where("library_id=? AND id IN ?", row.LibraryID, output.EntryIDs).Find(&replacement).Error; err != nil {
			return err
		}
		if catalogDeletionDigest(replacement) != output.EntryDigest {
			return mediaUpgradeChanged()
		}

		var profile models.MediaClassificationProfile
		var deletedScopes []deletedRecognitionScope
		var deletedManifests []uint
		if required && write.Head.Mode != "versioned" {
			if err := tx.First(&profile, library.ProfileID).Error; err != nil {
				return err
			}
			var err error
			deletedScopes, err = captureDeletedEntryRecognitionScopesTx(tx, library.ID, write.Entries)
			if err != nil {
				return err
			}
			identities := []string{fmt.Sprintf("entry:%d", snapshot.Entry.ID)}
			for _, asset := range snapshot.Assets {
				identities = append(identities, fmt.Sprintf("asset:%d", asset.ID))
			}
			var manifests []models.MediaArtifact
			if err := tx.Where("library_id=? AND source_identity IN ? AND managed=?", library.ID, identities, true).Find(&manifests).Error; err != nil {
				return err
			}
			for _, manifest := range manifests {
				if err := validateDeletedArtifactOwnerTx(tx, library, storage, profile, manifest.RunID); err != nil {
					return err
				}
				deletedManifests = append(deletedManifests, manifest.ID)
			}
		}
		if write.Head.Mode != "versioned" {
			if err := tx.Where("library_id=? AND id=?", library.ID, snapshot.Entry.ID).Delete(&models.MediaLibraryEntry{}).Error; err != nil {
				return err
			}
			assetIDs := []uint{}
			for _, asset := range snapshot.Assets {
				assetIDs = append(assetIDs, asset.ID)
			}
			if len(assetIDs) > 0 {
				if err := tx.Where("library_id=? AND id IN ?", library.ID, assetIDs).Delete(&models.MediaLibrarySourceAsset{}).Error; err != nil {
					return err
				}
			}
		}
		paths := []string{snapshot.Entry.RelativePath}
		for _, asset := range snapshot.Assets {
			paths = append(paths, asset.RelativePath)
		}
		if err := tx.Model(&models.MediaManagedItem{}).Where("library_id=? AND relative_path IN ? AND active=?", library.ID, paths, true).Updates(map[string]any{"active": false, "updated_at": s.now()}).Error; err != nil {
			return err
		}
		if !required && write.Head.Mode != "versioned" && snapshot.Entry.RecognitionID != nil {
			var count int64
			if err := tx.Model(&models.MediaLibraryEntry{}).Where("recognition_id=?", *snapshot.Entry.RecognitionID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				if err := tx.Delete(&models.MediaLibraryRecognition{}, *snapshot.Entry.RecognitionID).Error; err != nil {
					return err
				}
			}
		}
		generation = max(max(library.DirtyGeneration, library.ArtifactGeneration), library.BaselineGeneration) + 1
		if err := tx.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Updates(map[string]any{"dirty_generation": generation, "artifact_generation": generation}).Error; err != nil {
			return err
		}
		if required && write.Head.Mode == "versioned" {
			if _, err := libraries.artifacts.BindCatalogGenerationChangesTx(tx, library.ID, generation, write.ArtifactChanges); err != nil {
				return err
			}
		} else if required {
			shared, err := pruneDeletedEmptyRecognitionsTx(tx, library, storage, profile, deletedScopes)
			if err != nil {
				return err
			}
			fingerprint, err := artifactBatchSourceFingerprint(tx, library)
			if err != nil {
				return err
			}
			checkpoint := batchArtifactCheckpoint{Version: 1, Pending: true, SourceFingerprint: fingerprint, DeletedManifestIDs: append(deletedManifests, shared...)}
			raw, err := json.Marshal(checkpoint)
			if err != nil {
				return err
			}
			now := s.now()
			run := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: generation, Kind: "version_upgrade", Status: "success", Partial: true, CheckpointJSON: string(raw), StartedAt: now, FinishedAt: &now}
			if err := freezeDeletedWorkGuards(&run, deletedScopes); err != nil {
				return err
			}
			if err := tx.Create(&run).Error; err != nil {
				return err
			}
			artifactScanID = run.ID
		}
		if libraries.changes != nil {
			change, err := libraries.changes.RecordTx(tx, library.ID, generation, models.MediaLibraryChangeRemoval, !required)
			if err != nil {
				return err
			}
			changeRevision, changeReady = change.Revision, change.State == models.MediaLibraryChangeReady
		} else if err := tx.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Update("content_revision", gorm.Expr("content_revision+1")).Error; err != nil {
			return err
		}
		now := s.now()
		if err := tx.Model(&models.MediaVersionUpgrade{}).Where("id=? AND state IN ?", row.ID, []string{"replacement_ready", "recycle_pending"}).Updates(map[string]any{"state": "completed", "stage": "completed", "error_code": "", "next_attempt_at": nil, "finished_at": now, "revision": gorm.Expr("revision+1"), "updated_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.MediaCatalogDeletionPreview{}).Where("id=?", receipt.ID).Updates(map[string]any{"consumed_at": now, "started_at": nil, "last_error_code": "", "updated_at": now}).Error; err != nil {
			return err
		}
		if err := SettleCatalogPhysicalWriteTx(tx, permit, nil); err != nil {
			return err
		}
		if libraries.audit != nil {
			return libraries.audit.Record(tx, &row.OwnerID, "media_version_upgrade.completed", "media_library", uintID(row.LibraryID), "success", map[string]any{"entry_id": row.EntryID, "upgrade_id": row.ID}, RequestContext{})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if changeReady && changeRevision > 0 && libraries.changes != nil {
		libraries.changes.NotifyCommitted(library.ID, changeRevision)
	}
	if required && generation > 0 {
		if err := libraries.artifacts.ScheduleGeneration(library.ID, generation); err == nil && artifactScanID > 0 {
			_ = libraries.acknowledgeBatchArtifactFollowup(ctx, artifactScanID)
		}
	}
	libraries.wakeCatalogDeletionReconcile(library.ID)
	return nil
}
