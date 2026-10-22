package quarkshare

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"testing"
)

type receiverFixture struct {
	cloud.ShareBrowseDriver
	received, adopted int
	result            quark.SelectionResult
}

func (d *receiverFixture) Probe(context.Context) (cloud.Account, error) {
	return cloud.Account{ID: "quark-test"}, nil
}
func (d *receiverFixture) Stat(_ context.Context, id string) (cloud.Item, error) {
	parent := "root"
	if id == "root" || id == "outside" {
		parent = "0"
	}
	if id == "0" {
		parent = ""
	}
	return cloud.Item{ID: id, ParentID: parent, IsDir: true}, nil
}
func (d *receiverFixture) ResolveManagementItem(ctx context.Context, id string) (cloud.Item, error) {
	return d.Stat(ctx, id)
}
func (d *receiverFixture) ReceiveSelection(_ context.Context, _ string, _ string, _ string, _ string, _ *cloud.ShareSelection) (quark.SelectionResult, error) {
	d.received++
	return d.result, nil
}
func (d *receiverFixture) AdoptSelection(_ context.Context, _ string, _ string, _ string) (quark.SelectionResult, error) {
	d.adopted++
	return d.result, nil
}
func (d *receiverFixture) GetSelection(context.Context, string) (quark.SelectionResult, error) {
	return d.result, nil
}

func TestReceiverRejectsOtherSourcesAndOutsideFrozenRoot(t *testing.T) {
	d := &receiverFixture{result: quark.SelectionResult{Root: cloud.Item{ID: "result", Name: "omc-task"}, Entries: []cloud.TreeEntry{{Item: cloud.Item{ID: "canonical-tv", Name: "one.mkv", Size: 10}, RelativePath: "one.mkv"}}, Complete: true}}
	built, e := New(downloader.Config{ShareDriver: d, ProviderStorageRootID: "root", ProviderDirectoryID: "intake"})
	if e != nil {
		t.Fatal(e)
	}
	c := built.(*Client)
	ctx := context.Background()
	for _, kind := range []string{downloader.SourceURL, downloader.SourceTorrent, downloader.SourcePan115Share} {
		if _, e = c.Submit(ctx, downloader.SubmitRequest{Tag: "omc-task", Source: downloader.Source{Kind: kind}}); e == nil {
			t.Fatal("other source accepted")
		}
	}
	if _, e = c.Submit(ctx, downloader.SubmitRequest{Tag: "omc-task", ProviderDirectoryID: "outside", Source: downloader.Source{Kind: downloader.SourceQuarkShare}}); e == nil {
		t.Fatal("outside storage accepted")
	}
	if d.received != 0 || d.adopted != 0 {
		t.Fatal("rejected request mutated provider")
	}
	task, e := c.Submit(ctx, downloader.SubmitRequest{Tag: "omc-task", Source: downloader.Source{Kind: downloader.SourceQuarkShare, URL: "https://pan.quark.cn/s/example"}})
	if e != nil || !task.Completed || d.received != 1 {
		t.Fatal("share receive failed", e)
	}
	task, e = c.Submit(ctx, downloader.SubmitRequest{Tag: "omc-adopt", Source: downloader.Source{Kind: downloader.SourceProviderItem, ProviderItemID: "existing"}})
	if e != nil || !task.Completed || d.received != 1 || d.adopted != 1 {
		t.Fatal("intake submitted another share", e)
	}
	manifest, e := c.Manifest(ctx, "omc-task")
	if e != nil || len(manifest.Files) != 1 || manifest.Files[0].ProviderItemID != "canonical-tv" || !manifest.Complete {
		t.Fatal("manifest identity lost")
	}
	if Capabilities.NativeOffline || Capabilities.Seeding || Capabilities.DeleteData || !Capabilities.ShareReceive {
		t.Fatal("unsupported capabilities advertised")
	}
}
