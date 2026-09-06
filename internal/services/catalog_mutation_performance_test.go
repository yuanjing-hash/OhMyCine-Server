package services

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Full AC1 mutation matrix: OMC_RUN_CATALOG_MUTATION_PERFORMANCE=1 go test
// ./internal/services -run '^TestCatalogMutationPerformance$' -v -timeout 12h
// Each of the 20 samples has a fresh database, empty metadata cache and a full
// pending catalog. Setup and verification are excluded, never disguised as a
// mutation. The separate small smoke test does NOT satisfy AC1.
func TestCatalogMutationPerformance(t *testing.T) {
	if os.Getenv("OMC_RUN_CATALOG_MUTATION_PERFORMANCE") != "1" {
		t.Skip("opt-in full 20-sample catalog mutation matrix")
	}
	for _, size := range []int{10_000, 100_000} {
		catalogMutationMatrix(t, size, 20)
	}
}

func TestCatalogMutationPerformanceSmoke(t *testing.T) {
	if os.Getenv("OMC_RUN_CATALOG_MUTATION_SMOKE") != "1" {
		t.Skip("opt-in instrumentation smoke; not AC1")
	}
	catalogMutationMatrix(t, 1040, 1)
}

func TestCatalogMutationProbeCleanupOnAbnormalExit(t *testing.T) {
	for _, abort := range []string{"panic", "goexit"} {
		t.Run(abort, func(t *testing.T) {
			store, _, _, _ := catalogFixture(t)
			originalPool, originalLogger := store.writeDB.Statement.ConnPool, store.writeDB.Logger
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { _ = recover() }()
				catalogMutationMeasure(t, store, "abnormal_cleanup", 0, 0, func() error {
					if abort == "panic" {
						panic("fixture abort")
					}
					runtime.Goexit()
					return nil
				})
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("measurement sampler did not terminate after abnormal exit")
			}
			if store.writeDB.Statement.ConnPool != originalPool || store.writeDB.Logger != originalLogger {
				t.Fatal("measurement did not restore database instrumentation")
			}
		})
	}
}

type catalogMutationMeasurement struct {
	Scenario           string `json:"scenario"`
	Files              int    `json:"files"`
	Sample             int    `json:"sample"`
	ElapsedUS          int64  `json:"elapsed_us"`
	Queries            int64  `json:"queries"`
	MutationRows       int64  `json:"mutation_rows"`
	Transactions       int    `json:"writer_transactions"`
	TxP50US            int64  `json:"writer_p50_us"`
	TxP95US            int64  `json:"writer_p95_us"`
	TxMaxUS            int64  `json:"writer_max_including_begin_wait_us"`
	BeginMaxUS         int64  `json:"begin_wait_max_us"`
	HeapBaseline       uint64 `json:"heap_baseline_bytes"`
	HeapPeak           uint64 `json:"observed_heap_peak_bytes"`
	AllocBytes         uint64 `json:"allocated_bytes"`
	DBPeak             int64  `json:"db_peak_bytes"`
	WALPeak            int64  `json:"wal_peak_bytes"`
	SHMPeak            int64  `json:"shm_peak_bytes"`
	VisibleTempPeak    int64  `json:"visible_temp_peak_bytes"`
	GCCollected        int    `json:"gc_collected_snapshots"`
	GCInitial          int64  `json:"gc_initial_unreferenced_snapshots"`
	GCBatches          int    `json:"gc_batches"`
	WriterP95TargetUS  int64  `json:"writer_p95_target_us"`
	WriterTargetPassed bool   `json:"writer_target_passed"`
}

type catalogMutationProbe struct {
	logger.Interface
	active       atomic.Bool
	queries      atomic.Int64
	rows         atomic.Int64
	sweeps       atomic.Int64
	mu           sync.Mutex
	transactions []int64
	beginWaits   []int64
}

func (p *catalogMutationProbe) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	if !p.active.Load() {
		return
	}
	p.queries.Add(1)
	query, rows := fc()
	query = strings.ToLower(strings.TrimSpace(query))
	if strings.HasPrefix(query, "update ") || strings.HasPrefix(query, "insert ") || strings.HasPrefix(query, "delete ") {
		if rows > 0 {
			p.rows.Add(rows)
		}
	}
	// SQL is inspected only in memory: no SQL, paths or metadata are logged.
	if strings.HasPrefix(query, "update ") && strings.Contains(query, "generation") &&
		(strings.Contains(query, "media_library_entries") || strings.Contains(query, "media_library_recognitions") || strings.Contains(query, "media_library_source_assets")) {
		p.sweeps.Add(1)
	}
}

type catalogMutationPool struct {
	gorm.ConnPool
	probe *catalogMutationProbe
}

func (p *catalogMutationPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	start, measured := time.Now(), p.probe.active.Load()
	tx, err := p.ConnPool.(gorm.TxBeginner).BeginTx(ctx, opts)
	wait := time.Since(start)
	if err != nil {
		if measured {
			p.probe.recordTx(start, wait)
		}
		return nil, err
	}
	return &catalogMutationTx{ConnPool: tx, TxCommitter: tx, probe: p.probe, start: start, wait: wait, measured: measured}, nil
}

type catalogMutationTx struct {
	gorm.ConnPool
	gorm.TxCommitter
	probe    *catalogMutationProbe
	start    time.Time
	wait     time.Duration
	measured bool
	once     sync.Once
}

func (tx *catalogMutationTx) finish() {
	if tx.measured {
		tx.once.Do(func() { tx.probe.recordTx(tx.start, tx.wait) })
	}
}
func (tx *catalogMutationTx) Commit() error { err := tx.TxCommitter.Commit(); tx.finish(); return err }
func (tx *catalogMutationTx) Rollback() error {
	err := tx.TxCommitter.Rollback()
	tx.finish()
	return err
}
func (p *catalogMutationProbe) recordTx(start time.Time, wait time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.transactions = append(p.transactions, time.Since(start).Microseconds())
	p.beginWaits = append(p.beginWaits, wait.Microseconds())
}
func catalogMutationPercentile(v []int64, percentile int) int64 {
	if len(v) == 0 {
		return 0
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[(len(v)*percentile+99)/100-1]
}

func catalogMutationMeasure(t *testing.T, store *CatalogSnapshotStore, scenario string, size, sample int, fn func() error) catalogMutationMeasurement {
	t.Helper()
	var location struct{ File string }
	if err := store.writeDB.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	p := &catalogMutationProbe{Interface: logger.Default.LogMode(logger.Silent)}
	writeLog, readLog := store.writeDB.Logger, store.readDB.Logger
	writePool, statementPool := store.writeDB.ConnPool, store.writeDB.Statement.ConnPool
	pool := &catalogMutationPool{ConnPool: statementPool, probe: p}
	store.writeDB.Logger, store.readDB.Logger = p, p
	store.writeDB.ConnPool, store.writeDB.Statement.ConnPool = pool, pool
	defer func() {
		store.writeDB.Logger, store.readDB.Logger = writeLog, readLog
		store.writeDB.ConnPool, store.writeDB.Statement.ConnPool = writePool, statementPool
	}()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	m := catalogMutationMeasurement{Scenario: scenario, Files: size, Sample: sample, HeapBaseline: before.HeapAlloc, HeapPeak: before.HeapAlloc}
	observe := func() {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		m.HeapPeak = max(m.HeapPeak, mem.HeapAlloc)
		for i, suffix := range []string{"", "-wal", "-shm"} {
			if stat, err := os.Stat(location.File + suffix); err == nil {
				switch i {
				case 0:
					m.DBPeak = max(m.DBPeak, stat.Size())
				case 1:
					m.WALPeak = max(m.WALPeak, stat.Size())
				case 2:
					m.SHMPeak = max(m.SHMPeak, stat.Size())
				}
			}
		}
		var visible int64
		_ = filepath.WalkDir(filepath.Dir(location.File), func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && path != location.File && path != location.File+"-wal" && path != location.File+"-shm" {
				if info, e := d.Info(); e == nil {
					visible += info.Size()
				}
			}
			return nil
		})
		m.VisibleTempPeak = max(m.VisibleTempPeak, visible)
	}
	observe()
	stop, done := make(chan struct{}), make(chan struct{})
	var stopOnce sync.Once
	stopSampler := func() {
		p.active.Store(false)
		stopOnce.Do(func() { close(stop) })
		<-done
	}
	// Also runs on panic/testing.Fatal (Goexit), before restoring DB probes.
	defer stopSampler()
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				observe()
			case <-stop:
				observe()
				return
			}
		}
	}()
	p.active.Store(true)
	start := time.Now()
	err := fn()
	m.ElapsedUS = time.Since(start).Microseconds()
	stopSampler()
	runtime.ReadMemStats(&after)
	m.AllocBytes = after.TotalAlloc - before.TotalAlloc
	m.Queries, m.MutationRows = p.queries.Load(), p.rows.Load()
	p.mu.Lock()
	m.Transactions = len(p.transactions)
	m.TxP50US = catalogMutationPercentile(p.transactions, 50)
	m.TxP95US = catalogMutationPercentile(p.transactions, 95)
	m.TxMaxUS = catalogMutationPercentile(p.transactions, 100)
	m.BeginMaxUS = catalogMutationPercentile(p.beginWaits, 100)
	p.mu.Unlock()
	m.WriterP95TargetUS = (100 * time.Millisecond).Microseconds()
	m.WriterTargetPassed = m.Transactions > 0 && m.TxP95US <= m.WriterP95TargetUS
	if !m.WriterTargetPassed {
		// Nonfatal preserves the remaining diagnostic scenarios, but the test
		// must FAIL: an over-target measurement is not an accepted performance gate.
		t.Errorf("%s writer target FAILED: transactions=%d p95=%dus target<=%dus (includes BeginTx wait)", scenario, m.Transactions, m.TxP95US, m.WriterP95TargetUS)
	}
	if err != nil {
		b, _ := json.Marshal(m)
		t.Log(string(b))
		t.Fatal(err)
	}
	if p.sweeps.Load() != 0 {
		t.Fatalf("%s performed %d legacy fact generation updates", scenario, p.sweeps.Load())
	}
	return m
}

// Fixture verification only: stream one ordered effective query so multilayer
// suffix resolution/sorting is not repeated once per 250 files. This read and
// its transaction are outside all measured production mutation windows.
// Semantic verification includes recognition metadata, not only file fields.
func catalogMutationDigest(t *testing.T, store *CatalogSnapshotStore, id uint, semantic bool) (string, int, map[string]int) {
	t.Helper()
	h := sha256.New()
	count := 0
	works := map[string]int{}
	err := store.Read(context.Background(), []uint{id}, func(r *CatalogReader) error {
		query := r.Entries().Order("id")
		rows, err := query.Rows()
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var row models.MediaLibraryEntry
			if err := query.ScanRows(rows, &row); err != nil {
				return err
			}
			season, episode := -1, -1
			if row.Season != nil {
				season = *row.Season
			}
			if row.Episode != nil {
				episode = *row.Episode
			}
			index, parseErr := strconv.Atoi(strings.TrimPrefix(row.ProviderID, "file-"))
			if parseErr != nil || season != (index%1000)/4/25+1 || episode != (index%1000)/4%25+1 {
				return fmt.Errorf("incorrect season/episode for synthetic file %d: S%dE%d", index, season, episode)
			}
			_, _ = fmt.Fprintf(h, "%d|%s|%s|%d|%d|%d\n", row.ID, row.ProviderID, row.RelativePath, row.Size, season, episode)
			if semantic {
				b, err := json.Marshal(row)
				if err != nil {
					return err
				}
				_, _ = h.Write(b)
			}
			works[row.WorkKey]++
			count++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if semantic {
			// The fixture has <=100 works, but page metadata as well so a future
			// larger fixture does not silently turn validation into a large read.
			var last uint
			for {
				var records []models.MediaLibraryRecognition
				if err := r.Recognitions().Where("id>?", last).Order("id").Limit(250).Find(&records).Error; err != nil {
					return err
				}
				for _, record := range records {
					b, err := json.Marshal(record)
					if err != nil {
						return err
					}
					_, _ = h.Write(b)
					last = record.ID
				}
				if len(records) < 250 {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), count, works
}

// This bounded CPU diagnostic is explicitly separate from AC acceptance. Use
// go test -cpuprofile <existing-directory>/cpu.pprof to retain the profile even
// when full 100k recognition would take too long to diagnose interactively.
func TestCatalogMutationCPUProfile(t *testing.T) {
	if os.Getenv("OMC_RUN_CATALOG_CPU_PROFILE") != "1" {
		t.Skip("opt-in incomplete CPU diagnostic, not acceptance")
	}
	catalogMutationMatrix(t, 100000, 1, 90*time.Second)
}

func catalogMutationMatrix(t *testing.T, size, samples int, profileLimits ...time.Duration) {
	t.Helper()
	profileLimit := time.Duration(0)
	if len(profileLimits) == 1 {
		profileLimit = profileLimits[0]
	}
	measurements := map[string][]catalogMutationMeasurement{}
	for sample := 1; sample <= samples; sample++ {
		if !t.Run(fmt.Sprintf("files_%d/sample_%02d", size, sample), func(t *testing.T) {
			s, library, storage, profile := catalogFollowupFixture(t)
			t.Cleanup(s.Close)
			ctx := context.Background()
			store := s.catalogStore
			var user models.User
			if err := s.db.First(&user).Error; err != nil {
				t.Fatal(err)
			}
			actor := Actor{User: user, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}, authz.PermissionMediaLibrariesScan: {}}}
			recognitionSnapshotClient(t, s, func(w http.ResponseWriter, r *http.Request) {
				id, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/tv/"))
				if err != nil || id < 10001 {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": fmt.Sprintf("Perf Series %06d", id-10000), "original_language": "en", "first_air_date": "2024-01-01", "genres": []any{map[string]any{"id": 18}}, "number_of_seasons": 10})
			})
			files := make([]medialibrary.File, 0, size)
			variants := []string{"1080p.h264.AAC", "2160p.h265.AAC", "1080p.h264.DTS", "2160p.h265.DTS"}
			for i := 0; i < size; i++ {
				work, ep := i/1000+1, (i%1000)/4
				path := fmt.Sprintf("Perf Series %06d (2024) {tmdb-%d}/Season %02d/Perf.Series.S%02dE%02d.%s.mkv", work, 10000+work, ep/25+1, ep/25+1, ep%25+1, variants[i%4])
				files = append(files, medialibrary.File{RelativePath: path, ProviderID: fmt.Sprintf("file-%06d", i), ProviderIDStable: true, Size: int64(1000 + i), ModifiedAt: time.Unix(1700000000, 0).UTC()})
			}
			library, run := catalogScanRun(t, s, library.ID, storage, profile)
			ready, err := s.publishCatalogScan(ctx, library, storage, profile, run, medialibrary.Result{Files: files}, true, s.catalogScanCommit)
			if err != nil {
				t.Fatal(err)
			}
			if ready.Status != "catalog_ready" || ready.RecognitionTotal != (size+999)/1000 {
				t.Fatalf("pending workload status=%s works=%d", ready.Status, ready.RecognitionTotal)
			}
			physical, count, _ := catalogMutationDigest(t, store, library.ID, false)
			if count != size {
				t.Fatalf("seed files=%d", count)
			}
			t.Logf("setup excluded: pending_files=%d pending_works=%d metadata_rate=%d metadata_concurrency=%d heap_sample_ms=10 visible_temp_scope=isolated_db_directory unlinked_sqlite_temp_not_observable=true", size, ready.RecognitionTotal, library.MetadataRatePerSecond, library.MetadataConcurrency)
			if err := s.RecoverCatalogScanFollowups(ctx, 10); err != nil {
				t.Fatal(err)
			}
			claim, err := s.queue.Claim([]string{JobTypeMediaLibraryRecognition})
			if err != nil || claim == nil {
				t.Fatalf("claim=%v error=%v", claim, err)
			}
			if claimedLeaseDuration(*claim) != 60*time.Second {
				t.Fatalf("not production lease: %v", claimedLeaseDuration(*claim))
			}
			record := func(m catalogMutationMeasurement) {
				b, _ := json.Marshal(m)
				t.Log(string(b))
				measurements[m.Scenario] = append(measurements[m.Scenario], m)
			}
			scenario := "recognition_full_completion"
			if profileLimit > 0 {
				scenario = "recognition_profile_incomplete"
			}
			record(catalogMutationMeasure(t, store, scenario, size, sample, func() error {
				workerCtx, cancel := context.WithCancel(ctx)
				if profileLimit > 0 {
					cancel()
					workerCtx, cancel = context.WithTimeout(ctx, profileLimit)
				}
				defer cancel()
				scheduler := NewScheduler(s.queue, NewWorkerRegistry(), zerolog.Nop())
				keepalive := scheduler.startLeaseKeepalive(workerCtx, cancel, *claim)
				result := NewMediaLibraryRecognitionWorker(s).Run(workerCtx, workerRuntime{queue: s.queue, job: *claim}, *claim)
				keepaliveErr := keepalive.Stop()
				if profileLimit > 0 && workerCtx.Err() == context.DeadlineExceeded {
					return nil
				}
				if err := keepaliveErr; err != nil {
					return err
				}
				if result.ErrorCode != "" || result.Wait != nil || result.RetryAt != nil {
					return fmt.Errorf("recognition incomplete: %+v", result)
				}
				return s.queue.Complete(claim.Job.ID, claim.LeaseToken)
			}))
			if profileLimit > 0 {
				t.Log("profile_only=true full_completion=false acceptance=false")
				return
			}
			if err := s.db.First(&run, ready.ID).Error; err != nil {
				t.Fatal(err)
			}
			if run.Status != "success" || run.Unrecognized != 0 || run.RecognitionFailed != 0 {
				t.Fatalf("recognition incomplete: status=%s unrecognized=%d failed=%d", run.Status, run.Unrecognized, run.RecognitionFailed)
			}
			after, n, works := catalogMutationDigest(t, store, library.ID, false)
			if after != physical || n != size || len(works) != (size+999)/1000 {
				t.Fatalf("recognition identity/count mismatch: files=%d works=%d", n, len(works))
			}
			for key, n := range works {
				if !strings.HasPrefix(key, "series:tmdb:") || n%4 != 0 {
					t.Fatalf("lost trusted identity or variants: %s %d", key, n)
				}
			}
			token := encodeCatalogToken("series:tmdb:10001")
			document, err := s.CatalogMetadata(ctx, actor, library.ID, token)
			if err != nil {
				t.Fatal(err)
			}
			document.Editable.Title = "Manual performance correction"
			factCounts := func() [3]int64 {
				var counts [3]int64
				for i, model := range []any{&models.CatalogEntryFact{}, &models.CatalogRecognitionFact{}, &models.CatalogSourceAssetFact{}} {
					if err := s.db.Model(model).Where("library_id=?", library.ID).Count(&counts[i]).Error; err != nil {
						t.Fatal(err)
					}
				}
				return counts
			}
			beforeFacts := factCounts()
			var unrelatedBefore []models.MediaLibraryRecognition
			if err := store.Read(ctx, []uint{library.ID}, func(r *CatalogReader) error {
				return r.Recognitions().Where("tmdb_id<>?", 10001).Order("id").Find(&unrelatedBefore).Error
			}); err != nil {
				t.Fatal(err)
			}
			record(catalogMutationMeasure(t, store, "manual_single_work", size, sample, func() error {
				saved, err := s.UpdateCatalogMetadata(ctx, actor, library.ID, token, MediaMetadataUpdateInput{Revision: document.Revision, Editable: document.Editable}, RequestContext{})
				if err == nil && (!saved.ManualOverride || saved.Editable.Title != document.Editable.Title) {
					return fmt.Errorf("manual result mismatch")
				}
				return err
			}))
			afterFacts := factCounts()
			if afterFacts[0] != beforeFacts[0] || afterFacts[2] != beforeFacts[2] || afterFacts[1] != beforeFacts[1]+1 {
				t.Fatalf("manual fact amplification: %v -> %v", beforeFacts, afterFacts)
			}
			if got, _, _ := catalogMutationDigest(t, store, library.ID, false); got != physical {
				t.Fatal("manual changed physical identities")
			}
			var unrelatedAfter []models.MediaLibraryRecognition
			if err := store.Read(ctx, []uint{library.ID}, func(r *CatalogReader) error {
				var bad int64
				if err := r.Entries().Where("work_key=? AND title<>?", "series:tmdb:10001", document.Editable.Title).Count(&bad).Error; err != nil {
					return err
				}
				if bad != 0 {
					return fmt.Errorf("manual result did not reach %d file variants", bad)
				}
				return r.Recognitions().Where("tmdb_id<>?", 10001).Order("id").Find(&unrelatedAfter).Error
			}); err != nil {
				t.Fatal(err)
			}
			oldJSON, _ := json.Marshal(unrelatedBefore)
			newJSON, _ := json.Marshal(unrelatedAfter)
			if string(oldJSON) != string(newJSON) {
				t.Fatal("manual edit changed unrelated works")
			}
			semantic, _, _ := catalogMutationDigest(t, store, library.ID, true)
			var beforeHead models.CatalogHead
			var beforeLibrary models.MediaLibrary
			if err := s.db.First(&beforeLibrary, library.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.db.First(&beforeHead, "library_id=?", library.ID).Error; err != nil {
				t.Fatal(err)
			}
			record(catalogMutationMeasure(t, store, "compaction", size, sample, func() error {
				ok, err := store.Compact(ctx, CatalogCompactionInput{LibraryID: library.ID, Force: true})
				if err == nil && !ok {
					return fmt.Errorf("compaction no-op")
				}
				return err
			}))
			var afterHead models.CatalogHead
			if err := s.db.First(&afterHead, "library_id=?", library.ID).Error; err != nil {
				t.Fatal(err)
			}
			var afterLibrary models.MediaLibrary
			if err := s.db.First(&afterLibrary, library.ID).Error; err != nil {
				t.Fatal(err)
			}
			if beforeLibrary.ContentRevision != afterLibrary.ContentRevision || beforeLibrary.DirtyGeneration != afterLibrary.DirtyGeneration || beforeLibrary.BaselineGeneration != afterLibrary.BaselineGeneration || beforeHead.Revision == afterHead.Revision {
				t.Fatal("compaction changed logical content or did no work")
			}
			if got, _, _ := catalogMutationDigest(t, store, library.ID, true); got != semantic {
				t.Fatal("compaction changed effective result")
			}
			var gc CatalogMaintenanceResult
			var initialGC, anchorsBefore int64
			if err := s.db.Model(&models.CatalogSnapshot{}).Where("state IN ('published','abandoned') AND NOT EXISTS(SELECT 1 FROM catalog_head_layers l WHERE l.snapshot_id=catalog_snapshots.id) AND NOT EXISTS(SELECT 1 FROM catalog_snapshot_references r WHERE r.snapshot_id=catalog_snapshots.id)").Count(&initialGC).Error; err != nil {
				t.Fatal(err)
			}
			if err := s.db.Model(&models.CatalogIdentity{}).Where("library_id=?", library.ID).Count(&anchorsBefore).Error; err != nil {
				t.Fatal(err)
			}
			m := catalogMutationMeasure(t, store, "gc_drain", size, sample, func() error {
				for {
					batch, err := store.Maintain(ctx, 16)
					if err != nil {
						return err
					}
					gc.Collected += batch.Collected
					gc.Batches += batch.Batches
					if batch.Batches == 0 {
						return nil
					}
				}
			})
			m.GCCollected, m.GCBatches = gc.Collected, gc.Batches
			m.GCInitial = initialGC
			record(m)
			if gc.Collected == 0 || gc.Batches == 0 || int64(gc.Collected) != initialGC {
				t.Fatalf("GC did not collect exact retired backlog: initial=%d collected=%d", initialGC, gc.Collected)
			}
			var anchorsAfter int64
			if err := s.db.Model(&models.CatalogIdentity{}).Where("library_id=?", library.ID).Count(&anchorsAfter).Error; err != nil {
				t.Fatal(err)
			}
			if anchorsBefore != anchorsAfter || anchorsAfter < int64(size) {
				t.Fatal("GC removed stable identity anchors")
			}
			if got, _, _ := catalogMutationDigest(t, store, library.ID, true); got != semantic {
				t.Fatal("GC changed effective result")
			}
		}) {
			break
		}
	}
	for _, scenario := range []string{"recognition_full_completion", "manual_single_work", "compaction", "gc_drain"} {
		rows := measurements[scenario]
		var times, queries []int64
		var total int64
		for _, m := range rows {
			times = append(times, m.ElapsedUS)
			queries = append(queries, m.Queries)
			total += m.ElapsedUS
		}
		t.Logf("aggregate files=%d scenario=%s samples=%d total_us=%d p50_us=%d p95_us=%d max_us=%d queries_p50=%d queries_p95=%d", size, scenario, len(rows), total, catalogMutationPercentile(times, 50), catalogMutationPercentile(times, 95), catalogMutationPercentile(times, 100), catalogMutationPercentile(queries, 50), catalogMutationPercentile(queries, 95))
	}
}
