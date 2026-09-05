package services

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	pathpkg "path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

const (
	StructureSelectionRepair             = "repair"
	StructureSelectionKeepRecommended    = "keep_recommended"
	StructureSelectionKeepMember         = "keep_member"
	StructureSelectionKeepAllVersions    = "keep_all_versions"
	StructureSelectionSkip               = "skip"
	maxStructureSelections               = 5000
	structureSelectionConfirmationExpiry = 5 * time.Minute
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
	Revision    string                            `json:"revision"`
	Selections  []MediaLibraryStructureSelection  `json:"selections"`
	BulkActions []MediaLibraryStructureBulkAction `json:"bulk_actions,omitempty"`
}

type MediaLibraryStructureSelectionPreview struct {
	LibraryID         uint                             `json:"library_id"`
	Revision          string                           `json:"revision"`
	IssueCount        int                              `json:"issue_count"`
	RecycleCount      int                              `json:"recycle_count"`
	MoveCount         int                              `json:"move_count"`
	SkippedCount      int                              `json:"skipped_count"`
	Selections        []MediaLibraryStructureSelection `json:"selections"`
	ConfirmationToken string                           `json:"confirmation_token"`
	ExpiresAt         time.Time                        `json:"expires_at"`
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
	if err != nil || len(selectionJSON) > 2*1024*1024 {
		return MediaLibraryStructureSelectionPreview{}, appError(CodeInvalidRequest, "目录修复选择过多", err)
	}
	var autoState models.MediaLibraryStructureAutoState
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).First(&autoState).Error; err != nil {
		return MediaLibraryStructureSelectionPreview{}, err
	}
	expires := time.Now().UTC().Add(structureSelectionConfirmationExpiry)
	draft := models.MediaLibraryStructureRepairDraft{ID: draftID, OwnerID: actor.User.ID, LibraryID: libraryID, DiagnosisJobID: diagnosis.JobID, SourceRevision: autoState.SourceRevision, Generation: plan.Generation, RuleFingerprint: plan.RuleFingerprint, PlanHash: planHash, SelectionsJSON: string(selectionJSON), ExpiresAt: expires, CreatedAt: time.Now().UTC()}
	if err := s.db.WithContext(ctx).Create(&draft).Error; err != nil {
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
	return MediaLibraryStructureSelectionPreview{LibraryID: libraryID, Revision: input.Revision, IssueCount: len(selections), RecycleCount: len(plan.RecycleItems), MoveCount: len(plan.Items), SkippedCount: skipped, Selections: selections, ConfirmationToken: token, ExpiresAt: expires}, nil
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
	if err != nil || len(raw) > 8*1024*1024 {
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
		consumed := tx.Model(&models.MediaLibraryStructureRepairDraft{}).Where("id = ? AND owner_id = ? AND library_id = ? AND consumed_at IS NULL AND expires_at > ?", draft.ID, actor.User.ID, draft.LibraryID, now).Update("consumed_at", now)
		if consumed.Error != nil {
			return consumed.Error
		}
		if consumed.RowsAffected != 1 {
			return appError(CodeConflict, "目录修复确认已被使用或已经过期", nil)
		}
		repair.JobID = &job.ID
		if err := tx.Create(&repair).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.MediaLibrary{}).Where("id = ?", draft.LibraryID).Updates(map[string]any{"structure_status": models.MediaLibraryStructureRepairing, "structure_error_code": ""}).Error; err != nil {
			return err
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
	if len(selectionByIssue) == 0 || len(selectionByIssue) > maxStructureSelections {
		return StructurePlan{}, diagnosis, nil, appError(CodeInvalidRequest, "请选择要处理的问题", nil)
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
	var autoState models.MediaLibraryStructureAutoState
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).First(&autoState).Error; err != nil {
		return StructurePlan{}, diagnosis, nil, err
	}
	plan.SourceRevision = autoState.SourceRevision
	resolved := make([]structureSelectionResolved, 0, len(issues))
	for _, issue := range issues {
		members := membersByIssue[issue.ID]
		selection := selectionByIssue[issue.Token]
		selection.MemberToken = normalizeSelectionMemberToken(selection, issue)
		if err := appendStructureSelection(&plan, base, issue, members, selection, draftID); err != nil {
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
	if len(plan.ResolvedIssues) > 0 {
		var issue models.MediaLibraryStructureIssue
		result := s.db.WithContext(ctx).Where("library_id = ? AND diagnosis_job_id = ? AND token IN ? AND code IN ?", plan.LibraryID, plan.DiagnosisJobID, plan.ResolvedIssues, []string{"catalog_duplicate_conflict", "recognition_suspect_conflict"}).Limit(1).Find(&issue)
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
	rows, err := s.db.WithContext(ctx).Raw(`SELECT provider_id FROM (
		SELECT provider_id FROM media_library_entries WHERE library_id = ? AND provider_id <> ''
		UNION ALL
		SELECT provider_id FROM media_library_source_assets WHERE library_id = ? AND active = ? AND provider_id <> ''
	) AS source_facts GROUP BY provider_id HAVING COUNT(*) > 1`, plan.LibraryID, plan.LibraryID, true).Rows()
	if err != nil {
		return err
	}
	defer rows.Close()
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

func appendStructureSelection(plan *StructurePlan, base StructurePlan, issue models.MediaLibraryStructureIssue, members []models.MediaLibraryStructureIssueMember, selection MediaLibraryStructureSelection, draftID string) error {
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
		group := findStructureConflictGroup(base.ConflictGroups, issue.Code, issue.ExpectedPath)
		if group == nil || len(group.Members) != len(members) {
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
	for _, item := range base.Items {
		if item.Kind == issue.Kind && strings.EqualFold(safeStructurePath(item.SourceRelative), safeStructurePath(issue.CurrentPath)) && strings.EqualFold(safeStructurePath(item.TargetRelative), safeStructurePath(issue.ExpectedPath)) {
			plan.Items = append(plan.Items, item)
			plan.ResolvedIssues = append(plan.ResolvedIssues, issue.Token)
			return nil
		}
	}
	if issue.State == "manual_identity_resolved" {
		plan.ResolvedIssues = append(plan.ResolvedIssues, issue.Token)
		return nil
	}
	return appError(CodeConflict, "问题对应的修复计划已经变化，请重新诊断", nil)
}

func findStructureConflictGroup(groups []StructureConflictGroup, code, target string) *StructureConflictGroup {
	target = safeStructurePath(target)
	for index := range groups {
		if groups[index].Code == code && strings.EqualFold(safeStructurePath(groups[index].TargetRelative), target) {
			return &groups[index]
		}
	}
	return nil
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
	remaining := append([]StructurePlanItem(nil), items...)
	ordered := make([]StructurePlanItem, 0, len(items))
	for len(remaining) > 0 {
		sourceSet := make(map[string]struct{}, len(remaining))
		for _, item := range remaining {
			sourceSet[strings.ToLower(item.SourceRelative)] = struct{}{}
		}
		picked := -1
		for index, item := range remaining {
			if _, blocked := sourceSet[strings.ToLower(item.TargetRelative)]; !blocked {
				picked = index
				break
			}
		}
		if picked < 0 {
			// Cycles are not expected from version expansion. Preserve a stable
			// order and let the backend fail closed instead of inventing a temp path.
			sort.Slice(remaining, func(i, j int) bool { return remaining[i].SourceRelative < remaining[j].SourceRelative })
			return append(ordered, remaining...)
		}
		ordered = append(ordered, remaining[picked])
		remaining = append(remaining[:picked], remaining[picked+1:]...)
	}
	return ordered
}
