package services

import (
	"container/heap"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const (
	StructureSelectionRepair              = "repair"
	StructureSelectionKeepRecommended     = "keep_recommended"
	StructureSelectionKeepMember          = "keep_member"
	StructureSelectionKeepAllVersions     = "keep_all_versions"
	StructureSelectionSkip                = "skip"
	maxStructureSelections                = 5000
	maxStructureAutomaticSelections       = 25000
	maxStructurePreviewResponseSelections = maxStructureSelections
	maxStructureSelectionBytes            = 32 * 1024 * 1024
	structureSelectionConfirmationExpiry  = 5 * time.Minute
)

type MediaLibraryStructureSelection struct {
	IssueToken  string `json:"issue_token"`
	Action      string `json:"action"`
	MemberToken string `json:"member_token,omitempty"`
}

type MediaLibraryStructureBulkAction struct {
	Codes  []string `json:"codes"`
	Action string   `json:"action"`
}

type MediaLibraryStructureSelectionInput struct {
	Revision                string                            `json:"revision"`
	ReviewRevision          uint64                            `json:"review_revision,omitempty"`
	ReviewCode              string                            `json:"review_code,omitempty"`
	IncludeAutomaticRepairs bool                              `json:"include_automatic_repairs,omitempty"`
	Selections              []MediaLibraryStructureSelection  `json:"selections"`
	BulkActions             []MediaLibraryStructureBulkAction `json:"bulk_actions,omitempty"`
	reviewSessionID         string
}

type MediaLibraryStructureSelectionPreview struct {
	LibraryID         uint                             `json:"library_id"`
	Revision          string                           `json:"revision"`
	IssueCount        int                              `json:"issue_count"`
	RecycleCount      int                              `json:"recycle_count"`
	MoveCount         int                              `json:"move_count"`
	SkippedCount      int                              `json:"skipped_count"`
	Selections        []MediaLibraryStructureSelection `json:"selections,omitempty"`
	ConfirmationToken string                           `json:"confirmation_token"`
	ExpiresAt         time.Time                        `json:"expires_at"`
	Items             MediaLibraryStructurePreviewPage `json:"items"`
}

type structureSelectionResolved struct {
	issue     models.MediaLibraryStructureIssue
	members   []models.MediaLibraryStructureIssueMember
	selection MediaLibraryStructureSelection
}

func (s *MediaLibraryStructureService) PreviewSelectionRepair(ctx context.Context, actor Actor, libraryID uint, input MediaLibraryStructureSelectionInput) (MediaLibraryStructureSelectionPreview, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructureSelectionPreview{}, appError(CodePermissionDenied, "无权修复媒体库结构", nil)
	}
	var reviewSession models.MediaLibraryStructureReviewSession
	var err error
	input, reviewSession, err = s.mergeStructureReviewSelections(ctx, actor, libraryID, input)
	if err != nil {
		return MediaLibraryStructureSelectionPreview{}, err
	}
	draftID := uuid.NewString()
	plan, diagnosis, resolved, err := s.buildSelectionPlan(ctx, libraryID, input, draftID)
	if err != nil {
		return MediaLibraryStructureSelectionPreview{}, err
	}
	if err := s.validateSelectionRecycle(ctx, libraryID, plan.RecycleItems); err != nil {
		return MediaLibraryStructureSelectionPreview{}, err
	}
	planHash, err := structurePlanHash(plan)
	if err != nil {
		return MediaLibraryStructureSelectionPreview{}, err
	}
	selections := make([]MediaLibraryStructureSelection, 0, len(resolved))
	for _, item := range resolved {
		selections = append(selections, item.selection)
	}
	selectionJSON, err := json.Marshal(MediaLibraryStructureSelectionInput{Revision: input.Revision, Selections: selections})
	if err != nil || len(selectionJSON) > maxStructureSelectionBytes {
		return MediaLibraryStructureSelectionPreview{}, appError(CodeInvalidRequest, "目录修复选择过多", err)
	}
	var autoState models.MediaLibraryStructureAutoState
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).First(&autoState).Error; err != nil {
		return MediaLibraryStructureSelectionPreview{}, err
	}
	expires := time.Now().UTC().Add(structureSelectionConfirmationExpiry)
	draft := models.MediaLibraryStructureRepairDraft{ID: draftID, OwnerID: actor.User.ID, LibraryID: libraryID, DiagnosisJobID: diagnosis.JobID, ReviewSessionID: reviewSession.ID, ReviewRevision: input.ReviewRevision, SourceRevision: autoState.SourceRevision, Generation: plan.Generation, RuleFingerprint: plan.RuleFingerprint, PlanHash: planHash, SelectionsJSON: string(selectionJSON), ExpiresAt: expires, CreatedAt: time.Now().UTC()}
	previewItems, err := structurePreviewItems(plan, resolved)
	if err != nil {
		return MediaLibraryStructureSelectionPreview{}, err
	}
	previewJSON, err := json.Marshal(previewItems)
	if err != nil || len(previewJSON) > maxStructurePreviewBytes {
		return MediaLibraryStructureSelectionPreview{}, appError(CodeInvalidRequest, "目录修复预览过大，请分类型或减少选择后重试", err)
	}
	draft.PreviewItemsJSON = string(previewJSON)
	var admission *CatalogWriteAdmission
	if s.catalogStore != nil {
		admission = s.catalogStore.Admission()
	}
	if err := withForegroundTransaction(ctx, s.db, admission, func(tx *gorm.DB) error {
		if err := validateCatalogStructureSelectionTx(tx, plan, true); err != nil {
			return err
		}
		if plan.catalogFence != nil {
			reader, err := PinCatalogTx(tx, []uint{libraryID})
			if err != nil {
				return err
			}
			if err := validateStructureLogicalFenceTx(tx, reader, *plan.catalogFence); err != nil {
				return err
			}
		}
		return tx.Create(&draft).Error
	}); err != nil {
		return MediaLibraryStructureSelectionPreview{}, err
	}
	claim := mediaLibraryStructureClaim{DraftID: draft.ID, ActorID: actor.User.ID, LibraryID: libraryID, Generation: plan.Generation, RuleFingerprint: plan.RuleFingerprint, PlanHash: planHash, ExpiresAt: expires.Unix()}
	token, err := s.signStructureClaim(claim)
	if err != nil {
		_ = s.db.Delete(&draft).Error
		return MediaLibraryStructureSelectionPreview{}, err
	}
	skipped := 0
	for _, selection := range selections {
		if selection.Action == StructureSelectionSkip {
			skipped++
		}
	}
	responseSelections := structurePreviewResponseSelections(selections)
	return MediaLibraryStructureSelectionPreview{LibraryID: libraryID, Revision: input.Revision, IssueCount: len(selections), RecycleCount: len(plan.RecycleItems), MoveCount: len(plan.Items), SkippedCount: skipped, Selections: responseSelections, ConfirmationToken: token, ExpiresAt: expires, Items: structurePreviewPage(previewItems, 1, 50)}, nil
}

func structurePreviewResponseSelections(selections []MediaLibraryStructureSelection) []MediaLibraryStructureSelection {
	if len(selections) > maxStructurePreviewResponseSelections {
		// The immutable draft retains every resolved issue. The browser confirms
		// with its opaque token and does not need a second copy of a large
		// automatic selection set in the initial HTTP response.
		return nil
	}
	return selections
}

func (s *MediaLibraryStructureService) EnqueueSelectionRepair(ctx context.Context, actor Actor, libraryID uint, confirmationToken string, request RequestContext) (models.MediaLibraryStructureRepair, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return models.MediaLibraryStructureRepair{}, appError(CodePermissionDenied, "无权修复媒体库结构", nil)
	}
	claim, err := s.verifyStructureClaim(confirmationToken)
	if err != nil || claim.DraftID == "" || claim.ActorID != actor.User.ID || claim.LibraryID != libraryID || claim.ExpiresAt < time.Now().UTC().Unix() {
		return models.MediaLibraryStructureRepair{}, appError(CodeInvalidRequest, "目录修复确认已失效，请重新预览", err)
	}
	var draft models.MediaLibraryStructureRepairDraft
	if err := s.db.WithContext(ctx).Where("id = ? AND owner_id = ? AND library_id = ?", claim.DraftID, actor.User.ID, libraryID).First(&draft).Error; err != nil || draft.ConsumedAt != nil || draft.ExpiresAt.Before(time.Now().UTC()) {
		return models.MediaLibraryStructureRepair{}, appError(CodeInvalidRequest, "目录修复确认已失效，请重新预览", err)
	}
	var input MediaLibraryStructureSelectionInput
	if err := json.Unmarshal([]byte(draft.SelectionsJSON), &input); err != nil {
		return models.MediaLibraryStructureRepair{}, appError(CodeInvalidRequest, "目录修复选择已失效", err)
	}
	plan, diagnosis, _, err := s.buildSelectionPlan(ctx, libraryID, input, draft.ID)
	if err != nil {
		return models.MediaLibraryStructureRepair{}, err
	}
	if err := s.validateSelectionRecycle(ctx, libraryID, plan.RecycleItems); err != nil {
		return models.MediaLibraryStructureRepair{}, err
	}
	planHash, err := structurePlanHash(plan)
	if err != nil {
		return models.MediaLibraryStructureRepair{}, err
	}
	var autoState models.MediaLibraryStructureAutoState
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).First(&autoState).Error; err != nil {
		return models.MediaLibraryStructureRepair{}, err
	}
	if diagnosis.JobID != draft.DiagnosisJobID || autoState.SourceRevision != draft.SourceRevision || claim.Generation != draft.Generation || plan.Generation != draft.Generation || claim.RuleFingerprint != draft.RuleFingerprint || plan.RuleFingerprint != draft.RuleFingerprint || !hmac.Equal([]byte(claim.PlanHash), []byte(draft.PlanHash)) || !hmac.Equal([]byte(planHash), []byte(draft.PlanHash)) {
		return models.MediaLibraryStructureRepair{}, appError(CodeConflict, "媒体库来源、诊断结果或分类规则已变化，请重新预览", nil)
	}
	var review models.MediaLibraryStructureReviewSession
	reviewErr := s.db.WithContext(ctx).Where("owner_id = ? AND library_id = ? AND diagnosis_job_id = ?", actor.User.ID, libraryID, diagnosis.JobID).First(&review).Error
	if draft.ReviewSessionID == "" {
		if reviewErr == nil || !errors.Is(reviewErr, gorm.ErrRecordNotFound) {
			return models.MediaLibraryStructureRepair{}, appError(CodeConflict, "处理工作区已修改，请重新生成预览", reviewErr)
		}
	} else if reviewErr != nil || review.ID != draft.ReviewSessionID || review.Revision != draft.ReviewRevision {
		return models.MediaLibraryStructureRepair{}, appError(CodeConflict, "处理工作区已修改，请重新生成预览", reviewErr)
	}
	return s.enqueueSelectionPlan(actor, draft, plan, request)
}

func (s *MediaLibraryStructureService) validateSelectionRecycle(ctx context.Context, libraryID uint, items []StructureRecycleItem) error {
	if len(items) == 0 {
		return nil
	}
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, libraryID).Error; err != nil {
		return mediaLibraryNotFound(err)
	}
	var storage models.Storage
	if err := s.db.WithContext(ctx).First(&storage, library.StorageID).Error; err != nil {
		return appError(CodeMediaLibraryStructureUnavailable, "媒体库数据源不可用", nil)
	}
	backend, err := s.backends.Get(storage.Type)
	if err != nil || backend.ValidateRecycle(ctx, StructureBoundary{Library: library, Storage: storage}) != nil {
		return appError(CodeMediaLibraryStructureUnavailable, "当前数据源不支持可恢复回收，已拒绝覆盖操作", nil)
	}
	return nil
}

func (s *MediaLibraryStructureService) enqueueSelectionPlan(actor Actor, draft models.MediaLibraryStructureRepairDraft, plan StructurePlan, request RequestContext) (models.MediaLibraryStructureRepair, error) {
	raw, err := json.Marshal(plan)
	if err != nil || len(raw) > maxStructureSelectionBytes {
		return models.MediaLibraryStructureRepair{}, appError(CodeMediaLibraryStructureUnavailable, "媒体库修复计划过大", err)
	}
	var library models.MediaLibrary
	if err := s.db.First(&library, draft.LibraryID).Error; err != nil {
		return models.MediaLibraryStructureRepair{}, mediaLibraryNotFound(err)
	}
	var active models.MediaLibraryStructureRepair
	query := s.db.Where("library_id = ? AND scope = ? AND work_key = '' AND phase IN ?", draft.LibraryID, models.MediaLibraryStructureScopeFull, []string{"queued", "executing", "reconciling"}).Order("created_at DESC").First(&active)
	if query.Error == nil {
		return models.MediaLibraryStructureRepair{}, appError(CodeConflict, "已有媒体库结构修复任务正在执行", nil)
	}
	if !errors.Is(query.Error, gorm.ErrRecordNotFound) {
		return models.MediaLibraryStructureRepair{}, query.Error
	}
	now := time.Now().UTC()
	repair := models.MediaLibraryStructureRepair{ID: uuid.NewString(), OwnerID: actor.User.ID, LibraryID: draft.LibraryID, Scope: models.MediaLibraryStructureScopeFull, RuleFingerprint: plan.RuleFingerprint, Generation: plan.Generation, PlanJSON: string(raw), StateJSON: `{}`, Phase: "queued", IssueCount: len(plan.ResolvedIssues) + len(plan.SkippedIssues), TotalItems: len(plan.RecycleItems) + len(plan.Items), CreatedAt: now, UpdatedAt: now}
	job, err := s.queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: JobTypeMediaLibraryRepair, DisplayName: "修复媒体库结构 · " + library.Name, Provider: "media_library", ResourceKey: "library:" + uintID(draft.LibraryID), Payload: mediaLibraryRepairJobPayload{RepairID: repair.ID}}, func(tx *gorm.DB, job models.Job) error {
		// Revalidate the mutable review workspace inside the same transaction
		// that consumes the frozen draft. The earlier read is only a fast
		// rejection; without this fence a concurrent edit could land between
		// that read and enqueue, allowing a stale preview to execute.
		if draft.ReviewSessionID == "" {
			var sessions int64
			if err := tx.Model(&models.MediaLibraryStructureReviewSession{}).
				Where("owner_id = ? AND library_id = ? AND diagnosis_job_id = ?", actor.User.ID, draft.LibraryID, draft.DiagnosisJobID).
				Count(&sessions).Error; err != nil {
				return err
			}
			if sessions != 0 {
				return appError(CodeConflict, "处理工作区已修改，请重新生成预览", nil)
			}
		} else {
			var review models.MediaLibraryStructureReviewSession
			if err := tx.Where("id = ? AND owner_id = ? AND library_id = ? AND diagnosis_job_id = ? AND revision = ?", draft.ReviewSessionID, actor.User.ID, draft.LibraryID, draft.DiagnosisJobID, draft.ReviewRevision).First(&review).Error; err != nil {
				return appError(CodeConflict, "处理工作区已修改，请重新生成预览", err)
			}
		}
		consumed := tx.Model(&models.MediaLibraryStructureRepairDraft{}).Where("id = ? AND owner_id = ? AND library_id = ? AND consumed_at IS NULL AND expires_at > ?", draft.ID, actor.User.ID, draft.LibraryID, now).Update("consumed_at", now)
		if consumed.Error != nil {
			return consumed.Error
		}
		if consumed.RowsAffected != 1 {
			return appError(CodeConflict, "目录修复确认已被使用或已经过期", nil)
		}
		repair.JobID = &job.ID
		if err := s.freezeCatalogStructureRepairTx(tx, &repair, plan); err != nil {
			return err
		}
		if err := RegisterCatalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: repair.LibraryID, OwnerKind: CatalogPhysicalRepair, OwnerID: repair.ID, ActorID: repair.OwnerID}, func(tx *gorm.DB) error { return tx.Create(&repair).Error }); err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", draft.LibraryID).Updates(map[string]any{"structure_status": models.MediaLibraryStructureRepairing, "structure_error_code": ""}).Error; err != nil {
			return err
		}
		if draft.ReviewSessionID != "" && len(plan.ResolvedIssues)+len(plan.SkippedIssues) > 0 {
			tokens := append(append([]string{}, plan.ResolvedIssues...), plan.SkippedIssues...)
			keys := make([]string, 0, len(tokens))
			for _, token := range tokens {
				keys = append(keys, "issue:"+token)
			}
			updated := tx.Model(&models.MediaLibraryStructureReviewChoice{}).Where("session_id = ? AND subject_key IN ? AND state = ?", draft.ReviewSessionID, keys, "draft").Update("state", "submitted")
			if updated.Error != nil {
				return updated.Error
			}
			advanced := tx.Model(&models.MediaLibraryStructureReviewSession{}).
				Where("id = ? AND revision = ?", draft.ReviewSessionID, draft.ReviewRevision).
				Updates(map[string]any{"revision": draft.ReviewRevision + 1, "updated_at": now})
			if advanced.Error != nil {
				return advanced.Error
			}
			if advanced.RowsAffected != 1 {
				return appError(CodeConflict, "处理工作区已修改，请重新生成预览", nil)
			}
		}
		return s.audit.Record(tx, &actor.User.ID, "media_library.structure_selection_repair.enqueue", "media_library", uintID(draft.LibraryID), "success", map[string]any{"issue_count": len(plan.ResolvedIssues), "move_count": len(plan.Items), "recycle_count": len(plan.RecycleItems)}, request)
	})
	if err != nil {
		return models.MediaLibraryStructureRepair{}, err
	}
	repair.JobID = &job.ID
	return repair, nil
}

func (s *MediaLibraryStructureService) buildSelectionPlan(ctx context.Context, libraryID uint, input MediaLibraryStructureSelectionInput, draftID string) (StructurePlan, models.MediaLibraryStructureDiagnosis, []structureSelectionResolved, error) {
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, libraryID).Error; err != nil {
		return StructurePlan{}, models.MediaLibraryStructureDiagnosis{}, nil, mediaLibraryNotFound(err)
	}
	if strings.TrimSpace(input.Revision) == "" || input.Revision != structureDiagnosticRevision(library) {
		return StructurePlan{}, models.MediaLibraryStructureDiagnosis{}, nil, appError(CodeConflict, "目录诊断结果已变化，请重新检查", nil)
	}
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).First(&diagnosis).Error; err != nil {
		return StructurePlan{}, diagnosis, nil, appError(CodeConflict, "目录诊断结果不存在，请重新诊断", err)
	}
	if diagnosis.Status != models.MediaLibraryStructureHealthy && diagnosis.Status != models.MediaLibraryStructureIssues {
		return StructurePlan{}, diagnosis, nil, appError(CodeConflict, "目录诊断尚未完成或已经过期", nil)
	}
	selectionByIssue := make(map[string]MediaLibraryStructureSelection, len(input.Selections))
	for _, selection := range input.Selections {
		selection.IssueToken, selection.MemberToken, selection.Action = strings.TrimSpace(selection.IssueToken), strings.TrimSpace(selection.MemberToken), strings.TrimSpace(selection.Action)
		if selection.IssueToken == "" || !validStructureSelectionAction(selection.Action) {
			return StructurePlan{}, diagnosis, nil, appError(CodeInvalidRequest, "目录修复选择无效", nil)
		}
		selectionByIssue[selection.IssueToken] = selection
	}
	for _, bulk := range input.BulkActions {
		bulk.Action = strings.TrimSpace(bulk.Action)
		if bulk.Action != StructureSelectionKeepRecommended && bulk.Action != StructureSelectionSkip {
			return StructurePlan{}, diagnosis, nil, appError(CodeInvalidRequest, "批量冲突操作无效", nil)
		}
		codes := make([]string, 0, len(bulk.Codes))
		for _, code := range bulk.Codes {
			if code = safeLabel(strings.TrimSpace(code), 64); code != "" && code != "all" {
				codes = append(codes, code)
			}
		}
		if len(codes) == 0 {
			return StructurePlan{}, diagnosis, nil, appError(CodeInvalidRequest, "批量冲突操作必须指定问题类型", nil)
		}
		var rows []models.MediaLibraryStructureIssue
		if err := s.db.WithContext(ctx).Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND conflict_source_count > 1 AND code IN ?", libraryID, diagnosis.JobID, diagnosis.Generation, codes).Order("code,id").Find(&rows).Error; err != nil {
			return StructurePlan{}, diagnosis, nil, err
		}
		for _, row := range rows {
			if _, overridden := selectionByIssue[row.Token]; overridden {
				continue
			}
			action := bulk.Action
			if action == StructureSelectionKeepRecommended && (row.RecommendedMemberToken == "" || structureConflictRequiresReview(row.Code)) {
				action = StructureSelectionSkip
			}
			selectionByIssue[row.Token] = MediaLibraryStructureSelection{IssueToken: row.Token, Action: action, MemberToken: mapRecommendedMember(action, row.RecommendedMemberToken)}
		}
	}
	selectionLimit := maxStructureSelections
	if input.IncludeAutomaticRepairs {
		selectionLimit = maxStructureAutomaticSelections
		automatic := s.db.WithContext(ctx).
			Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND repairable = ?", libraryID, diagnosis.JobID, diagnosis.Generation, true)
		if code := safeLabel(strings.TrimSpace(input.ReviewCode), 64); code != "" && code != "all" {
			automatic = automatic.Where("code = ?", code)
		}
		if input.reviewSessionID != "" {
			automatic = automatic.Where("NOT EXISTS (SELECT 1 FROM media_library_structure_review_choices rc WHERE rc.session_id = ? AND rc.subject_key = ('issue:' || media_library_structure_issues.token) AND rc.state = ?)", input.reviewSessionID, "submitted")
		}
		var rows []models.MediaLibraryStructureIssue
		if err := automatic.Order("code,id").Limit(maxStructureAutomaticSelections + 1).Find(&rows).Error; err != nil {
			return StructurePlan{}, diagnosis, nil, err
		}
		if len(rows) > maxStructureAutomaticSelections {
			return StructurePlan{}, diagnosis, nil, appError(CodeInvalidRequest, "可自动整理项目超过单次安全上限，请按问题类型预览处理", nil)
		}
		for _, row := range rows {
			if _, explicitlyHandled := selectionByIssue[row.Token]; explicitlyHandled {
				continue
			}
			selectionByIssue[row.Token] = MediaLibraryStructureSelection{IssueToken: row.Token, Action: StructureSelectionRepair}
		}
	}
	if len(selectionByIssue) == 0 {
		return StructurePlan{}, diagnosis, nil, appError(CodeInvalidRequest, "请选择要处理的问题", nil)
	}
	if len(selectionByIssue) > selectionLimit {
		message := "本次处理选择过多，请分类型预览处理"
		if input.IncludeAutomaticRepairs {
			message = "可自动整理项目超过单次安全上限，请按问题类型预览处理"
		}
		return StructurePlan{}, diagnosis, nil, appError(CodeInvalidRequest, message, nil)
	}
	tokens := make([]string, 0, len(selectionByIssue))
	for token := range selectionByIssue {
		tokens = append(tokens, token)
	}
	var issues []models.MediaLibraryStructureIssue
	if err := s.db.WithContext(ctx).Where("library_id = ? AND diagnosis_job_id = ? AND generation = ? AND token IN ?", libraryID, diagnosis.JobID, diagnosis.Generation, tokens).Order("code,id").Find(&issues).Error; err != nil {
		return StructurePlan{}, diagnosis, nil, err
	}
	if len(issues) != len(tokens) {
		return StructurePlan{}, diagnosis, nil, appError(CodeConflict, "选择中包含跨库、已过期或不存在的问题", nil)
	}
	issueIDs := make([]uint, 0, len(issues))
	for _, issue := range issues {
		issueIDs = append(issueIDs, issue.ID)
	}
	membersByIssue, err := s.loadStructureIssueMembers(ctx, issueIDs)
	if err != nil {
		return StructurePlan{}, diagnosis, nil, err
	}
	base, _, err := s.buildPlan(ctx, libraryID, "")
	if err != nil {
		return StructurePlan{}, diagnosis, nil, err
	}
	plan := StructurePlan{Version: 1, LibraryID: libraryID, Generation: base.Generation, RuleFingerprint: base.RuleFingerprint, DiagnosisJobID: diagnosis.JobID, DiagnosisGeneration: diagnosis.Generation, SelectionBound: true}
	plan.catalogFence = base.catalogFence
	var autoState models.MediaLibraryStructureAutoState
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).First(&autoState).Error; err != nil {
		return StructurePlan{}, diagnosis, nil, err
	}
	plan.SourceRevision = autoState.SourceRevision
	selectionIndex := newStructureSelectionPlanIndex(base)
	resolved := make([]structureSelectionResolved, 0, len(issues))
	for _, issue := range issues {
		members := membersByIssue[issue.ID]
		selection := selectionByIssue[issue.Token]
		selection.MemberToken = normalizeSelectionMemberToken(selection, issue)
		if err := appendIndexedStructureSelection(&plan, base, selectionIndex, issue, members, selection, draftID); err != nil {
			return StructurePlan{}, diagnosis, nil, err
		}
		resolved = append(resolved, structureSelectionResolved{issue: issue, members: members, selection: selection})
	}
	plan.Items = orderStructureSelectionMoves(plan.Items)
	sort.Strings(plan.ResolvedIssues)
	sort.Strings(plan.SkippedIssues)
	if err := s.validateStructureSelectionSafety(ctx, plan); err != nil {
		return StructurePlan{}, diagnosis, nil, err
	}
	return plan, diagnosis, resolved, nil
}

func structureConflictRequiresReview(code string) bool {
	return code == "catalog_duplicate_conflict" || code == "recognition_suspect_conflict"
}

func structureConflictReviewError(code string) error {
	if code == "recognition_suspect_conflict" {
		return appError(CodeConflict, "作品识别尚有冲突，请先核对并修正识别，再重新检查目录问题", nil)
	}
	return appError(CodeConflict, "同一文件存在重复目录记录，请先扫描核对索引，再重新检查目录问题；不能按重复文件回收或改名", nil)
}

// validateStructureSelectionSafety also runs before executing persisted plans.
// Provider identity, not a catalog row or display path, identifies a real file.
// A canonical winner may need no move and therefore be absent from plan.Items;
// the catalog check below still protects it from a legacy recycle/version plan.
func (s *MediaLibraryStructureService) validateStructureSelectionSafety(ctx context.Context, plan StructurePlan) error {
	return s.withCatalogRead(ctx, plan.LibraryID, func(tx *gorm.DB, reader *CatalogReader) error {
		return s.validateStructureSelectionSafetyTx(tx, reader, plan)
	})
}

func (s *MediaLibraryStructureService) validateStructureSelectionSafetyTx(tx *gorm.DB, reader *CatalogReader, plan StructurePlan) error {
	if len(plan.ResolvedIssues) > 0 {
		var issue models.MediaLibraryStructureIssue
		result := tx.Where("library_id = ? AND diagnosis_job_id = ? AND token IN ? AND code IN ?", plan.LibraryID, plan.DiagnosisJobID, plan.ResolvedIssues, []string{"catalog_duplicate_conflict", "recognition_suspect_conflict"}).Limit(1).Find(&issue)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 0 {
			return structureConflictReviewError(issue.Code)
		}
	}
	items := append([]StructurePlanItem(nil), plan.Items...)
	for _, item := range plan.RecycleItems {
		items = append(items, StructurePlanItem{SourceRelative: item.SourceRelative, ProviderID: item.ProviderID})
	}
	if err := validateDistinctStructureSources(items); err != nil {
		return err
	}
	providerIDs := make(map[string]struct{}, len(items))
	for _, item := range items {
		if id := strings.TrimSpace(item.ProviderID); id != "" {
			providerIDs[id] = struct{}{}
		}
	}
	if len(providerIDs) == 0 {
		return nil
	}
	// Query the library once, including companion facts. Stream only duplicate
	// identities and match them against this plan in memory, avoiding repeated
	// catalog scans or one provider call per file in large selections.
	rows, err := tx.Raw(`SELECT provider_id FROM (
		SELECT provider_id FROM (?) AS entries
		UNION ALL
		SELECT provider_id FROM (?) AS assets
	) AS source_facts GROUP BY provider_id HAVING COUNT(*) > 1`,
		reader.Entries().Select("provider_id").Where("library_id=? AND provider_id<>''", plan.LibraryID),
		reader.SourceAssets().Select("provider_id").Where("library_id=? AND active=? AND provider_id<>''", plan.LibraryID, true)).Rows()
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var providerID string
		if err := rows.Scan(&providerID); err != nil {
			return err
		}
		if _, affected := providerIDs[providerID]; affected {
			return structureConflictReviewError("catalog_duplicate_conflict")
		}
	}
	return rows.Err()
}

func validateDistinctStructureSources(items []StructurePlanItem) error {
	providerIDs := make(map[string]struct{}, len(items))
	paths := make(map[string]struct{}, len(items))
	for _, item := range items {
		if id := strings.TrimSpace(item.ProviderID); id != "" {
			if _, exists := providerIDs[id]; exists {
				return structureConflictReviewError("catalog_duplicate_conflict")
			}
			providerIDs[id] = struct{}{}
		}
		if path := strings.ToLower(safeStructurePath(item.SourceRelative)); path != "" {
			if _, exists := paths[path]; exists {
				return structureConflictReviewError("catalog_duplicate_conflict")
			}
			paths[path] = struct{}{}
		}
	}
	return nil
}

func validStructureSelectionAction(action string) bool {
	switch action {
	case StructureSelectionRepair, StructureSelectionKeepRecommended, StructureSelectionKeepMember, StructureSelectionKeepAllVersions, StructureSelectionSkip:
		return true
	default:
		return false
	}
}

func mapRecommendedMember(action, token string) string {
	if action == StructureSelectionKeepRecommended {
		return token
	}
	return ""
}

func normalizeSelectionMemberToken(selection MediaLibraryStructureSelection, issue models.MediaLibraryStructureIssue) string {
	if selection.Action == StructureSelectionKeepRecommended {
		return issue.RecommendedMemberToken
	}
	if selection.Action == StructureSelectionKeepMember {
		return strings.TrimSpace(selection.MemberToken)
	}
	return ""
}

type structureSelectionPlanIndex struct {
	items          map[string]StructurePlanItem
	conflictGroups map[string]StructureConflictGroup
}

func structureSelectionItemKey(kind, source, target string) string {
	return kind + "\x00" + structureSelectionFoldPath(source) + "\x00" + structureSelectionFoldPath(target)
}

func structureSelectionConflictKey(code, target string) string {
	return code + "\x00" + structureSelectionFoldPath(target)
}

// structureSelectionFoldPath produces a stable key for the same Unicode
// simple-fold equivalence used by strings.EqualFold in the former linear
// lookup. strings.ToLower is not equivalent for every legal title rune (for
// example, long s), which could otherwise make a valid frozen preview look
// stale only after switching to the indexed path.
func structureSelectionFoldPath(value string) string {
	return strings.Map(func(current rune) rune {
		canonical := current
		for folded := unicode.SimpleFold(current); folded != current; folded = unicode.SimpleFold(folded) {
			if folded < canonical {
				canonical = folded
			}
		}
		return canonical
	}, safeStructurePath(value))
}

func newStructureSelectionPlanIndex(base StructurePlan) structureSelectionPlanIndex {
	index := structureSelectionPlanIndex{
		items:          make(map[string]StructurePlanItem, len(base.Items)),
		conflictGroups: make(map[string]StructureConflictGroup, len(base.ConflictGroups)),
	}
	for _, item := range base.Items {
		key := structureSelectionItemKey(item.Kind, item.SourceRelative, item.TargetRelative)
		if _, exists := index.items[key]; !exists {
			// Preserve the former linear lookup's first-match behavior.
			index.items[key] = item
		}
	}
	for _, group := range base.ConflictGroups {
		key := structureSelectionConflictKey(group.Code, group.TargetRelative)
		if _, exists := index.conflictGroups[key]; !exists {
			index.conflictGroups[key] = group
		}
	}
	return index
}

func appendStructureSelection(plan *StructurePlan, base StructurePlan, issue models.MediaLibraryStructureIssue, members []models.MediaLibraryStructureIssueMember, selection MediaLibraryStructureSelection, draftID string) error {
	index := newStructureSelectionPlanIndex(base)
	return appendIndexedStructureSelection(plan, base, index, issue, members, selection, draftID)
}

func appendIndexedStructureSelection(plan *StructurePlan, base StructurePlan, index structureSelectionPlanIndex, issue models.MediaLibraryStructureIssue, members []models.MediaLibraryStructureIssueMember, selection MediaLibraryStructureSelection, draftID string) error {
	if selection.Action == StructureSelectionSkip {
		plan.SkippedIssues = append(plan.SkippedIssues, issue.Token)
		return nil
	}
	if structureConflictRequiresReview(issue.Code) {
		return structureConflictReviewError(issue.Code)
	}
	if issue.ConflictSourceCount > 1 {
		if selection.Action == StructureSelectionRepair {
			return appError(CodeInvalidRequest, "冲突问题必须选择保留来源、全部保留为版本或跳过", nil)
		}
		group, exists := index.conflictGroups[structureSelectionConflictKey(issue.Code, issue.ExpectedPath)]
		if !exists || len(group.Members) != len(members) {
			return appError(CodeConflict, "冲突成员已经变化，请重新诊断", nil)
		}
		if err := validateDistinctStructureSources(group.Members); err != nil {
			return err
		}
		byToken := make(map[string]models.MediaLibraryStructureIssueMember, len(members))
		bySource := make(map[string]StructurePlanItem, len(group.Members))
		for _, member := range members {
			byToken[member.Token] = member
		}
		for _, member := range group.Members {
			bySource[safeStructurePath(member.SourceRelative)] = member
		}
		if selection.Action == StructureSelectionKeepAllVersions {
			primaryToken := issue.RecommendedMemberToken
			if primaryToken == "" && len(members) > 0 {
				primaryToken = members[0].Token
			}
			return appendAllConflictVersions(plan, base, issue, members, bySource, primaryToken)
		}
		if selection.Action != StructureSelectionKeepRecommended && selection.Action != StructureSelectionKeepMember {
			return appError(CodeInvalidRequest, "冲突处理方式无效", nil)
		}
		chosen, exists := byToken[selection.MemberToken]
		if !exists || (selection.Action == StructureSelectionKeepRecommended && selection.MemberToken == "") {
			return appError(CodeInvalidRequest, "请选择有效的冲突保留来源", nil)
		}
		winner, exists := bySource[safeStructurePath(chosen.SourcePath)]
		if !exists {
			return appError(CodeConflict, "冲突来源已经变化，请重新诊断", nil)
		}
		for _, member := range members {
			candidate, exists := bySource[safeStructurePath(member.SourcePath)]
			if !exists {
				return appError(CodeConflict, "冲突来源已经变化，请重新诊断", nil)
			}
			if member.Token == chosen.Token {
				continue
			}
			plan.RecycleItems = append(plan.RecycleItems, StructureRecycleItem{Kind: candidate.Kind, SourceRelative: candidate.SourceRelative, RecycleRelative: selectionRecycleRelative(draftID, candidate.SourceRelative), ProviderID: candidate.ProviderID, Size: candidate.Size, ModifiedAtUnixNano: candidate.ModifiedAtUnixNano})
		}
		if !strings.EqualFold(winner.SourceRelative, winner.TargetRelative) {
			plan.Items = append(plan.Items, winner)
		}
		plan.ResolvedIssues = append(plan.ResolvedIssues, issue.Token)
		return nil
	}
	if selection.Action != StructureSelectionRepair {
		return appError(CodeInvalidRequest, "普通问题只能选择修复或跳过", nil)
	}
	if item, exists := index.items[structureSelectionItemKey(issue.Kind, issue.CurrentPath, issue.ExpectedPath)]; exists {
		plan.Items = append(plan.Items, item)
		plan.ResolvedIssues = append(plan.ResolvedIssues, issue.Token)
		return nil
	}
	if issue.State == "manual_identity_resolved" {
		plan.ResolvedIssues = append(plan.ResolvedIssues, issue.Token)
		return nil
	}
	return appError(CodeConflict, "问题对应的修复计划已经变化，请重新诊断", nil)
}

func appendAllConflictVersions(plan *StructurePlan, base StructurePlan, issue models.MediaLibraryStructureIssue, members []models.MediaLibraryStructureIssueMember, bySource map[string]StructurePlanItem, primaryToken string) error {
	occupied := make(map[string]struct{}, len(base.OccupiedPaths)+len(base.Items)+len(base.ConflictGroups)*2)
	for path := range base.OccupiedPaths {
		occupied[path] = struct{}{}
	}
	for _, item := range base.Items {
		occupied[strings.ToLower(safeStructurePath(item.SourceRelative))] = struct{}{}
	}
	for _, group := range base.ConflictGroups {
		for _, member := range group.Members {
			occupied[strings.ToLower(safeStructurePath(member.SourceRelative))] = struct{}{}
		}
	}
	for _, member := range members {
		delete(occupied, strings.ToLower(safeStructurePath(member.SourcePath)))
	}
	target := safeStructurePath(issue.ExpectedPath)
	if target == "" {
		return appError(CodeConflict, "冲突目标已经变化，请重新诊断", nil)
	}
	nextVersion := 2
	for _, member := range members {
		item, exists := bySource[safeStructurePath(member.SourcePath)]
		if !exists {
			return appError(CodeConflict, "冲突来源已经变化，请重新诊断", nil)
		}
		destination := target
		if member.Token != primaryToken {
			for {
				destination = structureVersionTarget(target, nextVersion)
				nextVersion++
				if _, exists := occupied[strings.ToLower(destination)]; !exists {
					break
				}
			}
		}
		occupied[strings.ToLower(destination)] = struct{}{}
		item.TargetRelative = destination
		if !strings.EqualFold(item.SourceRelative, item.TargetRelative) {
			plan.Items = append(plan.Items, item)
		}
	}
	plan.ResolvedIssues = append(plan.ResolvedIssues, issue.Token)
	return nil
}

func structureVersionTarget(target string, version int) string {
	extension := pathpkg.Ext(target)
	base := strings.TrimSuffix(pathpkg.Base(target), extension)
	return pathpkg.Join(pathpkg.Dir(target), fmt.Sprintf("%s (%d)%s", base, version, extension))
}

func selectionRecycleRelative(draftID, source string) string {
	return pathpkg.Join(".ohmycine-recycle", draftID, safeStructurePath(source))
}

func orderStructureSelectionMoves(items []StructurePlanItem) []StructurePlanItem {
	if len(items) < 2 {
		return append([]StructurePlanItem(nil), items...)
	}
	sourceCounts := make(map[string]int, len(items))
	waitingBySource := make(map[string][]int, len(items))
	for index, item := range items {
		source := strings.ToLower(safeStructurePath(item.SourceRelative))
		target := strings.ToLower(safeStructurePath(item.TargetRelative))
		sourceCounts[source]++
		waitingBySource[target] = append(waitingBySource[target], index)
	}
	blocked := make([]bool, len(items))
	available := make(structureMoveIndexHeap, 0, len(items))
	for index, item := range items {
		target := strings.ToLower(safeStructurePath(item.TargetRelative))
		if sourceCounts[target] > 0 {
			blocked[index] = true
		} else {
			heap.Push(&available, index)
		}
	}
	removed := make([]bool, len(items))
	ordered := make([]StructurePlanItem, 0, len(items))
	for available.Len() > 0 {
		index := heap.Pop(&available).(int)
		if removed[index] || blocked[index] {
			continue
		}
		removed[index] = true
		ordered = append(ordered, items[index])
		source := strings.ToLower(safeStructurePath(items[index].SourceRelative))
		sourceCounts[source]--
		if sourceCounts[source] != 0 {
			continue
		}
		delete(sourceCounts, source)
		for _, waiting := range waitingBySource[source] {
			if !removed[waiting] && blocked[waiting] {
				blocked[waiting] = false
				heap.Push(&available, waiting)
			}
		}
	}
	if len(ordered) == len(items) {
		return ordered
	}
	// Cycles are not expected from version expansion. Preserve the former
	// deterministic fallback and let the backend fail closed instead of
	// inventing an unreviewed temporary path.
	remaining := make([]StructurePlanItem, 0, len(items)-len(ordered))
	for index, item := range items {
		if !removed[index] {
			remaining = append(remaining, item)
		}
	}
	sort.Slice(remaining, func(i, j int) bool { return remaining[i].SourceRelative < remaining[j].SourceRelative })
	return append(ordered, remaining...)
}

type structureMoveIndexHeap []int

func (h structureMoveIndexHeap) Len() int           { return len(h) }
func (h structureMoveIndexHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h structureMoveIndexHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *structureMoveIndexHeap) Push(value any)    { *h = append(*h, value.(int)) }
func (h *structureMoveIndexHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}
