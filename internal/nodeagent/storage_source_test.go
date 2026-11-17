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
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type fakeStorageSourceDriver struct {
	mu            sync.Mutex
	before        func(context.Context) error
	taskID        string
	complete      bool
	requiredReady bool
	submitCalls   int
	cancelCalls   int
	cancelFiles   bool
	loseCancel    bool
	cancelApplied bool
	extras        bool
	rejectSubmit  bool
	failed        bool
	receiveCalls  int
	readOffsets   []int64
	failReads     map[int64]int
	share         cloud.ShareSnapshot
	shareItems    []cloud.Item
	receiveErr    error
	body          []byte
	sha1          string
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
	items := driver.shareItems
	switch parentID {
	case "share-root":
	case "output-1":
		items = []cloud.Item{{ID: "season-1", ParentID: parentID, Name: "Season 01", IsDir: true}}
	case "season-1":
		items = []cloud.Item{{ID: "provider-file-1", ParentID: parentID, Name: "Episode 01.mkv", Size: int64(len(driver.body)), SHA1: driver.sha1}}
	default:
		return cloud.Page{}, nil
	}
	start := int(request.Offset)
	if start >= len(items) {
		return cloud.Page{Offset: request.Offset}, nil
	}
	end := min(len(items), start+int(request.Limit))
	return cloud.Page{Items: append([]cloud.Item(nil), items[start:end]...), Offset: request.Offset, HasMore: end < len(items)}, nil
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
	if id == "season-1" {
		return cloud.Item{ID: id, ParentID: "output-1", Name: "Season 01", IsDir: true}, nil
	}
	if id == "provider-file-1" {
		return cloud.Item{ID: id, ParentID: "season-1", Name: "Episode 01.mkv", Size: int64(len(driver.body)), SHA1: driver.sha1}, nil
	}
	return cloud.Item{}, cloud.Error(cloud.CodeNotFound, false, nil)
}
func (driver *fakeStorageSourceDriver) FinalizedFiles(ctx context.Context, leaves []cloud.Item) ([]cloud.FinalizedFile, error) {
	if err := driver.check(ctx); err != nil {
		return nil, err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if !driver.requiredReady {
		return nil, nil
	}
	var facts []cloud.FinalizedFile
	for _, leaf := range leaves {
		if leaf.ID != "provider-file-1" || leaf.ParentID != "season-1" || leaf.Name != "Episode 01.mkv" || leaf.Size != int64(len(driver.body)) {
			return nil, errors.New("unowned finalized leaf")
		}
		facts = append(facts, cloud.FinalizedFile{Item: leaf, State: cloud.OfflineFinalizationComplete})
	}
	return facts, nil
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
	if err := cloud.RecordOfflineDispatch(ctx, time.Now().UTC()); err != nil {
		return cloud.OfflineTask{}, err
	}
	driver.submitCalls++
	if driver.rejectSubmit {
		if _, err := cloud.RecordOfflineNonAcceptance(ctx); err != nil {
			return cloud.OfflineTask{}, err
		}
		return cloud.OfflineTask{}, cloud.WithDefiniteSubmissionNonAcceptance(cloud.Error(cloud.CodeOfflineBadLink, false, nil))
	}
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
	task := cloud.OfflineTask{DirectoryID: "0", ID: driver.taskID, Status: "downloading", Progress: &progress}
	if driver.requiredReady {
		task.OutputItemID = "output-1"
	}
	if driver.complete {
		progress = 1
		task.Status, task.Progress, task.Completed, task.OutputItemID = "completed", &progress, true, "output-1"
	}
	if driver.failed {
		task.Failed = true
		task.Status = "failed"
	}
	return task
}
func (driver *fakeStorageSourceDriver) CancelOffline(ctx context.Context, id string, deleteFiles bool) error {
	if err := driver.check(ctx); err != nil {
		return err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if id != driver.taskID {
		return errors.New("wrong original task")
	}
	driver.cancelCalls++
	driver.cancelFiles = deleteFiles
	if driver.cancelApplied {
		driver.taskID = ""
	}
	if driver.loseCancel {
		return errors.New("lost cancel acknowledgement")
	}
	return nil
}
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
	entries := []cloud.TreeEntry{{Item: cloud.Item{ID: "provider-file-1", ParentID: rootID, Name: "Episode 01.mkv", Size: int64(len(driver.body)), SHA1: driver.sha1}, RelativePath: "/Season 01/Episode 01.mkv"}}
	if driver.extras {
		entries = append(entries, cloud.TreeEntry{Item: cloud.Item{ID: "extra", ParentID: rootID, Name: "bonus.bin", Size: 123, SHA1: strings.Repeat("F", 40)}, RelativePath: "/bonus.bin"})
	}
	return emit(cloud.TreeBatch{Offset: 0, Total: int64(len(entries)), Entries: entries})
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
	fake := &fakeStorageSourceDriver{extras: true, body: body, sha1: strings.ToUpper(hex.EncodeToString(digest[:]))}
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
	required := &downloader.OfflineRequiredPlan{Version: 1, RootName: "Show", MultiFile: true, Files: []downloader.OfflineRequiredFile{{RelativePath: "Season 01/Episode 01.mkv", Size: int64(len(body))}}}
	requiredDigest, _ := required.Digest()
	plan := nodeprotocol.StorageSourcePlan{OfflineRequiredDigest: requiredDigest, CloudOfflineLifecycleVersion: 1, OfflineAccountNamespace: offlineAccountNamespace(nodeprotocol.StorageProviderPan115, "1"), StorageID: "storage-a", ProviderType: nodeprotocol.StorageProviderPan115, SourceKind: nodeprotocol.StorageSourceKindPan115OfflineMagnet, TargetKind: nodeprotocol.StorageSourceTargetPan115, SourceIdentityDigest: strings.Repeat("1", 64), TargetIdentityDigest: strings.Repeat("2", 64), SourceContentDigest: contentDigest, OfflineDestinationID: "0"}
	payload, _ := json.Marshal(plan)
	operation := nodeprotocol.OperationPlan{ProtocolVersion: nodeprotocol.VersionV1, OperationKey: "source-1", TaskID: "task-1", NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageSourceMaterialize, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(10 * time.Minute), Payload: payload}
	planDigest, _ := operation.Digest()
	if _, _, err := store.Put(context.Background(), "server-1", planDigest, operation, now); err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(nodeprotocol.StorageSourceCredential{OfflineRequired: required, ProviderType: nodeprotocol.StorageProviderPan115, Cookie: "UID=1_A1; CID=secret; SEID=secret", SourceURI: magnet})
	grant := nodeprotocol.CredentialGrant{GrantID: "grant-1", NodeID: "node-1", TaskID: "task-1", OperationKey: "source-1", ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: "storage-a", CredentialRevision: 1, AllowedActions: []string{nodeprotocol.StorageSourceGrantOfflineStop, nodeprotocol.StorageSourceGrantOfflineSubmit, nodeprotocol.StorageSourceGrantOfflineStatus, nodeprotocol.StorageSourceGrantRead}, ExpiresAt: now.Add(10 * time.Minute), Credential: credential}
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
	waiting := waitStorageSourceStatus(t, store, action.RequestID, nodeprotocol.StorageSourceWaiting)
	if waiting.Offline == nil || waiting.Offline.StartedAt == nil || waiting.Offline.Ready {
		t.Fatal("missing original native dispatch clock")
	}
	started := *waiting.Offline.StartedAt
	waitStorageSourceIdle(t, agent, action.OperationKey)
	firstServer.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	if fake.submitCalls != 1 {
		t.Fatalf("submit calls=%d", fake.submitCalls)
	}

	// Simulate a Node process restart after the provider task receipt was
	// persisted. The next wake uses the stored provider task ID and must not
	// submit the same offline task again.
	fake.mu.Lock()
	fake.requiredReady = true
	fake.cancelApplied = true
	fake.failReads = map[int64]int{nodeprotocol.FileChunkSize: 1}
	fake.mu.Unlock()
	restarted, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	restarted.sourceFactory = agent.sourceFactory
	secondServer := httptest.NewServer(restarted.Handler())
	release, err := restarted.acquireOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	statusInput := action
	statusInput.RequestID = "cloud-status"
	statusInput.Action = nodeprotocol.StorageSourceActionOfflineStatus
	rawStatus := doNodeJSON(t, secondServer.URL+"/node/v1/storage-source/actions", http.MethodPost, statusInput, http.StatusOK)
	var cloudStatus nodeprotocol.StorageSourceActionResponse
	if json.Unmarshal(rawStatus, &cloudStatus) != nil || cloudStatus.Offline == nil || !cloudStatus.Offline.Ready || cloudStatus.Offline.StartedAt == nil || !cloudStatus.Offline.StartedAt.Equal(started) || len(fake.readOffsets) != 0 {
		release()
		t.Fatal("cloud status waited for data capacity or conflated readiness with materialization")
	}
	release()
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
	if completed.Offline == nil || !completed.Offline.Ready || completed.Offline.StartedAt == nil || !completed.Offline.StartedAt.Equal(started) || completed.Offline.CompletionMode != downloader.OfflineCompletionRequiredFiles || !completed.Offline.UpstreamStopped || fake.cancelCalls != 1 || fake.taskID != "" {
		t.Fatal("required-ready stop/transport did not preserve output after original native task removal")
	}
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

func TestStorageSourceCloudDeadlineControlsPreserveOriginalReceiptsAndPermissions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config := Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: dir, ManagedRoot: filepath.Join(dir, "managed"), SealingPrivateKeyFile: filepath.Join(dir, "seal.key"), MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}
	if err := EnsureSealingIdentity(config); err != nil {
		t.Fatal(err)
	}
	agent, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	agent.now = func() time.Time { return now }
	fake := &fakeStorageSourceDriver{taskID: strings.Repeat("a", 40), loseCancel: true}
	agent.sourceFactory = func(_ nodeprotocol.StorageSourceCredential, guard func(context.Context) error) (storageSourceDriver, error) {
		fake.before = guard
		return fake, nil
	}
	uri := "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)
	contentDigest, _ := nodeprotocol.StorageSourceContentDigest(nodeprotocol.StorageSourceKindPan115OfflineMagnet, uri)
	makeOperation := func(key string, legacy bool) (nodeprotocol.StorageSourceActionRequest, nodeprotocol.StorageSourcePlan, nodeprotocol.StorageSourceCredential) {
		t.Helper()
		plan := nodeprotocol.StorageSourcePlan{CloudOfflineLifecycleVersion: 1, OfflineAccountNamespace: offlineAccountNamespace(nodeprotocol.StorageProviderPan115, "1"), StorageID: "storage", ProviderType: nodeprotocol.StorageProviderPan115, SourceKind: nodeprotocol.StorageSourceKindPan115OfflineMagnet, TargetKind: nodeprotocol.StorageSourceTargetLocal, SourceIdentityDigest: strings.Repeat("1", 64), SourceContentDigest: contentDigest, OfflineDestinationID: "0"}
		if legacy {
			plan.CloudOfflineLifecycleVersion = 0
			plan.OfflineAccountNamespace = ""
		}
		raw, _ := json.Marshal(plan)
		operation := nodeprotocol.OperationPlan{ProtocolVersion: 1, OperationKey: key, TaskID: "task-" + key, NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageSourceMaterialize, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(10 * time.Minute), Payload: raw}
		digest, _ := operation.Digest()
		if _, _, err := store.Put(ctx, "server-1", digest, operation, now); err != nil {
			t.Fatal(err)
		}
		input := nodeprotocol.StorageSourceActionRequest{RequestID: key, TaskID: operation.TaskID, OperationKey: key, PlanDigest: digest, StorageID: "storage", Action: nodeprotocol.StorageSourceActionMaterialize}
		if _, err := store.PrepareStorageSourceRun(ctx, "server-1", input, now); err != nil {
			t.Fatal(err)
		}
		credential := nodeprotocol.StorageSourceCredential{ProviderType: nodeprotocol.StorageProviderPan115, Cookie: "UID=1_A1; CID=private; SEID=private", SourceURI: uri}
		return input, plan, credential
	}
	saveGrant := func(input nodeprotocol.StorageSourceActionRequest, credential nodeprotocol.StorageSourceCredential, id string, actions []string) {
		t.Helper()
		raw, _ := json.Marshal(credential)
		grant := nodeprotocol.CredentialGrant{GrantID: id, NodeID: "node-1", TaskID: input.TaskID, OperationKey: input.OperationKey, ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: "storage", CredentialRevision: 1, AllowedActions: actions, ExpiresAt: now.Add(10 * time.Minute), Credential: raw}
		envelope, err := nodeprotocol.SealCredentialGrant(agent.sealingPublicKey, grant, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SaveCredentialGrant(ctx, "server-1", envelope, now); err != nil {
			t.Fatal(err)
		}
	}
	input, plan, credential := makeOperation("original", false)
	hold, err := agent.offlineLock(ctx, "server-1", input.OperationKey)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancelLockWait := context.WithCancel(ctx)
	cancelLockWait()
	if release, err := agent.offlineLock(cancelled, "server-1", input.OperationKey); !errors.Is(err, context.Canceled) || release != nil {
		t.Fatal("cancelled deadline observation was blocked behind native control", err)
	}
	hold()
	ledger, err := agent.loadOfflineLedger(ctx, "server-1", input, plan, credential, true)
	if err != nil {
		t.Fatal(err)
	}
	started := now.Add(-31 * time.Minute)
	ledger.state.DispatchedAt = &started
	ledger.state.AcceptedAt = &started
	if err := ledger.save(ctx, "accepted", now); err != nil {
		t.Fatal(err)
	}
	if err := agent.publishOfflineLifecycle(ctx, "server-1", input, plan, ledger); err != nil {
		t.Fatal(err)
	}
	allActions := []string{nodeprotocol.StorageSourceGrantRead, nodeprotocol.StorageSourceGrantOfflineStatus, nodeprotocol.StorageSourceGrantOfflineStop}
	saveGrant(input, credential, "current", allActions)
	saveGrant(input, credential, "old-no-stop", []string{nodeprotocol.StorageSourceGrantRead, nodeprotocol.StorageSourceGrantOfflineStatus})
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	control := input
	control.Action = nodeprotocol.StorageSourceActionOfflineStop
	control.GrantID = "old-no-stop"
	doNodeJSON(t, server.URL+"/node/v1/storage-source/actions", http.MethodPost, control, http.StatusForbidden)
	if fake.cancelCalls != 0 {
		t.Fatal("legacy grant widened stop permission")
	}
	wrong := credential
	wrong.Cookie = "UID=2_A1; CID=private; SEID=private"
	saveGrant(input, wrong, "wrong-account", allActions)
	control.GrantID = "wrong-account"
	doNodeJSON(t, server.URL+"/node/v1/storage-source/actions", http.MethodPost, control, http.StatusConflict)
	if fake.cancelCalls != 0 {
		t.Fatal("different account canceled original task")
	}
	release, err := agent.acquireOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := agent.ReconcileCloudOfflineDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	agent.offlineStopWG.Wait()
	run, err := store.StorageSourceRun(ctx, "server-1", input.OperationKey)
	if err != nil || run.Offline == nil || !run.Offline.DeadlineExceeded || !run.Offline.StopPending || run.Offline.UpstreamStopped || fake.cancelCalls != 1 || fake.cancelFiles {
		t.Fatal("deadline did not persist local cancel with truthful keep-output pending stop", err, run)
	}
	control.GrantID = "current"
	doNodeJSON(t, server.URL+"/node/v1/storage-source/actions", http.MethodPost, control, http.StatusOK)
	if fake.cancelCalls != 1 {
		t.Fatal("lost stop ACK replayed native cancel")
	}
	fake.mu.Lock()
	fake.taskID = ""
	fake.mu.Unlock()
	control.Action = nodeprotocol.StorageSourceActionOfflineStatus
	raw := doNodeJSON(t, server.URL+"/node/v1/storage-source/actions", http.MethodPost, control, http.StatusOK)
	var response nodeprotocol.StorageSourceActionResponse
	if json.Unmarshal(raw, &response) != nil || response.Offline == nil || !response.Offline.UpstreamStopped || response.Offline.StopPending || !response.Offline.DeadlineExceeded || !response.Offline.StartedAt.Equal(started) || fake.cancelCalls != 1 {
		t.Fatal("read-only reconciliation lost original cancellation/start proof")
	}
	legacy, _, legacyCredential := makeOperation("legacy", true)
	saveGrant(legacy, legacyCredential, "legacy-expanded", allActions)
	legacy.Action = nodeprotocol.StorageSourceActionOfflineStop
	legacy.GrantID = "legacy-expanded"
	doNodeJSON(t, server.URL+"/node/v1/storage-source/actions", http.MethodPost, legacy, http.StatusConflict)
	disconnected, disconnectedPlan, disconnectedCredential := makeOperation("disconnected", false)
	lostLedger, err := agent.loadOfflineLedger(ctx, "server-1", disconnected, disconnectedPlan, disconnectedCredential, true)
	if err != nil {
		t.Fatal(err)
	}
	lostLedger.state.DispatchedAt = &started
	if err := lostLedger.save(ctx, "entered", now); err != nil {
		t.Fatal(err)
	}
	if err := agent.publishOfflineLifecycle(ctx, "server-1", disconnected, disconnectedPlan, lostLedger); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Minute)
	if err := agent.ReconcileCloudOfflineDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	pending, _ := store.StorageSourceRun(ctx, "server-1", disconnected.OperationKey)
	if pending.Offline == nil || !pending.Offline.DeadlineExceeded || !pending.Offline.StopPending || fake.cancelCalls != 1 {
		t.Fatal("expired lease/grant either lost local timeout or invented upstream cancellation")
	}
	rejected, rejectedPlan, rejectedCredential := makeOperation("rejected", false)
	fake.taskID = ""
	fake.rejectSubmit = true
	rejectedGrant := nodeprotocol.CredentialGrant{AllowedActions: append(append([]string(nil), allActions...), nodeprotocol.StorageSourceGrantOfflineSubmit), ExpiresAt: now.Add(10 * time.Minute)}
	fake.before = nil
	if _, err := agent.progressCloudOffline(ctx, "server-1", rejected, rejectedPlan, rejectedGrant, rejectedCredential, fake, true); !downloader.DefiniteSubmissionNonAcceptance(err) {
		t.Fatal("explicit native rejection lost durable Node no-task proof", err)
	}
	rejectedRun, _ := store.StorageSourceRun(ctx, "server-1", rejected.OperationKey)
	if rejectedRun.Offline == nil || !rejectedRun.Offline.DefiniteNonAcceptance || rejectedRun.Offline.StopPending {
		t.Fatal("rejected native submission remained physically admitted")
	}
	now = now.Add(40 * time.Minute)
	if err := agent.ReconcileCloudOfflineDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	rejectedRun, _ = store.StorageSourceRun(ctx, "server-1", rejected.OperationKey)
	if rejectedRun.Offline.DeadlineExceeded || fake.cancelCalls != 1 {
		t.Fatal("no-task rejection was blacklisted as a timed-out acquisition")
	}

	terminal, terminalPlan, terminalCredential := makeOperation("terminal", false)
	fake.taskID = strings.Repeat("a", 40)
	fake.rejectSubmit = false
	fake.failed = true
	terminalLedger, err := agent.loadOfflineLedger(ctx, "server-1", terminal, terminalPlan, terminalCredential, true)
	if err != nil {
		t.Fatal(err)
	}
	terminalStart := now.Add(-31 * time.Minute)
	terminalLedger.state.DispatchedAt = &terminalStart
	terminalLedger.state.AcceptedAt = &terminalStart
	if err := terminalLedger.save(ctx, "accepted", now); err != nil {
		t.Fatal(err)
	}
	if err := agent.publishOfflineLifecycle(ctx, "server-1", terminal, terminalPlan, terminalLedger); err != nil {
		t.Fatal(err)
	}
	fake.before = nil
	if _, err := agent.progressCloudOffline(ctx, "server-1", terminal, terminalPlan, rejectedGrant, terminalCredential, fake, false); err != nil {
		t.Fatal(err)
	}
	terminalRun, _ := store.StorageSourceRun(ctx, "server-1", terminal.OperationKey)
	if terminalRun.Offline == nil || !terminalRun.Offline.TerminalFailure || terminalRun.Status != nodeprotocol.StorageSourceFailed {
		t.Fatal("original native terminal failure was reduced to transient transport failure")
	}
	if err := agent.ReconcileCloudOfflineDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	terminalRun, _ = store.StorageSourceRun(ctx, "server-1", terminal.OperationKey)
	if terminalRun.Offline.DeadlineExceeded {
		t.Fatal("original native terminal failure became deadline history")
	}

	var ciphertext []byte
	if err := store.db.QueryRow(`SELECT ciphertext FROM cloud_operation_receipts WHERE operation_key='original' AND receipt_id='cloud-offline-lifecycle'`).Scan(&ciphertext); err != nil || bytes.Contains(ciphertext, []byte("account_id")) || bytes.Contains(ciphertext, []byte("dispatched_at")) {
		t.Fatal("native lifecycle receipt was not private encrypted state")
	}
}
