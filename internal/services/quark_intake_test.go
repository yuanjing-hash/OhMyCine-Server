package services

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func quarkIntakeFixture(t *testing.T) (shareIngestFixture, uint, *ProviderObservationService) {
	t.Helper()
	f := newShareIngestFixture(t)
	library := f.createLibrary(t, "媒体库", "library", "intake", "/中转")
	c := f.libraries.connections
	if err := c.registry.Register(cloud.ProviderQuark, func(cloud.Config) (cloud.Driver, error) { return f.driver, nil }); err != nil {
		t.Fatal(err)
	}
	connection, err := c.Create(f.actor, ConnectionInput{Name: "夸克", Provider: cloud.ProviderQuark, Enabled: true}, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	saveQuarkTestBundle(t, c, connection.ID, func(bundle *quark.Credentials) {
		bundle.TV = testQuarkAuthorization()
		bundle.Agent = testQuarkAuthorization()
		bundle.Proofs = []quark.SpaceProof{{Version: 1, Generation: 1, TVAccount: bundle.TV.AccountID, AgentAccount: bundle.Agent.AccountID, TVRoot: f.storage.RootPath, AgentRoot: "agent-storage", Path: f.storage.RootDisplayPath, ContentSHA256: fmt.Sprintf("%064x", 1), VerifiedAt: time.Now().UTC()}}
	})
	if err := f.db.Model(&f.storage).Updates(map[string]any{"type": models.StorageTypeQuark, "connection_id": connection.ID}).Error; err != nil {
		t.Fatal(err)
	}
	capabilities := downloadpkg.Capabilities{ShareReceive: true, OutputConstraint: downloadpkg.OutputConstraintProviderStorage}
	raw, _ := json.Marshal(capabilities)
	if err := f.db.Model(&f.downloader).Updates(map[string]any{"type": models.DownloaderTypeQuarkShare, "capabilities_json": string(raw), "owner_id": f.actor.User.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.downloads.downloader.registry.Register(models.DownloaderTypeQuarkShare, capabilities, func(downloadpkg.Config) (downloadpkg.Client, error) { return &stubDownloadClient{}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Updates(map[string]any{"enabled": true, "transfer_mode": models.MediaLibraryTransferMove, "ingest_enabled": false, "ingest_provider_root_id": "", "ingest_downloader_id": nil, "ingest_owner_id": nil}).Error; err != nil {
		t.Fatal(err)
	}
	observer := NewProviderObservationService(f.db)
	f.libraries.SetProviderObservationService(observer)
	if _, err := f.libraries.ensureQuarkObservation(context.Background(), library.ID); err != nil {
		t.Fatal(err)
	}
	return f, library.ID, observer
}

type deferredQuarkIntakeEnqueuer struct {
	*DownloadService
	attempts map[string]int
}

func (e *deferredQuarkIntakeEnqueuer) AdoptQuarkObservedItem(_ context.Context, _ *ProviderObservationService, _ models.ProviderObservationScope, item models.ProviderObservationItem) error {
	e.attempts[item.ResourceID]++
	return ErrObservationDeferred
}

func TestQuarkIntakeRotatesDeferredCandidatesAndSkipsClaims(t *testing.T) {
	f, libraryID, _ := quarkIntakeFixture(t)
	ctx := context.Background()
	if _, err := f.libraries.SetQuarkIntake(ctx, f.actor, libraryID, true, f.downloader.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var scope models.ProviderObservationScope
	if err := f.db.Where("kind='intake' AND enabled=?", true).First(&scope).Error; err != nil {
		t.Fatal(err)
	}
	enqueuer := &deferredQuarkIntakeEnqueuer{DownloadService: f.downloads, attempts: map[string]int{}}
	f.libraries.SetIngestEnqueuer(enqueuer)
	for i := range 56 {
		id := fmt.Sprintf("file-%03d", i)
		item := models.ProviderObservationItem{ScopeID: scope.ID, ResourceID: id, ParentID: scope.RootID, Name: id + ".mkv", StableSince: time.Now().UTC().Add(-time.Hour), StableObservations: 2}
		if err := f.db.Create(&item).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := f.db.Create(&models.ProviderObservationIntakeClaim{ID: "claimed", ScopeID: scope.ID, ResourceID: "file-000", Fence: scope.Fence, TaskID: "existing"}).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.libraries.processQuarkIntake(ctx, libraryID); err != nil {
			t.Fatal(err)
		}
	}
	if len(enqueuer.attempts) != 55 || enqueuer.attempts["file-000"] != 0 {
		t.Fatalf("candidate rotation/claim exclusion failed: %+v", enqueuer.attempts)
	}
}

func TestQuarkIntakeStableObservationCreatesOneDurableTaskAndClaim(t *testing.T) {
	f, libraryID, observer := quarkIntakeFixture(t)
	ctx := context.Background()
	configured, err := f.libraries.SetQuarkIntake(ctx, f.actor, libraryID, true, f.downloader.ID, RequestContext{})
	if err != nil || !configured.Enabled || configured.Directory != "/媒体盘/中转" {
		t.Fatalf("configuration=%+v %v", configured, err)
	}
	var scope models.ProviderObservationScope
	if err := f.db.Where("kind='intake' AND enabled=?", true).First(&scope).Error; err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Add(-2 * time.Minute)
	observer.now = func() time.Time { return clock }
	reader := &observationTestReader{tree: map[string][]ObservationFact{"intake": {obsFile("episode", "intake", "Show.S01E01.mkv")}}}
	f.driver.items["episode"] = cloud.Item{ID: "episode", ParentID: "intake", Name: "Show.S01E01.mkv", Size: 1024}
	if err := f.db.Model(&models.ProviderObservationItem{}).Where("scope_id=?", scope.ID).Update("due_at", clock).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := observer.ObserveNext(ctx, scope.ID, reader); err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.processQuarkIntake(ctx, libraryID); err != nil {
		t.Fatal(err)
	}
	var count int64
	f.db.Model(&models.DownloadTask{}).Count(&count)
	if count != 0 {
		t.Fatal("first observation adopted a file")
	}
	clock = clock.Add(70 * time.Second)
	if _, err := observer.ObserveNext(ctx, scope.ID, reader); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.libraries.processQuarkIntake(ctx, libraryID); err != nil {
			t.Fatal(err)
		}
	}
	var tasks []models.DownloadTask
	if err := f.db.Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].SourceOrigin != models.DownloadSourceOriginProviderIngest || tasks[0].TargetLibraryID == nil || *tasks[0].TargetLibraryID != libraryID {
		t.Fatalf("tasks=%+v", tasks)
	}
	var claim models.ProviderObservationIntakeClaim
	if err := f.db.First(&claim).Error; err != nil || claim.TaskID != tasks[0].ID {
		t.Fatalf("claim missing or wrong: %v", err)
	}
	if tasks[0].JobID == "" || tasks[0].StagingProviderDirectoryID != "intake" {
		t.Fatal("frozen route/job missing")
	}
	if _, err := f.libraries.SetQuarkIntake(ctx, f.actor, libraryID, false, "", RequestContext{}); err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.processQuarkIntake(ctx, libraryID); err != nil {
		t.Fatal(err)
	}
	f.db.Model(&models.DownloadTask{}).Count(&count)
	if count != 1 {
		t.Fatal("disable changed a queued task")
	}
}

func TestQuarkIntakeRejectsFinalRootOverlapAndConfigurationDrift(t *testing.T) {
	f, libraryID, observer := quarkIntakeFixture(t)
	ctx := context.Background()
	if err := f.db.Model(&f.downloader).Updates(map[string]any{"provider_directory_id": "library", "provider_directory_path": "/媒体库"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.libraries.SetQuarkIntake(ctx, f.actor, libraryID, true, f.downloader.ID, RequestContext{}); ErrorCode(err) != CodeMediaLibraryOverlap {
		t.Fatalf("overlap accepted: %v", err)
	}
	if err := f.db.Model(&f.downloader).Updates(map[string]any{"provider_directory_id": "intake", "provider_directory_path": "/中转"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.libraries.SetQuarkIntake(ctx, f.actor, libraryID, true, f.downloader.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	var scope models.ProviderObservationScope
	if err := f.db.Where("kind='intake' AND enabled=?", true).First(&scope).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&f.downloader).Update("provider_directory_id", "nested").Error; err != nil {
		t.Fatal(err)
	}
	if err := observer.validScopeTx(f.db, scope); err == nil {
		t.Fatal("changed download root retained adoption authorization")
	}
}
