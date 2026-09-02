package services

import (
	"context"
	"errors"
	"sync"
	"time"

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
			select {
			case s.searchSlots <- struct{}{}:
				defer func() { <-s.searchSlots }()
			case <-ctx.Done():
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
			group := run(siteCtx, record)
			timedOut := errors.Is(siteCtx.Err(), context.DeadlineExceeded)
			cancel()
			if timedOut {
				group.Status = "error"
				group.ErrorCode = CodeSiteUnavailable
			}

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
