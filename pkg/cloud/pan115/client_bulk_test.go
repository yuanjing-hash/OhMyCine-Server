package pan115

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	pan115sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"golang.org/x/time/rate"
)

func TestInteractiveReadDoesNotWaitBehindBackgroundReservation(t *testing.T) {
	sdk := &bulkSDK{}
	unlimited := rate.NewLimiter(rate.Inf, 1)
	client := &Client{
		sdk: sdk, listRate: unlimited, interactiveRate: rate.NewLimiter(rate.Inf, 1), pipelineRate: rate.NewLimiter(rate.Inf, 1),
		callSlots: make(chan struct{}, maxInFlightCalls), backgroundRead: make(chan struct{}, 1), now: time.Now, jitter: func() time.Duration { return 0 },
	}
	client.backgroundRead <- struct{}{}
	interactiveCtx, cancel := context.WithTimeout(cloud.WithReadClass(context.Background(), cloud.ReadClassInteractive), time.Second)
	defer cancel()
	if _, err := client.List(interactiveCtx, "0", cloud.PageRequest{Limit: 1}); err != nil {
		t.Fatalf("interactive read waited behind background reservation: %v", err)
	}
	backgroundCtx, cancelBackground := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelBackground()
	if _, err := client.List(backgroundCtx, "0", cloud.PageRequest{Limit: 1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("background read bypassed reservation: %v", err)
	}
	<-client.backgroundRead
}

type bulkSDK struct {
	mu             sync.Mutex
	getFileCalls   int
	listPageCalls  int
	fileCalls      int
	folderCalls    int
	files          []pan115sdk.File
	folders        []bulkFolder
	totalFiles     int
	fileDelay      time.Duration
	shortPageAt    *int64
	blockPageAt    *int64
	blockPage      <-chan struct{}
	requested      []int64
	activeFiles    int
	maxActiveFiles int
}

func (s *bulkSDK) CookieCheck() error                    { return nil }
func (s *bulkSDK) GetUser() (*pan115sdk.UserInfo, error) { return &pan115sdk.UserInfo{}, nil }
func (s *bulkSDK) GetInfo() (pan115sdk.InfoData, error)  { return pan115sdk.InfoData{}, nil }
func (s *bulkSDK) DownloadWithUA(string, string) (*pan115sdk.DownloadInfo, error) {
	return &pan115sdk.DownloadInfo{}, nil
}
func (s *bulkSDK) AddOfflineTaskURIs([]string, string, ...pan115sdk.OfflineOption) ([]string, error) {
	return nil, nil
}
func (s *bulkSDK) ListOfflineTask(int64) (pan115sdk.OfflineTaskResp, error) {
	return pan115sdk.OfflineTaskResp{}, nil
}
func (s *bulkSDK) DeleteOfflineTasks([]string, bool) error { return nil }
func (s *bulkSDK) ListPage(string, int64, int64, ...pan115sdk.ListOption) (*[]pan115sdk.File, error) {
	s.mu.Lock()
	s.listPageCalls++
	s.mu.Unlock()
	items := []pan115sdk.File{}
	return &items, nil
}
func (s *bulkSDK) GetFile(id string) (*pan115sdk.File, error) {
	s.mu.Lock()
	s.getFileCalls++
	s.mu.Unlock()
	return &pan115sdk.File{FileID: id, ParentID: "0", Name: "媒体", IsDirectory: true, PickCode: "folder-pickcode"}, nil
}
func (s *bulkSDK) ListTreeFiles(_ string, offset, limit int64) ([]pan115sdk.File, int64, error) {
	s.mu.Lock()
	s.fileCalls++
	s.requested = append(s.requested, offset)
	if s.totalFiles > 0 {
		s.activeFiles++
		if s.activeFiles > s.maxActiveFiles {
			s.maxActiveFiles = s.activeFiles
		}
		delay := s.fileDelay
		blockPageAt := s.blockPageAt
		blockPage := s.blockPage
		total := s.totalFiles
		s.mu.Unlock()
		if blockPageAt != nil && offset == *blockPageAt && blockPage != nil {
			<-blockPage
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		end := min(int(offset+limit), total)
		files := make([]pan115sdk.File, 0, max(0, end-int(offset)))
		for index := int(offset); index < end; index++ {
			files = append(files, pan115sdk.File{FileID: fmt.Sprintf("file-%d", index), ParentID: "root", Name: fmt.Sprintf("Movie.%05d.mkv", index)})
		}
		if s.shortPageAt != nil && offset == *s.shortPageAt && len(files) > 0 {
			files = files[:len(files)-1]
		}
		s.mu.Lock()
		s.activeFiles--
		s.mu.Unlock()
		return files, int64(total), nil
	}
	s.mu.Unlock()
	return append([]pan115sdk.File(nil), s.files...), int64(len(s.files)), nil
}
func (s *bulkSDK) ListTreeFolders(string, int64, int64) ([]bulkFolder, bool, error) {
	s.mu.Lock()
	s.folderCalls++
	s.mu.Unlock()
	return append([]bulkFolder(nil), s.folders...), false, nil
}
func (s *bulkSDK) ListLifeEvents(int) (lifeEventBatch, error) {
	return lifeEventBatch{}, nil
}

func TestListTreeUsesRecursiveBulkStreamsAndBuildsPaths(t *testing.T) {
	modified := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	sdk := &bulkSDK{
		folders: []bulkFolder{{ID: "movies", ParentID: "root", Name: "电影"}},
		files: []pan115sdk.File{
			{FileID: "one", ParentID: "movies", Name: "One.2026.mkv", Size: 10, UpdateTime: modified},
			{FileID: "two", ParentID: "root", Name: "Two.2026.mp4", Size: 20, UpdateTime: modified},
		},
	}
	client := &Client{
		sdk: sdk, listRate: rate.NewLimiter(rate.Inf, 1), bulkRate: rate.NewLimiter(rate.Inf, 1),
		callSlots: make(chan struct{}, maxInFlightCalls), now: time.Now, jitter: func() time.Duration { return 0 },
	}
	result, err := client.ListTree(context.Background(), "root", 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Partial || len(result.Entries) != 2 {
		t.Fatalf("result=%+v", result)
	}
	paths := map[string]string{}
	for _, entry := range result.Entries {
		paths[entry.ID] = entry.RelativePath
	}
	if paths["one"] != "/电影/One.2026.mkv" || paths["two"] != "/Two.2026.mp4" {
		t.Fatalf("paths=%+v", paths)
	}
	sdk.mu.Lock()
	defer sdk.mu.Unlock()
	if sdk.getFileCalls != 1 || sdk.fileCalls != 1 || sdk.folderCalls != 1 || sdk.listPageCalls != 0 {
		t.Fatalf("get=%d files=%d folders=%d list=%d", sdk.getFileCalls, sdk.fileCalls, sdk.folderCalls, sdk.listPageCalls)
	}
}

func TestListTreeCapsProviderResultsAsPartial(t *testing.T) {
	sdk := &bulkSDK{files: []pan115sdk.File{
		{FileID: "one", ParentID: "root", Name: "One.mkv"},
		{FileID: "two", ParentID: "root", Name: "Two.mkv"},
	}}
	client := &Client{
		sdk: sdk, listRate: rate.NewLimiter(rate.Inf, 1), bulkRate: rate.NewLimiter(rate.Inf, 1),
		callSlots: make(chan struct{}, maxInFlightCalls), now: time.Now, jitter: func() time.Duration { return 0 },
	}
	result, err := client.ListTree(context.Background(), "root", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial || len(result.Entries) != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestStreamTreeAppliesConfiguredProviderConcurrency(t *testing.T) {
	sdk := &bulkSDK{totalFiles: int(bulkTreePageSize)*6 + 7, fileDelay: 30 * time.Millisecond}
	client := &Client{
		sdk: sdk, listRate: rate.NewLimiter(rate.Inf, 1), bulkRate: rate.NewLimiter(rate.Inf, 1),
		callSlots: make(chan struct{}, maxInFlightCalls), backgroundRead: make(chan struct{}, maxBackgroundCalls), now: time.Now, jitter: func() time.Duration { return 0 },
	}
	ctx := cloud.WithTreeScanTuning(context.Background(), cloud.TreeScanTuning{RatePerSecond: 1000, Concurrency: 4})
	count := 0
	if err := client.StreamTree(ctx, "root", sdk.totalFiles, func(batch cloud.TreeBatch) error {
		count += len(batch.Entries)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != sdk.totalFiles {
		t.Fatalf("entries=%d want=%d", count, sdk.totalFiles)
	}
	sdk.mu.Lock()
	maxActive := sdk.maxActiveFiles
	sdk.mu.Unlock()
	if maxActive < 2 || maxActive > 4 {
		t.Fatalf("max provider concurrency=%d, want 2..4", maxActive)
	}
}

func TestStreamTreeRejectsIncompletePageBeforeSnapshotPublication(t *testing.T) {
	shortOffset := bulkTreePageSize
	sdk := &bulkSDK{totalFiles: int(bulkTreePageSize) + 7, shortPageAt: &shortOffset}
	client := &Client{
		sdk: sdk, listRate: rate.NewLimiter(rate.Inf, 1), bulkRate: rate.NewLimiter(rate.Inf, 1),
		callSlots: make(chan struct{}, maxInFlightCalls), backgroundRead: make(chan struct{}, maxBackgroundCalls), now: time.Now, jitter: func() time.Duration { return 0 },
	}
	err := client.StreamTree(cloud.WithTreeScanTuning(context.Background(), cloud.TreeScanTuning{RatePerSecond: 1000, Concurrency: 2}), "root", sdk.totalFiles, func(cloud.TreeBatch) error { return nil })
	if err == nil {
		t.Fatal("incomplete provider page was accepted as a complete snapshot")
	}
}

func TestStreamTreeCapsProviderConcurrencyAt32(t *testing.T) {
	sdk := &bulkSDK{totalFiles: int(bulkTreePageSize)*40 + 1, fileDelay: 40 * time.Millisecond}
	client := &Client{
		sdk: sdk, listRate: rate.NewLimiter(rate.Inf, 1), bulkRate: rate.NewLimiter(rate.Inf, 1),
		callSlots: make(chan struct{}, maxInFlightCalls), backgroundRead: make(chan struct{}, maxBackgroundCalls), now: time.Now, jitter: func() time.Duration { return 0 },
	}
	ctx := cloud.WithTreeScanTuning(context.Background(), cloud.TreeScanTuning{RatePerSecond: 1000, Concurrency: 128})
	if err := client.StreamTree(ctx, "root", sdk.totalFiles, func(cloud.TreeBatch) error { return nil }); err != nil {
		t.Fatal(err)
	}
	sdk.mu.Lock()
	maxActive := sdk.maxActiveFiles
	sdk.mu.Unlock()
	if maxActive != maxBackgroundCalls {
		t.Fatalf("max provider concurrency=%d, want capped %d", maxActive, maxBackgroundCalls)
	}
}

func TestStreamTreeBoundsReorderWindowWhenEarlyPageIsSlow(t *testing.T) {
	blockedOffset := bulkTreePageSize
	release := make(chan struct{})
	sdk := &bulkSDK{totalFiles: int(bulkTreePageSize) * 20, blockPageAt: &blockedOffset, blockPage: release}
	client := &Client{
		sdk: sdk, listRate: rate.NewLimiter(rate.Inf, 1), bulkRate: rate.NewLimiter(rate.Inf, 1),
		callSlots: make(chan struct{}, maxInFlightCalls), backgroundRead: make(chan struct{}, maxBackgroundCalls), now: time.Now, jitter: func() time.Duration { return 0 },
	}
	done := make(chan error, 1)
	go func() {
		done <- client.StreamTree(cloud.WithTreeScanTuning(context.Background(), cloud.TreeScanTuning{RatePerSecond: 1000, Concurrency: 4}), "root", sdk.totalFiles, func(cloud.TreeBatch) error { return nil })
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		sdk.mu.Lock()
		calls := len(sdk.requested)
		sdk.mu.Unlock()
		if calls >= 5 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("initial bounded page window did not start")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	sdk.mu.Lock()
	callsWhileBlocked := len(sdk.requested)
	sdk.mu.Unlock()
	if callsWhileBlocked != 5 { // synchronous first page plus four look-ahead pages
		close(release)
		t.Fatalf("reorder window fetched %d pages while the first look-ahead page was blocked, want 5", callsWhileBlocked)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStreamTreeMarksInvalidProviderRowsPartial(t *testing.T) {
	sdk := &bulkSDK{files: []pan115sdk.File{{FileID: "video-1", ParentID: "missing-parent", Name: "Movie.mkv"}}}
	client := &Client{
		sdk: sdk, listRate: rate.NewLimiter(rate.Inf, 1), bulkRate: rate.NewLimiter(rate.Inf, 1),
		callSlots: make(chan struct{}, maxInFlightCalls), backgroundRead: make(chan struct{}, maxBackgroundCalls), now: time.Now, jitter: func() time.Duration { return 0 },
	}
	partial := false
	if err := client.StreamTree(context.Background(), "root", 10, func(batch cloud.TreeBatch) error {
		partial = partial || batch.Partial
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !partial {
		t.Fatal("an unmappable provider row was accepted as complete deletion proof")
	}
}
