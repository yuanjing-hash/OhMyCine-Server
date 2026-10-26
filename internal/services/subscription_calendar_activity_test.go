package services

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func calendarDownload(t *testing.T, s *SubscriptionCalendarService, actor Actor, id string) models.DownloadTask {
	t.Helper()
	job, err := s.queue.Enqueue(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "download", DisplayName: "测试下载", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	task := models.DownloadTask{ID: id, OwnerID: actor.User.ID, JobID: job.ID, DownloaderName: "测试下载器", ProviderType: "qbittorrent", SourceCiphertext: "private-source", DisplayName: "测试下载", Phase: "completed", CreatedAt: s.now(), UpdatedAt: s.now()}
	if err := s.db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	return task
}

func TestSubscriptionCalendarActivityUsesExactClaimsAndFencesChangedFacts(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	actor.Permissions[authz.PermissionDownloadsReadOwn] = struct{}{}
	actor.Permissions[authz.PermissionTransfersReadOwn] = struct{}{}
	calendarSubscription(t, s, actor, "sub", 100, 1)
	calendarSeed(t, s, 100, 1, tmdb.EpisodeSnapshot{EpisodeNumber: 1, AirDate: "2026-08-13"}, tmdb.EpisodeSnapshot{EpisodeNumber: 2, AirDate: "2026-08-14"})
	task := calendarDownload(t, s, actor, "task")
	if err := s.db.Create(&models.FollowEpisodeClaim{SubscriptionID: "sub", SeasonNumber: 1, EpisodeNumber: 1, State: "queued", DownloadTaskID: &task.ID, UpdatedAt: s.now()}).Error; err != nil {
		t.Fatal(err)
	}
	q := calendarMonthQuery()
	q.PageSize = 1
	first, err := s.Read(context.Background(), actor, q)
	if err != nil || len(first.Events) != 1 || first.Events[0].Activity != "downloading" || !first.HasMore {
		t.Fatalf("first=%+v %v", first, err)
	}
	if err := s.db.Model(&models.Job{}).Where("id = ?", task.JobID).Update("status", models.JobStatusCompleted).Error; err != nil {
		t.Fatal(err)
	}
	transferJob, err := s.queue.Enqueue(EnqueueJobInput{OwnerID: actor.User.ID, JobType: "transfer", DisplayName: "测试整理", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.Create(&models.TransferTask{ID: "transfer", OwnerID: actor.User.ID, JobID: transferJob.ID, DownloadTaskID: task.ID, LibraryID: 1, LibraryName: "媒体库", ManifestJSON: "{}", Phase: models.TransferTaskStatusMoving}).Error; err != nil {
		t.Fatal(err)
	}
	q.Cursor = first.NextCursor
	if _, err := s.Read(context.Background(), actor, q); ErrorCode(err) != CodeConflict {
		t.Fatalf("changed task facts accepted old cursor: %v", err)
	}
	q.Cursor = ""
	q.PageSize = 200
	page, err := s.Read(context.Background(), actor, q)
	if err != nil || page.Events[0].Activity != "organizing" || page.Events[1].Activity != "none" {
		t.Fatalf("exact episode binding=%+v %v", page, err)
	}
	delete(actor.Permissions, authz.PermissionTransfersReadOwn)
	page, err = s.Read(context.Background(), actor, q)
	if err != nil || page.Events[0].Activity != "none" {
		t.Fatalf("revoked transfer activity leaked: %+v %v", page, err)
	}
	delete(actor.Permissions, authz.PermissionDownloadsReadOwn)
	page, err = s.Read(context.Background(), actor, q)
	if err != nil || page.Events[0].Activity != "none" {
		t.Fatalf("revoked download activity leaked: %+v %v", page, err)
	}
}

func TestSubscriptionCalendarActivityBudgetIsExplicitlyUnknown(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	actor.Permissions[authz.PermissionDownloadsReadOwn] = struct{}{}
	calendarSubscription(t, s, actor, "sub", 100, 1)
	calendarSeed(t, s, 100, 1, tmdb.EpisodeSnapshot{EpisodeNumber: maxCalendarActivityClaims + 1, AirDate: "2026-08-13"})
	task := calendarDownload(t, s, actor, "task")
	claims := make([]models.FollowEpisodeClaim, maxCalendarActivityClaims+1)
	for i := range claims {
		claims[i] = models.FollowEpisodeClaim{SubscriptionID: "sub", SeasonNumber: 1, EpisodeNumber: i + 1, State: "queued", DownloadTaskID: &task.ID, UpdatedAt: s.now()}
	}
	if err := s.db.CreateInBatches(&claims, 100).Error; err != nil {
		t.Fatal(err)
	}
	page, err := s.Read(context.Background(), actor, calendarMonthQuery())
	if err != nil || !page.Partial || len(page.Events) != 1 || page.Events[0].Activity != "unknown" {
		t.Fatalf("budget became no activity: %+v %v", page, err)
	}
	found := false
	for _, reason := range page.Reasons {
		found = found || reason == "activity_budget"
	}
	if !found {
		t.Fatalf("missing budget reason: %+v", page.Reasons)
	}
}

func TestSubscriptionCalendarActivityBatchesReadsInsteadOfPerSubscription(t *testing.T) {
	s, actor, _ := calendarFixture(t)
	actor.Permissions[authz.PermissionDownloadsReadOwn] = struct{}{}
	actor.Permissions[authz.PermissionTransfersReadOwn] = struct{}{}
	task := calendarDownload(t, s, actor, "task")
	raw, _ := json.Marshal(FollowExecutionSnapshot{Version: 2, Seasons: []int{1}, MediaLibraryID: 1, SiteIDs: []uint{}})
	ids := []string{}
	subs := []models.FollowSubscription{}
	claims := []models.FollowEpisodeClaim{}
	for n := 0; n < 600; n++ {
		id := fmt.Sprintf("sub-%04d", n)
		ids = append(ids, id)
		subs = append(subs, models.FollowSubscription{ID: id, OwnerID: actor.User.ID, MediaType: "tv", TMDBID: 100, Title: "批量订阅", Status: models.FollowStatusActive, Revision: 1, ExecutionSnapshotJSON: string(raw), CreatedAt: s.now(), UpdatedAt: s.now()})
		claims = append(claims, models.FollowEpisodeClaim{SubscriptionID: id, SeasonNumber: 1, EpisodeNumber: 1, State: "queued", DownloadTaskID: &task.ID, UpdatedAt: s.now()})
	}
	for _, rows := range []any{&subs, &claims} {
		if err := s.db.CreateInBatches(rows, 100).Error; err != nil {
			t.Fatal(err)
		}
	}
	queries := 0
	if err := s.db.Callback().Query().After("gorm:query").Register("calendar_activity_test", func(tx *gorm.DB) {
		switch tx.Statement.Table {
		case "follow_episode_claims", "download_tasks", "jobs", "transfer_tasks":
			queries++
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.db.Callback().Query().Remove("calendar_activity_test") })
	facts, bounded, err := s.episodeActivityFacts(context.Background(), actor, ids)
	if err != nil || bounded || len(facts) != 600 || queries > 5 {
		t.Fatalf("facts=%d bounded=%v queries=%d err=%v", len(facts), bounded, queries, err)
	}
	for _, id := range ids {
		if facts[id][[2]int{1, 1}] != "downloading" || len(facts[id]) != 1 {
			t.Fatalf("wrong claim facts for %s: %+v", id, facts[id])
		}
	}
}
