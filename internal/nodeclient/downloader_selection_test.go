package nodeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestSelectedSubmitRejectsOldNodeBeforeGrantOrSubmission(t *testing.T) {
	requests := 0
	grants := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/node/v1/health" {
			t.Error("old Node received execution")
		}
		json.NewEncoder(w).Encode(nodeprotocol.HealthResponse{Capabilities: nodeprotocol.Capabilities{Codes: []string{nodeprotocol.CapabilityQBittorrentControl}}})
	}))
	defer server.Close()
	node := &Client{baseURL: server.URL, identity: Identity{ServerID: "server-1"}, http: server.Client()}
	client, _ := NewDownloaderClient(node, "downloader-1", "/downloads", t.TempDir(), "task-1", func(context.Context, string, string, string) (nodeprotocol.CredentialGrantEnvelope, error) {
		grants++
		return nodeprotocol.CredentialGrantEnvelope{}, nil
	})
	raw := []byte("d4:infod6:lengthi10e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
	m, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	selection := &resourcecontent.Selection{Version: 1, Identity: m.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "movie.mkv", Size: 10}}}
	_, err = client.Submit(context.Background(), downloadpkg.SubmitRequest{Source: downloadpkg.Source{Kind: downloadpkg.SourceTorrent, Torrent: raw, Filename: "movie.torrent", ContentSelection: selection}, Tag: "omc-task-1", SavePath: "/downloads/task-1"})
	if code, _ := downloadpkg.ErrorInfo(err); code != nodeprotocol.ErrorCapabilityMissing || requests != 1 || grants != 0 {
		t.Fatal(code, requests, grants)
	}
}

func TestSelectedSourceTravelsEncryptedAndUsesSealedGrantKey(t *testing.T) {
	raw := []byte("d4:infod6:lengthi10e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
	m, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	selection := &resourcecontent.Selection{Version: 1, Identity: m.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "movie.mkv", Size: 10}}}
	var keys sync.Map
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/node/v1/health":
			json.NewEncoder(w).Encode(nodeprotocol.HealthResponse{Capabilities: nodeprotocol.Capabilities{Codes: []string{nodeprotocol.CapabilitySelectiveFilesV1}}})
		case "/node/v1/credential-grants/grant-1":
			w.WriteHeader(http.StatusNoContent)
		case "/node/v1/downloader/actions":
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte("tracker-secret")) || bytes.Contains(body, raw) {
				t.Error("private source escaped into action wire")
			}
			var input nodeprotocol.DownloaderActionRequest
			if json.Unmarshal(body, &input) != nil || input.Submit.SourceEnvelope == nil || len(input.Submit.Source.Torrent) != 0 || input.Submit.Source.URL != "" {
				t.Error("missing encrypted source")
				return
			}
			value, _ := keys.Load("grant-1")
			key := value.(sourceGrantValue)
			opened, e := nodeprotocol.OpenDownloaderSource(input.Submit.SourceEnvelope, key.key, nodeprotocol.DownloaderSourceBinding{ServerID: "server-1", TaskID: input.TaskID, OperationKey: input.OperationKey, DownloaderID: input.DownloaderID, SavePath: input.Submit.SavePath, Tag: input.Submit.Tag, ConfigurationScope: key.scope})
			if e != nil || !bytes.Equal(opened.Torrent, raw) || opened.ContentSelection == nil {
				t.Error("sealed grant could not restore source", e)
			}
			json.NewEncoder(w).Encode(nodeprotocol.DownloaderActionResponse{RequestID: input.RequestID, Task: &nodeprotocol.DownloaderTask{ID: m.Identity.V1, Status: "stoppedDL"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	node := &Client{baseURL: server.URL, identity: Identity{ServerID: "server-1"}, http: server.Client()}
	client, _ := NewDownloaderClient(node, "downloader-1", "/downloads", t.TempDir(), "task-1", func(ctx context.Context, taskID, key, action string) (nodeprotocol.CredentialGrantEnvelope, error) {
		value := sourceGrantValue{key: DownloaderSourceGrantKey(ctx), scope: DownloaderSourceGrantScope(ctx)}
		if value.key == "" || value.scope == "" {
			t.Error("source key not confined to credential context")
		}
		keys.Store("grant-1", value)
		return nodeprotocol.CredentialGrantEnvelope{GrantID: "grant-1"}, nil
	})
	_, err = client.Submit(context.Background(), downloadpkg.SubmitRequest{Source: downloadpkg.Source{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:" + m.Identity.V1 + "&tr=https%3A%2F%2Ftracker.example%2Ftracker-secret", Torrent: raw, Filename: "movie.torrent", ContentSelection: selection}, Tag: "omc-task-1", SavePath: "/downloads/task-1"})
	if err != nil {
		t.Fatal(err)
	}
}
