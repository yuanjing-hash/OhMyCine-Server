package services

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type quarkCompletionFixture struct {
	libraries *MediaLibraryService
	observer  *ProviderObservationService
	reader    *observationTestReader
	library   models.MediaLibrary
	scope     models.ProviderObservationScope
	clock     time.Time
}

func newQuarkCompletionFixture(t *testing.T) *quarkCompletionFixture {
	t.Helper()
	s, library, _ := createCatalogTestLibrary(t)
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	connection := models.Connection{Name: "completion-quark", NameNormalized: "completion-quark", Provider: models.ConnectionProviderQuark, AccountID: "completion-account", Enabled: true}
	if err := s.db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	storage.Type, storage.ConnectionID, storage.RootPath, storage.RootDisplayPath = models.StorageTypeQuark, &connection.ID, "tv-root", "/媒体"
	library.Enabled, library.ProviderRootID, library.Status = true, storage.RootPath, models.MediaLibraryStatusInitializing
	if err := s.db.Save(&storage).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Save(&library).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error { return initializeFreshQuarkCatalogTx(tx, library, storage, profile) }); err != nil {
		t.Fatal(err)
	}
	var location struct{ File string }
	if err := s.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	readDB, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := readDB.DB()
	t.Cleanup(func() { _ = pool.Close() })
	s.SetCatalogSnapshotStore(NewCatalogSnapshotStore(s.db, readDB))
	s.changes = NewMediaChangeService(s.db)
	s.queue = NewQueueService(s.db, NewAuditService(s.db))
	s.structure = NewMediaLibraryStructureService(s.db, s.audit, s.queue, nil, zerolog.Nop())
	s.structure.SetCatalogSnapshotStore(s.catalogStore)
	s.EnableCatalogScanFollowups()
	f := &quarkCompletionFixture{libraries: s, library: library, clock: time.Now().UTC(), reader: &observationTestReader{tree: map[string][]ObservationFact{}}}
	f.observer = NewProviderObservationService(s.db)
	f.observer.now = func() time.Time { return f.clock }
	s.SetProviderObservationService(f.observer)
	f.observer.SetReaderResolver(func(context.Context, models.ProviderObservationScope) (ObservationDirectoryReader, error) {
		return f.reader, nil
	})
	if _, err := s.requestQuarkObservation(context.Background(), library.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Where("library_id = ? AND enabled = ?", library.ID, true).First(&f.scope).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *quarkCompletionFixture) observe(t *testing.T) {
	t.Helper()
	f.clock = f.clock.Add(2 * time.Second)
	if err := f.observer.RunDue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
}

func (f *quarkCompletionFixture) diagnose(t *testing.T) {
	t.Helper()
	s := f.libraries
	if err := s.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	job, err := s.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || job == nil {
		t.Fatalf("automatic diagnosis did not become runnable: job=%v err=%v", job, err)
	}
	if result := NewMediaLibraryStructureDiagnosisWorker(s.structure).Run(context.Background(), fastScanTestRuntime{}, *job); result.ErrorCode != "" {
		t.Fatalf("diagnosis: %+v", result)
	}
	if err := s.queue.Complete(job.Job.ID, job.LeaseToken); err != nil {
		t.Fatal(err)
	}
}

func TestQuarkCompleteEmptyObservationBecomesReady(t *testing.T) {
	f := newQuarkCompletionFixture(t)
	f.observe(t)
	var run models.MediaLibraryScanRun
	if err := f.libraries.db.First(&run, f.scope.RequestedScanRunID).Error; err != nil {
		t.Fatal(err)
	}
	if run.Status != "success" || run.Partial || run.Discovered != 0 {
		t.Fatalf("empty full observation did not finish: %+v", run)
	}
	var library models.MediaLibrary
	if err := f.libraries.db.First(&library, f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	readiness, err := libraryReadiness(f.libraries.db, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	if library.BaselineGeneration == 0 {
		t.Fatalf("successful empty observation lost baseline: scan=%s media=%d baseline=%d readiness=%+v", run.Status, run.Discovered, library.BaselineGeneration, readiness)
	}
	if readiness.Ready || readiness.ReadinessStatus != "checking" {
		t.Fatalf("diagnosis gate was bypassed: %+v", readiness)
	}
	f.diagnose(t)
	readiness, err = libraryReadiness(f.libraries.db, library.ID)
	if err != nil || !readiness.Ready {
		t.Fatalf("complete empty library not ready: %+v %v", readiness, err)
	}
}

func TestQuarkCompleteFilteredObservationAndNoop(t *testing.T) {
	f := newQuarkCompletionFixture(t)
	f.reader.tree["tv-root"] = []ObservationFact{obsFile("notes", "tv-root", "notes.txt")}
	var before models.CatalogHead
	if err := f.libraries.db.First(&before, "library_id = ?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	f.observe(t)
	f.diagnose(t)
	var library models.MediaLibrary
	if err := f.libraries.db.First(&library, f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	var followup models.CatalogScanFollowup
	if err := f.libraries.db.First(&followup, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := f.libraries.requestQuarkObservation(context.Background(), library.ID, true); err != nil {
			t.Fatal(err)
		}
		f.observe(t)
		if err := f.libraries.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
			t.Fatal(err)
		}
	}
	var after models.CatalogHead
	var saved models.MediaLibrary
	var finalFollowup models.CatalogScanFollowup
	if err := f.libraries.db.First(&after, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.db.First(&saved, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.db.First(&finalFollowup, "library_id = ?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || saved.BaselineGeneration != library.BaselineGeneration || finalFollowup.ReceiptID != followup.ReceiptID {
		t.Fatal("empty/no-media rounds churned head, baseline or followup ownership")
	}
	if saved.Status != models.MediaLibraryStatusListening {
		t.Fatalf("operational status=%s", saved.Status)
	}
	if rows := catalogReadEntries(t, f.libraries.catalogStore, library.ID, ""); len(rows) != 0 {
		t.Fatal("fabricated media")
	}
	for _, model := range []any{&models.CatalogArtifactBinding{}, &models.MediaArtifactRun{}, &models.MediaLibraryChange{}, &models.MediaChangeDispatch{}} {
		var count int64
		if err := f.libraries.db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("empty completion manufactured work: %T count=%d err=%v", model, count, err)
		}
	}
}

func TestQuarkCompleteObservationRecoversFinishedSummaryAfterRestart(t *testing.T) {
	f := newQuarkCompletionFixture(t)
	// Reproduce the released crash boundary: genuine complete coverage and a
	// successful summary persist, but no library/followup completion exists.
	f.observer.SetProgressListener(nil)
	f.observe(t)
	var scope models.ProviderObservationScope
	if err := f.libraries.db.First(&scope, "id = ?", f.scope.ID).Error; err != nil {
		t.Fatal(err)
	}
	if scope.LastCompleteAt == nil {
		t.Fatal("fixture did not complete provider coverage")
	}
	if err := f.libraries.db.Model(&models.MediaLibraryScanRun{}).Where("id = ?", scope.RequestedScanRunID).Updates(map[string]any{"status": "success", "phase": "completed", "partial": false, "finished_at": scope.LastCompleteAt}).Error; err != nil {
		t.Fatal(err)
	}
	s := f.libraries
	restarted := NewMediaLibraryService(s.db, s.audit, zerolog.Nop())
	t.Cleanup(restarted.Close)
	restarted.SetCatalogSnapshotStore(s.catalogStore)
	restarted.queue, restarted.changes, restarted.structure = s.queue, s.changes, s.structure
	restarted.EnableCatalogScanFollowups()
	observer := NewProviderObservationService(s.db)
	observer.now = func() time.Time { return f.clock }
	restarted.SetProviderObservationService(observer)
	reads := 0
	observer.SetReaderResolver(func(context.Context, models.ProviderObservationScope) (ObservationDirectoryReader, error) {
		reads++
		return f.reader, nil
	})
	f.libraries, f.observer = restarted, observer
	f.observe(t)
	if reads != 1 || len(f.reader.calls) != 1 {
		t.Fatal("completion recovery re-enumerated the provider")
	}
	f.diagnose(t)
	ready, err := libraryReadiness(s.db, f.library.ID)
	if err != nil || !ready.Ready {
		t.Fatalf("finished summary did not recover: %+v %v", ready, err)
	}
}

func TestQuarkCompleteObservationRejectsPartialAndRecovers(t *testing.T) {
	f := newQuarkCompletionFixture(t)
	f.reader.hook = func(string, int64) (ObservationDirectoryPage, error, bool) {
		n := int64(0)
		return ObservationDirectoryPage{Partial: true, Total: &n}, nil, true
	}
	f.observe(t)
	var library models.MediaLibrary
	if err := f.libraries.db.First(&library, f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if library.BaselineGeneration != 0 {
		t.Fatal("partial listing established baseline")
	}
	f.reader.hook = nil
	f.clock = f.clock.Add(time.Minute)
	f.observe(t)
	f.diagnose(t)
	ready, err := libraryReadiness(f.libraries.db, f.library.ID)
	if err != nil || !ready.Ready {
		t.Fatalf("ordinary complete retry did not converge: %+v %v", ready, err)
	}
}

func TestQuarkCompleteObservationFailureRollsBack(t *testing.T) {
	f := newQuarkCompletionFixture(t)
	f.observer.SetProgressListener(nil)
	f.observe(t)
	var scope models.ProviderObservationScope
	if err := f.libraries.db.First(&scope, "id = ?", f.scope.ID).Error; err != nil {
		t.Fatal(err)
	}
	f.libraries.SetCatalogScanCommit(func(*gorm.DB, CatalogScanPublication) error { return errors.New("fixture followup persistence failed") })
	if err := f.libraries.completeQuarkObservation(context.Background(), scope); err == nil {
		t.Fatal("persistence failure was swallowed")
	}
	var library models.MediaLibrary
	var run models.MediaLibraryScanRun
	if err := f.libraries.db.First(&library, f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.db.First(&run, scope.RequestedScanRunID).Error; err != nil {
		t.Fatal(err)
	}
	if library.BaselineGeneration != 0 || run.CatalogPublishedAt != nil || run.FinishedAt != nil {
		t.Fatal("failed completion leaked partial evidence")
	}
	f.libraries.EnableCatalogScanFollowups()
	if err := f.libraries.completeQuarkObservation(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	f.diagnose(t)
}

func TestQuarkCompletePositiveObservationKeepsPendingOwner(t *testing.T) {
	f := newQuarkCompletionFixture(t)
	f.observer.SetProgressListener(nil)
	f.reader.tree["tv-root"] = []ObservationFact{obsFile("video", "tv-root", "Movie.2026.mkv")}
	f.observe(t)
	// A real queued catalog publication can still own recognition when full
	// scope coverage drains. Completion must retain that exact receipt/run.
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := f.libraries.db.First(&storage, f.library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.db.First(&profile, f.library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	library, run := catalogScanRun(t, f.libraries, f.library.ID, storage, profile)
	run.Kind = "provider_observation"
	if _, err := f.libraries.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Partial: true, Scoped: true, Files: []medialibrary.File{{RelativePath: "/Queued.2026.mkv", ProviderID: "queued-video", ProviderIDStable: true, Size: 2}}}, true, f.libraries.catalogScanCommit); err != nil {
		t.Fatal(err)
	}
	var before models.CatalogScanFollowup
	if err := f.libraries.db.First(&before, "library_id = ?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !before.RecognitionPending || before.CompleteObserved {
		t.Fatalf("fixture did not publish pending partial facts: %+v", before)
	}
	var scope models.ProviderObservationScope
	if err := f.libraries.db.First(&scope, "id = ?", f.scope.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.completeQuarkObservation(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	var followup models.CatalogScanFollowup
	if err := f.libraries.db.First(&followup, "library_id = ?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !followup.CompleteObserved || !followup.DiagnosisPending || !followup.RecognitionPending || followup.ReceiptID != before.ReceiptID || followup.ScanRunID != before.ScanRunID || followup.Generation != before.Generation || followup.ArtifactPending != before.ArtifactPending {
		t.Fatalf("complete coverage replaced or lost the actual publication owner: %+v", followup)
	}
	if rows := catalogReadEntries(t, f.libraries.catalogStore, f.library.ID, ""); len(rows) != 2 {
		t.Fatalf("media count=%d", len(rows))
	}
	if err := f.libraries.RecoverCatalogScanFollowups(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	job, err := f.libraries.queue.Claim([]string{JobTypeMediaLibraryStructureDiagnosis})
	if err != nil || job != nil {
		t.Fatalf("diagnosis bypassed pending recognition: %v %v", job, err)
	}
	recognition, err := f.libraries.queue.Claim([]string{JobTypeMediaLibraryRecognition})
	if err != nil || recognition == nil {
		t.Fatalf("pending recognition not claimable: %v %v", recognition, err)
	}
	if result := NewMediaLibraryRecognitionWorker(f.libraries).Run(context.Background(), fastScanTestRuntime{}, *recognition); result.ErrorCode != "" {
		t.Fatalf("recognition: %+v", result)
	}
	if err := f.libraries.queue.Complete(recognition.Job.ID, recognition.LeaseToken); err != nil {
		t.Fatal(err)
	}
	f.diagnose(t)
	ready, err := libraryReadiness(f.libraries.db, f.library.ID)
	if err != nil || !ready.Ready {
		t.Fatalf("nonempty source did not become ready: %+v %v", ready, err)
	}
}

func TestQuarkCompleteObservationRechecksDurableFences(t *testing.T) {
	f := newQuarkCompletionFixture(t)
	f.observer.SetProgressListener(nil)
	f.observe(t)
	db := f.libraries.db
	var scope models.ProviderObservationScope
	if err := db.First(&scope, "id = ?", f.scope.ID).Error; err != nil {
		t.Fatal(err)
	}
	var root models.ProviderObservationItem
	if err := observationCurrent(db, scope.ID).First(&root, "resource_id = ?", scope.RootID).Error; err != nil {
		t.Fatal(err)
	}
	var connection models.Connection
	var profile models.MediaClassificationProfile
	if err := db.First(&connection, scope.ConnectionID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&profile, f.library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		model   any
		where   string
		id      any
		field   string
		value   any
		restore any
	}{
		{"old-coverage", &models.ProviderObservationScope{}, "id = ?", scope.ID, "last_complete_at", scope.CoverageStartedAt.Add(-time.Second), scope.LastCompleteAt},
		{"read-error", &models.ProviderObservationScope{}, "id = ?", scope.ID, "last_error_code", "read_failed", ""},
		{"new-round", &models.ProviderObservationScope{}, "id = ?", scope.ID, "coverage_round", scope.CoverageRound + 1, scope.CoverageRound},
		{"pending-child", &models.ProviderObservationItem{}, "scope_id = ? AND batch_id = '' AND resource_id = 'tv-root'", scope.ID, "completed_round", 0, root.CompletedRound},
		{"path-repair", &models.ProviderObservationItem{}, "scope_id = ? AND batch_id = '' AND resource_id = 'tv-root'", scope.ID, "path_repair_pending", true, false},
		{"active-reader", &models.ProviderObservationItem{}, "scope_id = ? AND batch_id = '' AND resource_id = 'tv-root'", scope.ID, "active_batch_id", "another-reader", ""},
		{"profile", &models.MediaClassificationProfile{}, "id = ?", profile.ID, "revision", profile.Revision + 1, profile.Revision},
		{"connection-config", &models.Connection{}, "id = ?", connection.ID, "revision", connection.Revision + 1, connection.Revision},
		{"account", &models.Connection{}, "id = ?", connection.ID, "account_id", "replacement-account", connection.AccountID},
		{"storage-root", &models.Storage{}, "id = ?", f.library.StorageID, "root_path", "replacement-root", scope.RootID},
		{"source-root", &models.MediaLibrary{}, "id = ?", f.library.ID, "provider_root_id", "replacement-root", scope.RootID},
		{"source-epoch", &models.CatalogHead{}, "library_id = ?", f.library.ID, "source_epoch", 2, 1},
		{"disabled", &models.MediaLibrary{}, "id = ?", f.library.ID, "enabled", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := db.Model(test.model).Where(test.where, test.id).Update(test.field, test.value).Error; err != nil {
				t.Fatal(err)
			}
			err := f.libraries.completeQuarkObservation(context.Background(), scope)
			if err != nil && !errors.Is(err, ErrObservationFence) && !errors.Is(err, ErrCatalogFence) {
				t.Fatal(err)
			}
			var library models.MediaLibrary
			if err := db.First(&library, f.library.ID).Error; err != nil {
				t.Fatal(err)
			}
			if library.BaselineGeneration != 0 {
				t.Fatal("stale or undrained scope established baseline")
			}
			if err := db.Model(test.model).Where(test.where, test.id).Update(test.field, test.restore).Error; err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, state := range []string{"unpublished", "reading", "confirming", "pending-write", "lease", "physical-write", "structure-hold", "retiring"} {
		t.Run(state, func(t *testing.T) {
			var cleanup func()
			switch state {
			case "unpublished", "reading", "confirming":
				batch := models.ProviderObservationBatch{ID: "pending-fixture", ScopeID: scope.ID, Fence: scope.Fence, State: state}
				if state == "unpublished" {
					batch.State = "complete"
					batch.Revision = 1
				}
				if err := db.Create(&batch).Error; err != nil {
					t.Fatal(err)
				}
				cleanup = func() { db.Delete(&batch) }
			case "pending-write":
				row := models.QuarkOperationReceipt{ID: "pending-fixture", ScopeID: scope.ID, ConnectionID: scope.ConnectionID, Kind: "upload", State: "unknown"}
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
				cleanup = func() { db.Delete(&row) }
			case "lease":
				if err := db.Model(&models.ProviderObservationAccountBudget{}).Where("account_key = ?", scope.AccountKey).Updates(map[string]any{"last_scope_id": scope.ID, "lease_token": "other-reader", "lease_until": f.clock.Add(time.Minute)}).Error; err != nil {
					t.Fatal(err)
				}
				cleanup = func() {
					db.Model(&models.ProviderObservationAccountBudget{}).Where("account_key = ?", scope.AccountKey).Updates(map[string]any{"lease_token": "", "lease_until": time.Time{}})
				}
			case "physical-write":
				row := models.CatalogPhysicalWrite{LibraryID: scope.LibraryID, OwnerKind: "repair", OwnerID: "pending-fixture", State: "entered", Revision: 1, EnteredAt: f.clock, UpdatedAt: f.clock}
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
				cleanup = func() { db.Delete(&row) }
			case "structure-hold":
				job := models.Job{ID: "held-job", CreatedByKind: "system", JobType: JobTypeMediaReorganization, Status: models.JobStatusFailed, Generation: 1, Revision: 1, PayloadJSON: "{}"}
				if err := db.Create(&job).Error; err != nil {
					t.Fatal(err)
				}
				row := models.MediaLibraryStructureRepair{ID: "held-repair", OwnerID: 1, JobID: &job.ID, LibraryID: scope.LibraryID, Scope: "full", RuleFingerprint: "fixture", Generation: 1, PlanJSON: "{}", StateJSON: `{"observation_hold_version":1}`, Phase: "failed"}
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
				cleanup = func() { db.Delete(&row); db.Delete(&job) }
			case "retiring":
				row := models.MediaLibraryRetirement{ID: "pending-fixture", LibraryID: scope.LibraryID, Phase: "queued"}
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
				cleanup = func() { db.Delete(&row) }
			}
			defer cleanup()
			if err := f.libraries.completeQuarkObservation(context.Background(), scope); err != nil && !errors.Is(err, ErrObservationDeferred) && !errors.Is(err, ErrObservationFence) {
				t.Fatal(err)
			}
			var library models.MediaLibrary
			if err := db.First(&library, f.library.ID).Error; err != nil {
				t.Fatal(err)
			}
			if library.BaselineGeneration != 0 {
				t.Fatal("pending work established baseline")
			}
		})
	}
	if err := f.libraries.completeQuarkObservation(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	f.diagnose(t)
}

func TestQuarkCompleteObservationWaitsForFailedDescendant(t *testing.T) {
	f := newQuarkCompletionFixture(t)
	f.reader.tree["tv-root"] = []ObservationFact{obsDir("a-empty", "tv-root", "空目录"), obsDir("z-failing", "tv-root", "待核对目录")}
	f.observe(t)
	f.observe(t)
	status, err := f.observer.Status(context.Background(), f.scope.ID)
	if err != nil || status.LastCompleteAt != nil || status.PendingDirectories != 1 {
		t.Fatalf("empty child falsely completed scope: %+v %v", status, err)
	}
	f.reader.hook = func(id string, _ int64) (ObservationDirectoryPage, error, bool) {
		return ObservationDirectoryPage{}, errors.New("fixture child unavailable"), id == "z-failing"
	}
	f.clock = f.clock.Add(2 * time.Second)
	if err := f.observer.RunDue(context.Background(), 1); err == nil {
		t.Fatal("child read failure not observed")
	}
	readiness, err := libraryReadiness(f.libraries.db, f.library.ID)
	if err != nil || readiness.Ready {
		t.Fatalf("failed descendant became ready: %+v %v", readiness, err)
	}
	var followup models.CatalogScanFollowup
	if err := f.libraries.db.First(&followup, "library_id = ?", f.library.ID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal(err)
	}
	if followup.CompleteObserved {
		t.Fatal("positive directory publication invented full coverage")
	}
	f.reader.hook = nil
	f.clock = f.clock.Add(time.Minute)
	f.observe(t)
	f.diagnose(t)
	readiness, err = libraryReadiness(f.libraries.db, f.library.ID)
	if err != nil || !readiness.Ready {
		t.Fatalf("completed descendant retry not ready: %+v %v", readiness, err)
	}
}
