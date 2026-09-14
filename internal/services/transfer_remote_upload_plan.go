package services

import (
	"context"
	"errors"
	pathpkg "path"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

const (
	remoteUploadFailIfExists = "fail_if_exists"
	remoteUploadSkipIfExists = "skip_if_exists"
	remoteUploadReplace      = "replace"
)

type remoteNodeUploadTarget struct {
	File           downloadpkg.File
	Relative       string
	Group          string
	ConflictAction string
}

// planRemoteNodeUpload performs only bounded provider reads. It resolves every
// conflict choice before the immutable Node operation is accepted; the Node
// therefore never invents a name or silently changes the Server's policy.
func (w *TransferWorker) planRemoteNodeUpload(ctx context.Context, download models.DownloadTask, manifest downloadpkg.Manifest, policy string) ([]remoteNodeUploadTarget, int, error) {
	if w.service.connections == nil || download.TargetConnectionID == nil || download.TargetStorageID == nil || strings.TrimSpace(download.TargetProviderRootID) == "" {
		return nil, 0, cloudTransferError("cloud_upload_snapshot_invalid", false, nil)
	}
	connection, driver, err := w.service.connections.driver(*download.TargetConnectionID)
	if err != nil {
		return nil, 0, err
	}
	capabilities := driver.Capabilities()
	if connection.Provider != cloudpkg.ProviderPan115 || !capabilities.DirectoryList || !capabilities.FileUpload || !capabilities.CreateDirectory || !capabilities.Recycle {
		return nil, 0, cloudTransferError("cloud_upload_capability_missing", false, nil)
	}
	var storage models.Storage
	if err := w.service.db.WithContext(ctx).First(&storage, *download.TargetStorageID).Error; err != nil || storage.Type != models.StorageTypePan115 || storage.ConnectionID == nil || *storage.ConnectionID != *download.TargetConnectionID {
		return nil, 0, cloudTransferError("cloud_transfer_boundary_invalid", false, err)
	}
	targetBasePath, _ := joinProviderPath(storage.RootDisplayPath, download.TargetRelativeRoot)
	root, err := resolveCloudTargetRoot(ctx, driver, targetBasePath, download.TargetProviderRootID, storage.RootPath)
	if err != nil || !root.IsDir {
		return nil, 0, cloudTransferError("cloud_transfer_boundary_invalid", false, err)
	}
	ctx = withCloudDirectoryAttempt(ctx, driver, targetBasePath)
	targets, err := buildTransferTargets(download, manifest)
	if err != nil {
		return nil, 0, cloudTransferError("transfer_plan_invalid", false, err)
	}
	directories := map[string]remoteExistingDirectory{".": {id: root.ID, exists: true}}
	listings := make(map[string][]cloudpkg.Item)
	reserved := make(map[string]map[string]struct{})
	for _, target := range targets {
		directory := pathpkg.Dir(target.Relative)
		if reserved[directory] == nil {
			reserved[directory] = make(map[string]struct{})
		}
		reserved[directory][strings.ToLower(pathpkg.Base(target.Relative))] = struct{}{}
	}
	result := make([]remoteNodeUploadTarget, 0, len(targets))
	conflicts := 0
	for _, target := range targets {
		directory := pathpkg.Dir(target.Relative)
		resolved, err := resolveRemoteExistingDirectory(ctx, driver, root.ID, directory, directories)
		if err != nil {
			return nil, 0, err
		}
		planned := remoteNodeUploadTarget{File: target.File, Relative: target.Relative, Group: target.Group, ConflictAction: remoteUploadFailIfExists}
		if !resolved.exists {
			result = append(result, planned)
			continue
		}
		items, ok := listings[resolved.id]
		if !ok {
			items, err = listCloudTargetDirectoryCached(ctx, driver, resolved.id)
			if err != nil {
				return nil, 0, err
			}
			listings[resolved.id] = items
		}
		matches := namedCloudItems(items, pathpkg.Base(target.Relative))
		if len(matches) > 1 {
			return nil, 0, cloudTransferError(cloudpkg.CodeConflict, false, errors.New("cloud upload target is ambiguous"))
		}
		if len(matches) == 0 {
			result = append(result, planned)
			continue
		}
		conflicts++
		switch policy {
		case models.MediaLibraryConflictAsk:
			// The caller returns one ordinary queue action before accepting any
			// immutable remote plan.
		case models.MediaLibraryConflictSkip:
			planned.ConflictAction = remoteUploadSkipIfExists
		case models.MediaLibraryConflictOverwrite:
			if matches[0].IsDir {
				return nil, 0, cloudTransferError("cloud_transfer_target_type_conflict", false, nil)
			}
			planned.ConflictAction = remoteUploadReplace
		case models.MediaLibraryConflictRename:
			name, nameErr := availableFrozenRemoteUploadName(pathpkg.Base(target.Relative), items, reserved[directory])
			if nameErr != nil {
				return nil, 0, nameErr
			}
			delete(reserved[directory], strings.ToLower(pathpkg.Base(target.Relative)))
			reserved[directory][strings.ToLower(name)] = struct{}{}
			planned.Relative = pathpkg.Join(directory, name)
		default:
			return nil, 0, cloudTransferError("transfer_conflict_failed", false, nil)
		}
		result = append(result, planned)
	}
	return result, conflicts, nil
}

type remoteExistingDirectory struct {
	id     string
	exists bool
}

func resolveRemoteExistingDirectory(ctx context.Context, driver cloudpkg.Driver, rootID, relative string, cache map[string]remoteExistingDirectory) (remoteExistingDirectory, error) {
	relative = pathpkg.Clean(strings.ReplaceAll(strings.TrimSpace(relative), "\\", "/"))
	if relative == "." {
		return cache["."], nil
	}
	if relative == "" || relative == ".." || pathpkg.IsAbs(relative) || strings.HasPrefix(relative, "../") || strings.Contains(relative, ":") {
		return remoteExistingDirectory{}, cloudTransferError("transfer_plan_invalid", false, errors.New("remote upload directory is unsafe"))
	}
	current := remoteExistingDirectory{id: rootID, exists: true}
	walked := "."
	for _, segment := range strings.Split(relative, "/") {
		walked = pathpkg.Join(walked, segment)
		if saved, ok := cache[walked]; ok {
			current = saved
			continue
		}
		if !current.exists {
			cache[walked] = current
			continue
		}
		items, err := listCloudTargetDirectoryCached(ctx, driver, current.id)
		if err != nil {
			return remoteExistingDirectory{}, err
		}
		matches := namedCloudItems(items, segment)
		if len(matches) > 1 || len(matches) == 1 && !matches[0].IsDir {
			return remoteExistingDirectory{}, cloudTransferError(cloudpkg.CodeConflict, false, errors.New("remote upload directory is ambiguous"))
		}
		if len(matches) == 0 {
			current = remoteExistingDirectory{exists: false}
		} else {
			current = remoteExistingDirectory{id: matches[0].ID, exists: true}
		}
		cache[walked] = current
	}
	return current, nil
}

func availableFrozenRemoteUploadName(name string, items []cloudpkg.Item, reserved map[string]struct{}) (string, error) {
	existing := make(map[string]struct{}, len(items)+len(reserved))
	for _, item := range items {
		existing[strings.ToLower(item.Name)] = struct{}{}
	}
	for value := range reserved {
		existing[value] = struct{}{}
	}
	extension := pathpkg.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	for suffix := 2; suffix <= 999; suffix++ {
		candidate := stem + " (" + strconv.Itoa(suffix) + ")" + extension
		if _, exists := existing[strings.ToLower(candidate)]; !exists {
			return candidate, nil
		}
	}
	return "", cloudTransferError(cloudpkg.CodeConflict, false, errors.New("no remote upload target name is available"))
}

func (w *TransferWorker) loadRemoteUploadFiles(ctx context.Context, task models.TransferTask, download models.DownloadTask, manifest downloadpkg.Manifest) ([]models.RemoteUploadFile, bool, error) {
	var rows []models.RemoteUploadFile
	if err := w.service.db.WithContext(ctx).Where("transfer_task_id = ?", task.ID).Order("ordinal ASC").Find(&rows).Error; err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	targets, err := buildTransferTargets(download, manifest)
	if err != nil {
		return nil, true, errors.New("remote upload manifest no longer produces a valid target set")
	}
	byToken := make(map[string]downloadpkg.File, len(targets))
	for _, target := range targets {
		file := target.File
		if strings.TrimSpace(file.RemoteFileToken) == "" || nodeprotocol.ValidateFileToken(file.RemoteFileToken) != nil || !nodeprotocol.ValidDigest(file.SHA256) {
			return nil, true, errors.New("remote upload manifest contains invalid source identity")
		}
		if _, duplicate := byToken[file.RemoteFileToken]; duplicate {
			return nil, true, errors.New("remote upload manifest contains duplicate token")
		}
		byToken[file.RemoteFileToken] = file
	}
	if len(rows) != len(byToken) {
		return nil, true, errors.New("remote upload plan does not cover the frozen manifest")
	}
	seen := make(map[string]struct{}, len(rows))
	for index, row := range rows {
		file, ok := byToken[row.SourceFileToken]
		if !ok || row.Ordinal != index || row.TransferTaskID != task.ID || row.DownloadTaskID != task.DownloadTaskID || row.SourceRelative != file.RelativePath || row.Size != file.Size || row.SHA256 != file.SHA256 || !validRemoteUploadConflictAction(row.ConflictAction) {
			return nil, true, errors.New("remote upload plan conflicts with frozen manifest")
		}
		if relative, pathErr := sanitizeTransferRelativePath(row.TargetRelative); pathErr != nil || relative != row.TargetRelative {
			return nil, true, errors.New("remote upload plan contains an unsafe target")
		}
		if _, duplicate := seen[row.SourceFileToken]; duplicate {
			return nil, true, errors.New("remote upload plan contains duplicate token")
		}
		seen[row.SourceFileToken] = struct{}{}
	}
	return rows, true, nil
}

func validRemoteUploadConflictAction(action string) bool {
	switch action {
	case remoteUploadFailIfExists, remoteUploadSkipIfExists, remoteUploadReplace:
		return true
	default:
		return false
	}
}

func (w *TransferWorker) createRemoteUploadFiles(ctx context.Context, task models.TransferTask, planned []remoteNodeUploadTarget) ([]models.RemoteUploadFile, error) {
	if len(planned) == 0 {
		return nil, errors.New("remote upload plan is empty")
	}
	now := time.Now().UTC()
	rows := make([]models.RemoteUploadFile, 0, len(planned))
	for index, target := range planned {
		if strings.TrimSpace(target.File.RemoteFileToken) == "" || !nodeprotocol.ValidDigest(target.File.SHA256) {
			return nil, errors.New("remote upload source identity is invalid")
		}
		rows = append(rows, models.RemoteUploadFile{
			TransferTaskID: task.ID, DownloadTaskID: task.DownloadTaskID, Ordinal: index,
			SourceFileToken: target.File.RemoteFileToken, SourceRelative: target.File.RelativePath,
			TargetRelative: target.Relative, Size: target.File.Size, SHA256: target.File.SHA256,
			ConflictAction: target.ConflictAction, Status: "pending", CreatedAt: now, UpdatedAt: now,
		})
	}
	err := w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&models.RemoteUploadFile{}).Where("transfer_task_id = ?", task.ID).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return gorm.ErrDuplicatedKey
		}
		for start := 0; start < len(rows); start += 500 {
			end := min(start+500, len(rows))
			if err := tx.Create(rows[start:end]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}
