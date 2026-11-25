package pan115offline

import (
	"context"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func (c *Client) AbandonPreparation(ctx context.Context, request downloader.SubmitRequest, deleteData bool) (bool, error) {
	if !validTaskTag(request.Tag) || request.MetadataOnly {
		return false, downloader.Error("downloader_source_invalid", false, nil)
	}
	directory := strings.TrimSpace(request.ProviderDirectoryID)
	if request.Source.ContentSelection != nil {
		if directory == "" {
			return false, downloader.Error("downloader_source_invalid", false, nil)
		}
		if _, err := downloader.ValidateContentSource(request.Source); err != nil {
			return false, err
		}
		controller, ok := c.driver.(cloud.SelectiveOfflinePreparationController)
		if !ok {
			return false, nil
		}
		prepared, err := controller.AbandonSelectedOfflinePreparation(ctx, cloud.SelectiveOfflineRequest{Authority: cloud.SelectiveOfflineAuthority{Tag: request.Tag, StorageRootID: c.rootID, Selection: *request.Source.ContentSelection}, ParentID: directory, Torrent: request.Source.Torrent}, deleteData)
		return prepared, selectedAdapterError(err)
	}
	if request.Source.Kind != downloader.SourcePan115Share {
		return false, nil
	}
	// Share receive never dispatches a long-lived native offline acquisition.
	// Once the submit worker has returned, cancellation needs no cloud polling.
	if !deleteData {
		return true, nil
	}
	if directory == "" {
		return false, downloader.Error("downloader_source_invalid", false, nil)
	}
	mutations, ok := c.driver.(cloud.MutationDriver)
	if !ok {
		return false, downloader.Error("downloader_action_unsupported", false, nil)
	}
	if _, err := c.validateDirectory(ctx, directory); err != nil {
		return false, err
	}
	workspace, found, err := c.findTaskDirectory(ctx, directory, request.Tag)
	if err != nil || !found {
		return err == nil, err
	}
	// The caller supplies the original frozen task source and parent, never
	// today's downloader default. A same-named sibling is not a cleanup target.
	actual, err := c.itemWithinDirectory(ctx, workspace.ID, directory)
	if err != nil {
		return false, err
	}
	if !actual.IsDir || actual.ParentID != directory || actual.Name != request.Tag || actual.ID == directory {
		return false, downloader.Error("downloader_response_invalid", false, nil)
	}
	if err := mutations.Recycle(ctx, workspace.ID); err != nil {
		return false, mapError(err)
	}
	return true, nil
}

var _ downloader.PreparationController = (*Client)(nil)

func (c *Client) ReleasePreparation(ctx context.Context, request downloader.SubmitRequest) error {
	if !validTaskTag(request.Tag) || request.MetadataOnly {
		return downloader.Error("downloader_source_invalid", false, nil)
	}
	if request.Source.ContentSelection == nil {
		// Share receives have no native resource claim.
		if request.Source.Kind == downloader.SourcePan115Share {
			return nil
		}
		return downloader.Error("downloader_action_unsupported", false, nil)
	}
	if _, err := downloader.ValidateContentSource(request.Source); err != nil {
		return err
	}
	releaser, ok := c.driver.(cloud.SelectiveOfflinePreparationReleaser)
	if !ok {
		return downloader.Error("downloader_action_unsupported", false, nil)
	}
	directory := strings.TrimSpace(request.ProviderDirectoryID)
	if directory == "" {
		return downloader.Error("downloader_source_invalid", false, nil)
	}
	return selectedAdapterError(releaser.ReleaseSelectedOfflinePreparation(ctx, cloud.SelectiveOfflineRequest{Authority: cloud.SelectiveOfflineAuthority{Tag: request.Tag, StorageRootID: c.rootID, Selection: *request.Source.ContentSelection}, ParentID: directory, Torrent: request.Source.Torrent}))
}

var _ downloader.PreparationReleaser = (*Client)(nil)
