package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestDownloaderActionUsesSealedGrantWithoutPersistingPlaintext(t *testing.T) {
	qbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session"})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/app/version":
			_, _ = w.Write([]byte("v5.0.4"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer qbit.Close()
	root := t.TempDir()
	mount := filepath.Join(root, "downloads")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	agent, err := New(Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: root, ManagedRoot: root, SealingPrivateKeyFile: filepath.Join(root, "missing.key"), MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}, store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	credential, _ := json.Marshal(nodeprotocol.DownloaderCredential{ProviderType: "qbittorrent", BaseURL: qbit.URL, Username: "admin", Password: "very-secret-password", DownloaderSaveRoot: "/downloads", NodeMountRoot: mount})
	grant := nodeprotocol.CredentialGrant{GrantID: "grant-1", NodeID: "node-1", TaskID: "task-1", OperationKey: "operation-1", ResourceKind: nodeprotocol.ResourceKindDownloader, ResourceID: "downloader-1", CredentialRevision: 1, AllowedActions: []string{nodeprotocol.DownloaderActionTest}, ExpiresAt: now.Add(5 * time.Minute), Credential: credential}
	envelope, err := nodeprotocol.SealCredentialGrant(agent.sealingPublicKey, grant, now)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	doNodeJSON(t, server.URL+"/node/v1/credential-grants/grant-1", http.MethodPut, envelope, http.StatusNoContent)
	action := nodeprotocol.DownloaderActionRequest{RequestID: "request-1", TaskID: "task-1", OperationKey: "operation-1", DownloaderID: "downloader-1", Action: nodeprotocol.DownloaderActionTest, GrantID: "grant-1"}
	response := doNodeJSON(t, server.URL+"/node/v1/downloader/actions", http.MethodPost, action, http.StatusOK)
	var result nodeprotocol.DownloaderActionResponse
	if err := json.Unmarshal(response, &result); err != nil || result.Health == nil || result.Health.Version != "v5.0.4" {
		t.Fatalf("response=%s err=%v", response, err)
	}
	var persisted string
	if err := store.db.QueryRowContext(context.Background(), `SELECT envelope_json FROM credential_grants WHERE grant_id='grant-1'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(persisted), []byte("very-secret-password")) || bytes.Contains([]byte(persisted), []byte(qbit.URL)) {
		t.Fatal("node database persisted decrypted downloader credential")
	}
}

func TestDownloaderCategoryActionValidatesMappingAndReusesReceipt(t *testing.T) {
	createCount := 0
	qbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "session"})
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/torrents/categories":
			_, _ = w.Write([]byte(`{}`))
		case "/api/v2/torrents/createCategory":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("category") != "电视剧" || r.Form.Get("savePath") != "/downloads/task-1/电视剧" {
				t.Fatalf("category form=%v", r.Form)
			}
			createCount++
		default:
			http.NotFound(w, r)
		}
	}))
	defer qbit.Close()
	root := t.TempDir()
	mount := filepath.Join(root, "downloads")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	agent, err := New(Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: root, ManagedRoot: root, SealingPrivateKeyFile: filepath.Join(root, "missing.key"), MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}, store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(agent.Handler())
	defer server.Close()
	now := time.Now().UTC()
	credential, _ := json.Marshal(nodeprotocol.DownloaderCredential{ProviderType: "qbittorrent", BaseURL: qbit.URL, DownloaderSaveRoot: "/downloads", NodeMountRoot: mount})

	putGrant := func(grantID, operationKey string) {
		t.Helper()
		grant := nodeprotocol.CredentialGrant{GrantID: grantID, NodeID: "node-1", TaskID: "task-1", OperationKey: operationKey, ResourceKind: nodeprotocol.ResourceKindDownloader, ResourceID: "downloader-1", CredentialRevision: 1, AllowedActions: []string{nodeprotocol.DownloaderActionEnsureCategory}, ExpiresAt: now.Add(5 * time.Minute), Credential: credential}
		envelope, sealErr := nodeprotocol.SealCredentialGrant(agent.sealingPublicKey, grant, now)
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		doNodeJSON(t, server.URL+"/node/v1/credential-grants/"+grantID, http.MethodPut, envelope, http.StatusNoContent)
	}

	operationKey := "qb:ensure_category:stable"
	putGrant("grant-category", operationKey)
	action := nodeprotocol.DownloaderActionRequest{RequestID: operationKey, TaskID: "task-1", OperationKey: operationKey, DownloaderID: "downloader-1", Action: nodeprotocol.DownloaderActionEnsureCategory, GrantID: "grant-category", CategoryName: "电视剧", SavePath: "/downloads/task-1/电视剧"}
	doNodeJSON(t, server.URL+"/node/v1/downloader/actions", http.MethodPost, action, http.StatusOK)
	doNodeJSON(t, server.URL+"/node/v1/downloader/actions", http.MethodPost, action, http.StatusOK)
	if createCount != 1 {
		t.Fatalf("idempotent category action reached qB %d times", createCount)
	}

	badOperationKey := "qb:ensure_category:outside"
	putGrant("grant-outside", badOperationKey)
	action.RequestID, action.OperationKey, action.GrantID, action.SavePath = badOperationKey, badOperationKey, "grant-outside", "/outside/电视剧"
	body := doNodeJSON(t, server.URL+"/node/v1/downloader/actions", http.MethodPost, action, http.StatusBadGateway)
	var response nodeprotocol.ErrorResponse
	if err := json.Unmarshal(body, &response); err != nil || response.Code != nodeprotocol.ErrorPathMappingInvalid {
		t.Fatalf("response=%s err=%v", body, err)
	}
	if createCount != 1 {
		t.Fatal("outside category path reached qB")
	}
}

func doNodeJSON(t *testing.T, target, method string, input any, status int) []byte {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, target, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-OhMyCine-Server-ID", "server-1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != status {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	return body
}
