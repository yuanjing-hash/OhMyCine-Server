package database

import (
	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"strings"
	"testing"
	"time"
)

func TestMigrationV116PreservesCredentialsAndStoresOnlyObservedSummary(t *testing.T) {
	db, err := Open(t.TempDir() + "/migration-v116.db")
	if err != nil {
		t.Fatal(err)
	}
	connection, _ := db.DB()
	t.Cleanup(func() { connection.Close() })
	applyMigrationsThrough(t, db, 115)
	now := time.Now().UTC()
	pkg := models.PluginPackage{PluginID: "org.example.fixture", Version: "0.1.0", ManifestJSON: `{}`, PackageSHA256: strings.Repeat("a", 64), CreatedAt: now, VerifiedAt: now}
	if err := db.Create(&pkg).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.PluginInstallation{PluginID: pkg.PluginID, ActivePackageID: pkg.ID, Status: models.PluginInstallationEnabled, Revision: 1, RuntimeGeneration: 1, InstalledAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if err := db.Exec(`INSERT INTO plugin_connections(id,plugin_id,name,credential_ciphertext,credential_version,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, id, pkg.PluginID, "Fixture", "preserved-ciphertext", 7, 3, now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var record models.PluginConnection
	if err := db.First(&record, "id = ?", id).Error; err != nil || record.AccountSummaryJSON != "" || record.AccountCheckedAt != nil || record.CredentialCiphertext != "preserved-ciphertext" || record.CredentialVersion != 7 || record.Revision != 3 {
		t.Fatalf("legacy credential changed: %+v %v", record, err)
	}
	if err := db.Model(&record).Updates(map[string]any{"account_summary_json": `{"id":"a","name":"Fixture","membership":{"status":"unknown"}}`, "account_checked_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&record, "id = ?", id).Error; err != nil || record.AccountSummaryJSON == "" || record.AccountCheckedAt == nil {
		t.Fatal("repeated migration erased observed summary")
	}
}
