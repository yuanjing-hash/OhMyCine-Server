package services

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

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
