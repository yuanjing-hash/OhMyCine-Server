package nodeagent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type fakeStorageSourceDriver struct {
	mu           sync.Mutex
	before       func(context.Context) error
	taskID       string
	complete     bool
	submitCalls  int
	receiveCalls int
	readOffsets  []int64
	failReads    map[int64]int
	share        cloud.ShareSnapshot
	shareItems   []cloud.Item
	receiveErr   error
	body         []byte
	sha1         string
}

func (driver *fakeStorageSourceDriver) check(ctx context.Context) error {
	if driver.before != nil {
		return driver.before(ctx)
	}
	return nil
}
func (driver *fakeStorageSourceDriver) Provider() string { return nodeprotocol.StorageProviderPan115 }
func (driver *fakeStorageSourceDriver) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{DirectoryList: true, NativeOfflineDownload: true, ShareReceive: true}
}
func (driver *fakeStorageSourceDriver) Probe(context.Context) (cloud.Account, error) {
	return cloud.Account{}, nil
}
func (driver *fakeStorageSourceDriver) List(ctx context.Context, parentID string, request cloud.PageRequest) (cloud.Page, error) {
	if err := driver.check(ctx); err != nil {
		return cloud.Page{}, err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if parentID != "share-root" {
		return cloud.Page{}, nil
	}
	start := int(request.Offset)
	if start >= len(driver.shareItems) {
		return cloud.Page{Offset: request.Offset}, nil
	}
	end := min(len(driver.shareItems), start+int(request.Limit))
	return cloud.Page{Items: append([]cloud.Item(nil), driver.shareItems[start:end]...), Offset: request.Offset, HasMore: end < len(driver.shareItems)}, nil
}
func (driver *fakeStorageSourceDriver) DirectURL(context.Context, cloud.DirectURLRequest) (cloud.TemporaryURL, error) {
	return cloud.TemporaryURL{}, errors.New("not implemented")
}
func (driver *fakeStorageSourceDriver) Stat(ctx context.Context, id string) (cloud.Item, error) {
	if err := driver.check(ctx); err != nil {
		return cloud.Item{}, err
	}
	if id == "output-1" {
		return cloud.Item{ID: id, ParentID: "0", Name: "Show", IsDir: true, PickCode: "pick"}, nil
	}
	if id == "share-root" {
		return cloud.Item{ID: id, ParentID: "0", Name: "Share Root", IsDir: true, PickCode: "share-pick"}, nil
	}
	return cloud.Item{}, cloud.Error(cloud.CodeNotFound, false, nil)
}
func (driver *fakeStorageSourceDriver) SubmitOffline(ctx context.Context, uri, directoryID string) (cloud.OfflineTask, error) {
	if err := driver.check(ctx); err != nil {
		return cloud.OfflineTask{}, err
	}
	if !strings.HasPrefix(uri, "magnet:?") || directoryID != "0" {
		return cloud.OfflineTask{}, errors.New("unexpected submit")
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.submitCalls++
	driver.taskID = strings.Repeat("a", 40)
	return driver.offlineTaskLocked(), nil
}
func (driver *fakeStorageSourceDriver) GetOffline(ctx context.Context, taskID string) (cloud.OfflineTask, error) {
	if err := driver.check(ctx); err != nil {
		return cloud.OfflineTask{}, err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.taskID == "" || !strings.EqualFold(driver.taskID, taskID) {
		return cloud.OfflineTask{}, cloud.Error(cloud.CodeNotFound, false, nil)
	}
	return driver.offlineTaskLocked(), nil
}
func (driver *fakeStorageSourceDriver) offlineTaskLocked() cloud.OfflineTask {
	progress := float64(0.5)
	task := cloud.OfflineTask{ID: driver.taskID, Status: "downloading", Progress: &progress}
	if driver.complete {
		progress = 1
		task.Status, task.Progress, task.Completed, task.OutputItemID = "completed", &progress, true, "output-1"
	}
	return task
}
func (driver *fakeStorageSourceDriver) CancelOffline(context.Context, string, bool) error { return nil }
func (driver *fakeStorageSourceDriver) InspectShare(ctx context.Context, _ string) (cloud.ShareSnapshot, error) {
	if err := driver.check(ctx); err != nil {
		return cloud.ShareSnapshot{}, err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.share, nil
}
func (driver *fakeStorageSourceDriver) ReceiveShare(ctx context.Context, snapshot cloud.ShareSnapshot, directoryID string) error {
	if err := driver.check(ctx); err != nil {
		return err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if directoryID != "share-root" || snapshot.ShareCode != driver.share.ShareCode {
		return errors.New("unexpected share receive")
	}
	driver.receiveCalls++
	driver.shareItems = make([]cloud.Item, 0, len(snapshot.Items))
	for index, item := range snapshot.Items {
		driver.shareItems = append(driver.shareItems, cloud.Item{ID: "received-" + string(rune('a'+index)), ParentID: directoryID, Name: item.Name, IsDir: item.IsDir, Size: item.Size})
	}
	return driver.receiveErr
}
func (driver *fakeStorageSourceDriver) StreamTree(ctx context.Context, rootID string, _ int, emit func(cloud.TreeBatch) error) error {
	if err := driver.check(ctx); err != nil {
		return err
	}
	if rootID != "output-1" {
		return errors.New("unexpected root")
	}
	return emit(cloud.TreeBatch{Offset: 0, Total: 1, Entries: []cloud.TreeEntry{{Item: cloud.Item{ID: "provider-file-1", ParentID: rootID, Name: "Episode 01.mkv", Size: int64(len(driver.body)), SHA1: driver.sha1}, RelativePath: "/Season 01/Episode 01.mkv"}}})
}
func (driver *fakeStorageSourceDriver) OpenRead(ctx context.Context, request cloud.ReadRequest) (cloud.ReadResult, error) {
	if err := driver.check(ctx); err != nil {
		return cloud.ReadResult{}, err
	}
	driver.mu.Lock()
	driver.readOffsets = append(driver.readOffsets, request.Offset)
	if driver.failReads[request.Offset] > 0 {
		driver.failReads[request.Offset]--
		driver.mu.Unlock()
		return cloud.ReadResult{}, cloud.Error(cloud.CodeUnavailable, true, errors.New("temporary source read failure"))
	}
	driver.mu.Unlock()
	total := int64(len(driver.body))
	if request.FileID != "provider-file-1" || request.Offset < 0 || request.Offset >= total {
		return cloud.ReadResult{}, errors.New("unexpected read")
	}
	return cloud.ReadResult{Body: io.NopCloser(bytes.NewReader(driver.body[request.Offset:])), OffsetAccepted: true, TotalSize: &total}, nil
}

func TestStorageSourceRestartDoesNotResubmitAndResumesVerifiedChunks(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "managed")
	if err := os.MkdirAll(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("m"), int(nodeprotocol.FileChunkSize)+257)
	digest := sha1.Sum(body)
	fake := &fakeStorageSourceDriver{body: body, sha1: strings.ToUpper(hex.EncodeToString(digest[:]))}
	store, err := OpenStore(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	config := Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: root, ManagedRoot: managed, SealingPrivateKeyFile: filepath.Join(root, "node.seal.key"), MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}
	if err := EnsureSealingIdentity(config); err != nil {
		t.Fatal(err)
	}
	agent, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	agent.sourceFactory = func(_ nodeprotocol.StorageSourceCredential, before func(context.Context) error) (storageSourceDriver, error) {
		fake.before = before
		return fake, nil
	}
	now := time.Now().UTC()
	magnet := "magnet:?xt=urn:btih:" + strings.Repeat("a", 40) + "&dn=secret-name"
	contentDigest, err := nodeprotocol.StorageSourceContentDigest(nodeprotocol.StorageSourceKindPan115OfflineMagnet, magnet)
	if err != nil {
		t.Fatal(err)
	}
	plan := nodeprotocol.StorageSourcePlan{StorageID: "storage-a", ProviderType: nodeprotocol.StorageProviderPan115, SourceKind: nodeprotocol.StorageSourceKindPan115OfflineMagnet, TargetKind: nodeprotocol.StorageSourceTargetPan115, SourceIdentityDigest: strings.Repeat("1", 64), TargetIdentityDigest: strings.Repeat("2", 64), SourceContentDigest: contentDigest, OfflineDestinationID: "0"}
	payload, _ := json.Marshal(plan)
	operation := nodeprotocol.OperationPlan{ProtocolVersion: nodeprotocol.VersionV1, OperationKey: "source-1", TaskID: "task-1", NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageSourceMaterialize, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(10 * time.Minute), Payload: payload}
	planDigest, _ := operation.Digest()
	if _, _, err := store.Put(context.Background(), "server-1", planDigest, operation, now); err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(nodeprotocol.StorageSourceCredential{ProviderType: nodeprotocol.StorageProviderPan115, Cookie: "UID=secret; CID=secret; SEID=secret", SourceURI: magnet})
	grant := nodeprotocol.CredentialGrant{GrantID: "grant-1", NodeID: "node-1", TaskID: "task-1", OperationKey: "source-1", ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: "storage-a", CredentialRevision: 1, AllowedActions: []string{nodeprotocol.StorageSourceGrantOfflineSubmit, nodeprotocol.StorageSourceGrantOfflineStatus, nodeprotocol.StorageSourceGrantRead}, ExpiresAt: now.Add(10 * time.Minute), Credential: credential}
	envelope, err := nodeprotocol.SealCredentialGrant(agent.sealingPublicKey, grant, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCredentialGrant(context.Background(), "server-1", envelope, now); err != nil {
		t.Fatal(err)
	}
	action := nodeprotocol.StorageSourceActionRequest{RequestID: "source-request-1", TaskID: "task-1", OperationKey: "source-1", PlanDigest: planDigest, StorageID: "storage-a", Action: nodeprotocol.StorageSourceActionMaterialize, GrantID: "grant-1"}
	firstServer := httptest.NewServer(agent.Handler())
	doNodeJSON(t, firstServer.URL+"/node/v1/storage-source/actions", http.MethodPost, action, http.StatusAccepted)
	waitStorageSourceStatus(t, store, action.RequestID, nodeprotocol.StorageSourceWaiting)
	firstServer.Close()
	if fake.submitCalls != 1 {
		t.Fatalf("submit calls=%d", fake.submitCalls)
	}

	// Simulate a Node process restart after the provider task receipt was
	// persisted. The next wake uses the stored provider task ID and must not
	// submit the same offline task again.
	fake.mu.Lock()
	fake.complete = true
	fake.failReads = map[int64]int{nodeprotocol.FileChunkSize: 1}
	fake.mu.Unlock()
	restarted, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	restarted.sourceFactory = agent.sourceFactory
	secondServer := httptest.NewServer(restarted.Handler())
	doNodeJSON(t, secondServer.URL+"/node/v1/storage-source/actions", http.MethodPost, action, http.StatusAccepted)
	waitStorageSourceError(t, store, action.RequestID, nodeprotocol.StorageSourceDownloading, "node_source_unavailable")
	secondServer.Close()
	fake.mu.Lock()
	firstReadOffsets := append([]int64(nil), fake.readOffsets...)
	fake.mu.Unlock()
	if len(firstReadOffsets) != 2 || firstReadOffsets[0] != 0 || firstReadOffsets[1] != nodeprotocol.FileChunkSize {
		t.Fatalf("first materialization read offsets=%v", firstReadOffsets)
	}

	// A second process restart revalidates the durable first-chunk hash and
	// requests only the missing second range.
	resumed, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	resumed.sourceFactory = agent.sourceFactory
	thirdServer := httptest.NewServer(resumed.Handler())
	defer thirdServer.Close()
	doNodeJSON(t, thirdServer.URL+"/node/v1/storage-source/actions", http.MethodPost, action, http.StatusAccepted)
	completed := waitStorageSourceStatus(t, store, action.RequestID, nodeprotocol.StorageSourceCompleted)
	if completed.FileExport == nil || completed.FileExport.TotalFiles != 1 || completed.DownloadedFiles != 1 || completed.DownloadedBytes != int64(len(body)) {
		t.Fatalf("response=%+v", completed)
	}
	if fake.submitCalls != 1 {
		t.Fatalf("restart resubmitted offline task: %d", fake.submitCalls)
	}
	fake.mu.Lock()
	readOffsets := append([]int64(nil), fake.readOffsets...)
	fake.mu.Unlock()
	if len(readOffsets) != 3 || readOffsets[0] != 0 || readOffsets[1] != nodeprotocol.FileChunkSize || readOffsets[2] != nodeprotocol.FileChunkSize {
		t.Fatalf("resumed read offsets=%v", readOffsets)
	}
	manifest, err := store.FileExportManifest(context.Background(), "server-1", "source-1", 1, 10, time.Now().UTC())
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].RelativePath != "Season 01/Episode 01.mkv" || manifest.Files[0].ChunkCount != 2 {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	var envelopeJSON string
	if err := store.db.QueryRow(`SELECT envelope_json FROM credential_grants WHERE grant_id='grant-1'`).Scan(&envelopeJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(envelopeJSON, "UID=secret") || strings.Contains(envelopeJSON, "secret-name") || strings.Contains(envelopeJSON, "magnet:") {
		t.Fatal("source credential leaked into Node SQLite")
	}
	waitStorageSourceIdle(t, resumed, action.OperationKey)
	operationRoot, err := resumed.storageSourceRoot(action.OperationKey)
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(operationRoot), "keep-this-sibling")
	if err := os.Mkdir(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	wrongCleanup := nodeprotocol.StorageSourceCleanupRequest{RequestID: "cleanup-wrong-task", OperationKey: action.OperationKey, TaskID: "task-other", PlanDigest: planDigest}
	doNodeJSON(t, thirdServer.URL+"/node/v1/operations/"+action.OperationKey+"/cleanup", http.MethodPost, wrongCleanup, http.StatusNotFound)
	if _, err := os.Stat(operationRoot); err != nil {
		t.Fatalf("mismatched cleanup changed operation root: %v", err)
	}
	cleanup := nodeprotocol.StorageSourceCleanupRequest{RequestID: "cleanup-source-1", OperationKey: action.OperationKey, TaskID: action.TaskID, PlanDigest: planDigest}
	doNodeJSON(t, thirdServer.URL+"/node/v1/operations/"+action.OperationKey+"/cleanup", http.MethodPost, cleanup, http.StatusOK)
	doNodeJSON(t, thirdServer.URL+"/node/v1/operations/"+action.OperationKey+"/cleanup", http.MethodPost, cleanup, http.StatusOK)
	if _, err := os.Stat(operationRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleaned operation root still exists: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("cleanup escaped into sibling: %v", err)
	}
	var exports, sourceFiles, sourceChunks int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM file_exports WHERE operation_key=?`, action.OperationKey).Scan(&exports); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM storage_source_files WHERE operation_key=?`, action.OperationKey).Scan(&sourceFiles); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM storage_source_chunks WHERE operation_key=?`, action.OperationKey).Scan(&sourceChunks); err != nil {
		t.Fatal(err)
	}
	if exports != 0 || sourceFiles != 0 || sourceChunks != 0 {
		t.Fatalf("cleanup retained export/checkpoints: exports=%d files=%d chunks=%d", exports, sourceFiles, sourceChunks)
	}
}

func waitStorageSourceIdle(t *testing.T, agent *Agent, operationKey string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		agent.sourceMu.Lock()
		_, running := agent.sourceRuns[operationKey]
		agent.sourceMu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("storage source execution did not become idle")
}

func TestStorageSourceShareReconcilesBeforeAndAfterReceive(t *testing.T) {
	for _, test := range []struct {
		name         string
		selected     bool
		existing     bool
		receiveErr   error
		wantReceives int
	}{
		{name: "selected file only", selected: true, wantReceives: 1},
		{name: "selected restart", selected: true, existing: true, wantReceives: 0},
		{name: "restart after receive", existing: true, wantReceives: 0},
		{name: "lost receive acknowledgement", receiveErr: cloud.Error(cloud.CodeUnavailable, true, errors.New("lost acknowledgement")), wantReceives: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			managed := filepath.Join(root, "managed")
			if err := os.MkdirAll(managed, 0o700); err != nil {
				t.Fatal(err)
			}
			store, err := OpenStore(filepath.Join(root, "node.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			config := Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: root, ManagedRoot: managed, SealingPrivateKeyFile: filepath.Join(root, "node.seal.key"), MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}
			if err := EnsureSealingIdentity(config); err != nil {
				t.Fatal(err)
			}
			agent, err := New(config, store)
			if err != nil {
				t.Fatal(err)
			}
			shareURI := "https://115.com/s/share-code?password=private"
			kind := nodeprotocol.StorageSourceKindPan115Share
			if test.selected {
				kind = nodeprotocol.StorageSourceKindPan115ShareSelected
				shareURI, err = cloud.EncodeSelectedShareSource(shareURI, cloud.ShareSelection{Version: 1, Files: []cloud.ShareTreeItem{{ID: "shared-file", RelativePath: "Movie.mkv", Size: 5}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			contentDigest, err := nodeprotocol.StorageSourceContentDigest(kind, shareURI)
			if err != nil {
				t.Fatal(err)
			}
			plan := nodeprotocol.StorageSourcePlan{StorageID: "storage-a", ProviderType: nodeprotocol.StorageProviderPan115, SourceKind: kind, TargetKind: nodeprotocol.StorageSourceTargetPan115, SourceIdentityDigest: strings.Repeat("1", 64), TargetIdentityDigest: strings.Repeat("2", 64), SourceContentDigest: contentDigest, OfflineDestinationID: "share-root"}
			payload, _ := json.Marshal(plan)
			now := time.Now().UTC()
			operation := nodeprotocol.OperationPlan{ProtocolVersion: nodeprotocol.VersionV1, OperationKey: "share-source-1", TaskID: "task-1", NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageSourceMaterialize, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(10 * time.Minute), Payload: payload}
			planDigest, _ := operation.Digest()
			if _, _, err := store.Put(context.Background(), "server-1", planDigest, operation, now); err != nil {
				t.Fatal(err)
			}
			input := nodeprotocol.StorageSourceActionRequest{RequestID: "share-request-1", TaskID: "task-1", OperationKey: operation.OperationKey, PlanDigest: planDigest, StorageID: plan.StorageID, Action: nodeprotocol.StorageSourceActionMaterialize, GrantID: "grant-1"}
			run, err := store.PrepareStorageSourceRun(context.Background(), "server-1", input, now)
			if err != nil {
				t.Fatal(err)
			}
			fake := &fakeStorageSourceDriver{
				share:      cloud.ShareSnapshot{ShareCode: "share-code", ReceiveCode: "private", Items: []cloud.ShareItem{{ID: "shared-file", Name: "Movie.mkv", Size: 5}}},
				receiveErr: test.receiveErr,
			}
			if test.selected {
				fake.share.Items = append(fake.share.Items, cloud.ShareItem{ID: "unselected", Name: "Other.mkv", Size: 20})
			}
			if test.existing {
				fake.shareItems = []cloud.Item{{ID: "received-existing", ParentID: "share-root", Name: "Movie.mkv", Size: 5}}
			}
			credential := nodeprotocol.StorageSourceCredential{ProviderType: nodeprotocol.StorageProviderPan115, Cookie: "UID=secret", SourceURI: shareURI}
			if err := agent.materializeStorageSourceShare(context.Background(), "server-1", input, plan, run, credential, fake, func(context.Context, string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			persisted, err := store.StorageSourceRun(context.Background(), "server-1", input.OperationKey)
			if err != nil || persisted.OutputRootID != "share-root" || (!test.selected && !strings.HasPrefix(persisted.ProviderTaskID, "share:")) {
				t.Fatalf("persisted=%+v err=%v", persisted, err)
			}
			if test.selected && len(fake.shareItems) != 1 {
				t.Fatal("Node received unselected files")
			}
			if fake.receiveCalls != test.wantReceives {
				t.Fatalf("receive calls=%d want=%d", fake.receiveCalls, test.wantReceives)
			}
		})
	}
}

func TestStorageSourceCredentialRejectsSourceIdentityDrift(t *testing.T) {
	root := t.TempDir()
	store, err := OpenStore(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	config := Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: root, ManagedRoot: filepath.Join(root, "managed"), SealingPrivateKeyFile: filepath.Join(root, "node.seal.key"), MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}
	if err := EnsureSealingIdentity(config); err != nil {
		t.Fatal(err)
	}
	agent, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	wantURI := "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)
	wrongURI := "magnet:?xt=urn:btih:" + strings.Repeat("b", 40)
	contentDigest, _ := nodeprotocol.StorageSourceContentDigest(nodeprotocol.StorageSourceKindPan115OfflineMagnet, wantURI)
	plan := nodeprotocol.StorageSourcePlan{StorageID: "storage-a", ProviderType: nodeprotocol.StorageProviderPan115, SourceKind: nodeprotocol.StorageSourceKindPan115OfflineMagnet, TargetKind: nodeprotocol.StorageSourceTargetPan115, SourceIdentityDigest: strings.Repeat("1", 64), TargetIdentityDigest: strings.Repeat("2", 64), SourceContentDigest: contentDigest, OfflineDestinationID: "0"}
	payload, _ := json.Marshal(plan)
	operation := nodeprotocol.OperationPlan{ProtocolVersion: nodeprotocol.VersionV1, OperationKey: "source-drift", TaskID: "task-1", NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageSourceMaterialize, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(10 * time.Minute), Payload: payload}
	planDigest, _ := operation.Digest()
	if _, _, err := store.Put(context.Background(), "server-1", planDigest, operation, now); err != nil {
		t.Fatal(err)
	}
	input := nodeprotocol.StorageSourceActionRequest{RequestID: "source-drift-request", TaskID: "task-1", OperationKey: operation.OperationKey, PlanDigest: planDigest, StorageID: plan.StorageID, Action: nodeprotocol.StorageSourceActionMaterialize, GrantID: "grant-drift"}
	run, err := store.PrepareStorageSourceRun(context.Background(), "server-1", input, now)
	if err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(nodeprotocol.StorageSourceCredential{ProviderType: nodeprotocol.StorageProviderPan115, Cookie: "UID=secret", SourceURI: wrongURI})
	grant := nodeprotocol.CredentialGrant{GrantID: input.GrantID, NodeID: "node-1", TaskID: input.TaskID, OperationKey: input.OperationKey, ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: input.StorageID, CredentialRevision: 1, AllowedActions: []string{nodeprotocol.StorageSourceGrantOfflineSubmit, nodeprotocol.StorageSourceGrantOfflineStatus}, ExpiresAt: now.Add(10 * time.Minute), Credential: credential}
	envelope, err := nodeprotocol.SealCredentialGrant(agent.sealingPublicKey, grant, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCredentialGrant(context.Background(), "server-1", envelope, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agent.storageSourceCredential(context.Background(), "server-1", input, plan, run); err == nil {
		t.Fatal("grant URI drifted from the immutable source content digest")
	}
}

func TestValidateStorageSourceRootKeepsOfflineOutputInsideFrozenDestination(t *testing.T) {
	magnetPlan := nodeprotocol.StorageSourcePlan{SourceKind: nodeprotocol.StorageSourceKindPan115OfflineMagnet, OfflineDestinationID: "download-root"}
	run := StorageSourceRun{OutputRootID: "output-1"}
	if err := validateStorageSourceRoot(magnetPlan, run, cloud.Item{ID: "output-1", ParentID: "download-root", Name: "Movie.mkv"}); err != nil {
		t.Fatal(err)
	}
	for _, item := range []cloud.Item{
		{ID: "output-1", ParentID: "other-root", Name: "Movie.mkv"},
		{ID: "download-root", ParentID: "0", Name: "Downloads", IsDir: true},
	} {
		if err := validateStorageSourceRoot(magnetPlan, run, item); err == nil {
			t.Fatalf("unsafe offline output root accepted: %+v", item)
		}
	}
	sharePlan := nodeprotocol.StorageSourcePlan{SourceKind: nodeprotocol.StorageSourceKindPan115Share, OfflineDestinationID: "share-root"}
	if err := validateStorageSourceRoot(sharePlan, StorageSourceRun{OutputRootID: "share-root"}, cloud.Item{ID: "share-root", ParentID: "0", Name: "Share Root", IsDir: true}); err != nil {
		t.Fatal(err)
	}
}

func waitStorageSourceError(t *testing.T, store *Store, requestID, status, errorCode string) nodeprotocol.StorageSourceActionResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err := store.ProviderReceipt(context.Background(), "server-1", requestID)
		if err == nil && receipt.ResponseJSON != "" {
			var response nodeprotocol.StorageSourceActionResponse
			if json.Unmarshal([]byte(receipt.ResponseJSON), &response) == nil && response.Status == status && response.ErrorCode == errorCode {
				return response
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("source action did not reach %s/%s", status, errorCode)
	return nodeprotocol.StorageSourceActionResponse{}
}

func waitStorageSourceStatus(t *testing.T, store *Store, requestID, status string) nodeprotocol.StorageSourceActionResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err := store.ProviderReceipt(context.Background(), "server-1", requestID)
		if err == nil && receipt.ResponseJSON != "" {
			var response nodeprotocol.StorageSourceActionResponse
			if json.Unmarshal([]byte(receipt.ResponseJSON), &response) == nil && response.Status == status {
				return response
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("source action did not reach %s", status)
	return nodeprotocol.StorageSourceActionResponse{}
}

func (d *fakeStorageSourceDriver) InspectShareDirectory(ctx context.Context, raw, id string) (cloud.ShareSnapshot, error) {
	if id != "0" {
		return cloud.ShareSnapshot{}, errors.New("unexpected directory")
	}
	return d.InspectShare(ctx, raw)
}
func (d *fakeStorageSourceDriver) CreateDirectory(context.Context, string, string) (cloud.Item, error) {
	return cloud.Item{}, errors.New("unexpected mkdir")
}
func (d *fakeStorageSourceDriver) Move(context.Context, string, string) error {
	return errors.New("unexpected move")
}
func (d *fakeStorageSourceDriver) Copy(context.Context, string, string) error {
	return errors.New("unexpected copy")
}
func (d *fakeStorageSourceDriver) Rename(context.Context, string, string) error {
	return errors.New("unexpected rename")
}
func (d *fakeStorageSourceDriver) Recycle(context.Context, string) error {
	return errors.New("unexpected recycle")
}

func TestShareFailuresPreserveSafeNodeClassification(t *testing.T) {
	for provider, expected := range map[string]string{cloud.CodeShareExpired: nodeprotocol.ErrorSourceShareExpired, cloud.CodeSharePassword: nodeprotocol.ErrorSourceSharePassword} {
		got := storageSourceErrorCode(cloud.Error(provider, false, errors.New("private share URL")))
		if got != expected || strings.Contains(storageSourceSafeMessage(got), "private") {
			t.Fatalf("code=%s", got)
		}
	}
}
