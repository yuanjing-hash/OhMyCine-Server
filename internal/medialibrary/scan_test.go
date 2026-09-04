package medialibrary

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

type scanCloudDriver struct {
	children  map[string][]cloudpkg.Item
	calls     []cloudpkg.PageRequest
	statCalls int
}

func (d *scanCloudDriver) Provider() string { return cloudpkg.ProviderPan115 }
func (d *scanCloudDriver) Capabilities() cloudpkg.Capabilities {
	return cloudpkg.Capabilities{DirectoryList: true}
}

type bulkScanCloudDriver struct {
	*scanCloudDriver
	tree      cloudpkg.TreeResult
	treeCalls int
}

type streamScanCloudDriver struct {
	*scanCloudDriver
	batches [][]cloudpkg.TreeEntry
	partial bool
}

func (d *streamScanCloudDriver) StreamTree(_ context.Context, _ string, _ int, emit func(cloudpkg.TreeBatch) error) error {
	total := 0
	for _, batch := range d.batches {
		total += len(batch)
	}
	for index, batch := range d.batches {
		if err := emit(cloudpkg.TreeBatch{Offset: int64(index * 1000), Total: int64(total), Entries: batch, Partial: d.partial}); err != nil {
			return err
		}
	}
	return nil
}

func (d *bulkScanCloudDriver) ListTree(context.Context, string, int) (cloudpkg.TreeResult, error) {
	d.treeCalls++
	return d.tree, nil
}
func (d *scanCloudDriver) Probe(context.Context) (cloudpkg.Account, error) {
	return cloudpkg.Account{}, nil
}

func TestScanProviderUsesBulkTreeWithoutListOrStatCalls(t *testing.T) {
	driver := &bulkScanCloudDriver{
		scanCloudDriver: &scanCloudDriver{},
		tree: cloudpkg.TreeResult{Entries: []cloudpkg.TreeEntry{
			{Item: cloudpkg.Item{ID: "movie", Name: "Movie.2026.mkv", Size: 1}, RelativePath: "/电影/Movie.2026.mkv"},
			{Item: cloudpkg.Item{ID: "note", Name: "note.txt"}, RelativePath: "/电影/note.txt"},
		}},
	}
	result, err := ScanProvider(context.Background(), driver, "root", true, []string{".mkv"}, []string{"srt", "ssa", "ass", "jpg"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if driver.treeCalls != 1 || len(driver.calls) != 0 || driver.statCalls != 0 {
		t.Fatalf("tree=%d list=%d stat=%d", driver.treeCalls, len(driver.calls), driver.statCalls)
	}
	if len(result.Files) != 1 || result.Files[0].RelativePath != "/电影/Movie.2026.mkv" {
		t.Fatalf("result=%+v", result)
	}
}

func TestScanProviderBulkTreeMarksInvalidRelativePathPartial(t *testing.T) {
	driver := &bulkScanCloudDriver{
		scanCloudDriver: &scanCloudDriver{},
		tree: cloudpkg.TreeResult{Entries: []cloudpkg.TreeEntry{
			{Item: cloudpkg.Item{ID: "unsafe", Name: "unsafe.mkv"}, RelativePath: "/../unsafe.mkv"},
			{Item: cloudpkg.Item{ID: "safe", Name: "safe.mkv"}, RelativePath: "/safe.mkv"},
		}},
	}
	result, err := ScanProvider(context.Background(), driver, "root", true, []string{".mkv"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial || result.Enumerated != 2 || len(result.Files) != 1 || result.Files[0].ProviderID != "safe" {
		t.Fatalf("invalid bulk provider path did not preserve deletion safety: %+v", result)
	}
}

func TestScanProviderStreamUses128WorkersAndDeterministicallyDeduplicates(t *testing.T) {
	if ProviderProcessingWorkers != 128 {
		t.Fatalf("workers=%d, want 128", ProviderProcessingWorkers)
	}
	modified := time.Date(2026, 9, 4, 1, 2, 3, 0, time.UTC)
	entries := make([]cloudpkg.TreeEntry, 0, 12175)
	for index := 0; index < 12171; index++ {
		name := fmt.Sprintf("Movie.%05d.mkv", index)
		entries = append(entries, cloudpkg.TreeEntry{Item: cloudpkg.Item{ID: fmt.Sprintf("id-%d", index), Name: name, Size: int64(index + 1), ModifiedAt: modified}, RelativePath: "/电影/" + name})
	}
	entries = append(entries,
		entries[0],
		cloudpkg.TreeEntry{Item: cloudpkg.Item{ID: "id-0", Name: "SameID.mkv", ModifiedAt: modified}, RelativePath: "/电影/SameID.mkv"},
		cloudpkg.TreeEntry{Item: cloudpkg.Item{ID: "different-id", Name: "Movie.00000.mkv", ModifiedAt: modified}, RelativePath: "/电影/Movie.00000.mkv"},
		cloudpkg.TreeEntry{Item: cloudpkg.Item{ID: "escape", Name: "Escape.mkv", ModifiedAt: modified}, RelativePath: "/../../Escape.mkv"},
	)
	batches := make([][]cloudpkg.TreeEntry, 0, 13)
	for start := 0; start < len(entries); start += 1000 {
		end := min(start+1000, len(entries))
		batches = append(batches, entries[start:end])
	}
	driver := &streamScanCloudDriver{scanCloudDriver: &scanCloudDriver{}, batches: batches}
	var startedWorkers atomic.Int32
	var activeWorkers atomic.Int32
	var maxActiveWorkers atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	observer := providerProcessingObserver{
		workerStarted: func() { startedWorkers.Add(1) },
		beforeProcess: func() {
			active := activeWorkers.Add(1)
			for current := maxActiveWorkers.Load(); active > current && !maxActiveWorkers.CompareAndSwap(current, active); current = maxActiveWorkers.Load() {
			}
			if active >= 8 {
				releaseOnce.Do(func() { close(release) })
			}
			<-release
		},
		afterProcess: func() { activeWorkers.Add(-1) },
	}
	started := time.Now()
	ctx := withProviderProcessingObserver(context.Background(), observer)
	result, err := ScanProvider(ctx, driver, "root", true, []string{".mkv"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 12171 || result.Deduplicated != 3 || result.Enumerated != len(entries) {
		t.Fatalf("files=%d deduplicated=%d enumerated=%d", len(result.Files), result.Deduplicated, result.Enumerated)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("12171 local facts took %s", elapsed)
	}
	if startedWorkers.Load() != ProviderProcessingWorkers || maxActiveWorkers.Load() < 2 {
		t.Fatalf("started workers=%d max active=%d", startedWorkers.Load(), maxActiveWorkers.Load())
	}
}

func TestScanProviderStreamPropagatesPartialSnapshot(t *testing.T) {
	driver := &streamScanCloudDriver{scanCloudDriver: &scanCloudDriver{}, partial: true, batches: [][]cloudpkg.TreeEntry{{{
		Item: cloudpkg.Item{ID: "video-1", Name: "Movie.mkv"}, RelativePath: "/Movie.mkv",
	}}}}
	result, err := ScanProvider(context.Background(), driver, "root", true, []string{".mkv"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial || result.Enumerated != 1 || len(result.Files) != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestScanProviderStreamMarksInvalidRelativePathPartial(t *testing.T) {
	driver := &streamScanCloudDriver{scanCloudDriver: &scanCloudDriver{}, batches: [][]cloudpkg.TreeEntry{{
		{Item: cloudpkg.Item{ID: "unsafe", Name: "unsafe.mkv", Size: 1}, RelativePath: "../unsafe.mkv"},
		{Item: cloudpkg.Item{ID: "safe", Name: "safe.mkv", Size: 1}, RelativePath: "/safe.mkv"},
	}}}
	result, err := ScanProvider(context.Background(), driver, "root", true, []string{".mkv"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial || result.Enumerated != 2 || len(result.Files) != 1 || result.Files[0].ProviderID != "safe" {
		t.Fatalf("invalid provider path did not preserve deletion safety: %+v", result)
	}
}

func TestScanProviderStreamContainsPerItemPanicAsPartial(t *testing.T) {
	driver := &streamScanCloudDriver{scanCloudDriver: &scanCloudDriver{}, batches: [][]cloudpkg.TreeEntry{{{
		Item: cloudpkg.Item{ID: "video-1", Name: "Movie.mkv"}, RelativePath: "/Movie.mkv",
	}}}}
	var once sync.Once
	ctx := withProviderProcessingObserver(context.Background(), providerProcessingObserver{beforeProcess: func() {
		once.Do(func() { panic("synthetic item panic") })
	}})
	result, err := ScanProvider(ctx, driver, "root", true, []string{".mkv"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial || len(result.Files) != 0 {
		t.Fatalf("panic was not isolated as a partial fact: %+v", result)
	}
}
func (d *scanCloudDriver) Stat(context.Context, string) (cloudpkg.Item, error) {
	d.statCalls++
	return cloudpkg.Item{}, nil
}
func (d *scanCloudDriver) DirectURL(context.Context, cloudpkg.DirectURLRequest) (cloudpkg.TemporaryURL, error) {
	return cloudpkg.TemporaryURL{Headers: http.Header{}}, nil
}
func (d *scanCloudDriver) List(_ context.Context, parentID string, page cloudpkg.PageRequest) (cloudpkg.Page, error) {
	d.calls = append(d.calls, page)
	items := d.children[parentID]
	start := int(page.Offset)
	if start >= len(items) {
		return cloudpkg.Page{Offset: page.Offset}, nil
	}
	end := start + int(page.Limit)
	if end > len(items) {
		end = len(items)
	}
	return cloudpkg.Page{Items: items[start:end], Offset: page.Offset, HasMore: end < len(items)}, nil
}

func TestScanLocalFindsMixedMediaWithoutWriting(t *testing.T) {
	root := t.TempDir()
	files := []string{"Movie.2024.mp4", "Movie.2024.srt", "Movie.2024.jpg", filepath.Join("Show", "Season 01", "Show.S01E01.mkv"), "ignore.txt"}
	for _, name := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.Stat(filepath.Join(root, files[0]))
	result, err := ScanLocal(context.Background(), root, "/", true, []string{".mp4", ".mkv"}, []string{"srt", "ssa", "ass", "jpg"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 2 {
		t.Fatalf("files=%+v", result.Files)
	}
	if len(result.Assets) != 2 || result.Assets[0].Extension != ".jpg" || result.Assets[1].Extension != ".srt" {
		t.Fatalf("source assets=%+v", result.Assets)
	}
	if result.Files[0].MediaType != "" || result.Files[1].Title != "" {
		t.Fatalf("scanner leaked recognition projection=%+v", result.Files)
	}
	units := GroupRecognitionUnits(result.Files)
	if len(units) != 2 || units[1].MediaTypeHint != "tv" {
		t.Fatalf("recognition units=%+v", units)
	}
	after, _ := os.Stat(filepath.Join(root, files[0]))
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Fatal("scan modified source")
	}
}

func TestScanLocalIncludesOnlyConfiguredSourceAssetExtensions(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"Movie.mkv", "Movie.srt", "Movie.png", "Movie.xml", "Movie.nfo"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("asset"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result, err := ScanLocal(context.Background(), root, "/", true, []string{".mkv"}, []string{"srt", "png", "xml"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || len(result.Assets) != 3 {
		t.Fatalf("result=%+v", result)
	}
	if result.Assets[0].Extension != ".png" || result.Assets[1].Extension != ".srt" || result.Assets[2].Extension != ".xml" {
		t.Fatalf("assets=%+v", result.Assets)
	}
}

func TestNormalizeRelativeRootRejectsTraversal(t *testing.T) {
	if got, err := NormalizeRelativeRoot("shows/season 1"); err != nil || got != "/shows/season 1" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if _, err := NormalizeRelativeRoot("../../outside"); err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestScanProviderPaginatesRecursivelyAndKeepsStableIDs(t *testing.T) {
	modified := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	rootItems := make([]cloudpkg.Item, 0, ProviderScanPageSize+2)
	for index := 0; index < ProviderScanPageSize; index++ {
		rootItems = append(rootItems, cloudpkg.Item{ID: "sidecar-" + string(rune(index+1)), ParentID: "root", Name: "note.txt"})
	}
	rootItems = append(rootItems,
		cloudpkg.Item{ID: "movie-id", ParentID: "root", Name: "Movie.2026.mkv", Size: 42, ModifiedAt: modified},
		cloudpkg.Item{ID: "show-dir", ParentID: "root", Name: "Shows", IsDir: true},
	)
	driver := &scanCloudDriver{children: map[string][]cloudpkg.Item{
		"root":     rootItems,
		"show-dir": {{ID: "episode-id", ParentID: "show-dir", Name: "Series.S01E02.mp4", Size: 84, ModifiedAt: modified}},
	}}
	result, err := ScanProvider(context.Background(), driver, "root", true, []string{".mkv", ".mp4"}, []string{"srt", "ssa", "ass", "jpg"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Partial || len(result.Files) != 2 || result.Files[0].ProviderID != "movie-id" || result.Files[1].ProviderID != "episode-id" {
		t.Fatalf("result=%+v", result)
	}
	if result.Files[1].RelativePath != "/Shows/Series.S01E02.mp4" || !result.Files[1].ProviderIDStable || result.Files[1].MediaType != "" {
		t.Fatalf("episode=%+v", result.Files[1])
	}
	if len(driver.calls) != 3 || driver.calls[0].Offset != 0 || driver.calls[0].Limit != ProviderScanPageSize || driver.calls[1].Offset != ProviderScanPageSize {
		t.Fatalf("pagination calls=%+v", driver.calls)
	}
	if driver.statCalls != 0 {
		t.Fatalf("full scan made %d avoidable Stat calls", driver.statCalls)
	}
}

func TestScanLocalAlwaysIgnoresManagedRecycleDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".ohmycine-recycle", "draft", "incoming"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".ohmycine-recycle", "draft", "incoming", "copy.mkv"), []byte("recycled"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("active"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := ScanLocal(context.Background(), root, "/", true, []string{".mkv"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].RelativePath != "/movie.mkv" {
		t.Fatalf("recycle directory leaked into scan: %+v", result.Files)
	}
}
