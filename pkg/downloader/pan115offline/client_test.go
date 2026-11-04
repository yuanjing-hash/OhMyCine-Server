package pan115offline

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

type fakeDriver struct {
	submittedURI string
	directoryID  string
	cancelFiles  bool
	task         cloud.OfflineTask
	items        map[string]cloud.Item
	statErr      error
	listErr      error
	submitErr    error
}

type fakeShareDriver struct {
	*fakeDriver
	children     map[string][]cloud.Item
	shareURL     string
	receiveCalls int
	receivedRoot string
	recycled     string
}

func (f *fakeShareDriver) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{NativeOfflineDownload: true, ShareReceive: true, CreateDirectory: true, Move: true, Copy: true, Rename: true, Recycle: true}
}
func (f *fakeShareDriver) List(_ context.Context, parent string, request cloud.PageRequest) (cloud.Page, error) {
	items := f.children[parent]
	start := int(request.Offset)
	if start > len(items) {
		start = len(items)
	}
	end := start + int(request.Limit)
	if request.Limit <= 0 || end > len(items) {
		end = len(items)
	}
	return cloud.Page{Items: append([]cloud.Item(nil), items[start:end]...), Offset: request.Offset, HasMore: end < len(items)}, nil
}
func (f *fakeShareDriver) InspectShare(_ context.Context, raw string) (cloud.ShareSnapshot, error) {
	f.shareURL = raw
	return cloud.ShareSnapshot{ShareCode: "share-code", ReceiveCode: "abcd", Items: []cloud.ShareItem{{ID: "shared-movie", Name: "Movie.mkv"}}}, nil
}
func (f *fakeShareDriver) ReceiveShare(_ context.Context, _ cloud.ShareSnapshot, root string) error {
	f.receiveCalls++
	f.receivedRoot = root
	item := cloud.Item{ID: "received-movie", ParentID: root, Name: "Movie.mkv", Size: 1024}
	f.items[item.ID] = item
	f.children[root] = []cloud.Item{item}
	return nil
}
func (f *fakeShareDriver) CreateDirectory(_ context.Context, parent, name string) (cloud.Item, error) {
	item := cloud.Item{ID: "share-task-root", ParentID: parent, Name: name, IsDir: true}
	f.items[item.ID] = item
	f.children[parent] = append(f.children[parent], item)
	return item, nil
}
func (f *fakeShareDriver) Move(context.Context, string, string) error   { return nil }
func (f *fakeShareDriver) Copy(context.Context, string, string) error   { return nil }
func (f *fakeShareDriver) Rename(context.Context, string, string) error { return nil }
func (f *fakeShareDriver) Recycle(_ context.Context, id string) error {
	f.recycled = id
	return nil
}

func newFakeShareDriver() *fakeShareDriver {
	base := &fakeDriver{items: map[string]cloud.Item{
		"root":   {ID: "root", ParentID: "0", Name: "root", IsDir: true},
		"intake": {ID: "intake", ParentID: "root", Name: "intake", IsDir: true},
	}}
	return &fakeShareDriver{fakeDriver: base, children: map[string][]cloud.Item{}}
}

func (f *fakeDriver) Provider() string { return cloud.ProviderPan115 }
func (f *fakeDriver) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{NativeOfflineDownload: true, CreateDirectory: true, Move: true, Copy: true, Rename: true, Recycle: true}
}
func (f *fakeDriver) Probe(context.Context) (cloud.Account, error) { return cloud.Account{}, nil }
func (f *fakeDriver) List(context.Context, string, cloud.PageRequest) (cloud.Page, error) {
	return cloud.Page{}, f.listErr
}
func (f *fakeDriver) Stat(_ context.Context, id string) (cloud.Item, error) {
	if f.statErr != nil {
		return cloud.Item{}, f.statErr
	}
	if item, ok := f.items[id]; ok {
		return item, nil
	}
	return cloud.Item{ID: id, ParentID: "root", IsDir: true}, nil
}
func (f *fakeDriver) DirectURL(context.Context, cloud.DirectURLRequest) (cloud.TemporaryURL, error) {
	return cloud.TemporaryURL{Headers: http.Header{}, ExpiresAt: time.Now()}, nil
}
func (f *fakeDriver) CreateDirectory(_ context.Context, parent, name string) (cloud.Item, error) {
	item := cloud.Item{ID: "offline-task-root", ParentID: parent, Name: name, IsDir: true}
	if f.items == nil {
		f.items = map[string]cloud.Item{}
	}
	f.items[item.ID] = item
	return item, nil
}
func (f *fakeDriver) Move(context.Context, string, string) error   { return nil }
func (f *fakeDriver) Copy(context.Context, string, string) error   { return nil }
func (f *fakeDriver) Rename(context.Context, string, string) error { return nil }
func (f *fakeDriver) Recycle(context.Context, string) error        { return nil }
func (f *fakeDriver) SubmitOffline(_ context.Context, uri, directoryID string) (cloud.OfflineTask, error) {
	f.submittedURI, f.directoryID = uri, directoryID
	return f.task, f.submitErr
}

func TestNativeOfflineRejectsTorrentPayload(t *testing.T) {
	driver := &fakeDriver{items: map[string]cloud.Item{
		"root":   {ID: "root", ParentID: "0", Name: "root", IsDir: true},
		"target": {ID: "target", ParentID: "root", Name: "target", IsDir: true},
	}, task: cloud.OfflineTask{ID: "offline-task"}}
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceTorrent, Filename: "movie.torrent", Torrent: []byte("d4:infoe")}})
	code, retryable := downloader.ErrorInfo(err)
	if code != "downloader_source_unsupported" || retryable || driver.submittedURI != "" {
		t.Fatalf("code=%q retryable=%v submitted=%q err=%v", code, retryable, driver.submittedURI, err)
	}
}

func TestNativeOfflineRejectsMalformedTorrentBeforeProviderSubmission(t *testing.T) {
	driver := &fakeDriver{items: map[string]cloud.Item{
		"root":   {ID: "root", ParentID: "0", Name: "root", IsDir: true},
		"target": {ID: "target", ParentID: "root", Name: "target", IsDir: true},
	}}
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceTorrent, Filename: "bad.torrent", Torrent: []byte("d4:infoi1ee")}})
	code, retryable := downloader.ErrorInfo(err)
	if code != "downloader_source_unsupported" || retryable || driver.submittedURI != "" {
		t.Fatalf("code=%q retryable=%v submitted=%q err=%v", code, retryable, driver.submittedURI, err)
	}
}

func TestNativeOfflineRejectsUnavailableTargetBeforeProviderSubmission(t *testing.T) {
	driver := &fakeDriver{statErr: cloud.Error(cloud.CodeNotFound, false, errors.New("missing"))}
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "moved-directory"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}, Tag: "omc-test"})
	code, retryable := downloader.ErrorInfo(err)
	if code != "downloader_storage_unavailable" || retryable || driver.submittedURI != "" {
		t.Fatalf("code=%q retryable=%v submitted=%q err=%v", code, retryable, driver.submittedURI, err)
	}
}

func TestNativeOfflineRejectsTargetMovedOutsideStorageRoot(t *testing.T) {
	driver := &fakeDriver{items: map[string]cloud.Item{
		"target": {ID: "target", ParentID: "other-root", IsDir: true},
	}}
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "selected-root", ProviderDirectoryID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}, Tag: "omc-test"})
	code, retryable := downloader.ErrorInfo(err)
	if code != "downloader_storage_unavailable" || retryable || driver.submittedURI != "" {
		t.Fatalf("code=%q retryable=%v submitted=%q err=%v", code, retryable, driver.submittedURI, err)
	}
}

func TestNativeOfflineAcceptsTargetBelowAccountRoot(t *testing.T) {
	driver := &fakeDriver{items: map[string]cloud.Item{
		"target": {ID: "target", ParentID: "0", IsDir: true},
	}}
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "0", ProviderDirectoryID: "target"})
	if err != nil {
		t.Fatal(err)
	}
	health, err := client.Test(context.Background())
	if err != nil || health.Version != "115 原生离线" {
		t.Fatalf("health=%+v err=%v", health, err)
	}
	_, err = client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}, Tag: "omc-test"})
	if err != nil || driver.submittedURI == "" || driver.directoryID != "offline-task-root" {
		t.Fatalf("submit err=%v uri=%q directory=%q", err, driver.submittedURI, driver.directoryID)
	}
}

func TestNativeOfflineAcceptsAccountRootAsTarget(t *testing.T) {
	driver := &fakeDriver{}
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "0", ProviderDirectoryID: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}, Tag: "omc-test"})
	if err != nil || driver.directoryID != "offline-task-root" {
		t.Fatalf("submit err=%v directory=%q", err, driver.directoryID)
	}
}

func TestNativeOfflinePreservesProviderErrorClassification(t *testing.T) {
	deadline := time.Now().UTC().Add(time.Minute)
	for _, item := range []struct {
		name, stage, code, providerCode string
		statErr, listErr, submitErr     error
		retryable, timeout, canceled    bool
	}{
		{name: "target-canceled", stage: downloader.FailureTargetValidation, code: "downloader_storage_unavailable", providerCode: cloud.CodeUnavailable, statErr: cloud.Error(cloud.CodeUnavailable, true, context.Canceled), retryable: true, canceled: true},
		{name: "directory-timeout", stage: downloader.FailureTaskDirectory, code: "downloader_unavailable", providerCode: cloud.CodeUnavailable, listErr: cloud.Error(cloud.CodeUnavailable, true, context.DeadlineExceeded), retryable: true, timeout: true},
		{name: "submission-invalid", stage: downloader.FailureOfflineSubmission, code: "downloader_response_invalid", providerCode: cloud.CodeResponseInvalid, submitErr: cloud.Error(cloud.CodeResponseInvalid, false, errors.New("private-output-id malformed provider response"))},
		{name: "submission-unknown", stage: downloader.FailureOfflineSubmission, code: "downloader_share_result_unknown", providerCode: cloud.CodeMutationUnknown, submitErr: cloud.WithRetryNotBefore(cloud.Error(cloud.CodeMutationUnknown, true, context.DeadlineExceeded), deadline, cloud.RetryReasonProviderCooldown), retryable: true, timeout: true},
		{name: "unknown-provider-code", stage: downloader.FailureOfflineSubmission, code: "downloader_unavailable", submitErr: cloud.Error("private-output-id", false, errors.New("private-output-id"))},
	} {
		t.Run(item.name, func(t *testing.T) {
			driver := &fakeDriver{statErr: item.statErr, listErr: item.listErr, submitErr: item.submitErr}
			client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "target-id"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}, Tag: "omc-test"})
			code, retryable := downloader.ErrorInfo(err)
			diagnostic, ok := downloader.FailureDiagnosticFromError(err)
			if code != item.code || retryable != item.retryable || !ok || diagnostic.Stage != item.stage || diagnostic.ProviderCode != item.providerCode || diagnostic.Timeout != item.timeout || diagnostic.Canceled != item.canceled {
				t.Fatalf("code=%q retryable=%v diagnostic=%+v ok=%v err=%v", code, retryable, diagnostic, ok, err)
			}
			if (driver.submittedURI != "") != (item.stage == downloader.FailureOfflineSubmission) {
				t.Fatal("read-side failure reached provider submission")
			}
			var provider *cloud.ProviderError
			if !errors.As(err, &provider) || (item.timeout && !errors.Is(err, context.DeadlineExceeded)) || (item.canceled && !errors.Is(err, context.Canceled)) {
				t.Fatal("stage wrapper lost original recovery cause")
			}
			if item.name == "submission-unknown" {
				if until, reason, ok := cloud.RetryTiming(err); !ok || !until.Equal(deadline) || reason != cloud.RetryReasonProviderCooldown {
					t.Fatal("stage wrapper lost provider cooldown")
				}
			}
			raw, _ := json.Marshal(diagnostic)
			if strings.Contains(string(raw)+err.Error(), "private-output-id") {
				t.Fatal("private cause crossed diagnostic boundary")
			}
		})
	}
}

func TestNativeOfflineMapsPermanentProviderSubmissionErrors(t *testing.T) {
	tests := []struct {
		providerCode string
		wantCode     string
	}{
		{providerCode: cloud.CodeOfflineNoQuota, wantCode: "downloader_quota_exhausted"},
		{providerCode: cloud.CodeOfflineBadLink, wantCode: "downloader_source_invalid"},
		{providerCode: cloud.CodeOfflineTaskExists, wantCode: "downloader_task_exists"},
	}
	for _, item := range tests {
		driver := &fakeDriver{submitErr: cloud.Error(item.providerCode, false, errors.New("provider rejected submission"))}
		client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "target-id"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}, Tag: "omc-test"})
		code, retryable := downloader.ErrorInfo(err)
		if code != item.wantCode || retryable {
			t.Fatalf("provider code %q mapped to %q, %t", item.providerCode, code, retryable)
		}
	}
}

func (f *fakeDriver) GetOffline(context.Context, string) (cloud.OfflineTask, error) {
	return f.task, nil
}
func (f *fakeDriver) CancelOffline(_ context.Context, _ string, deleteFiles bool) error {
	f.cancelFiles = deleteFiles
	return nil
}

func TestNativeOfflineMapsProviderStorageTask(t *testing.T) {
	if Capabilities.Seeding || Capabilities.Pause || Capabilities.Resume || Capabilities.UploadSpeed {
		t.Fatalf("115 native offline downloader must not expose seeding controls: %+v", Capabilities)
	}
	if !Capabilities.NativeOffline || Capabilities.OutputConstraint != downloader.OutputConstraintProviderStorage {
		t.Fatalf("unexpected native offline capabilities: %+v", Capabilities)
	}
	progress, total := .25, int64(400)
	driver := &fakeDriver{task: cloud.OfflineTask{ID: "hash", Name: "Movie", Status: "downloading", Progress: &progress, BytesTotal: &total, OutputItemID: "file-id"}}
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "target-id"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=urn:btih:abc"}, Tag: "omc-test"})
	if err != nil {
		t.Fatal(err)
	}
	if driver.directoryID != "offline-task-root" || task.ID != "hash" || task.OutputItemID != "file-id" || task.BytesCompleted == nil || *task.BytesCompleted != 100 {
		t.Fatalf("unexpected task mapping: %#v", task)
	}
	if err := client.Cancel(context.Background(), task.ID, true); err != nil || !driver.cancelFiles {
		t.Fatalf("cancel delete_data was not forwarded: %v", err)
	}
}

func TestShareSubmissionUsesStableTaskDirectoryAndReconcilesRetry(t *testing.T) {
	driver := newFakeShareDriver()
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "intake"})
	if err != nil {
		t.Fatal(err)
	}
	request := downloader.SubmitRequest{
		Source:              downloader.Source{Kind: downloader.SourcePan115Share, URL: "https://115.com/s/share-code?password=abcd"},
		Tag:                 "omc-task-id",
		ProviderDirectoryID: "intake",
	}
	first, err := client.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "share:share-task-root" || !first.Completed || driver.receiveCalls != 1 || driver.receivedRoot != "share-task-root" || driver.shareURL != request.Source.URL {
		t.Fatalf("task=%+v receive_calls=%d root=%q url=%q", first, driver.receiveCalls, driver.receivedRoot, driver.shareURL)
	}
	manifest, err := client.(downloader.ManifestClient).Manifest(context.Background(), first.ID)
	if err != nil || !manifest.Complete || len(manifest.Files) != 1 || manifest.Files[0].ProviderItemID != "received-movie" {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}

	second, err := client.Submit(context.Background(), request)
	if err != nil || second.ID != first.ID || driver.receiveCalls != 1 {
		t.Fatalf("retry task=%+v receive_calls=%d err=%v", second, driver.receiveCalls, err)
	}
}

func TestProviderItemAdoptionRevalidatesIdentityAndDestructiveCancel(t *testing.T) {
	driver := newFakeShareDriver()
	manual := cloud.Item{ID: "manual", ParentID: "intake", Name: "Seven.Samurai.1954", IsDir: true}
	driver.items[manual.ID] = manual
	driver.children["intake"] = []cloud.Item{manual}
	client, err := New(downloader.Config{CloudDriver: driver, ProviderStorageRootID: "root", ProviderDirectoryID: "intake"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceProviderItem, ProviderItemID: "manual"}, ProviderDirectoryID: "intake"})
	if err != nil || task.ID != "ingest:manual" || !task.Completed {
		t.Fatalf("task=%+v err=%v", task, err)
	}
	if err := client.Cancel(context.Background(), task.ID, true); err != nil || driver.recycled != "manual" {
		t.Fatalf("cancel err=%v recycled=%q", err, driver.recycled)
	}

	driver.items["manual"] = cloud.Item{ID: "different", ParentID: "intake", Name: "Seven.Samurai.1954", IsDir: true}
	_, err = client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceProviderItem, ProviderItemID: "manual"}, ProviderDirectoryID: "intake"})
	if code, retryable := downloader.ErrorInfo(err); code != "downloader_response_invalid" || retryable {
		t.Fatalf("identity mismatch code=%q retryable=%t err=%v", code, retryable, err)
	}
}
