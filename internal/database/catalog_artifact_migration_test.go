package database

import "testing"

func TestCatalogArtifactBindingMigrationAdditiveAndEmpty(t *testing.T) {
	db := structureMigrationDB(t, 77)
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"catalog_artifact_bindings", "catalog_snapshot_references", "media_artifacts"} {
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("table=%s count=%d err=%v", table, count, err)
		}
	}
	for _, column := range []string{"finalize_after_id", "recovery_attempts", "layers_json", "source_config_fingerprint"} {
		if !db.Migrator().HasColumn("catalog_artifact_bindings", column) {
			t.Fatalf("missing column %s", column)
		}
	}
	if !db.Migrator().HasColumn("media_artifacts", "catalog_binding_id") {
		t.Fatal("missing artifact binding identity")
	}
	var floor int
	if err := db.Raw("SELECT format FROM catalog_format_floor WHERE id=1").Scan(&floor).Error; err != nil || floor != 0 {
		t.Fatalf("migration activated catalog: floor=%d err=%v", floor, err)
	}
}
