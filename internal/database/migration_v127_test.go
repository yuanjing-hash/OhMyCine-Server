package database

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"testing"
)

func TestMigrationV127PreservesDefaultsAndAddsOnlyOptionalRunTiming(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{true: "upgrade126", false: "fresh"}[upgrade], func(t *testing.T) {
			db, err := Open(t.TempDir() + "/wait.db")
			if err != nil {
				t.Fatal(err)
			}
			sql, _ := db.DB()
			defer sql.Close()
			if upgrade {
				applyMigrationsThrough(t, db, 126)
				if err := db.Exec(`UPDATE follow_policy_defaults SET revision=9,subtitles_json='{"mode":"require","languages":["ja"]}' WHERE id=1`).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(db); err != nil {
				t.Fatal(err)
			}
			var policy models.FollowPolicyDefaults
			if err := db.First(&policy, 1).Error; err != nil || policy.AcquisitionPreference != "transfer_first" {
				t.Fatal("default preference missing", err)
			}
			if upgrade && (policy.Revision != 9 || policy.SubtitlesJSON != `{"mode":"require","languages":["ja"]}`) {
				t.Fatal("old policy bytes/revision changed")
			}
			columns, err := db.Migrator().ColumnTypes(&models.FollowRun{})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, column := range columns {
				if column.Name() == "retry_not_before" {
					nullable, known := column.Nullable()
					found = known && nullable
				}
			}
			if !found {
				t.Fatal("historical retry timestamp must stay nullable")
			}
		})
	}
}
