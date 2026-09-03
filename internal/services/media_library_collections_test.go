package services

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func TestFirstSuccessfulScanCreatesTMDBCollection(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	for _, name := range []string{"Alpha.Movie.2020.mkv", "Beta.Movie.2021.mkv"} {
		if err := os.WriteFile(filepath.Join(storage.RootPath, name), []byte("media"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/search/movie":
			if strings.Contains(request.URL.Query().Get("query"), "Alpha") {
				_, _ = response.Write([]byte(`{"results":[{"id":301,"title":"Alpha Movie","original_language":"en","release_date":"2020-01-01"}]}`))
			} else {
				_, _ = response.Write([]byte(`{"results":[{"id":302,"title":"Beta Movie","original_language":"en","release_date":"2021-01-01"}]}`))
			}
		case "/movie/301":
			_, _ = response.Write([]byte(`{"id":301,"title":"Alpha Movie","original_language":"en","release_date":"2020-01-01","belongs_to_collection":{"id":9300,"name":"First Scan Saga","poster_path":"/saga.jpg"}}`))
		case "/movie/302":
			_, _ = response.Write([]byte(`{"id":302,"title":"Beta Movie","original_language":"en","release_date":"2021-01-01","belongs_to_collection":{"id":9300,"name":"First Scan Saga","poster_path":"/saga.jpg"}}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer upstream.Close()
	client, err := tmdb.NewForTest("test-token", upstream.URL, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	metadata := NewMetadataSettingsService(db, NewAuditService(db), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "deployment-token"})
	metadata.clientFactory = func(tmdb.Credential, string, string) (*tmdb.Client, error) { return client, nil }
	service.SetMetadataSettingsService(metadata)
	library, err := service.Create(context.Background(), actor, testLibraryInput("First scan collection", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := service.ScanNow(context.Background(), actor, library.ID)
	if err != nil || run.Matched != 2 {
		t.Fatalf("first scan=%+v err=%v", run, err)
	}
	var automatic models.PlayerMediaCollection
	if err := db.Where("source = ? AND tmdb_collection_id = ?", models.PlayerMediaCollectionSourceTMDB, 9300).First(&automatic).Error; err != nil {
		t.Fatal(err)
	}
	var members int64
	if err := db.Model(&models.PlayerMediaCollectionItem{}).Where("collection_id = ? AND origin = ?", automatic.ID, models.PlayerMediaCollectionItemOriginTMDB).Count(&members).Error; err != nil || members != 2 || !automatic.Visible {
		t.Fatalf("automatic=%+v members=%d err=%v", automatic, members, err)
	}
}

func TestTMDBCollectionsFollowCompleteAndPartialScanLifecycle(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("Collection library", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	collection := &tmdb.Collection{TMDBID: 9001, Name: "Example Saga", PosterPath: "/collection.jpg"}
	createRecognition := func(source string, movieID int64, title string) models.MediaLibraryRecognition {
		t.Helper()
		metadata, marshalErr := marshalRecognitionMetadata(MediaRecognitionResult{
			Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: title, TMDBID: &movieID,
			Metadata: classification.Metadata{MediaType: classification.MediaTypeMovie},
			Snapshot: tmdb.Snapshot{Version: 1, TMDBID: movieID, MediaType: "movie", Title: title, Collection: collection},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		row := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: source, InputFingerprint: fmt.Sprintf("%064d", movieID), ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: title, TMDBID: &movieID, MetadataJSON: metadata, LastGeneration: 1, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		return row
	}
	first := createRecognition("first", 101, "First")
	second := createRecognition("second", 102, "Second")
	entries := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RelativePath: "First.mkv", RecognitionID: &first.ID, Size: 1, ModifiedAt: now, MediaType: "movie", Title: "First", WorkKey: "movie:tmdb:101", MatchStatus: mediaRecognitionStatusMatched, TMDBID: first.TMDBID, LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
		{LibraryID: library.ID, RelativePath: "First.4K.mkv", RecognitionID: &first.ID, Size: 1, ModifiedAt: now, MediaType: "movie", Title: "First 4K", WorkKey: "movie:tmdb:101", MatchStatus: mediaRecognitionStatusMatched, TMDBID: first.TMDBID, LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
		{LibraryID: library.ID, RelativePath: "Second.mkv", RecognitionID: &second.ID, Size: 1, ModifiedAt: now, MediaType: "movie", Title: "Second", WorkKey: "movie:tmdb:102", MatchStatus: mediaRecognitionStatusMatched, TMDBID: second.TMDBID, LastGeneration: 1, CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	manual := models.PlayerMediaCollection{ID: uuid.NewString(), OwnerID: &actor.User.ID, Source: models.PlayerMediaCollectionSourceManual, Kind: models.PlayerMediaCollectionKindCollection, Name: "My list", Visible: true, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&manual).Error; err != nil {
		t.Fatal(err)
	}
	manualItem := models.PlayerMediaCollectionItem{CollectionID: manual.ID, LibraryID: library.ID, WorkKey: "movie:tmdb:101", Origin: models.PlayerMediaCollectionItemOriginManual, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&manualItem).Error; err != nil {
		t.Fatal(err)
	}

	if err := db.Transaction(func(tx *gorm.DB) error { return reconcileTMDBCollectionsTx(tx, library.ID, false, now) }); err != nil {
		t.Fatal(err)
	}
	var automatic models.PlayerMediaCollection
	if err := db.Where("source = ? AND tmdb_collection_id = ?", models.PlayerMediaCollectionSourceTMDB, 9001).First(&automatic).Error; err != nil {
		t.Fatal(err)
	}
	if !automatic.Visible || automatic.Name != "Example Saga" {
		t.Fatalf("automatic=%+v", automatic)
	}
	var automaticCount int64
	if err := db.Model(&models.PlayerMediaCollectionItem{}).Where("collection_id = ? AND origin = ?", automatic.ID, models.PlayerMediaCollectionItemOriginTMDB).Count(&automaticCount).Error; err != nil || automaticCount != 2 {
		t.Fatalf("automatic members=%d err=%v", automaticCount, err)
	}

	if err := db.Where("library_id = ? AND work_key = ?", library.ID, "movie:tmdb:102").Delete(&models.MediaLibraryEntry{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { return reconcileTMDBCollectionsTx(tx, library.ID, true, now.Add(time.Minute)) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.PlayerMediaCollectionItem{}).Where("collection_id = ? AND origin = ?", automatic.ID, models.PlayerMediaCollectionItemOriginTMDB).Count(&automaticCount).Error; err != nil || automaticCount != 2 {
		t.Fatalf("partial scan removed members: count=%d err=%v", automaticCount, err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return reconcileTMDBCollectionsTx(tx, library.ID, false, now.Add(2*time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.PlayerMediaCollectionItem{}).Where("collection_id = ? AND origin = ?", automatic.ID, models.PlayerMediaCollectionItemOriginTMDB).Count(&automaticCount).Error; err != nil || automaticCount != 1 {
		t.Fatalf("complete scan members=%d err=%v", automaticCount, err)
	}
	if err := db.First(&automatic, "id = ?", automatic.ID).Error; err != nil || automatic.Visible {
		t.Fatalf("single-film collection visibility=%v err=%v", automatic.Visible, err)
	}
	var manualCount int64
	if err := db.Model(&models.PlayerMediaCollectionItem{}).Where("collection_id = ? AND origin = ?", manual.ID, models.PlayerMediaCollectionItemOriginManual).Count(&manualCount).Error; err != nil || manualCount != 1 {
		t.Fatalf("manual collection changed: count=%d err=%v", manualCount, err)
	}
}

func TestPlayerFavoritesAndManualCollectionsAreUserScoped(t *testing.T) {
	service, db, owner, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), owner, testLibraryInput("User state library", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	movieID := int64(201)
	metadata, err := marshalRecognitionMetadata(MediaRecognitionResult{Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "Favorite", TMDBID: &movieID, Metadata: classification.Metadata{MediaType: classification.MediaTypeMovie}, Snapshot: tmdb.Snapshot{Version: 1, TMDBID: movieID, MediaType: "movie", Title: "Favorite"}})
	if err != nil {
		t.Fatal(err)
	}
	recognition := models.MediaLibraryRecognition{LibraryID: library.ID, SourceKey: "favorite", InputFingerprint: fmt.Sprintf("%064d", movieID), ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, MediaType: "movie", Title: "Favorite", TMDBID: &movieID, MetadataJSON: metadata, LastGeneration: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&recognition).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "Favorite.mkv", RecognitionID: &recognition.ID, Size: 1, ModifiedAt: now, MediaType: "movie", Title: "Favorite", WorkKey: "movie:tmdb:201", MatchStatus: mediaRecognitionStatusMatched, TMDBID: &movieID, LastGeneration: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	foreignUser := models.User{Username: "foreign-state", UsernameNormalized: "foreign-state", DisplayName: "Foreign", PasswordHash: "unused", Status: models.UserStatusActive}
	if err := db.Create(&foreignUser).Error; err != nil {
		t.Fatal(err)
	}
	foreign := Actor{User: foreignUser, Permissions: owner.Permissions}
	state := NewPlayerMediaStateService(db, service)
	itemID := playerMediaStateItemID(library.ID, entry.WorkKey)
	if favorite, err := state.SetFavorite(owner, itemID, true); err != nil || !favorite {
		t.Fatalf("set favorite=%v err=%v", favorite, err)
	}
	if favorite, err := state.FavoriteState(foreign, itemID); err != nil || favorite {
		t.Fatalf("foreign favorite=%v err=%v", favorite, err)
	}
	if items, err := state.Favorites(owner); err != nil || len(items) != 1 || items[0].Title != "Favorite" {
		t.Fatalf("owner favorites=%+v err=%v", items, err)
	}
	collection, err := state.CreateCollection(owner, "My Collection", models.PlayerMediaCollectionKindCollection)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.AddCollectionItem(owner, collection.ID, itemID); err != nil {
		t.Fatal(err)
	}
	if summaries, err := state.Collections(owner, models.PlayerMediaCollectionKindCollection); err != nil || len(summaries) != 1 || summaries[0].ItemCount != 1 {
		t.Fatalf("owner collections=%+v err=%v", summaries, err)
	}
	if summaries, err := state.Collections(foreign, models.PlayerMediaCollectionKindCollection); err != nil || len(summaries) != 0 {
		t.Fatalf("foreign collections=%+v err=%v", summaries, err)
	}
	if err := state.AddCollectionItem(foreign, collection.ID, itemID); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("foreign collection mutation error=%v", err)
	}
}
