package services

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// MediaLibraryScanRequest contains only provider-neutral library scan facts.
// A backend owns the translation from these facts to its concrete storage API.
type MediaLibraryScanRequest struct {
	Library          models.MediaLibrary
	Storage          models.Storage
	VideoExtensions  []string
	AssetExtensions  []string
	IgnorePatterns   []string
	Progress         func(cloudpkg.TreeScanProgress)
	providerScope    *providerChangeScope
	knownProviderIDs map[string]struct{}
}

// MediaLibraryListener reports normalized reconciliation reasons. It never
// writes catalog state itself; MediaLibraryService remains the single owner of
// recognition, generations, artifacts, and downstream notifications.
type MediaLibraryListener interface {
	Run(context.Context, func(context.Context, string) error) error
	Close() error
}

// MediaLibraryBackend is the strict storage-facing contract used by the
// provider-neutral MediaLibrary lifecycle. Provider differences belong in an
// implementation or a narrow cloud capability, not in reconciliation.
type MediaLibraryBackend interface {
	StorageType() string
	Scan(context.Context, MediaLibraryScanRequest) (medialibrary.Result, error)
	OpenListener(context.Context, models.MediaLibrary, models.Storage, <-chan struct{}, *providerChangeAccumulator) (MediaLibraryListener, error)
}

type MediaLibraryBackendRegistry struct {
	mu       sync.RWMutex
	backends map[string]MediaLibraryBackend
}

func NewMediaLibraryBackendRegistry(backends ...MediaLibraryBackend) *MediaLibraryBackendRegistry {
	registry := &MediaLibraryBackendRegistry{backends: make(map[string]MediaLibraryBackend, len(backends))}
	for _, backend := range backends {
		registry.Register(backend)
	}
	return registry
}

func (r *MediaLibraryBackendRegistry) Register(backend MediaLibraryBackend) {
	if r == nil || backend == nil || backend.StorageType() == "" {
		return
	}
	r.mu.Lock()
	r.backends[backend.StorageType()] = backend
	r.mu.Unlock()
}

func (r *MediaLibraryBackendRegistry) Get(storageType string) (MediaLibraryBackend, error) {
	if r == nil {
		return nil, errors.New("media library backend registry is unavailable")
	}
	r.mu.RLock()
	backend := r.backends[storageType]
	r.mu.RUnlock()
	if backend == nil {
		return nil, errors.New("media library backend is unsupported")
	}
	return backend, nil
}

type localMediaLibraryBackend struct{}

func (localMediaLibraryBackend) StorageType() string { return models.StorageTypeLocal }

func (localMediaLibraryBackend) Scan(ctx context.Context, request MediaLibraryScanRequest) (medialibrary.Result, error) {
	return medialibrary.ScanLocal(ctx, request.Storage.RootPath, request.Library.RelativeRoot, request.Library.Recursive, request.VideoExtensions, request.AssetExtensions, request.IgnorePatterns)
}

func (localMediaLibraryBackend) OpenListener(_ context.Context, library models.MediaLibrary, storage models.Storage, _ <-chan struct{}, _ *providerChangeAccumulator) (MediaLibraryListener, error) {
	root, err := medialibrary.ResolveRoot(storage.RootPath, library.RelativeRoot)
	if err != nil {
		return nil, err
	}
	watcher, err := newRecursiveWatcher(root)
	if err != nil {
		return nil, err
	}
	return &localMediaLibraryListener{watcher: watcher, incremental: time.Duration(library.IncrementalMinutes) * time.Minute}, nil
}

type localMediaLibraryListener struct {
	watcher     *fsnotify.Watcher
	incremental time.Duration
}

func (l *localMediaLibraryListener) Close() error {
	if l == nil || l.watcher == nil {
		return nil
	}
	return l.watcher.Close()
}

func (l *localMediaLibraryListener) Run(ctx context.Context, reconcile func(context.Context, string) error) error {
	incremental := time.NewTicker(l.incremental)
	defer incremental.Stop()
	var debounce *time.Timer
	var debounceC <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-l.watcher.Events:
			if !ok {
				return nil
			}
			if event.Op&fsnotify.Create != 0 {
				if isDirectory, err := osStatDir(event.Name); err == nil && isDirectory {
					_ = addWatchTree(l.watcher, event.Name)
				}
			}
			if debounce == nil {
				debounce = time.NewTimer(600 * time.Millisecond)
			} else {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounce.Reset(600 * time.Millisecond)
			}
			debounceC = debounce.C
		case <-debounceC:
			_ = reconcile(ctx, "event")
			debounceC = nil
		case <-incremental.C:
			_ = reconcile(ctx, "incremental")
		case err, ok := <-l.watcher.Errors:
			if !ok {
				return nil
			}
			return err
		}
	}
}

type pan115MediaLibraryBackend struct {
	driver func(uint) (cloudpkg.Driver, error)
}

func (pan115MediaLibraryBackend) StorageType() string { return models.StorageTypePan115 }

func (b pan115MediaLibraryBackend) Scan(ctx context.Context, request MediaLibraryScanRequest) (medialibrary.Result, error) {
	ctx = cloudpkg.WithReadClass(ctx, cloudpkg.ReadClassBackground)
	ctx = cloudpkg.WithTreeScanTuning(ctx, cloudpkg.TreeScanTuning{
		RatePerSecond: request.Library.ProviderRatePerSecond,
		Concurrency:   request.Library.ProviderConcurrency,
		Progress:      request.Progress,
	})
	if request.Storage.ConnectionID == nil || b.driver == nil {
		return medialibrary.Result{}, errors.New("provider connection is unavailable")
	}
	driver, err := b.driver(*request.Storage.ConnectionID)
	if err != nil {
		return medialibrary.Result{}, err
	}
	providerRootID := request.Library.ProviderRootID
	if providerRootID == "" {
		providerRootID = request.Storage.RootPath
	}
	if request.providerScope != nil && !request.providerScope.FullFallback {
		result, fallback := scanPan115ProviderScope(ctx, driver, providerRootID, request)
		if !fallback {
			return result, nil
		}
		if request.Progress != nil {
			request.Progress(cloudpkg.TreeScanProgress{Phase: "scope_fallback"})
		}
	}
	return medialibrary.ScanProvider(ctx, driver, providerRootID, request.Library.Recursive, request.VideoExtensions, request.AssetExtensions, request.IgnorePatterns)
}

const maxPan115ScopedEntries = 5000

func scanPan115ProviderScope(ctx context.Context, driver cloudpkg.Driver, rootID string, request MediaLibraryScanRequest) (medialibrary.Result, bool) {
	scope := request.providerScope
	if scope == nil || scope.FullFallback || len(scope.Events) == 0 || strings.TrimSpace(rootID) == "" {
		return medialibrary.Result{}, true
	}
	root, err := driver.Stat(ctx, rootID)
	if err != nil || strings.TrimSpace(root.ID) != rootID || !root.IsDir {
		return medialibrary.Result{}, true
	}
	parentPaths := make(map[string]string, len(scope.ParentIDs))
	authoritativeParentPaths := make(map[string]struct{}, len(scope.ParentIDs))
	entriesByID := make(map[string]cloudpkg.TreeEntry)
	for _, parentID := range scope.ParentIDs {
		parent, err := driver.Stat(ctx, parentID)
		if err != nil || strings.TrimSpace(parent.ID) != parentID || !parent.IsDir {
			return medialibrary.Result{}, true
		}
		relative, inside, err := providerRelativePath(ctx, driver, parent, root.ID)
		if err != nil {
			return medialibrary.Result{}, true
		}
		if !inside {
			continue
		}
		parentPaths[parentID] = relative
		authoritativeParentPaths[relative] = struct{}{}
		children, complete := listPan115ScopedDirectory(ctx, driver, parentID)
		if !complete {
			return medialibrary.Result{}, true
		}
		for _, item := range children {
			if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.ParentID) != parentID {
				return medialibrary.Result{}, true
			}
			if !safeProviderPathSegment(item.Name) {
				return medialibrary.Result{}, true
			}
			if item.IsDir {
				continue
			}
			relativePath := path.Join(relative, item.Name)
			if !strings.HasPrefix(relativePath, "/") {
				relativePath = "/" + relativePath
			}
			entriesByID[item.ID] = cloudpkg.TreeEntry{Item: item, RelativePath: relativePath}
		}
	}

	deleted := make(map[string]struct{})
	for _, event := range scope.Events {
		if event.Kind == cloudpkg.ChangeMoved {
			return medialibrary.Result{}, true
		}
		item, statErr := driver.Stat(ctx, event.ItemID)
		if statErr != nil {
			code, _ := cloudpkg.ErrorInfo(statErr)
			if event.Kind != cloudpkg.ChangeDeleted || code != cloudpkg.CodeNotFound {
				return medialibrary.Result{}, true
			}
			if _, parentInside := parentPaths[event.ParentID]; !parentInside {
				return medialibrary.Result{}, true
			}
			// A missing provider identity may be a deleted directory. Exact
			// deletion is safe only when the current catalog proves that the
			// identity belonged to one file/source asset; otherwise descendants
			// could be left stale and a complete snapshot is required.
			if _, known := request.knownProviderIDs[event.ItemID]; !known {
				return medialibrary.Result{}, true
			}
			deleted[event.ItemID] = struct{}{}
			continue
		}
		if strings.TrimSpace(item.ID) != event.ItemID || item.IsDir {
			return medialibrary.Result{}, true
		}
		relative, inside, err := providerRelativePath(ctx, driver, item, root.ID)
		if err != nil {
			return medialibrary.Result{}, true
		}
		if !inside {
			deleted[event.ItemID] = struct{}{}
			continue
		}
		entriesByID[item.ID] = cloudpkg.TreeEntry{Item: item, RelativePath: relative}
	}

	entries := make([]cloudpkg.TreeEntry, 0, len(entriesByID))
	for _, entry := range entriesByID {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].RelativePath == entries[j].RelativePath {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].RelativePath < entries[j].RelativePath
	})
	result := medialibrary.ProjectProviderEntries(entries, request.VideoExtensions, request.AssetExtensions, request.IgnorePatterns)
	for relative := range authoritativeParentPaths {
		result.AuthoritativeParentPaths = append(result.AuthoritativeParentPaths, relative)
	}
	sort.Strings(result.AuthoritativeParentPaths)
	projected := make(map[string]struct{}, len(result.Files)+len(result.Assets))
	for _, file := range result.Files {
		projected[file.ProviderID] = struct{}{}
	}
	for _, asset := range result.Assets {
		projected[asset.ProviderID] = struct{}{}
	}
	// An affected file which is now ignored or has a non-media extension must
	// be removed from the catalog just as an explicit provider deletion is.
	for _, event := range scope.Events {
		if _, exists := entriesByID[event.ItemID]; exists {
			if _, kept := projected[event.ItemID]; !kept {
				deleted[event.ItemID] = struct{}{}
			}
		}
	}
	for providerID := range deleted {
		result.DeletedProviderIDs = append(result.DeletedProviderIDs, providerID)
	}
	sort.Strings(result.DeletedProviderIDs)
	result.Scoped, result.Partial = true, true
	return result, false
}

func listPan115ScopedDirectory(ctx context.Context, driver cloudpkg.Driver, parentID string) ([]cloudpkg.Item, bool) {
	items := make([]cloudpkg.Item, 0)
	for offset := int64(0); ; {
		page, err := driver.List(ctx, parentID, cloudpkg.PageRequest{Offset: offset, Limit: medialibrary.ProviderScanPageSize})
		if err != nil || page.Offset != offset || len(page.Items) == 0 && page.HasMore {
			return nil, false
		}
		items = append(items, page.Items...)
		if len(items) > maxPan115ScopedEntries {
			return nil, false
		}
		if !page.HasMore {
			return items, true
		}
		offset += int64(len(page.Items))
	}
}

func providerRelativePath(ctx context.Context, driver cloudpkg.Driver, item cloudpkg.Item, rootID string) (string, bool, error) {
	current := item
	if current.ID != rootID && !safeProviderPathSegment(current.Name) {
		return "", false, errors.New("provider scoped item name is invalid")
	}
	parts := []string{current.Name}
	visited := make(map[string]struct{}, maxCloudBoundaryDepth)
	for depth := 0; depth < maxCloudBoundaryDepth; depth++ {
		if current.ID == rootID {
			if len(parts) == 1 {
				return "/", true, nil
			}
			return "/" + path.Join(parts[1:]...), true, nil
		}
		if _, exists := visited[current.ID]; exists {
			return "", false, errors.New("provider scoped ancestry cycle")
		}
		visited[current.ID] = struct{}{}
		parentID := strings.TrimSpace(current.ParentID)
		if parentID == "" || (parentID == "0" && rootID != "0") {
			return "", false, nil
		}
		parent, err := driver.Stat(ctx, parentID)
		if err != nil {
			return "", false, err
		}
		if strings.TrimSpace(parent.ID) != parentID || !parent.IsDir {
			return "", false, errors.New("provider scoped ancestry is invalid")
		}
		if parent.ID != rootID && !safeProviderPathSegment(parent.Name) {
			return "", false, errors.New("provider scoped parent name is invalid")
		}
		parts = append([]string{parent.Name}, parts...)
		current = parent
	}
	return "", false, errors.New("provider scoped ancestry depth exceeded")
}

func safeProviderPathSegment(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00\r\n")
}

func (pan115MediaLibraryBackend) OpenListener(_ context.Context, library models.MediaLibrary, _ models.Storage, wake <-chan struct{}, pending *providerChangeAccumulator) (MediaLibraryListener, error) {
	return &providerMediaLibraryListener{wake: wake, pending: pending, incremental: time.Duration(library.IncrementalMinutes) * time.Minute}, nil
}

type providerMediaLibraryListener struct {
	wake        <-chan struct{}
	pending     *providerChangeAccumulator
	incremental time.Duration
	debounce    time.Duration
}

func (*providerMediaLibraryListener) Close() error { return nil }

func (l *providerMediaLibraryListener) Run(ctx context.Context, reconcile func(context.Context, string) error) error {
	incremental := time.NewTicker(l.incremental)
	defer incremental.Stop()
	var debounce *time.Timer
	var debounceC <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-l.wake:
			if !ok {
				return nil
			}
			debounceDelay := l.debounce
			if debounceDelay <= 0 {
				debounceDelay = 2 * time.Second
			}
			if debounce == nil {
				debounce = time.NewTimer(debounceDelay)
			} else {
				if !debounce.Stop() {
					select {
					case <-debounce.C:
					default:
					}
				}
				debounce.Reset(debounceDelay)
			}
			debounceC = debounce.C
		case <-debounceC:
			scope := l.pending.take()
			if !scope.empty() {
				if err := reconcile(withProviderChangeScope(ctx, scope), "event"); err != nil {
					l.pending.merge(scope)
					debounceDelay := l.debounce
					if debounceDelay <= 0 {
						debounceDelay = 2 * time.Second
					}
					if debounce == nil {
						debounce = time.NewTimer(debounceDelay)
					} else {
						debounce.Reset(debounceDelay)
					}
					debounceC = debounce.C
					continue
				}
			}
			debounceC = nil
		case <-incremental.C:
			_ = reconcile(ctx, "incremental")
		}
	}
}
