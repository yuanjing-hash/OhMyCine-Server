package services

import (
	"context"
	"errors"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	sitepkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/site"
)

type ResourceContentResolver interface {
	Resolve(context.Context, resourcecontent.ResolveRequest) (resourcecontent.Manifest, error)
}

type inspectedContent struct {
	Manifest        *resourcecontent.Manifest
	Entries         []cloud.ShareTreeItem
	RootName        string
	OriginalMagnet  string
	PluginLifecycle string
	ExpiresAt       time.Time
}

type contentReadFlight struct {
	done           chan struct{}
	cancel         context.CancelFunc
	waiters        int
	result         inspectedContent
	err            error
	waiterContexts map[uint64]context.Context
	nextWaiter     uint64
}

// Err performs the same last-waiter cancellation synchronously before a tree
// advances to another page. Done/Err still belong to the real cancel context.
// This closes the small gap between a caller cancellation and its select waking.
type contentOwnedContext struct {
	context.Context
	beforeErr func()
}

func (c contentOwnedContext) Err() error { c.beforeErr(); return c.Context.Err() }

type siteContentPreview struct {
	ActorID                                                       uint
	ResultToken, SourceDigest, IdentityDigest, AuthorizationScope string
	ExpiresAt                                                     time.Time
	Scope                                                         *ProviderInspectionScope
	Read                                                          inspectedContent
	Entries                                                       map[string]SiteSharePreviewEntry
	Native                                                        map[string]cloud.ShareTreeItem
	TorrentFiles                                                  map[string]resourcecontent.File
	AnalysisFiles                                                 []siteShareAnalyzedFile
	Summary                                                       ContentPreviewSummary
}

type siteContentSelection struct {
	ActorID                   uint
	ResultToken, PreviewToken string
	EntryTokens               []string
	ExpiresAt                 time.Time
	SubmissionDigest          string
}

type siteContentInspection struct {
	mu           sync.Mutex
	resolver     ResourceContentResolver
	flights      map[string]*contentReadFlight
	cache        map[string]inspectedContent
	previews     map[string]siteContentPreview
	selections   map[string]siteContentSelection
	sourceSlots  chan struct{}
	groupSlots   chan struct{}
	groupCache   map[string]contentGroupRecognition
	groupFlights map[string]*contentGroupFlight
}

func (s *SiteService) contents() *siteContentInspection {
	s.vaultMu.Lock()
	defer s.vaultMu.Unlock()
	if s.contentInspection == nil {
		s.contentInspection = &siteContentInspection{resolver: resourcecontent.NewEngine(resourcecontent.EngineConfig{}), flights: map[string]*contentReadFlight{}, cache: map[string]inspectedContent{}, previews: map[string]siteContentPreview{}, selections: map[string]siteContentSelection{}, sourceSlots: make(chan struct{}, 4), groupSlots: make(chan struct{}, 4), groupCache: map[string]contentGroupRecognition{}, groupFlights: map[string]*contentGroupFlight{}}
	}
	return s.contentInspection
}

// SetResourceContentResolver replaces only the metadata protocol engine; it is
// an internal composition seam, not a public URL/peer-policy option.
func (s *SiteService) SetResourceContentResolver(resolver ResourceContentResolver) {
	state := s.contents()
	state.mu.Lock()
	defer state.mu.Unlock()
	if resolver != nil {
		state.resolver = resolver
	}
}

func contentSourceDigest(claim siteResultClaim, record models.Site) string {
	return inspectionDigest([]any{record.ID, record.Revision, record.SourceType, record.PluginID, record.PluginConnectionID, claim.TorrentID, claim.SourceKind, claim.PayloadKind, claim.PluginClaimID, claim.PluginLifecycle})
}

// A cache flight owns its context. A cancelled first card does not cancel a
// second waiter; the last waiter cancels and the protocol engine joins its I/O.
func (s *SiteService) readContentShared(ctx context.Context, key string, load func(context.Context) (inspectedContent, error)) (inspectedContent, error) {
	state := s.contents()
	state.mu.Lock()
	if cached, ok := state.cache[key]; ok && cached.ExpiresAt.After(s.now()) {
		state.mu.Unlock()
		return cached, nil
	}
	flight := state.flights[key]
	if flight == nil {
		if len(state.flights) >= 32 {
			state.mu.Unlock()
			return inspectedContent{}, appError(CodeSiteUnavailable, "内容检查排队较多，请稍后重试", nil)
		}
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		flight = &contentReadFlight{done: make(chan struct{}), cancel: cancel, waiterContexts: map[uint64]context.Context{}}
		state.flights[key] = flight
		go func() {
			ownedContext := contentOwnedContext{Context: work, beforeErr: func() {
				state.mu.Lock()
				defer state.mu.Unlock()
				for _, waiting := range flight.waiterContexts {
					if waiting.Err() == nil {
						return
					}
				}
				flight.cancel()
			}}
			result, err := load(ownedContext)
			owned := work.Err() == nil
			cancel()
			state.mu.Lock()
			flight.result, flight.err = result, err
			delete(state.flights, key)
			if err == nil && owned && result.ExpiresAt.After(s.now()) {
				for id, cached := range state.cache {
					if !cached.ExpiresAt.After(s.now()) {
						delete(state.cache, id)
					}
				}
				for len(state.cache) > 0 && (len(state.cache) >= 128 || contentCacheSize(state.cache)+contentReadSize(result) > 32<<20) {
					oldest := ""
					var expiry time.Time
					for id, cached := range state.cache {
						if oldest == "" || cached.ExpiresAt.Before(expiry) {
							oldest, expiry = id, cached.ExpiresAt
						}
					}
					delete(state.cache, oldest)
				}
				if contentReadSize(result) <= 32<<20 {
					state.cache[key] = result
				}
			}
			close(flight.done)
			state.mu.Unlock()
		}()
	}
	flight.waiters++
	waiterID := flight.nextWaiter
	flight.nextWaiter++
	flight.waiterContexts[waiterID] = ctx
	state.mu.Unlock()
	defer func() {
		state.mu.Lock()
		flight.waiters--
		delete(flight.waiterContexts, waiterID)
		if flight.waiters == 0 {
			flight.cancel()
		}
		state.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		state.mu.Lock()
		otherLive := false
		for id, waiting := range flight.waiterContexts {
			if id != waiterID && waiting.Err() == nil {
				otherLive = true
				break
			}
		}
		if !otherLive {
			flight.cancel()
		}
		state.mu.Unlock()
		if !otherLive {
			<-flight.done
		}
		return inspectedContent{}, ctx.Err()
	case <-flight.done:
		return flight.result, flight.err
	}
}

func (s *SiteService) readCloudContent(ctx context.Context, actor Actor, claim siteResultClaim, scope ProviderInspectionScope) (inspectedContent, error) {
	raw, provider, _, err := normalizeCloudShare(claim.TorrentID)
	if err != nil || provider+"_share" != claimCloudSource(claim) {
		return inspectedContent{}, appError(CodeDownloadSourceInvalid, "分享来源与读取授权不一致", nil)
	}
	if err := s.validateInspectionScope(ctx, actor, claim, scope); err != nil {
		return inspectedContent{}, err
	}
	driver, err := s.inspectionDriver(scope)
	if err != nil {
		return inspectedContent{}, err
	}
	release, err := s.acquireSharePreview(ctx)
	if err != nil {
		return inspectedContent{}, err
	}
	defer release()
	root, entries, err := cloud.InspectShareTree(ctx, driver, raw)
	if err != nil {
		return inspectedContent{}, sharePreviewProviderError(err)
	}
	if err := s.validateInspectionScope(ctx, actor, claim, scope); err != nil {
		return inspectedContent{}, err
	}
	return inspectedContent{Entries: entries, RootName: root.Title, ExpiresAt: s.now().Add(5 * time.Minute)}, nil
}

func (s *SiteService) readTorrentContent(ctx context.Context, actor Actor, claim siteResultClaim, record models.Site) (inspectedContent, error) {
	pluginLifecycle := ""
	state := s.contents()
	select {
	case state.sourceSlots <- struct{}{}:
	case <-ctx.Done():
		return inspectedContent{}, ctx.Err()
	}
	var once sync.Once
	releaseSource := func() { once.Do(func() { <-state.sourceSlots }) }
	defer releaseSource()
	var source sitepkg.Source
	if record.SourceType == "plugin" {
		if s.pluginResources == nil || claim.PluginClaimID == "" {
			return inspectedContent{}, contentInspectionExpired()
		}
		var saved models.PluginResourceClaim
		if s.db.WithContext(ctx).Where("id = ? AND owner_id = ? AND site_id = ? AND consumed_at IS NULL AND expires_at > ?", claim.PluginClaimID, actor.User.ID, record.ID, s.now()).First(&saved).Error != nil {
			return inspectedContent{}, contentInspectionExpired()
		}
		pluginID, version, err := s.pluginResources.ResourceProvenance(record.PluginConnectionID)
		if err != nil {
			return inspectedContent{}, err
		}
		if saved.PluginID != pluginID || saved.PluginVersion != version || saved.PluginConnectionID != record.PluginConnectionID || saved.ResourceID != claim.TorrentID {
			return inspectedContent{}, contentInspectionExpired()
		}
		lifecycle, err := pluginResourceLifecycle(s.db, record, pluginID, version)
		if err != nil || claim.PluginLifecycle != "" && lifecycle != claim.PluginLifecycle {
			return inspectedContent{}, contentInspectionExpired()
		}
		pluginLifecycle = lifecycle
		resolved, err := s.pluginResources.ResolveResource(ctx, PluginResourceResolveInput{ConnectionID: saved.PluginConnectionID, ResourceID: saved.ResourceID})
		if err != nil {
			return inspectedContent{}, err
		}
		source.Magnet = resolved.Magnet
	} else {
		current, config, adapter, err := s.runtimeConfig(record.ID)
		if err != nil || current.Revision != record.Revision {
			return inspectedContent{}, contentInspectionExpired()
		}
		if err := s.waitLimit(ctx, current); err != nil {
			return inspectedContent{}, err
		}
		if resolver, ok := adapter.(sitepkg.SourceResolver); ok {
			source, err = resolver.ResolveSource(ctx, config, claim.TorrentID)
		} else {
			source.Torrent, source.Filename, err = adapter.Download(ctx, config, claim.TorrentID)
		}
		if err != nil {
			return inspectedContent{}, siteAdapterError(err, "资源内容暂时无法读取")
		}
	}
	if source.ShareURL != "" || (len(source.Torrent) > 0) == (strings.TrimSpace(source.Magnet) != "") {
		return inspectedContent{}, appError(CodeDownloadSourceInvalid, "内容来源响应无效", nil)
	}
	private := false
	if sourceKind, err := routeSourceForSite(record); err != nil {
		return inspectedContent{}, err
	} else {
		private = sourceKind == RouteSourcePT
	}
	var manifest resourcecontent.Manifest
	var err error
	if len(source.Torrent) > 0 {
		manifest, err = resourcecontent.ParseTorrent(source.Torrent, resourcecontent.Limits{})
	} else {
		// PT tracker authority cannot be taken from an arbitrary magnet query.
		// Its original authenticated torrent is required before public discovery.
		if private {
			return inspectedContent{}, appError("resource_content_private_metadata_unavailable", "私有来源需先读取原始种子，未启用公开磁力发现", nil)
		}
		releaseSource() // Long peer metadata sessions have their own engine budget.
		state := s.contents()
		state.mu.Lock()
		resolver := state.resolver
		state.mu.Unlock()
		manifest, err = resolver.Resolve(ctx, resourcecontent.ResolveRequest{URI: source.Magnet, Private: false})
	}
	if err != nil {
		return inspectedContent{}, resourceContentError(err)
	}
	originalMagnet := ""
	if !private && !manifest.Private {
		originalMagnet = source.Magnet
	}
	return inspectedContent{Manifest: &manifest, RootName: manifest.Name, OriginalMagnet: originalMagnet, PluginLifecycle: pluginLifecycle, ExpiresAt: s.now().Add(5 * time.Minute)}, nil
}

func resourceContentError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return appError("resource_content_unavailable", "实际文件清单尚未取得或未通过校验，可稍后重试；未创建下载任务", nil)
}

func contentPage(summary ContentPreviewSummary, offset, limit int) (ContentPreviewSummary, error) {
	if offset < 0 || offset > len(summary.Files) || limit < 0 || limit > 200 {
		return ContentPreviewSummary{}, appError(CodeInvalidRequest, "文件清单分页参数无效", nil)
	}
	if limit == 0 {
		limit = 100
	}
	end := min(offset+limit, len(summary.Files))
	summary.Files = append([]ContentFileFacts{}, summary.Files[offset:end]...)
	summary.DisplayedCount = len(summary.Files)
	if end < summary.TotalCount {
		summary.NextOffset = &end
	}
	return summary, nil
}

func (s *SiteService) PreviewContent(ctx context.Context, actor Actor, input ContentPreviewInput) (ContentPreviewSummary, error) {
	if input.Offset < 0 || input.Offset > 10000 || input.Limit < 0 || input.Limit > 200 {
		return ContentPreviewSummary{}, appError(CodeInvalidRequest, "文件分页参数无效", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, 65*time.Second)
	defer cancel()
	actor, err := s.inspectionActor(ctx, actor)
	if err != nil {
		return ContentPreviewSummary{}, err
	}
	claim, record, err := s.contentClaim(ctx, actor, input.ResultToken)
	if err != nil {
		return ContentPreviewSummary{}, err
	}
	if input.PreviewToken != "" {
		preview, err := s.resolveContentPreview(ctx, actor, claim, input.PreviewToken)
		if err != nil {
			return ContentPreviewSummary{}, err
		}
		if preview.ResultToken != strings.TrimSpace(input.ResultToken) {
			return ContentPreviewSummary{}, contentInspectionExpired()
		}
		return contentPage(preview.Summary, input.Offset, input.Limit)
	}
	var scope *ProviderInspectionScope
	if isCloudShareSource(claimCloudSource(claim)) {
		selected, err := s.providerInspectionScope(ctx, actor, claim, input.DownloaderID)
		if err != nil {
			return ContentPreviewSummary{}, err
		}
		scope = &selected
	} else if claim.SourceKind == "external_link" || claim.CloudProvider != "" {
		return ContentPreviewSummary{}, appError(CodeDownloadSourceInvalid, "该来源尚不支持内容检查", nil)
	}
	sourceDigest := contentSourceDigest(claim, record)
	pluginDigest, err := s.contentPluginLifecycle(s.db.WithContext(ctx), actor, claim, record)
	if err != nil {
		return ContentPreviewSummary{}, err
	}
	scopeDigest := ""
	if scope != nil {
		scopeDigest = scope.Digest
	}
	// Reuse only the exact result binding. A different opaque result keeps its
	// own receipt even when the immutable raw source is shared by the cache.
	state := s.contents()
	state.mu.Lock()
	var reused *siteContentPreview
	for _, saved := range state.previews {
		if saved.ActorID == actor.User.ID && saved.ResultToken == strings.TrimSpace(input.ResultToken) && saved.SourceDigest == sourceDigest && saved.Read.PluginLifecycle == pluginDigest && saved.IdentityDigest == shareAnalysisIdentityDigest(claim) && saved.AuthorizationScope == discoveryAuthorizationScope(actor) && saved.ExpiresAt.After(s.now()) && (saved.Scope == nil && scope == nil || saved.Scope != nil && scope != nil && saved.Scope.Digest == scope.Digest) {
			copy := saved
			reused = &copy
			break
		}
	}
	state.mu.Unlock()
	if reused != nil {
		return contentPage(reused.Summary, input.Offset, input.Limit)
	}
	key := inspectionDigest([]any{actor.User.ID, discoveryAuthorizationScope(actor), sourceDigest, scopeDigest, pluginDigest})
	read, err := s.readContentShared(ctx, key, func(work context.Context) (inspectedContent, error) {
		if scope != nil {
			return s.readCloudContent(work, actor, claim, *scope)
		}
		return s.readTorrentContent(work, actor, claim, record)
	})
	if err != nil {
		return ContentPreviewSummary{}, err
	}
	if read.PluginLifecycle != pluginDigest {
		return ContentPreviewSummary{}, contentInspectionExpired()
	}
	if _, err := s.inspectionActor(ctx, actor); err != nil {
		return ContentPreviewSummary{}, err
	}
	if _, err := s.validateContentClaim(ctx, actor, claim); err != nil {
		return ContentPreviewSummary{}, err
	}
	if scope != nil {
		if err := s.validateInspectionScope(ctx, actor, claim, *scope); err != nil {
			return ContentPreviewSummary{}, err
		}
	}
	expires := minTime(claim.ExpiresAt, read.ExpiresAt)
	preview := siteContentPreview{ActorID: actor.User.ID, ResultToken: strings.TrimSpace(input.ResultToken), SourceDigest: sourceDigest, IdentityDigest: shareAnalysisIdentityDigest(claim), AuthorizationScope: discoveryAuthorizationScope(actor), ExpiresAt: expires, Scope: scope, Read: read, Entries: map[string]SiteSharePreviewEntry{}, Native: map[string]cloud.ShareTreeItem{}, TorrentFiles: map[string]resourcecontent.File{}}
	summary := ContentPreviewSummary{Version: 1, Token: "content-" + uuid.NewString(), Status: "complete", Complete: true, Files: []ContentFileFacts{}, CheckedAt: s.now(), ExpiresAt: expires, Message: "已读取实际文件清单；检查没有创建下载任务"}
	var entries []SiteSharePreviewEntry
	if read.Manifest != nil {
		for _, file := range read.Manifest.Files {
			if file.Padding {
				continue
			}
			name := file.Path
			if read.Manifest.MultiFile {
				name = path.Join(read.Manifest.Name, file.Path)
			}
			token := uuid.NewString()
			entry := SiteSharePreviewEntry{Token: token, Path: name, Name: path.Base(name), Size: file.Size}
			entries = append(entries, entry)
			preview.TorrentFiles[token] = file
		}
	} else {
		for _, file := range read.Entries {
			token := uuid.NewString()
			entries = append(entries, SiteSharePreviewEntry{Token: token, Path: file.RelativePath, Name: path.Base(file.RelativePath), Size: file.Size, IsDir: file.IsDir})
			preview.Native[token] = file
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	var analyzed []siteShareAnalyzedFile
	summary.Analysis, analyzed = s.analyzeShareEntries(ctx, actor, claim, entries)
	preview.AnalysisFiles = analyzed
	byToken := map[string]siteShareAnalyzedFile{}
	for _, file := range analyzed {
		byToken[file.Token] = file
	}
	for _, entry := range entries {
		preview.Entries[entry.Token] = entry
		if !entry.IsDir {
			if entry.Size < 0 || summary.TotalSize > math.MaxInt64-entry.Size {
				return ContentPreviewSummary{}, appError(CodeInvalidRequest, "文件总量超出范围", nil)
			}
			summary.TotalSize += entry.Size
		}
		summary.Files = append(summary.Files, contentFileProjection(entry, claim, summary.Analysis, byToken))
	}
	summary.TotalCount = len(summary.Files)
	summary.Analysis.CheckedAt = summary.CheckedAt
	summary.Analysis.ExpiresAt = &expires
	if _, err := s.inspectionActor(ctx, actor); err != nil {
		return ContentPreviewSummary{}, err
	}
	if _, err := s.validateContentClaim(ctx, actor, claim); err != nil {
		return ContentPreviewSummary{}, err
	}
	latest, latestSite, err := s.contentClaim(ctx, actor, input.ResultToken)
	if err != nil || shareAnalysisIdentityDigest(latest) != preview.IdentityDigest || contentSourceDigest(latest, latestSite) != preview.SourceDigest {
		return ContentPreviewSummary{}, contentInspectionExpired()
	}
	latestLifecycle, err := s.contentPluginLifecycle(s.db.WithContext(ctx), actor, latest, latestSite)
	if err != nil || latestLifecycle != read.PluginLifecycle {
		return ContentPreviewSummary{}, contentInspectionExpired()
	}
	if scope != nil {
		if err := s.validateInspectionScope(ctx, actor, claim, *scope); err != nil {
			return ContentPreviewSummary{}, err
		}
	}
	preview.Summary = summary
	state = s.contents()
	state.mu.Lock()
	err = s.storeContentPreviewLocked(state, summary.Token, preview)
	state.mu.Unlock()
	if err != nil {
		return ContentPreviewSummary{}, err
	}
	return contentPage(summary, input.Offset, input.Limit)
}

const contentPreviewBudget = 64 << 20
const contentActorPreviewBudget = 32 << 20
const contentPreviewLimit = 128
const contentActorPreviewLimit = 64

// Called with state.mu held. Selections pin their original previews until expiry;
// budget exhaustion rejects the new preview instead of retiring a valid intent.
func (s *SiteService) storeContentPreviewLocked(state *siteContentInspection, token string, preview siteContentPreview) error {
	for id, saved := range state.previews {
		if !saved.ExpiresAt.After(s.now()) {
			delete(state.previews, id)
		}
	}
	protected := map[string]bool{}
	for token, selection := range state.selections {
		if !selection.ExpiresAt.After(s.now()) {
			delete(state.selections, token)
		} else {
			protected[selection.PreviewToken] = true
		}
	}
	newBytes := retainedContentPreviewSize(preview)
	actorCount, actorBytes := contentActorPreviewSize(state.previews, preview.ActorID)
	for len(state.previews) > 0 && (len(state.previews) >= contentPreviewLimit || contentPreviewSize(state.previews)+newBytes > contentPreviewBudget || actorCount >= contentActorPreviewLimit || actorBytes+newBytes > contentActorPreviewBudget) {
		oldest := ""
		var expiry time.Time
		for id, saved := range state.previews {
			if !protected[id] && (actorCount < contentActorPreviewLimit && actorBytes+newBytes <= contentActorPreviewBudget || saved.ActorID == preview.ActorID) && (oldest == "" || saved.ExpiresAt.Before(expiry)) {
				oldest, expiry = id, saved.ExpiresAt
			}
		}
		if oldest == "" {
			break
		}
		delete(state.previews, oldest)
		actorCount, actorBytes = contentActorPreviewSize(state.previews, preview.ActorID)
	}
	if len(state.previews) >= contentPreviewLimit || contentPreviewSize(state.previews)+newBytes > contentPreviewBudget || actorCount >= contentActorPreviewLimit || actorBytes+newBytes > contentActorPreviewBudget {
		return appError(CodeSiteUnavailable, "有效内容预览较多，请稍后重试", nil)
	}
	state.previews[token] = preview
	return nil
}

func contentReadSize(read inspectedContent) int {
	size := len(read.RootName) + len(read.OriginalMagnet) + len(read.PluginLifecycle)
	if read.Manifest != nil {
		size += len(read.Manifest.InfoBytes) + len(read.Manifest.MetainfoBytes)
		for _, file := range read.Manifest.Files {
			size += len(file.Path) + 128
		}
	}
	for _, file := range read.Entries {
		size += len(file.RelativePath) + len(file.ID) + 128
	}
	return size
}
func contentCacheSize(cache map[string]inspectedContent) int {
	size := 0
	for _, read := range cache {
		size += contentReadSize(read)
	}
	return size
}
func contentPreviewSize(previews map[string]siteContentPreview) int {
	size := 0
	for _, preview := range previews {
		size += retainedContentPreviewSize(preview)
	}
	return size
}

func retainedContentPreviewSize(preview siteContentPreview) int {
	size := contentReadSize(preview.Read) + 1024
	for token, entry := range preview.Entries {
		size += len(token) + len(entry.Path) + len(entry.Name) + 192
	}
	for token, entry := range preview.Native {
		size += len(token) + len(entry.ID) + len(entry.RelativePath) + 192
	}
	for token, file := range preview.TorrentFiles {
		size += len(token) + len(file.Path) + 192
	}
	for _, file := range preview.Summary.Files {
		size += len(file.Token) + len(file.Path) + len(file.Name) + len(file.WorkStatus) + len(file.Reason) + len(file.Episodes)*8 + len(file.Subtitles)*96 + 256
	}
	for _, file := range preview.AnalysisFiles {
		size += len(file.Token) + len(file.Path) + len(file.Title) + len(file.Episodes)*8 + 128
	}
	if preview.Summary.Analysis != nil {
		for _, group := range preview.Summary.Analysis.Groups {
			size += len(group.Title) + len(group.PosterURL) + len(group.Episodes)*8 + 256
		}
	}
	return size
}
func contentActorPreviewSize(previews map[string]siteContentPreview, actorID uint) (int, int) {
	count, size := 0, 0
	for _, preview := range previews {
		if preview.ActorID == actorID {
			count++
			size += retainedContentPreviewSize(preview)
		}
	}
	return count, size
}

func (s *SiteService) resolveContentPreview(ctx context.Context, actor Actor, claim siteResultClaim, token string) (siteContentPreview, error) {
	state := s.contents()
	state.mu.Lock()
	preview, ok := state.previews[strings.TrimSpace(token)]
	state.mu.Unlock()
	if !ok || preview.ActorID != actor.User.ID || !preview.ExpiresAt.After(s.now()) || preview.AuthorizationScope != discoveryAuthorizationScope(actor) || preview.IdentityDigest != shareAnalysisIdentityDigest(claim) {
		return preview, contentInspectionExpired()
	}
	record, err := s.validateContentClaim(ctx, actor, claim)
	if err != nil || preview.SourceDigest != contentSourceDigest(claim, record) {
		return preview, contentInspectionExpired()
	}
	lifecycle, err := s.contentPluginLifecycle(s.db.WithContext(ctx), actor, claim, record)
	if err != nil || lifecycle != preview.Read.PluginLifecycle {
		return preview, contentInspectionExpired()
	}
	if preview.Scope != nil {
		if err := s.validateInspectionScope(ctx, actor, claim, *preview.Scope); err != nil {
			return preview, err
		}
	}
	return preview, nil
}
