package services

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogResourcesBoundPreparationAndLeaveRoomForDeltas(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	input := CatalogCandidateInput{LibraryID: library.ID, Kind: "base", ExpectedRevision: 1, SourceEpoch: 1, SourceFingerprint: "source-a", ConfigFingerprint: "config-a", LeaseDuration: time.Minute}
	first, firstToken, err := s.BeginCandidate(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginCandidate(context.Background(), input); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("same library accepted parallel bases: %v", err)
	}
	for i := 0; i < 2; i++ {
		copy := library
		copy.ID, copy.Name = 0, "resource-test-"+strconv.Itoa(i)
		copy.NameNormalized = copy.Name
		if err := s.writeDB.Create(&copy).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
			return StartCatalogConversionTx(tx, models.CatalogHead{LibraryID: copy.ID, SourceEpoch: 1, SourceFingerprint: "source-a", ConfigFingerprint: "config-a"}, func(*gorm.DB) error { return nil })
		}); err != nil {
			t.Fatal(err)
		}
		other := input
		other.LibraryID, other.ExpectedRevision = copy.ID, 0
		_, _, err := s.BeginCandidate(context.Background(), other)
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && !errors.Is(err, ErrCatalogBudget) {
			t.Fatalf("global base cap: %v", err)
		}
	}
	input.Kind = "delta"
	for i := 2; i < CatalogMaxPreparations; i++ {
		if _, _, err := s.BeginCandidate(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.BeginCandidate(context.Background(), input); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("global cap: %v", err)
	}
	if err := s.Abandon(context.Background(), first.ID, firstToken); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BeginCandidate(context.Background(), input); err != nil {
		t.Fatalf("abandon did not release capacity: %v", err)
	}
}

func TestCatalogResourcesDiskPressureKeepsHeadAndAllowsCleanup(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	candidate, token := catalogCandidate(t, s, library, "delta", 1)
	s.spaceAvailable = func(context.Context) (uint64, error) { return 1, nil }
	if err := s.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("space gate: %v", err)
	}
	if err := s.Abandon(context.Background(), candidate.ID, token); err != nil {
		t.Fatalf("cleanup blocked by pressure: %v", err)
	}
	if len(catalogReadEntries(t, s, library.ID, "")) != len(entries) {
		t.Fatal("pressure hid current head")
	}
	if _, err := s.Maintain(context.Background(), 4); err != nil {
		t.Fatalf("GC blocked by pressure: %v", err)
	}
	s.spaceAvailable = func(context.Context) (uint64, error) { return math.MaxUint64, errors.New("unknown") }
	if err := s.checkCatalogSpace(context.Background(), 1); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("unknown capacity accepted: %v", err)
	}
	if _, err := catalogRequiredFreeBytes(math.MaxInt64); !errors.Is(err, ErrCatalogBudget) {
		t.Fatal("overflow accepted")
	}
}

func TestCatalogMaintenanceRechecksJobAndNeverExpiresOtherReferences(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	base := catalogConvert(t, s, library, rec, entries)
	expires := time.Now().UTC().Add(time.Minute)
	job := models.Job{ID: "maintenance-owner", CreatedByKind: "system", JobType: "fake", Status: "running", LeaseTokenHash: "lease-hash", LeaseExpiresAt: &expires, Generation: 1, PayloadJSON: "{}"}
	if err := s.writeDB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	candidate, _, err := s.BeginCandidate(context.Background(), CatalogCandidateInput{LibraryID: library.ID, Kind: "base", ExpectedRevision: 1, SourceEpoch: 1, SourceFingerprint: "source-a", ConfigFingerprint: "config-a", JobID: &job.ID, JobLeaseHash: job.LeaseTokenHash, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		if err := AcquireCatalogReferenceTx(tx, base.ID, "compaction", candidate.ID+":0"); err != nil {
			return err
		}
		if err := AcquireCatalogReferenceTx(tx, base.ID, "diagnosis", "durable-diagnosis"); err != nil {
			return err
		}
		return tx.Model(&candidate).Update("lease_expires_at", time.Now().UTC().Add(-time.Hour)).Error
	}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Maintain(context.Background(), 1)
	if err != nil || result.Abandoned != 0 {
		t.Fatalf("live job lost candidate: %+v %v", result, err)
	}
	if err := s.writeDB.Model(&job).Update("lease_token_hash", "new-owner").Error; err != nil {
		t.Fatal(err)
	}
	result, err = s.Maintain(context.Background(), 1)
	if err != nil || result.Abandoned != 1 || result.Batches != 1 {
		t.Fatalf("revoked owner not recovered: %+v %v", result, err)
	}
	var refs []models.CatalogSnapshotReference
	if err := s.writeDB.Where("snapshot_id=?", base.ID).Find(&refs).Error; err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].OwnerKind != "diagnosis" {
		t.Fatalf("reference ownership lost: %+v", refs)
	}
	if _, err := s.Maintain(context.Background(), 17); !errors.Is(err, ErrCatalogBudget) {
		t.Fatal("unbounded maintenance accepted")
	}
}
