package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

type versionedReorganizationFixture struct {
	store    *CatalogSnapshotStore
	worker   *MediaReorganizationWorker
	library  models.MediaLibrary
	storage  models.Storage
	profile  models.MediaClassificationProfile
	task     models.MediaReorganizationTask
	download models.DownloadTask
	plan     reorganizationPlan
	state    reorganizationState
	claim    ClaimedJob
	entries  []models.MediaLibraryEntry
	driver   *fakeMutationCloudDriver
}

func newVersionedReorganizationFixture(t *testing.T, provider ...bool) versionedReorganizationFixture {
	t.Helper()
	store, library, rec, entries := catalogFixture(t)
	root := t.TempDir()
	var storage models.Storage
	var profile models.MediaClassificationProfile
	var user models.User
	for _, read := range []error{store.writeDB.First(&storage, library.StorageID).Error, store.writeDB.First(&profile, library.ProfileID).Error, store.writeDB.First(&user).Error} {
		if read != nil {
			t.Fatal(read)
		}
	}
	storage.RootPath, storage.Type, storage.Enabled = root, models.StorageTypeLocal, true
	library.RelativeRoot, library.Enabled = "/", true
	if len(provider) > 1 && provider[1] {
		library.STRMEnabled = true
		library.SignedProxyEnabled = true
		library.DirtyGeneration = 9
		library.ArtifactGeneration = 3
	}
	var driver *fakeMutationCloudDriver
	if len(provider) > 0 && provider[0] {
		connection := models.Connection{Name: "reorg115", NameNormalized: "reorg115", Provider: cloudpkg.ProviderPan115, Enabled: true, Revision: 1}
		if err := store.writeDB.Create(&connection).Error; err != nil {
			t.Fatal(err)
		}
		storage.RootPath, storage.Type, storage.ConnectionID = "storage-root", models.StorageTypePan115, &connection.ID
		library.ProviderRootID = "library-root"
		driver = newFakeMutationCloudDriver()
		for _, item := range []cloudpkg.Item{{ID: "0", Name: "root", IsDir: true}, {ID: "storage-root", Name: "storage", ParentID: "0", IsDir: true}, {ID: "library-root", ParentID: "storage-root", Name: "library", IsDir: true}, {ID: "show-root", ParentID: "library-root", Name: "Show", IsDir: true}, {ID: "old-parent", ParentID: "show-root", Name: "Season 02", IsDir: true}} {
			driver.items[item.ID] = item
		}
	}
	if err := store.writeDB.Save(&storage).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Save(&library).Error; err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		entry := &entries[i]
		entry.ProviderID = ""
		if driver != nil {
			entry.ProviderID = []string{"file-a", "file-b"}[i]
			driver.items[entry.ProviderID] = cloudpkg.Item{ID: entry.ProviderID, ParentID: "old-parent", Name: filepath.Base(entry.RelativePath), Size: 3}
		}
		entry.Size = 3
		name := filepath.Join(root, filepath.FromSlash(entry.RelativePath))
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		entry.ModifiedAt = info.ModTime()
	}
	wrong := int64(777)
	entries[0].TMDBID = &wrong
	entries[0].WorkKey = "series:tmdb:777"
	if err := store.writeDB.Save(&entries).Error; err != nil {
		t.Fatal(err)
	}
	rec.ManualOverride = true
	catalogConvert(t, store, library, rec, entries)
	audit := NewAuditService(store.writeDB)
	queue := NewQueueService(store.writeDB, audit)
	service := NewMediaReorganizationService(store.writeDB, audit, queue, nil, nil, zerolog.Nop())
	if driver != nil {
		service.connections = &ConnectionService{db: store.writeDB, drivers: map[uint]cloudpkg.Driver{*storage.ConnectionID: driver}}
	}
	libraries := NewMediaLibraryService(store.writeDB, audit, zerolog.Nop())
	libraries.SetCatalogSnapshotStore(store)
	libraries.SetCatalogScanCommit(func(*gorm.DB, CatalogScanPublication) error { return nil })
	service.SetMediaLibraryService(libraries)
	identity := MediaIdentitySnapshot{Version: 1, Revision: 1, Source: mediaIdentitySourceAutomatic, Status: mediaIdentityStatusVerified, TMDBID: rec.TMDBID, MediaType: "tv", Title: rec.Title, Category: "TV"}
	raw, _ := json.Marshal(identity)
	for _, id := range []string{"finished-download", "finished-transfer"} {
		if err := store.writeDB.Create(&models.Job{ID: id, OwnerID: &user.ID, JobType: "transfer", Status: models.JobStatusCompleted, PayloadJSON: "{}"}).Error; err != nil {
			t.Fatal("job", err)
		}
	}
	download := models.DownloadTask{ID: "reorg-download", JobID: "finished-download", ProfileID: profile.ID, ProfileRevision: profile.Revision, OwnerID: user.ID, IdentityRevision: 1, IdentitySnapshotJSON: string(raw), TargetStorageID: &storage.ID, TargetStorageType: storage.Type, TargetStorageRoot: storage.RootPath, TargetRelativeRoot: library.RelativeRoot, TargetProviderRootID: library.ProviderRootID, TargetConnectionID: storage.ConnectionID}
	if err := store.writeDB.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	transfer := models.TransferTask{ID: "reorg-transfer", JobID: "finished-transfer", OwnerID: user.ID, LibraryID: library.ID, DownloadTaskID: download.ID, Phase: models.TransferTaskStatusCompleted}
	if err := store.writeDB.Create(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	managed := models.MediaManagedItem{LibraryID: library.ID, TransferTaskID: transfer.ID, DownloadTaskID: download.ID, Kind: models.MediaManagedItemKindVideo, RelativePath: entries[0].RelativePath, Size: 3, Managed: true, Active: true, IdentityRevision: 1}
	if driver != nil {
		managed.ProviderItemID, managed.ProviderParentID = entries[0].ProviderID, "old-parent"
	}
	if err := store.writeDB.Create(&managed).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.First(&transfer, "id=?", transfer.ID).Error; err != nil {
		t.Fatal(err)
	}
	newID := int64(999)
	target := identity
	target.Revision = 2
	target.Source = mediaIdentitySourceManual
	target.Locked = true
	target.TMDBID = &newID
	target.Title = "Correct Show"
	targetRaw, _ := json.Marshal(target)
	plan := reorganizationPlan{Version: 2, LibraryID: library.ID, TransferTaskID: transfer.ID, StorageType: storage.Type, RuleFingerprint: libraryRuleFingerprint(library), Items: []reorganizationPlanItem{{ManagedItemID: managed.ID, Kind: managed.Kind, OldRelativePath: managed.RelativePath, NewRelativePath: "Correct Show/Season 02/01.h264.mkv", Size: 3, Action: "move"}}, VerifiedResult: &MediaRecognitionResult{Status: "matched", MediaType: "tv", Title: target.Title, TMDBID: &newID, Snapshot: tmdb.Snapshot{Version: 1, TMDBID: newID, MediaType: "tv", Title: target.Title, PosterPath: "/correct.jpg"}}}
	plan.Items[0].ProviderItemID, plan.Items[0].ProviderParentID = managed.ProviderItemID, managed.ProviderParentID
	plan.ManagedRevision = transfer.ManagedRevision
	if driver != nil {
		plan.Items[0].NewRelativePath = "Correct Show/Season 02/Correct Show - S02E01.h264.mkv"
	}
	if err := service.freezeReorganizationPreview(context.Background(), &plan, library, storage, profile); err != nil {
		t.Fatal(err)
	}
	planRaw, _ := json.Marshal(plan)
	state := reorganizationState{Version: 1, Completed: map[uint]bool{}}
	task := models.MediaReorganizationTask{ID: "reorg-task", OwnerID: user.ID, LibraryID: library.ID, TransferTaskID: transfer.ID, SourceIdentityRevision: 1, TargetIdentityRevision: 2, TargetIdentityJSON: string(targetRaw), ManagedManifestDigest: managedManifestDigest([]models.MediaManagedItem{managed}), RuleRevision: library.ProfileRevision, PlanJSON: string(planRaw), Phase: models.MediaReorganizationPhaseQueued, TotalItems: 1}
	_, err := queue.EnqueueWith(EnqueueJobInput{OwnerID: user.ID, JobType: JobTypeMediaReorganization, DisplayName: "test", Payload: mediaReorganizationJobPayload{ReorganizationTaskID: task.ID}}, func(tx *gorm.DB, job models.Job) error {
		task.JobID = job.ID
		if err := service.freezeReorganizationConfirmationTx(tx, task.ID, library.ID, plan, &state); err != nil {
			return err
		}
		raw, _ := json.Marshal(state)
		task.StateJSON = string(raw)
		return tx.Create(&task).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim([]string{JobTypeMediaReorganization})
	if err != nil || claim == nil {
		t.Fatalf("claim %v", err)
	}
	return versionedReorganizationFixture{store: store, worker: NewMediaReorganizationWorker(service), library: library, storage: storage, profile: profile, task: task, download: download, plan: plan, state: state, claim: *claim, entries: entries, driver: driver}
}

func (f versionedReorganizationFixture) run() WorkerResult {
	return f.worker.Run(context.Background(), workerRuntime{queue: f.worker.service.queue, job: f.claim}, f.claim)
}

func TestReorganizationVersionedAtomicManualCorrectionAndFullScan(t *testing.T) {
	f := newVersionedReorganizationFixture(t)
	var before []models.MediaLibraryEntry
	if err := f.store.writeDB.Order("id").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	var mask uint64
	if err := f.store.Read(context.Background(), []uint{f.library.ID}, func(r *CatalogReader) error {
		var fact models.CatalogEntryFact
		err := r.RawEntryFacts().Where("id=?", f.entries[0].ID).Take(&fact).Error
		mask = fact.SharedOverrideMask
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if result := f.run(); result.ErrorCode != "" {
		t.Fatalf("run %+v", result)
	}
	rows := catalogReadEntries(t, f.store, f.library.ID, "")
	if len(rows) != 2 || rows[0].Title != "Correct Show" || rows[0].TMDBID == nil || *rows[0].TMDBID != 999 || rows[0].WorkKey != "series:tmdb:999" || *rows[0].Season != 2 || *rows[0].Episode != 1 || rows[1].Title != f.entries[1].Title || *rows[1].RecognitionID != *f.entries[1].RecognitionID {
		t.Fatalf("rows %+v", rows)
	}
	var after []models.MediaLibraryEntry
	_ = f.store.writeDB.Order("id").Find(&after).Error
	if !reflect.DeepEqual(before, after) {
		t.Fatal("mutated raw anchors")
	}
	var records []models.MediaLibraryRecognition
	if err := f.store.Read(context.Background(), []uint{f.library.ID}, func(r *CatalogReader) error {
		var fact models.CatalogEntryFact
		if err := r.RawEntryFacts().Where("id=?", rows[0].ID).Take(&fact).Error; err != nil {
			return err
		}
		if fact.SharedOverrideMask != mask {
			t.Fatal("mask cleared")
		}
		return r.Recognitions().Find(&records).Error
	}); err != nil {
		t.Fatal(err)
	}
	manual := false
	for _, rec := range records {
		if rec.TMDBID != nil && *rec.TMDBID == 999 {
			_, snap, err := decodeRecognitionMetadata(rec.MetadataJSON)
			if err != nil || snap.PosterPath != "/correct.jpg" || !rec.ManualOverride {
				t.Fatal("metadata not frozen")
			}
			manual = true
		}
	}
	if !manual {
		t.Fatal("missing manual R")
	}
	var refs int64
	_ = f.store.writeDB.Model(&models.CatalogSnapshotReference{}).Where("owner_kind=? AND owner_id=?", "reorganization", f.task.ID).Count(&refs).Error
	if refs != 0 {
		t.Fatal("completed references retained")
	}
	libraries := f.worker.service.libraries
	library, run := catalogScanRun(t, libraries, f.library.ID, f.storage, f.profile)
	files := []medialibrary.File{scanFile(rows[0]), scanFile(rows[1])}
	if _, err := libraries.publishCatalogScan(context.Background(), library, f.storage, f.profile, run, medialibrary.Result{Files: files}, true, func(*gorm.DB, CatalogScanPublication) error { return nil }); err != nil {
		t.Fatal(err)
	}
	afterScan := catalogReadEntries(t, f.store, f.library.ID, "")
	if len(afterScan) != 2 || afterScan[0].TMDBID == nil || *afterScan[0].TMDBID != 999 || afterScan[1].TMDBID == nil || *afterScan[1].TMDBID != 42 {
		t.Fatalf("full scan lost manual split %+v", afterScan)
	}
}

func TestReorganizationVersionedFinalizeFailurePreservesBoundRetry(t *testing.T) {
	f := newVersionedReorganizationFixture(t)
	if err := f.store.writeDB.Exec("CREATE TRIGGER reject_reorganization_complete BEFORE UPDATE OF phase ON media_reorganization_tasks WHEN NEW.phase='reconciling' BEGIN SELECT RAISE(ABORT,'fixture'); END").Error; err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r.ErrorCode == "" {
		t.Fatal("wanted failure")
	}
	if _, err := os.Stat(filepath.Join(f.storage.RootPath, filepath.FromSlash(f.plan.Items[0].NewRelativePath))); err != nil {
		t.Fatal(err)
	}
	rows := catalogReadEntries(t, f.store, f.library.ID, "")
	if rows[0].RelativePath != f.entries[0].RelativePath {
		t.Fatal("failed finalize published")
	}
	var current models.MediaManagedItem
	if err := f.store.writeDB.First(&current, f.plan.Items[0].ManagedItemID).Error; err != nil {
		t.Fatal(err)
	}
	if current.RelativePath != f.plan.Items[0].OldRelativePath || current.IdentityRevision != 1 {
		t.Fatal("partial manifest advanced")
	}
	var refs int64
	_ = f.store.writeDB.Model(&models.CatalogSnapshotReference{}).Where("owner_kind=?", "reorganization").Count(&refs).Error
	if refs == 0 {
		t.Fatal("lost retry binding")
	}
	if err := f.store.writeDB.Exec("DROP TRIGGER reject_reorganization_complete").Error; err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r.ErrorCode != "" {
		t.Fatalf("retry %+v", r)
	}
}

func TestReorganizationVersionedRejectsSourceLeaseAndOldPlanBeforeMove(t *testing.T) {
	for _, kind := range []string{"source", "lease", "old-plan", "managed"} {
		t.Run(kind, func(t *testing.T) {
			f := newVersionedReorganizationFixture(t)
			switch kind {
			case "source":
				_ = f.store.writeDB.Model(&models.Storage{}).Where("id=?", f.storage.ID).Update("root_path", t.TempDir()).Error
			case "lease":
				_ = f.store.writeDB.Model(&models.Job{}).Where("id=?", f.claim.Job.ID).Update("lease_token_hash", "replaced").Error
			case "old-plan":
				f.plan.Version = 1
				f.plan.CatalogFence = nil
				raw, _ := json.Marshal(f.plan)
				_ = f.store.writeDB.Model(&f.task).Update("plan_json", string(raw)).Error
			case "managed":
				_ = f.store.writeDB.Model(&models.MediaManagedItem{}).Where("id=?", f.plan.Items[0].ManagedItemID).Update("active", false).Error
			}
			if r := f.run(); r.ErrorCode == "" {
				t.Fatal("wanted refusal")
			}
			if _, err := os.Stat(filepath.Join(f.storage.RootPath, filepath.FromSlash(f.plan.Items[0].OldRelativePath))); err != nil {
				t.Fatal("moved before fence", err)
			}
		})
	}
}

func TestReorganizationVersionedBudgetPrecedesPhysicalChanges(t *testing.T) {
	f := newVersionedReorganizationFixture(t)
	c, token := catalogCandidate(t, f.store, f.library, "delta", 1)
	if err := f.store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{{MediaLibraryRecognition: models.MediaLibraryRecognition{ID: *f.entries[0].RecognitionID, LibraryID: f.library.ID, SourceKey: "show", ManualOverride: true, Status: "matched", MediaType: "tv", Title: "Show"}}}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, f.store, c, token)
	if err := f.store.writeDB.Model(&models.CatalogSnapshot{}).Where("id=?", c.ID).Update("row_count", CatalogMaxDeltaRows).Error; err != nil {
		t.Fatal(err)
	}
	p, err := f.worker.prepareReorganization(context.Background(), f.task, f.plan, f.state, f.claim)
	defer f.worker.abandonReorganization(&p)
	if !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("budget %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.storage.RootPath, filepath.FromSlash(f.plan.Items[0].OldRelativePath))); err != nil {
		t.Fatal(err)
	}
}

func TestReorganizationVersionedPan115PartialRenameRetry(t *testing.T) {
	f := newVersionedReorganizationFixture(t, true)
	f.driver.renameErrOnce = true
	if r := f.run(); r.ErrorCode == "" {
		t.Fatal("expected provider rename failure")
	}
	assertReorganizationPhysicalState(t, f.store.writeDB, f.task.ID, "quiescent")
	if f.driver.moveCalls != 1 {
		t.Fatalf("moves=%d", f.driver.moveCalls)
	}
	rows := catalogReadEntries(t, f.store, f.library.ID, "")
	if rows[0].RelativePath != f.entries[0].RelativePath {
		t.Fatal("partial provider move published")
	}
	if r := f.run(); r.ErrorCode != "" {
		t.Fatalf("retry %+v", r)
	}
	assertReorganizationPhysicalState(t, f.store.writeDB, f.task.ID, "settled")
	if f.driver.moveCalls != 1 || f.driver.renameCalls != 2 {
		t.Fatalf("replayed provider mutation moves=%d rename=%d", f.driver.moveCalls, f.driver.renameCalls)
	}
	rows = catalogReadEntries(t, f.store, f.library.ID, "")
	if rows[0].TMDBID == nil || *rows[0].TMDBID != 999 || rows[0].ProviderID != "file-a" {
		t.Fatalf("wrong publication %+v", rows)
	}
}

func TestReorganizationVersionedCompactionRetainsBindingBeforeAndAfterPhysical(t *testing.T) {
	f := newVersionedReorganizationFixture(t)
	if _, err := f.store.Compact(context.Background(), CatalogCompactionInput{LibraryID: f.library.ID, Force: true}); err != nil {
		t.Fatal(err)
	}
	p, err := f.worker.prepareReorganization(context.Background(), f.task, f.plan, f.state, f.claim)
	if err != nil {
		t.Fatal(err)
	}
	defer f.worker.abandonReorganization(&p)
	if err := f.worker.executeCatalogReorganization(context.Background(), workerRuntime{queue: f.worker.service.queue, job: f.claim}, f.task, f.library, f.storage, f.plan, &f.state, f.claim, p.Facts); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Compact(context.Background(), CatalogCompactionInput{LibraryID: f.library.ID, Force: true}); err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r.ErrorCode != "" {
		t.Fatalf("compaction retry %+v", r)
	}
}

func TestReorganizationVersionedLocalRejectsSameSizeChangedSource(t *testing.T) {
	f := newVersionedReorganizationFixture(t)
	path := filepath.Join(f.storage.RootPath, filepath.FromSlash(f.plan.Items[0].OldRelativePath))
	stamp := f.entries[0].ModifiedAt.Add(24 * time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r.ErrorCode == "" {
		t.Fatal("same-size replacement accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestReorganizationVersionedPendingArtifactsSurviveUnavailableQueue(t *testing.T) {
	f := newVersionedReorganizationFixture(t, true, true)
	artifacts := NewMediaArtifactService(f.store.writeDB, nil, nil, zerolog.Nop())
	artifacts.SetCatalogSnapshotStore(f.store)
	f.worker.service.libraries.SetArtifactService(artifacts)
	f.worker.service.libraries.SetMediaChangeService(NewMediaChangeService(f.store.writeDB))
	if r := f.run(); r.ErrorCode != "" {
		t.Fatalf("committed reorganization failed on artifact queue %+v", r)
	}
	var binding models.CatalogArtifactBinding
	var change models.MediaLibraryChange
	if err := f.store.writeDB.First(&binding, "library_id=?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.store.writeDB.First(&change, "library_id=?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if binding.Generation != 10 || change.Generation != 10 || binding.State != "pending" || change.State != models.MediaLibraryChangePending || binding.HeadRevision != 2 {
		t.Fatalf("binding %+v change %+v", binding, change)
	}
}

func TestReorganizationVersionedReconcileUsesIndependent250RowCommits(t *testing.T) {
	f := newVersionedReorganizationFixture(t)
	c, token := catalogCandidate(t, f.store, f.library, "delta", 1)
	requests := make([]CatalogIdentityRequest, 0, 250)
	for i := 0; i < 250; i++ {
		requests = append(requests, CatalogIdentityRequest{Kind: "entry", SourceKey: fmt.Sprintf("extra/%03d.mkv", i)})
	}
	ids, err := f.store.ResolveIdentities(context.Background(), c.ID, token, requests)
	if err != nil {
		t.Fatal(err)
	}
	batch := CatalogFactBatch{}
	managed := make([]models.MediaManagedItem, 0, 250)
	for i, id := range ids {
		path := requests[i].SourceKey
		absolute := filepath.Join(f.storage.RootPath, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte("abc"), 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(absolute)
		if err != nil {
			t.Fatal(err)
		}
		entry := f.entries[0]
		entry.ID = id
		entry.RelativePath = path
		entry.ModifiedAt = info.ModTime()
		batch.Entries = append(batch.Entries, CatalogEntryFromLegacy(entry, nil))
		managed = append(managed, models.MediaManagedItem{OpaqueID: fmt.Sprintf("managed-%d", i), LibraryID: f.library.ID, TransferTaskID: f.task.TransferTaskID, DownloadTaskID: f.download.ID, IdentityRevision: 1, Kind: "video", RelativePath: path, Size: 3, Managed: true, Active: true})
	}
	if err := f.store.AppendBatch(context.Background(), c.ID, token, batch); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, f.store, c, token)
	if err := f.store.writeDB.Create(&managed).Error; err != nil {
		t.Fatal(err)
	}
	for _, m := range managed {
		f.plan.Items = append(f.plan.Items, reorganizationPlanItem{ManagedItemID: m.ID, Kind: "video", OldRelativePath: m.RelativePath, NewRelativePath: m.RelativePath, Size: 3, Action: "unchanged"})
	}
	var all []models.MediaManagedItem
	if err := f.store.writeDB.Where("transfer_task_id=?", f.task.TransferTaskID).Order("id").Find(&all).Error; err != nil {
		t.Fatal(err)
	}
	var transfer models.TransferTask
	if err := f.store.writeDB.First(&transfer, "id=?", f.task.TransferTaskID).Error; err != nil {
		t.Fatal(err)
	}
	f.plan.ManagedRevision = transfer.ManagedRevision
	f.task.ManagedManifestDigest = managedManifestDigest(all)
	f.task.TotalItems = len(all)
	planRaw, _ := json.Marshal(f.plan)
	f.task.PlanJSON = string(planRaw)
	if err := f.store.writeDB.Transaction(func(tx *gorm.DB) error {
		if err := ReleaseCatalogBindingTx(tx, f.state.Catalog.binding(*f.plan.CatalogFence), "reorganization", f.task.ID); err != nil {
			return err
		}
		if err := f.worker.service.freezeReorganizationConfirmationTx(tx, f.task.ID, f.library.ID, f.plan, &f.state); err != nil {
			return err
		}
		raw, _ := json.Marshal(f.state)
		f.task.StateJSON = string(raw)
		return tx.Save(&f.task).Error
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.writeDB.Exec(`CREATE TRIGGER reject_second_manifest_batch BEFORE UPDATE ON media_reorganization_tasks WHEN json_extract(NEW.state_json,'$.catalog.after_managed')>250 BEGIN SELECT RAISE(ABORT,'stop after first committed batch'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r.ErrorCode == "" {
		t.Fatal("wanted second-batch failure")
	}
	var stored models.MediaReorganizationTask
	if err := f.store.writeDB.First(&stored, "id=?", f.task.ID).Error; err != nil {
		t.Fatal(err)
	}
	var state reorganizationState
	if err := json.Unmarshal([]byte(stored.StateJSON), &state); err != nil {
		t.Fatal(err)
	}
	if state.Catalog.Stage != "reconciling" || state.Catalog.AfterManaged != 250 {
		t.Fatalf("cursor %+v", state.Catalog)
	}
	assertReorganizationPhysicalState(t, f.store.writeDB, f.task.ID, "quiescent")
	var updated int64
	if err := f.store.writeDB.Model(&models.MediaManagedItem{}).Where("transfer_task_id=? AND identity_revision=2", f.task.TransferTaskID).Count(&updated).Error; err != nil || updated != 250 {
		t.Fatalf("bounded persisted rows %d %v", updated, err)
	}
	if err := f.store.writeDB.Exec("DROP TRIGGER reject_second_manifest_batch").Error; err != nil {
		t.Fatal(err)
	}
	// Removing a fixture-only target proves resume performs no media IO after
	// publication: it only completes the already-authorized manifest cursor.
	if err := os.Remove(filepath.Join(f.storage.RootPath, filepath.FromSlash(f.plan.Items[0].NewRelativePath))); err != nil {
		t.Fatal(err)
	}
	if r := f.run(); r.ErrorCode != "" {
		t.Fatalf("resume %+v", r)
	}
	assertReorganizationPhysicalState(t, f.store.writeDB, f.task.ID, "settled")
	if err := f.store.writeDB.Model(&models.MediaManagedItem{}).Where("transfer_task_id=? AND identity_revision=2", f.task.TransferTaskID).Count(&updated).Error; err != nil || updated != 251 {
		t.Fatalf("final rows %d %v", updated, err)
	}
}

func TestReorganizationVersionedHistoryCleanupPreservesRecoveryAndDetachesCompleted(t *testing.T) {
	for _, mode := range []string{"completed", "never_started", "physical_started", "old_proof"} {
		t.Run(mode, func(t *testing.T) {
			f := newVersionedReorganizationFixture(t)
			switch mode {
			case "completed":
				if r := f.run(); r.ErrorCode != "" {
					t.Fatal(r)
				}
			case "physical_started":
				if err := f.store.writeDB.Exec("CREATE TRIGGER fail_publish BEFORE UPDATE OF phase ON media_reorganization_tasks WHEN NEW.phase='reconciling' BEGIN SELECT RAISE(ABORT,'fixture'); END").Error; err != nil {
					t.Fatal(err)
				}
				if r := f.run(); r.ErrorCode == "" {
					t.Fatal("expected publication failure")
				}
			case "old_proof":
				f.state.Catalog.ExecutionProofVersion = 0
				raw, _ := json.Marshal(f.state)
				if err := f.store.writeDB.Model(&f.task).Update("state_json", string(raw)).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := f.worker.service.queue.Complete(f.claim.Job.ID, f.claim.LeaseToken); err != nil {
				t.Fatal(err)
			}
			err := f.store.writeDB.Transaction(func(tx *gorm.DB) error {
				_, err := cleanupTransferHistoryDependencies(tx, f.task.TransferTaskID)
				return err
			})
			if mode == "physical_started" || mode == "old_proof" {
				if ErrorCode(err) != CodeQueueStateConflict {
					t.Fatalf("unsafe cleanup %v", err)
				}
				var refs int64
				_ = f.store.writeDB.Model(&models.CatalogSnapshotReference{}).Where("owner_kind=? AND owner_id=?", "reorganization", f.task.ID).Count(&refs).Error
				if refs == 0 {
					t.Fatal("recovery refs removed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var jobs, snapshots int64
			_ = f.store.writeDB.Model(&models.Job{}).Where("id=?", f.claim.Job.ID).Count(&jobs).Error
			_ = f.store.writeDB.Model(&models.CatalogSnapshot{}).Where("job_id=?", f.claim.Job.ID).Count(&snapshots).Error
			if jobs != 0 || snapshots != 0 {
				t.Fatalf("dangling task ownership jobs=%d snapshots=%d", jobs, snapshots)
			}
			if mode == "completed" {
				rows := catalogReadEntries(t, f.store, f.library.ID, "")
				if len(rows) != 2 || rows[0].TMDBID == nil || *rows[0].TMDBID != 999 {
					t.Fatal("history cleanup removed catalog")
				}
			}
		})
	}
}

func TestReorganizationManualSplitSurvivesMixedDirectoryGrouping(t *testing.T) {
	a, b, c := uint(1), uint(2), uint(3)
	files := []medialibrary.File{{RelativePath: "/Show/Season 02/01.h264.mkv", ProviderID: "a", ProviderIDStable: true, Size: 10}, {RelativePath: "/Show/Season 02/01.h265.mkv", ProviderID: "b", ProviderIDStable: true, Size: 20}, {RelativePath: "/Show/Season 02/02.mkv", ProviderID: "replacement-c", ProviderIDStable: true, Size: 30}}
	units := medialibrary.GroupRecognitionUnits(files)
	if len(units) != 1 {
		t.Fatalf("expected actual shared folder unit %+v", units)
	}
	entries := []models.MediaLibraryEntry{{ID: 1, RelativePath: files[0].RelativePath, ProviderID: "a", RecognitionID: &a}, {ID: 2, RelativePath: files[1].RelativePath, ProviderID: "b", RecognitionID: &b}, {ID: 3, RelativePath: files[2].RelativePath, ProviderID: "old-c", RecognitionID: &c}}
	records := []models.MediaLibraryRecognition{{ID: a, SourceKey: "manual-a", ManualOverride: true}, {ID: b, SourceKey: "manual-b", ManualOverride: true}, {ID: c, SourceKey: units[0].SourceKey, ManualOverride: true}}
	got := stabilizeRecognitionUnits(units, entries, records)
	if len(got) != 3 {
		t.Fatalf("manual identities merged on scan: %+v", got)
	}
	byProvider := map[string]string{}
	for _, unit := range got {
		for _, file := range unit.Files {
			byProvider[file.ProviderID] = unit.SourceKey
		}
	}
	if byProvider["a"] != "manual-a" || byProvider["b"] != "manual-b" || byProvider["replacement-c"] == records[2].SourceKey {
		t.Fatalf("manual/source alias error %+v", byProvider)
	}
}

func TestReorganizationManualIdentityDoesNotFollowReusedProviderPath(t *testing.T) {
	id := uint(1)
	file := medialibrary.File{RelativePath: "/Show/Season 02/01.mkv", ProviderID: "new-file", ProviderIDStable: true}
	units := medialibrary.GroupRecognitionUnits([]medialibrary.File{file})
	record := models.MediaLibraryRecognition{ID: id, SourceKey: units[0].SourceKey, ManualOverride: true}
	got := stabilizeRecognitionUnits(units, []models.MediaLibraryEntry{{ID: 1, RelativePath: file.RelativePath, ProviderID: "old-file", RecognitionID: &id}}, []models.MediaLibraryRecognition{record})
	if len(got) != 1 || got[0].SourceKey == record.SourceKey {
		t.Fatalf("reused path inherited old manual identity %+v", got)
	}
}
