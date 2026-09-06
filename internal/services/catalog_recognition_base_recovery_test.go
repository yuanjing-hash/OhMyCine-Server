package services

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type recognitionBaseRecoveryFixture struct {
	service  *MediaLibraryService
	library  models.MediaLibrary
	ready    models.MediaLibraryScanRun
	claim    ClaimedJob
	payload  mediaLibraryRecognitionJobPayload
	head     models.CatalogHead
	entries  []models.MediaLibraryEntry
	original []models.MediaLibraryEntry
}

func newRecognitionBaseRecoveryFixture(t *testing.T) recognitionBaseRecoveryFixture {
	t.Helper()
	s, library, storage, profile, original := catalogScanFixture(t, true)
	library, run := catalogScanRun(t, s, library.ID, storage, profile)
	files := make([]medialibrary.File, 1004)
	for i := range files {
		files[i] = medialibrary.File{RelativePath: fmt.Sprintf("Recovery show/Season 02/S02E%03d.variant%d.mkv", i/4+1, i%4), ProviderID: fmt.Sprintf("recovery-file-%d", i), ProviderIDStable: true}
	}
	for _, row := range original {
		files = append(files, scanFile(row))
	}
	ready, err := s.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: files}, true, s.catalogScanCommit)
	if err != nil || ready.Status != "catalog_ready" {
		t.Fatalf("pending fixture %+v %v", ready, err)
	}
	var head models.CatalogHead
	if err := s.db.First(&head, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	return recognitionBaseRecoveryFixture{service: s, library: library, ready: ready, claim: catalogRecognitionJob(t, s), payload: mediaLibraryRecognitionJobPayload{LibraryID: library.ID, ScanRunID: ready.ID, Generation: ready.Generation}, head: head, entries: catalogReadEntries(t, s.catalogStore, library.ID, ""), original: original}
}

func (f recognitionBaseRecoveryFixture) assertUnpublished(t *testing.T) {
	t.Helper()
	var head models.CatalogHead
	if err := f.service.db.First(&head, "library_id=?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !catalogSameHead(head, f.head) {
		t.Fatalf("failure changed visible head: %+v -> %+v", f.head, head)
	}
	if rows := catalogReadEntries(t, f.service.catalogStore, f.library.ID, ""); !reflect.DeepEqual(rows, f.entries) {
		t.Fatal("failed preparation/publication exposed partial entries")
	}
	var run models.MediaLibraryScanRun
	if err := f.service.db.First(&run, f.ready.ID).Error; err != nil || run.Status != "catalog_ready" {
		t.Fatalf("failed work reported complete %+v %v", run, err)
	}
}

func (f recognitionBaseRecoveryFixture) complete(t *testing.T, claim ClaimedJob) {
	t.Helper()
	if err := f.service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, f.payload, claim); err != nil {
		t.Fatal(err)
	}
	rows := catalogReadEntries(t, f.service.catalogStore, f.library.ID, "")
	if len(rows) != len(f.entries) {
		t.Fatal("retry lost files")
	}
	for i, row := range rows {
		if row.ID != f.entries[i].ID || row.RecognitionID == nil || row.MatchStatus == mediaRecognitionStatusPending {
			t.Fatalf("retry lost identity/association at %d", i)
		}
	}
	var head models.CatalogHead
	if err := f.service.db.First(&head, "library_id=?", f.library.ID).Error; err != nil {
		t.Fatal(err)
	}
	var layers []models.CatalogHeadLayer
	if err := f.service.db.Where("library_id=?", f.library.ID).Find(&layers).Error; err != nil || len(layers) != 1 {
		t.Fatalf("retry did not publish one base: %v %v", layers, err)
	}
	var base models.CatalogSnapshot
	if err := f.service.db.First(&base, "id=?", layers[0].SnapshotID).Error; err != nil || base.Kind != "base" || base.State != "published" {
		t.Fatalf("wrong base %+v %v", base, err)
	}
	var run models.MediaLibraryScanRun
	if err := f.service.db.First(&run, f.ready.ID).Error; err != nil || run.Status != "success" {
		t.Fatalf("retry not complete %+v %v", run, err)
	}
}

type recognitionBaseHeartbeatHook struct {
	fastScanTestRuntime
	calls   int
	onThird func() error
}

func (r *recognitionBaseHeartbeatHook) Heartbeat(*float64, *int64, *int64, *float64, *int64) error {
	r.calls++
	if r.calls == 3 {
		return r.onThird()
	}
	return nil
}

func TestCatalogRecognitionBaseRecoveryPreservesNewerManualHead(t *testing.T) {
	f := newRecognitionBaseRecoveryFixture(t)
	runtime := &recognitionBaseHeartbeatHook{onThird: func() error {
		baseline, err := f.service.loadCatalogScanBaseline(context.Background(), f.library.ID)
		if err != nil {
			return err
		}
		manual := baseline.recognitions[0]
		manual.Title, manual.ManualOverride = "Manual won head race", true
		return f.service.publishCatalogRecognitionDelta(context.Background(), baseline.head, []models.MediaLibraryRecognition{manual}, func(*gorm.DB, *CatalogReader) error { return nil }, func(tx *gorm.DB) error {
			return tx.Model(&models.MediaLibrary{}).Where("id=?", f.library.ID).Update("dirty_generation", f.ready.Generation+1).Error
		})
	}}
	commits := 0
	f.service.SetCatalogScanCommit(func(_ *gorm.DB, p CatalogScanPublication) error {
		if p.Candidate != nil {
			commits++
			if p.Candidate.Kind != "base" {
				t.Fatal("large recovery fragmented to delta")
			}
		}
		return nil
	})
	if err := f.service.completeFastMediaLibraryRecognition(context.Background(), runtime, f.payload, f.claim); err != nil {
		t.Fatal(err)
	}
	if runtime.calls < 6 || commits != 1 {
		t.Fatalf("no fresh-head retry: heartbeats=%d commits=%d", runtime.calls, commits)
	}
	for _, row := range catalogReadEntries(t, f.service.catalogStore, f.library.ID, "") {
		if row.ID == f.original[0].ID && row.Title != "Manual won head race" {
			t.Fatalf("manual overwritten %+v", row)
		}
		if row.RecognitionID == nil || row.MatchStatus == mediaRecognitionStatusPending {
			t.Fatal("pending association not completed")
		}
	}
	f.complete(t, f.claim)
}

func TestCatalogRecognitionBaseRecoveryPreparationFailureAndCancel(t *testing.T) {
	for _, kind := range []string{"write_failure", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := newRecognitionBaseRecoveryFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := false
			callback := "test:recognition-base-preparation"
			if err := f.service.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table != "catalog_entry_facts" || injected {
					return
				}
				injected = true
				if kind == "cancel" {
					cancel()
					_ = tx.AddError(context.Canceled)
				} else {
					_ = tx.AddError(errors.New("recognition preparation fault"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.service.db.Callback().Create().Remove(callback) })
			err := f.service.completeFastMediaLibraryRecognition(ctx, fastScanTestRuntime{}, f.payload, f.claim)
			if !injected || err == nil {
				t.Fatalf("missing preparation failure: %v", err)
			}
			if kind == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation changed %v", err)
			}
			if kind == "write_failure" && !strings.Contains(err.Error(), "recognition preparation fault") {
				t.Fatal(err)
			}
			f.assertUnpublished(t)
			if err := f.service.db.Callback().Create().Remove(callback); err != nil {
				t.Fatal(err)
			}
			f.complete(t, f.claim)
		})
	}
}

func TestCatalogRecognitionBaseRecoveryPublicationFailureAndCancel(t *testing.T) {
	for _, kind := range []string{"hook_failure", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := newRecognitionBaseRecoveryFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			injected := false
			f.service.SetCatalogScanCommit(func(tx *gorm.DB, p CatalogScanPublication) error {
				if p.Candidate == nil {
					return nil
				}
				injected = true
				if p.Candidate.Kind != "base" {
					t.Fatal("expected single prepared base")
				}
				reader, err := PinCatalogTx(tx, []uint{f.library.ID})
				if err != nil {
					return err
				}
				if n, err := reader.EntryCount(); err != nil || n != int64(len(f.entries)) {
					t.Fatal("hook did not observe entire tentative base")
				}
				if kind == "cancel" {
					cancel()
					return context.Canceled
				}
				return errors.New("recognition publication fault")
			})
			err := f.service.completeFastMediaLibraryRecognition(ctx, fastScanTestRuntime{}, f.payload, f.claim)
			if !injected || err == nil {
				t.Fatalf("missing publication failure: %v", err)
			}
			f.assertUnpublished(t)
			f.service.SetCatalogScanCommit(func(*gorm.DB, CatalogScanPublication) error { return nil })
			f.complete(t, f.claim)
		})
	}
}

func TestCatalogRecognitionBaseRecoveryRevokedJobNeedsMaintenanceBeforeNewClaim(t *testing.T) {
	f := newRecognitionBaseRecoveryFixture(t)
	// A process crash may prevent the best-effort abandon from committing.
	// Keep that durable checkpoint using a test-only failed cleanup writer.
	if err := f.service.db.Exec(`CREATE TRIGGER recognition_crash_before_abandon BEFORE UPDATE OF state ON catalog_snapshots WHEN NEW.state='abandoned' AND OLD.job_id='catalog-background-recognition' BEGIN SELECT RAISE(ABORT,'simulated process exit before cleanup commit'); END`).Error; err != nil {
		t.Fatal(err)
	}
	next := f.claim
	next.LeaseToken = "replacement-recognition-lease"
	injected := false
	callback := "test:recognition-base-lease-revoke"
	if err := f.service.db.Callback().Create().After("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "catalog_entry_facts" || injected {
			return
		}
		injected = true
		// Commit the queue authority change together with a real partial batch.
		// Subsequent appends and abandonment by the old claim must be refused.
		if err := tx.Session(&gorm.Session{NewDB: true}).Model(&models.Job{}).Where("id=?", f.claim.Job.ID).Update("lease_token_hash", leaseHash(next.LeaseToken)).Error; err != nil {
			_ = tx.AddError(err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.service.db.Callback().Create().Remove(callback) })
	err := f.service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, f.payload, f.claim)
	if !injected || !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("old owner was not fenced %v", err)
	}
	if err := f.service.db.Callback().Create().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if err := f.service.db.Exec("DROP TRIGGER recognition_crash_before_abandon").Error; err != nil {
		t.Fatal(err)
	}
	f.assertUnpublished(t)
	var candidate models.CatalogSnapshot
	if err := f.service.db.Where("job_id=? AND state IN ?", f.claim.Job.ID, []string{"building", "validating", "ready"}).First(&candidate).Error; err != nil || candidate.Kind != "base" {
		t.Fatalf("old base evidence lost %+v %v", candidate, err)
	}
	if err := f.service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, f.payload, next); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("new claim bypassed old base slot %v", err)
	}
	maintained, err := f.service.catalogStore.Maintain(context.Background(), 1)
	if err != nil || maintained.Abandoned != 0 {
		t.Fatalf("unexpired candidate reclaimed %+v %v", maintained, err)
	}
	if err := f.service.db.Model(&candidate).Update("lease_expires_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	maintained, err = f.service.catalogStore.Maintain(context.Background(), 1)
	if err != nil || maintained.Abandoned != 1 {
		t.Fatalf("revoked candidate not reclaimed %+v %v", maintained, err)
	}
	f.assertUnpublished(t)
	if err := f.service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, f.payload, f.claim); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("old claim recovered authority %v", err)
	}
	f.complete(t, next)
}
