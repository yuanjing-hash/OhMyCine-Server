package services

import (
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

func TestMediaMetadataEditorPreservesIdentityAndValidatesImages(t *testing.T) {
	current := tmdb.Snapshot{Version: 1, TMDBID: 346, IMDbID: "tt0047478", MediaType: "movie", Title: "七武士", PosterPath: "/old.jpg", PosterPaths: []string{"/old.jpg", "/new.jpg"}, BackdropPath: "/back.jpg", BackdropPaths: []string{"/back.jpg", "/new-back.jpg"}}
	input := editableFromSnapshot(current)
	input.Title = "七武士（修订）"
	input.ReleaseDate = "1954-04-26"
	input.VoteAverage = 9.1
	input.PosterPath = "/new.jpg"
	input.BackdropPath = "/new-back.jpg"
	edited, err := (MediaMetadataEditor{}).Apply(current, input)
	if err != nil {
		t.Fatal(err)
	}
	if edited.TMDBID != current.TMDBID || edited.MediaType != current.MediaType || edited.IMDbID != current.IMDbID {
		t.Fatalf("identity changed: %+v", edited)
	}
	if edited.Title != input.Title || edited.PosterPath != "/new.jpg" || edited.VoteAverage != 9.1 {
		t.Fatalf("editable fields not applied: %+v", edited)
	}
	input.PosterPath = "https://example.invalid/poster.jpg"
	if _, err := (MediaMetadataEditor{}).Apply(current, input); err == nil {
		t.Fatal("arbitrary image URL accepted")
	}
}

func TestPersistCatalogMetadataResultsIsAtomicAcrossRecognitions(t *testing.T) {
	service, library, _ := createCatalogTestLibrary(t)
	if err := service.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Updates(map[string]any{"metadata_artifacts_enabled": true, "dirty_generation": 4}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.db.First(&library, library.ID).Error; err != nil {
		t.Fatal(err)
	}
	queue := NewQueueService(service.db, NewAuditService(service.db))
	service.SetArtifactService(NewMediaArtifactService(service.db, queue, nil, zerolog.Nop()))
	service.SetMediaChangeService(NewMediaChangeService(service.db))

	var profile models.MediaClassificationProfile
	if err := service.db.First(&profile, library.ProfileID).Error; err != nil {
		t.Fatal(err)
	}
	tmdbID, confidence, seasonOne, seasonTwo := int64(100), 0.99, 1, 2
	base := func(title string) MediaRecognitionResult {
		return MediaRecognitionResult{
			Status: mediaRecognitionStatusMatched, Title: title, MediaType: "tv", TMDBID: &tmdbID,
			Confidence: &confidence, CategoryName: "电视剧",
			Metadata: classification.Metadata{MediaType: classification.MediaType("tv"), OriginalLanguage: "zh"},
			Snapshot: tmdb.Snapshot{Version: 1, TMDBID: tmdbID, MediaType: "tv", Title: title, OriginalLanguage: "zh", SeasonCount: 2},
		}
	}
	metadataJSON, err := marshalRecognitionMetadata(base("Original"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	records := []models.MediaLibraryRecognition{
		{LibraryID: library.ID, SourceKey: "season-one", InputFingerprint: "fingerprint-one", ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, MediaType: "tv", Title: "Original", TMDBID: &tmdbID, Confidence: &confidence, CategoryName: "电视剧", MetadataJSON: metadataJSON, LastGeneration: 4, CreatedAt: now, UpdatedAt: now},
		{LibraryID: library.ID, SourceKey: "season-two", InputFingerprint: "fingerprint-two", ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusMatched, MediaType: "tv", Title: "Original", TMDBID: &tmdbID, Confidence: &confidence, CategoryName: "电视剧", MetadataJSON: metadataJSON, LastGeneration: 4, CreatedAt: now, UpdatedAt: now},
	}
	if err := service.db.Create(&records).Error; err != nil {
		t.Fatal(err)
	}
	entries := []models.MediaLibraryEntry{
		{LibraryID: library.ID, RelativePath: "/电视剧/Original/Season 01/E01.mkv", ProviderID: "episode-one", RecognitionID: &records[0].ID, Size: 1, ModifiedAt: now, MediaType: "tv", Title: "Original", SeriesTitle: "Original", WorkKey: "series:tmdb:100", Season: &seasonOne, MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, MatchConfidence: &confidence, CategoryName: "电视剧", LastGeneration: 4, CreatedAt: now, UpdatedAt: now},
		{LibraryID: library.ID, RelativePath: "/电视剧/Original/Season 02/E01.mkv", ProviderID: "episode-two", RecognitionID: &records[1].ID, Size: 1, ModifiedAt: now, MediaType: "tv", Title: "Original", SeriesTitle: "Original", WorkKey: "series:tmdb:100", Season: &seasonTwo, MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, MatchConfidence: &confidence, CategoryName: "电视剧", LastGeneration: 4, CreatedAt: now, UpdatedAt: now},
	}
	if err := service.db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}
	edited := base("Edited Together")
	updates := []catalogMetadataResult{{Record: records[0], Profile: profile, Result: edited}, {Record: records[1], Profile: profile, Result: edited}}
	if err := service.persistCatalogMetadataResults(updates); err != nil {
		t.Fatal(err)
	}
	var stored []models.MediaLibraryRecognition
	if err := service.db.Where("id IN ?", []uint{records[0].ID, records[1].ID}).Order("id").Find(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Title != "Edited Together" || stored[1].Title != "Edited Together" || !stored[0].ManualOverride || !stored[1].ManualOverride {
		t.Fatalf("recognitions were not updated together: %+v", stored)
	}
	var editedEntries int64
	if err := service.db.Model(&models.MediaLibraryEntry{}).Where("library_id = ? AND title = ? AND series_title = ?", library.ID, "Edited Together", "Edited Together").Count(&editedEntries).Error; err != nil || editedEntries != 2 {
		t.Fatalf("entry updates=%d err=%v", editedEntries, err)
	}
	var runCount, changeCount int64
	if err := service.db.Model(&models.MediaArtifactRun{}).Where("library_id = ?", library.ID).Count(&runCount).Error; err != nil || runCount != 1 {
		t.Fatalf("artifact runs=%d err=%v", runCount, err)
	}
	if err := service.db.Model(&models.MediaLibraryChange{}).Where("library_id = ?", library.ID).Count(&changeCount).Error; err != nil || changeCount != 1 {
		t.Fatalf("media changes=%d err=%v", changeCount, err)
	}

	staleUpdates := []catalogMetadataResult{{Record: stored[0], Profile: profile, Result: base("Must Roll Back")}, {Record: stored[1], Profile: profile, Result: base("Must Roll Back")}}
	if err := service.db.Model(&models.MediaLibraryRecognition{}).Where("id = ?", stored[1].ID).Update("updated_at", time.Now().UTC().Add(time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.persistCatalogMetadataResults(staleUpdates); ErrorCode(err) != CodeConflict {
		t.Fatalf("stale save err=%v", err)
	}
	var rolledBack []models.MediaLibraryRecognition
	if err := service.db.Where("id IN ?", []uint{records[0].ID, records[1].ID}).Order("id").Find(&rolledBack).Error; err != nil {
		t.Fatal(err)
	}
	if rolledBack[0].Title != "Edited Together" || rolledBack[1].Title != "Edited Together" {
		t.Fatalf("partial metadata update escaped rollback: %+v", rolledBack)
	}
	if err := service.db.Model(&models.MediaArtifactRun{}).Where("library_id = ?", library.ID).Count(&runCount).Error; err != nil || runCount != 1 {
		t.Fatalf("stale save scheduled artifacts: count=%d err=%v", runCount, err)
	}
}
