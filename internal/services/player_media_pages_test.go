package services

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestMediaStateBatchCardsMatchLegacyProjection(t *testing.T) {
	f := newPlayerHistoryCatalogFixture(t)
	s := NewPlayerMediaStateService(f.libraries.db, f.libraries)
	keys := []mediaStateWork{{f.libraryID, f.episodes[0].WorkKey}, {f.libraryID, f.movie[0].WorkKey}}
	got, err := s.browserStateItems(keys)
	if err != nil {
		t.Fatal(err)
	}
	for i, key := range keys {
		legacy, err := s.item(f.actor, key.libraryID, key.workKey)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got[i], browserMediaItem(legacy)) {
			t.Fatalf("card differs: got=%+v legacy=%+v", got[i], browserMediaItem(legacy))
		}
	}
	if _, err := s.SetFavorite(f.actor, playerMediaStateItemID(f.libraryID, f.movie[0].WorkKey), true); err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := f.libraries.db.First(&library, f.libraryID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.db.Model(&models.Storage{}).Where("id = ?", library.StorageID).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	page, err := s.FavoritePage(f.actor, 1, 24)
	if err != nil || page.Total != 0 || len(page.List) != 0 {
		t.Fatalf("disabled storage leaked: %+v %v", page, err)
	}
}

func TestMediaStatePagesReachBeyondLegacyCapsAndFilterBeforeCount(t *testing.T) {
	f := newPlayerHistoryCatalogFixture(t)
	s := NewPlayerMediaStateService(f.libraries.db, f.libraries)
	now := time.Now().UTC()
	collection, err := s.CreateCollection(f.actor, "Long collection", "collection")
	if err != nil {
		t.Fatal(err)
	}
	var entries []models.MediaLibraryEntry
	var favorites []models.PlayerMediaFavorite
	var members []models.PlayerMediaCollectionItem
	for i := 0; i < 1002; i++ {
		work := fmt.Sprintf("movie:tmdb:%d", 5000+i)
		entry := f.movie[0]
		entry.ID, entry.WorkKey, entry.ProviderID, entry.RelativePath = 0, work, fmt.Sprintf("state-%d", i), fmt.Sprintf("/state-%d.mkv", i)
		entries = append(entries, entry)
		favorites = append(favorites, models.PlayerMediaFavorite{UserID: f.actor.User.ID, LibraryID: f.libraryID, WorkKey: work, CreatedAt: now, UpdatedAt: now})
		members = append(members, models.PlayerMediaCollectionItem{CollectionID: collection.ID, LibraryID: f.libraryID, WorkKey: work, Origin: "manual", Ordinal: i, CreatedAt: now, UpdatedAt: now})
	}
	if err := f.libraries.db.CreateInBatches(&entries, 100).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.db.CreateInBatches(&favorites, 100).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.db.CreateInBatches(&members, 100).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.libraries.db.Delete(&entries[0]).Error; err != nil {
		t.Fatal(err)
	}
	page, err := s.FavoritePage(f.actor, 11, 100)
	if err != nil || page.Total != 1001 || len(page.List) != 1 || page.HasMore {
		t.Fatalf("favorites=%+v %v", page, err)
	}
	items, err := s.CollectionItemPage(f.actor, collection.ID, 11, 100)
	if err != nil || items.Total != 1001 || len(items.List) != 1 || items.HasMore {
		t.Fatalf("items=%+v %v", items, err)
	}
	var collections []models.PlayerMediaCollection
	for i := 0; i < 501; i++ {
		collections = append(collections, models.PlayerMediaCollection{ID: uuid.NewString(), OwnerID: &f.actor.User.ID, Source: "manual", Kind: "collection", Name: fmt.Sprintf("C%04d", i), Visible: true, Revision: 1, CreatedAt: now, UpdatedAt: now})
	}
	if err := f.libraries.db.CreateInBatches(&collections, 100).Error; err != nil {
		t.Fatal(err)
	}
	list, err := s.CollectionPage(f.actor, "collection", "manual", 6, 100)
	if err != nil || list.Total != 502 || len(list.List) != 2 || list.HasMore {
		t.Fatalf("collections=%+v %v", list, err)
	}
	foreign := f.actor
	foreign.User.ID += 999
	other, err := s.FavoritePage(foreign, 1, 100)
	if err != nil || other.Total != 0 {
		t.Fatalf("foreign favorites=%+v %v", other, err)
	}
	if _, err := s.CollectionItemPage(foreign, collection.ID, 1, 100); ErrorCode(err) != CodeNotFound {
		t.Fatal("foreign members", err)
	}
}

func TestCollectionRenameReorderCASAndAutomaticProtection(t *testing.T) {
	f := newPlayerHistoryCatalogFixture(t)
	s := NewPlayerMediaStateService(f.libraries.db, f.libraries)
	c, err := s.CreateCollection(f.actor, "Original", "collection")
	if err != nil {
		t.Fatal(err)
	}
	one := playerMediaStateItemID(f.libraryID, f.movie[0].WorkKey)
	two := playerMediaStateItemID(f.libraryID, f.episodes[0].WorkKey)
	for _, id := range []string{one, two} {
		if err := s.AddCollectionItem(f.actor, c.ID, id); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.CollectionItemPage(f.actor, c.ID, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RenameCollection(f.actor, c.ID, "Renamed", p.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameCollection(f.actor, c.ID, "Stale", p.Revision); ErrorCode(err) != CodeConflict {
		t.Fatal("stale rename", err)
	}
	if err := s.MoveCollectionItem(f.actor, c.ID, two, one, p.Revision); ErrorCode(err) != CodeConflict {
		t.Fatal("stale move", err)
	}
	if err := s.MoveCollectionItem(f.actor, c.ID, two, one, p.Revision+1); err != nil {
		t.Fatal(err)
	}
	p, err = s.CollectionItemPage(f.actor, c.ID, 1, 10)
	if err != nil || len(p.List) != 2 || p.List[0].Kind != "series" {
		t.Fatalf("order=%+v %v", p, err)
	}
	if err := f.libraries.db.Model(&c).Updates(map[string]any{"source": "tmdb", "owner_id": nil}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.RenameCollection(f.actor, c.ID, "No", p.Revision); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("tmdb rename", err)
	}
	if err := s.MoveCollectionItem(f.actor, c.ID, one, two, p.Revision); ErrorCode(err) != CodePermissionDenied {
		t.Fatal("tmdb move", err)
	}
}
