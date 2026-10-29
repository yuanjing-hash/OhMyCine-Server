package nodeagent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestNodeSelectionGroupRestartEpochAndImmutableOwnReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	identity := resourcecontent.Identity{V1: strings.Repeat("a", 40)}
	a := downloadpkg.SelectionContext{Tag: "omc-task-a", RootName: "Pack", MultiFile: true, Selection: resourcecontent.Selection{Version: 1, Identity: identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "a.mkv", Size: 10}}}}
	b := a
	b.Tag = "omc-task-b"
	b.Selection = resourcecontent.Selection{Version: 1, Identity: identity, Files: []resourcecontent.SelectedFile{{Index: 1, Path: "b.mkv", Size: 20}}}
	id := strings.Repeat("b", 64)
	g := downloadpkg.SelectionGroup{Version: 1, ID: id, Tag: "omc-group-" + id[:32], Workspace: "/downloads/.omc-bt/" + id, Epoch: 1, Revision: 1, Union: a.Selection}
	finish, release, err := store.beginDownloadGroup(ctx, "server", "downloader", "task-a", "scope", "/local/group", a, g)
	if err != nil {
		t.Fatal(err)
	}
	if err = finish("native"); err != nil {
		t.Fatal(err)
	}
	release()
	old := g
	g.Epoch++
	g.Revision++
	g.Union.Files = append(append([]resourcecontent.SelectedFile(nil), a.Selection.Files...), b.Selection.Files...)
	g.Resume = true
	finish, release, err = store.beginDownloadGroup(ctx, "server", "downloader", "task-b", "scope", "/local/group", b, g)
	if err != nil {
		t.Fatal(err)
	}
	if err = finish("native"); err != nil {
		t.Fatal(err)
	}
	release()
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, binding, err := store.managedGroupDownload(ctx, "server", "downloader", "task-a", "native")
	if err != nil || len(binding.Selection.Files) != 1 || len(binding.Group.Union.Files) != 2 {
		t.Fatal(binding, err)
	}
	if _, _, err = store.beginDownloadGroup(ctx, "server", "downloader", "task-a", "scope", "/local/group", a, old); !errors.Is(err, errDownloadGroupStale) {
		t.Fatal("stale epoch accepted", err)
	}
	changed := g
	changed.Union = a.Selection
	if _, _, err = store.beginDownloadGroup(ctx, "server", "downloader", "task-a", "scope", "/local/group", a, changed); !errors.Is(err, ErrPlanConflict) {
		t.Fatal("same epoch different union accepted", err)
	}
	agent := &Agent{store: store}
	provider := &selectedOnlyProvider{}
	input := nodeprotocol.DownloaderActionRequest{TaskID: "foreign", DownloaderID: "downloader", ProviderTaskID: "native"}
	if _, err = agent.bindManagedSelection(ctx, "server", input, provider); err == nil {
		t.Fatal("foreign task acquired shared provider")
	}
	input.TaskID = "task-a"
	bound, err := agent.bindManagedSelection(ctx, "server", input, provider)
	if err != nil {
		t.Fatal(err)
	}
	task, err := bound.Get(ctx, "native")
	if err != nil || *task.BytesTotal != 10 {
		t.Fatal(task, err)
	}
	if err = bound.Cancel(ctx, "native", false); err == nil {
		t.Fatal("per-ref cancel directly mutated shared torrent")
	}
	if _, _, err = store.beginDownloadGroup(ctx, "server", "downloader", "task-a", "new-scope", "/local/group", a, g); !errors.Is(err, ErrPlanConflict) {
		t.Fatal("config drift accepted", err)
	}
}

func TestNodeSelectionGroupLeaseCancellationAndLateOwnerFence(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	id := strings.Repeat("b", 64)
	sel := resourcecontent.Selection{Version: 1, Identity: resourcecontent.Identity{V1: strings.Repeat("a", 40)}, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "a.mkv", Size: 10}}}
	binding := downloadpkg.SelectionContext{Tag: "omc-task", Selection: sel, RootName: "Pack", MultiFile: true}
	g := downloadpkg.SelectionGroup{Version: 1, ID: id, Tag: "omc-group-" + id[:32], Workspace: "/managed/" + id, Epoch: 1, Revision: 1, Union: sel}
	finish, release, err := store.beginDownloadGroup(ctx, "server", "downloader", "task", "scope", "/local", binding, g)
	if err != nil {
		t.Fatal(err)
	}
	fenced, err := store.downloadGroupFence(ctx, "server", "downloader", g)
	if err != nil {
		t.Fatal(err)
	}
	next := g
	next.Epoch++
	waiting, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if _, _, err = store.beginDownloadGroup(waiting, "server", "downloader", "task", "scope", "/local", binding, next); err == nil {
		t.Fatal("second epoch bypassed physical lease")
	}
	if err = downloadpkg.CheckSelectionFence(fenced); err != nil {
		t.Fatal("cancelled waiter displaced owner", err)
	}
	release()
	nextFinish, nextRelease, err := store.beginDownloadGroup(ctx, "server", "downloader", "task", "scope", "/local", binding, next)
	if err != nil {
		t.Fatal(err)
	}
	defer nextRelease()
	if err = downloadpkg.CheckSelectionFence(fenced); err == nil {
		t.Fatal("old owner retained mutation authority")
	}
	if err = finish("old"); err == nil {
		t.Fatal("old owner rewrote native receipt")
	}
	if err = nextFinish("native"); err != nil {
		t.Fatal(err)
	}
}
