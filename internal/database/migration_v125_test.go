package database

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"testing"
	"time"
)

func TestMigrationV125SelectionGroupsAreAdditiveAndRepeatable(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade"}[upgrade], func(t *testing.T) {
			db, err := Open(t.TempDir() + "/groups.db")
			if err != nil {
				t.Fatal(err)
			}
			sql, _ := db.DB()
			defer sql.Close()
			if upgrade {
				applyMigrationsThrough(t, db, 124)
				now := time.Now().UTC()
				user := models.User{Username: "legacy125", UsernameNormalized: "legacy125", DisplayName: "legacy125", PasswordHash: "retained", Status: models.UserStatusActive, CreatedAt: now, UpdatedAt: now}
				if err = db.Create(&user).Error; err != nil {
					t.Fatal(err)
				}
				job := models.Job{ID: "legacy125-job", OwnerID: &user.ID, JobType: "download", Status: models.JobStatusCompleted, PayloadJSON: "{}"}
				if err = db.Create(&job).Error; err != nil {
					t.Fatal(err)
				}
				if err = seedMediaClassificationProfiles(db); err != nil {
					t.Fatal(err)
				}
				var profile models.MediaClassificationProfile
				if err = db.First(&profile).Error; err != nil {
					t.Fatal(err)
				}
				legacy := models.DownloadTask{ID: "legacy125-task", JobID: job.ID, OwnerID: user.ID, ProfileID: profile.ID, DownloaderName: "legacy", ProviderType: "fake", SourceCiphertext: "opaque-old-source-retained", Phase: models.DownloadTaskStatusCompleted}
				if err = db.Omit("BTSelectionGroupID", "ContentSelectionRequired").Create(&legacy).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err = Migrate(db); err != nil {
				t.Fatal(err)
			}
			if err = Migrate(db); err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasColumn(&models.DownloadTask{}, "BTSelectionGroupID") || !db.Migrator().HasColumn(&models.DownloadTask{}, "ContentSelectionRequired") || !db.Migrator().HasTable(&models.BTSelectionGroup{}) || !db.Migrator().HasTable(&models.BTSelectionRef{}) {
				t.Fatal("selection registry missing")
			}
			if upgrade {
				var legacy models.DownloadTask
				if err = db.First(&legacy, "id = ?", "legacy125-task").Error; err != nil || legacy.ContentSelectionRequired || legacy.BTSelectionGroupID != "" || legacy.SourceCiphertext != "opaque-old-source-retained" || legacy.Phase != models.DownloadTaskStatusCompleted {
					t.Fatal("legacy selection absence/ciphertext changed", legacy, err)
				}
				selected := legacy
				selected.ID = "selected125-task"
				selected.JobID = "selected125-job"
				selected.ContentSelectionRequired = true
				job := models.Job{ID: selected.JobID, OwnerID: &selected.OwnerID, JobType: "download", Status: models.JobStatusQueued, PayloadJSON: "{}"}
				if err = db.Create(&job).Error; err != nil {
					t.Fatal(err)
				}
				if err = db.Create(&selected).Error; err != nil {
					t.Fatal(err)
				}
				if err = db.Model(&selected).Updates(map[string]any{"content_selection_required": false, "display_name": "unchanged-scope"}).Error; err != nil {
					t.Fatal(err)
				}
				var reloaded models.DownloadTask
				if err = db.First(&reloaded, "id = ?", selected.ID).Error; err != nil || !reloaded.ContentSelectionRequired {
					t.Fatal("create-only required-selection marker was cleared", err)
				}
			}
			g := models.BTSelectionGroup{ID: "group", OwnerID: 7, DownloaderID: "executor", ScopeDigest: "scope", Workspace: "/managed", Revision: 2, KnownDigestsJSON: "[]"}
			if err = db.Create(&g).Error; err != nil {
				t.Fatal(err)
			}
			if err = Migrate(db); err != nil {
				t.Fatal(err)
			}
			var actual models.BTSelectionGroup
			if err = db.First(&actual, "id = ?", g.ID).Error; err != nil || actual.Revision != 2 || actual.Workspace != g.Workspace {
				t.Fatal("migration changed registry", err)
			}
		})
	}
}
