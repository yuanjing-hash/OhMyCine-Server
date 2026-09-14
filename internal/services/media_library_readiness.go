package services

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// MediaLibraryReadiness is a read model of durable execution evidence. Finding
// naming problems is not the same thing as starting a physical repair.
type MediaLibraryReadiness struct {
	Ready           bool   `json:"ready"`
	ReadinessStatus string `json:"readiness_status"`
}

// Only positive no-I/O/settlement evidence can exclude historical failures.
// A newer successful repair is not proof that an older unknown write stopped.
const readinessRepairCurrentSQL = `NOT EXISTS (
	SELECT 1 FROM catalog_physical_writes rp
	WHERE rp.owner_kind='repair' AND rp.owner_id=r.id AND rp.library_id=l.id
	AND (rp.state='settled' OR (rp.state='admitted' AND (
		EXISTS(SELECT 1 FROM jobs j WHERE j.id=r.job_id AND j.status='cancelled' AND j.lease_token_hash='' AND j.lease_expires_at IS NULL)
		OR EXISTS(SELECT 1 FROM catalog_heads h WHERE h.library_id=l.id AND h.source_epoch<>rp.source_epoch)
	)))
)`

const readinessRepairFailedSQL = readinessRepairCurrentSQL

// A cancelled original admission never entered file execution. Do not let the
// stale domain phase hold the library forever; a retry removes this exemption.
// Missing receipts, partial results and unacknowledged leases remain blocking.
const readinessRepairCancelledUnenteredSQL = `EXISTS (
	SELECT 1 FROM catalog_physical_writes cp JOIN jobs cj ON cj.id=cp.job_id
	WHERE cp.library_id=l.id AND cp.owner_kind='repair' AND cp.owner_id=r.id
	AND cp.job_id=r.job_id AND ((cp.state='admitted' AND r.succeeded_items=0) OR cp.state='settled')
	AND cj.job_type='media_library_repair' AND cj.status='cancelled'
	AND cj.lease_token_hash='' AND cj.lease_expires_at IS NULL
)`

const readinessRepairActiveSQL = `(r.phase IN ('queued','executing','reconciling') AND NOT (` + readinessRepairCancelledUnenteredSQL + `)) OR
	(r.phase='failed' AND EXISTS(SELECT 1 FROM jobs retry_job WHERE retry_job.id=r.job_id AND retry_job.job_type='media_library_repair' AND retry_job.status IN ('queued','running')))`

func libraryReadinessRows(db *gorm.DB, ids []uint) (map[uint]MediaLibraryReadiness, error) {
	type row struct {
		ID                 uint
		Enabled            bool
		BaselineGeneration uint64
		Status             string
		StorageEnabled     bool
		ConnectionEnabled  bool
		AuthExpired        bool
		Checking           bool
		Repairing          bool
		RepairFailed       bool
		PhysicalPending    bool
	}
	var rows []row
	query := db.Table("media_libraries AS l").Select(`l.id, l.enabled, l.baseline_generation, l.status,
		s.enabled AS storage_enabled,
		COALESCE(c.enabled,1) AS connection_enabled,
		COALESCE(c.last_health_status = 'offline' AND c.last_health_error_code = 'pan115_auth_expired', 0) AS auth_expired,
		EXISTS(SELECT 1 FROM media_library_structure_auto_states a WHERE a.library_id=l.id AND a.diagnosed_revision<a.source_revision) AS checking,
		EXISTS(SELECT 1 FROM media_library_structure_repairs r WHERE r.library_id=l.id AND ((` + readinessRepairActiveSQL + `) OR (r.phase='failed' AND (` + readinessRepairFailedSQL + `)))) AS repairing,
		EXISTS(SELECT 1 FROM media_library_structure_repairs r WHERE r.library_id=l.id AND r.phase = 'failed' AND (` + readinessRepairFailedSQL + `)) AS repair_failed,
		EXISTS(SELECT 1 FROM catalog_physical_writes p WHERE p.library_id=l.id AND p.state IN ('entered','quiescent')) AS physical_pending`).
		Joins("JOIN storages s ON s.id=l.storage_id").Joins("LEFT JOIN connections c ON c.id=s.connection_id")
	if len(ids) > 0 {
		query = query.Where("l.id IN ?", ids)
	}
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]MediaLibraryReadiness, len(rows))
	for _, r := range rows {
		status := "ready"
		switch {
		case r.AuthExpired:
			status = "credentials_required"
		case !r.StorageEnabled || !r.ConnectionEnabled:
			status = "unavailable"
		case r.RepairFailed:
			status = "repair_failed"
		case r.Repairing:
			status = "repairing"
		case r.PhysicalPending:
			status = "busy"
		case !r.Enabled:
			status = "disabled"
		case r.BaselineGeneration == 0:
			status = "initializing"
		case r.Checking:
			status = "checking"
		case r.Status == models.MediaLibraryStatusInitializationFailed:
			status = "unavailable"
		}
		out[r.ID] = MediaLibraryReadiness{Ready: status == "ready", ReadinessStatus: status}
	}
	return out, nil
}

func libraryReadiness(db *gorm.DB, id uint) (MediaLibraryReadiness, error) {
	rows, err := libraryReadinessRows(db, []uint{id})
	if err != nil {
		return MediaLibraryReadiness{}, err
	}
	r, ok := rows[id]
	if !ok {
		return MediaLibraryReadiness{}, gorm.ErrRecordNotFound
	}
	return r, nil
}

func currentLibraryRepairOwner(db *gorm.DB, libraryID uint) (string, error) {
	var row struct{ ID string }
	err := db.Table("media_library_structure_repairs AS r").
		Select("r.id").
		Joins("JOIN media_libraries AS l ON l.id=r.library_id").
		Where("r.library_id=? AND (("+readinessRepairActiveSQL+") OR (r.phase='failed' AND ("+readinessRepairFailedSQL+")))", libraryID).
		Order("r.created_at,r.id").
		Limit(1).
		Scan(&row).Error
	return row.ID, err
}

func otherPendingCatalogPhysicalWrite(db *gorm.DB, libraryID uint, ownerKind, ownerID string) (bool, error) {
	var count int64
	err := db.Model(&models.CatalogPhysicalWrite{}).
		Where("library_id=? AND state IN ? AND NOT (owner_kind=? AND owner_id=?)", libraryID, []string{"entered", "quiescent"}, ownerKind, ownerID).
		Count(&count).Error
	return count != 0, err
}

func acceptedLibraryJob(db *gorm.DB, libraryID uint, job models.Job) (bool, error) {
	var physical int64
	if err := db.Model(&models.CatalogPhysicalWrite{}).
		Where("library_id=? AND job_id=? AND state IN ?", libraryID, job.ID, []string{"admitted", "entered", "quiescent"}).
		Count(&physical).Error; err != nil || physical != 0 {
		return physical != 0, err
	}
	switch job.JobType {
	case "download":
		// Manual recognition recovery resumes the exact failed download Job
		// after the user has bound a verified identity. It reuses the durable
		// completed provider fact/manifest and cannot submit new provider work.
		var recoveries int64
		err := db.Model(&models.DownloadTask{}).
			Where("job_id=? AND target_library_id=? AND phase=? AND scrape_status=?", job.ID, libraryID, models.DownloadTaskStatusVerifying, "completed_unrecognized").
			Where("identity_locked=? AND identity_source=? AND identity_status=?", true, mediaIdentitySourceManual, mediaIdentityStatusVerified).
			Where("recognition_override_tmdb_id IS NOT NULL AND recognition_override_media_type IN ?", []string{"movie", "tv"}).
			Where("provider_task_id<>'' OR completed_manifest_json NOT IN ('','{}')").
			Count(&recoveries).Error
		return recoveries == 1, err
	case JobTypeMediaArtifact:
		var runs int64
		err := db.Model(&models.MediaArtifactRun{}).Where("library_id=? AND job_id=?", libraryID, job.ID).Count(&runs).Error
		return runs == 1, err
	case "transfer":
		var transfers int64
		err := db.Model(&models.TransferTask{}).Where("library_id=? AND job_id=?", libraryID, job.ID).Count(&transfers).Error
		return transfers == 1, err
	case JobTypeMediaReorganization:
		var tasks int64
		err := db.Model(&models.MediaReorganizationTask{}).Where("library_id=? AND job_id=?", libraryID, job.ID).Count(&tasks).Error
		return tasks == 1, err
	case JobTypeMediaLibraryRecognition:
		var payload mediaLibraryRecognitionJobPayload
		if json.Unmarshal([]byte(job.PayloadJSON), &payload) != nil || payload.LibraryID != libraryID || payload.ScanRunID == 0 || payload.Generation == 0 {
			return false, nil
		}
		var runs int64
		err := db.Model(&models.MediaLibraryScanRun{}).
			Where("id=? AND library_id=? AND generation=? AND status IN ?", payload.ScanRunID, libraryID, payload.Generation, []string{"catalog_ready", "completed"}).
			Count(&runs).Error
		return runs == 1, err
	case JobTypeMediaLibraryStructureDiagnosis:
		var diagnoses int64
		err := db.Model(&models.MediaLibraryStructureDiagnosis{}).Where("library_id=? AND job_id=?", libraryID, job.ID).Count(&diagnoses).Error
		return diagnoses == 1, err
	default:
		return false, nil
	}
}

// Claim gates leave the original queued row and checkpoint untouched. Recovery
// and baseline-building work must remain runnable, otherwise readiness could
// never become true. Resource keys are private server-created identities.
func libraryJobBlocked(db *gorm.DB, job models.Job, caches ...map[uint]MediaLibraryReadiness) (bool, error) {
	if job.JobType == JobTypeFollowSearch && strings.HasPrefix(job.ResourceKey, "follow:") {
		return followReadinessBlocked(db, strings.TrimPrefix(job.ResourceKey, "follow:"))
	}
	if job.JobType == JobTypeUnifiedSchedule {
		var run models.ScheduleRun
		if err := db.Where("job_id=?", job.ID).First(&run).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil
			}
			return false, err
		}
		var definition models.ScheduleDefinition
		if err := db.First(&definition, "id=?", run.ScheduleID).Error; err != nil {
			return false, err
		}
		return scheduleReadinessBlocked(db, definition)
	}
	// Transfer owns one exact target library and carries a registered physical
	// receipt. Do not replace that precise gate with an aggregate check of every
	// library sharing the provider connection.
	if job.JobType != "transfer" && strings.HasPrefix(job.ResourceKey, "connection:") {
		id, _ := strconv.ParseUint(strings.TrimPrefix(job.ResourceKey, "connection:"), 10, 64)
		if id != 0 {
			return connectionReadinessBlocked(db, uint(id))
		}
	}
	var id uint64
	if job.JobType == "transfer" {
		var task models.TransferTask
		if err := db.Where("job_id = ?", job.ID).First(&task).Error; err == nil {
			id = uint64(task.LibraryID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return false, err
		}
	}
	if job.JobType == "download" {
		var expired int64
		if err := db.Table("download_tasks t").Joins("JOIN downloaders d ON d.id=t.downloader_id").Joins("JOIN storages s ON s.id=d.storage_id").Joins("JOIN connections c ON c.id=s.connection_id").Where("t.job_id=? AND c.last_health_status='offline' AND c.last_health_error_code='pan115_auth_expired'", job.ID).Count(&expired).Error; err != nil {
			return false, err
		}
		if expired > 0 {
			return true, nil
		}
		var task models.DownloadTask
		if err := db.Where("job_id = ?", job.ID).First(&task).Error; err == nil && task.TargetLibraryID != nil {
			id = uint64(*task.TargetLibraryID)
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return false, err
		}
	}
	for _, prefix := range []string{"library:", "media-artifact-library:", "strm-library:", "structure-diagnosis-library:"} {
		if strings.HasPrefix(job.ResourceKey, prefix) {
			id, _ = strconv.ParseUint(strings.TrimPrefix(job.ResourceKey, prefix), 10, 64)
			break
		}
	}
	if id == 0 {
		return false, nil
	}
	var r MediaLibraryReadiness
	var err error
	cached := false
	if len(caches) > 0 {
		r, cached = caches[0][uint(id)]
	}
	if !cached {
		r, err = libraryReadiness(db, uint(id))
		if err == nil && len(caches) > 0 {
			caches[0][uint(id)] = r
		}
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.ReadinessStatus == "credentials_required" {
		return true, nil
	}
	if job.JobType == JobTypeMediaLibraryRepair {
		var repair models.MediaLibraryStructureRepair
		if err := db.Where("library_id=? AND job_id=?", id, job.ID).First(&repair).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return true, nil
			}
			return false, err
		}
		currentOwner, err := currentLibraryRepairOwner(db, uint(id))
		if err != nil {
			return false, err
		}
		return currentOwner == "" || currentOwner != repair.ID, nil
	}
	if job.JobType == JobTypeMediaLibraryRetirement {
		return false, nil
	}
	if r.ReadinessStatus == "busy" || r.ReadinessStatus == "repairing" || r.ReadinessStatus == "repair_failed" {
		// A prior physical owner needs its original job to reconcile its receipts.
		var owners int64
		err := db.Model(&models.CatalogPhysicalWrite{}).Where("library_id = ? AND job_id = ? AND state IN ?", id, job.ID, []string{"entered", "quiescent"}).Count(&owners).Error
		return owners == 0, err
	}
	if r.ReadinessStatus == "checking" || r.ReadinessStatus == "initializing" || r.ReadinessStatus == "disabled" {
		// Enabled/readiness controls automatic admission. An already accepted,
		// exact durable domain owner must still finish the work that establishes
		// the baseline or reconciles a manual operation. Arbitrary Jobs sharing a
		// resource-key prefix do not receive this exemption.
		accepted, err := acceptedLibraryJob(db, uint(id), job)
		return !accepted, err
	}
	return !r.Ready, nil
}

func connectionReadinessBlocked(db *gorm.DB, id uint) (bool, error) {
	var connection models.Connection
	if err := db.First(&connection, id).Error; err != nil {
		return false, err
	}
	if !connection.Enabled || (connection.LastHealthStatus == "offline" && connection.LastHealthErrorCode == "pan115_auth_expired") {
		return true, nil
	}
	var ids []uint
	if err := db.Table("media_libraries l").Joins("JOIN storages s ON s.id=l.storage_id").Where("s.connection_id=? AND l.enabled=?", id, true).Pluck("l.id", &ids).Error; err != nil {
		return false, err
	}
	if len(ids) == 0 {
		return false, nil
	}
	rows, err := libraryReadinessRows(db, ids)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if !r.Ready {
			return true, nil
		}
	}
	return false, nil
}

func scheduleReadinessBlocked(db *gorm.DB, definition models.ScheduleDefinition) (bool, error) {
	if definition.TargetType == "follow" {
		return followReadinessBlocked(db, definition.TargetID)
	}
	id, _ := strconv.ParseUint(definition.TargetID, 10, 64)
	if id == 0 {
		return false, nil
	}
	switch definition.TargetType {
	case "media_library":
		r, err := libraryReadiness(db, uint(id))
		return !r.Ready, err
	case "connection":
		return connectionReadinessBlocked(db, uint(id))
	}
	return false, nil
}

func followReadinessBlocked(db *gorm.DB, id string) (bool, error) {
	var follow models.FollowSubscription
	if err := db.First(&follow, "id=?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	var snapshot FollowExecutionSnapshot
	if json.Unmarshal([]byte(follow.ExecutionSnapshotJSON), &snapshot) != nil || snapshot.MediaLibraryID == 0 {
		return false, nil
	}
	r, err := libraryReadiness(db, snapshot.MediaLibraryID)
	return !r.Ready, err
}
