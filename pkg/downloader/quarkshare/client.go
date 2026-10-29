package quarkshare

import (
	"context"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

var Capabilities = downloader.Capabilities{ShareReceive: true, OutputConstraint: downloader.OutputConstraintProviderStorage}

type selectionDriver interface {
	cloud.ShareBrowseDriver
	ResolveManagementItem(context.Context, string) (cloud.Item, error)
	ReceiveSelection(context.Context, string, string, string, string, *cloud.ShareSelection) (quark.SelectionResult, error)
	AdoptSelection(context.Context, string, string, string) (quark.SelectionResult, error)
	GetSelection(context.Context, string) (quark.SelectionResult, error)
}
type Client struct {
	driver          selectionDriver
	root, directory string
}

func New(c downloader.Config) (downloader.Client, error) {
	d, ok := c.ShareDriver.(selectionDriver)
	if !ok || c.ProviderStorageRootID == "" || c.ProviderDirectoryID == "" {
		return nil, downloader.Error("downloader_storage_unavailable", false, nil)
	}
	if locator, ok := d.(interface {
		SetDirectoryLocator(string, string, string, string) error
	}); ok && c.ProviderStorageRootPath != "" {
		if err := locator.SetDirectoryLocator(c.ProviderStorageRootID, c.ProviderStorageRootPath, c.ProviderDirectoryPath, c.ProviderDirectoryID); err != nil {
			return nil, mapError(err)
		}
	}
	return &Client{d, c.ProviderStorageRootID, c.ProviderDirectoryID}, nil
}
func (c *Client) Test(ctx context.Context) (downloader.Health, error) {
	if _, e := c.driver.Probe(ctx); e != nil {
		return downloader.Health{}, mapError(e)
	}
	if e := c.validateTarget(ctx, c.directory); e != nil {
		return downloader.Health{}, mapError(e)
	}
	return downloader.Health{Version: "夸克分享接收"}, nil
}
func (c *Client) Submit(ctx context.Context, r downloader.SubmitRequest) (downloader.Task, error) {
	if r.Source.ContentSelection != nil {
		return downloader.Task{}, downloader.Error("downloader_selective_files_unsupported", false, nil)
	}
	if (r.Source.Kind != downloader.SourceQuarkShare && r.Source.Kind != downloader.SourceProviderItem) || r.MetadataOnly || !strings.HasPrefix(r.Tag, "omc-") || len(r.Tag) > 64 || strings.ContainsAny(r.Tag, "/\\\x00\r\n") {
		return downloader.Task{}, downloader.Error("downloader_source_unsupported", false, nil)
	}
	parent := c.directory
	if r.ProviderDirectoryID != "" {
		parent = r.ProviderDirectoryID
	}
	if e := c.validateTarget(ctx, parent); e != nil {
		return downloader.Task{}, e
	}
	if r.Source.Kind == downloader.SourceProviderItem {
		if r.Source.ProviderItemID == "" || r.Source.URL != "" || r.Source.ShareSelection != nil {
			return downloader.Task{}, downloader.Error("downloader_source_unsupported", false, nil)
		}
		result, e := c.driver.AdoptSelection(ctx, r.Tag, parent, r.Source.ProviderItemID)
		if e != nil {
			return downloader.Task{}, mapError(e)
		}
		return task(r.Tag, result), nil
	}
	// The service freezes the canonical scoped intake root in the task. Provider
	// mapping independently revalidates the common-space proof before mutation.
	result, e := c.driver.ReceiveSelection(ctx, r.Tag, r.Source.URL, parent, r.Tag, r.Source.ShareSelection)
	if e != nil {
		return downloader.Task{}, mapError(e)
	}
	return task(r.Tag, result), nil
}

func (c *Client) validateTarget(ctx context.Context, id string) error {
	if _, e := c.driver.ResolveManagementItem(ctx, id); e != nil {
		return mapError(e)
	}
	seen := map[string]bool{}
	for i := 0; i < 128; i++ {
		item, e := c.driver.Stat(ctx, id)
		if e != nil {
			return mapError(e)
		}
		if item.ID != id || !item.IsDir || seen[id] {
			return downloader.Error("downloader_storage_unavailable", false, nil)
		}
		if id == c.root {
			return nil
		}
		seen[id] = true
		id = item.ParentID
		if id == "" {
			break
		}
	}
	return downloader.Error("downloader_storage_unavailable", false, nil)
}
func (c *Client) Get(ctx context.Context, id string) (downloader.Task, error) {
	r, e := c.driver.GetSelection(ctx, id)
	if e != nil {
		return downloader.Task{}, mapError(e)
	}
	return task(id, r), nil
}
func task(id string, r quark.SelectionResult) downloader.Task {
	return downloader.Task{ID: id, Name: r.Root.Name, Status: "completed", Completed: r.Complete, OutputItemID: r.Root.ID}
}
func (c *Client) Manifest(ctx context.Context, id string) (downloader.Manifest, error) {
	r, e := c.driver.GetSelection(ctx, id)
	if e != nil {
		return downloader.Manifest{}, mapError(e)
	}
	m := downloader.Manifest{Name: r.Root.Name, Complete: r.Complete}
	for _, f := range r.Entries {
		m.Files = append(m.Files, downloader.File{RelativePath: f.RelativePath, Size: f.Size, ProviderItemID: f.ID, ProviderParentID: f.ParentID, SHA1: f.SHA1})
	}
	return m, nil
}
func (c *Client) Cancel(context.Context, string, bool) error {
	return downloader.Error("downloader_cancel_unsupported", false, nil)
}
func mapError(e error) error {
	code, retry := cloud.ErrorInfo(e)
	switch code {
	case quark.CodeAuth:
		code = "downloader_auth_failed"
	case quark.CodeLimited:
		code = "downloader_rate_limited"
	case quark.CodePending:
		code = "downloader_share_result_unknown"
		retry = true
	case cloud.CodeMutationUnknown:
		code = "downloader_submit_unknown"
		retry = false
	}
	return downloader.Error(code, retry, nil)
}

var _ downloader.ManifestClient = (*Client)(nil)
