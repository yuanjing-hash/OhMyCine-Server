package services

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

func TestCatalogAccessPinnedServiceViewsUseEffectiveFacts(t *testing.T) {
	store, library, rec, entries := catalogFixture(t)
	service := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
	service.SetCatalogSnapshotStore(store)
	service.SetMetadataSettingsService(NewMetadataSettingsService(store.writeDB, NewAuditService(store.writeDB), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "test-token"}))
	actor := Actor{Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	if err := store.writeDB.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	metadata := func(title, poster string) string {
		value, err := marshalRecognitionMetadata(MediaRecognitionResult{Snapshot: tmdb.Snapshot{Version: 1, TMDBID: *rec.TMDBID, MediaType: "tv", Title: title, PosterPath: poster}})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	rec.MetadataJSON = metadata("Show", "/old.jpg")
	catalogConvert(t, store, library, rec, entries)
	legacyLibrary := models.MediaLibrary{Name: "Legacy", StorageID: library.StorageID, ProfileID: library.ProfileID, Enabled: true}
	if err := store.writeDB.Create(&legacyLibrary).Error; err != nil {
		t.Fatal(err)
	}
	legacy := models.MediaLibraryEntry{LibraryID: legacyLibrary.ID, RelativePath: "Other.mkv", ProviderID: "legacy", MediaType: "movie", Title: "Other", WorkKey: "movie:other", MatchStatus: "unrecognized"}
	if err := store.writeDB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}

	err := service.withCatalogReadTx(context.Background(), func(tx *gorm.DB) error {
		ids, err := service.authorizedMediaLibraryIDsTx(tx, actor, authz.PermissionMediaLibrariesRead, true)
		if err != nil {
			return err
		}
		reader, err := PinCatalogTx(tx, ids)
		if err != nil {
			return err
		}
		before, err := service.catalogDetailTx(tx, reader, actor, library.ID, encodeCatalogToken(entries[0].WorkKey))
		if err != nil {
			return err
		}
		if before.Work.Title != "Show" || before.Work.PosterURL != proxyDiscoveryImage("tmdb", "https://image.tmdb.org/t/p/w500/old.jpg") || before.Work.FileCount != 2 || len(before.Seasons) != 1 || before.Seasons[0].Number != 2 || len(before.Seasons[0].Episodes) != 2 {
			t.Fatalf("old snapshot/detail lost metadata or versions: %+v", before)
		}
		// Publish through the immediate writer while all subsequent service
		// reads retain this deferred snapshot, including metadata decoration.
		candidate, token := catalogCandidate(t, store, library, "delta", 1)
		rec.Title, rec.ManualOverride, rec.MetadataJSON = "Corrected", true, metadata("Corrected", "/new.jpg")
		if err := store.AppendBatch(context.Background(), candidate.ID, token, CatalogFactBatch{Recognitions: []models.CatalogRecognitionFact{CatalogRecognitionFromLegacy(rec)}}); err != nil {
			return err
		}
		catalogPublish(t, store, candidate, token)
		after, err := service.catalogDetailTx(tx, reader, actor, library.ID, encodeCatalogToken(entries[0].WorkKey))
		if err != nil {
			return err
		}
		if after.Work.Title != "Show" || after.Work.PosterURL != before.Work.PosterURL {
			t.Fatal("detail mixed old aggregate with newer recognition")
		}
		page, err := service.aggregateCatalogTx(tx, reader, actor, ids, MediaPageQuery{Page: 1, PageSize: 20})
		if err != nil {
			return err
		}
		if page.Total != 2 || len(page.List) != 2 {
			t.Fatalf("mixed aggregate lost a library: %+v", page)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.Catalog(actor, library.ID, MediaPageQuery{Query: "Corrected", Page: 1, PageSize: 20})
	if err != nil || page.Total != 1 || len(page.List) != 1 || page.List[0].FileCount != 2 || page.List[0].Title != "Corrected" || !page.List[0].ManualOverride || page.List[0].PosterURL != proxyDiscoveryImage("tmdb", "https://image.tmdb.org/t/p/w500/new.jpg") {
		t.Fatalf("new view not fully effective: %+v, %v", page, err)
	}
	old, err := service.Catalog(actor, library.ID, MediaPageQuery{Query: "Show", Page: 1, PageSize: 20})
	if err != nil || old.Total != 0 {
		t.Fatalf("old title resurrected: %+v, %v", old, err)
	}
	files, err := service.EntryPage(actor, library.ID, MediaPageQuery{Page: 1, PageSize: 20})
	if err != nil || files.Total != 2 || len(files.List) != 2 || *files.List[0].Season != 2 || *files.List[0].Episode != 1 {
		t.Fatalf("effective entry page: %+v, %v", files, err)
	}
	tokens, err := service.catalogRecognitionTokens(actor, library.ID, encodeCatalogToken(entries[0].WorkKey))
	if err != nil || len(tokens) != 1 || tokens[0] != encodeRecognitionToken(rec.ID) {
		t.Fatalf("effective recognition tokens: %v, %v", tokens, err)
	}
	service.SetCatalogSnapshotStore(nil)
	if _, err := service.Catalog(actor, library.ID, MediaPageQuery{}); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("versioned no-store must fail closed, got %v", err)
	}
}

func TestCatalogAccessAuthorizedScopeDoesNotSilentlyTruncate(t *testing.T) {
	service, library, actor := createCatalogTestLibrary(t)
	items := make([]models.MediaLibrary, 2001)
	for i := range items {
		items[i] = models.MediaLibrary{Name: fmt.Sprintf("scope-%d", i), NameNormalized: fmt.Sprintf("scope-%d", i), StorageID: library.StorageID, ProfileID: library.ProfileID, Enabled: true}
	}
	if err := service.db.CreateInBatches(items, 100).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.AggregateCatalog(actor, MediaPageQuery{}); !errors.Is(err, ErrCatalogBudget) {
		t.Fatalf("large authorized scope must return explicit budget error: %v", err)
	}
}
