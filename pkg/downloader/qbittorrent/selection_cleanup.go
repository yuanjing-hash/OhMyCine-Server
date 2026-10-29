package qbittorrent

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

// This adapter removes the owned native task only, never qB's whole-package
// data. Exact filesystem cleanup is performed at the execution location.
func (c *Client) CleanupSelectionGroup(ctx context.Context, source downloader.Source, binding downloader.SelectionContext, cleanup downloader.GroupCleanup) (bool, error) {
	manifest, err := downloader.ValidateContentSource(source)
	if err != nil {
		return false, err
	}
	if cleanup.Group.Validate(binding.Selection) != nil || cleanup.Files.ValidateAgainst(manifest) != nil || binding.Group == nil || binding.Group.ID != cleanup.Group.ID {
		return false, downloader.Error("downloader_selection_cleanup_invalid", false, nil)
	}
	release, err := c.selectionLock(ctx, manifest.Identity)
	if err != nil {
		return false, err
	}
	defer release()
	info, _, err := c.selectedState(ctx, selectionHash(manifest.Identity), binding, true)
	if err != nil {
		code, _ := downloader.ErrorInfo(err)
		if code == "downloader_task_not_found" {
			return true, nil
		}
		// A later lifecycle may already own this hash in a different workspace.
		// It is not the old task and must never be cancelled by a late cleanup.
		if code == "downloader_selection_owner_conflict" && info.Hash != "" && !sameWorkspace(info.SavePath, cleanup.Group.Workspace) {
			return true, nil
		}
		return false, err
	}
	if !sameWorkspace(info.SavePath, cleanup.Group.Workspace) || !stopped(info.State) {
		return false, downloader.Error("downloader_selection_cleanup_invalid", false, nil)
	}
	if cleanup.ProbeOnly {
		return false, downloader.Error("downloader_selection_cleanup_unconfirmed", true, nil)
	}
	if err = c.cancelHash(ctx, info.Hash, false); err != nil {
		return false, downloader.Error("downloader_selection_cleanup_unconfirmed", true, err)
	}
	_, _, err = c.selectedState(ctx, info.Hash, binding, false)
	if code, _ := downloader.ErrorInfo(err); code == "downloader_task_not_found" {
		return true, nil
	}
	return false, downloader.Error("downloader_selection_cleanup_unconfirmed", true, err)
}
