package services

import (
	"context"
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
	if len(tokens) > maxStructureSelections {
		return result, appError(CodeInvalidRequest, "目录修复选择过多", nil)
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
	// One statement pins the summary and membership together without reserving
	// SQLite's writer. A left join still returns the library when no token lives.
	var rows []struct {
		ID                  uint
		BaselineGeneration  uint64
		StructureStatus     string
		StructureIssueCount int
		StructureErrorCode  string
		StructureCheckedAt  *time.Time
		IssueToken          *string
	}
	err := s.db.WithContext(ctx).Table("media_libraries AS l").
		Select("l.id,l.baseline_generation,l.structure_status,l.structure_issue_count,l.structure_error_code,l.structure_checked_at,i.token AS issue_token").
		Joins("LEFT JOIN media_library_structure_diagnoses AS d ON d.library_id=l.id AND d.status IN ?", []string{models.MediaLibraryStructureHealthy, models.MediaLibraryStructureIssues}).
		Joins("LEFT JOIN media_library_structure_issues AS i ON i.library_id=l.id AND i.diagnosis_job_id=d.job_id AND i.generation=d.generation AND i.token IN ?", unique).
		Where("l.id = ?", libraryID).Scan(&rows).Error
	if err != nil {
		return result, err
	}
	if len(rows) == 0 {
		return result, mediaLibraryNotFound(gorm.ErrRecordNotFound)
	}
	first := rows[0]
	result.Revision = structureDiagnosticRevision(models.MediaLibrary{ID: first.ID, BaselineGeneration: first.BaselineGeneration, StructureStatus: first.StructureStatus, StructureIssueCount: first.StructureIssueCount, StructureErrorCode: first.StructureErrorCode, StructureCheckedAt: first.StructureCheckedAt})
	valid := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.IssueToken != nil {
			valid[*row.IssueToken] = true
		}
	}
	for _, token := range unique {
		if !valid[token] {
			result.InvalidIssueTokens = append(result.InvalidIssueTokens, token)
		}
	}
	return result, nil
}
