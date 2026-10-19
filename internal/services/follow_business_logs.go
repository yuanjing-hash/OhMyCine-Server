package services

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// These counters observe existing calls. They do not drive selection, retries,
// claim ownership, or the historical FollowRun count fields.
type followBusinessStats struct {
	mu                                    sync.Mutex
	raw, aliases, chosen, created, reused int
	sites                                 map[uint]bool
	requested                             map[uint]struct{}
	reservationFailures                   int
}
type followBusinessStatsKey struct{}

func (s *followBusinessStats) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	failed := 0
	for _, success := range s.sites {
		if !success {
			failed++
		}
	}
	return map[string]any{"raw_results": s.raw, "alias_requests": s.aliases, "chosen": s.chosen, "created": s.created, "reused": s.reused, "accepted_count": s.created + s.reused, "searched_sites_count": len(s.requested), "failed_sites": failed}
}

func followStats(ctx context.Context) *followBusinessStats {
	if ctx == nil {
		return nil
	}
	stats, _ := ctx.Value(followBusinessStatsKey{}).(*followBusinessStats)
	return stats
}

func observeFollowRequest(ctx context.Context, siteID uint) {
	if stats := followStats(ctx); stats != nil {
		stats.mu.Lock()
		stats.aliases++
		if stats.requested == nil {
			stats.requested = map[uint]struct{}{}
		}
		stats.requested[siteID] = struct{}{}
		stats.mu.Unlock()
	}
}

func observeFollowSite(ctx context.Context, id uint, success bool) {
	if stats := followStats(ctx); stats != nil {
		stats.mu.Lock()
		stats.sites[id] = stats.sites[id] || success
		stats.mu.Unlock()
	}
}

func (w *FollowSearchWorker) record(ctx context.Context, code, stage, outcome, message string, fields map[string]any) {
	serverlog.RecordBusiness(ctx, w.follows.log, serverlog.OperationFollowSearch, serverlog.BusinessEvent{Code: code, Stage: stage, Outcome: outcome, Message: message, Fields: fields})
}

func (w *FollowSearchWorker) recordRunEnd(ctx context.Context, runtime JobRuntime, run models.FollowRun, started time.Time, stats *followBusinessStats, result WorkerResult) {
	fields := stats.snapshot()
	fields["duration_ms"] = time.Since(started).Milliseconds()
	outcome, message := "failed", "本轮执行未完成，未确认后续下载或入库结果"
	var persisted models.FollowRun
	if err := w.follows.db.First(&persisted, "id = ? AND job_id = ?", run.ID, run.JobID).Error; err == nil {
		fields["status"] = persisted.Status
		fields["searched_names_count"] = persisted.SearchedNamesCount
		fields["candidates"] = persisted.Candidates
		fields["accepted_submissions"] = persisted.Selected
		var reasons map[string]int
		_ = json.Unmarshal([]byte(persisted.FilterSummaryJSON), &reasons)
		stats.mu.Lock()
		if stats.reservationFailures > 0 {
			if reasons == nil {
				reasons = map[string]int{}
			}
			reasons["reservation_failed"] = stats.reservationFailures
		}
		stats.mu.Unlock()
		fields["reason_counts"] = reasons
		if persisted.ErrorCode != "" {
			fields["error_code"] = persisted.ErrorCode
		}
		switch persisted.Status {
		case models.FollowRunCompleted:
			outcome, message = "finished", "所选已播剧集均已在库，本轮无需搜索或创建下载"
		case models.FollowRunSubmitted:
			outcome = "finished"
			if persisted.Selected == 0 {
				message = "缺集已有下载任务占用，本轮未重复提交；下载和入库仍由原任务继续"
			} else {
				message = fmt.Sprintf("搜索执行结束：%d 个可用候选，接受 %d 次提交（新建 %v，复用 %v）；下载及入库结果请查看关联任务", persisted.Candidates, persisted.Selected, fields["created"], fields["reused"])
			}
		case models.FollowRunNoMatch:
			outcome, message = "finished", fmt.Sprintf("搜索执行结束：%d 个可用候选，本轮未找到可补齐缺集的资源，订阅继续保留", persisted.Candidates)
		case models.FollowRunCancelled, models.FollowRunStale:
			outcome, message = "cancelled", "本轮追更已停止，已接受的独立下载任务保持原状态"
		case models.FollowRunFailed:
			message = "本轮追更失败：" + followBusinessFailure(persisted.ErrorCode)
		}
		if outcome == "finished" && (persisted.ErrorCode != "" || reasons["site_error"] > 0) {
			outcome = "partial"
			message += "；部分搜索或提交未成功，详见站点与资源记录"
		}
		if persisted.Status == models.FollowRunNoMatch && reasons["site_error"] > 0 {
			stats.mu.Lock()
			anySuccess := false
			for _, success := range stats.sites {
				anySuccess = anySuccess || success
			}
			stats.mu.Unlock()
			if !anySuccess {
				outcome, message, fields["error_code"] = "failed", "本轮站点搜索均未成功，无法判断是否存在匹配资源；订阅保持既有调度状态，不将失败解释为零结果", "follow_search_failed"
			}
		}
		if reasons["reservation_failed"] > 0 {
			outcome = "partial"
			message += "；部分资源无法保留季集占用，未提交"
		}
		if outcome == "finished" && persisted.FinishedAt != nil && result.ErrorCode == "" && result.RetryAt == nil && ctx.Err() == nil {
			progress, processed, total := float64(100), int64(4), int64(4)
			_ = runtime.Heartbeat(&progress, &processed, &total, nil, nil)
		}
	}
	if result.RetryAt != nil {
		outcome, message = "waiting", "本轮暂时等待目标或连接恢复，已保留执行阶段；不会因此重复提交已接受的任务"
		fields["error_code"] = result.ErrorCode
	} else if ctx.Err() != nil {
		outcome, message = "cancelled", "本轮执行已取消，尚未执行的搜索和提交已停止"
	} else if result.ErrorCode != "" {
		outcome, message = "failed", "本轮执行失败："+followBusinessFailure(result.ErrorCode)
		fields["error_code"] = result.ErrorCode
	}
	w.record(ctx, "follow.finish", "finish", outcome, message, fields)
}

func followBusinessFailure(code string) string {
	switch code {
	case "follow_submit_failed":
		return "所选资源均未能提交下载，请检查下载器和目标媒体库路线"
	case "follow_search_failed":
		return "所有实际站点搜索均失败，不能视为没有匹配资源"
	case "follow_coverage_unknown":
		return "无法确认所选季集的媒体库覆盖状态"
	case "follow_owner_unavailable", CodePermissionDenied:
		return "订阅用户或当前资源权限不可用"
	case "follow_state_persist_failed":
		return "追更执行状态未能保存"
	case CodeFollowConfigurationInvalid:
		return "订阅配置或执行快照不可用"
	default:
		return "当前阶段未完成，请结合错误码及前序记录检查"
	}
}

var followRejectionLabels = map[string]string{
	"episode_unproven": "无法确认所选季或集号", "include_keyword": "未包含要求的关键词", "exclude_keyword": "命中排除关键词",
	"resolution": "分辨率不符", "video_codec": "视频编码不符", "quality": "资源质量不符", "release_group": "制作组不符", "excluded_release_group": "制作组被排除",
	"seeders": "做种人数不足或未知", "size": "大小不符或未知", "age": "发布时间超限或未知", "duplicate": "同一资源重复返回", "no_missing_coverage": "未覆盖本轮缺集",
	"identity_mismatch": "作品身份或所选季不符", "claim_unavailable": "搜索结果凭据不可用", "identity_bind_failed": "无法绑定已确认的作品身份",
}

type followRejections struct {
	counts  map[string]int
	samples map[string][]string
}

func newFollowRejections() *followRejections {
	return &followRejections{counts: map[string]int{}, samples: map[string][]string{}}
}
func (r *followRejections) add(reason, title string) {
	r.counts[reason]++
	if len(r.samples[reason]) < 3 {
		r.samples[reason] = append(r.samples[reason], serverlog.BusinessDisplayName(title))
	}
}
func (r *followRejections) record(ctx context.Context, emit func(context.Context, string, string, string, string, map[string]any)) {
	keys := make([]string, 0, len(r.counts))
	for reason := range r.counts {
		keys = append(keys, reason)
	}
	sort.Strings(keys)
	for _, reason := range keys {
		label := followRejectionLabels[reason]
		if label == "" {
			label = "不符合本轮筛选条件"
		}
		message := fmt.Sprintf("跳过 %d 条资源：%s", r.counts[reason], label)
		if len(r.samples[reason]) > 0 {
			message += "；样本：" + strings.Join(r.samples[reason], "、")
		}
		emit(ctx, "resource.filter", "filter", "skipped", message, map[string]any{"reason": reason, "rejected": r.counts[reason], "samples": r.samples[reason], "omitted": r.counts[reason] - len(r.samples[reason])})
	}
}

func followEpisodeDisplay(values []FollowEpisodeCoordinate) string {
	if len(values) == 0 {
		return "无"
	}
	parts := []string{}
	for index, value := range values {
		if index == 20 {
			parts = append(parts, fmt.Sprintf("另 %d 集", len(values)-index))
			break
		}
		parts = append(parts, fmt.Sprintf("S%02dE%02d", value.Season, value.Episode))
	}
	return strings.Join(parts, "、")
}
