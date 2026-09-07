package services

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type MediaLibraryStructureSelectionStatus struct {
	Revision           string   `json:"revision"`
	InvalidIssueTokens []string `json:"invalid_issue_tokens"`
}

// SelectionStatus reconciles only the caller's bounded draft. A changed issue
// gets a new identity, never silently inherits an old destructive choice. This
// read is not execution authority; PreviewSelectionRepair still validates all
// current facts and the returned revision before creating a frozen plan.
func (s *MediaLibraryStructureService) SelectionStatus(ctx context.Context, actor Actor, libraryID uint, tokens []string) (MediaLibraryStructureSelectionStatus, error) {
	result := MediaLibraryStructureSelectionStatus{InvalidIssueTokens: []string{}}
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return result, appError(CodePermissionDenied, "无权核对目录修复选择", nil)
	}
	unique := make([]string, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		if token == "" || len(token) > 256 || strings.TrimSpace(token) != token {
			return result, appError(CodeInvalidRequest, "目录修复选择无效", nil)
		}
		if !seen[token] {
			unique = append(unique, token)
			seen[token] = true
		}
	}
	// This is reconciliation only, not execution authority. Keep each read
	// bounded and let PreviewSelectionRepair perform the final current-revision
	// fence; do not reserve SQLite's writer merely to pin this UI check.
	var summary struct {
		ID                  uint
		BaselineGeneration  uint64
		StructureStatus     string
		StructureIssueCount int
		StructureErrorCode  string
		StructureCheckedAt  *time.Time
	}
	valid := make(map[string]bool, len(unique))
	db := s.db.WithContext(ctx)
	if err := db.Table("media_libraries AS l").Select("l.id,l.baseline_generation,l.structure_status,l.structure_issue_count,l.structure_error_code,l.structure_checked_at").Where("l.id = ?", libraryID).Take(&summary).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return result, mediaLibraryNotFound(err)
		}
		return result, err
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	diagnosisFound := true
	if err := db.Where("library_id = ? AND status IN ?", libraryID, []string{models.MediaLibraryStructureHealthy, models.MediaLibraryStructureIssues}).First(&diagnosis).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		diagnosisFound = false
	} else if err != nil {
		return result, err
	}
	if diagnosisFound {
		for start := 0; start < len(unique); start += CatalogBatchRows {
			part := unique[start:min(start+CatalogBatchRows, len(unique))]
			var found []string
			if err := db.Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND token IN ?", libraryID, diagnosis.JobID, diagnosis.Generation, part).Pluck("token", &found).Error; err != nil {
				return result, err
			}
			for _, token := range found {
				valid[token] = true
			}
		}
	}
	result.Revision = structureDiagnosticRevision(models.MediaLibrary{ID: summary.ID, BaselineGeneration: summary.BaselineGeneration, StructureStatus: summary.StructureStatus, StructureIssueCount: summary.StructureIssueCount, StructureErrorCode: summary.StructureErrorCode, StructureCheckedAt: summary.StructureCheckedAt})
	for _, token := range unique {
		if !valid[token] {
			result.InvalidIssueTokens = append(result.InvalidIssueTokens, token)
		}
	}
	return result, nil
}
