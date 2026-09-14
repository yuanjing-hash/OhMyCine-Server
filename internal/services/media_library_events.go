package services

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
)

const maxProviderChangeScopeItems = 512

var errProviderChangeScopeUnproven = errors.New("provider event scope cannot be proven; full scan prohibited")

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
	LocalPaths     []string
	VerifiedResult *medialibrary.Result
	Events         []providerChangeEvent
	ParentIDs      []string
	DeliveryIDs    []uint
	EventCount     int
	DeliveryMaxID  uint
	Blocked        bool
	BlockCode      string
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
	return !s.Blocked && len(s.Events) == 0 && len(s.ParentIDs) == 0 && len(s.LocalPaths) == 0
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
	blocked       bool
	blockCode     string
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
			a.markBlockedLocked("invalid_event_payload")
			continue
		}
		if payload.Kind == cloudpkg.ChangeFallback {
			a.markBlockedLocked("cursor_gap")
			continue
		}
		if a.blocked {
			continue
		}
		event := providerChangeEvent(payload)
		a.events[event.ItemID] = event
		if event.ParentID != "" {
			a.parents[event.ParentID] = struct{}{}
		}
		if event.PreviousParentID != "" {
			a.parents[event.PreviousParentID] = struct{}{}
		}
		if len(a.events)+len(a.parents) > maxProviderChangeScopeItems {
			a.markBlockedLocked("scope_overflow")
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
		if a.blocked {
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
			a.markBlockedLocked("invalid_event_payload")
			continue
		}
		if payload.Kind == cloudpkg.ChangeFallback {
			a.markBlockedLocked("cursor_gap")
			continue
		}
		event := providerChangeEvent(payload)
		a.events[event.ItemID] = event
		if event.ParentID != "" {
			a.parents[event.ParentID] = struct{}{}
		}
		if event.PreviousParentID != "" {
			a.parents[event.PreviousParentID] = struct{}{}
		}
		if len(a.events)+len(a.parents) > maxProviderChangeScopeItems {
			a.markBlockedLocked("scope_overflow")
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
	if scope.Blocked {
		a.markBlockedLocked(scope.BlockCode)
		return
	}
	if a.blocked {
		return
	}
	for _, event := range scope.Events {
		a.events[event.ItemID] = event
	}
	for _, parentID := range scope.ParentIDs {
		a.parents[parentID] = struct{}{}
	}
	if len(a.events)+len(a.parents) > maxProviderChangeScopeItems {
		a.markBlockedLocked("scope_overflow")
	}
}

func (a *providerChangeAccumulator) markBlockedLocked(code string) {
	a.blocked = true
	if a.blockCode == "" {
		a.blockCode = code
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
	scope := providerChangeScope{EventCount: a.eventCount, DeliveryMaxID: a.deliveryMaxID, Blocked: a.blocked, BlockCode: a.blockCode}
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
	a.eventCount, a.deliveryMaxID, a.blocked, a.blockCode = 0, 0, false, ""
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

func (s *MediaLibraryService) knownPan115CatalogProviderIDs(ctx context.Context, libraryID uint, scope providerChangeScope) (map[string]struct{}, error) {
	identities := make(map[string]struct{})
	ids := make([]string, 0, len(scope.Events))
	for _, event := range scope.Events {
		ids = append(ids, event.ItemID)
	}
	if len(ids) == 0 {
		return identities, nil
	}
	var entryIDs []string
	var assetIDs []string
	if err := s.withCatalogRead(ctx, []uint{libraryID}, func(tx *gorm.DB, reader *CatalogReader) error {
		if err := reader.Entries().Where("provider_id IN ?", ids).Pluck("provider_id", &entryIDs).Error; err != nil {
			return err
		}
		return reader.SourceAssets().Where("provider_id IN ?", ids).Pluck("provider_id", &assetIDs).Error
	}); err != nil {
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
	return mergeScopedPan115CatalogFacts(delta, entries, sourceAssets)
}

func mergeScopedPan115CatalogFacts(delta medialibrary.Result, entries []models.MediaLibraryEntry, sourceAssets []models.MediaLibrarySourceAsset) (medialibrary.Result, error) {
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

// providerDeltaAlreadyApplied compares only identities named by this delivery.
// A late/repeated provider notification must not allocate another artifact batch.
func (s *MediaLibraryService) providerDeltaAlreadyApplied(ctx context.Context, libraryID uint, delta medialibrary.Result) (bool, error) {
	ids := make([]string, 0, len(delta.Files)+len(delta.Assets)+len(delta.DeletedProviderIDs))
	for _, f := range delta.Files {
		ids = append(ids, f.ProviderID)
	}
	for _, a := range delta.Assets {
		ids = append(ids, a.ProviderID)
	}
	ids = append(ids, delta.DeletedProviderIDs...)
	if len(ids) == 0 {
		return true, nil
	}
	entries := map[string]models.MediaLibraryEntry{}
	assets := map[string]models.MediaLibrarySourceAsset{}
	err := s.withCatalogRead(ctx, []uint{libraryID}, func(tx *gorm.DB, reader *CatalogReader) error {
		for start := 0; start < len(ids); start += 250 {
			end := start + 250
			if end > len(ids) {
				end = len(ids)
			}
			var es []models.MediaLibraryEntry
			if err := reader.Entries().Where("provider_id IN ?", ids[start:end]).Find(&es).Error; err != nil {
				return err
			}
			for _, e := range es {
				entries[e.ProviderID] = e
			}
			var as []models.MediaLibrarySourceAsset
			if err := reader.SourceAssets().Where("provider_id IN ?", ids[start:end]).Find(&as).Error; err != nil {
				return err
			}
			for _, a := range as {
				assets[a.ProviderID] = a
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	for _, f := range delta.Files {
		e, ok := entries[f.ProviderID]
		if !ok || e.RelativePath != f.RelativePath || e.Size != f.Size || !e.ModifiedAt.Equal(f.ModifiedAt) {
			return false, nil
		}
	}
	for _, a := range delta.Assets {
		existing, ok := assets[a.ProviderID]
		if !ok || existing.RelativePath != a.RelativePath || existing.Size != a.Size || !existing.ModifiedAt.Equal(a.ModifiedAt) || existing.HashHint != a.HashHint {
			return false, nil
		}
	}
	for _, id := range delta.DeletedProviderIDs {
		if _, ok := entries[id]; ok {
			return false, nil
		}
		if _, ok := assets[id]; ok {
			return false, nil
		}
	}
	return true, nil
}

// prepareProviderDeliveryPage isolates unprovable deliveries before publication.
// Successful identities form one verified delta; failed rows stay durable and
// rotate behind newer work, so a deleted directory cannot starve valid files.
func (s *MediaLibraryService) prepareProviderDeliveryPage(ctx context.Context, libraryID uint, scope providerChangeScope) (providerChangeScope, error) {
	if len(scope.DeliveryIDs) == 0 {
		return scope, nil
	}
	var library models.MediaLibrary
	if err := s.db.WithContext(ctx).First(&library, libraryID).Error; err != nil {
		return scope, err
	}
	var storage models.Storage
	if err := s.db.WithContext(ctx).First(&storage, library.StorageID).Error; err != nil {
		return scope, err
	}
	// Honor physical ownership before any provider calls.
	entered, err := catalogPhysicalWriteEntered(ctx, s.db, libraryID)
	if err != nil {
		return scope, err
	}
	if entered {
		return scope, errMediaLibraryEventReconcileDeferred
	}
	readiness, err := libraryReadiness(s.db.WithContext(ctx), libraryID)
	if err != nil {
		return scope, err
	}
	if readiness.ReadinessStatus == "repairing" || readiness.ReadinessStatus == "repair_failed" || readiness.ReadinessStatus == "credentials_required" || readiness.ReadinessStatus == "checking" {
		return scope, errMediaLibraryEventReconcileDeferred
	}
	backend, err := s.backends.Get(storage.Type)
	if err != nil {
		return scope, err
	}
	var extra, ignores []string
	_ = json.Unmarshal([]byte(library.STRMAssetExtraExtensionsJSON), &extra)
	_ = json.Unmarshal([]byte(library.IgnorePatternsJSON), &ignores)
	known, err := s.knownPan115CatalogProviderIDs(ctx, libraryID, scope)
	if err != nil {
		return scope, err
	}
	var rows []models.MediaLibraryProviderEvent
	if err := s.db.WithContext(ctx).Where("library_id = ? AND id IN ?", libraryID, scope.DeliveryIDs).Order("id").Find(&rows).Error; err != nil {
		return scope, err
	}
	result := medialibrary.Result{Partial: true, Scoped: true}
	files := map[string]medialibrary.File{}
	assets := map[string]medialibrary.SourceAsset{}
	deleted := map[string]struct{}{}
	prepared := providerChangeScope{}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return scope, err
		}
		payload, valid := decodeProviderEventPayload(row.PayloadJSON)
		if !valid || payload.Kind == cloudpkg.ChangeFallback {
			continue
		}
		if isPan115DirectoryTreeTombstone(storage.Type, payload, known, effectiveSourceAssetExtensions(extra)) {
			prepared.Events = append(prepared.Events, providerChangeEvent(payload))
			prepared.DeliveryIDs = append(prepared.DeliveryIDs, row.ID)
			if row.ID > prepared.DeliveryMaxID {
				prepared.DeliveryMaxID = row.ID
			}
			prepared.EventCount++
			continue
		}
		single := providerChangeScope{Events: []providerChangeEvent{providerChangeEvent(payload)}}
		delta, scanErr := backend.Scan(ctx, MediaLibraryScanRequest{Library: library, Storage: storage, VideoExtensions: defaultVideoExtensions, AssetExtensions: effectiveSourceAssetExtensions(extra), IgnorePatterns: ignores, providerScope: &single, knownProviderIDs: known})
		if scanErr == nil && result.Enumerated+delta.Enumerated > maxPan115ScopedEntries {
			scanErr = errProviderChangeScopeUnproven
		}
		if scanErr != nil {
			if err := s.db.WithContext(ctx).Model(&models.MediaLibraryProviderEvent{}).Where("id = ?", row.ID).Update("updated_at", time.Now().UTC()).Error; err != nil {
				return scope, err
			}
			s.log.Warn().Uint("library_id", libraryID).Uint("delivery_id", row.ID).Msg("单个事件范围无法确认，保留重试；继续处理其他文件")
			continue
		}
		prepared.Events = append(prepared.Events, single.Events...)
		prepared.DeliveryIDs = append(prepared.DeliveryIDs, row.ID)
		if row.ID > prepared.DeliveryMaxID {
			prepared.DeliveryMaxID = row.ID
		}
		prepared.EventCount++
		result.Enumerated += delta.Enumerated
		for _, f := range delta.Files {
			files[f.ProviderID] = f
			delete(deleted, f.ProviderID)
		}
		for _, a := range delta.Assets {
			assets[a.ProviderID] = a
			delete(deleted, a.ProviderID)
		}
		for _, id := range delta.DeletedProviderIDs {
			deleted[id] = struct{}{}
			delete(files, id)
			delete(assets, id)
		}
	}
	if len(prepared.DeliveryIDs) == 0 {
		return scope, errProviderChangeScopeUnproven
	}
	for _, f := range files {
		result.Files = append(result.Files, f)
	}
	for _, a := range assets {
		result.Assets = append(result.Assets, a)
	}
	for id := range deleted {
		result.DeletedProviderIDs = append(result.DeletedProviderIDs, id)
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].RelativePath < result.Files[j].RelativePath })
	sort.Slice(result.Assets, func(i, j int) bool { return result.Assets[i].RelativePath < result.Assets[j].RelativePath })
	sort.Strings(result.DeletedProviderIDs)
	prepared.VerifiedResult = &result
	return prepared, nil
}

// Ignore only an untracked 115 directory export tombstone. Tracked identities
// and explicitly configured text assets always retain ordinary deletion handling.
// This runs for durable deliveries too, so older queued notices are acknowledged.
func isPan115DirectoryTreeTombstone(storageType string, p providerEventPayload, known map[string]struct{}, assetExtensions []string) bool {
	if storageType != models.StorageTypePan115 || p.Kind != cloudpkg.ChangeDeleted || p.ParentID != "0" || p.PreviousParentID != "" {
		return false
	}
	if _, exists := known[p.ItemID]; exists {
		return false
	}
	for _, ext := range assetExtensions {
		if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(ext), "."), "txt") {
			return false
		}
	}
	if !strings.HasSuffix(p.Name, "_目录树.txt") {
		return false
	}
	prefix := strings.TrimSuffix(p.Name, "_目录树.txt")
	if !strings.HasPrefix(prefix, "共享") || len(prefix) != len("共享")+14 {
		return false
	}
	timestamp := prefix[len("共享"):]
	for _, r := range timestamp {
		if r < '0' || r > '9' {
			return false
		}
	}
	_, err := time.Parse("20060102150405", timestamp)
	return err == nil
}
