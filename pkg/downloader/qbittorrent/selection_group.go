package qbittorrent

import (
	"context"
	"net/url"
	"path"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

// ApplySelectionGroup operates only on the authority's current union. No
// task-scoped cancel or stale task source is allowed to shrink this union.
func (c *Client) ApplySelectionGroup(ctx context.Context, request downloader.SubmitRequest, group downloader.SelectionGroup) (downloader.Task, error) {
	manifest, err := downloader.ValidateContentSource(request.Source)
	if err != nil {
		return downloader.Task{}, err
	}
	if err = group.Validate(*request.Source.ContentSelection); err != nil {
		return downloader.Task{}, err
	}
	if err = group.Union.ValidateAgainst(manifest); err != nil {
		return downloader.Task{}, downloader.Error("downloader_selection_group_invalid", false, err)
	}
	if request.SavePath != group.Workspace {
		return downloader.Task{}, downloader.Error("downloader_selection_group_workspace_conflict", false, nil)
	}
	binding := downloader.SelectionContext{Tag: request.Tag, Selection: *request.Source.ContentSelection, RootName: manifest.Name, MultiFile: manifest.MultiFile, Group: &group}
	release, err := c.selectionLock(ctx, manifest.Identity)
	if err != nil {
		return downloader.Task{}, err
	}
	defer release()
	id := selectionHash(manifest.Identity)
	if manifest.Identity.V1 != "" && manifest.Identity.V2 != "" {
		id = manifest.Identity.V1 + "|" + manifest.Identity.V2[:40]
	}
	info, files, err := c.selectedState(ctx, id, binding, false)
	if err != nil {
		code, _ := downloader.ErrorInfo(err)
		if code != "downloader_task_not_found" {
			return downloader.Task{}, err
		}
		if group.Empty {
			return downloader.Task{ID: selectionHash(manifest.Identity), Status: "stoppedDL"}, nil
		}
		if err = c.requireSelectiveAPI(ctx); err != nil {
			return downloader.Task{}, err
		}
		add := request
		add.Tag, add.MetadataOnly = group.Tag, false
		add.Source.Kind, add.Source.URL, add.Source.Filename = downloader.SourceTorrent, "", path.Base(manifest.Name)+".torrent"
		add.Source.ContentSelection = &group.Union
		if _, err = c.submit(ctx, add, true); err != nil {
			return downloader.Task{}, err
		}
		info, files, err = c.selectedState(ctx, id, binding, false)
		if err != nil {
			return downloader.Task{}, err
		}
	}
	if !sameWorkspace(info.SavePath, group.Workspace) {
		return downloader.Task{}, downloader.Error("downloader_selection_group_workspace_conflict", false, nil)
	}
	marker, _ := selectionMarker(group.Union)
	if validateNativeFiles(files, binding, true) == nil && (group.Resume && !stopped(info.State) || !group.Resume && stopped(info.State)) && strings.Contains(info.Tags, marker) && len(strings.Split(info.Tags, ",")) == 2 {
		return selectedTask(info, files, binding), nil
	}
	// A running known group may contain another reference's active work. Pause
	// only for the bounded union update and restore the authority's run intent.
	if !stopped(info.State) {
		if err = c.pauseHash(ctx, info.Hash); err != nil {
			return downloader.Task{}, err
		}
		info, files, err = c.selectedState(ctx, info.Hash, binding, false)
		if err != nil {
			return downloader.Task{}, err
		}
		if !stopped(info.State) {
			return downloader.Task{}, downloader.Error("downloader_selection_stop_unconfirmed", true, nil)
		}
	}
	if validateNativeFiles(files, binding, true) != nil {
		all, wanted := []int{}, []int{}
		unionBinding := binding
		unionBinding.Selection = group.Union
		paths := selectedPaths(unionBinding)
		for _, f := range files {
			all = append(all, *f.Index)
			if !group.Empty {
				if _, ok := paths[f.Name]; ok {
					wanted = append(wanted, *f.Index)
				}
			}
		}
		if err = c.setPriorities(ctx, info.Hash, all, 0); err != nil {
			return downloader.Task{}, err
		}
		if err = c.setPriorities(ctx, info.Hash, wanted, 1); err != nil {
			return downloader.Task{}, err
		}
	}
	// Add the successor marker before removing predecessors. An interrupted
	// marker update is recoverable because only persisted known markers are
	// accepted; arbitrary user tags remain a conflict.
	if err = c.action(ctx, "/api/v2/torrents/addTags", url.Values{"hashes": {info.Hash}, "tags": {marker}}); err != nil {
		return downloader.Task{}, err
	}
	for _, old := range strings.Split(info.Tags, ",") {
		old = strings.TrimSpace(old)
		if old != group.Tag && old != marker && old != "" {
			if err = c.action(ctx, "/api/v2/torrents/removeTags", url.Values{"hashes": {info.Hash}, "tags": {old}}); err != nil {
				return downloader.Task{}, err
			}
		}
	}
	info, files, err = c.selectedState(ctx, info.Hash, binding, true)
	if err != nil {
		return downloader.Task{}, err
	}
	if group.Resume {
		if err = c.resumeHash(ctx, info.Hash); err != nil {
			return downloader.Task{}, err
		}
	}
	return selectedTask(info, files, binding), nil
}

func stopped(state string) bool {
	state = strings.ToLower(state)
	return strings.HasPrefix(state, "stopped") || strings.HasPrefix(state, "paused")
}
func sameWorkspace(a, b string) bool {
	a = strings.TrimRight(strings.ReplaceAll(a, `\`, "/"), "/")
	b = strings.TrimRight(strings.ReplaceAll(b, `\`, "/"), "/")
	windows := len(a) > 1 && a[1] == ':' && len(b) > 1 && b[1] == ':' || strings.HasPrefix(a, "//") && strings.HasPrefix(b, "//")
	if windows {
		return strings.EqualFold(a, b)
	}
	return a == b
}
