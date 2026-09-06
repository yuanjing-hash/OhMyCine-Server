package services

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func collectionSnapshotMetadata(t *testing.T, movieID, collectionID int64, name string) string {
	t.Helper()
	value, err := marshalRecognitionMetadata(MediaRecognitionResult{Snapshot: tmdb.Snapshot{Version: 1, MediaType: "movie", TMDBID: movieID, Title: "Film", Collection: &tmdb.Collection{TMDBID: collectionID, Name: name, PosterPath: "/collection.jpg"}}})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func collectionSnapshotFixture(t *testing.T) (*CatalogSnapshotStore, *PlayerMediaStateService, models.MediaLibrary, models.MediaLibraryRecognition, []models.MediaLibraryEntry, models.PlayerMediaCollection, Actor) {
	t.Helper()
	store, library, rec, entries := catalogFixture(t)
	library.Enabled = true
	if err := store.writeDB.Model(&library).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	filmID, collectionID := int64(101), int64(9001)
	rec.MediaType, rec.Title, rec.TMDBID = "movie", "Film", &filmID
	rec.MetadataJSON = collectionSnapshotMetadata(t, filmID, collectionID, "Committed Saga")
	rec.UpdatedAt = time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	for i := range entries {
		entries[i].MediaType, entries[i].Title, entries[i].SeriesTitle, entries[i].WorkKey, entries[i].TMDBID = "movie", "Film", "", "movie:tmdb:101", &filmID
		entries[i].Season, entries[i].Episode = nil, nil
	}
	legacy := models.PlayerMediaCollection{ID: uuid.NewString(), Source: models.PlayerMediaCollectionSourceTMDB, Kind: models.PlayerMediaCollectionKindCollection, TMDBCollectionID: &collectionID, Name: "Old global metadata", Visible: true, Revision: 7}
	if err := store.writeDB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	libraries := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
	libraries.SetCatalogSnapshotStore(store)
	actor := Actor{Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	if err := store.writeDB.First(&actor.User).Error; err != nil {
		t.Fatal(err)
	}
	return store, NewPlayerMediaStateService(store.writeDB, libraries), library, rec, entries, legacy, actor
}

func addLegacyCollectionFilm(t *testing.T, store *CatalogSnapshotStore, library models.MediaLibrary, collection models.PlayerMediaCollection, movieID int64) models.MediaLibrary {
	t.Helper()
	other := models.MediaLibrary{Name: "Legacy", NameNormalized: "legacy-" + strconv.FormatInt(movieID, 10), StorageID: library.StorageID, ProfileID: library.ProfileID, Enabled: true}
	if err := store.writeDB.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	work := "movie:tmdb:" + strconv.FormatInt(movieID, 10)
	entry := models.MediaLibraryEntry{LibraryID: other.ID, RelativePath: "other.mkv", MediaType: "movie", Title: "Other", WorkKey: work, TMDBID: &movieID, MatchStatus: "matched"}
	if err := store.writeDB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	member := models.PlayerMediaCollectionItem{CollectionID: collection.ID, LibraryID: other.ID, WorkKey: work, TMDBMovieID: &movieID, Origin: models.PlayerMediaCollectionItemOriginTMDB}
	if err := store.writeDB.Create(&member).Error; err != nil {
		t.Fatal(err)
	}
	return other
}

func TestCatalogCollectionsCommittedGlobalThresholdThenActorAndVersions(t *testing.T) {
	store, state, library, rec, entries, legacy, actor := collectionSnapshotFixture(t)
	catalogConvert(t, store, library, rec, entries)
	page, err := state.CollectionPage(actor, "", "tmdb", 1, 20)
	if err != nil || page.Total != 0 {
		t.Fatalf("two versions counted as films: %+v %v", page, err)
	}
	other := addLegacyCollectionFilm(t, store, library, legacy, 102)
	denied := actor
	denied.ResourceRules = []AuthorizationRule{{PermissionCode: authz.PermissionMediaLibrariesRead, Effect: models.AuthorizationEffectDeny, ResourceType: models.AuthorizationResourceMediaLibrary, ResourceID: strconv.FormatUint(uint64(other.ID), 10)}}
	page, err = state.CollectionPage(denied, "", "tmdb", 1, 20)
	if err != nil || page.Total != 1 || len(page.List) != 1 || page.List[0].ID != legacy.ID || page.List[0].ItemCount != 1 || page.List[0].Name != "Committed Saga" {
		t.Fatalf("global eligibility/metadata=%+v %v", page, err)
	}
	items, err := state.CollectionItemPage(denied, legacy.ID, 1, 20)
	if err != nil || items.Total != 1 || len(items.List) != 1 || items.List[0].LibraryID != library.ID {
		t.Fatalf("actor members=%+v %v", items, err)
	}
	playerItems, err := state.CollectionItems(denied, legacy.ID)
	if err != nil || len(playerItems) != 1 || playerItems[0].FileCount != 2 {
		t.Fatalf("player versions=%+v %v", playerItems, err)
	}
	var anchor models.PlayerMediaCollection
	if err := store.writeDB.First(&anchor, "id=?", legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if anchor.Name != legacy.Name || anchor.Revision != legacy.Revision {
		t.Fatal("candidate changed global metadata")
	}
	if err := store.writeDB.Model(&other).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	page, err = state.CollectionPage(actor, "", "tmdb", 1, 20)
	if err != nil || page.Total != 0 {
		t.Fatalf("disabled library supplied global threshold: %+v %v", page, err)
	}
}

func TestCatalogCollectionsPreparedMetadataInvisibleAndPartialKeepsUnseen(t *testing.T) {
	store, state, library, rec, entries, legacy, actor := collectionSnapshotFixture(t)
	catalogConvert(t, store, library, rec, entries)
	addLegacyCollectionFilm(t, store, library, legacy, 102)
	manual, err := state.CreateCollection(actor, "My list", "collection")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.AddCollectionItem(actor, manual.ID, playerMediaStateItemID(library.ID, entries[0].WorkKey)); err != nil {
		t.Fatal(err)
	}
	c, token := catalogCandidate(t, store, library, "delta", 1)
	rec.MetadataJSON = collectionSnapshotMetadata(t, 101, 9001, "New hidden metadata")
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Seal(context.Background(), c.ID, token); err != nil {
		t.Fatal(err)
	}
	page, err := state.CollectionPage(actor, "", "tmdb", 1, 20)
	if err != nil || len(page.List) != 1 || page.List[0].Name != "Committed Saga" {
		t.Fatalf("candidate leaked=%+v %v", page, err)
	}
	if err := store.writeDB.Transaction(func(tx *gorm.DB) error {
		_, err := store.PublishTx(tx, c.ID, token, 1, func(*gorm.DB) error { return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	page, err = state.CollectionPage(actor, "", "tmdb", 1, 20)
	if err != nil || len(page.List) != 1 || page.List[0].Name != "New hidden metadata" || page.List[0].ItemCount != 2 {
		t.Fatalf("partial lost unseen=%+v %v", page, err)
	}
	manualPage, err := state.CollectionItemPage(actor, manual.ID, 1, 20)
	if err != nil || manualPage.Total != 1 || manualPage.Revision != 2 {
		t.Fatalf("manual state changed=%+v %v", manualPage, err)
	}
	// An observed correction changes work identity. Explicitly tombstone the
	// old logical member while preserving untouched library membership.
	c, token = catalogCandidate(t, store, library, "delta", 2)
	id := int64(103)
	rec.TMDBID = &id
	rec.MetadataJSON = collectionSnapshotMetadata(t, 103, 9001, "Corrected film")
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	var old models.CatalogCollectionMemberFact
	if err := store.writeDB.Where("snapshot_id=? AND work_key=?", c.ID, "movie:tmdb:101").First(&old).Error; err != nil || !old.Tombstone {
		t.Fatalf("old member not tombstoned: %+v %v", old, err)
	}
	items, err := state.CollectionItemPage(actor, legacy.ID, 1, 20)
	if err != nil || items.Total != 2 {
		t.Fatalf("corrected membership=%+v %v", items, err)
	}
}

func TestCatalogCollectionsPreparationBudgetAndManifestFence(t *testing.T) {
	store, _, library, rec, entries, _, _ := collectionSnapshotFixture(t)
	catalogConvert(t, store, library, rec, entries)
	c, token := catalogCandidate(t, store, library, "delta", 1)
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	// Simulates already-budgeted facts without allocating a giant fixture;
	// one derived membership must not escape the total delta byte budget.
	if err := store.writeDB.Model(&models.CatalogSnapshot{}).Where("id=?", c.ID).Update("byte_count", CatalogMaxDeltaBytes).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.Seal(context.Background(), c.ID, token); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("projection bypassed budget: %v", err)
	}
	var head models.CatalogHead
	if err := store.writeDB.First(&head, "library_id=?", library.ID).Error; err != nil || head.Revision != 1 {
		t.Fatalf("budget failure advanced head: %+v %v", head, err)
	}
	if err := store.Abandon(context.Background(), c.ID, token); err != nil {
		t.Fatal(err)
	}
	c, token = catalogCandidate(t, store, library, "delta", 1)
	if err := store.Seal(context.Background(), c.ID, token); err != nil {
		t.Fatal(err)
	}
	if err := store.writeDB.Model(&models.CatalogCollectionPreparation{}).Where("snapshot_id=?", c.ID).Update("state", "preparing").Error; err != nil {
		t.Fatal(err)
	}
	err := store.writeDB.Transaction(func(tx *gorm.DB) error {
		_, err := store.PublishTx(tx, c.ID, token, 1, func(*gorm.DB) error { return nil })
		return err
	})
	if !errors.Is(err, ErrCatalogFence) {
		t.Fatalf("unprepared projection published: %v", err)
	}
}

func TestCatalogCollectionsPartialDeltaRetainsSameLibraryUnseenAndCompleteRemoval(t *testing.T) {
	store, state, library, rec, entries, legacy, actor := collectionSnapshotFixture(t)
	catalogConvert(t, store, library, rec, entries)
	id := int64(102)
	second := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "second-film", ProfileID: library.ProfileID, ProfileRevision: library.ProfileRevision, MediaType: "movie", Title: "Second", Status: "matched", TMDBID: &id, MetadataJSON: collectionSnapshotMetadata(t, id, 9001, "Committed Saga")}
	secondEntry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "second-film.mkv", RecognitionID: &second.ID, MediaType: "movie", Title: second.Title, WorkKey: "movie:tmdb:102", TMDBID: &id, MatchStatus: "matched"}
	c, token := catalogCandidate(t, store, library, "delta", 1)
	allocated, err := store.ResolveIdentities(context.Background(), c.ID, token, []CatalogIdentityRequest{{Kind: "recognition", SourceKey: second.SourceKey}, {Kind: "entry", SourceKey: secondEntry.RelativePath}})
	if err != nil {
		t.Fatal(err)
	}
	second.ID, secondEntry.ID = allocated[0], allocated[1]
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(second)}, Entries: []models.CatalogEntryFact{CatalogEntryFromLegacy(secondEntry, &second)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	c, token = catalogCandidate(t, store, library, "delta", 2)
	rec.MetadataJSON = collectionSnapshotMetadata(t, 101, 9001, "Partial refresh")
	if err := store.AppendBatch(context.Background(), c.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	page, err := state.CollectionItemPage(actor, legacy.ID, 1, 20)
	if err != nil || page.Total != 2 {
		t.Fatalf("partial dropped same-library unseen film: %+v %v", page, err)
	}
	c, token = catalogCandidate(t, store, library, "delta", 3)
	batch := CatalogFactBatch{}
	for _, entry := range entries {
		fact := CatalogEntryFromLegacy(entry, &rec)
		fact.Tombstone = true
		batch.Entries = append(batch.Entries, fact)
	}
	if err := store.AppendBatch(context.Background(), c.ID, token, batch); err != nil {
		t.Fatal(err)
	}
	catalogPublish(t, store, c, token)
	collections, err := state.CollectionPage(actor, "", "tmdb", 1, 20)
	if err != nil || collections.Total != 0 {
		t.Fatalf("removed film still supplied threshold: %+v %v", collections, err)
	}
	var tombstone models.CatalogCollectionMemberFact
	if err := store.writeDB.Where("snapshot_id=? AND work_key=?", c.ID, "movie:tmdb:101").First(&tombstone).Error; err != nil || !tombstone.Tombstone {
		t.Fatalf("projection removal absent: %+v %v", tombstone, err)
	}
}
