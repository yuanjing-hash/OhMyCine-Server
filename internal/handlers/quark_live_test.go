//go:build quark_live

package handlers_test

// Explicit media-read-only acceptance helper, excluded from ordinary tests.
// Startup may renew the existing diagnostic TV session under its original
// state lock. The disposable Server never receives a refresh token. Never run
// against a Server's database/key files.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/config"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/handlers"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/httpserver"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

func TestQuarkLiveSignedSTRM(t *testing.T) {
	dir := os.Getenv("OMC_QUARK_LIVE_TV_STATE")
	samplePath := os.Getenv("OMC_QUARK_LIVE_SAMPLE_PATH")
	if dir == "" || samplePath == "" {
		t.Skip("explicit existing TV state directory and authorized sample path required")
	}
	manualSeconds := 0
	if value := strings.TrimSpace(os.Getenv("OMC_QUARK_LIVE_MANUAL_SECONDS")); value != "" {
		var err error
		manualSeconds, err = strconv.Atoi(value)
		if err != nil || manualSeconds < 0 || manualSeconds > 600 {
			t.Fatal("manual validation duration must be 0 through 600 seconds")
		}
	}
	if !strings.HasPrefix(samplePath, "/") || strings.ContainsAny(samplePath, "\\\r\n") {
		t.Fatal("absolute cloud sample path required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(120+manualSeconds)*time.Second)
	defer cancel()
	// Interactive finite-range playback periodically revalidates cloud scope.
	// Its fixed allowance includes setup; automatic probes retain their 40 cap.
	maxRequests := 40
	if manualSeconds > 0 {
		maxRequests = 160
	}
	var mu sync.Mutex
	requests := 0
	guard := func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if requests > maxRequests {
			return fmt.Errorf("live request budget exceeded")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	}
	tvState, err := openLiveTVStore(dir)
	if err != nil {
		t.Fatal(liveTVMessage(err))
	}
	defer tvState.lock.Close()
	defer func() {
		if err := tvState.unchanged(); err != nil {
			t.Error(liveTVMessage(err))
		}
	}()
	bundle, renewed, err := tvState.ready(ctx, quark.NewAuthenticator(cloud.Config{BeforeCall: guard}))
	if err != nil {
		t.Fatal(liveTVMessage(err))
	}
	if renewed {
		t.Log("TV 启动续期或已保存检查点恢复成功，同账号身份已确认，已加密保存。")
	}
	// Marshal only after startup validation/renewal. The temporary database and
	// driver must receive the new access-only snapshot, never the expired pair.
	raw, _ := json.Marshal(bundle)
	driver, err := quark.NewClient(cloud.Config{Credentials: raw, BeforeCall: guard})
	if err != nil {
		t.Fatal("native driver initialization failed")
	}
	// Resolve only the explicitly authorized sample, with exact directory names.
	parent := "0"
	var root cloud.Item
	for _, wanted := range strings.Split(strings.Trim(samplePath, "/"), "/") {
		if wanted == "" || wanted == "." || wanted == ".." {
			t.Fatal("invalid cloud sample path")
		}
		page, e := driver.List(ctx, parent, cloud.PageRequest{Limit: 100})
		if e != nil || page.HasMore {
			code, _ := cloud.ErrorInfo(e)
			t.Fatalf("sample ancestor lookup: %s", code)
		}
		count := 0
		for _, item := range page.Items {
			if item.IsDir && item.Name == wanted {
				root = item
				count++
			}
		}
		if count != 1 {
			t.Fatalf("sample directory has %d exact matches", count)
		}
		parent = root.ID
	}
	root, err = driver.Stat(ctx, root.ID)
	if err != nil {
		code, _ := cloud.ErrorInfo(err)
		t.Fatalf("native directory lookup: %s", code)
	}
	var sample cloud.Item
	for depth := 0; depth < 3; depth++ {
		page, err := driver.List(ctx, root.ID, cloud.PageRequest{Limit: 100})
		if err != nil || page.HasMore {
			code, _ := cloud.ErrorInfo(err)
			t.Fatalf("bounded sample directory: %s", code)
		}
		var children []cloud.Item
		for _, item := range page.Items {
			if item.IsDir {
				children = append(children, item)
			} else if sample.ID == "" && strings.EqualFold(filepath.Ext(item.Name), ".mkv") {
				sample = item
			}
		}
		if sample.ID != "" {
			break
		}
		if len(children) != 1 {
			t.Fatal("sample directory has no unique bounded descent")
		}
		root = children[0]
	}
	if sample.ID == "" || sample.Size < 131072 {
		t.Fatal("existing MKV sample unavailable")
	}
	if quarkBridgeEnabled() && (samplePath != quarkLibmpvSamplePath || sample.Size != quarkLibmpvExpectedSize) {
		t.Fatal("bridge_validation_authorized_sample_required")
	}
	db, err := database.Open(filepath.Join(t.TempDir(), "isolated-live.db"))
	if err != nil {
		t.Fatal("isolated database unavailable")
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err := database.Migrate(db); err != nil {
		t.Fatal("isolated migration failed")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal("isolated key unavailable")
	}
	store, err := credential.Open("", base64.RawStdEncoding.EncodeToString(secret))
	if err != nil {
		t.Fatal("isolated encryption unavailable")
	}
	connection := models.Connection{Name: "isolated-live", NameNormalized: "isolated-live", Provider: models.ConnectionProviderQuark, Enabled: true, Revision: 1}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal("isolated connection failed")
	}
	ciphertext, err := store.Encrypt(fmt.Sprintf("connection:%d:quark:authorizations:v1", connection.ID), string(raw))
	if err != nil {
		t.Fatal("isolated credential encryption failed")
	}
	if err := db.Model(&connection).Update("credential_ciphertext", ciphertext).Error; err != nil {
		t.Fatal("isolated credential persistence failed")
	}
	playbackDiagnostic := newQuarkPlaybackDiagnostic()
	registry := cloud.NewRegistry()
	if err := registry.Register(cloud.ProviderQuark, func(c cloud.Config) (cloud.Driver, error) {
		c.PersistCredentials, c.LoadCredentials = nil, nil
		c.BeforeCall = guard
		client, err := quark.NewClient(c)
		if err != nil || playbackDiagnostic == nil {
			return client, err
		}
		return &quarkPlaybackDiagnosticDriver{Client: client, diagnostic: playbackDiagnostic}, nil
	}); err != nil {
		t.Fatal("native registry failed")
	}
	connections := services.NewConnectionService(db, services.NewAuditService(db), store, registry, zerolog.Nop())
	storage := models.Storage{Name: "isolated", NameNormalized: "isolated", Type: models.StorageTypeQuark, RootPath: root.ID, RootDisplayPath: "/", RootPathNormalized: "isolated-live", ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal("isolated storage failed")
	}
	var profile models.MediaClassificationProfile
	if err := db.Where("code=?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal("isolated profile failed")
	}
	library := models.MediaLibrary{Name: "isolated", NameNormalized: "isolated", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", ProviderRootID: root.ID, Enabled: true, STRMEnabled: true, SignedProxyEnabled: true, STRMLocalRoot: t.TempDir(), VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`}
	if err := db.Select("*").Create(&library).Error; err != nil {
		t.Fatal("isolated library failed")
	}
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/" + sample.Name, ProviderID: sample.ID, Size: sample.Size, MatchStatus: "unrecognized", WorkKey: "file:live-original", MediaType: "movie", Title: "Isolated original sample"}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal("isolated catalog failed")
	}
	run := models.MediaArtifactRun{ID: "live-original", LibraryID: library.ID, Generation: 1, Status: models.MediaArtifactStatusCompleted}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal("isolated artifact run failed")
	}
	artifact := models.MediaArtifact{OpaqueID: "quark_live_AAAAAAAAAAAAAAAA", RunID: run.ID, LibraryID: library.ID, ProviderItemID: sample.ID, SourceFingerprint: "live-sample", Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/sample.strm", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted}
	if err := db.Select("*").Create(&artifact).Error; err != nil {
		t.Fatal("isolated artifact failed")
	}
	gin.SetMode(gin.TestMode)
	server := httptest.NewUnstartedServer(nil)
	defer server.Close()
	origin := "http://" + server.Listener.Addr().String()
	cfg := config.Config{PublicOrigin: origin, SessionIdleTTL: time.Hour, SessionMaxTTL: 24 * time.Hour}
	audit := services.NewAuditService(db)
	authorization := services.NewAuthorizationService(db)
	auth, err := services.NewAuthService(db, cfg, authorization, audit, zerolog.Nop())
	if err != nil {
		t.Fatal("isolated authentication initialization failed")
	}
	loginSecret := make([]byte, 24)
	if _, err := rand.Read(loginSecret); err != nil {
		t.Fatal("isolated account password unavailable")
	}
	password := "live-" + base64.RawURLEncoding.EncodeToString(loginSecret)
	owner, _, err := auth.SetupOwner(services.SetupInput{Username: "validation-owner", Password: password}, services.RequestContext{})
	if err != nil {
		t.Fatal("isolated owner setup failed")
	}
	admin := services.NewAdminService(db, authorization, auth, audit)
	var normalRole models.Role
	if err := db.Where("code = ?", authz.RoleUser).First(&normalRole).Error; err != nil {
		t.Fatal("built-in normal user role unavailable")
	}
	viewer, err := admin.CreateUser(owner, services.CreateUserInput{Username: "validation-viewer", Password: password, RoleIDs: []uint{normalRole.ID}}, services.RequestContext{})
	if err != nil {
		t.Fatal("isolated normal user setup failed")
	}
	// Use the normal administration path to grant this non-owner access to only
	// the isolated library. This validates Player authorization independently of
	// the current built-in role's permission seed.
	if err := admin.ReplaceUserAuthorizationRules(owner, viewer.ID, services.ReplaceUserAuthorizationRulesInput{
		Rules: []services.AuthorizationRule{{
			PermissionCode: authz.PermissionMediaLibrariesRead,
			Effect:         models.AuthorizationEffectAllow,
			ResourceType:   models.AuthorizationResourceMediaLibrary,
			ResourceID:     fmt.Sprint(library.ID),
		}},
	}, services.RequestContext{}); err != nil {
		t.Fatal("isolated scoped library grant failed")
	}
	libraries := services.NewMediaLibraryService(db, audit, zerolog.Nop())
	libraries.SetConnectionService(connections)
	api := handlers.NewAPI(cfg, auth, admin, audit, nil, nil, nil, zerolog.Nop())
	api.SetConnectionService(connections)
	api.SetMediaLibraryService(libraries)
	proxy, err := services.NewSignedProxyService(db, store, connections, origin, zerolog.Nop())
	if err != nil {
		t.Fatal("formal proxy initialization failed")
	}
	api.SetSignedProxyService(proxy)
	serverHandler, bridgeMeter := quarkBridgeInstrument(httpserver.New(cfg, api, auth, zerolog.Nop()))
	server.Config.Handler = playbackDiagnostic.instrument(serverHandler)
	server.Start()
	signed, err := proxy.SignArtifact(artifact.OpaqueID, library.ID, time.Hour)
	if err != nil {
		t.Fatal("formal signature failed")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	media := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	apiRequest := func(method, endpoint, token string, body any, want int, output any) http.Header {
		t.Helper()
		var payload []byte
		if body != nil {
			payload, _ = json.Marshal(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, origin+endpoint, bytes.NewReader(payload))
		if err != nil {
			t.Fatal("isolated API request invalid")
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("User-Agent", "OhMyCine-original-validation")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal("isolated API request failed")
		}
		defer res.Body.Close()
		rawResponse, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		if err != nil || res.StatusCode != want || res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("formal Player API status=%d expected=%d", res.StatusCode, want)
		}
		if output != nil {
			var result struct {
				Code int             `json:"code"`
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal(rawResponse, &result) != nil || json.Unmarshal(result.Data, output) != nil {
				t.Fatal("formal Player API response shape invalid")
			}
		}
		return res.Header.Clone()
	}
	streamPath := fmt.Sprintf("/api/v1/player/media-entries/%d/stream", entry.ID)
	apiRequest(http.MethodGet, streamPath, "", nil, http.StatusUnauthorized, nil)
	var login struct {
		AccessToken string `json:"access_token"`
	}
	loginHeaders := apiRequest(http.MethodPost, "/api/v1/player/auth/login", "", map[string]string{"username": viewer.Username, "password": password, "device_id": "quark-live-readonly", "device_name": "Isolated read-only validation"}, http.StatusOK, &login)
	if !strings.HasPrefix(login.AccessToken, "omc_player_") || len(loginHeaders.Values("Set-Cookie")) != 0 {
		t.Fatal("formal Player bearer login failed")
	}
	var visible struct {
		List []services.PlayerMediaLibrary `json:"list"`
	}
	apiRequest(http.MethodGet, "/api/v1/player/media-libraries", login.AccessToken, nil, http.StatusOK, &visible)
	if len(visible.List) != 1 || visible.List[0].ID != library.ID || visible.List[0].StorageType != cloud.ProviderQuark || !visible.List[0].DirectStream {
		t.Fatal("normal user Quark library unavailable")
	}
	catalogPath := fmt.Sprintf("/api/v1/player/media-libraries/%d/catalog", library.ID)
	var catalog services.PlayerMediaItemPage
	apiRequest(http.MethodGet, catalogPath, login.AccessToken, nil, http.StatusOK, &catalog)
	if len(catalog.List) != 1 {
		t.Fatal("normal user catalog sample missing")
	}
	var detail services.PlayerMediaDetail
	apiRequest(http.MethodGet, catalogPath+"/"+url.PathEscape(catalog.List[0].ID), login.AccessToken, nil, http.StatusOK, &detail)
	if len(detail.Versions) != 1 || detail.Versions[0].ID != entry.ID || !detail.Versions[0].Playable || detail.Versions[0].DeliveryKind != "server_redirect" || detail.Versions[0].StreamPath != streamPath {
		t.Fatal("formal Player detail lost original redirect route")
	}
	t.Log("normal_user_login=200 library=200 catalog=200 detail=200 unauthenticated_stream=401")
	redirect := func(endpoint, token, rangeHeader string) string {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		req.Header.Set("Range", rangeHeader)
		req.Header.Set("User-Agent", "OhMyCine-original-validation")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal("formal HTTP redirect failed")
		}
		defer res.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(res.Body, 1024))
		if readErr != nil || res.StatusCode != 302 || len(body) != 0 || res.Header.Get("Location") == "" || res.Header.Get("Cache-Control") != "no-store" || len(res.Header.Values("Set-Cookie")) != 0 {
			t.Fatalf("formal redirect status=%d body_bytes=%d", res.StatusCode, len(body))
		}
		return res.Header.Get("Location")
	}
	for _, start := range []int64{0, sample.Size - 65536} {
		rangeHeader := fmt.Sprintf("bytes=%d-%d", start, start+65535)
		strmURL := redirect(signed, "", rangeHeader)
		playerURL := redirect(origin+streamPath, login.AccessToken, rangeHeader)
		// Both formal routes must redirect with no media body. Read only one
		// range from each: STRM head and authenticated Player tail, 128 KiB total.
		location, route := strmURL, "signed_strm"
		if start != 0 {
			location, route = playerURL, "authenticated_player"
		}
		upstream, _ := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
		upstream.Header.Set("Range", rangeHeader)
		upstream.Header.Set("User-Agent", "OhMyCine-original-validation")
		data, err := media.Do(upstream)
		if err != nil {
			t.Fatal("anonymous original range request failed")
		}
		content, readErr := io.ReadAll(io.LimitReader(data.Body, 65537))
		data.Body.Close()
		if readErr != nil || data.StatusCode != 206 || len(content) != 65536 || data.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", start, start+65535, sample.Size) {
			t.Fatalf("anonymous range status=%d bytes=%d", data.StatusCode, len(content))
		}
		t.Logf("route=%s original_size=%d range_start=%d bytes=%d sha256=%x server_video_bytes=0", route, sample.Size, start, len(content), sha256.Sum256(content))
	}
	func() {
		stopDiagnostic := playbackDiagnostic.begin(t)
		defer stopDiagnostic()
		runQuarkLiveBridgeHook(t, ctx, origin, streamPath, login.AccessToken, sample.Size, bridgeMeter, func() int {
			mu.Lock()
			defer mu.Unlock()
			return requests
		})
	}()
	if manualSeconds > 0 {
		// Explicit opt-in only. The disposable local password below is unrelated
		// to any cloud or production account and is never printed by normal runs.
		// The service stays on loopback; no background process survives this test.
		t.Logf("manual_player_url=%s username=%s temporary_password=%s expires_in_seconds=%d", origin, viewer.Username, password, manualSeconds)
		t.Logf("manual Player validation enabled; close with Ctrl+C or wait for expiry; cloud API budget includes setup and is limited to %d requests", maxRequests)
		manualContext, stop := signal.NotifyContext(ctx, os.Interrupt)
		timer := time.NewTimer(time.Duration(manualSeconds) * time.Second)
		select {
		case <-manualContext.Done():
		case <-timer.C:
		}
		timer.Stop()
		stop()
	}
	// Revoke this library through the real user-management service. Existing
	// bearer sessions must lose access before another provider request is made.
	access, err := admin.UserResourceAccess(owner, viewer.ID)
	if err != nil {
		t.Fatal("isolated resource access read failed")
	}
	for i := range access.Policies {
		if access.Policies[i].Scope == models.ResourceAccessScopeLibraryRead {
			access.Policies[i].Mode, access.Policies[i].ResourceIDs = models.ResourceAccessModeAllowlist, []string{}
		}
	}
	if _, err = admin.ReplaceUserResourceAccess(owner, viewer.ID, access, services.RequestContext{}); err != nil {
		t.Fatal("isolated resource access update failed")
	}
	mu.Lock()
	beforeDenied := requests
	mu.Unlock()
	apiRequest(http.MethodGet, catalogPath, login.AccessToken, nil, http.StatusForbidden, nil)
	apiRequest(http.MethodGet, streamPath, login.AccessToken, nil, http.StatusForbidden, nil)
	apiRequest(http.MethodPost, "/api/v1/player/auth/logout", login.AccessToken, map[string]any{}, http.StatusOK, nil)
	apiRequest(http.MethodGet, streamPath, login.AccessToken, nil, http.StatusUnauthorized, nil)
	mu.Lock()
	afterDenied := requests
	mu.Unlock()
	if beforeDenied != afterDenied {
		t.Fatal("denied or revoked Player access reached provider")
	}
	t.Logf("library_permission_denied=403 stream_permission_denied=403 revoked_stream=401 denied_provider_requests=0 api_requests=%d", afterDenied)

}
