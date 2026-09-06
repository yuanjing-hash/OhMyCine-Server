package services

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

// A diagnostic of the actual atomic publisher with concurrent domain writes.
// Opt-in because 100k rows is unsuitable for every ordinary test invocation.
func TestPublicationContention(t *testing.T) {
	if os.Getenv("OMC_RUN_PERFORMANCE") != "1" {
		t.Skip("opt-in synthetic contention fixture")
	}
	for _, size := range []int{10_000, 100_000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s, db, actor, storage, profile := mediaLibraryTestService(t)
			created, err := s.Create(context.Background(), actor, testLibraryInput("Contention", storage, profile, false), RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			var library models.MediaLibrary
			if err := db.First(&library, created.ID).Error; err != nil {
				t.Fatal(err)
			}
			queue := NewQueueService(db, NewAuditService(db))
			if _, err := queue.Enqueue(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "fake", Priority: 10, DisplayName: "Heartbeat fixture", Payload: map[string]any{}}); err != nil {
				t.Fatal(err)
			}
			claim, err := queue.Claim([]string{"fake"})
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v error=%v", claim, err)
			}
			history := NewPlayerHistoryService(db)
			now := time.Now().UTC()
			files := make([]medialibrary.File, size)
			for i := range files {
				files[i] = medialibrary.File{RelativePath: fmt.Sprintf("/Movie.%06d.2026.mkv", i), ProviderID: fmt.Sprint(i), ProviderIDStable: true, Size: 100, ModifiedAt: now}
			}
			run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Phase: "enumerating", Generation: 1, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: now}
			if err := db.Create(&run).Error; err != nil {
				t.Fatal(err)
			}
			type stats struct {
				attempts, failures, partial int
				max                         time.Duration
				errors                      map[string]int
			}
			read, write, heartbeat := &stats{}, &stats{}, &stats{}
			stop := make(chan struct{})
			var workers sync.WaitGroup
			launch := func(stat *stats, fn func() error) {
				stat.errors = make(map[string]int)
				workers.Add(1)
				go func() {
					defer workers.Done()
					timer := time.NewTicker(100 * time.Millisecond)
					defer timer.Stop()
					for {
						select {
						case <-stop:
							return
						case <-timer.C:
							started := time.Now()
							err := fn()
							elapsed := time.Since(started)
							stat.attempts++
							if elapsed > stat.max {
								stat.max = elapsed
							}
							if err != nil {
								stat.failures++
								code := ErrorCode(err)
								if code == "" {
									_, code = mediaLibraryPersistenceDiagnostics(err)
								}
								stat.errors[code]++
							}
						}
					}
				}()
			}
			launch(read, func() error {
				var count int64
				err := db.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Count(&count).Error
				if err == nil && count != 0 && count != int64(size) {
					read.partial++
				}
				return err
			})
			launch(write, func() error {
				_, err := history.Sync(actor, 0, []PlayerHistoryChange{{SyncKey: fmt.Sprintf("%064x", size+1), SourceKind: "local", SourceID: "contention", MediaIdentity: "fixture", Title: "Fixture", Position: 10, UpdatedAt: time.Now().UnixMilli()}})
				return err
			})
			launch(heartbeat, func() error { return queue.Heartbeat(claim.Job.ID, claim.LeaseToken, nil, nil, nil, nil, nil) })
			published, err := s.publishFastPan115Scan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: files}, time.Now(), serverlog.OperationLibraryFullScan)
			close(stop)
			workers.Wait()
			if err != nil || published.Persisted != size {
				t.Fatalf("publication=%d error=%v", published.Persisted, err)
			}
			for name, stat := range map[string]*stats{"read": read, "history_sync": write, "heartbeat": heartbeat} {
				t.Logf("%s attempts=%d failures=%d partial=%d max_latency_ms=%d errors=%v", name, stat.attempts, stat.failures, stat.partial, stat.max.Milliseconds(), stat.errors)
			}
			if read.partial != 0 || read.failures != 0 {
				t.Fatal("atomic browsing regressed")
			}
			// Write failures are deliberately reported, not called a passing latency
			// guarantee. A new generation/publication architecture needs review.
			if write.failures+heartbeat.failures > 0 {
				t.Log("LIMITATION: SQLite single-writer contention remains; do not claim non-blocking publication")
			}
		})
	}
}
