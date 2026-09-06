package database

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructurePreviewMigrationV76PreservesExistingDrafts(t *testing.T) {
	db := structureMigrationDB(t, 75)
	library := seedStructureMigrationLibrary(t, db, 75, "preview", "healthy", 0, 0)
	user := models.User{Username: "preview", UsernameNormalized: "preview", PasswordHash: "unused", Status: models.UserStatusActive}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Exec(`INSERT INTO media_library_structure_repair_drafts
		(id,owner_id,library_id,diagnosis_job_id,source_revision,generation,rule_fingerprint,plan_hash,selections_json,expires_at,created_at)
		VALUES ('old-preview',?,?, 'old-job',7,9,'old-rule','old-hash','{"selections":[]}',?,?)`, user.ID, library.ID, now.Add(time.Hour), now).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var draft models.MediaLibraryStructureRepairDraft
	if err := db.First(&draft, "id = ?", "old-preview").Error; err != nil {
		t.Fatal(err)
	}
	if draft.PreviewItemsJSON != "" || draft.PlanHash != "old-hash" || draft.RuleFingerprint != "old-rule" || draft.Generation != 9 || draft.SourceRevision != 7 || draft.ConsumedAt != nil || draft.SelectionsJSON != `{"selections":[]}` {
		t.Fatalf("migration modified confirmation authority: %+v", draft)
	}
	if err := db.Model(&draft).Update("preview_items_json", `[{"current_path":"private/path"}]`).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&draft, "id = ?", "old-preview").Error; err != nil || draft.PreviewItemsJSON != `[{"current_path":"private/path"}]` {
		t.Fatalf("repeat changed manifest: %v", err)
	}
	raw, err := json.Marshal(draft)
	if err != nil || string(raw) != "{}" {
		t.Fatalf("private draft serialized: %s, %v", raw, err)
	}
	var versions int64
	if err := db.Table("schema_migrations").Where("version = 76").Count(&versions).Error; err != nil || versions != 1 {
		t.Fatalf("v76 count=%d err=%v", versions, err)
	}
}
