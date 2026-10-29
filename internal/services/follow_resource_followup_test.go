package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"gorm.io/gorm"
)

func TestFollowScreenshotTorznabTitlesKeepExactEpisode(t *testing.T) {
	season := 1
	coverage := MediaCoverageSeason{SeasonNumber: season, Counts: MediaCoverageCounts{Total: 24, Present: 10, Missing: 5, Unknown: 9}}
	for episode := 1; episode <= 24; episode++ {
		status := "present"
		if episode > 10 {
			status = "missing"
		}
		if episode > 15 {
			status = "unknown"
		}
		coverage.Episodes = append(coverage.Episodes, MediaCoverageEpisode{EpisodeNumber: episode, Status: status})
	}
	for _, episode := range []int{13, 15, 12, 1, 9, 4} {
		t.Run(fmt.Sprintf("E%02d", episode), func(t *testing.T) {
			title := fmt.Sprintf("Jia jin chai S01E%02d 1080p NF WEB-DL AAC 2 0 H 264-playWEB", episode)
			parsed, err := mediarecognition.Parse(mediarecognition.InputFacts{PackageName: title, SourceKind: mediarecognition.SourceDownload})
			if err != nil {
				t.Fatal(err)
			}
			if !mediaIdentityResultMatches(title, []tmdb.SearchName{{Value: "Jia jin chai"}}, "tv", nil, &season) {
				t.Fatalf("verified romanized alias rejected: %+v", parsed)
			}
			seeders := 15
			candidate, reason, ok := buildFollowCandidate(SiteSearchResult{Title: title, Seeders: &seeders}, 1, 0, season, FollowExecutionSnapshot{Filters: FollowFilters{MinSeeders: 1}})
			if !ok || !reflect.DeepEqual(candidate.Episodes, []int{episode}) {
				t.Fatalf("exact raw title produced episodes=%v reason=%s parsed=%+v", candidate.Episodes, reason, parsed)
			}
			if _, ok := constrainFollowCandidateCoverage(candidate, coverage); !ok {
				t.Fatalf("known episode rejected: %+v", candidate)
			}
		})
	}
}

type followPreviewDriver struct {
	*previewFixtureDriver
	readMu sync.Mutex
	reads  []string
	onRead func()
}

func (d *followPreviewDriver) InspectShare(ctx context.Context, raw string) (cloud.ShareSnapshot, error) {
	return d.InspectShareDirectory(ctx, raw, "0")
}

func (d *followPreviewDriver) InspectShareDirectory(ctx context.Context, raw, id string) (cloud.ShareSnapshot, error) {
	d.readMu.Lock()
	d.reads = append(d.reads, raw+":"+id)
	d.readMu.Unlock()
	if d.onRead != nil {
		d.onRead()
	}
	if ctx.Err() != nil {
		return cloud.ShareSnapshot{}, ctx.Err()
	}
	result := cloud.ShareSnapshot{ShareCode: "follow", ReceiveCode: "test"}
	if id == "0" {
		result.Items = []cloud.ShareItem{{ID: "work", Name: "Fixture Show(81)", IsDir: true}}
	} else if strings.Contains(raw, "/unusable") {
		result.Items = []cloud.ShareItem{{ID: "zip", Name: "2026.S01E11.1080p.zip", Size: 100}, {ID: "present", Name: "10.mkv", Size: 100}}
	} else {
		for episode := 11; episode <= 16; episode++ {
			result.Items = append(result.Items, cloud.ShareItem{ID: fmt.Sprintf("episode-%d", episode), Name: fmt.Sprintf("%d.mkv", episode), Size: 100})
		}
	}
	return result, nil
}

type followSharesAdapter struct{ *followResultsAdapter }

func (a *followSharesAdapter) ResolveSource(_ context.Context, _ sitepkg.Config, id string) (sitepkg.Source, error) {
	return sitepkg.Source{ShareURL: id, CloudProvider: "115"}, nil
}

func newFollowPreviewFixture(t *testing.T) (*followCoverageFixture, *followPreviewDriver) {
	t.Helper()
	f := newFollowCoverageFixture(t, "missing_dates", "complete", 10)
	sites := f.worker.sites
	driver := &followPreviewDriver{previewFixtureDriver: &previewFixtureDriver{&shareIngestDriver{&fakeCloudDriver{items: map[string]cloud.Item{
		"storage": {ID: "storage", ParentID: "0", Name: "媒体盘", IsDir: true},
		"library": {ID: "library", ParentID: "storage", Name: "媒体库", IsDir: true},
		"intake":  {ID: "intake", ParentID: "storage", Name: "中转", IsDir: true},
	}, children: map[string][]cloud.Item{}}}}}
	registry := cloud.NewRegistry()
	if err := registry.Register(cloud.ProviderPan115, func(cloud.Config) (cloud.Driver, error) { return driver, nil }); err != nil {
		t.Fatal(err)
	}
	connections := NewConnectionService(sites.db, sites.audit, sites.credentials, registry, zerolog.Nop())
	connection, err := connections.Create(f.actor, ConnectionInput{Name: "Follow115", Provider: cloud.ProviderPan115, Cookie: testPan115Cookie, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	sites.downloads.downloader.SetConnectionService(connections)
	storage := models.Storage{Name: "Follow cloud", NameNormalized: "follow-cloud", Type: models.StorageTypePan115, RootPath: "storage", RootDisplayPath: "/媒体盘", RootPathNormalized: "storage", ConnectionID: &connection.ID, Enabled: true, Capabilities: `{}`}
	if err := sites.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	if err := sites.db.Model(&models.MediaLibrary{}).Where("id = ?", f.libraryID).Updates(map[string]any{"storage_id": storage.ID, "provider_root_id": "library", "relative_root": "/媒体库"}).Error; err != nil {
		t.Fatal(err)
	}
	// The local fixture's complete scan cannot establish absence after changing
	// its source. Model a new complete enumeration of this exact cloud root;
	// retain the original local receipt so stale-source fencing remains tested.
	var library models.MediaLibrary
	var profile models.MediaClassificationProfile
	if err := sites.db.First(&library, f.libraryID).Error; err != nil {
		t.Fatal(err)
	}
	if err := sites.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	now := f.follows.now()
	generation := library.BaselineGeneration + 1
	library.DirtyGeneration = generation - 1
	if err := sites.db.Create(&models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "success", SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), Generation: generation, StartedAt: now, FinishedAt: &now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := sites.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{"baseline_generation": generation, "dirty_generation": generation, "last_successful_scan_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := sites.db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Update("last_generation", generation).Error; err != nil {
		t.Fatal(err)
	}
	coverage, err := f.follows.coverage.Coverage(context.Background(), f.actor, "tv", 100)
	if err != nil || coverage.TV == nil || coverage.TV.Counts != (MediaCoverageCounts{Total: 24, Present: 10, Missing: 5, Unknown: 9}) {
		t.Fatalf("cloud-source fixture lacks complete current evidence: %+v %v", coverage, err)
	}
	caps := downloadpkg.Capabilities{ShareReceive: true, NativeOffline: true, OutputConstraint: downloadpkg.OutputConstraintProviderStorage}
	if err := sites.downloads.downloader.registry.Register(models.DownloaderTypePan115Offline, caps, func(downloadpkg.Config) (downloadpkg.Client, error) { return &stubDownloadClient{}, nil }); err != nil {
		t.Fatal(err)
	}
	capsRaw, _ := json.Marshal(caps)
	receiver := models.Downloader{ID: "follow-preview-115", Name: "Follow115", NameNormalized: "follow115", Type: models.DownloaderTypePan115Offline, StorageID: &storage.ID, ProviderDirectoryID: "intake", ProviderDirectoryPath: "/中转", Enabled: true, CapabilitiesJSON: string(capsRaw)}
	if err := sites.db.Create(&receiver).Error; err != nil {
		t.Fatal(err)
	}
	adapter := &followSharesAdapter{&followResultsAdapter{stubSiteAdapter: &stubSiteAdapter{kind: "pansou_tg"}}}
	for _, id := range []string{"unusable", "valid"} {
		seeders := 1
		if id == "unusable" {
			seeders = 10
		}
		adapter.items = append(adapter.items, sitepkg.Result{PayloadKind: sitepkg.PayloadShare, TorrentID: "https://115.com/s/" + id + "?password=test", Title: "Fixture.Show.S01E11-E15.1080p.WEB-DL", SourceKind: RouteSource115Share, CloudProvider: "115", ResultType: "115", Seeders: &seeders})
	}
	sites.adapters["pansou_tg"] = adapter
	shareSite, err := sites.Create(context.Background(), f.actor, SiteInput{Name: "Follow previews", Kind: "pansou_tg", BaseURL: "https://follow-preview.example.test", CloudConfig: &sitepkg.CloudConfig{SourceScope: "plugin"}, Enabled: true, Priority: 1, TimeoutSeconds: 12, RateLimitPerMinute: 120}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	setFollowFixtureSnapshot(t, f, []uint{shareSite.ID}, 1)
	return f, driver
}

func TestFollowEmptySharePreviewContinuesToActualMissingLeaves(t *testing.T) {
	f, driver := newFollowPreviewFixture(t)
	sites := f.worker.sites
	run := f.run(t)
	var reasons map[string]int
	if err := json.Unmarshal([]byte(run.FilterSummaryJSON), &reasons); err != nil {
		t.Fatal(err)
	}
	if run.Selected != 1 || reasons["share_selection_failed"] != 1 {
		t.Fatalf("empty preview starved later valid share: run=%+v reads=%v", run, driver.reads)
	}
	var task models.DownloadTask
	if err := sites.db.Where("follow_subscription_id = ?", f.subscription.ID).First(&task).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := sites.downloads.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	var source downloadSourceEnvelope
	if err := json.Unmarshal([]byte(raw), &source); err != nil {
		t.Fatal(err)
	}
	if source.ShareSelection == nil || len(source.ShareSelection.Files) != 5 {
		t.Fatalf("selection did not retain exact missing leaves: %+v", source.ShareSelection)
	}
	for _, entry := range source.ShareSelection.Files {
		if entry.RelativePath == "Fixture Show(81)/16.mkv" {
			t.Fatal("unknown episode was selected")
		}
	}
}

func TestFollowParentCancellationStopsFurtherCandidatePreparation(t *testing.T) {
	f, driver := newFollowPreviewFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driver.onRead = cancel
	claimed, err := f.follows.queue.Claim([]string{JobTypeFollowSearch})
	if err != nil || claimed == nil {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	result := f.worker.Run(ctx, &followProgressRuntime{}, *claimed)
	if result.ErrorCode != "" {
		t.Fatalf("worker=%+v", result)
	}
	var run models.FollowRun
	if err := f.follows.db.First(&run, "job_id = ?", claimed.Job.ID).Error; err != nil {
		t.Fatal(err)
	}
	var tasks int64
	if err := f.follows.db.Model(&models.DownloadTask{}).Where("follow_subscription_id = ?", f.subscription.ID).Count(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if run.Status != models.FollowRunCancelled || tasks != 0 || len(driver.reads) != 1 {
		t.Fatalf("cancelled preparation continued: run=%+v tasks=%d reads=%v", run, tasks, driver.reads)
	}
}

func TestFollowScreenshotNumericLeavesUseVerifiedSeasonContext(t *testing.T) {
	entries := []SiteSharePreviewEntry{
		{Token: "folder", Path: "嫁金钗(81)", IsDir: true},
		{Token: "present", Path: "嫁金钗(81)/10.mkv"},
		{Token: "eleven", Path: "嫁金钗(81)/11.mkv"},
		{Token: "twelve", Path: "嫁金钗(81)/12.mkv"},
		{Token: "fourteen", Path: "嫁金钗(81)/14.mp4"},
		{Token: "fifteen-mp4", Path: "嫁金钗(81)/15.mp4"},
		{Token: "fifteen-mkv", Path: "嫁金钗(81)/15.mkv"},
		{Token: "future", Path: "嫁金钗(81)/16.mkv"},
		{Token: "sample", Path: "嫁金钗(81)/Samples/12.mkv"},
		{Token: "wrong-season", Path: "嫁金钗(81)/Season 02/12.mkv"},
		{Token: "unknown", Path: "嫁金钗(81)/bonus.mkv"},
	}
	for episode := 1; episode <= 9; episode++ {
		entries = append(entries, SiteSharePreviewEntry{Token: fmt.Sprintf("zip-%d", episode), Path: fmt.Sprintf("嫁金钗(81)/2026.S01E%02d.2160p.HQ.60fps.10bit.AAC-PanWEB.zip", episode)})
	}
	tokens, episodes := selectFollowShareFiles(entries, 1, []int{11, 12, 13, 14, 15}, "嫁金钗.S01.Complete.1080p.WEB-DL")
	if !reflect.DeepEqual(episodes, []int{11, 12, 14, 15}) {
		t.Fatalf("real bare-number leaves were not recognized: tokens=%v episodes=%v", tokens, episodes)
	}
	if !reflect.DeepEqual(tokens, []string{"eleven", "fifteen-mkv", "fifteen-mp4", "fourteen", "twelve"}) {
		t.Fatalf("existing multiple-video-per-episode behavior changed: %v", tokens)
	}
	for _, token := range tokens {
		if token != "eleven" && token != "twelve" && token != "fourteen" && token != "fifteen-mp4" && token != "fifteen-mkv" {
			t.Fatalf("unproven file selected: %s", token)
		}
	}
}

func TestFollowNumericLeavesRequireUnambiguousVerifiedContext(t *testing.T) {
	const verified = "嫁金钗.S01.Complete.1080p.WEB-DL"
	for _, tt := range []struct {
		path, title string
		wanted      []int
		want        bool
	}{
		{"12.mkv", verified, []int{12}, true},
		{"嫁金钗(81)/Season 01/012.mkv", verified, []int{12}, true},
		{"嫁金钗(81)/第1季/12.mkv", verified, []int{12}, true},
		{"嫁金钗(81)/12.mkv", "", []int{12}, false},
		{"嫁金钗(81)/12.mkv", "嫁金钗.1080p", []int{12}, false},
		{"嫁金钗(81)/12.mkv", "嫁金钗.S02.1080p", []int{12}, false},
		{"嫁金钗(81)/Season 02/12.mkv", verified, []int{12}, false},
		{"嫁金钗(81)/S01/S02/12.mkv", verified, []int{12}, false},
		{"嫁金钗(81)/S01S02/12.mkv", verified, []int{12}, false},
		{"嫁金钗(81)/Season 01 Season 02/12.mkv", verified, []int{12}, false},
		{"嫁金钗(81)/第二季/12.mkv", verified, []int{12}, false},
		{"嫁金钗(81)/S01E12/12.mkv", verified, []int{12}, false},
		{"另一个作品/12.mkv", verified, []int{12}, false},
		{"嫁金钗(81)/Sample/12.mkv", verified, []int{12}, false},
		{"嫁金钗(81)/12.sample.mkv", verified, []int{12}, false},
		{"嫁金钗(81)/花絮/12.mkv", verified, []int{12}, false},
		{"嫁金钗(81)/BDMV/STREAM/0012.m2ts", verified, []int{12}, false},
		{"嫁金钗(81)/1080.mkv", verified, []int{1080}, false},
		{"嫁金钗(81)/720.mp4", verified, []int{720}, false},
		{"嫁金钗(81)/2026.mkv", verified, []int{2026}, false},
		{"嫁金钗(81)/0.mkv", verified, []int{0}, false},
		{"嫁金钗(81)/201.mkv", verified, []int{201}, false},
		{"嫁金钗(81)/12.mkv", verified, []int{13}, false},
		{"嫁金钗(81)/11-16.mkv", verified, []int{11, 12, 13, 14, 15}, false},
		{"嫁金钗(81)/S01E11-E16.mkv", verified, []int{11, 12, 13, 14, 15}, false},
		{"嫁金钗(81)/2026.S01E12.1080p.zip", verified, []int{12}, false},
		{"嫁金钗(81)/bonus.mkv", verified, []int{81}, false},
	} {
		t.Run(tt.path+"/"+tt.title, func(t *testing.T) {
			tokens, episodes := selectFollowShareFiles([]SiteSharePreviewEntry{{Token: "leaf", Path: tt.path}}, 1, tt.wanted, tt.title)
			if (len(tokens) > 0) != tt.want || (len(episodes) > 0) != tt.want {
				t.Fatalf("tokens=%v episodes=%v", tokens, episodes)
			}
		})
	}
}

type followResultsAdapter struct {
	*stubSiteAdapter
	items     []sitepkg.Result
	requested []string
}

func (a *followResultsAdapter) Search(_ context.Context, _ sitepkg.Config, query sitepkg.Query) (sitepkg.Page, error) {
	return sitepkg.Page{Page: query.Page, Items: a.items}, nil
}

func (a *followResultsAdapter) Download(ctx context.Context, config sitepkg.Config, id string) ([]byte, string, error) {
	a.requested = append(a.requested, id)
	return a.stubSiteAdapter.Download(ctx, config, "42")
}

func setFollowFixtureSnapshot(t *testing.T, f *followCoverageFixture, sites []uint, limit int) {
	t.Helper()
	var record models.FollowSubscription
	if err := f.follows.db.First(&record, "id = ?", f.subscription.ID).Error; err != nil {
		t.Fatal(err)
	}
	var snapshot FollowExecutionSnapshot
	if err := json.Unmarshal([]byte(record.ExecutionSnapshotJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if sites != nil {
		snapshot.SiteIDs = sites
	}
	snapshot.MaxResourcesPerRun = limit
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.follows.db.Model(&record).Update("execution_snapshot_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.follows.db.Model(&models.FollowRun{}).Where("subscription_id = ?", record.ID).Update("execution_snapshot_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
}

func followSingleEpisodeResults() []sitepkg.Result {
	items := []sitepkg.Result{}
	for episode := 11; episode <= 13; episode++ {
		seeders := 100 - episode
		items = append(items, sitepkg.Result{TorrentID: fmt.Sprintf("episode-%d", episode), Title: fmt.Sprintf("Fixture.Show.S01E%02d.1080p.WEB-DL", episode), SizeBytes: 1024, Seeders: &seeders})
	}
	return items
}

func TestFollowUnusableSharesDoNotExhaustAcceptedResourceLimit(t *testing.T) {
	for _, shares := range []int{3, 50} {
		t.Run(fmt.Sprintf("shares-%d", shares), func(t *testing.T) {
			f := newFollowCoverageFixture(t, "missing_dates", "complete", 10)
			sites := f.worker.sites
			var original models.Site
			if err := sites.db.Where("kind = ?", "pttime").First(&original).Error; err != nil {
				t.Fatal(err)
			}
			pt := &followResultsAdapter{stubSiteAdapter: f.adapter, items: followSingleEpisodeResults()}
			sites.adapters["pttime"] = pt
			cloud := &followResultsAdapter{stubSiteAdapter: &stubSiteAdapter{kind: "pansou_tg"}}
			for n := 0; n < shares; n++ {
				cloud.items = append(cloud.items, sitepkg.Result{TorrentID: fmt.Sprintf("https://115.com/s/follow%03d?password=test", n), Title: "Fixture.Show.S01E11-E15.1080p.WEB-DL", SourceKind: RouteSource115Share, CloudProvider: "115", ResultType: "115"})
			}
			sites.adapters["pansou_tg"] = cloud
			shareSite, err := sites.Create(context.Background(), f.actor, SiteInput{Name: "Follow shares", Kind: "pansou_tg", BaseURL: "https://follow-shares.example.test", CloudConfig: &sitepkg.CloudConfig{SourceScope: "plugin", TypePolicy: "manual", CloudTypes: []string{"115"}}, Enabled: true, Priority: 1, TimeoutSeconds: 12, RateLimitPerMinute: 120}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			setFollowFixtureSnapshot(t, f, []uint{shareSite.ID, original.ID}, 1)
			run := f.run(t)
			var reasons map[string]int
			if err := json.Unmarshal([]byte(run.FilterSummaryJSON), &reasons); err != nil {
				t.Fatal(err)
			}
			wantAccepted := 1
			if shares == 50 {
				wantAccepted = 0
			}
			if run.Selected != wantAccepted || len(pt.requested) != wantAccepted {
				t.Fatalf("unusable shares starved later PT or exceeded limit: run=%+v requests=%v", run, pt.requested)
			}
			if reasons["submit_failed"] != shares {
				t.Fatalf("route failures=%v", reasons)
			}
			if shares == 50 && reasons["candidate_budget"] != 1 {
				t.Fatalf("bounded work was not reported: %+v", run)
			}
			var tasks []models.DownloadTask
			if err := sites.db.Where("follow_subscription_id = ?", f.subscription.ID).Find(&tasks).Error; err != nil {
				t.Fatal(err)
			}
			if len(tasks) != wantAccepted {
				t.Fatalf("accepted task count=%d", len(tasks))
			}
			if wantAccepted == 1 && (!strings.Contains(tasks[0].DisplayName, "E11") || run.Candidates != 6) {
				t.Fatalf("wrong replacement candidate: run=%+v tasks=%+v", run, tasks)
			}
		})
	}
}

func TestFollowUncertainHandoffKeepsOriginalClaimAcrossRuns(t *testing.T) {
	f := newFollowCoverageFixture(t, "missing_dates", "complete", 10)
	items := followSingleEpisodeResults()
	alternate := items[0]
	alternate.TorrentID = "alternate-episode-11"
	seeders := 1
	alternate.Seeders = &seeders
	adapter := &followResultsAdapter{stubSiteAdapter: f.adapter, items: []sitepkg.Result{items[0], alternate}}
	f.worker.sites.adapters["pttime"] = adapter
	if err := f.follows.db.Callback().Create().After("gorm:create").Register("follow:test:uncertain", func(tx *gorm.DB) {
		if tx.Statement.Table == "download_tasks" {
			tx.AddError(errors.New("fixture persistence acknowledgement unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.follows.db.Callback().Create().Remove("follow:test:uncertain") })
	first := f.run(t)
	if first.Selected != 0 || len(adapter.requested) != 1 {
		t.Fatalf("uncertain handoff tried another resource: run=%+v requests=%v", first, adapter.requested)
	}
	var claim models.FollowEpisodeClaim
	if err := f.follows.db.Where("subscription_id = ? AND episode_number = 11", f.subscription.ID).First(&claim).Error; err != nil {
		t.Fatal(err)
	}
	if claim.State != "queued" || claim.DownloadTaskID != nil {
		t.Fatalf("original uncertain reservation was released: %+v", claim)
	}
	if err := f.follows.db.Callback().Create().Remove("follow:test:uncertain"); err != nil {
		t.Fatal(err)
	}
	adapter.items = []sitepkg.Result{alternate}
	if _, err := f.follows.Enqueue(context.Background(), f.actor, f.subscription.ID, "manual", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	second := f.run(t)
	if second.Selected != 0 || len(adapter.requested) != 1 {
		t.Fatalf("alternate replaced uncertain claim on next run: %+v %v", second, adapter.requested)
	}
	adapter.items = []sitepkg.Result{items[0]}
	f.adapter.downloadErr = sitepkg.ErrUnavailable
	if _, err := f.follows.Enqueue(context.Background(), f.actor, f.subscription.ID, "manual", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	readFailed := f.run(t)
	var retained models.FollowEpisodeClaim
	if err := f.follows.db.Where("subscription_id = ? AND episode_number = 11", f.subscription.ID).First(&retained).Error; err != nil {
		t.Fatal(err)
	}
	if readFailed.Selected != 0 || retained.State != "queued" || retained.ResourceFingerprint != claim.ResourceFingerprint {
		t.Fatalf("a later source-read failure erased earlier uncertainty: run=%+v claim=%+v", readFailed, retained)
	}
	f.adapter.downloadErr = nil
	if _, err := f.follows.Enqueue(context.Background(), f.actor, f.subscription.ID, "manual", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	third := f.run(t)
	if third.Selected != 1 || len(adapter.requested) != 3 {
		t.Fatalf("original fingerprint could not recover: %+v %v", third, adapter.requested)
	}
	var recovered models.FollowEpisodeClaim
	if err := f.follows.db.Where("subscription_id = ? AND episode_number = 11", f.subscription.ID).First(&recovered).Error; err != nil {
		t.Fatal(err)
	}
	if recovered.DownloadTaskID == nil || recovered.ResourceFingerprint != claim.ResourceFingerprint {
		t.Fatalf("recovery lost original identity: %+v", recovered)
	}
}

func TestFollowRecoversAcceptedTaskBeforeSourceReads(t *testing.T) {
	f := newFollowCoverageFixture(t, "missing_dates", "complete", 10)
	adapter := &followResultsAdapter{stubSiteAdapter: f.adapter, items: followSingleEpisodeResults()[:1]}
	f.worker.sites.adapters["pttime"] = adapter
	first := f.run(t)
	if first.Selected != 1 {
		t.Fatalf("fixture did not create original task: %+v", first)
	}
	var claim models.FollowEpisodeClaim
	if err := f.follows.db.Where("subscription_id = ? AND episode_number = 11", f.subscription.ID).First(&claim).Error; err != nil {
		t.Fatal(err)
	}
	originalID := *claim.DownloadTaskID
	if err := f.follows.db.Model(&claim).Updates(map[string]any{"state": "queued", "download_task_id": nil}).Error; err != nil {
		t.Fatal(err)
	}
	f.adapter.downloadErr = sitepkg.ErrUnavailable
	if _, err := f.follows.Enqueue(context.Background(), f.actor, f.subscription.ID, "manual", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	var recovered models.FollowEpisodeClaim
	if err := f.follows.db.Where("subscription_id = ? AND episode_number = 11", f.subscription.ID).First(&recovered).Error; err != nil {
		t.Fatal(err)
	}
	var tasks int64
	if err := f.follows.db.Model(&models.DownloadTask{}).Where("follow_subscription_id = ?", f.subscription.ID).Count(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if tasks != 1 || len(adapter.requested) != 1 || recovered.DownloadTaskID == nil || *recovered.DownloadTaskID != originalID || recovered.ResourceFingerprint != claim.ResourceFingerprint {
		t.Fatalf("accepted task was replayed or replaced: tasks=%d requests=%v claim=%+v", tasks, adapter.requested, recovered)
	}
}
