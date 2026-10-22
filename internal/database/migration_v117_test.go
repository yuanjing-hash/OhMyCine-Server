package database

import (
	"reflect"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationQuarkPreservesLegacyRowsConstraintsAndIdentity(t *testing.T) {
	db, err := Open(t.TempDir() + "/quark-upgrade.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 115)
	connection := models.Connection{Name: "legacy", NameNormalized: "legacy", Provider: models.ConnectionProviderPan115, CredentialCiphertext: "unchanged-encrypted115", Enabled: true, Revision: 9}
	if err := db.Create(&connection).Error; err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "legacy", NameNormalized: "legacy", Type: models.StorageTypePan115, RootPath: "42", RootPathNormalized: "115:1:42", ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var before models.Storage
	if err := db.First(&before, storage.ID).Error; err != nil {
		t.Fatal(err)
	}
	// Retain identities already issued even if the highest row was removed.
	if err := db.Exec("UPDATE sqlite_sequence SET seq=100 WHERE name='storages'").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var after models.Storage
	if err := db.First(&after, storage.ID).Error; err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("legacy storage changed: %v", err)
	}
	var gotConnection models.Connection
	if err := db.First(&gotConnection, connection.ID).Error; err != nil || gotConnection.CredentialCiphertext != connection.CredentialCiphertext || gotConnection.Revision != 9 {
		t.Fatalf("legacy credential/revision changed: %v", err)
	}
	quark := models.Storage{Name: "quark", NameNormalized: "quark", Type: models.StorageTypeQuark, RootPath: "tv:root", RootPathNormalized: "quark:1:root", ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err := db.Create(&quark).Error; err != nil || quark.ID <= 100 {
		t.Fatalf("new provider/identity: %d %v", quark.ID, err)
	}
	if err := db.Model(&quark).Update("type", "unrecognized").Error; err == nil {
		t.Fatal("unknown storage accepted")
	}
	if err := db.Delete(&connection).Error; err == nil {
		t.Fatal("connection foreign key lost")
	}
	duplicate := quark
	duplicate.ID = 0
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("unique storage identity lost")
	}
	var violations int64
	if err := db.Raw("SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&violations).Error; err != nil || violations != 0 {
		t.Fatalf("foreign key violations %d: %v", violations, err)
	}
}

func TestMigrationQuarkRetainsSequenceAfterAllRowsWereDeleted(t *testing.T) {
	db, err := Open(t.TempDir() + "/empty-upgrade.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 115)
	if err := db.Exec("INSERT INTO sqlite_sequence(name,seq) VALUES ('storages',812)").Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	row := models.Storage{Name: "local", NameNormalized: "local", Type: models.StorageTypeLocal, RootPath: "/media", RootPathNormalized: "/media", Enabled: true, Capabilities: "{}"}
	if err := db.Create(&row).Error; err != nil || row.ID <= 812 {
		t.Fatalf("retired ID reused: %d %v", row.ID, err)
	}
}
