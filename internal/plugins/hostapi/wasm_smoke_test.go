package hostapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	pluginruntime "github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/runtime"
)

// Opt-in integration against a locally built official plugin. This exercises
// actual WASM → current Server Host HTTP/storage/clock ABI with public fixtures;
// it neither contacts the provider nor authenticates a real account.
func TestMangoWASMCurrentHostPublicFixtureSmoke(t *testing.T) {
	entry, fixtures := os.Getenv("OMC_MGTV_WASM"), os.Getenv("OMC_MGTV_FIXTURES")
	if entry == "" || fixtures == "" {
		t.Skip("set OMC_MGTV_WASM and OMC_MGTV_FIXTURES to opt into actual plugin integration")
	}
	storageBytes := int64(8 << 20)
	fixture := newHostFixture(t, []contract.Permission{
		{Kind: contract.PermissionNetworkHTTP, Domains: []string{"pianku.api.mgtv.com", "pcweb.api.mgtv.com", "nuc.api.mgtv.com"}},
		{Kind: contract.PermissionPrivateStorage, MaxBytes: &storageBytes},
	})
	if err := fixture.db.Model(&models.PluginConnection{}).Where("id = ?", fixture.connection.ID).Updates(map[string]any{"credential_scope": "mgtv.session", "credential_ciphertext": ""}).Error; err != nil {
		t.Fatal(err)
	}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		t.Logf("public control path: %s", request.URL.Path)
		if request.Method != http.MethodGet || request.Header.Get("Cookie") != "" {
			t.Fatal("public smoke issued a write or credential request")
		}
		var file string
		switch request.URL.Path {
		case "/rider/config/platformChannels/v1":
			file = "channels.json"
		case "/video/info":
			file = "info.json"
		case "/episode/list":
			file = "episodes.json"
		case "/GetMultiPic":
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":200,"data":{"rcode":"fixture-code","url":"https://oauth.mgtv.com/qrcode/fixture"}}`)), Request: request}, nil
		default:
			t.Fatalf("unexpected public control endpoint %s", request.URL.Path)
		}
		body, err := os.ReadFile(filepath.Join(fixtures, file))
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
	})}
	api := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), WithResolver(publicResolver))
	runtime := pluginruntime.NewHost(context.Background())
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runtime.SetCapabilityHost(api)
	if err := runtime.Start(context.Background(), fixture.pluginID, entry, 1); err != nil {
		t.Fatal(err)
	}
	for _, selection := range []struct {
		operation string
		input     map[string]string
	}{
		{"site.navigation", map[string]string{"connectionId": fixture.connection.ID}},
		{"site.detail", map[string]string{"connectionId": fixture.connection.ID, "itemId": "clip:778406"}},
		{"site.auth.start", map[string]string{"connectionId": fixture.connection.ID}},
	} {
		t.Run(selection.operation, func(t *testing.T) {
			input, _ := json.Marshal(selection.input)
			output, err := runtime.Invoke(context.Background(), fixture.pluginID, selection.operation, input)
			if err != nil {
				t.Fatalf("%s: %v", selection.operation, err)
			}
			var response map[string]any
			if json.Unmarshal(output, &response) != nil || response["pluginError"] != nil {
				t.Fatalf("%s returned business error: %s", selection.operation, output)
			}
			if selection.operation == "site.auth.start" && response["loginSession"] == nil {
				t.Fatal("QR storage/clock response missing")
			}
		})
	}
	if requests < 4 {
		t.Fatal("actual Host HTTP ABI not exercised")
	}
}

func TestMangoWASMCurrentHostMediaPlaybackAndOfflineEntitlement(t *testing.T) {
	entry, fixtures := os.Getenv("OMC_MGTV_WASM"), os.Getenv("OMC_MGTV_FIXTURES")
	if entry == "" || fixtures == "" {
		t.Skip("set actual WASM and public fixture paths")
	}
	fixture := newHostFixture(t, []contract.Permission{
		{Kind: contract.PermissionNetworkHTTP, Domains: []string{"pcweb.api.mgtv.com", "web-disp.titan.mgtv.com", "web-disp1.titan.mgtv.com", "web-disp2.titan.mgtv.com", "pcvideomigu.titan.mgtv.com", "mobile-stream.api.mgtv.com"}},
		{Kind: contract.PermissionCredentialUse, Scopes: []string{"mgtv.session"}}, {Kind: contract.PermissionDownloadPlan},
	})
	if err := fixture.db.Model(&models.PluginConnection{}).Where("id = ?", fixture.connection.ID).Updates(map[string]any{"credential_scope": "mgtv.session", "credential_ciphertext": ""}).Error; err != nil {
		t.Fatal(err)
	}
	denyDownload := false
	dispatches := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.Header.Get("Cookie") != "" {
			t.Fatal("media public smoke issued write or real credential request")
		}
		var body []byte
		var file string
		switch request.URL.Path {
		case "/video/streamList":
			file = "stream-free.json"
			if request.URL.Query().Get("video_id") != "22025165" {
				file = "stream-preview.json"
			}
		case "/video/info":
			file = "info-free.json"
			if request.URL.Query().Get("cid") != "687100" {
				file = "info.json"
			}
		case "/atcl":
			dispatches++
			body = []byte(`{"status":"ok","info":"https://pcvideomigu.titan.mgtv.com/free.m3u8?short=fixture"}`)
		case "/v2/video/title":
			body = []byte(`{"code":200,"data":{"title":[]}}`)
		default:
			t.Fatalf("smoke attempted media fetch or unknown control path %s", request.URL.Path)
		}
		if file != "" {
			var err error
			body, err = os.ReadFile(filepath.Join(fixtures, file))
			if err != nil {
				return nil, err
			}
		}
		if denyDownload && file == "info-free.json" {
			var info map[string]any
			if json.Unmarshal(body, &info) != nil {
				t.Fatal("invalid info fixture")
			}
			info["data"].(map[string]any)["config"].(map[string]any)["download"].(map[string]any)["downloadDisplay"] = 0
			body, _ = json.Marshal(info)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
	})}
	api := New(fixture.db, fixture.credentials, zerolog.Nop(), WithHTTPClient(client), WithResolver(publicResolver))
	runtime := pluginruntime.NewHost(context.Background())
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runtime.SetCapabilityHost(api)
	if err := runtime.Start(context.Background(), fixture.pluginID, entry, 1); err != nil {
		t.Fatal(err)
	}
	free := map[string]string{"connectionId": fixture.connection.ID, "itemId": "clip:687100", "segmentId": "video:22025165", "versionId": "mgtv:687100:22025165", "variantId": "h264:3:239212057"}
	for _, operation := range []string{"media.playback", "media.offline_download_plan"} {
		t.Run(operation+"_free", func(t *testing.T) {
			input, _ := json.Marshal(free)
			output, err := runtime.Invoke(context.Background(), fixture.pluginID, operation, input)
			if err != nil {
				t.Fatal(err)
			}
			var errorResponse struct {
				Error any `json:"pluginError"`
			}
			if json.Unmarshal(output, &errorResponse) != nil || errorResponse.Error != nil {
				t.Fatalf("free media rejected: %s", output)
			}
			if strings.Contains(string(output), "https://") || strings.Contains(string(output), "short=fixture") {
				t.Fatal("ordinary plugin DTO leaked final transport")
			}
			var ref string
			if operation == "media.playback" {
				var plan contract.PlaybackPlan
				if json.Unmarshal(output, &plan) != nil || contract.ValidatePlaybackPlan(plan, time.Now().UTC()) != nil || plan.VariantID != free["variantId"] {
					t.Fatal("current Go playback DTO invalid")
				}
				ref = plan.Assets[0].URLRef
			} else {
				var plan contract.OfflineDownloadPlan
				if json.Unmarshal(output, &plan) != nil || plan.Version != 1 || plan.Format != "hls" || plan.WorkID != free["itemId"] || plan.SegmentID != free["segmentId"] || plan.VersionID != free["versionId"] || plan.VariantID != free["variantId"] || len(plan.Assets) != 1 || plan.ExpiresAt <= time.Now().Unix() {
					t.Fatal("op20 exact identity/format DTO invalid")
				}
				ref = plan.Assets[0].URLRef
			}
			asset, err := api.ResolveAsset(ref)
			if err != nil || asset.PluginID != fixture.pluginID || asset.ConnectionID != fixture.connection.ID {
				t.Fatal("media did not register owner-bound Host asset")
			}
		})
	}
	if dispatches != 2 {
		t.Fatal("media resolution did not use controlled dispatch twice")
	}
	for _, operation := range []string{"media.playback", "media.offline_download_plan"} {
		t.Run(operation+"_real_trial_rejected", func(t *testing.T) {
			trial := map[string]string{"connectionId": fixture.connection.ID, "itemId": "clip:778406", "segmentId": "video:24623021", "versionId": "mgtv:778406:24623021", "variantId": "h264:3:239415301"}
			input, _ := json.Marshal(trial)
			output, err := runtime.Invoke(context.Background(), fixture.pluginID, operation, input)
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"pluginError"`
			}
			if json.Unmarshal(output, &envelope) != nil || envelope.Error.Code != "permission-denied" {
				t.Fatalf("real trial not rejected: %s", output)
			}
			if dispatches != 2 {
				t.Fatal("trial reached CDN dispatch")
			}
		})
	}
	denyDownload = true
	input, _ := json.Marshal(free)
	output, err := runtime.Invoke(context.Background(), fixture.pluginID, "media.offline_download_plan", input)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"pluginError"`
	}
	if json.Unmarshal(output, &envelope) != nil || envelope.Error.Code != "permission-denied" || dispatches != 2 {
		t.Fatal("synthetic disabled-download negative reached media")
	}
}
