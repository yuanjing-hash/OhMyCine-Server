package services

import (
	"strconv"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// JobWaitReasonDTO describes a current, proven gate, not an execution error.
// It deliberately contains no blocking Job identity or private domain payload.
type JobWaitReasonDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func waitReasonLibraryID(job models.Job) uint {
	for _, prefix := range []string{"library:", "media-artifact-library:", "strm-library:", "structure-diagnosis-library:"} {
		if strings.HasPrefix(job.ResourceKey, prefix) {
			id, _ := strconv.ParseUint(strings.TrimPrefix(job.ResourceKey, prefix), 10, 64)
			return uint(id)
		}
	}
	return 0
}

func (s *QueueService) projectJobWaitReasons(actor Actor, jobs []models.Job, dtos []JobDTO) error {
	ids := make([]uint, 0, len(jobs))
	seen := make(map[uint]bool)
	for _, job := range jobs {
		if job.Status != models.JobStatusQueued && job.Status != models.JobStatusRetryWait {
			continue
		}
		if id := waitReasonLibraryID(job); id != 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	// Never pass an empty ID list: the readiness API then selects all libraries.
	cache := make(map[uint]MediaLibraryReadiness)
	if len(ids) > 0 {
		var err error
		cache, err = libraryReadinessRows(s.db, ids)
		if err != nil {
			return err
		}
	}
	now := s.clock.Now()
	for i, job := range jobs {
		if job.Status != models.JobStatusQueued && job.Status != models.JobStatusRetryWait {
			continue
		}
		if id := waitReasonLibraryID(job); id != 0 {
			if readiness, exists := cache[id]; exists {
				blocked, err := libraryJobBlocked(s.db, job, cache)
				if err != nil {
					return err
				}
				if blocked {
					dtos[i].WaitReason = &JobWaitReasonDTO{Code: "dependency_not_ready", Message: "依赖资源尚未准备好，请联系管理员检查资源状态"}
					if actor.CanResource(authz.PermissionMediaLibrariesRead, models.AuthorizationResourceMediaLibrary, uintID(id)) && !readiness.Ready {
						dtos[i].WaitReason = libraryWaitReason(readiness.ReadinessStatus)
					}
					continue
				}
			}
		}
		if job.NextAttemptAt != nil && job.NextAttemptAt.After(now) {
			dtos[i].WaitReason = &JobWaitReasonDTO{Code: "retry_at", Message: "等待已安排的重试时间，到时自动重新检查执行条件"}
		}
	}
	return nil
}

func libraryWaitReason(status string) *JobWaitReasonDTO {
	message := "媒体库尚未准备好，请查看媒体库状态"
	switch status {
	case "credentials_required":
		message = "连接凭据已失效，请在连接设置中更新凭据后继续"
	case "busy":
		message = "媒体库正在执行文件操作或自动核验其结果；无需手动确认，完成后任务会自动继续"
	case "repairing":
		message = "媒体库整理尚未完成，等待原整理任务完成或核验恢复"
	case "repair_failed":
		message = "媒体库整理尚有未完成结果，请检查原整理任务的失败详情"
	case "disabled":
		message = "媒体库已停用，启用后自动任务才能继续"
	case "initializing":
		message = "等待媒体库首次扫描完成"
	case "checking":
		message = "等待媒体库首次目录检查完成"
	case "unavailable":
		message = "媒体库或关联存储不可用，请检查媒体库和连接状态"
	}
	return &JobWaitReasonDTO{Code: "library_" + status, Message: message}
}
