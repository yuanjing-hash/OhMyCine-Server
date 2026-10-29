package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"gorm.io/gorm"
)

type versionUpgradeFixture struct {
	service *MediaVersionUpgradeService
	actor   Actor
	library models.MediaLibrary
	storage models.Storage
	old     models.MediaLibraryEntry
}

func newVersionUpgradeFixture(t *testing.T) versionUpgradeFixture {
	t.Helper()
	libraries, db, actor, storage, profile := mediaLibraryTestService(t)
	actor.Permissions[authz.PermissionDownloadsCreate] = struct{}{}
	actor.Permissions[authz.PermissionMediaLibrariesMediaDelete] = struct{}{}
	persistFixtureAuthority(t, db, actor)
	detail, err := libraries.Create(context.Background(), actor, testLibraryInput("upgrade", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id=?", detail.ID).Updates(map[string]any{"enabled": true, "metadata_artifacts_enabled": false, "baseline_generation": 1, "dirty_generation": 1, "status": models.MediaLibraryStatusListening}).Error; err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, detail.ID).Error; err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(storage.RootPath, "Show.S01E02.old.mkv")
	if err := os.WriteFile(file, []byte("old-video"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	tmdb, season, episode := int64(42), 1, 2
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/Show.S01E02.old.mkv", ProviderID: "old-physical", Size: info.Size(), ModifiedAt: info.ModTime().UTC(), MediaType: "tv", Title: "Show", SeriesTitle: "Show", WorkKey: "series:42", Season: &season, Episode: &episode, TMDBID: &tmdb, MatchStatus: "matched", CategoryName: "TV", LastGeneration: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	return versionUpgradeFixture{service: NewMediaVersionUpgradeService(db, libraries, nil, NewQueueService(db, libraries.audit)), actor: actor, library: library, storage: storage, old: entry}
}
func (f versionUpgradeFixture) prepare(t *testing.T) (models.MediaVersionUpgrade, mediaUpgradeSnapshot) {
	t.Helper()
	dto, err := f.service.Prepare(context.Background(), f.actor, f.library.ID, encodeCatalogToken(f.old.WorkKey), f.old.ID)
	if err != nil {
		t.Fatal(err)
	}
	var row models.MediaVersionUpgrade
	if err := f.service.db.First(&row, "id=?", dto.ID).Error; err != nil {
		t.Fatal(err)
	}
	snapshot, err := decodeMediaUpgradeSnapshot(row.SnapshotJSON)
	if err != nil {
		t.Fatal(err)
	}
	return row, snapshot
}
func (f versionUpgradeFixture) asset(t *testing.T, name string) models.MediaLibrarySourceAsset {
	t.Helper()
	target := filepath.Join(f.storage.RootPath, name)
	if err := os.WriteFile(target, []byte("subtitle"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(target)
	asset := models.MediaLibrarySourceAsset{LibraryID: f.library.ID, Generation: 1, ProviderID: "asset-" + name, RelativePath: "/" + name, Name: name, Extension: filepath.Ext(name), Size: info.Size(), ModifiedAt: info.ModTime().UTC(), Active: true, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := f.service.db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	return asset
}
func TestMediaVersionUpgradePrepareExactScopeAndNoMutation(t *testing.T) {
	f := newVersionUpgradeFixture(t)
	subtitle := f.asset(t, "Show.S01E02.old.zh-Hans.ass")
	f.asset(t, "poster.jpg")
	f.asset(t, "Show.S01E03.ass")
	f.asset(t, "tvshow.nfo")
	row, snapshot := f.prepare(t)
	if len(snapshot.Assets) != 1 || snapshot.Assets[0].ID != subtitle.ID || len(snapshot.Items) != 2 {
		t.Fatalf("unexpected cleanup scope=%+v", snapshot.Assets)
	}
	repeated, _ := f.prepare(t)
	if row.ID != repeated.ID {
		t.Fatalf("double-click created second intent %s != %s", row.ID, repeated.ID)
	}
	if _, err := os.Stat(filepath.Join(f.storage.RootPath, "Show.S01E02.old.mkv")); err != nil {
		t.Fatal("prepare changed old file", err)
	}
	var count int64
	f.service.db.Model(&models.DownloadTask{}).Count(&count)
	if count != 0 {
		t.Fatal("prepare created download")
	}
	dto, err := f.service.Get(context.Background(), f.actor, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(dto)
	for _, private := range []string{"snapshot_json", "provider_id", "old-physical", f.storage.RootPath, subtitle.RelativePath} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("DTO leaked private fact %q: %s", private, raw)
		}
	}
}

func TestMediaVersionUpgradePrepareRefusesOldMultiEpisodePhysicalFile(t *testing.T) {
	for _, test := range []struct {
		name  string
		valid bool
	}{
		{"Show.S01E02-E03.old.mkv", false},
		{"Show.S01E02E04.old.mkv", false},
		{"Unlabelled-video.mkv", false},
		{"Show [01-52]/Season 01/Show.S01E02.old.mkv", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newVersionUpgradeFixture(t)
			oldPath := filepath.Join(f.storage.RootPath, "Show.S01E02.old.mkv")
			newPath := filepath.Join(f.storage.RootPath, filepath.FromSlash(test.name))
			if err := os.MkdirAll(filepath.Dir(newPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(oldPath, newPath); err != nil {
				t.Fatal(err)
			}
			if err := f.service.db.Model(&f.old).Update("relative_path", "/"+test.name).Error; err != nil {
				t.Fatal(err)
			}
			_, err := f.service.Prepare(context.Background(), f.actor, f.library.ID, encodeCatalogToken(f.old.WorkKey), f.old.ID)
			if test.valid && err != nil || !test.valid && ErrorCode(err) != CodeMediaUpgradeSelection {
				t.Fatalf("physical coordinate guard valid=%v err=%v", test.valid, err)
			}
			if _, err := os.Stat(newPath); err != nil {
				t.Fatal("old file was changed by preparation")
			}
			var rows int64
			f.service.db.Model(&models.MediaVersionUpgrade{}).Count(&rows)
			if !test.valid && rows != 0 {
				t.Fatal("unsupported physical file acquired an active replacement claim")
			}
		})
	}
}
func TestMediaVersionUpgradeRejectsWrongEntryAndPermission(t *testing.T) {
	f := newVersionUpgradeFixture(t)
	if _, err := f.service.Prepare(context.Background(), f.actor, f.library.ID, encodeCatalogToken("series:99"), f.old.ID); ErrorCode(err) != CodeNotFound {
		t.Fatal(err)
	}
	delete(f.actor.Permissions, authz.PermissionMediaLibrariesMediaDelete)
	if _, err := f.service.Prepare(context.Background(), f.actor, f.library.ID, encodeCatalogToken(f.old.WorkKey), f.old.ID); ErrorCode(err) != CodePermissionDenied {
		t.Fatal(err)
	}
}
func TestMediaVersionUpgradeFenceAllowsNewHeadButRejectsOldEntryDrift(t *testing.T) {
	f := newVersionUpgradeFixture(t)
	row, snapshot := f.prepare(t)
	if err := f.service.db.Model(&models.MediaLibrary{}).Where("id=?", f.library.ID).Update("content_revision", 7).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.db.Transaction(func(tx *gorm.DB) error { return f.service.validateSnapshotTx(tx, row, snapshot) }); err != nil {
		t.Fatalf("legitimate replacement publication blocked: %v", err)
	}
	if err := f.service.db.Model(&f.old).Update("provider_id", "different-file").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.db.Transaction(func(tx *gorm.DB) error { return f.service.validateSnapshotTx(tx, row, snapshot) }); ErrorCode(err) != CodeMediaUpgradeChanged {
		t.Fatalf("drift accepted %v", err)
	}
}
func TestMediaVersionUpgradeManagedTrashReconcilesRestartAndRestoresWithoutOverwrite(t *testing.T) {
	f := newVersionUpgradeFixture(t)
	row, snapshot := f.prepare(t)
	item := snapshot.Items[0]
	if err := f.service.recycleLocalItem(context.Background(), row, item, f.library, f.storage); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.storage.RootPath, "Show.S01E02.old.mkv")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old still present %v", err)
	}
	// New service object simulates recovery with no in-memory operation state.
	restarted := NewMediaVersionUpgradeService(f.service.db, f.service.libraries, nil, f.service.queue)
	if err := restarted.recycleLocalItem(context.Background(), row, item, f.library, f.storage); err != nil {
		t.Fatalf("recovery replay failed: %v", err)
	}
	var receipt models.ManagedMediaTrash
	if err := f.service.db.Where("upgrade_id=?", row.ID).First(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	result, err := medialibrary.ScanLocal(context.Background(), f.storage.RootPath, "/", true, []string{".mkv"}, []string{".ass"}, nil)
	if err != nil || len(result.Files) != 0 {
		t.Fatalf("trash rescanned=%+v err=%v", result.Files, err)
	}
	original := filepath.Join(f.storage.RootPath, "Show.S01E02.old.mkv")
	if err := os.WriteFile(original, []byte("newer-data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RestoreManagedTrash(context.Background(), f.actor, receipt.ID); ErrorCode(err) != CodeConflict {
		t.Fatalf("restore overwrote newer file: %v", err)
	}
	_ = os.Remove(original)
	if err := restarted.RestoreManagedTrash(context.Background(), f.actor, receipt.ID); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(original)
	if string(data) != "old-video" {
		t.Fatalf("restore=%q", data)
	}
}
func TestMediaVersionUpgradeRejectsReplacedLocalFileBeforeRecycle(t *testing.T) {
	f := newVersionUpgradeFixture(t)
	row, snapshot := f.prepare(t)
	old := filepath.Join(f.storage.RootPath, "Show.S01E02.old.mkv")
	if err := os.Rename(old, old+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("old-video"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, snapshot.Items[0].ModifiedAt, snapshot.Items[0].ModifiedAt); err != nil {
		t.Fatal(err)
	}
	if err := f.service.recycleLocalItem(context.Background(), row, snapshot.Items[0], f.library, f.storage); ErrorCode(err) != CodeMediaUpgradeChanged {
		t.Fatalf("reused path/size/mtime accepted: %v", err)
	}
}
func TestMediaVersionUpgradeCompletionNeedsExactNewOutputAndArtifacts(t *testing.T) {
	f := newVersionUpgradeFixture(t)
	row, snapshot := f.prepare(t)
	newEntry := f.old
	newEntry.ID = 0
	newEntry.RelativePath = "/Show.S01E02.new.mkv"
	newEntry.ProviderID = "new-file"
	target := filepath.Join(f.storage.RootPath, "Show.S01E02.new.mkv")
	if err := os.WriteFile(target, []byte("new-video"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(target)
	newEntry.ModifiedAt = info.ModTime().UTC()
	newEntry.Size = info.Size()
	if err := f.service.db.Create(&newEntry).Error; err != nil {
		t.Fatal(err)
	}
	transfer := models.TransferTask{ID: "upgrade-transfer", OwnerID: f.actor.User.ID, JobID: "transfer-job", DownloadTaskID: "upgrade-download", LibraryID: f.library.ID, Phase: models.TransferTaskStatusCompleted, TotalFiles: 1, PlanSummaryJSON: `{"completed_files":1,"skipped_files":0}`}
	row.DownloadTaskID = transfer.DownloadTaskID
	f.createDownload(t, transfer.DownloadTaskID)
	if _, err := f.service.queue.EnqueueWith(EnqueueJobInput{OwnerID: f.actor.User.ID, JobType: "transfer", DisplayName: "replacement transfer", Payload: map[string]string{"transfer_task_id": transfer.ID}}, func(tx *gorm.DB, job models.Job) error { transfer.JobID = job.ID; return tx.Create(&transfer).Error }); err != nil {
		t.Fatal(err)
	}
	scan := models.MediaLibraryScanRun{LibraryID: f.library.ID, Kind: "transfer_batch", Status: "success", Generation: 2, CheckpointJSON: fmt.Sprintf(`{"transfer_batch_id":%q}`, transfer.ID), StartedAt: time.Now().UTC()}
	if err := f.service.db.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	item := models.MediaManagedItem{OpaqueID: "new-managed", LibraryID: f.library.ID, TransferTaskID: transfer.ID, DownloadTaskID: transfer.DownloadTaskID, RelativePath: newEntry.RelativePath, Kind: "video", Managed: true, Active: true}
	if err := f.service.db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	if _, ready, err := f.service.replacementReady(context.Background(), row, snapshot, transfer); err != nil || !ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	if err := f.service.db.Model(&f.library).Updates(map[string]any{"metadata_artifacts_enabled": true, "artifact_applied_generation": 99}).Error; err != nil {
		t.Fatal(err)
	}
	if _, ready, err := f.service.replacementReady(context.Background(), row, snapshot, transfer); err != nil || ready {
		t.Fatalf("global ready authorized exact replacement: ready=%v err=%v", ready, err)
	}
	run := models.MediaArtifactRun{ID: "exact-run", LibraryID: f.library.ID, Generation: 2, Status: models.MediaArtifactStatusCompleted, PolicyJSON: fmt.Sprintf(`{"library_id":%d,"generation":2,"metadata_artifacts_enabled":true}`, f.library.ID)}
	if err := f.service.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	artifact := models.MediaArtifact{OpaqueID: "new-artifact", RunID: run.ID, LibraryID: f.library.ID, SourceIdentity: fmt.Sprintf("entry:%d", newEntry.ID), Kind: models.MediaArtifactKindNFO, TargetKind: models.MediaArtifactTargetLocalAdjacent, RelativePath: "/Show.S01E02.new.nfo", Managed: true, Active: true, Status: models.MediaArtifactStatusCompleted}
	if err := f.service.db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	if _, ready, err := f.service.replacementReady(context.Background(), row, snapshot, transfer); err != nil || !ready {
		t.Fatalf("exact artifacts rejected: ready=%v err=%v", ready, err)
	}
	if err := f.service.db.Model(&item).Update("relative_path", f.old.RelativePath).Error; err != nil {
		t.Fatal(err)
	}
	if _, ready, err := f.service.replacementReady(context.Background(), row, snapshot, transfer); err == nil || ready {
		t.Fatalf("same old output accepted ready=%v", ready)
	}
}

func (f versionUpgradeFixture) createDownload(t *testing.T, id string) models.DownloadTask {
	t.Helper()
	download := models.DownloadTask{ID: id, OwnerID: f.actor.User.ID, Phase: models.DownloadTaskStatusCompleted, ProviderType: models.DownloaderTypeQBittorrent, ProfileID: f.library.ProfileID, ProfileRevision: f.library.ProfileRevision, SourceCiphertext: "unused", TargetLibraryID: &f.library.ID, TargetStorageID: &f.storage.ID, TargetStorageType: f.storage.Type, TargetStorageRoot: f.storage.RootPath, TargetRelativeRoot: "/", ConflictPolicy: models.MediaLibraryConflictRename, DisplayName: "Show"}
	if _, err := f.service.queue.EnqueueWith(EnqueueJobInput{OwnerID: f.actor.User.ID, JobType: "download", DisplayName: "Show", Payload: downloadJobPayload{DownloadTaskID: id}}, func(tx *gorm.DB, job models.Job) error { download.JobID = job.ID; return tx.Create(&download).Error }); err != nil {
		t.Fatal(err)
	}
	return download
}
func runMediaVersionUpgradeWorkerScenario(t *testing.T, failPublish, versioned bool) {
	f := newVersionUpgradeFixture(t)
	oldSubtitle := f.asset(t, "Show.S01E02.old.zh-Hans.ass")
	poster := f.asset(t, "poster.jpg")
	var store *CatalogSnapshotStore
	if versioned {
		store = deletionSnapshotStore(t, f.service.db, f.library, []models.MediaLibraryEntry{f.old}, []models.MediaLibrarySourceAsset{oldSubtitle, poster})
		f.service.libraries.SetCatalogSnapshotStore(store)
	}
	row, _ := f.prepare(t)
	download := f.createDownload(t, "worker-download")
	if _, err := f.service.queue.EnqueueWith(EnqueueJobInput{OwnerID: f.actor.User.ID, JobType: JobTypeMediaVersionUpgrade, DisplayName: "wash Show", ResourceKey: "library", Payload: map[string]string{"upgrade_id": row.ID}}, func(tx *gorm.DB, job models.Job) error {
		row.JobID = job.ID
		row.DownloadTaskID = download.ID
		row.State = "submitted"
		return tx.Save(&row).Error
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := f.service.queue.Claim([]string{JobTypeMediaVersionUpgrade})
	if err != nil || claim == nil {
		t.Fatalf("claim=%v err=%v", claim, err)
	}
	runtime := workerRuntime{queue: f.service.queue, job: *claim}
	worker := NewMediaVersionUpgradeWorker(f.service)
	waiting := worker.Run(context.Background(), runtime, *claim)
	if waiting.RetryAt == nil {
		t.Fatalf("did not wait for actual import %+v", waiting)
	}
	oldPath := filepath.Join(f.storage.RootPath, "Show.S01E02.old.mkv")
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("old removed before import: %v", err)
	}
	// Simulate the exact successfully published target receipts, not library totals.
	target := filepath.Join(f.storage.RootPath, "Show.S01E02.new.mkv")
	if err := os.WriteFile(target, []byte("new-video"), 0600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(target)
	entry := f.old
	entry.ID = 0
	entry.RelativePath = "/Show.S01E02.new.mkv"
	entry.ProviderID = "new-physical"
	entry.Size = info.Size()
	entry.ModifiedAt = info.ModTime().UTC()
	entry.LastGeneration = 2
	if !versioned {
		if err := f.service.db.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
	}
	if versioned {
		candidate, token := catalogCandidate(t, store, f.library, "delta", 1)
		ids, err := store.ResolveIdentities(context.Background(), candidate.ID, token, []CatalogIdentityRequest{{Kind: "entry", SourceKey: entry.RelativePath, ProviderID: entry.ProviderID}})
		if err != nil {
			t.Fatal(err)
		}
		entry.ID = ids[0]
		if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(entry, nil)}}); err != nil {
			t.Fatal(err)
		}
		catalogPublish(t, store, candidate, token)
	}
	now := time.Now().UTC()
	transfer := models.TransferTask{ID: "worker-transfer", OwnerID: f.actor.User.ID, DownloadTaskID: download.ID, LibraryID: f.library.ID, LibraryName: f.library.Name, Phase: models.TransferTaskStatusCompleted, TotalFiles: 1, PlanSummaryJSON: `{"completed_files":1,"skipped_files":0}`, CatalogPublishedAt: &now, FinishedAt: &now}
	if _, err := f.service.queue.EnqueueWith(EnqueueJobInput{OwnerID: f.actor.User.ID, JobType: "transfer", DisplayName: "replacement transfer", Payload: map[string]string{"transfer_task_id": transfer.ID}}, func(tx *gorm.DB, job models.Job) error { transfer.JobID = job.ID; return tx.Create(&transfer).Error }); err != nil {
		t.Fatal(err)
	}
	scan := models.MediaLibraryScanRun{LibraryID: f.library.ID, Kind: "transfer_batch", Status: "success", Generation: 2, CheckpointJSON: fmt.Sprintf(`{"transfer_batch_id":%q}`, transfer.ID), StartedAt: now, FinishedAt: &now}
	if err := f.service.db.Create(&scan).Error; err != nil {
		t.Fatal(err)
	}
	item := models.MediaManagedItem{OpaqueID: "worker-managed", LibraryID: f.library.ID, TransferTaskID: transfer.ID, DownloadTaskID: download.ID, RelativePath: entry.RelativePath, Size: entry.Size, Kind: "video", Managed: true, Active: true}
	if err := f.service.db.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	if failPublish {
		if err := f.service.db.Exec(`CREATE TRIGGER fail_upgrade_publish BEFORE UPDATE OF state ON media_version_upgrades WHEN NEW.state='completed' BEGIN SELECT RAISE(ABORT,'fixture publish interruption'); END`).Error; err != nil {
			t.Fatal(err)
		}
		interrupted := worker.Run(context.Background(), runtime, *claim)
		if interrupted.RetryAt == nil || interrupted.ErrorCode != CodeMediaUpgradePending {
			t.Fatalf("interruption not recoverable: %+v", interrupted)
		}
		if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fixture did not reach physical move: %v", err)
		}
		var count int64
		f.service.db.Model(&models.MediaLibraryEntry{}).Where("id=?", f.old.ID).Count(&count)
		if count != 1 {
			t.Fatal("failed publication lost old catalog rollback")
		}
		if err := f.service.db.Exec(`DROP TRIGGER fail_upgrade_publish`).Error; err != nil {
			t.Fatal(err)
		}
		worker = NewMediaVersionUpgradeWorker(NewMediaVersionUpgradeService(f.service.db, f.service.libraries, nil, f.service.queue))
	}
	completed := worker.Run(context.Background(), runtime, *claim)
	if completed.ErrorCode != "" || completed.RetryAt != nil {
		t.Fatalf("replacement did not complete %+v", completed)
	}
	if err := f.service.db.First(&row, "id=?", row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.State != "completed" {
		t.Fatalf("state=%+v", row)
	}
	for _, name := range []string{f.old.RelativePath, oldSubtitle.RelativePath} {
		if _, err := os.Stat(filepath.Join(f.storage.RootPath, name[1:])); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("selected old not recycled %q %v", name, err)
		}
	}
	for _, name := range []string{entry.RelativePath, poster.RelativePath} {
		if _, err := os.Stat(filepath.Join(f.storage.RootPath, name[1:])); err != nil {
			t.Fatalf("retained output removed %q %v", name, err)
		}
	}
	var count int64
	f.service.db.Model(&models.MediaLibraryEntry{}).Where("id=?", f.old.ID).Count(&count)
	if (!versioned && count != 0) || (versioned && count != 1) {
		t.Fatalf("old anchor retention does not match mode: %d", count)
	}
	if versioned {
		if err := f.service.libraries.withCatalogRead(context.Background(), []uint{f.library.ID}, func(_ *gorm.DB, reader *CatalogReader) error {
			var count int64
			if err := reader.Entries().Where("library_id=? AND id=?", f.library.ID, f.old.ID).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("old immutable anchor still visible")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	f.service.db.Model(&models.MediaLibraryEntry{}).Where("id=?", entry.ID).Count(&count)
	if count != 1 {
		t.Fatal("new catalog entry removed")
	}
	var trash []models.ManagedMediaTrash
	f.service.db.Where("upgrade_id=?", row.ID).Find(&trash)
	if len(trash) != 2 {
		t.Fatalf("reversible mappings=%d", len(trash))
	}
	// A late scheduler replay completes without a second physical operation.
	restarted := NewMediaVersionUpgradeService(f.service.db, f.service.libraries, nil, f.service.queue)
	if replay := NewMediaVersionUpgradeWorker(restarted).Run(context.Background(), runtime, *claim); replay.ErrorCode != "" || replay.RetryAt != nil {
		t.Fatalf("late replay=%+v", replay)
	}
}

type upgradeRecycleTestDriver struct {
	*fakeMutationCloudDriver
	calls            int
	fail             bool
	applyBeforeError bool
	noRecycle        bool
}

func (d *upgradeRecycleTestDriver) Capabilities() cloudpkg.Capabilities {
	caps := d.fakeMutationCloudDriver.Capabilities()
	caps.Recycle = !d.noRecycle
	return caps
}
func (d *upgradeRecycleTestDriver) Recycle(ctx context.Context, id string) error {
	d.calls++
	if d.fail {
		if d.applyBeforeError {
			delete(d.items, id)
		}
		return cloudpkg.Error(cloudpkg.CodeUnavailable, true, errors.New("lost upstream reply"))
	}
	return d.fakeMutationCloudDriver.Recycle(ctx, id)
}
func TestMediaVersionUpgradeCloudUnknownRecycleNeverBlindlyReplays(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(fmt.Sprint(applied), func(t *testing.T) {
			fixture := newPan115CatalogDeletionFixture(t, 1)
			var storage models.Storage
			if err := fixture.service.db.First(&storage, fixture.library.StorageID).Error; err != nil {
				t.Fatal(err)
			}
			driver := &upgradeRecycleTestDriver{fakeMutationCloudDriver: fixture.driver, fail: true, applyBeforeError: applied}
			fixture.service.connections.drivers[*storage.ConnectionID] = driver
			service := NewMediaVersionUpgradeService(fixture.service.db, fixture.service, nil, fixture.queue)
			row := models.MediaVersionUpgrade{ID: "cloud-upgrade", OwnerID: fixture.actor.User.ID, LibraryID: fixture.library.ID, EntryID: fixture.entries[0].ID, Revision: 1, State: "replacement_ready", Stage: "recycle_old", SnapshotJSON: "{}", RecycleJSON: `{"version":1,"items":{}}`, OutputJSON: "{}", ExpiresAt: time.Now().Add(time.Hour)}
			if err := service.db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			actual := driver.items[fixture.entries[0].ProviderID]
			item := mediaUpgradeRecycleItem{Key: "entry:1", ProviderID: actual.ID, ParentID: actual.ParentID, Name: actual.Name, Size: actual.Size, SHA1: actual.SHA1, ModifiedAt: actual.ModifiedAt}
			state := mediaUpgradeRecycleState{Version: 1, Items: map[string]string{}}
			if err := service.recycleCloudItem(context.Background(), row, item, storage, &state); ErrorCode(err) != CodeMediaUpgradePending {
				t.Fatalf("first result=%v", err)
			}
			if state.Items[item.Key] != "dispatched" || driver.calls != 1 {
				t.Fatalf("first state=%+v calls=%d", state, driver.calls)
			}
			// Restart from the persisted receipt, then reconcile only the original ID.
			var restored models.MediaVersionUpgrade
			if err := service.db.First(&restored, "id=?", row.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(restored.RecycleJSON), &state); err != nil {
				t.Fatal(err)
			}
			err := service.recycleCloudItem(context.Background(), restored, item, storage, &state)
			if applied && err != nil {
				t.Fatalf("applied unknown not reconciled %v", err)
			}
			if !applied && ErrorCode(err) != CodeMediaUpgradePending {
				t.Fatalf("unapplied unknown replayed %v", err)
			}
			if driver.calls != 1 {
				t.Fatalf("unknown operation dispatched twice: %d", driver.calls)
			}
		})
	}
}
func TestMediaVersionUpgradeRejectsUnsupportedNativeRecycleBeforeSubmission(t *testing.T) {
	fixture := newPan115CatalogDeletionFixture(t, 1)
	var storage models.Storage
	if err := fixture.service.db.First(&storage, fixture.library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	driver := &upgradeRecycleTestDriver{fakeMutationCloudDriver: fixture.driver, noRecycle: true}
	fixture.service.connections.drivers[*storage.ConnectionID] = driver
	service := NewMediaVersionUpgradeService(fixture.service.db, fixture.service, nil, fixture.queue)
	if _, _, err := service.recycleDriver(storage); ErrorCode(err) != CodeMediaUpgradeUnsupported {
		t.Fatalf("unsupported accepted %v", err)
	}
	if driver.calls != 0 {
		t.Fatal("unsupported driver mutated")
	}
}
func TestMediaVersionUpgradePrivateReceiptCannotUseOrdinaryDelete(t *testing.T) {
	f := newVersionUpgradeFixture(t)
	token := strings.Repeat("x", 43)
	sum := sha256.Sum256([]byte(token))
	now := time.Now().UTC()
	receipt := models.MediaCatalogDeletionPreview{ID: "upgrade-private", TokenHash: hex.EncodeToString(sum[:]), ActorID: f.actor.User.ID, LibraryID: f.library.ID, WorkKey: f.old.WorkKey, EntryDigest: catalogDeletionDigest([]models.MediaLibraryEntry{f.old}), StorageType: f.storage.Type, SnapshotJSON: `{"version":4,"purpose":"media_version_upgrade","upgrade_id":"private"}`, StateJSON: `{"version":1,"completed":{}}`, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	if err := f.service.db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.libraries.ConfirmCatalogDeletion(context.Background(), f.actor, f.library.ID, encodeCatalogToken(f.old.WorkKey), token, RequestContext{}); ErrorCode(err) != CodeMediaCatalogDeletionChanged {
		t.Fatalf("upgrade receipt executed ordinary permanent-delete path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.storage.RootPath, "Show.S01E02.old.mkv")); err != nil {
		t.Fatalf("ordinary route touched old media: %v", err)
	}
	var count int64
	f.service.db.Model(&models.CatalogPhysicalWrite{}).Where("owner_id=?", receipt.ID).Count(&count)
	if count != 0 {
		t.Fatal("ordinary route admitted upgrade receipt")
	}
}

func TestMediaVersionUpgradeRejectsUnfulfillableCloudMetadataBeforeAcquisition(t *testing.T) {
	for _, storageType := range []string{models.StorageTypePan115, models.StorageTypeQuark} {
		storage := models.Storage{Type: storageType}
		library := models.MediaLibrary{MetadataArtifactsEnabled: true, STRMEnabled: false, SignedProxyEnabled: false}
		if err := validateUpgradeArtifactConfiguration(library, storage); ErrorCode(err) != CodeMediaUpgradeUnsupported {
			t.Fatalf("metadata obligation bypassed for %s: %v", storageType, err)
		}
		library.STRMEnabled, library.SignedProxyEnabled = true, true
		if err := validateUpgradeArtifactConfiguration(library, storage); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateUpgradeArtifactConfiguration(models.MediaLibrary{MetadataArtifactsEnabled: true}, models.Storage{Type: models.StorageTypeLocal}); err != nil {
		t.Fatal(err)
	}
}

func TestMediaVersionUpgradeDigestEquivalenceAllowsRealSubtitleChange(t *testing.T) {
	video := strings.Repeat("a", 40)
	subtitle := strings.Repeat("b", 40)
	newSubtitle := strings.Repeat("c", 40)
	if !upgradeContentProvablyUnchanged(video, video, nil, nil) {
		t.Fatal("identical bytes without subtitle change accepted")
	}
	if !upgradeContentProvablyUnchanged(video, video, []string{subtitle}, []string{subtitle}) {
		t.Fatal("identical video and subtitle bytes accepted")
	}
	if upgradeContentProvablyUnchanged(video, video, nil, []string{newSubtitle}) {
		t.Fatal("added subtitle rejected solely due equal video bytes")
	}
	if upgradeContentProvablyUnchanged(video, video, []string{subtitle}, []string{newSubtitle}) {
		t.Fatal("replacement subtitle rejected solely due equal video bytes")
	}
	if upgradeContentProvablyUnchanged("", "", nil, nil) || upgradeContentProvablyUnchanged("1080p", "1080p", nil, nil) {
		t.Fatal("coarse tags treated as byte identity")
	}
}

func TestMediaVersionUpgradeProtectsSeedPathButNotSeparateLibraryLink(t *testing.T) {
	f := newVersionUpgradeFixture(t)
	download := f.createDownload(t, "old-seeding-download")
	manifest := `{"Name":"Show","Complete":true,"Files":[{"relative_path":"Show.S01E02.old.mkv","size":9}]}`
	if err := f.service.db.Model(&download).Updates(map[string]any{"staging_absolute_path": f.storage.RootPath, "staging_category": ".", "completed_manifest_json": manifest}).Error; err != nil {
		t.Fatal(err)
	}
	seed := models.SeedingTask{ID: "old-seed", OwnerID: f.actor.User.ID, DownloadTaskID: download.ID, ProviderType: models.DownloaderTypeQBittorrent, Phase: models.SeedingTaskStatusRetained}
	if _, err := f.service.queue.EnqueueWith(EnqueueJobInput{OwnerID: f.actor.User.ID, JobType: "seeding", DisplayName: "old seed", Payload: map[string]string{"seeding_task_id": seed.ID}}, func(tx *gorm.DB, job models.Job) error { seed.JobID = job.ID; return tx.Create(&seed).Error }); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(f.storage.RootPath, "Show.S01E02.old.mkv")
	if err := f.service.protectUpgradeSeedingSource(context.Background(), original); ErrorCode(err) != CodeMediaUpgradeSeedingBusy {
		t.Fatalf("seed path not protected: %v", err)
	}
	libraryLink := filepath.Join(f.storage.RootPath, "Show.S01E02.library.mkv")
	if err := os.Link(original, libraryLink); err != nil {
		t.Skipf("hardlink unavailable: %v", err)
	}
	if err := f.service.protectUpgradeSeedingSource(context.Background(), libraryLink); err != nil {
		t.Fatalf("separate library link blocked: %v", err)
	}
}

func TestMediaVersionUpgradeWorkerWaitsThenRecyclesOnlySelectedOldVersion(t *testing.T) {
	runMediaVersionUpgradeWorkerScenario(t, false, false)
}
func TestMediaVersionUpgradeWorkerRestartsAfterPhysicalRecycleBeforeCatalogCommit(t *testing.T) {
	runMediaVersionUpgradeWorkerScenario(t, true, false)
}
func TestMediaVersionUpgradeManagedTrashRecoversRenameBeforeReceiptSave(t *testing.T) {
	f := newVersionUpgradeFixture(t)
	row, snapshot := f.prepare(t)
	if err := f.service.db.Exec(`CREATE TRIGGER fail_trash_save BEFORE UPDATE OF state ON managed_media_trashes WHEN NEW.state='recycled' BEGIN SELECT RAISE(ABORT,'fixture receipt interruption'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.service.recycleLocalItem(context.Background(), row, snapshot.Items[0], f.library, f.storage); err == nil {
		t.Fatal("fixture did not interrupt after rename")
	}
	if _, err := os.Stat(filepath.Join(f.storage.RootPath, "Show.S01E02.old.mkv")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rename not reached: %v", err)
	}
	var receipt models.ManagedMediaTrash
	if err := f.service.db.Where("upgrade_id=?", row.ID).First(&receipt).Error; err != nil || receipt.State != "prepared" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if err := f.service.db.Exec(`DROP TRIGGER fail_trash_save`).Error; err != nil {
		t.Fatal(err)
	}
	restarted := NewMediaVersionUpgradeService(f.service.db, f.service.libraries, nil, f.service.queue)
	if err := restarted.recycleLocalItem(context.Background(), row, snapshot.Items[0], f.library, f.storage); err != nil {
		t.Fatal(err)
	}
	if err := f.service.db.First(&receipt, "id=?", receipt.ID).Error; err != nil || receipt.State != "recycled" {
		t.Fatalf("recovered receipt=%+v err=%v", receipt, err)
	}
}

func TestMediaVersionUpgradeUsesSharedActualSelectionForOneExactEpisode(t *testing.T) {
	f := newVersionUpgradeFixture(t)
	service := &SiteService{db: f.service.db, now: func() time.Time { return time.Now().UTC() }}
	raw := inspectionTorrent("Fixture.Show.S01E02.mkv", "Fixture.Show.S01E15.mkv")
	manifest, err := resourcecontent.ParseTorrent(raw, resourcecontent.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	season, episode := 1, 2
	expected := &DownloadRecognitionIdentity{TMDBID: 42, MediaType: "tv", Season: &season, Episode: &episode}
	claim := siteResultClaim{SearchIdentity: &siteShareIdentity{TMDBID: 42, MediaType: "tv", Title: "Fixture Show", Season: &season}}
	selected := resourcecontent.Selection{Version: 1, Identity: manifest.Identity, Files: []resourcecontent.SelectedFile{{Index: manifest.Files[0].Index, Path: manifest.Files[0].Path, Size: manifest.Files[0].Size}}}
	source := DownloadSourceInput{Kind: "torrent", Torrent: raw, ContentSelection: &selected}
	if err := service.validateUpgradeSource(context.Background(), f.actor, claim, source, expected); err != nil {
		t.Fatalf("exact selected episode rejected: %v", err)
	}
	selected.Files = []resourcecontent.SelectedFile{{Index: manifest.Files[1].Index, Path: manifest.Files[1].Path, Size: manifest.Files[1].Size}}
	if err := service.validateUpgradeSource(context.Background(), f.actor, claim, source, expected); err == nil {
		t.Fatal("wrong episode accepted")
	}
	source.ContentSelection = nil
	if err := service.validateUpgradeSource(context.Background(), f.actor, claim, source, expected); err == nil {
		t.Fatal("whole pack accepted for one-episode replacement")
	}
	source = DownloadSourceInput{Kind: "url", URL: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}
	if err := service.validateUpgradeSource(context.Background(), f.actor, claim, source, expected); err == nil {
		t.Fatal("magnet title substituted for actual content")
	}
}

func TestMediaVersionUpgradeVersionedWorkerPreservesAnchorsAndPublishesExactRemoval(t *testing.T) {
	runMediaVersionUpgradeWorkerScenario(t, false, true)
}
func TestMediaVersionUpgradeVersionedWorkerRecoversAfterPhysicalRecycle(t *testing.T) {
	runMediaVersionUpgradeWorkerScenario(t, true, true)
}

func TestMediaVersionUpgradeInternalKeepBothDoesNotOverwriteExistingLocalVersion(t *testing.T) {
	queue, _, download, _, destination := transferFixture(t, models.MediaLibraryTransferCopy, models.MediaLibraryConflictOverwrite, true)
	download.ConflictPolicy = models.MediaLibraryConflictRename
	if err := queue.db.Model(&models.DownloadTask{}).Where("id=?", download.ID).Update("conflict_policy", download.ConflictPolicy).Error; err != nil {
		t.Fatal(err)
	}
	service := NewTransferService(queue.db, queue.audit, queue, zerolog.Nop())
	manifest := downloadpkg.Manifest{Name: "Movie.2024", Complete: true, Files: []downloadpkg.File{{RelativePath: "Movie.2024.mkv", Size: minimumAutomaticTransferVideoBytes}}}
	if err := service.Enqueue(download, manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := queue.Claim([]string{"transfer"})
	if err != nil || claim == nil {
		t.Fatalf("claim=%v err=%v", claim, err)
	}
	result := NewTransferWorker(service).Run(context.Background(), workerRuntime{queue: queue, job: *claim}, *claim)
	if result.ErrorCode != "" || result.Wait != nil || result.RetryAt != nil {
		t.Fatalf("keep-both failed %+v", result)
	}
	original, err := os.ReadFile(destination)
	if err != nil || string(original) != "existing" {
		t.Fatalf("old overwritten %q %v", original, err)
	}
	var items []models.MediaManagedItem
	if err := queue.db.Where("download_task_id=? AND active=?", download.ID, true).Find(&items).Error; err != nil || len(items) != 1 {
		t.Fatalf("outputs=%+v %v", items, err)
	}
	actual := filepath.Join(download.TargetStorageRoot, strings.TrimPrefix(items[0].RelativePath, "/"))
	if actual == destination {
		t.Fatal("replacement overwrote original target")
	}
	replacement, err := os.ReadFile(actual)
	if err != nil || string(replacement) != "video-data" {
		t.Fatalf("new output=%q %v", replacement, err)
	}
	var library models.MediaLibrary
	if err := queue.db.First(&library, *download.TargetLibraryID).Error; err != nil || library.ConflictPolicy != models.MediaLibraryConflictOverwrite {
		t.Fatal("library-wide conflict policy was changed")
	}
}
func TestMediaVersionUpgradeInternalKeepBothDoesNotRecycleCloudConflict(t *testing.T) {
	fixture := newCloudTransferFixture(t, models.MediaLibraryTransferCopy, models.MediaLibraryConflictOverwrite, true)
	fixture.download.ConflictPolicy = models.MediaLibraryConflictRename
	if err := fixture.queue.db.Model(&models.DownloadTask{}).Where("id=?", fixture.download.ID).Update("conflict_policy", fixture.download.ConflictPolicy).Error; err != nil {
		t.Fatal(err)
	}
	result := fixture.run(t)
	if result.ErrorCode != "" || result.Wait != nil || result.RetryAt != nil {
		t.Fatalf("keep-both failed %+v", result)
	}
	for _, id := range fixture.driver.recycled {
		if id == "existing-video" {
			t.Fatalf("old conflict recycled before new ready: %v", fixture.driver.recycled)
		}
	}
	if _, exists := fixture.driver.items["existing-video"]; !exists {
		t.Fatal("old cloud version removed")
	}
}
