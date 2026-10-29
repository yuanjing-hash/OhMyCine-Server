package qbittorrent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestV2SelectionRequiresFullNativeHashProof(t *testing.T) {
	info := []byte("d9:file treed9:movie.mkvd0:d6:lengthi10e11:pieces root32:" + strings.Repeat("r", 32) + "eee12:meta versioni2e4:name9:movie.mkv12:piece lengthi16384ee")
	raw := append(append([]byte("d4:info"), info...), 'e')
	manifest, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	selection := &resourcecontent.Selection{Version: 1, Identity: manifest.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "movie.mkv", Size: 10}}}
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{true: "complete v2 proof", false: "v2 proof unavailable"}[supported], func(t *testing.T) {
			exists := false
			tags := ""
			tag := "omc-" + uuid.NewString()
			resumes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "test"})
					io.WriteString(w, "Ok.")
				case "/api/v2/app/webapiVersion":
					io.WriteString(w, "2.11.3")
				case "/api/v2/torrents/info":
					if !exists {
						io.WriteString(w, "[]")
						return
					}
					json.NewEncoder(w).Encode([]map[string]any{{"hash": manifest.Identity.V2[:40], "name": "movie.mkv", "state": "stoppedDL", "tags": tags}})
				case "/api/v2/torrents/add":
					r.ParseMultipartForm(1 << 20)
					exists = true
					tags = r.FormValue("tags")
					io.WriteString(w, "Ok.")
				case "/api/v2/torrents/properties":
					value := ""
					if supported {
						value = manifest.Identity.V2
					}
					json.NewEncoder(w).Encode(map[string]string{"infohash_v1": "", "infohash_v2": value})
				case "/api/v2/torrents/files":
					io.WriteString(w, `[{"index":7,"name":"movie.mkv","size":10,"priority":1,"progress":0}]`)
				case "/api/v2/torrents/resume", "/api/v2/torrents/start":
					resumes++
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, _ := New(downloader.Config{BaseURL: server.URL})
			_, err := client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceTorrent, Torrent: raw, Filename: "v2.torrent", ContentSelection: selection}, Tag: tag, SavePath: "/managed"})
			if supported && err != nil {
				t.Fatal(err)
			}
			if !supported {
				if code, _ := downloader.ErrorInfo(err); code != "downloader_selection_v2_identity_unsupported" {
					t.Fatal(code, err)
				}
			}
			if resumes != 0 {
				t.Fatal("v2 submit started before route approval")
			}
		})
	}
}
