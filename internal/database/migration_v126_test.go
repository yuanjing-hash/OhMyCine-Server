package database

import (
	"bytes"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"testing"
)

func TestMigrationV126ProviderReceiptsPreserveLegacyQuarkBytes(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade125"}[upgrade], func(t *testing.T) {
			db, err := Open(t.TempDir() + "/scope.db")
			if err != nil {
				t.Fatal(err)
			}
			sql, _ := db.DB()
			defer sql.Close()
			if upgrade {
				applyMigrationsThrough(t, db, 125)
			} else {
				if err = Migrate(db); err != nil {
					t.Fatal(err)
				}
			}
			original := models.QuarkOperationReceipt{ID: "old-receipt", ConnectionID: 1, OperationKey: "legacy-key", Adapter: "agent", Kind: "upload", State: "unknown", Revision: 2, CheckpointCiphertext: []byte("existing-quark-ciphertext")}
			if err = db.Create(&original).Error; err != nil {
				t.Fatal(err)
			}
			if err = Migrate(db); err != nil {
				t.Fatal(err)
			}
			if err = Migrate(db); err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasTable(&models.ProviderOperationReceipt{}) {
				t.Fatal("new table missing")
			}
			var actual models.QuarkOperationReceipt
			if err = db.First(&actual, "id=?", original.ID).Error; err != nil || !bytes.Equal(actual.CheckpointCiphertext, original.CheckpointCiphertext) || actual.Revision != original.Revision || actual.State != original.State {
				t.Fatal("old quark receipt changed", err)
			}
		})
	}
}
