package services

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestMediaChangeDispatchResumesAndCoalescesBoundedTargets(t *testing.T) {
	management, _, _, library, _ := strmManagementFixture(t)
	db := management.db
	now := time.Now().UTC()
	connection := models.Connection{Name: "Dispatch", NameNormalized: "dispatch", Provider: models.ConnectionProviderEmby, Endpoint: "https://example.invalid", CredentialCiphertext: "unused", Enabled: true, LastHealthStatus: "unknown", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	targets := make([]models.MediaServerRefreshTarget, 235)
	for i := range targets {
		targets[i] = models.MediaServerRefreshTarget{LibraryID: library.ID, ConnectionID: connection.ID, UpstreamLibraryID: fmt.Sprint(i), UpstreamLibraryName: "test", Enabled: true, LastStatus: "idle", Revision: 1, CreatedAt: now, UpdatedAt: now}
	}
	if err := db.CreateInBatches(&targets, 100).Error; err != nil {
		t.Fatal(err)
	}
	s := NewMediaChangeService(db)
	record := func(generation uint64) {
		t.Helper()
		if err := db.Transaction(func(tx *gorm.DB) error {
			_, err := s.RecordTx(tx, library.ID, generation, models.MediaLibraryChangeCatalog, true)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	countAt := func(revision uint64) int64 {
		t.Helper()
		var count int64
		if err := db.Model(&models.MediaServerRefreshTarget{}).Where("library_id=? AND desired_revision=?", library.ID, revision).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		return count
	}
	record(1)
	if countAt(0) != 235 {
		t.Fatal("ready commit performed inline fanout")
	}
	if _, err := s.DispatchBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countAt(1) != mediaChangeDispatchBatch {
		t.Fatal("fanout batch was not bounded")
	}
	// A newer commit restarts the compact cursor so already-visited targets
	// also receive the new revision. Retention cannot delete this authority.
	record(2)
	s.retained = 1
	s.prune()
	s = NewMediaChangeService(db) // process restart before any wake
	var notified int
	s.SetReadyHandler(func(id uint, revision uint64) {
		if id != library.ID || revision != 2 {
			t.Errorf("wrong callback %d/%d", id, revision)
		}
		notified++
	})
	for i := 0; i < 5; i++ {
		before := countAt(2)
		found, err := s.DispatchBatch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if countAt(2)-before > mediaChangeDispatchBatch {
			t.Fatal("unbounded fanout")
		}
		if !found {
			break
		}
	}
	if countAt(2) != 235 || notified != 1 {
		t.Fatalf("lost replay: targets=%d notifications=%d", countAt(2), notified)
	}
	var remaining int64
	if err := db.Model(&models.MediaChangeDispatch{}).Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("marker=%d %v", remaining, err)
	}
	// Replay/older signals cannot regress target authority.
	s.SetReadyHandler(nil)
	if err := db.Transaction(func(tx *gorm.DB) error { return s.advanceTargetsTx(tx, library.ID, 1, now) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DispatchBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countAt(2) != 235 {
		t.Fatal("old marker regressed desired revision")
	}
}

func TestMediaChangeDispatchCursorAndTargetsRollbackTogether(t *testing.T) {
	fixture := newMediaServerRefreshFixture(t)
	connectionID := fixture.createConnection(t, "https://example.invalid")
	target := fixture.createTarget(t, connectionID, 0)
	s := NewMediaChangeService(fixture.service.db)
	if err := s.db.Transaction(func(tx *gorm.DB) error { return s.advanceTargetsTx(tx, target.LibraryID, 7, time.Now().UTC()) }); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected dispatch cursor failure")
	if err := s.db.Callback().Update().Before("gorm:update").Register("test:dispatch-cursor-fault", func(tx *gorm.DB) {
		if tx.Statement.Table == "media_change_dispatches" {
			_ = tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.db.Callback().Update().Remove("test:dispatch-cursor-fault") })
	if _, err := s.DispatchBatch(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("fault=%v", err)
	}
	if err := s.db.First(&target, target.ID).Error; err != nil || target.DesiredRevision != 0 {
		t.Fatalf("targets escaped rollback: %+v %v", target, err)
	}
	var marker models.MediaChangeDispatch
	if err := s.db.First(&marker, "library_id=?", target.LibraryID).Error; err != nil || marker.AfterTargetID != 0 {
		t.Fatalf("cursor escaped rollback: %+v %v", marker, err)
	}
	if err := s.db.Callback().Update().Remove("test:dispatch-cursor-fault"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DispatchBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.db.First(&target, target.ID).Error; err != nil || target.DesiredRevision != 7 {
		t.Fatalf("retry failed: %+v %v", target, err)
	}
}

func TestMediaChangeDispatchRunStopsAndDoesNotNeedWake(t *testing.T) {
	fixture := newMediaServerRefreshFixture(t)
	target := fixture.createTarget(t, fixture.createConnection(t, "https://example.invalid"), 0)
	s := NewMediaChangeService(fixture.service.db)
	if err := s.db.Transaction(func(tx *gorm.DB) error { return s.advanceTargetsTx(tx, target.LibraryID, 3, time.Now().UTC()) }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delivered, stopped := make(chan struct{}, 1), make(chan struct{})
	s.SetReadyHandler(func(uint, uint64) { delivered <- struct{}{} })
	go func() { defer close(stopped); s.Run(ctx, nil, func(err error) { t.Error(err) }) }()
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("restart without wake lost dispatch")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("dispatch lifecycle did not stop")
	}
}
