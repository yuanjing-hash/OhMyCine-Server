package services

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

func TestDownloadRecognitionOverrideSearchesByKeywordAndRetriesExistingProviderTask(t *testing.T) {
	requests := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.Path)
		switch request.URL.Path {
		case "/search/tv":
			if request.URL.Query().Get("query") != "Ming Dynasty in 1566" {
				t.Fatalf("query=%q", request.URL.Query().Get("query"))
			}
			_, _ = io.WriteString(writer, `{"results":[{"id":100,"name":"大明王朝1566","original_name":"Ming Dynasty in 1566","original_language":"zh","first_air_date":"2007-01-08"}]}`)
		case "/tv/100":
			_, _ = io.WriteString(writer, `{"id":100,"name":"大明王朝1566","original_name":"Ming Dynasty in 1566","original_language":"zh","first_air_date":"2007-01-08","genres":[{"id":18,"name":"剧情"}],"origin_country":["CN"]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	downloads, _, queue, actor, provider := downloadFixture(t)
	metadata := NewMetadataSettingsService(queue.db, queue.audit, downloads.credentials, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "test-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("test-token", server.URL, server.Client())
	}
	downloads.SetMetadataSettings(metadata)
	var profile models.MediaClassificationProfile
	if err := queue.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	connection := models.Connection{Name: "Recognition recovery", NameNormalized: "recognition-recovery", Provider: cloudpkg.ProviderPan115, CredentialCiphertext: "encrypted", Enabled: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "Recognition recovery", NameNormalized: "recognition-recovery", Type: models.StorageTypePan115, RootPath: "recognition-storage-root", RootPathNormalized: "pan115:recognition-recovery", ConnectionID: &connection.ID, Enabled: true, Capabilities: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Recognition recovery", NameNormalized: "recognition-recovery-library", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", ProviderRootID: "recognition-library-root", TransferMode: models.MediaLibraryTransferMove, ConflictPolicy: models.MediaLibraryConflictAsk, MovieDirectoryTemplate: "{category}/{title} ({year})", MovieFilenameTemplate: "{title} ({year})", TVDirectoryTemplate: "{category}/{title} ({year})/Season {season:02}", TVFilenameTemplate: "{title} - S{season:02}E{episode:02}", Enabled: false, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	releaseName := `[jibaketa合成&音频压制][ViuTV粤语]超人 / 超人力霸王奥米加 / 奥美迦奥特曼 / Ultraman Omega - 09 [粤语+无字幕] (WEB 1920x1080 AVC AAC YUE)`
	manifest := downloadpkg.Manifest{Name: releaseName, Complete: true, Files: []downloadpkg.File{{RelativePath: releaseName + ".mkv", Size: 2 * 1024 * 1024 * 1024}}}
	completedManifest, err := encodeCompletedDownloadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := json.Marshal(classification.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	taskID := "download-recognition-recovery"
	job, err := queue.EnqueueWith(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "download", DisplayName: "大明王朝", Provider: models.DownloaderTypePan115Offline, ResourceKey: "provider:115", Payload: downloadJobPayload{DownloadTaskID: taskID, ResourceAccessVersion: 1}}, func(tx *gorm.DB, queued models.Job) error {
		return tx.Create(&models.DownloadTask{
			ID: taskID, OwnerID: actor.User.ID, JobID: queued.ID, DownloaderName: "115", ProviderType: models.DownloaderTypePan115Offline,
			ProviderTaskID: "completed-provider-task", SourceCiphertext: "encrypted", DisplayName: releaseName,
			Phase: models.DownloadTaskStatusFailed, ScrapeStatus: "completed_unrecognized", ScrapeTitle: "Ming Dynasty in 1566", ScrapeCategory: "未识别", StagingCategory: "未识别",
			ProfileID: profile.ID, ProfileRevision: profile.Revision, ProfileRulesJSON: string(rules), ProfileRecognitionRulesJSON: "[]", ProfileBuiltinRecognitionPacksJSON: "[]",
			TargetLibraryID: &library.ID, TargetLibraryName: library.Name, TargetStorageID: &storage.ID, TargetStorageType: models.StorageTypePan115, TargetConnectionID: &connection.ID, TargetProviderRootID: library.ProviderRootID, TargetStorageRoot: storage.RootPath, TargetRelativeRoot: "/", TransferMode: library.TransferMode, ConflictPolicy: library.ConflictPolicy,
			MovieDirectoryTemplate: library.MovieDirectoryTemplate, MovieFilenameTemplate: library.MovieFilenameTemplate, TVDirectoryTemplate: library.TVDirectoryTemplate, TVFilenameTemplate: library.TVFilenameTemplate,
			CompletedManifestJSON: completedManifest,
			CreatedAt:             time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", job.ID).Updates(map[string]any{"status": models.JobStatusFailed, "last_error_code": CodeTransferMediaUnrecognized}).Error; err != nil {
		t.Fatal(err)
	}
	deniedReader := Actor{User: models.User{ID: actor.User.ID + 1}, Permissions: map[string]struct{}{authz.PermissionDownloadsReadOwn: {}}}
	if _, err := downloads.RecognitionCandidates(context.Background(), deniedReader, taskID, "Ming Dynasty in 1566", "tv", nil); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("foreign candidate search error=%v code=%s", err, ErrorCode(err))
	}
	deniedController := Actor{User: models.User{ID: actor.User.ID + 1}, Permissions: map[string]struct{}{authz.PermissionJobsControlOwn: {}}}
	if _, err := downloads.OverrideRecognition(context.Background(), deniedController, taskID, DownloadRecognitionOverrideInput{TMDBID: 100, MediaType: "tv"}, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("foreign override error=%v code=%s", err, ErrorCode(err))
	}

	candidates, err := downloads.RecognitionCandidates(context.Background(), actor, taskID, "Ming Dynasty in 1566", "tv", nil)
	if err != nil || len(candidates) != 1 || candidates[0].ID != 100 || candidates[0].OriginalTitle != "Ming Dynasty in 1566" {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	result, err := downloads.OverrideRecognition(context.Background(), actor, taskID, DownloadRecognitionOverrideInput{TMDBID: 100, MediaType: "tv"}, RequestContext{})
	if err != nil || result.JobStatus != models.JobStatusQueued {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var persisted models.DownloadTask
	if err := queue.db.First(&persisted, "id = ?", taskID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.RecognitionOverrideTMDBID == nil || *persisted.RecognitionOverrideTMDBID != 100 || persisted.RecognitionOverrideMediaType != "tv" || persisted.ProviderTaskID != "completed-provider-task" {
		t.Fatalf("persisted=%+v", persisted)
	}
	if !persisted.IdentityLocked || persisted.IdentitySource != mediaIdentitySourceManual || persisted.IdentityStatus != mediaIdentityStatusVerified || persisted.IdentityRevision != 1 || !strings.Contains(persisted.IdentitySnapshotJSON, `"tmdb_id":100`) {
		t.Fatalf("manual identity snapshot was not locked: %+v", persisted)
	}
	provider.mu.Lock()
	submits := provider.submits
	provider.mu.Unlock()
	if submits != 0 {
		t.Fatalf("manual recovery resubmitted provider task %d time(s)", submits)
	}
	if len(requests) != 2 || requests[0] != "/search/tv" || requests[1] != "/tv/100" {
		t.Fatalf("requests=%v", requests)
	}
	transfers := NewTransferService(queue.db, queue.audit, queue, zerolog.Nop())
	downloads.SetTransferService(transfers)
	claimed, err := queue.Claim([]string{"download"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	worker := NewDownloadWorker(downloads)
	workerResult := worker.Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if workerResult.ErrorCode != "" || workerResult.RetryAt != nil || workerResult.Wait != nil {
		t.Fatalf("worker result=%+v", workerResult)
	}
	if err := queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	clientSnapshot := func() (gets, submits int, paused, resumed bool) {
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return provider.gets, provider.submits, provider.paused, provider.resumed
	}
	gets, submits, paused, resumed := clientSnapshot()
	if gets != 0 || submits != 0 || paused || resumed {
		t.Fatalf("completed recovery touched downloader: gets=%d submits=%d paused=%v resumed=%v", gets, submits, paused, resumed)
	}
	if err := queue.db.First(&persisted, "id = ?", taskID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != models.DownloadTaskStatusCompleted || persisted.ScrapeStatus != "completed_verified" || persisted.StagingCategory != "未识别" {
		t.Fatalf("recovered task=%+v", persisted)
	}
	summary := downloadTaskSummary(persisted, models.JobStatusCompleted)
	if summary.ScrapeSeason == nil || *summary.ScrapeSeason != 1 || summary.ScrapeEpisode == nil || *summary.ScrapeEpisode != 9 {
		t.Fatalf("episode summary=%+v", summary)
	}
	var transferTasks []models.TransferTask
	if err := queue.db.Where("download_task_id = ?", taskID).Find(&transferTasks).Error; err != nil || len(transferTasks) != 1 {
		t.Fatalf("transfer tasks=%+v err=%v", transferTasks, err)
	}
	var sourceManifest downloadpkg.Manifest
	if err := json.Unmarshal([]byte(transferTasks[0].SourceManifestJSON), &sourceManifest); err != nil || len(sourceManifest.Files) != 1 || sourceManifest.Files[0].RelativePath != manifest.Files[0].RelativePath {
		t.Fatalf("source manifest=%+v err=%v", sourceManifest, err)
	}
	if _, err := downloads.OverrideRecognition(context.Background(), actor, taskID, DownloadRecognitionOverrideInput{TMDBID: 100, MediaType: "tv"}, RequestContext{}); ErrorCode(err) != CodeQueueStateConflict {
		t.Fatalf("duplicate override error=%v code=%s", err, ErrorCode(err))
	}
	if _, err := downloads.RecognitionCandidates(context.Background(), actor, taskID, "Ming Dynasty in 1566", "tv", nil); ErrorCode(err) != CodeQueueStateConflict {
		t.Fatalf("queued recovery candidate search error=%v code=%s", err, ErrorCode(err))
	}
	if len(requests) != 3 || requests[2] != "/tv/100" {
		t.Fatalf("rejected duplicate recovery called TMDB: requests=%v", requests)
	}
}

func TestCompletedRecognitionRecoveryBackfillsLegacyManifestOnlyOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/tv/100" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.WriteString(writer, `{"id":100,"name":"示例剧","original_name":"Example Show","original_language":"zh","first_air_date":"2024-01-01","genres":[{"id":18,"name":"剧情"}],"origin_country":["CN"]}`)
	}))
	defer server.Close()

	downloads, downloaders, queue, actor, _ := downloadFixture(t)
	client := &metadataDownloadClient{stubDownloadClient: &stubDownloadClient{}}
	if err := downloaders.registry.Register(models.DownloaderTypeFake, downloadpkg.Capabilities{Pause: true, Resume: true}, func(downloadpkg.Config) (downloadpkg.Client, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	metadata := NewMetadataSettingsService(queue.db, queue.audit, downloads.credentials, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "test-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("test-token", server.URL, server.Client())
	}
	downloads.SetMetadataSettings(metadata)
	downloads.SetTransferService(NewTransferService(queue.db, queue.audit, queue, zerolog.Nop()))

	var profile models.MediaClassificationProfile
	if err := queue.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	stagingRoot, targetRoot := t.TempDir(), t.TempDir()
	stagingStorage := models.Storage{Name: "Legacy staging", NameNormalized: strings.ToLower("legacy staging" + stagingRoot), Type: models.StorageTypeLocal, RootPath: stagingRoot, RootPathNormalized: strings.ToLower(stagingRoot), Enabled: true, Capabilities: `{}`}
	targetStorage := models.Storage{Name: "Legacy target", NameNormalized: strings.ToLower("legacy target" + targetRoot), Type: models.StorageTypeLocal, RootPath: targetRoot, RootPathNormalized: strings.ToLower(targetRoot), Enabled: true, Capabilities: `{}`}
	if err := queue.db.Create(&stagingStorage).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Create(&targetStorage).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, queue, stagingStorage.ID)
	library := models.MediaLibrary{Name: "Legacy library", NameNormalized: strings.ToLower("legacy library" + targetRoot), StorageID: targetStorage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", SortOrder: 1, TransferMode: models.MediaLibraryTransferMove, ConflictPolicy: models.MediaLibraryConflictAsk, MovieDirectoryTemplate: "{category}/{title} ({year})", MovieFilenameTemplate: "{title} ({year})", TVDirectoryTemplate: "{category}/{title} ({year})/Season {season:02}", TVFilenameTemplate: "{title} - S{season:02}E{episode:02}", Enabled: true, Recursive: true, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`}
	if err := queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	provider, err := downloaders.Create(actor, DownloaderInput{Name: "Legacy metadata provider", Type: models.DownloaderTypeFake, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	created, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, MediaLibraryID: &library.ID, DisplayName: "Example Show", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:legacy-manifest"}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.DownloadTask{}).Where("id = ?", created.ID).Updates(map[string]any{
		"provider_task_id": "completed-provider-task", "phase": models.DownloadTaskStatusFailed, "scrape_status": "completed_unrecognized", "scrape_category": "未识别", "staging_category": "未识别", "completed_manifest_json": "{}", "source_ciphertext": "legacy-source-no-longer-readable",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id = ?", created.JobID).Updates(map[string]any{"status": models.JobStatusFailed, "last_error_code": CodeTransferMediaUnrecognized}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := downloads.OverrideRecognition(context.Background(), actor, created.ID, DownloadRecognitionOverrideInput{TMDBID: 100, MediaType: "tv"}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	claimed, err := queue.Claim([]string{"download"})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	workerResult := NewDownloadWorker(downloads).Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed)
	if workerResult.ErrorCode != "" || workerResult.RetryAt != nil || workerResult.Wait != nil {
		t.Fatalf("worker result=%+v", workerResult)
	}
	client.mu.Lock()
	manifestCalls, gets, submits := client.manifestCalls, client.gets, client.submits
	client.mu.Unlock()
	if manifestCalls != 1 || gets != 0 || submits != 0 {
		t.Fatalf("provider calls: manifest=%d get=%d submit=%d", manifestCalls, gets, submits)
	}
	var persisted models.DownloadTask
	if err := queue.db.First(&persisted, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.CompletedManifestJSON == "" || persisted.CompletedManifestJSON == "{}" || persisted.ScrapeStatus != "completed_verified" {
		t.Fatalf("persisted=%+v", persisted)
	}
	// Simulate a later recognition-stage retry. The durable snapshot, rather
	// than another provider call, remains authoritative after the first legacy
	// backfill.
	if err := queue.db.Model(&persisted).Updates(map[string]any{"phase": models.DownloadTaskStatusFailed, "scrape_status": "completed_unrecognized"}).Error; err != nil {
		t.Fatal(err)
	}
	if rerun := NewDownloadWorker(downloads).Run(context.Background(), workerRuntime{queue: queue, job: *claimed}, *claimed); rerun.ErrorCode != "" {
		t.Fatalf("idempotent rerun=%+v", rerun)
	}
	client.mu.Lock()
	manifestCalls = client.manifestCalls
	client.mu.Unlock()
	if manifestCalls != 1 {
		t.Fatalf("persisted manifest was fetched again: calls=%d", manifestCalls)
	}
	if err := queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&library).Updates(map[string]any{"baseline_generation": 1, "status": models.MediaLibraryStatusListening}).Error; err != nil {
		t.Fatal(err)
	}
	// A completed provider with an unreadable output remains the same original
	// acquisition. Recover the failed file-read stage without preclassification.
	accepted, err := downloads.Submit(context.Background(), actor, SubmitDownloadInput{DownloaderID: provider.ID, MediaLibraryID: &library.ID, DisplayName: "Example Show", Source: DownloadSourceInput{Kind: downloadpkg.SourceURL, URL: "magnet:?xt=urn:btih:completion-output"}, RecognitionOverride: &DownloadRecognitionIdentity{TMDBID: 100, MediaType: "tv", Source: mediaIdentitySourceDirectID, Status: mediaIdentityStatusVerified}}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	subscription := models.FollowSubscription{ID: "completed-original-follow", OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 100, Title: "Example Show", Status: models.FollowStatusActive, Revision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: `{}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("a", 64)
	if err := queue.db.Model(&models.DownloadTask{}).Where("id=?", accepted.ID).Updates(map[string]any{"provider_task_id": "provider-hash", "phase": models.DownloadTaskStatusDownloading, "scrape_status": "classified", "scrape_category": "未识别", "staging_category": "未识别", "follow_subscription_id": subscription.ID, "follow_resource_fingerprint": fingerprint}).Error; err != nil {
		t.Fatal(err)
	}
	episode := models.FollowEpisodeClaim{SubscriptionID: subscription.ID, SeasonNumber: 1, EpisodeNumber: 1, State: "downloading", DownloadTaskID: &accepted.ID, ResourceFingerprint: fingerprint, UpdatedAt: time.Now().UTC()}
	if err := queue.db.Create(&episode).Error; err != nil {
		t.Fatal(err)
	}
	client.manifestErr = downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, nil)
	client.seedTask = &downloadpkg.Task{ID: "provider-hash", Status: "stalledUP", Completed: true}
	original, err := queue.Claim([]string{"download"})
	if err != nil || original == nil {
		t.Fatalf("claim=%+v %v", original, err)
	}
	worker := NewDownloadWorker(downloads)
	failed := worker.Run(context.Background(), workerRuntime{queue: queue, job: *original}, *original)
	if failed.ErrorCode != nodeprotocol.ErrorPathMappingInvalid || !strings.Contains(failed.ErrorMessage, "节点") {
		t.Fatalf("completion error lost its stage: %+v", failed)
	}
	if err := queue.Fail(original.Job.ID, original.LeaseToken, failed.ErrorCode, failed.ErrorMessage); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&episode, "subscription_id=? AND season_number=1 AND episode_number=1", subscription.ID).Error; err != nil || episode.DownloadTaskID == nil || *episode.DownloadTaskID != accepted.ID {
		t.Fatalf("pending original lost reservation %+v %v", episode, err)
	}
	// A released old-build claim can be repaired only by this exact task's
	// retained fingerprint and durable provider-completed marker.
	if err := queue.db.Model(&episode).Updates(map[string]any{"state": "failed", "download_task_id": nil}).Error; err != nil {
		t.Fatal(err)
	}
	follows := NewFollowService(queue.db, queue.audit, queue, nil, nil)
	followWorker := NewFollowSearchWorker(follows, nil)
	newRun := models.FollowRun{ID: "completion-competing-run", SubscriptionID: subscription.ID, OwnerID: actor.User.ID, SubscriptionRevision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: `{}`, JobID: accepted.JobID, Trigger: "manual", Status: models.FollowRunRunning}
	if err := queue.db.Create(&newRun).Error; err != nil {
		t.Fatal(err)
	}
	reservedDuringRead := false
	const reserveCallback = "completion:concurrent_follow_reservation"
	if err := queue.db.Callback().Query().After("gorm:after_query").Register(reserveCallback, func(query *gorm.DB) {
		observed, ok := query.Statement.Dest.(*models.DownloadTask)
		if reservedDuringRead || !ok || observed.ID != accepted.ID {
			return
		}
		reservedDuringRead = true
		reserved, _, err := followWorker.reserveEpisodes(newRun, subscription.ID, followCandidate{Season: 1, Fingerprint: fingerprint}, []int{1})
		if err != nil || !reserved {
			t.Fatalf("concurrent run did not reserve the episode: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	missing, err := followWorker.excludeClaimedDownloads(context.Background(), subscription.ID, map[[2]int]struct{}{{1, 1}: {}, {1, 2}: {}})
	_ = queue.db.Callback().Query().Remove(reserveCallback)
	if err != nil || !reservedDuringRead || len(missing) != 2 {
		t.Fatalf("stale completion read consumed a newer reservation: missing=%v err=%v", missing, err)
	}
	if err := queue.db.First(&episode, "subscription_id=? AND season_number=1 AND episode_number=1", subscription.ID).Error; err != nil || episode.DownloadTaskID != nil || episode.RunID == nil || *episode.RunID != newRun.ID || episode.State != "queued" {
		t.Fatalf("new run reservation was overwritten: %+v %v", episode, err)
	}
	followWorker.releaseEpisodes(subscription.ID, newRun.ID, fingerprint)
	missing, err = followWorker.excludeClaimedDownloads(context.Background(), subscription.ID, map[[2]int]struct{}{{1, 1}: {}, {1, 2}: {}})
	if err != nil || len(missing) != 1 {
		t.Fatalf("pending completion became another download: missing=%v err=%v", missing, err)
	}
	if _, exists := missing[[2]int{1, 2}]; !exists {
		t.Fatal("unrelated missing episode was blocked")
	}
	if err := queue.db.First(&episode, "subscription_id=? AND season_number=1 AND episode_number=1", subscription.ID).Error; err != nil || episode.DownloadTaskID == nil || *episode.DownloadTaskID != accepted.ID {
		t.Fatalf("exact old claim not restored %+v %v", episode, err)
	}
	client.mu.Lock()
	client.manifestErr = nil
	beforeGet, beforeSubmit, beforeManifest, beforeCategories := client.getCalls, client.submits, client.manifestCalls, len(client.categoryCalls)
	client.mu.Unlock()
	if _, err := queue.Control(actor, accepted.JobID, "retry", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	resumed, err := queue.Claim([]string{"download"})
	if err != nil || resumed == nil {
		t.Fatalf("recovery claim=%+v %v", resumed, err)
	}
	const transferCallback = "completion:transfer_persistence_failure"
	if err := queue.db.Callback().Create().Before("gorm:create").Register(transferCallback, func(create *gorm.DB) {
		if create.Statement.Table == "transfer_tasks" {
			create.AddError(appError("fixture_transfer_persistence_failed", "fixture", nil))
		}
	}); err != nil {
		t.Fatal(err)
	}
	transferFailure := NewDownloadWorker(downloads).Run(context.Background(), workerRuntime{queue: queue, job: *resumed}, *resumed)
	_ = queue.db.Callback().Create().Remove(transferCallback)
	if transferFailure.ErrorCode == "" {
		t.Fatal("transfer persistence failure was accepted")
	}
	if err := queue.Fail(resumed.Job.ID, resumed.LeaseToken, transferFailure.ErrorCode, transferFailure.ErrorMessage); err != nil {
		t.Fatal(err)
	}
	var verified models.DownloadTask
	if err := queue.db.First(&verified, "id=?", accepted.ID).Error; err != nil || verified.ScrapeStatus != "completed_verified" || !downloadCompletionPending(verified) {
		t.Fatalf("verified original lost its pending completion: %+v %v", verified, err)
	}
	if err := queue.db.First(&episode, "subscription_id=? AND season_number=1 AND episode_number=1", subscription.ID).Error; err != nil || episode.DownloadTaskID == nil || *episode.DownloadTaskID != accepted.ID {
		t.Fatalf("failed transfer released completed files: %+v %v", episode, err)
	}
	if _, err := queue.Control(actor, accepted.JobID, "retry", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	resumed, err = queue.Claim([]string{"download"})
	if err != nil || resumed == nil {
		t.Fatalf("verified recovery claim=%+v %v", resumed, err)
	}
	recovered := NewDownloadWorker(downloads).Run(context.Background(), workerRuntime{queue: queue, job: *resumed}, *resumed)
	if recovered.ErrorCode != "" || recovered.RetryAt != nil {
		t.Fatalf("recovery=%+v", recovered)
	}
	client.mu.Lock()
	afterGet, afterSubmit, afterManifest, afterCategories := client.getCalls, client.submits, client.manifestCalls, len(client.categoryCalls)
	client.mu.Unlock()
	if afterGet != beforeGet || afterSubmit != beforeSubmit || afterManifest != beforeManifest+1 || afterCategories != beforeCategories {
		t.Fatalf("completion replayed provider workflow: gets %d/%d submits %d/%d manifests %d/%d categories %d/%d", beforeGet, afterGet, beforeSubmit, afterSubmit, beforeManifest, afterManifest, beforeCategories, afterCategories)
	}
	var completed models.DownloadTask
	if err := queue.db.First(&completed, "id=?", accepted.ID).Error; err != nil || completed.ProviderTaskID != "provider-hash" || completed.ScrapeStatus != "completed_verified" || completed.CompletedManifestJSON == "" {
		t.Fatalf("completed original=%+v %v", completed, err)
	}
	var transfer models.TransferTask
	if err := queue.db.First(&transfer, "download_task_id=?", accepted.ID).Error; err != nil {
		t.Fatal("normal transfer handoff absent", err)
	}
	// A cancellation can settle after the task read but before lost-attachment
	// recovery. Its completed file snapshot cannot revive the cancelled task.
	if err := queue.db.Model(&episode).Updates(map[string]any{"state": "queued", "download_task_id": nil}).Error; err != nil {
		t.Fatal(err)
	}
	cancelledDuringRead := false
	const cancelCallback = "completion:cancel_before_follow_attachment"
	if err := queue.db.Callback().Query().After("gorm:after_query").Register(cancelCallback, func(query *gorm.DB) {
		observed, ok := query.Statement.Dest.(*models.DownloadTask)
		if cancelledDuringRead || !ok || observed.ID != accepted.ID {
			return
		}
		cancelledDuringRead = true
		if err := downloads.CancelPipeline(context.Background(), actor, accepted.ID, RequestContext{}); err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	missing, err = followWorker.excludeClaimedDownloads(context.Background(), subscription.ID, map[[2]int]struct{}{{1, 1}: {}})
	_ = queue.db.Callback().Query().Remove(cancelCallback)
	if err != nil || !cancelledDuringRead || len(missing) != 1 {
		t.Fatalf("cancelled completion was restored from a stale read: missing=%v err=%v", missing, err)
	}
	if err := queue.db.First(&episode, "subscription_id=? AND season_number=1 AND episode_number=1", subscription.ID).Error; err != nil || episode.DownloadTaskID != nil {
		t.Fatalf("cancelled task regained its episode: %+v %v", episode, err)
	}

}

func TestValidateCompletedManifestEpisodeOverrideRequiresExactlyOneVideo(t *testing.T) {
	episode := 9
	if err := validateCompletedManifestEpisodeOverride("{}", &episode); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("legacy manifest err=%v", err)
	}
	manifest := downloadpkg.Manifest{Name: "series", Complete: true, Files: []downloadpkg.File{
		{RelativePath: "series/episode-1.mkv", Size: 100},
		{RelativePath: "series/episode-2.mkv", Size: 100},
	}}
	raw, err := encodeCompletedDownloadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCompletedManifestEpisodeOverride(raw, &episode); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("multi-video manifest err=%v", err)
	}
	if err := validateCompletedManifestEpisodeOverride(raw, nil); err != nil {
		t.Fatalf("season-only correction should remain safe: %v", err)
	}
}
