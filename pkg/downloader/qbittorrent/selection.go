package qbittorrent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

type nativeFile struct {
	Index    *int     `json:"index"`
	Name     string   `json:"name"`
	Size     int64    `json:"size"`
	Priority *int     `json:"priority"`
	Progress *float64 `json:"progress"`
}

type selectiveLock struct {
	token chan struct{}
	refs  int
}

var selectiveLocks = struct {
	sync.Mutex
	entries map[string]*selectiveLock
}{entries: map[string]*selectiveLock{}}

func (c *Client) selectionLock(ctx context.Context, identity resourcecontent.Identity) (func(), error) {
	return c.nativeLocks(ctx, identityHashes(identity))
}

func selectionMarker(selection resourcecontent.Selection) (string, error) {
	digest, err := selection.Digest()
	if err != nil {
		return "", downloader.Error("downloader_selection_invalid", false, err)
	}
	return "omc-sel-v1-" + digest, nil
}
func selectionHash(identity resourcecontent.Identity) string {
	if identity.V1 != "" {
		return identity.V1
	}
	return identity.V2[:40]
}
func validateBinding(binding downloader.SelectionContext) error {
	if !validManagedTag(binding.Tag) || binding.RootName == "" || strings.ContainsAny(binding.RootName, "/\\:\x00\r\n") {
		return downloader.Error("downloader_selection_invalid", false, nil)
	}
	_, err := selectionMarker(binding.Selection)
	if err == nil && binding.Group != nil {
		err = binding.Group.Validate(binding.Selection)
	}
	return err
}

func (c *Client) selectedState(ctx context.Context, id string, binding downloader.SelectionContext, requirePriorities bool) (torrentInfo, []nativeFile, error) {
	if err := validateBinding(binding); err != nil {
		return torrentInfo{}, nil, err
	}
	var info torrentInfo
	var files []nativeFile
	err := c.session(ctx, func(cookie string) error {
		values := url.Values{}
		if strings.HasPrefix(id, "tag:") {
			if strings.TrimPrefix(id, "tag:") != binding.Tag {
				return downloader.Error("downloader_selection_owner_conflict", false, nil)
			}
			tag := binding.Tag
			if binding.Group != nil {
				tag = binding.Group.Tag
			}
			values.Set("tag", tag)
		} else {
			values.Set("hashes", id)
		}
		body, status, err := c.do(ctx, cookie, http.MethodGet, "/api/v2/torrents/info?"+values.Encode(), "", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return statusError(status)
		}
		var infos []torrentInfo
		if json.Unmarshal(body, &infos) != nil || len(infos) > 1 {
			return downloader.Error("downloader_selection_owner_conflict", false, nil)
		}
		if len(infos) == 0 {
			return downloader.Error("downloader_task_not_found", false, nil)
		}
		info = infos[0]
		identity := binding.Selection.Identity
		v2Short := identity.V2 != "" && len(identity.V2) == 64 && info.Hash == identity.V2[:40]
		if info.Hash != identity.V1 && info.Hash != identity.V2 && !v2Short {
			return downloader.Error("downloader_selection_identity_mismatch", false, nil)
		}
		if identity.V2 != "" {
			properties, status, e := c.do(ctx, cookie, http.MethodGet, "/api/v2/torrents/properties?"+url.Values{"hash": {info.Hash}}.Encode(), "", nil)
			var hashes struct {
				V1 string `json:"infohash_v1"`
				V2 string `json:"infohash_v2"`
			}
			if e != nil {
				return e
			}
			if status != http.StatusOK || json.Unmarshal(properties, &hashes) != nil || hashes.V2 != identity.V2 || identity.V1 != "" && hashes.V1 != identity.V1 {
				return downloader.Error("downloader_selection_v2_identity_unsupported", false, nil)
			}
		}
		marker, _ := selectionMarker(binding.Selection)
		seen := map[string]bool{}
		for _, tag := range strings.Split(info.Tags, ",") {
			tag = strings.TrimSpace(tag)
			if tag != "" {
				seen[tag] = true
			}
		}
		validTags := len(seen) == 2 && seen[binding.Tag] && seen[marker]
		if binding.Group != nil {
			g := binding.Group
			current, _ := selectionMarker(g.Union)
			known := seen[current]
			allowed := map[string]bool{g.Tag: true, current: true}
			for _, digest := range g.PreviousDigests {
				known = known || seen["omc-sel-v1-"+digest]
				allowed["omc-sel-v1-"+digest] = true
			}
			validTags = seen[g.Tag] && known
			for tag := range seen {
				validTags = validTags && allowed[tag]
			}
		}
		if !validTags {
			return downloader.Error("downloader_selection_owner_conflict", false, nil)
		}
		body, status, err = c.do(ctx, cookie, http.MethodGet, "/api/v2/torrents/files?"+url.Values{"hash": {info.Hash}}.Encode(), "", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return statusError(status)
		}
		if json.Unmarshal(body, &files) != nil || len(files) == 0 || len(files) > maxManifestFiles {
			return downloader.Error("downloader_manifest_invalid", false, nil)
		}
		return validateNativeFiles(files, binding, requirePriorities)
	})
	return info, files, err
}

func validateNativeFiles(files []nativeFile, binding downloader.SelectionContext, priorities bool) error {
	if binding.Group != nil {
		binding.Selection = binding.Group.Union
	}
	selected := selectedPaths(binding)
	if binding.Group != nil && binding.Group.Empty {
		selected = map[string]resourcecontent.SelectedFile{}
	}
	seen := map[int]bool{}
	seenNames := map[string]bool{}
	matched := 0
	for nativePosition, file := range files {
		if file.Index == nil || *file.Index < 0 || seen[*file.Index] || file.Size < 0 {
			return downloader.Error("downloader_selection_mapping_invalid", false, nil)
		}
		seen[*file.Index] = true
		name, ok := safeManifestPath(file.Name)
		if !ok || seenNames[name] {
			return downloader.Error("downloader_manifest_invalid", false, nil)
		}
		seenNames[name] = true
		files[nativePosition].Name = name
		if wanted, ok := selected[name]; ok {
			if file.Size != wanted.Size {
				return downloader.Error("downloader_selection_mapping_invalid", false, nil)
			}
			if priorities && (file.Priority == nil || *file.Priority != 1) {
				return downloader.Error("downloader_selection_priority_mismatch", false, nil)
			}
			if file.Progress == nil || *file.Progress < 0 || *file.Progress > 1 {
				return downloader.Error("downloader_selection_progress_unknown", false, nil)
			}
			matched++
		} else if priorities && (file.Priority == nil || *file.Priority != 0) {
			return downloader.Error("downloader_selection_priority_mismatch", false, nil)
		}
	}
	if matched != len(selected) {
		return downloader.Error("downloader_selection_mapping_invalid", false, nil)
	}
	return nil
}

func selectedPaths(binding downloader.SelectionContext) map[string]resourcecontent.SelectedFile {
	selected := make(map[string]resourcecontent.SelectedFile, len(binding.Selection.Files))
	for _, file := range binding.Selection.Files {
		key := file.Path
		if binding.MultiFile {
			key = binding.RootName + "/" + key
		}
		selected[key] = file
	}
	return selected
}

func (c *Client) submitSelected(ctx context.Context, request downloader.SubmitRequest) (downloader.Task, error) {
	manifest, err := downloader.ValidateContentSource(request.Source)
	if err != nil {
		return downloader.Task{}, err
	}
	binding := downloader.SelectionContext{Tag: request.Tag, Selection: *request.Source.ContentSelection, RootName: manifest.Name, MultiFile: manifest.MultiFile}
	if err = validateBinding(binding); err != nil {
		return downloader.Task{}, err
	}
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
		if err = c.requireSelectiveAPI(ctx); err != nil {
			return downloader.Task{}, err
		}
		// Complete metainfo is submitted stopped on both supported qB generations.
		// An existing hash cannot be adopted merely because it is named OMC.
		request.Source.Kind, request.Source.URL, request.Source.Filename = downloader.SourceTorrent, "", path.Base(manifest.Name)+".torrent"
		request.MetadataOnly = false
		if _, err = c.submit(ctx, request, true); err != nil {
			return downloader.Task{}, err
		}
		info, files, err = c.selectedState(ctx, id, binding, false)
		if err != nil {
			code, retryable := downloader.ErrorInfo(err)
			if !retryable && code != "downloader_task_not_found" {
				return downloader.Task{}, err
			}
			return downloader.Task{}, downloader.Error("downloader_selection_submission_unconfirmed", true, err)
		}
	}
	if validateNativeFiles(files, binding, true) == nil {
		return selectedTask(info, files, binding), nil
	}
	state := strings.ToLower(info.State)
	if !strings.HasPrefix(state, "paused") && !strings.HasPrefix(state, "stopped") {
		return downloader.Task{}, downloader.Error("downloader_selection_stop_unconfirmed", false, nil)
	}
	all := make([]int, 0, len(files))
	wanted := make([]int, 0, len(binding.Selection.Files))
	for _, file := range files {
		all = append(all, *file.Index)
	}
	paths := selectedPaths(binding)
	for _, file := range files {
		if _, ok := paths[file.Name]; ok {
			wanted = append(wanted, *file.Index)
		}
	}
	if err = c.setPriorities(ctx, info.Hash, all, 0); err != nil {
		return downloader.Task{}, err
	}
	if err = c.setPriorities(ctx, info.Hash, wanted, 1); err != nil {
		return downloader.Task{}, err
	}
	info, files, err = c.selectedState(ctx, info.Hash, binding, true)
	if err != nil {
		return downloader.Task{}, err
	}
	return selectedTask(info, files, binding), nil
}

func (c *Client) requireSelectiveAPI(ctx context.Context) error {
	return c.session(ctx, func(cookie string) error {
		body, status, err := c.do(ctx, cookie, http.MethodGet, "/api/v2/app/webapiVersion", "", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return downloader.Error("downloader_selective_files_unsupported", false, nil)
		}
		parts := strings.Split(strings.TrimSpace(string(body)), ".")
		if len(parts) < 3 {
			return downloader.Error("downloader_selective_files_unsupported", false, nil)
		}
		numbers := [3]int{}
		for i := range numbers {
			n, e := strconv.Atoi(parts[i])
			if e != nil {
				return downloader.Error("downloader_selective_files_unsupported", false, nil)
			}
			numbers[i] = n
		}
		if numbers[0] != 2 || numbers[1] < 8 || numbers[1] == 8 && numbers[2] < 3 {
			return downloader.Error("downloader_selective_files_unsupported", false, nil)
		}
		return nil
	})
}

func (c *Client) setPriorities(ctx context.Context, hash string, indexes []int, priority int) error {
	sort.Ints(indexes)
	for start := 0; start < len(indexes); start += 256 {
		end := start + 256
		if end > len(indexes) {
			end = len(indexes)
		}
		ids := make([]string, 0, end-start)
		for _, index := range indexes[start:end] {
			ids = append(ids, strconv.Itoa(index))
		}
		if err := c.action(ctx, "/api/v2/torrents/filePrio", url.Values{"hash": {hash}, "id": {strings.Join(ids, "|")}, "priority": {strconv.Itoa(priority)}}); err != nil {
			return err
		}
	}
	return nil
}

func selectedTask(info torrentInfo, files []nativeFile, binding downloader.SelectionContext) downloader.Task {
	task := toTask(info)
	selected := selectedPaths(binding)
	var total, completed int64
	done := true
	for _, file := range files {
		if _, ok := selected[file.Name]; ok {
			total += file.Size
			if file.Progress == nil {
				done = false
				continue
			}
			if *file.Progress >= 1 {
				completed += file.Size
			} else {
				completed += int64(float64(file.Size) * *file.Progress)
			}
			if *file.Progress < 1 {
				done = false
			}
		}
	}
	progress := float64(100)
	if total > 0 {
		progress = float64(completed) * 100 / float64(total)
	}
	task.BytesTotal, task.BytesCompleted, task.Progress = &total, &completed, &progress
	state := strings.ToLower(info.State)
	task.Completed = done && !strings.Contains(state, "checking") && !strings.Contains(state, "meta") && !task.Failed
	return task
}

func (c *Client) GetSelected(ctx context.Context, id string, binding downloader.SelectionContext) (downloader.Task, error) {
	info, files, err := c.selectedState(ctx, id, binding, true)
	if err != nil {
		return downloader.Task{}, err
	}
	return selectedTask(info, files, binding), nil
}
func (c *Client) ManifestSelected(ctx context.Context, id string, binding downloader.SelectionContext) (downloader.Manifest, error) {
	info, files, err := c.selectedState(ctx, id, binding, true)
	if err != nil {
		return downloader.Manifest{}, err
	}
	selected := selectedPaths(binding)
	result := downloader.Manifest{Name: info.Name, Complete: !strings.Contains(strings.ToLower(info.State), "metadata")}
	for _, file := range files {
		if _, ok := selected[file.Name]; ok {
			index := *file.Index
			result.Files = append(result.Files, downloader.File{NativeIndex: &index, RelativePath: file.Name, Size: file.Size})
		}
	}
	return result, nil
}
func (c *Client) PauseSelected(ctx context.Context, id string, binding downloader.SelectionContext) error {
	if binding.Group != nil {
		return downloader.Error("downloader_selection_group_update_required", false, nil)
	}
	release, err := c.selectionLock(ctx, binding.Selection.Identity)
	if err != nil {
		return err
	}
	defer release()
	info, _, err := c.selectedState(ctx, id, binding, true)
	if err != nil {
		return err
	}
	return c.pauseHash(ctx, info.Hash)
}
func (c *Client) ResumeSelected(ctx context.Context, id string, binding downloader.SelectionContext) error {
	if binding.Group != nil {
		return downloader.Error("downloader_selection_group_update_required", false, nil)
	}
	release, err := c.selectionLock(ctx, binding.Selection.Identity)
	if err != nil {
		return err
	}
	defer release()
	info, _, err := c.selectedState(ctx, id, binding, true)
	if err != nil {
		return err
	}
	return c.resumeHash(ctx, info.Hash)
}
func (c *Client) CancelSelected(ctx context.Context, id string, binding downloader.SelectionContext, deleteData bool) error {
	if binding.Group != nil {
		return downloader.Error("downloader_selection_group_update_required", false, nil)
	}
	release, err := c.selectionLock(ctx, binding.Selection.Identity)
	if err != nil {
		return err
	}
	defer release()
	info, _, err := c.selectedState(ctx, id, binding, true)
	if err != nil {
		return err
	}
	if deleteData {
		return downloader.Error("downloader_selected_data_delete_unsupported", false, nil)
	}
	// Keep the task and verified outputs. Removing a selection is not authority
	// to delete the shared torrent/data; another valid plan may later reuse it.
	return c.pauseHash(ctx, info.Hash)
}

func (c *Client) SetCategorySelected(ctx context.Context, id string, binding downloader.SelectionContext, name, savePath string) error {
	if binding.Group != nil {
		return downloader.Error("downloader_selection_group_workspace_conflict", false, nil)
	}
	release, err := c.selectionLock(ctx, binding.Selection.Identity)
	if err != nil {
		return err
	}
	defer release()
	info, _, err := c.selectedState(ctx, id, binding, true)
	if err != nil {
		return err
	}
	return c.setCategoryHash(ctx, info.Hash, name, savePath)
}
