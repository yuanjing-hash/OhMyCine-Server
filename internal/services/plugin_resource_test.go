package services

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/hostapi"
)

type resourcePluginRuntime struct {
	operations []string
	responses  map[string][]byte
}

func (*resourcePluginRuntime) Validate(context.Context, string) error              { return nil }
func (*resourcePluginRuntime) Start(context.Context, string, string, uint64) error { return nil }
func (runtime *resourcePluginRuntime) Invoke(_ context.Context, _ string, operation string, _ []byte) ([]byte, error) {
	runtime.operations = append(runtime.operations, operation)
	response, ok := runtime.responses[operation]
	if !ok {
		return nil, errors.New("unexpected plugin operation")
	}
	return append([]byte(nil), response...), nil
}
func (*resourcePluginRuntime) Stop(string) error           { return nil }
func (*resourcePluginRuntime) Close(context.Context) error { return nil }

func resourcePluginServiceFixture(t *testing.T, pluginID string) (*PluginRepositoryService, Actor, *resourcePluginRuntime, *credential.Store, models.PluginConnection, models.Site) {
	t.Helper()
	service, actor, _ := pluginRepositoryFixture(t)
	store, err := credential.Open(filepath.Join(t.TempDir(), "credential.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &resourcePluginRuntime{responses: map[string][]byte{}}
	service.credentials = store
	service.runtime = runtime

	manifest := contract.Manifest{
		SchemaVersion: 1,
		ID:            pluginID, Name: "Resource fixture", Description: "Resource fixture plugin",
		Version: "0.1.0", APIVersion: contract.APIVersion, MinServerVersion: "0.1.0",
		Runtime: "wasm", Entry: "plugin.wasm",
		Capabilities: []contract.Capability{
			contract.CapabilityResourceSearch, contract.CapabilityResourceResolve,
			contract.CapabilityResourceHealth, contract.CapabilityResourceLogin,
			contract.CapabilityResourceCaptcha, contract.CapabilityResourceCookie,
		},
		Permissions: []contract.Permission{
			{Kind: contract.PermissionNetworkHTTP, Domains: []string{"mirror.example"}},
			{Kind: contract.PermissionCredentialUse, Scopes: []string{"resource.session"}},
		},
		ConfigSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["entryOrigin"],"properties":{"entryOrigin":{"type":"string","enum":["https://mirror.example/"]}}}`),
		Author:       "OhMyCine", License: "MIT", Source: "https://github.com/ohmycine/plugins",
		PackageSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	packageRecord := models.PluginPackage{PluginID: pluginID, Version: manifest.Version, RepositoryOwner: "ohmycine", RepositoryRepo: "plugins", RegistryCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RegistryEntryJSON: `{}`, ManifestURL: "https://github.com/ohmycine/plugins/releases/download/v0.1.0/plugin.json", PackageURL: "https://github.com/ohmycine/plugins/releases/download/v0.1.0/plugin.omcp", PackageSHA256: manifest.PackageSHA256, ExtractedTreeSHA256: manifest.PackageSHA256, ManifestJSON: string(manifestJSON), PackagePath: t.TempDir(), VerifiedAt: now, CreatedAt: now}
	if err := service.db.Create(&packageRecord).Error; err != nil {
		t.Fatal(err)
	}
	installation := models.PluginInstallation{PluginID: pluginID, ActivePackageID: packageRecord.ID, Status: models.PluginInstallationEnabled, Revision: 1, RuntimeGeneration: 1, InstalledAt: now, UpdatedAt: now}
	if err := service.db.Create(&installation).Error; err != nil {
		t.Fatal(err)
	}
	connectionID := uuid.NewString()
	oldCiphertext, err := store.Encrypt(hostapi.CredentialPurpose(pluginID, connectionID, "resource.session"), "old=valid")
	if err != nil {
		t.Fatal(err)
	}
	connection := models.PluginConnection{ID: connectionID, PluginID: pluginID, Name: "Resource", ConfigJSON: `{"entryOrigin":"https://mirror.example/"}`, CredentialScope: "resource.session", CredentialMode: models.PluginCredentialModeCookie, CredentialCiphertext: oldCiphertext, ResourceType: "bt_resource", EntryOrigin: "https://mirror.example/", CredentialVersion: 1, Enabled: true, LastHealthStatus: "healthy", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	site := models.Site{Name: "Resource", NameNormalized: "plugin-" + connectionID, Kind: pluginResourceKind, SourceType: "plugin", PluginID: pluginID, PluginConnectionID: connectionID, BaseURL: connection.EntryOrigin, Enabled: true, Priority: 100, TimeoutSeconds: 15, RateLimitPerMinute: 30, LastHealthStatus: "healthy", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&site).Error; err != nil {
		t.Fatal(err)
	}
	return service, actor, runtime, store, connection, site
}

func TestPluginResourceAuthenticatedLoginRequiresHealthyProbe(t *testing.T) {
	service, actor, runtime, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.resource-login")
	runtime.responses["resource.auth.login"] = []byte(`{"state":"authenticated","accountName":"claimed"}`)
	runtime.responses["resource.health"] = []byte(`{"status":"auth_required"}`)

	_, err := service.LoginResource(context.Background(), actor, connection.PluginID, contract.ResourceLoginRequest{ConnectionID: connection.ID, Username: "tester", Password: "secret-password"})
	if ErrorCode(err) != "resource_auth_failed" {
		t.Fatalf("error=%v code=%s", err, ErrorCode(err))
	}
	if got := runtime.operations; len(got) != 2 || got[0] != "resource.auth.login" || got[1] != "resource.health" {
		t.Fatalf("operations=%v", got)
	}
	var stored models.PluginConnection
	if err := service.db.First(&stored, "id = ?", connection.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.LastHealthStatus != "auth_required" {
		t.Fatalf("health=%s", stored.LastHealthStatus)
	}
}

func TestPluginResourceInvalidPastedCookieRestoresPreviousCredential(t *testing.T) {
	service, actor, runtime, store, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.resource-cookie")
	runtime.responses["resource.auth.cookie"] = []byte(`{"state":"authenticated"}`)
	runtime.responses["resource.health"] = []byte(`{"status":"auth_required"}`)

	_, err := service.SubmitResourceCookie(context.Background(), actor, connection.PluginID, contract.ResourceCookieRequest{ConnectionID: connection.ID, Cookie: "candidate=expired"})
	if ErrorCode(err) != "resource_auth_failed" {
		t.Fatalf("error=%v code=%s", err, ErrorCode(err))
	}
	var stored models.PluginConnection
	if err := service.db.First(&stored, "id = ?", connection.ID).Error; err != nil {
		t.Fatal(err)
	}
	plaintext, err := store.Decrypt(hostapi.CredentialPurpose(connection.PluginID, connection.ID, connection.CredentialScope), stored.CredentialCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext != "old=valid" || stored.LastHealthStatus != "healthy" || stored.CredentialVersion != connection.CredentialVersion+2 {
		t.Fatalf("credential=%q health=%s version=%d", plaintext, stored.LastHealthStatus, stored.CredentialVersion)
	}
	if got := runtime.operations; len(got) != 2 || got[0] != "resource.auth.cookie" || got[1] != "resource.health" {
		t.Fatalf("operations=%v", got)
	}
}

func TestPluginResourceRouteRejectsMismatchedPlugin(t *testing.T) {
	service, _, runtime, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.resource-route")
	_, err := service.ResourceHealth(context.Background(), "org.ohmycine.other", connection.ID)
	if ErrorCode(err) != CodeNotFound {
		t.Fatalf("error=%v code=%s", err, ErrorCode(err))
	}
	if len(runtime.operations) != 0 {
		t.Fatalf("unexpected operations=%v", runtime.operations)
	}
}

func TestPluginResourceConnectionsRequireDeclaredCookieScope(t *testing.T) {
	service, actor, _, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.resource-mode")
	config := json.RawMessage(`{"entryOrigin":"https://mirror.example/"}`)
	for _, input := range []CreatePluginConnectionInput{
		{Name: "anonymous", Config: config, CredentialMode: models.PluginCredentialModeNone, Enabled: true},
		{Name: "bearer", Config: config, CredentialScope: connection.CredentialScope, CredentialMode: models.PluginCredentialModeBearer, Credential: "token", Enabled: true},
	} {
		if _, err := service.CreateConnection(actor, connection.PluginID, input, RequestContext{}); ErrorCode(err) != CodeInvalidRequest {
			t.Fatalf("input=%+v error=%v code=%s", input, err, ErrorCode(err))
		}
	}
	created, err := service.CreateConnection(actor, connection.PluginID, CreatePluginConnectionInput{Name: "cookie", Config: config, CredentialScope: connection.CredentialScope, CredentialMode: models.PluginCredentialModeCookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if created.CredentialMode != models.PluginCredentialModeCookie || created.CredentialScope != connection.CredentialScope || created.HealthStatus != "auth_required" {
		t.Fatalf("created=%+v", created)
	}
}

func TestPluginResourceProvenanceRejectsDisabledConnection(t *testing.T) {
	service, _, _, _, connection, _ := resourcePluginServiceFixture(t, "org.ohmycine.resource-provenance")
	if err := service.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ResourceProvenance(connection.ID); ErrorCode(err) != CodeNotFound {
		t.Fatalf("error=%v code=%s", err, ErrorCode(err))
	}
}
