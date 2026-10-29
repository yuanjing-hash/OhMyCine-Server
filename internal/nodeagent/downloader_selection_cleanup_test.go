package nodeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

type nodeGroupCleanupProvider struct {
	*selectedOnlyProvider
	deletes, probes int
	gone            bool
}

func (p *nodeGroupCleanupProvider) CleanupSelectionGroup(_ context.Context, _ downloadpkg.Source, _ downloadpkg.SelectionContext, cleanup downloadpkg.GroupCleanup) (bool, error) {
	if cleanup.ProbeOnly {
		p.probes++
		return p.gone, nil
	}
	p.deletes++
	p.gone = true
	return false, downloadpkg.Error("fixture_native_ack_lost", true, nil)
}

func TestNodeGroupCleanupRecoversReceiptAndRemovesOnlyExactDeliveredFiles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "node.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	raw := []byte("d4:infod6:lengthi10e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
	m, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	sel := resourcecontent.Selection{Version: 1, Identity: m.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "movie.mkv", Size: 10}}}
	source := downloadpkg.Source{Kind: downloadpkg.SourceTorrent, Torrent: raw, ContentSelection: &sel}
	id := strings.Repeat("b", 64)
	task := uuid.NewString()
	group := downloadpkg.SelectionGroup{Version: 1, ID: id, Tag: "omc-group-" + id[:32], Workspace: "/downloads/.omc-bt/" + id, Epoch: 1, Revision: 1, Union: sel}
	local := filepath.Join(t.TempDir(), "mount", ".omc-bt", id)
	if err = os.MkdirAll(local, 0700); err != nil {
		t.Fatal(err)
	}
	video, extra := filepath.Join(local, "movie.mkv"), filepath.Join(local, "user-note.txt")
	if err = os.WriteFile(video, make([]byte, 10), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(extra, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	binding := downloadpkg.SelectionContext{Tag: "omc-" + task, Selection: sel, RootName: m.Name, MultiFile: m.MultiFile}
	finish, release, err := store.beginDownloadGroup(ctx, "server", "downloader", task, "scope", local, binding, group)
	if err != nil {
		t.Fatal(err)
	}
	if err = finish("native"); err != nil {
		t.Fatal(err)
	}
	release()
	group.Epoch++
	group.Revision++
	cleanup := downloadpkg.GroupCleanup{Key: strings.Repeat("c", 64), Group: group, Files: sel, DeleteFiles: true}
	input := nodeprotocol.DownloaderActionRequest{TaskID: task, DownloaderID: "downloader", Group: &group, Cleanup: &cleanup, Submit: &nodeprotocol.DownloaderSubmit{Tag: binding.Tag, SavePath: group.Workspace}}
	credential := nodeprotocol.DownloaderCredential{DownloaderSaveRoot: "/downloads", SourceScope: "scope"}
	provider := &nodeGroupCleanupProvider{selectedOnlyProvider: &selectedOnlyProvider{}}
	agent := &Agent{store: store}
	if _, err = agent.executeSelectionGroupCleanup(ctx, "server", input, credential, provider, source, local); err == nil || provider.deletes != 1 {
		t.Fatal("unknown removal not persisted", err, provider.deletes)
	}
	if _, err = os.Stat(video); err != nil {
		t.Fatal("file deleted before native removal was confirmed", err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	agent = &Agent{store: store}
	group.Epoch++
	result, err := agent.executeSelectionGroupCleanup(ctx, "server", input, credential, provider, source, local)
	if err != nil || result.ID != "cleanup:"+cleanup.Key || provider.deletes != 1 || provider.probes != 1 {
		t.Fatal("restart replayed original mutation", result, err, provider.deletes, provider.probes)
	}
	if _, err = os.Stat(video); !os.IsNotExist(err) {
		t.Fatal("Node did not perform real exact file cleanup", err)
	}
	if _, err = os.Stat(extra); err != nil {
		t.Fatal("Node deleted unrelated file", err)
	}
	newRoot := filepath.Join(filepath.Dir(local), strings.Repeat("d", 64))
	if err = os.MkdirAll(newRoot, 0700); err != nil {
		t.Fatal(err)
	}
	newFile := filepath.Join(newRoot, "movie.mkv")
	if err = os.WriteFile(newFile, make([]byte, 10), 0600); err != nil {
		t.Fatal(err)
	}
	newTask := uuid.NewString()
	next := group
	next.ID = strings.Repeat("d", 64)
	next.Tag = "omc-group-" + next.ID[:32]
	next.Workspace = "/downloads/.omc-bt/" + next.ID
	next.Epoch = 1
	next.Revision = 1
	newBinding := binding
	newBinding.Tag = "omc-" + newTask
	finish, release, err = store.beginDownloadGroup(ctx, "server", "downloader", newTask, "scope", newRoot, newBinding, next)
	if err != nil {
		t.Fatal(err)
	}
	if err = finish("native"); err != nil {
		t.Fatal("completed lifecycle blocked same hash reuse", err)
	}
	release()
	group.Epoch++
	if _, err = agent.executeSelectionGroupCleanup(ctx, "server", input, credential, provider, source, local); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(newFile); err != nil || provider.deletes != 1 || provider.probes != 1 {
		t.Fatal("late cleanup affected new lifecycle", err)
	}
	// The old lifecycle cannot be submitted again through the normal union
	// route after its cleanup receipt has been accepted.
	group.Epoch++
	if _, err = agent.executeSelectionGroup(ctx, "server", input, credential, provider, source, local); err == nil {
		t.Fatal("cleaned lifecycle was recreated")
	}
}
