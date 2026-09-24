package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogSourceActualLibraryUpdateRetainsAnchorsAndRecordsRemoval(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	storage, profile := catalogSourceContext(t, s, library)
	if err := os.Mkdir(filepath.Join(storage.RootPath, "replacement"), 0700); err != nil {
		t.Fatal(err)
	}
	var user models.User
	if err := s.writeDB.Where("username_normalized=?", "library-test").First(&user).Error; err != nil {
		t.Fatal(err)
	}
	actor := Actor{User: user, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesUpdate: {}, authz.PermissionMediaLibrariesRead: {}}}
	service := NewMediaLibraryService(s.writeDB, NewAuditService(s.writeDB), zerolog.Nop())
	service.SetCatalogSnapshotStore(s)
	service.SetMediaChangeService(NewMediaChangeService(s.writeDB))
	input := testLibraryInput(library.Name, storage, profile, false)
	input.RelativeRoot = "/replacement"
	updated, err := service.Update(context.Background(), actor, library.ID, input, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if updated.BaselineGeneration != 0 || updated.DirtyGeneration != library.DirtyGeneration+1 {
		t.Fatal("source counters not replaced")
	}
	var persisted models.MediaLibrary
	if err := s.writeDB.First(&persisted, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.ExclusionEpoch != library.ExclusionEpoch+1 {
		t.Fatalf("source exclusion epoch=%d want=%d", persisted.ExclusionEpoch, library.ExclusionEpoch+1)
	}
	var count int64
	if err := s.writeDB.Model(&models.MediaLibraryEntry{}).Where("library_id=?", library.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 || len(catalogReadEntries(t, s, library.ID, "")) != 0 {
		t.Fatal("Update deleted anchors or kept old live projection")
	}
	var change models.MediaLibraryChange
	if err := s.writeDB.Where("library_id=?", library.ID).First(&change).Error; err != nil {
		t.Fatal(err)
	}
	if change.Kind != models.MediaLibraryChangeRemoval || change.State != models.MediaLibraryChangeReady {
		t.Fatal("missing ready removal")
	}
}

func TestCatalogConfigProfileRefreshFencesCandidatesWithoutReplacingSource(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	_, profile := catalogSourceContext(t, s, library)
	active, token := catalogCandidate(t, s, library, "base", 1)
	if err := s.writeDB.Model(&profile).Update("revision", profile.Revision+1).Error; err != nil {
		t.Fatal(err)
	}
	service := NewMediaLibraryService(s.writeDB, NewAuditService(s.writeDB), zerolog.Nop())
	service.SetCatalogSnapshotStore(s)
	if err := service.ProfileRevisionChanged(profile.ID, profile.Revision); err != nil {
		t.Fatal(err)
	}
	var head models.CatalogHead
	if err := s.writeDB.First(&head, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if head.Revision != 2 || head.SourceEpoch != 1 || len(catalogReadEntries(t, s, library.ID, "")) != 2 {
		t.Fatal("profile invalidation changed physical catalog")
	}
	if err := s.RenewCandidate(context.Background(), active.ID, token, time.Minute); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("old profile candidate survived: %v", err)
	}
	if err := service.ProfileRevisionChanged(profile.ID, profile.Revision); err != nil {
		t.Fatal(err)
	}
	var again models.CatalogHead
	if err := s.writeDB.First(&again, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if again.Revision != head.Revision {
		t.Fatal("same profile refresh consumed physical revisions")
	}
}

func catalogSourceContext(t *testing.T, s *CatalogSnapshotStore, library models.MediaLibrary) (models.Storage, models.MediaClassificationProfile) {
	t.Helper()
	var storage models.Storage
	var profile models.MediaClassificationProfile
	if err := s.writeDB.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	return storage, profile
}

func TestCatalogSourceResetEmptyHeadPreservesReferencesAndOldIdentityCannotRebind(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	base := catalogConvert(t, s, library, rec, entries)
	storage, profile := catalogSourceContext(t, s, library)
	active, token := catalogCandidate(t, s, library, "base", 1)
	next := library
	next.RelativeRoot = "new-root"
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		if err := AcquireCatalogReferenceTx(tx, base.ID, "diagnosis", "keep-frozen"); err != nil {
			return err
		}
		versioned, err := applyCatalogLibraryChangeTx(tx, library, &next, storage, storage, profile)
		if !versioned && err == nil {
			return ErrCatalogInvalid
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rows := catalogReadEntries(t, s, library.ID, "id=?", entries[0].ID)
	if len(rows) != 0 {
		t.Fatal("old entry token resolves under new source")
	}
	if err := s.AppendBatch(context.Background(), active.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("old worker still writes: %v", err)
	}
	var old models.MediaLibraryEntry
	if err := s.writeDB.First(&old, entries[0].ID).Error; err != nil {
		t.Fatal("stable anchor was deleted", err)
	}
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error { return MarkCatalogGCTx(tx, base.ID) }); !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("referenced old base was collectable: %v", err)
	}
	var head models.CatalogHead
	if err := s.writeDB.First(&head, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if head.SourceEpoch != 2 || head.Revision != 2 || next.BaselineGeneration != 0 || next.DirtyGeneration != library.DirtyGeneration+1 {
		t.Fatalf("source lifetime not advanced: %+v %+v", head, next)
	}
	candidate, ct, err := s.BeginCandidate(context.Background(), CatalogCandidateInput{LibraryID: library.ID, Kind: "base", ExpectedRevision: head.Revision, SourceEpoch: head.SourceEpoch, SourceFingerprint: head.SourceFingerprint, ConfigFingerprint: head.ConfigFingerprint, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveIdentities(context.Background(), candidate.ID, ct, []CatalogIdentityRequest{{Kind: "entry", SourceKey: entries[0].RelativePath, ExistingID: entries[0].ID}}); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("old identity rebound: %v", err)
	}
	ids, err := s.ResolveIdentities(context.Background(), candidate.ID, ct, []CatalogIdentityRequest{{Kind: "entry", SourceKey: entries[0].RelativePath}})
	if err != nil || len(ids) != 1 || ids[0] == entries[0].ID {
		t.Fatalf("new source identity: %v %v", ids, err)
	}
}

func TestCatalogConfigFencePreservesPublishedDataAndSourceResetRollsBack(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	storage, profile := catalogSourceContext(t, s, library)
	initial := catalogConfigFingerprint(library, storage, profile)
	logical := library
	logical.DirtyGeneration, logical.BaselineGeneration, logical.ArtifactGeneration, logical.ProfileRevision = 88, 77, 66, 55
	if catalogConfigFingerprint(logical, storage, profile) != initial {
		t.Fatal("logical cursors polluted config authority")
	}
	next := library
	next.MetadataLanguage = "ja-JP"
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		_, err := applyCatalogLibraryChangeTx(tx, library, &next, storage, storage, profile)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(catalogReadEntries(t, s, library.ID, "")) != 2 {
		t.Fatal("config change hid old published data")
	}
	var head models.CatalogHead
	if err := s.writeDB.First(&head, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if head.Revision != 2 || head.SourceEpoch != 1 {
		t.Fatal("config-only change replaced physical source")
	}
	next.RelativeRoot = "changed"
	sentinel := errors.New("after-head-hook-failure")
	err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		if _, err := applyCatalogLibraryChangeTx(tx, library, &next, storage, storage, profile); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if len(catalogReadEntries(t, s, library.ID, "")) != 2 {
		t.Fatal("rollback did not restore old head")
	}
	var current models.CatalogHead
	if err := s.writeDB.First(&current, "library_id=?", library.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current != head {
		t.Fatal("rollback leaked source/config revision")
	}
}

func TestCatalogSourceStorageBudgetRejectsBeforeAnyChangeAndProbeCannotUndoRoot(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	storage, _ := catalogSourceContext(t, s, library)
	for i := 1; i <= catalogStorageMutationLibraries; i++ {
		copy := library
		copy.ID, copy.Name = 0, "storage-budget-"+strconv.Itoa(i)
		copy.NameNormalized = copy.Name
		if err := s.writeDB.Create(&copy).Error; err != nil {
			t.Fatal(err)
		}
		if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
			return StartCatalogConversionTx(tx, models.CatalogHead{LibraryID: copy.ID, SourceEpoch: 1, SourceFingerprint: "old", ConfigFingerprint: "config"}, func(*gorm.DB) error { return nil })
		}); err != nil {
			t.Fatal(err)
		}
	}
	next := storage
	next.RootPath, next.RootPathNormalized = "new-root", "new-root"
	err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		if err := applyCatalogStorageChangeTx(tx, storage, next, NewMediaChangeService(s.writeDB)); err != nil {
			return err
		}
		return tx.Save(&next).Error
	})
	var app *AppError
	if !errors.As(err, &app) || app.Code != CodeConflict {
		t.Fatalf("unbounded storage mutation accepted: %v", err)
	}
	var current models.Storage
	if err := s.writeDB.First(&current, storage.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.RootPath != storage.RootPath || len(catalogReadEntries(t, s, library.ID, "")) != 2 {
		t.Fatal("failed budget changed root or head")
	}
	if err := s.writeDB.Model(&current).Updates(map[string]any{"root_path": "new-root", "root_path_normalized": "new-root"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error { return saveStorageProbeTx(tx, storage) }); err == nil {
		t.Fatal("old root probe accepted")
	}
	if err := s.writeDB.First(&current, storage.ID).Error; err != nil {
		t.Fatal(err)
	}
	if current.RootPath != "new-root" {
		t.Fatal("old probe rewrote root")
	}
}

func TestCatalogSourceStorageResetPublishesReadyRemoval(t *testing.T) {
	s, library, rec, entries := catalogFixture(t)
	catalogConvert(t, s, library, rec, entries)
	storage, _ := catalogSourceContext(t, s, library)
	next := storage
	next.RootPath, next.RootPathNormalized = "new-root", "new-root"
	if err := s.writeDB.Transaction(func(tx *gorm.DB) error {
		if err := applyCatalogStorageChangeTx(tx, storage, next, NewMediaChangeService(s.writeDB)); err != nil {
			return err
		}
		return tx.Save(&next).Error
	}); err != nil {
		t.Fatal(err)
	}
	if len(catalogReadEntries(t, s, library.ID, "")) != 0 {
		t.Fatal("storage root changed without empty source head")
	}
	var changes []models.MediaLibraryChange
	if err := s.writeDB.Where("library_id=?", library.ID).Find(&changes).Error; err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Kind != models.MediaLibraryChangeRemoval || changes[0].State != models.MediaLibraryChangeReady {
		t.Fatalf("source reset outbox: %+v", changes)
	}
}
