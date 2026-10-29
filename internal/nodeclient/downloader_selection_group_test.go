package nodeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestSelectionGroupRemotePathStylesAndOldNodePreflight(t *testing.T) {
	for _, root := range []string{"/downloads/root", `D:\downloads\root`} {
		t.Run(root, func(t *testing.T) {
			grants, actions := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/node/v1/health" {
					actions++
				}
				json.NewEncoder(w).Encode(nodeprotocol.HealthResponse{Capabilities: nodeprotocol.Capabilities{Codes: []string{nodeprotocol.CapabilitySelectiveFilesV1}}})
			}))
			defer server.Close()
			node := &Client{baseURL: server.URL, identity: Identity{ServerID: "server"}, http: server.Client()}
			local := filepath.Join(t.TempDir(), ".omc-bt", strings.Repeat("a", 64))
			client, _ := NewDownloaderClient(node, "downloader", root, local, "task", func(context.Context, string, string, string) (nodeprotocol.CredentialGrantEnvelope, error) {
				grants++
				return nodeprotocol.CredentialGrantEnvelope{}, nil
			})
			id := strings.Repeat("a", 64)
			if err := client.SetSelectionGroupWorkspace(id); err != nil {
				t.Fatal(err)
			}
			remote, ok := client.toRemotePath(filepath.Join(local, "Pack", "e.mkv"))
			if !ok {
				t.Fatal("local mapping failed")
			}
			if strings.HasPrefix(root, "/") {
				if remote != root+"/.omc-bt/"+id+"/Pack/e.mkv" || strings.Contains(remote, `\`) {
					t.Fatal(remote)
				}
			} else {
				if remote != root+`\.omc-bt\`+id+`\Pack\e.mkv` || strings.Contains(remote, "/") {
					t.Fatal(remote)
				}
			}
			raw := []byte("d4:infod6:lengthi10e4:name9:movie.mkv12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
			m, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			sel := resourcecontent.Selection{Version: 1, Identity: m.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "movie.mkv", Size: 10}}}
			g := downloadpkg.SelectionGroup{Version: 1, ID: id, Tag: "omc-group-" + id[:32], Workspace: local, Epoch: 1, Revision: 1, Union: sel}
			_, err = client.ApplySelectionGroup(context.Background(), downloadpkg.SubmitRequest{Source: downloadpkg.Source{Kind: downloadpkg.SourceTorrent, Torrent: raw, ContentSelection: &sel}, SavePath: local, Tag: "omc-task"}, g)
			if code, _ := downloadpkg.ErrorInfo(err); code != nodeprotocol.ErrorCapabilityMissing || grants != 0 || actions != 0 {
				t.Fatal(code, grants, actions)
			}
		})
	}
}

func TestSelectionGroupRemoteUncertainReceiptsRemainRetryable(t *testing.T) {
	for _, code := range []string{"downloader_selection_submission_unconfirmed", "downloader_selection_group_busy", "downloader_selection_group_cleanup_pending", "downloader_selection_cleanup_unconfirmed"} {
		err := remoteDownloaderError(&RemoteError{Code: code}, "")
		actual, retry := downloadpkg.ErrorInfo(err)
		if actual != code || !retry {
			t.Fatal("uncertain original operation became terminal", actual, retry)
		}
	}
	if _, retry := downloadpkg.ErrorInfo(remoteDownloaderError(&RemoteError{Code: "downloader_selection_owner_conflict"}, "")); retry {
		t.Fatal("foreign native owner became retryable mutation")
	}
}
