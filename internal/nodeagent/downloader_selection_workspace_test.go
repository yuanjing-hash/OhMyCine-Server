package nodeagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestNodeSelectionGroupFirstWorkspaceUsesOnlySafeMappedRoot(t *testing.T) {
	for _, reparse := range []bool{false, true} {
		t.Run(strconv.FormatBool(reparse), func(t *testing.T) {
			root := t.TempDir()
			mount := filepath.Join(root, "mount")
			if err := os.Mkdir(mount, 0700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if reparse {
				link := filepath.Join(mount, ".omc-bt")
				err := os.Symlink(outside, link)
				if err != nil && runtime.GOOS == "windows" {
					_, err = exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, outside).CombinedOutput()
				}
				if err != nil {
					t.Skipf("host cannot create reparse fixture: %v", err)
				}
			}
			info := []byte("d5:filesld6:lengthi10e4:pathl5:a.mkveed6:lengthi20e4:pathl5:b.mkveee4:name4:Pack12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaae")
			raw := append(append([]byte("d4:info"), info...), 'e')
			manifest, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			selection := resourcecontent.Selection{Version: 1, Identity: manifest.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "a.mkv", Size: 10}}}
			id := strings.Repeat("b", 64)
			group := downloadpkg.SelectionGroup{Version: 1, ID: id, Tag: "omc-group-" + id[:32], Workspace: "/downloads/.omc-bt/" + id, Revision: 1, Epoch: 1, Union: selection}
			local := filepath.Join(mount, ".omc-bt", id)
			if _, err := os.Stat(local); !os.IsNotExist(err) {
				t.Fatal("fixture workspace already exists")
			}
			exists, adds, tags, priorities := false, 0, "", [2]int{1, 1}
			qbit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture"})
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/app/webapiVersion":
					_, _ = io.WriteString(w, "2.11.3")
				case "/api/v2/torrents/info":
					if !exists || r.URL.Query().Get("tag") != "" && !strings.Contains(tags, r.URL.Query().Get("tag")) {
						_, _ = io.WriteString(w, "[]")
						return
					}
					_ = json.NewEncoder(w).Encode([]map[string]any{{"hash": manifest.Identity.V1, "name": "Pack", "state": "stoppedDL", "tags": tags, "save_path": group.Workspace}})
				case "/api/v2/torrents/add":
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
						return
					}
					if r.FormValue("savepath") != group.Workspace || r.FormValue("paused") != "true" {
						t.Error("Node sent local host path or running add to qB")
					}
					file, _, err := r.FormFile("torrents")
					if err != nil {
						t.Error(err)
						return
					}
					body, _ := io.ReadAll(file)
					_ = file.Close()
					if string(body) != string(raw) {
						t.Error("original selected source changed")
					}
					tags, exists = r.FormValue("tags"), true
					adds++
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/torrents/files":
					_ = json.NewEncoder(w).Encode([]map[string]any{{"index": 8, "name": "Pack/b.mkv", "size": 20, "priority": priorities[1], "progress": 0}, {"index": 7, "name": "Pack/a.mkv", "size": 10, "priority": priorities[0], "progress": 0}})
				case "/api/v2/torrents/filePrio":
					_ = r.ParseForm()
					p, _ := strconv.Atoi(r.Form.Get("priority"))
					for _, value := range strings.Split(r.Form.Get("id"), "|") {
						n, e := strconv.Atoi(value)
						if e != nil || n < 7 || n > 8 {
							t.Error("wrong native wanted index")
							return
						}
						priorities[n-7] = p
					}
				case "/api/v2/torrents/addTags":
					_ = r.ParseForm()
					tags += "," + r.Form.Get("tags")
				default:
					t.Errorf("unexpected provider operation %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer qbit.Close()
			store, err := OpenStore(filepath.Join(root, "node.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			agent, err := New(Config{NodeID: "node-1", ListenAddress: "127.0.0.1:0", DataDirectory: root, ManagedRoot: filepath.Join(root, "managed"), SealingPrivateKeyFile: filepath.Join(root, "sealing.key"), MaxConcurrentOperations: 1, AllowInsecureDevelopment: true}, store)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(agent.Handler())
			defer server.Close()
			taskID, operation := uuid.NewString(), uuid.NewString()
			tag := "omc-" + taskID
			binding := nodeprotocol.DownloaderSourceBinding{ServerID: "server-1", TaskID: taskID, OperationKey: operation, DownloaderID: "downloader-1", SavePath: group.Workspace, Tag: tag, ConfigurationScope: strings.Repeat("c", 64)}
			source, key, err := nodeprotocol.SealDownloaderSource(nodeprotocol.DownloaderSource{Kind: "torrent", Torrent: raw, Filename: "pack.torrent", ContentSelection: &selection}, binding)
			if err != nil {
				t.Fatal(err)
			}
			credential, _ := json.Marshal(nodeprotocol.DownloaderCredential{ProviderType: "qbittorrent", BaseURL: qbit.URL, DownloaderSaveRoot: "/downloads", NodeMountRoot: mount, SourceKey: key, SourceScope: binding.ConfigurationScope})
			now := time.Now().UTC()
			grant := nodeprotocol.CredentialGrant{GrantID: uuid.NewString(), NodeID: "node-1", TaskID: taskID, OperationKey: operation, ResourceKind: nodeprotocol.ResourceKindDownloader, ResourceID: "downloader-1", CredentialRevision: 1, AllowedActions: []string{nodeprotocol.DownloaderActionSelectionGroup}, ExpiresAt: now.Add(time.Minute), Credential: credential}
			sealed, err := nodeprotocol.SealCredentialGrant(agent.sealingPublicKey, grant, now)
			if err != nil {
				t.Fatal(err)
			}
			doNodeJSON(t, server.URL+"/node/v1/credential-grants/"+grant.GrantID, http.MethodPut, sealed, http.StatusNoContent)
			input := nodeprotocol.DownloaderActionRequest{RequestID: uuid.NewString(), TaskID: taskID, OperationKey: operation, DownloaderID: "downloader-1", Action: nodeprotocol.DownloaderActionSelectionGroup, GrantID: grant.GrantID, Group: &group, Submit: &nodeprotocol.DownloaderSubmit{Source: nodeprotocol.DownloaderSource{Kind: "torrent"}, SourceEnvelope: source, SavePath: group.Workspace, Tag: tag}}
			status := http.StatusOK
			if reparse {
				status = http.StatusBadGateway
			}
			doNodeJSON(t, server.URL+"/node/v1/downloader/actions", http.MethodPost, input, status)
			if reparse {
				if adds != 0 {
					t.Fatal("Node dispatched qB write through a reparse parent")
				}
				if _, err := os.Stat(filepath.Join(outside, id)); !os.IsNotExist(err) {
					t.Fatal("Node created directory outside mapped root")
				}
			} else {
				if adds != 1 || priorities != [2]int{1, 0} {
					t.Fatalf("wanted execution adds=%d priorities=%v", adds, priorities)
				}
				if info, err := os.Stat(local); err != nil || !info.IsDir() {
					t.Fatalf("first mapped workspace was not prepared: %v", err)
				}
				managed, _, err := store.managedGroupDownload(context.Background(), "server-1", "downloader-1", taskID, manifest.Identity.V1)
				canonical, canonicalErr := filepath.EvalSymlinks(local)
				if err != nil || canonicalErr != nil || !sameCleanPath(managed.NodeLocalRoot, canonical) || managed.DownloaderSavePath != group.Workspace {
					t.Fatalf("mapping receipt=%+v err=%v", managed, err)
				}
			}
		})
	}
}
