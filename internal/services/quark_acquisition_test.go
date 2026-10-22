package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/pansou"
)

type quarkPreviewFixtureDriver struct{ *previewFixtureDriver }

func (d *quarkPreviewFixtureDriver) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{NetworkDrive: true, DirectoryList: true, ShareReceive: true, CreateDirectory: true, Move: true, Rename: true}
}

func TestQuarkSharePreviewRoutesAndPersistsExactSelectedManifest(t *testing.T) {
	f, libraryID, _ := quarkIntakeFixture(t)
	ctx := context.Background()
	var storage models.Storage
	if e := f.db.First(&storage, f.storage.ID).Error; e != nil {
		t.Fatal(e)
	}
	f.downloads.downloader.connections.drivers[*storage.ConnectionID] = &quarkPreviewFixtureDriver{&previewFixtureDriver{f.driver}}
	rawURL := "https://pan.quark.cn/s/example"
	adapter := &previewFixtureAdapter{&cloudFixtureAdapter{stubResolverAdapter: &stubResolverAdapter{stubSiteAdapter: &stubSiteAdapter{kind: pansou.Kind}, resolved: sitepkg.Source{ShareURL: rawURL, CloudProvider: "quark"}}}}
	s := NewSiteServiceWithAdapters(f.db, NewAuditService(f.db), f.store, f.downloads, []sitepkg.Adapter{adapter}, zerolog.Nop())
	site, e := s.Create(ctx, f.actor, SiteInput{Name: "Local PanSou", Kind: pansou.Kind, BaseURL: "http://pansou:8888", CloudConfig: &sitepkg.CloudConfig{Provider: "all", Channels: []string{"movies"}}, Enabled: true, Priority: 100, TimeoutSeconds: 30, RateLimitPerMinute: 120}, RequestContext{})
	if e != nil {
		t.Fatal(e)
	}
	claim := siteResultClaim{ActorID: f.actor.User.ID, SiteID: site.ID, SiteRevision: site.Revision, TorrentID: rawURL, SourceKind: downloadpkg.SourceQuarkShare, CloudProvider: "quark", Title: "Collection", ExpiresAt: time.Now().Add(10 * time.Minute)}
	token, e := s.issueClaim(claim)
	if e != nil {
		t.Fatal(e)
	}
	recommendation, e := s.RecommendSiteRoute(ctx, f.actor, SourceRouteRecommendationInput{ResultToken: token, MediaLibraryID: &libraryID})
	if e != nil || recommendation.SourceKind != RouteSourceQuarkShare || recommendation.Recommended == nil || recommendation.Recommended.DownloaderID != f.downloader.ID {
		t.Fatalf("wrong source route %+v %v", recommendation, e)
	}
	preview, e := s.PreviewShare(ctx, f.actor, token, f.downloader.ID)
	if e != nil {
		t.Fatal(e)
	}
	selected := ""
	for _, entry := range preview.Entries {
		if entry.Name == "Seven.Samurai.1954.mkv" {
			selected = entry.Token
		}
	}
	result, e := s.Download(ctx, f.actor, SiteDownloadInput{ResultToken: token, DownloaderID: f.downloader.ID, MediaLibraryID: &libraryID, PreviewToken: preview.Token, SelectedEntryTokens: []string{selected}}, RequestContext{})
	if e != nil {
		t.Fatal(e)
	}
	var task models.DownloadTask
	if e = f.db.First(&task, "id=?", result.ID).Error; e != nil {
		t.Fatal(e)
	}
	raw, e := f.store.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if e != nil {
		t.Fatal(e)
	}
	var src downloadSourceEnvelope
	if e = json.Unmarshal([]byte(raw), &src); e != nil {
		t.Fatal(e)
	}
	if task.ProviderType != models.DownloaderTypeQuarkShare || src.Kind != downloadpkg.SourceQuarkShare || src.ShareSelection == nil || len(src.ShareSelection.Files) != 1 || src.ShareSelection.Files[0].ID != "private-one" {
		t.Fatalf("source did not retain exact provider selection %+v", src)
	}
	worker := NewDownloadWorker(f.downloads)
	good := downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "Movies/Seven.Samurai.1954.mkv", Size: 100}}}
	if e = worker.validateSelectedShareManifest(task, good); e != nil {
		t.Fatal(e)
	}
	good.Files = append(good.Files, downloadpkg.File{RelativePath: "Movies/Other.mkv", Size: 200})
	if e = worker.validateSelectedShareManifest(task, good); e == nil {
		t.Fatal("extra unselected output entered pipeline")
	}
	// A cloud source without verified deletion may be copied across providers,
	// but admission must never promise a move while silently retaining it.
	root := t.TempDir()
	local := models.Storage{Name: "Local target", NameNormalized: "local-target", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: root, Enabled: true, Capabilities: "{}"}
	if e = f.db.Create(&local).Error; e != nil {
		t.Fatal(e)
	}
	var library models.MediaLibrary
	var downloader models.Downloader
	if e = f.db.First(&library, libraryID).Error; e != nil {
		t.Fatal(e)
	}
	if e = f.db.First(&downloader, "id=?", f.downloader.ID).Error; e != nil {
		t.Fatal(e)
	}
	library.StorageID = local.ID
	library.RelativeRoot = "/"
	library.TransferMode = models.MediaLibraryTransferMove
	_, _, e = f.downloads.previewDownloadTarget(ctx, downloader, library, downloadpkg.SourceQuarkShare)
	if ErrorCode(e) != CodeTransferRouteUnsupported || !strings.Contains(ErrorMessage(e), "源文件回收") {
		t.Fatalf("cross-cloud move silently retained source: %v", e)
	}
}
