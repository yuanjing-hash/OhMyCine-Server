package services

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/site/builtin"
)

const (
	CodeSearchRequestTimeout = "request_timeout"
	CodeSearchSiteBudget     = "site_budget_exceeded"
	CodeSearchRateWait       = "rate_limit_wait_exceeded"
	CodeSearchBudget         = "search_budget_exceeded"
	CodeSearchCancelled      = "cancelled"
	CodeSearchResultLimit    = "result_limit"
	SearchResponseTimeout    = 390 * time.Second
)

// All budgets are children of the caller's context. Tests may inject short
// durations without waiting for the production limits.
type siteSearchPolicy struct {
	Interactive, Follow, TorznabSite, PluginSite, NativeSite, RateWait time.Duration
	Query                                                              time.Duration
}

var defaultSearchPolicy = siteSearchPolicy{
	Interactive: 360 * time.Second, Follow: 10 * time.Minute,
	TorznabSite: 300 * time.Second, PluginSite: 180 * time.Second,
	NativeSite: 180 * time.Second, RateWait: 20 * time.Second,
}

func (s *SiteService) searchPolicyValue() siteSearchPolicy {
	if s.searchPolicy != nil {
		return *s.searchPolicy
	}
	return defaultSearchPolicy
}

func normalizeSearchTimeout(value *int) (*int, error) {
	if value == nil || *value == 0 {
		return nil, nil
	}
	if *value < 3 || *value > 180 {
		return nil, appError(CodeInvalidRequest, "搜索请求超时须为 3 到 180 秒，0 表示自动", nil)
	}
	copy := *value
	return &copy, nil
}

func effectiveSearchTimeout(record models.Site) int {
	if record.SearchTimeoutSeconds != nil && *record.SearchTimeoutSeconds >= 3 && *record.SearchTimeoutSeconds <= 180 {
		return *record.SearchTimeoutSeconds
	}
	if record.Kind == "torznab" {
		return 120
	}
	if record.SourceType == "plugin" {
		return 35
	}
	if record.TimeoutSeconds >= 3 && record.TimeoutSeconds <= 30 {
		return record.TimeoutSeconds
	}
	return 12
}

func (s *SiteService) queryTimeout(record models.Site) time.Duration {
	if duration := s.searchPolicyValue().Query; duration > 0 {
		return duration
	}
	return time.Duration(effectiveSearchTimeout(record)) * time.Second
}

func (s *SiteService) siteTimeout(record models.Site) time.Duration {
	policy := s.searchPolicyValue()
	if record.Kind == "torznab" {
		return policy.TorznabSite
	}
	if record.SourceType == "plugin" {
		return policy.PluginSite
	}
	return policy.NativeSite
}

type followSearchBudgetKey struct{}
type searchPageKey struct{}

func (s *SiteService) searchRunContext(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := s.searchPolicyValue().Interactive
	if ctx.Value(followSearchBudgetKey{}) != nil {
		budget = s.searchPolicyValue().Follow
	}
	return context.WithTimeoutCause(ctx, budget, errors.New(CodeSearchBudget))
}

func searchContextError(ctx context.Context, fallback string) string {
	if ctx.Err() == nil {
		return fallback
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return CodeSearchCancelled
	}
	if cause := context.Cause(ctx); cause != nil {
		switch cause.Error() {
		case CodeSearchBudget, CodeSearchSiteBudget, CodeSearchRequestTimeout, CodeSearchRateWait:
			return cause.Error()
		}
	}
	return fallback
}

func searchRequestError(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return searchContextError(ctx, CodeSearchRequestTimeout)
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return CodeSearchRequestTimeout
	}
	return siteErrorCode(err)
}

type siteSearchObserverKey struct{}
type siteSearchObserver struct {
	phase    func(string)
	query    func(int, int, string)
	snapshot func(SiteSearchGroup) bool
}

// ReportResourceSearchPhase accepts host-owned phases only. No provider text,
// credentials or URLs may be used as progress labels.
func ReportResourceSearchPhase(ctx context.Context, phase string) {
	switch phase {
	case "preparing_media", "queued", "preparing_site", "restoring_session", "checking_session", "waiting_rate_limit", "querying", "processing_results", "finished":
	default:
		return
	}
	if observer, ok := ctx.Value(siteSearchObserverKey{}).(*siteSearchObserver); ok && observer.phase != nil {
		observer.phase(phase)
	}
}

func reportSearchQuery(ctx context.Context, index, total int, name string) {
	if observer, ok := ctx.Value(siteSearchObserverKey{}).(*siteSearchObserver); ok && observer.query != nil {
		observer.query(index, total, name)
	}
}

func publishSearchSnapshot(ctx context.Context, group SiteSearchGroup) bool {
	if observer, ok := ctx.Value(siteSearchObserverKey{}).(*siteSearchObserver); ok && observer.snapshot != nil {
		return observer.snapshot(group)
	}
	return true
}

func (s *SiteService) markSearchClaimsPublished(group SiteSearchGroup) {
	s.vaultMu.Lock()
	defer s.vaultMu.Unlock()
	for _, result := range group.Items {
		if claim, ok := s.vault[result.Token]; ok {
			claim.Published = true
			s.vault[result.Token] = claim
		}
	}
}

func (s *SiteService) prepareSiteSearch(ctx context.Context, record models.Site) error {
	if record.SourceType != "plugin" {
		return nil
	}
	if bridge, ok := s.pluginResources.(interface {
		PrepareResourceSearch(context.Context, string) error
	}); ok {
		return bridge.PrepareResourceSearch(ctx, record.PluginConnectionID)
	}
	return nil
}

func searchPreparationFailure(record models.Site, page, total int, err error) SiteSearchGroup {
	group := emptySearchGroup(record, page, ErrorCode(err))
	group.QueriesTotal, group.IncompleteReason = total, group.ErrorCode
	return group
}

func emptySearchGroup(record models.Site, page int, code string) SiteSearchGroup {
	definition, _ := builtin.DefinitionForKey(record.Kind)
	siteType := definition.SiteType
	if record.SourceType == "plugin" {
		siteType = "bt_resource"
	}
	return SiteSearchGroup{SiteID: record.ID, SiteName: record.Name, SiteType: siteType, Page: page, Status: "error", ErrorCode: code, Items: []SiteSearchResult{}}
}

func (s *SiteService) waitSearchLimit(ctx context.Context, record models.Site) error {
	waitCtx, cancel := context.WithTimeoutCause(ctx, s.searchPolicyValue().RateWait, errors.New(CodeSearchRateWait))
	defer cancel()
	if err := s.waitLimit(waitCtx, record); err != nil {
		if ctx.Err() != nil {
			return appError(searchContextError(ctx, CodeSearchSiteBudget), "搜索已取消或预算耗尽", nil)
		}
		return appError(CodeSearchRateWait, "等待站点限速名额超过上限，未发起本次查询", nil)
	}
	return nil
}
