package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
	"gorm.io/gorm"
)

type remoteBatchFixture struct {
	cloudTransferFixture
	libraries  *MediaLibraryService
	artifacts  *MediaArtifactService
	catalog    *CatalogSnapshotStore
	task       models.TransferTask
	rows       []models.RemoteUploadFile
	projection string
	dbPath     string
}

// Exercise the same durable row writers that consume Node completion, then
// install the production reconciler. No Node or cloud network is involved.
func newRemoteBatchFixture(t *testing.T, provider string) remoteBatchFixture {
	t.Helper()
	ctx := context.Background()
	f := newCloudTransferFixture(t, models.MediaLibraryTransferCopy, models.MediaLibraryConflictAsk, false)
	if provider == cloud.ProviderQuark {
		f = setQuarkTransferRoute(t, f)
		f.service.connections.drivers[f.connection] = makeQuarkTransferFake(f.driver)
	}
	nodeID := uuid.NewString()
	now := time.Now().UTC()
	node := models.TransferNode{ID: nodeID, OwnerID: f.download.OwnerID, Name: "Fixture Node", NameNormalized: "fixture-node", APIURL: "http://node.example.test", Status: models.NodeStatusOnline, CapabilitiesJSON: `{}`, CreatedAt: now, UpdatedAt: now}
	if err := f.queue.db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	f.download.ExecutionLocation, f.download.NodeID = models.NodeLocationRemote, &nodeID
	f.download.ProviderType = models.DownloaderTypeQBittorrent
	f.download.StagingStorageID = nil
	f.download.TargetRelativeRoot = "/library"
	f.download.TransferRouteVersion = models.TransferRouteVersionCurrent
	f.download.TransferRouteKind = models.TransferRouteCrossSource
	f.download.SourceDataSourceJSON, _ = marshalDataSourceIdentity(localDataSourceIdentity())
	f.download.TargetDataSourceJSON, _ = marshalDataSourceIdentity(models.DataSourceIdentity{Kind: models.DataSourceKindProvider, ProviderType: provider, ConnectionIdentity: strconv.Itoa(int(f.connection)), StorageScope: strconv.Itoa(int(*f.download.TargetStorageID))})
	if err := f.queue.db.Save(&f.download).Error; err != nil {
		t.Fatal(err)
	}
	f.manifest.Files[0].RemoteFileToken = "file:remote-fixture"
	f.manifest.Files[0].SHA256 = strings.Repeat("a", 64)
	f.manifest.RemoteExportOperationKey = "qb:file_export:fixture"
	f.manifest.RemoteExportDigest = strings.Repeat("b", 64)
	expires := now.Add(time.Hour)
	f.manifest.RemoteExportExpiresAt = &expires
	if err := f.service.Enqueue(f.download, f.manifest); err != nil {
		t.Fatal(err)
	}
	var task models.TransferTask
	if err := f.queue.db.First(&task, "download_task_id = ?", f.download.ID).Error; err != nil {
		t.Fatal(err)
	}
	worker := NewTransferWorker(f.service)
	targets, err := buildTransferTargets(f.download, f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	planned := make([]remoteNodeUploadTarget, len(targets))
	for i, target := range targets {
		planned[i] = remoteNodeUploadTarget{File: target.File, Relative: target.Relative, Group: target.Group, ConflictAction: remoteUploadFailIfExists}
	}
	rows, err := worker.createRemoteUploadFiles(ctx, task, planned)
	if err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	if err = f.queue.db.First(&storage, f.library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	batches, err := buildRemoteUploadBatches(rows, storage, f.download, f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, batch := range batches {
		_, record, _, err := worker.prepareRemoteUploadOperation(ctx, nodeID, f.download.ID, remoteUploadOperationKey(task.ID, batch.index), batch.plan)
		if err != nil {
			t.Fatal(err)
		}
		if err = worker.persistRemoteOperationState(ctx, record, nodeprotocol.OperationCompleted, nodeprotocol.PhaseCompleted, "", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []cloud.Item{{ID: "category-dir", ParentID: "library-root", Name: "电影", IsDir: true}, {ID: "movie-dir", ParentID: "category-dir", Name: "Movie (2024)", IsDir: true}, {ID: "uploaded-video", ParentID: "movie-dir", Name: "Movie (2024).mkv", Size: rows[0].Size, SHA1: strings.Repeat("c", 40)}} {
		f.driver.items[item.ID] = item
	}
	file := nodeprotocol.StorageUploadFileResult{SourceFileToken: rows[0].SourceFileToken, Status: nodeprotocol.StorageFileCompleted, TargetParentID: "movie-dir", TargetItemID: "uploaded-video", TargetName: "Movie (2024).mkv", Size: rows[0].Size, SHA1: strings.Repeat("c", 40)}
	if provider == cloud.ProviderQuark {
		file.SHA1 = ""
		file.ContentDigest = &nodeprotocol.ContentDigest{Version: 2, Algorithm: "sha256", Value: rows[0].SHA256, VerifiedBy: "full_readback"}
	}
	if err = worker.applyRemoteUploadResponse(ctx, &task, rows, rows, nodeprotocol.StorageActionResponse{Files: []nodeprotocol.StorageUploadFileResult{file}}, provider); err != nil {
		t.Fatal(err)
	}
	task.Phase, task.ProcessedFiles = models.TransferTaskStatusCompleted, len(rows)
	if err = f.queue.db.Save(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.CloudStateJSON != "" {
		t.Fatal("Node fixture fabricated a Server cloud checkpoint")
	}
	projection := t.TempDir()
	f.library.STRMEnabled, f.library.SignedProxyEnabled, f.library.STRMLocalRoot = true, true, projection
	if err = f.queue.db.Save(&f.library).Error; err != nil {
		t.Fatal(err)
	}
	storage.Capabilities = `{"temporary_direct_url":true,"signed_proxy":true}`
	if err = f.queue.db.Save(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err = f.queue.db.First(&profile, f.library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	if provider == cloud.ProviderQuark {
		if err = f.queue.db.Transaction(func(tx *gorm.DB) error { return initializeFreshQuarkCatalogTx(tx, f.library, storage, profile) }); err != nil {
			t.Fatal(err)
		}
	}
	if err = captureRemoteUploadManagedItems(f.queue.db, task, f.download, rows); err != nil {
		t.Fatal(err)
	}
	var location struct{ File string }
	if err = f.queue.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	readDB, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := readDB.DB()
	t.Cleanup(func() { _ = pool.Close() })
	catalog := NewCatalogSnapshotStore(f.queue.db, readDB)
	s := NewMediaLibraryService(f.queue.db, f.queue.audit, zerolog.Nop())
	s.connections, s.queue = f.service.connections, f.queue
	s.SetCatalogSnapshotStore(catalog)
	s.changes = NewMediaChangeService(f.queue.db)
	installHydrationMetadata(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%d,"title":"Movie","name":"Movie"}`, *f.download.ScrapeTMDBID)
	})
	secrets, err := credential.Open(filepath.Join(t.TempDir(), "fixture.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := NewSignedProxyService(f.queue.db, secrets, f.service.connections, "https://media.example.test", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	proxy.SetCatalogSnapshotStore(catalog)
	artifacts := NewMediaArtifactService(f.queue.db, f.queue, proxy, zerolog.Nop())
	artifacts.SetCatalogSnapshotStore(catalog)
	artifacts.SetConnectionService(f.service.connections)
	artifacts.SetMediaChangeService(s.changes)
	s.SetArtifactService(artifacts)
	s.EnableCatalogScanFollowups()
	f.service.SetMediaLibraryReconciler(s)
	return remoteBatchFixture{f, s, artifacts, catalog, task, rows, projection, location.File}
}

func TestRemoteUploadBatchPublishesActualCatalogAndSTRM(t *testing.T) {
	for _, provider := range []string{cloud.ProviderQuark, cloud.ProviderPan115} {
		t.Run(provider, func(t *testing.T) {
			f := newRemoteBatchFixture(t, provider)
			payload, _ := json.Marshal(transferJobPayload{TransferTaskID: f.task.ID})
			result := NewTransferWorker(f.service).Run(context.Background(), nil, ClaimedJob{Job: models.Job{PayloadJSON: string(payload)}})
			if result.ErrorCode != "" || result.RetryAt != nil {
				t.Fatalf("completed Node worker: %+v", result)
			}
			entries := catalogReadEntries(t, f.catalog, f.library.ID, "")
			if len(entries) != 1 || entries[0].ProviderID != "uploaded-video" {
				t.Fatalf("entries=%+v", entries)
			}
			if err := f.libraries.RecoverCatalogScanFollowups(context.Background(), 20); err != nil {
				t.Fatal(err)
			}
			claim, err := f.queue.Claim([]string{JobTypeMediaArtifact})
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			result = NewMediaArtifactWorker(f.artifacts).Run(context.Background(), workerRuntime{queue: f.queue, job: *claim}, *claim)
			if result.ErrorCode != "" || result.RetryAt != nil {
				t.Fatalf("artifact=%+v", result)
			}
			body, err := os.ReadFile(filepath.Join(f.projection, "电影", "Movie (2024)", "Movie (2024).strm"))
			if err != nil || !strings.HasPrefix(string(body), "https://media.example.test/") || strings.Contains(string(body), "uploaded-video") {
				t.Fatalf("STRM failed err=%v", err)
			}
			var saved models.TransferTask
			if err = f.queue.db.First(&saved, "id = ?", f.task.ID).Error; err != nil || saved.CatalogPublishedAt == nil || saved.CloudStateJSON != "" {
				t.Fatalf("completion marker invalid: %v", err)
			}
			// A durable catalog publication can precede its transfer ACK. Close
			// both real SQLite pools, then recover through a fresh service without
			// a Node client or provider driver: it must reuse that same batch.
			var scans int64
			if err = f.queue.db.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", f.library.ID).Count(&scans).Error; err != nil {
				t.Fatal(err)
			}
			if err = f.queue.db.Model(&models.TransferTask{}).Where("id = ?", f.task.ID).Update("catalog_published_at", nil).Error; err != nil {
				t.Fatal(err)
			}
			for _, db := range []*gorm.DB{f.catalog.readDB, f.queue.db} {
				pool, _ := db.DB()
				if err = pool.Close(); err != nil {
					t.Fatal(err)
				}
			}
			writeDB, err := database.Open(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			readDB, err := database.OpenReadOnly(f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, db := range []*gorm.DB{writeDB, readDB} {
				pool, _ := db.DB()
				t.Cleanup(func() { _ = pool.Close() })
			}
			fresh := NewMediaLibraryService(writeDB, NewAuditService(writeDB), zerolog.Nop())
			store := NewCatalogSnapshotStore(writeDB, readDB)
			fresh.SetCatalogSnapshotStore(store)
			if err = fresh.ReconcileTransferBatch(context.Background(), f.task.ID); err != nil {
				t.Fatal(err)
			}
			if err = fresh.ReconcileTransferBatch(context.Background(), f.task.ID); err != nil {
				t.Fatal(err)
			}
			var after int64
			if err = writeDB.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", f.library.ID).Count(&after).Error; err != nil || after != scans {
				t.Fatal("cold replay published another catalog batch")
			}
			actual := catalogReadEntries(t, store, f.library.ID, "")
			if len(actual) != 1 || actual[0].ID != entries[0].ID || !actual[0].CreatedAt.Equal(entries[0].CreatedAt) {
				t.Fatal("cold replay changed stable media identity")
			}
			afterBody, err := os.ReadFile(filepath.Join(f.projection, "电影", "Movie (2024)", "Movie (2024).strm"))
			if err != nil || string(afterBody) != string(body) {
				t.Fatal("cold replay changed STRM")
			}
		})
	}
}

func TestRemoteUploadBatchRejectsIncompleteEvidenceBeforeProviderRead(t *testing.T) {
	for _, provider := range []string{cloud.ProviderQuark, cloud.ProviderPan115} {
		t.Run(provider, func(t *testing.T) {
			f := newRemoteBatchFixture(t, provider)
			rowUpdate := func(values map[string]any) func(*gorm.DB) error {
				return func(tx *gorm.DB) error {
					return tx.Model(&models.RemoteUploadFile{}).Where("transfer_task_id = ?", f.task.ID).Updates(values).Error
				}
			}
			managedUpdate := func(values map[string]any) func(*gorm.DB) error {
				return func(tx *gorm.DB) error {
					return tx.Model(&models.MediaManagedItem{}).Where("transfer_task_id = ?", f.task.ID).Updates(values).Error
				}
			}
			opUpdate := func(values map[string]any) func(*gorm.DB) error {
				return func(tx *gorm.DB) error {
					return tx.Model(&models.RemoteOperation{}).Where("operation_key = ?", remoteUploadOperationKey(f.task.ID, 0)).Updates(values).Error
				}
			}
			cases := map[string]func(*gorm.DB) error{
				"missing_file": func(tx *gorm.DB) error {
					return tx.Where("transfer_task_id = ?", f.task.ID).Delete(&models.RemoteUploadFile{}).Error
				},
				"unknown_file":   rowUpdate(map[string]any{"status": "unknown"}),
				"pending_file":   rowUpdate(map[string]any{"status": nodeprotocol.StorageFilePending}),
				"missing_target": rowUpdate(map[string]any{"target_item_id": ""}),
				"wrong_parent":   rowUpdate(map[string]any{"target_parent_id": "foreign"}),
				"wrong_file_owner": func(tx *gorm.DB) error {
					var job models.Job
					if err := tx.First(&job, "id = ?", f.download.JobID).Error; err != nil {
						return err
					}
					job.ID = uuid.NewString()
					if err := tx.Create(&job).Error; err != nil {
						return err
					}
					other := f.download
					other.ID = uuid.NewString()
					other.JobID = job.ID
					if err := tx.Create(&other).Error; err != nil {
						return err
					}
					return rowUpdate(map[string]any{"download_task_id": other.ID})(tx)
				},
				"ordinal_gap":           rowUpdate(map[string]any{"ordinal": 2}),
				"changed_source_hash":   rowUpdate(map[string]any{"sha256": strings.Repeat("d", 64)}),
				"changed_frozen_target": rowUpdate(map[string]any{"target_relative": "foreign.mkv"}),
				"missing_managed": func(tx *gorm.DB) error {
					return tx.Where("transfer_task_id = ?", f.task.ID).Delete(&models.MediaManagedItem{}).Error
				},
				"managed_wrong_id":       managedUpdate(map[string]any{"provider_item_id": "foreign"}),
				"managed_wrong_size":     managedUpdate(map[string]any{"size": 1}),
				"managed_old_identity":   managedUpdate(map[string]any{"identity_revision": f.download.IdentityRevision + 1}),
				"operation_unknown":      opUpdate(map[string]any{"status": nodeprotocol.OperationRunning}),
				"operation_bad_phase":    opUpdate(map[string]any{"phase": nodeprotocol.PhaseUploadingTarget}),
				"operation_wrong_digest": opUpdate(map[string]any{"plan_digest": strings.Repeat("e", 64)}),
				"operation_missing": func(tx *gorm.DB) error {
					return tx.Where("operation_key = ?", remoteUploadOperationKey(f.task.ID, 0)).Delete(&models.RemoteOperation{}).Error
				},
				"task_location_drift": func(tx *gorm.DB) error {
					return tx.Model(&models.TransferTask{}).Where("id = ?", f.task.ID).Update("execution_location", models.NodeLocationServer).Error
				},
				"incomplete_count": func(tx *gorm.DB) error {
					return tx.Model(&models.TransferTask{}).Where("id = ?", f.task.ID).Update("processed_files", 0).Error
				},
				"duplicate_manifest_token": func(tx *gorm.DB) error {
					manifest := f.manifest
					manifest.Files = append(append(manifest.Files[:0:0], manifest.Files...), manifest.Files[0])
					raw, _ := json.Marshal(manifest)
					return tx.Model(&models.TransferTask{}).Where("id = ?", f.task.ID).Update("manifest_json", string(raw)).Error
				},
			}
			if provider == cloud.ProviderQuark {
				cases["missing_full_readback"] = rowUpdate(map[string]any{"target_digest_json": ""})
				cases["invented_115_hash"] = rowUpdate(map[string]any{"target_sha1": strings.Repeat("f", 40)})
			} else {
				cases["invalid_115_hash"] = rowUpdate(map[string]any{"target_sha1": "invalid"})
			}
			rollback := errors.New("fixture rollback")
			for name, mutate := range cases {
				t.Run(name, func(t *testing.T) {
					err := f.queue.db.Transaction(func(tx *gorm.DB) error {
						if err := mutate(tx); err != nil {
							t.Fatal(err)
						}
						service := NewMediaLibraryService(tx, f.queue.audit, zerolog.Nop())
						service.connections = f.libraries.connections
						beforeStat, beforeList := f.driver.statCalls, f.driver.listCalls
						if err := service.ReconcileTransferBatch(context.Background(), f.task.ID); err == nil {
							t.Fatal("invalid evidence was published")
						}
						if beforeStat != f.driver.statCalls || beforeList != f.driver.listCalls {
							t.Fatal("invalid evidence reached provider")
						}
						var saved models.TransferTask
						if err := tx.First(&saved, "id = ?", f.task.ID).Error; err != nil || saved.CatalogPublishedAt != nil {
							t.Fatal("invalid evidence completed")
						}
						var scans int64
						if err := tx.Model(&models.MediaLibraryScanRun{}).Where("library_id = ?", f.library.ID).Count(&scans).Error; err != nil || scans != 0 {
							t.Fatal("invalid evidence created scan")
						}
						return rollback
					})
					if !errors.Is(err, rollback) {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestRemoteUploadBatchRejectsProviderDrift(t *testing.T) {
	f := newRemoteBatchFixture(t, cloud.ProviderQuark)
	original := f.driver.items["uploaded-video"]
	for _, change := range []string{"missing", "parent", "name", "size"} {
		t.Run(change, func(t *testing.T) {
			item := original
			switch change {
			case "parent":
				item.ParentID = "foreign"
			case "name":
				item.Name = "foreign.mkv"
			case "size":
				item.Size++
			}
			f.driver.items[item.ID] = item
			if change == "missing" {
				delete(f.driver.items, item.ID)
			}
			if err := f.libraries.ReconcileTransferBatch(context.Background(), f.task.ID); err == nil {
				t.Fatal("changed target was published")
			}
			f.driver.items[item.ID] = original
		})
	}
	if entries := catalogReadEntries(t, f.catalog, f.library.ID, ""); len(entries) != 0 {
		t.Fatal("invalid provider facts entered catalog")
	}
}

func TestRemoteUploadBatchRejectsDuplicateTargetIdentity(t *testing.T) {
	f := newRemoteBatchFixture(t, cloud.ProviderQuark)
	manifest := f.manifest
	secondSource := manifest.Files[0]
	secondSource.RemoteFileToken, secondSource.RelativePath = "file:second", "Movie.2024.zh.srt"
	manifest.Files = append(append(manifest.Files[:0:0], manifest.Files...), secondSource)
	var rows []models.RemoteUploadFile
	if err := f.queue.db.Where("transfer_task_id = ?", f.task.ID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	second := rows[0]
	second.ID, second.Ordinal = 0, 1
	second.SourceFileToken, second.SourceRelative = secondSource.RemoteFileToken, secondSource.RelativePath
	second.TargetRelative, second.TargetItemID = "电影/Movie (2024)/Movie (2024).zh.srt", "second-uploaded-sidecar"
	if err := f.queue.db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	rows = append(rows, second)
	raw, _ := json.Marshal(manifest)
	f.task.ManifestJSON, f.task.TotalFiles, f.task.ProcessedFiles = string(raw), 2, 2
	if err := f.queue.db.Save(&f.task).Error; err != nil {
		t.Fatal(err)
	}
	if err := captureRemoteUploadManagedItems(f.queue.db, f.task, f.download, rows); err != nil {
		t.Fatal(err)
	}
	var storage models.Storage
	if err := f.queue.db.First(&storage, f.library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	batches, err := buildRemoteUploadBatches(rows, storage, f.download, manifest)
	if err != nil || len(batches) != 1 {
		t.Fatal(err)
	}
	// Construct a second valid synthetic frozen plan, then change only its
	// returned target identity. The rejection must not rely on a stale digest.
	payload, _ := json.Marshal(batches[0].plan)
	key := remoteUploadOperationKey(f.task.ID, 0)
	plan := nodeprotocol.OperationPlan{ProtocolVersion: nodeprotocol.VersionV1, OperationKey: key, TaskID: f.download.ID, NodeID: *f.task.NodeID, Kind: nodeprotocol.OperationKindStorageUpload, PlanRevision: 1, Payload: payload}
	digest, _ := plan.Digest()
	if err := f.queue.db.Model(&models.RemoteOperation{}).Where("operation_key = ?", key).Update("plan_digest", digest).Error; err != nil {
		t.Fatal(err)
	}
	var items []models.MediaManagedItem
	if err := f.queue.db.Where("transfer_task_id = ?", f.task.ID).Find(&items).Error; err != nil {
		t.Fatal(err)
	}
	if count, err := remoteUploadBatchEvidenceTx(f.queue.db, f.task, f.download, storage, items); err != nil || count != 2 {
		t.Fatalf("valid two-file control failed: count=%d err=%v", count, err)
	}
	if err := f.queue.db.Model(&models.RemoteUploadFile{}).Where("id = ?", second.ID).Update("target_item_id", rows[0].TargetItemID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Model(&models.MediaManagedItem{}).Where("transfer_task_id = ? AND relative_path = ?", f.task.ID, second.TargetRelative).Update("provider_item_id", rows[0].TargetItemID).Error; err != nil {
		t.Fatal(err)
	}
	stats, lists := f.driver.statCalls, f.driver.listCalls
	if err := f.libraries.ReconcileTransferBatch(context.Background(), f.task.ID); err == nil {
		t.Fatal("two files claimed one target identity")
	}
	if stats != f.driver.statCalls || lists != f.driver.listCalls {
		t.Fatal("duplicate target reached provider")
	}
}

func TestRemoteUploadBatchSkippedFilesAreNotPublished(t *testing.T) {
	f := newRemoteBatchFixture(t, cloud.ProviderQuark)
	if err := f.queue.db.Model(&models.RemoteUploadFile{}).Where("transfer_task_id = ?", f.task.ID).Updates(map[string]any{"status": nodeprotocol.StorageFileSkipped, "target_item_id": "", "target_sha1": "", "target_digest_json": ""}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.queue.db.Where("transfer_task_id = ?", f.task.ID).Delete(&models.MediaManagedItem{}).Error; err != nil {
		t.Fatal(err)
	}
	stats, lists := f.driver.statCalls, f.driver.listCalls
	if err := f.libraries.ReconcileTransferBatch(context.Background(), f.task.ID); err != nil {
		t.Fatal(err)
	}
	if stats != f.driver.statCalls || lists != f.driver.listCalls {
		t.Fatal("skipped file was inspected for publication")
	}
	if entries := catalogReadEntries(t, f.catalog, f.library.ID, ""); len(entries) != 0 {
		t.Fatal("skipped file entered catalog")
	}
}
