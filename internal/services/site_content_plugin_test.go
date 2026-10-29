package services

import (
	"context"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

type contentResolverFunc func(context.Context, resourcecontent.ResolveRequest) (resourcecontent.Manifest, error)

func (f contentResolverFunc) Resolve(ctx context.Context, input resourcecontent.ResolveRequest) (resourcecontent.Manifest, error) {
	return f(ctx, input)
}

type contentPluginBridge struct{ *stubPluginResourceBridge }

func (b *contentPluginBridge) SearchResource(context.Context, PluginResourceSearchInput) (PluginResourceSearchPage, error) {
	b.searchCalls++
	return PluginResourceSearchPage{Page: 1, Items: []PluginResourceSearchItem{{ID: "actual-plugin-resource", Title: "Fixture.Show.S01.Complete", Seeders: 12}}}, nil
}

func TestContentPluginBridgeAdvertisesModernInspectionAndFollowUsesActualSelection(t *testing.T) {
	s, _, actor, _, _, _ := siteFixture(t)
	now := s.now()
	raw := inspectionTorrent("Season 01/01.mkv", "Season 01/13.mkv", "Season 01/15.mkv")
	manifest, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	bridge := &contentPluginBridge{&stubPluginResourceBridge{pluginID: "org.ohmycine.content-fixture", version: "1.0.0", magnet: "magnet:?xt=urn:btih:" + manifest.Identity.V1}}
	s.SetPluginResourceBridge(bridge)
	packageRecord := models.PluginPackage{PluginID: bridge.pluginID, Version: bridge.version, RepositoryOwner: "fixture", RepositoryRepo: "fixture", RegistryCommit: strings.Repeat("a", 40), RegistryEntryJSON: `{}`, ManifestURL: "https://fixture.example.test/manifest.json", PackageURL: "https://fixture.example.test/pkg.omcp", PackageSHA256: strings.Repeat("b", 64), ExtractedTreeSHA256: strings.Repeat("c", 64), ManifestJSON: `{}`, PackagePath: t.TempDir(), VerifiedAt: now, CreatedAt: now}
	if err := s.db.Create(&packageRecord).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&models.PluginInstallation{PluginID: bridge.pluginID, ActivePackageID: packageRecord.ID, Status: models.PluginInstallationEnabled, Revision: 1, InstalledAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	connection := models.PluginConnection{ID: "11111111-1111-4111-8111-111111111111", PluginID: bridge.pluginID, Name: "Content fixture", ConfigJSON: `{}`, CredentialMode: models.PluginCredentialModeCookie, ResourceType: "bt_resource", EntryOrigin: "https://resource.example.test", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	site := models.Site{Name: "Plugin content", NameNormalized: "plugin-content", Kind: pluginResourceKind, SourceType: "plugin", PluginID: bridge.pluginID, PluginConnectionID: connection.ID, BaseURL: "https://resource.example.test", Enabled: true, Priority: 1, TimeoutSeconds: 15, RateLimitPerMinute: 120, LastHealthStatus: "healthy", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.db.Create(&site).Error; err != nil {
		t.Fatal(err)
	}
	resolves := 0
	s.SetResourceContentResolver(contentResolverFunc(func(_ context.Context, input resourcecontent.ResolveRequest) (resourcecontent.Manifest, error) {
		resolves++
		if input.URI != bridge.magnet || input.Private {
			t.Error("plugin source provenance changed")
		}
		return manifest, nil
	}))
	groups, err := s.Search(context.Background(), actor, SiteSearchInput{Keyword: "Fixture Show", SiteID: &site.ID, Page: 1})
	if err != nil || len(groups) != 1 || len(groups[0].Items) != 1 {
		t.Fatalf("groups=%+v err=%v", groups, err)
	}
	item := groups[0].Items[0]
	if !item.ContentInspection || item.PayloadKind != sitepkg.PayloadMagnet {
		t.Fatal("production plugin projection skipped modern lane")
	}
	if err := s.bindContentSearchIdentity(item.Token, actor.User.ID, *analysisIdentity()); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.prepareFollowContent(context.Background(), actor, item.Token, 1, []int{13}, SubtitlePolicy{Mode: "ignore"})
	if err != nil || len(prepared.Episodes) != 1 || prepared.Episodes[0] != 13 || resolves != 1 {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	claim, err := s.resolveClaim(item.Token, actor.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.ResolveContentSelection(context.Background(), actor, ContentSelectionInput{ResultToken: item.Token, SelectionToken: prepared.SelectionToken}, claim)
	if err != nil || selected.Torrent == nil || len(selected.Torrent.Files) != 1 || selected.Torrent.Files[0].Index != 1 || selected.AllFilesVerified {
		t.Fatalf("actual selected source=%+v err=%v", selected, err)
	}
	var tasks int64
	s.db.Model(&models.DownloadTask{}).Count(&tasks)
	if tasks != 0 {
		t.Fatal("plugin inspection or Follow preparation submitted a downloader task")
	}
	if err := s.db.Model(&models.PluginConnection{}).Where("id = ?", connection.ID).Update("revision", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveContentSelection(context.Background(), actor, ContentSelectionInput{ResultToken: item.Token, SelectionToken: prepared.SelectionToken}, claim); err == nil {
		t.Fatal("changed plugin connection reused a cached content plan")
	}
}
