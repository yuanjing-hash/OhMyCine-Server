package nodeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func TestDownloaderRemoteOwnershipErrorsRemainTerminalAcrossHTTP(t *testing.T) {
	for _, code := range []string{"downloader_selection_identity_required", "downloader_selection_owner_conflict"} {
		t.Run(code, func(t *testing.T) {
			grants, actions := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					grants++
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/node/v1/downloader/actions" {
					http.NotFound(w, r)
					return
				}
				var request nodeprotocol.DownloaderActionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Action != nodeprotocol.DownloaderActionSubmit || request.TaskID != "task-42" {
					t.Error("lost actual submit binding", err)
				}
				actions++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(nodeprotocol.ErrorResponse{Code: code})
			}))
			defer server.Close()
			node := &Client{baseURL: server.URL, identity: Identity{ServerID: "server-1"}, http: server.Client()}
			client, err := NewDownloaderClient(node, "downloader-1", "/downloads", t.TempDir(), "task-42", func(context.Context, string, string, string) (nodeprotocol.CredentialGrantEnvelope, error) {
				return nodeprotocol.CredentialGrantEnvelope{GrantID: "grant-1"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Submit(context.Background(), downloadpkg.SubmitRequest{Tag: "omc-task-42", Source: downloadpkg.Source{Kind: downloadpkg.SourceURL, URL: "https://source.example/private-path/file.torrent?token=private-token"}})
			actual, retryable := downloadpkg.ErrorInfo(err)
			if actual != code || retryable || grants != 1 || actions != 1 {
				t.Fatalf("ownership conflict lost terminal classification: code=%q retryable=%t grants=%d actions=%d", actual, retryable, grants, actions)
			}
			if err == nil || strings.Contains(err.Error(), "private-path") || strings.Contains(err.Error(), "private-token") {
				t.Fatal("unsafe source detail reached error projection")
			}
		})
	}
}
