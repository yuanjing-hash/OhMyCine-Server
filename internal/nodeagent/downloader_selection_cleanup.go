package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func (a *Agent) executeSelectionGroupCleanup(ctx context.Context, server string, input nodeprotocol.DownloaderActionRequest, credential nodeprotocol.DownloaderCredential, base downloadpkg.Client, source downloadpkg.Source, local string) (downloadpkg.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if input.Group == nil || input.Cleanup == nil || input.Cleanup.Group.ID != input.Group.ID {
		return downloadpkg.Task{}, ErrPlanConflict
	}
	group := *input.Group
	cleanup := *input.Cleanup
	cleanup.Group = group
	expected := strings.TrimRight(strings.ReplaceAll(credential.DownloaderSaveRoot, `\`, "/"), "/") + "/.omc-bt/" + group.ID
	if strings.ReplaceAll(group.Workspace, `\`, "/") != expected {
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, nil)
	}
	manifest, err := downloadpkg.ValidateContentSource(source)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	if cleanup.Files.ValidateAgainst(manifest) != nil || group.Validate(*source.ContentSelection) != nil {
		return downloadpkg.Task{}, ErrPlanConflict
	}
	binding := downloadpkg.SelectionContext{Tag: input.Submit.Tag, Selection: *source.ContentSelection, RootName: manifest.Name, MultiFile: manifest.MultiFile, Group: &group}
	finish, release, err := a.store.beginDownloadGroup(ctx, server, input.DownloaderID, input.TaskID, credential.SourceScope, local, binding, group, true)
	if err != nil {
		if errors.Is(err, errDownloadGroupStale) {
			return downloadpkg.Task{}, downloadpkg.Error("downloader_selection_group_busy", true, err)
		}
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
	}
	defer release()
	ctx, err = a.store.downloadGroupFence(ctx, server, input.DownloaderID, group)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	var state, key, digest string
	err = a.store.db.QueryRowContext(ctx, `SELECT cleanup_state,cleanup_key,cleanup_digest FROM managed_download_groups WHERE paired_server_id=? AND downloader_id=? AND group_id=?`, server, input.DownloaderID, group.ID).Scan(&state, &key, &digest)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	planRaw, _ := json.Marshal(struct {
		Key    string
		Files  any
		Delete bool
	}{cleanup.Key, cleanup.Files, cleanup.DeleteFiles})
	actualDigest := fmt.Sprintf("%x", sha256.Sum256(planRaw))
	if state != "" && (key != cleanup.Key || digest != actualDigest) {
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	if state == "completed" {
		return downloadpkg.Task{ID: "cleanup:" + cleanup.Key}, nil
	}
	// A missing Node receipt proves that no previous Node invocation crossed
	// the durable write-ahead boundary, even if Server's caller lost contact.
	cleanup.ProbeOnly = state != ""
	if state == "" {
		_, err = a.store.db.ExecContext(ctx, `UPDATE managed_download_groups SET cleanup_state='requested',cleanup_key=?,cleanup_digest=? WHERE paired_server_id=? AND downloader_id=? AND group_id=? AND epoch=?`, cleanup.Key, actualDigest, server, input.DownloaderID, group.ID, group.Epoch)
		if err != nil {
			return downloadpkg.Task{}, err
		}
	}
	if state != "native_removed" {
		cleaner, ok := base.(downloadpkg.SelectionGroupCleaner)
		if !ok {
			return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
		}
		gone, err := cleaner.CleanupSelectionGroup(ctx, source, binding, cleanup)
		if err != nil {
			return downloadpkg.Task{}, err
		}
		if !gone {
			return downloadpkg.Task{}, downloadpkg.Error("downloader_selection_cleanup_unconfirmed", true, nil)
		}
		_, err = a.store.db.ExecContext(ctx, `UPDATE managed_download_groups SET cleanup_state='native_removed' WHERE paired_server_id=? AND downloader_id=? AND group_id=? AND epoch=?`, server, input.DownloaderID, group.ID, group.Epoch)
		if err != nil {
			return downloadpkg.Task{}, err
		}
	}
	if cleanup.DeleteFiles {
		if _, err = downloadpkg.RemoveSelectionGroupFiles(ctx, local, binding, cleanup.Files); err != nil {
			return downloadpkg.Task{}, err
		}
	}
	// finish verifies the same lease/epoch without changing native identity.
	var provider string
	if err = a.store.db.QueryRowContext(ctx, `SELECT provider_task_id FROM managed_download_groups WHERE paired_server_id=? AND downloader_id=? AND group_id=?`, server, input.DownloaderID, group.ID).Scan(&provider); err != nil {
		return downloadpkg.Task{}, err
	}
	if err = finish(provider); err != nil {
		return downloadpkg.Task{}, err
	}
	_, err = a.store.db.ExecContext(ctx, `UPDATE managed_download_groups SET cleanup_state='completed' WHERE paired_server_id=? AND downloader_id=? AND group_id=? AND epoch=?`, server, input.DownloaderID, group.ID, group.Epoch)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	return downloadpkg.Task{ID: "cleanup:" + cleanup.Key}, nil
}
