package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
	"gorm.io/gorm"
)

type preferenceMixedAdapter struct {
	*followSharesAdapter
	metainfo     []byte
	torrentReads int
}

func (a *preferenceMixedAdapter) ResolveSource(ctx context.Context, cfg sitepkg.Config, id string) (sitepkg.Source, error) {
	if strings.Contains(id, "https://115.com/s/") {
		return a.followSharesAdapter.ResolveSource(ctx, cfg, id)
	}
	a.torrentReads++
	return sitepkg.Source{Magnet: id}, nil
}

func TestFollowTransferFirstUsesVerifiedShareThenOnlyRemainingTorrentLeaf(t *testing.T) {
	for _, mode := range []string{"accepted", "title-unknown", "wrong-title", "preparation-deadline", "read-rejected", "cooldown-recovered", "uncertain-write", "offline-zero", "offline-unknown", "live-zero", "live-unknown", "resumed-share-first", "dense-magnets"} {
		t.Run(mode, func(t *testing.T) {
			f, driver := newFollowPreviewFixture(t)
			// This fixture's final library is on 115. Give its same-storage native
			// executor the exact-file capability before route preflight; no worker or
			// physical download is started by the acquisition plan assertions below.
			registry := downloadpkg.NewRegistry()
			if err := registry.Register(models.DownloaderTypePan115Offline, downloadpkg.Capabilities{CanTryWithoutSeeders: mode != "live-zero" && mode != "live-unknown", ShareReceive: true, NativeOffline: true, SelectiveFiles: true, Pause: true, Resume: true, OutputConstraint: downloadpkg.OutputConstraintProviderStorage}, func(downloadpkg.Config) (downloadpkg.Client, error) { return &stubDownloadClient{}, nil }); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(mode, "offline-") || strings.HasPrefix(mode, "live-") {
				if err := registry.Register(models.DownloaderTypeQBittorrent, downloadpkg.Capabilities{SelectiveFiles: true, Pause: true, Resume: true}, func(downloadpkg.Config) (downloadpkg.Client, error) { return &stubDownloadClient{}, nil }); err != nil {
					t.Fatal(err)
				}
				if err := f.follows.db.Model(&models.Downloader{}).Where("type = ?", models.DownloaderTypePan115Offline).Update("sort_order", 10).Error; err != nil {
					t.Fatal(err)
				}
			}
			f.worker.sites.downloads.downloader.registry = registry
			driver.readItems = []cloud.ShareItem{{ID: "episode-13", Name: "Fixture.Show.S01E13.mkv", Size: 100}}
			base := f.worker.sites.adapters["pansou_tg"].(*followSharesAdapter)
			base.items = base.items[1:]
			base.items = append(base.items, sitepkg.Result{PayloadKind: sitepkg.PayloadMagnet, Title: "Fixture.Show.S01E13-E15.1080p.WEB-DL", SourceKind: RouteSourceBT, ResultType: "magnet"})
			adapter := &preferenceMixedAdapter{followSharesAdapter: base, metainfo: inspectionTorrentWithSize(32<<20, "Fixture.Show.S01E13.mkv", "Fixture.Show.S01E15.mkv")}
			if mode == "title-unknown" {
				base.items[len(base.items)-1].Title = "Fixture.Show.1080p.WEB-DL"
				adapter.metainfo = inspectionTorrentWithSize(32<<20, "Fixture.Show.S01E13.mkv", "Fixture.Show.S01E15.mkv", "Other.Work.S01E12.mkv", "Fixture.Show.S01E16.mkv")
			} else if mode == "wrong-title" {
				base.items[len(base.items)-1].Title = "Other.Work.1080p.WEB-DL"
			}
			if strings.HasSuffix(mode, "zero") {
				zero := 0
				base.items[len(base.items)-1].Seeders = &zero
			}
			if mode == "dense-magnets" {
				for index := 1; index <= 51; index++ {
					seeds := 100
					base.items = append(base.items, sitepkg.Result{PayloadKind: sitepkg.PayloadMagnet, TorrentID: fmt.Sprintf("magnet:?xt=urn:btih:%040x", index), Title: "Fixture.Show.S01E13-E15.1080p.WEB-DL", SourceKind: RouteSourceBT, ResultType: "magnet", Seeders: &seeds})
				}
			}
			f.worker.sites.adapters["pansou_tg"] = adapter
			// Enable the preference on a frozen fixture snapshot before the run.
			var subscription models.FollowSubscription
			if err := f.follows.db.First(&subscription, "id=?", f.subscription.ID).Error; err != nil {
				t.Fatal(err)
			}
			var snapshot FollowExecutionSnapshot
			if err := json.Unmarshal([]byte(subscription.ExecutionSnapshotJSON), &snapshot); err != nil {
				t.Fatal(err)
			}
			snapshot.Filters.AcquisitionPreference = FollowAcquisitionTransferFirst
			if strings.HasPrefix(mode, "offline-") || strings.HasPrefix(mode, "live-") {
				snapshot.Filters.MinSeeders = 1
			}
			if mode == "preparation-deadline" {
				// Both candidates enter one read window; the magnet stays owned
				// until cancellation while the share has time to prepare normally.
				snapshot.Filters.AcquisitionPreference = FollowAcquisitionExistingOrder
				f.worker.preparationTimeout = 2 * time.Second
			}
			snapshot.MaxResourcesPerRun = 2
			searchOrder := []string{}
			if mode == "resumed-share-first" {
				var native models.Site
				if err := f.follows.db.Where("kind = ?", "pttime").First(&native).Error; err != nil {
					t.Fatal(err)
				}
				f.worker.sites.adapters["pttime"] = &followResultsAdapter{stubSiteAdapter: f.adapter, items: followSingleEpisodeResults(), onSearch: func(string) { searchOrder = append(searchOrder, "bt") }}
				base.onSearch = func(string) { searchOrder = append(searchOrder, "share") }
				snapshot.SiteIDs = append([]uint{native.ID}, snapshot.SiteIDs...)
			}
			if mode == "resumed-share-first" || mode == "dense-magnets" {
				snapshot.MaxResourcesPerRun = 1
			}
			raw, _ := json.Marshal(snapshot)
			for _, model := range []any{&models.FollowSubscription{}, &models.FollowRun{}} {
				column := "subscription_id"
				if _, ok := model.(*models.FollowSubscription); ok {
					column = "id"
				}
				if err := f.follows.db.Model(model).Where(column+"=?", subscription.ID).Update("execution_snapshot_json", string(raw)).Error; err != nil {
					t.Fatal(err)
				}
			}
			if mode == "read-rejected" {
				driver.readErr = cloud.Error(cloud.CodeShareInvalid, false, nil)
			}
			if mode == "cooldown-recovered" {
				driver.readErr = cloud.WithRetryNotBefore(cloud.Error(cloud.CodeRateLimited, true, nil), f.follows.now().Add(5*time.Minute), cloud.RetryReasonProviderCooldown)
				adapter.metainfo = inspectionTorrentWithSize(32<<20, "Fixture.Show.S01E11.mkv", "Fixture.Show.S01E12.mkv", "Fixture.Show.S01E13.mkv", "Fixture.Show.S01E14.mkv", "Fixture.Show.S01E15.mkv")
			}
			manifest, err := resourcecontent.ParseTorrent(adapter.metainfo, resourcecontent.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			base.items[len(base.items)-1].TorrentID = "magnet:?xt=urn:btih:" + manifest.Identity.V1
			if mode == "resumed-share-first" {
				var run models.FollowRun
				var job models.Job
				if err := f.follows.db.Where("subscription_id = ?", subscription.ID).First(&run).Error; err != nil {
					t.Fatal(err)
				}
				if err := f.follows.db.First(&job, "id = ?", run.JobID).Error; err != nil {
					t.Fatal(err)
				}
				missing := map[[2]int]struct{}{}
				for e := 11; e <= 15; e++ {
					missing[[2]int{1, e}] = struct{}{}
				}
				checkpoint, err := f.worker.preparationCheckpoint(context.Background(), run, subscription, job, coordinateList(missing))
				if err != nil {
					t.Fatal(err)
				}
				// Simulate a pre-fix cursor in a BT-biased cycle. It must restart
				// at the new share-first boundary without carrying content proof.
				checkpoint.PreparationScope = inspectionDigest([]any{run.OwnerID, run.SubscriptionID, run.SubscriptionRevision, run.LifecycleRevision, coordinateList(missing)})
				checkpoint.PreparationAfter = &followCandidatePosition{Fingerprint: privateResultFingerprint(snapshot.SiteIDs[0], "episode-11"), Season: 1}
				frozen, _ := json.Marshal(checkpoint)
				if err := f.follows.db.Model(&job).Update("checkpoint_json", string(frozen)).Error; err != nil {
					t.Fatal(err)
				}
			}
			f.worker.sites.SetResourceContentResolver(contentResolverFunc(func(ctx context.Context, input resourcecontent.ResolveRequest) (resourcecontent.Manifest, error) {
				if input.URI != base.items[len(base.items)-1].TorrentID {
					return resourcecontent.Manifest{}, errors.New("fixture magnet identity mismatch")
				}
				if mode == "preparation-deadline" {
					<-ctx.Done()
					return resourcecontent.Manifest{}, ctx.Err()
				}
				return manifest, nil
			}))
			if mode == "uncertain-write" {
				if err := f.follows.db.Callback().Create().After("gorm:create").Register("preference:uncertain", func(tx *gorm.DB) {
					if tx.Statement.Table == "download_tasks" {
						tx.AddError(errors.New("fixture acknowledgement lost"))
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.follows.db.Callback().Create().Remove("preference:uncertain") })
			}
			run := f.run(t)
			if mode == "uncertain-write" {
				var claims []models.FollowEpisodeClaim
				if err := f.follows.db.Where("subscription_id=? AND state <> ?", subscription.ID, "imported").Find(&claims).Error; err != nil {
					t.Fatal(err)
				}
				if run.Selected != 0 || adapter.torrentReads != 0 || len(claims) != 1 || claims[0].EpisodeNumber != 13 || claims[0].State != "queued" {
					t.Fatalf("uncertain write switched source: selected=%d reads=%d claims=%+v", run.Selected, adapter.torrentReads, claims)
				}
				return
			}
			var tasks []models.DownloadTask
			if err := f.follows.db.Where("follow_subscription_id=?", subscription.ID).Order("created_at,id").Find(&tasks).Error; err != nil {
				t.Fatal(err)
			}
			want := 2
			if mode == "read-rejected" || mode == "cooldown-recovered" || mode == "wrong-title" || mode == "preparation-deadline" || strings.HasPrefix(mode, "live-") || mode == "resumed-share-first" || mode == "dense-magnets" {
				want = 1
			}
			if run.Selected != want || len(tasks) != want {
				t.Fatalf("mode=%s selected=%d tasks=%d error=%s", mode, run.Selected, len(tasks), run.ErrorMessage)
			}
			if mode == "resumed-share-first" || mode == "dense-magnets" || strings.HasPrefix(mode, "live-") {
				if adapter.torrentReads != 0 || len(driver.reads) == 0 {
					t.Fatalf("share was hidden behind BT paging/admission or live policy bypassed: bt=%d shares=%d", adapter.torrentReads, len(driver.reads))
				}
				if mode == "resumed-share-first" && (len(searchOrder) == 0 || searchOrder[0] != "share") {
					t.Fatalf("share-capable source was not queried first: %v", searchOrder)
				}
			}
			if mode == "wrong-title" && adapter.torrentReads != 0 {
				t.Fatal("unrelated title admitted a content read")
			}
			if mode == "preparation-deadline" {
				var reasons map[string]int
				if err := json.Unmarshal([]byte(run.FilterSummaryJSON), &reasons); err != nil {
					t.Fatal(err)
				}
				if run.Status != models.FollowRunSubmitted || run.ErrorCode != "follow_candidate_budget" || reasons["candidate_checks_started"] != 2 || reasons["candidate_checks_deferred"] != 1 || reasons["submit_failed"] != 0 || reasons["content_preparation_failed"] != 0 {
					t.Fatalf("spent read window invalidated prepared share or invented a submit failure: run=%+v reasons=%v", run, reasons)
				}
			}
			if mode == "cooldown-recovered" {
				if run.Status != models.FollowRunSubmitted || run.ErrorCode != "" || run.ErrorMessage != "" || run.RetryNotBefore != nil {
					t.Fatalf("earlier failed candidate misreported as pending failure: %+v", run)
				}
				return
			}
			for _, task := range tasks {
				plaintext, err := f.worker.sites.downloads.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
				if err != nil {
					t.Fatal(err)
				}
				var frozen downloadSourceEnvelope
				if err := json.Unmarshal([]byte(plaintext), &frozen); err != nil {
					t.Fatal(err)
				}
				if len(frozen.Torrent) > 0 {
					if strings.HasPrefix(mode, "offline-") && (task.DownloaderID == nil || *task.DownloaderID != "follow-preview-115") {
						t.Fatal("offline seed exception leaked to live-swarm route")
					}
					if frozen.ContentSelection == nil {
						t.Fatal("torrent exact selection missing")
					}
					indices := []int{}
					for _, file := range frozen.ContentSelection.Files {
						indices = append(indices, file.Index)
					}
					wantIndices := []int{1}
					if mode == "read-rejected" {
						wantIndices = []int{0, 1}
					}
					if !reflect.DeepEqual(indices, wantIndices) {
						t.Fatalf("torrent plan repeated accepted E13: %v", indices)
					}
				}
			}
		})
	}
}
