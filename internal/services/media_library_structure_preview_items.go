package services

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

const maxStructurePreviewBytes = 32 * 1024 * 1024

type MediaLibraryStructurePreviewItem struct {
	Action       string `json:"action"`
	Kind         string `json:"kind"`
	CurrentPath  string `json:"current_path"`
	ExpectedPath string `json:"expected_path"`
}

type MediaLibraryStructurePreviewPage struct {
	List     []MediaLibraryStructurePreviewItem `json:"list"`
	Total    int                                `json:"total"`
	Page     int                                `json:"page"`
	PageSize int                                `json:"page_size"`
}

func structurePreviewItems(plan StructurePlan, resolved []structureSelectionResolved) ([]MediaLibraryStructurePreviewItem, error) {
	items := make([]MediaLibraryStructurePreviewItem, 0, len(plan.RecycleItems)+len(plan.Items))
	for _, item := range plan.RecycleItems {
		current := safeStructurePath(item.SourceRelative)
		if current == "" {
			return nil, appError(CodeConflict, "预览来源不可显示，请重新检查", nil)
		}
		items = append(items, MediaLibraryStructurePreviewItem{Action: "recycle", Kind: item.Kind, CurrentPath: current, ExpectedPath: "可恢复回收站"})
	}
	for _, item := range plan.Items {
		current, target := safeStructurePath(item.SourceRelative), safeStructurePath(item.TargetRelative)
		if current == "" || target == "" {
			return nil, appError(CodeConflict, "预览路径不可显示，请重新检查", nil)
		}
		items = append(items, MediaLibraryStructurePreviewItem{Action: "move", Kind: item.Kind, CurrentPath: current, ExpectedPath: target})
	}
	// A canonical winner may need no move. Still show it, so keeping all files
	// does not hide the unchanged version from the user's complete preview.
	covered := make(map[string]bool, len(items))
	for _, item := range items {
		covered[item.CurrentPath] = true
	}
	for _, selection := range resolved {
		if selection.selection.Action == StructureSelectionSkip {
			continue
		}
		for _, member := range selection.members {
			current := safeStructurePath(member.SourcePath)
			if current != "" && !covered[current] {
				items = append(items, MediaLibraryStructurePreviewItem{Action: "keep", Kind: selection.issue.Kind, CurrentPath: current, ExpectedPath: current})
				covered[current] = true
			}
		}
	}
	return items, nil
}

func structurePreviewPage(items []MediaLibraryStructurePreviewItem, page, pageSize int) MediaLibraryStructurePreviewPage {
	start := min((page-1)*pageSize, len(items))
	end := min(start+pageSize, len(items))
	return MediaLibraryStructurePreviewPage{List: items[start:end], Total: len(items), Page: page, PageSize: pageSize}
}

// SelectionPreviewItems reads a frozen display projection. It neither rebuilds
// a plan nor consumes confirmation authority, and performs no provider work.
func (s *MediaLibraryStructureService) SelectionPreviewItems(ctx context.Context, actor Actor, libraryID uint, token string, page, pageSize int) (MediaLibraryStructurePreviewPage, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesScan, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaLibraryStructurePreviewPage{}, appError(CodePermissionDenied, "无权查看目录修复预览", nil)
	}
	if page < 1 || page > 100000 || pageSize < 1 || pageSize > 200 {
		return MediaLibraryStructurePreviewPage{}, appError(CodeInvalidRequest, "预览分页参数无效", nil)
	}
	claim, err := s.verifyStructureClaim(token)
	if err != nil || claim.DraftID == "" || claim.ActorID != actor.User.ID || claim.LibraryID != libraryID || claim.ExpiresAt < time.Now().UTC().Unix() {
		return MediaLibraryStructurePreviewPage{}, appError(CodeInvalidRequest, "目录修复确认已失效，请重新预览", err)
	}
	var draft models.MediaLibraryStructureRepairDraft
	if err := s.db.WithContext(ctx).Where("id = ? AND owner_id = ? AND library_id = ?", claim.DraftID, actor.User.ID, libraryID).First(&draft).Error; err != nil {
		return MediaLibraryStructurePreviewPage{}, appError(CodeInvalidRequest, "目录修复预览不存在或已经失效", err)
	}
	if draft.ConsumedAt != nil || draft.ExpiresAt.Before(time.Now().UTC()) || claim.Generation != draft.Generation || claim.RuleFingerprint != draft.RuleFingerprint || !hmac.Equal([]byte(claim.PlanHash), []byte(draft.PlanHash)) {
		return MediaLibraryStructurePreviewPage{}, appError(CodeConflict, "目录修复预览已失效，请重新预览", nil)
	}
	var library models.MediaLibrary
	var source models.MediaLibraryStructureAutoState
	var diagnosis models.MediaLibraryStructureDiagnosis
	if err := s.db.WithContext(ctx).First(&library, libraryID).Error; err != nil {
		return MediaLibraryStructurePreviewPage{}, mediaLibraryNotFound(err)
	}
	if err := s.db.WithContext(ctx).First(&source, "library_id = ?", libraryID).Error; err != nil {
		return MediaLibraryStructurePreviewPage{}, err
	}
	if err := s.db.WithContext(ctx).First(&diagnosis, "library_id = ?", libraryID).Error; err != nil {
		return MediaLibraryStructurePreviewPage{}, err
	}
	if library.BaselineGeneration != draft.Generation || source.SourceRevision != draft.SourceRevision || diagnosis.JobID != draft.DiagnosisJobID || libraryRuleFingerprint(library) != draft.RuleFingerprint {
		return MediaLibraryStructurePreviewPage{}, appError(CodeConflict, "媒体库来源、诊断或规则已变化，请重新预览", nil)
	}
	if draft.PreviewItemsJSON == "" || len(draft.PreviewItemsJSON) > maxStructurePreviewBytes {
		return MediaLibraryStructurePreviewPage{}, appError(CodeConflict, "旧预览没有文件明细，请重新预览", nil)
	}
	var items []MediaLibraryStructurePreviewItem
	if err := json.Unmarshal([]byte(draft.PreviewItemsJSON), &items); err != nil {
		return MediaLibraryStructurePreviewPage{}, appError(CodeConflict, "目录修复预览不可读取，请重新预览", err)
	}
	return structurePreviewPage(items, page, pageSize), nil
}
