package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/updater"
	"gorm.io/gorm"
)

func catalogConversionFixture(t *testing.T) (*CatalogSnapshotStore, catalogConversionInput, models.MediaLibrary, models.MediaLibraryRecognition, []models.MediaLibraryEntry) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "conversion.db")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	compatibility, err := updater.CheckStartupCompatibility(path, directory, executable, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := compatibility.ReserveFreshDatabase(); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := compatibility.CompleteInitialization(); err != nil {
		t.Fatal(err)
	}
	reader, err := database.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlReader, _ := reader.DB()
	t.Cleanup(func() { _ = sqlReader.Close() })
	store := NewCatalogSnapshotStore(db, reader)
	storage := models.Storage{Name: "conversion", NameNormalized: "conversion", Type: models.StorageTypeLocal, RootPath: directory, RootPathNormalized: directory, Enabled: true, Capabilities: "{}"}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := db.Where("code=?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "conversion", NameNormalized: "conversion", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, BaselineGeneration: 7, DirtyGeneration: 8, ArtifactGeneration: 6, RelativeRoot: "/"}
	if err := db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	tmdb := int64(42)
	rec := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "show", ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: "matched", MediaType: "tv", Title: "Show", TMDBID: &tmdb, MetadataJSON: `{"title":"private metadata must affect proof"}`, ManualOverride: true, LastGeneration: 7}
	if err := db.Create(&rec).Error; err != nil {
		t.Fatal(err)
	}
	season, episode := 2, 1
	entries := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RecognitionID: &rec.ID, RelativePath: "Show/Season 02/01.h264.mkv", ProviderID: "private-provider-a", MediaType: "tv", Title: "Show", SeriesTitle: "Show", WorkKey: "series:tmdb:42", Season: &season, Episode: &episode, TMDBID: &tmdb, MatchStatus: "matched", LastGeneration: 7},
		{LibraryID: library.ID, RecognitionID: &rec.ID, RelativePath: "Show/Season 02/01.h265.mkv", ProviderID: "private-provider-b", MediaType: "tv", Title: "File override", SeriesTitle: "Show", WorkKey: "series:tmdb:42", Season: &season, Episode: &episode, TMDBID: &tmdb, MatchStatus: "matched", LastGeneration: 7},
	}
	if err := db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Minute)
	lease := "first-conversion-lease"
	job := models.Job{ID: "catalog-converter-job", CreatedByKind: "system", JobType: catalogConversionJobType, ResourceKey: mediaArtifactResourceKey(library.ID), Status: models.JobStatusRunning, LeaseTokenHash: leaseHash(lease), LeaseExpiresAt: &expires, Generation: 1, PayloadJSON: fmt.Sprintf(`{"version":1,"library_id":%d}`, library.ID)}
	if err := db.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	input := catalogConversionInput{LibraryID: library.ID, Job: ClaimedJob{Job: job, LeaseToken: lease}, Compatibility: compatibility, Guard: func(*gorm.DB, uint) error { return nil }}
	return store, input, library, rec, entries
}

func TestCatalogConversionPreservesRawIdentityManualOverridesAndSemanticState(t *testing.T) {
	s, input, library, rec, entries := catalogConversionFixture(t)
	var before models.MediaLibrary
	if err := s.writeDB.First(&before, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	head, err := s.convertLegacyCatalog(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if head.Mode != "versioned" || head.Revision != 1 || head.SourceEpoch != 1 {
		t.Fatalf("head=%+v", head)
	}
	var after models.MediaLibrary
	if err := s.writeDB.First(&after, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("conversion changed logical scan/artifact/diagnosis state")
	}
	rows := catalogReadEntries(t, s, library.ID, "")
	if len(rows) != 2 || rows[0].ID != entries[0].ID || rows[1].Title != "File override" || *rows[0].Season != 2 || *rows[0].Episode != 1 {
		t.Fatalf("converted facts=%+v", rows)
	}
	var original models.MediaLibraryRecognition
	if err := s.writeDB.First(&original, rec.ID).Error; err != nil {
		t.Fatal(err)
	}
	if original.MetadataJSON != rec.MetadataJSON || !original.ManualOverride {
		t.Fatal("legacy anchor was rewritten")
	}
	format, err := database.ReadCatalogFormat(context.Background(), s.readDB)
	if err != nil || format != CatalogFormat {
		t.Fatalf("format=%d %v", format, err)
	}
	if err := input.Compatibility.ValidateDatabaseFormat(format); err != nil {
		t.Fatal(err)
	}
	var manifest models.CatalogConversionManifest
	if err := s.writeDB.First(&manifest).Error; err != nil {
		t.Fatal(err)
	}
	if manifest.State != "verified" || manifest.InputRows != 3 || len(manifest.InputDigest) != 64 {
		t.Fatalf("manifest=%+v", manifest)
	}
}

func TestCatalogConversionSuccessorResumesOnlyCommittedFactPrefix(t *testing.T) {
	s, input, library, rec, entries := catalogConversionFixture(t)
	candidate, token, err := s.startCatalogConversion(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.captureCatalogConversionInput(context.Background(), candidate, token, input); err != nil {
		t.Fatal(err)
	}
	requests := []CatalogIdentityRequest{{Kind: "recognition", SourceKey: rec.SourceKey, ExistingID: rec.ID}, {Kind: "entry", SourceKey: entries[0].RelativePath, ExistingID: entries[0].ID}}
	if _, err := s.ResolveIdentities(context.Background(), candidate.ID, token, requests); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	if len(catalogReadEntries(t, s, library.ID, "")) != 2 {
		t.Fatal("converting stopped legacy reads")
	}
	if _, _, err := s.startCatalogConversion(context.Background(), input); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("live owner stolen: %v", err)
	}
	input.Job.LeaseToken = "successor-conversion-lease"
	if err := s.writeDB.Model(&models.Job{}).Where("id=?", input.Job.Job.ID).Update("lease_token_hash", leaseHash(input.Job.LeaseToken)).Error; err != nil {
		t.Fatal(err)
	}
	head, err := s.convertLegacyCatalog(context.Background(), input)
	if err != nil || head.Mode != "versioned" {
		t.Fatalf("resume=%+v %v", head, err)
	}
	var facts int64
	if err := s.writeDB.Model(&models.CatalogRecognitionFact{}).Where("snapshot_id=?", candidate.ID).Count(&facts).Error; err != nil {
		t.Fatal(err)
	}
	if facts != 1 {
		t.Fatal("resume duplicated committed recognition prefix")
	}
	if err := s.RenewCandidate(context.Background(), candidate.ID, token, time.Minute); !errors.Is(err, ErrCatalogFence) {
		t.Fatal("revoked token survived")
	}
}

func TestCatalogConversionProofDetectsPrivateLegacyMutationAndRejectsWrongJob(t *testing.T) {
	s, input, library, _, entries := catalogConversionFixture(t)
	candidate, token, err := s.startCatalogConversion(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.captureCatalogConversionInput(context.Background(), candidate, token, input); err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Model(&models.MediaLibraryEntry{}).Where("id=?", entries[0].ID).Update("provider_id", "changed-private-source").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.copyCatalogConversionFacts(context.Background(), candidate, token, input); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyCatalogConversion(context.Background(), candidate, token, input); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("private field not hashed: %v", err)
	}
	var head models.CatalogHead
	if err := s.writeDB.First(&head, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if head.Mode != "converting" {
		t.Fatal("failed proof published")
	}
	other := input.Job.Job
	other.ID = "unrelated-valid-job"
	if err := s.writeDB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	input.Job.Job = other
	if _, _, err := s.startCatalogConversion(context.Background(), input); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("unrelated job stole candidate: %v", err)
	}
}

func TestCatalogConversionRequiresRealDatabaseBoundCompatibilityAndDrainGuard(t *testing.T) {
	s, input, _, _, _ := catalogConversionFixture(t)
	denied := errors.New("writers not drained")
	input.Guard = func(*gorm.DB, uint) error { return denied }
	if _, err := s.convertLegacyCatalog(context.Background(), input); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	var heads int64
	if err := s.writeDB.Model(&models.CatalogHead{}).Count(&heads).Error; err != nil {
		t.Fatal(err)
	}
	if heads != 0 {
		t.Fatal("guard failure installed conversion fence")
	}
	input.Guard = func(*gorm.DB, uint) error { return nil }
	input.Compatibility = nil
	if _, err := s.convertLegacyCatalog(context.Background(), input); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatal("nil capability accepted")
	}
	other, otherInput, _, _, _ := catalogConversionFixture(t)
	input.Compatibility = otherInput.Compatibility
	if _, err := s.convertLegacyCatalog(context.Background(), input); err == nil {
		t.Fatal("other database capability accepted")
	}
	var otherHeads int64
	if err := other.writeDB.Model(&models.CatalogHead{}).Count(&otherHeads).Error; err != nil {
		t.Fatal(err)
	}
	if otherHeads != 0 {
		t.Fatal("wrong database touched")
	}
}

func TestCatalogConversionPublishedReceiptSurvivesLostACK(t *testing.T) {
	s, input, library, _, _ := catalogConversionFixture(t)
	first, err := s.convertLegacyCatalog(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.Job.LeaseToken = "receipt-successor"
	if err := s.writeDB.Model(&models.Job{}).Where("id=?", input.Job.Job.ID).Update("lease_token_hash", leaseHash(input.Job.LeaseToken)).Error; err != nil {
		t.Fatal(err)
	}
	replayed, err := s.convertLegacyCatalog(context.Background(), input)
	if err != nil || !catalogSameHead(first, replayed) {
		t.Fatalf("lost ACK repeated conversion: %+v %v", replayed, err)
	}
	var count int64
	if err := s.writeDB.Model(&models.CatalogSnapshot{}).Where("library_id=?", library.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("new candidate on receipt replay: %d %v", count, err)
	}
	input.Guard = func(*gorm.DB, uint) error { return ErrCatalogFence }
	if _, err := s.convertLegacyCatalog(context.Background(), input); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("receipt bypassed authority guard: %v", err)
	}
	input.Guard = func(*gorm.DB, uint) error { return nil }
	if err := s.writeDB.Model(&models.CatalogHead{}).Where("library_id=?", library.ID).Update("source_epoch", 2).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.convertLegacyCatalog(context.Background(), input); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("receipt accepted another source lifetime: %v", err)
	}
}

func TestCatalogConversionRevokedRecoveryRebuildsNewConfigWithoutChangingLegacy(t *testing.T) {
	s, input, library, _, _ := catalogConversionFixture(t)
	// Compare persisted facts before/after recovery, not GORM's in-memory Create
	// values: SQLite round trips may represent the same instant in another zone.
	var entries []models.MediaLibraryEntry
	if err := s.writeDB.Where("library_id=?", library.ID).Order("id").Find(&entries).Error; err != nil {
		t.Fatal(err)
	}
	candidate, token, err := s.startCatalogConversion(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.captureCatalogConversionInput(context.Background(), candidate, token, input); err != nil {
		t.Fatal(err)
	}
	var originalManifest models.CatalogConversionManifest
	if err := s.writeDB.First(&originalManifest, "snapshot_id=?", candidate.ID).Error; err != nil {
		t.Fatal(err)
	}
	recovery := catalogConversionRecoveryInput{LibraryID: library.ID, JobID: input.Job.Job.ID, Guard: input.Guard}
	if err := s.abortCatalogConversion(context.Background(), recovery); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("live owner was abandoned: %v", err)
	}
	if err := s.writeDB.Model(&models.MediaClassificationProfile{}).Where("id=?", library.ProfileID).Update("revision", gorm.Expr("revision+1")).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.captureCatalogConversionInput(context.Background(), candidate, token, input); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("changed profile reused old manifest: %v", err)
	}
	if err := s.writeDB.Model(&models.Job{}).Where("id=?", input.Job.Job.ID).Update("cancellation_asked", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Model(&models.CatalogSnapshot{}).Where("id=?", candidate.ID).Update("lease_expires_at", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Maintain(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	var retained models.CatalogSnapshot
	if err := s.writeDB.First(&retained, "id=?", candidate.ID).Error; err != nil || retained.State != "building" {
		t.Fatalf("maintenance destroyed recovery proof: %+v %v", retained, err)
	}
	recovery.Guard = func(*gorm.DB, uint) error { return ErrCatalogFence }
	if err := s.abortCatalogConversion(context.Background(), recovery); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("recovery ignored drain: %v", err)
	}
	recovery.Guard = input.Guard
	if err := s.abortCatalogConversion(context.Background(), recovery); err != nil {
		t.Fatal(err)
	}
	var legacy []models.MediaLibraryEntry
	if err := s.writeDB.Where("library_id=?", library.ID).Order("id").Find(&legacy).Error; err != nil || !reflect.DeepEqual(entries, legacy) {
		t.Fatalf("abort mutated legacy facts: %+v %v", legacy, err)
	}
	var oldManifest models.CatalogConversionManifest
	if err := s.writeDB.First(&oldManifest, "snapshot_id=?", candidate.ID).Error; err != nil || !reflect.DeepEqual(originalManifest, oldManifest) {
		t.Fatalf("recovery laundered old proof: %+v %v", oldManifest, err)
	}
	if _, _, err := s.startCatalogConversion(context.Background(), input); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("canceled lease rebuilt: %v", err)
	}
	input.Job.LeaseToken = "rebuild-new-config"
	expires := time.Now().Add(time.Minute)
	if err := s.writeDB.Model(&models.Job{}).Where("id=?", input.Job.Job.ID).Updates(map[string]any{"cancellation_asked": false, "lease_token_hash": leaseHash(input.Job.LeaseToken), "lease_expires_at": expires}).Error; err != nil {
		t.Fatal(err)
	}
	head, err := s.convertLegacyCatalog(context.Background(), input)
	if err != nil || head.Mode != "versioned" || head.SourceEpoch != 2 {
		t.Fatalf("new-config rebuild=%+v %v", head, err)
	}
	if err := s.abortCatalogConversion(context.Background(), recovery); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("recovery rolled back published head: %v", err)
	}
}

func TestCatalogConversionFinalGuardRollsBackHeadAndReceiptTogether(t *testing.T) {
	s, input, library, _, _ := catalogConversionFixture(t)
	input.Guard = func(tx *gorm.DB, id uint) error {
		var ready int64
		if err := tx.Model(&models.CatalogSnapshot{}).Where("library_id=? AND state='ready'", id).Count(&ready).Error; err != nil {
			return err
		}
		if ready > 0 {
			return ErrCatalogFence
		}
		return nil
	}
	if _, err := s.convertLegacyCatalog(context.Background(), input); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("final guard ignored: %v", err)
	}
	var head models.CatalogHead
	var job models.Job
	if err := s.writeDB.First(&head, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.First(&job, "id=?", input.Job.Job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if head.Mode != "converting" || head.Revision != 0 || job.CheckpointJSON != "{}" {
		t.Fatalf("failed publication leaked head/receipt: %+v checkpoint=%s", head, job.CheckpointJSON)
	}
}
