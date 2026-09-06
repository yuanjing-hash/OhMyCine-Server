package database

import "testing"

func TestMediaChangePendingCleanupMigrationIsAdditive(t *testing.T) {
	db := structureMigrationDB(t, 80)
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var rows int64
	if err := db.Table("media_change_pending_cleanups").Count(&rows).Error; err != nil || rows != 0 {
		t.Fatalf("startup cleanup marker=%d err=%v", rows, err)
	}
	for _, name := range []string{"idx_media_change_pending_kind", "idx_media_change_pending_sequence", "idx_media_change_pending_revision"} {
		var count int64
		if err := db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&count).Error; err != nil || count != 1 {
			t.Fatalf("index=%s count=%d err=%v", name, count, err)
		}
	}
	var applied int64
	if err := db.Table("schema_migrations").Where("version = ?", 81).Count(&applied).Error; err != nil || applied != 1 {
		t.Fatalf("applied=%d err=%v", applied, err)
	}
}
