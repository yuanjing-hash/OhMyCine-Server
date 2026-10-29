package nodeclient

import (
	"context"
	"encoding/json"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type sourceGrantKeyContext struct{}
type sourceGrantValue struct{ key, scope string }

// DownloaderSourceGrantKey is consumed only by the trusted sealed-grant
// factory. It must never be logged or copied into an action/receipt DTO.
func DownloaderSourceGrantKey(ctx context.Context) string {
	value, _ := ctx.Value(sourceGrantKeyContext{}).(sourceGrantValue)
	return value.key
}
func DownloaderSourceGrantScope(ctx context.Context) string {
	value, _ := ctx.Value(sourceGrantKeyContext{}).(sourceGrantValue)
	return value.scope
}

func (c *DownloaderClient) selectedCall(ctx context.Context, action, id string, binding downloadpkg.SelectionContext, remove bool) (nodeprotocol.DownloaderActionResponse, error) {
	input := c.request(action, c.boundTaskID(), id, action == nodeprotocol.DownloaderActionResume || action == nodeprotocol.DownloaderActionPause || action == nodeprotocol.DownloaderActionCancel)
	input.Selection = &nodeprotocol.DownloaderSelection{Tag: binding.Tag, Selection: binding.Selection, RootName: binding.RootName, MultiFile: binding.MultiFile}
	if binding.Group != nil {
		group := *binding.Group
		group.Workspace = c.remoteTaskRoot()
		input.Selection.Group = &group
	}
	input.DeleteData = remove
	if action == nodeprotocol.DownloaderActionManifest {
		input.OperationKey = stableOperationKey("file_export", c.downloaderID, c.boundTaskID(), id)
	}
	if raw, err := json.Marshal(input); err != nil || len(raw) > nodeprotocol.MaxWireBodyBytes {
		return nodeprotocol.DownloaderActionResponse{}, downloadpkg.Error("downloader_selection_wire_limit", false, err)
	}
	return c.call(ctx, input)
}
func (c *DownloaderClient) GetSelected(ctx context.Context, id string, b downloadpkg.SelectionContext) (downloadpkg.Task, error) {
	r, e := c.selectedCall(ctx, nodeprotocol.DownloaderActionGet, id, b, false)
	if e != nil || r.Task == nil {
		return downloadpkg.Task{}, remoteDownloaderError(e, "downloader_response_invalid")
	}
	return localDownloaderTask(*r.Task), nil
}
func (c *DownloaderClient) ManifestSelected(ctx context.Context, id string, b downloadpkg.SelectionContext) (downloadpkg.Manifest, error) {
	r, e := c.selectedCall(ctx, nodeprotocol.DownloaderActionManifest, id, b, false)
	if e != nil || r.Manifest == nil {
		return downloadpkg.Manifest{}, remoteDownloaderError(e, "downloader_manifest_invalid")
	}
	if r.FileExport != nil {
		return c.fileExportManifest(ctx, *r.FileExport, stableOperationKey("file_export", c.downloaderID, c.boundTaskID(), id))
	}
	out := downloadpkg.Manifest{Name: r.Manifest.Name, Complete: r.Manifest.Complete}
	for _, f := range r.Manifest.Files {
		out.Files = append(out.Files, downloadpkg.File{NativeIndex: f.NativeIndex, RelativePath: f.RelativePath, Size: f.Size})
	}
	return out, nil
}
func (c *DownloaderClient) PauseSelected(ctx context.Context, id string, b downloadpkg.SelectionContext) error {
	_, e := c.selectedCall(ctx, nodeprotocol.DownloaderActionPause, id, b, false)
	return remoteDownloaderError(e, "")
}
func (c *DownloaderClient) ResumeSelected(ctx context.Context, id string, b downloadpkg.SelectionContext) error {
	_, e := c.selectedCall(ctx, nodeprotocol.DownloaderActionResume, id, b, false)
	return remoteDownloaderError(e, "")
}
func (c *DownloaderClient) CancelSelected(ctx context.Context, id string, b downloadpkg.SelectionContext, remove bool) error {
	_, e := c.selectedCall(ctx, nodeprotocol.DownloaderActionCancel, id, b, remove)
	return remoteDownloaderError(e, "")
}
