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
	download := models.DownloadTask{ID: uuid.NewString(), OwnerID: actor.User.ID, JobID: downloadJob.ID, DownloaderName: "Test", ProviderType: models.DownloaderTypeFake, SourceCiphertext: "encrypted", DisplayName: "Test", Phase: models.DownloadTaskStatusCompleted, TargetLibraryID: &library.ID, ScrapeTMDBID: &tmdbID, ScrapeMediaType: "tv", CreatedAt: now, UpdatedAt: now}
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
	if status.Stage != "library" || status.Status != models.TransferTaskStatusCompleted || status.TransferTaskID != transfer.ID || status.ProcessedFiles != 4 || status.TotalFiles != 4 || status.UpdatedAt == nil || !status.UpdatedAt.Equal(finished) || status.TargetLibraryID == nil {
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
}
