package services

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func assertPhysicalOwnerState(t *testing.T, db *gorm.DB, kind, ownerID, expected string) models.CatalogPhysicalWrite {
	t.Helper()
	var row models.CatalogPhysicalWrite
	if err := db.Where("owner_kind = ? AND owner_id = ?", kind, ownerID).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.State != expected {
		t.Fatalf("physical owner %s state=%s, want %s", kind, row.State, expected)
	}
	return row
}

func TestManualSTRMCleanupKeepsPhysicalClaimUntilExactRetry(t *testing.T) {
	service, _, actor, library, root := strmManagementFixture(t)
	_, artifact, path := createAutoCleanupScenario(t, service, library, root, "full", false, models.MediaArtifactStatusCompleted)
	if err := service.db.Model(&models.MediaArtifactRun{}).Where("library_id = ?", library.ID).Updates(map[string]any{"finished_at": time.Now().UTC(), "cleanup_status": models.MediaArtifactCleanupSkipped}).Error; err != nil {
		t.Fatal(err)
	}
	assertDrain := func(blocked bool) {
		t.Helper()
		err := service.db.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, library.ID) })
		if (err != nil) != blocked {
			t.Fatalf("drain blocked=%t, want %t: %v", err != nil, blocked, err)
		}
	}
	assertDrain(false)
	preview, err := service.PreviewCleanup(actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	service.removeFile = func(target string) error {
		assertDrain(true)
		if err := os.Remove(target); err != nil {
			return err
		}
		return errors.New("delete succeeded but operation response failed")
	}
	if _, err := service.ExecuteCleanup(actor, library.ID, preview.ConfirmationToken, RequestContext{}); err == nil {
		t.Fatal("ambiguous physical delete unexpectedly succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("physical deletion=%v", err)
	}
	var current models.MediaArtifact
	if err := service.db.First(&current, artifact.ID).Error; err != nil || current.Status != models.MediaArtifactStatusCleanup {
		t.Fatalf("cleanup evidence=%+v err=%v", current, err)
	}
	assertDrain(true)
	service.removeFile = os.Remove
	preview, err = service.PreviewCleanup(actor, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := service.ExecuteCleanup(actor, library.ID, preview.ConfirmationToken, RequestContext{}); err != nil || removed != 1 {
		t.Fatalf("exact retry=%d %v", removed, err)
	}
	assertDrain(false)
}
