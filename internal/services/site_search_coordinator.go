package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func (s *SiteService) runSiteSearches(ctx context.Context, records []models.Site, run func(context.Context, models.Site) SiteSearchGroup, emit func(SiteSearchGroup), emitProgress func(SiteSearchProgress)) error {
	detailed, _ := ctx.Value(detailedSearchKey{}).(bool)
	if len(records) == 0 {
		if emitProgress != nil {
			progress := SiteSearchProgress{EventSequence: 1}
			if detailed {
				progress.ProgressVersion, progress.Phase = 2, "finished"
			}
			emitProgress(progress)
		}
		return nil
	}
	if s.searchSlots == nil {
		s.searchSlots = make(chan struct{}, defaultSiteSearchConcurrency)
	}
	page, _ := ctx.Value(searchPageKey{}).(int)
	if page < 1 {
		page = 1
	}
	progress := SiteSearchProgress{Total: len(records), Pending: len(records)}
	budget := newSearchSnapshotBudget()
	counts := map[uint]int{}
	var eventMu sync.Mutex
	publishProgress := func(detail *SiteSearchDetail, status, code string) {
		progress.EventSequence++
		progress.SiteDetail = nil
		if detailed {
			progress.ProgressVersion = 2
			if detail != nil {
				copy := *detail
				progress.SiteDetail = &copy
			}
		}
		if detail != nil {
			progress.SiteID, progress.SiteName, progress.SiteStatus, progress.ErrorCode = detail.SiteID, detail.SiteName, status, code
		}
		if emitProgress != nil {
			emitProgress(progress)
		}
	}
	publishProgress(nil, "", "")
	var wait sync.WaitGroup
	for _, record := range records {
		record := record
		wait.Add(1)
		go func() {
			defer wait.Done()
			siteCtx := serverlog.WithBusinessFields(ctx, map[string]any{"site_id": record.ID, "site_name": record.Name})
			started := time.Now()
			detail := SiteSearchDetail{SiteID: record.ID, SiteName: record.Name, Phase: "queued"}
			var latest SiteSearchGroup
			update := func(phase string) {
				detail.Phase, detail.ElapsedMS = phase, time.Since(started).Milliseconds()
				detail.Revision++
				publishProgress(&detail, "running", "")
			}
			eventMu.Lock()
			if detailed {
				detail.Revision++
				publishProgress(&detail, "pending", "")
			}
			eventMu.Unlock()
			acquired := false
			select {
			case s.searchSlots <- struct{}{}:
				acquired = true
			case <-ctx.Done():
			}
			if acquired {
				defer func() { <-s.searchSlots }()
			}
			eventMu.Lock()
			progress.Pending--
			progress.Running++
			update("preparing_site")
			eventMu.Unlock()
			publish := func(group SiteSearchGroup, final bool) bool {
				eventMu.Lock()
				defer eventMu.Unlock()
				detail.QueriesCompleted, detail.QueriesFailed, detail.QueriesTotal = group.QueriesCompleted, group.QueriesFailed, group.QueriesTotal
				detail.Revision++
				group.Revision = detail.Revision
				complete := final
				group.Complete = &complete
				keepGoing := true
				if detailed {
					group.ProgressVersion = 2
					group, keepGoing = budget.admit(group)
				}
				latest = cloneSearchGroup(group)
				progress.ResultCount += len(group.Items) - counts[record.ID]
				counts[record.ID] = len(group.Items)
				detail.ResultCount = len(group.Items)
				if emit != nil && (final || detailed && group.Status == "success") && ctx.Err() != context.Canceled {
					encoded, _ := json.Marshal(group)
					if final || budget.interim+len(encoded)+32 <= budget.interimLimit {
						if !final {
							budget.interim += len(encoded) + 32
						}
						s.markSearchClaimsPublished(group)
						emit(cloneSearchGroup(group))
					}
				}
				if !final && detailed {
					update("processing_results")
				}
				return keepGoing
			}
			observer := &siteSearchObserver{
				phase: func(phase string) {
					if detailed && ctx.Err() == nil {
						eventMu.Lock()
						update(phase)
						eventMu.Unlock()
					}
				},
				query: func(index, total int, name string) {
					eventMu.Lock()
					detail.QueryIndex, detail.QueriesTotal, detail.QueryName = index, total, serverlog.BusinessDisplayName(name)
					if detailed {
						update("preparing_site")
					}
					eventMu.Unlock()
				},
				snapshot: func(group SiteSearchGroup) bool { return publish(group, false) },
			}
			siteCtx = context.WithValue(siteCtx, siteSearchObserverKey{}, observer)
			workCtx, cancel := context.WithTimeoutCause(siteCtx, s.siteTimeout(record), errors.New(CodeSearchSiteBudget))
			group := emptySearchGroup(record, page, searchContextError(ctx, CodeSiteUnavailable))
			if acquired && ctx.Err() == nil {
				s.recordSearch(workCtx, "site.search.start", "search", "started", fmt.Sprintf("开始搜索站点 %s，逐名称查询，单站预算 %d 秒", serverlog.BusinessDisplayName(record.Name), int(s.siteTimeout(record)/time.Second)), nil)
				group = run(workCtx, record)
			}
			// The observer owns published truth. Even a later stage returning only
			// an error must not retract a batch already handed to the client.
			if latest.SiteID != 0 {
				group.Items = mergeSearchItems(latest.Items, group.Items)
				group.QueriesCompleted = max(group.QueriesCompleted, latest.QueriesCompleted)
				group.QueriesFailed = max(group.QueriesFailed, latest.QueriesFailed)
				group.QueriesTotal = max(group.QueriesTotal, latest.QueriesTotal)
				if latest.QueriesCompleted > 0 && group.Status != "success" {
					group.Partial, group.IncompleteReason = true, firstNonEmpty(group.ErrorCode, firstNonEmpty(group.IncompleteReason, CodeSiteUnavailable))
					group.Status, group.ErrorCode = "success", ""
				}
			}
			if group.QueriesTotal == 0 {
				group.QueriesTotal = 1
				if group.Status == "success" {
					group.QueriesCompleted = 1
				} else if acquired {
					group.QueriesFailed = 1
				}
			}
			if workCtx.Err() != nil {
				code := searchContextError(workCtx, CodeSearchSiteBudget)
				if group.QueriesCompleted > 0 {
					group.Status, group.ErrorCode, group.Partial, group.IncompleteReason = "success", "", true, code
				} else {
					group.Status, group.ErrorCode, group.IncompleteReason = "error", code, code
				}
			}
			cancel()
			publish(group, true)
			group = latest
			observeFollowSite(ctx, record.ID, group.Status == "success" && !group.Partial)
			outcome, message := "finished", fmt.Sprintf("站点 %s 搜索完成，正常查询 %d/%d 次，保留 %d 条结果", serverlog.BusinessDisplayName(record.Name), group.QueriesCompleted, group.QueriesTotal, len(group.Items))
			if group.Partial {
				outcome, message = "partial", fmt.Sprintf("站点 %s 搜索未完整完成：%s；已保留 %d 条可用结果", serverlog.BusinessDisplayName(record.Name), siteBusinessFailure(group.IncompleteReason), len(group.Items))
			}
			if group.Status != "success" {
				outcome, message = "failed", fmt.Sprintf("站点 %s 搜索失败：%s；不视为没有资源", serverlog.BusinessDisplayName(record.Name), siteBusinessFailure(group.ErrorCode))
			}
			if errors.Is(ctx.Err(), context.Canceled) {
				outcome, message = "cancelled", "本轮搜索已取消，未取得完整结果"
			}
			s.recordSearch(ctx, "site.search.finish", "search", outcome, message, map[string]any{"duration_ms": time.Since(started).Milliseconds(), "identity_kept": len(group.Items), "failed_count": group.QueriesFailed})
			eventMu.Lock()
			progress.Running--
			progress.Completed++
			if group.Status == "success" {
				progress.Succeeded++
			} else {
				progress.Failed++
			}
			detail.Phase, detail.ElapsedMS = "finished", time.Since(started).Milliseconds()
			detail.Revision++
			publishProgress(&detail, group.Status, firstNonEmpty(group.ErrorCode, group.IncompleteReason))
			eventMu.Unlock()
		}()
	}
	wait.Wait()
	if ctx.Err() != nil {
		return appError(searchContextError(ctx, CodeSearchBudget), "本轮搜索未完整完成，已取得的结果予以保留", nil)
	}
	return nil
}
