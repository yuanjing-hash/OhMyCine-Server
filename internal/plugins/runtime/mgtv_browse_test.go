package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type mangoBrowseHost struct {
	t       *testing.T
	files   map[string][]byte
	delay   time.Duration
	blocked chan struct{}
	release chan struct{}
	calls   int
}

func (host *mangoBrowseHost) Call(ctx context.Context, _ string, operation uint32, payload []byte) ([]byte, error) {
	host.calls++
	if operation == 7 {
		return json.Marshal(map[string]any{"data": map[string]any{"ref": fmt.Sprintf("00000000-0000-4000-8000-%012d", host.calls), "expiresAt": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)}})
	}
	if operation != 1 {
		host.t.Fatalf("unexpected Host operation %d", operation)
	}
	var request struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(payload, &request) != nil {
		host.t.Fatal("invalid guest HTTP request")
	}
	u, err := url.Parse(request.URL)
	if err != nil || u.Hostname() != "pianku.api.mgtv.com" {
		host.t.Fatal("unexpected guest control origin")
	}
	if host.blocked != nil {
		close(host.blocked)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-host.release:
		}
	}
	if host.delay != 0 {
		timer := time.NewTimer(host.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	body, ok := host.files[u.Path]
	if !ok {
		host.t.Fatalf("unexpected catalogue control path %s", u.Path)
	}
	return json.Marshal(map[string]any{"data": map[string]any{"status": 200, "bodyBase64": base64.StdEncoding.EncodeToString(body)}})
}

func mangoBrowseFixture(t *testing.T) (*Host, *mangoBrowseHost, string) {
	t.Helper()
	entry, fixtures := os.Getenv("OMC_MGTV_WASM"), os.Getenv("OMC_MGTV_FIXTURES")
	if entry == "" || fixtures == "" {
		t.Skip("set OMC_MGTV_WASM and OMC_MGTV_FIXTURES for actual released guest regression")
	}
	api := &mangoBrowseHost{t: t, files: make(map[string][]byte)}
	for path, file := range map[string]string{
		"/rider/config/platformChannels/v1": "channels.json",
		"/rider/config/channel/v1":          "channel.json",
		"/rider/list/pcweb/v3":              "catalog.json",
	} {
		body, err := os.ReadFile(filepath.Join(fixtures, file))
		if err != nil {
			t.Fatal(err)
		}
		api.files[path] = body
	}
	host := NewHost(context.Background())
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	host.SetCapabilityHost(api)
	const pluginID = "org.ohmycine.mgtv"
	if err := host.Start(context.Background(), pluginID, entry, 1); err != nil {
		t.Fatal(err)
	}
	return host, api, pluginID
}

func invokeMangoBrowse(t *testing.T, host *Host, pluginID, operation string, request any) []byte {
	t.Helper()
	input, _ := json.Marshal(request)
	output, err := host.Invoke(context.Background(), pluginID, operation, input)
	if err != nil {
		t.Fatalf("operation=%s runtime_code=%s", operation, ErrorCode(err))
	}
	var decoded any
	if json.Unmarshal(output, &decoded) != nil {
		t.Fatalf("operation=%s invalid JSON", operation)
	}
	if envelope, ok := decoded.(map[string]any); ok && (envelope["pluginError"] != nil || envelope["error"] != nil) {
		t.Fatalf("operation=%s business error", operation)
	}
	return output
}

func TestMangoWASMBrowseRepeatedFullPayloadMemory(t *testing.T) {
	host, api, pluginID := mangoBrowseFixture(t)
	var warmSize uint32
	for iteration := 0; iteration < 80; iteration++ {
		invokeMangoBrowse(t, host, pluginID, "site.navigation", map[string]any{"connectionId": "fixture"})
		invokeMangoBrowse(t, host, pluginID, "site.navigation", map[string]any{"connectionId": "fixture", "parentNodeKey": "category:2", "depth": 1})
		output := invokeMangoBrowse(t, host, pluginID, "site.feed", map[string]any{"connectionId": "fixture", "routeKey": "catalog:2", "refreshSession": "fixture"})
		var sections []struct {
			Items []struct {
				Work struct {
					PosterURL string `json:"posterUrl"`
				} `json:"work"`
			} `json:"items"`
		}
		if json.Unmarshal(output, &sections) != nil || len(sections) != 1 || len(sections[0].Items) != 2 || sections[0].Items[0].Work.PosterURL == "" {
			t.Fatal("complete recorded catalogue lost items or allowed poster")
		}
		invokeMangoBrowse(t, host, pluginID, "library.artwork_candidates", map[string]any{"connectionId": "fixture", "scopeKey": "route:catalog:2"})
		size := host.modules[pluginID].module.Memory().Size()
		if iteration == 4 {
			warmSize = size
		}
		if iteration > 4 && size != warmSize {
			t.Fatalf("guest heap grows after warmup: warm=%d current=%d", warmSize, size)
		}
	}
	t.Logf("actual guest: 320 browse calls, %d Host calls, stable warm heap=%d bytes", api.calls, warmSize)
}

func TestMangoWASMBrowseArtworkHasNetworkBudget(t *testing.T) {
	host, api, pluginID := mangoBrowseFixture(t)
	api.delay = 1100 * time.Millisecond // two valid control requests exceed the old 2s artwork budget
	invokeMangoBrowse(t, host, pluginID, "library.artwork_candidates", map[string]any{"connectionId": "fixture", "scopeKey": "route:catalog:2"})
	api.delay = 0
	invokeMangoBrowse(t, host, pluginID, "site.navigation", map[string]any{"connectionId": "fixture"})
}

func TestMangoWASMBrowseRecoversAfterCancelledRequest(t *testing.T) {
	host, api, pluginID := mangoBrowseFixture(t)
	invokeMangoBrowse(t, host, pluginID, "site.navigation", map[string]any{"connectionId": "fixture"})
	api.blocked = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := host.Invoke(ctx, pluginID, "site.navigation", []byte(`{"connectionId":"fixture","parentNodeKey":"category:2","depth":1}`))
		done <- err
	}()
	<-api.blocked
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled browse was accepted")
	}
	if !host.modules[pluginID].module.IsClosed() {
		t.Fatal("cancel did not interrupt and close guest")
	}
	api.blocked = nil
	invokeMangoBrowse(t, host, pluginID, "site.navigation", map[string]any{"connectionId": "fixture"})
	invokeMangoBrowse(t, host, pluginID, "site.feed", map[string]any{"connectionId": "fixture", "routeKey": "catalog:2"})
}

func TestMangoWASMBrowseQueuedCancelledRequestDoesNotCloseHealthyGuest(t *testing.T) {
	host, api, pluginID := mangoBrowseFixture(t)
	original := host.modules[pluginID].module
	api.blocked, api.release = make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := host.Invoke(context.Background(), pluginID, "site.navigation", []byte(`{"connectionId":"fixture"}`))
		first <- err
	}()
	<-api.blocked
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	queued := make(chan error, 1)
	go func() {
		_, err := host.Invoke(ctx, pluginID, "site.navigation", []byte(`{"connectionId":"fixture"}`))
		queued <- err
	}()
	close(api.release)
	if err := <-first; err != nil {
		t.Fatalf("healthy call failed: code=%s", ErrorCode(err))
	}
	if err := <-queued; err == nil {
		t.Fatal("cancelled queued call succeeded")
	}
	if api.calls != 1 || original.IsClosed() || host.modules[pluginID].module != original {
		t.Fatal("queued cancellation touched or replaced healthy guest")
	}
	api.blocked, api.release = nil, nil
	invokeMangoBrowse(t, host, pluginID, "site.navigation", map[string]any{"connectionId": "fixture"})
}

func TestMangoWASMBrowseRecoveryPinsOriginalBytesAndSerializes(t *testing.T) {
	host, _, pluginID := mangoBrowseFixture(t)
	path := filepath.Join(t.TempDir(), "plugin.wasm")
	if err := os.WriteFile(path, host.modules[pluginID].source, 0600); err != nil {
		t.Fatal(err)
	}
	if err := host.Start(context.Background(), pluginID, path, 2); err != nil {
		t.Fatal(err)
	}
	original := host.modules[pluginID].module
	if err := original.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, apiVersionWASM(2), 0600); err != nil {
		t.Fatal(err)
	}
	const callers = 8
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, err := host.Invoke(context.Background(), pluginID, "site.navigation", []byte(`{"connectionId":"fixture"}`))
			results <- err
		}()
	}
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatalf("recovery code=%s", ErrorCode(err))
		}
	}
	if host.modules[pluginID].module == original || host.modules[pluginID].module.IsClosed() {
		t.Fatal("recovery failed")
	}
	if err := host.Stop(pluginID); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Invoke(context.Background(), pluginID, "site.navigation", []byte(`{"connectionId":"fixture"}`)); ErrorCode(err) != CodeUnavailable {
		t.Fatal("stopped generation was resurrected")
	}
}

func TestMangoWASMBrowseDeadlineRecoversOnlyNextCall(t *testing.T) {
	host, api, pluginID := mangoBrowseFixture(t)
	api.blocked = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := host.Invoke(ctx, pluginID, "site.navigation", []byte(`{"connectionId":"fixture"}`))
	if ErrorCode(err) != CodeStartTimeout || api.calls != 1 || !host.modules[pluginID].module.IsClosed() {
		t.Fatal("deadline did not interrupt exactly one invocation")
	}
	api.blocked = nil
	invokeMangoBrowse(t, host, pluginID, "site.navigation", map[string]any{"connectionId": "fixture"})
	if api.calls != 2 {
		t.Fatal("timed out invocation was automatically replayed")
	}
}

func TestMangoWASMBrowseQueuedOldGenerationCannotRecoverAfterStopOrReplace(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace=%t", replace), func(t *testing.T) {
			host, _, pluginID := mangoBrowseFixture(t)
			running := host.modules[pluginID]
			if err := running.module.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			running.mu.Lock()
			results := make(chan error, 1)
			go func() {
				// The request already captured this generation before waiting on
				// the lock, exactly as Invoke does after registry lookup.
				_, err := host.invokeRunning(context.Background(), pluginID, "site.navigation", 1, []byte(`{"connectionId":"fixture"}`), running)
				results <- err
			}()
			if replace {
				path := filepath.Join(t.TempDir(), "replacement.wasm")
				if err := os.WriteFile(path, running.source, 0600); err != nil {
					t.Fatal(err)
				}
				if err := host.Start(context.Background(), pluginID, path, 2); err != nil {
					t.Fatal(err)
				}
			} else if err := host.Stop(pluginID); err != nil {
				t.Fatal(err)
			}
			running.mu.Unlock()
			if ErrorCode(<-results) != CodeUnavailable || !running.module.IsClosed() || host.runtime.Module(running.name) != nil {
				t.Fatal("old queued generation was resurrected")
			}
			if replace {
				invokeMangoBrowse(t, host, pluginID, "site.navigation", map[string]any{"connectionId": "fixture"})
			}
		})
	}
}
