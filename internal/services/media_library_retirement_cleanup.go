package services

import "gorm.io/gorm"

// Every entry is one independent transaction, not SQL chunks nested in a
// library-sized writer. Predicates are compile-time identifiers only.
type libraryRetirementCleanupStep struct {
	table, predicate, update string
	anchor                   bool
}

func (step libraryRetirementCleanupStep) run(tx *gorm.DB, libraryID uint) (int64, error) {
	if step.table == "catalog_artifact_cleanup_claims" || step.table == "media_artifacts" {
		// A cleanup claim is unresolved execution, including manual/null owners.
		// Never discard it or let the artifact FK CASCADE conceal its existence.
		var claim uint
		if err := tx.Raw("SELECT artifact_id FROM catalog_artifact_cleanup_claims WHERE library_id=? LIMIT 1", libraryID).Scan(&claim).Error; err != nil {
			return 0, err
		}
		if claim != 0 {
			return 0, catalogPhysicalUnsettledError()
		}
		if step.table == "catalog_artifact_cleanup_claims" {
			return 0, nil
		}
	}
	sql := "DELETE FROM " + step.table
	if step.update != "" {
		sql = "UPDATE " + step.table + " SET " + step.update
	}
	result := tx.Exec(sql+" WHERE rowid IN (SELECT rowid FROM "+step.table+" WHERE "+step.predicate+" ORDER BY rowid LIMIT ?)", libraryID, CatalogBatchRows)
	return result.RowsAffected, result.Error
}

var libraryRetirementCleanup = []libraryRetirementCleanupStep{
	{"catalog_structure_directory_receipts", "repair_id IN (SELECT id FROM media_library_structure_repairs WHERE library_id=?)", "", false},
	{"media_library_structure_issue_members", "issue_id IN (SELECT id FROM media_library_structure_issues WHERE library_id=?)", "", false},
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
	{"catalog_artifact_write_receipts", "library_id=? AND phase IN ('reconciled_before','reconciled_after') AND EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.id=physical_write_id AND p.library_id=catalog_artifact_write_receipts.library_id AND p.owner_kind='artifact' AND p.owner_id=run_id AND p.state='settled')", "", false},
	{"catalog_artifact_cleanup_claims", "library_id=?", "", false},
	{"media_artifacts", "library_id=?", "", false},
	{"media_artifact_runs", "library_id=?", "", false},
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
	{"player_media_favorites", "library_id=?", "", false},
	{"player_media_collection_items", "library_id=?", "", false},
	{"media_acquisitions", "target_library_id=?", "target_library_id=NULL", false},
	{"media_library_entries", "library_id=?", "", true},
	{"media_library_source_assets", "library_id=?", "", true},
	{"media_library_recognitions", "library_id=?", "", true},
	{"catalog_identities", "library_id=?", "", true},
	{"catalog_physical_writes", "library_id=? AND state IN ('admitted','settled')", "", true},
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
	return nil
}
