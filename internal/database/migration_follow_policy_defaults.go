package database

import "gorm.io/gorm"

func migrateFollowPolicyDefaults(db *gorm.DB) error {
	if err := db.Exec(`CREATE TABLE follow_policy_defaults (id INTEGER PRIMARY KEY CHECK(id = 1), revision INTEGER NOT NULL CHECK(revision > 0), subtitles_json TEXT NOT NULL, updated_at DATETIME NOT NULL)`).Error; err != nil {
		return err
	}
	return db.Exec(`INSERT INTO follow_policy_defaults(id,revision,subtitles_json,updated_at) VALUES(1,1,'{"mode":"prefer","languages":["zh-Hans","zh-Hant"]}',CURRENT_TIMESTAMP)`).Error
}
