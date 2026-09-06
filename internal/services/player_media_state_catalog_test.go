package services

import (
	"context"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

func TestMediaStateVersionedPinAndTombstoneEligibility(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	base := catalogConvert(t, store, library, rec, entries)
	if err := store.writeDB.Model(&library).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	var user models.User
	if err := store.writeDB.First(&user).Error; err != nil {
		t.Fatal(err)
	}
	actor := Actor{User: user, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	libraries := &MediaLibraryService{db: store.writeDB}
	libraries.SetCatalogSnapshotStore(store)
	s := NewPlayerMediaStateService(store.writeDB, libraries)
	itemID := playerMediaStateItemID(library.ID, entries[0].WorkKey)
	if ok, err := s.SetFavorite(actor, itemID, true); err != nil || !ok {
		t.Fatalf("favorite=%v err=%v", ok, err)
	}
	collection, err := s.CreateCollection(actor, "Pinned versions", "collection")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddCollectionItem(actor, collection.ID, itemID); err != nil {
		t.Fatal(err)
	}
	_, err = readPlayerMediaState(s, actor, func(view *PlayerMediaStateService) (bool, error) {
		before, err := view.FavoritePage(actor, 1, 20)
		if err != nil || before.Total != 1 {
			t.Fatalf("before=%+v err=%v", before, err)
		}
		candidate, token := catalogCandidate(t, store, library, "delta", 1)
		var facts []models.CatalogEntryFact
		for _, entry := range entries {
			fact := CatalogEntryFromLegacy(entry, &rec)
			fact.Tombstone = true
			facts = append(facts, fact)
		}
		if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Entries: facts}); err != nil {
			t.Fatal(err)
		}
		catalogPublish(t, store, candidate, token)
		// Publication must succeed while a deferred reader holds its old head.
		after, err := view.CollectionItemPage(actor, collection.ID, 1, 20)
		if err != nil || after.Total != 1 {
			t.Fatalf("mixed snapshot=%+v err=%v", after, err)
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.FavoritePage(actor, 1, 20)
	if err != nil || page.Total != 0 || len(page.List) != 0 {
		t.Fatalf("retired favorite=%+v err=%v", page, err)
	}
	items, err := s.CollectionItems(actor, collection.ID)
	if err != nil || len(items) != 0 {
		t.Fatalf("retired collection=%+v err=%v", items, err)
	}
	if _, err := s.SetFavorite(actor, itemID, true); ErrorCode(err) != CodeNotFound {
		t.Fatalf("anchor accepted as live: %v", err)
	}
	if err := s.AddCollectionItem(actor, collection.ID, itemID); ErrorCode(err) != CodeNotFound {
		t.Fatalf("anchor membership accepted: %v", err)
	}
	var anchors, favorites int64
	if err := store.writeDB.Model(&models.MediaLibraryEntry{}).Where("library_id = ?", library.ID).Count(&anchors).Error; err != nil || anchors != 2 {
		t.Fatalf("anchors=%d err=%v", anchors, err)
	}
	if err := store.writeDB.Model(&models.PlayerMediaFavorite{}).Where("user_id = ?", user.ID).Count(&favorites).Error; err != nil || favorites != 1 {
		t.Fatalf("account data deleted=%d err=%v", favorites, err)
	}
	if err := store.writeDB.Model(&models.CatalogSnapshot{}).Where("id = ?", base.ID).Update("state", "building").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Favorites(actor); err == nil {
		t.Fatal("corrupt catalog disguised as empty favorites")
	}
}

func TestMediaStateMutationsValidateInsideWriterTransaction(t *testing.T) {
	f := newPlayerHistoryCatalogFixture(t)
	s := NewPlayerMediaStateService(f.libraries.db, f.libraries)
	c, err := s.CreateCollection(f.actor, "Writer-bound eligibility", "collection")
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	const callback = "test:state_writer_eligibility"
	if err := s.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "media_libraries" {
			checks++
			if _, ok := tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
				t.Error("eligibility read escaped writer transaction")
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.db.Callback().Query().Remove(callback) })
	itemID := playerMediaStateItemID(f.libraryID, f.movie[0].WorkKey)
	if _, err := s.SetFavorite(f.actor, itemID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddCollectionItem(f.actor, c.ID, itemID); err != nil {
		t.Fatal(err)
	}
	if checks < 2 {
		t.Fatalf("eligibility checks=%d", checks)
	}
}
