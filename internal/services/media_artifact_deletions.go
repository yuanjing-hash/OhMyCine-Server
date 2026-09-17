package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Exact provider tombstones authorize only manifests with recorded provenance
// matching the current source. Names/extensions are never evidence of ownership.
func deletedProviderArtifactIDs(tx *gorm.DB, libraryID uint, providerIDs []string) ([]uint, error) {
	if len(providerIDs) == 0 {
		return nil, nil
	}
	var library models.MediaLibrary
	if err := tx.First(&library, libraryID).Error; err != nil {
		return nil, err
	}
	var storage models.Storage
	if err := tx.First(&storage, library.StorageID).Error; err != nil {
		return nil, err
	}
	var profile models.MediaClassificationProfile
	if err := tx.First(&profile, library.ProfileID).Error; err != nil {
		return nil, err
	}
	ids := []uint{}
	validatedOwners := map[string]bool{}
	for start := 0; start < len(providerIDs); start += CatalogBatchRows {
		batch := providerIDs[start:min(start+CatalogBatchRows, len(providerIDs))]
		for after := uint(0); ; {
			var rows []models.MediaArtifact
			if err := tx.Where("library_id = ? AND provider_item_id IN ? AND managed = ? AND id > ?", libraryID, batch, true, after).Order("id").Limit(CatalogBatchRows).Find(&rows).Error; err != nil {
				return nil, err
			}
			if len(rows) == 0 {
				break
			}
			for _, row := range rows {
				if validatedOwners[row.RunID] {
					ids = append(ids, row.ID)
					continue
				}
				if err := validateDeletedArtifactOwnerTx(tx, library, storage, profile, row.RunID); err != nil {
					return nil, err
				}
				ids = append(ids, row.ID)
				validatedOwners[row.RunID] = true
			}
			after = rows[len(rows)-1].ID
		}
	}
	return normalizeCatalogArtifactChanges(CatalogArtifactChangeSet{Manifests: ids}).Manifests, nil
}

func validateDeletedArtifactOwnerTx(tx *gorm.DB, library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile, runID string) error {
	var run models.MediaArtifactRun
	if err := tx.First(&run, "id = ? AND library_id = ?", runID, library.ID).Error; err != nil {
		return err
	}
	var policy mediaArtifactPolicy
	if json.Unmarshal([]byte(run.PolicyJSON), &policy) != nil || policy.LibraryID != library.ID || policy.StorageID != library.StorageID {
		return ErrCatalogFence
	}
	if policy.SourceBoundaryFingerprint != "" {
		if policy.SourceBoundaryFingerprint != catalogSourceFingerprint(library, storage) {
			return ErrCatalogFence
		}
		return nil
	}
	if policy.CatalogBindingID != "" {
		var owner models.CatalogArtifactBinding
		var head models.CatalogHead
		if tx.First(&owner, "id = ? AND library_id = ?", policy.CatalogBindingID, library.ID).Error != nil || tx.First(&head, "library_id = ?", library.ID).Error != nil || owner.SourceEpoch != head.SourceEpoch || owner.SourceFingerprint != head.SourceFingerprint {
			return ErrCatalogFence
		}
		return nil
	}
	var scan models.MediaLibraryScanRun
	if policy.ScanRunID == 0 || tx.First(&scan, "id = ? AND library_id = ?", policy.ScanRunID, library.ID).Error != nil || scan.Generation == 0 || scan.SourceFingerprint == "" {
		return ErrCatalogFence
	}
	old := library
	old.DirtyGeneration = scan.Generation - 1
	if mediaLibraryScanSourceFingerprint(old, storage, profile) != scan.SourceFingerprint {
		return ErrCatalogFence
	}
	return nil
}

type deletedRecognitionScope struct {
	RecognitionID  uint
	WorkKeys       []string
	RecognitionIDs []uint
	TMDBID         *int64
	MediaType      string
}

func captureDeletedRecognitionScopesTx(tx *gorm.DB, libraryID uint, providerIDs []string) ([]deletedRecognitionScope, error) {
	var all []models.MediaLibraryEntry
	for start := 0; start < len(providerIDs); start += CatalogBatchRows {
		var entries []models.MediaLibraryEntry
		if err := tx.Select("recognition_id", "work_key").Where("library_id = ? AND provider_id IN ?", libraryID, providerIDs[start:min(start+CatalogBatchRows, len(providerIDs))]).Find(&entries).Error; err != nil {
			return nil, err
		}
		all = append(all, entries...)
	}
	return captureDeletedEntryRecognitionScopesTx(tx, libraryID, all)
}

func captureDeletedEntryRecognitionScopesTx(tx *gorm.DB, libraryID uint, entries []models.MediaLibraryEntry) ([]deletedRecognitionScope, error) {
	byID := map[uint]*deletedRecognitionScope{}
	for _, entry := range entries {
		if entry.RecognitionID == nil {
			continue
		}
		id := *entry.RecognitionID
		if byID[id] == nil {
			byID[id] = &deletedRecognitionScope{RecognitionID: id}
		}
		if entry.WorkKey != "" {
			byID[id].WorkKeys = append(byID[id].WorkKeys, entry.WorkKey)
		}
	}
	scopes := make([]deletedRecognitionScope, 0, len(byID))
	for _, scope := range byID {
		var record models.MediaLibraryRecognition
		if err := tx.Where("library_id = ? AND id = ?", libraryID, scope.RecognitionID).First(&record).Error; err != nil {
			return nil, err
		}
		scope.RecognitionIDs = []uint{scope.RecognitionID}
		scope.TMDBID, scope.MediaType = record.TMDBID, record.MediaType
		if record.TMDBID != nil && *record.TMDBID > 0 && record.MediaType != "" {
			if err := tx.Model(&models.MediaLibraryRecognition{}).Where("library_id = ? AND tmdb_id = ? AND media_type = ?", libraryID, *record.TMDBID, record.MediaType).Pluck("id", &scope.RecognitionIDs).Error; err != nil {
				return nil, err
			}
		}
		scopes = append(scopes, *scope)
	}
	return scopes, nil
}

func freezeDeletedWorkGuards(run *models.MediaLibraryScanRun, scopes []deletedRecognitionScope) error {
	if len(scopes) == 0 {
		return nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(run.CheckpointJSON), &fields); err != nil {
		return err
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(scopes)
	if err != nil {
		return err
	}
	fields["artifact_deleted_work_guards"] = raw
	raw, err = json.Marshal(fields)
	run.CheckpointJSON = string(raw)
	return err
}

// Shared artwork has no provider file ID. Freeze its work identity before the
// recognition is pruned, then reject stale deletion when any season is restored.
func freezeArtifactDeletedWorkGuardsTx(tx *gorm.DB, policy *mediaArtifactPolicy) error {
	if policy.ScanRunID == 0 {
		return nil
	}
	var scan models.MediaLibraryScanRun
	if err := tx.Where("id = ? AND library_id = ?", policy.ScanRunID, policy.LibraryID).First(&scan).Error; err != nil {
		return err
	}
	var checkpoint struct {
		Guards []deletedRecognitionScope `json:"artifact_deleted_work_guards"`
	}
	if strings.TrimSpace(scan.CheckpointJSON) == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(scan.CheckpointJSON), &checkpoint); err != nil {
		return err
	}
	policy.DeletedWorkGuards = checkpoint.Guards
	return nil
}

func checkDeletedWorkRestoration(_ *gorm.DB, reader *CatalogReader, policy mediaArtifactPolicy, artifacts []models.MediaArtifact) error {
	covered := map[uint]bool{}
	for _, guard := range policy.DeletedWorkGuards {
		selected := false
		for _, id := range guard.RecognitionIDs {
			identity := fmt.Sprintf("recognition:%d", id)
			for _, artifact := range artifacts {
				if artifact.SourceIdentity == identity || strings.HasPrefix(artifact.SourceIdentity, identity+":") {
					covered[artifact.ID] = true
					selected = true
				}
			}
		}
		if !selected {
			continue
		}
		query := reader.Entries()
		if guard.TMDBID != nil && *guard.TMDBID > 0 && guard.MediaType != "" {
			related := reader.Recognitions().Select("id").Where("tmdb_id = ? AND media_type = ?", *guard.TMDBID, guard.MediaType)
			query = query.Where("recognition_id IN ? OR work_key IN ? OR (tmdb_id = ? AND media_type = ?) OR recognition_id IN (?)", guard.RecognitionIDs, guard.WorkKeys, *guard.TMDBID, guard.MediaType, related)
		} else {
			query = query.Where("recognition_id IN ? OR work_key IN ?", guard.RecognitionIDs, guard.WorkKeys)
		}
		var entries []models.MediaLibraryEntry
		if err := query.Limit(1).Find(&entries).Error; err != nil {
			return err
		}
		if len(entries) > 0 {
			return cleanupFailure("artifact_cleanup_source_restored")
		}
	}
	for _, artifact := range artifacts {
		if strings.HasPrefix(artifact.SourceIdentity, "recognition:") && !covered[artifact.ID] {
			return cleanupFailure("artifact_cleanup_ownership_invalid")
		}
	}
	return nil
}

// Only explicit deleted references can authorize legacy work pruning. A failed
// scan or absent page never enters here; other seasons/versions protect sharing.
func pruneDeletedEmptyRecognitionsTx(tx *gorm.DB, library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile, scopes []deletedRecognitionScope) ([]uint, error) {
	ids := []uint{}
	validatedOwners := map[string]bool{}
	for _, scope := range scopes {
		var remaining int64
		if err := tx.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Where("recognition_id IN ? OR work_key IN ?", scope.RecognitionIDs, scope.WorkKeys).Count(&remaining).Error; err != nil {
			return nil, err
		}
		if remaining != 0 {
			continue
		}
		for _, recognitionID := range scope.RecognitionIDs {
			identity := fmt.Sprintf("recognition:%d", recognitionID)
			for after := uint(0); ; {
				var rows []models.MediaArtifact
				if err := tx.Where("library_id = ? AND managed = ? AND id > ?", library.ID, true, after).Where("source_identity = ? OR source_identity LIKE ?", identity, identity+":%").Order("id").Limit(CatalogBatchRows).Find(&rows).Error; err != nil {
					return nil, err
				}
				if len(rows) == 0 {
					break
				}
				for _, row := range rows {
					if !validatedOwners[row.RunID] {
						if err := validateDeletedArtifactOwnerTx(tx, library, storage, profile, row.RunID); err != nil {
							return nil, err
						}
						validatedOwners[row.RunID] = true
					}
					ids = append(ids, row.ID)
				}
				after = rows[len(rows)-1].ID
			}
			if err := tx.Where("library_id = ? AND id = ?", library.ID, recognitionID).Delete(&models.MediaLibraryRecognition{}).Error; err != nil {
				return nil, err
			}
		}
	}
	return normalizeCatalogArtifactChanges(CatalogArtifactChangeSet{Manifests: ids}).Manifests, nil
}

func appendDeletedArtifactCheckpoint(run *models.MediaLibraryScanRun, additional []uint) error {
	if len(additional) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(run.CheckpointJSON), &fields); err != nil {
		return err
	}
	var ids []uint
	if raw := fields["artifact_deleted_manifest_ids"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &ids); err != nil {
			return err
		}
	}
	ids = normalizeCatalogArtifactChanges(CatalogArtifactChangeSet{Manifests: append(ids, additional...)}).Manifests
	raw, _ := json.Marshal(ids)
	fields["artifact_deleted_manifest_ids"] = raw
	raw, err := json.Marshal(fields)
	run.CheckpointJSON = string(raw)
	return err
}

func freezeDeletedArtifactCheckpointTx(tx *gorm.DB, run *models.MediaLibraryScanRun, providerIDs []string) error {
	if err := freezeCloudCleanupDirectoriesTx(tx, run, providerIDs); err != nil {
		return err
	}
	ids, err := deletedProviderArtifactIDs(tx, run.LibraryID, providerIDs)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(run.CheckpointJSON), &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("missing artifact checkpoint")
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	fields["artifact_deleted_manifest_ids"] = raw
	raw, err = json.Marshal(fields)
	run.CheckpointJSON = string(raw)
	return err
}

func scopedCleanupArtifactQuery(query *gorm.DB, policy mediaArtifactPolicy) *gorm.DB {
	if policy.CatalogBindingID != "" && policy.CatalogScopeMode == catalogArtifactScopeIncremental {
		return query.Where(`EXISTS (SELECT 1 FROM catalog_artifact_binding_items work WHERE work.binding_id = ? AND work.status = 'completed' AND work.outcome <> 'noop' AND (
		(work.entity_kind = 'manifest' AND work.entity_id = media_artifacts.id) OR
		(work.entity_kind = 'entry' AND media_artifacts.source_identity = 'entry:' || work.entity_id) OR
		(work.entity_kind = 'asset' AND media_artifacts.source_identity = 'asset:' || work.entity_id) OR
		(work.entity_kind = 'recognition' AND (media_artifacts.source_identity = 'recognition:' || work.entity_id OR media_artifacts.source_identity LIKE 'recognition:' || work.entity_id || ':%'))))`, policy.CatalogBindingID)
	}
	if policy.ScopeVersion == 1 {
		return query.Where("media_artifacts.id IN ?", policy.DeletedManifestIDs)
	}
	return query
}
