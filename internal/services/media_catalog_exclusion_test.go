package services

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogExclusionHidesWorkAndRestoreKeepsSourceFacts(t *testing.T) {
	fixture := newPlayerHistoryCatalogFixture(t)
	ctx := context.Background()
	before, err := fixture.libraries.Catalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || before.Total < 2 {
		t.Fatalf("before=%+v err=%v", before, err)
	}
	denied := fixture.actor
	denied.ResourceRules = append(append([]AuthorizationRule{}, denied.ResourceRules...), AuthorizationRule{
		PermissionCode: authz.PermissionMediaLibrariesScan, Effect: models.AuthorizationEffectDeny,
		ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: uintID(fixture.libraryID),
	})
	if _, err := fixture.libraries.ExcludeCatalogWork(ctx, denied, fixture.libraryID, fixture.seriesWork, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("resource-denied exclusion=%v", err)
	}
	excluded, err := fixture.libraries.ExcludeCatalogWork(ctx, fixture.actor, fixture.libraryID, fixture.seriesWork, RequestContext{})
	if err != nil || excluded.EntryCount != len(fixture.episodes) {
		t.Fatalf("exclude=%+v err=%v", excluded, err)
	}
	page, err := fixture.libraries.Catalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || page.Total != before.Total-1 {
		t.Fatalf("hidden catalog=%+v err=%v", page, err)
	}
	var rawCount int64
	if err := fixture.libraries.db.Model(&models.MediaLibraryEntry{}).Where("library_id=? AND work_key=?", fixture.libraryID, "series:tmdb:300").Count(&rawCount).Error; err != nil || rawCount != int64(len(fixture.episodes)) {
		t.Fatalf("source facts=%d err=%v", rawCount, err)
	}
	season, episode := 1, 3
	newer := fixture.episodes[0]
	newer.ID = 0
	newer.RelativePath = "/Series/Season 01/Series.S01E03.mkv"
	newer.ProviderID = "e3"
	newer.Season = &season
	newer.Episode = &episode
	newer.CreatedAt = time.Now()
	newer.UpdatedAt = newer.CreatedAt
	if err := fixture.libraries.db.Create(&newer).Error; err != nil {
		t.Fatal(err)
	}
	page, err = fixture.libraries.Catalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || page.Total != before.Total-1 {
		t.Fatalf("new episode resurfaced=%+v err=%v", page, err)
	}
	exclusions, err := fixture.libraries.CatalogExclusions(ctx, fixture.actor, fixture.libraryID, 1, 20)
	if err != nil || exclusions.Total != 1 || exclusions.List[0].ID != excluded.ID {
		t.Fatalf("exclusions=%+v err=%v", exclusions, err)
	}
	if err := fixture.libraries.RestoreCatalogExclusion(ctx, denied, fixture.libraryID, excluded.ID, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("resource-denied restoration=%v", err)
	}
	if err := fixture.libraries.RestoreCatalogExclusion(ctx, fixture.actor, fixture.libraryID, excluded.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	page, err = fixture.libraries.Catalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || page.Total != before.Total {
		t.Fatalf("restore=%+v err=%v", page, err)
	}
}

func TestBrowserHistoryDeletionWritesUserScopedRelayTombstone(t *testing.T) {
	fixture := newPlayerHistoryCatalogFixture(t)
	change := fixture.change(strings.Repeat("a1", 32), "server-a", "https://server.example.test", playerHistoryEntryToken(fixture.libraryID, fixture.movieWork, fixture.movie[0].ID), 120, time.Now().Add(-time.Minute).UnixMilli())
	first, err := fixture.history.Sync(fixture.actor, 0, []PlayerHistoryChange{change})
	if err != nil || len(first.Changes) < 1 {
		t.Fatalf("sync=%+v err=%v", first, err)
	}
	page, err := fixture.history.BrowserList(fixture.actor, 1, 10)
	if err != nil || page.Total != 1 || page.List[0].HistoryID == "" {
		t.Fatalf("browser=%+v err=%v", page, err)
	}
	other := fixture.actor
	other.User.ID += 100
	if err := fixture.history.DeleteBrowserHistory(context.Background(), other, page.List[0].HistoryID); ErrorCode(err) != CodeNotFound {
		t.Fatalf("foreign deletion=%v", err)
	}
	if err := fixture.history.DeleteBrowserHistory(context.Background(), fixture.actor, page.List[0].HistoryID); err != nil {
		t.Fatal(err)
	}
	page, err = fixture.history.BrowserList(fixture.actor, 1, 10)
	if err != nil || page.Total != 0 {
		t.Fatalf("deleted browser=%+v err=%v", page, err)
	}
	relay, err := fixture.history.Sync(fixture.actor, first.Cursor, nil)
	if err != nil || len(relay.Changes) != 1 || !relay.Changes[0].Deleted {
		t.Fatalf("relay=%+v err=%v", relay, err)
	}
	if err := fixture.history.DeleteBrowserHistory(context.Background(), fixture.actor, change.SyncKey); ErrorCode(err) != CodeNotFound {
		t.Fatalf("repeat deletion=%v", err)
	}
	// A second device catches the deletion from its older cursor. Genuine later
	// playback is allowed to establish a new visible history entry.
	secondDevice, err := fixture.history.Sync(fixture.actor, first.Cursor, nil)
	if err != nil || len(secondDevice.Changes) != 1 || !secondDevice.Changes[0].Deleted {
		t.Fatalf("second device=%+v err=%v", secondDevice, err)
	}
	replayed := change
	replayed.Position = 30
	replayed.UpdatedAt = time.Now().Add(time.Second).UnixMilli()
	if _, err := fixture.history.Sync(fixture.actor, secondDevice.Cursor, []PlayerHistoryChange{replayed}); err != nil {
		t.Fatal(err)
	}
	page, err = fixture.history.BrowserList(fixture.actor, 1, 10)
	if err != nil || page.Total != 1 || page.List[0].Position != 30 {
		t.Fatalf("new playback=%+v err=%v", page, err)
	}
}

func TestCatalogExclusionSurvivesVersionedDeltaAndIsLibraryScoped(t *testing.T) {
	fixture, store, library := historySnapshotFixture(t)
	other := library
	other.ID, other.Name, other.NameNormalized, other.RelativeRoot = 0, "Other library", "other library", "/other"
	if err := fixture.libraries.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	otherEntry := fixture.movie[0]
	otherEntry.ID, otherEntry.LibraryID, otherEntry.RecognitionID = 0, other.ID, nil
	if err := fixture.libraries.db.Create(&otherEntry).Error; err != nil {
		t.Fatal(err)
	}
	_, err := fixture.libraries.ExcludeCatalogWork(context.Background(), fixture.actor, fixture.libraryID, fixture.movieWork, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := fixture.libraries.Catalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || page.Total != 1 {
		t.Fatalf("versioned exclude=%+v err=%v", page, err)
	}
	otherPage, err := fixture.libraries.Catalog(fixture.actor, other.ID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || otherPage.Total != 1 {
		t.Fatalf("other library=%+v err=%v", otherPage, err)
	}
	delta, token := catalogCandidate(t, store, library, "delta", 1)
	catalogPublish(t, store, delta, token)
	page, err = fixture.libraries.Catalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || page.Total != 1 {
		t.Fatalf("delta resurfaced exclusion=%+v err=%v", page, err)
	}
	if err := fixture.libraries.db.Model(&models.MediaLibrary{}).Where("id=?", fixture.libraryID).Update("exclusion_epoch", library.ExclusionEpoch+1).Error; err != nil {
		t.Fatal(err)
	}
	page, err = fixture.libraries.Catalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || page.Total != 2 {
		t.Fatalf("new source epoch must retire exclusion=%+v err=%v", page, err)
	}
}

func TestCatalogExclusionKeepsReidentifiedSourceMemberHidden(t *testing.T) {
	fixture := newPlayerHistoryCatalogFixture(t)
	excluded, err := fixture.libraries.ExcludeCatalogWork(context.Background(), fixture.actor, fixture.libraryID, fixture.seriesWork, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.libraries.db.Model(&models.MediaLibraryEntry{}).Where("id=?", fixture.episodes[0].ID).
		Update("work_key", "series:tmdb:301").Error; err != nil {
		t.Fatal(err)
	}
	page, err := fixture.libraries.Catalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || page.Total != 1 {
		t.Fatalf("reidentified source entry resurfaced=%+v err=%v", page, err)
	}
	if err := fixture.libraries.RestoreCatalogExclusion(context.Background(), fixture.actor, fixture.libraryID, excluded.ID, RequestContext{}); err != nil {
		t.Fatal(err)
	}
	page, err = fixture.libraries.Catalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || page.Total != 3 {
		t.Fatalf("restored reidentified entry=%+v err=%v", page, err)
	}
}

func TestCatalogExclusionCapturesLargeSeriesWithoutTruncatingMembers(t *testing.T) {
	fixture := newPlayerHistoryCatalogFixture(t)
	now := time.Now().UTC()
	const extra = 2001
	for start := 0; start < extra; start += 100 {
		batch := make([]models.MediaLibraryEntry, 0, 100)
		for offset := start; offset < extra && offset < start+100; offset++ {
			entry := fixture.episodes[0]
			entry.ID = 0
			entry.RelativePath = fmt.Sprintf("/Series/Season 99/Series.S99E%04d.mkv", offset)
			entry.ProviderID = fmt.Sprintf("large-%d", offset)
			entry.CreatedAt, entry.UpdatedAt = now, now
			batch = append(batch, entry)
		}
		if err := fixture.libraries.db.CreateInBatches(batch, 100).Error; err != nil {
			t.Fatal(err)
		}
	}
	excluded, err := fixture.libraries.ExcludeCatalogWork(context.Background(), fixture.actor, fixture.libraryID, fixture.seriesWork, RequestContext{})
	if err != nil || excluded.EntryCount != extra+len(fixture.episodes) {
		t.Fatalf("excluded=%+v err=%v", excluded, err)
	}
	var members int64
	if err := fixture.libraries.db.Model(&models.MediaCatalogExclusionMember{}).Where("exclusion_id=?", excluded.ID).Count(&members).Error; err != nil || members != int64(excluded.EntryCount) {
		t.Fatalf("captured members=%d err=%v", members, err)
	}
	page, err := fixture.libraries.Catalog(fixture.actor, fixture.libraryID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || page.Total != 1 {
		t.Fatalf("large series resurfaced: page=%+v err=%v", page, err)
	}
}
