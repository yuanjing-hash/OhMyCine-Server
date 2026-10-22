package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func lifecycleDownloadFixture(t *testing.T) (*SiteService, Actor, models.Site, models.PluginInstallation, models.PluginConnection, SiteDownloadInput, string) {
	t.Helper()
	s, _, actor, _, _, downloaders := siteFixture(t)
	now := s.now()
	bridge := &stubPluginResourceBridge{pluginID: "org.ohmycine.lifecyclefixture", version: "1.2.3", magnet: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}
	s.SetPluginResourceBridge(bridge)
	pkg := models.PluginPackage{PluginID: bridge.pluginID, Version: bridge.version, RepositoryOwner: "fixture", RepositoryRepo: "plugins", RegistryCommit: strings.Repeat("a", 40), RegistryEntryJSON: `{}`, ManifestURL: "https://example.test/manifest", PackageURL: "https://example.test/pkg", PackageSHA256: strings.Repeat("b", 64), ExtractedTreeSHA256: strings.Repeat("c", 64), ManifestJSON: `{}`, PackagePath: t.TempDir(), VerifiedAt: now, CreatedAt: now}
	if err := s.db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	installation := models.PluginInstallation{PluginID: bridge.pluginID, ActivePackageID: pkg.ID, Status: models.PluginInstallationEnabled, Revision: 1, RuntimeGeneration: 7, InstalledAt: now, UpdatedAt: now}
	if err := s.db.Create(&installation).Error; err != nil {
		t.Fatal(err)
	}
	connection := models.PluginConnection{ID: uuid.NewString(), PluginID: bridge.pluginID, Name: "Fixture", ConfigJSON: `{}`, ResourceType: "bt_resource", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	site := models.Site{Name: "Fixture resource", NameNormalized: "fixture-resource", Kind: pluginResourceKind, SourceType: "plugin", PluginID: bridge.pluginID, PluginConnectionID: connection.ID, BaseURL: "https://resource.example.test", Enabled: true, Revision: 1, Priority: 100, TimeoutSeconds: 10, RateLimitPerMinute: 120, LastHealthStatus: "healthy", CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&site).Error; err != nil {
		t.Fatal(err)
	}
	claimID := uuid.NewString()
	token, err := s.issueClaim(siteResultClaim{ActorID: actor.User.ID, SiteID: site.ID, SiteRevision: site.Revision, PluginClaimID: claimID, TorrentID: "resource", Title: "Sample.2026", ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	claim := models.PluginResourceClaim{ID: claimID, TokenHash: tokenDigest(token), OwnerID: actor.User.ID, SiteID: site.ID, PluginID: bridge.pluginID, PluginVersion: bridge.version, PluginConnectionID: connection.ID, ResourceID: "resource", Title: "Sample.2026", ExpiresAt: now.Add(time.Minute), CreatedAt: now}
	if err := s.db.Create(&claim).Error; err != nil {
		t.Fatal(err)
	}
	downloader, err := downloaders.Create(actor, DownloaderInput{Name: "Lifecycle qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://qbit.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	return s, actor, site, installation, connection, SiteDownloadInput{ResultToken: token, DownloaderID: downloader.ID, Priority: 10}, claimID
}

func TestPluginLifecycleFinalTransactionRejectsResolvedChanges(t *testing.T) {
	for _, change := range []string{"disable", "disable-reenable", "connection-revision", "active-package", "site-disabled", "site-disabled-during-enqueue"} {
		t.Run(change, func(t *testing.T) {
			s, actor, site, installation, connection, input, claimID := lifecycleDownloadFixture(t)
			input.BeforeSubmit = func() error {
				switch change {
				case "disable":
					return s.db.Model(&models.PluginInstallation{}).Where("plugin_id = ?", installation.PluginID).Updates(map[string]any{"status": models.PluginInstallationDisabled, "revision": gorm.Expr("revision + 1")}).Error
				case "disable-reenable":
					return s.db.Model(&models.PluginInstallation{}).Where("plugin_id = ?", installation.PluginID).Updates(map[string]any{"status": models.PluginInstallationEnabled, "revision": gorm.Expr("revision + 2"), "runtime_generation": gorm.Expr("runtime_generation + 1")}).Error
				case "connection-revision":
					return s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("revision", gorm.Expr("revision + 1")).Error
				case "site-disabled":
					return s.db.Model(&models.Site{}).Where("id = ?", site.ID).Update("enabled", false).Error
				case "active-package":
					var pkg models.PluginPackage
					if err := s.db.First(&pkg, installation.ActivePackageID).Error; err != nil {
						return err
					}
					pkg.ID = 0
					pkg.PackageSHA256 = strings.Repeat("d", 64)
					if err := s.db.Create(&pkg).Error; err != nil {
						return err
					}
					return s.db.Model(&models.PluginInstallation{}).Where("plugin_id = ?", installation.PluginID).Update("active_package_id", pkg.ID).Error
				}
				return nil
			}
			if change == "site-disabled-during-enqueue" {
				input.BeforePersist = func(tx *gorm.DB) error {
					return tx.Model(&models.Site{}).Where("id = ?", site.ID).Update("enabled", false).Error
				}
			}
			expectedCode := CodeSiteResultExpired
			if change == "site-disabled" {
				// The refreshed site check rejects this case before the claim-consuming
				// enqueue transaction; other lifecycle changes reach its final fence.
				expectedCode = CodeSiteUnavailable
			}
			if _, err := s.Download(context.Background(), actor, input, RequestContext{}); ErrorCode(err) != expectedCode {
				t.Fatalf("change=%s expected=%s code=%s err=%v", change, expectedCode, ErrorCode(err), err)
			}
			var claims models.PluginResourceClaim
			if err := s.db.First(&claims, "id = ?", claimID).Error; err != nil || claims.ConsumedAt != nil {
				t.Fatalf("failed transaction consumed claim: %+v err=%v", claims, err)
			}
			var count int64
			if err := s.db.Model(&models.DownloadTask{}).Where("plugin_resource_claim_id = ?", claimID).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("changed lifecycle enqueued", count)
			}
		})
	}
}

func TestPluginLifecycleManagementPreservesConfigurationAndAcceptedJob(t *testing.T) {
	s, actor, site, installation, connection, input, claimID := lifecycleDownloadFixture(t)
	// Use the real lifecycle bridge for status projection; it is metadata-only.
	bridge := s.pluginResources
	result, err := s.Download(context.Background(), actor, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Model(&models.PluginInstallation{}).Where("plugin_id = ?", installation.PluginID).Updates(map[string]any{"status": models.PluginInstallationDisabled, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
		t.Fatal(err)
	}
	stub := bridge.(*stubPluginResourceBridge)
	stub.provenanceErr = appError(CodePluginRuntimeUnavailable, "disabled", nil)
	if err := s.db.First(&site, site.ID).Error; err != nil {
		t.Fatal(err)
	}
	summary := s.siteSummary(site)
	if !summary.Enabled || summary.EffectiveEnabled || summary.Capabilities.Search || summary.Capabilities.Download || summary.UnavailableReason == "" {
		t.Fatal(summary)
	}
	options, err := s.SearchOptions(actor)
	if err != nil || len(options) != 0 {
		t.Fatal("disabled plugin selectable", options, err)
	}
	var accepted models.DownloadTask
	if err := s.db.First(&accepted, "id = ? AND plugin_resource_claim_id = ?", result.ID, claimID).Error; err != nil {
		t.Fatal("accepted frozen job lost", err)
	}
	if err := s.db.Model(&models.PluginInstallation{}).Where("plugin_id = ?", installation.PluginID).Updates(map[string]any{"status": models.PluginInstallationEnabled, "revision": gorm.Expr("revision + 1")}).Error; err != nil {
		t.Fatal(err)
	}
	stub.provenanceErr = nil
	if summary = s.siteSummary(site); !summary.EffectiveEnabled || !summary.Enabled {
		t.Fatal("re-enable did not restore", summary)
	}
	if err := s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	stub.provenanceErr = appError(CodeNotFound, "connection disabled", nil)
	if summary = s.siteSummary(site); summary.EffectiveEnabled || !summary.Enabled {
		t.Fatal("manual connection state overwritten", summary)
	}
	if err := s.db.First(&connection, "id = ?", connection.ID).Error; err != nil || connection.Enabled {
		t.Fatal("connection was blindly reenabled", err)
	}
}
