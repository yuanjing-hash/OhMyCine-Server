package services

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// refreshStructureSummaryTx projects the same grouped rows used by the paged
// API. It runs in the writer transaction so cards and detail cannot disagree.
func refreshStructureSummaryTx(tx *gorm.DB, libraryID uint, now time.Time) error {
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := tx.Select("status").Where("library_id = ?", libraryID).Take(&diagnosis).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if diagnosis.Status == models.MediaLibraryStructureQueued || diagnosis.Status == models.MediaLibraryStructureRunning {
		return nil
	}
	var counts []struct {
		Code                string
		Repairable          bool
		State               string
		ConflictSourceCount int
		Total               int
	}
	if err := tx.Model(&models.MediaLibraryStructureIssue{}).
		Select("code, repairable, state, conflict_source_count, COUNT(*) AS total").
		Where("library_id = ? AND code <> ?", libraryID, "missing_season_episode").
		Group("code, repairable, state, conflict_source_count").Scan(&counts).Error; err != nil {
		return err
	}
	total, repairable := 0, 0
	classes := StructureIssueClassifications{}
	for _, group := range counts {
		total += group.Total
		if group.Repairable || (group.State == "manual_identity_resolved" && group.ConflictSourceCount <= 1) {
			repairable += group.Total
		}
		switch group.Code {
		case "media_unrecognized":
			classes.Unrecognized += group.Total
		case "invalid_path":
			classes.InvalidPath += group.Total
		case "template_unavailable":
			classes.TemplateError += group.Total
		case "duplicate_target", "recognition_suspect_conflict", "catalog_duplicate_conflict":
			classes.DuplicateTarget += group.Total
		case "sidecar_target_conflict":
			classes.SidecarConflict += group.Total
		}
	}
	var rows []models.MediaLibraryStructureIssue
	if err := tx.Where("library_id = ? AND code <> ?", libraryID, "missing_season_episode").Order("code,id").Limit(maxStructureIssueSamples).Find(&rows).Error; err != nil {
		return err
	}
	samples := make([]StructureIssue, 0, len(rows))
	ids := make([]uint, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	var members []models.MediaLibraryStructureIssueMember
	if len(ids) > 0 {
		if err := tx.Raw("SELECT issue_id, source_path FROM (SELECT issue_id, source_path, ROW_NUMBER() OVER (PARTITION BY issue_id ORDER BY id) AS member_number FROM media_library_structure_issue_members WHERE issue_id IN ?) WHERE member_number <= ?", ids, maxStructureConflictSourceSamples).Scan(&members).Error; err != nil {
			return err
		}
	}
	byIssue := make(map[uint][]string, len(rows))
	for _, member := range members {
		byIssue[member.IssueID] = append(byIssue[member.IssueID], member.SourcePath)
	}
	for _, row := range rows {
		var sources []string
		if row.ConflictSourceCount > 1 {
			sources = byIssue[row.ID]
		}
		samples = append(samples, StructureIssue{Code: row.Code, Kind: row.Kind, Title: row.Title, CurrentPath: row.CurrentPath, ExpectedPath: row.ExpectedPath, Repairable: row.Repairable, ConflictSourceCount: row.ConflictSourceCount, ConflictSources: sources})
	}
	raw, err := json.Marshal(samples)
	if err != nil {
		return err
	}
	status := models.MediaLibraryStructureHealthy
	if total > 0 {
		status = models.MediaLibraryStructureIssues
	}
	if err := tx.Model(&models.MediaLibraryStructureDiagnosis{}).Where("library_id = ?", libraryID).Updates(map[string]any{
		"status": status, "issue_count": total, "repairable_count": repairable,
		"unrecognized_count": classes.Unrecognized, "missing_episode_count": 0,
		"invalid_path_count": classes.InvalidPath, "template_error_count": classes.TemplateError,
		"duplicate_target_count": classes.DuplicateTarget, "sidecar_conflict_count": classes.SidecarConflict,
		"issues_json": string(raw), "last_error_code": "", "updated_at": now,
	}).Error; err != nil {
		return err
	}
	return tx.Model(&models.MediaLibrary{}).Where("id = ?", libraryID).Updates(map[string]any{
		"structure_status": status, "structure_issue_count": total, "structure_error_code": "", "structure_checked_at": now,
	}).Error
}

// resolveCompletedStructureMovesTx reconciles older full/work repair paths
// that do not carry selection tokens. Only an actually completed source/target
// pair can clear a single-source movement issue; other works and conflicts
// retain their diagnosis rows. A newer diagnosis owns its pending projection.
func resolveCompletedStructureMovesTx(tx *gorm.DB, libraryID uint, items []StructurePlanItem) (bool, error) {
	if len(items) == 0 {
		return false, nil
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := tx.Where("library_id = ?", libraryID).Take(&diagnosis).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if diagnosis.Status != models.MediaLibraryStructureHealthy && diagnosis.Status != models.MediaLibraryStructureIssues {
		return false, nil
	}
	for _, item := range items {
		source, target := safeStructurePath(item.SourceRelative), safeStructurePath(item.TargetRelative)
		if source == "" || target == "" {
			continue
		}
		if err := tx.Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND kind = ? AND current_path = ? AND expected_path = ? AND repairable = ? AND conflict_source_count <= ?", libraryID, diagnosis.JobID, diagnosis.Generation, item.Kind, source, target, true, 1).Delete(&models.MediaLibraryStructureIssue{}).Error; err != nil {
			return false, err
		}
	}
	return true, nil
}

// RecoverLegacyProjections schedules only migration-marked incomplete snapshots.
// It never scans the provider or changes files; the worker rebuilds from catalog.
func (s *MediaLibraryStructureService) RecoverLegacyProjections(ctx context.Context) error {
	var states []models.MediaLibraryStructureAutoState
	if err := s.db.WithContext(ctx).Where("status = ?", "projection_pending").Find(&states).Error; err != nil {
		return err
	}
	for _, state := range states {
		var library models.MediaLibrary
		if err := s.db.WithContext(ctx).First(&library, state.LibraryID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		} else if err != nil {
			return err
		}
		if err := s.recoverLegacyProjection(ctx, library, state); err != nil {
			return err
		}
	}
	return nil
}

func (s *MediaLibraryStructureService) recoverLegacyProjection(ctx context.Context, library models.MediaLibrary, state models.MediaLibraryStructureAutoState) error {
	if state.Status != "projection_pending" || library.BaselineGeneration == 0 {
		return nil
	}
	var run models.MediaLibraryScanRun
	err := s.db.WithContext(ctx).Where("library_id = ? AND generation = ?", library.ID, library.BaselineGeneration).Order("id DESC").First(&run).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil && (run.Status != "success" || run.Partial || run.RecognitionCompleted < run.RecognitionTotal) {
		return nil
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	err = s.db.WithContext(ctx).Where("library_id = ?", library.ID).First(&diagnosis).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil && diagnosis.Generation == library.BaselineGeneration && (diagnosis.Status == models.MediaLibraryStructureQueued || diagnosis.Status == models.MediaLibraryStructureRunning) {
		// A stale worker can finish without publishing. Its old projection
		// status alone must not block compatibility recovery forever.
		var active int64
		if err := s.db.WithContext(ctx).Model(&models.Job{}).Where("id = ? AND status IN ?", diagnosis.JobID, []string{models.JobStatusQueued, models.JobStatusRunning, models.JobStatusRetryWait, models.JobStatusPaused, models.JobStatusWaitingUserAction}).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return nil
		}
	}
	err = s.enqueueDiagnosis(ctx, library.ID, 0, library.BaselineGeneration, "upgrade_projection", false, state.SourceRevision)
	// Supervisors may publish a catch-up generation or remove a library while
	// startup visits the recovery snapshot. Keep recovery pending for the next
	// convergence callback instead of failing Server startup on that race.
	if ErrorCode(err) == CodeConflict || ErrorCode(err) == CodeNotFound {
		return nil
	}
	return err
}
