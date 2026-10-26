package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func seedBrowserEpisodeHistory(t *testing.T, f playerHistoryCatalogFixture) PlayerHistorySyncResult {
	t.Helper()
	changes := []PlayerHistoryChange{
		f.change(strings.Repeat("a", 64), "device-one", "https://server.example.test", playerHistoryEntryToken(f.libraryID, f.seriesWork, f.episodes[0].ID), 120, 1_000),
		f.change(strings.Repeat("b", 64), "device-two", "https://server.example.test", playerHistoryEntryToken(f.libraryID, f.seriesWork, f.episodes[2].ID), 30, 2_000),
	}
	result, err := f.history.Sync(f.actor, 0, changes)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestBrowserHistoryGroupsBeforePagingAndPreservesExactEpisodeReads(t *testing.T) {
	f := newPlayerHistoryCatalogFixture(t)
	seedBrowserEpisodeHistory(t, f)
	movie := f.change(strings.Repeat("c", 64), "device", "https://server.example.test", playerHistoryEntryToken(f.libraryID, f.movieWork, f.movie[0].ID), 10, 3_000)
	if _, err := f.history.Sync(f.actor, 0, []PlayerHistoryChange{movie}); err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.db.Model(&models.PlayerPlaybackHistory{}).Where("user_id = ?", f.actor.User.ID).Update("display_title", "同名作品").Error; err != nil {
		t.Fatal(err)
	}
	first, err := f.history.BrowserListWithOptions(f.actor, 1, 1, BrowserHistoryOptions{GroupByWork: true})
	if err != nil || first.Total != 2 || len(first.List) != 1 || first.List[0].WorkID != f.movieWork || !first.HasMore {
		t.Fatalf("first grouped page=%+v err=%v", first, err)
	}
	second, err := f.history.BrowserListWithOptions(f.actor, 2, 1, BrowserHistoryOptions{GroupByWork: true})
	if err != nil || second.Total != 2 || len(second.List) != 1 || second.List[0].WorkID != f.seriesWork || second.List[0].Subtitle != "S01E02 · 第二集" || second.List[0].RecordCount != 2 || second.HasMore {
		t.Fatalf("second grouped page=%+v err=%v", second, err)
	}
	exact, err := f.history.BrowserList(f.actor, 1, 10)
	if err != nil || exact.Total != 3 || len(exact.List) != 3 {
		t.Fatalf("legacy exact page=%+v err=%v", exact, err)
	}
	work, err := f.history.BrowserListWithOptions(f.actor, 1, 10, BrowserHistoryOptions{LibraryID: f.libraryID, WorkID: f.seriesWork})
	if err != nil || work.Total != 2 || len(work.List) != 2 || work.List[0].Position != 30 || work.List[1].Position != 120 {
		t.Fatalf("exact work page=%+v err=%v", work, err)
	}
	foreign := f.actor
	foreign.User.ID += 100
	private, err := f.history.BrowserListWithOptions(foreign, 1, 10, BrowserHistoryOptions{GroupByWork: true})
	if err != nil || private.Total != 0 {
		t.Fatalf("foreign history=%+v err=%v", private, err)
	}
	if _, err := f.history.BrowserListWithOptions(f.actor, 1, 10, BrowserHistoryOptions{WorkID: f.seriesWork}); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("partial work scope accepted: %v", err)
	}
}

func TestBrowserHistoryWatchedUsesLatestWorkStateAndSyncsExactRow(t *testing.T) {
	f := newPlayerHistoryCatalogFixture(t)
	seed := seedBrowserEpisodeHistory(t, f)
	items, more, err := f.history.BrowserContinueWatching(f.actor, 1)
	if err != nil || len(items) != 1 || more || items[0].Subtitle != "S01E02 · 第二集" {
		t.Fatalf("initial work continue=%+v more=%v err=%v", items, more, err)
	}
	id := items[0].HistoryID
	f.history.now = func() time.Time { return time.UnixMilli(4_000) }
	if err := f.history.SetBrowserHistoryWatched(context.Background(), f.actor, id, true); err != nil {
		t.Fatal(err)
	}
	items, more, err = f.history.BrowserContinueWatching(f.actor, 1)
	if err != nil || len(items) != 0 || more {
		t.Fatalf("completion resurrected old unfinished episode: %+v more=%v err=%v", items, more, err)
	}
	delta, err := f.history.Sync(f.actor, seed.Cursor, nil)
	if err != nil || len(delta.Changes) != 1 || delta.Changes[0].SyncKey != id || !delta.Changes[0].Completed || delta.Changes[0].Position != 30 {
		t.Fatalf("watched delta=%+v err=%v", delta, err)
	}
	if err := f.history.SetBrowserHistoryWatched(context.Background(), f.actor, id, true); err != nil {
		t.Fatal(err)
	}
	idempotent, err := f.history.Sync(f.actor, delta.Cursor, nil)
	if err != nil || len(idempotent.Changes) != 0 || idempotent.Cursor != delta.Cursor {
		t.Fatalf("repeated watched action changed cursor: %+v err=%v", idempotent, err)
	}
	old := delta.Changes[0]
	old.Completed, old.UpdatedAt = false, 2_000
	if _, err := f.history.Sync(f.actor, 0, []PlayerHistoryChange{old}); err != nil {
		t.Fatal(err)
	}
	var stored models.PlayerPlaybackHistory
	if err := f.libraries.db.Where("user_id = ? AND sync_key = ?", f.actor.User.ID, id).First(&stored).Error; err != nil || !stored.Completed || stored.Position != 30 {
		t.Fatalf("stale progress undid completion: %+v err=%v", stored, err)
	}
	if err := f.history.SetBrowserHistoryWatched(context.Background(), f.actor, id, false); err != nil {
		t.Fatal(err)
	}
	items, _, err = f.history.BrowserContinueWatching(f.actor, 1)
	if err != nil || len(items) != 1 || items[0].HistoryID != id || items[0].Position != 30 {
		t.Fatalf("unwatched lost exact resume: %+v err=%v", items, err)
	}
}

func TestBrowserContinueWatchingIgnoresUnstartedRowsBeforeWorkSelection(t *testing.T) {
	f := newPlayerHistoryCatalogFixture(t)
	seedBrowserEpisodeHistory(t, f)
	unstarted := f.change(strings.Repeat("d", 64), "device", "https://server.example.test", playerHistoryEntryToken(f.libraryID, f.seriesWork, f.episodes[0].ID), 0, 3_000)
	if _, err := f.history.Sync(f.actor, 0, []PlayerHistoryChange{unstarted}); err != nil {
		t.Fatal(err)
	}
	items, more, err := f.history.BrowserContinueWatching(f.actor, 1)
	if err != nil || len(items) != 1 || more || items[0].Subtitle != "S01E02 · 第二集" || items[0].Position != 30 {
		t.Fatalf("unstarted row hid confirmed progress: %+v more=%v err=%v", items, more, err)
	}
}

func TestBrowserHistoryWatchedRechecksOwnershipAndCatalogVisibility(t *testing.T) {
	f := newPlayerHistoryCatalogFixture(t)
	seedBrowserEpisodeHistory(t, f)
	items, _, err := f.history.BrowserContinueWatching(f.actor, 1)
	if err != nil || len(items) != 1 {
		t.Fatalf("continue=%+v err=%v", items, err)
	}
	id := items[0].HistoryID
	foreign := f.actor
	foreign.User.ID += 100
	if err := f.history.SetBrowserHistoryWatched(context.Background(), foreign, id, true); ErrorCode(err) != CodeNotFound {
		t.Fatalf("foreign actor changed watched: %v", err)
	}
	denied := f.actor
	denied.Permissions = nil
	if err := f.history.SetBrowserHistoryWatched(context.Background(), denied, id, true); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("missing read permission changed watched: %v", err)
	}
	if err := f.libraries.db.Model(&models.MediaLibrary{}).Where("id = ?", f.libraryID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.history.SetBrowserHistoryWatched(context.Background(), f.actor, id, true); ErrorCode(err) != CodeNotFound {
		t.Fatalf("disabled catalog changed watched: %v", err)
	}
}
