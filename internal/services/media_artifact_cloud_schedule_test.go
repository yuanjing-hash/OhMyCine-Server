package services

import (
	"context"
	"encoding/json"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
	"testing"
	"time"
)

type rootlessRecoveryDriver struct {
	*cleanupCloudDriver
	recycleCalls int
}

func (d *rootlessRecoveryDriver) Recycle(ctx context.Context, id string) error {
	d.recycleCalls++
	return d.cleanupCloudDriver.Recycle(ctx, id)
}

func TestCloudOnlyArtifactScheduleHasNoLocalRootDependency(t *testing.T) {
	for _, scenario := range []string{"no_scope_no_job", "exact_scope", "lost_ack_new_lease"} {
		t.Run(scenario, func(t *testing.T) {
			withDirectories := scenario != "no_scope_no_job"
			cleanup, queue, _, library, _ := strmManagementFixture(t)
			db := cleanup.db
			library.STRMEnabled = false
			library.SignedProxyEnabled = false
			library.MetadataArtifactsEnabled = false
			library.STRMLocalRoot = ""
			library.CloudEmptyCleanupEnabled = true
			library.ProviderRootID = "root"
			if err := db.Save(&library).Error; err != nil {
				t.Fatal(err)
			}
			dirs := []cloudCleanupDirectory{}
			if withDirectories {
				dirs = append(dirs, cloudCleanupDirectory{ProviderID: "season", ParentProviderID: "root", RelativePath: "/Season", ScanRunID: 1})
			}
			raw, _ := json.Marshal(map[string]any{"cloud_cleanup_directories": dirs})
			now := time.Now().UTC()
			scan := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: 2, Kind: "event", Status: "success", Partial: true, CheckpointJSON: string(raw), StartedAt: now, CatalogPublishedAt: &now}
			if err := db.Create(&scan).Error; err != nil {
				t.Fatal(err)
			}
			service := NewMediaArtifactService(db, queue, nil, zerolog.Nop())
			var storage models.Storage
			if err := db.First(&storage, library.StorageID).Error; err != nil {
				t.Fatal(err)
			}
			driver := &rootlessRecoveryDriver{cleanupCloudDriver: &cleanupCloudDriver{fakeCloudDriver: &fakeCloudDriver{recycleItems: true, items: map[string]cloudpkg.Item{"season": {ID: "season", ParentID: "root", Name: "Season", IsDir: true}}}, lostACK: scenario == "lost_ack_new_lease"}}
			connections := NewConnectionService(db, NewAuditService(db), nil, nil, zerolog.Nop())
			connections.drivers[*storage.ConnectionID] = driver
			service.SetConnectionService(connections)
			paths := medialibrary.Result{Directories: []cloudpkg.TreeEntry{{Item: cloudpkg.Item{ID: "season", ParentID: "root", IsDir: true}, RelativePath: "/Season"}}}
			if err := stageProviderPaths(context.Background(), db, library, storage, paths, scan); err != nil {
				t.Fatal(err)
			}
			if err := db.Transaction(func(tx *gorm.DB) error {
				return persistProviderPathsTx(tx, library, storage, paths, scan.StartedAt, scan.ID)
			}); err != nil {
				t.Fatal(err)
			}
			if err := service.ScheduleGeneration(library.ID, 2); err != nil {
				t.Fatal(err)
			}
			var runs []models.MediaArtifactRun
			if err := db.Where("library_id = ?", library.ID).Find(&runs).Error; err != nil {
				t.Fatal(err)
			}
			if !withDirectories {
				if len(runs) != 0 {
					t.Fatal("no-op created job")
				}
				var jobs int64
				if err := db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaArtifact).Count(&jobs).Error; err != nil || jobs != 0 {
					t.Fatalf("no-op queued job: %d %v", jobs, err)
				}
				return
			}
			if len(runs) != 1 {
				t.Fatalf("runs=%d", len(runs))
			}
			_, policy, err := service.loadRun(runs[0].ID)
			if err != nil || policy.TargetKind != artifactTargetCloudCleanup || policy.ProjectionRoot != "" || policy.Metadata || policy.STRMEnabled {
				t.Fatalf("policy=%+v err=%v", policy, err)
			}
			claim, err := queue.Claim([]string{JobTypeMediaArtifact})
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			result := (&MediaArtifactWorker{service: service}).Run(context.Background(), nil, *claim)
			if scenario == "lost_ack_new_lease" {
				if result.RetryAt == nil || result.ErrorCode != cloudpkg.CodeMutationUnknown {
					t.Fatalf("first result=%+v", result)
				}
				var quiescent int64
				if err := db.Table("catalog_physical_writes").Where("owner_id = ? AND state = ?", runs[0].ID, "quiescent").Count(&quiescent).Error; err != nil || quiescent != 1 {
					t.Fatalf("quiescent=%d err=%v", quiescent, err)
				}
				// The production runtime applies retry results with RetryLater,
				// not Complete (which denotes success). Release the original lease.
				if err := queue.RetryLater(claim.Job.ID, claim.LeaseToken, result.ErrorCode, result.ErrorMessage, *result.RetryAt); err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&models.Job{}).Where("id = ?", claim.Job.ID).Update("next_attempt_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
					t.Fatal(err)
				}
				previousLease := claim.LeaseToken
				if err := queue.PromoteDueRetries(); err != nil {
					t.Fatal(err)
				}
				claim, err = queue.Claim([]string{JobTypeMediaArtifact})
				if err != nil || claim == nil || claim.LeaseToken == previousLease {
					t.Fatalf("resumed claim=%+v err=%v", claim, err)
				}
				result = (&MediaArtifactWorker{service: service}).Run(context.Background(), nil, *claim)
			}
			if result.ErrorCode != "" || result.RetryAt != nil {
				t.Fatalf("rootless cleanup failed: %+v", result)
			}
			if err := queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
				t.Fatal(err)
			}
			var completed models.Job
			if err := db.First(&completed, "id = ?", claim.Job.ID).Error; err != nil {
				t.Fatal(err)
			}
			if completed.Status != models.JobStatusCompleted || completed.LeaseTokenHash != "" {
				t.Fatalf("job not fully completed: %s", completed.Status)
			}
			if _, exists := driver.items["season"]; exists {
				t.Fatal("directory not recycled")
			}
			if driver.recycleCalls != 1 {
				t.Fatalf("recycle calls=%d", driver.recycleCalls)
			}
			var settled int64
			if err := db.Table("catalog_physical_writes").Where("owner_id = ? AND state = ?", runs[0].ID, "settled").Count(&settled).Error; err != nil || settled != 1 {
				t.Fatalf("settled=%d err=%v", settled, err)
			}
			if err := service.ScheduleGeneration(library.ID, 2); err != nil {
				t.Fatal("idempotent schedule", err)
			}
			var jobs int64
			if err := db.Model(&models.Job{}).Where("job_type = ?", JobTypeMediaArtifact).Count(&jobs).Error; err != nil || jobs != 1 {
				t.Fatalf("replay created extra job: %d %v", jobs, err)
			}
		})
	}
}
