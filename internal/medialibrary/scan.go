package medialibrary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	storagefs "github.com/yuanjing-hash/OhMyCine-Server/internal/storage"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

const (
	MaxEntries           = 250000
	MaxDepth             = 64
	ProviderScanPageSize = 1000
	// ProviderProcessingWorkers is deliberately independent from provider HTTP
	// concurrency. These workers only normalize/filter in-memory file facts.
	ProviderProcessingWorkers = 128
)

type File struct {
	RelativePath string
	ProviderID   string
	// ProviderIDStable is true only when the provider guarantees identity
	// across rename/move. Local scan fingerprints intentionally set it false.
	ProviderIDStable bool
	Size             int64
	ModifiedAt       time.Time
	MediaType        string
	Title            string
	WorkKey          string
	SeriesTitle      string
	Season           *int
	Episode          *int
}
type SourceAsset struct {
	RelativePath     string
	ProviderID       string
	ParentProviderID string
	Name             string
	Extension        string
	Size             int64
	ModifiedAt       time.Time
	HashHint         string
}
type Result struct {
	Files        []File
	Assets       []SourceAsset
	Partial      bool
	Deduplicated int
	Enumerated   int
	// Scoped reconciliation facts are private to the Server event path. A
	// parent path is included only after its direct-child listing completed, so
	// it authorizes pruning missing direct children but never a subtree.
	Scoped                   bool
	DeletedProviderIDs       []string
	AuthoritativeParentPaths []string
}

type providerDirectory struct {
	ID           string
	RelativePath string
	Depth        int
}

// ScanProvider builds a provider-relative file tree from stable provider IDs.
// Providers with a recursive tree endpoint use it for full scans. Other
// providers retain the conservative sequential BFS fallback.
func ScanProvider(ctx context.Context, driver cloudpkg.Driver, rootID string, recursive bool, extensions, assetExtensions, ignores []string) (Result, error) {
	if driver == nil || strings.TrimSpace(rootID) == "" {
		return Result{}, errors.New("provider scanner is not configured")
	}
	extensionsSet := map[string]struct{}{}
	for _, ext := range extensions {
		extensionsSet[strings.ToLower(ext)] = struct{}{}
	}
	assetExtensionsSet := extensionSet(assetExtensions)
	if recursive {
		if stream, ok := driver.(cloudpkg.TreeStreamDriver); ok {
			return scanProviderTreeStream(ctx, stream, rootID, extensionsSet, assetExtensionsSet, ignores)
		}
		if bulk, ok := driver.(cloudpkg.BulkTreeDriver); ok {
			return scanProviderTree(ctx, bulk, rootID, extensionsSet, assetExtensionsSet, ignores)
		}
	}
	result := Result{Files: make([]File, 0)}
	queue := []providerDirectory{{ID: rootID}}
	visited := map[string]struct{}{rootID: {}}
	seenItems := 0
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		current := queue[0]
		queue = queue[1:]
		items, err := listProviderDirectory(ctx, driver, current.ID)
		if err != nil {
			return result, err
		}
		for _, item := range items {
			seenItems++
			if seenItems > MaxEntries {
				result.Partial = true
				return sortedResult(result), nil
			}
			relativePath := current.RelativePath + "/" + item.Name
			if shouldIgnore(relativePath, ignores) {
				continue
			}
			if item.IsDir {
				if !recursive || current.Depth+1 >= MaxDepth {
					continue
				}
				if _, exists := visited[item.ID]; exists {
					continue
				}
				visited[item.ID] = struct{}{}
				queue = append(queue, providerDirectory{ID: item.ID, RelativePath: relativePath, Depth: current.Depth + 1})
				continue
			}
			extension := strings.ToLower(filepath.Ext(item.Name))
			if _, ok := extensionsSet[extension]; ok {
				result.Files = append(result.Files, File{RelativePath: relativePath, ProviderID: item.ID, ProviderIDStable: true, Size: item.Size, ModifiedAt: item.ModifiedAt.UTC()})
			} else if _, ok := assetExtensionsSet[extension]; ok {
				result.Assets = append(result.Assets, SourceAsset{RelativePath: relativePath, ProviderID: item.ID, ParentProviderID: current.ID, Name: item.Name, Extension: extension, Size: item.Size, ModifiedAt: item.ModifiedAt.UTC(), HashHint: item.SHA1})
			}
		}
	}
	return sortedResult(result), nil
}

type providerTreeJob struct {
	sequence int
	entry    cloudpkg.TreeEntry
}

type providerTreeProcessed struct {
	sequence int
	file     *File
	asset    *SourceAsset
	failed   bool
}

// providerProcessingObserver is an internal test seam used to prove the
// fixed worker contract. Production callers never install one.
type providerProcessingObserver struct {
	workerStarted func()
	beforeProcess func()
	afterProcess  func()
}

type providerProcessingObserverContextKey struct{}

func withProviderProcessingObserver(ctx context.Context, observer providerProcessingObserver) context.Context {
	return context.WithValue(ctx, providerProcessingObserverContextKey{}, observer)
}

// scanProviderTreeStream uses exactly 128 bounded local workers and consumes
// their results in provider order so duplicate winners are deterministic.
func scanProviderTreeStream(ctx context.Context, driver cloudpkg.TreeStreamDriver, rootID string, extensionsSet, assetExtensionsSet map[string]struct{}, ignores []string) (Result, error) {
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	observer, _ := ctx.Value(providerProcessingObserverContextKey{}).(providerProcessingObserver)
	jobs := make(chan providerTreeJob, ProviderProcessingWorkers*2)
	processed := make(chan providerTreeProcessed, ProviderProcessingWorkers*2)
	var workers sync.WaitGroup
	workers.Add(ProviderProcessingWorkers)
	for worker := 0; worker < ProviderProcessingWorkers; worker++ {
		go func() {
			defer workers.Done()
			if observer.workerStarted != nil {
				observer.workerStarted()
			}
			for job := range jobs {
				item := safelyProcessProviderTreeEntry(job, extensionsSet, assetExtensionsSet, ignores, observer)
				select {
				case processed <- item:
				case <-workerCtx.Done():
					return
				}
			}
		}()
	}

	type streamOutcome struct {
		err     error
		partial bool
		entries int
	}
	streamDone := make(chan streamOutcome, 1)
	go func() {
		sequence := 0
		partial := false
		err := driver.StreamTree(workerCtx, rootID, MaxEntries, func(batch cloudpkg.TreeBatch) error {
			partial = partial || batch.Partial
			for _, entry := range batch.Entries {
				select {
				case jobs <- providerTreeJob{sequence: sequence, entry: entry}:
					sequence++
				case <-workerCtx.Done():
					return workerCtx.Err()
				}
			}
			return nil
		})
		close(jobs)
		workers.Wait()
		close(processed)
		streamDone <- streamOutcome{err: err, partial: partial, entries: sequence}
	}()

	result := Result{Files: make([]File, 0), Assets: make([]SourceAsset, 0)}
	pending := make(map[int]providerTreeProcessed, ProviderProcessingWorkers*2)
	seenPaths := make(map[string]struct{})
	seenProviders := make(map[string]struct{})
	next := 0
	processingPartial := false
	consume := func(item providerTreeProcessed) {
		if item.failed {
			processingPartial = true
			return
		}
		if item.file == nil && item.asset == nil {
			return
		}
		path, providerID := "", ""
		if item.file != nil {
			path, providerID = item.file.RelativePath, item.file.ProviderID
		} else {
			path, providerID = item.asset.RelativePath, item.asset.ProviderID
		}
		if _, exists := seenPaths[path]; exists {
			result.Deduplicated++
			return
		}
		if providerID != "" {
			if _, exists := seenProviders[providerID]; exists {
				result.Deduplicated++
				return
			}
			seenProviders[providerID] = struct{}{}
		}
		seenPaths[path] = struct{}{}
		if item.file != nil {
			result.Files = append(result.Files, *item.file)
		} else {
			result.Assets = append(result.Assets, *item.asset)
		}
	}
	for item := range processed {
		pending[item.sequence] = item
		for {
			ready, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			consume(ready)
			next++
		}
	}
	outcome := <-streamDone
	result.Partial = outcome.partial || processingPartial
	result.Enumerated = outcome.entries
	if outcome.err != nil {
		return result, outcome.err
	}
	if len(result.Files)+len(result.Assets) >= MaxEntries {
		result.Partial = true
	}
	return sortedResult(result), nil
}

// safelyProcessProviderTreeEntry converts an unexpected per-item panic into a
// partial snapshot. One malformed/provider-specific fact must never crash the
// Server or authorize destructive convergence for the rest of the library.
func safelyProcessProviderTreeEntry(job providerTreeJob, extensionsSet, assetExtensionsSet map[string]struct{}, ignores []string, observer providerProcessingObserver) (result providerTreeProcessed) {
	result.sequence = job.sequence
	defer func() {
		if recover() != nil {
			result = providerTreeProcessed{sequence: job.sequence, failed: true}
		}
	}()
	if observer.beforeProcess != nil {
		observer.beforeProcess()
	}
	result = processProviderTreeEntry(job.sequence, job.entry, extensionsSet, assetExtensionsSet, ignores)
	if observer.afterProcess != nil {
		observer.afterProcess()
	}
	return result
}

func processProviderTreeEntry(sequence int, entry cloudpkg.TreeEntry, extensionsSet, assetExtensionsSet map[string]struct{}, ignores []string) providerTreeProcessed {
	result := providerTreeProcessed{sequence: sequence}
	providerPath := strings.TrimSpace(entry.RelativePath)
	if entry.IsDir || shouldIgnore(providerPath, ignores) {
		return result
	}
	if !strings.HasPrefix(providerPath, "/") {
		result.failed = true
		return result
	}
	for _, segment := range strings.Split(strings.ReplaceAll(providerPath, "\\", "/"), "/") {
		if segment == ".." {
			result.failed = true
			return result
		}
	}
	clean := filepath.ToSlash(filepath.Clean(providerPath))
	if clean == "." || clean == "/" || !strings.HasPrefix(clean, "/") || strings.ContainsAny(clean, "\x00\r\n") {
		result.failed = true
		return result
	}
	extension := strings.ToLower(filepath.Ext(entry.Name))
	if _, ok := extensionsSet[extension]; ok {
		file := File{RelativePath: clean, ProviderID: entry.ID, ProviderIDStable: true, Size: entry.Size, ModifiedAt: entry.ModifiedAt.UTC()}
		result.file = &file
	} else if _, ok := assetExtensionsSet[extension]; ok {
		asset := SourceAsset{RelativePath: clean, ProviderID: entry.ID, ParentProviderID: entry.ParentID, Name: entry.Name, Extension: extension, Size: entry.Size, ModifiedAt: entry.ModifiedAt.UTC(), HashHint: entry.SHA1}
		result.asset = &asset
	}
	return result
}

// ProjectProviderEntries applies the same normalization/filtering rules as a
// full provider scan to a bounded set of already ancestry-verified entries.
// It performs no provider I/O and is used only by scoped reconciliation.
func ProjectProviderEntries(entries []cloudpkg.TreeEntry, extensions, assetExtensions, ignores []string) Result {
	extensionsSet := extensionSet(extensions)
	assetExtensionsSet := extensionSet(assetExtensions)
	result := Result{Files: make([]File, 0, len(entries)), Assets: make([]SourceAsset, 0, len(entries)), Enumerated: len(entries), Partial: true, Scoped: true}
	seenPaths := make(map[string]struct{}, len(entries))
	seenProviders := make(map[string]struct{}, len(entries))
	for sequence, entry := range entries {
		projected := processProviderTreeEntry(sequence, entry, extensionsSet, assetExtensionsSet, ignores)
		pathValue, providerID := "", ""
		if projected.file != nil {
			pathValue, providerID = projected.file.RelativePath, projected.file.ProviderID
		} else if projected.asset != nil {
			pathValue, providerID = projected.asset.RelativePath, projected.asset.ProviderID
		} else {
			continue
		}
		if _, duplicate := seenPaths[pathValue]; duplicate {
			result.Deduplicated++
			continue
		}
		if providerID != "" {
			if _, duplicate := seenProviders[providerID]; duplicate {
				result.Deduplicated++
				continue
			}
			seenProviders[providerID] = struct{}{}
		}
		seenPaths[pathValue] = struct{}{}
		if projected.file != nil {
			result.Files = append(result.Files, *projected.file)
		} else {
			result.Assets = append(result.Assets, *projected.asset)
		}
	}
	return sortedResult(result)
}

func scanProviderTree(ctx context.Context, driver cloudpkg.BulkTreeDriver, rootID string, extensionsSet, assetExtensionsSet map[string]struct{}, ignores []string) (Result, error) {
	tree, err := driver.ListTree(ctx, rootID, MaxEntries)
	if err != nil {
		return Result{}, err
	}
	result := Result{Files: make([]File, 0, min(len(tree.Entries), MaxEntries)), Assets: make([]SourceAsset, 0), Partial: tree.Partial, Enumerated: len(tree.Entries)}
	seenPaths := make(map[string]struct{}, len(tree.Entries))
	seenProviders := make(map[string]struct{}, len(tree.Entries))
	for sequence, entry := range tree.Entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		projected := processProviderTreeEntry(sequence, entry, extensionsSet, assetExtensionsSet, ignores)
		if projected.failed {
			result.Partial = true
			continue
		}
		if projected.file == nil && projected.asset == nil {
			continue
		}
		pathValue, providerID := "", ""
		if projected.file != nil {
			pathValue, providerID = projected.file.RelativePath, projected.file.ProviderID
		} else {
			pathValue, providerID = projected.asset.RelativePath, projected.asset.ProviderID
		}
		if _, duplicate := seenPaths[pathValue]; duplicate {
			result.Deduplicated++
			continue
		}
		if providerID != "" {
			if _, duplicate := seenProviders[providerID]; duplicate {
				result.Deduplicated++
				continue
			}
			seenProviders[providerID] = struct{}{}
		}
		seenPaths[pathValue] = struct{}{}
		if len(result.Files)+len(result.Assets) >= MaxEntries {
			result.Partial = true
			break
		}
		if projected.file != nil {
			result.Files = append(result.Files, *projected.file)
		} else {
			result.Assets = append(result.Assets, *projected.asset)
		}
	}
	return sortedResult(result), nil
}

func listProviderDirectory(ctx context.Context, driver cloudpkg.Driver, directoryID string) ([]cloudpkg.Item, error) {
	items := make([]cloudpkg.Item, 0)
	for offset := int64(0); ; {
		page, err := driver.List(ctx, directoryID, cloudpkg.PageRequest{Offset: offset, Limit: ProviderScanPageSize})
		if err != nil {
			return nil, err
		}
		if len(page.Items) == 0 && page.HasMore {
			return nil, errors.New("provider returned a non-progressing page")
		}
		items = append(items, page.Items...)
		if !page.HasMore {
			return items, nil
		}
		offset += int64(len(page.Items))
	}
}

func sortedResult(result Result) Result {
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].RelativePath < result.Files[j].RelativePath })
	sort.Slice(result.Assets, func(i, j int) bool { return result.Assets[i].RelativePath < result.Assets[j].RelativePath })
	return result
}

func ScanLocal(ctx context.Context, storageRoot, relativeRoot string, recursive bool, extensions, assetExtensions, ignores []string) (Result, error) {
	root, err := ResolveRoot(storageRoot, relativeRoot)
	if err != nil {
		return Result{}, err
	}
	extensionsSet := map[string]struct{}{}
	for _, ext := range extensions {
		extensionsSet[strings.ToLower(ext)] = struct{}{}
	}
	assetExtensionsSet := extensionSet(assetExtensions)
	result := Result{Files: make([]File, 0)}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		providerRel := "/" + filepath.ToSlash(rel)
		if shouldIgnore(providerRel, ignores) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		depth := strings.Count(filepath.ToSlash(rel), "/") + 1
		if depth > MaxDepth {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || isUnsafePath(path, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if !recursive && filepath.Dir(rel) == "." {
				return filepath.SkipDir
			}
			return nil
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		_, video := extensionsSet[extension]
		_, asset := assetExtensionsSet[extension]
		if !video && !asset {
			return nil
		}
		if len(result.Files)+len(result.Assets) >= MaxEntries {
			result.Partial = true
			return filepath.SkipAll
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		identity := sha256.Sum256([]byte(providerRel + "\x00" + info.ModTime().UTC().Format(time.RFC3339Nano) + "\x00" + strconv.FormatInt(info.Size(), 10)))
		providerID := hex.EncodeToString(identity[:16])
		if video {
			result.Files = append(result.Files, File{RelativePath: providerRel, ProviderID: providerID, Size: info.Size(), ModifiedAt: info.ModTime().UTC()})
		} else {
			result.Assets = append(result.Assets, SourceAsset{RelativePath: providerRel, ProviderID: providerID, Name: entry.Name(), Extension: extension, Size: info.Size(), ModifiedAt: info.ModTime().UTC(), HashHint: providerID})
		}
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return Result{}, err
	}
	return sortedResult(result), nil
}

func extensionSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if !strings.HasPrefix(value, ".") {
			value = "." + value
		}
		result[value] = struct{}{}
	}
	return result
}

// InspectLocalFile validates and parses one watcher event target without
// walking the whole library tree. The boolean is false for directories,
// ignored paths, and non-media extensions.
func InspectLocalFile(ctx context.Context, libraryRoot, path string, extensions, ignores []string) (File, bool, error) {
	if err := ctx.Err(); err != nil {
		return File{}, false, err
	}
	constrained, err := storagefs.Constrain(libraryRoot, path)
	if err != nil {
		return File{}, false, err
	}
	rel, err := filepath.Rel(libraryRoot, constrained)
	if err != nil || rel == "." {
		return File{}, false, err
	}
	current := libraryRoot
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return File{}, false, statErr
		}
		entry := dirEntryInfo{info}
		if IsUnsafeDirectory(current, entry) {
			return File{}, false, &storagefs.PathError{Code: storagefs.CodePathReparsePoint}
		}
	}
	info, err := os.Stat(constrained)
	if err != nil {
		return File{}, false, err
	}
	if info.IsDir() {
		return File{}, false, nil
	}
	providerRel := "/" + filepath.ToSlash(rel)
	if shouldIgnore(providerRel, ignores) {
		return File{}, false, nil
	}
	extensionsSet := map[string]struct{}{}
	for _, ext := range extensions {
		extensionsSet[strings.ToLower(ext)] = struct{}{}
	}
	if _, ok := extensionsSet[strings.ToLower(filepath.Ext(info.Name()))]; !ok {
		return File{}, false, nil
	}
	identity := sha256.Sum256([]byte(providerRel + "\x00" + info.ModTime().UTC().Format(time.RFC3339Nano) + "\x00" + strconv.FormatInt(info.Size(), 10)))
	return File{RelativePath: providerRel, ProviderID: hex.EncodeToString(identity[:16]), Size: info.Size(), ModifiedAt: info.ModTime().UTC()}, true, nil
}

// IsUnsafeDirectory exposes the same symlink/Reparse Point policy used by the
// scanner so event watchers cannot register a broader tree than scans accept.
func IsUnsafeDirectory(path string, entry os.DirEntry) bool {
	return entry.Type()&os.ModeSymlink != 0 || isUnsafePath(path, entry)
}

func ResolveRoot(storageRoot, relativeRoot string) (string, error) {
	rel := strings.Trim(strings.ReplaceAll(strings.TrimSpace(relativeRoot), "\\", "/"), "/")
	candidate := storageRoot
	if rel != "" {
		candidate = filepath.Join(storageRoot, filepath.FromSlash(rel))
	}
	resolved, err := storagefs.Constrain(storageRoot, candidate)
	if err != nil {
		return "", err
	}
	current := storageRoot
	if rel != "" {
		for _, part := range strings.Split(rel, "/") {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if err != nil {
				return "", err
			}
			if info.Mode()&os.ModeSymlink != 0 || isUnsafePath(current, dirEntryInfo{info}) {
				return "", &storagefs.PathError{Code: storagefs.CodePathReparsePoint}
			}
		}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", &storagefs.PathError{Code: storagefs.CodePathNotDirectory}
	}
	return resolved, nil
}

func NormalizeRelativeRoot(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" || value == "/" {
		return "/", nil
	}
	if strings.Contains(value, "\x00") || value == ".." || strings.HasPrefix(value, "../") || strings.Contains(value, "/../") || strings.HasSuffix(value, "/..") {
		return "", errors.New("invalid relative root")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash("/" + strings.TrimLeft(value, "/"))))
	if clean == "/.." || strings.HasPrefix(clean, "/../") {
		return "", errors.New("relative root escapes storage")
	}
	return clean, nil
}
func ParseFilename(name, path string) (string, string, *int, *int) {
	parsed := ParseMedia(name, path)
	return parsed.MediaType, parsed.Title, parsed.Season, parsed.Episode
}
func shouldIgnore(path string, patterns []string) bool {
	lower := strings.ToLower(path)
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern != "" && strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

type dirEntryInfo struct{ os.FileInfo }

func (d dirEntryInfo) Type() os.FileMode          { return d.Mode().Type() }
func (d dirEntryInfo) Info() (os.FileInfo, error) { return d.FileInfo, nil }
func (d dirEntryInfo) Name() string               { return d.FileInfo.Name() }
func (d dirEntryInfo) IsDir() bool                { return d.FileInfo.IsDir() }
