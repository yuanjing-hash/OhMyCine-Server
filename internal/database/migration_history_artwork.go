package database

import "gorm.io/gorm"

func migrateHistoryArtwork(db *gorm.DB) error {
	for _, statement := range []string{
		`ALTER TABLE player_playback_history ADD COLUMN poster_asset_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE player_playback_history ADD COLUMN backdrop_asset_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE player_playback_history ADD COLUMN title_logo_asset_id TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE player_history_artworks (id TEXT PRIMARY KEY, user_id INTEGER NOT NULL, sync_key TEXT NOT NULL, slot TEXT NOT NULL CHECK(slot IN ('poster','backdrop','title_logo')), body BLOB NOT NULL, created_at DATETIME NOT NULL, UNIQUE(user_id,sync_key,slot), FOREIGN KEY(user_id,sync_key) REFERENCES player_playback_history(user_id,sync_key) ON DELETE CASCADE)`,
		`CREATE TRIGGER player_history_artwork_tombstone AFTER UPDATE OF deleted ON player_playback_history WHEN NEW.deleted = 1 BEGIN DELETE FROM player_history_artworks WHERE user_id = NEW.user_id AND sync_key = NEW.sync_key; UPDATE player_playback_history SET poster_asset_id = '', backdrop_asset_id = '', title_logo_asset_id = '' WHERE user_id = NEW.user_id AND sync_key = NEW.sync_key; END`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
