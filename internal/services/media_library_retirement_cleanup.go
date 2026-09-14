package services

import (
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Every entry is one independent transaction, not SQL chunks nested in a
// library-sized writer. Predicates are compile-time identifiers only.
type libraryRetirementCleanupStep struct {
	table, predicate, update string
	anchor                   bool
}

func (step libraryRetirementCleanupStep) run(tx *gorm.DB, libraryID uint) (int64, error) {
	sql := "DELETE FROM " + step.table
	if step.update != "" {
		sql = "UPDATE " + step.table + " SET " + step.update
	}
	result := tx.Exec(sql+" WHERE rowid IN (SELECT rowid FROM "+step.table+" WHERE "+step.predicate+" ORDER BY rowid LIMIT ?)", libraryID, CatalogBatchRows)
	return result.RowsAffected, result.Error
}

var libraryRetirementCleanup = []libraryRetirementCleanupStep{
	{"media_library_structure_repair_items", "repair_id IN (SELECT id FROM media_library_structure_repairs WHERE library_id=?)", "", false},
	{"media_library_structure_repair_draft_preview_items", "draft_id IN (SELECT id FROM media_library_structure_repair_drafts WHERE library_id=?)", "", false},
	{"catalog_structure_directory_receipts", "repair_id IN (SELECT id FROM media_library_structure_repairs WHERE library_id=?)", "", false},
	{"media_library_structure_issue_members", "issue_id IN (SELECT id FROM media_library_structure_issues WHERE library_id=?)", "", false},
	{"media_library_structure_review_choices", "session_id IN (SELECT id FROM media_library_structure_review_sessions WHERE library_id=?)", "", false},
	{"media_library_structure_review_sessions", "library_id=?", "", false},
	{"media_library_structure_issues", "library_id=?", "", false},
	{"media_library_structure_repair_drafts", "library_id=?", "", false},
	{"media_library_structure_diagnoses", "library_id=?", "", false},
	{"media_library_structure_auto_states", "library_id=?", "", false},
	{"media_library_structure_repairs", "library_id=?", "", false},
	{"media_reorganization_previews", "library_id=?", "", false},
	{"media_reorganization_tasks", "library_id=?", "", false},
	{"transfer_deletion_previews", "library_id=?", "", false},
	{"media_catalog_deletion_previews", "library_id=?", "", false},
	{"media_library_scan_stagings", "library_id=?", "", false},
	{"media_library_scan_runs", "library_id=?", "", false},
	{"catalog_artifact_write_receipts", "library_id=?", "", false},
	{"catalog_artifact_cleanup_claims", "library_id=?", "", false},
	{"media_artifacts", "library_id=?", "", false},
	{"media_artifact_runs", "library_id=?", "", false},
	{"catalog_artifact_binding_items", "binding_id IN (SELECT id FROM catalog_artifact_bindings WHERE library_id=?)", "", false},
	{"catalog_artifact_bindings", "library_id=?", "", false},
	{"media_category_artworks", "library_id=?", "", false},
	{"media_library_provider_events", "library_id=?", "", false},
	{"catalog_scan_followups", "library_id=?", "", false},
	{"media_change_dispatches", "library_id=?", "", false},
	{"media_change_pending_cleanups", "library_id=?", "", false},
	{"media_library_changes", "library_id=?", "", false},
	{"media_server_refresh_runs", "target_id IN (SELECT id FROM media_server_refresh_targets WHERE library_id=?)", "", false},
	{"media_server_refresh_targets", "library_id=?", "", false},
	{"schedule_runs", "schedule_id IN (SELECT id FROM schedule_definitions WHERE target_type='media_library' AND target_id=CAST(? AS TEXT))", "", false},
	{"schedule_definitions", "target_type='media_library' AND target_id=CAST(? AS TEXT)", "", false},
	{"media_managed_items", "library_id=?", "", false},
	// Transfer previews, reorganizations and managed items are already gone, so
	// the target-library execution row can now be removed without touching its
	// upstream Download or any source/provider files.
	{"transfer_tasks", "library_id=?", "", false},
	{"player_media_favorites", "library_id=?", "", false},
	{"player_media_collection_items", "library_id=?", "", false},
	{"media_acquisitions", "target_library_id=?", "target_library_id=NULL", false},
	{"media_library_entries", "library_id=?", "", true},
	{"media_library_source_assets", "library_id=?", "", true},
	{"media_library_recognitions", "library_id=?", "", true},
	{"catalog_identities", "library_id=?", "", true},
	{"catalog_physical_writes", "library_id=?", "", true},
}

var libraryRetirementJobCleanupTables = []string{
	"job_attempts",
	"job_status_events",
	"job_action_requests",
	"notification_receipts",
}

func cleanupLibraryRetirementJobsTx(tx *gorm.DB, row models.MediaLibraryRetirement) (int64, error) {
	for _, table := range libraryRetirementJobCleanupTables {
		result := tx.Exec("DELETE FROM "+table+" WHERE rowid IN (SELECT d.rowid FROM "+table+" d JOIN media_library_retirement_jobs r ON r.job_id=d.job_id WHERE r.retirement_id=? ORDER BY d.rowid LIMIT ?)", row.ID, CatalogBatchRows)
		if result.Error != nil || result.RowsAffected != 0 {
			return result.RowsAffected, result.Error
		}
	}
	result := tx.Exec("DELETE FROM jobs WHERE rowid IN (SELECT j.rowid FROM jobs j JOIN media_library_retirement_jobs r ON r.job_id=j.id WHERE r.retirement_id=? ORDER BY j.rowid LIMIT ?)", row.ID, CatalogBatchRows)
	if result.Error != nil || result.RowsAffected != 0 {
		return result.RowsAffected, result.Error
	}
	result = tx.Exec("DELETE FROM media_library_retirement_jobs WHERE rowid IN (SELECT rowid FROM media_library_retirement_jobs WHERE retirement_id=? ORDER BY rowid LIMIT ?)", row.ID, CatalogBatchRows)
	return result.RowsAffected, result.Error
}

// Assert all reviewed CASCADE/SET NULL children are empty before the single
// config delete. A schema inventory test requires every new FK to join this
// reviewed list, rather than silently performing a large cascade at finalization.
func assertLibraryRetirementEmptyTx(tx *gorm.DB, libraryID uint) error {
	for _, step := range libraryRetirementCleanup {
		var found int
		if err := tx.Raw("SELECT 1 FROM "+step.table+" WHERE "+step.predicate+" LIMIT 1", libraryID).Scan(&found).Error; err != nil {
			return err
		}
		if found != 0 {
			return ErrCatalogFence
		}
	}
	for _, table := range []string{"catalog_head_layers", "catalog_snapshots", "catalog_physical_writes", "catalog_artifact_write_receipts"} {
		var found int
		if err := tx.Raw("SELECT 1 FROM "+table+" WHERE library_id=? LIMIT 1", libraryID).Scan(&found).Error; err != nil {
			return err
		}
		if found != 0 {
			return ErrCatalogFence
		}
	}
	var ownedJob int
	if err := tx.Raw("SELECT 1 FROM media_library_retirement_jobs WHERE retirement_id=(SELECT id FROM media_library_retirements WHERE library_id=?) LIMIT 1", libraryID).Scan(&ownedJob).Error; err != nil {
		return err
	}
	if ownedJob != 0 {
		return ErrCatalogFence
	}
	return nil
}
