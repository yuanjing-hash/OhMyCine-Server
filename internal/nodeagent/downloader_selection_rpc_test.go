package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestSelectedPendingRPCReopensWithReencryptedSourceAndNewGrant(t *testing.T) {
	raw := []byte("d8:announce45:https://tracker.example/announce/tracker-secret4:infod6:lengthi10e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
	// Construct the outer container without touching the original info.
	info := []byte("d6:lengthi10e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaae")
	announce := "https://tracker.example/announce/tracker-secret"
	raw = append([]byte("d8:announce"+strconv.Itoa(len(announce))+":"+announce+"4:info"), info...)
	raw = append(raw, 'e')
	m, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	selection := &resourcecontent.Selection{Version: 1, Identity: m.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "movie.mkv", Size: 10}}}
	var mu sync.Mutex
	exists, failFiles := false, true
	adds := 0
	tags := ""
	qbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture"})
			io.WriteString(w, "Ok.")
		case "/api/v2/app/webapiVersion":
			io.WriteString(w, "2.11.3")
		case "/api/v2/torrents/info":
			if !exists {
				io.WriteString(w, "[]")
			} else {
				json.NewEncoder(w).Encode([]map[string]any{{"hash": m.Identity.V1, "name": "movie.mkv", "state": "stoppedDL", "tags": tags}})
			}
		case "/api/v2/torrents/add":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			file, _, e := r.FormFile("torrents")
			if e != nil {
				t.Error(e)
				return
			}
			body, _ := io.ReadAll(file)
			file.Close()
			if !bytes.Equal(body, raw) || r.FormValue("paused") != "true" {
				t.Error("did not send original stopped metainfo")
			}
			exists = true
			adds++
			tags = r.FormValue("tags")
			io.WriteString(w, "Ok.")
		case "/api/v2/torrents/files":
			if failFiles {
				failFiles = false
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{{"index": 7, "name": "movie.mkv", "size": 10, "priority": 1, "progress": 0}})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer qbit.Close()
	root := t.TempDir()
	mount := filepath.Join(root, "mount")
	if err = os.MkdirAll(mount, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: root, ManagedRoot: filepath.Join(root, "managed"), SealingPrivateKeyFile: filepath.Join(root, "sealing.key"), MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}
	dbpath := filepath.Join(root, "node.db")
	store, err := OpenStore(dbpath)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(agent.Handler())
	t.Cleanup(func() { server.Close(); _ = store.Close() })
	binding := nodeprotocol.DownloaderSourceBinding{ServerID: "server-1", TaskID: "task-1", OperationKey: "submit-operation", DownloaderID: "downloader-1", SavePath: "/downloads/task-1", Tag: "omc-task-1", ConfigurationScope: strings.Repeat("c", 64)}
	binding.TaskID = uuid.NewString()
	binding.Tag = "omc-" + binding.TaskID
	source := nodeprotocol.DownloaderSource{Kind: "torrent", Torrent: raw, Filename: "movie.torrent", ContentSelection: selection}
	seal := func(a *Agent, grantID string) (nodeprotocol.DownloaderActionRequest, nodeprotocol.CredentialGrantEnvelope) {
		envelope, key, e := nodeprotocol.SealDownloaderSource(source, binding)
		if e != nil {
			t.Fatal(e)
		}
		credential, _ := json.Marshal(nodeprotocol.DownloaderCredential{ProviderType: "qbittorrent", BaseURL: qbit.URL, DownloaderSaveRoot: "/downloads", NodeMountRoot: mount, SourceKey: key, SourceScope: binding.ConfigurationScope})
		now := time.Now().UTC()
		grant := nodeprotocol.CredentialGrant{GrantID: grantID, NodeID: "node-1", TaskID: binding.TaskID, OperationKey: binding.OperationKey, ResourceKind: nodeprotocol.ResourceKindDownloader, ResourceID: binding.DownloaderID, CredentialRevision: 1, AllowedActions: []string{nodeprotocol.DownloaderActionSubmit}, ExpiresAt: now.Add(time.Minute), Credential: credential}
		sealed, e := nodeprotocol.SealCredentialGrant(a.sealingPublicKey, grant, now)
		if e != nil {
			t.Fatal(e)
		}
		input := nodeprotocol.DownloaderActionRequest{RequestID: "same-business-request", TaskID: binding.TaskID, OperationKey: binding.OperationKey, DownloaderID: binding.DownloaderID, Action: nodeprotocol.DownloaderActionSubmit, GrantID: grantID, Submit: &nodeprotocol.DownloaderSubmit{Source: nodeprotocol.DownloaderSource{Kind: "torrent"}, SourceEnvelope: envelope, SavePath: binding.SavePath, Tag: binding.Tag}}
		return input, sealed
	}
	old, grant := seal(agent, "grant-old")
	doNodeJSON(t, server.URL+"/node/v1/credential-grants/grant-old", http.MethodPut, grant, http.StatusNoContent)
	firstResponse := doNodeJSON(t, server.URL+"/node/v1/downloader/actions", http.MethodPost, old, http.StatusBadGateway)
	if adds != 1 {
		t.Fatal("original stopped submit did not reach native provider", string(firstResponse))
	}
	// Reproduce a crash before the ACK is committed. The receipt is pending;
	// neither its business identity nor the existing native task is discarded.
	if _, err = store.db.Exec(`UPDATE provider_receipts SET status='started',response_json='',error_code='' WHERE request_id=?`, old.RequestID); err != nil {
		t.Fatal(err)
	}
	server.Close()
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(dbpath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent, err = New(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	server = httptest.NewServer(agent.Handler())
	defer server.Close()
	fresh, grant := seal(agent, "grant-refreshed")
	oldDigest, _ := old.Digest()
	newDigest, _ := fresh.Digest()
	if oldDigest != newDigest || bytes.Equal(old.Submit.SourceEnvelope.Ciphertext, fresh.Submit.SourceEnvelope.Ciphertext) {
		t.Fatal("business identity or re-encryption did not behave as required")
	}
	doNodeJSON(t, server.URL+"/node/v1/credential-grants/grant-refreshed", http.MethodPut, grant, http.StatusNoContent)
	body := doNodeJSON(t, server.URL+"/node/v1/downloader/actions", http.MethodPost, fresh, http.StatusOK)
	var response nodeprotocol.DownloaderActionResponse
	if json.Unmarshal(body, &response) != nil || response.Task == nil || response.Task.ID != m.Identity.V1 || adds != 1 {
		t.Fatal("pending request did not resume original native task", string(body), adds)
	}
	stored, provider, err := store.downloadSelection(context.Background(), "server-1", "downloader-1", binding.TaskID)
	if err != nil || provider != m.Identity.V1 || len(stored.Selection.Files) != 1 {
		t.Fatal("actual recovery did not persist own binding", err)
	}
	// A different plaintext under the same business key is still a conflict,
	// even if it has a fresh, valid credential envelope.
	source.Filename = "different-name.torrent"
	changed, grant := seal(agent, "grant-changed")
	doNodeJSON(t, server.URL+"/node/v1/credential-grants/grant-changed", http.MethodPut, grant, http.StatusNoContent)
	doNodeJSON(t, server.URL+"/node/v1/downloader/actions", http.MethodPost, changed, http.StatusConflict)
	if adds != 1 {
		t.Fatal("conflict submitted another native task")
	}
}
