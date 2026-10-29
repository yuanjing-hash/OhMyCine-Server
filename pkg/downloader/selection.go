package downloader

import (
	"context"
	"net/url"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

// SelectionContext is private, immutable task authority. Its tag and root
// facts come from the persisted Server plan, never a browser native index.
type SelectionContext struct {
	Group     *SelectionGroup           `json:"group,omitempty"`
	Tag       string                    `json:"tag"`
	Selection resourcecontent.Selection `json:"selection"`
	RootName  string                    `json:"root_name"`
	MultiFile bool                      `json:"multi_file"`
}

type SelectiveFilesClient interface {
	GetSelected(context.Context, string, SelectionContext) (Task, error)
	ManifestSelected(context.Context, string, SelectionContext) (Manifest, error)
	PauseSelected(context.Context, string, SelectionContext) error
	ResumeSelected(context.Context, string, SelectionContext) error
	CancelSelected(context.Context, string, SelectionContext, bool) error
}

// Optional runtime availability, e.g. a native provider whose exact wanted
// mutation requires an injected durable operation store in addition to SDK
// support. Construction/preflight must not submit a provider operation.
type SelectiveFilesAvailability interface{ SupportsSelectiveFiles() bool }

// Optional selected metadata routing. Implementations recheck the exact
// selection authority under their resource lock before moving its workspace.
type SelectiveMetadataClient interface {
	SetCategorySelected(context.Context, string, SelectionContext, string, string) error
}

// ValidateContentSource never fetches metadata or substitutes an entire
// resource. Files mode requires the original metainfo already owned by OMC.
func ValidateContentSource(source Source) (resourcecontent.Manifest, error) {
	if source.ContentSelection == nil {
		return resourcecontent.Manifest{}, Error("downloader_selection_required", false, nil)
	}
	if source.Kind != SourceTorrent && source.Kind != SourceURL {
		return resourcecontent.Manifest{}, Error("downloader_selection_source_invalid", false, nil)
	}
	manifest, err := resourcecontent.ParseTorrent(source.Torrent, resourcecontent.Limits{MaxMetainfoBytes: MaxTorrentBytes})
	if err != nil {
		return resourcecontent.Manifest{}, Error("downloader_selection_metainfo_required", false, err)
	}
	if err = source.ContentSelection.ValidateAgainst(manifest); err != nil {
		return resourcecontent.Manifest{}, Error("downloader_selection_invalid", false, err)
	}
	if err = resourcecontent.ValidateExecutableMetainfo(manifest); err != nil {
		return resourcecontent.Manifest{}, Error("downloader_selection_metainfo_incomplete", false, err)
	}
	if source.Kind == SourceURL {
		u, e := url.Parse(source.URL)
		if e != nil || !strings.EqualFold(u.Scheme, "magnet") {
			return resourcecontent.Manifest{}, Error("downloader_selection_source_invalid", false, nil)
		}
		magnet, e := resourcecontent.ParseMagnet(source.URL)
		if e != nil || manifest.ValidateIdentity(magnet.Identity) != nil {
			return resourcecontent.Manifest{}, Error("downloader_selection_identity_mismatch", false, e)
		}
	}
	return manifest, nil
}

func BindSelection(client Client, source Source, tag string) (Client, error) {
	if source.ContentSelection == nil {
		return client, nil
	}
	manifest, err := ValidateContentSource(source)
	if err != nil {
		return nil, err
	}
	selected, ok := client.(SelectiveFilesClient)
	if !ok {
		return nil, Error("downloader_selective_files_unsupported", false, nil)
	}
	binding := SelectionContext{Tag: tag, Selection: *source.ContentSelection, RootName: manifest.Name, MultiFile: manifest.MultiFile}
	binding.Selection.Files = append([]resourcecontent.SelectedFile(nil), source.ContentSelection.Files...)
	bound := &selectionClient{Client: client, selected: selected, binding: binding}
	if metadata, ok := client.(MetadataClient); ok {
		wrapped := &metadataSelectionClient{selectionClient: bound, metadata: metadata}
		if reader, ok := client.(RemoteFileReader); ok {
			return &selectedRemoteMetadataClient{metadataSelectionClient: wrapped, reader: reader}, nil
		}
		return wrapped, nil
	}
	if reader, ok := client.(RemoteFileReader); ok {
		return &selectedRemoteClient{selectionClient: bound, reader: reader}, nil
	}
	return bound, nil
}

type selectionClient struct {
	Client
	selected SelectiveFilesClient
	binding  SelectionContext
}

func (c *selectionClient) Submit(ctx context.Context, r SubmitRequest) (Task, error) {
	if r.Source.ContentSelection == nil || r.Tag != c.binding.Tag {
		return Task{}, Error("downloader_selection_required", false, nil)
	}
	left, err := r.Source.ContentSelection.Digest()
	right, _ := c.binding.Selection.Digest()
	if err != nil || left != right {
		return Task{}, Error("downloader_selection_invalid", false, err)
	}
	return c.Client.Submit(ctx, r)
}
func (c *selectionClient) Get(ctx context.Context, id string) (Task, error) {
	return c.selected.GetSelected(ctx, id, c.binding)
}
func (c *selectionClient) Manifest(ctx context.Context, id string) (Manifest, error) {
	return c.selected.ManifestSelected(ctx, id, c.binding)
}
func (c *selectionClient) Pause(ctx context.Context, id string) error {
	return c.selected.PauseSelected(ctx, id, c.binding)
}
func (c *selectionClient) Resume(ctx context.Context, id string) error {
	return c.selected.ResumeSelected(ctx, id, c.binding)
}
func (c *selectionClient) Cancel(ctx context.Context, id string, remove bool) error {
	return c.selected.CancelSelected(ctx, id, c.binding, remove)
}

type metadataSelectionClient struct {
	*selectionClient
	metadata MetadataClient
}

func (c *metadataSelectionClient) Categories(ctx context.Context) ([]Category, error) {
	return c.metadata.Categories(ctx)
}
func (c *metadataSelectionClient) EnsureCategory(ctx context.Context, n, p string) error {
	return c.metadata.EnsureCategory(ctx, n, p)
}
func (c *metadataSelectionClient) UpdateCategory(ctx context.Context, n, p string) error {
	return c.metadata.UpdateCategory(ctx, n, p)
}
func (c *metadataSelectionClient) SetCategory(ctx context.Context, id, n, p string) error {
	if selected, ok := c.metadata.(SelectiveMetadataClient); ok {
		return selected.SetCategorySelected(ctx, id, c.binding, n, p)
	}
	if _, err := c.selected.GetSelected(ctx, id, c.binding); err != nil {
		return err
	}
	return c.metadata.SetCategory(ctx, id, n, p)
}
func (c *selectionClient) RetainManagedTag() bool { return true }

type selectedRemoteClient struct {
	*selectionClient
	reader RemoteFileReader
}

func (c *selectedRemoteClient) RemoteFileChunks(ctx context.Context, m Manifest, f File) ([]RemoteFileChunk, error) {
	return c.reader.RemoteFileChunks(ctx, m, f)
}
func (c *selectedRemoteClient) ReadRemoteFileChunk(ctx context.Context, m Manifest, f File, chunk RemoteFileChunk) ([]byte, error) {
	return c.reader.ReadRemoteFileChunk(ctx, m, f, chunk)
}
func (c *selectedRemoteClient) CleanupManagedSource(ctx context.Context) error {
	if cleaner, ok := c.Client.(ManagedSourceCleaner); ok {
		return cleaner.CleanupManagedSource(ctx)
	}
	return nil
}

type selectedRemoteMetadataClient struct {
	*metadataSelectionClient
	reader RemoteFileReader
}

func (c *selectedRemoteMetadataClient) RemoteFileChunks(ctx context.Context, m Manifest, f File) ([]RemoteFileChunk, error) {
	return c.reader.RemoteFileChunks(ctx, m, f)
}
func (c *selectedRemoteMetadataClient) ReadRemoteFileChunk(ctx context.Context, m Manifest, f File, r RemoteFileChunk) ([]byte, error) {
	return c.reader.ReadRemoteFileChunk(ctx, m, f, r)
}
func (c *selectedRemoteMetadataClient) CleanupManagedSource(ctx context.Context) error {
	if cleaner, ok := c.Client.(ManagedSourceCleaner); ok {
		return cleaner.CleanupManagedSource(ctx)
	}
	return nil
}
