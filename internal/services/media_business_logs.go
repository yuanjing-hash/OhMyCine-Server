package services

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func recordMediaBusiness(ctx context.Context, log zerolog.Logger, operation serverlog.Operation, code, stage, outcome, message string, fields map[string]any) {
	serverlog.RecordBusiness(ctx, log, operation, serverlog.BusinessEvent{Code: code, Stage: stage, Outcome: outcome, Message: message, Fields: fields})
}

type artifactBusinessState struct {
	mu               sync.Mutex
	sampled, omitted int
	counts           map[string]int
}
type artifactBusinessKey struct{}

// Accepts only the product's MediaArtifact.RelativePath namespace (historically
// stored with one leading slash). It is NOT an arbitrary provider/host-path
// sanitizer. Callers have already proved a destination in the artifact scope:
// writeLocalArtifact uses storagefs.Constrain(root, target); retained full-audit
// rows passed catalogSTRMArtifactsHealthy/fullRecognitionArtifactsHealthy or
// artifactFileMatches; cleanup uses safeCleanupTarget plus root identity.
// Cloud/provider source paths must never be passed here. Removing the namespace
// slash only formats that already-proven local artifact location for display.
func artifactDisplayPath(relative string) string {
	return serverlog.BusinessRelativePath(strings.TrimPrefix(relative, "/"))
}

func (s *MediaArtifactService) observeLocalArtifact(ctx context.Context, run models.MediaArtifactRun, kind, relative, outcome, reason string, size int64, writeErr error, physicallyWritten bool) {
	fields := map[string]any{"library_id": run.LibraryID, "task_id": run.ID, "generation": run.Generation, "action": kind, "reason": reason}
	if run.JobID != nil {
		fields["job_id"] = *run.JobID
	}
	state, _ := ctx.Value(artifactBusinessKey{}).(*artifactBusinessState)
	if state != nil {
		state.mu.Lock()
		state.counts[outcome]++
		if outcome == "skipped" && reason != "" {
			state.counts[reason]++
		}
		if writeErr != nil {
			state.counts["failed"]++
		}
		if outcome == "skipped" && writeErr == nil {
			if state.sampled >= 20 {
				state.omitted++
				state.mu.Unlock()
				return
			}
			state.sampled++
		}
		state.mu.Unlock()
	}
	status, message := "confirmed", ""
	safeRelative := artifactDisplayPath(relative)
	if safeRelative != "" {
		fields["relative_path"], fields["file_name"] = safeRelative, path.Base(safeRelative)
	}
	if size >= 0 {
		fields["size_bytes"] = size
	}
	scope := "媒体库产物区"
	if safeRelative != "" {
		scope += " / " + safeRelative
	}
	switch outcome {
	case "written":
		message = fmt.Sprintf("已生成 %s：%s（%d 字节）；仅确认产物写入，发布与媒体服务器刷新分别处理", artifactKindLabel(kind), scope, size)
	case "updated":
		message = fmt.Sprintf("已替换受管 %s：%s（%d 字节）；旧内容更新完成，未提前宣称上游索引完成", artifactKindLabel(kind), scope, size)
	case "skipped":
		status = "skipped"
		message = "保留 " + artifactKindLabel(kind) + "：" + scope + "；" + artifactSkipLabel(reason)
	default:
		message = "产物未完成：" + artifactKindLabel(kind) + "；未确认写入完成"
	}
	if writeErr != nil {
		status, fields["error_code"] = "failed", "artifact_write_failed"
		message = "产物阶段失败：" + artifactKindLabel(kind) + "；未确认完整结果，未输出私有目录或上游响应"
		if physicallyWritten {
			status, message = "uncertain", "产物文件写入已发生，但后续确认或清单保存失败；保留原任务等待对账，不能记为生成全部完成"
		}
	}
	recordMediaBusiness(ctx, s.log, serverlog.OperationMediaArtifact, "artifact.file", "artifact_write", status, message, fields)
}

func artifactKindLabel(kind string) string {
	switch kind {
	case models.MediaArtifactKindSTRM:
		return "STRM"
	case models.MediaArtifactKindNFO:
		return "NFO"
	case "poster", "fanart", "thumb", "image", "source_poster", "source_fanart":
		return "图片"
	case "subtitle":
		return "字幕"
	default:
		return "媒体旁挂文件"
	}
}
func artifactSkipLabel(reason string) string {
	switch reason {
	case "unmanaged_collision":
		return "同名文件不属于系统，未覆盖"
	case "unmanaged":
		return "非系统受管文件，保持原样"
	case "content_reused":
		return "现有受管内容已核验，直接复用，没有重复写文件"
	case "snapshot_incomplete":
		return "元数据快照不完整，暂不生成 NFO"
	case "no_media":
		return "没有可关联媒体，未生成文件"
	default:
		return "当前条件无需改写"
	}
}

func artifactCleanupSkipLabel(reason string) string {
	switch reason {
	case "artifact_cleanup_run_incomplete":
		return "产物生成尚未全部确认完成"
	case "artifact_cleanup_snapshot_changed", "artifact_cleanup_generation_changed":
		return "目录或产物版本已变化，保留当前文件等待新版本处理"
	case "artifact_cleanup_snapshot_missing":
		return "缺少可确认的清理快照，不扩大删除范围"
	case "artifact_cleanup_policy_disabled":
		return "当前配置没有启用此类清理"
	case "artifact_cleanup_scan_ineligible":
		return "本轮扫描范围或状态不满足自动清理条件"
	default:
		return "未满足安全清理条件，保留原文件"
	}
}

func mediaScanKindLabel(kind string) string {
	switch kind {
	case "full", "strm_full_manual":
		return "全量核对"
	case "event", "incremental", "catch_up", "strm_incremental_manual":
		return "增量核对"
	case "initial":
		return "首次扫描"
	default:
		return "指定范围核对"
	}
}

func mediaScanActionLabel(action string) string {
	switch action {
	case "added":
		return "新增"
	case "updated":
		return "更新"
	case "removed":
		return "移除索引"
	default:
		return "变更"
	}
}

type mediaScanSample struct{ action, title, mediaType string }
type mediaScanSamples struct{ items []mediaScanSample }
type mediaScanSamplesKey struct{}

func observeCatalogScanSample(ctx context.Context, action, title, mediaType string) {
	if action == "unchanged" {
		return
	}
	if samples, _ := ctx.Value(mediaScanSamplesKey{}).(*mediaScanSamples); samples != nil && len(samples.items) < 20 {
		samples.items = append(samples.items, mediaScanSample{action, title, mediaType})
	}
}

func (s *MediaLibraryService) recordCatalogScanSamples(ctx context.Context, run models.MediaLibraryScanRun) {
	samples, _ := ctx.Value(mediaScanSamplesKey{}).(*mediaScanSamples)
	if samples == nil {
		return
	}
	operation := mediaLibraryScanOperation(run.Kind)
	for _, sample := range samples.items {
		recordMediaBusiness(ctx, s.log, operation, "library.catalog.changed", "publishing", "confirmed", fmt.Sprintf("媒体目录%s：%s；记录已提交，扫描未对源文件执行移动或删除", mediaScanActionLabel(sample.action), serverlog.BusinessDisplayName(sample.title)), map[string]any{"library_id": run.LibraryID, "scan_run_id": run.ID, "action": sample.action, "media_display_name": sample.title, "media_type": sample.mediaType})
	}
	changed := run.Added + run.Updated + run.Removed
	if changed > len(samples.items) {
		recordMediaBusiness(ctx, s.log, operation, "library.catalog.samples", "publishing", "confirmed", fmt.Sprintf("目录实际变更 %d 条，默认展示 %d 条样本，另 %d 条仅计入汇总", changed, len(samples.items), changed-len(samples.items)), map[string]any{"library_id": run.LibraryID, "scan_run_id": run.ID, "sample_count": len(samples.items), "omitted_count": changed - len(samples.items), "total_count": changed})
	}
}

func (s *MediaArtifactService) observeArtifactEnd(ctx context.Context, run models.MediaArtifactRun, state *artifactBusinessState, started time.Time, result WorkerResult) {
	var current models.MediaArtifactRun
	if s.db.First(&current, "id = ?", run.ID).Error == nil {
		run = current
	}
	state.mu.Lock()
	fields := map[string]any{"duration_ms": time.Since(started).Milliseconds(), "generated_count": state.counts["written"], "updated_count": state.counts["updated"], "reused_count": state.counts["content_reused"], "skipped_count": state.counts["skipped"] - state.counts["content_reused"], "failed_count": state.counts["failed"], "omitted_count": state.omitted, "status": run.Status}
	state.mu.Unlock()
	outcome, message := "partial", "本次产物执行已结束，当前未确认全部产物及发布流程完成"
	switch {
	case ctx.Err() != nil:
		outcome, message = "cancelled", "产物执行已停止，未将取消记为文件生成完成"
	case result.RetryAt != nil:
		outcome, message = "waiting", "产物执行等待边界或对账恢复，保留原执行记录，不重复声明写入成功"
	case result.ErrorCode != "":
		outcome, message = "failed", "产物执行失败，已确认的单文件结果保留；完整结果请结合错误码核对"
	case run.Status == models.MediaArtifactStatusSuperseded:
		outcome, message = "skipped", "本轮产物已被更新的目录版本接管，没有继续覆盖新版本"
	case run.Status == models.MediaArtifactStatusCompleted:
		outcome, message = "finished", fmt.Sprintf("产物生成阶段结束：本次文件级确认新写 %v、替换 %v、核验复用 %v、条件跳过或保留 %v；另省略 %v 条未改写明细，目录发布和上游刷新按各自阶段确认", fields["generated_count"], fields["updated_count"], fields["reused_count"], fields["skipped_count"], fields["omitted_count"])
	}
	if result.ErrorCode != "" {
		fields["error_code"] = result.ErrorCode
	}
	recordMediaBusiness(ctx, s.log, serverlog.OperationMediaArtifact, "artifact.finish", "finish", outcome, message, fields)
}
