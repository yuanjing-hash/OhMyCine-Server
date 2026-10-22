package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
)

func (s *SiteService) recordSearch(ctx context.Context, code, stage, outcome, message string, fields map[string]any) {
	serverlog.RecordBusiness(ctx, s.log, serverlog.OperationDiscoverySearch, serverlog.BusinessEvent{Code: code, Stage: stage, Outcome: outcome, Message: message, Fields: fields})
}

func (s *SiteService) recordSearchResult(ctx context.Context, group SiteSearchGroup, started time.Time, stage string, requested bool, raw int) {
	outcome := "finished"
	message := fmt.Sprintf("站点 %s 查询结束：适配器返回 %d 条，搜索凭据可用 %d 条，适配器另跳过 %d 条；同一资源可能在不同名称查询中重复返回", serverlog.BusinessDisplayName(group.SiteName), raw, len(group.Items), group.Skipped)
	fields := map[string]any{"duration_ms": time.Since(started).Milliseconds(), "raw_results": raw, "result_count": len(group.Items), "skipped_count": group.Skipped}
	if group.Status != "success" {
		delete(fields, "raw_results")
		delete(fields, "result_count")
		delete(fields, "skipped_count")
		outcome = "failed"
		fields["error_code"] = group.ErrorCode
		message = fmt.Sprintf("站点 %s 查询失败：%s；不视为零结果", serverlog.BusinessDisplayName(group.SiteName), siteBusinessFailure(group.ErrorCode))
		if !requested {
			message += "；尚未发起站点请求"
		}
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = "failed"
			if group.Status == "success" {
				outcome = "partial"
			}
			fields["reason"], message = "timeout", "站点查询超过本轮超时限制，未取得完整结果"
		} else {
			outcome, message = "cancelled", "站点查询已取消，未取得完整结果"
		}
	}
	s.recordSearch(ctx, "site.query.finish", stage, outcome, message, fields)
}

func siteBusinessFailure(code string) string {
	switch code {
	case CodeSearchRequestTimeout:
		return "本次上游查询超时"
	case CodeSearchSiteBudget:
		return "该站本轮搜索预算耗尽"
	case CodeSearchRateWait:
		return "等待共享限速名额超时，本次查询未发送"
	case CodeSearchBudget:
		return "整轮搜索等待时间已到"
	case CodeSearchCancelled:
		return "用户取消搜索"
	case CodeSearchResultLimit:
		return "已达到安全结果容量上限"
	case CodeSiteCredentialInvalid:
		return "站点凭据不可用"
	case CodePermissionDenied:
		return "当前用户无权使用该站点"
	case "site_rate_limited":
		return "站点请求受限或正在冷却"
	case "site_response_invalid", "site_parse_failed":
		return "站点响应无法解析"
	case CodeSiteUnavailable:
		return "连接不可用或请求超时"
	default:
		return "站点未返回可确认的搜索结果，请结合错误码检查"
	}
}
