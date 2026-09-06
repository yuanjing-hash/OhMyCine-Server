package services

import (
	"gorm.io/gorm"
	"strings"
)

func assertCatalogConversionDrainedTx(tx *gorm.DB, libraryID uint) error {
	if err := requireMediaLibraryNotRetiringTx(tx, libraryID); err != nil {
		return err
	}
	return AssertCatalogPhysicalDrainedTx(tx, libraryID)
}

// AssertCatalogPhysicalDrainedTx must be in the SAME writer that creates the
// conversion/retirement fence. Queries include pre-ledger owners: cancellation,
// supersession, an expired lease or an old timestamp is never drain evidence.
func AssertCatalogPhysicalDrainedTx(tx *gorm.DB, libraryID uint) error {
	return assertCatalogPhysicalScopeDrainedTx(tx, "id=?", libraryID)
}

// scope is an internal fixed SQL predicate, never a request string. Using the
// indexed library subquery keeps Profile/Storage precommit checks in the SAME
// writer without materializing every catalog or delaying validation until a
// notifier runs after the global configuration is already committed.
func assertCatalogPhysicalScopeDrainedTx(tx *gorm.DB, scope string, id uint) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if id == 0 {
		return ErrCatalogInvalid
	}
	libraryID := id
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT id FROM catalog_physical_writes WHERE library_id=? AND state IN ('entered','quiescent') LIMIT 1`, []any{libraryID}},
		{`SELECT id FROM media_artifacts WHERE library_id=? AND managed=1 AND status='cleanup' LIMIT 1`, []any{libraryID}},
		{`SELECT id FROM catalog_artifact_write_receipts WHERE library_id=? AND phase IN ('prepared','conflict') LIMIT 1`, []any{libraryID}},
		{`SELECT artifact_id FROM catalog_artifact_cleanup_claims WHERE library_id=? LIMIT 1`, []any{libraryID}},
		{`SELECT t.id FROM transfer_tasks t WHERE t.library_id=? AND (t.phase<>'completed' OR t.finished_at IS NULL) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='transfer' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}},
		{`SELECT t.id FROM media_library_structure_repairs t WHERE t.library_id=? AND (t.phase<>'completed' OR t.finished_at IS NULL) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='repair' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}},
		{`SELECT t.id FROM media_reorganization_tasks t WHERE t.library_id=? AND (t.phase<>'completed' OR t.finished_at IS NULL) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='reorganization' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}},
		{`SELECT t.id FROM media_artifact_runs t WHERE t.library_id=? AND NOT (` + catalogLegacyArtifactCompletionSQL + `) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='artifact' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}},
		{`SELECT t.id FROM media_catalog_deletion_previews t WHERE t.library_id=? AND t.consumed_at IS NULL AND (t.started_at IS NOT NULL OR t.last_error_code<>'') AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='catalog_deletion' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}},
		{`SELECT t.id FROM transfer_deletion_previews t WHERE t.library_id=? AND t.consumed_at IS NOT NULL AND t.completed_at IS NULL AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='transfer_deletion' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}},
	}
	// Old domain completion alone cannot erase an active physical lease. New
	// admitted/settled receipts are stronger evidence and deliberately exempt.
	for _, domain := range []struct{ table, kind string }{{"transfer_tasks", "transfer"}, {"media_library_structure_repairs", "repair"}, {"media_reorganization_tasks", "reorganization"}, {"media_artifact_runs", "artifact"}} {
		queries = append(queries, struct {
			sql  string
			args []any
		}{`SELECT t.id FROM ` + domain.table + ` t JOIN jobs j ON j.id=t.job_id WHERE t.library_id=? AND (j.status='running' OR j.lease_token_hash<>'' OR j.lease_expires_at IS NOT NULL) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='` + domain.kind + `' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}})
	}
	for _, query := range queries {
		var id string
		sql := query.sql
		if scope != "id=?" {
			sql = strings.ReplaceAll(sql, "library_id=?", "library_id IN (SELECT id FROM media_libraries WHERE "+scope+")")
		}
		if err := tx.Raw(sql, query.args...).Scan(&id).Error; err != nil {
			return err
		}
		if id != "" {
			return catalogPhysicalUnsettledError()
		}
	}
	return nil
}

// Older generators without an attached cleanup service could finish their
// Job while leaving cleanup_status at its default. Accept only an explicitly
// completed, lease-free Job and successful domain output, plus either a cleanup
// receipt or immutable scan facts proving automatic cleanup inapplicable.
// JSON CASE avoids malformed historical policy text raising a generic SQL 500.
const catalogLegacyArtifactCompletionSQL = `t.status='completed' AND t.finished_at IS NOT NULL AND t.failed_count=0 AND t.error_code='' AND t.cleanup_error_code='' AND (
 (t.cleanup_status IN ('completed','skipped'))
 OR (t.cleanup_status IN ('','pending')
  AND EXISTS (SELECT 1 FROM jobs j WHERE j.id=t.job_id AND j.job_type='media_artifact' AND j.status='completed' AND j.finished_at IS NOT NULL AND j.lease_token_hash='' AND j.lease_expires_at IS NULL)
  AND CASE WHEN json_valid(t.policy_json) THEN (
   COALESCE(json_extract(t.policy_json,'$.library_id'),0)=t.library_id
   AND COALESCE(json_extract(t.policy_json,'$.generation'),0)=t.generation
   AND (COALESCE(json_extract(t.policy_json,'$.scan_run_id'),0)=0
    OR COALESCE(json_extract(t.policy_json,'$.scan_partial'),0)=1
    OR (COALESCE(json_extract(t.policy_json,'$.cleanup_eligible'),0)=0 AND COALESCE(json_extract(t.policy_json,'$.target_kind'),'')='local_projection'))
  ) ELSE 0 END)
)`
