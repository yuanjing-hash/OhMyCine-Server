package services

import (
	"encoding/json"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type jobResourceAccess struct {
	LibraryID    uint
	DownloaderID *string
	SiteIDs      []uint
	UnknownSite  bool
	Bound        bool
	invalid      bool
}

// Read frozen resource identities in bounded batches, without decrypting sources
// or exposing private job payloads. Workers do not call this authorization gate.
func jobResourceAccessRows(db *gorm.DB, jobs []models.Job) (map[string]jobResourceAccess, error) {
	result := make(map[string]jobResourceAccess, len(jobs))
	byType := make(map[string][]string)
	for _, job := range jobs {
		libraryID := waitReasonLibraryID(job)
		result[job.ID] = jobResourceAccess{LibraryID: libraryID, Bound: libraryID != 0}
		byType[job.JobType] = append(byType[job.JobType], job.ID)
	}
	type binding struct {
		JobID               string
		LibraryID           uint
		DownloaderID        *string
		SourceOrigin        string
		DownloadPayloadJSON string
		SiteID              uint
	}
	for jobType, table := range map[string]string{"download": "download_tasks", "transfer": "transfer_tasks", "seeding": "seeding_tasks"} {
		ids := byType[jobType]
		if len(ids) == 0 {
			continue
		}
		query := db.Table("download_tasks AS download")
		columns := "download.job_id, COALESCE(download.target_library_id,0) AS library_id"
		if jobType != "download" {
			query = db.Table(table + " AS phase").Joins("JOIN download_tasks AS download ON download.id = phase.download_task_id")
			columns = "phase.job_id, COALESCE(download.target_library_id,0) AS library_id"
			if jobType == "transfer" {
				columns = "phase.job_id, phase.library_id"
			}
		}
		query = query.Joins("JOIN jobs AS source_job ON source_job.id = download.job_id").Joins("LEFT JOIN plugin_resource_claims AS claim ON claim.id = download.plugin_resource_claim_id")
		columns += ", download.downloader_id, download.source_origin, source_job.payload_json AS download_payload_json, COALESCE(claim.site_id,0) AS site_id"
		jobColumn := "download.job_id"
		if jobType != "download" {
			jobColumn = "phase.job_id"
		}
		var rows []binding
		if err := query.Select(columns).Where(jobColumn+" IN ?", ids).Scan(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			access := jobResourceAccess{LibraryID: row.LibraryID, DownloaderID: row.DownloaderID, Bound: true}
			var payload downloadJobPayload
			if err := json.Unmarshal([]byte(row.DownloadPayloadJSON), &payload); err != nil {
				access.invalid = true
			} else if payload.SourceSiteID != 0 {
				access.SiteIDs = []uint{payload.SourceSiteID}
			} else if row.SiteID != 0 {
				access.SiteIDs = []uint{row.SiteID}
			} else if payload.ResourceAccessVersion == 0 && row.SourceOrigin != models.DownloadSourceOriginProviderIngest && row.SourceOrigin != models.DownloadSourceOriginPlugin {
				// Old tasks did not preserve site provenance. Do not infer it from
				// URL shape or silently accept it under a restricted site policy.
				access.UnknownSite = true
			}
			result[row.JobID] = access
		}
	}
	if ids := byType[JobTypeFollowSearch]; len(ids) > 0 {
		var rows []models.FollowRun
		if err := db.Select("job_id", "execution_snapshot_json").Where("job_id IN ?", ids).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			var snapshot FollowExecutionSnapshot
			if err := json.Unmarshal([]byte(row.ExecutionSnapshotJSON), &snapshot); err != nil {
				result[row.JobID] = jobResourceAccess{Bound: true, invalid: true}
				continue
			}
			result[row.JobID] = jobResourceAccess{LibraryID: snapshot.MediaLibraryID, SiteIDs: snapshot.SiteIDs, Bound: true}
		}
	}
	return result, nil
}

func restrictedSiteAccess(actor Actor) bool {
	if !actor.Can(authz.PermissionDiscoveryRead) {
		return true
	}
	policy, configured := actor.ResourceAccessPolicies[models.ResourceAccessScopeSiteSearch]
	if configured {
		if policy.invalid {
			return true
		}
		switch policy.Mode {
		case models.ResourceAccessModeAll, models.ResourceAccessModeDenylist:
			if len(policy.ResourceIDs) != 0 {
				return true
			}
		default:
			return true
		}
	}
	for _, rule := range actor.ResourceRules {
		if rule.PermissionCode == authz.PermissionDiscoveryRead && rule.ResourceType == models.AuthorizationResourceSite && rule.Effect == models.AuthorizationEffectDeny {
			return true
		}
	}
	return false
}

func (access jobResourceAccess) visible(actor Actor) bool {
	if access.invalid {
		return false
	}
	if !taskLibraryVisible(actor, access.LibraryID) || !taskDownloaderVisible(actor, access.DownloaderID) || (access.UnknownSite && restrictedSiteAccess(actor)) {
		return false
	}
	for _, id := range access.SiteIDs {
		if !actor.CanResource(authz.PermissionDiscoveryRead, models.AuthorizationResourceSite, uintID(id)) {
			return false
		}
	}
	return true
}

func redactJobResources(dto *JobDTO) {
	dto.DisplayName = "资源权限受限的任务"
	dto.ResourceKey, dto.Provider, dto.LastErrorMessage = "", "", ""
	dto.Action, dto.WaitReason = nil, nil
}

func (s *QueueService) projectJobResourceAccess(actor Actor, jobs []models.Job, dtos []JobDTO) error {
	rows, err := jobResourceAccessRows(s.db, jobs)
	if err != nil {
		return err
	}
	for i, job := range jobs {
		if !rows[job.ID].visible(actor) {
			redactJobResources(&dtos[i])
		}
	}
	return nil
}

func (access jobResourceAccess) canContinue(actor Actor, jobType string) bool {
	if access.invalid {
		return false
	}
	if access.UnknownSite && restrictedSiteAccess(actor) {
		return false
	}
	if access.DownloaderID != nil && !actor.CanResource(authz.PermissionDownloadsCreate, models.AuthorizationResourceDownloader, *access.DownloaderID) {
		return false
	}
	switch jobType {
	case "download", "transfer", "seeding", JobTypeFollowSearch:
		if !actor.HasPermission(authz.PermissionDownloadsCreate) {
			return false
		}
		if access.LibraryID != 0 && !actor.CanIngestLibrary(uintID(access.LibraryID)) {
			return false
		}
	case JobTypeMediaReorganization:
		if !actor.Can(authz.PermissionJobsControlOwn) && !actor.Can(authz.PermissionJobsControlAll) {
			return false
		}
		if !actor.Can(authz.PermissionTransfersReadOwn) && !actor.Can(authz.PermissionTransfersReadAll) {
			return false
		}
		if !taskLibraryVisible(actor, access.LibraryID) || !actor.ResourceAccessAllows(models.ResourceAccessScopeLibraryIngest, uintID(access.LibraryID)) {
			return false
		}
	default:
		permission := authz.PermissionMediaLibrariesScan
		if jobType == JobTypeSTRMReconcile || jobType == JobTypeMediaArtifact {
			permission = authz.PermissionSTRMRunsCreate
		}
		if access.LibraryID != 0 && !actor.CanResource(permission, models.AuthorizationResourceMediaLibrary, uintID(access.LibraryID)) {
			return false
		}
	}
	allowedSites := 0
	for _, id := range access.SiteIDs {
		if actor.CanResource(authz.PermissionDiscoveryRead, models.AuthorizationResourceSite, uintID(id)) {
			allowedSites++
		}
	}
	if jobType == JobTypeFollowSearch {
		return len(access.SiteIDs) > 0 && allowedSites > 0
	}
	return allowedSites == len(access.SiteIDs)
}

// Only explicit continuation crosses this gate. Cancel/pause and already
// accepted worker plans remain available after a user's resources are revoked.
func authorizeJobContinuationTx(tx *gorm.DB, requester Actor, job models.Job, purposes ...string) error {
	rows, err := jobResourceAccessRows(tx, []models.Job{job})
	if err != nil {
		return err
	}
	access := rows[job.ID]
	if !access.Bound {
		return nil
	}
	current, err := NewAuthorizationService(tx).Resolve(requester.User.ID)
	if err != nil {
		return err
	}
	control := current.Can(authz.PermissionJobsControlAll) || (job.OwnerID != nil && *job.OwnerID == current.User.ID && current.Can(authz.PermissionJobsControlOwn))
	if len(purposes) > 0 && purposes[0] == "respond" {
		control = current.Can(authz.PermissionJobsRespond) && (current.Can(authz.PermissionJobsReadAll) || (job.OwnerID != nil && *job.OwnerID == current.User.ID && current.Can(authz.PermissionJobsReadOwn)))
	}
	if !control {
		return appError(CodePermissionDenied, "当前权限不允许继续这个任务", nil)
	}
	if !access.canContinue(current, job.JobType) {
		return appError(CodePermissionDenied, "当前权限不允许继续这个任务，请调整权限或重新搜索后提交", nil)
	}
	if job.OwnerID != nil && *job.OwnerID != requester.User.ID {
		owner, err := NewAuthorizationService(tx).Resolve(*job.OwnerID)
		if err != nil || !access.canContinue(owner, job.JobType) {
			return appError(CodePermissionDenied, "任务所属用户当前无权继续这个任务", nil)
		}
	}
	return nil
}

func downloadResourcesVisible(db *gorm.DB, actor Actor, task models.DownloadTask) (bool, error) {
	var job models.Job
	if err := db.First(&job, "id = ?", task.JobID).Error; err != nil {
		return false, err
	}
	rows, err := jobResourceAccessRows(db, []models.Job{job})
	if err != nil {
		return false, err
	}
	return rows[job.ID].visible(actor), nil
}
