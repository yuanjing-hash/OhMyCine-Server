package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	serverlog "github.com/yuanjing-hash/OhMyCine-Server/internal/logging"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// A large show's shared snapshot must not be decoded once for every episode
// while the sole SQLite writer blocks unrelated queue heartbeats.
func TestFastScanSharedSnapshotDoesNotStarveWriter(t *testing.T) {
	s, db, actor, storage, profile := mediaLibraryTestService(t)
	queue := NewQueueService(db, NewAuditService(db))
	s.SetQueueService(queue)
	created, err := s.Create(context.Background(), actor, testLibraryInput("shared snapshot", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	files := make([]medialibrary.File, 4100)
	for i := range files {
		files[i] = medialibrary.File{RelativePath: fmt.Sprintf("/Show/Season 01/Show.S01E%04d.mkv", i+1), ProviderID: fmt.Sprint(i + 1), ProviderIDStable: true, ModifiedAt: now, Size: 1}
	}
	units := medialibrary.GroupRecognitionUnits(files)
	if len(units) != 1 {
		t.Fatalf("fixture units=%d", len(units))
	}
	// Valid, large metadata comparable to the observed 299864-byte show record.
	raw := fmt.Sprintf(`{"version":1,"engine_version":%q,"snapshot":{"overview":%q}}`, mediarecognition.EngineVersion, strings.Repeat("x", 299000))
	record := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: units[0].SourceKey, InputFingerprint: units[0].InputFingerprint, ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, MediaType: "tv", Title: "Show", MetadataJSON: raw}
	if err := db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	run := models.MediaLibraryScanRun{LibraryID: library.ID, Kind: "full", Status: "running", Generation: 1, SourceFingerprint: mediaLibraryScanSourceFingerprint(library, storage, profile), CheckpointJSON: "{}", StartedAt: now}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.QueuePolicy{}).Where("job_type = ?", "fake").Update("lease_seconds", 5).Error; err != nil {
		t.Fatal(err)
	}
	enqueueFake(t, queue, actor, "Concurrent tracking heartbeat", "freshness-heartbeat")
	claim, err := queue.Claim([]string{"fake"})
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	writerEntered := make(chan struct{})
	var notified atomic.Bool
	const callback = "test:freshness_writer"
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "media_library_recognitions" {
			if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); ok && notified.CompareAndSwap(false, true) {
				close(writerEntered)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer db.Callback().Query().Remove(callback)
	heartbeat := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go func() {
		select {
		case <-writerEntered:
		case <-ctx.Done():
			heartbeat <- ctx.Err()
			return
		}
		heartbeat <- queue.Heartbeat(claim.Job.ID, claim.LeaseToken, nil, nil, nil, nil, nil)
	}()
	started := time.Now()
	published, err := s.publishFastPan115Scan(ctx, library, storage, profile, run, medialibrary.Result{Files: files, Enumerated: len(files)}, started, serverlog.OperationLibraryFullScan)
	if err != nil {
		t.Fatalf("publication: %v", err)
	}
	if published.CacheHits != 1 {
		t.Fatalf("reuse=%d", published.CacheHits)
	}
	if err := <-heartbeat; err != nil {
		t.Fatalf("foreground writer starved: %v", err)
	}
	var tracked models.Job
	if err := db.First(&tracked, "id = ?", claim.Job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if tracked.Status != models.JobStatusRunning || tracked.AttemptCount != 1 || tracked.HeartbeatAt == nil || !tracked.HeartbeatAt.After(*claim.Job.HeartbeatAt) {
		t.Fatalf("publication lost/reclaimed tracking lease: status=%s attempts=%d", tracked.Status, tracked.AttemptCount)
	}
	if err := queue.Complete(claim.Job.ID, claim.LeaseToken); err != nil {
		t.Fatalf("renewed tracking lease is unusable: %v", err)
	}
	t.Logf("4100 episodes,299KB shared metadata publication+writer=%s", time.Since(started))
}

func BenchmarkRecognitionFreshnessAmplification(b *testing.B) {
	raw := fmt.Sprintf(`{"version":1,"engine_version":%q,"snapshot":{"overview":%q}}`, mediarecognition.EngineVersion, strings.Repeat("x", 299000))
	record := models.MediaLibraryRecognition{MetadataJSON: raw}
	b.Run("old_per_file_4100", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			for i := 0; i < 4100; i++ {
				var old recognitionMetadataEnvelope
				if err := json.Unmarshal([]byte(raw), &old); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("once_per_work", func(b *testing.B) {
		for n := 0; n < b.N; n++ {
			fresh := mediaLibraryRecognitionProjectionFresh(record)
			for i := 0; i < 4100; i++ {
				if !fresh {
					b.Fatal("stale")
				}
			}
		}
	})
}
