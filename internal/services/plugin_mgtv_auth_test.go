package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
	pluginruntime "github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/runtime"
)

type authControlTransport func(*http.Request) (*http.Response, error)

func (transport authControlTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type authTraceHost struct {
	api *hostapi.Host
	t   *testing.T
}

func (host authTraceHost) Call(ctx context.Context, pluginID string, operation uint32, payload []byte) ([]byte, error) {
	response, err := host.api.Call(ctx, pluginID, operation, payload)
	// Log stable Host error codes only: no provider response, URL, ticket or header.
	if err != nil {
		host.t.Logf("Host operation=%d error_code=%s", operation, hostapi.ErrorCode(err))
	}
	return response, err
}

// Opt-in actual guest → Host → auth service contract. Default HTTP is entirely
// controlled; explicit OMC_MGTV_QR_LIVE=1 permits one anonymous QR generation
// and pending poll without scanning, authenticating or committing credentials.
func TestMangoWASMAuthServiceValidatesObservedQRStartAndPending(t *testing.T) {
	entry, template, fixtures := os.Getenv("OMC_MGTV_WASM"), os.Getenv("OMC_MGTV_MANIFEST"), os.Getenv("OMC_MGTV_FIXTURES")
	if entry == "" || template == "" || fixtures == "" {
		t.Skip("set actual WASM, template manifest and redacted fixture paths")
	}
	service, actor, _ := pluginRepositoryFixture(t)
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
	connection := models.PluginConnection{ID: uuid.NewString(), PluginID: manifest.ID, Name: "QR fixture", ConfigJSON: `{}`, CredentialMode: models.PluginCredentialModeCookie, CredentialScope: "mgtv.session", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	service.credentials, err = credential.Open(filepath.Join(t.TempDir(), "fixture.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	providerFailure := false
	fakeDNS := false
	client := &http.Client{Transport: authControlTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.Hostname() != "nuc.api.mgtv.com" || request.Header.Get("Cookie") != "" {
			t.Fatal("QR regression issued a write, foreign-host or authenticated request")
		}
		var body []byte
		switch request.URL.Path {
		case "/GetMultiPic":
			if providerFailure {
				body = []byte(`{"code":503,"msg":"fixture-secret https://provider.invalid/private","seqId":""}`)
			} else {
				body, err = os.ReadFile(filepath.Join(fixtures, "auth-generate.json"))
			}
		case "/GetQrcodeResult":
			body = []byte(`{"code":203,"msg":"未扫码登录","seqId":""}`)
		default:
			t.Fatal("QR regression attempted an account or credential endpoint")
		}
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
	})}
	options := []hostapi.Option{hostapi.WithHTTPClient(client), hostapi.WithResolver(func(context.Context, string) ([]net.IPAddr, error) {
		if fakeDNS {
			return []net.IPAddr{{IP: net.ParseIP("198.18.0.149")}, {IP: net.ParseIP("fdfe:dcba:9876::8d")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.10")}}, nil
	})}
	if os.Getenv("OMC_MGTV_QR_LIVE") == "1" {
		options = nil // actual current Host public DNS/dial/redirect policy
	}
	api := hostapi.New(service.db, service.credentials, zerolog.Nop(), options...)
	runtime := pluginruntime.NewHost(context.Background())
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	runtime.SetCapabilityHost(authTraceHost{api: api, t: t})
	if err := runtime.Start(context.Background(), manifest.ID, entry, 1); err != nil {
		t.Fatal(err)
	}
	service.runtime = runtime
	start, err := service.StartConnectionAuth(context.Background(), actor, manifest.ID, connection.ID)
	if err != nil {
		t.Fatalf("actual WASM auth start failed service contract: code=%s", ErrorCode(err))
	}
	qr, err := url.Parse(start.QRCodeURL)
	if err != nil || qr.Scheme != "https" || qr.Hostname() != "oauth.mgtv.com" || qr.Path != "/2.0/multicode_login" {
		t.Fatal("observed official QR origin/path was not retained")
	}
	if !strings.HasPrefix(start.LoginSession, "qr:") || start.PollAfterSeconds != 3 {
		t.Fatal("normalized QR session or poll interval invalid")
	}
	expires, err := time.Parse(time.RFC3339Nano, start.ExpiresAt)
	if err != nil || !expires.After(now) || expires.After(now.Add(4*time.Minute)) {
		t.Fatal("guest clock expiration was not accepted in the service contract")
	}
	pending, err := service.PollConnectionAuth(context.Background(), actor, manifest.ID, connection.ID, start.LoginSession)
	if err != nil || pending.State != "pending" || pending.Authenticated || pending.Account != nil || pending.CredentialVersion != nil {
		t.Fatalf("unscanned QR did not stay pending: code=%s", ErrorCode(err))
	}
	var stored models.PluginConnection
	if err := service.db.First(&stored, "id = ?", connection.ID).Error; err != nil || stored.CredentialCiphertext != "" || stored.CredentialVersion != 0 || stored.AccountSummaryJSON != "" {
		t.Fatal("anonymous QR test unexpectedly committed account credentials")
	}
	if os.Getenv("OMC_MGTV_QR_LIVE") != "1" && requests != 2 {
		t.Fatal("expected exactly generation and pending control reads")
	}
	if os.Getenv("OMC_MGTV_QR_LIVE") != "1" {
		providerFailure = true
		_, err := service.StartConnectionAuth(context.Background(), actor, manifest.ID, connection.ID)
		if ErrorCode(err) != CodePluginOnlineLibraryUnavailable {
			t.Fatalf("actual guest business error was misclassified as malformed auth DTO: code=%s", ErrorCode(err))
		}
		if strings.Contains(fmt.Sprint(err), "fixture-secret") || strings.Contains(fmt.Sprint(err), "provider.invalid") {
			t.Fatal("untrusted upstream diagnostics escaped the plugin boundary")
		}
		providerFailure = false
		fakeDNS = true
		before := requests
		_, err = service.StartConnectionAuth(context.Background(), actor, manifest.ID, connection.ID)
		if ErrorCode(err) != CodePermissionDenied || !strings.Contains(fmt.Sprint(err), "DNS") || !strings.Contains(fmt.Sprint(err), "fake-IP") {
			t.Fatalf("fake DNS did not preserve safe actionable denial: code=%s", ErrorCode(err))
		}
		if requests != before {
			t.Fatal("private fake-IP DNS response reached HTTP transport")
		}
		if strings.Contains(fmt.Sprint(err), "198.18.0.149") || strings.Contains(fmt.Sprint(err), "fdfe:") {
			t.Fatal("raw DNS addresses escaped through plugin error text")
		}
		if err := service.db.First(&stored, "id = ?", connection.ID).Error; err != nil || stored.CredentialCiphertext != "" || stored.CredentialVersion != 0 || stored.AccountSummaryJSON != "" {
			t.Fatal("denied QR generation wrote credentials or account summary")
		}
	}
}
