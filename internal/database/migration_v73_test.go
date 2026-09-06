package database

import (
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestHistoryArtworkMigrationV73UpgradeRepeatAndCascade(t *testing.T) {
	db := structureMigrationDB(t, 72)
	now := time.Now().UTC()
	user := models.User{Username: "art", UsernameNormalized: "art", DisplayName: "Art", PasswordHash: "x", Status: models.UserStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("a", 64)
	if err := db.Exec(`INSERT INTO player_playback_history (user_id,sync_key,source_kind,source_id,media_identity,title,client_updated_at,revision,created_at,updated_at) VALUES (?,?,'emby','external','1','Movie',1000,1,?,?)`, user.ID, key, now, now).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var row models.PlayerPlaybackHistory
	if err := db.First(&row, "user_id = ? AND sync_key = ?", user.ID, key).Error; err != nil {
		t.Fatal(err)
	}
	if row.ClientUpdatedAt != 1000 || row.PosterAssetID != "" || row.SourceKind != "emby" {
		t.Fatalf("legacy row changed: %+v", row)
	}
	asset := models.PlayerHistoryArtwork{ID: "opaque", UserID: user.ID, SyncKey: key, Slot: "poster", Body: []byte("fixture"), CreatedAt: now}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&row).Updates(map[string]any{"poster_asset_id": "opaque", "deleted": true}).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&models.PlayerHistoryArtwork{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("tombstone assets=%d %v", count, err)
	}
	if err := db.First(&row, "user_id = ? AND sync_key = ?", user.ID, key).Error; err != nil || row.PosterAssetID != "" {
		t.Fatalf("stale association=%q %v", row.PosterAssetID, err)
	}
	if err := db.Model(&row).Update("deleted", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.PlayerHistoryArtwork{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("hard-delete assets=%d %v", count, err)
	}
	if err := db.Table("schema_migrations").Where("version = 73").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("migration count=%d %v", count, err)
	}
}
