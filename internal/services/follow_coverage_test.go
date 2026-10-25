package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	storagepkg "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

type followCoverageFixture struct {
	follows      *FollowService
	worker       *FollowSearchWorker
	actor        Actor
	adapter      *stubSiteAdapter
	libraryID    uint
	subscription FollowSummary
	logs         *bytes.Buffer
	setTrailing  func(string)
	advanceClock func()
}

// Use the real coverage, identity search, private result claim and download
// services. Only TMDB and the PT/downloader adapters are offline fixtures.
func newFollowCoverageFixture(t *testing.T, trailing, scanState string, presentCount int) *followCoverageFixture {
	t.Helper()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	var trailingState atomic.Value
	trailingState.Store(trailing)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/100":
			if strings.HasPrefix(trailingState.Load().(string), "zero_") {
				_, _ = w.Write([]byte(`{"id":100,"name":"嫁金钗","original_name":"Fixture Show","first_air_date":"2026-10-01","seasons":[{"id":1,"season_number":1,"name":"第 1 季","episode_count":0}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":100,"name":"嫁金钗","original_name":"Fixture Show","original_language":"en","first_air_date":"2026-10-01","seasons":[{"id":1,"season_number":1,"name":"第 1 季","episode_count":24}],"alternative_titles":{"results":[]},"translations":{"translations":[]}}`))
		case "/tv/100/season/1":
			trailing := trailingState.Load().(string)
			if trailing == "season_error" || trailing == "zero_error" {
				http.Error(w, "fixture season unavailable", http.StatusNotFound)
				return
			}
			if trailing == "zero_empty" {
				_, _ = w.Write([]byte(`{"season_number":1,"episodes":[]}`))
				return
			}
			episodes := []map[string]any{}
			for episode := 1; episode <= 24; episode++ {
				if episode > 15 && trailing == "missing_rows" {
					continue
				}
				date := "2026-10-01"
				if episode > 15 {
					switch trailing {
					case "missing_dates":
						date = ""
					case "invalid_dates":
						date = "2026-unknown"
					case "future":
						date = "2026-11-01"
					}
				}
				episodes = append(episodes, map[string]any{"id": 1000 + episode, "season_number": 1, "episode_number": episode, "name": fmt.Sprintf("第 %d 集", episode), "air_date": date})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"season_number": 1, "episodes": episodes})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	sites, adapter, fixtureActor, store, downloads, downloaders := siteFixture(t)
	queue := downloads.queue
	logs := &bytes.Buffer{}
	logger := zerolog.New(zerolog.SyncWriter(logs))
	sites.log, downloads.log = logger, logger
	metadata := NewMetadataSettingsService(queue.db, queue.audit, store, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "test-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) {
		return tmdb.NewForTest("test-token", upstream.URL, upstream.Client())
	}
	sites.SetMetadataSettings(metadata)
	adapter.searchTitle = "Fixture.Show.S01E11-E15.1080p.WEB-DL.HEVC-GROUP"
	var administrator models.Role
	if err := queue.db.First(&administrator, "code = ?", authz.RoleAdministrator).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Create(&models.UserRole{UserID: fixtureActor.User.ID, RoleID: administrator.ID, CreatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	actor, err := NewAuthorizationService(queue.db).Resolve(fixtureActor.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := queue.db.First(&profile, "code = ?", "default-v1").Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	storage := models.Storage{Name: "Follow target", NameNormalized: "follow-target", Type: models.StorageTypeLocal, RootPath: root, RootPathNormalized: storagepkg.NormalizeForComparison(root), Enabled: true, Capabilities: `{}`}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	lastScan := now.Add(-time.Hour)
	library := models.MediaLibrary{Name: "Follow library", NameNormalized: "follow-library", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", SortOrder: 1, Enabled: true, Status: models.MediaLibraryStatusListening, BaselineGeneration: 1, LastSuccessfulScanAt: &lastScan, TransferMode: models.MediaLibraryTransferMove, ConflictPolicy: models.MediaLibraryConflictSkip, VideoExtensionsJSON: `[]`, STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`}
	if scanState == "unscanned" {
		library.BaselineGeneration, library.LastSuccessfulScanAt = 0, nil
	}
	if err := queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	if scanState != "unscanned" {
		if err := queue.db.Create(&models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "success", SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), Partial: scanState == "partial", Generation: 1, StartedAt: lastScan, FinishedAt: &lastScan}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for episode := 1; episode <= presentCount; episode++ {
		season, tmdbID := 1, int64(100)
		entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: fmt.Sprintf("/fixture/s01e%02d.mkv", episode), ProviderID: fmt.Sprintf("episode-%d", episode), MediaType: "tv", Title: "嫁金钗", WorkKey: "series:tmdb:100", Season: &season, Episode: &episode, MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, LastGeneration: 1}
		if err := queue.db.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
	}
	site, err := sites.Create(context.Background(), actor, validSiteInput("Follow PT", "https://follow-coverage.example.test"), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := downloaders.Create(actor, DownloaderInput{Name: "Follow qBit", Type: models.DownloaderTypeQBittorrent, BaseURL: "http://follow-qbit.example.test", Enabled: true}, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	coverage := NewMediaCoverageService(queue.db, metadata)
	coverage.now = func() time.Time { return now }
	follows := NewFollowService(queue.db, queue.audit, queue, coverage, NewAuthorizationService(queue.db))
	follows.SetLogger(logger)
	follows.now = func() time.Time { return now }
	follows.SetDownloadService(downloads)
	snapshot := FollowExecutionSnapshot{Version: 2, RoutingPolicy: "source_priority", Seasons: []int{1}, SiteIDs: []uint{site.ID}, MediaLibraryID: library.ID, Schedule: FollowSchedule{Kind: "interval", Minutes: 60}, MaxResourcesPerRun: 2}
	subscription, err := follows.Create(context.Background(), actor, CreateFollowInput{TMDBID: 100, Title: "嫁金钗", Snapshot: snapshot}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	return &followCoverageFixture{follows: follows, worker: NewFollowSearchWorker(follows, sites), actor: actor, adapter: adapter, libraryID: library.ID, subscription: subscription, logs: logs, setTrailing: func(value string) { trailingState.Store(value) }, advanceClock: func() { now = now.Add(2 * time.Hour) }}
}

func (f *followCoverageFixture) run(t *testing.T) models.FollowRun {
	t.Helper()
	claimed, err := f.follows.queue.Claim([]string{JobTypeFollowSearch})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	result := f.worker.Run(context.Background(), &followProgressRuntime{}, *claimed)
	if result.ErrorCode != "" || result.RetryAt != nil {
		t.Fatalf("worker result=%+v", result)
	}
	if err := f.follows.queue.Complete(claimed.Job.ID, claimed.LeaseToken); err != nil {
		t.Fatal(err)
	}
	var run models.FollowRun
	if err := f.follows.db.First(&run, "job_id = ?", claimed.Job.ID).Error; err != nil {
		t.Fatal(err)
	}
	return run
}

func TestFollowKnownMissingEpisodesSurviveUnrelatedUnknownCoverage(t *testing.T) {
	for _, trailing := range []string{"missing_dates", "invalid_dates", "missing_rows"} {
		t.Run(trailing, func(t *testing.T) {
			f := newFollowCoverageFixture(t, trailing, "complete", 10)
			coverage, err := f.follows.coverage.Coverage(context.Background(), f.actor, "tv", 100)
			if err != nil || coverage.TV == nil || coverage.TV.Counts != (MediaCoverageCounts{Total: 24, Present: 10, Missing: 5, Unknown: 9}) {
				t.Fatalf("fixture did not reproduce known and unknown facts: %+v err=%v", coverage, err)
			}
			// Existing subscriptions may already have the pre-fix blocked state.
			if err := f.follows.db.Model(&models.FollowSubscription{}).Where("id = ?", f.subscription.ID).Updates(map[string]any{"status": models.FollowStatusBlocked, "last_error_code": "follow_coverage_unknown"}).Error; err != nil {
				t.Fatal(err)
			}
			run := f.run(t)
			if run.Status != models.FollowRunSubmitted || run.Selected != 1 || run.SearchedNamesCount == 0 {
				t.Fatalf("known missing episodes were blocked by unrelated unknown facts: %+v", run)
			}
			var missing []FollowEpisodeCoordinate
			if err := json.Unmarshal([]byte(run.MissingSnapshotJSON), &missing); err != nil || len(missing) != 5 {
				t.Fatalf("missing snapshot=%v err=%v", missing, err)
			}
			for index, coordinate := range missing {
				if coordinate != (FollowEpisodeCoordinate{Season: 1, Episode: index + 11}) {
					t.Fatalf("unknown episode entered missing snapshot: %+v", missing)
				}
			}
			var claims []models.FollowEpisodeClaim
			if err := f.follows.db.Where("subscription_id = ? AND download_task_id IS NOT NULL", f.subscription.ID).Order("episode_number").Find(&claims).Error; err != nil || len(claims) != 5 {
				t.Fatalf("download claims=%+v err=%v", claims, err)
			}
			for index, claim := range claims {
				if claim.SeasonNumber != 1 || claim.EpisodeNumber != index+11 {
					t.Fatalf("unknown episode was claimed: %+v", claim)
				}
			}
			summary, err := f.follows.Get(f.actor, f.subscription.ID)
			if err != nil || summary.Status != models.FollowStatusActive || summary.ProgressTarget != 15 || summary.ProgressPresent != 10 || summary.ProgressMissing != 5 {
				t.Fatalf("subscription lost truthful counts/state: %+v err=%v", summary, err)
			}
			var reasons map[string]int
			if err := json.Unmarshal([]byte(run.FilterSummaryJSON), &reasons); err != nil || reasons["coverage_unknown"] != 9 {
				t.Fatalf("unknown count was hidden from run details: %v err=%v", reasons, err)
			}
			if _, err := f.follows.Enqueue(context.Background(), f.actor, f.subscription.ID, "manual", RequestContext{}); err != nil {
				t.Fatal(err)
			}
			claimedRun := f.run(t)
			if claimedRun.Status != models.FollowRunSubmitted || claimedRun.Selected != 0 || claimedRun.SearchedNamesCount != 0 {
				t.Fatalf("active claims plus unknown facts were failed/completed/repeated: %+v", claimedRun)
			}
			var taskCount int64
			if err := f.follows.db.Model(&models.DownloadTask{}).Where("follow_subscription_id = ?", f.subscription.ID).Count(&taskCount).Error; err != nil || taskCount != 1 {
				t.Fatalf("repeated download count=%d err=%v", taskCount, err)
			}
			for _, secret := range []string{"test-token", "server-only-secret", "passkey-server-only", "https://follow-coverage.example.test"} {
				if strings.Contains(f.logs.String(), secret) {
					t.Fatalf("follow logs leaked %q", secret)
				}
			}
		})
	}
}

func TestFollowUnknownOnlyCoverageWaitsWithoutInventingMissing(t *testing.T) {
	for _, test := range []struct {
		name, scan string
		present    int
	}{
		{name: "unscanned", scan: "unscanned", present: 10},
		{name: "partial_scan", scan: "partial", present: 10},
		{name: "only_undated_remaining", scan: "complete", present: 15},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFollowCoverageFixture(t, "missing_dates", test.scan, test.present)
			coverage, err := f.follows.coverage.Coverage(context.Background(), f.actor, "tv", 100)
			if err != nil || coverage.TV == nil || coverage.TV.Counts.Missing != 0 || coverage.TV.Counts.Unknown == 0 {
				t.Fatalf("unknown coverage was converted to missing: %+v err=%v", coverage, err)
			}
			if test.scan == "unscanned" {
				claimed, err := f.follows.queue.Claim([]string{JobTypeFollowSearch})
				if err != nil || claimed != nil || len(f.adapter.searchBases) != 0 {
					t.Fatalf("unscanned library bypassed queue readiness: %+v err=%v", claimed, err)
				}
				return
			}
			run := f.run(t)
			if run.Status != models.FollowRunFailed || run.ErrorCode != "follow_coverage_unknown" || run.SearchedNamesCount != 0 || run.Selected != 0 || len(f.adapter.searchBases) != 0 {
				t.Fatalf("uncertain absence was searched or marked complete: %+v", run)
			}
			summary, err := f.follows.Get(f.actor, f.subscription.ID)
			if err != nil || summary.Status != models.FollowStatusActive || summary.ProgressMissing != 0 || summary.NextRunAt == nil || !summary.NextRunAt.After(f.follows.now()) {
				t.Fatalf("unknown coverage became definite missing/completed: %+v err=%v", summary, err)
			}
			if test.scan == "partial" && !strings.Contains(run.ErrorMessage, "扫描尚不完整") || test.scan == "complete" && !strings.Contains(run.ErrorMessage, "播出信息不完整") {
				t.Fatalf("blocked run did not explain the missing fact: %s", run.ErrorMessage)
			}
		})
	}
}

func TestFollowUnknownCoverageKeepsScheduleUntilNewEpisodesAreConfirmed(t *testing.T) {
	f := newFollowCoverageFixture(t, "missing_dates", "complete", 10)
	first := f.run(t)
	if first.Status != models.FollowRunSubmitted || first.Selected != 1 {
		t.Fatalf("known missing episodes were not submitted: %+v", first)
	}
	for episode := 11; episode <= 15; episode++ {
		season, tmdbID := 1, int64(100)
		entry := models.MediaLibraryEntry{LibraryID: f.libraryID, RelativePath: fmt.Sprintf("/fixture/s01e%02d.mkv", episode), ProviderID: fmt.Sprintf("episode-%d", episode), MediaType: "tv", Title: "嫁金钗", WorkKey: "series:tmdb:100", Season: &season, Episode: &episode, MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, LastGeneration: 1}
		if err := f.follows.db.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
	}
	f.advanceClock()
	if err := f.follows.EnqueueDue(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	waiting := f.run(t)
	if waiting.Status != models.FollowRunFailed || waiting.ErrorCode != "follow_coverage_unknown" || waiting.SearchedNamesCount != 0 || !strings.Contains(waiting.ErrorMessage, "按计划重新检查") {
		t.Fatalf("unknown remainder was completed or searched: %+v", waiting)
	}
	waitingLogged := false
	for _, event := range businessRecords(t, f.logs, "follow_run_id", waiting.ID) {
		if event["event_code"] == "follow.finish" && event["outcome"] == "waiting" {
			waitingLogged = true
		}
	}
	if !waitingLogged {
		t.Fatal("temporary unknown coverage lacked a readable waiting outcome")
	}
	summary, err := f.follows.Get(f.actor, f.subscription.ID)
	if err != nil || summary.Status != models.FollowStatusActive || summary.ProgressTarget != 15 || summary.ProgressPresent != 15 || summary.ProgressMissing != 0 {
		t.Fatalf("unknown remainder stopped future reconciliation: %+v err=%v", summary, err)
	}
	f.setTrailing("aired")
	f.adapter.searchTitle = "Fixture.Show.S01E16-E24.1080p.WEB-DL.HEVC-GROUP"
	f.advanceClock()
	if err := f.follows.EnqueueDue(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	resumed := f.run(t)
	if resumed.Status != models.FollowRunSubmitted || resumed.Selected != 1 || resumed.SearchedNamesCount == 0 {
		t.Fatalf("confirmed later episodes did not resume on the existing schedule: %+v", resumed)
	}
	var missing []FollowEpisodeCoordinate
	if err := json.Unmarshal([]byte(resumed.MissingSnapshotJSON), &missing); err != nil || len(missing) != 9 || missing[0].Episode != 16 || missing[8].Episode != 24 {
		t.Fatalf("resumed missing snapshot=%v err=%v", missing, err)
	}
}

func TestFollowTorrentCoverageExcludesUnknownAndFuturePackages(t *testing.T) {
	for _, test := range []struct {
		name, trailing, title string
	}{
		{name: "unknown_single", trailing: "missing_dates", title: "Fixture.Show.S01E16.1080p.WEB-DL"},
		{name: "unknown_range", trailing: "missing_dates", title: "Fixture.Show.S01E11-E16.1080p.WEB-DL"},
		{name: "unknown_season_pack", trailing: "missing_rows", title: "Fixture.Show.S01.Complete.Season.1080p.WEB-DL"},
		{name: "future_range", trailing: "future", title: "Fixture.Show.S01E11-E16.1080p.WEB-DL"},
		{name: "future_season_pack", trailing: "future", title: "Fixture.Show.S01.Complete.Season.1080p.WEB-DL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFollowCoverageFixture(t, test.trailing, "complete", 10)
			f.adapter.searchTitle = test.title
			run := f.run(t)
			var reasons map[string]int
			_ = json.Unmarshal([]byte(run.FilterSummaryJSON), &reasons)
			if run.Status != models.FollowRunNoMatch || run.SearchedNamesCount == 0 || run.Selected != 0 || reasons["episode_not_confirmed"] < 1 {
				t.Fatalf("unsafe package submitted or no search performed: %+v", run)
			}
			var downloads int64
			if err := f.follows.db.Model(&models.DownloadTask{}).Where("follow_subscription_id = ?", f.subscription.ID).Count(&downloads).Error; err != nil || downloads != 0 {
				t.Fatalf("unsafe package created download count=%d err=%v", downloads, err)
			}
		})
	}
}

func TestFollowKnownCoverageAcceptsExactAndCompletePackages(t *testing.T) {
	for _, title := range []string{"Fixture.Show.S01E11.1080p.WEB-DL", "Fixture.Show.S01E01-E15.1080p.WEB-DL", "Fixture.Show.S01.Complete.Season.1080p.WEB-DL"} {
		t.Run(title, func(t *testing.T) {
			f := newFollowCoverageFixture(t, "aired", "complete", 10)
			f.adapter.searchTitle = title
			run := f.run(t)
			if run.Status != models.FollowRunSubmitted || run.Selected != 1 {
				t.Fatalf("confirmed episode package was rejected: %+v", run)
			}
		})
	}
}

func TestFollowUnknownCoverageCloudShareSelectsOnlyConfirmedMissingLeaves(t *testing.T) {
	for _, source := range []string{RouteSource115Share, RouteSourceQuarkShare} {
		t.Run(source, func(t *testing.T) {
			item := SiteSearchResult{Title: "Fixture.Show.S01.Complete.Season.1080p", SourceKind: source}
			candidate, reason, ok := buildFollowCandidate(item, 1, 0, 1, FollowExecutionSnapshot{})
			if !ok {
				t.Fatalf("share title rejected: %s", reason)
			}
			coverage := MediaCoverageSeason{SeasonNumber: 1, Counts: MediaCoverageCounts{Total: 24, Present: 10, Missing: 5, Unknown: 9}}
			for episode := 1; episode <= 24; episode++ {
				status := "unknown"
				if episode <= 10 {
					status = "present"
				} else if episode <= 15 {
					status = "missing"
				}
				coverage.Episodes = append(coverage.Episodes, MediaCoverageEpisode{EpisodeNumber: episode, Status: status})
			}
			candidate, ok = constrainFollowCandidateCoverage(candidate, coverage)
			if !ok {
				t.Fatal("share was rejected before its real files could be selected")
			}
			missing := map[[2]int]struct{}{{1, 11}: {}, {1, 12}: {}, {1, 13}: {}, {1, 14}: {}, {1, 15}: {}}
			tokens, episodes := selectFollowShareFiles([]SiteSharePreviewEntry{
				{Token: "old", Path: "Show/Show.S01E01.mkv"},
				{Token: "known", Path: "Show/Show.S01E11.mkv"},
				{Token: "known-sub", Path: "Show/Show.S01E11.zh.srt"},
				{Token: "unknown", Path: "Show/Show.S01E16.mkv"},
				{Token: "unknown-sub", Path: "Show/Show.S01E16.zh.srt"},
				{Token: "mixed-range", Path: "Show/Show.S01E12-E16.mkv"},
			}, 1, candidateMissingEpisodes(candidate, missing))
			if !reflect.DeepEqual(tokens, []string{"known", "known-sub"}) || !reflect.DeepEqual(episodes, []int{11}) {
				t.Fatalf("share selected nonmissing content: %v episodes %v", tokens, episodes)
			}
		})
	}
}

func TestFollowCompoundEpisodeDeclarationsCannotHideUncertainEpisodes(t *testing.T) {
	coverage := MediaCoverageSeason{SeasonNumber: 1, Counts: MediaCoverageCounts{Total: 24, Missing: 15, Unknown: 9}}
	for episode := 1; episode <= 24; episode++ {
		status := "unknown"
		if episode <= 15 {
			status = "missing"
		}
		coverage.Episodes = append(coverage.Episodes, MediaCoverageEpisode{EpisodeNumber: episode, Status: status})
	}
	for _, title := range []string{
		"Fixture.Show.S01E11-S01E16.1080p",
		"Fixture.Show.S01E11–E16.1080p",
		"Fixture.Show.S01E11.E16.1080p",
		"Fixture.Show.S01E11E12E16.1080p",
		"Fixture.Show.S01E11-E15.S02E01.1080p",
		"Fixture.Show.S01E11-E15.S02.Complete.1080p",
		"Fixture.Show.S01E11-E15-24.1080p",
		"Fixture.Show.S01E11-E00150.1080p",
	} {
		t.Run(title, func(t *testing.T) {
			candidate, _, ok := buildFollowCandidate(SiteSearchResult{Title: title}, 1, 0, 1, FollowExecutionSnapshot{})
			if ok {
				if _, safe := constrainFollowCandidateCoverage(candidate, coverage); safe {
					t.Errorf("torrent declaration lost part of its coverage: %v", candidate.Episodes)
				}
			}
			tokens, _ := selectFollowShareFiles([]SiteSharePreviewEntry{{Token: "unsafe", Path: title + ".mkv"}}, 1, []int{11, 12, 13, 14, 15})
			if len(tokens) != 0 {
				t.Errorf("share leaf lost part of its coverage: %v", tokens)
			}
		})
	}
}

func TestFollowCompleteLabelCannotEraseDeclaredEpisodeBounds(t *testing.T) {
	coverage := MediaCoverageSeason{SeasonNumber: 1, Counts: MediaCoverageCounts{Total: 15, Missing: 15}}
	for episode := 1; episode <= 15; episode++ {
		coverage.Episodes = append(coverage.Episodes, MediaCoverageEpisode{EpisodeNumber: episode, Status: "missing"})
	}
	candidate, _, ok := buildFollowCandidate(SiteSearchResult{Title: "Fixture.Show.S01.[01-24全集].1080p"}, 1, 0, 1, FollowExecutionSnapshot{})
	if ok {
		if _, safe := constrainFollowCandidateCoverage(candidate, coverage); safe {
			t.Fatalf("complete label erased the declared 24-episode bound: %v", candidate.Episodes)
		}
	}
}

func TestFollowKnownCompoundEpisodeBoundsRemainSelectable(t *testing.T) {
	coverage := MediaCoverageSeason{SeasonNumber: 1, Counts: MediaCoverageCounts{Total: 15, Missing: 15}}
	for episode := 1; episode <= 15; episode++ {
		coverage.Episodes = append(coverage.Episodes, MediaCoverageEpisode{EpisodeNumber: episode, Status: "missing"})
	}
	for _, title := range []string{"Show.S01E11-E15", "Show.S01E11-S01E15", "Show.S01E11–E15", "Show.S01.[11-15全集]"} {
		t.Run(title, func(t *testing.T) {
			candidate, reason, ok := buildFollowCandidate(SiteSearchResult{Title: title}, 1, 0, 1, FollowExecutionSnapshot{})
			if !ok {
				t.Fatalf("known declaration rejected: %s", reason)
			}
			if _, ok = constrainFollowCandidateCoverage(candidate, coverage); !ok {
				t.Fatal("known episode bounds rejected")
			}
			tokens, episodes := selectFollowShareFiles([]SiteSharePreviewEntry{{Token: "known", Path: title + ".mkv"}}, 1, []int{11, 12, 13, 14, 15})
			if !reflect.DeepEqual(tokens, []string{"known"}) || !reflect.DeepEqual(episodes, []int{11, 12, 13, 14, 15}) {
				t.Fatalf("known share range rejected: %v %v", tokens, episodes)
			}
		})
	}
}
