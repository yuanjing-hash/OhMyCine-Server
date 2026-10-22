package services

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
)

type quarkPlaybackFixtureDriver struct {
	*fakeCloudDriver
	expiry time.Time
}

func (d *quarkPlaybackFixtureDriver) DirectURL(ctx context.Context, r cloud.DirectURLRequest) (cloud.TemporaryURL, error) {
	link, err := d.fakeCloudDriver.DirectURL(ctx, r)
	link.ExpiresAt = d.expiry
	return link, err
}

func TestQuarkSignedPlaybackOriginalNoPickCodeAndCredentialCacheFence(t *testing.T) {
	s, actor, connection := createQuarkConnection(t)
	driver := &quarkPlaybackFixtureDriver{fakeCloudDriver: &fakeCloudDriver{signedProxy: true, items: map[string]cloud.Item{"tv-file": {ID: "tv-file", ParentID: "tv-root", Name: "Original.mkv", Size: 1024}}}, expiry: time.Now().Add(time.Minute)}
	if err := s.registry.Register(cloud.ProviderQuark, func(cloud.Config) (cloud.Driver, error) { return driver, nil }); err != nil {
		t.Fatal(err)
	}
	saveQuarkTestBundle(t, s, connection.ID, func(c *quark.Credentials) { c.TV = testQuarkAuthorization() })
	now := time.Now().UTC()
	storage := models.Storage{Name: "q", NameNormalized: "q", Type: models.StorageTypeQuark, RootPath: "tv-root", RootPathNormalized: "quark:test", ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err := s.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := s.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "q", NameNormalized: "q", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, ProviderRootID: "tv-root", RelativeRoot: "/", Enabled: true, STRMEnabled: true, SignedProxyEnabled: true, STRMLocalRoot: t.TempDir(), VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`}
	if err := s.db.Select("*").Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaArtifactRun{ID: "quark-original-test", LibraryID: library.ID, Generation: 1, Status: models.MediaArtifactStatusCompleted, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	artifact := models.MediaArtifact{OpaqueID: "quark_artifact_AAAAAAAAAA", RunID: run.ID, LibraryID: library.ID, ProviderItemID: "tv-file", SourceFingerprint: "content-v1", Kind: models.MediaArtifactKindSTRM, TargetKind: models.MediaArtifactTargetLocalProjection, RelativePath: "/Original.strm", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted}
	if err := s.db.Select("*").Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	proxy, err := NewSignedProxyService(s.db, s.credentials, s, "https://media.example.test", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	signed, err := proxy.SignArtifact(artifact.OpaqueID, library.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(signed)
	expires, _ := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	resolve := func() (ProxyRedirect, error) {
		return proxy.Resolve(context.Background(), artifact.OpaqueID, u.Query().Get("kid"), expires, u.Query().Get("sig"), "Player")
	}
	for range 2 {
		if _, err = resolve(); err != nil {
			t.Fatal(err)
		}
	}
	if driver.directURLCalls.Load() != 1 {
		t.Fatal("valid credential/file cache not reused")
	}
	saveQuarkTestBundle(t, s, connection.ID, func(c *quark.Credentials) { c.TV.AccessToken = "rotated-access" })
	if _, err = resolve(); err != nil {
		t.Fatal(err)
	}
	if driver.directURLCalls.Load() != 2 {
		t.Fatal("rotated credential reused old direct link")
	}
	if err = s.db.Model(&artifact).Update("source_fingerprint", "content-v2").Error; err != nil {
		t.Fatal(err)
	}
	driver.directHeaders = http.Header{"Cookie": []string{"private=must-not-escape"}}
	if _, err = resolve(); ErrorCode(err) != CodeProxyHeadersUnsupported {
		t.Fatalf("Cookie direct link accepted: %v", err)
	}
	driver.directHeaders = nil
	driver.expiry = time.Time{}
	for range 2 {
		if _, err = resolve(); err != nil {
			t.Fatal(err)
		}
	}
	if driver.directURLCalls.Load() != 5 {
		t.Fatal("unknown-expiry link was cached")
	}
	driver.items["tv-file"] = cloud.Item{ID: "tv-file", ParentID: "outside-root", Name: "Original.mkv", Size: 1024}
	if _, err = resolve(); ErrorCode(err) != CodeProxyTargetUnavailable {
		t.Fatalf("moved outside library remained playable: %v", err)
	}
	_ = actor
}

func TestQuarkPlayerCatalogAdvertisesOriginalRedirect(t *testing.T) {
	s, store, library, _, entries, actor := playerSnapshotFixture(t)
	connection := models.Connection{Name: "quark-player", NameNormalized: "quark-player", Provider: models.ConnectionProviderQuark, Enabled: true}
	if err := store.writeDB.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Model(&models.Storage{}).Where("id = ?", library.StorageID).Updates(map[string]any{"type": models.StorageTypeQuark, "connection_id": connection.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Model(&library).Updates(map[string]any{"strm_enabled": true, "signed_proxy_enabled": true}).Error; err != nil {
		t.Fatal(err)
	}
	libraries, err := s.PlayerLibraries(actor)
	if err != nil || len(libraries) != 1 || !libraries[0].DirectStream || libraries[0].StorageType != models.StorageTypeQuark {
		t.Fatalf("Quark library not playable: %+v %v", libraries, err)
	}
	detail, err := s.PlayerCatalogDetail(context.Background(), actor, library.ID, encodeCatalogToken(entries[0].WorkKey))
	if err != nil || len(detail.Versions) == 0 {
		t.Fatalf("Quark versions missing: %+v %v", detail, err)
	}
	for _, version := range detail.Versions {
		if !version.Playable || version.DeliveryKind != playerDeliveryServerRedirect || version.StreamPath == "" {
			t.Fatalf("Quark original redirect missing: %+v", version)
		}
	}
}
