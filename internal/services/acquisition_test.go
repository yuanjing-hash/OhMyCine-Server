package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func acquisitionIdentitySnapshot(t *testing.T, revision uint64, source, status, mediaType string, tmdbID int64, locked bool) string {
	t.Helper()
	raw, err := json.Marshal(MediaIdentitySnapshot{Version: 1, Revision: revision, Source: source, Status: status, Locked: locked, TMDBID: &tmdbID, MediaType: mediaType, Title: "Test"})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestAcquisitionRecordDownloadUsesTrustedFrozenIdentityBeforeScrape(t *testing.T) {
	queue, actor, clock := queueFixture(t)
	actor.Permissions[authz.PermissionDiscoveryRead] = struct{}{}
	tmdbID := int64(65733)
	progress := 21.5
	job := enqueueFake(t, queue, actor, "Doraemon", "acquisition-direct-create")
	task := models.DownloadTask{
		ID:                   uuid.NewString(),
		OwnerID:              actor.User.ID,
		JobID:                job.ID,
		DownloaderName:       "Test",
		ProviderType:         models.DownloaderTypeFake,
		SourceCiphertext:     "encrypted",
		DisplayName:          "哆啦A梦",
		Phase:                models.DownloadTaskStatusDownloading,
		Progress:             &progress,
		IdentitySource:       mediaIdentitySourceDirectID,
		IdentityStatus:       mediaIdentityStatusVerified,
		IdentityRevision:     1,
		IdentitySnapshotJSON: acquisitionIdentitySnapshot(t, 1, mediaIdentitySourceDirectID, mediaIdentityStatusVerified, "tv", tmdbID, false),
		CreatedAt:            clock.Now(),
		UpdatedAt:            clock.Now(),
	}
	if err := queue.db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	service := NewAcquisitionService(queue.db)
	if err := service.RecordDownload(actor.User.ID, downloadTaskSummary(task, "running")); err != nil {
		t.Fatal(err)
	}
	status, err := service.Get(actor, "tv", tmdbID)
	if err != nil {
		t.Fatal(err)
	}
	if status.DownloadTaskID != task.ID || status.Stage != "download" || status.Status != models.DownloadTaskStatusDownloading || status.Progress == nil || *status.Progress != progress {
		t.Fatalf("status=%+v", status)
	}
	var row models.MediaAcquisition
	if err := queue.db.Where("owner_id = ? AND media_type = ? AND tmdb_id = ?", actor.User.ID, "tv", tmdbID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	revision := row.Revision
	if err := service.RecordDownload(actor.User.ID, downloadTaskSummary(task, "running")); err != nil {
		t.Fatal(err)
	}
	if err := queue.db.First(&row, "id = ?", row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Revision != revision {
		t.Fatalf("repeated record changed revision from %d to %d", revision, row.Revision)
	}
}

func TestAcquisitionRecordDownloadRejectsInvalidFrozenIdentity(t *testing.T) {
	tests := []struct {
		name       string
		ownerDelta uint
		revision   uint64
		source     string
		status     string
		mediaType  string
		snapshot   func(*testing.T, int64) string
	}{
		{name: "owner mismatch", ownerDelta: 1, revision: 1, source: mediaIdentitySourceDirectID, status: mediaIdentityStatusVerified, mediaType: "tv"},
		{name: "revision mismatch", revision: 2, source: mediaIdentitySourceDirectID, status: mediaIdentityStatusVerified, mediaType: "tv", snapshot: func(t *testing.T, id int64) string {
			return acquisitionIdentitySnapshot(t, 1, mediaIdentitySourceDirectID, mediaIdentityStatusVerified, "tv", id, false)
		}},
		{name: "state mismatch", revision: 1, source: mediaIdentitySourceDirectID, status: mediaIdentityStatusProvisional, mediaType: "tv", snapshot: func(t *testing.T, id int64) string {
			return acquisitionIdentitySnapshot(t, 1, mediaIdentitySourceDirectID, mediaIdentityStatusVerified, "tv", id, false)
		}},
		{name: "invalid media type", revision: 1, source: mediaIdentitySourceDirectID, status: mediaIdentityStatusVerified, mediaType: "episode"},
		{name: "malformed snapshot", revision: 1, source: mediaIdentitySourceDirectID, status: mediaIdentityStatusVerified, mediaType: "tv", snapshot: func(*testing.T, int64) string { return `{` }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			queue, actor, clock := queueFixture(t)
			job := enqueueFake(t, queue, actor, "Invalid identity", "acquisition-invalid-"+test.name)
			tmdbID := int64(65733)
			raw := ""
			if test.snapshot != nil {
				raw = test.snapshot(t, tmdbID)
			} else {
				raw = acquisitionIdentitySnapshot(t, test.revision, test.source, test.status, test.mediaType, tmdbID, false)
			}
			task := models.DownloadTask{ID: uuid.NewString(), OwnerID: actor.User.ID, JobID: job.ID, DownloaderName: "Test", ProviderType: models.DownloaderTypeFake, SourceCiphertext: "encrypted", DisplayName: "Invalid", Phase: models.DownloadTaskStatusQueued, IdentitySource: test.source, IdentityStatus: test.status, IdentityRevision: test.revision, IdentitySnapshotJSON: raw, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}
			if err := queue.db.Create(&task).Error; err != nil {
				t.Fatal(err)
			}
			if err := NewAcquisitionService(queue.db).RecordDownload(actor.User.ID+test.ownerDelta, downloadTaskSummary(task, "queued")); err != nil {
				t.Fatal(err)
			}
			var count int64
			if err := queue.db.Model(&models.MediaAcquisition{}).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("invalid identity created %d acquisitions", count)
			}
		})
	}
}

func TestAcquisitionListReconcilesLatestDirectIdentityOwnerScopedAndIdempotent(t *testing.T) {
	queue, actor, clock := queueFixture(t)
	actor.Permissions[authz.PermissionDiscoveryRead] = struct{}{}
	tmdbID := int64(65733)
	created := clock.Now().Add(-time.Hour)
	makeTask := func(name string, at time.Time, progress float64) models.DownloadTask {
		job := enqueueFake(t, queue, actor, name, "acquisition-history-"+uuid.NewString())
		task := models.DownloadTask{ID: uuid.NewString(), OwnerID: actor.User.ID, JobID: job.ID, DownloaderName: "Test", ProviderType: models.DownloaderTypeFake, SourceCiphertext: "encrypted", DisplayName: name, Phase: models.DownloadTaskStatusDownloading, Progress: &progress, IdentitySource: mediaIdentitySourceDirectID, IdentityStatus: mediaIdentityStatusVerified, IdentityRevision: 1, IdentitySnapshotJSON: acquisitionIdentitySnapshot(t, 1, mediaIdentitySourceDirectID, mediaIdentityStatusVerified, "tv", tmdbID, false), CreatedAt: at, UpdatedAt: at}
		if err := queue.db.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
		return task
	}
	_ = makeTask("旧任务", created, 100)
	latest := makeTask("当前任务", created.Add(30*time.Minute), 42)

	foreignUser := models.User{Username: "acquisition-reconcile-foreign", UsernameNormalized: "acquisition-reconcile-foreign", DisplayName: "Foreign", PasswordHash: "unused", Status: models.UserStatusActive, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}
	if err := queue.db.Create(&foreignUser).Error; err != nil {
		t.Fatal(err)
	}
	foreignJob := enqueueFake(t, queue, actor, "Foreign", "acquisition-history-foreign")
	foreignID := int64(999)
	foreign := models.DownloadTask{ID: uuid.NewString(), OwnerID: foreignUser.ID, JobID: foreignJob.ID, DownloaderName: "Test", ProviderType: models.DownloaderTypeFake, SourceCiphertext: "encrypted", DisplayName: "Foreign", Phase: models.DownloadTaskStatusDownloading, IdentitySource: mediaIdentitySourceDirectID, IdentityStatus: mediaIdentityStatusVerified, IdentityRevision: 1, IdentitySnapshotJSON: acquisitionIdentitySnapshot(t, 1, mediaIdentitySourceDirectID, mediaIdentityStatusVerified, "movie", foreignID, false), CreatedAt: clock.Now(), UpdatedAt: clock.Now()}
	if err := queue.db.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}

	automaticID := int64(1234)
	automaticJob := enqueueFake(t, queue, actor, "Automatic", "acquisition-history-automatic")
	automatic := models.DownloadTask{ID: uuid.NewString(), OwnerID: actor.User.ID, JobID: automaticJob.ID, DownloaderName: "Test", ProviderType: models.DownloaderTypeFake, SourceCiphertext: "encrypted", DisplayName: "Automatic", Phase: models.DownloadTaskStatusDownloading, IdentitySource: mediaIdentitySourceAutomatic, IdentityStatus: mediaIdentityStatusVerified, IdentityRevision: 1, IdentitySnapshotJSON: acquisitionIdentitySnapshot(t, 1, mediaIdentitySourceAutomatic, mediaIdentityStatusVerified, "movie", automaticID, false), CreatedAt: clock.Now(), UpdatedAt: clock.Now()}
	if err := queue.db.Create(&automatic).Error; err != nil {
		t.Fatal(err)
	}

	service := NewAcquisitionService(queue.db)
	page, err := service.List(actor, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.List) != 1 || page.List[0].DownloadTaskID != latest.ID || page.List[0].Title != latest.DisplayName || page.List[0].Progress == nil || *page.List[0].Progress != 42 {
		t.Fatalf("page=%+v", page)
	}
	var row models.MediaAcquisition
	if err := queue.db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	revision := row.Revision
	page, err = service.List(actor, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.List[0].DownloadTaskID != latest.ID {
		t.Fatalf("repeated page=%+v", page)
	}
	if err := queue.db.First(&row, "id = ?", row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if row.Revision != revision {
		t.Fatalf("reconciliation changed revision from %d to %d", revision, row.Revision)
	}
}

func TestAcquisitionReprojectsTransferProgressAndHidesDeniedLibrary(t *testing.T) {
	queue, actor, clock := queueFixture(t)
	actor.Permissions[authz.PermissionMediaLibrariesRead] = struct{}{}
	actor.Permissions[authz.PermissionDiscoveryRead] = struct{}{}
	now := clock.Now()
	storage := models.Storage{Name: "Acquisition storage", NameNormalized: "acquisition-storage", Type: models.StorageTypeLocal, RootPath: t.TempDir(), RootPathNormalized: uuid.NewString(), Enabled: true, Capabilities: `{}`}
	if err := queue.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	profile := models.MediaClassificationProfile{Name: "Acquisition profile", NameNormalized: "acquisition-profile", Kind: models.MediaClassificationProfileKindCustom, SchemaVersion: 1, RulesJSON: `{"version":1,"groups":[]}`, BuiltinRecognitionPacksJSON: `[]`, RecognitionRulesJSON: `[]`, Revision: 1}
	if err := queue.db.Create(&profile).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Acquisition library", NameNormalized: "acquisition-library", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: 1, RelativeRoot: "/", Enabled: true, VideoExtensionsJSON: `[]`, STRMAssetExtraExtensionsJSON: `[]`, IgnorePatternsJSON: `[]`, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	downloadJob := enqueueFake(t, queue, actor, "Acquisition download", "acquisition-download")
	tmdbID := int64(100)
	progress := 73.5
	bytesCompleted, bytesTotal, downloadSpeed, etaSeconds := int64(735), int64(1000), int64(100), int64(3)
	download := models.DownloadTask{ID: uuid.NewString(), OwnerID: actor.User.ID, JobID: downloadJob.ID, DownloaderName: "Test", ProviderType: models.DownloaderTypeFake, SourceCiphertext: "encrypted", DisplayName: "Test", Phase: models.DownloadTaskStatusCompleted, Progress: &progress, BytesCompleted: &bytesCompleted, BytesTotal: &bytesTotal, DownloadSpeed: &downloadSpeed, ETASeconds: &etaSeconds, TargetLibraryID: &library.ID, TargetStorageID: &storage.ID, TargetStorageType: storage.Type, TargetStorageRoot: storage.RootPath, TargetRelativeRoot: library.RelativeRoot, ScrapeTMDBID: &tmdbID, ScrapeMediaType: "tv", CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&download).Error; err != nil {
		t.Fatal(err)
	}
	transferJob := enqueueFake(t, queue, actor, "Acquisition transfer", "acquisition-transfer")
	finished := now.Add(time.Minute)
	transfer := models.TransferTask{ID: uuid.NewString(), OwnerID: actor.User.ID, JobID: transferJob.ID, DownloadTaskID: download.ID, LibraryID: library.ID, LibraryName: library.Name, ManifestJSON: `[]`, Phase: models.TransferTaskStatusCompleted, ProcessedFiles: 4, TotalFiles: 4, FinishedAt: &finished, CreatedAt: now, UpdatedAt: finished}
	if err := queue.db.Create(&transfer).Error; err != nil {
		t.Fatal(err)
	}
	acquisition := models.MediaAcquisition{ID: uuid.NewString(), OwnerID: actor.User.ID, MediaType: "tv", TMDBID: tmdbID, Stage: "download", Status: "queued", DownloadTaskID: download.ID, TargetLibraryID: &library.ID, FrozenSnapshotJSON: `{}`, Revision: 3, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&acquisition).Error; err != nil {
		t.Fatal(err)
	}
	service := NewAcquisitionService(queue.db)
	status, err := service.Get(actor, "tv", tmdbID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Stage != "import" || status.Status != "publishing" || status.CatalogTarget != nil {
		t.Fatalf("physical completion was treated as published: %+v", status)
	}
	// Publication must name the same transfer's current managed video targets.
	// An old queued aggregate and stale download percentage do not override it.
	for index := 0; index < 4; index++ {
		relative := fmt.Sprintf("Show/S01E%02d.mkv", index+1)
		entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: relative, ProviderID: fmt.Sprintf("video-%d", index), Size: 100, MediaType: "tv", Title: "Published show", WorkKey: "series:tmdb:100", TMDBID: &tmdbID, MatchStatus: mediaRecognitionStatusMatched}
		if err := queue.db.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
		managed := models.MediaManagedItem{OpaqueID: uuid.NewString(), LibraryID: library.ID, TransferTaskID: transfer.ID, DownloadTaskID: download.ID, Kind: models.MediaManagedItemKindVideo, RelativePath: relative, ProviderItemID: entry.ProviderID, Size: 100, Active: true, Managed: true}
		if err := queue.db.Create(&managed).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := queue.db.Model(&transfer).Update("catalog_published_at", finished).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&transfer).Update("plan_summary_json", `{"completed_files":4,"skipped_files":0}`).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&library).Update("exclusion_epoch", 0).Error; err != nil {
		t.Fatal(err)
	}
	library.ExclusionEpoch = 0
	publication := persistAcquisitionPublication(t, queue.db, library.ID, transfer)
	status, err = service.Get(actor, "tv", tmdbID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Stage != "library" || status.Status != models.TransferTaskStatusCompleted || status.TransferTaskID != transfer.ID || status.Progress == nil || *status.Progress != progress || status.BytesCompleted == nil || *status.BytesCompleted != bytesCompleted || status.DownloadSpeed == nil || *status.DownloadSpeed != downloadSpeed || status.ProcessedFiles != 4 || status.TotalFiles != 4 || status.UpdatedAt == nil || !status.UpdatedAt.Equal(finished) || status.TargetLibraryID == nil {
		t.Fatalf("status=%+v", status)
	}
	if status.CatalogTarget == nil || status.CatalogTarget.LibraryID != library.ID || status.CatalogTarget.WorkToken != encodeCatalogToken("series:tmdb:100") {
		t.Fatalf("missing authoritative navigation: %+v", status)
	}

	// Excluding and restoring a different work is not a source-lifetime change.
	otherID := int64(101)
	other := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "Other.mkv", ProviderID: "other", Size: 1, MediaType: "movie", Title: "Other", WorkKey: "movie:tmdb:101", TMDBID: &otherID, MatchStatus: mediaRecognitionStatusMatched}
	if err := queue.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	actor.Permissions[authz.PermissionMediaLibrariesScan] = struct{}{}
	facade := &MediaLibraryService{db: queue.db}
	exclusion, err := facade.ExcludeCatalogWork(context.Background(), actor, library.ID, encodeCatalogToken(other.WorkKey), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if value, err := service.Get(actor, "tv", tmdbID); err != nil || value.Stage != "library" {
		t.Fatalf("unrelated exclusion invalidated original publication: %+v %v", value, err)
	}
	if err := facade.RestoreCatalogExclusion(context.Background(), actor, library.ID, exclusion.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if value, err := service.Get(actor, "tv", tmdbID); err != nil || value.Stage != "library" {
		t.Fatalf("unrelated restore invalidated original publication: %+v %v", value, err)
	}
	// A first-generation zero is explicit proof, never the default for a missing field.
	originalCheckpoint := publication.CheckpointJSON
	var checkpoint map[string]json.RawMessage
	if err := json.Unmarshal([]byte(publication.CheckpointJSON), &checkpoint); err != nil {
		t.Fatal(err)
	}
	var originalScope map[string]json.RawMessage
	if err := json.Unmarshal(checkpoint["acquisition_publication_scope"], &originalScope); err != nil {
		t.Fatal(err)
	}
	if string(originalScope["root_generation"]) != "0" || string(originalScope["source_epoch"]) != "0" {
		t.Fatalf("zero generation not explicit: %s", publication.CheckpointJSON)
	}
	unverified := func(label string) {
		t.Helper()
		value, err := service.Get(actor, "tv", tmdbID)
		if err != nil || value.Stage != "import" || value.Status != "publishing" || value.CatalogTarget == nil || value.LastErrorCode != "acquisition_publication_unverified" {
			t.Fatalf("%s falsely completed: %+v %v", label, value, err)
		}
	}
	for _, field := range []string{"version", "library_id", "source_epoch", "root_generation", "source_fingerprint"} {
		values := map[string]json.RawMessage{}
		for k, v := range originalScope {
			values[k] = v
		}
		delete(values, field)
		raw, _ := json.Marshal(values)
		checkpoint["acquisition_publication_scope"] = raw
		encoded, _ := json.Marshal(checkpoint)
		if err := queue.db.Model(&publication).Update("checkpoint_json", string(encoded)).Error; err != nil {
			t.Fatal(err)
		}
		unverified("missing " + field)
	}
	for _, rawScope := range []string{`{"version":2,"library_id":1,"source_epoch":0,"root_generation":0,"source_fingerprint":"old"}`, `{"version":1,"library_id":1,"source_epoch":-1,"root_generation":0,"source_fingerprint":"old"}`, `{"version":1,"library_id":1,"source_epoch":18446744073709551616,"root_generation":0,"source_fingerprint":"old"}`, `{"version":1,"library_id":1,"source_epoch":"0","root_generation":0,"source_fingerprint":"old"}`} {
		checkpoint["acquisition_publication_scope"] = json.RawMessage(rawScope)
		encoded, _ := json.Marshal(checkpoint)
		if err := queue.db.Model(&publication).Update("checkpoint_json", string(encoded)).Error; err != nil {
			t.Fatal(err)
		}
		unverified("invalid scope")
	}
	if err := queue.db.Model(&publication).Update("checkpoint_json", originalCheckpoint).Error; err != nil {
		t.Fatal(err)
	}
	// Root away/back increments the real source lifetime; matching names and sizes cannot undo it.
	for _, root := range []string{"/other-root", library.RelativeRoot} {
		if err := queue.db.Transaction(func(tx *gorm.DB) error {
			var current models.MediaLibrary
			if err := tx.First(&current, library.ID).Error; err != nil {
				return err
			}
			replacement := current
			replacement.RelativeRoot = root
			if _, err := applyCatalogLibraryChangeTx(tx, current, &replacement, storage, storage, profile); err != nil {
				return err
			}
			return tx.Model(&current).Updates(map[string]any{"relative_root": replacement.RelativeRoot, "exclusion_epoch": replacement.ExclusionEpoch}).Error
		}); err != nil {
			t.Fatal(err)
		}
	}
	unverified("root ABA")
	if err := queue.db.Model(&library).Update("exclusion_epoch", 0).Error; err != nil {
		t.Fatal(err)
	}
	// A different physical credential/source lifetime cannot be blessed by a newer catalog stamp.
	if err := queue.db.Model(&models.CatalogPhysicalWrite{}).Where("owner_id=?", transfer.ID).Update("source_fingerprint", "stale-source").Error; err != nil {
		t.Fatal(err)
	}
	unverified("physical source")
	if err := queue.db.Model(&models.CatalogPhysicalWrite{}).Where("owner_id=?", transfer.ID).Update("source_fingerprint", catalogSourceFingerprint(library, storage)).Error; err != nil {
		t.Fatal(err)
	}
	// Original completed counts include every managed file, not the currently remaining subset.
	if err := queue.db.Model(&models.MediaManagedItem{}).Where("transfer_task_id=? AND relative_path=?", transfer.ID, "Show/S01E04.mkv").Update("active", false).Error; err != nil {
		t.Fatal(err)
	}
	unverified("missing original member")
	if err := queue.db.Model(&models.MediaManagedItem{}).Where("transfer_task_id=?", transfer.ID).Update("active", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.MediaLibraryEntry{}).Where("library_id=? AND relative_path=?", library.ID, "Show/S01E04.mkv").Update("match_status", "pending").Error; err != nil {
		t.Fatal(err)
	}
	unverified("unready original member")
	if err := queue.db.Model(&models.MediaLibraryEntry{}).Where("library_id=?", library.ID).Update("match_status", mediaRecognitionStatusMatched).Error; err != nil {
		t.Fatal(err)
	}
	// Queue binding/physical evidence is mandatory, independent of existing-work navigation.
	foreignJobOwner := actor.User
	foreignJobOwner.ID = 0
	foreignJobOwner.Username = "foreign-job-owner"
	foreignJobOwner.UsernameNormalized = "foreign-job-owner"
	if err := queue.db.Create(&foreignJobOwner).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.Job{}).Where("id=?", transfer.JobID).Update("owner_id", foreignJobOwner.ID).Error; err != nil {
		t.Fatal(err)
	}
	unverified("foreign queue owner")
	if err := queue.db.Model(&models.Job{}).Where("id=?", transfer.JobID).Update("owner_id", actor.User.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.CatalogPhysicalWrite{}).Where("owner_id=?", transfer.ID).Update("state", "admitted").Error; err != nil {
		t.Fatal(err)
	}
	unverified("no settled permit")
	if err := queue.db.Model(&models.CatalogPhysicalWrite{}).Where("owner_id=?", transfer.ID).Update("state", "settled").Error; err != nil {
		t.Fatal(err)
	}
	valueAfterRepair, err := service.Get(actor, "tv", tmdbID)
	if err != nil || valueAfterRepair.Stage != "library" {
		t.Fatalf("valid original proof not restored: %+v %v", valueAfterRepair, err)
	}
	// A new source root may publish the same name/size/TMDB, but this earlier
	// transfer's frozen destination does not prove the new root was populated.
	if err := queue.db.Model(&library).Update("relative_root", "/replacement").Error; err != nil {
		t.Fatal(err)
	}
	status, err = service.Get(actor, "tv", tmdbID)
	if err != nil || status.Stage != "import" || status.Status != "publishing" || status.CatalogTarget == nil {
		t.Fatalf("replacement source trusted an old transfer receipt: %+v, %v", status, err)
	}
	if err := queue.db.Model(&library).Update("relative_root", download.TargetRelativeRoot).Error; err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{models.DownloadTaskStatusFailed, models.DownloadTaskStatusCancelled} {
		if err := queue.db.Model(&download).Updates(map[string]any{"phase": phase, "last_error_code": "parent_failure"}).Error; err != nil {
			t.Fatal(err)
		}
		value, err := service.Get(actor, "tv", tmdbID)
		if err != nil || value.Stage != "download" || value.Status != phase || value.LastErrorCode != "parent_failure" || value.CatalogTarget == nil {
			t.Fatalf("published child erased terminal parent: %+v, %v", value, err)
		}
	}
	if err := queue.db.Model(&download).Updates(map[string]any{"phase": models.DownloadTaskStatusCompleted, "last_error_code": ""}).Error; err != nil {
		t.Fatal(err)
	}
	// A new episode task has no exact linked transfer, even though the previous
	// intake of this same show is published in this same library.
	nextJob := enqueueFake(t, queue, actor, "Next episode", "acquisition-next-episode")
	nextDownload := download
	nextDownload.ID, nextDownload.JobID, nextDownload.Phase = uuid.NewString(), nextJob.ID, models.DownloadTaskStatusQueued
	if err := queue.db.Create(&nextDownload).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&acquisition).Update("download_task_id", nextDownload.ID).Error; err != nil {
		t.Fatal(err)
	}
	value, err := service.Get(actor, "tv", tmdbID)
	if err != nil || value.Status != models.DownloadTaskStatusQueued || value.Stage != "download" || value.TransferTaskID != "" || value.CatalogTarget == nil {
		t.Fatalf("old show completed queued new episode: %+v, %v", value, err)
	}
	if err := queue.db.Model(&acquisition).Update("download_task_id", download.ID).Error; err != nil {
		t.Fatal(err)
	}
	denied := actor
	denied.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: uintID(library.ID)}}
	status, err = service.Get(denied, "tv", tmdbID)
	if err != nil {
		t.Fatal(err)
	}
	if status.TargetLibraryID != nil || status.CatalogTarget != nil || status.Title != "" || status.TransferTaskID != transfer.ID || status.Stage != "import" {
		t.Fatalf("denied status=%+v", status)
	}

	foreignUser := models.User{Username: "acquisition-foreign", UsernameNormalized: "acquisition-foreign", DisplayName: "Acquisition Foreign", PasswordHash: "unused", Status: models.UserStatusActive, AuthzVersion: 1, CreatedAt: now, UpdatedAt: now}
	if err := queue.db.Create(&foreignUser).Error; err != nil {
		t.Fatal(err)
	}
	foreign := acquisition
	foreign.ID = uuid.NewString()
	foreign.OwnerID = foreignUser.ID
	foreign.TMDBID++
	foreign.DownloadTaskID = ""
	foreign.TargetLibraryID = nil
	foreign.UpdatedAt = finished.Add(time.Minute)
	if err := queue.db.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	page, err := service.List(actor, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.List) != 1 || page.List[0].ID != acquisition.ID || page.List[0].Title != download.DisplayName || page.List[0].TransferTaskID != transfer.ID {
		t.Fatalf("owner-scoped page=%+v", page)
	}
	deniedPage, err := service.List(denied, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deniedPage.List) != 1 || deniedPage.List[0].TargetLibraryID != nil {
		t.Fatalf("denied page=%+v", deniedPage)
	}
	// Neither an existing show nor a historical publication receipt completes a
	// queued/failed next episode. Catalog navigation remains independently usable.
	for _, phase := range []string{models.TransferTaskStatusQueued, models.TransferTaskStatusFailed} {
		if err := queue.db.Model(&transfer).Update("phase", phase).Error; err != nil {
			t.Fatal(err)
		}
		status, err = service.Get(actor, "tv", tmdbID)
		if err != nil || status.Status != phase || status.Stage == "library" || status.CatalogTarget == nil {
			t.Fatalf("later task falsely completed: %+v, %v", status, err)
		}
	}
	if err := queue.db.Model(&transfer).Update("phase", models.TransferTaskStatusCompleted).Error; err != nil {
		t.Fatal(err)
	}
	if err := queue.db.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND relative_path = ?", library.ID, "Show/S01E04.mkv").Update("provider_id", "replaced-video").Error; err != nil {
		t.Fatal(err)
	}
	status, err = service.Get(actor, "tv", tmdbID)
	if err != nil || status.Stage != "import" || status.Status != "publishing" || status.CatalogTarget == nil {
		t.Fatalf("receipt with replaced current member was trusted: %+v, %v", status, err)
	}
	if err := queue.db.Model(&library).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	status, err = service.Get(actor, "tv", tmdbID)
	if err != nil || status.CatalogTarget != nil {
		t.Fatalf("disabled library target: %+v, %v", status, err)
	}
	if err := queue.db.Model(&library).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}

	// Missing linkage is explicitly unavailable; a storage/query failure is an
	// error, never a successful stale queued projection.
	missing := acquisition
	missing.DownloadTaskID = uuid.NewString()
	err = service.withProjection(actor, func(p *acquisitionProjection) error {
		value, err := p.project(missing)
		if err == nil && (value.Status != "unavailable" || value.Stage == "library") {
			t.Fatalf("dangling task: %+v", value)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	queryFailure := errors.New("fixture transfer read failure")
	if err := queue.db.Callback().Query().Before("gorm:query").Register("acquisition_transfer_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "transfer_tasks" {
			tx.AddError(queryFailure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, readErr := service.Get(actor, "tv", tmdbID)
	queue.db.Callback().Query().Remove("acquisition_transfer_failure")
	if !errors.Is(readErr, queryFailure) {
		t.Fatalf("query failure swallowed: %v", readErr)
	}
	for _, input := range [][2]int{{0, 10}, {1, 0}, {1, 101}} {
		if _, err := service.List(actor, input[0], input[1]); ErrorCode(err) != CodeInvalidRequest {
			t.Fatalf("invalid page input=%v err=%v", input, err)
		}
	}
	withoutPermission := actor
	delete(withoutPermission.Permissions, authz.PermissionDiscoveryRead)
	if _, err := service.Get(withoutPermission, "tv", tmdbID); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("get permission error=%v", err)
	}
	if _, err := service.List(withoutPermission, 1, 10); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("permission error=%v", err)
	}
}

func TestAcquisitionCatalogTargetUsesCurrentEffectiveAuthorizedWork(t *testing.T) {
	_, store, library, rec, entries, actor := playerSnapshotFixture(t)
	service := NewAcquisitionService(store.writeDB)
	service.SetCatalogSnapshotStore(store)
	row := models.MediaAcquisition{OwnerID: actor.User.ID, MediaType: "tv", TMDBID: *rec.TMDBID, Stage: "download", Status: "queued", TargetLibraryID: &library.ID}
	project := func(actor Actor) AcquisitionStatus {
		t.Helper()
		var result AcquisitionStatus
		err := service.withProjection(actor, func(p *acquisitionProjection) error {
			var err error
			result, err = p.project(row)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	initial := project(actor)
	if initial.CatalogTarget == nil || initial.Status != "queued" || initial.Stage != "download" {
		t.Fatalf("availability changed task progress: %+v", initial)
	}
	denied := actor
	denied.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: uintID(library.ID)}}
	if value := project(denied); value.CatalogTarget != nil || value.TargetLibraryID != nil {
		t.Fatalf("denied target: %+v", value)
	}

	// Effective tombstones must win over retained legacy anchors.
	candidate, token := catalogCandidate(t, store, library, "delta", 1)
	facts := make([]models.CatalogEntryFact, 0, len(entries))
	for _, entry := range entries {
		fact := CatalogEntryFromLegacy(entry, &rec)
		fact.Tombstone = true
		facts = append(facts, fact)
	}
	if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Entries: facts}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, candidate, token)
	if value := project(actor); value.CatalogTarget != nil || value.Status != "queued" {
		t.Fatalf("tombstoned target resurrected: %+v", value)
	}
	service.SetCatalogSnapshotStore(nil)
	if err := service.withProjection(actor, func(*acquisitionProjection) error { return nil }); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("versioned ordinary-pool fallback: %v", err)
	}
}

func persistAcquisitionPublication(t *testing.T, db *gorm.DB, libraryID uint, transfer models.TransferTask) models.MediaLibraryScanRun {
	t.Helper()
	var run models.MediaLibraryScanRun
	err := db.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		payload, _ := json.Marshal(transferJobPayload{TransferTaskID: transfer.ID})
		if err := tx.Model(&models.Job{}).Where("id=?", transfer.JobID).Updates(map[string]any{"job_type": "transfer", "payload_json": string(payload), "status": models.JobStatusCompleted}).Error; err != nil {
			return err
		}
		owner, err := catalogPhysicalOwnerTx(tx, CatalogPhysicalWriteInput{LibraryID: libraryID, OwnerKind: CatalogPhysicalTransfer, OwnerID: transfer.ID}, true, false)
		if err != nil {
			return err
		}
		library, storage, _, err := catalogConversionContextTx(tx, libraryID)
		if err != nil {
			return err
		}
		proof := models.CatalogPhysicalWrite{LibraryID: libraryID, OwnerKind: CatalogPhysicalTransfer, OwnerID: transfer.ID, JobID: transfer.JobID, Revision: 1, State: "settled", OwnerDigest: owner.digest, SourceFingerprint: catalogSourceFingerprint(library, storage), SettledAt: &now, EnteredAt: now, UpdatedAt: now}
		if err := tx.Create(&proof).Error; err != nil {
			return err
		}
		checkpoint, _ := json.Marshal(map[string]string{"transfer_batch_id": transfer.ID})
		run = models.MediaLibraryScanRun{LibraryID: libraryID, Kind: "transfer_batch", Status: "success", Phase: "completed", CheckpointJSON: string(checkpoint), CatalogPublishedAt: &now, FinishedAt: &now, StartedAt: now}
		if err := stampAcquisitionPublicationTx(tx, &run); err != nil {
			return err
		}
		return tx.Create(&run).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}
