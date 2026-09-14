package services

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestExpiredPluginResourceClaimsAreCleanedInBoundedBatches(t *testing.T) {
	service, _, actor, _, _, _ := siteFixture(t)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	pluginID := "org.ohmycine.resource-cleanup"
	packageRecord := models.PluginPackage{PluginID: pluginID, Version: "0.1.0", RepositoryOwner: "fixture", RepositoryRepo: "plugins", RegistryCommit: strings.Repeat("a", 40), RegistryEntryJSON: `{}`, ManifestURL: "https://example.test/manifest.json", PackageURL: "https://example.test/plugin.omcp", PackageSHA256: strings.Repeat("b", 64), ExtractedTreeSHA256: strings.Repeat("c", 64), ManifestJSON: `{}`, PackagePath: t.TempDir(), VerifiedAt: now, CreatedAt: now}
	if err := service.db.Create(&packageRecord).Error; err != nil {
		t.Fatal(err)
	}
	installation := models.PluginInstallation{PluginID: pluginID, ActivePackageID: packageRecord.ID, Status: models.PluginInstallationEnabled, Revision: 1, InstalledAt: now, UpdatedAt: now}
	if err := service.db.Create(&installation).Error; err != nil {
		t.Fatal(err)
	}
	connection := models.PluginConnection{ID: uuid.NewString(), PluginID: pluginID, Name: "cleanup", ConfigJSON: `{}`, CredentialScope: "resource.session", CredentialMode: models.PluginCredentialModeCookie, ResourceType: "bt_resource", EntryOrigin: "https://mirror.example/", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	site := models.Site{Name: "cleanup", NameNormalized: "resource-cleanup", Kind: pluginResourceKind, SourceType: "plugin", PluginID: connection.PluginID, PluginConnectionID: connection.ID, BaseURL: connection.EntryOrigin, Enabled: true, Priority: 100, TimeoutSeconds: 15, RateLimitPerMinute: 30, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := service.db.Create(&site).Error; err != nil {
		t.Fatal(err)
	}
	consumed := now.Add(-time.Minute)
	claims := []models.PluginResourceClaim{
		{ID: uuid.NewString(), TokenHash: tokenDigest("expired-unconsumed"), OwnerID: actor.User.ID, SiteID: site.ID, PluginID: connection.PluginID, PluginVersion: "0.1.0", PluginConnectionID: connection.ID, ResourceID: "1", Title: "expired", ExpiresAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour)},
		{ID: uuid.NewString(), TokenHash: tokenDigest("expired-consumed"), OwnerID: actor.User.ID, SiteID: site.ID, PluginID: connection.PluginID, PluginVersion: "0.1.0", PluginConnectionID: connection.ID, ResourceID: "2", Title: "expired", ExpiresAt: now.Add(-time.Second), ConsumedAt: &consumed, CreatedAt: now.Add(-time.Hour)},
		{ID: uuid.NewString(), TokenHash: tokenDigest("live"), OwnerID: actor.User.ID, SiteID: site.ID, PluginID: connection.PluginID, PluginVersion: "0.1.0", PluginConnectionID: connection.ID, ResourceID: "3", Title: "live", ExpiresAt: now.Add(time.Minute), CreatedAt: now},
	}
	if err := service.db.Create(&claims).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.cleanupExpiredPluginResourceClaims(1); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := service.db.Model(&models.PluginResourceClaim{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("first bounded cleanup left %d claims", count)
	}
	if err := service.cleanupExpiredPluginResourceClaims(1); err != nil {
		t.Fatal(err)
	}
	var remaining []models.PluginResourceClaim
	if err := service.db.Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || !remaining[0].ExpiresAt.After(now) || remaining[0].ResourceID != "3" {
		t.Fatalf("remaining=%+v", remaining)
	}
}
