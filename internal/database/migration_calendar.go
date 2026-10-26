package database

import "gorm.io/gorm"

func migrateSubscriptionCalendar(db *gorm.DB) error {
	for _, statement := range []string{
		`CREATE TABLE calendar_seasons (key TEXT PRIMARY KEY, generation INTEGER NOT NULL, tmdb_id INTEGER NOT NULL, season_number INTEGER NOT NULL, language TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 0, fetched_at DATETIME, fresh_until DATETIME, retry_at DATETIME, last_requested_at DATETIME, last_error_code TEXT NOT NULL DEFAULT '', complete INTEGER NOT NULL DEFAULT 0, episode_count INTEGER NOT NULL DEFAULT 0 CHECK(episode_count BETWEEN 0 AND 5000), payload_bytes INTEGER NOT NULL DEFAULT 0 CHECK(payload_bytes >= 0), updated_at DATETIME NOT NULL)`,
		`CREATE INDEX idx_calendar_generation ON calendar_seasons(generation)`,
		`CREATE INDEX idx_calendar_fresh_until ON calendar_seasons(fresh_until)`,
		`CREATE INDEX idx_calendar_retry_at ON calendar_seasons(retry_at)`,
		`CREATE TABLE calendar_episodes (season_key TEXT NOT NULL, episode_number INTEGER NOT NULL CHECK(episode_number > 0), tmdb_id INTEGER NOT NULL, name TEXT NOT NULL, air_date TEXT NOT NULL DEFAULT '', PRIMARY KEY(season_key,episode_number), FOREIGN KEY(season_key) REFERENCES calendar_seasons(key) ON DELETE CASCADE)`,
		`CREATE INDEX idx_calendar_date ON calendar_episodes(season_key,air_date)`,
		`CREATE TABLE calendar_refresh_cursors (name TEXT PRIMARY KEY, generation INTEGER NOT NULL, subscription_id TEXT NOT NULL DEFAULT '', season_number INTEGER NOT NULL DEFAULT -1, updated_at DATETIME NOT NULL)`,
		`ALTER TABLE storages ADD COLUMN last_capacity_free_bytes INTEGER`,
		`ALTER TABLE storages ADD COLUMN last_capacity_total_bytes INTEGER`,
		`ALTER TABLE storages ADD COLUMN last_capacity_checked_at DATETIME`,
		`ALTER TABLE storages ADD COLUMN last_capacity_sampled_at DATETIME`,
		`ALTER TABLE storages ADD COLUMN last_capacity_error_code TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE storages ADD COLUMN last_capacity_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE storages ADD COLUMN last_capacity_source_fingerprint TEXT NOT NULL DEFAULT ''`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}
