package database

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestCanonicalPlayerPlaybackHistoryMigrationUpgradesLegacyRows(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "player-history-v66.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at DATETIME NOT NULL)`).Error; err != nil {
		t.Fatal(err)
	}
	for _, item := range schemaMigrations() {
		if item.Version > 65 {
			break
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := item.Apply(tx); err != nil {
				return err
			}
			return tx.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)", item.Version, time.Now().UTC()).Error
		}); err != nil {
			t.Fatalf("apply v%d: %v", item.Version, err)
		}
	}
	now := time.Now().UTC()
	user := models.User{Username: "history-v66", UsernameNormalized: "history-v66", DisplayName: "History V66", PasswordHash: "x", Status: models.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	legacyKey := strings.Repeat("a", 64)
	if err := db.Exec(`INSERT INTO player_playback_history (user_id,sync_key,source_kind,source_locator,source_id,library_id,item_id,media_identity,title,stream_identity,media_type,poster_url,backdrop_url,title_logo_url,position,duration,completed,deleted,client_updated_at,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, user.ID, legacyKey, "server", "https://legacy.example.test", "legacy", "", "", "legacy-item", "Legacy", "", "movie", "", "", "", 120, 1000, false, false, int64(1000), uint64(1), now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("repeat migrate: %v", err)
	}
	for _, column := range []string{"canonical_identity", "item_token", "display_title", "display_subtitle", "series_title", "episode_title", "season_number", "episode_number", "poster_path", "backdrop_path", "episode_still_path"} {
		if !db.Migrator().HasColumn(&models.PlayerPlaybackHistory{}, column) {
			t.Fatalf("missing v66 column %q", column)
		}
	}
	var legacy models.PlayerPlaybackHistory
	if err := db.Where("user_id = ? AND sync_key = ?", user.ID, legacyKey).First(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.MediaIdentity != "legacy-item" || legacy.HistoryIdentity != "" || legacy.ItemToken != "" || legacy.DisplayTitle != "" {
		t.Fatalf("legacy row was rewritten: %+v", legacy)
	}
	var indexSQL string
	if err := db.Raw(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_player_history_user_canonical_active'`).Scan(&indexSQL).Error; err != nil || !strings.Contains(indexSQL, "deleted = 0") {
		t.Fatalf("canonical active index=%q err=%v", indexSQL, err)
	}
	var migrationCount int64
	if err := db.Table("schema_migrations").Where("version = ?", 66).Count(&migrationCount).Error; err != nil || migrationCount != 1 {
		t.Fatalf("v66 migration count=%d err=%v", migrationCount, err)
	}
}
