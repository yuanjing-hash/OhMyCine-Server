package nodeclient

import (
	"context"
	"encoding/json"
	"strconv"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func (c *DownloaderClient) SetSelectionGroupWorkspace(id string) error {
	if !nodeprotocol.ValidDigest(id) {
		return downloadpkg.Error("downloader_selection_group_invalid", false, nil)
	}
	c.groupID = id
	return nil
}

func (c *DownloaderClient) ApplySelectionGroup(ctx context.Context, request downloadpkg.SubmitRequest, group downloadpkg.SelectionGroup) (downloadpkg.Task, error) {
	if request.Source.ContentSelection == nil || c.groupID != group.ID || request.SavePath != c.localRoot || request.Tag != "omc-"+c.taskID {
		return downloadpkg.Task{}, downloadpkg.Error("downloader_selection_group_invalid", false, nil)
	}
	if _, err := downloadpkg.ValidateContentSource(request.Source); err != nil {
		return downloadpkg.Task{}, err
	}
	if err := group.Validate(*request.Source.ContentSelection); err != nil {
		return downloadpkg.Task{}, err
	}
	health, err := c.node.Health(ctx)
	if err != nil {
		return downloadpkg.Task{}, remoteDownloaderError(err, "")
	}
	if !health.Capabilities.Has(nodeprotocol.CapabilitySelectiveFilesUnionV1) {
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
	}
	group.Workspace = c.remoteTaskRoot()
	digest, _ := group.Union.Digest()
	key := stableOperationKey(nodeprotocol.DownloaderActionSelectionGroup, c.downloaderID, c.taskID, group.ID, strconv.FormatUint(group.Epoch, 10), strconv.FormatUint(group.Revision, 10), digest, strconv.FormatBool(group.Resume), strconv.FormatBool(group.Empty))
	source := nodeprotocol.DownloaderSource{Kind: request.Source.Kind, URL: request.Source.URL, Torrent: request.Source.Torrent, Filename: request.Source.Filename, ContentSelection: request.Source.ContentSelection}
	input := nodeprotocol.DownloaderActionRequest{RequestID: key, TaskID: c.taskID, OperationKey: key, DownloaderID: c.downloaderID, Action: nodeprotocol.DownloaderActionSelectionGroup, Group: &group, Submit: &nodeprotocol.DownloaderSubmit{Source: source, SavePath: group.Workspace, Tag: request.Tag}}
	envelope, keyMaterial, err := nodeprotocol.SealDownloaderSource(source, nodeprotocol.DownloaderSourceBinding{ServerID: c.node.identity.ServerID, TaskID: input.TaskID, OperationKey: key, DownloaderID: c.downloaderID, SavePath: group.Workspace, Tag: request.Tag, ConfigurationScope: c.selectionScope})
	if err != nil {
		return downloadpkg.Task{}, err
	}
	input.Submit.SourceEnvelope = envelope
	input.Submit.Source = nodeprotocol.DownloaderSource{Kind: source.Kind}
	if raw, err := json.Marshal(input); err != nil || len(raw) > nodeprotocol.MaxWireBodyBytes {
		return downloadpkg.Task{}, downloadpkg.Error("downloader_selection_wire_limit", false, err)
	}
	ctx = context.WithValue(ctx, sourceGrantKeyContext{}, sourceGrantValue{key: keyMaterial, scope: c.selectionScope})
	response, err := c.call(ctx, input)
	if err != nil || response.Task == nil {
		return downloadpkg.Task{}, remoteDownloaderError(err, "downloader_response_invalid")
	}
	return localDownloaderTask(*response.Task), nil
}
