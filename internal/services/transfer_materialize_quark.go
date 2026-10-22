package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloader "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

// Quark does not provide the trusted SHA1 assumed by the old 115 route. Keep
// that route unchanged and record a versioned, locally computed SHA256 receipt.
func (w *TransferWorker) materializeQuarkSource(ctx context.Context, runtime JobRuntime, task models.TransferTask, download models.DownloadTask, manifest downloader.Manifest) (models.TransferTask, string, error) {
	if w.service.connections == nil || download.StagingStorageID == nil || download.ProviderOutputID == "" || download.TransferMode == models.MediaLibraryTransferSymlink {
		return task, "", cloudTransferError("cross_source_snapshot_invalid", false, nil)
	}
	var storage models.Storage
	if err := w.service.db.First(&storage, *download.StagingStorageID).Error; err != nil || storage.Type != models.StorageTypeQuark || storage.ConnectionID == nil {
		return task, "", cloudTransferError("cross_source_source_missing", false, err)
	}
	_, driver, err := w.service.connections.driver(*storage.ConnectionID)
	if err != nil {
		return task, "", err
	}
	reader, ok := driver.(cloud.ReadDriver)
	if !ok || driver.Provider() != cloud.ProviderQuark || !driver.Capabilities().TemporaryDirectURL {
		return task, "", cloudTransferError("cross_source_read_capability_missing", false, nil)
	}
	packageRoot, err := providerItemWithinRoot(ctx, driver, download.ProviderOutputID, storage.RootPath)
	if err != nil || !packageRoot.IsDir {
		return task, "", cloudTransferError("cross_source_boundary_invalid", false, err)
	}
	state, err := decodeCloudTransferState(task.CloudStateJSON)
	if err != nil {
		return task, "", err
	}
	root, err := w.crossSourceManagedRoot(ctx, task, download, &state)
	if err != nil {
		return task, "", err
	}
	remaining, err := materializationRemainingBytes(root, manifest)
	if err != nil {
		return task, "", err
	}
	// An unacknowledged finalized file must remain until its replacement has
	// been verified. Reserve that extra space rather than counting it as a
	// resumable prefix (only the old partial can be safely truncated).
	for _, file := range manifest.Files {
		if state.Materialized[normalizedManifestPath(file.RelativePath)].Status == "completed" {
			continue
		}
		destination, pathErr := materializedPath(root, file.RelativePath)
		if pathErr != nil {
			return task, "", pathErr
		}
		if info, statErr := os.Lstat(destination); statErr == nil {
			if uint64(info.Size()) > ^uint64(0)-remaining {
				return task, "", cloudTransferError("cross_source_manifest_invalid", false, nil)
			}
			remaining += uint64(info.Size())
		}
	}
	if err = requireMaterializationSpace(root, remaining, manifestTotalBytes(manifest)); err != nil {
		return task, "", err
	}
	for _, file := range manifest.Files {
		key := normalizedManifestPath(file.RelativePath)
		if key == "" || key != strings.ReplaceAll(strings.TrimSpace(file.RelativePath), "\\", "/") || file.ProviderItemID == "" || file.ProviderParentID == "" || file.Size < 0 {
			return task, "", cloudTransferError("cross_source_manifest_invalid", false, nil)
		}
		current, err := providerItemWithinRoot(ctx, driver, file.ProviderItemID, packageRoot.ID)
		if err != nil || current.IsDir || current.ParentID != file.ProviderParentID || current.Size != file.Size {
			return task, "", cloudTransferError("cross_source_source_changed", false, err)
		}
		destination, err := materializedPath(root, file.RelativePath)
		if err != nil {
			return task, "", err
		}
		saved := state.Materialized[key]
		if saved.Status == "completed" {
			if saved.RelativePath != key || saved.Size != file.Size || saved.DigestVersion != 2 || saved.DigestAlgorithm != "sha256" || saved.SourceRevision != current.Revision || saved.SourceModifiedAt == nil || !saved.SourceModifiedAt.Equal(current.ModifiedAt) {
				return task, "", cloudTransferError("cross_source_source_changed", false, nil)
			}
			if err := verifyMaterializedSHA256Runtime(ctx, runtime, destination, file.Size, saved.DigestValue); err != nil {
				return task, "", cloudTransferError("cross_source_staging_changed", false, err)
			}
			continue
		}
		state.Materialized[key] = materializedItemState{RelativePath: key, Size: file.Size, Status: "materializing", DigestVersion: 2, DigestAlgorithm: "sha256", SourceRevision: current.Revision, SourceModifiedAt: &current.ModifiedAt}
		if err := w.persistCloudState(&task, state, models.TransferTaskStatusTransferring, completedMaterializedItems(state), nil); err != nil {
			return task, "", err
		}
		digest, err := materializeQuarkFile(ctx, runtime, reader, file, root, destination)
		if err != nil {
			return task, "", err
		}
		fresh, err := providerItemWithinRoot(ctx, driver, file.ProviderItemID, packageRoot.ID)
		if err != nil || fresh.IsDir || fresh.ParentID != current.ParentID || fresh.Size != current.Size || fresh.Revision != current.Revision || !fresh.ModifiedAt.Equal(current.ModifiedAt) {
			return task, "", cloudTransferError("cross_source_source_changed", false, err)
		}
		saved = state.Materialized[key]
		saved.DigestValue, saved.Status = digest, "completed"
		state.Materialized[key] = saved
		if err := w.persistCloudState(&task, state, models.TransferTaskStatusTransferring, completedMaterializedItems(state), nil); err != nil {
			return task, "", err
		}
	}
	return task, root, nil
}

func materializeQuarkFile(ctx context.Context, runtime JobRuntime, reader cloud.ReadDriver, file downloader.File, root, destination string) (string, error) {
	if err := ensureSafeDirectoryPath(root, filepath.Dir(destination), true); err != nil {
		return "", err
	}
	// Without a provider digest/snapshot version, old partial bytes may belong
	// to another revision. Restart incomplete files instead of splicing them.
	for _, candidate := range []string{destination, destination + ".partial"} {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || medialibrary.IsUnsafeDirectory(candidate, fs.FileInfoToDirEntry(info)) {
			return "", cloudTransferError("cross_source_staging_changed", false, err)
		}
	}
	stream, err := reader.OpenRead(ctx, cloud.ReadRequest{FileID: file.ProviderItemID})
	if err != nil {
		return "", err
	}
	if stream.Body == nil {
		return "", cloudTransferError("cross_source_read_failed", true, nil)
	}
	defer stream.Body.Close()
	if stream.TotalSize != nil && *stream.TotalSize != file.Size {
		return "", cloudTransferError("cross_source_source_changed", false, nil)
	}
	output, err := os.OpenFile(destination+".partial", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	n, copyErr := copyMaterializedStream(ctx, runtime, io.MultiWriter(output, hash), stream.Body, file.Size)
	syncErr, closeErr := output.Sync(), output.Close()
	if err := firstNonNil(copyErr, syncErr, closeErr); err != nil {
		return "", err
	}
	if n != file.Size {
		return "", cloudTransferError("cross_source_download_incomplete", true, nil)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if file.SHA256 != "" && !strings.EqualFold(file.SHA256, digest) {
		return "", cloudTransferError("cross_source_checksum_mismatch", false, nil)
	}
	if err := verifyMaterializedSHA256Runtime(ctx, runtime, destination+".partial", file.Size, digest); err != nil {
		return "", err
	}
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(destination+".partial", destination); err != nil {
		return "", err
	}
	return digest, nil
}

func verifyMaterializedSHA256(path string, size int64, expected string) error {
	return verifyMaterializedSHA256Runtime(context.Background(), nil, path, size, expected)
}

func verifyMaterializedSHA256Runtime(ctx context.Context, runtime JobRuntime, path string, size int64, expected string) error {
	if len(expected) != sha256.Size*2 {
		return errors.New("materialized SHA256 receipt is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || medialibrary.IsUnsafeDirectory(path, fs.FileInfoToDirEntry(info)) || info.Size() != size {
		return errors.New("materialized file identity changed")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	digest := sha256.New()
	var copyErr error
	if runtime != nil {
		_, copyErr = copyMaterializedStream(ctx, runtime, digest, f, size)
	} else {
		_, copyErr = io.Copy(digest, io.LimitReader(f, size+1))
	}
	if copyErr != nil {
		return copyErr
	}
	if !strings.EqualFold(expected, hex.EncodeToString(digest.Sum(nil))) {
		return errors.New("materialized SHA256 mismatch")
	}
	return nil
}
