package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

const remotePullConcurrency = 4

func (w *TransferWorker) materializeRemoteNodeSource(ctx context.Context, runtime JobRuntime, task models.TransferTask, download models.DownloadTask, manifest downloadpkg.Manifest) (models.TransferTask, string, error) {
	if w.service.downloader == nil || normalizeExecutionLocation(download.ExecutionLocation) != models.NodeLocationRemote || download.NodeID == nil || task.NodeID == nil || *download.NodeID != *task.NodeID {
		return task, "", cloudTransferError("node_transfer_snapshot_invalid", false, nil)
	}
	if download.TransferMode == models.MediaLibraryTransferSymlink {
		return task, "", cloudTransferError("node_transfer_mode_invalid", false, errors.New("a local symlink cannot be backed by a remote node source"))
	}
	if manifest.RemoteExportOperationKey == "" || !nodeprotocol.ValidDigest(manifest.RemoteExportDigest) || manifest.RemoteExportExpiresAt == nil {
		return task, "", cloudTransferError("node_file_export_invalid", false, nil)
	}
	if !manifest.RemoteExportExpiresAt.After(time.Now().UTC().Add(10 * time.Minute)) {
		refreshed, refreshErr := w.service.downloader.refreshRemoteFileExport(ctx, download)
		if refreshErr != nil {
			return task, "", refreshErr
		}
		if refreshed.RemoteExportOperationKey != manifest.RemoteExportOperationKey || refreshed.RemoteExportDigest != manifest.RemoteExportDigest || refreshed.RemoteExportExpiresAt == nil || !refreshed.RemoteExportExpiresAt.After(time.Now().UTC()) {
			return task, "", cloudTransferError("node_file_export_changed", false, nil)
		}
		manifest.RemoteExportExpiresAt = refreshed.RemoteExportExpiresAt
		encoded, encodeErr := json.Marshal(manifest)
		if encodeErr != nil || len(encoded) > 1024*1024 {
			return task, "", cloudTransferError("node_file_export_invalid", false, encodeErr)
		}
		if err := w.service.db.WithContext(ctx).Model(&models.TransferTask{}).Where("id = ?", task.ID).Updates(map[string]any{"manifest_json": string(encoded), "updated_at": time.Now().UTC()}).Error; err != nil {
			return task, "", cloudTransferError("transfer_state_persist_failed", true, err)
		}
		task.ManifestJSON = string(encoded)
	}
	state, err := decodeCloudTransferState(task.CloudStateJSON)
	if err != nil {
		return task, "", cloudTransferError("cloud_transfer_state_invalid", false, err)
	}
	managedRoot, err := w.crossSourceManagedRoot(ctx, task, download, &state)
	if err != nil {
		return task, "", err
	}
	if err := w.persistCloudState(&task, state, models.TransferTaskStatusPlanning, completedRemoteTransferFiles(w.service.db, task.ID), nil); err != nil {
		return task, "", cloudTransferError("transfer_state_persist_failed", true, err)
	}
	remaining, err := remoteMaterializationRemainingBytes(w.service.db, task.ID, managedRoot, manifest)
	if err != nil {
		return task, "", cloudTransferError("node_staging_changed", false, err)
	}
	if err := requireMaterializationSpace(managedRoot, remaining, manifestTotalBytes(manifest)); err != nil {
		return task, "", err
	}
	reader, err := w.service.downloader.remoteFileReaderForTask(ctx, download)
	if err != nil {
		return task, "", err
	}
	for _, file := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return task, "", err
		}
		if err := validateRemoteManifestFile(file); err != nil {
			return task, "", cloudTransferError("node_file_manifest_invalid", false, err)
		}
		destination, err := materializedPath(managedRoot, file.RelativePath)
		if err != nil {
			return task, "", cloudTransferError("node_file_manifest_invalid", false, err)
		}
		chunks, err := reader.RemoteFileChunks(ctx, manifest, file)
		if err != nil {
			return task, "", err
		}
		checkpoint, err := w.loadOrCreateRemoteFileCheckpoint(ctx, task, download, manifest, file, len(chunks))
		if err != nil {
			return task, "", err
		}
		if checkpoint.Status == "completed" {
			if err := verifyRemoteMaterializedFile(destination, file); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return task, "", cloudTransferError("node_staging_changed", false, err)
			}
			checkpoint.Status = "pending"
			checkpoint.CompletedBitmap = make([]byte, len(checkpoint.CompletedBitmap))
			if err := w.persistRemoteBitmap(ctx, &checkpoint); err != nil {
				return task, "", err
			}
		}
		if err := w.pullRemoteFile(ctx, runtime, reader, manifest, file, chunks, managedRoot, destination, &checkpoint); err != nil {
			return task, "", err
		}
		processed, total := int64(completedRemoteTransferFiles(w.service.db, task.ID)), int64(len(manifest.Files))
		progress := float64(processed) * 100 / float64(total)
		if err := runtime.Heartbeat(&progress, &processed, &total, nil, nil); err != nil {
			return task, "", cloudTransferError(CodeQueueLeaseInvalid, true, err)
		}
	}
	return task, managedRoot, nil
}

func validateRemoteManifestFile(file downloadpkg.File) error {
	relative := strings.ReplaceAll(strings.TrimSpace(file.RelativePath), "\\", "/")
	if relative == "" || relative != file.RelativePath || file.Size < 0 || !nodeprotocol.ValidDigest(file.SHA256) || nodeprotocol.ValidateFileToken(file.RemoteFileToken) != nil {
		return errors.New("remote file identity is invalid")
	}
	_, err := materializedPath(".", relative)
	return err
}

func (w *TransferWorker) loadOrCreateRemoteFileCheckpoint(ctx context.Context, task models.TransferTask, download models.DownloadTask, manifest downloadpkg.Manifest, file downloadpkg.File, chunkCount int) (models.RemoteTransferFile, error) {
	bitmapBytes := (chunkCount + 7) / 8
	now := time.Now().UTC()
	var result models.RemoteTransferFile
	err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Where("transfer_task_id = ? AND file_token = ?", task.ID, file.RemoteFileToken).First(&result).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = models.RemoteTransferFile{TransferTaskID: task.ID, DownloadTaskID: download.ID, OperationKey: manifest.RemoteExportOperationKey, ManifestDigest: manifest.RemoteExportDigest, FileToken: file.RemoteFileToken, RelativePath: file.RelativePath, Size: file.Size, SHA256: file.SHA256, ChunkSize: nodeprotocol.FileChunkSize, ChunkCount: chunkCount, CompletedBitmap: make([]byte, bitmapBytes), Status: "pending", CreatedAt: now, UpdatedAt: now}
			return tx.Create(&result).Error
		}
		if err != nil {
			return err
		}
		if result.DownloadTaskID != download.ID || result.OperationKey != manifest.RemoteExportOperationKey || result.ManifestDigest != manifest.RemoteExportDigest || result.RelativePath != file.RelativePath || result.Size != file.Size || result.SHA256 != file.SHA256 || result.ChunkSize != nodeprotocol.FileChunkSize || result.ChunkCount != chunkCount || len(result.CompletedBitmap) != bitmapBytes {
			return errors.New("node transfer checkpoint conflicts with frozen manifest")
		}
		return nil
	})
	if err != nil {
		return models.RemoteTransferFile{}, cloudTransferError("node_transfer_checkpoint_invalid", false, err)
	}
	return result, nil
}

func (w *TransferWorker) pullRemoteFile(ctx context.Context, runtime JobRuntime, reader downloadpkg.RemoteFileReader, manifest downloadpkg.Manifest, file downloadpkg.File, chunks []downloadpkg.RemoteFileChunk, root, destination string, checkpoint *models.RemoteTransferFile) error {
	if err := ensureSafeDirectoryPath(root, filepath.Dir(destination), true); err != nil {
		return cloudTransferError("node_staging_boundary_invalid", false, err)
	}
	if err := verifyRemoteMaterializedFile(destination, file); err == nil {
		return w.markRemoteFileCompleted(ctx, checkpoint)
	} else if !errors.Is(err, os.ErrNotExist) {
		return cloudTransferError("node_staging_changed", false, err)
	}
	partial := destination + ".partial"
	output, err := openRemotePartial(partial, file.Size)
	if err != nil {
		return cloudTransferError("node_staging_unavailable", true, err)
	}
	defer func() { _ = output.Close() }()
	if err := reconcileRemoteBitmap(output, checkpoint, chunks); err != nil {
		return cloudTransferError("node_staging_changed", false, err)
	}
	for start := 0; start < len(chunks); start += remotePullConcurrency {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch := make([]downloadpkg.RemoteFileChunk, 0, remotePullConcurrency)
		for index := start; index < len(chunks) && index < start+remotePullConcurrency; index++ {
			if !bitmapHas(checkpoint.CompletedBitmap, chunks[index].Index) {
				batch = append(batch, chunks[index])
			}
		}
		if len(batch) == 0 {
			continue
		}
		type chunkResult struct {
			chunk downloadpkg.RemoteFileChunk
			err   error
		}
		results := make(chan chunkResult, len(batch))
		var group sync.WaitGroup
		for _, chunk := range batch {
			chunk := chunk
			group.Add(1)
			go func() {
				defer group.Done()
				payload, readErr := reader.ReadRemoteFileChunk(ctx, manifest, file, chunk)
				if readErr == nil {
					written, writeErr := output.WriteAt(payload, chunk.Offset)
					if writeErr != nil {
						readErr = writeErr
					} else if written != len(payload) {
						readErr = io.ErrShortWrite
					}
				}
				results <- chunkResult{chunk: chunk, err: readErr}
			}()
		}
		group.Wait()
		close(results)
		succeeded := make([]downloadpkg.RemoteFileChunk, 0, len(batch))
		var firstErr error
		for result := range results {
			if result.err != nil {
				if firstErr == nil {
					firstErr = result.err
				}
				continue
			}
			succeeded = append(succeeded, result.chunk)
		}
		if len(succeeded) > 0 {
			if err := output.Sync(); err != nil {
				return cloudTransferError("node_staging_write_failed", true, err)
			}
			for _, chunk := range succeeded {
				bitmapSet(checkpoint.CompletedBitmap, chunk.Index)
			}
			if err := w.persistRemoteBitmap(ctx, checkpoint); err != nil {
				return err
			}
			if err := runtime.Heartbeat(nil, nil, nil, nil, nil); err != nil {
				return cloudTransferError(CodeQueueLeaseInvalid, true, err)
			}
		}
		if firstErr != nil {
			return firstErr
		}
	}
	if !bitmapComplete(checkpoint.CompletedBitmap, len(chunks)) {
		return cloudTransferError("node_file_pull_incomplete", true, nil)
	}
	if err := output.Sync(); err != nil {
		return cloudTransferError("node_staging_write_failed", true, err)
	}
	if err := output.Close(); err != nil {
		return cloudTransferError("node_staging_write_failed", true, err)
	}
	if err := verifyRemoteMaterializedFile(partial, file); err != nil {
		return cloudTransferError("node_checksum_mismatch", false, err)
	}
	if err := os.Rename(partial, destination); err != nil {
		if verifyErr := verifyRemoteMaterializedFile(destination, file); verifyErr != nil {
			return cloudTransferError("node_staging_finalize_failed", true, err)
		}
		_ = os.Remove(partial)
	}
	return w.markRemoteFileCompleted(ctx, checkpoint)
}

func openRemotePartial(path string, size int64) (*os.File, error) {
	var before fs.FileInfo
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || medialibrary.IsUnsafeDirectory(path, fs.FileInfoToDirEntry(info)) || info.Size() != size {
			return nil, errors.New("remote partial identity changed")
		}
		before = info
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("remote partial identity changed")
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || medialibrary.IsUnsafeDirectory(path, fs.FileInfoToDirEntry(current)) || !os.SameFile(opened, current) || before != nil && !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, errors.New("remote partial identity changed")
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		return nil, err
	}
	opened, err = file.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || opened.Size() != size || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		_ = file.Close()
		return nil, errors.New("remote partial identity changed")
	}
	return file, nil
}

func reconcileRemoteBitmap(file *os.File, checkpoint *models.RemoteTransferFile, chunks []downloadpkg.RemoteFileChunk) error {
	changed := false
	for _, chunk := range chunks {
		if !bitmapHas(checkpoint.CompletedBitmap, chunk.Index) {
			continue
		}
		hash := sha256.New()
		if _, err := io.CopyN(hash, io.NewSectionReader(file, chunk.Offset, chunk.Size), chunk.Size); err != nil || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), chunk.SHA256) {
			bitmapClear(checkpoint.CompletedBitmap, chunk.Index)
			changed = true
		}
	}
	if changed {
		checkpoint.Status = "pending"
	}
	return nil
}

func (w *TransferWorker) persistRemoteBitmap(ctx context.Context, checkpoint *models.RemoteTransferFile) error {
	checkpoint.UpdatedAt = time.Now().UTC()
	result := w.service.db.WithContext(ctx).Model(&models.RemoteTransferFile{}).
		Where("id = ? AND operation_key = ? AND manifest_digest = ? AND file_token = ?", checkpoint.ID, checkpoint.OperationKey, checkpoint.ManifestDigest, checkpoint.FileToken).
		Updates(map[string]any{"completed_bitmap": checkpoint.CompletedBitmap, "status": checkpoint.Status, "updated_at": checkpoint.UpdatedAt})
	if result.Error != nil || result.RowsAffected != 1 {
		return cloudTransferError("node_transfer_checkpoint_persist_failed", true, result.Error)
	}
	return nil
}

func (w *TransferWorker) markRemoteFileCompleted(ctx context.Context, checkpoint *models.RemoteTransferFile) error {
	checkpoint.Status = "completed"
	return w.persistRemoteBitmap(ctx, checkpoint)
}

func verifyRemoteMaterializedFile(path string, expected downloadpkg.File) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || medialibrary.IsUnsafeDirectory(path, fs.FileInfoToDirEntry(info)) || info.Size() != expected.Size {
		return errors.New("remote materialized file identity changed")
	}
	input, err := os.Open(path)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, input)
	closeErr := input.Close()
	if copyErr != nil || closeErr != nil {
		return firstNonNil(copyErr, closeErr)
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected.SHA256) {
		return errors.New("remote materialized file checksum changed")
	}
	return nil
}

func remoteMaterializationRemainingBytes(db *gorm.DB, transferTaskID, root string, manifest downloadpkg.Manifest) (uint64, error) {
	var completed []models.RemoteTransferFile
	if err := db.Where("transfer_task_id = ? AND status = ?", transferTaskID, "completed").Find(&completed).Error; err != nil {
		return 0, err
	}
	completedPaths := make(map[string]models.RemoteTransferFile, len(completed))
	for _, file := range completed {
		completedPaths[file.RelativePath] = file
	}
	var remaining uint64
	for _, file := range manifest.Files {
		if file.Size < 0 {
			return 0, errors.New("remote materialization size is invalid")
		}
		destination, err := materializedPath(root, file.RelativePath)
		if err != nil {
			return 0, err
		}
		if saved, exists := completedPaths[file.RelativePath]; exists && saved.Size == file.Size && saved.SHA256 == file.SHA256 {
			if err := verifyRemoteMaterializedFile(destination, file); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
		}
		if uint64(file.Size) > ^uint64(0)-remaining {
			return 0, errors.New("remote materialization size overflow")
		}
		remaining += uint64(file.Size)
	}
	return remaining, nil
}

func completedRemoteTransferFiles(db *gorm.DB, transferTaskID string) int {
	var count int64
	if db.Model(&models.RemoteTransferFile{}).Where("transfer_task_id = ? AND status = ?", transferTaskID, "completed").Count(&count).Error != nil {
		return 0
	}
	return int(count)
}

func bitmapHas(bitmap []byte, index int) bool {
	return index >= 0 && index/8 < len(bitmap) && bitmap[index/8]&(1<<uint(index%8)) != 0
}

func bitmapSet(bitmap []byte, index int) {
	if index >= 0 && index/8 < len(bitmap) {
		bitmap[index/8] |= 1 << uint(index%8)
	}
}

func bitmapClear(bitmap []byte, index int) {
	if index >= 0 && index/8 < len(bitmap) {
		bitmap[index/8] &^= 1 << uint(index%8)
	}
}

func bitmapComplete(bitmap []byte, count int) bool {
	for index := 0; index < count; index++ {
		if !bitmapHas(bitmap, index) {
			return false
		}
	}
	return true
}
