package pan115offline

import (
	"context"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

// This executor implements one immutable wanted set, never mutable group
// unions. Native receipt lookup is scoped to the original plan and account.
func (c *Client) SupportsSelectiveFiles() bool {
	driver, ok := c.driver.(cloud.SelectiveOfflineDriver)
	return ok && driver.SupportsSelectiveOffline()
}
func (c *Client) selectedAuthority(binding downloader.SelectionContext) cloud.SelectiveOfflineAuthority {
	return cloud.SelectiveOfflineAuthority{Tag: binding.Tag, StorageRootID: c.rootID, Selection: binding.Selection}
}
func selectedTask(id string) bool { return strings.HasPrefix(id, "omc-selected-") }
func selectedAdapterError(err error) error {
	if err == nil {
		return nil
	}
	if code, _ := cloud.ErrorInfo(err); code == cloud.CodeMutationUnknown {
		return downloader.Error("downloader_selection_result_unknown", true, nil)
	}
	return mapError(err)
}
func selectedSnapshot(snapshot cloud.SelectiveOfflineSnapshot, id string, binding downloader.SelectionContext) error {
	if snapshot.Task.ID != id || !selectedTask(id) || snapshot.Name != binding.RootName || snapshot.Complete != snapshot.Task.Completed {
		return downloader.Error("downloader_response_invalid", false, nil)
	}
	if !snapshot.Complete {
		return nil
	}
	if len(snapshot.Files) != len(binding.Selection.Files) {
		return downloader.Error("downloader_response_invalid", false, nil)
	}
	expected := map[string]int64{}
	for _, file := range binding.Selection.Files {
		expected[file.Path] = file.Size
	}
	paths := map[string]bool{}
	indices := map[int]bool{}
	identities := map[string]bool{}
	for _, file := range snapshot.Files {
		size, ok := expected[file.Path]
		if !ok || paths[file.Path] || file.Item.Size != size || file.Item.IsDir || file.Item.ID == "" || file.Item.ParentID == "" || identities[file.Item.ID] || file.Index < 0 || indices[file.Index] {
			return downloader.Error("downloader_response_invalid", false, nil)
		}
		paths[file.Path] = true
		indices[file.Index] = true
		identities[file.Item.ID] = true
	}
	return nil
}

func (c *Client) submitSelected(ctx context.Context, request downloader.SubmitRequest) (downloader.Task, error) {
	if !c.SupportsSelectiveFiles() {
		return downloader.Task{}, downloader.Error("downloader_selective_files_unsupported", false, nil)
	}
	if request.MetadataOnly || !validTaskTag(request.Tag) {
		return downloader.Task{}, downloader.Error("downloader_selection_invalid", false, nil)
	}
	manifest, err := downloader.ValidateContentSource(request.Source)
	if err != nil {
		return downloader.Task{}, err
	}
	if manifest.Identity.V1 == "" || manifest.Identity.V2 != "" {
		return downloader.Task{}, downloader.Error("downloader_selective_files_unsupported", false, nil)
	}
	for _, file := range manifest.Files {
		if file.Padding {
			return downloader.Task{}, downloader.Error("downloader_selective_files_unsupported", false, nil)
		}
	}
	directory := strings.TrimSpace(request.ProviderDirectoryID)
	if directory == "" {
		directory = c.directoryID
	}
	if _, err := c.validateDirectory(ctx, directory); err != nil {
		return downloader.Task{}, err
	}
	snapshot, err := c.driver.(cloud.SelectiveOfflineDriver).SubmitSelectedOffline(ctx, cloud.SelectiveOfflineRequest{Authority: cloud.SelectiveOfflineAuthority{Tag: request.Tag, StorageRootID: c.rootID, Selection: *request.Source.ContentSelection}, ParentID: directory, Torrent: append([]byte(nil), request.Source.Torrent...)})
	if err != nil {
		return downloader.Task{}, selectedAdapterError(err)
	}
	if err = selectedSnapshot(snapshot, snapshot.Task.ID, downloader.SelectionContext{Tag: request.Tag, Selection: *request.Source.ContentSelection, RootName: manifest.Name, MultiFile: manifest.MultiFile}); err != nil {
		return downloader.Task{}, err
	}
	return mapTask(snapshot.Task), nil
}
func (c *Client) GetSelected(ctx context.Context, id string, binding downloader.SelectionContext) (downloader.Task, error) {
	if !c.SupportsSelectiveFiles() || binding.Group != nil {
		return downloader.Task{}, downloader.Error("downloader_selective_files_unsupported", false, nil)
	}
	snapshot, err := c.driver.(cloud.SelectiveOfflineDriver).ObserveSelectedOffline(ctx, id, c.selectedAuthority(binding))
	if err != nil {
		return downloader.Task{}, selectedAdapterError(err)
	}
	if err = selectedSnapshot(snapshot, id, binding); err != nil {
		return downloader.Task{}, err
	}
	return mapTask(snapshot.Task), nil
}
func (c *Client) ManifestSelected(ctx context.Context, id string, binding downloader.SelectionContext) (downloader.Manifest, error) {
	if !c.SupportsSelectiveFiles() || binding.Group != nil {
		return downloader.Manifest{}, downloader.Error("downloader_selective_files_unsupported", false, nil)
	}
	snapshot, err := c.driver.(cloud.SelectiveOfflineDriver).ObserveSelectedOffline(ctx, id, c.selectedAuthority(binding))
	if err != nil {
		return downloader.Manifest{}, selectedAdapterError(err)
	}
	if err = selectedSnapshot(snapshot, id, binding); err != nil {
		return downloader.Manifest{}, err
	}
	manifest := downloader.Manifest{Name: snapshot.Name, Complete: snapshot.Complete}
	if !snapshot.Complete {
		return manifest, nil
	}
	for _, file := range snapshot.Files {
		index := file.Index
		manifest.Files = append(manifest.Files, downloader.File{NativeIndex: &index, RelativePath: file.Path, Size: file.Item.Size, ProviderItemID: file.Item.ID, ProviderParentID: file.Item.ParentID, SHA1: file.Item.SHA1})
	}
	return manifest, nil
}
func (c *Client) PauseSelected(context.Context, string, downloader.SelectionContext) error {
	return downloader.Error("downloader_action_unsupported", false, nil)
}
func (c *Client) ResumeSelected(context.Context, string, downloader.SelectionContext) error {
	return downloader.Error("downloader_action_unsupported", false, nil)
}
func (c *Client) CancelSelected(ctx context.Context, id string, binding downloader.SelectionContext, deleteData bool) error {
	if !c.SupportsSelectiveFiles() || binding.Group != nil || deleteData {
		return downloader.Error("downloader_action_unsupported", false, nil)
	}
	return selectedAdapterError(c.driver.(cloud.SelectiveOfflineDriver).CancelSelectedOffline(ctx, id, c.selectedAuthority(binding), false))
}

var _ downloader.SelectiveFilesClient = (*Client)(nil)
