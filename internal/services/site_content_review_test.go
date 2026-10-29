package services

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

// Exercise the same actual-file preparation used by Follow, then the public
// Site acquisition path and its encrypted durable plan. A helper-only episode
// count would miss a plan that still contains the overlapping physical leaf.
func TestContentFollowPreparationFreezesOnlyDisjointActualLeaves(t *testing.T) {
	s, _, actor, _, downloads, downloaders := siteFixture(t)
	raw := inspectionTorrentWithSize(32<<20, "Season 01/Fixture.Show.S01E13.1080p.mkv", "Season 01/Fixture.Show.S01E13.1080p.zh-Hans.ass", "Season 01/Fixture.Show.S01E13-E14.2160p.mkv")
	s.adapters["mikan"] = &stubResolverAdapter{stubSiteAdapter: &stubSiteAdapter{kind: "mikan"}, resolved: sitepkg.Source{Torrent: raw}}
	site, err := s.Create(context.Background(), actor, SiteInput{Name: "Overlap content", Kind: "mikan", BaseURL: "https://mikanani.me", Enabled: true, Priority: 1, TimeoutSeconds: 15, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	claim := siteResultClaim{ActorID: actor.User.ID, SiteID: site.ID, SiteRevision: site.Revision, TorrentID: "42", Title: "Fixture Show [01-52]", SearchIdentity: analysisIdentity(), ExpiresAt: s.now().Add(time.Minute)}
	token := issueAnalysisClaim(t, s, claim)
	prepared, err := s.prepareFollowContent(context.Background(), actor, token, 1, []int{13, 14}, SubtitlePolicy{Mode: "prefer", Languages: []string{"zh-Hans"}})
	if err != nil || !reflect.DeepEqual(prepared.Episodes, []int{13}) {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	client := &stubDownloadClient{}
	registry := downloadpkg.NewRegistry()
	if err := registry.Register(models.DownloaderTypeQBittorrent, downloadpkg.Capabilities{SelectiveFiles: true, Pause: true, Resume: true}, func(downloadpkg.Config) (downloadpkg.Client, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	downloaders.registry = registry
	downloader, err := downloaders.Create(actor, DownloaderInput{Name: "Disjoint selection", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://executor.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	storage := models.Storage{Name: "Disjoint target", NameNormalized: "disjoint-target", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: root, Enabled: true, Capabilities: `{}`}
	if err := s.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := s.db.First(&profile, "code = ?", "default-v1").Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Disjoint target", NameNormalized: "disjoint-target", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true, Status: models.MediaLibraryStatusListening, TransferMode: models.MediaLibraryTransferMove, VideoExtensionsJSON: `[]`, STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`}
	if err := s.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	queued, err := s.Download(context.Background(), actor, SiteDownloadInput{ResultToken: token, ContentMode: "files", ContentSelectionToken: prepared.SelectionToken, DownloaderID: downloader.ID, MediaLibraryID: &library.ID}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var task models.DownloadTask
	if err := s.db.First(&task, "id = ?", queued.ID).Error; err != nil {
		t.Fatal(err)
	}
	plaintext, err := downloads.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	var frozen downloadSourceEnvelope
	if err := json.Unmarshal([]byte(plaintext), &frozen); err != nil {
		t.Fatal(err)
	}
	if frozen.SelectionMode != "files" || frozen.ContentSelection == nil || len(frozen.ContentSelection.Files) != 2 || frozen.ContentSelection.Files[0].Index != 0 || frozen.ContentSelection.Files[1].Index != 1 || !bytes.Equal(frozen.Torrent, raw) {
		t.Fatalf("durable plan includes overlapping E14 package or lost original metainfo: %+v", frozen.ContentSelection)
	}
	if client.submits != 0 {
		t.Fatal("read preparation/local persistence submitted to a remote executor")
	}
}

type contentReviewRuntime struct{ checkpoint func(any) }

func (*contentReviewRuntime) Heartbeat(*float64, *int64, *int64, *float64, *int64) error { return nil }
func (r *contentReviewRuntime) Checkpoint(value any) error {
	r.checkpoint(value)
	return nil
}

func TestContentFollowDiscardPoppedPreparationAfterRouteIsRevoked(t *testing.T) {
	f, _ := newFollowPreviewFixture(t)
	claimed, err := f.follows.queue.Claim([]string{JobTypeFollowSearch})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	changed := false
	runtime := &contentReviewRuntime{checkpoint: func(value any) {
		checkpoint, ok := value.(map[string]any)
		if !ok || checkpoint["stage"] != "submit" || changed {
			return
		}
		changed = true
		if err := f.follows.db.Model(&models.Downloader{}).Where("id = ?", "follow-preview-115").Update("enabled", false).Error; err != nil {
			t.Fatal(err)
		}
	}}
	f.worker.Run(context.Background(), runtime, *claimed)
	if !changed {
		t.Fatal("fixture did not reach the real post-prepare submit checkpoint")
	}
	state := f.worker.sites.contents()
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.selections) != 0 || len(state.previews) != 0 {
		t.Fatalf("rejected popped candidate retained unused authority: selections=%d previews=%d", len(state.selections), len(state.previews))
	}
	var tasks int64
	f.follows.db.Model(&models.DownloadTask{}).Count(&tasks)
	if tasks != 0 {
		t.Fatal("a revoked route submitted a download")
	}
}
