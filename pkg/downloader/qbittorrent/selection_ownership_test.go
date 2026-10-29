package qbittorrent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func TestOrdinaryAccessCannotAdoptOrControlSelectedResource(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "group"}[grouped], func(t *testing.T) {
			source, own := selectedFixture(t)
			state := &selectedQB{source: source, binding: own, api: "2.11.3", priorities: [2]int{1, 1}, groupMode: grouped, allowCleanup: true}
			base := state.handler(t)
			var duplicateAdds atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				state.mu.Lock()
				exists := state.exists
				state.mu.Unlock()
				if r.URL.Path != "/api/v2/torrents/add" || !exists {
					base.ServeHTTP(w, r)
					return
				}
				// A duplicate add is forbidden even if a qB version happens to
				// merge supplied tags rather than explicitly rejecting the add.
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				duplicateAdds.Add(1)
				state.mu.Lock()
				state.tags += "," + r.FormValue("tags")
				state.mu.Unlock()
				_, _ = io.WriteString(w, "Ok.")
			}))
			defer server.Close()
			client, _ := New(downloader.Config{BaseURL: server.URL})
			request := downloader.SubmitRequest{Source: source, Tag: own.Tag, SavePath: "/managed/single"}
			binding := own
			if grouped {
				id := strings.Repeat("a", 64)
				group := downloader.SelectionGroup{Version: 1, ID: id, Tag: "omc-group-" + id[:32], Workspace: "/managed/.omc-bt/" + id, Revision: 1, Epoch: 1, Union: own.Selection}
				request.SavePath = group.Workspace
				binding.Group = &group
				if _, err := client.ApplySelectionGroup(context.Background(), request, group); err != nil {
					t.Fatal(err)
				}
			} else if _, err := client.Submit(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			hash := own.Selection.Identity.V1
			for _, kind := range []string{downloader.SourceTorrent, downloader.SourceURL} {
				ordinary := source
				ordinary.ContentSelection = nil
				ordinary.Kind = kind
				if kind == downloader.SourceURL {
					ordinary.Torrent = nil
					ordinary.URL = "magnet:?xt=urn:btih:" + hash
				}
				_, err := client.Submit(context.Background(), downloader.SubmitRequest{Source: ordinary, Tag: "omc-" + uuid.NewString(), SavePath: "/managed/ordinary"})
				if code, _ := downloader.ErrorInfo(err); code != "downloader_selection_owner_conflict" {
					t.Fatalf("ordinary %s adopted managed hash: %v", kind, err)
				}
			}
			for _, id := range []string{hash, "tag:" + own.Tag} {
				if grouped && strings.HasPrefix(id, "tag:") {
					id = "tag:" + binding.Group.Tag
				}
				calls := map[string]func() error{
					"get":      func() error { _, err := client.Get(context.Background(), id); return err },
					"manifest": func() error { _, err := client.Manifest(context.Background(), id); return err },
					"pause":    func() error { return client.Pause(context.Background(), id) },
					"resume":   func() error { return client.Resume(context.Background(), id) },
					"cancel":   func() error { return client.Cancel(context.Background(), id, false) },
					"delete":   func() error { return client.Cancel(context.Background(), id, true) },
					"category": func() error { return client.SetCategory(context.Background(), id, "tv", "/other") },
				}
				for name, call := range calls {
					if code, _ := downloader.ErrorInfo(call()); code != "downloader_selection_owner_conflict" {
						t.Fatalf("ordinary %s permitted managed access by %s: %s", name, id, code)
					}
				}
			}
			if _, err := client.GetSelected(context.Background(), hash, binding); err != nil {
				t.Fatal("ordinary attempt damaged selected authority", err)
			}
			if !grouped {
				if code, _ := downloader.ErrorInfo(client.DeleteManagedTag(context.Background(), own.Tag)); code != "downloader_selection_owner_conflict" {
					t.Fatalf("ordinary tag deletion touched selected owner: %s", code)
				}
			}
			state.mu.Lock()
			deletes, adds := state.deletes, state.adds
			state.mu.Unlock()
			if duplicateAdds.Load() != 0 || deletes != 0 || adds != 1 {
				t.Fatalf("ordinary touched native resource: duplicates=%d deletes=%d initial_adds=%d", duplicateAdds.Load(), deletes, adds)
			}
		})
	}
}

func TestUnknownSourceRequiresAbsenceOfManagedNativeTasks(t *testing.T) {
	for _, raw := range []string{"https://source.example/file.torrent", "http://source.example:8080/file.torrent", "magnet:?xt=urn:btih:legacy-unproven"} {
		t.Run(raw, func(t *testing.T) {
			var adds atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture"})
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/torrents/info":
					if r.URL.Query().Get("limit") != "2049" {
						t.Error("unknown source was not checked against bounded native ownership")
					}
					_, _ = io.WriteString(w, `[{"hash":"`+strings.Repeat("a", 40)+`","tags":"omc-group-owned,omc-sel-v1-proof"}]`)
				case "/api/v2/torrents/add":
					adds.Add(1)
					_, _ = io.WriteString(w, "Ok.")
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, _ := New(downloader.Config{BaseURL: server.URL})
			_, err := client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: raw}, Tag: "ordinary", SavePath: "/ordinary"})
			if code, _ := downloader.ErrorInfo(err); code != "downloader_selection_identity_required" || adds.Load() != 0 {
				t.Fatalf("unknown source touched managed task: %v, adds=%d", err, adds.Load())
			}
		})
	}
}

func TestOrdinaryKnownSourcesAndLegacyHTTPKeepNormalBehavior(t *testing.T) {
	for _, kind := range []string{"magnet", "torrent", "http", "https"} {
		t.Run(kind, func(t *testing.T) {
			source, own := selectedFixture(t)
			source.ContentSelection = nil
			if kind != "torrent" {
				source.Kind, source.Torrent = downloader.SourceURL, nil
				source.URL = "magnet:?xt=urn:btih:" + own.Selection.Identity.V1
				if kind == "http" || kind == "https" {
					source.URL = kind + "://source.example:8080/file.torrent"
				}
			}
			const tag = "ordinary-owned"
			exists, nativeTags, mutations, adds := false, "", 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture"})
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/torrents/info":
					if (kind == "magnet" || kind == "torrent") && r.URL.Query().Get("limit") != "" {
						// A different selected hash coexists in this account. A known
						// ordinary identity must be checked narrowly, not globally banned.
						_ = json.NewEncoder(w).Encode([]torrentInfo{{Hash: strings.Repeat("f", 40), Tags: "omc-group-unrelated,omc-sel-v1-unrelated"}})
						return
					}
					if !exists || r.URL.Query().Get("tag") != "" && r.URL.Query().Get("tag") != nativeTags {
						_, _ = io.WriteString(w, "[]")
						return
					}
					_ = json.NewEncoder(w).Encode([]torrentInfo{{Hash: own.Selection.Identity.V1, Tags: nativeTags, Name: "Pack", State: "downloading"}})
				case "/api/v2/torrents/add":
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if r.FormValue("paused") != "" || r.FormValue("stopCondition") != "" {
						t.Error("ordinary source acquired selected/metadata flags")
					}
					exists, nativeTags = true, r.FormValue("tags")
					adds++
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/torrents/files":
					_, _ = io.WriteString(w, `[{"name":"Pack/a.mkv","size":10}]`)
				case "/api/v2/torrents/pause", "/api/v2/torrents/resume", "/api/v2/torrents/setCategory", "/api/v2/torrents/setLocation", "/api/v2/torrents/delete":
					mutations++
				default:
					t.Errorf("unexpected %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, _ := New(downloader.Config{BaseURL: server.URL})
			request := downloader.SubmitRequest{Source: source, Tag: tag, SavePath: "/ordinary"}
			task, err := client.Submit(context.Background(), request)
			if err != nil || task.ID != own.Selection.Identity.V1 {
				t.Fatal(task, err)
			}
			if _, err = client.Submit(context.Background(), request); err != nil {
				t.Fatal("ordinary idempotent retry", err)
			}
			if _, err = client.Get(context.Background(), task.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = client.Manifest(context.Background(), task.ID); err != nil {
				t.Fatal(err)
			}
			if err = client.Pause(context.Background(), task.ID); err != nil {
				t.Fatal(err)
			}
			if err = client.Resume(context.Background(), task.ID); err != nil {
				t.Fatal(err)
			}
			if err = client.SetCategory(context.Background(), task.ID, "tv", "/ordinary/tv"); err != nil {
				t.Fatal(err)
			}
			if err = client.Cancel(context.Background(), task.ID, true); err != nil {
				t.Fatal(err)
			}
			if adds != 1 || mutations != 5 {
				t.Fatalf("ordinary regression: adds=%d mutations=%d", adds, mutations)
			}
		})
	}
}

func TestUnknownSourceCannotUseTruncatedNativeOwnershipView(t *testing.T) {
	var adds atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture"})
			_, _ = io.WriteString(w, "Ok.")
		case "/api/v2/torrents/info":
			_ = json.NewEncoder(w).Encode(make([]torrentInfo, maxManifestFiles+1))
		case "/api/v2/torrents/add":
			adds.Add(1)
			_, _ = io.WriteString(w, "Ok.")
		}
	}))
	defer server.Close()
	client, _ := New(downloader.Config{BaseURL: server.URL})
	_, err := client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "http://source.example/file.torrent"}, Tag: "ordinary", SavePath: "/ordinary"})
	if err == nil || adds.Load() != 0 {
		t.Fatal("bounded absence was treated as complete", err, adds.Load())
	}
}

func TestUnknownSourceOwnershipViewRequiresCompleteFacts(t *testing.T) {
	hash := strings.Repeat("a", 40)
	for _, item := range []struct {
		name, view string
		allowed    bool
	}{
		{name: "null", view: "null"},
		{name: "object", view: "{}"},
		{name: "empty row", view: "[{}]"},
		{name: "null row", view: "[null]"},
		{name: "missing hash", view: `[{"tags":""}]`},
		{name: "missing tags", view: `[{"hash":"` + hash + `"}]`},
		{name: "null tags", view: `[{"hash":"` + hash + `","tags":null}]`},
		{name: "wrong hash type", view: `[{"hash":7,"tags":""}]`},
		{name: "wrong tags type", view: `[{"hash":"` + hash + `","tags":[]}]`},
		{name: "invalid hash", view: `[{"hash":"bad","tags":""}]`},
		{name: "explicit empty", view: "[]", allowed: true},
		{name: "ordinary complete row", view: `[{"hash":"` + hash + `","tags":"user-owned"}]`, allowed: true},
	} {
		t.Run(item.name, func(t *testing.T) {
			var created atomic.Bool
			var adds atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture"})
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/torrents/info":
					if created.Load() {
						_ = json.NewEncoder(w).Encode([]torrentInfo{{Hash: hash, Tags: "ordinary", Name: "Pack", State: "downloading"}})
					} else if r.URL.Query().Get("tag") != "" {
						_, _ = io.WriteString(w, "[]")
					} else {
						_, _ = io.WriteString(w, item.view)
					}
				case "/api/v2/torrents/add":
					adds.Add(1)
					created.Store(true)
					_, _ = io.WriteString(w, "Ok.")
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, _ := New(downloader.Config{BaseURL: server.URL})
			task, err := client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "https://source.example/file.torrent"}, Tag: "ordinary", SavePath: "/ordinary"})
			if item.allowed {
				if err != nil || task.ID != hash || adds.Load() != 1 {
					t.Fatalf("complete ordinary view lost compatibility: error=%v id=%q adds=%d", err, task.ID, adds.Load())
				}
			} else if code, retryable := downloader.ErrorInfo(err); code != "downloader_response_invalid" || retryable || adds.Load() != 0 {
				t.Fatalf("incomplete view admitted unknown identity: code=%q retryable=%t adds=%d", code, retryable, adds.Load())
			}
		})
	}
}

func TestOrdinaryGetCannotReturnEmptyNativeIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture"})
			_, _ = io.WriteString(w, "Ok.")
			return
		}
		_, _ = io.WriteString(w, `[{"name":"invalid task","tags":""}]`)
	}))
	defer server.Close()
	client, _ := New(downloader.Config{BaseURL: server.URL})
	task, err := client.Get(context.Background(), "tag:ordinary")
	if code, retryable := downloader.ErrorInfo(err); code != "downloader_response_invalid" || retryable || task.ID != "" {
		t.Fatalf("empty native identity became a successful task: code=%q retryable=%t id=%q", code, retryable, task.ID)
	}
}

func TestKnownSourceRejectsIncompleteNativeOwnershipFacts(t *testing.T) {
	for _, view := range []string{"null", "[{}]", `[{"hash":"` + strings.Repeat("a", 40) + `"}]`} {
		t.Run(view, func(t *testing.T) {
			var adds atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/auth/login":
					http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture"})
					_, _ = io.WriteString(w, "Ok.")
				case "/api/v2/torrents/info":
					_, _ = io.WriteString(w, view)
				case "/api/v2/torrents/add":
					adds.Add(1)
					_, _ = io.WriteString(w, "Ok.")
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client, _ := New(downloader.Config{BaseURL: server.URL})
			_, err := client.Submit(context.Background(), downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}, Tag: "ordinary", SavePath: "/ordinary"})
			if code, retryable := downloader.ErrorInfo(err); code != "downloader_response_invalid" || retryable || adds.Load() != 0 {
				t.Fatalf("known identity bypassed incomplete ownership proof: error=%v code=%q adds=%d", err, code, adds.Load())
			}
		})
	}
}

func TestOrdinaryAndSelectedConcurrentAdmissionsHaveOneOwner(t *testing.T) {
	for _, first := range []string{"selected", "ordinary", "unknown ordinary", "selected before unknown", "torrent ordinary", "selected before torrent"} {
		t.Run(first, func(t *testing.T) {
			source, own := selectedFixture(t)
			state := &selectedQB{source: source, binding: own, api: "2.11.3", priorities: [2]int{1, 1}}
			base := state.handler(t)
			entered, proceed, checkedSelected := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
			var adds, ordinaryAdds atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v2/app/webapiVersion" {
					select {
					case checkedSelected <- struct{}{}:
					default:
					}
				}
				if r.URL.Path != "/api/v2/torrents/add" {
					base.ServeHTTP(w, r)
					return
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				isSelected := r.FormValue("paused") == "true"
				if adds.Add(1) == 1 {
					close(entered)
					select {
					case <-proceed:
					case <-r.Context().Done():
						return
					}
				}
				if isSelected {
					base.ServeHTTP(w, r)
					return
				}
				ordinaryAdds.Add(1)
				state.mu.Lock()
				state.exists, state.tags, state.state, state.workspace = true, r.FormValue("tags"), "downloading", r.FormValue("savepath")
				state.mu.Unlock()
				_, _ = io.WriteString(w, "Ok.")
			}))
			defer server.Close()
			// Separate clients model separate callers, sharing endpoint/account.
			selectedClient, _ := New(downloader.Config{BaseURL: server.URL})
			ordinaryClient, _ := New(downloader.Config{BaseURL: server.URL})
			ordinary := downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=urn:btih:" + own.Selection.Identity.V1}
			if strings.Contains(first, "unknown") {
				ordinary.URL = "https://source.example/file.torrent"
			}
			if strings.Contains(first, "torrent") {
				ordinary = source
				ordinary.ContentSelection = nil
			}
			selectedResult, ordinaryResult := make(chan error, 1), make(chan error, 1)
			submitSelected := func() {
				_, err := selectedClient.Submit(context.Background(), downloader.SubmitRequest{Source: source, Tag: own.Tag, SavePath: "/managed/selected"})
				selectedResult <- err
			}
			submitOrdinary := func() {
				_, err := ordinaryClient.Submit(context.Background(), downloader.SubmitRequest{Source: ordinary, Tag: "ordinary", SavePath: "/managed/ordinary"})
				ordinaryResult <- err
			}
			startsSelected := strings.HasPrefix(first, "selected")
			if startsSelected {
				go submitSelected()
			} else {
				go submitOrdinary()
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("first admission did not reach add")
			}
			if startsSelected {
				go submitOrdinary()
			} else {
				go submitSelected()
			}
			if first == "unknown ordinary" {
				// Force selectedState to have observed absence before the unknown
				// URL add becomes visible. The in-admission recheck must catch it.
				select {
				case <-checkedSelected:
				case <-time.After(3 * time.Second):
					close(proceed)
					t.Fatal("selected preflight did not reach creation window")
				}
			}
			close(proceed)
			var selectedErr, ordinaryErr error
			select {
			case selectedErr = <-selectedResult:
			case <-time.After(4 * time.Second):
				t.Fatal("selected admission did not finish")
			}
			select {
			case ordinaryErr = <-ordinaryResult:
			case <-time.After(4 * time.Second):
				t.Fatal("ordinary admission did not finish")
			}
			if startsSelected {
				if selectedErr != nil || ordinaryErr == nil || ordinaryAdds.Load() != 0 {
					t.Fatal(selectedErr, ordinaryErr, ordinaryAdds.Load())
				}
			} else if ordinaryErr != nil || selectedErr == nil || ordinaryAdds.Load() != 1 {
				t.Fatal(selectedErr, ordinaryErr, ordinaryAdds.Load())
			}
			if adds.Load() != 1 {
				t.Fatalf("duplicate native add during competing admission: %d", adds.Load())
			}
		})
	}
}

func TestOrdinaryKnownIdentitySharesSelectedResourceLock(t *testing.T) {
	identity := resourcecontent.Identity{V1: strings.Repeat("1", 40), V2: strings.Repeat("2", 64)}
	for _, xt := range []string{"urn:btih:" + identity.V1, "urn:btmh:1220" + identity.V2} {
		t.Run(xt[:8], func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests.Add(1); w.WriteHeader(500) }))
			defer server.Close()
			client, _ := New(downloader.Config{BaseURL: server.URL})
			release, err := client.selectionLock(context.Background(), identity)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			_, err = client.Submit(ctx, downloader.SubmitRequest{Source: downloader.Source{Kind: downloader.SourceURL, URL: "magnet:?xt=" + xt}, Tag: "ordinary", SavePath: "/ordinary"})
			if err == nil || requests.Load() != 0 {
				t.Fatalf("ordinary bypassed selected native identity lock: err=%v requests=%d", err, requests.Load())
			}
		})
	}
}
