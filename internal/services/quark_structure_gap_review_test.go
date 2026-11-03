package services

// Shared synthetic fixture for native Quark structure workflow contracts.
import (
	"context"

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
