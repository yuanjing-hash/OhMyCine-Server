package services

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

type cleanupCloudDriver struct {
	*fakeCloudDriver
	listErr   error
	partial   bool
	lostACK   bool
	listCalls int
}

func (d *cleanupCloudDriver) List(ctx context.Context, id string, p cloudpkg.PageRequest) (cloudpkg.Page, error) {
	d.listCalls++
	if d.listErr != nil {
		return cloudpkg.Page{}, d.listErr
	}
	if d.partial {
		return cloudpkg.Page{HasMore: true}, nil
	}
	return d.fakeCloudDriver.List(ctx, id, p)
}
func (d *cleanupCloudDriver) Recycle(ctx context.Context, id string) error {
	if err := d.fakeCloudDriver.Recycle(ctx, id); err != nil {
		return err
	}
	if d.lostACK {
		d.lostACK = false
		return cloudpkg.Error(cloudpkg.CodeMutationUnknown, true, errors.New("private upstream token"))
	}
	return nil
}

type cloudCleanupFixture struct {
	s       *MediaArtifactService
	library models.MediaLibrary
	storage models.Storage
	scan    models.MediaLibraryScanRun
	policy  mediaArtifactPolicy
	run     models.MediaArtifactRun
	claim   ClaimedJob
	permit  CatalogPhysicalWritePermit
	driver  *cleanupCloudDriver
}

func newCloudCleanupFixture(t *testing.T) cloudCleanupFixture {
	t.Helper()
	management, queue, _, library, _ := strmManagementFixture(t)
	db := management.db
	library.CloudEmptyCleanupEnabled = true
	library.ProviderRootID = "root"
	library.STRMEnabled = false
	library.SignedProxyEnabled = false
	library.STRMLocalRoot = ""
	library.MetadataArtifactsEnabled = false
	if err := db.Save(&library).Error; err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	if err := db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	driver := &cleanupCloudDriver{fakeCloudDriver: &fakeCloudDriver{recycleItems: true, items: map[string]cloudpkg.Item{}, children: map[string][]cloudpkg.Item{}}}
	show := cloudpkg.Item{ID: "show", ParentID: "root", Name: "Show", IsDir: true}
	season := cloudpkg.Item{ID: "season", ParentID: "show", Name: "Season02", IsDir: true}
	driver.items[show.ID] = show
	driver.items[season.ID] = season
	driver.children["root"] = []cloudpkg.Item{show}
	driver.children["show"] = []cloudpkg.Item{season}
	scan := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: library.ArtifactGeneration, Kind: "event", Status: "success", Partial: true, StartedAt: time.Now().UTC(), CheckpointJSON: "{}"}
	if err := db.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	result := medialibrary.Result{Directories: []cloudpkg.TreeEntry{{Item: show, RelativePath: "/Show"}, {Item: season, RelativePath: "/Show/Season02"}}, Files: []medialibrary.File{{ProviderID: "removed", RelativePath: "/Show/Season02/01.mkv"}}}
	if err := stageProviderPaths(context.Background(), db, library, storage, result, scan); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return persistProviderPathsTx(tx, library, storage, result, scan.StartedAt, scan.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := freezeCloudCleanupDirectoriesTx(db, &scan, []string{"removed"}); err != nil {
		t.Fatal(err)
	}
	var frozen struct {
		Directories []cloudCleanupDirectory `json:"cloud_cleanup_directories"`
	}
	if err := json.Unmarshal([]byte(scan.CheckpointJSON), &frozen); err != nil {
		t.Fatal(err)
	}
	if len(frozen.Directories) != 2 {
		t.Fatalf("real staged file parent relation not resolved: %s", scan.CheckpointJSON)
	}
	if err := db.Save(&scan).Error; err != nil {
		t.Fatal(err)
	}
	policy := mediaArtifactPolicy{LibraryID: library.ID, StorageID: storage.ID, StorageType: storage.Type, ConnectionID: *storage.ConnectionID, Generation: library.ArtifactGeneration, TargetKind: artifactTargetCloudCleanup, CloudEmptyCleanupEnabled: true, CloudCleanupDirectories: frozen.Directories, SourceBoundaryFingerprint: catalogSourceFingerprint(library, storage)}
	raw, _ := json.Marshal(policy)
	run := models.MediaArtifactRun{ID: "cloud-run", LibraryID: library.ID, Generation: library.ArtifactGeneration, PolicyJSON: string(raw), Status: models.MediaArtifactStatusQueued}
	job, err := queue.Enqueue(EnqueueJobInput{System: true, JobType: JobTypeMediaArtifact, Priority: 100, DisplayName: "cloud cleanup", Provider: "media_library", ResourceKey: mediaArtifactResourceKey(library.ID), CoalescingKey: run.ID, Payload: mediaArtifactJobPayload{ArtifactRunID: run.ID}})
	if err != nil {
		t.Fatal(err)
	}
	run.JobID = &job.ID
	input := CatalogPhysicalWriteInput{LibraryID: library.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: run.ID, ArtifactReceiptVersion: 1}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return RegisterCatalogPhysicalOwnerTx(tx, input, func(tx *gorm.DB) error { return tx.Create(&run).Error })
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim([]string{JobTypeMediaArtifact})
	if err != nil || claim == nil {
		t.Fatalf("claim %v %v", claim, err)
	}
	input.Job = claim
	var permit CatalogPhysicalWritePermit
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&run).Update("status", models.MediaArtifactStatusRunning).Error; err != nil {
			return err
		}
		var err error
		permit, err = EnterCatalogPhysicalWriteTx(tx, input)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s := NewMediaArtifactService(db, queue, nil, zerolog.Nop())
	s.SetConnectionService(&ConnectionService{db: db, drivers: map[uint]cloudpkg.Driver{*storage.ConnectionID: driver}})
	return cloudCleanupFixture{s, library, storage, scan, policy, run, *claim, permit, driver}
}
func (f cloudCleanupFixture) execute() error {
	return f.s.cleanupCloudEmptyDirectories(context.Background(), f.claim, f.permit, f.run, f.policy)
}

func TestCloudEmptyCleanupExactEmptyAndUserFiles(t *testing.T) {
	for _, file := range []string{"", "poster.jpg", "movie.nfo", "subtitle.srt", "video.mkv"} {
		name := file
		if name == "" {
			name = "empty-directory"
		}
		t.Run(name, func(t *testing.T) {
			f := newCloudCleanupFixture(t)
			if file != "" {
				f.driver.children["season"] = []cloudpkg.Item{{ID: "user-file", Name: file, ParentID: "season"}}
			}
			if err := f.execute(); err != nil {
				t.Fatal(err)
			}
			want := []string{"season", "show"}
			if file != "" {
				want = nil
			}
			if !reflect.DeepEqual(f.driver.recycled, want) {
				t.Fatalf("recycled=%v want=%v", f.driver.recycled, want)
			}
			if err := f.execute(); err != nil {
				t.Fatal("idempotent replay", err)
			}
			if len(f.driver.purged) != 0 {
				t.Fatal("permanent delete")
			}
		})
	}
}
func TestCloudEmptyCleanupFailureNeverMeansEmpty(t *testing.T) {
	for _, scenario := range []string{"auth", "timeout", "partial", "moved", "disabled", "root"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCloudCleanupFixture(t)
			switch scenario {
			case "auth":
				f.driver.listErr = cloudpkg.Error(cloudpkg.CodeAuthExpired, false, errors.New("private cookie"))
			case "timeout":
				f.driver.listErr = context.DeadlineExceeded
			case "partial":
				f.driver.partial = true
			case "moved":
				d := f.driver.items["show"]
				d.ParentID = "foreign"
				f.driver.items["show"] = d
			case "disabled":
				f.s.db.Model(&f.library).Update("cloud_empty_cleanup_enabled", false)
			case "root":
				f.policy.CloudCleanupDirectories[0].ProviderID = "root"
			}
			err := f.execute()
			if err == nil {
				t.Fatal("unsafe success")
			}
			if len(f.driver.recycled) != 0 {
				t.Fatal("unsafe recycle")
			}
			if scenario == "auth" {
				if code, retry := cloudpkg.ErrorInfo(err); code != cloudpkg.CodeAuthExpired || retry {
					t.Fatal("lost credential failure classification")
				}
			}
		})
	}
}
func TestCloudEmptyCleanupLostACKDurableClaim(t *testing.T) {
	f := newCloudCleanupFixture(t)
	f.driver.lostACK = true
	if err := f.execute(); err == nil {
		t.Fatal("lost ACK not retained")
	}
	var claim models.CatalogCloudCleanupClaim
	if err := f.s.db.Where("run_id=? AND provider_id=?", f.run.ID, "season").Take(&claim).Error; err != nil || claim.Status != "prepared" {
		t.Fatal(claim, err)
	}
	if err := f.s.db.Transaction(func(tx *gorm.DB) error { return assertCatalogArtifactReceiptsResolvedTx(tx, f.permit.evidence) }); err == nil {
		t.Fatal("unresolved mutation settled")
	}
	if err := f.s.db.Model(&f.run).Update("status", models.MediaArtifactStatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.Transaction(func(tx *gorm.DB) error {
		return f.s.queue.assertHistoryRecordClearableTx(tx, HistoryScopeSTRMRuns, f.run.ID)
	}); !errors.Is(err, errHistoryRecoveryBearing) {
		t.Fatal("history discarded intent", err)
	}
	if err := f.execute(); err != nil {
		t.Fatal("same owner retry", err)
	}
	if !reflect.DeepEqual(f.driver.recycled, []string{"season", "show"}) {
		t.Fatal("double recycle", f.driver.recycled)
	}
	if err := f.s.db.Transaction(func(tx *gorm.DB) error { return assertCatalogArtifactReceiptsResolvedTx(tx, f.permit.evidence) }); err != nil {
		t.Fatal("claim not settled", err)
	}
}
func TestCloudEmptyCleanupBusyDownloadDoesNotRetainOwner(t *testing.T) {
	f := newCloudCleanupFixture(t)
	job, err := f.s.queue.Enqueue(EnqueueJobInput{System: true, JobType: JobTypeMediaArtifact, Priority: 1, DisplayName: "download in progress", Provider: "media_library", ResourceKey: "download-other", CoalescingKey: "download-other", Payload: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	download := models.DownloadTask{ID: "download", JobID: job.ID, TargetLibraryID: &f.library.ID, OwnerID: 1, SourceCiphertext: "test", Phase: "downloading"}
	if err := f.s.db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.execute(); err != nil {
		t.Fatal("optional cleanup blocked import", err)
	}
	if f.driver.statCalls != 0 || len(f.driver.recycled) != 0 {
		t.Fatal("busy source accessed")
	}
	if err := f.s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&f.run).Updates(map[string]any{"status": models.MediaArtifactStatusCompleted, "cleanup_status": models.MediaArtifactCleanupSkipped, "finished_at": time.Now().UTC()}).Error; err != nil {
			return err
		}
		return SettleCatalogPhysicalWriteTx(tx, f.permit, &f.claim)
	}); err != nil {
		t.Fatal("busy optional cleanup stranded owner", err)
	}
}

func TestCloudEmptyCleanupFreezeNoAuthorityForUnknownOrReplacement(t *testing.T) {
	for _, scenario := range []string{"unknown", "off", "replacement-parent"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCloudCleanupFixture(t)
			scan := f.scan
			scan.CheckpointJSON = "{}"
			deleted := []string{"removed"}
			switch scenario {
			case "unknown":
				deleted = []string{"unknown-id"}
			case "off":
				if err := f.s.db.Model(&f.library).Update("cloud_empty_cleanup_enabled", false).Error; err != nil {
					t.Fatal(err)
				}
			case "replacement-parent":
				next := models.MediaLibraryScanRun{LibraryID: f.library.ID, Kind: "full", Status: "success", StartedAt: time.Now().UTC(), CheckpointJSON: "{}"}
				if err := f.s.db.Create(&next).Error; err != nil {
					t.Fatal(err)
				}
				result := medialibrary.Result{Directories: []cloudpkg.TreeEntry{{Item: cloudpkg.Item{ID: "season", ParentID: "show", IsDir: true}, RelativePath: "/Show/Moved"}, {Item: cloudpkg.Item{ID: "replacement", ParentID: "show", IsDir: true}, RelativePath: "/Show/Season02"}}}
				if err := stageProviderPaths(context.Background(), f.s.db, f.library, f.storage, result, next); err != nil {
					t.Fatal(err)
				}
				if err := f.s.db.Transaction(func(tx *gorm.DB) error {
					return persistProviderPathsTx(tx, f.library, f.storage, result, next.StartedAt, next.ID)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := freezeCloudCleanupDirectoriesTx(f.s.db, &scan, deleted); err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(scan.CheckpointJSON), &fields); err != nil {
				t.Fatal(err)
			}
			if _, ok := fields["cloud_cleanup_directories"]; ok {
				t.Fatal("unrelated path gained delete authority", scan.CheckpointJSON)
			}
			if f.driver.statCalls != 0 || f.driver.listCalls != 0 {
				t.Fatal("freeze called provider")
			}
		})
	}
}

func TestCloudEmptyCleanupBusyCanReconcilePreparedWithoutRecyclingMore(t *testing.T) {
	f := newCloudCleanupFixture(t)
	f.driver.lostACK = true
	if err := f.execute(); err == nil {
		t.Fatal("expected lost ACK")
	}
	job, err := f.s.queue.Enqueue(EnqueueJobInput{System: true, JobType: JobTypeMediaArtifact, Priority: 1, DisplayName: "download", Provider: "media_library", ResourceKey: "busy-later", CoalescingKey: "busy-later", Payload: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.Create(&models.DownloadTask{ID: "busy-later", JobID: job.ID, TargetLibraryID: &f.library.ID, OwnerID: 1, SourceCiphertext: "test", Phase: "downloading"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.execute(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.driver.recycled, []string{"season"}) {
		t.Fatal("busy cleanup recycled another directory", f.driver.recycled)
	}
	if err := f.s.db.Transaction(func(tx *gorm.DB) error { return assertCatalogArtifactReceiptsResolvedTx(tx, f.permit.evidence) }); err != nil {
		t.Fatal("busy download stranded known missing claim", err)
	}
}

func TestCloudEmptyCleanupCancelledAbandonsOnlyOptionalIntent(t *testing.T) {
	f := newCloudCleanupFixture(t)
	f.driver.lostACK = true
	if err := f.execute(); err == nil {
		t.Fatal("expected lost ACK")
	}
	// A still-running owner cannot use the cancellation path to drop intent.
	if err := f.s.settleStoppedArtifactExecution(f.permit, f.policy, true); err == nil {
		t.Fatal("live job abandoned mutation")
	}
	var pending models.CatalogCloudCleanupClaim
	if err := f.s.db.Where("run_id=? AND status='prepared'", f.run.ID).Take(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.queue.finishLease(f.claim.Job.ID, f.claim.LeaseToken, models.JobStatusCancelled, "cancelled", "cancelled", nil); err != nil {
		t.Fatal(err)
	}
	calls := f.driver.statCalls
	lists := f.driver.listCalls
	if err := f.s.settleStoppedArtifactExecution(f.permit, f.policy, true); err != nil {
		t.Fatal(err)
	}
	if f.driver.statCalls != calls || f.driver.listCalls != lists || !reflect.DeepEqual(f.driver.recycled, []string{"season"}) {
		t.Fatal("cancelled cleanup accessed provider")
	}
	if err := f.s.db.First(&pending, pending.ID).Error; err != nil || pending.Status != "abandoned" {
		t.Fatal("intent not retained as unconfirmed", pending, err)
	}
	var physical models.CatalogPhysicalWrite
	if err := f.s.db.First(&physical, f.permit.evidence.ID).Error; err != nil || physical.State != "settled" {
		t.Fatal("cancelled optional cleanup blocks library", physical.State, err)
	}
	if err := f.s.db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalAdmissionTx(tx, f.library.ID) }); err != nil {
		t.Fatal("library remains blocked", err)
	}
}
