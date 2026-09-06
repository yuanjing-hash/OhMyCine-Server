package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogForegroundHistoryAndHeartbeatPrecedeNextBackgroundBatch(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	enqueueFake(t, queue, actor, "Lease", "lease")
	job, err := queue.Claim([]string{"fake"})
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	admission := NewCatalogWriteAdmission()
	queue.SetWriteAdmission(admission)
	history := NewPlayerHistoryService(queue.db)
	history.SetWriteAdmission(admission)
	started, release := make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- admission.WithBackground(context.Background(), func() error { close(started); <-release; return nil })
	}()
	<-started
	foregroundDone := make(chan error, 2)
	go func() {
		_, err := history.SyncContext(context.Background(), actor, 0, []PlayerHistoryChange{{SyncKey: strings.Repeat("a", 64), SourceKind: "emby", SourceID: "living-room", MediaIdentity: "movie:1", Title: "Movie", Position: 10, UpdatedAt: time.Now().Add(-time.Minute).UnixMilli()}})
		foregroundDone <- err
	}()
	go func() { foregroundDone <- queue.Heartbeat(job.Job.ID, job.LeaseToken, nil, nil, nil, nil, nil) }()
	backgroundDone := make(chan error, 1)
	go func() {
		backgroundDone <- admission.WithBackground(context.Background(), func() error {
			if admission.Stats().ForegroundCompleted != 2 {
				return errors.New("background overtook real history/heartbeat writes")
			}
			return nil
		})
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	waiting := true
	for waiting {
		stats := admission.Stats()
		if stats.ForegroundQueued == 2 && stats.BackgroundQueued == 1 {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			close(release)
			t.Fatalf("services did not join shared admission: %+v", stats)
		}
	}
	close(release)
	for _, result := range []<-chan error{firstDone, foregroundDone, foregroundDone, backgroundDone} {
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("admitted writer stalled")
		}
	}
	var count int64
	if err := queue.db.Model(&models.PlayerPlaybackHistory{}).Where("user_id=?", actor.User.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("history write=%d %v", count, err)
	}
	if _, err := queue.renewLease(job.Job.ID, job.LeaseToken); err != nil {
		t.Fatal(err)
	}
	if admission.Stats().ForegroundCompleted != 3 {
		t.Fatal("automatic lease renewal bypassed foreground admission")
	}
}

func TestCatalogForegroundCancelledSyncDoesNotWrite(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	history := NewPlayerHistoryService(queue.db)
	history.SetWriteAdmission(NewCatalogWriteAdmission())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := history.SyncContext(ctx, actor, 0, []PlayerHistoryChange{{SyncKey: strings.Repeat("b", 64), SourceKind: "emby", SourceID: "bedroom", MediaIdentity: "movie:2", Title: "Movie", Position: 2, UpdatedAt: time.Now().UnixMilli()}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sync=%v", err)
	}
	var rows int64
	if err := queue.db.Model(&models.PlayerPlaybackHistory{}).Count(&rows).Error; err != nil || rows != 0 {
		t.Fatalf("cancelled request persisted history=%d %v", rows, err)
	}
}
