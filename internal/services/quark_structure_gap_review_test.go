package services

// Original gap fixture retained for bounded source/namespace tests. All cloud
// behavior here is synthetic; no credentials or external transport are used.
import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func quarkStructureGapFixture(t *testing.T, correct bool) (*MediaLibraryStructureService, models.MediaLibrary, uint) {
	t.Helper()
	store, library, rec, entries := catalogFixture(t)
	var storage models.Storage
	var profile models.MediaClassificationProfile
	var user models.User
	for _, err := range []error{store.writeDB.First(&storage, library.StorageID).Error, store.writeDB.First(&profile, library.ProfileID).Error, store.writeDB.First(&user).Error} {
		if err != nil {
			t.Fatal(err)
		}
	}
	connection := models.Connection{Name: "isolated-quark-structure", NameNormalized: "isolated-quark-structure", Provider: cloud.ProviderQuark, Enabled: true, Revision: 1}
	if err := store.writeDB.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	storage.Type, storage.RootPath, storage.ConnectionID, storage.Enabled = models.StorageTypeQuark, "synthetic-storage", &connection.ID, true
	library.ProviderRootID, library.RelativeRoot, library.Enabled = "synthetic-library", "/", true
	library.STRMEnabled, library.SignedProxyEnabled = false, false
	library.TVDirectoryTemplate, library.TVFilenameTemplate = "TV/{title}/Season {season:02}", "{title} - S{season:02}E{episode:02}"
	if err := store.writeDB.Save(&storage).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Save(&library).Error; err != nil {
		t.Fatal(err)
	}
	s := NewMediaLibraryStructureService(store.writeDB, NewAuditService(store.writeDB), nil, nil, zerolog.Nop())
	s.SetCatalogSnapshotStore(store)
	s.SetCatalogPublicationServices(NewMediaChangeService(store.writeDB), nil)
	if correct {
		plan, err := s.planner.BuildContext(context.Background(), library, entries, nil, "series:tmdb:42", nil)
		if err != nil || len(plan.Items) == 0 {
			t.Fatalf("expected repairable starting fixture: %v", err)
		}
		for i := range entries {
			for _, item := range plan.Items {
				if item.ProviderID == entries[i].ProviderID {
					entries[i].RelativePath = item.TargetRelative
				}
			}
		}
	}
	if err := store.writeDB.Save(&entries).Error; err != nil {
		t.Fatal(err)
	}
	catalogConvert(t, store, library, rec, entries)
	fingerprints := map[string]any{"source_fingerprint": catalogSourceFingerprint(library, storage), "config_fingerprint": catalogConfigFingerprint(library, storage, profile)}
	if err := store.writeDB.Model(&models.CatalogHead{}).Where("library_id = ?", library.ID).Updates(fingerprints).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Model(&models.CatalogSnapshot{}).Where("library_id = ?", library.ID).Updates(fingerprints).Error; err != nil {
		t.Fatal(err)
	}
	return s, library, user.ID
}

func TestQuarkStructureGapEnsureWorkLayoutBoundaries(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		correct bool
		tmdb    int64
		blocked bool
	}{
		{"existing-correct-layout", true, 42, false},
		{"new-work", false, 999, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s, library, owner := quarkStructureGapFixture(t, scenario.correct)
			plan, _, err := s.buildPlan(context.Background(), library.ID, "series:tmdb:42")
			if err != nil {
				t.Fatal(err)
			}
			if (len(plan.Items) == 0) != scenario.correct {
				t.Fatalf("fixture plan items=%d correct=%v", len(plan.Items), scenario.correct)
			}
			var before, after models.CatalogHead
			if err := s.db.First(&before, "library_id = ?", library.ID).Error; err != nil {
				t.Fatal(err)
			}
			err = s.EnsureWorkLayout(context.Background(), owner, library.ID, scenario.tmdb, "tv")
			if err != nil {
				t.Fatalf("unaffected path blocked: %v", err)
			}
			var repairs, physical int64
			if err := s.db.Model(&models.MediaLibraryStructureRepair{}).Count(&repairs).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.db.Model(&models.CatalogPhysicalWrite{}).Count(&physical).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.db.First(&after, "library_id = ?", library.ID).Error; err != nil {
				t.Fatal(err)
			}
			if repairs != 0 || physical != 0 || before.Revision != after.Revision {
				t.Fatal("support check mutated repair/owner/catalog")
			}
		})
	}
}

type quarkStructureGapOnlyBackend struct{ called bool }

func (*quarkStructureGapOnlyBackend) StorageType() string { return models.StorageTypeQuark }
func (b *quarkStructureGapOnlyBackend) ValidateRecycle(context.Context, StructureBoundary) error {
	b.called = true
	return errors.New("unexpected backend call")
}
func (b *quarkStructureGapOnlyBackend) Recycle(context.Context, StructureBoundary, []StructureRecycleItem, StructureProgress) error {
	b.called = true
	return errors.New("unexpected backend call")
}
func (b *quarkStructureGapOnlyBackend) Apply(context.Context, StructureBoundary, []StructurePlanItem, StructureProgress) error {
	b.called = true
	return errors.New("unexpected backend call")
}

func TestQuarkStructureGapBackendRegistrationAloneIsInsufficient(t *testing.T) {
	s, library, _ := quarkStructureGapFixture(t, false)
	backend := &quarkStructureGapOnlyBackend{}
	s.backends.Register(backend)
	plan, _, err := s.buildPlan(context.Background(), library.ID, "series:tmdb:42")
	if err != nil || len(plan.Items) == 0 {
		t.Fatalf("plan: %v", err)
	}
	var storage models.Storage
	if err := s.db.First(&storage, library.StorageID).Error; err != nil {
		t.Fatal(err)
	}
	_, err = s.prepareCatalogStructureDirectories(context.Background(), models.MediaLibraryStructureRepair{}, structureCatalogRepairState{}, plan, StructureBoundary{Library: library, Storage: storage}, backend, nil)
	if !errors.Is(err, ErrCatalogInvalid) || backend.called {
		t.Fatalf("directory preparation no longer has its Pan115-only gate: %v", err)
	}
}
