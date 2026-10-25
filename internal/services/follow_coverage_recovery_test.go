package services

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"gorm.io/gorm"
)

func coverageCatalogFixture(t *testing.T, storageType string) (*MediaLibraryService, models.MediaLibrary, models.Storage, models.MediaClassificationProfile) {
	t.Helper()
	s, library, _ := createCatalogTestLibrary(t)
	library.Enabled = true
	if err := s.db.Model(&library).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	storage.Type = storageType
	if err := s.db.Model(&storage).Update("type", storageType).Error; err != nil {
		t.Fatal(err)
	}
	activateCoverageCatalogFixture(t, s, library, storage, profile)
	return s, library, storage, profile
}

func activateCoverageCatalogFixture(t *testing.T, s *MediaLibraryService, library models.MediaLibrary, storage models.Storage, profile models.MediaClassificationProfile) {
	t.Helper()
	var location struct{ File string }
	if err := s.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	reader, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := reader.DB()
	t.Cleanup(func() { _ = pool.Close() })
	store := NewCatalogSnapshotStore(s.db, reader)
	s.SetCatalogSnapshotStore(store)
	head := models.CatalogHead{LibraryID: library.ID, SourceEpoch: 1, SourceFingerprint: catalogSourceFingerprint(library, storage), ConfigFingerprint: catalogConfigFingerprint(library, storage, profile)}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		return StartCatalogConversionTx(tx, head, func(*gorm.DB) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	base, token, err := store.BeginCandidate(context.Background(), CatalogCandidateInput{LibraryID: library.ID, Kind: "base", SourceEpoch: head.SourceEpoch, SourceFingerprint: head.SourceFingerprint, ConfigFingerprint: head.ConfigFingerprint, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var entries []models.MediaLibraryEntry
	if err := s.db.Where("library_id = ?", library.ID).Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	requests := make([]CatalogIdentityRequest, 0, len(entries))
	batch := CatalogFactBatch{}
	for _, entry := range entries {
		requests = append(requests, CatalogIdentityRequest{Kind: "entry", SourceKey: entry.RelativePath, ExistingID: entry.ID})
		batch.Entries = append(batch.Entries, CatalogEntryFromLegacy(entry, nil))
	}
	if len(requests) > 0 {
		if _, err := store.ResolveIdentities(context.Background(), base.ID, token, requests); err != nil {
			t.Fatal(err)
		}
		if err := store.AppendBatch(context.Background(), base.ID, token, batch); err != nil {
			t.Fatal(err)
		}
	}
	catalogPublish(t, store, base, token)
	s.changes = NewMediaChangeService(s.db)
	s.queue = NewQueueService(s.db, NewAuditService(s.db))
	s.EnableCatalogScanFollowups()
}

func TestCoverageRetainsCurrentFullBaselineAcrossBoundedPublication(t *testing.T) {
	for _, storageType := range []string{models.StorageTypeLocal, models.StorageTypePan115, models.StorageTypeQuark} {
		t.Run(storageType, func(t *testing.T) {
			s, library, storage, profile := coverageCatalogFixture(t, storageType)
			_ = saveCatalogFollowup(t, s, library, storage, profile, false, false)
			partial := saveCatalogFollowup(t, s, library, storage, profile, true, false)
			if !partial.CompleteObserved {
				t.Fatal("fixture did not retain a complete source receipt")
			}
			service := NewMediaCoverageService(s.db, nil)
			service.SetCatalogSnapshotStore(s.catalogStore)
			actor := Actor{Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
			libraries, _, reliable, state, err := service.coverageCatalog(context.Background(), actor, "tv", 100)
			if err != nil || len(libraries) != 1 || !reliable || state != "complete" {
				t.Fatalf("committed bounded update discarded current full-source authority: libraries=%+v reliable=%v state=%s err=%v", libraries, reliable, state, err)
			}
		})
	}
}

func TestFollowCatalogPositivesSurviveMissingMetadataRows(t *testing.T) {
	f := newFollowCoverageFixture(t, "missing_rows", "complete", 16)
	coverage, err := f.follows.coverage.Coverage(context.Background(), f.actor, "tv", 100)
	if err != nil || coverage.TV == nil || coverage.TV.Counts.Present != 16 || coverage.TV.Counts.Unknown != 8 || coverage.TV.Counts.Missing != 0 {
		t.Fatalf("truncated metadata erased a catalog-proven episode: coverage=%+v err=%v", coverage, err)
	}
	run := f.run(t)
	if run.ErrorCode != "follow_coverage_unknown" || run.SearchedNamesCount != 0 || run.Selected != 0 {
		t.Fatalf("metadata uncertainty triggered acquisition: %+v", run)
	}
	var claim models.FollowEpisodeClaim
	if err := f.follows.db.First(&claim, "subscription_id = ? AND season_number = 1 AND episode_number = 16", f.subscription.ID).Error; err != nil || claim.State != "imported" {
		t.Fatalf("known catalog presence lost imported claim: %+v err=%v", claim, err)
	}
}

func TestFollowUndatedOnlyWaitDoesNotBlameAnUnrelatedPartialScan(t *testing.T) {
	f := newFollowCoverageFixture(t, "missing_dates", "partial", 15)
	run := f.run(t)
	if run.ErrorCode != "follow_coverage_unknown" || run.SearchedNamesCount != 0 || strings.Contains(run.ErrorMessage, "扫描") || !strings.Contains(run.ErrorMessage, "播出信息不完整") {
		t.Fatalf("all known aired episodes present but wait blamed a scan: %+v", run)
	}
}

func TestCoverageSourceAuthorityRejectsIncompleteAndStaleEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *MediaLibraryService, models.MediaLibrary, models.Storage, models.MediaClassificationProfile, models.CatalogScanFollowup)
	}{
		{"initial_partial", func(t *testing.T, s *MediaLibraryService, l models.MediaLibrary, st models.Storage, p models.MediaClassificationProfile, row models.CatalogScanFollowup) {
			if err := s.db.Model(&row).Update("complete_observed", false).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"source_root", func(t *testing.T, s *MediaLibraryService, l models.MediaLibrary, st models.Storage, p models.MediaClassificationProfile, row models.CatalogScanFollowup) {
			if err := s.db.Model(&l).Update("relative_root", "/changed").Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"profile_revision", func(t *testing.T, s *MediaLibraryService, l models.MediaLibrary, st models.Storage, p models.MediaClassificationProfile, row models.CatalogScanFollowup) {
			if err := s.db.Model(&p).Update("revision", p.Revision+1).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"receipt_epoch", func(t *testing.T, s *MediaLibraryService, l models.MediaLibrary, st models.Storage, p models.MediaClassificationProfile, row models.CatalogScanFollowup) {
			if err := s.db.Model(&row).Update("source_epoch", row.SourceEpoch+1).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"baseline_generation", func(t *testing.T, s *MediaLibraryService, l models.MediaLibrary, st models.Storage, p models.MediaClassificationProfile, row models.CatalogScanFollowup) {
			if err := s.db.Model(&l).Update("baseline_generation", row.Generation+1).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"unpublished_bound_run", func(t *testing.T, s *MediaLibraryService, l models.MediaLibrary, st models.Storage, p models.MediaClassificationProfile, row models.CatalogScanFollowup) {
			if err := s.db.Model(&models.MediaLibraryScanRun{}).Where("id = ?", row.ScanRunID).Update("catalog_published_at", nil).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"later_failed_scan", func(t *testing.T, s *MediaLibraryService, l models.MediaLibrary, st models.Storage, p models.MediaClassificationProfile, row models.CatalogScanFollowup) {
			_, run := catalogScanRun(t, s, l.ID, st, p)
			if err := s.db.Model(&run).Update("status", "failed").Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"recognition_pending", func(t *testing.T, s *MediaLibraryService, l models.MediaLibrary, st models.Storage, p models.MediaClassificationProfile, row models.CatalogScanFollowup) {
			if err := s.db.Model(&row).Update("recognition_pending", true).Error; err != nil {
				t.Fatal(err)
			}
		}},
		{"no_scan_evidence", func(t *testing.T, s *MediaLibraryService, l models.MediaLibrary, st models.Storage, p models.MediaClassificationProfile, row models.CatalogScanFollowup) {
			if err := s.db.Where("library_id = ?", l.ID).Delete(&models.MediaLibraryScanRun{}).Error; err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, library, storage, profile := coverageCatalogFixture(t, models.StorageTypeLocal)
			row := saveCatalogFollowup(t, s, library, storage, profile, false, false)
			test.mutate(t, s, library, storage, profile, row)
			coverage := NewMediaCoverageService(s.db, nil)
			coverage.SetCatalogSnapshotStore(s.catalogStore)
			actor := Actor{Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
			_, _, reliable, state, err := coverage.coverageCatalog(context.Background(), actor, "tv", 100)
			if err != nil || reliable || state == "complete" {
				t.Fatalf("stale/incomplete source established absence: reliable=%v state=%s err=%v", reliable, state, err)
			}
		})
	}
}

func TestFollowFullBaselineThenBoundedPublicationUsesSharedCoverage(t *testing.T) {
	f := newFollowCoverageFixture(t, "missing_dates", "complete", 15)
	s := NewMediaLibraryService(f.follows.db, f.follows.audit, f.worker.sites.log)
	var library models.MediaLibrary
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := s.db.First(&library, f.libraryID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	activateCoverageCatalogFixture(t, s, library, storage, profile)
	_ = saveCatalogFollowup(t, s, library, storage, profile, false, false)
	_ = saveCatalogFollowup(t, s, library, storage, profile, true, false)
	f.follows.coverage.SetCatalogSnapshotStore(s.catalogStore)
	coverage, err := f.follows.coverage.Coverage(context.Background(), f.actor, "tv", 100)
	if err != nil || coverage.Freshness.LibraryScanState != "complete" || coverage.TV.Counts.Present != 15 || coverage.TV.Counts.Missing != 0 {
		t.Fatalf("shared complete baseline projection=%+v err=%v", coverage, err)
	}
	run := f.run(t)
	if run.ErrorCode != "follow_coverage_unknown" || run.SearchedNamesCount != 0 || strings.Contains(run.ErrorMessage, "扫描") || !strings.Contains(run.FilterSummaryJSON, `"coverage_air_date_unknown":9`) {
		t.Fatalf("full-to-partial Follow used obsolete scan summary: %+v", run)
	}
}

func TestFollowUnavailableSeasonPreservesPositivesAndWaitsWithZeroDeclaredCount(t *testing.T) {
	for _, mode := range []string{"season_error", "zero_error", "zero_empty"} {
		for _, present := range []int{0, 16} {
			t.Run(mode+"/"+uintID(uint(present)), func(t *testing.T) {
				f := newFollowCoverageFixture(t, mode, "complete", present)
				coverage, err := f.follows.coverage.Coverage(context.Background(), f.actor, "tv", 100)
				if err != nil || coverage.TV == nil || coverage.TV.Counts.Present != present || coverage.TV.Counts.Missing != 0 || coverage.Freshness.TMDBState != "partial" {
					t.Fatalf("unavailable season lost positive evidence: %+v err=%v", coverage, err)
				}
				run := f.run(t)
				if run.ErrorCode != "follow_coverage_unknown" || run.SearchedNamesCount != 0 || run.Selected != 0 || !strings.Contains(run.FilterSummaryJSON, `"coverage_seasons_unavailable":1`) || strings.Contains(run.ErrorMessage, "扫描") {
					t.Fatalf("zero declared count fabricated complete metadata: %+v", run)
				}
			})
		}
	}
}

func TestCompletedFollowRecoveryIgnoresUnrelatedFailedPackReservation(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	now := time.Now().UTC()
	if err := f.queue.db.Model(&models.FollowSubscription{}).Where("id = ?", f.task.FollowSubscriptionID).Updates(map[string]any{"status": models.FollowStatusActive, "lifecycle_revision": 1}).Error; err != nil {
		t.Fatal(err)
	}
	var original models.FollowRun
	if err := f.queue.db.First(&original, "id = ?", "original-run").Error; err != nil {
		t.Fatal(err)
	}
	job, err := f.queue.Enqueue(EnqueueJobInput{OwnerID: f.actor.User.ID, JobType: JobTypeFollowSearch, DisplayName: "Later pack selection", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	later := original
	later.ID, later.JobID, later.Status = "later-pack-run", job.ID, models.FollowRunRunning
	later.MissingSnapshotJSON = `[{"season":1,"episode":12}]`
	if err := f.queue.db.Create(&later).Error; err != nil {
		t.Fatal(err)
	}
	follows := NewFollowService(f.queue.db, f.queue.audit, f.queue, nil, NewAuthorizationService(f.queue.db))
	follows.now = func() time.Time { return now }
	worker := NewFollowSearchWorker(follows, nil)
	reserved, _, err := worker.reserveEpisodes(later, f.task.FollowSubscriptionID, followCandidate{Season: 1, Fingerprint: f.task.FollowResourceFingerprint}, []int{12})
	if err != nil || !reserved {
		t.Fatalf("real later reservation failed: %v %v", reserved, err)
	}
	worker.releaseEpisodes(f.task.FollowSubscriptionID, later.ID, f.task.FollowResourceFingerprint)
	var before models.FollowEpisodeClaim
	if err := f.queue.db.First(&before, "subscription_id = ? AND episode_number = 12", f.task.FollowSubscriptionID).Error; err != nil || before.State != "failed" || before.DownloadTaskID != nil {
		t.Fatalf("fixture did not reach released pre-handoff claim: %+v %v", before, err)
	}
	result, _ := f.retry(t)
	if result.ErrorCode != "" || result.Wait != nil || result.RetryAt != nil {
		t.Fatalf("unrelated failed reservation contaminated exact original package proof: %+v", result)
	}
	var after models.FollowEpisodeClaim
	if err := f.queue.db.First(&after, "subscription_id = ? AND episode_number = 12", f.task.FollowSubscriptionID).Error; err != nil || after.RunID == nil || *after.RunID != later.ID || after.State != before.State || after.DownloadTaskID != nil {
		t.Fatalf("recovery stole another pack reservation: %+v %v", after, err)
	}
	var count int64
	if err := f.queue.db.Model(&models.TransferTask{}).Where("download_task_id = ?", f.task.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("exact completed package not admitted once: %d %v", count, err)
	}
}

// Roll back every write if the automated submission's transactional guard
// rejects the replacement. The earlier completed package must remain bound.
func TestFollowReplacementRejectedBeforePersistKeepsOriginalBinding(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	staging := models.Storage{Name: "Replacement staging", NameNormalized: "replacement-staging", Type: models.StorageTypeLocal, RootPath: t.TempDir(), RootPathNormalized: "replacement-staging", Enabled: true, Capabilities: `{}`}
	if err := f.queue.db.Create(&staging).Error; err != nil {
		t.Fatal(err)
	}
	configureDownloadStaging(t, f.queue, staging.ID)
	downloader, err := f.downloads.downloader.Create(f.actor, DownloaderInput{Name: "Replacement", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://replacement.example.test", Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	guardCalled := false
	_, err = f.downloads.Submit(context.Background(), f.actor, SubmitDownloadInput{DownloaderID: downloader.ID, FollowSubscriptionID: f.task.FollowSubscriptionID, FollowResourceFingerprint: f.task.FollowResourceFingerprint, Source: DownloadSourceInput{Kind: "url", URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}, BeforePersist: func(*gorm.DB) error {
		guardCalled = true
		return appError(CodeConflict, "fixture guard refused", nil)
	}}, RequestContext{})
	if err == nil || !guardCalled {
		t.Fatalf("real Submit did not reach its rejected persistence guard: called=%v err=%v", guardCalled, err)
	}
	var current models.DownloadTask
	if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil || current.FollowSubscriptionID != f.task.FollowSubscriptionID || current.FollowResourceFingerprint != f.task.FollowResourceFingerprint || current.CompletedManifestJSON != f.task.CompletedManifestJSON || current.IdentitySnapshotJSON != f.task.IdentitySnapshotJSON {
		t.Fatalf("rejected replacement detached the original completed package: %+v err=%v", current, err)
	}
}

func TestFollowReplacementAcceptsAtomicallyAndRollsBackAfterCreate(t *testing.T) {
	for _, rejectAfterCreate := range []bool{true, false} {
		t.Run(map[bool]string{true: "after_create_failure", false: "accepted"}[rejectAfterCreate], func(t *testing.T) {
			f := newLegacyFollowIdentityFixture(t)
			staging := models.Storage{Name: "Replacement staging", NameNormalized: "replacement-staging", Type: models.StorageTypeLocal, RootPath: t.TempDir(), RootPathNormalized: "replacement-staging", Enabled: true, Capabilities: `{}`}
			if err := f.queue.db.Create(&staging).Error; err != nil {
				t.Fatal(err)
			}
			configureDownloadStaging(t, f.queue, staging.ID)
			downloader, err := f.downloads.downloader.Create(f.actor, DownloaderInput{Name: "Replacement", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://replacement.example.test", Enabled: true}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			input := SubmitDownloadInput{DownloaderID: downloader.ID, FollowSubscriptionID: f.task.FollowSubscriptionID, FollowResourceFingerprint: f.task.FollowResourceFingerprint, Source: DownloadSourceInput{Kind: "url", URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"}}
			afterCalled := false
			input.AfterPersist = func(tx *gorm.DB, accepted models.DownloadTask) error {
				afterCalled = true
				var previous models.DownloadTask
				if err := tx.First(&previous, "id = ?", f.task.ID).Error; err != nil {
					return err
				}
				if previous.FollowSubscriptionID != "" || previous.FollowResourceFingerprint != "" {
					t.Fatal("key release and new row were not in the same transaction")
				}
				if rejectAfterCreate {
					return appError(CodeConflict, "fixture after-create refusal", nil)
				}
				return nil
			}
			created, submitErr := f.downloads.Submit(context.Background(), f.actor, input, RequestContext{})
			if !afterCalled || (submitErr != nil) != rejectAfterCreate {
				t.Fatalf("actual submission outcome mismatch: after=%v err=%v", afterCalled, submitErr)
			}
			var original models.DownloadTask
			if err := f.queue.db.First(&original, "id = ?", f.task.ID).Error; err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := f.queue.db.Model(&models.DownloadTask{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if rejectAfterCreate {
				if count != 1 || original.FollowSubscriptionID != f.task.FollowSubscriptionID || original.FollowResourceFingerprint != f.task.FollowResourceFingerprint {
					t.Fatalf("post-create refusal lost the old binding or leaked a successor: count=%d", count)
				}
			} else {
				if count != 2 || original.FollowSubscriptionID != "" || original.FollowResourceFingerprint != "" {
					t.Fatalf("accepted replacement did not release exactly its old key: count=%d", count)
				}
				input.AfterPersist = nil
				reused, err := f.downloads.Submit(context.Background(), f.actor, input, RequestContext{})
				if err != nil || reused.ID != created.ID {
					t.Fatalf("repeated accepted handoff duplicated its task: %+v %v", reused, err)
				}
				if _, err := f.queue.Control(f.actor, created.JobID, "pause", RequestContext{}); err != nil {
					t.Fatal(err)
				}
				var logs bytes.Buffer
				f.downloads.log = zerolog.New(&logs)
				result, _ := f.retry(t)
				if result.ErrorCode != CodeTransferMediaUnrecognized || !strings.Contains(result.ErrorMessage, "追更关联依据已不可用") || strings.Contains(result.ErrorMessage, "媒体未识别") {
					t.Fatalf("detached old task was misdiagnosed or stole its successor: %+v", result)
				}
				if !strings.Contains(logs.String(), `"identity_reason":"binding_unavailable"`) {
					t.Fatalf("finite recovery cause missing from real worker log: %s", logs.String())
				}
				for _, private := range []string{f.task.ProviderTaskID, f.task.SourceCiphertext, "original-file", "Fixture.Show.S01E11.mkv"} {
					if strings.Contains(logs.String(), private) {
						t.Fatalf("private identity evidence leaked in recovery diagnostics: %q", private)
					}
				}
			}
			if original.CompletedManifestJSON != f.task.CompletedManifestJSON || original.IdentitySnapshotJSON != f.task.IdentitySnapshotJSON || original.ProviderTaskID != f.task.ProviderTaskID {
				t.Fatal("replacement changed the original completed package")
			}
			f.provider.mu.Lock()
			calls := f.provider.submits + f.provider.getCalls
			f.provider.mu.Unlock()
			if calls != 0 {
				t.Fatal("submission persistence fixture called a provider")
			}
		})
	}
}

func TestCompletedFollowRecoveryRetiresChangedProviderEvidence(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	changed := false
	f.metadata = func() {
		if !changed {
			changed = true
			if err := f.queue.db.Model(&models.DownloadTask{}).Where("id = ?", f.task.ID).Update("provider_output_id", "newer-output").Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	result, _ := f.retry(t)
	if result.ErrorCode != "" || !changed {
		t.Fatalf("retired provider evidence published a failure/success: %+v changed=%v", result, changed)
	}
	var current models.DownloadTask
	if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil || !current.IdentityLocked || current.IdentitySnapshotJSON != f.task.IdentitySnapshotJSON || current.ProviderOutputID != "newer-output" || current.LastErrorCode != "" {
		t.Fatalf("old verifier overwrote changed provider evidence: %+v %v", current, err)
	}
	var count int64
	if err := f.queue.db.Model(&models.TransferTask{}).Where("download_task_id = ?", f.task.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("retired output reached Transfer: %d %v", count, err)
	}
}

func TestCompletedFollowRecoveryRejectsAnotherJobPayload(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	job, err := f.queue.Enqueue(EnqueueJobInput{OwnerID: f.actor.User.ID, JobType: "download", DisplayName: "Different execution", Payload: downloadJobPayload{DownloadTaskID: f.task.ID, ResourceAccessVersion: 1}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := f.queue.Claim([]string{"download"})
	if err != nil || claim == nil || claim.Job.ID != job.ID {
		t.Fatalf("different job fixture did not claim: %+v %v", claim, err)
	}
	result := NewDownloadWorker(f.downloads).Run(context.Background(), workerRuntime{queue: f.queue, job: *claim}, *claim)
	if result.ErrorCode != "" {
		t.Fatalf("retired foreign job wrote an error onto original task: %+v", result)
	}
	var current models.DownloadTask
	if err := f.queue.db.First(&current, "id = ?", f.task.ID).Error; err != nil || !current.IdentityLocked || current.IdentitySnapshotJSON != f.task.IdentitySnapshotJSON || current.Phase != f.task.Phase || current.LastErrorCode != f.task.LastErrorCode {
		t.Fatalf("different job changed original package: %+v %v", current, err)
	}
	var count int64
	if err := f.queue.db.Model(&models.TransferTask{}).Where("download_task_id = ?", f.task.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("different job admitted original Transfer: %d %v", count, err)
	}
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	if f.provider.submits != 0 || f.provider.getCalls != 0 {
		t.Fatal("different job touched original provider output")
	}
}

type coverageObservationReadFunc func(context.Context, models.ProviderObservationScope, string, int64, int) (ObservationDirectoryPage, error)

func (f coverageObservationReadFunc) ObserveDirectory(ctx context.Context, scope models.ProviderObservationScope, id string, offset int64, limit int) (ObservationDirectoryPage, error) {
	return f(ctx, scope, id, offset, limit)
}

func TestCoverageCompleteObserverBaselineSurvivesRoutineEnumeration(t *testing.T) {
	f := newQuarkCompletionFixture(t)
	f.observe(t)
	f.diagnose(t)
	coverage := NewMediaCoverageService(f.libraries.db, nil)
	coverage.SetCatalogSnapshotStore(f.libraries.catalogStore)
	actor := Actor{Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	assertComplete := func(stage string) {
		t.Helper()
		_, _, reliable, state, err := coverage.coverageCatalog(context.Background(), actor, "tv", 100)
		if err != nil || !reliable || state != "complete" {
			t.Errorf("%s revoked an unchanged complete baseline: reliable=%v state=%s err=%v", stage, reliable, state, err)
		}
	}
	assertComplete("initial complete")
	f.clock = f.clock.Add(2 * time.Second)
	if _, err := f.libraries.requestQuarkObservation(context.Background(), f.library.ID, true); err != nil {
		t.Fatal(err)
	}
	assertComplete("new summary without observed change")
	read := false
	f.observer.SetReaderResolver(func(context.Context, models.ProviderObservationScope) (ObservationDirectoryReader, error) {
		return coverageObservationReadFunc(func(ctx context.Context, scope models.ProviderObservationScope, id string, offset int64, limit int) (ObservationDirectoryPage, error) {
			read = true
			assertComplete("actual in-flight unchanged directory read")
			return f.reader.ObserveDirectory(ctx, scope, id, offset, limit)
		}), nil
	})
	f.observe(t)
	if !read {
		t.Fatal("new observation never entered its real directory reader")
	}
	assertComplete("confirmed unchanged round")
}

func TestCoverageCompleteObserverBaselineRejectsKnownPendingFacts(t *testing.T) {
	for _, reason := range []string{"unpublished", "missing", "write", "path", "stale_scope", "logical_hold"} {
		t.Run(reason, func(t *testing.T) {
			f := newQuarkCompletionFixture(t)
			if reason == "missing" {
				f.reader.tree["tv-root"] = []ObservationFact{obsFile("notes", "tv-root", "notes.txt")}
			}
			f.observe(t)
			f.diagnose(t)
			f.clock = f.clock.Add(2 * time.Second)
			if _, err := f.libraries.requestQuarkObservation(context.Background(), f.library.ID, true); err != nil {
				t.Fatal(err)
			}
			switch reason {
			case "logical_hold":
				owner := models.User{Username: "coverage-hold-owner", UsernameNormalized: "coverage-hold-owner", DisplayName: "Coverage hold fixture", PasswordHash: "not-an-authentication-fixture", Status: "active"}
				if err := f.libraries.db.Create(&owner).Error; err != nil {
					t.Fatal(err)
				}
				// A terminal phase cannot erase a retained logical mutation hold.
				// Its real owner/library FKs remain valid, and execution readiness
				// alone would otherwise regard this completed repair as settled.
				hold := models.MediaLibraryStructureRepair{ID: "coverage-logical-hold", OwnerID: owner.ID, LibraryID: f.library.ID, Scope: "full", RuleFingerprint: "fixture", Generation: 1, PlanJSON: `{}`, StateJSON: `{"observation_hold_version":1}`, Phase: "completed"}
				if err := f.libraries.db.Create(&hold).Error; err != nil {
					t.Fatal(err)
				}
			case "stale_scope":
				if err := f.libraries.db.Model(&models.ProviderObservationScope{}).Where("id = ?", f.scope.ID).Update("fence", "retired-source-fence").Error; err != nil {
					t.Fatal(err)
				}
			case "path":
				if err := observationCurrent(f.libraries.db, f.scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id = ?", f.scope.RootID).Update("path_repair_pending", true).Error; err != nil {
					t.Fatal(err)
				}
			case "write":
				receipt := models.QuarkOperationReceipt{ID: "coverage-held-write", ConnectionID: f.scope.ConnectionID, ScopeID: f.scope.ID, OperationKey: "coverage-held-write", Adapter: "agent", Kind: "move", Fence: f.scope.Fence, State: "prepared"}
				if err := f.libraries.db.Create(&receipt).Error; err != nil {
					t.Fatal(err)
				}
			default:
				f.reader.tree["tv-root"] = nil
				if reason == "unpublished" {
					f.reader.tree["tv-root"] = []ObservationFact{obsFile("new-video", "tv-root", "Show.S01E01.mkv")}
				}
				f.clock = f.clock.Add(2 * time.Second)
				if _, err := f.observer.ObserveNext(context.Background(), f.scope.ID, f.reader); err != nil {
					t.Fatal(err)
				}
			}
			coverage := NewMediaCoverageService(f.libraries.db, nil)
			coverage.SetCatalogSnapshotStore(f.libraries.catalogStore)
			actor := Actor{Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
			_, _, reliable, state, err := coverage.coverageCatalog(context.Background(), actor, "tv", 100)
			if err != nil || reliable || state == "complete" {
				t.Fatalf("known %s facts authorized absence: reliable=%v state=%s err=%v", reason, reliable, state, err)
			}
		})
	}
}

func TestCoverageCompleteObserverBaselineSurvivesUnchangedPagedConfirmation(t *testing.T) {
	f := newQuarkCompletionFixture(t)
	for i := 1; i <= 251; i++ {
		name := "notes-" + uintID(uint(i))
		f.reader.tree["tv-root"] = append(f.reader.tree["tv-root"], obsFile(name, "tv-root", name+".txt"))
	}
	for i := 0; i < 10; i++ {
		f.observe(t)
		if err := f.libraries.db.First(&f.scope, "id = ?", f.scope.ID).Error; err != nil {
			t.Fatal(err)
		}
		if f.scope.LastCompleteAt != nil {
			break
		}
	}
	if f.scope.LastCompleteAt == nil {
		t.Fatal("real paged observation did not establish the initial baseline")
	}
	f.diagnose(t)
	if _, err := f.libraries.requestQuarkObservation(context.Background(), f.library.ID, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		f.clock = f.clock.Add(2 * time.Second)
		if _, err := f.observer.ObserveNext(context.Background(), f.scope.ID, f.reader); err != nil {
			t.Fatal(err)
		}
	}
	var batch models.ProviderObservationBatch
	if err := f.libraries.db.Where("scope_id = ? AND state = ?", f.scope.ID, "confirming").First(&batch).Error; err != nil || batch.Revision != batch.PublishedRevision {
		t.Fatalf("real unchanged confirmation was not paged: batch=%+v err=%v", batch, err)
	}
	coverage := NewMediaCoverageService(f.libraries.db, nil)
	coverage.SetCatalogSnapshotStore(f.libraries.catalogStore)
	actor := Actor{Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	_, _, reliable, state, err := coverage.coverageCatalog(context.Background(), actor, "tv", 100)
	if err != nil || !reliable || state != "complete" {
		t.Fatalf("unchanged local confirmation revoked full-source authority: reliable=%v state=%s err=%v", reliable, state, err)
	}
}

func TestCompletedFollowRecoveryKeepsExactMixedBoundAndReleasedClaims(t *testing.T) {
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
	if err := f.queue.db.Create(&models.FollowEpisodeClaim{SubscriptionID: f.task.FollowSubscriptionID, SeasonNumber: 1, EpisodeNumber: 12, State: "failed", RunID: &runID, ResourceFingerprint: f.task.FollowResourceFingerprint}).Error; err != nil {
		t.Fatal(err)
	}
	result, _ := f.retry(t)
	if result.ErrorCode != "" {
		t.Fatalf("exact original-run released coordinate narrowed existing compatibility: %+v", result)
	}
}

func TestCompletedFollowRecoveryRejectsExtraTaskBoundCoordinate(t *testing.T) {
	f := newLegacyFollowIdentityFixture(t)
	runID := "original-run"
	if err := f.queue.db.Create(&models.FollowEpisodeClaim{SubscriptionID: f.task.FollowSubscriptionID, SeasonNumber: 1, EpisodeNumber: 12, State: "downloading", RunID: &runID, DownloadTaskID: &f.task.ID, ResourceFingerprint: f.task.FollowResourceFingerprint}).Error; err != nil {
		t.Fatal(err)
	}
	result, _ := f.retry(t)
	if result.ErrorCode != CodeTransferMediaUnrecognized {
		t.Fatalf("extra original-task coordinate bypassed the exact selected-set gate: %+v", result)
	}
	var count int64
	if err := f.queue.db.Model(&models.TransferTask{}).Where("download_task_id = ?", f.task.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("mismatched original selection created Transfer: %d %v", count, err)
	}
}

func TestFollowCoverageWaitPublishesSpecificSafePresentationSummary(t *testing.T) {
	f := newFollowCoverageFixture(t, "missing_dates", "partial", 15)
	run := f.run(t)
	var reasons map[string]int
	if err := json.Unmarshal([]byte(run.FilterSummaryJSON), &reasons); err != nil {
		t.Fatal(err)
	}
	if run.ErrorCode != "follow_coverage_unknown" || run.SearchedNamesCount != 0 || reasons["coverage_air_date_unknown"] != 9 || reasons["coverage_library_unknown"] != 0 || strings.Contains(run.ErrorMessage, "扫描") {
		t.Fatalf("actual worker presentation lost the selected uncertainty cause: %+v", run)
	}
	public, err := json.Marshal(map[string]any{"status": run.Status, "error_code": run.ErrorCode, "error_message": run.ErrorMessage, "filter_summary": reasons})
	if err != nil {
		t.Fatal(err)
	}
	// This is also replayed through the actual WebUI formatter by independent
	// review. It contains only the existing public run-summary fields.
	t.Logf("REVIEW_COVERAGE_PUBLIC %s", public)
}
