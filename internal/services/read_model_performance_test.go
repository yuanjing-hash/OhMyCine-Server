package services

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Opt-in, isolated SQLite fixtures, no network/provider calls or real media.
// go test ./internal/services -run '^TestReadModelPerformance$' -v -timeout 30m
// with OMC_RUN_PERFORMANCE=1. Five samples are diagnostic, not an SLA estimate.
type performanceLogger struct {
	logger.Interface
	queries atomic.Int64
}

func (l *performanceLogger) Trace(context.Context, time.Time, func() (string, int64), error) {
	l.queries.Add(1)
}

type performancePool struct {
	gorm.ConnPool
	longest atomic.Int64
}

func (p *performancePool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	start := time.Now()
	tx, err := p.ConnPool.(gorm.TxBeginner).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &performanceTx{ConnPool: tx, TxCommitter: tx, owner: p, start: start}, nil
}

type performanceTx struct {
	gorm.ConnPool
	gorm.TxCommitter
	owner *performancePool
	start time.Time
}

func (tx *performanceTx) Commit() error {
	err := tx.TxCommitter.Commit()
	elapsed := time.Since(tx.start).Nanoseconds()
	for prior := tx.owner.longest.Load(); elapsed > prior; prior = tx.owner.longest.Load() {
		if tx.owner.longest.CompareAndSwap(prior, elapsed) {
			break
		}
	}
	return err
}

func performanceSample(t *testing.T, label string, log *performanceLogger, pool *performancePool, fn func()) {
	t.Helper()
	var times, allocations, queries, transactions []int64
	for i := 0; i < 5; i++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		count := log.queries.Load()
		pool.longest.Store(0)
		start := time.Now()
		fn()
		times = append(times, time.Since(start).Microseconds())
		runtime.ReadMemStats(&after)
		allocations = append(allocations, int64(after.TotalAlloc-before.TotalAlloc))
		queries = append(queries, log.queries.Load()-count)
		transactions = append(transactions, pool.longest.Load()/int64(time.Microsecond))
	}
	for _, values := range [][]int64{times, allocations, queries, transactions} {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	}
	t.Logf("%s samples=5 p50_us=%d p95_us=%d alloc_p50_bytes=%d queries_p50=%d longest_commit_p95_us=%d", label, times[2], times[4], allocations[2], queries[2], transactions[4])
}

// The pre-remediation traversal, retained only for result/performance comparison.
func legacyContinueForPerformance(t *testing.T, s *PlayerHistoryService, actor Actor) []string {
	t.Helper()
	var keys []string
	for offset := 0; ; offset += playerHistorySyncLimit {
		var rows []models.PlayerPlaybackHistory
		if err := s.db.Where("user_id = ? AND deleted = ?", actor.User.ID, false).Order("client_updated_at DESC, sync_key ASC").Offset(offset).Limit(playerHistorySyncLimit).Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		available, err := s.browserHistoryAvailability(actor, rows)
		if err != nil {
			t.Fatal(err)
		}
		for i, row := range rows {
			change := playerHistoryChangeDTO(row)
			if available[i] && playerOverviewContinueEligible(change) {
				if _, ok := browserHistoryItem(change, s.libraries); ok {
					keys = append(keys, row.SyncKey)
				}
				if len(keys) == 13 {
					return keys
				}
			}
		}
		if len(rows) < playerHistorySyncLimit {
			return keys
		}
	}
}

func TestReadModelPerformance(t *testing.T) {
	if os.Getenv("OMC_RUN_PERFORMANCE") != "1" {
		t.Skip("opt-in synthetic performance fixture")
	}
	for _, size := range []int{10_000, 100_000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newPlayerHistoryCatalogFixture(t)
			db := f.libraries.db
			log := &performanceLogger{Interface: logger.Default.LogMode(logger.Silent)}
			pool := &performancePool{ConnPool: db.Statement.ConnPool}
			db.Logger = log
			db.ConnPool, db.Statement.ConnPool = pool, pool
			now := time.Now().UTC()
			rows := make([]models.PlayerPlaybackHistory, 0, size)
			entries := make([]models.MediaLibraryEntry, 0, size)
			favorites := make([]models.PlayerMediaFavorite, 0, size)
			files := make([]medialibrary.File, 0, size)
			for i := 0; i < size; i++ {
				work := fmt.Sprintf("movie:tmdb:%d", 10000+i)
				key := fmt.Sprintf("%064x", i+1)
				rows = append(rows, models.PlayerPlaybackHistory{UserID: f.actor.User.ID, SyncKey: key, SourceKind: "local", SourceID: "performance", MediaIdentity: key, Title: "Synthetic movie", Completed: i < size-13, Position: 100, Duration: floatPointer(1000), ClientUpdatedAt: now.Add(-time.Duration(i) * time.Second).UnixMilli(), CreatedAt: now, UpdatedAt: now})
				entry := f.movie[0]
				entry.ID, entry.WorkKey, entry.ProviderID, entry.RelativePath = 0, work, key, fmt.Sprintf("/Movie.%06d.2026.mkv", i)
				entries = append(entries, entry)
				favorites = append(favorites, models.PlayerMediaFavorite{UserID: f.actor.User.ID, LibraryID: f.libraryID, WorkKey: work, CreatedAt: now, UpdatedAt: now})
				files = append(files, medialibrary.File{RelativePath: entry.RelativePath, ProviderID: key, ProviderIDStable: true, Size: 100, ModifiedAt: now})
			}
			for _, values := range []any{&rows, &entries, &favorites} {
				if err := db.CreateInBatches(values, 100).Error; err != nil {
					t.Fatal(err)
				}
			}
			var expected []string
			performanceSample(t, "continue_before", log, pool, func() { expected = legacyContinueForPerformance(t, f.history, f.actor) })
			performanceSample(t, "continue_after", log, pool, func() {
				items, more, err := f.history.BrowserContinueWatching(f.actor, 12)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, item := range items {
					got = append(got, item.WorkID)
				}
				if !more || !reflect.DeepEqual(got, expected[:12]) {
					t.Fatalf("continue result differs: %v %v", got, more)
				}
			})
			state := NewPlayerMediaStateService(db, f.libraries)
			performanceSample(t, "favorites_page", log, pool, func() {
				page, err := state.FavoritePage(f.actor, 1, 24)
				if err != nil || page.Total != int64(size) || len(page.List) != 24 {
					t.Fatalf("favorites total=%d items=%d error=%v", page.Total, len(page.List), err)
				}
			})
			var library models.MediaLibrary
			var storage models.Storage
			var profile models.MediaClassificationProfile
			if err := db.First(&library, f.libraryID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&storage, library.StorageID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&profile, library.ProfileID).Error; err != nil {
				t.Fatal(err)
			}
			performanceSample(t, "atomic_publish", log, pool, func() {
				if err := db.First(&library, f.libraryID).Error; err != nil {
					t.Fatal(err)
				}
				run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "enumerating", Generation: library.DirtyGeneration + 1, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: now}
				if err := db.Create(&run).Error; err != nil {
					t.Fatal(err)
				}
				published, err := f.libraries.publishFastPan115Scan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: files}, time.Now(), serverlog.OperationLibraryFullScan)
				if err != nil || published.Persisted != size {
					t.Fatalf("publication persisted=%d error=%v", published.Persisted, err)
				}
				var count int64
				if err := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Count(&count).Error; err != nil || count != int64(size) {
					t.Fatalf("catalog count=%d error=%v", count, err)
				}
			})
		})
	}
}
