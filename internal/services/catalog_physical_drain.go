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
	// Queue cancellation/history retention and physical retirement share the
	// same evidence boundary. Close only owners with durable terminal Job state
	// and positive proof that no external call was entered; ambiguous owners are
	// deliberately left untouched and continue to block below.
	if err := finalizeTerminalNoIOCatalogScopeTx(tx, scope, id); err != nil {
		return err
	}
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT id FROM catalog_physical_writes WHERE library_id=? AND state IN ('entered','quiescent') LIMIT 1`, []any{libraryID}},
		{`SELECT id FROM media_artifacts WHERE library_id=? AND managed=1 AND status='cleanup' LIMIT 1`, []any{libraryID}},
		{`SELECT id FROM catalog_artifact_write_receipts WHERE library_id=? AND phase IN ('prepared','conflict') LIMIT 1`, []any{libraryID}},
		{`SELECT artifact_id FROM catalog_artifact_cleanup_claims WHERE library_id=? LIMIT 1`, []any{libraryID}},
		{`SELECT t.id FROM transfer_tasks t WHERE t.library_id=? AND NOT (` + catalogTransferCompletionSQL + `) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='transfer' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}},
		{`SELECT t.id FROM media_library_structure_repairs t WHERE t.library_id=? AND (t.phase<>'completed' OR t.finished_at IS NULL) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='repair' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}},
		{`SELECT t.id FROM media_reorganization_tasks t WHERE t.library_id=? AND (t.phase<>'completed' OR t.finished_at IS NULL) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='reorganization' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}},
		{`SELECT t.id FROM media_artifact_runs t WHERE t.library_id=? AND NOT (` + catalogArtifactTerminalCompletionSQL + `) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='artifact' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, []any{libraryID}},
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

// A failed transfer is a terminal no-I/O fact only when every independently
// persisted boundary agrees: its terminal lease-free Job has finished, the
// executor never persisted a plan/provider checkpoint or processed a file, no
// managed output exists, and the physical-write ledger has no owner at all.
// Counters alone are intentionally insufficient.
const catalogTransferTerminalNoIOSQL = `t.phase='failed' AND t.processed_files=0 AND COALESCE(t.plan_summary_json,'')='' AND COALESCE(t.cloud_state_json,'')='' AND t.cleanup_removed=0 AND COALESCE(t.cleanup_error_code,'')='' AND EXISTS (SELECT 1 FROM jobs j WHERE j.id=t.job_id AND j.job_type='transfer' AND json_valid(j.payload_json) AND json_type(j.payload_json,'$.transfer_task_id')='text' AND json_extract(j.payload_json,'$.transfer_task_id')=t.id AND j.status IN ('completed','failed','cancelled') AND j.finished_at IS NOT NULL AND j.lease_token_hash='' AND j.lease_expires_at IS NULL AND j.interrupt_status='') AND NOT EXISTS (SELECT 1 FROM media_managed_items m WHERE m.transfer_task_id=t.id) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='transfer' AND p.owner_id=t.id)`

const catalogTransferCompletionSQL = `(t.phase='completed' AND t.finished_at IS NOT NULL) OR (` + catalogTransferTerminalNoIOSQL + `)`

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

// A failed artifact run is terminal no-I/O only when its exact queue Job is
// durably terminal and lease-free, every item counter is zero, and no physical,
// manifest, receipt or cleanup evidence exists. The error code is deliberately
// irrelevant: this is a positive evidence classifier, not a historical-error
// compatibility list.
const catalogArtifactFailedTerminalNoIOSQL = `t.status='failed' AND t.finished_at IS NOT NULL AND t.cleanup_status='skipped' AND t.cleanup_error_code='' AND t.expected_count=0 AND t.written_count=0 AND t.updated_count=0 AND t.removed_count=0 AND t.skipped_count=0 AND t.failed_count=0 AND t.processed_count=0 AND t.succeeded_count=0 AND EXISTS (SELECT 1 FROM jobs j WHERE j.id=t.job_id AND j.job_type='media_artifact' AND json_valid(j.payload_json) AND json_type(j.payload_json,'$.artifact_run_id')='text' AND json_extract(j.payload_json,'$.artifact_run_id')=t.id AND j.status IN ('completed','failed','cancelled') AND j.finished_at IS NOT NULL AND j.lease_token_hash='' AND j.lease_expires_at IS NULL AND j.interrupt_status='') AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='artifact' AND p.owner_id=t.id) AND NOT EXISTS (SELECT 1 FROM media_artifacts a WHERE a.run_id=t.id) AND NOT EXISTS (SELECT 1 FROM catalog_artifact_write_receipts r WHERE r.run_id=t.id) AND NOT EXISTS (SELECT 1 FROM catalog_artifact_cleanup_claims c WHERE c.owner_run_id=t.id)`

// Superseded+finished+cleanup-skipped is an explicit no-more-work outcome, but
// old builds may have persisted positive progress counters before a later
// generation replaced the run. Counters are not ownership evidence. Such a run
// is terminal only when a strictly newer generation is the library's completed
// applied generation and every independent physical/recovery owner has gone.
// This intentionally does not key on a historical error code.
const catalogArtifactSupersededReconciledSQL = `t.status='superseded' AND t.finished_at IS NOT NULL AND t.cleanup_status='skipped' AND t.cleanup_error_code='' AND EXISTS (SELECT 1 FROM media_libraries l WHERE l.id=t.library_id AND l.artifact_applied_generation>t.generation AND l.artifact_generation=l.artifact_applied_generation AND l.artifact_status='completed') AND (t.job_id IS NULL OR EXISTS (SELECT 1 FROM jobs j WHERE j.id=t.job_id AND j.job_type='media_artifact' AND j.status IN ('completed','failed','cancelled') AND j.finished_at IS NOT NULL AND j.lease_token_hash='' AND j.lease_expires_at IS NULL AND j.interrupt_status='')) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='artifact' AND p.owner_id=t.id) AND NOT EXISTS (SELECT 1 FROM media_artifacts a WHERE a.run_id=t.id) AND NOT EXISTS (SELECT 1 FROM catalog_artifact_write_receipts r WHERE r.run_id=t.id) AND NOT EXISTS (SELECT 1 FROM catalog_artifact_cleanup_claims c WHERE c.owner_run_id=t.id)`

const catalogArtifactTerminalCompletionSQL = `(` + catalogLegacyArtifactCompletionSQL + `) OR (` + catalogArtifactFailedTerminalNoIOSQL + `) OR (` + catalogArtifactSupersededReconciledSQL + `)`
