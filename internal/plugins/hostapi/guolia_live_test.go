package hostapi

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	pluginruntime "github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/runtime"
	"golang.org/x/net/html"
)

// TestGuoliaLiveHost is opt-in: OMC_GUOLIA_LIVE_WASM supplies the built WASM
// path, and stdin supplies a single Cookie line (blank means anonymous).
// Run the compiled test executable, not go test, to retain stdin. Never pass
// the Cookie as a command argument or environment variable. All DB/keys are
// memory-only. No production services, configuration or credentials are read.
func TestGuoliaLiveHost(t *testing.T) {
	wasmPath := os.Getenv("OMC_GUOLIA_LIVE_WASM")
	if wasmPath == "" {
		t.Skip("explicit WASM path and stdin required for live diagnosis")
	}
	reader := bufio.NewReader(io.LimitReader(os.Stdin, 16386))
	cookie, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		t.Fatal("stdin read failed")
	}
	cookie = strings.TrimSpace(cookie)
	if len(cookie) > 16384 || strings.ContainsAny(cookie, "\r\n") {
		t.Fatal("invalid Cookie input")
	}
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal("isolated DB open failed")
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&models.PluginPackage{}, &models.PluginInstallation{}, &models.PluginPermissionGrant{}, &models.PluginConnection{}); err != nil {
		t.Fatal("isolated schema setup failed")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal("key generation failed")
	}
	store, err := credential.Open("", base64.RawStdEncoding.EncodeToString(key))
	clear(key)
	if err != nil {
		t.Fatal("memory credential store failed")
	}
	const pluginID = "org.ohmycine.guolia"
	const connectionID = "11111111-1111-4111-8111-111111111111"
	const origin = "https://www.xn--kivn76b41nnhi.com"
	const scope = "guolia.session"
	now := time.Now().UTC()
	pkg := models.PluginPackage{PluginID: pluginID, Version: "0.1.2", ManifestJSON: `{}`, RegistryEntryJSON: `{}`, CreatedAt: now}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatal("package setup failed")
	}
	installation := models.PluginInstallation{PluginID: pluginID, ActivePackageID: pkg.ID, Status: models.PluginInstallationEnabled, Revision: 1, RuntimeGeneration: 1, InstalledAt: now, UpdatedAt: now, EnabledAt: &now}
	if err := db.Create(&installation).Error; err != nil {
		t.Fatal("installation setup failed")
	}
	for _, p := range []contract.Permission{{Kind: contract.PermissionNetworkHTTP, Domains: []string{"www.xn--kivn76b41nnhi.com"}}, {Kind: contract.PermissionCredentialUse, Scopes: []string{scope}}} {
		encoded, _ := json.Marshal(p)
		if err := db.Create(&models.PluginPermissionGrant{PluginID: pluginID, PluginPackageID: pkg.ID, PermissionKey: string(p.Kind), PermissionJSON: string(encoded), CreatedAt: now}).Error; err != nil {
			t.Fatal("permission setup failed")
		}
	}
	ciphertext := ""
	if cookie != "" {
		ciphertext, err = store.Encrypt(CredentialPurpose(pluginID, connectionID, scope), cookie)
		if err != nil {
			t.Fatal("credential encryption failed")
		}
	}
	cookie = ""
	connection := models.PluginConnection{ID: connectionID, PluginID: pluginID, Name: "isolated live diagnosis", ResourceType: "bt_resource", EntryOrigin: origin, ConfigJSON: `{"entryOrigin":"` + origin + `/"}`, CredentialScope: scope, CredentialMode: models.PluginCredentialModeCookie, CredentialCiphertext: ciphertext, Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal("connection setup failed")
	}
	host := New(db, store, zerolog.Nop())
	// Wrap the production transport, preserving public-IP validation, TLS,
	// proxy prohibition, redirect validation and credential injection.
	base := host.client.Transport
	host.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		path := "other"
		if req.URL.Path == "/res/search" || req.URL.Path == "/search" {
			path = req.URL.Path
		}
		response, transportErr := base.RoundTrip(req)
		status := 0
		if response != nil {
			status = response.StatusCode
			response.Body = &liveDiagnosticBody{ReadCloser: response.Body, contentType: response.Header.Get("Content-Type"), report: func(kind, code string, title bool, truncated bool) {
				t.Logf("upstream type=%s json_code=%s challenge_title=%t truncated=%t", kind, code, title, truncated)
			}}
		}
		t.Logf("http path=%s cookie_present=%t cookie_count=%d status=%d transport_failed=%t", path, req.Header.Get("Cookie") != "", len(req.Cookies()), status, transportErr != nil)
		return response, transportErr
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	runtime := pluginruntime.NewHost(ctx)
	defer runtime.Close(context.Background())
	runtime.SetCapabilityHost(host)
	if err := runtime.Start(ctx, pluginID, wasmPath, 1); err != nil {
		t.Fatalf("WASM start code=%s", pluginruntime.ErrorCode(err))
	}
	request, _ := json.Marshal(map[string]string{"connectionId": connectionID})
	result, err := runtime.Invoke(ctx, pluginID, "resource.health", request)
	if err != nil {
		t.Fatalf("WASM invoke code=%s", pluginruntime.ErrorCode(err))
	}
	// Never print arbitrary plugin strings, account names or upstream bodies.
	var envelope struct {
		Status string `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"pluginError"`
	}
	if json.Unmarshal(result, &envelope) != nil {
		t.Fatal("invalid result envelope")
	}
	status := safeLiveDiagnosticCode(envelope.Status)
	code := safeLiveDiagnosticCode(envelope.Error.Code)
	t.Logf("plugin status=%s error=%s", status, code)
}

// Observe bytes as the production Host consumes them: preserve the original
// stream, read errors, response limit and Close semantics without a second read.
type liveDiagnosticBody struct {
	io.ReadCloser
	contentType string
	buffer      []byte
	truncated   bool
	report      func(string, string, bool, bool)
}

func (b *liveDiagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	remaining := maxHTTPResponseBytes - len(b.buffer)
	keep := min(n, remaining)
	b.buffer = append(b.buffer, p[:keep]...)
	b.truncated = b.truncated || keep != n
	return n, err
}

func (b *liveDiagnosticBody) Close() error {
	kind, code, title := classifyLiveDiagnosticBody(b.contentType, b.buffer)
	b.report(kind, code, title, b.truncated)
	clear(b.buffer)
	b.buffer = nil
	return b.ReadCloser.Close()
}

func classifyLiveDiagnosticBody(contentType string, body []byte) (string, string, bool) {
	kind := "other"
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "application/json" || mediaType == "text/html" {
		kind = mediaType
	}
	code := "none"
	var object struct {
		Code json.RawMessage `json:"code"`
	}
	if json.Unmarshal(body, &object) == nil && object.Code != nil {
		value := strings.Trim(string(object.Code), "\"")
		switch value {
		case "0", "401", "403", "419", "429":
			code = value
		default:
			code = "other"
		}
	}
	challengeTitle := false
	tokens := html.NewTokenizer(bytes.NewReader(body))
	inTitle := false
	for {
		tokenType := tokens.Next()
		if tokenType == html.ErrorToken {
			break
		}
		token := tokens.Token()
		if tokenType == html.StartTagToken && token.Data == "title" {
			inTitle = true
		}
		if tokenType == html.EndTagToken && token.Data == "title" {
			inTitle = false
		}
		if tokenType == html.TextToken && inTitle {
			text := strings.ToLower(token.Data)
			challengeTitle = challengeTitle || strings.Contains(text, "浏览器安全验证") || strings.Contains(text, "browser verification") || strings.Contains(text, "安全验证")
		}
	}
	return kind, code, challengeTitle
}

func safeLiveDiagnosticCode(value string) string {
	switch value {
	case "", "healthy", "auth_required", "rate_limited", "unavailable", "browser-verification-required", "rate-limited", "not-authenticated", "invalid-response", "network-error", "permission-denied":
		return value
	default:
		return "unclassified"
	}
}

func TestLiveDiagnosticCodeRedaction(t *testing.T) {
	for _, value := range []string{"session=synthetic-secret", "https://example.test/?token=secret", "<html>account</html>"} {
		if safeLiveDiagnosticCode(value) != "unclassified" {
			t.Fatal("diagnostic must suppress arbitrary upstream values")
		}
	}
	if safeLiveDiagnosticCode("browser-verification-required") != "browser-verification-required" {
		t.Fatal("known classification must be retained")
	}
}

func TestLiveDiagnosticBodyClassification(t *testing.T) {
	for _, tc := range []struct {
		body, code string
		title      bool
	}{
		{`{"code":419,"message":"secret"}`, "419", false},
		{`{"code":"private-secret"}`, "other", false},
		{`<html><title>浏览器安全验证</title></html>`, "none", true},
		{`<html><title>Search</title><script>"浏览器安全验证"</script></html>`, "none", false},
	} {
		_, code, title := classifyLiveDiagnosticBody("application/json", []byte(tc.body))
		if code != tc.code || title != tc.title {
			t.Fatal("incorrect safe response classification")
		}
	}
	input := strings.Repeat("x", maxHTTPResponseBytes+5)
	reported := false
	body := &liveDiagnosticBody{ReadCloser: io.NopCloser(strings.NewReader(input)), report: func(_, _ string, _ bool, truncated bool) { reported = truncated }}
	output, err := io.ReadAll(body)
	if err != nil || string(output) != input {
		t.Fatal("diagnostic changed body stream")
	}
	if err := body.Close(); err != nil || !reported || body.buffer != nil {
		t.Fatal("diagnostic limit/cleanup failed")
	}
}
