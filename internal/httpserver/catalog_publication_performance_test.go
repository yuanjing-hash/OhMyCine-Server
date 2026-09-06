package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/services"
	cloudpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This uses the actual 115 scan/publication entry, authenticated HTTP history
// route over a real loopback socket, and actual queue policies/lease checks.
// Only provider enumeration is synthetic. Smoke mode is never SLA acceptance.
type catalogHTTPPerformanceDriver struct {
	routerCloudDriver
	size    int
	partial bool
	now     time.Time
}

func (d *catalogHTTPPerformanceDriver) ListTree(ctx context.Context, _ string, _ int) (cloudpkg.TreeResult, error) {
	size := d.size
	if d.partial {
		size = 1
	}
	result := cloudpkg.TreeResult{Partial: d.partial, Entries: make([]cloudpkg.TreeEntry, 0, size)}
	for i := 0; i < size; i++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		name := fmt.Sprintf("Show %04d - S02E%03d.mkv", i/100, i%100+1)
		item := cloudpkg.Item{ID: fmt.Sprint("file-", i), ParentID: "root-1", Name: name, Size: 100, ModifiedAt: d.now}
		if d.partial {
			item.Size = 101
		}
		result.Entries = append(result.Entries, cloudpkg.TreeEntry{Item: item, RelativePath: fmt.Sprintf("/Show %04d (2026)/Season 02/%s", i/100, name)})
	}
	return result, nil
}

type catalogHTTPQueryLogger struct {
	logger.Interface
	queries     atomic.Int64
	mu          sync.Mutex
	slowQueries map[string]time.Duration
}

func (l *catalogHTTPQueryLogger) Trace(_ context.Context, begin time.Time, sql func() (string, int64), _ error) {
	l.queries.Add(1)
	elapsed := time.Since(begin)
	if elapsed < 50*time.Millisecond {
		return
	}
	query, _ := sql()
	label := "other"
	for _, table := range []string{"catalog_identities", "catalog_entry_facts", "catalog_recognition_facts", "media_library_entries", "catalog_heads", "catalog_snapshots", "jobs", "player_playback_history"} {
		if strings.Contains(query, table) {
			label = table
			break
		}
	}
	if fields := strings.Fields(query); len(fields) > 0 {
		switch strings.ToLower(fields[0]) {
		case "insert", "update", "delete", "select":
			label = strings.ToLower(fields[0]) + ":" + label
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.slowQueries == nil {
		l.slowQueries = make(map[string]time.Duration)
	}
	l.slowQueries[label] = max(l.slowQueries[label], elapsed)
}

type catalogHTTPPerformancePool struct {
	gorm.ConnPool
	mu              sync.Mutex
	times           []time.Duration
	backgroundTimes []time.Duration
	switches        []time.Duration
	longestStage    string
	longestDuration time.Duration
}
type catalogHTTPPerformanceTx struct {
	gorm.ConnPool
	gorm.TxCommitter
	owner     *catalogHTTPPerformancePool
	start     time.Time
	switching bool
	stage     string
}

func (p *catalogHTTPPerformancePool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	start := time.Now()
	tx, err := p.ConnPool.(gorm.TxBeginner).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &catalogHTTPPerformanceTx{ConnPool: tx, TxCommitter: tx, owner: p, start: start}, nil
}
func (t *catalogHTTPPerformanceTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	t.observe(q)
	return t.ConnPool.ExecContext(ctx, q, args...)
}
func (t *catalogHTTPPerformanceTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	t.observe(q)
	return t.ConnPool.QueryContext(ctx, q, args...)
}
func (t *catalogHTTPPerformanceTx) observe(q string) {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(q)), "update") && strings.Contains(q, "catalog_heads") {
		t.switching = true
	}
	verb := strings.ToLower(strings.TrimSpace(q))
	if !strings.HasPrefix(verb, "insert") && !strings.HasPrefix(verb, "update") && !strings.HasPrefix(verb, "delete") {
		return
	}
	// Record static table labels only, never SQL, paths or bound values.
	for _, table := range []string{"media_library_scan_staging", "catalog_identities", "catalog_entry_facts", "catalog_recognition_facts", "catalog_source_asset_facts", "catalog_heads", "catalog_snapshots", "jobs", "player_playback_histories", "media_library_scan_runs"} {
		if strings.Contains(q, table) {
			if t.stage == "" || table != "catalog_snapshots" {
				t.stage = table
			}
			break
		}
	}
}
func (t *catalogHTTPPerformanceTx) finish() {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	d := time.Since(t.start)
	t.owner.times = append(t.owner.times, d)
	if !t.switching && (t.stage == "catalog_identities" || t.stage == "catalog_entry_facts" || t.stage == "catalog_recognition_facts" || t.stage == "catalog_source_asset_facts" || t.stage == "catalog_snapshots") {
		t.owner.backgroundTimes = append(t.owner.backgroundTimes, d)
	}
	if d > t.owner.longestDuration {
		t.owner.longestDuration, t.owner.longestStage = d, t.stage
	}
	if t.switching {
		t.owner.switches = append(t.owner.switches, d)
	}
}
func (t *catalogHTTPPerformanceTx) Commit() error {
	err := t.TxCommitter.Commit()
	t.finish()
	return err
}
func (t *catalogHTTPPerformanceTx) Rollback() error {
	err := t.TxCommitter.Rollback()
	t.finish()
	return err
}

type catalogHTTPLatency struct {
	attempts, failures int
	times              []time.Duration
}

func (s *catalogHTTPLatency) add(start time.Time, err error) {
	s.attempts++
	s.times = append(s.times, time.Since(start))
	if err != nil {
		s.failures++
	}
}
func durationPercentiles(values []time.Duration) (time.Duration, time.Duration, time.Duration) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	v := append([]time.Duration(nil), values...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v[(len(v)-1)/2], v[(len(v)*95+99)/100-1], v[len(v)-1]
}

type catalogHTTPPeakResources struct {
	heap, processMemory uint64
	db, wal, temporary  int64
}

type catalogHTTPPerformanceMeasurement struct {
	key               string
	elapsed           time.Duration
	queries           int64
	txP95, timeSwitch time.Duration
	peakHeap          uint64
}

func catalogHTTPFileSize(name string) int64 {
	info, err := os.Stat(name)
	if err != nil {
		return 0
	}
	return info.Size()
}

func (p *catalogHTTPPeakResources) sample(db, temporary string) {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	p.heap = max(p.heap, memory.HeapAlloc)
	p.processMemory = max(p.processMemory, memory.Sys)
	p.db = max(p.db, catalogHTTPFileSize(db))
	p.wal = max(p.wal, catalogHTTPFileSize(db+"-wal"))
	var tempBytes int64
	files, _ := os.ReadDir(temporary)
	for _, file := range files {
		if !file.IsDir() {
			tempBytes += catalogHTTPFileSize(filepath.Join(temporary, file.Name()))
		}
	}
	p.temporary = max(p.temporary, tempBytes)
}

func TestCatalogPublicationHTTPPerformance(t *testing.T) {
	if os.Getenv("OMC_RUN_PERFORMANCE") != "1" {
		t.Skip("opt-in 20-sample actual HTTP/production-lease performance gate")
	}
	previousLevel, previousMode := zerolog.GlobalLevel(), gin.Mode()
	zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	gin.SetMode(gin.ReleaseMode)
	defer func() { zerolog.SetGlobalLevel(previousLevel); gin.SetMode(previousMode) }()
	sizes, samples := []int{10_000, 100_000}, 20
	measurements := make(map[string][]catalogHTTPPerformanceMeasurement)
	record := func(m catalogHTTPPerformanceMeasurement) { measurements[m.key] = append(measurements[m.key], m) }
	smoke := os.Getenv("OMC_PERF_SMOKE") == "1"
	if smoke {
		sizes, samples = []int{1000}, 1
		t.Log("SMOKE ONLY: below acceptance size/sample count")
	}
	for _, size := range sizes {
		for sample := 0; sample < samples; sample++ {
			t.Run(fmt.Sprintf("size_%d/sample_%02d", size, sample), func(t *testing.T) {
				for _, scenario := range []string{"production", "stress_10s"} {
					t.Run(scenario, func(t *testing.T) { catalogPublicationHTTPSample(t, size, smoke, scenario, record) })
				}
			})
		}
	}
	keys := make([]string, 0, len(measurements))
	for key := range measurements {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rows := measurements[key]
		elapsed, switches := make([]time.Duration, 0, len(rows)), make([]time.Duration, 0, len(rows))
		var queryTotal int64
		var peakHeap uint64
		var maxWriterP95 time.Duration
		for _, row := range rows {
			elapsed = append(elapsed, row.elapsed)
			switches = append(switches, row.timeSwitch)
			queryTotal += row.queries
			peakHeap = max(peakHeap, row.peakHeap)
			maxWriterP95 = max(maxWriterP95, row.txP95)
		}
		p50, p95, longest := durationPercentiles(elapsed)
		_, switchP95, switchMax := durationPercentiles(switches)
		t.Logf("PERF_SUMMARY key=%s independent_samples=%d full_sample_count=%v elapsed_p50_ms=%d elapsed_p95_ms=%d elapsed_max_ms=%d mean_queries=%d peak_heap_bytes=%d worst_writer_p95_ms=%.3f switch_p95_ms=%.3f switch_max_ms=%.3f", key, len(rows), len(rows) >= 20 && !smoke, p50.Milliseconds(), p95.Milliseconds(), longest.Milliseconds(), queryTotal/int64(len(rows)), peakHeap, float64(maxWriterP95)/float64(time.Millisecond), float64(switchP95)/float64(time.Millisecond), float64(switchMax)/float64(time.Millisecond))
	}
}

func catalogPublicationHTTPSample(t *testing.T, size int, smoke bool, scenario string, record func(catalogHTTPPerformanceMeasurement)) {
	sqliteTemporary := t.TempDir()
	t.Setenv("SQLITE_TMPDIR", sqliteTemporary)
	driver := &catalogHTTPPerformanceDriver{size: size, now: time.Now().UTC()}
	client := newTestClient(t, driver)
	client.setup(t)
	status, envelope, _ := client.playerRequest(t, http.MethodPost, "/api/v1/player/auth/login", "", map[string]any{"username": "owner", "password": "strong-owner-password", "device_id": "catalog-performance", "device_name": "Performance fixture"})
	if status != 200 {
		t.Fatal(status, envelope.Message)
	}
	var login struct {
		Token string `json:"access_token"`
	}
	if err := json.Unmarshal(envelope.Data, &login); err != nil {
		t.Fatal(err)
	}
	var owner models.User
	if err := client.db.First(&owner, "username = ?", "owner").Error; err != nil {
		t.Fatal(err)
	}
	actor := services.Actor{User: owner, Permissions: map[string]struct{}{authz.PermissionConnectionsCreate: {}, authz.PermissionMediaLibrariesScan: {}}}
	connection, err := client.connections.Create(actor, services.ConnectionInput{Name: "Synthetic cloud", Provider: cloudpkg.ProviderPan115, Cookie: "UID=100_A1; CID=cid-value; SEID=seid-value; KID=kid-value", Enabled: true}, services.RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	storage := models.Storage{Name: "Performance", NameNormalized: "performance", Type: models.StorageTypePan115, RootPath: "root-1", RootPathNormalized: "performance-root", RootDisplayPath: "/媒体", ConnectionID: &connection.ID, Enabled: true, Capabilities: "{}"}
	if err := client.db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := client.db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{Name: "Performance", NameNormalized: "performance", StorageID: storage.ID, ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", ProviderRootID: "root-1", Enabled: true, Recursive: true, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`, STRMAssetExtraExtensionsJSON: `[]`, ProviderConcurrency: 8, ProviderRatePerSecond: 10, MetadataConcurrency: 8, MetadataRatePerSecond: 10}
	if err := client.db.Select("*").Create(&library).Error; err != nil {
		t.Fatal(err)
	}
	var location struct{ File string }
	if err := client.db.Raw("PRAGMA database_list").Scan(&location).Error; err != nil {
		t.Fatal(err)
	}
	reader, err := database.OpenReadOnly(location.File)
	if err != nil {
		t.Fatal(err)
	}
	readPool, _ := reader.DB()
	defer func() { _ = readPool.Close() }()
	store := services.NewCatalogSnapshotStore(client.db, reader)
	client.libraries.SetCatalogSnapshotStore(store)
	client.libraries.SetQueueService(client.queue)
	client.libraries.EnableCatalogScanFollowups()
	client.history.SetWriteAdmission(store.Admission())
	client.queue.SetWriteAdmission(store.Admission())
	client.changes.SetWriteAdmission(store.Admission())
	// Explicit isolated empty conversion only; this is not the production gate.
	if err := client.db.Transaction(func(tx *gorm.DB) error {
		return services.StartCatalogConversionTx(tx, models.CatalogHead{LibraryID: library.ID, SourceEpoch: 1, SourceFingerprint: "synthetic-source", ConfigFingerprint: "synthetic-config"}, func(*gorm.DB) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	candidate, token, err := store.BeginCandidate(context.Background(), services.CatalogCandidateInput{LibraryID: library.ID, Kind: "base", SourceEpoch: 1, SourceFingerprint: "synthetic-source", ConfigFingerprint: "synthetic-config", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Seal(context.Background(), candidate.ID, token); err != nil {
		t.Fatal(err)
	}
	if err := client.db.Transaction(func(tx *gorm.DB) error {
		_, err := store.PublishTx(tx, candidate.ID, token, 0, func(*gorm.DB) error { return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var claims []*services.ClaimedJob
	kinds := []string{services.JobTypeMediaArtifact, services.JobTypeMediaLibraryStructureDiagnosis}
	if scenario == "stress_10s" {
		kinds = []string{"fake"}
	}
	for _, kind := range kinds {
		_, err := client.queue.Enqueue(services.EnqueueJobInput{System: true, JobType: kind, Priority: 10, DisplayName: "Lease fixture", ResourceKey: "performance-lease-" + kind, Payload: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		claim, err := client.queue.Claim([]string{kind})
		if err != nil || claim == nil {
			t.Fatalf("claim %s %v", kind, err)
		}
		var policy models.QueuePolicy
		if err := client.db.First(&policy, "job_type = ?", kind).Error; err != nil {
			t.Fatal(err)
		}
		expected := map[string]int{services.JobTypeMediaArtifact: 30, services.JobTypeMediaLibraryStructureDiagnosis: 60, "fake": 10}[kind]
		if policy.LeaseSeconds != expected {
			t.Fatalf("real lease %s got %d want %d", kind, policy.LeaseSeconds, expected)
		}
		claims = append(claims, claim)
	}
	server := httptest.NewServer(client.router)
	defer server.Close()
	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second
	queryLog := &catalogHTTPQueryLogger{Interface: logger.Default.LogMode(logger.Silent)}
	client.db.Logger = queryLog
	reader.Logger = queryLog
	pool := &catalogHTTPPerformancePool{ConnPool: client.db.Statement.ConnPool}
	client.db.ConnPool = pool
	client.db.Statement.ConnPool = pool
	for _, phase := range []string{"first_full", "repeat_full", "partial"} {
		if phase == "partial" {
			driver.partial = true
		}
		pool.mu.Lock()
		pool.times = nil
		pool.backgroundTimes = nil
		pool.switches = nil
		pool.longestDuration, pool.longestStage = 0, ""
		pool.mu.Unlock()
		queries := queryLog.queries.Load()
		queryLog.mu.Lock()
		queryLog.slowQueries = nil
		queryLog.mu.Unlock()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		ctx, cancel := context.WithCancel(context.Background())
		var workers sync.WaitGroup
		peak := &catalogHTTPPeakResources{}
		peak.sample(location.File, sqliteTemporary)
		workers.Add(1)
		go func() {
			defer workers.Done()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					peak.sample(location.File, sqliteTemporary)
				case <-ctx.Done():
					peak.sample(location.File, sqliteTemporary)
					return
				}
			}
		}()
		historyStat := &catalogHTTPLatency{}
		leaseStats := make([]*catalogHTTPLatency, len(claims))
		readStat := &catalogHTTPLatency{}
		launch := func(stat *catalogHTTPLatency, action func() error) {
			workers.Add(1)
			go func() {
				defer workers.Done()
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				for {
					start := time.Now()
					err := action()
					stat.add(start, err)
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
					}
				}
			}()
		}
		launch(historyStat, func() error {
			body, err := json.Marshal(map[string]any{"cursor": 0, "changes": []services.PlayerHistoryChange{{SyncKey: strings.Repeat("a", 64), SourceKind: "local", SourceID: "fixture", MediaIdentity: "fixture", Title: "Synthetic", Position: 10, UpdatedAt: time.Now().UnixMilli()}}})
			if err != nil {
				return err
			}
			r, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+"/api/v1/player/history/sync", bytes.NewReader(body))
			if err != nil {
				return err
			}
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+login.Token)
			response, err := httpClient.Do(r)
			if err != nil {
				return err
			}
			defer func() { _ = response.Body.Close() }()
			data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			if err != nil {
				return err
			}
			if response.StatusCode != 200 {
				return fmt.Errorf("HTTP history status %d", response.StatusCode)
			}
			var result testEnvelope
			if err := json.Unmarshal(data, &result); err != nil {
				return err
			}
			if result.Code != 0 {
				return fmt.Errorf("history code %d", result.Code)
			}
			var synced services.PlayerHistorySyncResult
			if err := json.Unmarshal(result.Data, &synced); err != nil {
				return err
			}
			if len(synced.Rejected) != 0 {
				return fmt.Errorf("history rejected %d changes", len(synced.Rejected))
			}
			found := false
			for _, change := range synced.Changes {
				if change.SyncKey == strings.Repeat("a", 64) && !change.Deleted && change.Position == 10 {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("history HTTP success did not return the persisted fixture")
			}
			return nil
		})
		for i, claim := range claims {
			stat := &catalogHTTPLatency{}
			leaseStats[i] = stat
			launch(stat, func() error { return client.queue.Heartbeat(claim.Job.ID, claim.LeaseToken, nil, nil, nil, nil, nil) })
		}
		launch(readStat, func() error {
			return store.Read(context.Background(), []uint{library.ID}, func(r *services.CatalogReader) error {
				count, err := r.EntryCount()
				if err != nil {
					return err
				}
				if count != 0 && count != int64(size) {
					return fmt.Errorf("partial visible count %d", count)
				}
				return nil
			})
		})
		start := time.Now()
		published, scanErr := client.libraries.Scan(context.Background(), actor, library.ID, "full")
		elapsed := time.Since(start)
		cancel()
		workers.Wait()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		pool.mu.Lock()
		transactions := append([]time.Duration(nil), pool.times...)
		backgroundTransactions := append([]time.Duration(nil), pool.backgroundTimes...)
		switches := append([]time.Duration(nil), pool.switches...)
		longestStage := pool.longestStage
		pool.mu.Unlock()
		_, _, longest := durationPercentiles(transactions)
		_, writerP95, _ := durationPercentiles(backgroundTransactions)
		_, switchP95, switchMax := durationPercentiles(switches)
		fileSize := func(name string) int64 {
			info, err := os.Stat(name)
			if err != nil {
				return 0
			}
			return info.Size()
		}
		t.Logf("PERF phase=%s size=%d elapsed_ms=%d queries=%d allocated_bytes=%d heap_bytes=%d db_bytes=%d wal_bytes=%d longest_tx_ms=%.3f switch_p95_ms=%.3f switch_max_ms=%.3f switches=%d smoke=%v", phase, size, elapsed.Milliseconds(), queryLog.queries.Load()-queries, after.TotalAlloc-before.TotalAlloc, after.HeapAlloc, fileSize(location.File), fileSize(location.File+"-wal"), float64(longest)/float64(time.Millisecond), float64(switchP95)/float64(time.Millisecond), float64(switchMax)/float64(time.Millisecond), len(switches), smoke)
		t.Logf("PERF phase=%s longest_tx_stage=%s", phase, longestStage)
		t.Logf("PERF phase=%s writer_p95_ms=%.3f", phase, float64(writerP95)/float64(time.Millisecond))
		record(catalogHTTPPerformanceMeasurement{key: fmt.Sprintf("size_%d/%s/%s", size, scenario, phase), elapsed: elapsed, queries: queryLog.queries.Load() - queries, txP95: writerP95, timeSwitch: switchMax, peakHeap: peak.heap})
		t.Logf("PERF phase=%s peak_sample_interval_ms=100 peak_heap_bytes=%d peak_go_sys_bytes=%d peak_db_bytes=%d peak_wal_bytes=%d peak_sqlite_temp_file_bytes=%d", phase, peak.heap, peak.processMemory, peak.db, peak.wal, peak.temporary)
		queryLog.mu.Lock()
		for label, duration := range queryLog.slowQueries {
			t.Logf("PERF phase=%s slow_query=%s max_ms=%.3f", phase, label, float64(duration)/float64(time.Millisecond))
		}
		queryLog.mu.Unlock()
		operationStats := map[string]*catalogHTTPLatency{"HTTP_history": historyStat, "catalog_read": readStat}
		leaseNames := map[string]string{services.JobTypeMediaArtifact: "lease_30s", services.JobTypeMediaLibraryStructureDiagnosis: "lease_60s", "fake": "stress_10s"}
		for i, kind := range kinds {
			operationStats[leaseNames[kind]] = leaseStats[i]
		}
		for name, stat := range operationStats {
			p50, p95, max := durationPercentiles(stat.times)
			t.Logf("PERF phase=%s operation=%s samples=%d failures=%d p50_ms=%.3f p95_ms=%.3f max_ms=%.3f", phase, name, stat.attempts, stat.failures, float64(p50)/float64(time.Millisecond), float64(p95)/float64(time.Millisecond), float64(max)/float64(time.Millisecond))
			if stat.failures > 0 {
				t.Errorf("%s failures=%d", name, stat.failures)
			}
			if name != "catalog_read" && (p95 > time.Second || max > 2*time.Second) {
				t.Errorf("%s foreground latency exceeds gate", name)
			}
		}
		if scanErr != nil {
			t.Fatalf("scan phase=%s status=%s err=%v", phase, published.Status, scanErr)
		}
		if switchP95 > 100*time.Millisecond || switchMax > 250*time.Millisecond {
			t.Error("atomic switch latency exceeds gate")
		}
		if writerP95 > 100*time.Millisecond {
			t.Error("writer batch p95 exceeds the 100ms preparation target")
		}
		if phase == "first_full" && len(switches) == 0 {
			t.Error("switch instrumentation did not observe publication")
		}
		if t.Failed() {
			return
		}
	}
	// Ensure the temporary fixture, not a production runtime, was measured.
	if filepath.Base(location.File) != "server.db" {
		t.Fatal("unexpected isolated database location")
	}
}
