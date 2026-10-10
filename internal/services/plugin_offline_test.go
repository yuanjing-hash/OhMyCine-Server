package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
)

func TestOfflinePlanExactSelectionAuthorizationAndLegacyCompatibility(t *testing.T) {
	service, actor, _ := pluginRepositoryFixture(t)
	actor.Permissions[authz.PermissionMediaLibrariesRead] = struct{}{}
	actor.Permissions[authz.PermissionDownloadsCreate] = struct{}{}
	connectionID := uuid.NewString()
	ref := uuid.NewString()
	now := time.Now().UTC()
	manifest := fmt.Sprintf(`{"schemaVersion":1,"id":"org.example.offline","name":"Fixture","description":"Fixture","version":"0.1.0","apiVersion":"1","minServerVersion":"0.1.1","runtime":"wasm","entry":"plugin.wasm","capabilities":["site.feed","site.detail","media.playback","media.offline_download_plan","site.auth"],"permissions":[{"kind":"download.plan"},{"kind":"network.http","domains":["cdn.example.test"]},{"kind":"credential.use","scopes":["site.session"]}],"configSchema":{"type":"object"},"author":"test","license":"MIT","source":"https://github.com/example/plugins","packageSha256":"%s"}`, strings.Repeat("a", 64))
	pkg := models.PluginPackage{PluginID: "org.example.offline", Version: "0.1.0", ManifestJSON: manifest, PackageSHA256: strings.Repeat("a", 64), CreatedAt: now, VerifiedAt: now}
	if err := service.db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.PluginInstallation{PluginID: pkg.PluginID, ActivePackageID: pkg.ID, Status: models.PluginInstallationEnabled, RuntimeGeneration: 1, Revision: 1, InstalledAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.PluginPermissionGrant{PluginID: pkg.PluginID, PluginPackageID: pkg.ID, PermissionKey: "download.plan", PermissionJSON: `{"kind":"download.plan"}`, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	connection := models.PluginConnection{ID: connectionID, PluginID: pkg.PluginID, Name: "Fixture", ConfigJSON: `{}`, Enabled: true, CredentialMode: models.PluginCredentialModeCookie, CredentialScope: "site.session", CredentialVersion: 1, CredentialCiphertext: "fixture-encrypted", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.Create(&models.PluginOnlineLibrary{ID: connectionID, PluginID: pkg.PluginID, ConnectionID: connectionID, ExternalKey: "default", Name: "Fixture", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	service.offline = &offlineFixtureGateway{urls: map[string]string{ref: "https://cdn.example.test/video.mp4?temporary=opaque"}, bodies: map[string][]byte{}}
	calls := 0
	plan := contract.OfflineDownloadPlan{Version: 1, WorkID: "work", SegmentID: "episode", VersionID: "original", VariantID: "1080p", SuggestedFileName: "Fixture.mp4", Format: "progressive", Assets: []contract.DownloadAsset{{ID: "video", Kind: "video", URLRef: ref, ExpectedContentType: "video/mp4"}}}
	service.runtime = &onlinePluginRuntime{handler: func(operation string, request []byte) ([]byte, error) {
		calls++
		if operation == "site.auth.start" {
			return []byte(fmt.Sprintf(`{"loginSession":"fixture","qrCodeUrl":"https://cdn.example.test/qr","expiresAt":%q,"pollAfterSeconds":2}`, now.Add(time.Minute).Format(time.RFC3339))), nil
		}
		if operation == "site.auth.poll" {
			return []byte(`{"state":"confirmed","authenticated":true,"credentialVersion":1,"account":{"id":"account","name":"Fixture","membership":{"status":"active","label":"VIP"}}}`), nil
		}
		var input map[string]string
		if err := json.Unmarshal(request, &input); err != nil {
			t.Fatal(err)
		}
		if input["connectionId"] != connectionID || input["variantId"] != "1080p" {
			t.Fatal("selection not forwarded exactly")
		}
		if operation == string(contract.CapabilityMediaDownload) {
			return json.Marshal(contract.DownloadPlan{WorkID: plan.WorkID, SegmentID: plan.SegmentID, VersionID: plan.VersionID, VariantID: plan.VariantID, SuggestedFileName: plan.SuggestedFileName, Assets: plan.Assets})
		}
		return json.Marshal(plan)
	}}
	before := calls
	if _, err := service.OnlineOfflinePlan(context.Background(), Actor{}, connectionID, "work", "episode", "original", "1080p"); ErrorCode(err) != CodePermissionDenied || calls != before {
		t.Fatal("unauthorized plugin invocation")
	}
	if _, err := service.OnlineOfflinePlan(context.Background(), actor, connectionID, "work", "episode", "original", ""); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("implicit quality accepted")
	}
	result, err := service.OnlineOfflinePlan(context.Background(), actor, connectionID, "work", "episode", "original", "1080p")
	if err != nil || result.LibraryID != connectionID || len(result.Tracks) != 1 || result.Tracks[0].Units[0].URL == "" || result.ExpiresAt <= now.Unix() {
		t.Fatalf("plan=%+v err=%v", result, err)
	}
	libraries, err := service.OnlineLibraries(actor)
	if err != nil || len(libraries) != 1 || !libraries[0].OfflineDownloadSupported {
		t.Fatal("capability not advertised")
	}
	plan.VariantID = "720p"
	if _, err := service.OnlineOfflinePlan(context.Background(), actor, connectionID, "work", "episode", "original", "1080p"); ErrorCode(err) != CodePluginResponseInvalid {
		t.Fatal("provider changed quality silently")
	}
	plan.VariantID = "1080p"
	// Read-only auth must not require the account-write capability.
	started, err := service.StartConnectionAuth(context.Background(), actor, pkg.PluginID, connectionID)
	if err != nil {
		t.Fatal(err)
	}
	polled, err := service.PollConnectionAuth(context.Background(), actor, pkg.PluginID, connectionID, started.LoginSession)
	if err != nil || polled.Account == nil || polled.Account.Membership == nil || polled.Account.Membership.Status != "active" {
		t.Fatal("membership summary lost")
	}
	var stored models.PluginConnection
	if err := service.db.First(&stored, "id = ?", connectionID).Error; err != nil {
		t.Fatal(err)
	}
	if summary := pluginConnectionSummary(stored); summary.Account == nil || summary.Account.Membership == nil || summary.Account.Membership.Status != "active" || summary.AccountCheckedAt == nil {
		t.Fatal("membership lost on reload")
	}
	missingCredential := stored
	missingCredential.CredentialCiphertext = ""
	if summary := pluginConnectionSummary(missingCredential); summary.Account.Membership.Status != "unknown" {
		t.Fatal("cleared credential kept active membership")
	}
	stale := now.Add(-25 * time.Hour)
	stored.AccountCheckedAt = &stale
	if summary := pluginConnectionSummary(stored); summary.Account.Membership.Status != "unknown" {
		t.Fatal("stale membership presented as current")
	}
	if err := service.db.Model(&stored).Update("credential_version", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.PollConnectionAuth(context.Background(), actor, pkg.PluginID, connectionID, started.LoginSession); ErrorCode(err) != CodeConflict {
		t.Fatal("stale QR overwrote newer account summary")
	}
	if err := service.db.Model(&stored).Update("credential_version", 1).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.InvokeSiteAction(context.Background(), actor, connectionID, "work", "favorite.add", PluginSiteActionInput{IdempotencyKey: "test"}); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("read-only auth enabled writes")
	}
	legacyManifest := strings.Replace(manifest, "media.offline_download_plan", "media.download_plan", 1)
	if err := service.db.Model(&pkg).Update("manifest_json", legacyManifest).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.OnlineOfflinePlan(context.Background(), actor, connectionID, "work", "episode", "original", "1080p"); err != nil {
		t.Fatalf("legacy explicit MP4 rejected: %v", err)
	}
	plan.Assets[0].ExpectedContentType = "application/vnd.apple.mpegurl"
	if _, err := service.OnlineOfflinePlan(context.Background(), actor, connectionID, "work", "episode", "original", "1080p"); ErrorCode(err) != CodePluginResponseInvalid {
		t.Fatal("legacy implicit HLS accepted")
	}
	if err := service.db.Where("plugin_id = ?", pkg.PluginID).Delete(&models.PluginPermissionGrant{}).Error; err != nil {
		t.Fatal(err)
	}
	before = calls
	if _, err := service.OnlineOfflinePlan(context.Background(), actor, connectionID, "work", "episode", "original", "1080p"); ErrorCode(err) != CodePermissionDenied || calls != before {
		t.Fatal("revoked download grant invoked plugin")
	}
	if libraries, err := service.OnlineLibraries(actor); err != nil || libraries[0].OfflineDownloadSupported {
		t.Fatal("revoked download grant advertised")
	}
	if err := service.db.Model(&connection).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	before = calls
	if _, err := service.OnlineOfflinePlan(context.Background(), actor, connectionID, "work", "episode", "original", "1080p"); err == nil || calls != before {
		t.Fatal("disabled connection invoked")
	}
}

func TestMembershipSummaryIsBoundedAndDisplayOnly(t *testing.T) {
	for _, summary := range []*PluginMembershipSummary{{Status: "vip"}, {Status: "active", Label: strings.Repeat("a", 129)}, {Status: "active", ExpiresAt: "secret"}, {Status: "unknown", ExpiresAt: "3000-01-01T00:00:00Z"}} {
		if validMembershipSummary(summary) {
			t.Fatalf("invalid membership accepted: %+v", summary)
		}
	}
	if !validMembershipSummary(&PluginMembershipSummary{Status: "unknown"}) || !validMembershipSummary(nil) {
		t.Fatal("unknown optional membership rejected")
	}
}
