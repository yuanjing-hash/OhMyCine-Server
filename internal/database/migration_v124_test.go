package database

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"testing"
)

func TestMigrationV124FollowDefaultsAreAdditiveAndDurable(t *testing.T) {
	db, err := Open(t.TempDir() + "/follow-defaults.db")
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	t.Cleanup(func() { _ = sql.Close() })
	applyMigrationsThrough(t, db, 123)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var policy models.FollowPolicyDefaults
	if err := db.First(&policy, 1).Error; err != nil || policy.Revision != 1 || policy.SubtitlesJSON != `{"mode":"prefer","languages":["zh-Hans","zh-Hant"]}` {
		t.Fatalf("seed=%+v err=%v", policy, err)
	}
	if err := db.Model(&policy).Updates(map[string]any{"revision": 2, "subtitles_json": `{"mode":"ignore","languages":[]}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&policy, 1).Error; err != nil || policy.Revision != 2 || policy.SubtitlesJSON != `{"mode":"ignore","languages":[]}` {
		t.Fatal("migration overwrote administrator policy")
	}
	if err := db.Exec(`INSERT INTO follow_policy_defaults(id,revision,subtitles_json,updated_at) VALUES(2,1,'{}',CURRENT_TIMESTAMP)`).Error; err == nil {
		t.Fatal("singleton constraint missing")
	}
}
