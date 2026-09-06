package services

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCatalogPhysicalRuntimeRecoveryIsBoundedAndRequiresLiveExclusiveDatabase(t *testing.T) {
	db, repair, _ := catalogPhysicalRepairFixture(t, false)
	var databases []struct{ Name, File string }
	if err := db.Raw("PRAGMA database_list").Scan(&databases).Error; err != nil {
		t.Fatal(err)
	}
	var path string
	for _, item := range databases {
		if item.Name == "main" {
			path = item.File
		}
	}
	runtime, err := database.AcquireExclusiveRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	bound, err := runtime.Bind(db)
	if err != nil {
		t.Fatal(err)
	}
	runtimeID, err := database.ExclusiveRuntimeID(bound)
	if err != nil || runtimeID == "" {
		t.Fatalf("runtime=%q %v", runtimeID, err)
	}
	rows := make([]models.CatalogPhysicalWrite, 0, 603)
	for i := 0; i < 603; i++ {
		row := models.CatalogPhysicalWrite{LibraryID: repair.LibraryID, OwnerKind: "repair", OwnerID: fmt.Sprintf("crash-owner-%d", i), Revision: 1, State: "entered", RuntimeID: "prior-process", OwnerDigest: "proof", SourceFingerprint: "source", ConfigFingerprint: "config", EnteredAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		if i == 600 {
			row.RuntimeID = runtimeID
		}
		if i == 601 {
			row.RuntimeID = ""
		}
		if i == 602 {
			row.State = "quiescent"
		}
		rows = append(rows, row)
	}
	if err := db.CreateInBatches(rows, 100).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverCatalogPhysicalRuntimeBatch(context.Background(), db); err == nil {
		t.Fatal("unbound DB forged a process-exit proof")
	}
	for _, want := range []int64{250, 250, 100, 0} {
		got, err := RecoverCatalogPhysicalRuntimeBatch(context.Background(), bound)
		if err != nil || got != want {
			t.Fatalf("recovered=%d want=%d err=%v", got, want, err)
		}
	}
	var entered, quiescent, settled int64
	if err := db.Model(&models.CatalogPhysicalWrite{}).Where("state='entered'").Count(&entered).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.CatalogPhysicalWrite{}).Where("state='quiescent'").Count(&quiescent).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.CatalogPhysicalWrite{}).Where("state='settled'").Count(&settled).Error; err != nil {
		t.Fatal(err)
	}
	if entered != 2 || quiescent != 601 || settled != 0 {
		t.Fatalf("entered=%d quiescent=%d settled=%d", entered, quiescent, settled)
	}
	if err := bound.Transaction(func(tx *gorm.DB) error { return AssertCatalogPhysicalDrainedTx(tx, repair.LibraryID) }); err == nil {
		t.Fatal("crash recovery falsely settled domain operations")
	}
	foreign, err := database.Open(filepath.Join(t.TempDir(), "foreign.db"))
	if err != nil {
		t.Fatal(err)
	}
	foreignSQL, _ := foreign.DB()
	t.Cleanup(func() { _ = foreignSQL.Close() })
	if _, err := runtime.Bind(foreign); err == nil {
		t.Fatal("runtime proof moved to foreign database")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverCatalogPhysicalRuntimeBatch(context.Background(), bound); err == nil {
		t.Fatal("closed OS lock still authorized recovery")
	}
}
