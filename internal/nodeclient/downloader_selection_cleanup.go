package nodeclient

import (
	"context"
	"encoding/json"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"strconv"
)

func (c *DownloaderClient) CleanupSelectionGroup(ctx context.Context, source downloadpkg.Source, binding downloadpkg.SelectionContext, cleanup downloadpkg.GroupCleanup) (bool, error) {
	if source.ContentSelection == nil || c.groupID != cleanup.Group.ID || binding.Group == nil {
		return false, downloadpkg.Error("downloader_selection_cleanup_invalid", false, nil)
	}
	health, err := c.node.Health(ctx)
	if err != nil {
		return false, remoteDownloaderError(err, "")
	}
	if !health.Capabilities.Has(nodeprotocol.CapabilitySelectiveFilesUnionV1) {
		return false, downloadpkg.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
	}
	cleanup.Group.Workspace = c.remoteTaskRoot()
	group := cleanup.Group
	key := stableOperationKey(nodeprotocol.DownloaderActionSelectionGroupCleanup, c.downloaderID, c.taskID, cleanup.Key, strconv.FormatUint(group.Epoch, 10), strconv.FormatBool(cleanup.ProbeOnly))
	wire := nodeprotocol.DownloaderSource{Kind: source.Kind, URL: source.URL, Torrent: source.Torrent, Filename: source.Filename, ContentSelection: source.ContentSelection}
	input := nodeprotocol.DownloaderActionRequest{RequestID: key, TaskID: c.taskID, OperationKey: key, DownloaderID: c.downloaderID, Action: nodeprotocol.DownloaderActionSelectionGroupCleanup, Group: &group, Cleanup: &cleanup, Submit: &nodeprotocol.DownloaderSubmit{Source: nodeprotocol.DownloaderSource{Kind: source.Kind}, SavePath: group.Workspace, Tag: binding.Tag}}
	envelope, material, err := nodeprotocol.SealDownloaderSource(wire, nodeprotocol.DownloaderSourceBinding{ServerID: c.node.identity.ServerID, TaskID: c.taskID, OperationKey: key, DownloaderID: c.downloaderID, SavePath: group.Workspace, Tag: binding.Tag, ConfigurationScope: c.selectionScope})
	if err != nil {
		return false, err
	}
	input.Submit.SourceEnvelope = envelope
	if raw, err := json.Marshal(input); err != nil || len(raw) > nodeprotocol.MaxWireBodyBytes {
		return false, downloadpkg.Error("downloader_selection_wire_limit", false, err)
	}
	ctx = context.WithValue(ctx, sourceGrantKeyContext{}, sourceGrantValue{key: material, scope: c.selectionScope})
	response, err := c.call(ctx, input)
	if err != nil {
		return false, remoteDownloaderError(err, "")
	}
	return response.Task != nil && response.Task.ID == "cleanup:"+cleanup.Key, nil
}
