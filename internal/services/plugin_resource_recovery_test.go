package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/browsercompanion"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
)

// The guest substitute executes real Host authorization, DNS policy, credential
// scope and BrowserRequest dispatch. Only provider parsing and local IPC/browser
// processes are controlled substitutes; no network or account is contacted.
type recoveryRuntime struct {
	*resourcePluginRuntime
	host               *hostapi.Host
	method             string
	mu                 sync.Mutex
	operations         []string
	remaining          map[string]time.Duration
	uncredentialedPOST bool
}

func (r *recoveryRuntime) Invoke(ctx context.Context, pluginID, operation string, request []byte) ([]byte, error) {
	var input struct {
		ConnectionID string `json:"connectionId"`
	}
	if err := json.Unmarshal(request, &input); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.operations = append(r.operations, operation)
	if deadline, ok := ctx.Deadline(); ok {
		r.remaining[operation] = time.Until(deadline)
	}
	r.mu.Unlock()
	method := r.method
	if method == "" {
		method = http.MethodGet
	}
	endpoint := "health"
	if operation == "resource.search" {
		endpoint = "search"
		if r.uncredentialedPOST {
			public, _ := json.Marshal(map[string]any{"method": "POST", "url": "https://mirror.example/public-search"})
			if _, err := r.host.Call(ctx, pluginID, hostapi.OperationHTTP, public); err != nil {
				return nil, err
			}
		}
	}
	raw, _ := json.Marshal(map[string]any{"connectionId": input.ConnectionID, "credentialRef": "resource.session", "method": method, "url": "https://mirror.example/" + endpoint, "timeoutMs": 12000})
	response, err := r.host.Call(ctx, pluginID, hostapi.OperationHTTP, raw)
	if err != nil {
		return []byte(`{"pluginError":{"code":"upstream-unavailable","message":"untrusted hidden detail"}}`), nil
	}
	var envelope struct {
		Data struct {
			Status int    `json:"status"`
			Body   string `json:"bodyBase64"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data.Status != http.StatusOK {
		return []byte(`{"pluginError":{"code":"upstream-unavailable"}}`), nil
	}
	return base64.StdEncoding.DecodeString(envelope.Data.Body)
}

type recoveryBrowser struct {
	mu                                        sync.Mutex
	creates, requests, closes, health, search int
	imported                                  []browserCookie
	healthResponses                           []string
	failNext                                  string
	failAlways                                string
	status                                    int
	create                                    func(context.Context)
	directPosts                               int
}

type recoveryHTTPRoundTrip func(*http.Request) (*http.Response, error)

func (f recoveryHTTPRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (b *recoveryBrowser) Call(ctx context.Context, operation string, input, output any) error {
	if operation == "session/create" && b.create != nil {
		b.create(ctx)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var response any = map[string]any{}
	switch operation {
	case "session/create":
		b.creates++
		raw, _ := json.Marshal(input)
		var payload struct {
			Cookies []browserCookie `json:"cookies"`
		}
		_ = json.Unmarshal(raw, &payload)
		b.imported = payload.Cookies
		response = map[string]any{"sessionId": uuid.NewString(), "expiresAt": time.Now().Add(29 * time.Minute)}
	case "session/request":
		b.requests++
		payload := input.(map[string]any)
		body := `{"page":1,"hasNext":false,"items":[{"id":"private-resource-identity","title":"Detected.Film.2025.1080p","sizeBytes":2048,"seeders":7,"tags":[]}]}`
		if strings.HasSuffix(payload["url"].(string), "/health") {
			b.health++
			body = `{"status":"healthy","accountName":"Fixture"}`
			if len(b.healthResponses) > 0 {
				body, b.healthResponses = b.healthResponses[0], b.healthResponses[1:]
			}
		} else {
			b.search++
		}
		code := b.failAlways
		if b.failNext != "" {
			code, b.failNext = b.failNext, ""
		}
		if code != "" {
			return &browsercompanion.Failure{Code: code}
		}
		status := b.status
		if status == 0 {
			status = 200
		}
		response = map[string]any{"status": status, "headers": map[string]string{"content-type": "application/json"}, "bodyBase64": base64.StdEncoding.EncodeToString([]byte(body)), "cookies": b.imported}
	case "session/cookies":
		response = map[string]any{"cookies": b.imported}
	case "session/close":
		b.closes++
	}
	if output == nil {
		return nil
	}
	raw, _ := json.Marshal(response)
	return json.Unmarshal(raw, output)
}

func recoveryServiceFixture(t *testing.T) (*PluginRepositoryService, Actor, models.PluginConnection, models.Site, *recoveryRuntime, *recoveryBrowser) {
	t.Helper()
	service, actor, _, store, connection, site := resourcePluginServiceFixture(t, "org.ohmycine.recovery")
	connection.CredentialCiphertext, connection.LastHealthStatus = "", "unavailable"
	if err := service.db.Model(&connection).Updates(map[string]any{"credential_ciphertext": "", "last_health_status": "unavailable"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&site).Update("last_health_status", "unavailable").Error; err != nil {
		t.Fatal(err)
	}
	fingerprint, err := service.browserStateFingerprint(connection)
	if err != nil {
		t.Fatal(err)
	}
	cookieJSON, _ := json.Marshal([]browserCookie{{Name: "session", Value: "encrypted-restart-cookie", Domain: "mirror.example", Path: "/", Expires: -1, Secure: true, HTTPOnly: true, SameSite: "Lax"}})
	ciphertext, err := store.Encrypt(browserStatePurpose(connection.PluginID, connection.ID), string(cookieJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.PluginBrowserState{ConnectionID: connection.ID, PluginID: connection.PluginID, OwnerID: actor.User.ID, Fingerprint: fingerprint, Ciphertext: ciphertext, UpdatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	var installation models.PluginInstallation
	if err := service.db.First(&installation, "plugin_id = ?", connection.PluginID).Error; err != nil {
		t.Fatal(err)
	}
	for _, permission := range []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"mirror.example"}}, {Kind: contract.PermissionCredentialUse, Scopes: []string{"resource.session"}}} {
		raw, _ := json.Marshal(permission)
		if err := service.db.Create(&models.PluginPermissionGrant{PluginID: connection.PluginID, PluginPackageID: installation.ActivePackageID, PermissionKey: uuid.NewString(), PermissionJSON: string(raw), CreatedAt: time.Now()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Recreate both the service and runtime generation as after a Server restart.
	if err := service.db.Model(&installation).Update("runtime_generation", installation.RuntimeGeneration+1).Error; err != nil {
		t.Fatal(err)
	}
	browser := &recoveryBrowser{}
	restarted := NewPluginRepositoryService(service.db, service.audit, nil, zerolog.Nop(), WithPluginCredentialStore(store), WithPluginBrowser(browser))
	client := &http.Client{Transport: recoveryHTTPRoundTrip(func(request *http.Request) (*http.Response, error) {
		browser.mu.Lock()
		if request.Method == http.MethodPost {
			browser.directPosts++
		}
		browser.mu.Unlock()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
	})}
	host := hostapi.New(service.db, store, zerolog.Nop(), hostapi.WithHTTPClient(client), hostapi.WithResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}))
	host.SetBrowserRequest(restarted.BrowserRequest)
	runtime := &recoveryRuntime{resourcePluginRuntime: &resourcePluginRuntime{}, host: host, remaining: map[string]time.Duration{}}
	restarted.runtime = runtime
	return restarted, actor, connection, site, runtime, browser
}

func TestPluginResourceRecoveryThroughSiteHostAfterRestart(t *testing.T) {
	service, actor, connection, record, runtime, browser := recoveryServiceFixture(t)
	browser.create = func(ctx context.Context) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 60*time.Second || time.Until(deadline) > resourceBrowserRestoreTimeout {
			t.Errorf("cold preparation budget=%v", time.Until(deadline))
		}
	}
	actor.Permissions[authz.PermissionDiscoveryRead] = struct{}{}
	sites := NewSiteService(service.db, service.audit, service.credentials, nil, zerolog.Nop())
	sites.SetPluginResourceBridge(service)
	groups, err := sites.Search(context.Background(), actor, SiteSearchInput{SiteID: &record.ID, Keyword: "Detected Film", Page: 1})
	if err != nil || len(groups) != 1 || groups[0].Status != "success" || len(groups[0].Items) != 1 {
		t.Fatalf("search=%+v err=%v", groups, err)
	}
	if browser.creates != 1 || browser.health != 1 || browser.search != 1 || len(browser.imported) != 1 || browser.imported[0].Value != "encrypted-restart-cookie" {
		t.Fatalf("recovery counters: %+v", browser)
	}
	if strings.Join(runtime.operations, ",") != "resource.health,resource.search" {
		t.Fatalf("unexpected operations: %v", runtime.operations)
	}
	if runtime.remaining["resource.search"] <= 30*time.Second || runtime.remaining["resource.search"] > resourceSearchTimeout {
		t.Fatalf("search budget=%v", runtime.remaining["resource.search"])
	}
	var stored models.PluginConnection
	var storedSite models.Site
	service.db.First(&stored, "id = ?", connection.ID)
	service.db.First(&storedSite, record.ID)
	if stored.LastHealthStatus != "healthy" || storedSite.LastHealthStatus != "healthy" || !service.browserSession.HealthChecked {
		t.Fatal("confirmed recovery did not synchronize both projections")
	}
	raw, _ := json.Marshal(groups)
	for _, secret := range []string{"encrypted-restart-cookie", "private-resource-identity", "mirror.example"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("public search leaked %q", secret)
		}
	}
}

func TestPluginResourceRecoverySerializesAndHonorsWaiterCancellation(t *testing.T) {
	service, _, connection, _, _, browser := recoveryServiceFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	browser.create = func(context.Context) { close(entered); <-release }
	first := make(chan error, 1)
	go func() { first <- service.PrepareResourceSearch(context.Background(), connection.ID) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.PrepareResourceSearch(ctx, connection.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter=%v", err)
	}
	second := make(chan error, 1)
	go func() { second <- service.PrepareResourceSearch(context.Background(), connection.ID) }()
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if browser.creates != 1 || browser.health != 1 {
		t.Fatalf("duplicate recovery: creates=%d health=%d", browser.creates, browser.health)
	}
	if len(service.recovery.entries) != 0 {
		t.Fatal("idle recovery entries leaked")
	}
}

func TestPluginResourceRecoveryChecksActualChallengeAndRateCooldown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replies  []string
		code     string
		requests int
	}{
		{"settled", []string{`{"pluginError":{"code":"browser-verification-required"}}`, `{"status":"healthy"}`}, "", 2},
		{"challenge", []string{`{"pluginError":{"code":"browser-verification-required"}}`, `{"pluginError":{"code":"browser-verification-required"}}`}, "resource_browser_verification_required", 2},
		{"rate", []string{`{"status":"rate_limited"}`}, "resource_rate_limited", 1},
		{"unknown", []string{`{"unexpected":"shape"}`}, "resource_result_invalid", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _, connection, _, _, browser := recoveryServiceFixture(t)
			browser.healthResponses = tc.replies
			err := service.PrepareResourceSearch(context.Background(), connection.ID)
			if (tc.code == "" && err != nil) || (tc.code != "" && ErrorCode(err) != tc.code) {
				t.Fatalf("error=%v code=%s want=%s", err, ErrorCode(err), tc.code)
			}
			if browser.requests != tc.requests {
				t.Fatalf("health requests=%d", browser.requests)
			}
			if tc.name == "rate" {
				_ = service.PrepareResourceSearch(context.Background(), connection.ID)
				_, _ = service.ResourceHealth(context.Background(), connection.PluginID, connection.ID)
				if browser.requests != 1 {
					t.Fatal("retry bypassed real rate cooldown")
				}
			}
			if service.browserSession.Pending != nil {
				t.Fatal("read-only check created pending credentials")
			}
		})
	}
}

func TestPluginResourceRecoverySharesFailedPreflightWithConcurrentWaiter(t *testing.T) {
	service, _, connection, _, _, browser := recoveryServiceFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseBrowser := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseBrowser()
	browser.create = func(context.Context) { close(entered); <-release }
	browser.healthResponses = []string{`{"status":"unavailable"}`}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- service.PrepareResourceSearch(context.Background(), connection.ID) }()
	<-entered
	go func() { second <- service.PrepareResourceSearch(context.Background(), connection.ID) }()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		service.recovery.mu.Lock()
		entry := service.recovery.entries[connection.ID]
		waiting := entry != nil && entry.refs == 2
		service.recovery.mu.Unlock()
		if waiting {
			break
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			releaseBrowser()
			<-first
			<-second
			t.Fatal("second search did not join recovery")
		}
	}
	releaseBrowser()
	for _, err := range []error{<-first, <-second} {
		if ErrorCode(err) != "resource_entry_unavailable" {
			t.Fatalf("shared failure=%v", err)
		}
	}
	if browser.creates != 1 || browser.health != 1 || browser.requests != 1 {
		t.Fatal("failed preflight was repeated for concurrent search")
	}
}

func TestPluginResourceRecoveryRebuildsLostGETOnceButNeverPOST(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			service, _, connection, _, runtime, browser := recoveryServiceFixture(t)
			if err := service.PrepareResourceSearch(context.Background(), connection.ID); err != nil {
				t.Fatal(err)
			}
			runtime.method = method
			browser.failNext = "session_unavailable"
			page, err := service.SearchResource(context.Background(), PluginResourceSearchInput{ConnectionID: connection.ID, Query: "Film", Page: 1})
			if method == http.MethodGet {
				if err != nil || len(page.Items) != 1 || browser.creates != 2 || browser.health != 2 || browser.search != 2 {
					t.Fatalf("GET recovery page=%+v err=%v creates=%d health=%d search=%d", page, err, browser.creates, browser.health, browser.search)
				}
			} else if err == nil || browser.creates != 1 || browser.search != 1 {
				t.Fatalf("POST replayed: err=%v creates=%d search=%d", err, browser.creates, browser.search)
			}
		})
	}
}

func TestPluginResourceRecoveryDoesNotLoopOnHTTPRejectionOrRepeatedLostSession(t *testing.T) {
	for _, mode := range []string{"405", "429", "lost_again"} {
		t.Run(mode, func(t *testing.T) {
			service, _, connection, _, _, browser := recoveryServiceFixture(t)
			if err := service.PrepareResourceSearch(context.Background(), connection.ID); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "405":
				browser.status = 405
			case "429":
				browser.status = 429
			default:
				browser.failAlways = "session_unavailable"
			}
			_, err := service.SearchResource(context.Background(), PluginResourceSearchInput{ConnectionID: connection.ID, Query: "Film", Page: 1})
			if err == nil {
				t.Fatal("rejected read reported success")
			}
			if mode == "lost_again" {
				if browser.creates != 2 || browser.requests != 3 {
					t.Fatalf("rebuild exceeded one shared attempt: creates=%d requests=%d", browser.creates, browser.requests)
				}
			} else if browser.creates != 1 || browser.search != 1 {
				t.Fatalf("HTTP rejection retried: creates=%d search=%d", browser.creates, browser.search)
			}
			if mode == "429" {
				if ErrorCode(err) != "resource_rate_limited" {
					t.Fatalf("lost 429 classification: %v", err)
				}
				_, _ = service.SearchResource(context.Background(), PluginResourceSearchInput{ConnectionID: connection.ID, Query: "Film", Page: 1})
				if browser.search != 1 {
					t.Fatal("search bypassed HTTP429 cooldown")
				}
			}
		})
	}
}

func TestPluginResourceRecoveryDoesNotReplayUncredentialedPOSTBeforeLostGET(t *testing.T) {
	service, _, connection, _, runtime, browser := recoveryServiceFixture(t)
	if err := service.PrepareResourceSearch(context.Background(), connection.ID); err != nil {
		t.Fatal(err)
	}
	runtime.uncredentialedPOST = true
	browser.failNext = "session_unavailable"
	_, err := service.SearchResource(context.Background(), PluginResourceSearchInput{ConnectionID: connection.ID, Query: "Film", Page: 1})
	if err == nil || browser.creates != 1 || browser.directPosts != 1 || browser.search != 1 {
		t.Fatalf("unsafe guest replay: err=%v creates=%d POST=%d search=%d", err, browser.creates, browser.directPosts, browser.search)
	}
}

func TestPluginResourceRecoveryParseDiagnosticsAreFiniteAndSecretFree(t *testing.T) {
	service, _, connection, _, _, browser := recoveryServiceFixture(t)
	var output bytes.Buffer
	service.log = zerolog.New(&output)
	browser.healthResponses = []string{`{"unrecognized":"Cookie=private-value https://private.invalid/?token=hidden"}`}
	_, err := service.ResourceHealth(context.Background(), connection.PluginID, connection.ID)
	if ErrorCode(err) != "resource_result_invalid" {
		t.Fatalf("unexpected parse error: %v", err)
	}
	logs := output.String()
	for _, secret := range []string{"private-value", "private.invalid", "encrypted-restart-cookie", "hidden"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("diagnostic leaked %s", secret)
		}
	}
	for _, field := range []string{`"parse_reason":"response_invalid"`, `"response_class":"json"`, `"upstream_http_status":200`, `"error_code":"resource_result_invalid"`} {
		if !strings.Contains(logs, field) {
			t.Fatalf("missing safe diagnostic %s", field)
		}
	}
}

func TestPluginResourceRecoveryPreservesPendingIdentityAndRejectsRevocation(t *testing.T) {
	service, _, connection, _, _, browser := recoveryServiceFixture(t)
	if err := service.PrepareResourceSearch(context.Background(), connection.ID); err != nil {
		t.Fatal(err)
	}
	session := service.browserSession
	session.Pending = &pendingBrowserLogin{Username: "private-user", Password: []byte("private-password"), Expires: time.Now().Add(time.Minute)}
	before := browser.requests
	if err := service.PrepareResourceSearch(context.Background(), connection.ID); ErrorCode(err) != "resource_browser_login_pending" {
		t.Fatalf("pending search=%v", err)
	}
	if _, err := service.ResourceHealth(context.Background(), connection.PluginID, connection.ID); ErrorCode(err) != "resource_browser_login_pending" {
		t.Fatalf("ordinary health borrowed pending identity: %v", err)
	}
	if service.browserSession != session || session.Pending == nil || browser.requests != before {
		t.Fatal("pending state was consumed or replaced")
	}
	clearPending(session)
	if err := service.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.PrepareResourceSearch(context.Background(), connection.ID); err == nil {
		t.Fatal("disabled connection recovered")
	}
}

func TestPluginResourceRecoveryCancelledLateCreateCannotPublish(t *testing.T) {
	service, _, connection, _, _, browser := recoveryServiceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	browser.create = func(context.Context) { cancel() }
	if err := service.PrepareResourceSearch(ctx, connection.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("late create=%v", err)
	}
	if service.browserSession != nil || browser.creates != 1 || browser.closes != 1 || browser.requests != 0 {
		t.Fatal("cancelled late browser remained or queried provider")
	}
}

func TestPluginResourceRecoveryRejectsConfigChangeDuringColdCreation(t *testing.T) {
	service, _, connection, _, _, browser := recoveryServiceFixture(t)
	browser.create = func(context.Context) {
		if err := service.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("credential_version", connection.CredentialVersion+1).Error; err != nil {
			t.Error(err)
		}
	}
	if err := service.PrepareResourceSearch(context.Background(), connection.ID); ErrorCode(err) != "resource_browser_session_expired" {
		t.Fatalf("changed credentials accepted: %v", err)
	}
	if service.browserSession != nil || browser.closes != 1 || browser.requests != 0 {
		t.Fatal("stale-created context was published or queried")
	}
}

func TestPluginResourceRecoveryMissingBrowserIsNotExpiredLogin(t *testing.T) {
	service, _, connection, _, _, _ := recoveryServiceFixture(t)
	service.browser = nil
	if !service.hasBrowserState(connection.ID) {
		t.Fatal("component absence erased durable credential presence")
	}
	if err := service.PrepareResourceSearch(context.Background(), connection.ID); ErrorCode(err) != "resource_browser_unavailable" {
		t.Fatalf("missing component misclassified: %v", err)
	}
	if _, err := service.ResourceHealth(context.Background(), connection.PluginID, connection.ID); ErrorCode(err) != "resource_browser_unavailable" {
		t.Fatalf("health misclassified component: %v", err)
	}
	var stored models.PluginConnection
	service.db.First(&stored, "id = ?", connection.ID)
	if stored.LastHealthStatus == "auth_required" {
		t.Fatal("component failure changed login state")
	}
}
