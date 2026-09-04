package services

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

const maxProviderChangeScopeItems = 512

type providerEventPayload struct {
	Kind             string `json:"kind"`
	ItemID           string `json:"item_id"`
	ParentID         string `json:"parent_id"`
	PreviousParentID string `json:"previous_parent_id"`
	Name             string `json:"name"`
}

type providerChangeEvent struct {
	Kind             string
	ItemID           string
	ParentID         string
	PreviousParentID string
	Name             string
}

type providerChangeScope struct {
	Events        []providerChangeEvent
	ParentIDs     []string
	DeliveryIDs   []uint
	EventCount    int
	DeliveryMaxID uint
	FullFallback  bool
	FallbackCode  string
}

type providerChangeScopeContextKey struct{}

func withProviderChangeScope(ctx context.Context, scope providerChangeScope) context.Context {
	return context.WithValue(ctx, providerChangeScopeContextKey{}, scope)
}

func providerChangeScopeFromContext(ctx context.Context) (providerChangeScope, bool) {
	if ctx == nil {
		return providerChangeScope{}, false
	}
	scope, ok := ctx.Value(providerChangeScopeContextKey{}).(providerChangeScope)
	return scope, ok
}

func (s providerChangeScope) empty() bool {
	return !s.FullFallback && len(s.Events) == 0 && len(s.ParentIDs) == 0
}

// providerChangeAccumulator is shared by one supervisor and its provider
// listener. It bounds unique identities, coalesces event storms by stable item
// identity, and never exposes its private provider facts outside the service.
type providerChangeAccumulator struct {
	mu            sync.Mutex
	events        map[string]providerChangeEvent
	parents       map[string]struct{}
	deliveryIDs   map[uint]struct{}
	eventCount    int
	deliveryMaxID uint
	fullFallback  bool
	fallbackCode  string
}

func newProviderChangeAccumulator() *providerChangeAccumulator {
	return &providerChangeAccumulator{events: make(map[string]providerChangeEvent), parents: make(map[string]struct{}), deliveryIDs: make(map[uint]struct{})}
}

func (a *providerChangeAccumulator) add(rows []models.ProviderEvent) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, row := range rows {
		a.eventCount++
		payload, ok := decodePersistedProviderEvent(row)
		if !ok {
			a.markFallbackLocked("invalid_event_payload")
			continue
		}
		if payload.Kind == cloudpkg.ChangeFallback {
			a.markFallbackLocked("cursor_gap")
			continue
		}
		if payload.Kind == cloudpkg.ChangeMoved {
			a.markFallbackLocked("move_scope_unknown")
			continue
		}
		if a.fullFallback {
			continue
		}
		event := providerChangeEvent{Kind: payload.Kind, ItemID: payload.ItemID, ParentID: payload.ParentID, PreviousParentID: payload.PreviousParentID, Name: payload.Name}
		a.events[event.ItemID] = event
		if event.ParentID != "" {
			a.parents[event.ParentID] = struct{}{}
		}
		if event.PreviousParentID != "" {
			a.parents[event.PreviousParentID] = struct{}{}
		}
		if len(a.events)+len(a.parents) > maxProviderChangeScopeItems {
			a.markFallbackLocked("scope_overflow")
		}
	}
}

func (a *providerChangeAccumulator) addDeliveries(rows []models.MediaLibraryProviderEvent, deliveryMaxID uint) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if deliveryMaxID > a.deliveryMaxID {
		a.deliveryMaxID = deliveryMaxID
	}
	for _, row := range rows {
		if row.ID > a.deliveryMaxID {
			a.deliveryMaxID = row.ID
		}
		if a.fullFallback {
			continue
		}
		if row.ID != 0 {
			if _, exists := a.deliveryIDs[row.ID]; exists {
				continue
			}
			a.deliveryIDs[row.ID] = struct{}{}
		}
		a.eventCount++
		payload, ok := decodeProviderEventPayload(row.PayloadJSON)
		if !ok {
			a.markFallbackLocked("invalid_event_payload")
			continue
		}
		if payload.Kind == cloudpkg.ChangeFallback {
			a.markFallbackLocked("cursor_gap")
			continue
		}
		if payload.Kind == cloudpkg.ChangeMoved {
			a.markFallbackLocked("move_scope_unknown")
			continue
		}
		event := providerChangeEvent{Kind: payload.Kind, ItemID: payload.ItemID, ParentID: payload.ParentID, PreviousParentID: payload.PreviousParentID, Name: payload.Name}
		a.events[event.ItemID] = event
		if event.ParentID != "" {
			a.parents[event.ParentID] = struct{}{}
		}
		if event.PreviousParentID != "" {
			a.parents[event.PreviousParentID] = struct{}{}
		}
		if len(a.events)+len(a.parents) > maxProviderChangeScopeItems {
			a.markFallbackLocked("scope_overflow")
		}
	}
}

func (a *providerChangeAccumulator) merge(scope providerChangeScope) {
	if a == nil || scope.empty() && scope.DeliveryMaxID == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if scope.DeliveryMaxID > a.deliveryMaxID {
		a.deliveryMaxID = scope.DeliveryMaxID
	}
	if len(scope.DeliveryIDs) == 0 {
		a.eventCount += scope.EventCount
	} else {
		for _, deliveryID := range scope.DeliveryIDs {
			if _, exists := a.deliveryIDs[deliveryID]; exists {
				continue
			}
			a.deliveryIDs[deliveryID] = struct{}{}
			a.eventCount++
		}
	}
	if scope.FullFallback {
		a.markFallbackLocked(scope.FallbackCode)
		return
	}
	if a.fullFallback {
		return
	}
	for _, event := range scope.Events {
		a.events[event.ItemID] = event
	}
	for _, parentID := range scope.ParentIDs {
		a.parents[parentID] = struct{}{}
	}
	if len(a.events)+len(a.parents) > maxProviderChangeScopeItems {
		a.markFallbackLocked("scope_overflow")
	}
}

func (a *providerChangeAccumulator) markFullFallback(code string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.eventCount++
	a.markFallbackLocked(code)
	a.mu.Unlock()
}

func (a *providerChangeAccumulator) markFallbackLocked(code string) {
	a.fullFallback = true
	if a.fallbackCode == "" {
		a.fallbackCode = code
	}
	clear(a.events)
	clear(a.parents)
}

func (a *providerChangeAccumulator) take() providerChangeScope {
	if a == nil {
		return providerChangeScope{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	scope := providerChangeScope{EventCount: a.eventCount, DeliveryMaxID: a.deliveryMaxID, FullFallback: a.fullFallback, FallbackCode: a.fallbackCode}
	for _, event := range a.events {
		scope.Events = append(scope.Events, event)
	}
	for parentID := range a.parents {
		scope.ParentIDs = append(scope.ParentIDs, parentID)
	}
	for deliveryID := range a.deliveryIDs {
		scope.DeliveryIDs = append(scope.DeliveryIDs, deliveryID)
	}
	sort.Slice(scope.Events, func(i, j int) bool { return scope.Events[i].ItemID < scope.Events[j].ItemID })
	sort.Strings(scope.ParentIDs)
	sort.Slice(scope.DeliveryIDs, func(i, j int) bool { return scope.DeliveryIDs[i] < scope.DeliveryIDs[j] })
	a.events = make(map[string]providerChangeEvent)
	a.parents = make(map[string]struct{})
	a.deliveryIDs = make(map[uint]struct{})
	a.eventCount, a.deliveryMaxID, a.fullFallback, a.fallbackCode = 0, 0, false, ""
	return scope
}

func decodePersistedProviderEvent(row models.ProviderEvent) (providerEventPayload, bool) {
	return decodeProviderEventPayload(row.PayloadJSON)
}

func decodeProviderEventPayload(value string) (providerEventPayload, bool) {
	var payload providerEventPayload
	if strings.TrimSpace(value) == "" || json.Unmarshal([]byte(value), &payload) != nil {
		return providerEventPayload{}, false
	}
	payload.Kind = strings.TrimSpace(payload.Kind)
	payload.ItemID = strings.TrimSpace(payload.ItemID)
	payload.ParentID = strings.TrimSpace(payload.ParentID)
	payload.PreviousParentID = strings.TrimSpace(payload.PreviousParentID)
	payload.Name = strings.TrimSpace(payload.Name)
	if payload.Kind == cloudpkg.ChangeFallback {
		return payload, true
	}
	if payload.ItemID == "" || len(payload.ItemID) > 128 || len(payload.ParentID) > 128 || len(payload.PreviousParentID) > 128 || len(payload.Name) > 512 ||
		strings.ContainsAny(payload.ItemID+payload.ParentID+payload.PreviousParentID+payload.Name, "\x00\r\n") {
		return providerEventPayload{}, false
	}
	switch payload.Kind {
	case cloudpkg.ChangeCreated, cloudpkg.ChangeRenamed, cloudpkg.ChangeDeleted, cloudpkg.ChangeMoved:
	default:
		return providerEventPayload{}, false
	}
	if payload.Kind == cloudpkg.ChangeDeleted && payload.ParentID == "" {
		return providerEventPayload{}, false
	}
	return payload, true
}

func (s *MediaLibraryService) knownPan115CatalogProviderIDs(ctx context.Context, libraryID uint) (map[string]struct{}, error) {
	identities := make(map[string]struct{})
	var entryIDs []string
	if err := s.db.WithContext(ctx).Model(&models.MediaLibraryEntry{}).Where("library_id = ?", libraryID).Pluck("provider_id", &entryIDs).Error; err != nil {
		return nil, err
	}
	var assetIDs []string
	if err := s.db.WithContext(ctx).Model(&models.MediaLibrarySourceAsset{}).Where("library_id = ?", libraryID).Pluck("provider_id", &assetIDs).Error; err != nil {
		return nil, err
	}
	for _, providerID := range append(entryIDs, assetIDs...) {
		if providerID = strings.TrimSpace(providerID); providerID != "" {
			identities[providerID] = struct{}{}
		}
	}
	return identities, nil
}

// mergeScopedPan115Catalog overlays a provider-verified delta on the last
// authoritative catalog. Provider I/O remains scoped, while recognition and
// artifact grouping still receive the complete current logical catalog.
func (s *MediaLibraryService) mergeScopedPan115Catalog(ctx context.Context, libraryID uint, delta medialibrary.Result) (medialibrary.Result, error) {
	if !delta.Scoped || libraryID == 0 {
		return delta, nil
	}
	var entries []models.MediaLibraryEntry
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).Order("relative_path").Find(&entries).Error; err != nil {
		return medialibrary.Result{}, err
	}
	var sourceAssets []models.MediaLibrarySourceAsset
	if err := s.db.WithContext(ctx).Where("library_id = ?", libraryID).Order("relative_path").Find(&sourceAssets).Error; err != nil {
		return medialibrary.Result{}, err
	}
	files := make(map[string]medialibrary.File, len(entries)+len(delta.Files))
	assets := make(map[string]medialibrary.SourceAsset, len(sourceAssets)+len(delta.Assets))
	for _, entry := range entries {
		if strings.TrimSpace(entry.ProviderID) == "" {
			return medialibrary.Result{}, errors.New("scoped catalog contains an entry without stable identity")
		}
		files[entry.ProviderID] = medialibrary.File{RelativePath: entry.RelativePath, ProviderID: entry.ProviderID, ProviderIDStable: true, Size: entry.Size, ModifiedAt: entry.ModifiedAt}
	}
	for _, asset := range sourceAssets {
		if strings.TrimSpace(asset.ProviderID) == "" {
			continue
		}
		assets[asset.ProviderID] = medialibrary.SourceAsset{RelativePath: asset.RelativePath, ProviderID: asset.ProviderID, ParentProviderID: asset.ParentProviderID, Name: asset.Name, Extension: asset.Extension, Size: asset.Size, ModifiedAt: asset.ModifiedAt, HashHint: asset.HashHint}
	}
	deleted := make(map[string]struct{}, len(delta.DeletedProviderIDs))
	for _, providerID := range delta.DeletedProviderIDs {
		deleted[providerID] = struct{}{}
		delete(files, providerID)
		delete(assets, providerID)
	}
	scopedProviders := make(map[string]struct{}, len(delta.Files)+len(delta.Assets))
	for _, file := range delta.Files {
		scopedProviders[file.ProviderID] = struct{}{}
		delete(assets, file.ProviderID)
		files[file.ProviderID] = file
	}
	for _, asset := range delta.Assets {
		scopedProviders[asset.ProviderID] = struct{}{}
		delete(files, asset.ProviderID)
		assets[asset.ProviderID] = asset
	}
	// Each listed parent is authoritative only for direct children. Anything
	// previously cataloged directly below it but absent from the completed
	// listing has been deleted, moved, or changed into a filtered file.
	authoritativeParents := make(map[string]struct{}, len(delta.AuthoritativeParentPaths))
	for _, parentPath := range delta.AuthoritativeParentPaths {
		authoritativeParents[path.Clean(parentPath)] = struct{}{}
	}
	for providerID, file := range files {
		if _, present := scopedProviders[providerID]; present {
			continue
		}
		if _, authoritative := authoritativeParents[path.Dir(path.Clean(file.RelativePath))]; authoritative {
			delete(files, providerID)
			deleted[providerID] = struct{}{}
		}
	}
	for providerID, asset := range assets {
		if _, present := scopedProviders[providerID]; present {
			continue
		}
		if _, authoritative := authoritativeParents[path.Dir(path.Clean(asset.RelativePath))]; authoritative {
			delete(assets, providerID)
			deleted[providerID] = struct{}{}
		}
	}
	// A complete scoped directory listing is authoritative for path conflicts
	// inside that scope. Remove the stale stable identity, never an unrelated
	// path, and carry the exact deletion into the publish transaction.
	type catalogIdentity struct{ providerID, relativePath string }
	identities := make([]catalogIdentity, 0, len(files)+len(assets))
	for providerID, file := range files {
		identities = append(identities, catalogIdentity{providerID: providerID, relativePath: file.RelativePath})
	}
	for providerID, asset := range assets {
		identities = append(identities, catalogIdentity{providerID: providerID, relativePath: asset.RelativePath})
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].relativePath == identities[j].relativePath {
			_, leftScoped := scopedProviders[identities[i].providerID]
			_, rightScoped := scopedProviders[identities[j].providerID]
			if leftScoped != rightScoped {
				return leftScoped
			}
			return identities[i].providerID < identities[j].providerID
		}
		return identities[i].relativePath < identities[j].relativePath
	})
	pathOwner := make(map[string]string, len(identities))
	for _, identity := range identities {
		if winner := pathOwner[identity.relativePath]; winner != "" {
			delete(files, identity.providerID)
			delete(assets, identity.providerID)
			deleted[identity.providerID] = struct{}{}
			continue
		}
		pathOwner[identity.relativePath] = identity.providerID
	}
	merged := medialibrary.Result{Files: make([]medialibrary.File, 0, len(files)), Assets: make([]medialibrary.SourceAsset, 0, len(assets)), Partial: true, Scoped: true, Enumerated: delta.Enumerated, Deduplicated: delta.Deduplicated, AuthoritativeParentPaths: append([]string(nil), delta.AuthoritativeParentPaths...)}
	for _, file := range files {
		merged.Files = append(merged.Files, file)
	}
	for _, asset := range assets {
		merged.Assets = append(merged.Assets, asset)
	}
	for providerID := range deleted {
		merged.DeletedProviderIDs = append(merged.DeletedProviderIDs, providerID)
	}
	sort.Slice(merged.Files, func(i, j int) bool { return merged.Files[i].RelativePath < merged.Files[j].RelativePath })
	sort.Slice(merged.Assets, func(i, j int) bool { return merged.Assets[i].RelativePath < merged.Assets[j].RelativePath })
	sort.Strings(merged.DeletedProviderIDs)
	return merged, nil
}
