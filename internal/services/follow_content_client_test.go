package services

import (
	"context"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

// A deterministic executor fixture for the full Follow -> files mode ->
// completion -> Transfer boundary. It reports the same original file facts.
type followSelectedCompletedClient struct {
	*metadataDownloadClient
	manifest downloadpkg.Manifest
}

func (c *followSelectedCompletedClient) Manifest(context.Context, string) (downloadpkg.Manifest, error) {
	return c.manifest, nil
}
func (c *followSelectedCompletedClient) ManifestSelected(_ context.Context, _ string, selection downloadpkg.SelectionContext) (downloadpkg.Manifest, error) {
	if len(selection.Selection.Files) != 1 || selection.Selection.Files[0].Index != 0 || selection.Selection.Files[0].Path != "Fixture.Show.S01E02.1080p.mkv" || selection.Selection.Files[0].Size != 32<<20 {
		return downloadpkg.Manifest{}, downloadpkg.Error("fixture_selection_mismatch", false, nil)
	}
	return c.manifest, nil
}
func (c *followSelectedCompletedClient) GetSelected(ctx context.Context, id string, _ downloadpkg.SelectionContext) (downloadpkg.Task, error) {
	return c.Get(ctx, id)
}
func (c *followSelectedCompletedClient) PauseSelected(ctx context.Context, id string, _ downloadpkg.SelectionContext) error {
	return c.Pause(ctx, id)
}
func (c *followSelectedCompletedClient) ResumeSelected(ctx context.Context, id string, _ downloadpkg.SelectionContext) error {
	return c.Resume(ctx, id)
}
func (c *followSelectedCompletedClient) CancelSelected(ctx context.Context, id string, _ downloadpkg.SelectionContext, remove bool) error {
	return c.Cancel(ctx, id, remove)
}
