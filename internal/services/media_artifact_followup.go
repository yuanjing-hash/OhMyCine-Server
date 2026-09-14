package services

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type batchArtifactCheckpoint struct {
	Version           int    `json:"artifact_scope_version"`
	Pending           bool   `json:"artifact_followup_pending"`
	EntryIDs          []uint `json:"artifact_entry_ids"`
	AssetIDs          []uint `json:"artifact_asset_ids"`
	SourceFingerprint string `json:"artifact_source_fingerprint"`
}

func freezeBatchArtifactCheckpointTx(tx *gorm.DB, run *models.MediaLibraryScanRun) error {
	checkpoint := batchArtifactCheckpoint{Version: 1, Pending: true}
	if err := tx.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND last_generation = ?", run.LibraryID, run.Generation).Order("id").Pluck("id", &checkpoint.EntryIDs).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.MediaLibrarySourceAsset{}).Where("library_id = ? AND generation = ? AND active = ?", run.LibraryID, run.Generation, true).Order("id").Pluck("id", &checkpoint.AssetIDs).Error; err != nil {
		return err
	}
	var library models.MediaLibrary
	if err := tx.First(&library, run.LibraryID).Error; err != nil {
		return err
	}
	fingerprint, err := artifactBatchSourceFingerprint(tx, library)
	if err != nil {
		return err
	}
	checkpoint.SourceFingerprint = fingerprint
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(run.CheckpointJSON), &fields); err != nil {
		return err
	}
	raw, _ := json.Marshal(checkpoint)
	additions := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &additions); err != nil {
		return err
	}
	for key, value := range additions {
		fields[key] = value
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		return err
	}
	run.CheckpointJSON = string(raw)
	return nil
}

func (s *MediaLibraryService) acknowledgeBatchArtifactFollowup(ctx context.Context, runID uint) error {
	return s.db.WithContext(ctx).Model(&models.MediaLibraryScanRun{}).Where("id = ?", runID).Update("checkpoint_json", gorm.Expr("json_set(checkpoint_json, '$.artifact_followup_pending', json('false'))")).Error
}

// Called under the library scan lock before accepting another scan. Durable
// published scopes must reach the queue before duplicate events can be ACKed.
func (s *MediaLibraryService) recoverBatchArtifactFollowups(ctx context.Context, libraryID uint) error {
	if s.artifacts == nil {
		return nil
	}
	var runs []models.MediaLibraryScanRun
	if err := s.db.WithContext(ctx).Where("library_id = ? AND status = 'success' AND CASE WHEN json_valid(checkpoint_json) THEN json_extract(checkpoint_json, '$.artifact_followup_pending') ELSE 0 END = 1", libraryID).Order("id").Limit(32).Find(&runs).Error; err != nil {
		return err
	}
	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.artifacts.ScheduleGeneration(libraryID, run.Generation); err != nil {
			return err
		}
		if err := s.acknowledgeBatchArtifactFollowup(ctx, run.ID); err != nil {
			return err
		}
	}
	if len(runs) == 32 {
		return errors.New("batch artifact followups remain pending")
	}
	return nil
}
