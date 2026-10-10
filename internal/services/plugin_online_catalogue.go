package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/plugins/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	pluginCatalogueNavigationTTL  = 12 * time.Hour
	pluginCatalogueFeedTTL        = 15 * time.Minute
	pluginCatalogueStaleTTL       = 24 * time.Hour
	maxPluginCatalogueBytes       = 512 << 10
	maxPluginCatalogueTotalBytes  = 64 << 20
	maxPluginCatalogueLibraryRows = 128
	maxPluginCatalogueRows        = 4096
)

type catalogueFlight struct {
	done    chan struct{}
	raw     json.RawMessage
	session string
	err     error
}
type catalogueRetry struct {
	until    time.Time
	failures int
}
type pluginCatalogueCache struct {
	mu         sync.Mutex
	flights    map[string]*catalogueFlight
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	now        func() time.Time
	wake       chan struct{}
	foreground atomic.Int32
	retries    map[string]catalogueRetry
}
type pluginCatalogueScope struct {
	library    models.PluginOnlineLibrary
	connection models.PluginConnection
	manifest   contract.Manifest
	packageID  uint
	generation uint64
	key        string
}

func (s *PluginRepositoryService) catalogueState() *pluginCatalogueCache {
	s.catalogueOnce.Do(func() {
		s.catalogue = &pluginCatalogueCache{flights: make(map[string]*catalogueFlight), retries: make(map[string]catalogueRetry), now: time.Now, wake: make(chan struct{}, 1)}
	})
	return s.catalogue
}

func (s *PluginRepositoryService) foregroundOnline() func() {
	state := s.catalogueState()
	state.foreground.Add(1)
	return func() { state.foreground.Add(-1) }
}

func (s *PluginRepositoryService) catalogueScope(libraryID string) (pluginCatalogueScope, error) {
	library, connection, manifest, err := s.onlineLibrary(libraryID)
	if err != nil {
		return pluginCatalogueScope{}, err
	}
	var installation models.PluginInstallation
	if err = s.db.First(&installation, "plugin_id = ? AND status = ?", connection.PluginID, models.PluginInstallationEnabled).Error; err != nil {
		return pluginCatalogueScope{}, err
	}
	// Runtime generation is a commit fence, not the durable identity: a reboot
	// starts a fresh guest while unchanged public catalogue snapshots survive.
	key := catalogueHash(fmt.Sprintf("v1\x00%s\x00%s\x00%s\x00%d\x00%s", library.ID, connection.ID, manifest.PackageSHA256, connection.CredentialVersion, connection.ConfigJSON))
	return pluginCatalogueScope{library: library, connection: connection, manifest: manifest, packageID: installation.ActivePackageID, generation: installation.RuntimeGeneration, key: key}, nil
}
func catalogueHash(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }
func catalogueID(scope pluginCatalogueScope, kind, selector, extra string) string {
	return catalogueHash(scope.key + "\x00" + kind + "\x00" + selector + "\x00" + extra)
}

// catalogueRead is used only after request authorization. A stale hit starts a
// coalesced refresh but never resurrects another package/account/config scope.
func (s *PluginRepositoryService) catalogueRead(ctx context.Context, scope pluginCatalogueScope, kind, selector string, depth int, ancestors []string, force bool) (json.RawMessage, string, error) {
	state := s.catalogueState()
	now := state.now().UTC()
	id := catalogueID(scope, kind, selector, "")
	var row models.PluginCatalogueSnapshot
	err := s.db.WithContext(ctx).First(&row, "id = ? AND scope_key = ? AND stale_until > ?", id, scope.key, now).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, "", err
	}
	if err == nil && !force {
		if _, validateErr := s.sanitizeCatalogue(scope, kind, json.RawMessage(row.ResponseJSON), row.RefreshSession, depth, ancestors); validateErr == nil {
			if err := checkOnlineScopeTx(s.db.WithContext(ctx), scope); err != nil {
				return nil, "", err
			}
			if !row.FreshUntil.After(now) && !row.RetryAt.After(now) {
				s.queueCatalogueRefresh(scope, kind, selector, depth, ancestors)
			}
			return json.RawMessage(row.ResponseJSON), row.RefreshSession, nil
		}
	}
	return s.refreshCatalogue(ctx, scope, kind, selector, depth, ancestors)
}

func (s *PluginRepositoryService) refreshCatalogue(ctx context.Context, scope pluginCatalogueScope, kind, selector string, depth int, ancestors []string) (json.RawMessage, string, error) {
	state := s.catalogueState()
	id := catalogueID(scope, kind, selector, "")
	state.mu.Lock()
	if flight := state.flights[id]; flight != nil {
		state.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-flight.done:
			return append(json.RawMessage(nil), flight.raw...), flight.session, flight.err
		}
	}
	flight := &catalogueFlight{done: make(chan struct{})}
	if len(state.flights) >= 32 {
		state.mu.Unlock()
		return nil, "", appError(CodePluginOnlineRateLimited, "在线目录请求繁忙，请稍后重试", nil)
	}
	state.flights[id] = flight
	state.mu.Unlock()
	defer func() { state.mu.Lock(); delete(state.flights, id); close(flight.done); state.mu.Unlock() }()
	request := map[string]any{"connectionId": scope.connection.ID}
	capability := contract.CapabilitySiteNavigation
	session := ""
	ttl := pluginCatalogueNavigationTTL
	if kind == "navigation" {
		request["depth"] = depth
		if selector != "" {
			request["parentNodeKey"] = selector
		}
	} else {
		capability = contract.CapabilitySiteFeed
		session = uuid.NewString()
		ttl = pluginCatalogueFeedTTL
		request["routeKey"] = selector
		request["cursor"] = nil
		request["refreshSession"] = session
	}
	raw, err := s.InvokePlugin(ctx, scope.connection.ID, string(capability), request)
	if err == nil {
		raw, err = checkCataloguePluginError(raw, capability)
	}
	liveRaw := append(json.RawMessage(nil), raw...)
	if kind == "feed" && err == nil {
		liveRaw, err = contract.NormalizeFeedSections(raw, session)
	}
	if err == nil {
		raw, err = s.sanitizeCatalogue(scope, kind, raw, session, depth, ancestors)
	}
	if err == nil {
		current, currentErr := s.catalogueScope(scope.library.ID)
		if currentErr != nil || current.key != scope.key || current.packageID != scope.packageID || current.generation != scope.generation {
			err = appError(CodePluginOnlineLibraryUnavailable, "在线目录来源已变更，请重试", nil)
		}
	}
	now := state.now().UTC()
	if err == nil {
		ancestorsJSON, _ := json.Marshal(ancestors)
		row := models.PluginCatalogueSnapshot{ID: id, LibraryID: scope.library.ID, ScopeKey: scope.key, Kind: kind, Selector: selector, Depth: depth, AncestorsJSON: string(ancestorsJSON), ResponseJSON: string(raw), RefreshSession: session, FreshUntil: now.Add(ttl), StaleUntil: now.Add(pluginCatalogueStaleTTL), RetryAt: now, UpdatedAt: now}
		err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// Short transaction: no provider invocation or network inside it.
			if e := checkOnlineScopeTx(tx, scope); e != nil {
				return e
			}
			if e := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, UpdateAll: true}).Create(&row).Error; e != nil {
				return e
			}
			if kind == "feed" {
				page := models.PluginFeedCache{LibraryID: scope.library.ID, RouteKey: selector, CursorKey: catalogueHash(""), RefreshSession: session, ScopeKey: scope.key, ResponseJSON: string(raw), ExpiresAt: now.Add(30 * time.Minute), CreatedAt: now, UpdatedAt: now}
				if e := tx.Create(&page).Error; e != nil {
					return e
				}
			}
			return pruneCatalogueTx(tx, scope.library.ID, now)
		})
	} else {
		var previous models.PluginCatalogueSnapshot
		if s.db.First(&previous, "id = ?", id).Error == nil {
			failures := previous.Failures + 1
			if failures > 6 {
				failures = 6
			}
			delay := time.Minute * time.Duration(1<<uint(failures-1))
			_ = s.db.Model(&models.PluginCatalogueSnapshot{}).Where("id = ? AND scope_key = ?", id, scope.key).Updates(map[string]any{"failures": failures, "retry_at": now.Add(delay)}).Error
		}
	}
	if err == nil {
		raw = liveRaw
	}
	state.mu.Lock()
	if err == nil {
		delete(state.retries, id)
	} else if ctx.Err() == nil {
		previous := state.retries[id]
		for key, retry := range state.retries {
			if !retry.until.After(now) {
				delete(state.retries, key)
			}
		}
		if len(state.retries) < maxPluginCatalogueRows || previous.failures > 0 {
			previous.failures++
			if previous.failures > 6 {
				previous.failures = 6
			}
			previous.until = now.Add(time.Minute * time.Duration(1<<uint(previous.failures-1)))
			state.retries[id] = previous
		}
	}
	state.mu.Unlock()
	flight.raw, flight.session, flight.err = raw, session, err
	return append(json.RawMessage(nil), raw...), session, err
}

func checkCataloguePluginError(raw json.RawMessage, capability contract.Capability) (json.RawMessage, error) {
	var value map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '{' && json.Unmarshal(raw, &value) == nil {
		if _, exists := value["pluginError"]; exists {
			var envelope pluginErrorEnvelope
			if json.Unmarshal(raw, &envelope) != nil || envelope.PluginError == nil || envelope.PluginError.Code == "" {
				return nil, appError(CodePluginResponseInvalid, "在线目录响应无效", nil)
			}
			return nil, mapPluginOnlineErrorReason(envelope.PluginError.Code, envelope.PluginError.Reason, capability)
		}
	}
	return raw, nil
}

func (s *PluginRepositoryService) sanitizeCatalogue(scope pluginCatalogueScope, kind string, raw json.RawMessage, session string, depth int, ancestors []string) (json.RawMessage, error) {
	if len(raw) > maxPluginCatalogueBytes {
		return nil, appError(CodePluginResponseInvalid, "在线目录响应过大", nil)
	}
	if kind == "feed" {
		validated, err := contract.NormalizeFeedSections(raw, session)
		if err != nil {
			return nil, appError(CodePluginResponseInvalid, "在线媒体栏目响应无效", err)
		}
		var sections []contract.FeedSection
		_ = json.Unmarshal(validated, &sections)
		for si := range sections {
			if !safeCatalogueCursor(sections[si].Cursor) {
				sections[si].Cursor = ""
			}
			for ii := range sections[si].Items {
				work := &sections[si].Items[ii].Work
				work.PosterURL = durableCatalogueArtwork(work.PosterURL)
				work.BackdropURL = durableCatalogueArtwork(work.BackdropURL)
			}
		}
		return json.Marshal(sections)
	}
	// Validate via the same strict live-navigation contract, but persist the
	// guest's stable node keys, never process-private signed node tokens.
	if scope.manifest.NavigationMode == "hierarchical" {
		if _, err := s.normalizeHierarchicalNavigation(scope.library.ID, raw, depth, ancestors); err != nil {
			return nil, err
		}
		var input struct {
			Version int                         `json:"version"`
			Mode    string                      `json:"mode"`
			Nodes   []pluginNavigationInputNode `json:"nodes"`
		}
		_ = json.Unmarshal(raw, &input)
		return json.Marshal(input)
	}
	var nodes []struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		PageType    string `json:"pageType,omitempty"`
		Kind        string `json:"kind,omitempty"`
		RouteKey    string `json:"routeKey,omitempty"`
		Refreshable bool   `json:"refreshable,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&nodes); err != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) || len(nodes) > 100 {
		return nil, appError(CodePluginResponseInvalid, "在线媒体导航响应无效", err)
	}
	for _, node := range nodes {
		if !safeOnlineText(node.ID, 128) || !safeOnlineText(node.Title, 256) || !safeOptionalOnlineText(node.RouteKey, 256) {
			return nil, appError(CodePluginResponseInvalid, "在线媒体导航节点无效", nil)
		}
	}
	return json.Marshal(nodes)
}
func safeCatalogueCursor(value string) bool {
	return value == "" || safeOnlineText(value, 512) && !strings.Contains(value, "://") && !strings.ContainsAny(value, "?&=")
}
func durableCatalogueArtwork(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || len(value) > 2048 {
		return ""
	}
	return value
}

func pruneCatalogueTx(tx *gorm.DB, libraryID string, now time.Time) error {
	if err := tx.Where("stale_until <= ?", now).Delete(&models.PluginCatalogueSnapshot{}).Error; err != nil {
		return err
	}
	var ids []string
	if err := tx.Model(&models.PluginCatalogueSnapshot{}).Where("library_id = ?", libraryID).Order("updated_at DESC,id ASC").Offset(maxPluginCatalogueLibraryRows).Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) > 0 {
		if err := tx.Where("id IN ?", ids).Delete(&models.PluginCatalogueSnapshot{}).Error; err != nil {
			return err
		}
	}
	ids = nil
	if err := tx.Model(&models.PluginCatalogueSnapshot{}).Order("updated_at DESC,id ASC").Offset(maxPluginCatalogueRows).Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) > 0 {
		if err := tx.Where("id IN ?", ids).Delete(&models.PluginCatalogueSnapshot{}).Error; err != nil {
			return err
		}
	}
	var total int64
	if err := tx.Model(&models.PluginCatalogueSnapshot{}).Select("COALESCE(SUM(length(response_json)),0)").Scan(&total).Error; err != nil {
		return err
	}
	if total > maxPluginCatalogueTotalBytes*3/4 {
		var rows []models.PluginCatalogueSnapshot
		if err := tx.Select("id,length(response_json) AS depth").Order("updated_at ASC,id ASC").Find(&rows).Error; err != nil {
			return err
		}
		ids = nil
		for _, row := range rows {
			if total <= maxPluginCatalogueTotalBytes*3/4 {
				break
			}
			ids = append(ids, row.ID)
			total -= int64(row.Depth)
		}
		if len(ids) > 0 {
			if err := tx.Where("id IN ?", ids).Delete(&models.PluginCatalogueSnapshot{}).Error; err != nil {
				return err
			}
		}
	}
	return prunePluginFeedTx(tx, libraryID, now)
}

func prunePluginFeedTx(tx *gorm.DB, libraryID string, now time.Time) error {
	if err := tx.Where("expires_at <= ?", now).Delete(&models.PluginFeedCache{}).Error; err != nil {
		return err
	}
	var ids []uint
	if err := tx.Model(&models.PluginFeedCache{}).Where("library_id = ?", libraryID).Order("updated_at DESC,id DESC").Offset(128).Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) > 0 {
		if err := tx.Where("id IN ?", ids).Delete(&models.PluginFeedCache{}).Error; err != nil {
			return err
		}
	}
	ids = nil
	if err := tx.Model(&models.PluginFeedCache{}).Order("updated_at DESC,id DESC").Offset(512).Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) > 0 {
		if err := tx.Where("id IN ?", ids).Delete(&models.PluginFeedCache{}).Error; err != nil {
			return err
		}
	}
	var rows []struct {
		ID    uint
		Bytes int64
	}
	if err := tx.Model(&models.PluginFeedCache{}).Select("id,length(response_json) AS bytes").Order("updated_at DESC,id DESC").Find(&rows).Error; err != nil {
		return err
	}
	var total int64
	ids = nil
	for _, row := range rows {
		total += row.Bytes
		if total > maxPluginCatalogueTotalBytes/4 {
			ids = append(ids, row.ID)
		}
	}
	if len(ids) > 0 {
		return tx.Where("id IN ?", ids).Delete(&models.PluginFeedCache{}).Error
	}
	return nil
}

// This is fixed cache maintenance, not a user-configurable Cron business job.
// Startup is asynchronous and shutdown cancels/joins before guest teardown.
func (s *PluginRepositoryService) StartOnlineCatalogue(ctx context.Context) {
	state := s.catalogueState()
	state.mu.Lock()
	if state.cancel != nil {
		state.mu.Unlock()
		return
	}
	ctx, state.cancel = context.WithCancel(ctx)
	state.wg.Add(1)
	state.mu.Unlock()
	go func() {
		defer state.wg.Done()
		s.warmOnlineCatalogue(ctx)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.warmOnlineCatalogue(ctx)
			case <-state.wake:
				s.warmOnlineCatalogue(ctx)
			}
		}
	}()
}
func (s *PluginRepositoryService) CloseOnlineCatalogue() {
	state := s.catalogueState()
	state.mu.Lock()
	cancel := state.cancel
	state.mu.Unlock()
	if cancel != nil {
		cancel()
		state.wg.Wait()
		state.mu.Lock()
		state.cancel = nil
		state.mu.Unlock()
	}
}
func (s *PluginRepositoryService) queueCatalogueRefresh(scope pluginCatalogueScope, kind, selector string, depth int, ancestors []string) {
	state := s.catalogueState()
	state.mu.Lock()
	if state.cancel == nil {
		state.mu.Unlock()
		return
	}
	if len(state.flights) >= 2 {
		state.mu.Unlock()
		return
	}
	state.mu.Unlock()
	// Wake the one worker: no unbounded goroutines or caller context reuse.
	select {
	case state.wake <- struct{}{}:
	default:
	}
}
func (s *PluginRepositoryService) warmOnlineCatalogue(ctx context.Context) {
	// Expired metadata is cleaned even when the last online library is disabled.
	now := s.catalogueState().now().UTC()
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("stale_until <= ?", now).Delete(&models.PluginCatalogueSnapshot{}).Error; err != nil {
			return err
		}
		if err := tx.Where("expires_at <= ?", now).Delete(&models.PluginFeedCache{}).Error; err != nil {
			return err
		}
		if err := tx.Where("expires_at <= ?", now).Delete(&models.PluginOnlineMediaIdentity{}).Error; err != nil {
			return err
		}
		return tx.Where("expires_at <= ?", now).Delete(&models.PluginOnlinePlaybackReceipt{}).Error
	}); err != nil {
		return
	}
	libraries, err := s.enabledOnlineLibraries("")
	if err != nil {
		return
	}
	for _, library := range libraries {
		if ctx.Err() != nil {
			return
		}
		scope, err := s.catalogueScope(library.ID)
		if err != nil {
			continue
		}
		_ = s.db.Where("library_id = ? AND scope_key <> ?", library.ID, scope.key).Delete(&models.PluginCatalogueSnapshot{}).Error
		if !manifestHasCapability(scope.manifest, contract.CapabilitySiteNavigation) {
			continue
		}
		root, _, err := s.refreshDueCatalogue(ctx, scope, "navigation", "", 0, nil)
		if err != nil {
			continue
		}
		var tree struct {
			Nodes []pluginNavigationInputNode `json:"nodes"`
		}
		_ = json.Unmarshal(root, &tree)
		if scope.manifest.NavigationMode != "hierarchical" {
			_ = json.Unmarshal(root, &tree.Nodes)
			for i := range tree.Nodes {
				if tree.Nodes[i].RouteKey != "" {
					tree.Nodes[i].Kind = "feed"
				}
			}
		}
		feeds := 0
		for _, node := range tree.Nodes {
			if ctx.Err() != nil {
				return
			}
			if node.Kind == "feed" && feeds < 32 && manifestHasCapability(scope.manifest, contract.CapabilitySiteFeed) {
				_, _, _ = s.refreshDueCatalogue(ctx, scope, "feed", node.RouteKey, 0, nil)
				feeds++
				continue
			}
			if node.Kind != "branch" {
				continue
			}
			child, _, err := s.refreshDueCatalogue(ctx, scope, "navigation", node.NodeKey, 1, []string{node.NodeKey})
			if err != nil {
				continue
			}
			var level struct {
				Nodes []pluginNavigationInputNode `json:"nodes"`
			}
			_ = json.Unmarshal(child, &level)
			for _, leaf := range level.Nodes {
				if leaf.Kind == "feed" && feeds < 32 && manifestHasCapability(scope.manifest, contract.CapabilitySiteFeed) {
					_, _, _ = s.refreshDueCatalogue(ctx, scope, "feed", leaf.RouteKey, 0, nil)
					feeds++
					break
				}
			}
		}
		var observed []models.PluginCatalogueSnapshot
		now := s.catalogueState().now().UTC()
		if s.db.Where("library_id = ? AND scope_key = ? AND fresh_until <= ? AND retry_at <= ?", library.ID, scope.key, now, now).Order("updated_at ASC").Limit(maxPluginCatalogueLibraryRows).Find(&observed).Error != nil {
			continue
		}
		for _, row := range observed {
			if ctx.Err() != nil {
				return
			}
			var ancestors []string
			_ = json.Unmarshal([]byte(row.AncestorsJSON), &ancestors)
			_, _, _ = s.refreshDueCatalogue(ctx, scope, row.Kind, row.Selector, row.Depth, ancestors)
		}
		_ = s.db.Where("expires_at <= ? OR (library_id = ? AND scope_key <> ?)", now, library.ID, scope.key).Delete(&models.PluginFeedCache{}).Error
	}
}
func (s *PluginRepositoryService) refreshDueCatalogue(ctx context.Context, scope pluginCatalogueScope, kind, selector string, depth int, ancestors []string) (json.RawMessage, string, error) {
	if s.catalogueState().foreground.Load() > 0 {
		return nil, "", appError(CodePluginOnlineRateLimited, "在线目录预热等待用户请求完成", nil)
	}
	var row models.PluginCatalogueSnapshot
	now := s.catalogueState().now().UTC()
	state := s.catalogueState()
	state.mu.Lock()
	retry := state.retries[catalogueID(scope, kind, selector, "")]
	state.mu.Unlock()
	if retry.until.After(now) {
		return nil, "", appError(CodePluginOnlineRateLimited, "在线目录暂缓刷新", nil)
	}
	if s.db.First(&row, "id = ?", catalogueID(scope, kind, selector, "")).Error == nil && (row.FreshUntil.After(now) || row.RetryAt.After(now)) {
		if _, err := s.sanitizeCatalogue(scope, kind, json.RawMessage(row.ResponseJSON), row.RefreshSession, depth, ancestors); err == nil {
			return json.RawMessage(row.ResponseJSON), row.RefreshSession, nil
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return s.refreshCatalogue(ctx, scope, kind, selector, depth, ancestors)
}
