package services

import (
	"context"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func drainPendingMediaChangeCleanup(t *testing.T, s *MediaChangeService) {
	t.Helper()
	for i := 0; i < 100; i++ {
		more, err := s.CleanupPendingBatch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return
		}
	}
	t.Fatal("pending cleanup did not drain within bounded test iterations")
}

func TestMediaChangeReadinessCoalescesThousandChangesInBoundedWriter(t *testing.T) {
	management, _, _, library, _ := strmManagementFixture(t)
	db := management.db
	service := NewMediaChangeService(db)
	kinds := []string{models.MediaLibraryChangeCatalog, models.MediaLibraryChangeMetadata, models.MediaLibraryChangeRemoval}
	rows := make([]models.MediaLibraryChange, 1000)
	for i := range rows {
		rows[i] = models.MediaLibraryChange{LibraryID: library.ID, Revision: uint64(i + 1), Kind: kinds[i%3], State: models.MediaLibraryChangePending, Generation: 7, CreatedAt: time.Now().UTC()}
	}
	if err := db.CreateInBatches(&rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	var ready []models.MediaLibraryChange
	if err := db.Transaction(func(tx *gorm.DB) error {
		var before, after int64
		if err := tx.Raw("SELECT total_changes()").Scan(&before).Error; err != nil {
			return err
		}
		var err error
		ready, err = service.MarkGenerationReadyTx(tx, library.ID, 7)
		if err != nil {
			return err
		}
		if err := tx.Raw("SELECT total_changes()").Scan(&after).Error; err != nil {
			return err
		}
		if after-before > 10 {
			t.Fatalf("unbounded readiness writes=%d", after-before)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(ready) != 3 || ready[0].Revision != 998 || ready[1].Revision != 999 || ready[2].Revision != 1000 {
		t.Fatalf("ready=%+v", ready)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		repeated, err := service.MarkGenerationReadyTx(tx, library.ID, 7)
		if err == nil && len(repeated) != 0 {
			t.Fatalf("cleanup-pending duplicates republished: %+v", repeated)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var same, newer models.MediaLibraryChange
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		same, err = service.RecordTx(tx, library.ID, 7, models.MediaLibraryChangeMetadata, false)
		if err != nil {
			return err
		}
		newer, err = service.RecordTx(tx, library.ID, 8, models.MediaLibraryChangeCatalog, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var before, after int64
	if err := db.Model(&models.MediaLibraryChange{}).Where("state = ?", models.MediaLibraryChangePending).Count(&before).Error; err != nil {
		t.Fatal(err)
	}
	if more, err := service.CleanupPendingBatch(context.Background()); err != nil || !more {
		t.Fatalf("first cleanup=%v err=%v", more, err)
	}
	if err := db.Model(&models.MediaLibraryChange{}).Where("state = ?", models.MediaLibraryChangePending).Count(&after).Error; err != nil {
		t.Fatal(err)
	}
	if before-after != mediaChangeDispatchBatch {
		t.Fatalf("batch removed=%d", before-after)
	}
	// A new service has no process-local state: resume from the durable cursor.
	drainPendingMediaChangeCleanup(t, NewMediaChangeService(db))
	var pending []models.MediaLibraryChange
	if err := db.Where("state = ?", models.MediaLibraryChangePending).Order("sequence").Find(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].Sequence != same.Sequence || pending[1].Sequence != newer.Sequence {
		t.Fatalf("post-cutoff pending lost=%+v", pending)
	}
	page, err := service.ReadyAfter(0, 10)
	if err != nil || len(page.Changes) != 3 || page.ResyncRequired {
		t.Fatalf("feed=%+v err=%v", page, err)
	}
	for i := range ready {
		if page.Changes[i].Sequence != ready[i].Sequence {
			t.Fatalf("cleanup changed feed cursor=%+v", page)
		}
	}
}

func TestMediaChangeReadinessFailureRollsBackMarkerAndRows(t *testing.T) {
	management, _, _, library, _ := strmManagementFixture(t)
	db := management.db
	service := NewMediaChangeService(db)
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := service.RecordTx(tx, library.ID, 7, models.MediaLibraryChangeMetadata, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER test_ready_dispatch_failure BEFORE INSERT ON media_change_dispatches BEGIN SELECT RAISE(ABORT, 'injected'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, err := service.MarkGenerationReadyTx(tx, library.ID, 7); return err }); err == nil {
		t.Fatal("expected dispatch failure")
	}
	var ready, marker int64
	db.Model(&models.MediaLibraryChange{}).Where("state = ?", models.MediaLibraryChangeReady).Count(&ready)
	db.Model(&models.MediaChangePendingCleanup{}).Count(&marker)
	if ready != 0 || marker != 0 {
		t.Fatalf("partial commit ready=%d marker=%d", ready, marker)
	}
}

func TestMediaChangeLateArtifactPublishesAfterAlreadyConsumedOtherLibraryCursor(t *testing.T) {
	management, _, _, first, _ := strmManagementFixture(t)
	db := management.db
	service := NewMediaChangeService(db)
	second := first
	second.ID = 0
	second.Name = "Cursor second library"
	second.NameNormalized = "cursor second library"
	second.RelativeRoot = "/second"
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	var pending, seen models.MediaLibraryChange
	if err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		pending, err = service.RecordTx(tx, first.ID, 7, models.MediaLibraryChangeCatalog, false)
		if err != nil {
			return err
		}
		seen, err = service.RecordTx(tx, second.ID, 1, models.MediaLibraryChangeMetadata, true)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	page, err := service.ReadyAfter(0, 10)
	if err != nil || len(page.Changes) != 1 || page.LatestSequence != seen.Sequence {
		t.Fatalf("initial feed=%+v err=%v", page, err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		ready, err := service.MarkGenerationReadyTx(tx, first.ID, 7)
		if err == nil && (len(ready) != 1 || ready[0].Sequence <= seen.Sequence || ready[0].Revision != pending.Revision) {
			t.Fatalf("late ready=%+v", ready)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	late, err := service.ReadyAfter(seen.Sequence, 10)
	if err != nil || late.ResyncRequired || len(late.Changes) != 1 || late.Changes[0].LibraryID != first.ID {
		t.Fatalf("late event hidden=%+v err=%v", late, err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		again, err := service.MarkGenerationReadyTx(tx, first.ID, 7)
		if err == nil && len(again) != 0 {
			t.Fatalf("duplicate published=%+v", again)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	drainPendingMediaChangeCleanup(t, NewMediaChangeService(db))
	after, err := service.ReadyAfter(late.LatestSequence, 10)
	if err != nil || after.ResyncRequired || len(after.Changes) != 0 {
		t.Fatalf("cursor changed on cleanup=%+v err=%v", after, err)
	}
}
