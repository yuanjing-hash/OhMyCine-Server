package nodeagent

import (
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
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

type fakeStorageDriver struct {
	mu          sync.Mutex
	before      func(context.Context) error
	items       map[string]cloud.Item
	children    map[string][]string
	next        int
	uploadCalls int
}

func newFakeStorageDriver(before func(context.Context) error) *fakeStorageDriver {
	return &fakeStorageDriver{before: before, items: map[string]cloud.Item{"0": {ID: "0", Name: "root", IsDir: true}}, children: map[string][]string{}}
}

func (driver *fakeStorageDriver) Provider() string { return nodeprotocol.StorageProviderPan115 }
func (driver *fakeStorageDriver) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{DirectoryList: true, FileUpload: true, CreateDirectory: true, Recycle: true}
}
func (driver *fakeStorageDriver) Probe(context.Context) (cloud.Account, error) {
	return cloud.Account{}, nil
}
func (driver *fakeStorageDriver) DirectURL(context.Context, cloud.DirectURLRequest) (cloud.TemporaryURL, error) {
	return cloud.TemporaryURL{}, errors.New("not implemented")
}
func (driver *fakeStorageDriver) check(ctx context.Context) error {
	if driver.before != nil {
		return driver.before(ctx)
	}
	return nil
}
func (driver *fakeStorageDriver) Stat(ctx context.Context, id string) (cloud.Item, error) {
	if err := driver.check(ctx); err != nil {
		return cloud.Item{}, err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	item, ok := driver.items[id]
	if !ok {
		return cloud.Item{}, cloud.Error(cloud.CodeNotFound, false, nil)
	}
	return item, nil
}
func (driver *fakeStorageDriver) List(ctx context.Context, parentID string, page cloud.PageRequest) (cloud.Page, error) {
	if err := driver.check(ctx); err != nil {
		return cloud.Page{}, err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	ids := driver.children[parentID]
	start := int(page.Offset)
	if start > len(ids) {
		start = len(ids)
	}
	end := min(len(ids), start+int(page.Limit))
	items := make([]cloud.Item, 0, end-start)
	for _, id := range ids[start:end] {
		items = append(items, driver.items[id])
	}
	return cloud.Page{Items: items, Offset: page.Offset, HasMore: end < len(ids)}, nil
}
func (driver *fakeStorageDriver) CreateDirectory(ctx context.Context, parentID, name string) (cloud.Item, error) {
	if err := driver.check(ctx); err != nil {
		return cloud.Item{}, err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.next++
	item := cloud.Item{ID: "dir-" + string(rune('0'+driver.next)), ParentID: parentID, Name: name, IsDir: true}
	driver.items[item.ID] = item
	driver.children[parentID] = append(driver.children[parentID], item.ID)
	return item, nil
}
func (driver *fakeStorageDriver) Upload(ctx context.Context, request cloud.UploadRequest) (cloud.Item, error) {
	if err := driver.check(ctx); err != nil {
		return cloud.Item{}, err
	}
	body, err := io.ReadAll(request.Reader)
	if err != nil {
		return cloud.Item{}, err
	}
	digest := sha1.Sum(body)
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.uploadCalls++
	driver.next++
	item := cloud.Item{ID: "file-" + string(rune('0'+driver.next)), ParentID: request.ParentID, Name: request.Name, Size: request.Size, SHA1: strings.ToUpper(hex.EncodeToString(digest[:]))}
	driver.items[item.ID] = item
	driver.children[request.ParentID] = append(driver.children[request.ParentID], item.ID)
	return item, nil
}
func (driver *fakeStorageDriver) Recycle(ctx context.Context, id string) error {
	if err := driver.check(ctx); err != nil {
		return err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	item, ok := driver.items[id]
	if !ok {
		return cloud.Error(cloud.CodeNotFound, false, nil)
	}
	delete(driver.items, id)
	ids := driver.children[item.ParentID]
	for index, child := range ids {
		if child == id {
			driver.children[item.ParentID] = append(ids[:index], ids[index+1:]...)
			break
		}
	}
	return nil
}

func TestStorageUploadRunsInBackgroundPersistsCheckpointAndReusesReceipt(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "managed")
	if err := os.MkdirAll(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceBody := []byte("managed remote media")
	sourcePath := filepath.Join(managed, "episode.mkv")
	if err := os.WriteFile(sourcePath, sourceBody, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	agent, err := New(Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: root, ManagedRoot: managed, SealingPrivateKeyFile: filepath.Join(root, "missing.key"), MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}, store)
	if err != nil {
		t.Fatal(err)
	}
	var fake *fakeStorageDriver
	agent.storageFactory = func(_ nodeprotocol.StorageCredential, before func(context.Context) error) (storageUploadDriver, error) {
		if fake == nil {
			fake = newFakeStorageDriver(before)
		} else {
			fake.before = before
		}
		return fake, nil
	}
	now := time.Now().UTC()
	export, err := buildFileExport(context.Background(), managed, "server-1", "export-1", ManagedDownload{TaskID: "task-1", NodeLocalRoot: managed}, downloadpkg.Manifest{Name: "episode", Complete: true, Files: []downloadpkg.File{{RelativePath: "episode.mkv", Size: int64(len(sourceBody))}}}, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveFileExport(context.Background(), "server-1", export); err != nil {
		t.Fatal(err)
	}
	file := export.Files[0].Public
	uploadPlan := nodeprotocol.StorageUploadPlan{StorageID: "storage-1", ProviderType: nodeprotocol.StorageProviderPan115, SourceExportOperationKey: "export-1", SourceManifestDigest: export.Summary.ManifestDigest, TargetRootID: "0", Files: []nodeprotocol.StorageUploadFile{{SourceFileToken: file.FileToken, TargetRelativePath: "TV/Season 01/Episode 01.mkv", Size: file.Size, SHA256: file.SHA256, ConflictAction: nodeprotocol.StorageConflictFailIfExists}}}
	payload, _ := json.Marshal(uploadPlan)
	operation := nodeprotocol.OperationPlan{ProtocolVersion: nodeprotocol.VersionV1, OperationKey: "upload-1", TaskID: "task-1", NodeID: "node-1", Kind: nodeprotocol.OperationKindStorageUpload, PlanRevision: 1, LeaseEpoch: 1, LeaseExpiresAt: now.Add(5 * time.Minute), Payload: payload}
	planDigest, _ := operation.Digest()
	if _, _, err := store.Put(context.Background(), "server-1", planDigest, operation, now); err != nil {
		t.Fatal(err)
	}
	credential, _ := json.Marshal(nodeprotocol.StorageCredential{ProviderType: nodeprotocol.StorageProviderPan115, Cookie: "UID=secret; CID=secret; SEID=secret"})
	grant := nodeprotocol.CredentialGrant{GrantID: "grant-1", NodeID: "node-1", TaskID: "task-1", OperationKey: "upload-1", ResourceKind: nodeprotocol.ResourceKindStorage, ResourceID: "storage-1", CredentialRevision: 1, AllowedActions: []string{nodeprotocol.StorageActionUpload}, ExpiresAt: now.Add(5 * time.Minute), Credential: credential}
	envelope, err := nodeprotocol.SealCredentialGrant(agent.sealingPublicKey, grant, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCredentialGrant(context.Background(), "server-1", envelope, now); err != nil {
		t.Fatal(err)
	}
	testServer := httptest.NewServer(agent.Handler())
	defer testServer.Close()
	action := nodeprotocol.StorageActionRequest{RequestID: "request-1", TaskID: "task-1", OperationKey: "upload-1", PlanDigest: planDigest, StorageID: "storage-1", Action: nodeprotocol.StorageActionUpload, GrantID: "grant-1"}
	doNodeJSON(t, testServer.URL+"/node/v1/storage/actions", http.MethodPost, action, http.StatusAccepted)
	response := waitStorageReceipt(t, store, "request-1")
	if response.Status != nodeprotocol.StorageActionCompleted || response.CompletedFiles != 1 || len(response.Files) != 1 || response.Files[0].TargetParentID == "" || response.Files[0].TargetName != "Episode 01.mkv" || response.Files[0].TargetItemID == "" {
		t.Fatalf("response=%+v", response)
	}
	if fake.uploadCalls != 1 {
		t.Fatalf("upload calls=%d", fake.uploadCalls)
	}
	checkpoint, err := store.StorageUploadCheckpoint(context.Background(), "upload-1", file.FileToken)
	if err != nil || checkpoint.Status != nodeprotocol.StorageFileCompleted || checkpoint.TargetItemID != response.Files[0].TargetItemID {
		t.Fatalf("checkpoint=%+v err=%v", checkpoint, err)
	}
	var persisted string
	if err := store.db.QueryRow(`SELECT envelope_json FROM credential_grants WHERE grant_id='grant-1'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted, "UID=secret") {
		t.Fatal("decrypted 115 Cookie was persisted")
	}
	resultRequest, _ := http.NewRequest(http.MethodGet, testServer.URL+"/node/v1/storage/actions/request-1", nil)
	resultRequest.Header.Set("X-OhMyCine-Server-ID", "server-1")
	resultRequest.Header.Set(storageTaskHeader, "task-1")
	resultRequest.Header.Set(storageOperationHeader, "upload-1")
	resultHTTP, err := http.DefaultClient.Do(resultRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resultHTTP.Body.Close() }()
	if resultHTTP.StatusCode != http.StatusOK {
		t.Fatalf("result query status=%d", resultHTTP.StatusCode)
	}
	repeated := doNodeJSON(t, testServer.URL+"/node/v1/storage/actions", http.MethodPost, action, http.StatusOK)
	var repeatedResponse nodeprotocol.StorageActionResponse
	if err := json.Unmarshal(repeated, &repeatedResponse); err != nil || repeatedResponse.Status != nodeprotocol.StorageActionCompleted || fake.uploadCalls != 1 {
		t.Fatalf("repeat=%s calls=%d err=%v", repeated, fake.uploadCalls, err)
	}
}

func waitStorageReceipt(t *testing.T, store *Store, requestID string) nodeprotocol.StorageActionResponse {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err := store.ProviderReceipt(context.Background(), "server-1", requestID)
		if err == nil && receipt.Status == "completed" {
			var response nodeprotocol.StorageActionResponse
			if json.Unmarshal([]byte(receipt.ResponseJSON), &response) != nil {
				t.Fatal("invalid stored response")
			}
			return response
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("storage upload did not complete")
	return nodeprotocol.StorageActionResponse{}
}
