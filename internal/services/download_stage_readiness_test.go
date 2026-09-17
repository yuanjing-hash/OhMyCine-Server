package services

import (
	"context"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStagedDownloadArtifactAdmission(t *testing.T) {
	for _, name := range []string{"separate", "cloud_target", "provider_to_local", "provider_source_expired", "node_staging", "checking", "disabled", "overlap", "projection_overlap", "provider_direct", "route_missing", "cancelled", "other_writer", "repair", "credentials"} {
		t.Run(name, func(t *testing.T) {
			s, db, actor, storage, profile := mediaLibraryTestService(t)
			l, err := s.Create(context.Background(), actor, testLibraryInput("staged admission", storage, profile, false), RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			check := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			check(db.Model(&models.MediaLibrary{}).Where("id=?", l.ID).Updates(map[string]any{"enabled": true, "baseline_generation": 1, "status": models.MediaLibraryStatusListening}).Error)
			check(db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id=?", l.ID).Updates(map[string]any{"diagnosed_revision": 1, "source_revision": 1}).Error)
			var library models.MediaLibrary
			check(db.First(&library, l.ID).Error)
			queue := NewQueueService(db, NewAuditService(db))
			job, err := queue.Enqueue(EnqueueJobInput{System: true, JobType: "download", DisplayName: "staged", ResourceKey: "downloader:stage", Payload: map[string]any{}})
			check(err)
			d := models.Downloader{ID: "stage", Name: "stage", Type: models.DownloaderTypeQBittorrent, Enabled: true}
			check(db.Create(&d).Error)
			identity, _ := marshalDataSourceIdentity(localDataSourceIdentity())
			task := models.DownloadTask{ID: "stage-task", JobID: job.ID, OwnerID: actor.User.ID, DownloaderID: &d.ID, ProviderType: d.Type, Phase: models.DownloadTaskStatusQueued, StagingAbsolutePath: t.TempDir(), TargetLibraryID: &library.ID, TargetStorageID: &storage.ID, TargetStorageType: storage.Type, TargetStorageRoot: storage.RootPath, TargetRelativeRoot: library.RelativeRoot, TransferRouteKind: models.TransferRouteSameSourceLocal, TransferRouteVersion: models.TransferRouteVersionCurrent, SourceDataSourceJSON: identity, TargetDataSourceJSON: identity}
			proof := models.CatalogPhysicalWrite{LibraryID: library.ID, OwnerKind: CatalogPhysicalArtifact, OwnerID: "artifact-stage", State: "entered", Revision: 1, EnteredAt: time.Now(), UpdatedAt: time.Now()}
			switch name {
			case "cloud_target":
				connection := models.Connection{Name: "cloud", NameNormalized: "cloud", Provider: models.StorageTypePan115, Enabled: true}
				check(db.Create(&connection).Error)
				check(db.Model(&storage).Updates(map[string]any{"type": models.StorageTypePan115, "connection_id": connection.ID, "root_path": "0"}).Error)
				check(db.First(&storage, storage.ID).Error)
				target, _ := mediaLibraryDataSourceIdentity(storage)
				task.TargetDataSourceJSON, _ = marshalDataSourceIdentity(target)
				task.TargetStorageType, task.TargetStorageRoot, task.TransferRouteKind = models.StorageTypePan115, "0", models.TransferRouteCrossSource
			case "provider_to_local", "provider_source_expired":
				connection := models.Connection{Name: "source", NameNormalized: "source", Provider: models.StorageTypePan115, Enabled: true}
				if name == "provider_source_expired" {
					connection.LastHealthStatus = "offline"
					connection.LastHealthErrorCode = "pan115_auth_expired"
				}
				check(db.Create(&connection).Error)
				sourceStorage := models.Storage{Name: "cloud-source", NameNormalized: "cloud-source", Type: models.StorageTypePan115, ConnectionID: &connection.ID, RootPath: "0", RootPathNormalized: "0", Enabled: true}
				check(db.Create(&sourceStorage).Error)
				check(db.Model(&d).Updates(map[string]any{"type": models.DownloaderTypePan115Offline, "storage_id": sourceStorage.ID}).Error)
				source, _ := mediaLibraryDataSourceIdentity(sourceStorage)
				task.SourceDataSourceJSON, _ = marshalDataSourceIdentity(source)
				task.ProviderType, task.StagingStorageID, task.TransferRouteKind = models.DownloaderTypePan115Offline, &sourceStorage.ID, models.TransferRouteCrossSource
			case "node_staging":
				task.ExecutionLocation = models.NodeLocationRemote
			case "checking":
				check(db.Model(&models.MediaLibraryStructureAutoState{}).Where("library_id=?", l.ID).Update("source_revision", 2).Error)
			case "disabled":
				check(db.Model(&library).Update("enabled", false).Error)
			case "overlap":
				task.StagingAbsolutePath = storage.RootPath
			case "projection_overlap":
				check(db.Model(&library).Update("strm_local_root", task.StagingAbsolutePath).Error)
			case "provider_direct":
				task.ProviderType = models.DownloaderTypePan115Offline
				check(db.Model(&d).Update("type", task.ProviderType).Error)
			case "route_missing":
				task.SourceDataSourceJSON = "{}"
			case "cancelled":
				task.Phase = models.DownloadTaskStatusCancelled
			case "other_writer":
				proof.OwnerKind = CatalogPhysicalRepair
			case "repair":
				check(db.Create(&models.MediaLibraryStructureRepair{ID: "stage-repair", OwnerID: actor.User.ID, LibraryID: library.ID, Phase: "queued", Scope: "full", PlanJSON: "{}", StateJSON: "{}"}).Error)
			case "credentials":
				connection := models.Connection{Name: "expired", NameNormalized: "expired", Enabled: true, LastHealthStatus: "offline", LastHealthErrorCode: "pan115_auth_expired"}
				check(db.Create(&connection).Error)
				check(db.Model(&storage).Update("connection_id", connection.ID).Error)
			}
			check(db.Create(&task).Error)
			check(db.Create(&proof).Error)
			var persistedJob models.Job
			check(db.First(&persistedJob, "id=?", job.ID).Error)
			dtos := make([]JobDTO, 1)
			check(queue.projectJobWaitReasons(actor, []models.Job{persistedJob}, dtos))
			allowed := name == "separate" || name == "cloud_target" || name == "provider_to_local" || name == "node_staging"
			if (dtos[0].WaitReason == nil) != allowed {
				t.Fatalf("wait reason=%+v allowed=%v", dtos[0].WaitReason, allowed)
			}
			claim, err := queue.Claim([]string{"download"})
			check(err)
			if allowed {
				if claim == nil || claim.Job.ID != job.ID {
					t.Fatalf("staged download blocked: %+v", claim)
				}
			} else {
				if claim != nil {
					t.Fatalf("unsafe download claimed: %+v", claim)
				}
				var persisted models.Job
				check(db.First(&persisted, "id=?", job.ID).Error)
				if persisted.AttemptCount != 0 || persisted.Status != models.JobStatusQueued {
					t.Fatalf("blocked download changed: %+v", persisted)
				}
			}
			// Target mutations stay gated even where staging was admitted.
			blocked, err := libraryJobBlocked(db, models.Job{ID: "other-transfer", JobType: "transfer", ResourceKey: "library:" + uintID(library.ID)})
			if err != nil || !blocked {
				t.Fatalf("target transfer bypass: %v %v", blocked, err)
			}
		})
	}
}
