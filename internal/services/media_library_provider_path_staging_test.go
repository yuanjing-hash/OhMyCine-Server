package services

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestProviderPathStagingVisibilityHistoryAndMove(t *testing.T) {
	_, db, library, storage, _, _ := deletionFixture(t)
	ctx := context.Background()
	fingerprint := catalogSourceFingerprint(library, storage)
	stage := func(relative string) (models.MediaLibraryScanRun, medialibrary.Result) {
		t.Helper()
		run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", StartedAt: time.Now().UTC()}
		if err := db.Create(&run).Error; err != nil {
			t.Fatal(err)
		}
		result := medialibrary.Result{Files: []medialibrary.File{{ProviderID: "stable", RelativePath: relative}}}
		if err := stageProviderPaths(ctx, db, library, storage, result, run); err != nil {
			t.Fatal(err)
		}
		return run, result
	}
	visible := func(expected string) {
		t.Helper()
		var rows []models.MediaLibraryProviderPath
		if err := publishedProviderPaths(db, library.ID, fingerprint).Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		if expected == "" {
			if len(rows) != 0 {
				t.Fatal("unpublished path exposed")
			}
			return
		}
		if len(rows) != 1 || rows[0].RelativePath != expected {
			t.Fatalf("published paths=%+v expected=%s", rows, expected)
		}
	}
	activate := func(run models.MediaLibraryScanRun, result medialibrary.Result) error {
		return db.Transaction(func(tx *gorm.DB) error {
			return persistProviderPathsTx(tx, library, storage, result, run.StartedAt, run.ID)
		})
	}
	first, original := stage("/Show/Season02/01.mkv")
	visible("")
	rollback := errors.New("publication rolled back")
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := persistProviderPathsTx(tx, library, storage, original, first.StartedAt, first.ID); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	visible("")
	if err := activate(first, original); err != nil {
		t.Fatal(err)
	}
	visible("/Show/Season02/01.mkv")
	second, moved := stage("/Outside/Season02/01.mkv")
	visible("/Show/Season02/01.mkv")
	if err := activate(second, moved); err != nil {
		t.Fatal(err)
	}
	visible("/Outside/Season02/01.mkv")
	_, _ = stage("/Failed/01.mkv")
	visible("/Outside/Season02/01.mkv")
	if err := db.Where("library_id = ?", library.ID).Delete(&models.MediaLibraryScanRun{}).Error; err != nil {
		t.Fatal(err)
	}
	visible("/Outside/Season02/01.mkv")
	var oldRows []models.MediaLibraryProviderPath
	if err := publishedProviderPaths(db, library.ID, fingerprint).Where("relative_path LIKE ?", "/Show/%").Find(&oldRows).Error; err != nil || len(oldRows) != 0 {
		t.Fatalf("old directory can still claim moved identity: %+v %v", oldRows, err)
	}
	var foreign []models.MediaLibraryProviderPath
	if err := publishedProviderPaths(db, library.ID, "another-source").Find(&foreign).Error; err != nil || len(foreign) != 0 {
		t.Fatal("foreign source visible", err)
	}
}

func TestProviderPathStagingImmutableManifestAndSourceFence(t *testing.T) {
	_, db, library, storage, _, _ := deletionFixture(t)
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", StartedAt: time.Now().UTC()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	result := medialibrary.Result{Files: []medialibrary.File{{ProviderID: "stable", RelativePath: "/Show/01.mkv"}}}
	if err := stageProviderPaths(context.Background(), db, library, storage, result, run); err != nil {
		t.Fatal(err)
	}
	if err := stageProviderPaths(context.Background(), db, library, storage, result, run); err != nil {
		t.Fatal("identical staging retry rejected", err)
	}
	changed := medialibrary.Result{Files: []medialibrary.File{{ProviderID: "stable", RelativePath: "/Different/01.mkv"}}}
	if err := stageProviderPaths(context.Background(), db, library, storage, changed, run); !errors.Is(err, ErrCatalogFence) {
		t.Fatal("staged manifest rewritten", err)
	}
	library.ProviderRootID = "replacement"
	if err := db.Transaction(func(tx *gorm.DB) error {
		return persistProviderPathsTx(tx, library, storage, result, run.StartedAt, run.ID)
	}); !errors.Is(err, ErrCatalogFence) {
		t.Fatal("old source activated", err)
	}
}

func TestProviderPathStagingWritesBoundedPagesAndActivationOnlyHeader(t *testing.T) {
	_, db, library, storage, _, _ := deletionFixture(t)
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", StartedAt: time.Now().UTC()}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	result := medialibrary.Result{}
	for i := 0; i < 251; i++ {
		result.Files = append(result.Files, medialibrary.File{ProviderID: fmt.Sprint(i), RelativePath: fmt.Sprintf("/Show/%d.mkv", i)})
	}
	pages := 0
	callback := "test:provider_path_pages"
	if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if rows, ok := tx.Statement.Dest.([]models.MediaLibraryProviderPath); ok {
			pages++
			if len(rows) > 100 {
				t.Errorf("unbounded page: %d", len(rows))
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Create().Remove(callback) })
	if err := stageProviderPaths(context.Background(), db, library, storage, result, run); err != nil {
		t.Fatal(err)
	}
	if pages != 3 {
		t.Fatalf("pages=%d want3", pages)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return persistProviderPathsTx(tx, library, storage, result, run.StartedAt, run.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if pages != 3 {
		t.Fatal("publication repeated row writes")
	}
	var visible int64
	if err := publishedProviderPaths(db, library.ID, catalogSourceFingerprint(library, storage)).Select("paths.provider_id").Count(&visible).Error; err != nil || visible != 251 {
		t.Fatalf("visible=%d error=%v", visible, err)
	}
}
