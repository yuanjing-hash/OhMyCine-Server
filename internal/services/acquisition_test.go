package services

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
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
	download := models.DownloadTask{ID: uuid.NewString(), OwnerID: actor.User.ID, JobID: downloadJob.ID, DownloaderName: "Test", ProviderType: models.DownloaderTypeFake, SourceCiphertext: "encrypted", DisplayName: "Test", Phase: models.DownloadTaskStatusCompleted, Progress: &progress, BytesCompleted: &bytesCompleted, BytesTotal: &bytesTotal, DownloadSpeed: &downloadSpeed, ETASeconds: &etaSeconds, TargetLibraryID: &library.ID, ScrapeTMDBID: &tmdbID, ScrapeMediaType: "tv", CreatedAt: now, UpdatedAt: now}
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
	if status.Stage != "library" || status.Status != models.TransferTaskStatusCompleted || status.TransferTaskID != transfer.ID || status.Progress == nil || *status.Progress != progress || status.BytesCompleted == nil || *status.BytesCompleted != bytesCompleted || status.DownloadSpeed == nil || *status.DownloadSpeed != downloadSpeed || status.ProcessedFiles != 4 || status.TotalFiles != 4 || status.UpdatedAt == nil || !status.UpdatedAt.Equal(finished) || status.TargetLibraryID == nil {
		t.Fatalf("status=%+v", status)
	}
	denied := actor
	denied.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: uintID(library.ID)}}
	status, err = service.Get(denied, "tv", tmdbID)
	if err != nil {
		t.Fatal(err)
	}
	if status.TargetLibraryID != nil || status.TransferTaskID != transfer.ID || status.Stage != "library" {
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
	for _, input := range [][2]int{{0, 10}, {1, 0}, {1, 101}} {
		if _, err := service.List(actor, input[0], input[1]); ErrorCode(err) != CodeInvalidRequest {
			t.Fatalf("invalid page input=%v err=%v", input, err)
		}
	}
	withoutPermission := actor
	delete(withoutPermission.Permissions, authz.PermissionDiscoveryRead)
	if _, err := service.List(withoutPermission, 1, 10); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("permission error=%v", err)
	}
}
