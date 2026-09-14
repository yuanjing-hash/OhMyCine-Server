package database

import (
	"time"

	"gorm.io/gorm"
)

const legacyArtifactNoIOBatch = 250

// ReconcileLegacyArtifactNoIOPhysicalWriteTx closes one legacy artifact
// receipt-v0 physical owner only when the database contains complete positive
// proof that no external write was published. It removes only this run's
// never-written manifest placeholders; it never inspects or changes files.
func ReconcileLegacyArtifactNoIOPhysicalWriteTx(tx *gorm.DB, physicalWriteID uint64) (bool, error) {
	if physicalWriteID == 0 {
		return false, nil
	}
	var candidate struct {
		ID    uint64
		RunID string
	}
	if err := tx.Raw(legacyArtifactNoIOCandidateSQL+` AND p.id=? LIMIT 1`, physicalWriteID).Scan(&candidate).Error; err != nil {
		return false, err
	}
	if candidate.ID == 0 || candidate.RunID == "" {
		return false, nil
	}
	// The candidate query proves every row owned by this run is a queued,
	// fingerprint-free placeholder. Delete only those database placeholders so
	// a later exact incremental run can recreate genuinely missing artifacts.
	if err := tx.Where("run_id=? AND status='queued' AND content_fingerprint=''", candidate.RunID).Delete(&artifactManifestPlaceholder{}).Error; err != nil {
		return false, err
	}
	now := time.Now().UTC()
	result := tx.Exec(`UPDATE catalog_physical_writes AS p
		SET state='settled', settled_at=?, updated_at=?
		WHERE p.id=? AND p.owner_kind='artifact' AND p.state='quiescent'
		  AND p.artifact_receipt_version=0
		  AND NOT EXISTS (SELECT 1 FROM media_artifacts a WHERE a.run_id=p.owner_id)`, now, now, physicalWriteID)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// artifactManifestPlaceholder supplies only the table name needed by the
// migration package without importing the much wider model graph here.
type artifactManifestPlaceholder struct{}

func (artifactManifestPlaceholder) TableName() string { return "media_artifacts" }

const legacyArtifactNoIOCandidateSQL = `
	SELECT p.id AS id, r.id AS run_id
	FROM catalog_physical_writes p
	JOIN media_artifact_runs r
	  ON r.id=p.owner_id AND r.library_id=p.library_id AND r.job_id=p.job_id
	JOIN jobs j ON j.id=p.job_id
	WHERE p.owner_kind='artifact'
	  AND p.state='quiescent'
	  AND p.settled_at IS NULL
	  AND p.runtime_id<>''
	  AND p.artifact_receipt_version=0
	  AND j.job_type='media_artifact'
	  AND j.status IN ('completed','failed','cancelled')
	  AND j.finished_at IS NOT NULL
	  AND j.lease_token_hash=''
	  AND j.lease_expires_at IS NULL
	  AND j.interrupt_status=''
	  AND json_valid(j.payload_json)
	  AND json_type(j.payload_json,'$.artifact_run_id')='text'
	  AND json_extract(j.payload_json,'$.artifact_run_id')=r.id
	  AND (SELECT COUNT(*) FROM json_each(j.payload_json) WHERE key='artifact_run_id')=1
	  AND NOT EXISTS (
		SELECT 1 FROM json_each(CASE WHEN json_valid(j.payload_json) THEN j.payload_json ELSE '{}' END)
		WHERE key<>'artifact_run_id'
	  )
	  AND r.status IN ('failed','superseded')
	  AND r.finished_at IS NOT NULL
	  AND r.cleanup_status='skipped'
	  AND r.cleanup_error_code=''
	  AND r.written_count=0
	  AND r.updated_count=0
	  AND r.removed_count=0
	  AND NOT EXISTS (SELECT 1 FROM catalog_artifact_write_receipts wr WHERE wr.run_id=r.id)
	  AND NOT EXISTS (
		SELECT 1 FROM catalog_artifact_cleanup_claims cc
		WHERE cc.owner_run_id=r.id OR cc.physical_write_id=p.id
		   OR cc.artifact_id IN (SELECT a.id FROM media_artifacts a WHERE a.run_id=r.id)
	  )
	  AND NOT EXISTS (
		SELECT 1 FROM media_artifacts a
		WHERE a.run_id=r.id AND (a.status<>'queued' OR a.content_fingerprint<>'')
	  )`

// v101 is a normal, installation-wide data migration. It has no knowledge of
// customer IDs, generations, historical error codes, or individual tasks.
func migrateLegacyArtifactNoIORecovery(db *gorm.DB) error {
	for {
		var ids []uint64
		if err := db.Raw(`SELECT proven.id FROM (`+legacyArtifactNoIOCandidateSQL+`) proven ORDER BY proven.id LIMIT ?`, legacyArtifactNoIOBatch).Scan(&ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			settled, err := ReconcileLegacyArtifactNoIOPhysicalWriteTx(db, id)
			if err != nil {
				return err
			}
			if !settled {
				// The migration transaction owns the SQLite writer, so a proven
				// candidate cannot disappear between selection and settlement.
				return gorm.ErrInvalidTransaction
			}
		}
	}
}
