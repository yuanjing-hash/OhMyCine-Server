package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func catalogScanFixture(t *testing.T, provider bool) (*MediaLibraryService, models.MediaLibrary, models.Storage, models.MediaClassificationProfile, []models.MediaLibraryEntry) {
	t.Helper()
	store, library, rec, entries := catalogFixture(t)
	rec.ManualOverride = true
	catalogConvert(t, store, library, rec, entries)
	service := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
	service.SetCatalogSnapshotStore(store)
	// This no-op is fixture-only. Production requires the durable projection and
	// follow-up hook; these tests do not assert those downstream units complete.
	service.SetCatalogScanCommit(func(*gorm.DB, CatalogScanPublication) error { return nil })
	var storage models.Storage
	if err := store.writeDB.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if provider {
		storage.Type = models.StorageTypePan115
		if err := store.writeDB.Model(&storage).Update("type", storage.Type).Error; err != nil {
			t.Fatal(err)
		}
	}
	var profile models.MediaClassificationProfile
	if err := store.writeDB.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	return service, library, storage, profile, entries
}

func catalogScanRun(t *testing.T, service *MediaLibraryService, libraryID uint, storage models.Storage, profile models.MediaClassificationProfile) (models.MediaLibrary, models.MediaLibraryScanRun) {
	t.Helper()
	var library models.MediaLibrary
	if err := service.db.First(&library, libraryID).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Generation: library.DirtyGeneration + 1, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), Status: "running", Phase: "enumerating", Kind: "manual", StartedAt: time.Now().UTC(), CheckpointJSON: "{}"}
	if err := service.db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	return library, run
}

func scanFile(entry models.MediaLibraryEntry) medialibrary.File {
	return medialibrary.File{RelativePath: entry.RelativePath, ProviderID: entry.ProviderID, ProviderIDStable: true, Size: entry.Size, ModifiedAt: entry.ModifiedAt}
}

func TestCatalogScanFullAndPartialPreserveVersionsAndStableIdentity(t *testing.T) {
	for _, provider := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "115"}[provider], func(t *testing.T) {
			service, library, storage, profile, entries := catalogScanFixture(t, provider)
			library, run := catalogScanRun(t, service, library.ID, storage, profile)
			result := medialibrary.Result{Files: []medialibrary.File{scanFile(entries[0]), scanFile(entries[1])}, Assets: []medialibrary.SourceAsset{{RelativePath: "Show/poster.jpg", ProviderID: "poster", Name: "poster.jpg", Extension: ".jpg", Size: 10}}}
			published, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, result, provider, service.catalogScanCommit)
			if err != nil {
				t.Fatal(err)
			}
			if published.Status != "success" || published.Added != 0 || published.Removed != 0 {
				t.Fatalf("full run: %+v", published)
			}
			rows := catalogReadEntries(t, service.catalogStore, library.ID, "")
			if len(rows) != 2 || rows[0].ID != entries[0].ID || rows[1].ID != entries[1].ID || *rows[0].Season != 2 || *rows[0].Episode != 1 {
				t.Fatalf("version identity: %+v", rows)
			}
			library, run = catalogScanRun(t, service, library.ID, storage, profile)
			changed := scanFile(entries[0])
			changed.Size = 123
			partial := medialibrary.Result{Partial: true, Files: []medialibrary.File{changed}}
			published, err = service.publishCatalogScan(context.Background(), library, storage, profile, run, partial, provider, service.catalogScanCommit)
			if err != nil {
				t.Fatal(err)
			}
			if published.Updated != 1 || published.Removed != 0 {
				t.Fatalf("partial run: %+v", published)
			}
			rows = catalogReadEntries(t, service.catalogStore, library.ID, "")
			if len(rows) != 2 || rows[0].Size != 123 || rows[1].ID != entries[1].ID {
				t.Fatalf("partial dropped unseen: %+v", rows)
			}
			var layers []models.CatalogHeadLayer
			if err := service.db.Where("library_id=?", library.ID).Order("rank").Find(&layers).Error; err != nil {
				t.Fatal(err)
			}
			if len(layers) != 2 {
				t.Fatalf("partial must append bounded delta, layers=%+v", layers)
			}
			var delta models.CatalogSnapshot
			if err := service.db.First(&delta, "id=?", layers[1].SnapshotID).Error; err != nil {
				t.Fatal(err)
			}
			if delta.Kind != "delta" || delta.RowCount > 2 {
				t.Fatalf("partial rewrote unchanged library: %+v", delta)
			}
			if err := service.catalogStore.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error {
				var assets []models.MediaLibrarySourceAsset
				if err := r.SourceAssets().Find(&assets).Error; err != nil {
					return err
				}
				if len(assets) != 1 {
					t.Fatalf("partial dropped unseen sidecar: %+v", assets)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			library, run = catalogScanRun(t, service, library.ID, storage, profile)
			full := medialibrary.Result{Files: []medialibrary.File{changed}}
			published, err = service.publishCatalogScan(context.Background(), library, storage, profile, run, full, provider, service.catalogScanCommit)
			if err != nil {
				t.Fatal(err)
			}
			rows = catalogReadEntries(t, service.catalogStore, library.ID, "")
			if published.Removed != 1 || len(rows) != 1 || rows[0].ID != entries[0].ID {
				t.Fatalf("full absence: %+v %+v", published, rows)
			}
			var anchor models.MediaLibraryEntry
			if err := service.db.First(&anchor, entries[1].ID).Error; err != nil {
				t.Fatal("retired entry anchor removed", err)
			}
		})
	}
}

func TestCatalogScanScopedRenameAndFailureKeepOldHead(t *testing.T) {
	service, library, storage, profile, entries := catalogScanFixture(t, true)
	library, run := catalogScanRun(t, service, library.ID, storage, profile)
	renamed := scanFile(entries[0])
	renamed.RelativePath = "Show/Season 02/01-new.h264.mkv"
	result := medialibrary.Result{Partial: true, Scoped: true, Files: []medialibrary.File{renamed}, DeletedProviderIDs: []string{entries[1].ProviderID}}
	published, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, result, true, service.catalogScanCommit)
	if err != nil {
		t.Fatal(err)
	}
	rows := catalogReadEntries(t, service.catalogStore, library.ID, "")
	if len(rows) != 1 || rows[0].ID != entries[0].ID || rows[0].RelativePath != renamed.RelativePath || published.Removed != 1 {
		t.Fatalf("rename/delete: %+v %+v", published, rows)
	}
	before := rows[0]
	library, run = catalogScanRun(t, service, library.ID, storage, profile)
	renamed.Size = 500
	failure := errors.New("fixture publication hook failure")
	_, err = service.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: []medialibrary.File{renamed}}, true, func(*gorm.DB, CatalogScanPublication) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("hook error: %v", err)
	}
	rows = catalogReadEntries(t, service.catalogStore, library.ID, "")
	if len(rows) != 1 || rows[0].Size != before.Size {
		t.Fatal("failed publication escaped")
	}
	var storedRun models.MediaLibraryScanRun
	if err := service.db.First(&storedRun, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if storedRun.Status != "running" {
		t.Fatal("failed hook acknowledged scan success")
	}
	service.SetCatalogScanCommit(nil)
	if _, err := service.catalogScanVersioned(context.Background(), library.ID); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("missing hook not gated: %v", err)
	}
	if _, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, result, true, nil); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("nil hook: %v", err)
	}
}

type catalogScanBackend struct {
	kind string
	scan func(context.Context, MediaLibraryScanRequest) (medialibrary.Result, error)
}

func (b catalogScanBackend) StorageType() string { return b.kind }
func (b catalogScanBackend) Scan(ctx context.Context, request MediaLibraryScanRequest) (medialibrary.Result, error) {
	return b.scan(ctx, request)
}
func (b catalogScanBackend) OpenListener(context.Context, models.MediaLibrary, models.Storage, <-chan struct{}, *providerChangeAccumulator) (MediaLibraryListener, error) {
	return nil, errors.New("fixture listener unused")
}

func TestCatalogScanReconcileRoutesLocalAnd115WithoutLegacyPublication(t *testing.T) {
	for _, provider := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "115"}[provider], func(t *testing.T) {
			service, library, storage, _, entries := catalogScanFixture(t, provider)
			if provider {
				service.queue = &QueueService{}
			}
			called := 0
			service.backends = NewMediaLibraryBackendRegistry(catalogScanBackend{kind: storage.Type, scan: func(ctx context.Context, _ MediaLibraryScanRequest) (medialibrary.Result, error) {
				called++
				// A provider may take arbitrarily long. It must not run with the
				// ordinary DB connection/transaction reserved by the publisher.
				probe, cancel := context.WithTimeout(ctx, time.Second)
				defer cancel()
				if err := service.db.WithContext(probe).Exec("SELECT 1").Error; err != nil {
					return medialibrary.Result{}, err
				}
				return medialibrary.Result{Files: []medialibrary.File{scanFile(entries[0]), scanFile(entries[1])}}, nil
			}})
			var anchorBefore models.MediaLibraryEntry
			if err := service.db.First(&anchorBefore, entries[0].ID).Error; err != nil {
				t.Fatal(err)
			}
			run, err := service.reconcile(context.Background(), library.ID, "manual")
			if err != nil {
				t.Fatal(err)
			}
			if called != 1 || run.Status != "success" {
				t.Fatalf("route called=%d run=%+v", called, run)
			}
			var anchorAfter models.MediaLibraryEntry
			if err := service.db.First(&anchorAfter, entries[0].ID).Error; err != nil {
				t.Fatal(err)
			}
			if anchorAfter.LastGeneration != anchorBefore.LastGeneration || anchorAfter.UpdatedAt != anchorBefore.UpdatedAt {
				t.Fatal("reconcile mutated old live-table anchor")
			}
			var staging int64
			if err := service.db.Model(&models.MediaLibraryScanStaging{}).Where("run_id=?", run.ID).Count(&staging).Error; err != nil {
				t.Fatal(err)
			}
			if staging != 0 {
				t.Fatal("versioned path wrote legacy fast staging")
			}
		})
	}
}

func TestCatalogScanCanceledAndOversizedBatchCannotPublish(t *testing.T) {
	service, library, storage, profile, entries := catalogScanFixture(t, true)
	library, run := catalogScanRun(t, service, library.ID, storage, profile)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.publishCatalogScan(ctx, library, storage, profile, run, medialibrary.Result{Files: []medialibrary.File{scanFile(entries[0])}}, true, service.catalogScanCommit); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled=%v", err)
	}
	rows := catalogReadEntries(t, service.catalogStore, library.ID, "")
	if len(rows) != 2 {
		t.Fatal("canceled scan changed current head")
	}
	candidate, token := catalogCandidate(t, service.catalogStore, library, "delta", 1)
	var record models.MediaLibraryRecognition
	if err := service.catalogStore.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error { return r.Recognitions().First(&record).Error }); err != nil {
		t.Fatal(err)
	}
	record.MetadataJSON = string(make([]byte, CatalogBatchBytes+1))
	if err := service.appendCatalogScanFacts(context.Background(), candidate, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(record)}}); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("oversized=%v", err)
	}
	abandonCatalogScan(service.catalogStore, candidate.ID, token)
}

func TestCatalogScanRebasesManualCorrectionWithoutRepeatingProviderScan(t *testing.T) {
	service, library, storage, _, entries := catalogScanFixture(t, false)
	manualGeneration := library.DirtyGeneration + 3
	scans := 0
	service.backends = NewMediaLibraryBackendRegistry(catalogScanBackend{kind: storage.Type, scan: func(context.Context, MediaLibraryScanRequest) (medialibrary.Result, error) {
		scans++
		return medialibrary.Result{Files: []medialibrary.File{scanFile(entries[0]), scanFile(entries[1])}}, nil
	}})
	metadata := NewMetadataSettingsService(service.db, NewAuditService(service.db), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "test-token"})
	lookups := 0
	metadata.clientFactory = func(credential tmdb.Credential, apiBase, imageBase string) (*tmdb.Client, error) {
		lookups++
		if lookups == 1 {
			// Recognition setup is outside the pinned reader and writer. Publish
			// a newer manual result between baseline capture and candidate CAS.
			var rec models.MediaLibraryRecognition
			if err := service.catalogStore.Read(context.Background(), []uint{library.ID}, func(r *CatalogReader) error { return r.Recognitions().First(&rec).Error }); err != nil {
				t.Fatal(err)
			}
			candidate, token := catalogCandidate(t, service.catalogStore, library, "delta", 1)
			rec.Title = "User corrected during scan"
			rec.ManualOverride = true
			if err := service.catalogStore.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
				t.Fatal(err)
			}
			catalogPublish(t, service.catalogStore, candidate, token)
			if err := service.db.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Updates(map[string]any{"dirty_generation": manualGeneration, "artifact_generation": manualGeneration}).Error; err != nil {
				t.Fatal(err)
			}
		}
		return tmdb.NewWithCredentialRoutes(credential, apiBase, imageBase)
	}
	service.SetMetadataSettingsService(metadata)
	run, err := service.reconcile(context.Background(), library.ID, "manual")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "success" || scans != 1 || lookups != 2 {
		t.Fatalf("rebase run=%+v scans=%d lookups=%d", run, scans, lookups)
	}
	if run.Generation != manualGeneration+1 {
		t.Fatalf("scan reused manual artifact generation: %d", run.Generation)
	}
	rows := catalogReadEntries(t, service.catalogStore, library.ID, "")
	if len(rows) != 2 || rows[0].SeriesTitle != "User corrected during scan" || rows[0].ID != entries[0].ID {
		t.Fatalf("manual result lost on rebase: %+v", rows)
	}
}

func TestCatalogScanRebaseDoesNotLaunderChangedSourceEvidence(t *testing.T) {
	service, library, storage, profile, entries := catalogScanFixture(t, true)
	library, run := catalogScanRun(t, service, library.ID, storage, profile)
	if err := service.db.Model(&models.MediaLibrary{}).Where("id=?", library.ID).Updates(map[string]any{"relative_root": "another-root", "dirty_generation": library.DirtyGeneration + 3}).Error; err != nil {
		t.Fatal(err)
	}
	_, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: []medialibrary.File{scanFile(entries[0])}}, true, service.catalogScanCommit)
	if !errors.Is(err, errMediaLibraryConfigurationChanged) {
		t.Fatalf("changed source accepted through rebase: %v", err)
	}
	var current models.CatalogHead
	if err := service.db.First(&current, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.Revision != 1 {
		t.Fatal("old-source enumeration changed head")
	}
	var active models.MediaLibraryScanRun
	if err := service.db.First(&active, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if active.SourceFingerprint != run.SourceFingerprint || active.Generation != run.Generation {
		t.Fatal("original enumeration evidence was rewritten")
	}
}

func TestCatalogScanNoopPartialDoesNotConsumeDeltaLayers(t *testing.T) {
	service, library, storage, profile, entries := catalogScanFixture(t, true)
	// Align both entries with the shared recognition once; the fixture's second
	// entry deliberately starts with an explicit old title.
	library, run := catalogScanRun(t, service, library.ID, storage, profile)
	input := medialibrary.Result{Files: []medialibrary.File{scanFile(entries[0]), scanFile(entries[1])}}
	if _, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, input, true, service.catalogScanCommit); err != nil {
		t.Fatal(err)
	}
	var head models.CatalogHead
	if err := service.db.First(&head, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	baselineGeneration := library.BaselineGeneration + 1
	noops := 0
	hook := func(tx *gorm.DB, p CatalogScanPublication) error {
		if !p.NoContentChange || p.Candidate != nil || p.Head.Revision != head.Revision {
			t.Fatalf("noop payload: %+v", p)
		}
		noops++
		return nil
	}
	input.Partial = true
	for range CatalogMaxDeltas + 2 {
		library, run = catalogScanRun(t, service, library.ID, storage, profile)
		published, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, input, true, hook)
		if err != nil {
			t.Fatal(err)
		}
		if published.Status != "success" || published.Added != 0 || published.Updated != 0 || published.Removed != 0 {
			t.Fatalf("noop run: %+v", published)
		}
		if published.Generation != baselineGeneration {
			t.Fatalf("no-op advanced generation: got=%d want=%d", published.Generation, baselineGeneration)
		}
	}
	var layers int64
	if err := service.db.Model(&models.CatalogHeadLayer{}).Where("library_id=?", library.ID).Count(&layers).Error; err != nil {
		t.Fatal(err)
	}
	if layers != 1 || noops != CatalogMaxDeltas+2 {
		t.Fatalf("empty deltas accumulated: %d", layers)
	}
}

func TestCatalogScanHookSeesFinalLogicalStateAndFailureRollsItBack(t *testing.T) {
	service, library, storage, profile, entries := catalogScanFixture(t, true)
	library, run := catalogScanRun(t, service, library.ID, storage, profile)
	failure := errors.New("fixture bind rollback")
	checked := false
	hook := func(tx *gorm.DB, p CatalogScanPublication) error {
		var current models.MediaLibrary
		if err := tx.First(&current, library.ID).Error; err != nil {
			return err
		}
		var stored models.MediaLibraryScanRun
		if err := tx.First(&stored, run.ID).Error; err != nil {
			return err
		}
		if current.DirtyGeneration != p.Run.Generation || current.BaselineGeneration != p.Run.Generation || stored.Status != p.Run.Status || stored.Phase != p.Run.Phase {
			t.Fatalf("hook saw old logical state: library=%+v run=%+v payload=%+v", current, stored, p)
		}
		checked = true
		return failure
	}
	_, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: []medialibrary.File{scanFile(entries[0]), scanFile(entries[1])}}, true, hook)
	if !checked || !errors.Is(err, failure) {
		t.Fatalf("hook checked=%v err=%v", checked, err)
	}
	var current models.MediaLibrary
	if err := service.db.First(&current, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	var stored models.MediaLibraryScanRun
	if err := service.db.First(&stored, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.DirtyGeneration != library.DirtyGeneration || current.BaselineGeneration != library.BaselineGeneration || stored.Status != "running" {
		t.Fatal("failed binding left logical scan committed")
	}
}
