package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const structureReviewManualRecognition = "manual_recognition"

type MediaLibraryStructureReviewChoiceInput struct {
	DiagnosisRevision string `json:"diagnosis_revision"`
	ReviewRevision    uint64 `json:"review_revision"`
	Action            string `json:"action"`
	MemberToken       string `json:"member_token,omitempty"`
}

type MediaLibraryStructureReviewChoiceResult struct {
	ReviewRevision uint64                          `json:"review_revision"`
	Choice         *MediaLibraryStructureSelection `json:"choice,omitempty"`
}

type MediaLibraryStructureReviewBulkInput struct {
	DiagnosisRevision string   `json:"diagnosis_revision"`
	ReviewRevision    uint64   `json:"review_revision"`
	Codes             []string `json:"codes"`
	Action            string   `json:"action"`
}

type MediaLibraryStructureReviewBulkResult struct {
	ReviewRevision uint64 `json:"review_revision"`
	Updated        int    `json:"updated"`
}

func (s *MediaLibraryStructureService) SaveStructureReviewBulk(ctx context.Context, actor Actor, libraryID uint, input MediaLibraryStructureReviewBulkInput, request RequestContext) (MediaLibraryStructureReviewBulkResult, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureReviewBulkResult{}, appError(CodePermissionDenied, "无权修改目录处理工作区", nil)
	}
	input.Action = strings.TrimSpace(input.Action)
	if input.Action != StructureSelectionKeepRecommended && input.Action != StructureSelectionSkip {
		return MediaLibraryStructureReviewBulkResult{}, appError(CodeInvalidRequest, "批量处理方式无效", nil)
	}
	codes := make([]string, 0, len(input.Codes))
	seen := map[string]struct{}{}
	for _, raw := range input.Codes {
		code := safeLabel(strings.TrimSpace(raw), 64)
		if code != "" {
			if _, ok := seen[code]; !ok {
				seen[code] = struct{}{}
				codes = append(codes, code)
			}
		}
	}
	if len(codes) == 0 {
		return MediaLibraryStructureReviewBulkResult{}, appError(CodeInvalidRequest, "请选择要批量处理的问题类型", nil)
	}
	var result MediaLibraryStructureReviewBulkResult
	var admission *CatalogWriteAdmission
	if s.catalogStore != nil {
		admission = s.catalogStore.Admission()
	}
	err := withForegroundTransaction(ctx, s.db, admission, func(tx *gorm.DB) error {
		var library models.MediaLibrary
		if err := tx.First(&library, libraryID).Error; err != nil {
			return mediaLibraryNotFound(err)
		}
		if input.DiagnosisRevision == "" || input.DiagnosisRevision != structureDiagnosticRevision(library) {
			return appError(CodeConflict, "目录诊断结果已变化，请刷新后重试", nil)
		}
		var diagnosis models.MediaLibraryStructureDiagnosis
		if err := tx.Where("library_id = ?", libraryID).First(&diagnosis).Error; err != nil {
			return appError(CodeConflict, "目录诊断结果不存在，请重新检测", err)
		}
		rowScope := tx.Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND code IN ? AND code <> ?", libraryID, diagnosis.JobID, diagnosis.Generation, codes, "missing_season_episode")
		if input.Action == StructureSelectionKeepRecommended {
			rowScope = rowScope.Where("code IN ? AND conflict_source_count > 1", []string{"duplicate_target", "sidecar_target_conflict"})
		}
		var rows []models.MediaLibraryStructureIssue
		if err := rowScope.Order("id").Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return appError(CodeInvalidRequest, "当前筛选没有可批量处理的问题", nil)
		}
		now := time.Now().UTC()
		var session models.MediaLibraryStructureReviewSession
		err := tx.Where("owner_id = ? AND library_id = ? AND diagnosis_job_id = ?", actor.User.ID, libraryID, diagnosis.JobID).First(&session).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if input.ReviewRevision != 0 {
				return appError(CodeConflict, "处理工作区已变化，请刷新后重试", nil)
			}
			session = models.MediaLibraryStructureReviewSession{ID: uuid.NewString(), OwnerID: actor.User.ID, LibraryID: libraryID, DiagnosisJobID: diagnosis.JobID, CreatedAt: now, UpdatedAt: now}
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if session.Revision != input.ReviewRevision {
			return appError(CodeConflict, "处理工作区已被其他页面修改，请刷新后重试", nil)
		}
		var submitted int64
		submittedJoin := "JOIN media_library_structure_issues i ON i.token = media_library_structure_review_choices.issue_token AND i.library_id = ? AND i.diagnosis_job_id = ? AND i.generation = ? AND i.code IN ? AND i.code <> ?"
		submittedArgs := []any{libraryID, diagnosis.JobID, diagnosis.Generation, codes, "missing_season_episode"}
		if input.Action == StructureSelectionKeepRecommended {
			submittedJoin += " AND i.code IN ? AND i.conflict_source_count > 1"
			submittedArgs = append(submittedArgs, []string{"duplicate_target", "sidecar_target_conflict"})
		}
		if err := tx.Model(&models.MediaLibraryStructureReviewChoice{}).
			Joins(submittedJoin, submittedArgs...).
			Where("media_library_structure_review_choices.session_id = ? AND media_library_structure_review_choices.subject_kind = ? AND media_library_structure_review_choices.state = ?", session.ID, "issue", "submitted").
			Count(&submitted).Error; err != nil {
			return err
		}
		if submitted > 0 {
			return appError(CodeConflict, "批量范围包含已提交执行的选择，请重新检测后再处理", nil)
		}
		choices := make([]models.MediaLibraryStructureReviewChoice, 0, len(rows))
		for _, row := range rows {
			action, member := input.Action, ""
			if action == StructureSelectionKeepRecommended {
				if structureConflictRequiresReview(row.Code) || row.RecommendedMemberToken == "" {
					action = StructureSelectionSkip
				} else {
					member = row.RecommendedMemberToken
				}
			}
			choices = append(choices, models.MediaLibraryStructureReviewChoice{SessionID: session.ID, SubjectKey: "issue:" + row.Token, SubjectKind: "issue", IssueToken: row.Token, Action: action, MemberToken: member, State: "draft", CreatedAt: now, UpdatedAt: now})
		}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "session_id"}, {Name: "subject_key"}}, DoUpdates: clause.AssignmentColumns([]string{"action", "member_token", "state", "updated_at"})}).CreateInBatches(&choices, 500).Error; err != nil {
			return err
		}
		next := session.Revision + 1
		update := tx.Model(&models.MediaLibraryStructureReviewSession{}).Where("id = ? AND revision = ?", session.ID, session.Revision).Updates(map[string]any{"revision": next, "updated_at": now})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return appError(CodeConflict, "处理工作区已被其他页面修改，请刷新后重试", nil)
		}
		result.ReviewRevision, result.Updated = next, len(rows)
		if s.audit != nil {
			return s.audit.Record(tx, &actor.User.ID, "media_library.structure_review.bulk", "media_library", uintID(libraryID), "success", map[string]any{"action": input.Action, "count": len(rows)}, request)
		}
		return nil
	})
	return result, err
}

func (s *MediaLibraryStructureService) mergeStructureReviewSelections(ctx context.Context, actor Actor, libraryID uint, input MediaLibraryStructureSelectionInput) (MediaLibraryStructureSelectionInput, models.MediaLibraryStructureReviewSession, error) {
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).First(&diagnosis).Error; err != nil {
		return input, models.MediaLibraryStructureReviewSession{}, appError(CodeConflict, "目录诊断结果不存在，请重新检测", err)
	}
	var session models.MediaLibraryStructureReviewSession
	err := s.db.WithContext(ctx).Where("owner_id = ? AND library_id = ? AND diagnosis_job_id = ?", actor.User.ID, libraryID, diagnosis.JobID).First(&session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if input.ReviewRevision != 0 {
			return input, session, appError(CodeConflict, "处理工作区已变化，请刷新后重新预览", nil)
		}
		return input, models.MediaLibraryStructureReviewSession{}, nil
	}
	if err != nil {
		return input, session, err
	}
	if session.Revision != input.ReviewRevision {
		return input, session, appError(CodeConflict, "处理工作区已变化，请刷新后重新预览", nil)
	}
	input.reviewSessionID = session.ID
	query := s.db.WithContext(ctx).Model(&models.MediaLibraryStructureReviewChoice{}).
		Where("media_library_structure_review_choices.session_id = ? AND media_library_structure_review_choices.subject_kind = ? AND media_library_structure_review_choices.state = ?", session.ID, "issue", "draft")
	if code := safeLabel(strings.TrimSpace(input.ReviewCode), 64); code != "" {
		query = query.Joins("JOIN media_library_structure_issues i ON i.token = media_library_structure_review_choices.issue_token AND i.library_id = ? AND i.diagnosis_job_id = ? AND i.generation = ? AND i.code = ?", libraryID, diagnosis.JobID, diagnosis.Generation, code)
	}
	var choices []models.MediaLibraryStructureReviewChoice
	if err := query.Order("media_library_structure_review_choices.id").Find(&choices).Error; err != nil {
		return input, session, err
	}
	byIssue := make(map[string]MediaLibraryStructureSelection, len(choices)+len(input.Selections))
	for _, choice := range choices {
		byIssue[choice.IssueToken] = MediaLibraryStructureSelection{IssueToken: choice.IssueToken, Action: choice.Action, MemberToken: choice.MemberToken}
	}
	for _, selection := range input.Selections {
		byIssue[selection.IssueToken] = selection
	}
	input.Selections = input.Selections[:0]
	for _, selection := range byIssue {
		input.Selections = append(input.Selections, selection)
	}
	return input, session, nil
}

func (s *MediaLibraryStructureService) StructureIssueMembers(ctx context.Context, actor Actor, libraryID uint, token string, page, pageSize int) (MediaLibraryStructureIssueMemberPage, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureIssueMemberPage{}, appError(CodePermissionDenied, "无权查看媒体库结构", nil)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return MediaLibraryStructureIssueMemberPage{}, appError(CodeInvalidRequest, "目录问题标识无效", nil)
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	result := MediaLibraryStructureIssueMemberPage{List: []MediaLibraryStructureIssueMemberSummary{}, Page: page, PageSize: pageSize}
	err := s.withCatalogRead(ctx, libraryID, func(tx *gorm.DB, _ *CatalogReader) error {
		query, err := currentStructureIssueScopeTx(tx, tx.Model(&models.MediaLibraryStructureIssue{}).Where("library_id = ? AND token = ?", libraryID, token), libraryID)
		if err != nil {
			return err
		}
		var issue models.MediaLibraryStructureIssue
		if err := query.First(&issue).Error; err != nil {
			return appError(CodeNotFound, "目录问题不存在或已变化", err)
		}
		if err := tx.Model(&models.MediaLibraryStructureIssueMember{}).Where("issue_id = ?", issue.ID).Count(&result.Total).Error; err != nil {
			return err
		}
		var members []models.MediaLibraryStructureIssueMember
		if err := tx.Where("issue_id = ?", issue.ID).Order("id").Offset((page - 1) * pageSize).Limit(pageSize).Find(&members).Error; err != nil {
			return err
		}
		for _, member := range members {
			path := safeStructurePath(member.SourcePath)
			if path != "" {
				result.List = append(result.List, MediaLibraryStructureIssueMemberSummary{Token: member.Token, SourcePath: path, Recommended: member.Recommended})
			}
		}
		return nil
	})
	return result, err
}

func (s *MediaLibraryStructureService) SaveStructureReviewChoice(ctx context.Context, actor Actor, libraryID uint, issueToken string, input MediaLibraryStructureReviewChoiceInput, request RequestContext) (MediaLibraryStructureReviewChoiceResult, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureReviewChoiceResult{}, appError(CodePermissionDenied, "无权修改目录处理工作区", nil)
	}
	issueToken, input.Action, input.MemberToken = strings.TrimSpace(issueToken), strings.TrimSpace(input.Action), strings.TrimSpace(input.MemberToken)
	if issueToken == "" || !validStructureSelectionAction(input.Action) {
		return MediaLibraryStructureReviewChoiceResult{}, appError(CodeInvalidRequest, "目录处理选择无效", nil)
	}
	return s.writeStructureReviewChoice(ctx, actor, libraryID, "issue", issueToken, 0, input, request, false)
}

func (s *MediaLibraryStructureService) DeleteStructureReviewChoice(ctx context.Context, actor Actor, libraryID uint, issueToken string, input MediaLibraryStructureReviewChoiceInput, request RequestContext) (MediaLibraryStructureReviewChoiceResult, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureReviewChoiceResult{}, appError(CodePermissionDenied, "无权修改目录处理工作区", nil)
	}
	issueToken = strings.TrimSpace(issueToken)
	if issueToken == "" {
		return MediaLibraryStructureReviewChoiceResult{}, appError(CodeInvalidRequest, "目录问题标识无效", nil)
	}
	return s.writeStructureReviewChoice(ctx, actor, libraryID, "issue", issueToken, 0, input, request, true)
}

func (s *MediaLibraryStructureService) SaveStructureRecognitionReview(ctx context.Context, actor Actor, libraryID uint, recognitionToken string, input MediaLibraryStructureReviewChoiceInput, request RequestContext) (MediaLibraryStructureReviewChoiceResult, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureReviewChoiceResult{}, appError(CodePermissionDenied, "无权修改目录处理工作区", nil)
	}
	recognitionID, err := decodeRecognitionToken(strings.TrimSpace(recognitionToken))
	if err != nil {
		return MediaLibraryStructureReviewChoiceResult{}, err
	}
	input.Action, input.MemberToken = structureReviewManualRecognition, ""
	return s.writeStructureReviewChoice(ctx, actor, libraryID, "recognition", "", recognitionID, input, request, false)
}

func (s *MediaLibraryStructureService) DeleteStructureRecognitionReview(ctx context.Context, actor Actor, libraryID uint, recognitionToken string, input MediaLibraryStructureReviewChoiceInput, request RequestContext) (MediaLibraryStructureReviewChoiceResult, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureReviewChoiceResult{}, appError(CodePermissionDenied, "无权修改目录处理工作区", nil)
	}
	recognitionID, err := decodeRecognitionToken(strings.TrimSpace(recognitionToken))
	if err != nil {
		return MediaLibraryStructureReviewChoiceResult{}, err
	}
	return s.writeStructureReviewChoice(ctx, actor, libraryID, "recognition", "", recognitionID, input, request, true)
}

func (s *MediaLibraryStructureService) writeStructureReviewChoice(ctx context.Context, actor Actor, libraryID uint, subjectKind, issueToken string, recognitionID uint, input MediaLibraryStructureReviewChoiceInput, request RequestContext, remove bool) (MediaLibraryStructureReviewChoiceResult, error) {
	var result MediaLibraryStructureReviewChoiceResult
	var admission *CatalogWriteAdmission
	if s.catalogStore != nil {
		admission = s.catalogStore.Admission()
	}
	err := withForegroundTransaction(ctx, s.db, admission, func(tx *gorm.DB) error {
		var library models.MediaLibrary
		if err := tx.First(&library, libraryID).Error; err != nil {
			return mediaLibraryNotFound(err)
		}
		if input.DiagnosisRevision == "" || input.DiagnosisRevision != structureDiagnosticRevision(library) {
			return appError(CodeConflict, "目录诊断结果已变化，请刷新后重试", nil)
		}
		var diagnosis models.MediaLibraryStructureDiagnosis
		if err := tx.Where("library_id = ?", libraryID).First(&diagnosis).Error; err != nil {
			return appError(CodeConflict, "目录诊断结果不存在，请重新检测", err)
		}
		if diagnosis.Status != models.MediaLibraryStructureHealthy && diagnosis.Status != models.MediaLibraryStructureIssues {
			return appError(CodeConflict, "目录诊断尚未完成或已经变化", nil)
		}
		var issue models.MediaLibraryStructureIssue
		if subjectKind == "issue" {
			if err := tx.Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND token = ?", libraryID, diagnosis.JobID, diagnosis.Generation, issueToken).First(&issue).Error; err != nil {
				return appError(CodeConflict, "目录问题不存在或已变化", err)
			}
			if !remove {
				if err := validateReviewChoiceTx(tx, issue, input); err != nil {
					return err
				}
			}
		} else {
			if recognitionID == 0 {
				return appError(CodeInvalidRequest, "识别记录无效", nil)
			}
			var count int64
			if remove {
				// Restoring automatic recognition may make the projected issue
				// disappear before the UI removes its personal review marker. The
				// effective recognition still proves the subject belongs to this
				// library, so an idempotent marker deletion remains valid. Read it
				// through the pinned catalog projection; the legacy anchor table is
				// not authoritative while a versioned catalog is active.
				reader, err := PinCatalogTx(tx, []uint{libraryID})
				if err != nil {
					return err
				}
				if err := reader.Recognitions().Where("library_id = ? AND id = ?", libraryID, recognitionID).Count(&count).Error; err != nil {
					return err
				}
			} else if err := tx.Model(&models.MediaLibraryStructureIssue{}).
				Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND recognition_id = ?", libraryID, diagnosis.JobID, diagnosis.Generation, recognitionID).
				Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return appError(CodeConflict, "识别结果已不属于当前媒体库或检测任务", nil)
			}
		}
		now := time.Now().UTC()
		var session models.MediaLibraryStructureReviewSession
		err := tx.Where("owner_id = ? AND library_id = ? AND diagnosis_job_id = ?", actor.User.ID, libraryID, diagnosis.JobID).First(&session).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if input.ReviewRevision != 0 {
				return appError(CodeConflict, "处理工作区已变化，请刷新后重试", nil)
			}
			if remove {
				result.ReviewRevision = 0
				return nil
			}
			session = models.MediaLibraryStructureReviewSession{ID: uuid.NewString(), OwnerID: actor.User.ID, LibraryID: libraryID, DiagnosisJobID: diagnosis.JobID, Revision: 0, CreatedAt: now, UpdatedAt: now}
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if session.Revision != input.ReviewRevision {
			return appError(CodeConflict, "处理工作区已被其他页面修改，请刷新后重试", nil)
		}
		subjectKey := "issue:" + issueToken
		if subjectKind == "recognition" {
			subjectKey = fmt.Sprintf("recognition:%d", recognitionID)
		}
		var existing models.MediaLibraryStructureReviewChoice
		existingErr := tx.Where("session_id = ? AND subject_key = ?", session.ID, subjectKey).First(&existing).Error
		if existingErr != nil && !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return existingErr
		}
		if existingErr == nil && existing.State == "submitted" {
			return appError(CodeConflict, "该选择已提交执行，不能在当前工作区修改或撤销，请重新检测", nil)
		}
		if remove {
			if errors.Is(existingErr, gorm.ErrRecordNotFound) {
				result.ReviewRevision = session.Revision
				return nil
			}
			if err := tx.Delete(&existing).Error; err != nil {
				return err
			}
		} else {
			if existingErr == nil && existing.Action == input.Action && existing.MemberToken == input.MemberToken && existing.State == "draft" {
				result.ReviewRevision = session.Revision
				result.Choice = &MediaLibraryStructureSelection{IssueToken: issueToken, Action: existing.Action, MemberToken: existing.MemberToken}
				return nil
			}
			choice := models.MediaLibraryStructureReviewChoice{SessionID: session.ID, SubjectKey: subjectKey, SubjectKind: subjectKind, IssueToken: issueToken, Action: input.Action, MemberToken: input.MemberToken, State: "draft", CreatedAt: now, UpdatedAt: now}
			if recognitionID != 0 {
				choice.RecognitionID = &recognitionID
			}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "session_id"}, {Name: "subject_key"}}, DoUpdates: clause.Assignments(map[string]any{"action": choice.Action, "member_token": choice.MemberToken, "state": "draft", "issue_token": choice.IssueToken, "recognition_id": choice.RecognitionID, "updated_at": now})}).Create(&choice).Error; err != nil {
				return err
			}
			result.Choice = &MediaLibraryStructureSelection{IssueToken: issueToken, Action: choice.Action, MemberToken: choice.MemberToken}
		}
		next := session.Revision + 1
		updated := tx.Model(&models.MediaLibraryStructureReviewSession{}).Where("id = ? AND revision = ?", session.ID, session.Revision).Updates(map[string]any{"revision": next, "updated_at": now})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return appError(CodeConflict, "处理工作区已被其他页面修改，请刷新后重试", nil)
		}
		result.ReviewRevision = next
		if s.audit != nil {
			action := "save"
			if remove {
				action = "undo"
			}
			if err := s.audit.Record(tx, &actor.User.ID, "media_library.structure_review."+action, "media_library", uintID(libraryID), "success", map[string]any{"subject_kind": subjectKind, "action": input.Action}, request); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func validateReviewChoiceTx(tx *gorm.DB, issue models.MediaLibraryStructureIssue, input MediaLibraryStructureReviewChoiceInput) error {
	physicalConflict := issue.Code == "duplicate_target" || issue.Code == "sidecar_target_conflict"
	allowed := input.Action == StructureSelectionSkip
	if issue.Repairable || issue.State == "manual_identity_resolved" {
		allowed = allowed || input.Action == StructureSelectionRepair
	}
	if physicalConflict && issue.ConflictSourceCount > 1 {
		allowed = allowed || input.Action == StructureSelectionKeepRecommended || input.Action == StructureSelectionKeepMember || input.Action == StructureSelectionKeepAllVersions
	}
	if !allowed {
		return appError(CodeInvalidRequest, "该问题不支持所选处理方式", nil)
	}
	if input.Action == StructureSelectionKeepRecommended && issue.RecommendedMemberToken == "" {
		return appError(CodeConflict, "该冲突没有唯一推荐来源，请手动选择", nil)
	}
	if input.Action == StructureSelectionKeepMember {
		if input.MemberToken == "" {
			return appError(CodeInvalidRequest, "请选择要保留的来源", nil)
		}
		var count int64
		if err := tx.Model(&models.MediaLibraryStructureIssueMember{}).Where("issue_id = ? AND token = ?", issue.ID, input.MemberToken).Count(&count).Error; err != nil {
			return err
		}
		if count != 1 {
			return appError(CodeConflict, "所选来源不存在或已变化", nil)
		}
	} else if input.MemberToken != "" {
		return appError(CodeInvalidRequest, "当前操作不接受来源选择", nil)
	}
	return nil
}
