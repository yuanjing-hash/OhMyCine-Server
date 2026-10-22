package database

import (
	"fmt"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Rebuild only the declared enum constraint, retaining every existing column,
// index, trigger and row. Foreign keys are disabled on the migration connection
// by the normal migration runner, and checked again before startup continues.
func extendQuarkTypeConstraint(db *gorm.DB, table, old, replacement string) error {
	if table != "storages" && table != "downloaders" {
		return fmt.Errorf("unsupported cloud migration table")
	}
	var schema string
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&schema).Error; err != nil {
		return err
	}
	if strings.Contains(schema, replacement) {
		return nil
	}
	if strings.Count(schema, old) != 1 {
		return fmt.Errorf("unexpected %s type constraint", table)
	}
	start := strings.IndexByte(schema, '(')
	if start < 0 {
		return fmt.Errorf("invalid %s table definition", table)
	}
	temporary := table + "_quark_upgrade"
	definition := "CREATE TABLE " + temporary + " " + strings.Replace(schema[start:], old, replacement, 1)
	var ancillary []struct{ SQL string }
	if err := db.Raw("SELECT sql FROM sqlite_master WHERE tbl_name=? AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY type,name", table).Scan(&ancillary).Error; err != nil {
		return err
	}
	var columns []struct{ Name string }
	if err := db.Raw("PRAGMA table_info(" + table + ")").Scan(&columns).Error; err != nil {
		return err
	}
	if len(columns) == 0 {
		return fmt.Errorf("missing %s columns", table)
	}
	names := make([]string, len(columns))
	for i, column := range columns {
		names[i] = `"` + strings.ReplaceAll(column.Name, `"`, `""`) + `"`
	}
	projection := strings.Join(names, ",")
	var priorSequence int64
	if err := db.Raw("SELECT COALESCE(MAX(seq),0) FROM sqlite_sequence WHERE name=?", table).Scan(&priorSequence).Error; err != nil {
		return err
	}
	for _, statement := range []string{definition, "INSERT INTO " + temporary + " (" + projection + ") SELECT " + projection + " FROM " + table, "DROP TABLE " + table, "ALTER TABLE " + temporary + " RENAME TO " + table} {
		if err := db.Exec(statement).Error; err != nil {
			return err
		}
	}
	for _, item := range ancillary {
		if err := db.Exec(item.SQL).Error; err != nil {
			return err
		}
	}
	if priorSequence > 0 {
		if err := db.Exec("UPDATE sqlite_sequence SET seq=MAX(seq,?) WHERE name=?", priorSequence, table).Error; err != nil {
			return err
		}
		if err := db.Exec("INSERT INTO sqlite_sequence(name,seq) SELECT ?,? WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name=?)", table, priorSequence, table).Error; err != nil {
			return err
		}
	}
	return nil
}

func migrateQuarkProviderTypes(db *gorm.DB) error {
	if err := extendQuarkTypeConstraint(db, "storages", "('local','pan115')", "('local','pan115','quark')"); err != nil {
		return err
	}
	return extendQuarkTypeConstraint(db, "downloaders", "('fake','qbittorrent','pan115_offline')", "('fake','qbittorrent','pan115_offline','quark_share')")
}

func migrateProviderObservations(db *gorm.DB) error {
	if err := db.AutoMigrate(&models.ProviderObservationScope{}, &models.ProviderObservationBatch{}, &models.ProviderObservationItem{}, &models.ProviderObservationAccountBudget{}, &models.QuarkOperationReceipt{}, &models.ProviderObservationIntakeClaim{}); err != nil {
		return err
	}
	// Existing remote upload tables have handwritten constraints. Add only this
	// compatible column; do not ask GORM to rebuild their foreign-key schema.
	if !db.Migrator().HasColumn(&models.RemoteUploadFile{}, "target_digest_json") {
		return db.Exec("ALTER TABLE remote_upload_files ADD COLUMN target_digest_json TEXT NOT NULL DEFAULT ''").Error
	}
	return nil
}
