package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func selectedFixture(t *testing.T) (downloader.Source, downloader.SelectionContext) {
	t.Helper()
	info := []byte("d5:filesld6:lengthi10e4:pathl5:a.mkveed6:lengthi20e4:pathl5:b.mkveee4:name4:Pack12:piece lengthi16384e6:pieces20:" + strings.Repeat("a", 20) + "e")
	raw := append(append([]byte("d4:info"), info...), 'e')
	manifest, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	selection := resourcecontent.Selection{Version: 1, Identity: manifest.Identity, Files: []resourcecontent.SelectedFile{{Index: 0, Path: "a.mkv", Size: 10}}}
	return downloader.Source{Kind: downloader.SourceTorrent, Torrent: raw, Filename: "pack.torrent", ContentSelection: &selection}, downloader.SelectionContext{Tag: "omc-" + uuid.NewString(), Selection: selection, RootName: "Pack", MultiFile: true}
}

type selectedQB struct {
	mu                    sync.Mutex
	source                downloader.Source
	binding               downloader.SelectionContext
	exists                bool
	tags, state, api      string
	priorities            [2]int
	progress              [2]float64
	adds, resumes, writes int
	ignorePriorities      bool
	wrongMapping          bool
	indexShift            int
	route                 bool
	groupMode             bool
	workspace             string
	allowCleanup          bool
	deletes               int
	lostDeleteACK         bool
}

func (s *selectedQB) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture"})
			io.WriteString(w, "Ok.")
		case "/api/v2/app/webapiVersion":
			io.WriteString(w, s.api)
		case "/api/v2/torrents/info":
			if !s.exists {
				io.WriteString(w, "[]")
				return
			}
			if tag := r.URL.Query().Get("tag"); tag != "" && !strings.Contains(s.tags, tag) {
				io.WriteString(w, "[]")
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{{"hash": s.binding.Selection.Identity.V1, "name": "Pack", "state": s.state, "tags": s.tags, "save_path": s.workspace, "progress": 0.3, "total_size": 30, "downloaded": 9}})
		case "/api/v2/torrents/files":
			name := "Pack/a.mkv"
			if s.wrongMapping {
				name = "Pack/other.mkv"
			}
			// Deliberately reverse the response array. Native index zero remains
			// file a; it must never be interpreted as the first display row.
			json.NewEncoder(w).Encode([]map[string]any{{"index": 1 + s.indexShift, "name": "Pack/b.mkv", "size": 20, "priority": s.priorities[1], "progress": s.progress[1]}, {"index": s.indexShift, "name": name, "size": 10, "priority": s.priorities[0], "progress": s.progress[0]}})
		case "/api/v2/torrents/add":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				http.Error(w, "bad", 400)
				return
			}
			file, _, err := r.FormFile("torrents")
			if err != nil {
				t.Error("not original metainfo", err)
				return
			}
			raw, _ := io.ReadAll(file)
			file.Close()
			if !bytes.Equal(raw, s.source.Torrent) || r.FormValue("urls") != "" || r.FormValue("paused") != "true" || r.FormValue("autoTMM") != "false" {
				t.Error("selection started without original stopped metainfo")
			}
			s.exists = true
			s.workspace = r.FormValue("savepath")
			s.tags = r.FormValue("tags")
			s.state = "stoppedDL"
			s.adds++
			io.WriteString(w, "Ok.")
		case "/api/v2/torrents/filePrio":
			r.ParseForm()
			s.writes++
			priority, _ := strconv.Atoi(r.Form.Get("priority"))
			if !s.ignorePriorities {
				for _, id := range strings.Split(r.Form.Get("id"), "|") {
					index, _ := strconv.Atoi(id)
					index -= s.indexShift
					if index < 0 || index > 1 {
						t.Error("wrong index")
					}
					s.priorities[index] = priority
				}
			}
		case "/api/v2/torrents/setCategory":
		case "/api/v2/torrents/setLocation":
			if s.groupMode {
				t.Error("shared workspace moved")
			}
			s.route = true
		case "/api/v2/torrents/addTags":
			r.ParseForm()
			for _, tag := range strings.Split(r.Form.Get("tags"), ",") {
				if !strings.Contains(s.tags, tag) {
					s.tags += "," + tag
				}
			}
		case "/api/v2/torrents/removeTags":
			r.ParseForm()
			left := []string{}
			for _, tag := range strings.Split(s.tags, ",") {
				if tag != r.Form.Get("tags") {
					left = append(left, tag)
				}
			}
			s.tags = strings.Join(left, ",")
		case "/api/v2/torrents/resume", "/api/v2/torrents/start":
			if !s.groupMode && (s.priorities != [2]int{1, 0} || !s.route) {
				t.Error("resumed before verified selection and route")
			}
			s.resumes++
			s.state = "downloading"
		case "/api/v2/torrents/pause", "/api/v2/torrents/stop":
			s.state = "stoppedDL"
		case "/api/v2/torrents/delete":
			if !s.allowCleanup {
				t.Error("selected cancellation deleted torrent")
			} else {
				r.ParseForm()
				if r.Form.Get("deleteFiles") != "false" {
					t.Error("shared cleanup used whole package data deletion")
				}
				s.exists = false
				s.deletes++
				if s.lostDeleteACK {
					s.lostDeleteACK = false
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func TestSelectiveStoppedMappingProgressAndOwnership(t *testing.T) {
	source, binding := selectedFixture(t)
	state := &selectedQB{source: source, binding: binding, api: "2.11.3", priorities: [2]int{1, 1}, indexShift: 7}
	server := httptest.NewServer(state.handler(t))
	defer server.Close()
	client, _ := New(downloader.Config{BaseURL: server.URL})
	ctx := context.Background()
	task, err := client.Submit(ctx, downloader.SubmitRequest{Source: source, Tag: binding.Tag, SavePath: "/managed/task"})
	if err != nil {
		t.Fatal(err)
	}
	if state.adds != 1 || state.resumes != 0 || state.priorities != [2]int{1, 0} {
		t.Fatalf("unsafe submit: %+v", state)
	}
	bound, err := downloader.BindSelection(client, source, binding.Tag)
	if err != nil {
		t.Fatal(err)
	}
	if err = bound.(downloader.MetadataClient).SetCategory(ctx, task.ID, "tv", "/managed/task/tv"); err != nil {
		t.Fatal(err)
	}
	if err = client.ResumeSelected(ctx, task.ID, binding); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.progress = [2]float64{1, 0}
	state.mu.Unlock()
	selected, err := client.GetSelected(ctx, task.ID, binding)
	if err != nil || !selected.Completed || *selected.BytesTotal != 10 || *selected.BytesCompleted != 10 {
		t.Fatalf("selected progress %+v %v", selected, err)
	}
	manifest, err := client.ManifestSelected(ctx, task.ID, binding)
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].NativeIndex == nil || *manifest.Files[0].NativeIndex != 7 || manifest.Files[0].RelativePath != "Pack/a.mkv" {
		t.Fatal(manifest, err)
	}
	if err = client.CancelSelected(ctx, task.ID, binding, false); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Submit(ctx, downloader.SubmitRequest{Source: source, Tag: binding.Tag, SavePath: "/managed/task"}); err != nil || state.adds != 1 {
		t.Fatal("retry duplicated", err)
	}
	other := binding
	other.Tag = "omc-" + uuid.NewString()
	before := state.writes
	if _, err = client.Submit(ctx, downloader.SubmitRequest{Source: source, Tag: other.Tag, SavePath: "/managed/other"}); err == nil || state.writes != before || state.adds != 1 {
		t.Fatal("modified another plan", err)
	}
	state.mu.Lock()
	state.tags = "user-task"
	state.mu.Unlock()
	if err = client.ResumeSelected(ctx, task.ID, binding); err == nil {
		t.Fatal("resumed foreign torrent")
	}
}

func TestSelectiveFailureNeverResumesOrFallsBack(t *testing.T) {
	for _, kind := range []string{"mapping", "priority readback", "old API", "lost selection"} {
		t.Run(kind, func(t *testing.T) {
			source, binding := selectedFixture(t)
			state := &selectedQB{source: source, binding: binding, api: "2.11.3", priorities: [2]int{1, 1}}
			switch kind {
			case "mapping":
				state.wrongMapping = true
			case "priority readback":
				state.ignorePriorities = true
			case "old API":
				state.api = "2.7.0"
			case "lost selection":
				source.Torrent = nil
			}
			server := httptest.NewServer(state.handler(t))
			defer server.Close()
			client, _ := New(downloader.Config{BaseURL: server.URL})
			if _, err := client.Submit(context.Background(), downloader.SubmitRequest{Source: source, Tag: binding.Tag, SavePath: "/managed/task"}); err == nil {
				t.Fatal("accepted invalid selection")
			}
			if state.resumes != 0 {
				t.Fatal("failed selection resumed")
			}
			if kind == "lost selection" || kind == "old API" {
				if state.adds != 0 {
					t.Fatal("submitted before preflight")
				}
			}
		})
	}
}
