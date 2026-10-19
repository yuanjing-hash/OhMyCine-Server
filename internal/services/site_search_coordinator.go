package services

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func (s *SiteService) runSiteSearches(
	ctx context.Context,
	records []models.Site,
	run func(context.Context, models.Site) SiteSearchGroup,
	emit func(SiteSearchGroup),
	emitProgress func(SiteSearchProgress),
) error {
	if len(records) == 0 {
		return nil
	}
	if s.searchSlots == nil {
		s.searchSlots = make(chan struct{}, defaultSiteSearchConcurrency)
	}

	progress := SiteSearchProgress{Total: len(records), Pending: len(records)}
	var eventMu sync.Mutex
	publishProgress := func(snapshot SiteSearchProgress) {
		if emitProgress != nil {
			emitProgress(snapshot)
		}
	}
	eventMu.Lock()
	publishProgress(progress)
	eventMu.Unlock()

	var wait sync.WaitGroup
	for _, record := range records {
		record := record
		wait.Add(1)
		go func() {
			defer wait.Done()
			ctx := serverlog.WithBusinessFields(ctx, map[string]any{"site_id": record.ID, "site_name": record.Name})
			select {
			case s.searchSlots <- struct{}{}:
				defer func() { <-s.searchSlots }()
			case <-ctx.Done():
				s.recordSearch(ctx, "site.search.cancelled", "search", "cancelled", "等待搜索并发名额时本轮已取消，未访问该站点", nil)
				return
			}

			eventMu.Lock()
			progress.Pending--
			progress.Running++
			progress.SiteID = record.ID
			progress.SiteName = record.Name
			progress.SiteStatus = "running"
			progress.ErrorCode = ""
			publishProgress(progress)
			eventMu.Unlock()

			timeout := time.Duration(record.TimeoutSeconds) * time.Second
			if timeout <= 0 {
				timeout = 15 * time.Second
			}
			siteCtx, cancel := context.WithTimeout(ctx, timeout)
			started := time.Now()
			s.recordSearch(siteCtx, "site.search.start", "search", "started", fmt.Sprintf("开始搜索站点 %s，按本轮名称依次查询；超时上限 %d 秒", serverlog.BusinessDisplayName(record.Name), int(timeout/time.Second)), nil)
			group := run(siteCtx, record)
			timedOut := errors.Is(siteCtx.Err(), context.DeadlineExceeded)
			cancel()
			if timedOut {
				group.Status = "error"
				group.ErrorCode = CodeSiteUnavailable
			}
			observeFollowSite(ctx, record.ID, group.Status == "success")
			outcome, message := "finished", fmt.Sprintf("站点 %s 搜索结束，身份与去重后保留 %d 条，跳过 %d 条，失败的别名查询 %d 次", serverlog.BusinessDisplayName(record.Name), len(group.Items), group.Skipped, group.ErrorCount)
			fields := map[string]any{"duration_ms": time.Since(started).Milliseconds(), "identity_kept": len(group.Items), "skipped_count": group.Skipped, "failed_count": group.ErrorCount}
			if group.Status != "success" {
				outcome, message = "failed", fmt.Sprintf("站点 %s 搜索失败：%s；不视为没有资源", serverlog.BusinessDisplayName(record.Name), siteBusinessFailure(group.ErrorCode))
				fields["error_code"] = group.ErrorCode
			} else if group.ErrorCount > 0 {
				outcome = "partial"
			}
			if timedOut {
				fields["reason"] = "timeout"
				message = fmt.Sprintf("站点 %s 搜索超过 %d 秒限制，未取得完整结果", serverlog.BusinessDisplayName(record.Name), int(timeout/time.Second))
			}
			if ctx.Err() != nil {
				outcome, message = "cancelled", "站点搜索已取消，未将不完整结果当作完整搜索"
			}
			s.recordSearch(ctx, "site.search.finish", "search", outcome, message, fields)

			eventMu.Lock()
			if emit != nil && ctx.Err() == nil {
				emit(group)
			}
			progress.Running--
			progress.Completed++
			progress.ResultCount += len(group.Items)
			if group.Status == "success" {
				progress.Succeeded++
			} else {
				progress.Failed++
			}
			progress.SiteID = group.SiteID
			progress.SiteName = group.SiteName
			progress.SiteStatus = group.Status
			progress.ErrorCode = group.ErrorCode
			publishProgress(progress)
			eventMu.Unlock()
		}()
	}
	wait.Wait()
	if ctx.Err() != nil {
		return appError(CodeSiteUnavailable, "种子资源搜索已取消", ctx.Err())
	}
	return nil
}
