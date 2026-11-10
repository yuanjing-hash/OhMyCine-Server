package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestCompletedQBittorrentManifestPublishesPersistentPagedExport(t *testing.T) {
	root := t.TempDir()
	managedRoot := filepath.Join(root, "managed")
	mountRoot := filepath.Join(root, "downloads")
	downloadRoot := filepath.Join(mountRoot, "task-1")
	if err := os.MkdirAll(filepath.Join(downloadRoot, "Movie"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A real grant canonicalizes the Node mount before recording task paths.
	canonicalMount, err := filepath.EvalSymlinks(mountRoot)
	if err != nil {
		t.Fatal(err)
	}
	mountRoot, downloadRoot = canonicalMount, filepath.Join(canonicalMount, "task-1")
	files := map[string][]byte{
		"Movie/Movie.mkv": []byte("video-payload"),
		"Movie/Movie.srt": []byte("subtitle-payload"),
	}
	for relative, payload := range files {
		filename := filepath.Join(downloadRoot, filepath.FromSlash(relative))
		if err := os.WriteFile(filename, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var providerMu sync.Mutex
	providerAvailable := true
	providerRequests := 0
	locationMoves := 0
	qbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerMu.Lock()
		providerRequests++
		available := providerAvailable
		providerMu.Unlock()
		if !available {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session"})
			_, _ = io.WriteString(w, "Ok.")
		case "/api/v2/torrents/info":
			_, _ = io.WriteString(w, `[{"hash":"hash-1","name":"Movie","state":"stalledUP","progress":1,"downloaded":29,"total_size":29}]`)
		case "/api/v2/torrents/setCategory":
		case "/api/v2/torrents/setLocation":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			target := r.Form.Get("location")
			if target != filepath.Join(downloadRoot, "电视剧") {
				t.Error("unexpected category location")
				w.WriteHeader(400)
				return
			}
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Error(err)
				return
			}
			if err := os.Rename(filepath.Join(downloadRoot, "Movie"), filepath.Join(target, "Movie")); err != nil {
				t.Error(err)
				return
			}
			locationMoves++
		case "/api/v2/torrents/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "Movie/Movie.mkv", "size": len(files["Movie/Movie.mkv"])},
				{"name": "Movie/Movie.srt", "size": len(files["Movie/Movie.srt"])},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer qbit.Close()
	databasePath := filepath.Join(root, "node.db")
	store, err := OpenStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// Keep the two historical tables in their exact server-v1.1.70 shape.
	// Completion-location recovery must read the original columns and receipt.
	for _, statement := range []string{
		`DROP TABLE managed_downloads`,
		`DROP TABLE provider_receipts`,
		`CREATE TABLE provider_receipts (request_id TEXT PRIMARY KEY, paired_server_id TEXT NOT NULL, operation_key TEXT NOT NULL, request_digest TEXT NOT NULL, action TEXT NOT NULL, status TEXT NOT NULL, response_json TEXT NOT NULL DEFAULT '', error_code TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, UNIQUE(paired_server_id, request_id))`,
		`CREATE TABLE managed_downloads (id INTEGER PRIMARY KEY AUTOINCREMENT, paired_server_id TEXT NOT NULL, downloader_id TEXT NOT NULL, task_id TEXT NOT NULL, provider_task_id TEXT NOT NULL, tag TEXT NOT NULL DEFAULT '', downloader_save_path TEXT NOT NULL, node_local_root TEXT NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, UNIQUE(paired_server_id, downloader_id, provider_task_id))`,
	} {
		if _, err := store.db.ExecContext(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.SaveManagedDownload(context.Background(), ManagedDownload{
		ServerID:           "server-1",
		DownloaderID:       "downloader-1",
		TaskID:             "task-1",
		ProviderTaskID:     "hash-1",
		Tag:                "omc-task-1",
		DownloaderSavePath: downloadRoot,
		NodeLocalRoot:      downloadRoot,
	}, now); err != nil {
		t.Fatal(err)
	}
	config := Config{
		NodeID:                   "node-1",
		ListenAddress:            "127.0.0.1:0",
		DataDirectory:            root,
		ManagedRoot:              managedRoot,
		SealingPrivateKeyFile:    filepath.Join(root, "node.seal.key"),
		MaxConcurrentOperations:  2,
		AllowInsecureDevelopment: true,
	}
	if err := EnsureSealingIdentity(config); err != nil {
		t.Fatal(err)
	}
	agent, err := New(config, store)
	if err != nil {
		t.Fatal(err)
	}
	agent.now = func() time.Time { return now }
	node := httptest.NewServer(agent.Handler())
	t.Cleanup(node.Close)

	categoryPath := filepath.Join(downloadRoot, "电视剧")
	categoryKey := nodeprotocol.DownloaderOperationKey(nodeprotocol.DownloaderActionSetCategory, "downloader-1", "task-1", "hash-1", "电视剧", categoryPath)
	category := putManifestGrant(t, node.URL, agent, now, qbit.URL, mountRoot, "grant-category", categoryKey, nodeprotocol.DownloaderActionSetCategory)
	category.Action, category.CategoryName, category.SavePath = nodeprotocol.DownloaderActionSetCategory, "电视剧", categoryPath
	doNodeJSON(t, node.URL+"/node/v1/downloader/actions", http.MethodPost, category, http.StatusOK)
	if locationMoves != 1 {
		t.Fatal("normal category action did not relocate source")
	}
	// v1.1.70 canonicalized these ordinary wire fields and cleared GrantID.
	legacyWire, err := json.Marshal(map[string]any{"request_id": categoryKey, "task_id": "task-1", "operation_key": categoryKey, "downloader_id": "downloader-1", "action": "set_category", "grant_id": "", "provider_task_id": "hash-1", "category_name": "电视剧", "save_path": categoryPath})
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest := sha256.Sum256(legacyWire)
	receipt, err := store.ProviderReceipt(context.Background(), "server-1", categoryKey)
	if err != nil || receipt.RequestDigest != hex.EncodeToString(legacyDigest[:]) {
		t.Fatalf("ordinary category receipt drifted from v1.1.70: %+v %v", receipt, err)
	}

	const operationKey = "qb:file_export:task-1"
	action := putManifestGrant(t, node.URL, agent, now, qbit.URL, mountRoot, "grant-export-1", operationKey)
	legacy := doNodeJSON(t, node.URL+"/node/v1/downloader/actions", http.MethodPost, action, http.StatusBadGateway)
	var oldFailure nodeprotocol.ErrorResponse
	if json.Unmarshal(legacy, &oldFailure) != nil || oldFailure.Code != nodeprotocol.ErrorPathMappingInvalid {
		t.Fatalf("legacy completion failure=%s", legacy)
	}
	for _, path := range []string{filepath.Join(downloadRoot, "unrecorded"), filepath.Join(mountRoot, "another-task", "电视剧")} {
		action.CategoryName, action.SavePath = "unrecorded", path
		body := doNodeJSON(t, node.URL+"/node/v1/downloader/actions", http.MethodPost, action, http.StatusBadGateway)
		if json.Unmarshal(body, &oldFailure) != nil || oldFailure.Code != nodeprotocol.ErrorPathMappingInvalid {
			t.Fatalf("unreceipted path was accepted: %s", body)
		}
	}
	// Retry the same read-only export identity using the successful original
	// category receipt. No second relocation or submission is permitted.
	action.CategoryName, action.SavePath = category.CategoryName, category.SavePath
	body := doNodeJSON(t, node.URL+"/node/v1/downloader/actions", http.MethodPost, action, http.StatusBadGateway)
	var preparing nodeprotocol.ErrorResponse
	if err := json.Unmarshal(body, &preparing); err != nil || preparing.Code != nodeprotocol.ErrorExportPreparing {
		t.Fatalf("first manifest response=%s err=%v", body, err)
	}

	firstPage := waitForFileExport(t, store, operationKey, now)
	if firstPage.Summary.TaskID != "task-1" || firstPage.Summary.TotalFiles != 2 || firstPage.Summary.TotalBytes != int64(len(files["Movie/Movie.mkv"])+len(files["Movie/Movie.srt"])) || len(firstPage.Files) != 1 || !firstPage.HasMore {
		t.Fatalf("first page=%+v", firstPage)
	}
	secondPage, err := store.FileExportManifest(context.Background(), "server-1", operationKey, 2, 1, now)
	if err != nil || len(secondPage.Files) != 1 || secondPage.HasMore {
		t.Fatalf("second page=%+v err=%v", secondPage, err)
	}
	if firstPage.Files[0].FileToken == secondPage.Files[0].FileToken || firstPage.Files[0].SHA256 == "" || secondPage.Files[0].SHA256 == "" {
		t.Fatalf("invalid opaque export files: first=%+v second=%+v", firstPage.Files[0], secondPage.Files[0])
	}

	readyBody := doNodeJSON(t, node.URL+"/node/v1/downloader/actions", http.MethodPost, action, http.StatusOK)
	var ready nodeprotocol.DownloaderActionResponse
	if err := json.Unmarshal(readyBody, &ready); err != nil || ready.FileExport == nil || ready.FileExport.ManifestDigest != firstPage.Summary.ManifestDigest {
		t.Fatalf("ready response=%s err=%v", readyBody, err)
	}

	providerMu.Lock()
	providerAvailable = false
	requestsBeforeRemoval := providerRequests
	providerMu.Unlock()
	readyBody = doNodeJSON(t, node.URL+"/node/v1/downloader/actions", http.MethodPost, action, http.StatusOK)
	if err := json.Unmarshal(readyBody, &ready); err != nil || ready.FileExport == nil {
		t.Fatalf("persisted response=%s err=%v", readyBody, err)
	}
	providerMu.Lock()
	requestsAfterRemoval := providerRequests
	providerMu.Unlock()
	if requestsAfterRemoval != requestsBeforeRemoval {
		t.Fatalf("completed export still called deleted provider: before=%d after=%d", requestsBeforeRemoval, requestsAfterRemoval)
	}

	firstToken := firstPage.Files[0].FileToken
	agent.startFileExport("server-1", operationKey, mountRoot, ManagedDownload{TaskID: "task-1", NodeLocalRoot: downloadRoot}, downloadpkg.Manifest{Complete: true, Name: "Movie", Files: []downloadpkg.File{{RelativePath: "missing-after-export.mkv", Size: 1}}})
	agent.exportMu.Lock()
	runningAfterPersist := len(agent.exports)
	agent.exportMu.Unlock()
	if runningAfterPersist != 0 {
		t.Fatal("completed immutable export was scheduled for hashing again")
	}
	stablePage, err := store.FileExportManifest(context.Background(), "server-1", operationKey, 1, 1, now)
	if err != nil || stablePage.Files[0].FileToken != firstToken {
		t.Fatalf("repeated export changed file identity: page=%+v err=%v", stablePage, err)
	}

	node.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restartedAgent, err := New(config, reopened)
	if err != nil {
		t.Fatal(err)
	}
	restartedAgent.now = func() time.Time { return now }
	restartedNode := httptest.NewServer(restartedAgent.Handler())
	defer restartedNode.Close()
	restartBody := doNodeJSON(t, restartedNode.URL+"/node/v1/downloader/actions", http.MethodPost, action, http.StatusOK)
	var restarted nodeprotocol.DownloaderActionResponse
	if err := json.Unmarshal(restartBody, &restarted); err != nil || restarted.FileExport == nil || restarted.FileExport.ManifestDigest != firstPage.Summary.ManifestDigest {
		t.Fatalf("restart response=%s err=%v", restartBody, err)
	}
	providerMu.Lock()
	requestsAfterRestart := providerRequests
	providerMu.Unlock()
	if requestsAfterRestart != requestsBeforeRemoval {
		t.Fatalf("restart queried deleted provider: before=%d after=%d", requestsBeforeRemoval, requestsAfterRestart)
	}
}

func TestFileExportHashFailureCanRetryWithoutPublishingPartialState(t *testing.T) {
	root := t.TempDir()
	managedRoot := filepath.Join(root, "managed")
	downloadRoot := filepath.Join(managedRoot, "downloads", "task-1")
	if err := os.MkdirAll(downloadRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(downloadRoot, "movie.mkv")
	if err := os.WriteFile(filename, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	agent, err := New(Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: root, ManagedRoot: managedRoot, MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}, store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	agent.now = func() time.Time { return now }
	manifest := downloadpkg.Manifest{Name: "Movie", Complete: true, Files: []downloadpkg.File{{RelativePath: "movie.mkv", Size: 6}}}
	download := ManagedDownload{TaskID: "task-1", NodeLocalRoot: downloadRoot}
	agent.startFileExport("server-1", "export:retry", managedRoot, download, manifest)
	waitForExportIdle(t, agent, "export:retry")
	if _, err := store.FileExportManifest(context.Background(), "server-1", "export:retry", 1, 10, now); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("failed hash published partial export: %v", err)
	}
	var persisted int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM file_exports WHERE operation_key='export:retry'`).Scan(&persisted); err != nil || persisted != 0 {
		t.Fatalf("failed hash persisted export=%d err=%v", persisted, err)
	}

	if err := os.WriteFile(filename, []byte("fixed!"), 0o600); err != nil {
		t.Fatal(err)
	}
	agent.startFileExport("server-1", "export:retry", managedRoot, download, manifest)
	page := waitForFileExport(t, store, "export:retry", now)
	if page.Summary.TotalFiles != 1 || page.Summary.TotalBytes != 6 || page.Files[0].SHA256 == "" {
		t.Fatalf("retry export=%+v", page)
	}
}

func putManifestGrant(t *testing.T, nodeURL string, agent *Agent, now time.Time, qbitURL, mountRoot, grantID, operationKey string, actions ...string) nodeprotocol.DownloaderActionRequest {
	t.Helper()
	credential, err := json.Marshal(nodeprotocol.DownloaderCredential{ProviderType: "qbittorrent", BaseURL: qbitURL, DownloaderSaveRoot: mountRoot, NodeMountRoot: mountRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) == 0 {
		actions = []string{nodeprotocol.DownloaderActionManifest}
	}
	grant := nodeprotocol.CredentialGrant{GrantID: grantID, NodeID: "node-1", TaskID: "task-1", OperationKey: operationKey, ResourceKind: nodeprotocol.ResourceKindDownloader, ResourceID: "downloader-1", CredentialRevision: 1, AllowedActions: actions, ExpiresAt: now.Add(5 * time.Minute), Credential: credential}
	envelope, err := nodeprotocol.SealCredentialGrant(agent.sealingPublicKey, grant, now)
	if err != nil {
		t.Fatal(err)
	}
	doNodeJSON(t, nodeURL+"/node/v1/credential-grants/"+grantID, http.MethodPut, envelope, http.StatusNoContent)
	return nodeprotocol.DownloaderActionRequest{RequestID: operationKey, TaskID: "task-1", OperationKey: operationKey, DownloaderID: "downloader-1", Action: nodeprotocol.DownloaderActionManifest, GrantID: grantID, ProviderTaskID: "hash-1"}
}

func waitForFileExport(t *testing.T, store *Store, operationKey string, now time.Time) nodeprotocol.FileExportManifestPage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		page, err := store.FileExportManifest(context.Background(), "server-1", operationKey, 1, 1, now)
		if err == nil {
			return page
		}
		if !errors.Is(err, ErrOperationNotFound) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file export %s was not published", operationKey)
	return nodeprotocol.FileExportManifestPage{}
}

func waitForExportIdle(t *testing.T, agent *Agent, operationKey string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		agent.exportMu.Lock()
		_, running := agent.exports[operationKey]
		agent.exportMu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("file export %s did not become idle", operationKey)
}
