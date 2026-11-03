package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

type selectedOriginRecoveryAdapter struct {
	*stubSiteAdapter
	magnet   string
	resolves int
}

func (a *selectedOriginRecoveryAdapter) ResolveSource(_ context.Context, _ sitepkg.Config, identity string) (sitepkg.Source, error) {
	if identity != a.magnet {
		return sitepkg.Source{}, sitepkg.ErrNotFound
	}
	a.resolves++
	return sitepkg.Source{Magnet: a.magnet}, nil
}

// Exercise real claim/preview/selection/admission and the original queue retry.
// Only source metadata and the executor are local fixtures; no video is fetched.
func TestSelectedMagnetOriginAdmissionSurvivesDBReopenAndOriginalRetry(t *testing.T) {
	sites, _, actor, _, downloads, downloaders := siteFixture(t)
	baseClient := &stubDownloadClient{}
	client := &followSelectedCompletedClient{metadataDownloadClient: &metadataDownloadClient{stubDownloadClient: baseClient}}
	registry := downloadpkg.NewRegistry()
	if err := registry.Register(models.DownloaderTypeQBittorrent, downloadpkg.Capabilities{SelectiveFiles: true, Pause: true, Resume: true, OutputConstraint: downloadpkg.OutputConstraintLocalStaging}, func(downloadpkg.Config) (downloadpkg.Client, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	downloaders.registry = registry
	executor, err := downloaders.Create(actor, DownloaderInput{Name: "Selected recovery", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://recovery-fixture.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	raw := inspectionTorrentWithSize(32<<20, "Fixture.Show.S01E13.mkv", "Fixture.Show.S01E15.mkv")
	manifest, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	magnet := "magnet:?xt=urn:btih:" + manifest.Identity.V1
	adapter := &selectedOriginRecoveryAdapter{stubSiteAdapter: &stubSiteAdapter{kind: "pansou_tg"}, magnet: magnet}
	sites.adapters["pansou_tg"] = adapter
	sourceReads := 0
	sites.SetResourceContentResolver(contentResolverFunc(func(_ context.Context, input resourcecontent.ResolveRequest) (resourcecontent.Manifest, error) {
		if input.URI != magnet {
			return resourcecontent.Manifest{}, errors.New("fixture resolved another original source")
		}
		sourceReads++
		return manifest, nil
	}))
	site, err := sites.Create(context.Background(), actor, SiteInput{Name: "Mixed selected recovery", Kind: "pansou_tg", BaseURL: "https://recovery-fixture.test", CloudConfig: &sitepkg.CloudConfig{SourceScope: "plugin"}, Enabled: true, Priority: 1, TimeoutSeconds: 10, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	token, err := sites.issueClaim(siteResultClaim{ActorID: actor.User.ID, SiteID: site.ID, SiteRevision: site.Revision, PayloadKind: sitepkg.PayloadMagnet, SourceKind: RouteSourceBT, ResultType: "magnet", TorrentID: magnet, Title: "Fixture.Show.S01E13-E15", ExpiresAt: sites.now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := sites.PreviewContent(context.Background(), actor, ContentPreviewInput{ResultToken: token})
	if err != nil || !preview.Complete {
		t.Fatalf("preview complete=%t err=%v", preview.Complete, err)
	}
	entry := ""
	for _, file := range preview.Files {
		if strings.HasSuffix(file.Path, "S01E15.mkv") {
			entry = file.Token
		}
	}
	if entry == "" {
		t.Fatal("actual E15 leaf missing")
	}
	selection, err := sites.SelectContent(context.Background(), actor, ContentSelectionInput{ResultToken: token, PreviewToken: preview.Token, SelectedEntryTokens: []string{entry}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := sites.Download(context.Background(), actor, SiteDownloadInput{ResultToken: token, DownloaderID: executor.ID, ContentSelectionToken: selection.Token}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var original models.DownloadTask
	if err := downloads.db.First(&original, "id=?", accepted.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !original.ContentSelectionRequired {
		t.Fatal("accepted task lost exact-selection requirement")
	}
	resolvedBeforeRetry := adapter.resolves
	if err := downloads.db.Model(&models.Job{}).Where("id=?", original.JobID).Update("status", models.JobStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if err := downloads.db.Model(&models.DownloadTask{}).Where("id=?", original.ID).Update("phase", models.DownloadTaskStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	var paths []struct{ Name, File string }
	if err := downloads.db.Raw("PRAGMA database_list").Scan(&paths).Error; err != nil {
		t.Fatal(err)
	}
	path := ""
	for _, row := range paths {
		if row.Name == "main" {
			path = row.File
		}
	}
	if path == "" {
		t.Fatal("fixture must use a durable disposable database")
	}
	oldSQL, _ := downloads.db.DB()
	if err := oldSQL.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	newSQL, _ := reopened.DB()
	defer newSQL.Close()
	audit := NewAuditService(reopened)
	queue := NewQueueService(reopened, audit)
	restartedDownloaders := NewDownloaderService(reopened, audit, downloaders.credentials, registry)
	restarted := NewDownloadService(reopened, audit, downloads.credentials, restartedDownloaders, NewDownloadSettingsService(reopened, audit), queue, zerolog.Nop())
	currentActor, err := NewAuthorizationService(reopened).Resolve(actor.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.Control(currentActor, original.JobID, "retry", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var retried models.Job
	if err := reopened.First(&retried, "id=?", original.JobID).Error; err != nil || retried.Status != models.JobStatusQueued {
		t.Fatalf("original retry did not queue: status=%s err=%v", retried.Status, err)
	}
	worker := NewDownloadWorker(restarted)
	loaded, _, _, source, _, err := worker.load(context.Background(), ClaimedJob{Job: retried})
	if err != nil || loaded.ID != original.ID || source.Kind != downloadpkg.SourceTorrent || source.ContentSelection == nil || len(source.ContentSelection.Files) != 1 || source.ContentSelection.Files[0].Index != 1 {
		t.Fatalf("original selected task failed to reload: id=%s err=%v", loaded.ID, err)
	}
	plain, err := restarted.credentials.Decrypt(downloadSourcePurpose(original.ID), loaded.SourceCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	var frozen downloadSourceEnvelope
	if err := json.Unmarshal([]byte(plain), &frozen); err != nil {
		t.Fatal(err)
	}
	// Semantic corruption must still fail after a valid authenticated decrypt;
	// loss of the ephemeral admission proof cannot authorize whole-resource mode.
	frozen.ContentSelectionDigest = strings.Repeat("0", 64)
	corrupt, _ := json.Marshal(frozen)
	cipher, err := restarted.credentials.Encrypt(downloadSourcePurpose(original.ID), string(corrupt))
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Model(&models.DownloadTask{}).Where("id=?", original.ID).Update("source_ciphertext", cipher).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := worker.load(context.Background(), ClaimedJob{Job: retried}); ErrorCode(err) != CodeDownloadSourceInvalid {
		t.Fatalf("corrupt selection bypassed reload: %v", err)
	}
	var tasks, jobs int64
	if err := reopened.Model(&models.DownloadTask{}).Count(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if err := reopened.Model(&models.Job{}).Where("job_type=?", "download").Count(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || jobs != 1 || sourceReads != 1 || adapter.resolves != resolvedBeforeRetry || baseClient.submits != 0 {
		t.Fatalf("retry created/resolved new work: tasks=%d jobs=%d reads=%d submits=%d", tasks, jobs, sourceReads, baseClient.submits)
	}
}
