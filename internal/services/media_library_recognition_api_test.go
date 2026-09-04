package services

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/authz"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

func TestExistingLibraryLargeTVWorkUsesBoundedRecognitionEvidence(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	created, err := service.Create(context.Background(), actor, testLibraryInput("Large TV work", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var library models.MediaLibrary
	if err := db.First(&library, created.ID).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, episodeCount := range []int{1096, 3866} {
		t.Run(fmt.Sprintf("episodes_%d", episodeCount), func(t *testing.T) {
			files := make([]medialibrary.File, 0, episodeCount)
			entries := make([]models.MediaLibraryEntry, 0, episodeCount)
			for episode := 1; episode <= episodeCount; episode++ {
				relative := fmt.Sprintf("/哆啦A梦 (2005)/Season 01/哆啦A梦 %04d.mp4", episode)
				files = append(files, medialibrary.File{RelativePath: relative, ProviderID: fmt.Sprintf("provider-%04d", episode), ProviderIDStable: true, Size: int64(episode), ModifiedAt: now})
				entries = append(entries, models.MediaLibraryEntry{RelativePath: relative, ProviderID: fmt.Sprintf("provider-%04d", episode), MediaType: "tv", Size: int64(episode), ModifiedAt: now})
			}
			units := medialibrary.GroupRecognitionUnits(files)
			if len(units) != 1 || len(units[0].EvidenceFiles) > medialibrary.MaxRecognitionEvidenceFiles {
				t.Fatalf("units=%d evidence=%d", len(units), len(units[0].EvidenceFiles))
			}
			recognized, err := service.recognizeLibraryUnits(context.Background(), library, profile, units)
			if err != nil || len(recognized) != 1 || recognized[0].Result.ErrorCode == tmdb.ErrorInvalidRequest || recognized[0].Result.ErrorCode == mediaLibraryRecognitionInputInvalid || recognized[0].Result.Title != "哆啦A梦" || recognized[0].Result.ReleaseYear == nil || *recognized[0].Result.ReleaseYear != 2005 {
				t.Fatalf("background recognition=%+v err=%v", recognized, err)
			}
			retried, err := service.recognizeStoredUnit(context.Background(), library, profile, entries)
			if err != nil || retried.ErrorCode == tmdb.ErrorInvalidRequest || retried.ErrorCode == mediaLibraryRecognitionInputInvalid || retried.Title != "哆啦A梦" || retried.ReleaseYear == nil || *retried.ReleaseYear != 2005 {
				t.Fatalf("retry recognition=%+v err=%v", retried, err)
			}
		})
	}
}

func TestRetryRecognitionPersistsInvalidInferredTitleAsItemOutcome(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("Invalid recognition title", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := models.MediaLibraryRecognition{
		LibraryID: library.ID, SourceKey: "invalid-title", InputFingerprint: strings.Repeat("a", 64),
		ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusUnrecognized,
		ErrorCode: tmdb.ErrorInvalidRequest, MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{
		LibraryID: library.ID, RelativePath: `C:\private\bad.mkv`, ProviderID: "private-provider-id", RecognitionID: &record.ID,
		MatchStatus: mediaRecognitionStatusUnrecognized, RecognitionErrorCode: tmdb.ErrorInvalidRequest, ModifiedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}

	result, err := service.RetryRecognition(context.Background(), actor, library.ID, encodeRecognitionToken(record.ID), RequestContext{})
	if err != nil {
		t.Fatalf("retry returned an HTTP-level error: %v", err)
	}
	if result.Status != mediaRecognitionStatusUnrecognized || result.ErrorCode != mediaLibraryRecognitionInputInvalid || result.SourceDirectory != "private" {
		t.Fatalf("retry result=%+v", result)
	}
	var persisted models.MediaLibraryRecognition
	if err := db.First(&persisted, record.ID).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.ErrorCode != mediaLibraryRecognitionInputInvalid || persisted.Status != mediaRecognitionStatusUnrecognized {
		t.Fatalf("persisted result=%+v", persisted)
	}
}

func TestRecognitionSummaryExposesOnlySafeContainingDirectoryName(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("Safe recognition directory", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := models.MediaLibraryRecognition{
		LibraryID: library.ID, SourceKey: "safe-directory", InputFingerprint: strings.Repeat("b", 64),
		ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusUnrecognized,
		ErrorCode: tmdb.ErrorNoMatch, MediaType: "tv", MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	entry := models.MediaLibraryEntry{
		LibraryID: library.ID, RelativePath: "/电视剧/清晰剧名/Season 01/unknown.S01E01.mkv", ProviderID: "private-provider-id", RecognitionID: &record.ID,
		MediaType: "tv", MatchStatus: mediaRecognitionStatusUnrecognized, RecognitionErrorCode: tmdb.ErrorNoMatch, ModifiedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}

	page, err := service.Recognitions(actor, library.ID, MediaPageQuery{Page: 1, PageSize: 20}, mediaRecognitionStatusUnrecognized)
	if err != nil || len(page.List) != 1 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	item := page.List[0]
	if item.SourceDirectory != "清晰剧名" || strings.ContainsAny(item.SourceDirectory, "/\\") {
		t.Fatalf("unsafe directory display=%q", item.SourceDirectory)
	}
	payload, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(payload)
	for _, private := range []string{storage.RootPath, entry.RelativePath, entry.ProviderID, "provider_id"} {
		if strings.Contains(serialized, private) {
			t.Fatalf("private source identity leaked: %s", serialized)
		}
	}

	if _, err := service.RecognitionCandidates(context.Background(), actor, library.ID, item.Token, "", "", nil); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("empty manual query error=%v", err)
	}
	if _, err := service.RecognitionCandidates(context.Background(), actor, library.ID, item.Token, "C:\\private\\movie", "tv", nil); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("path-like manual query error=%v", err)
	}
	denied := Actor{User: actor.User, Permissions: map[string]struct{}{}}
	if _, err := service.RecognitionCandidates(context.Background(), denied, library.ID, item.Token, "清晰剧名", "tv", nil); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("candidate permission error=%v", err)
	}
	reader := Actor{User: actor.User, Permissions: map[string]struct{}{authz.PermissionMediaLibrariesRead: {}}}
	if _, err := service.RetryRecognition(context.Background(), reader, library.ID, item.Token, RequestContext{}); ErrorCode(err) != CodePermissionDenied {
		t.Fatalf("retry permission error=%v", err)
	}
}

func TestManualRecognitionSearchAndOverrideUseServerVerifiedTMDBIdentity(t *testing.T) {
	service, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := service.Create(context.Background(), actor, testLibraryInput("Manual recognition", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := models.MediaLibraryRecognition{
		LibraryID: library.ID, SourceKey: "manual-search", InputFingerprint: strings.Repeat("c", 64),
		ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: mediaRecognitionStatusUnrecognized,
		ErrorCode: tmdb.ErrorNoMatch, MediaType: "tv", MetadataJSON: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&record).Error; err != nil {
		t.Fatal(err)
	}
	season, episode := 1, 1
	entry := models.MediaLibraryEntry{
		LibraryID: library.ID, RelativePath: "/Wrong.Name/Season 01/episode.mkv", ProviderID: "manual-private-id", RecognitionID: &record.ID,
		MediaType: "tv", Season: &season, Episode: &episode, MatchStatus: mediaRecognitionStatusUnrecognized, RecognitionErrorCode: tmdb.ErrorNoMatch, ModifiedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}

	var searchQuery url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/search/tv":
			searchQuery = request.URL.Query()
			_, _ = response.Write([]byte(`{"results":[{"id":42,"name":"正确剧名","original_name":"Correct Show","original_language":"zh","first_air_date":"2024-01-01"}]}`))
		case "/tv/42":
			_, _ = response.Write([]byte(`{"id":42,"name":"正确剧名","original_name":"Correct Show","original_language":"zh","first_air_date":"2024-01-01","genres":[{"id":18}],"origin_country":["CN"]}`))
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

	year := 2024
	token := encodeRecognitionToken(record.ID)
	candidates, err := service.RecognitionCandidates(context.Background(), actor, library.ID, token, "正确剧名", "tv", &year)
	if err != nil || len(candidates) != 1 || candidates[0].ID != 42 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	if searchQuery.Get("query") != "正确剧名" || searchQuery.Get("first_air_date_year") != "2024" {
		t.Fatalf("manual query=%v", searchQuery)
	}
	result, err := service.OverrideRecognition(context.Background(), actor, library.ID, token, MediaRecognitionOverrideInput{TMDBID: candidates[0].ID, MediaType: candidates[0].MediaType}, RequestContext{})
	if err != nil || !result.ManualOverride || result.Status != mediaRecognitionStatusMatched || result.TMDBID == nil || *result.TMDBID != 42 || result.Title != "正确剧名" {
		t.Fatalf("override=%+v err=%v", result, err)
	}
	var persisted models.MediaLibraryRecognition
	if err := db.First(&persisted, record.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !persisted.ManualOverride || persisted.TMDBID == nil || *persisted.TMDBID != 42 {
		t.Fatalf("persisted override=%+v", persisted)
	}
}
