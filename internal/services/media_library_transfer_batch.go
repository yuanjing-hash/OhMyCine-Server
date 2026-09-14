package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

type transferBatchContextKey struct{}

type transferCatalogBatch struct {
	TaskID   string
	Result   medialibrary.Result
	Identity MediaIdentitySnapshot
}

func transferBatchIDFromContext(ctx context.Context) string {
	batch, _ := ctx.Value(transferBatchContextKey{}).(transferCatalogBatch)
	return batch.TaskID
}

func transferBatchResultFromContext(ctx context.Context) (medialibrary.Result, bool) {
	batch, ok := ctx.Value(transferBatchContextKey{}).(transferCatalogBatch)
	return batch.Result, ok
}

// ReconcileTransferBatch publishes only durable successful transfer targets.
// Neither missing evidence nor a failed Stat can broaden the batch into a scan.
// The transfer job itself supplies durable retry scheduling until publication.
func (s *MediaLibraryService) ReconcileTransferBatch(ctx context.Context, taskID string) error {
	var task models.TransferTask
	if err := s.db.WithContext(ctx).First(&task, "id = ?", taskID).Error; err != nil {
		return err
	}
	if task.CatalogPublishedAt != nil {
		return nil
	}
	if task.Phase != models.TransferTaskStatusCompleted {
		return errors.New("transfer batch is not completed")
	}
	var published models.MediaLibraryScanRun
	replayErr := s.db.WithContext(ctx).Where("library_id = ? AND kind = ? AND status = ? AND CASE WHEN json_valid(checkpoint_json) THEN json_extract(checkpoint_json, '$.transfer_batch_id') ELSE '' END = ?", task.LibraryID, "transfer_batch", "success", task.ID).Order("id DESC").First(&published).Error
	if replayErr == nil {
		if s.artifacts != nil {
			if err := s.artifacts.ScheduleGeneration(task.LibraryID, published.Generation); err != nil {
				return err
			}
		}
		return s.db.WithContext(ctx).Model(&models.TransferTask{}).Where("id = ? AND catalog_published_at IS NULL", task.ID).Update("catalog_published_at", time.Now().UTC()).Error
	}
	if !errors.Is(replayErr, gorm.ErrRecordNotFound) {
		return replayErr
	}
	var download models.DownloadTask
	if err := s.db.WithContext(ctx).First(&download, "id = ?", task.DownloadTaskID).Error; err != nil {
		return err
	}
	if err := validateTransferIdentitySnapshot(download); err != nil {
		return err
	}
	identity, err := decodeMediaIdentity(download.IdentitySnapshotJSON)
	if err != nil {
		return err
	}
	var library models.MediaLibrary
	if err = s.db.WithContext(ctx).First(&library, task.LibraryID).Error; err != nil {
		return err
	}
	if download.TargetStorageID == nil || *download.TargetStorageID != library.StorageID || download.TargetLibraryID == nil || *download.TargetLibraryID != library.ID {
		return errors.New("transfer batch destination changed")
	}
	var storage models.Storage
	if err = s.db.WithContext(ctx).First(&storage, library.StorageID).Error; err != nil {
		return err
	}
	var items []models.MediaManagedItem
	if err = s.db.WithContext(ctx).Where("transfer_task_id = ? AND library_id = ? AND active = ? AND managed = ?", task.ID, library.ID, true, true).Order("id").Find(&items).Error; err != nil {
		return err
	}
	expected, proofErr := transferBatchExpectedFiles(task, storage.Type)
	if proofErr != nil || expected != len(items) {
		return errors.New("transfer batch successful target evidence is incomplete")
	}
	batch, err := s.inspectTransferCatalogBatch(ctx, library, storage, download, identity, items)
	if err != nil {
		return err
	}
	batch.TaskID = task.ID
	if len(batch.Result.Files) > 0 || len(batch.Result.Assets) > 0 {
		if _, err = s.reconcile(context.WithValue(ctx, transferBatchContextKey{}, batch), library.ID, "transfer_batch"); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	return s.db.WithContext(ctx).Model(&models.TransferTask{}).Where("id = ? AND catalog_published_at IS NULL", task.ID).Update("catalog_published_at", now).Error
}

func transferBatchExpectedFiles(task models.TransferTask, storageType string) (int, error) {
	var summary TransferPlanSummary
	if storageType == models.StorageTypeLocal {
		if json.Unmarshal([]byte(task.PlanSummaryJSON), &summary) != nil || summary.CompletedFiles < 0 || summary.SkippedFiles < 0 || summary.CompletedFiles+summary.SkippedFiles != task.TotalFiles {
			return 0, errors.New("transfer batch completion counts missing")
		}
		return summary.CompletedFiles, nil
	}
	var state cloudTransferState
	if json.Unmarshal([]byte(task.CloudStateJSON), &state) != nil || len(state.Items) != task.TotalFiles {
		return 0, errors.New("transfer batch completion state missing")
	}
	completed := 0
	for _, item := range state.Items {
		switch item.Status {
		case "completed":
			completed++
		case "skipped":
		default:
			return 0, errors.New("transfer batch completion state invalid")
		}
	}
	return completed, nil
}

func (s *MediaLibraryService) inspectTransferCatalogBatch(ctx context.Context, library models.MediaLibrary, storage models.Storage, download models.DownloadTask, identity MediaIdentitySnapshot, items []models.MediaManagedItem) (transferCatalogBatch, error) {
	// Scoped=true in the provider-event protocol merges the entire baseline.
	// A transfer instead supplies exact upserts with Partial=true: unseen rows
	// are preserved and never become recognition/production work for this batch.
	batch := transferCatalogBatch{Identity: identity, Result: medialibrary.Result{Partial: true}}
	var proof *providerBoundaryProof
	localRoot := ""
	if storage.Type == models.StorageTypeLocal {
		var err error
		localRoot, err = medialibrary.ResolveRoot(storage.RootPath, library.RelativeRoot)
		if err != nil {
			return batch, err
		}
	} else {
		if s.connections == nil || storage.ConnectionID == nil || library.ProviderRootID == "" || library.ProviderRootID != download.TargetProviderRootID {
			return batch, errors.New("transfer batch provider boundary unavailable")
		}
		_, driver, err := s.connections.driver(*storage.ConnectionID)
		if err != nil {
			return batch, err
		}
		proof = newProviderBoundaryProof(driver)
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return batch, err
		}
		relative, err := sanitizeTransferRelativePath(item.RelativePath)
		if err != nil || item.IdentityRevision != identity.Revision {
			return batch, errors.New("transfer batch target evidence changed")
		}
		var file medialibrary.File
		var hashHint string
		if localRoot != "" {
			var present bool
			file, present, err = medialibrary.InspectLocalFile(ctx, localRoot, filepath.Join(localRoot, filepath.FromSlash(relative)), []string{strings.ToLower(path.Ext(relative))}, nil)
			if err != nil && download.TransferMode == models.MediaLibraryTransferSymlink {
				file, err = inspectTransferBatchSymlink(localRoot, relative, download.StagingAbsolutePath)
				present = err == nil
			}
			if err != nil {
				return batch, err
			}
			if !present {
				return batch, errors.New("transfer batch local target missing")
			}
			hashHint = file.ProviderID
		} else {
			if item.ProviderItemID == "" {
				return batch, errors.New("transfer batch provider identity missing")
			}
			current, statErr := proof.within(ctx, item.ProviderItemID, library.ProviderRootID)
			if statErr != nil {
				return batch, statErr
			}
			if current.IsDir || current.ParentID != item.ProviderParentID || current.Name != path.Base(relative) {
				return batch, errors.New("transfer batch provider target changed")
			}
			actualPath, ancestor := current.Name, current
			for ancestor.ParentID != library.ProviderRootID {
				parent, exists := proof.items[ancestor.ParentID]
				if !exists || !safeProviderPathSegment(parent.Name) {
					return batch, errors.New("transfer batch ancestor evidence missing")
				}
				actualPath = parent.Name + "/" + actualPath
				ancestor = parent
			}
			if actualPath != relative {
				return batch, errors.New("transfer batch destination path changed")
			}
			file = medialibrary.File{RelativePath: "/" + relative, ProviderID: current.ID, ProviderIDStable: true, Size: current.Size, ModifiedAt: current.ModifiedAt.UTC()}
			hashHint = current.SHA1
		}
		if file.Size != item.Size {
			return batch, errors.New("transfer batch target size changed")
		}
		if item.Kind == models.MediaManagedItemKindVideo {
			batch.Result.Files = append(batch.Result.Files, file)
		} else {
			batch.Result.Assets = append(batch.Result.Assets, medialibrary.SourceAsset{RelativePath: file.RelativePath, ProviderID: file.ProviderID, ParentProviderID: item.ProviderParentID, Name: path.Base(relative), Extension: strings.ToLower(path.Ext(relative)), Size: file.Size, ModifiedAt: file.ModifiedAt, HashHint: hashHint})
		}
		batch.Result.Enumerated++
	}
	return batch, nil
}

// Only the final file may be the symlink deliberately created by this transfer;
// its target must remain beneath the immutable download staging root.
func inspectTransferBatchSymlink(root, relative, stagingRoot string) (medialibrary.File, error) {
	target := filepath.Join(root, filepath.FromSlash(relative))
	if err := ensureWithin(root, target); err != nil {
		return medialibrary.File{}, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil || !strings.EqualFold(filepath.Clean(parent), filepath.Clean(filepath.Dir(target))) {
		return medialibrary.File{}, errors.New("transfer batch symlink ancestor changed")
	}
	link, err := os.Lstat(target)
	if err != nil || link.Mode()&os.ModeSymlink == 0 {
		return medialibrary.File{}, errors.New("transfer batch symlink target changed")
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return medialibrary.File{}, err
	}
	if !filepath.IsAbs(stagingRoot) {
		return medialibrary.File{}, errors.New("transfer batch staging root unavailable")
	}
	if err := ensureWithin(stagingRoot, resolved); err != nil {
		return medialibrary.File{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return medialibrary.File{}, err
	}
	if !info.Mode().IsRegular() {
		return medialibrary.File{}, errors.New("transfer batch symlink is not a regular media file")
	}
	providerRel := "/" + filepath.ToSlash(relative)
	identity := sha256.Sum256([]byte(providerRel + "\x00" + info.ModTime().UTC().Format(time.RFC3339Nano) + "\x00" + strconv.FormatInt(info.Size(), 10)))
	return medialibrary.File{RelativePath: providerRel, ProviderID: hex.EncodeToString(identity[:16]), Size: info.Size(), ModifiedAt: info.ModTime().UTC()}, nil
}

func transferBatchRecognition(ctx context.Context, unit medialibrary.RecognitionUnit) (mediaLibraryRecognizedUnit, bool) {
	batch, ok := ctx.Value(transferBatchContextKey{}).(transferCatalogBatch)
	if !ok {
		return mediaLibraryRecognizedUnit{}, false
	}
	i := batch.Identity
	result := MediaRecognitionResult{Status: mediaRecognitionStatusMatched, Title: i.Title, MediaType: i.MediaType, CategoryName: i.Category, TMDBID: cloneInt64(i.TMDBID), ReleaseYear: cloneInt(i.Year), Confidence: cloneFloat64(i.Confidence), SeasonHint: cloneInt(i.Season), EpisodeHint: cloneInt(i.Episode), IdentitySource: i.Source, IdentityStatus: i.Status, Metadata: classification.Metadata{MediaType: classification.MediaType(i.MediaType), ReleaseYear: cloneInt(i.Year)}}
	if i.TMDBID != nil {
		result.Snapshot = tmdb.Snapshot{Version: 1, TMDBID: *i.TMDBID, MediaType: i.MediaType, Title: i.Title}
	}
	return mediaLibraryRecognizedUnit{Unit: unit, Result: result, Manual: i.Locked}, true
}
