package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/pan115"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

// These are encrypted synthetic credentials and local-only native builders.
// No provider request is needed to decide whether a receiver is configured.
func seedPanSouReceiver(t *testing.T, s *SiteService, provider string) (models.Downloader, models.Storage, models.Connection) {
	t.Helper()
	downloaders := s.downloads.downloader
	if downloaders.connections == nil {
		registry := cloud.NewRegistry()
		if err := registry.Register(cloud.ProviderPan115, pan115.New); err != nil {
			t.Fatal(err)
		}
		if err := registry.Register(cloud.ProviderQuark, quark.New); err != nil {
			t.Fatal(err)
		}
		downloaders.SetConnectionService(NewConnectionService(s.db, s.audit, downloaders.credentials, registry, s.log))
	}
	connection := models.Connection{Name: "pansou-" + provider, NameNormalized: "pansou-" + provider, Provider: provider, Enabled: true, Revision: 1, LastHealthStatus: "offline"}
	if err := s.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	credential := testPan115Cookie
	if provider == models.ConnectionProviderQuark {
		bundle := quark.NewCredentials()
		bundle.TV, bundle.Agent = testQuarkAuthorization(), testQuarkAuthorization()
		connection.AccountID = quarkAccountReference(bundle.TV.AccountID)
		bundle.Proofs = []quark.SpaceProof{{Version: 1, Generation: 1, TVAccount: bundle.TV.AccountID, AgentAccount: bundle.Agent.AccountID, TVRoot: "root", AgentRoot: "agent-root", Path: "/media", ContentSHA256: strings.Repeat("a", 64), VerifiedAt: time.Now().UTC()}}
		raw, err := json.Marshal(bundle)
		if err != nil {
			t.Fatal(err)
		}
		credential = string(raw)
	}
	ciphertext, err := downloaders.credentials.Encrypt(connectionPurpose(connection.ID, provider), credential)
	if err != nil {
		t.Fatal(err)
	}
	connection.CredentialCiphertext = ciphertext
	if err := s.db.Save(&connection).Error; err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: provider, NameNormalized: provider, Type: provider, ConnectionID: &connection.ID, RootPath: "root", RootPathNormalized: provider + ":root", RootDisplayPath: "/media", Enabled: true, Capabilities: `{}`}
	if err := s.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	kind := models.DownloaderTypePan115Offline
	if provider == models.ConnectionProviderQuark {
		kind = models.DownloaderTypeQuarkShare
	}
	row := models.Downloader{ID: "receiver-" + provider, Name: provider, NameNormalized: "receiver-" + provider, Type: kind, StorageID: &storage.ID, ProviderDirectoryID: "root", ProviderDirectoryPath: "/", Enabled: true, CapabilitiesJSON: `{"share_receive":true}`, LastHealthStatus: "offline"}
	if err := s.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row, storage, connection
}

func TestPanSouAutomaticTypesRejectIncompleteReceivingConfiguration(t *testing.T) {
	for _, mode := range []string{"ready", "auth_only", "library_only", "no_proof", "wrong_root", "wrong_account", "missing_credential", "disabled_receiver", "disabled_storage", "disabled_connection", "missing_directory", "wrong_provider", "denied_receiver"} {
		t.Run(mode, func(t *testing.T) {
			s, _, actor, _, _, _ := siteFixture(t)
			actor.Permissions[authz.PermissionConnectionsRead] = struct{}{}
			_, _, _ = seedPanSouReceiver(t, s, models.ConnectionProviderPan115)
			receiver, storage, connection := seedPanSouReceiver(t, s, models.ConnectionProviderQuark)
			switch mode {
			case "auth_only", "library_only":
				if err := s.db.Delete(&receiver).Error; err != nil {
					t.Fatal(err)
				}
				if mode == "library_only" {
					var profile models.MediaClassificationProfile
					if err := s.db.First(&profile).Error; err != nil {
						t.Fatal(err)
					}
					library := models.MediaLibrary{Name: "Visible Quark", NameNormalized: "visible-quark", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/", ProviderRootID: "root", Enabled: true}
					if err := s.db.Create(&library).Error; err != nil {
						t.Fatal(err)
					}
				}
			case "no_proof", "wrong_account":
				saveQuarkTestBundle(t, s.downloads.downloader.connections, connection.ID, func(c *quark.Credentials) {
					if mode == "no_proof" {
						c.Proofs = nil
					} else {
						c.Proofs[0].AgentAccount = "other-account"
					}
				})
			case "wrong_root":
				if err := s.db.Model(&storage).Update("root_path", "unproved-root").Error; err != nil {
					t.Fatal(err)
				}
			case "missing_credential":
				if err := s.db.Model(&connection).Update("credential_ciphertext", "").Error; err != nil {
					t.Fatal(err)
				}
			case "disabled_receiver":
				if err := s.db.Model(&receiver).Update("enabled", false).Error; err != nil {
					t.Fatal(err)
				}
			case "disabled_storage":
				if err := s.db.Model(&storage).Update("enabled", false).Error; err != nil {
					t.Fatal(err)
				}
			case "disabled_connection":
				if err := s.db.Model(&connection).Update("enabled", false).Error; err != nil {
					t.Fatal(err)
				}
			case "missing_directory":
				if err := s.db.Model(&receiver).Update("provider_directory_id", "").Error; err != nil {
					t.Fatal(err)
				}
			case "wrong_provider":
				if err := s.db.Model(&storage).Update("type", models.StorageTypePan115).Error; err != nil {
					t.Fatal(err)
				}
			case "denied_receiver":
				actor.DeniedPermissions = map[string]struct{}{authz.PermissionConnectionsRead: {}}
				actor.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeDownloaderUse: {Scope: models.ResourceAccessScopeDownloaderUse, Mode: models.ResourceAccessModeDenylist, ResourceIDs: []string{receiver.ID}}}
			}
			types, err := s.panSouAutomaticTypes(actor)
			want := "115,magnet"
			if mode == "ready" {
				want += ",quark"
			}
			if err != nil || strings.Join(types, ",") != want {
				t.Fatalf("types=%v want=%s err=%v", types, want, err)
			}
		})
	}
}

func TestPanSouExplicitUnavailableShareKeepsItsProviderAndLink(t *testing.T) {
	s, _, actor, _, _, _ := siteFixture(t)
	_, _, _ = seedPanSouReceiver(t, s, models.ConnectionProviderPan115)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"total":1,"results":[{"unique_id":"fixture-quark","title":"Show","links":[{"type":"quark","url":"https://pan.quark.cn/s/fixture"}]}]}}`)
	}))
	defer upstream.Close()
	s.adapters[pansou.Kind] = pansou.NewForTest(upstream.Client())
	site, err := s.Create(context.Background(), actor, SiteInput{Name: "Explicit PanSou", Kind: pansou.Kind, BaseURL: upstream.URL, CloudConfig: &sitepkg.CloudConfig{SourceScope: "plugin"}, Enabled: true, Priority: 100, TimeoutSeconds: 10, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []string{"all", "manual"} {
		groups, err := s.Search(context.Background(), actor, SiteSearchInput{Keyword: "Show", Page: 1, SiteID: &site.ID, ClientCapabilities: []string{MixedDiscoveryCapability}, TypePolicy: policy, CloudTypes: []string{"quark"}})
		if err != nil || len(groups) != 1 || len(groups[0].Items) != 1 {
			t.Fatalf("groups=%+v err=%v", groups, err)
		}
		item := groups[0].Items[0]
		if item.SourceKind != RouteSourceQuarkShare || item.CloudProvider != "quark" || item.ResultType != "quark" || item.AcquisitionAvailable == nil || *item.AcquisitionAvailable || !strings.Contains(item.AcquisitionReason, "夸克") {
			t.Fatalf("unready Quark misrepresented: %+v", item)
		}
		link, err := s.ResultLink(context.Background(), actor, item.Token)
		if err != nil || link.ResultType != "quark" {
			t.Fatalf("view-only link=%+v err=%v", link, err)
		}
	}
}

func TestPanSouSnapshotBindsReceiverConfigurationWithoutHealthOrTokenChurn(t *testing.T) {
	for _, policy := range []string{"auto", "all", "manual"} {
		for _, change := range []string{"health", "refresh_tokens", "disabled", "directory", "root", "proof", "reproof", "account", "permission"} {
			t.Run(policy+"/"+change, func(t *testing.T) {
				s, _, actor, _, _, _ := siteFixture(t)
				receiver, storage, connection := seedPanSouReceiver(t, s, models.ConnectionProviderQuark)
				requests := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					items := []any{}
					for i := range 61 {
						items = append(items, map[string]any{"unique_id": fmt.Sprintf("fixture-%d", i), "title": fmt.Sprintf("Show.%03d", i), "links": []any{map[string]string{"type": "magnet", "url": fmt.Sprintf("magnet:?xt=urn:btih:%040x", i+1)}}})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"total": len(items), "results": items}})
				}))
				defer upstream.Close()
				s.adapters[pansou.Kind] = pansou.NewForTest(upstream.Client())
				site, err := s.Create(context.Background(), actor, SiteInput{Name: "Snapshot PanSou", Kind: pansou.Kind, BaseURL: upstream.URL, CloudConfig: &sitepkg.CloudConfig{SourceScope: "plugin"}, Enabled: true, Priority: 100, TimeoutSeconds: 10, RateLimitPerMinute: 120}, RequestContext{})
				if err != nil {
					t.Fatal(err)
				}
				input := SiteSearchInput{Keyword: "Show", Page: 1, SiteID: &site.ID, ClientCapabilities: []string{MixedDiscoveryCapability}, TypePolicy: policy, CloudTypes: []string{"magnet", "quark"}}
				first, err := s.Search(context.Background(), actor, input)
				if err != nil || len(first) != 1 || len(first[0].Items) != 50 || !first[0].HasNext {
					t.Fatalf("first=%+v err=%v", first, err)
				}
				before := requests
				input.Page, input.SnapshotID = 2, first[0].SnapshotID
				switch change {
				case "health":
					if err := s.db.Model(&connection).Updates(map[string]any{"last_health_status": "offline", "last_health_error_code": quark.CodeLimited}).Error; err != nil {
						t.Fatal(err)
					}
				case "refresh_tokens", "proof", "reproof", "account":
					saveQuarkTestBundle(t, s.downloads.downloader.connections, connection.ID, func(c *quark.Credentials) {
						switch change {
						case "refresh_tokens":
							c.TV.AccessToken, c.TV.RefreshToken = "rotated-access", "rotated-refresh"
							c.Agent.AccessToken, c.Agent.RefreshToken = "rotated-agent", "rotated-agent-refresh"
						case "proof":
							c.Proofs = nil
						case "reproof":
							c.Proofs[0].Generation++
						case "account":
							c.Proofs[0].AgentAccount = "other-account"
						}
					})
				case "disabled":
					if err := s.db.Model(&receiver).Update("enabled", false).Error; err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := s.db.Model(&receiver).Update("provider_directory_id", "other-directory").Error; err != nil {
						t.Fatal(err)
					}
				case "root":
					if err := s.db.Model(&storage).Update("root_path", "other-root").Error; err != nil {
						t.Fatal(err)
					}
				case "permission":
					actor.ResourceAccessPolicies = map[string]ResourceAccessPolicy{models.ResourceAccessScopeDownloaderUse: {Scope: models.ResourceAccessScopeDownloaderUse, Mode: models.ResourceAccessModeDenylist, ResourceIDs: []string{receiver.ID}}}
				}
				second, err := s.Search(context.Background(), actor, input)
				if err != nil || len(second) != 1 || requests != before {
					t.Fatalf("snapshot contacted upstream: groups=%+v requests=%d/%d err=%v", second, requests, before, err)
				}
				if change == "health" || change == "refresh_tokens" {
					if second[0].Status != "success" || len(second[0].Items) != 11 {
						t.Fatalf("transient state invalidated snapshot: %+v", second)
					}
				} else if second[0].ErrorCode != CodeSiteResultExpired || len(second[0].Items) != 0 {
					t.Fatalf("configuration change reused snapshot: %+v", second)
				}
			})
		}
	}
}

func TestPanSouReceiverSummaryAndPreviewRejectMissingProofBeforeProviderRead(t *testing.T) {
	s, _, actor, _, downloads, downloaders := siteFixture(t)
	receiver, storage, connection := seedPanSouReceiver(t, s, models.ConnectionProviderQuark)
	if summary := downloaders.summary(actor, receiver); !summary.Capabilities.ShareReceive {
		t.Fatal("ready receiver was hidden", summary)
	}
	saveQuarkTestBundle(t, downloaders.connections, connection.ID, func(c *quark.Credentials) { c.Proofs = nil })
	if summary := downloaders.summary(actor, receiver); summary.Capabilities.ShareReceive {
		t.Fatal("unproved receiver advertised share receive", summary)
	}
	var profile models.MediaClassificationProfile
	if err := s.db.First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Quark target", NameNormalized: "quark-target", StorageID: storage.ID, ProfileID: profile.ID, RelativeRoot: "/", ProviderRootID: "root", TransferMode: models.MediaLibraryTransferMove, Enabled: true}
	if err := s.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	preview, err := downloads.PreviewRoutes(context.Background(), actor, DownloadRoutePreviewInput{DownloaderID: receiver.ID, SourceKind: RouteSourceQuarkShare})
	if err != nil || len(preview.Options) != 1 || preview.Options[0].Enabled || preview.Options[0].ReasonCode == "" {
		t.Fatalf("unproved preview=%+v err=%v", preview, err)
	}
	claim := siteResultClaim{ActorID: actor.User.ID, SourceKind: RouteSourceQuarkShare, CloudProvider: "quark", TorrentID: "https://pan.quark.cn/s/fixture"}
	_, _, _, err = s.inspectResultShare(context.Background(), actor, claim, claim.TorrentID, receiver.ID, true, RequestContext{})
	if ErrorCode(err) != CodeDownloaderUnavailable {
		t.Fatalf("unproved share reached provider: %v", err)
	}
}

type panSouAuthorizationGuardDriver struct {
	cloud.Driver
	resumed int
}

func (d *panSouAuthorizationGuardDriver) ResumeAuthorizations(context.Context) error {
	d.resumed++
	return fmt.Errorf("discovery must not resume authorization")
}

func TestPanSouSearchAndSummaryNeverResumeProviderAuthorization(t *testing.T) {
	s, _, actor, _, _, downloaders := siteFixture(t)
	receiver, _, connection := seedPanSouReceiver(t, s, models.ConnectionProviderQuark)
	_, driver, err := downloaders.connections.cachedDriver(connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	guard := &panSouAuthorizationGuardDriver{Driver: driver}
	downloaders.connections.drivers[connection.ID] = guard
	types, err := s.panSouAutomaticTypes(actor)
	if err != nil || strings.Join(types, ",") != "magnet,quark" {
		t.Fatalf("types=%v err=%v", types, err)
	}
	summary := downloaders.summary(actor, receiver)
	if !summary.Capabilities.ShareReceive || guard.resumed != 0 {
		t.Fatalf("discovery contacted provider: summary=%+v calls=%d", summary, guard.resumed)
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "private-account", "agent-root", "content_sha256", "credential_ciphertext"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("receiver summary leaked %q", secret)
		}
	}
}

func TestPanSouNodeReceiverConfigurationDistinguishesOfflineFromUnsupported(t *testing.T) {
	f, node, _ := quarkNodeAdmissionFixture(t)
	var receiver models.Downloader
	if err := f.db.First(&receiver, "id = ?", f.downloader.ID).Error; err != nil {
		t.Fatal(err)
	}
	service := f.downloads.downloader
	configured, err := service.configuredShareReceiver(context.Background(), receiver)
	if err != nil || configured == "" {
		t.Fatalf("configured=%q err=%v", configured, err)
	}
	if err := f.db.Model(&node).Update("status", models.NodeStatusOffline).Error; err != nil {
		t.Fatal(err)
	}
	offline, err := service.configuredShareReceiver(context.Background(), receiver)
	if err != nil || offline != configured {
		t.Fatalf("temporary node outage erased configuration: key=%q err=%v", offline, err)
	}
	codes := []string{nodeprotocol.CapabilityQuarkReadV2, nodeprotocol.CapabilityContentDigestV2, nodeprotocol.CapabilityRangeExport}
	raw, _ := json.Marshal(nodeprotocol.Capabilities{Codes: codes})
	if err := f.db.Model(&node).Update("capabilities_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.configuredShareReceiver(context.Background(), receiver); err == nil {
		t.Fatal("node without share-receive capability was eligible")
	}
	if err := f.db.Model(&node).Updates(map[string]any{"status": models.NodeStatusRevoked, "capabilities_json": node.CapabilitiesJSON}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.configuredShareReceiver(context.Background(), receiver); err == nil {
		t.Fatal("revoked node was eligible")
	}
}
