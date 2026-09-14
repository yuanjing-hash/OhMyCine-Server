package database

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMigrationV103AddsRemoteNodeStateWithoutMovingLocalDownloaders(t *testing.T) {
	db, err := Open(t.TempDir() + "/migration-v103.db")
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	applyMigrationsThrough(t, db, 102)

	now := time.Now().UTC()
	owner := models.User{
		Username:           "v103-owner",
		UsernameNormalized: "v103-owner-" + uuid.NewString(),
		DisplayName:        "v103 owner",
		PasswordHash:       "x",
		Status:             models.UserStatusActive,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	downloader := models.Downloader{
		ID:               uuid.NewString(),
		OwnerID:          owner.ID,
		Name:             "Local qBittorrent",
		NameNormalized:   "v103-local-" + uuid.NewString(),
		Type:             models.DownloaderTypeQBittorrent,
		BaseURL:          "http://127.0.0.1:8080",
		Enabled:          true,
		CapabilitiesJSON: `{}`,
		LastHealthStatus: "unknown",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := db.Omit("ExecutionLocation", "NodeID", "NodeName").Create(&downloader).Error; err != nil {
		t.Fatal(err)
	}

	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"transfer_nodes",
		"node_enrollments",
		"node_downloader_bindings",
		"remote_operations",
		"node_credential_grants",
		"node_controller_identities",
		"transfer_node_settings",
	} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("v103 table %s missing", table)
		}
	}
	for _, check := range []struct {
		model any
		name  string
	}{
		{&models.Downloader{}, "execution_location"},
		{&models.DownloadTask{}, "execution_location"},
		{&models.TransferTask{}, "execution_location"},
		{&models.SeedingTask{}, "execution_location"},
		{&models.SeedingTask{}, "node_id"},
		{&models.TransferNode{}, "encryption_public_key"},
	} {
		if !db.Migrator().HasColumn(check.model, check.name) {
			t.Fatalf("v103 column %s missing", check.name)
		}
	}
	var preserved models.Downloader
	if err := db.First(&preserved, "id = ?", downloader.ID).Error; err != nil {
		t.Fatal(err)
	}
	if preserved.ExecutionLocation != models.NodeLocationServer || preserved.NodeID != nil || preserved.NodeName != "" {
		t.Fatalf("legacy downloader was moved to a node: %+v", preserved)
	}
	var settings models.TransferNodeSettings
	if err := db.First(&settings, 1).Error; err != nil {
		t.Fatal(err)
	}
	if settings.DefaultNodeID != nil || settings.Revision != 1 {
		t.Fatalf("unexpected default node settings: %+v", settings)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Table("schema_migrations").Where("version = ?", 103).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("v103 migration count=%d err=%v", count, err)
	}
}
