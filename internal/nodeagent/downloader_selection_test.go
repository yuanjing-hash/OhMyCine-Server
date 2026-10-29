package nodeagent

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

// Nil ordinary implementations make a whole-package fallback fail loudly.
type selectedOnlyProvider struct {
	downloadpkg.Client
	downloadpkg.MetadataClient
	getCalls, manifestCalls, routeCalls int
}

func (c *selectedOnlyProvider) GetSelected(_ context.Context, _ string, b downloadpkg.SelectionContext) (downloadpkg.Task, error) {
	c.getCalls++
	size := b.Selection.Files[0].Size
	return downloadpkg.Task{ID: "native", BytesTotal: &size}, nil
}
func (c *selectedOnlyProvider) ManifestSelected(_ context.Context, _ string, b downloadpkg.SelectionContext) (downloadpkg.Manifest, error) {
	c.manifestCalls++
	return downloadpkg.Manifest{Files: []downloadpkg.File{{RelativePath: b.RootName + "/" + b.Selection.Files[0].Path, Size: b.Selection.Files[0].Size}}}, nil
}
func (c *selectedOnlyProvider) PauseSelected(context.Context, string, downloadpkg.SelectionContext) error {
	return nil
}
func (c *selectedOnlyProvider) ResumeSelected(context.Context, string, downloadpkg.SelectionContext) error {
	return nil
}
func (c *selectedOnlyProvider) CancelSelected(context.Context, string, downloadpkg.SelectionContext, bool) error {
	return nil
}

func (c *selectedOnlyProvider) SetCategorySelected(_ context.Context, id string, binding downloadpkg.SelectionContext, name, savePath string) error {
	if id != "native" || binding.RootName != "Pack" || len(binding.Selection.Files) != 1 || name != "tv" || savePath != "/managed/tv" {
		return downloadpkg.Error("test_invalid_selected_routing", false, nil)
	}
	c.routeCalls++
	return nil
}

func TestManagedSelectedFacadeKeepsSubsetAndRejectsOtherTask(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	taskID := uuid.NewString()
	binding := downloadpkg.SelectionContext{Tag: "omc-" + taskID, RootName: "Pack", MultiFile: true, Selection: resourcecontent.Selection{Version: 1, Identity: resourcecontent.Identity{V1: "0123456789abcdef0123456789abcdef01234567"}, Files: []resourcecontent.SelectedFile{{Index: 7, Path: "episode.mkv", Size: 10}}}}
	if err = store.saveDownloadSelection(ctx, "server", "downloader", taskID, "native", binding); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveManagedDownload(ctx, ManagedDownload{ServerID: "server", DownloaderID: "downloader", TaskID: taskID, ProviderTaskID: "native", Tag: binding.Tag, NodeLocalRoot: "/managed", DownloaderSavePath: "/downloads"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{store: store}
	provider := &selectedOnlyProvider{}
	input := nodeprotocol.DownloaderActionRequest{TaskID: taskID, DownloaderID: "downloader", ProviderTaskID: "native"}
	// Even a later request omitting selection cannot downgrade a persisted
	// selected task into the old whole-package get/manifest path.
	bound, err := agent.bindManagedSelection(ctx, "server", input, provider)
	if err != nil {
		t.Fatal(err)
	}
	task, err := bound.Get(ctx, "native")
	if err != nil || *task.BytesTotal != 10 || provider.getCalls != 1 {
		t.Fatal(task, err)
	}
	manifest, err := bound.(downloadpkg.ManifestClient).Manifest(ctx, "native")
	if err != nil || len(manifest.Files) != 1 || provider.manifestCalls != 1 {
		t.Fatal(manifest, err)
	}
	if err = bound.(downloadpkg.MetadataClient).SetCategory(ctx, "native", "tv", "/managed/tv"); err != nil || provider.routeCalls != 1 {
		t.Fatal("selected category did not retain its exact authority", err, provider.routeCalls)
	}
	input.TaskID = uuid.NewString()
	if _, err = agent.bindManagedSelection(ctx, "server", input, provider); err == nil {
		t.Fatal("other task adopted selection")
	}
	input.TaskID = taskID
	input.Selection = &nodeprotocol.DownloaderSelection{Tag: binding.Tag, Selection: binding.Selection, RootName: "Wrong", MultiFile: true}
	if _, err = agent.bindManagedSelection(ctx, "server", input, provider); err == nil {
		t.Fatal("changed root binding adopted")
	}
}
