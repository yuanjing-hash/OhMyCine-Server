package database

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestManagedRevisionMigrationV86AdditiveAndRepeatable(t *testing.T) {
	db := structureMigrationDB(t, 85)
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	if !db.Migrator().HasColumn(&models.TransferTask{}, "managed_revision") {
		t.Fatal("missing counter")
	}
	var count int64
	if err := db.Raw("SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'managed_revision_%'").Scan(&count).Error; err != nil || count != 3 {
		t.Fatalf("trigger count %d %v", count, err)
	}
}

func TestManagedRevisionTriggersAreExactTransactionalAndOverflowSafe(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "counter.db"))
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	defer func() { _ = sql.Close() }()
	if err := db.Exec("CREATE TABLE transfer_tasks (id TEXT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&models.MediaManagedItem{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO transfer_tasks(id) VALUES ('one'),('two')").Error; err != nil {
		t.Fatal(err)
	}
	row := models.MediaManagedItem{OpaqueID: "old", TransferTaskID: "one", RelativePath: "one.mkv", Managed: true, Active: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := migrateManagedManifestRevision(db); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id string) int64 {
		var n int64
		if err := db.Raw("SELECT managed_revision FROM transfer_tasks WHERE id=?", id).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	if read("one") != 0 {
		t.Fatal("migration backfilled old items")
	}
	for _, updates := range []map[string]any{{"updated_at": time.Now().UTC()}, {"size": int64(0)}} {
		if err := db.Model(&row).Updates(updates).Error; err != nil {
			t.Fatal(err)
		}
	}
	if read("one") != 0 {
		t.Fatal("no-op/timestamp bumped")
	}
	if err := db.Model(&row).Update("size", 5).Error; err != nil {
		t.Fatal(err)
	}
	if read("one") != 1 {
		t.Fatal("identity update missing")
	}
	if err := db.Model(&row).Update("transfer_task_id", "two").Error; err != nil {
		t.Fatal(err)
	}
	if read("one") != 2 || read("two") != 1 {
		t.Fatal("old/new ownership revision missing")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&row).Update("active", false).Error; err != nil {
			return err
		}
		return fmt.Errorf("rollback")
	}); err == nil {
		t.Fatal("expected rollback")
	}
	if read("two") != 1 {
		t.Fatal("rollback advanced counter")
	}
	if err := db.Exec("UPDATE transfer_tasks SET managed_revision=9223372036854775807 WHERE id='two'").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&row).Update("size", 6).Error; err == nil {
		t.Fatal("overflow did not reject child mutation")
	}
	var after models.MediaManagedItem
	if err := db.First(&after, row.ID).Error; err != nil || after.Size != 5 {
		t.Fatalf("overflow modified child %+v %v", after, err)
	}
	if err := db.Exec("UPDATE transfer_tasks SET managed_revision=1 WHERE id='two'").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&row).Error; err != nil {
		t.Fatal(err)
	}
	if read("two") != 2 {
		t.Fatal("delete not counted")
	}
	started := time.Now()
	for start := 0; start < 10000; start += 250 {
		batch := make([]models.MediaManagedItem, 0, 250)
		for i := start; i < start+250; i++ {
			key := fmt.Sprintf("row-%d", i)
			batch = append(batch, models.MediaManagedItem{OpaqueID: key, TransferTaskID: "one", RelativePath: key})
		}
		if err := db.Create(&batch).Error; err != nil {
			t.Fatal(err)
		}
	}
	if read("one") != 10002 {
		t.Fatal("bulk mutation revision not accumulated")
	}
	t.Logf("10k managed rows with indexed parent trigger, 250-row batches: %s", time.Since(started))
}
