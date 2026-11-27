package services

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type MediaCatalogDeletionProgress struct {
	DeletionID     string `json:"deletion_id,omitempty"`
	Phase          string `json:"phase"`
	Title          string `json:"title"`
	ProcessedFiles int    `json:"processed_files"`
	TotalFiles     int    `json:"total_files"`
	RemovedFiles   int    `json:"removed_files"`
	MissingFiles   int    `json:"missing_files"`
}

// This reads the original checkpoint only. It never renews a confirmation or
// resumes a write when a page is opened again after a lost response.
func (s *MediaLibraryService) CatalogDeletionStatus(ctx context.Context, actor Actor, libraryID uint, workToken, deletionID string) (MediaCatalogDeletionProgress, error) {
	if !actor.CanResource(authz.PermissionMediaLibrariesMediaDelete, models.AuthorizationResourceMediaLibrary, uintID(libraryID)) {
		return MediaCatalogDeletionProgress{}, appError(CodePermissionDenied, "无权查看这次源文件删除", nil)
	}
	workKey, err := decodeCatalogToken(workToken)
	if err != nil {
		return MediaCatalogDeletionProgress{}, err
	}
	var preview models.MediaCatalogDeletionPreview
	if err := s.db.WithContext(ctx).Where("id=? AND actor_id=? AND library_id=? AND work_key=?", deletionID, actor.User.ID, libraryID, workKey).First(&preview).Error; err != nil {
		return MediaCatalogDeletionProgress{}, appError(CodeNotFound, "未找到这次删除记录", err)
	}
	var snapshot mediaCatalogDeletionSnapshot
	var state mediaCatalogDeletionState
	if json.Unmarshal([]byte(preview.SnapshotJSON), &snapshot) != nil || json.Unmarshal([]byte(preview.StateJSON), &state) != nil {
		return MediaCatalogDeletionProgress{}, appError(CodeMediaCatalogDeletionUnavailable, "暂时无法读取删除进度", nil)
	}
	result := MediaCatalogDeletionProgress{DeletionID: preview.ID, Phase: "ready", Title: snapshot.Title, ProcessedFiles: len(state.Completed), TotalFiles: len(snapshot.Items), RemovedFiles: state.Removed, MissingFiles: state.Missing}
	switch {
	case preview.ConsumedAt != nil:
		result.Phase = "completed"
	case preview.StartedAt != nil:
		var owner models.CatalogPhysicalWrite
		if err := s.db.WithContext(ctx).Where("owner_kind=? AND owner_id=?", CatalogPhysicalDeletion, preview.ID).First(&owner).Error; err != nil {
			return MediaCatalogDeletionProgress{}, err
		}
		result.Phase = "interrupted"
		if owner.State == "entered" {
			result.Phase = "deleting"
		}
	case preview.LastErrorCode != "":
		result.Phase = "interrupted"
	}
	return result, nil
}

// Preparation reads each ancestor once. Confirmation leaves directories nil
// to read both the current file identity and every ancestor before recycling.
type catalogDeletionReads struct {
	cloudpkg.Driver
	directories map[string]cloudpkg.Item
}

func newCatalogDeletionReads(driver cloudpkg.Driver) *catalogDeletionReads {
	return &catalogDeletionReads{Driver: driver, directories: make(map[string]cloudpkg.Item)}
}

func (r *catalogDeletionReads) Stat(ctx context.Context, id string) (cloudpkg.Item, error) {
	if item, ok := r.directories[id]; ok {
		return item, nil
	}
	callCtx, cancel := boundedTransferDeletionContext(ctx)
	defer cancel()
	item, err := r.Driver.Stat(callCtx, id)
	if err != nil {
		if code, _ := cloudpkg.ErrorInfo(err); code == cloudpkg.CodeNotFound {
			return item, err
		}
		return item, catalogDeletionProviderError(err)
	}
	if r.directories != nil && item.IsDir && item.ID == id {
		r.directories[id] = item
	}
	return item, nil
}

func recycleCatalogItem(ctx context.Context, driver cloudpkg.MutationDriver, id string) error {
	callCtx, cancel := boundedTransferDeletionContext(ctx)
	defer cancel()
	if err := driver.Recycle(callCtx, id); err != nil {
		return catalogDeletionProviderError(err)
	}
	return nil
}

func catalogDeletionProviderError(err error) error {
	code, _ := cloudpkg.ErrorInfo(err)
	message := connectionTestMessage(cloudpkg.ProviderPan115, code)
	if errors.Is(err, context.DeadlineExceeded) {
		message = "读取或删除 115 文件超时，请重试"
	} else if errors.Is(err, context.Canceled) {
		message = "本次删除连接已中断，已保存的进度可继续查看"
	}
	return appError(CodeMediaCatalogDeletionUnavailable, message, err)
}
