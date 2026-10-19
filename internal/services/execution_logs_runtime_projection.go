package services

import (
	"context"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// Global log grants remain independent of job/follow grants. They do not
// override current resource policies. Resolve references in bounded batches;
// never infer a legacy record's private resource from its free-form text.
func (s *RuntimeLogService) projectRuntimeEntries(ctx context.Context, actor Actor, entries []logging.Entry) ([]logging.Entry, bool, error) {
	visible := make([]logging.Entry, 0, len(entries))
	limited := limitedRuntimeResources(actor)
	ids, taskIDs := map[string]bool{}, map[string]bool{}
	for _, entry := range entries {
		for _, key := range []string{"job_id", "related_job_id", "task_id", "related_task_id"} {
			if id, _ := entry.Fields[key].(string); id != "" && len(id) <= 128 {
				ids[id] = true
				if key == "task_id" || key == "related_task_id" {
					taskIDs[id] = true
				}
			}
		}
	}
	taskJobs := map[string][]string{}
	if len(taskIDs) > 0 {
		values := make([]string, 0, len(taskIDs))
		for id := range taskIDs {
			values = append(values, id)
		}
		for _, table := range []string{"download_tasks", "transfer_tasks", "seeding_tasks", "media_reorganization_tasks"} {
			var bindings []struct{ ID, JobID string }
			if err := s.db.WithContext(ctx).Table(table).Select("id", "job_id").Where("id IN ?", values).Find(&bindings).Error; err != nil {
				if ctx.Err() != nil {
					return visible, true, nil
				}
				return nil, false, err
			}
			for _, binding := range bindings {
				ids[binding.JobID] = true
				taskJobs[binding.ID] = append(taskJobs[binding.ID], binding.JobID)
			}
		}
	}
	jobs := map[string]models.Job{}
	access := map[string]jobResourceAccess{}
	if len(ids) > 0 {
		values := make([]string, 0, len(ids))
		for id := range ids {
			values = append(values, id)
		}
		for start := 0; start < len(values); start += 300 {
			end := min(start+300, len(values))
			var rows []models.Job
			if err := s.db.WithContext(ctx).Where("id IN ? AND history_cleared_at IS NULL", values[start:end]).Find(&rows).Error; err != nil {
				if ctx.Err() != nil {
					return visible, true, nil
				}
				return nil, false, err
			}
			resources, err := jobResourceAccessRows(s.db.WithContext(ctx), rows)
			if err != nil {
				if ctx.Err() != nil {
					return visible, true, nil
				}
				return nil, false, err
			}
			for _, row := range rows {
				jobs[row.ID], access[row.ID] = row, resources[row.ID]
			}
		}
	}
	type resourceAccess struct {
		allowed, bound bool
		scope          *executionInfrastructure
	}
	cache := map[string]resourceAccess{}
	checkJob := func(id string) (resourceAccess, error) {
		if cached, ok := cache[id]; ok {
			return cached, nil
		}
		job, found := jobs[id]
		if !found {
			result := resourceAccess{allowed: !limited}
			cache[id] = result
			return result, nil
		}
		if !access[id].visible(actor) {
			cache[id] = resourceAccess{}
			return resourceAccess{}, nil
		}
		result := resourceAccess{allowed: true, bound: access[id].Bound}
		if limited && (job.JobType == "download" || job.JobType == "transfer" || job.JobType == "seeding" || job.JobType == JobTypeFollowSearch) && (!access[id].Bound || (job.JobType == JobTypeFollowSearch && access[id].LibraryID == 0)) {
			result.allowed = false
		}
		if limited || job.JobType == JobTypeMediaServerRefresh {
			scope, err := s.executionInfrastructureForRead(ctx, actor, job, access[id], true)
			if err != nil {
				return resourceAccess{}, err
			}
			result.scope = &scope
			result.allowed = result.allowed && !scope.restricted
			if job.JobType == JobTypeMediaServerRefresh || job.JobType == JobTypePan115RecycleCleanup {
				result.bound = !scope.restricted
			}
		}
		cache[id] = result
		return result, nil
	}
	restricted := false
	for _, entry := range entries {
		if !executionResourceReferencesVisible(actor, entry.Fields) {
			restricted = true
			continue
		}
		allowed, bound, infrastructureBound := true, false, false
		for _, key := range []string{"library_id", "source_library_id", "target_library_id", "site_id", "downloader_id", "site_ids", "library_ids"} {
			if value := entry.Fields[key]; value != nil {
				if ids, ok := value.([]any); !ok || len(ids) > 0 {
					bound = true
				}
			}
		}
		for _, key := range []string{"job_id", "related_job_id", "task_id", "related_task_id"} {
			id, _ := entry.Fields[key].(string)
			if id == "" {
				continue
			}
			refs := taskJobs[id]
			if _, found := jobs[id]; found || key == "job_id" || key == "related_job_id" {
				refs = append(append([]string{}, refs...), id)
			}
			for _, ref := range refs {
				state, err := checkJob(ref)
				if err != nil {
					if ctx.Err() != nil {
						return visible, true, nil
					}
					return nil, false, err
				}
				allowed, bound = allowed && state.allowed, bound || state.bound
				if (key == "job_id" || key == "task_id") && state.scope != nil && !state.scope.visible(entry.Fields) {
					allowed = false
				}
				infrastructureBound = infrastructureBound || ((key == "job_id" || key == "task_id") && state.scope != nil)
			}
		}
		if limited && protectedRuntimeOperation(entry) && !infrastructureBound {
			for key := range entry.Fields {
				if strings.Contains(key, "connection_id") || strings.Contains(key, "storage_id") {
					allowed = false
				}
			}
		}
		if !allowed || (limited && protectedRuntimeOperation(entry) && !bound) {
			restricted = true
			continue
		}
		visible = append(visible, entry)
	}
	return visible, restricted, nil
}

func limitedRuntimeResources(actor Actor) bool {
	if !actor.Can(authz.PermissionMediaLibrariesRead) || restrictedSiteAccess(actor) {
		return true
	}
	for _, scope := range []string{models.ResourceAccessScopeLibraryRead, models.ResourceAccessScopeDownloaderUse} {
		if policy, found := actor.ResourceAccessPolicies[scope]; found && (policy.invalid || (policy.Mode != models.ResourceAccessModeAll && (policy.Mode != models.ResourceAccessModeDenylist || len(policy.ResourceIDs) > 0))) {
			return true
		}
	}
	for _, rule := range actor.ResourceRules {
		if rule.Effect == models.AuthorizationEffectDeny && (rule.ResourceType == models.AuthorizationResourceMediaLibrary || rule.ResourceType == models.AuthorizationResourceSite || rule.ResourceType == models.AuthorizationResourceDownloader) {
			return true
		}
	}
	return false
}

func protectedRuntimeOperation(entry logging.Entry) bool {
	code := entry.Operation
	if code == logging.OperationAuthentication.Code || code == logging.OperationServerLifecycle.Code || code == logging.OperationServerUpdate.Code || code == logging.OperationSystemHealth.Code || code == logging.OperationRuntimeLogging.Code || (strings.HasPrefix(code, "plugin_") && code != logging.OperationPluginDownload.Code) || strings.HasPrefix(code, "connection_") || code == logging.OperationStorageManagement.Code || code == logging.OperationDirectoryBrowsing.Code {
		return false
	}
	if strings.HasPrefix(code, "library_") || strings.HasPrefix(code, "media_") || strings.Contains(code, "strm") || strings.Contains(code, "download") || strings.HasPrefix(code, "follow_") || code == "pan115_cloud_transfer" || code == "pan115_share_ingest" || code == "metadata_snapshot" || code == "artifact_cleanup" || code == "seeding_management" || code == "discovery_search" || code == "pt_site_management" || code == "task_queue" || code == "unified_schedule" {
		return true
	}
	for _, module := range []string{"download", "transfer", "follow", "site", "discovery", "media", "media_library", "media_artifact", "seeding", "strm", "catalog"} {
		if entry.Module == module {
			return true
		}
	}
	for _, key := range []string{"title", "work_title", "resource_name", "media_display_name", "file_name", "old_name", "new_name", "library_name", "site_name", "downloader_name", "samples", "source_relative_path", "target_relative_path", "relative_path"} {
		if entry.Fields[key] != nil {
			return true
		}
	}
	return false
}
