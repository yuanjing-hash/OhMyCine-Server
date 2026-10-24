package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

type followCompletedDownloadClient struct {
	*stubDownloadClient
	manifest      downloadpkg.Manifest
	manifestCalls int
}

func (c *followCompletedDownloadClient) Manifest(context.Context, string) (downloadpkg.Manifest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manifestCalls++
	return c.manifest, nil
}

type legacyFollowIdentityFixture struct {
	downloads *DownloadService
	queue     *QueueService
	actor     Actor
	provider  *stubDownloadClient
	task      models.DownloadTask
	manifest  downloadpkg.Manifest
	metadata  func()
}

func newLegacyFollowIdentityFixture(t *testing.T) *legacyFollowIdentityFixture {
	t.Helper()
	f := &legacyFollowIdentityFixture{}
	f.downloads, _, f.queue, f.actor, f.provider = downloadFixture(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/tv/100" && request.URL.Path != "/tv/101" {
			t.Errorf("completion must revalidate the frozen ID, not fuzzy search: %s", request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		if f.metadata != nil {
			f.metadata()
		}
		if request.URL.Path == "/tv/101" {
			_, _ = writer.Write([]byte(`{"id":101,"name":"另一部剧","original_name":"Corrected Show","original_language":"zh","first_air_date":"2025-01-01","genres":[{"id":18,"name":"剧情"}],"origin_country":["CN"]}`))
			return
		}
		_, _ = writer.Write([]byte(`{"id":100,"name":"示例剧","original_name":"Fixture Show","original_language":"zh","first_air_date":"2024-01-01","genres":[{"id":18,"name":"剧情"}],"origin_country":["CN"]}`))
	}))
	t.Cleanup(upstream.Close)
	metadata := NewMetadataSettingsService(f.queue.db, f.queue.audit, f.downloads.credentials, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "fixture-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("fixture-token", upstream.URL, upstream.Client())
	}
	f.downloads.SetMetadataSettings(metadata)
	f.downloads.SetTransferService(NewTransferService(f.queue.db, f.queue.audit, f.queue, zerolog.Nop()))
	var profile models.MediaClassificationProfile
	if err := f.queue.db.First(&profile, "code = ?", "default-v1").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	storage := models.Storage{Name: "Follow identity", NameNormalized: "follow-identity", Type: models.StorageTypePan115, RootPath: "fixture-storage", RootPathNormalized: "pan115:fixture-storage", Enabled: true, Capabilities: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := f.queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Follow identity", NameNormalized: "follow-identity-library", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", ProviderRootID: "fixture-library", TransferMode: models.MediaLibraryTransferMove, ConflictPolicy: models.MediaLibraryConflictAsk, Enabled: true, BaselineGeneration: 1, Status: models.MediaLibraryStatusListening, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`}
	if err := f.queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.FollowSubscription{ID: "original-follow", OwnerID: f.actor.User.ID, MediaType: "tv", TMDBID: 100, Title: "示例剧", Status: models.FollowStatusPaused, Revision: 8, LifecycleRevision: 9, ExecutionSnapshotJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := f.queue.db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	frozen, _ := json.Marshal(FollowExecutionSnapshot{Version: 2, RoutingPolicy: "source_priority", Seasons: []int{1}, SiteIDs: []uint{1}, MediaLibraryID: library.ID, Schedule: FollowSchedule{Kind: "interval", Minutes: 360}, MaxResourcesPerRun: 3})
	followJob, err := f.queue.Enqueue(EnqueueJobInput{OwnerID: f.actor.User.ID, JobType: JobTypeFollowSearch, DisplayName: "Original subscription", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	run := models.FollowRun{ID: "original-run", SubscriptionID: subscription.ID, OwnerID: f.actor.User.ID, SubscriptionRevision: 1, LifecycleRevision: 1, ExecutionSnapshotJSON: string(frozen), JobID: followJob.ID, Status: models.FollowRunSubmitted, Trigger: "scheduled", MissingSnapshotJSON: `[{"season":1,"episode":11}]`, FilterSummaryJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := f.queue.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	rules, _ := json.Marshal(classification.DefaultRules())
	tmdbID, season, episode, year, confidence := int64(100), 1, 11, 2024, 1.0
	f.manifest = downloadpkg.Manifest{Name: "Fixture.Show.S01E11", Complete: true, Files: []downloadpkg.File{{RelativePath: "Fixture.Show.S01E11.mkv", Size: 2 * 1024 * 1024 * 1024, ProviderItemID: "original-file"}}}
	completed, err := encodeCompletedDownloadManifest(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	f.task = models.DownloadTask{ID: "original-download", OwnerID: f.actor.User.ID, DownloaderName: "115", ProviderType: models.DownloaderTypePan115Offline, ProviderTaskID: "original-completed-task", SourceCiphertext: "unreadable-original-source", DisplayName: f.manifest.Name, Phase: models.DownloadTaskStatusFailed,
		ProfileID: profile.ID, ProfileRevision: profile.Revision, ProfileRulesJSON: string(rules), ProfileRecognitionRulesJSON: `[]`, ProfileBuiltinRecognitionPacksJSON: `[]`,
		TargetLibraryID: &library.ID, TargetLibraryName: library.Name, TargetStorageID: &storage.ID, TargetStorageType: storage.Type, TargetProviderRootID: library.ProviderRootID, TargetStorageRoot: storage.RootPath, TargetRelativeRoot: "/", TransferMode: library.TransferMode, ConflictPolicy: library.ConflictPolicy,
		ScrapeStatus: "completed_unrecognized", ScrapeTMDBID: &tmdbID, ScrapeMediaType: "tv", ScrapeTitle: "示例剧", ScrapeCategory: "国产剧", ScrapeYear: &year, ScrapeConfidence: &confidence, ScrapeSeason: &season, ScrapeEpisode: &episode,
		RecognitionOverrideTMDBID: &tmdbID, RecognitionOverrideMediaType: "tv", RecognitionOverrideSeason: &season, RecognitionOverrideEpisode: &episode,
		IdentitySource: mediaIdentitySourceDirectID, IdentityStatus: mediaIdentityStatusVerified, IdentityLocked: true, IdentityRevision: 1,
		FollowSubscriptionID: subscription.ID, FollowResourceFingerprint: strings.Repeat("a", 64), CompletedManifestJSON: completed, LastErrorCode: CodeTransferMediaUnrecognized, CreatedAt: now, UpdatedAt: now}
	_, raw, err := buildDownloadIdentitySnapshot(f.task, scrapeMatch{Title: f.task.ScrapeTitle, TMDBID: f.task.ScrapeTMDBID, MediaType: f.task.ScrapeMediaType, Category: f.task.ScrapeCategory, Year: f.task.ScrapeYear, Confidence: f.task.ScrapeConfidence, Season: f.task.ScrapeSeason, Episode: f.task.ScrapeEpisode}, f.manifest, f.task.IdentitySource, f.task.IdentityStatus, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.task.IdentitySnapshotJSON = raw
	job, err := f.queue.EnqueueWith(EnqueueJobInput{OwnerID: f.actor.User.ID, JobType: "download", DisplayName: f.task.DisplayName, Provider: models.DownloaderTypePan115Offline, Payload: downloadJobPayload{DownloadTaskID: f.task.ID, ResourceAccessVersion: 1}}, func(tx *gorm.DB, job models.Job) error {
		f.task.JobID = job.ID
		return tx.Create(&f.task).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Model(&models.Job{}).Where("id = ?", job.ID).Updates(map[string]any{"status": models.JobStatusFailed, "last_error_code": CodeTransferMediaUnrecognized}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Create(&models.FollowEpisodeClaim{SubscriptionID: subscription.ID, SeasonNumber: season, EpisodeNumber: episode, State: "downloading", RunID: &run.ID, DownloadTaskID: &f.task.ID, ResourceFingerprint: f.task.FollowResourceFingerprint, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *legacyFollowIdentityFixture) retry(t *testing.T) (WorkerResult, ClaimedJob) {
	t.Helper()
	if _, err := f.queue.Control(f.actor, f.task.JobID, "retry", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	claim, err := f.queue.Claim([]string{"download"})
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	// Fresh worker/service value models a restart; it has no in-memory repair
	// result or original share/source. The durable complete package is enough.
	restarted := *f.downloads
	result := NewDownloadWorker(&restarted).Run(context.Background(), workerRuntime{queue: f.queue, job: *claim}, *claim)
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	if f.provider.submits != 0 || f.provider.getCalls != 0 || f.provider.paused || f.provider.resumed || f.provider.cancelled {
		t.Fatalf("completed retry touched provider: submit=%d get=%d", f.provider.submits, f.provider.getCalls)
	}
	return result, *claim
}

func TestCompletedLegacyFollowIdentityRecoversWithoutRedownload(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	// A later Follow pass can release a failed task's claim attachment while
	// retaining the original run/fingerprint. This is still the original intent.
	if err := f.queue.db.Model(&models.FollowEpisodeClaim{}).Where("subscription_id = ?", f.task.FollowSubscriptionID).Updates(map[string]any{"state": "failed", "download_task_id": nil}).Error; err != nil {
		t.Fatal(err)
	}
	original := f.task
	for attempt := 0; attempt < 2; attempt++ {
		if attempt != 0 {
			if err := f.queue.db.Model(&models.DownloadTask{}).Where("id = ?", f.task.ID).Updates(map[string]any{"phase": models.DownloadTaskStatusFailed, "scrape_status": "completed_unrecognized"}).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.queue.db.Model(&models.Job{}).Where("id = ?", f.task.JobID).Update("status", models.JobStatusFailed).Error; err != nil {
				t.Fatal(err)
			}
		}
		result, claim := f.retry(t)
		if result.ErrorCode != "" || result.Wait != nil || result.RetryAt != nil {
			t.Fatalf("legacy completion recovery failed: %+v", result)
		}
		if err := f.queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
			t.Fatal(err)
		}
		var current models.DownloadTask
		if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil {
			t.Fatal(err)
		}
		identity, err := decodeMediaIdentity(current.IdentitySnapshotJSON)
		if err != nil || identity.Locked || current.IdentityLocked || current.IdentitySource != mediaIdentitySourceDirectID || current.IdentityRevision != 1 || current.IdentityStatus != mediaIdentityStatusVerified || current.ScrapeStatus != "completed_verified" || identity.TMDBID == nil || *identity.TMDBID != 100 || identity.Season == nil || *identity.Season != 1 || identity.Episode == nil || *identity.Episode != 11 {
			t.Fatalf("recovered identity changed work/revision/provenance: %+v %v", identity, err)
		}
		if current.CompletedManifestJSON != original.CompletedManifestJSON || current.ProviderTaskID != original.ProviderTaskID || current.SourceCiphertext != original.SourceCiphertext || current.SourceDataSourceJSON != original.SourceDataSourceJSON || current.TargetDataSourceJSON != original.TargetDataSourceJSON || !sameOptional(current.TargetLibraryID, original.TargetLibraryID) {
			t.Fatal("completed retry replaced the original package/provider/route")
		}
		var count int64
		if err := f.queue.db.Model(&models.TransferTask{}).Where("download_task_id = ?", f.task.ID).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("duplicate/lost transfer: count=%d err=%v", count, err)
		}
	}
}

func TestCompletedLegacyFollowIdentityRejectsUnprovenCorrection(t *testing.T) {
	cases := map[string]func(*legacyFollowIdentityFixture){
		"missing_snapshot": func(f *legacyFollowIdentityFixture) { f.task.IdentitySnapshotJSON = `{}` },
		"invalid_snapshot": func(f *legacyFollowIdentityFixture) { f.task.IdentitySnapshotJSON += ` {}` },
		"newer_revision":   func(f *legacyFollowIdentityFixture) { f.task.IdentityRevision = 2 },
		"owner_drift": func(f *legacyFollowIdentityFixture) {
			other := models.User{Username: "other", UsernameNormalized: "other", DisplayName: "Other", PasswordHash: "x", Status: models.UserStatusActive}
			if err := f.queue.db.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.queue.db.Model(&models.FollowSubscription{}).Where("id = ?", f.task.FollowSubscriptionID).Update("owner_id", other.ID).Error; err != nil {
				t.Fatal(err)
			}
		},
		"fingerprint_drift": func(f *legacyFollowIdentityFixture) { f.task.FollowResourceFingerprint = strings.Repeat("b", 64) },
		"legacy_id_drift":   func(f *legacyFollowIdentityFixture) { value := int64(101); f.task.RecognitionOverrideTMDBID = &value },
		"season_drift":      func(f *legacyFollowIdentityFixture) { value := 2; f.task.RecognitionOverrideSeason = &value },
		"episode_drift":     func(f *legacyFollowIdentityFixture) { value := 12; f.task.RecognitionOverrideEpisode = &value },
		"snapshot_unlock_mismatch": func(f *legacyFollowIdentityFixture) {
			f.task.IdentitySnapshotJSON = strings.Replace(f.task.IdentitySnapshotJSON, `"locked":true`, `"locked":false`, 1)
		},
		"forged_file_episode": func(f *legacyFollowIdentityFixture) {
			identity, _ := decodeMediaIdentity(f.task.IdentitySnapshotJSON)
			episode := 12
			identity.Episodes[0].Episode = &episode
			raw, _ := json.Marshal(identity)
			f.task.IdentitySnapshotJSON = string(raw)
		},
		"unknown_run_version": func(f *legacyFollowIdentityFixture) {
			if err := f.queue.db.Model(&models.FollowRun{}).Where("id = ?", "original-run").Update("execution_snapshot_json", `{"version":99,"routing_policy":"source_priority","media_library_id":1,"seasons":[1]}`).Error; err != nil {
				t.Fatal(err)
			}
		},
		"unknown_run_policy": func(f *legacyFollowIdentityFixture) {
			var run models.FollowRun
			if err := f.queue.db.First(&run, "id = ?", "original-run").Error; err != nil {
				t.Fatal(err)
			}
			raw := strings.Replace(run.ExecutionSnapshotJSON, "source_priority", "unknown_policy", 1)
			if err := f.queue.db.Model(&run).Update("execution_snapshot_json", raw).Error; err != nil {
				t.Fatal(err)
			}
		},
		"run_library_drift": func(f *legacyFollowIdentityFixture) {
			var run models.FollowRun
			_ = f.queue.db.First(&run, "id = ?", "original-run").Error
			var snapshot FollowExecutionSnapshot
			_ = json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot)
			snapshot.MediaLibraryID++
			raw, _ := json.Marshal(snapshot)
			if err := f.queue.db.Model(&run).Update("execution_snapshot_json", string(raw)).Error; err != nil {
				t.Fatal(err)
			}
		},
		"run_season_drift": func(f *legacyFollowIdentityFixture) {
			var run models.FollowRun
			_ = f.queue.db.First(&run, "id = ?", "original-run").Error
			raw := strings.Replace(run.ExecutionSnapshotJSON, `"seasons":[1]`, `"seasons":[2]`, 1)
			if err := f.queue.db.Model(&run).Update("execution_snapshot_json", raw).Error; err != nil {
				t.Fatal(err)
			}
		},
		"no_follow": func(f *legacyFollowIdentityFixture) { f.task.FollowSubscriptionID = "" },
		"snapshot_projection_drift": func(f *legacyFollowIdentityFixture) {
			f.task.IdentitySnapshotJSON = strings.Replace(f.task.IdentitySnapshotJSON, "示例剧", "Other show", 1)
		},
		"claim_taken_by_other_task": func(f *legacyFollowIdentityFixture) {
			other := f.task
			other.ID, other.FollowSubscriptionID, other.FollowResourceFingerprint = "different-download", "", ""
			_, err := f.queue.EnqueueWith(EnqueueJobInput{OwnerID: f.actor.User.ID, JobType: "download", DisplayName: "Different task", Payload: downloadJobPayload{DownloadTaskID: other.ID}}, func(tx *gorm.DB, job models.Job) error { other.JobID = job.ID; return tx.Create(&other).Error })
			if err != nil {
				t.Fatal(err)
			}
			if err := f.queue.db.Model(&models.Job{}).Where("id = ?", other.JobID).Update("status", models.JobStatusFailed).Error; err != nil {
				t.Fatal(err)
			}
			if err := f.queue.db.Model(&models.FollowEpisodeClaim{}).Where("subscription_id = ?", f.task.FollowSubscriptionID).Update("download_task_id", other.ID).Error; err != nil {
				t.Fatal(err)
			}
		},
		"ambiguous_package": func(f *legacyFollowIdentityFixture) {
			f.manifest.Files = append(f.manifest.Files, downloadpkg.File{RelativePath: "Fixture.Show.S01E12.mkv", Size: 2 * 1024 * 1024 * 1024})
			f.task.CompletedManifestJSON, _ = encodeCompletedDownloadManifest(f.manifest)
		},
		"missing_episodes": func(f *legacyFollowIdentityFixture) {
			f.manifest.Files[0].RelativePath = "opaque.mkv"
			f.task.CompletedManifestJSON, _ = encodeCompletedDownloadManifest(f.manifest)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newLegacyFollowIdentityFixture(t)
			change(f)
			if err := f.queue.db.Save(&f.task).Error; err != nil {
				t.Fatal(err)
			}
			before := f.task.IdentitySnapshotJSON
			result, _ := f.retry(t)
			if result.ErrorCode != CodeTransferMediaUnrecognized {
				t.Fatalf("unproven legacy identity was accepted: %+v", result)
			}
			var current models.DownloadTask
			if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil || !current.IdentityLocked || current.IdentitySnapshotJSON != before {
				t.Fatalf("unproven identity was rewritten: locked=%v err=%v", current.IdentityLocked, err)
			}
			var count int64
			if err := f.queue.db.Model(&models.TransferTask{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("unproven identity created transfer: count=%d err=%v", count, err)
			}
		})
	}
}

func TestCompletedLegacyFollowIdentityKeepsDuplicateFormatSourceAndSelectsOne(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	f.manifest.Files = append(f.manifest.Files, downloadpkg.File{RelativePath: "Fixture.Show.S01E11.mp4", Size: 2 * 1024 * 1024 * 1024, ProviderItemID: "alternate-file"})
	f.task.CompletedManifestJSON, _ = encodeCompletedDownloadManifest(f.manifest)
	if err := f.queue.db.Save(&f.task).Error; err != nil {
		t.Fatal(err)
	}
	result, _ := f.retry(t)
	if result.ErrorCode != "" {
		t.Fatalf("duplicate format selection failed: %+v", result)
	}
	var transfer models.TransferTask
	if err := f.queue.db.First(&transfer, "download_task_id = ?", f.task.ID).Error; err != nil {
		t.Fatal(err)
	}
	var selected, source downloadpkg.Manifest
	if json.Unmarshal([]byte(transfer.ManifestJSON), &selected) != nil || json.Unmarshal([]byte(transfer.SourceManifestJSON), &source) != nil || len(selected.Files) != 1 || len(source.Files) != 2 || !sameAutomaticTransferFiles(source.Files, f.manifest.Files) {
		t.Fatal("duplicate format selection lost the complete source or entered duplicate episodes")
	}
}

func TestCompletedLegacyFollowIdentityKeepsMultiEpisodePackage(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	f.manifest.Files = append(f.manifest.Files, downloadpkg.File{RelativePath: "Fixture.Show.S01E12.mkv", Size: 2 * 1024 * 1024 * 1024, ProviderItemID: "episode-twelve"})
	f.task.ScrapeEpisode, f.task.RecognitionOverrideEpisode = nil, nil
	f.task.CompletedManifestJSON, _ = encodeCompletedDownloadManifest(f.manifest)
	_, raw, err := buildDownloadIdentitySnapshot(f.task, scrapeMatch{Title: f.task.ScrapeTitle, TMDBID: f.task.ScrapeTMDBID, MediaType: f.task.ScrapeMediaType, Category: f.task.ScrapeCategory, Year: f.task.ScrapeYear, Confidence: f.task.ScrapeConfidence, Season: f.task.ScrapeSeason}, f.manifest, f.task.IdentitySource, f.task.IdentityStatus, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	f.task.IdentitySnapshotJSON = raw
	if err := f.queue.db.Save(&f.task).Error; err != nil {
		t.Fatal(err)
	}
	runID := "original-run"
	if err := f.queue.db.Create(&models.FollowEpisodeClaim{SubscriptionID: f.task.FollowSubscriptionID, SeasonNumber: 1, EpisodeNumber: 12, State: "downloading", RunID: &runID, DownloadTaskID: &f.task.ID, ResourceFingerprint: f.task.FollowResourceFingerprint}).Error; err != nil {
		t.Fatal(err)
	}
	result, _ := f.retry(t)
	if result.ErrorCode != "" {
		t.Fatalf("multi-episode package failed: %+v", result)
	}
	var current models.DownloadTask
	if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil {
		t.Fatal(err)
	}
	identity, err := decodeMediaIdentity(current.IdentitySnapshotJSON)
	if err != nil || current.IdentityLocked || identity.Episode != nil || len(identity.Episodes) != 2 || identity.Episodes[0].Episode == nil || *identity.Episodes[0].Episode != 11 || identity.Episodes[1].Episode == nil || *identity.Episodes[1].Episode != 12 {
		t.Fatalf("multi-episode identity=%+v err=%v", identity, err)
	}
}

func TestCompletedLegacyFollowIdentityRespectsPipelineCancellation(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	if err := f.queue.db.Model(&models.DownloadTask{}).Where("id = ?", f.task.ID).Update("phase", models.DownloadTaskStatusCancelled).Error; err != nil {
		t.Fatal(err)
	}
	result, _ := f.retry(t)
	if result.ErrorCode != "" {
		t.Fatalf("cancelled worker result=%+v", result)
	}
	var current models.DownloadTask
	if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil || current.Phase != models.DownloadTaskStatusCancelled || !current.IdentityLocked || current.IdentitySnapshotJSON != f.task.IdentitySnapshotJSON {
		t.Fatal("cancelled task was repaired or advanced")
	}
	var count int64
	if err := f.queue.db.Model(&models.TransferTask{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("cancelled task created transfer: %d %v", count, err)
	}
}

func TestCompletedIdentityPreservesCanonicalSourceAndManualLock(t *testing.T) {
	for _, source := range []string{mediaIdentitySourceDirectID, mediaIdentitySourceAutomatic, mediaIdentitySourceAI, mediaIdentitySourceManual} {
		t.Run(source, func(t *testing.T) {
			f := newLegacyFollowIdentityFixture(t)
			identity, _ := decodeMediaIdentity(f.task.IdentitySnapshotJSON)
			identity.Source, identity.Locked = source, source == mediaIdentitySourceManual
			identity.Revision = 2
			if source == mediaIdentitySourceAI {
				identity.Status = mediaIdentityStatusProvisional
			}
			f.task.IdentitySource, f.task.IdentityLocked, f.task.IdentityRevision, f.task.IdentityStatus = identity.Source, identity.Locked, identity.Revision, identity.Status
			if source == mediaIdentitySourceAutomatic || source == mediaIdentitySourceAI {
				f.task.RecognitionOverrideTMDBID, f.task.RecognitionOverrideMediaType = nil, ""
			}
			raw, _ := json.Marshal(identity)
			f.task.IdentitySnapshotJSON = string(raw)
			if err := f.queue.db.Save(&f.task).Error; err != nil {
				t.Fatal(err)
			}
			result, _ := f.retry(t)
			if result.ErrorCode != "" {
				t.Fatalf("canonical %s completion failed: %+v", source, result)
			}
			var current models.DownloadTask
			if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil || current.IdentityLocked != identity.Locked || current.IdentitySource != source || current.IdentityStatus != identity.Status || current.IdentityRevision != 2 {
				t.Fatalf("canonical source/lock/revision changed: %+v %v", current, err)
			}
		})
	}
}

func TestCompletedLegacyFollowIdentityCannotOverwriteNewManualRevision(t *testing.T) {
	for _, name := range []string{"original_claim", "claim_released"} {
		t.Run(name, func(t *testing.T) {
			f := newLegacyFollowIdentityFixture(t)
			identity, _ := decodeMediaIdentity(f.task.IdentitySnapshotJSON)
			identity.Source, identity.Revision = mediaIdentitySourceManual, 2
			raw, _ := json.Marshal(identity)
			f.metadata = func() {
				if err := f.queue.db.Model(&models.DownloadTask{}).Where("id = ?", f.task.ID).Updates(map[string]any{"identity_source": mediaIdentitySourceManual, "identity_revision": 2, "identity_snapshot_json": string(raw), "last_error_code": "manual-owned"}).Error; err != nil {
					t.Error(err)
				}
				if name == "claim_released" {
					if err := f.queue.db.Where("subscription_id = ?", f.task.FollowSubscriptionID).Delete(&models.FollowEpisodeClaim{}).Error; err != nil {
						t.Error(err)
					}
				}
			}
			result, _ := f.retry(t)
			if result.ErrorCode != "" {
				t.Fatalf("retired worker decorated the new identity with failure: %+v", result)
			}
			var current models.DownloadTask
			if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil || current.IdentitySource != mediaIdentitySourceManual || current.IdentityRevision != 2 || !current.IdentityLocked || current.IdentitySnapshotJSON != string(raw) || current.LastErrorCode != "manual-owned" {
				t.Fatalf("new manual correction lost its authority: %+v %v", current, err)
			}
			var count int64
			if err := f.queue.db.Model(&models.TransferTask{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("stale worker enqueued transfer: count=%d err=%v", count, err)
			}
		})
	}
}

func TestDownloadIdentityBindingRejectsContradictoryDirectLock(t *testing.T) {
	if validDownloadIdentityBinding(DownloadRecognitionIdentity{TMDBID: 100, MediaType: "tv", Source: mediaIdentitySourceDirectID, Status: mediaIdentityStatusVerified, Locked: true}) {
		t.Fatal("submission permits direct_id+locked while Transfer rejects it")
	}
}

func TestCompletedIdentityUsesRealManualCorrectionBeforeOldScrapeProjection(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	season, episode := 1, 12
	corrected, err := f.downloads.OverrideRecognition(context.Background(), f.actor, f.task.ID, DownloadRecognitionOverrideInput{TMDBID: 101, MediaType: "tv", Season: &season, Episode: &episode}, RequestContext{})
	if err != nil || corrected.JobStatus != models.JobStatusQueued {
		t.Fatalf("real manual correction failed: %+v %v", corrected, err)
	}
	var before models.DownloadTask
	if err := f.queue.db.First(&before, "id = ?", f.task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if before.ScrapeTMDBID == nil || *before.ScrapeTMDBID != 100 || before.RecognitionOverrideTMDBID == nil || *before.RecognitionOverrideTMDBID != 101 || before.IdentityRevision != 2 || before.IdentitySource != mediaIdentitySourceManual || !before.IdentityLocked {
		t.Fatal("fixture did not exercise new manual identity with old scrape projection")
	}
	claim, err := f.queue.Claim([]string{"download"})
	if err != nil || claim == nil {
		t.Fatalf("manual recovery claim=%+v err=%v", claim, err)
	}
	result := NewDownloadWorker(f.downloads).Run(context.Background(), workerRuntime{queue: f.queue, job: *claim}, *claim)
	if result.ErrorCode != "" || result.RetryAt != nil || result.Wait != nil {
		t.Fatalf("real manual correction was rejected by old projection: %+v", result)
	}
	var current models.DownloadTask
	if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil || current.ScrapeTMDBID == nil || *current.ScrapeTMDBID != 101 || current.ScrapeTitle != "另一部剧" || current.IdentityRevision != 2 || current.IdentitySource != mediaIdentitySourceManual || !current.IdentityLocked || current.ScrapeEpisode == nil || *current.ScrapeEpisode != 12 || current.CompletedManifestJSON != f.task.CompletedManifestJSON {
		t.Fatalf("manual identity/episode not preserved: %+v %v", current, err)
	}
	var count int64
	if err := f.queue.db.Model(&models.TransferTask{}).Where("download_task_id = ?", f.task.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("manual correction transfer=%d %v", count, err)
	}
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	if f.provider.submits != 0 || f.provider.getCalls != 0 || f.provider.paused || f.provider.resumed || f.provider.cancelled {
		t.Fatal("manual correction touched the original completed provider task")
	}
}

func TestCompletedLegacyFollowIdentityInitialIntentCannotOverwriteNewManualRevision(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	identity, _ := decodeMediaIdentity(f.task.IdentitySnapshotJSON)
	identity.Source, identity.Revision = mediaIdentitySourceManual, 2
	raw, _ := json.Marshal(identity)
	armed := true
	const callback = "test:replace-manual-before-original-claim-query"
	if err := f.queue.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if !armed || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "follow_episode_claims" {
			return
		}
		armed = false
		if err := f.queue.db.Model(&models.DownloadTask{}).Where("id = ?", f.task.ID).Updates(map[string]any{"identity_source": mediaIdentitySourceManual, "identity_revision": 2, "identity_snapshot_json": string(raw), "phase": models.DownloadTaskStatusVerifying, "last_error_code": "manual-owned"}).Error; err != nil {
			tx.AddError(err)
			return
		}
		if err := f.queue.db.Where("subscription_id = ?", f.task.FollowSubscriptionID).Delete(&models.FollowEpisodeClaim{}).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.queue.db.Callback().Query().Remove(callback) })
	result, _ := f.retry(t)
	if armed || result.ErrorCode != "" {
		t.Fatalf("original-intent race was not retired: armed=%v result=%+v", armed, result)
	}
	var current models.DownloadTask
	if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil || current.IdentitySource != mediaIdentitySourceManual || current.IdentityRevision != 2 || !current.IdentityLocked || current.IdentitySnapshotJSON != string(raw) || current.LastErrorCode != "manual-owned" || current.Phase != models.DownloadTaskStatusVerifying {
		t.Fatalf("early old failure decorated new manual identity: %+v %v", current, err)
	}
	var count int64
	if err := f.queue.db.Model(&models.TransferTask{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("early stale worker enqueued transfer: %d %v", count, err)
	}
}
