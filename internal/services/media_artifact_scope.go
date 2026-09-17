package services

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Automatic file batches have no authority to enumerate or retire unrelated
// library content, including when an older caller forgot to mark a scan partial.
func artifactRequiresBoundedScope(policy mediaArtifactPolicy) bool {
	switch policy.ScanKind {
	case "event", "incremental", "catch_up", "transfer_batch", "metadata", "deletion":
		return true
	}
	return policy.ScanPartial
}

func legacyArtifactRunScopeQuery(query *gorm.DB, policy mediaArtifactPolicy) *gorm.DB {
	// A generation counter is reusable after library rebuilds. Only the exact
	// scan occurrence may reuse its run; malformed/older scope payloads never match.
	query = query.Where("CASE WHEN json_valid(policy_json) THEN COALESCE(json_extract(policy_json, '$.scan_run_id'), 0) ELSE -1 END = ?", policy.ScanRunID)
	if policy.ScopeVersion == 1 {
		query = query.Where("CASE WHEN json_valid(policy_json) THEN json_extract(policy_json, '$.scope_version') ELSE 0 END = 1")
	}
	return query
}

func (s *MediaArtifactService) freezeLegacyArtifactScope(policy *mediaArtifactPolicy) error {
	if !artifactRequiresBoundedScope(*policy) {
		return nil
	}
	policy.ScanPartial = true
	policy.CleanupEligible = false
	if policy.CatalogBindingID != "" {
		if policy.CatalogScopeMode != catalogArtifactScopeIncremental {
			return errors.New("automatic artifact batch cannot use full catalog scope")
		}
		return nil
	}
	if policy.ScanRunID == 0 {
		return errors.New("automatic artifact batch requires an exact scan")
	}
	policy.ScopeVersion = 1
	return s.db.Transaction(func(tx *gorm.DB) error {
		var previous models.MediaArtifactRun
		lookupErr := legacyArtifactRunScopeQuery(tx.Where("library_id = ? AND generation = ? AND catalog_binding_id = ''", policy.LibraryID, policy.Generation), *policy).First(&previous).Error
		if lookupErr != nil && !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return lookupErr
		}
		entryQuery := tx.Select("id", "recognition_id").Where("library_id = ?", policy.LibraryID)
		if lookupErr == nil {
			var frozen mediaArtifactPolicy
			if err := json.Unmarshal([]byte(previous.PolicyJSON), &frozen); err != nil {
				return err
			}
			entryQuery = entryQuery.Where("id IN ?", frozen.EntryIDs)
			policy.SourceAssetIDs = frozen.SourceAssetIDs
			policy.DeletedManifestIDs = frozen.DeletedManifestIDs
			policy.DeletedWorkGuards = frozen.DeletedWorkGuards
			policy.ChangeRevisions = frozen.ChangeRevisions
			policy.BatchSourceFingerprint = frozen.BatchSourceFingerprint
		} else {
			var scan models.MediaLibraryScanRun
			if err := tx.First(&scan, policy.ScanRunID).Error; err != nil {
				return err
			}
			var checkpoint batchArtifactCheckpoint
			_ = json.Unmarshal([]byte(scan.CheckpointJSON), &checkpoint)
			if checkpoint.Version == 1 {
				entryQuery = entryQuery.Where("id IN ?", checkpoint.EntryIDs)
				policy.SourceAssetIDs = checkpoint.AssetIDs
				policy.DeletedManifestIDs = checkpoint.DeletedManifestIDs
				policy.BatchSourceFingerprint = checkpoint.SourceFingerprint
			} else {
				var library models.MediaLibrary
				if err := tx.First(&library, policy.LibraryID).Error; err != nil {
					return err
				}
				fingerprint, err := artifactBatchSourceFingerprint(tx, library)
				if err != nil {
					return err
				}
				policy.BatchSourceFingerprint = fingerprint
				entryQuery = entryQuery.Where("last_generation = ?", policy.Generation)
			}
		}
		var entries []models.MediaLibraryEntry
		if err := entryQuery.Order("id").Find(&entries).Error; err != nil {
			return err
		}
		seen := map[uint]bool{}
		for _, entry := range entries {
			policy.EntryIDs = append(policy.EntryIDs, entry.ID)
			if entry.RecognitionID != nil && !seen[*entry.RecognitionID] {
				seen[*entry.RecognitionID] = true
				policy.RecognitionIDs = append(policy.RecognitionIDs, *entry.RecognitionID)
			}
		}
		if lookupErr == nil {
			return nil
		}
		if err := tx.Model(&models.MediaLibraryChange{}).Where("library_id = ? AND generation = ? AND state = ?", policy.LibraryID, policy.Generation, models.MediaLibraryChangePending).Order("revision").Pluck("revision", &policy.ChangeRevisions).Error; err != nil {
			return err
		}
		var scan models.MediaLibraryScanRun
		if err := tx.First(&scan, policy.ScanRunID).Error; err != nil {
			return err
		}
		var checkpoint batchArtifactCheckpoint
		_ = json.Unmarshal([]byte(scan.CheckpointJSON), &checkpoint)
		if checkpoint.Version == 1 {
			return nil
		}
		return tx.Model(&models.MediaLibrarySourceAsset{}).Where("library_id = ? AND generation = ? AND active = ?", policy.LibraryID, policy.Generation, true).Order("id").Pluck("id", &policy.SourceAssetIDs).Error
	})
}

// A partial batch cannot stand for older pending changes or authorize their
// cleanup. Publish only the revisions frozen with this batch.
func (s *MediaArtifactService) markBatchChangesReadyTx(tx *gorm.DB, policy mediaArtifactPolicy) ([]models.MediaLibraryChange, error) {
	var changes []models.MediaLibraryChange
	if len(policy.ChangeRevisions) == 0 {
		return nil, nil
	}
	if err := tx.Where("library_id = ? AND generation = ? AND revision IN ? AND state = ?", policy.LibraryID, policy.Generation, policy.ChangeRevisions, models.MediaLibraryChangePending).Order("revision").Find(&changes).Error; err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	for i := range changes {
		result := tx.Where("sequence = ? AND state = ?", changes[i].Sequence, models.MediaLibraryChangePending).Delete(&models.MediaLibraryChange{})
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected != 1 {
			return nil, ErrCatalogFence
		}
		changes[i].Sequence = 0
		changes[i].State, changes[i].ReadyAt = models.MediaLibraryChangeReady, &now
		if err := tx.Create(&changes[i]).Error; err != nil {
			return nil, err
		}
	}
	if len(changes) > 0 {
		if err := s.changes.advanceTargetsTx(tx, policy.LibraryID, changes[len(changes)-1].Revision, now); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func artifactBatchSourceFingerprint(db *gorm.DB, library models.MediaLibrary) (string, error) {
	var storage models.Storage
	if err := db.First(&storage, library.StorageID).Error; err != nil {
		return "", err
	}
	var profile models.MediaClassificationProfile
	if err := db.First(&profile, library.ProfileID).Error; err != nil {
		return "", err
	}
	// New file batches advance dirty generation without changing source/config.
	library.DirtyGeneration = 0
	return mediaLibraryScanSourceFingerprint(library, storage, profile), nil
}

func artifactScheduledLibraryQuery(tx *gorm.DB, policy mediaArtifactPolicy) *gorm.DB {
	query := tx.Model(&models.MediaLibrary{}).Where("id = ?", policy.LibraryID)
	if policy.ScopeVersion == 1 {
		query = query.Where("artifact_generation <= ?", policy.Generation)
	}
	return query
}

func artifactFullAudit(policy mediaArtifactPolicy) bool {
	if policy.ScanPartial {
		return false
	}
	if policy.CatalogScopeMode == catalogArtifactScopeFull {
		return true
	}
	if policy.CatalogBindingID != "" {
		return false
	}
	switch policy.ScanKind {
	case "initial", "manual", "full", "strm_full_manual", "reorganization":
		return true
	}
	return false
}

func artifactAssetSourceFingerprint(a models.MediaLibrarySourceAsset) string {
	raw, _ := json.Marshal([]any{a.ProviderID, a.RelativePath, a.Size, a.ModifiedAt.UTC().Format(time.RFC3339Nano), a.HashHint})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}
