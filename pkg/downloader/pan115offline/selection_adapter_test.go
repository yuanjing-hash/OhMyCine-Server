package pan115offline

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"strings"
	"testing"
)

type selectedCloudFixture struct {
	*fakeShareDriver
	supported                                bool
	submissions, observations, cancellations int
	request                                  cloud.SelectiveOfflineRequest
	result                                   cloud.SelectiveOfflineSnapshot
}

func (f *selectedCloudFixture) SupportsSelectiveOffline() bool { return f.supported }
func (f *selectedCloudFixture) SubmitSelectedOffline(_ context.Context, r cloud.SelectiveOfflineRequest) (cloud.SelectiveOfflineSnapshot, error) {
	f.submissions++
	f.request = r
	return f.result, nil
}
func (f *selectedCloudFixture) ObserveSelectedOffline(_ context.Context, id string, a cloud.SelectiveOfflineAuthority) (cloud.SelectiveOfflineSnapshot, error) {
	f.observations++
	x, _ := a.Selection.Digest()
	y, _ := f.request.Authority.Selection.Digest()
	if id != f.result.Task.ID || a.Tag != f.request.Authority.Tag || a.StorageRootID != "root" || x != y {
		return cloud.SelectiveOfflineSnapshot{}, cloud.Error(cloud.CodeResponseInvalid, false, nil)
	}
	return f.result, nil
}
func (f *selectedCloudFixture) CancelSelectedOffline(ctx context.Context, id string, a cloud.SelectiveOfflineAuthority, remove bool) error {
	if remove {
		return cloud.Error(cloud.CodeUnavailable, false, nil)
	}
	if _, err := f.ObserveSelectedOffline(ctx, id, a); err != nil {
		return err
	}
	f.cancellations++
	return nil
}
func selectedDownloadSource(t *testing.T) downloader.Source {
	t.Helper()
	raw := []byte("d4:infod5:filesld6:lengthi10e4:pathl5:a.mkveed6:lengthi20e4:pathl5:b.mkveee4:name4:Show12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
	m, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	selection := resourcecontent.Selection{Version: 1, Identity: m.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "a.mkv", Size: 10}}}
	return downloader.Source{Kind: downloader.SourceTorrent, Filename: "source.torrent", Torrent: raw, ContentSelection: &selection}
}
func TestSelectedAdapterUsesOptionalDriverAndOwnManifestThroughoutBinding(t *testing.T) {
	driver := &selectedCloudFixture{fakeShareDriver: newFakeShareDriver(), supported: true, result: cloud.SelectiveOfflineSnapshot{Task: cloud.OfflineTask{ID: "omc-selected-" + strings.Repeat("a", 64), Completed: true, Status: "completed"}, Name: "Show", Complete: true, Files: []cloud.SelectiveOfflineFile{{Index: 7, Path: "a.mkv", Item: cloud.Item{ID: "actual-a", ParentID: "own-output", Name: "a.mkv", Size: 10}}}}}
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "intake"})
	if err != nil {
		t.Fatal(err)
	}
	source := selectedDownloadSource(t)
	bound, err := downloader.BindSelection(client, source, "omc-plan")
	if err != nil {
		t.Fatal(err)
	}
	task, err := bound.Submit(context.Background(), downloader.SubmitRequest{Source: source, Tag: "omc-plan", ProviderDirectoryID: "intake"})
	if err != nil {
		t.Fatal(err)
	}
	if driver.submissions != 1 || driver.submittedURI != "" || driver.request.ParentID != "intake" || driver.request.Authority.StorageRootID != "root" {
		t.Fatal("selected source fell back or lost scope")
	}
	if _, err = bound.Get(context.Background(), task.ID); err != nil {
		t.Fatal(err)
	}
	manifest, err := bound.(downloader.ManifestClient).Manifest(context.Background(), task.ID)
	if err != nil || !manifest.Complete || len(manifest.Files) != 1 || manifest.Files[0].ProviderItemID != "actual-a" || manifest.Files[0].NativeIndex == nil || *manifest.Files[0].NativeIndex != 7 {
		t.Fatal("own output lost", err)
	}
	if err = client.Cancel(context.Background(), task.ID, false); err == nil {
		t.Fatal("unbound selected cancel accepted")
	}
	if err = bound.Cancel(context.Background(), task.ID, true); err == nil {
		t.Fatal("selected delete-data accepted")
	}
	if err = bound.Cancel(context.Background(), task.ID, false); err != nil || driver.cancellations != 1 {
		t.Fatal("selected cancel", err)
	}
	if Capabilities.SelectiveFileGroups || Capabilities.RemoteSelectiveFilesCapability != "" || !Capabilities.SelectiveFiles {
		t.Fatal("unsupported group/Node capability advertised")
	}
}
func TestSelectedAdapterRejectsMissingLedgerAndGroupBeforeProviderWrite(t *testing.T) {
	driver := &selectedCloudFixture{fakeShareDriver: newFakeShareDriver()}
	client, _ := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "intake"})
	source := selectedDownloadSource(t)
	if _, err := client.Submit(context.Background(), downloader.SubmitRequest{Source: source, Tag: "omc-plan"}); err == nil || driver.submissions != 0 {
		t.Fatal("missing ledger wrote")
	}
	driver.supported = true
	selection := *source.ContentSelection
	_, err := client.(downloader.SelectiveFilesClient).GetSelected(context.Background(), "id", downloader.SelectionContext{Tag: "omc-plan", Selection: selection, Group: &downloader.SelectionGroup{}})
	if err == nil || driver.observations != 0 {
		t.Fatal("group support invented")
	}
}

func TestSelectedAdapterRejectsForeignFilesDespiteDriverCompleteClaim(t *testing.T) {
	source := selectedDownloadSource(t)
	binding := downloader.SelectionContext{Tag: "omc-plan", RootName: "Show", MultiFile: true, Selection: *source.ContentSelection}
	snapshot := cloud.SelectiveOfflineSnapshot{Task: cloud.OfflineTask{ID: "omc-selected-" + strings.Repeat("a", 64), Completed: true}, Name: "Show", Complete: true, Files: []cloud.SelectiveOfflineFile{{Path: "b.mkv", Index: 1, Item: cloud.Item{ID: "foreign", ParentID: "root", Size: 20}}}}
	if err := selectedSnapshot(snapshot, snapshot.Task.ID, binding); err == nil {
		t.Fatal("unselected file accepted")
	}
}
