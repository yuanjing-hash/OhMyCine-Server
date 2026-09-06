package services

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestPlayerOverviewContinueWatchingFindsOlderServerHistory(t *testing.T) {
	f := newPlayerHistoryCatalogFixture(t)
	now := time.Now().UTC()
	rows := make([]models.PlayerPlaybackHistory, 0, 102)
	entries := make([]models.MediaLibraryEntry, 0, 102)
	for i := 0; i < 102; i++ {
		work := fmt.Sprintf("movie:tmdb:%d", 5000+i)
		entry := f.movie[0]
		entry.ID, entry.WorkKey, entry.ProviderID, entry.RelativePath = 0, work, fmt.Sprintf("older-%d", i), fmt.Sprintf("/older-%d.mkv", i)
		entries = append(entries, entry)
		identity := playerHistoryCanonicalIdentity(f.libraryID, encodeCatalogToken(work), "movie", nil, nil, 0)
		rows = append(rows, models.PlayerPlaybackHistory{
			UserID: f.actor.User.ID, SyncKey: playerHistoryCanonicalSyncKey(identity), HistoryIdentity: identity,
			SourceKind: "server", SourceID: "server", LibraryID: uintID(f.libraryID),
			MediaIdentity: work, Title: fmt.Sprintf("Movie %d", i), Completed: i < 100,
			Position: 100, Duration: floatPointer(1000), ClientUpdatedAt: now.Add(-time.Duration(i) * time.Second).UnixMilli(), CreatedAt: now, UpdatedAt: now,
		})
	}
	if err := f.libraries.db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	service := NewPlayerOverviewService(f.history, NewPlayerMediaStateService(f.libraries.db, f.libraries), f.libraries)
	got := service.Overview(f.actor).Sections.ContinueWatching
	if got.Status != "ok" || len(got.List) != 2 || got.List[0].SyncKey != rows[100].SyncKey || got.HasMore {
		t.Fatalf("older server continue section=%+v", got)
	}
	// A missing catalog item is hidden without deleting the sync row.
	if err := f.libraries.db.Delete(&entries[100]).Error; err != nil {
		t.Fatal(err)
	}
	got = service.Overview(f.actor).Sections.ContinueWatching
	if len(got.List) != 1 || got.List[0].SyncKey != rows[101].SyncKey {
		t.Fatalf("deleted work visible: %+v", got)
	}
	page, err := f.history.List(f.actor, 2, 100, "server")
	if err != nil || page.Total != 101 || len(page.List) != 1 || page.HasMore {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

func TestPlayerHistoryFutureClockRejectsOnlyInvalidSibling(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	s := NewPlayerHistoryService(queue.db)
	valid := PlayerHistoryChange{SyncKey: strings.Repeat("a", 64), SourceKind: "emby", SourceID: "bedroom", MediaIdentity: "movie:1", Title: "Offline movie", Position: 100, UpdatedAt: time.Now().Add(-24 * time.Hour).UnixMilli()}
	future := valid
	future.SyncKey, future.UpdatedAt = strings.Repeat("b", 64), time.Now().Add(24*time.Hour).UnixMilli()
	result, err := s.Sync(actor, 0, []PlayerHistoryChange{valid, future})
	if err != nil || len(result.Changes) != 1 || len(result.Rejected) != 1 || result.Rejected[0].Code != "history_clock_ahead" {
		t.Fatalf("future batch result=%+v err=%v", result, err)
	}
	if _, err := s.Sync(actor, 0, []PlayerHistoryChange{future}); ErrorCode(err) != "history_clock_ahead" {
		t.Fatalf("single future error=%v", err)
	}
}

func TestPlayerHistoryClockBoundaryAndLegacyAnomalyPreserved(t *testing.T) {
	queue, actor, _ := queueFixture(t)
	s := NewPlayerHistoryService(queue.db)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	change := PlayerHistoryChange{SyncKey: strings.Repeat("e", 64), SourceKind: "local", SourceID: "device", MediaIdentity: "offline", Title: "Offline", Position: 10, UpdatedAt: now.Add(5 * time.Minute).UnixMilli()}
	if _, err := s.Sync(actor, 0, []PlayerHistoryChange{change}); err != nil {
		t.Fatal(err)
	}
	change.SyncKey, change.UpdatedAt = strings.Repeat("f", 64), now.Add(5*time.Minute).UnixMilli()+1
	if _, err := s.Sync(actor, 0, []PlayerHistoryChange{change}); ErrorCode(err) != CodeHistoryClockAhead {
		t.Fatalf("boundary=%v", err)
	}
	// Simulate a pre-guard client timestamp. Do not emit a lower timestamp
	// that old clients would silently ignore or misorder offline playback.
	change.UpdatedAt = now.AddDate(2, 0, 0).UnixMilli()
	row := playerHistoryRecord(actor.User.ID, change)
	row.Revision, row.CreatedAt, row.UpdatedAt = 100, now, now
	if err := queue.db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	result, err := s.Sync(actor, 0, nil)
	if err != nil || len(result.Warnings) != 1 || result.Warnings[0].SyncKey != change.SyncKey || result.Warnings[0].Code != CodeHistoryClockAhead {
		t.Fatalf("anomaly=%+v err=%v", result, err)
	}
	var stored models.PlayerPlaybackHistory
	if err := queue.db.First(&stored, "user_id = ? AND sync_key = ?", actor.User.ID, change.SyncKey).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ClientUpdatedAt != change.UpdatedAt || stored.Revision != 100 {
		t.Fatal("legacy anomaly silently rewritten")
	}
}
