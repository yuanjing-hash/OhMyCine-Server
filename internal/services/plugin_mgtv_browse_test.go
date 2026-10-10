package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
	pluginruntime "github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/runtime"
)

type mangoBrowseControl struct {
	files  string
	bad    bool
	reads  int
	client *http.Client
}

func mangoBrowseService(t *testing.T, live bool) (*PluginRepositoryService, Actor, string, *hostapi.Host, *mangoBrowseControl) {
	t.Helper()
	entry, template, fixtures := os.Getenv("OMC_MGTV_WASM"), os.Getenv("OMC_MGTV_MANIFEST"), os.Getenv("OMC_MGTV_FIXTURES")
	if entry == "" || template == "" || fixtures == "" {
		t.Skip("set actual WASM, source manifest and full public fixture paths")
	}
	service, actor, _ := pluginRepositoryFixture(t)
	actor.Permissions[authz.PermissionMediaLibrariesRead] = struct{}{}
	manifestBytes, err := os.ReadFile(template)
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes = []byte(strings.ReplaceAll(string(manifestBytes), "${PACKAGE_SHA256}", strings.Repeat("a", 64)))
	var manifest contract.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pkg := models.PluginPackage{PluginID: manifest.ID, Version: manifest.Version, ManifestJSON: string(manifestBytes), PackageSHA256: strings.Repeat("a", 64), CreatedAt: now, VerifiedAt: now}
	if err := service.db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.PluginInstallation{PluginID: manifest.ID, ActivePackageID: pkg.ID, Status: models.PluginInstallationEnabled, RuntimeGeneration: 1, Revision: 1, InstalledAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	for index, permission := range manifest.Permissions {
		encoded, _ := json.Marshal(permission)
		if err := service.db.Create(&models.PluginPermissionGrant{PluginID: manifest.ID, PluginPackageID: pkg.ID, PermissionKey: fmt.Sprintf("fixture-%d", index), PermissionJSON: string(encoded), CreatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	connection := models.PluginConnection{ID: uuid.NewString(), PluginID: manifest.ID, Name: "Anonymous browse fixture", ConfigJSON: `{}`, CredentialMode: models.PluginCredentialModeCookie, CredentialScope: "mgtv.session", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.PluginOnlineLibrary{ID: connection.ID, PluginID: manifest.ID, ConnectionID: connection.ID, ExternalKey: "default", Name: connection.Name, HomeContributionsJSON: `[]`, Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	service.credentials, err = credential.Open(filepath.Join(t.TempDir(), "fixture.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	control := &mangoBrowseControl{files: fixtures}
	client := &http.Client{Transport: authControlTransport(func(request *http.Request) (*http.Response, error) {
		control.reads++
		if request.Method != http.MethodGet || request.Header.Get("Cookie") != "" || request.Header.Get("Authorization") != "" {
			t.Fatal("browse issued a write or account credential request")
		}
		var body []byte
		mime := "application/json"
		var file string
		switch request.URL.Path {
		case "/rider/config/platformChannels/v1":
			file = "channels.json"
		case "/rider/config/channel/v1":
			file = "channel.json"
		case "/rider/list/pcweb/v3":
			file = "catalog.json"
		case "/video/info":
			file = "info.json"
		case "/episode/list":
			file = "episodes.json"
		default:
			if !strings.HasSuffix(request.URL.Hostname(), "img.hitv.com") {
				t.Fatal("browse attempted an unknown or media endpoint")
			}
			body, mime = []byte{0xff, 0xd8, 0xff, 0xe0}, "image/jpeg"
		}
		if file != "" {
			var readErr error
			body, readErr = os.ReadFile(filepath.Join(fixtures, file))
			if readErr != nil {
				return nil, readErr
			}
		}
		if file == "catalog.json" {
			if control.bad {
				body = []byte(`{"code":200,"data":{"hitDocs":"malformed","hasMore":true}}`)
			} else {
				// The recorded pc=2 payload is genuine. Expand its full card schema
				// to the plugin's pc=40 bound with distinct stable clip identities.
				var document map[string]any
				if json.Unmarshal(body, &document) != nil {
					t.Fatal("invalid recorded catalogue")
				}
				data := document["data"].(map[string]any)
				original := data["hitDocs"].([]any)
				rows := make([]any, 0, 40)
				for index := 0; index < 40; index++ {
					encoded, _ := json.Marshal(original[index%len(original)])
					var row map[string]any
					_ = json.Unmarshal(encoded, &row)
					row["clipId"] = fmt.Sprintf("%d", 778406+index)
					rows = append(rows, row)
				}
				data["hitDocs"] = rows
				body, _ = json.Marshal(document)
			}
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
	})}
	options := []hostapi.Option{hostapi.WithHTTPClient(client), hostapi.WithResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	})}
	control.client = client
	if live {
		options = nil
	}
	api := hostapi.New(service.db, service.credentials, zerolog.Nop(), options...)
	runtime := pluginruntime.NewHost(context.Background())
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runtime.SetCapabilityHost(authTraceHost{api: api, t: t})
	if err := runtime.Start(context.Background(), manifest.ID, entry, 1); err != nil {
		t.Fatal(err)
	}
	service.runtime, service.artwork = runtime, api
	return service, actor, connection.ID, api, control
}

func mangoBrowseCycle(t *testing.T, service *PluginRepositoryService, actor Actor, libraryID string, api *hostapi.Host, expectedCards int) {
	t.Helper()
	raw, err := service.OnlineNavigation(context.Background(), actor, libraryID)
	if err != nil {
		t.Fatalf("navigation service code=%s", ErrorCode(err))
	}
	var navigation pluginNavigationResponse
	if json.Unmarshal(raw, &navigation) != nil || len(navigation.Nodes) < 7 {
		t.Fatal("official root navigation missing")
	}
	var token string
	for _, node := range navigation.Nodes {
		if node.ID == "channel-2" {
			token = node.NodeToken
		}
	}
	if token == "" {
		t.Fatal("official category token missing")
	}
	children, err := service.OnlineNavigationChildren(context.Background(), actor, libraryID, token)
	if err != nil {
		t.Fatalf("children service code=%s", ErrorCode(err))
	}
	var childNodes pluginNavigationResponse
	if json.Unmarshal(children, &childNodes) != nil || len(childNodes.Nodes) < 2 {
		t.Fatal("official category fields missing")
	}
	var filterToken string
	for _, node := range childNodes.Nodes {
		if node.Kind == "branch" {
			filterToken = node.NodeToken
			break
		}
	}
	if filterToken == "" {
		t.Fatal("official filters missing")
	}
	if _, err := service.OnlineNavigationChildren(context.Background(), actor, libraryID, filterToken); err != nil {
		t.Fatalf("filter children code=%s", ErrorCode(err))
	}
	feed, err := service.OnlineFeed(context.Background(), actor, libraryID, "catalog:2", "", "")
	if err != nil {
		t.Fatalf("feed service code=%s", ErrorCode(err))
	}
	var sections []struct {
		Items []struct {
			Work struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				PosterURL string `json:"posterUrl"`
			} `json:"work"`
		} `json:"items"`
	}
	if json.Unmarshal(feed, &sections) != nil || len(sections) != 1 || len(sections[0].Items) == 0 || (expectedCards != 0 && len(sections[0].Items) != expectedCards) {
		t.Fatal("feed lost complete cards")
	}
	work := sections[0].Items[0].Work
	if work.Title == "" || !strings.HasPrefix(work.PosterURL, "/api/v1/player/artwork/") || bytes.Contains(feed, []byte("https://")) {
		t.Fatal("title/poster projection missing or raw transport exposed")
	}
	image, mime, err := api.OpenArtwork(context.Background(), strings.TrimPrefix(work.PosterURL, "/api/v1/player/artwork/"))
	if err != nil || len(image) == 0 || !strings.HasPrefix(mime, "image/") {
		t.Fatalf("projected poster unavailable: code=%s", hostapi.ErrorCode(err))
	}
	if _, err := service.OnlineDetail(context.Background(), actor, libraryID, work.ID); err != nil {
		t.Fatalf("detail service code=%s", ErrorCode(err))
	}
	artwork, err := service.OnlineArtworkCandidates(context.Background(), actor, libraryID, "route:catalog:2")
	if err != nil || len(artwork) == 0 {
		t.Fatalf("artwork service code=%s", ErrorCode(err))
	}
	for _, candidate := range artwork {
		asset, err := api.ResolveAsset(candidate.AssetRef)
		if err != nil || asset.ConnectionID != libraryID || asset.PluginID != "org.ohmycine.mgtv" {
			t.Fatal("artwork candidate lost owner-scoped asset")
		}
	}
	t.Logf("complete browse: categories=%d cards=%d owner-bound artwork=%d", len(navigation.Nodes), len(sections[0].Items), len(artwork))
}

func TestMangoWASMBrowseServiceFullPayloadAndErrors(t *testing.T) {
	service, actor, libraryID, api, control := mangoBrowseService(t, false)
	for i := 0; i < 8; i++ {
		mangoBrowseCycle(t, service, actor, libraryID, api, 40)
	}
	control.bad = true
	// A normal cached first page survives an upstream outage. Explicit refresh
	// must still reject malformed data and retain the previous valid snapshot.
	scope, err := service.catalogueScope(libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _, err := service.catalogueRead(context.Background(), scope, "feed", "catalog:2", 0, nil, true); err == nil || len(raw) != 0 {
		t.Fatal("malformed upstream catalogue was accepted as empty/successful feed")
	}
	control.bad = false
	mangoBrowseCycle(t, service, actor, libraryID, api, 40)
	t.Logf("controlled complete Host/service requests=%d; no credentials or media bytes", control.reads)
}

func TestMangoWASMBrowseServiceAnonymousLive(t *testing.T) {
	if os.Getenv("OMC_MGTV_BROWSE_LIVE") != "1" {
		t.Skip("set OMC_MGTV_BROWSE_LIVE=1 to opt into anonymous official catalogue and bounded poster reads")
	}
	service, actor, libraryID, api, _ := mangoBrowseService(t, true)
	mangoBrowseCycle(t, service, actor, libraryID, api, 0)
}

func TestMangoWASMBrowseRuntimeDiagnosticContainsOnlyStableFields(t *testing.T) {
	service, actor, libraryID, _, _ := mangoBrowseService(t, false)
	var records bytes.Buffer
	service.log = zerolog.New(&records)
	service.runtime = &onlinePluginRuntime{handler: func(string, []byte) ([]byte, error) {
		return nil, &pluginruntime.Error{Code: pluginruntime.CodeResponseInvalid, Cause: errors.New("private-cause https://provider.invalid/media?token=private-secret Cookie=private-cookie")}
	}}
	if _, err := service.OnlineNavigation(context.Background(), actor, libraryID); ErrorCode(err) != CodePluginRuntimeUnavailable {
		t.Fatal("runtime failure was swallowed")
	}
	var record map[string]any
	if json.Unmarshal(records.Bytes(), &record) != nil || record["error_code"] != pluginruntime.CodeResponseInvalid || record["operation"] != "site.navigation" || record["plugin_id"] != "org.ohmycine.mgtv" {
		t.Fatal("safe runtime diagnostic missing")
	}
	for _, secret := range []string{"private-cause", "provider.invalid", "private-secret", "private-cookie", "connectionId", libraryID} {
		if strings.Contains(records.String(), secret) {
			t.Fatal("runtime diagnostic leaked cause or request")
		}
	}
}
