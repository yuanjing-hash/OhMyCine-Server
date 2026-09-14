package services

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func terminalLeaseFreeCatalogJob(job models.Job) bool {
	return containsString(historyTerminalStatuses(), job.Status) && job.FinishedAt != nil && job.LeaseTokenHash == "" && job.LeaseExpiresAt == nil && job.InterruptStatus == ""
}

// finalizeTerminalNoIOCatalogJobTx performs lifecycle closure using positive
// no-I/O evidence only. An admitted ledger row is immutable proof that the
// owner was registered but never entered an external call. Legacy transfers
// have no such row, so their stricter preflight facts must all agree.
func finalizeTerminalNoIOCatalogJobTx(tx *gorm.DB, jobID string) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	if strings.TrimSpace(jobID) == "" {
		return nil
	}
	var job models.Job
	if err := tx.First(&job, "id = ?", jobID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if !terminalLeaseFreeCatalogJob(job) {
		return nil
	}
	transferTaskID := ""
	if job.JobType == "transfer" {
		var payload map[string]json.RawMessage
		if json.Unmarshal([]byte(job.PayloadJSON), &payload) == nil && len(payload) == 1 {
			_ = json.Unmarshal(payload["transfer_task_id"], &transferTaskID)
		}
	}
	if transferTaskID != "" {
		// No ledger means the legacy worker never reached its persisted plan or
		// provider checkpoint. An admitted ledger is even stronger: this exact
		// owner was registered but never entered an external call.
		if err := tx.Exec(`UPDATE transfer_tasks AS t SET phase='failed', last_error_code=CASE WHEN COALESCE(t.last_error_code,'')='' THEN 'transfer_cancelled' ELSE t.last_error_code END, finished_at=COALESCE(t.finished_at, ?), updated_at=CASE WHEN t.updated_at<? THEN ? ELSE t.updated_at END WHERE t.id=? AND t.job_id=? AND t.phase IN ('queued','planning','failed') AND t.processed_files=0 AND COALESCE(t.plan_summary_json,'')='' AND COALESCE(t.cloud_state_json,'')='' AND t.cleanup_removed=0 AND COALESCE(t.cleanup_error_code,'')='' AND NOT EXISTS (SELECT 1 FROM media_managed_items m WHERE m.transfer_task_id=t.id) AND (NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='transfer' AND p.owner_id=t.id) OR EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='transfer' AND p.owner_id=t.id AND p.job_id=? AND p.state='admitted' AND p.job_lease_hash=''))`, job.FinishedAt, *job.FinishedAt, *job.FinishedAt, transferTaskID, jobID, jobID).Error; err != nil {
			return err
		}
	}

	var admitted []models.CatalogPhysicalWrite
	if err := tx.Where("job_id = ? AND state = 'admitted' AND job_lease_hash = ''", jobID).Order("id").Find(&admitted).Error; err != nil {
		return err
	}
	for _, proof := range admitted {
		if err := settleAdmittedCatalogPhysicalWriteTx(tx, proof); err != nil {
			return err
		}
	}

	return nil
}

func finalizeTerminalNoIOCatalogScopeTx(tx *gorm.DB, scope string, id uint) error {
	if err := requireCatalogTransaction(tx); err != nil {
		return err
	}
	predicate := "library_id=?"
	if scope != "id=?" {
		predicate = "library_id IN (SELECT id FROM media_libraries WHERE " + scope + ")"
	}
	query := `SELECT DISTINCT job_id FROM (` +
		`SELECT job_id,library_id FROM catalog_physical_writes WHERE job_id<>'' AND ` + predicate + ` UNION ALL ` +
		`SELECT job_id,library_id FROM transfer_tasks WHERE job_id<>'' AND ` + predicate +
		`) owners WHERE job_id<>''`
	var jobIDs []string
	if err := tx.Raw(query, id, id).Scan(&jobIDs).Error; err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		if err := finalizeTerminalNoIOCatalogJobTx(tx, jobID); err != nil {
			return err
		}
	}
	return nil
}

// assertCatalogJobDomainSettledTx is the job-scoped counterpart of the media
// library drain. History may be hidden only when doing so cannot conceal a
// domain row that would still block retirement.
func assertCatalogJobDomainSettledTx(tx *gorm.DB, jobID string) error {
	queries := []string{
		`SELECT t.id FROM transfer_tasks t WHERE t.job_id=? AND NOT (` + catalogTransferCompletionSQL + `) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='transfer' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`,
		`SELECT t.id FROM media_library_structure_repairs t WHERE t.job_id=? AND (t.phase<>'completed' OR t.finished_at IS NULL) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='repair' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`,
		`SELECT t.id FROM media_reorganization_tasks t WHERE t.job_id=? AND (t.phase<>'completed' OR t.finished_at IS NULL) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='reorganization' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`,
		`SELECT t.id FROM media_artifact_runs t WHERE t.job_id=? AND NOT (` + catalogArtifactTerminalCompletionSQL + `) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='artifact' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`,
	}
	for _, query := range queries {
		var ownerID string
		if err := tx.Raw(query, jobID).Scan(&ownerID).Error; err != nil {
			return err
		}
		if ownerID != "" {
			return errHistoryRecoveryBearing
		}
	}
	return nil
}

func assertCatalogArtifactRunDomainSettledTx(tx *gorm.DB, runID string) error {
	var blocker string
	if err := tx.Raw(`SELECT t.id FROM media_artifact_runs t WHERE t.id=? AND NOT (`+catalogArtifactTerminalCompletionSQL+`) AND NOT EXISTS (SELECT 1 FROM catalog_physical_writes p WHERE p.owner_kind='artifact' AND p.owner_id=t.id AND p.state IN ('admitted','settled')) LIMIT 1`, runID).Scan(&blocker).Error; err != nil {
		return err
	}
	if blocker != "" {
		return errHistoryRecoveryBearing
	}
	return nil
}
