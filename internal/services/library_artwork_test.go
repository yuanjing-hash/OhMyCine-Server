package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

func TestLibraryArtworkGeneratorComposesAndCachesStableCover(t *testing.T) {
	service := &LibraryArtworkService{cache: make(map[string][]byte), generation: make(map[string]string)}
	candidates := []artworkCandidate{
		{key: "movie:one", load: solidArtworkLoader(color.RGBA{R: 220, G: 40, B: 40, A: 255})},
		{key: "movie:two", load: solidArtworkLoader(color.RGBA{R: 40, G: 90, B: 220, A: 255})},
	}

	first, err := service.generate(context.Background(), "测试媒体库", candidates)
	if err != nil {
		t.Fatal(err)
	}
	secondService := &LibraryArtworkService{cache: make(map[string][]byte), generation: make(map[string]string)}
	second, err := secondService.generate(context.Background(), "测试媒体库", []artworkCandidate{candidates[1], candidates[0]})
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest || !bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatalf("stable input produced different artwork: %s != %s", first.Digest, second.Digest)
	}
	contentHash := sha256.Sum256(first.Bytes)
	if got := hex.EncodeToString(contentHash[:]); first.Digest != got {
		t.Fatalf("public digest=%s does not identify encoded bytes=%s", first.Digest, got)
	}
	decoded, _, err := image.Decode(bytes.NewReader(first.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.Bounds().Size(); got.X != libraryArtworkWidth || got.Y != libraryArtworkHeight {
		t.Fatalf("cover size=%v", got)
	}
	opened, err := service.Open(first.Digest)
	if err != nil || !bytes.Equal(opened.Bytes, first.Bytes) {
		t.Fatalf("cached artwork unavailable: err=%v", err)
	}
	parsed, err := url.Parse(service.artworkURL(first.Digest))
	if err != nil || parsed.RawQuery != "" || !strings.HasPrefix(parsed.Path, "/api/v1/assets/generated-library-covers/") {
		t.Fatalf("content-addressed artwork URL invalid: url=%q err=%v", parsed, err)
	}
}

func TestMediaCategoryArtworkPersistsAcrossRestartAndRetainsActiveVersionOnFailure(t *testing.T) {
	mediaLibraries, db, actor, storage, profile := mediaLibraryTestService(t)
	library, err := mediaLibraries.Create(context.Background(), actor, testLibraryInput("Artwork library", storage, profile, false), RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	loads := 0
	candidates := func(uint, string, string) ([]artworkCandidate, error) {
		return []artworkCandidate{{key: "tmdb:/stable-poster.jpg", load: func(ctx context.Context) ([]byte, error) {
			loads++
			return solidArtworkLoader(color.RGBA{R: 30, G: 100, B: 210, A: 255})(ctx)
		}}}, nil
	}
	service := NewLibraryArtworkService(db, nil, nil, nil, zerolog.Nop(), WithLibraryArtworkRoot(root))
	service.categoryCandidates = candidates
	categoryKey := mediaCategoryArtworkKey("movie", "电影")
	if err := service.reconcileCategory(context.Background(), library.ID, categoryKey, "电影", "movie"); err != nil {
		t.Fatal(err)
	}
	var first models.MediaCategoryArtwork
	if err := db.Where("library_id = ? AND category_key = ?", library.ID, categoryKey).First(&first).Error; err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || first.Status != "ready" || first.ContentHash == "" || first.RelativePath == "" || loads != 1 {
		t.Fatalf("initial artwork=%+v loads=%d", first, loads)
	}
	firstInfo, err := os.Stat(filepath.Join(root, first.RelativePath))
	if err != nil {
		t.Fatal(err)
	}

	restarted := NewLibraryArtworkService(db, nil, nil, nil, zerolog.Nop(), WithLibraryArtworkRoot(root))
	restarted.categoryCandidates = candidates
	if err := restarted.reconcileCategory(context.Background(), library.ID, categoryKey, "电影", "movie"); err != nil {
		t.Fatal(err)
	}
	var unchanged models.MediaCategoryArtwork
	if err := db.First(&unchanged, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(filepath.Join(root, unchanged.RelativePath))
	if err != nil {
		t.Fatal(err)
	}
	if loads != 1 || unchanged.Revision != first.Revision || unchanged.ContentHash != first.ContentHash || !secondInfo.ModTime().Equal(firstInfo.ModTime()) {
		t.Fatalf("unchanged generation rewrote artwork: loads=%d first=%+v unchanged=%+v mtimes=%s/%s", loads, first, unchanged, firstInfo.ModTime(), secondInfo.ModTime())
	}
	opened, err := restarted.Open(first.ContentHash)
	if err != nil || opened.Digest != first.ContentHash || len(opened.Bytes) == 0 {
		t.Fatalf("restart open failed: asset=%+v err=%v", opened, err)
	}
	decorated := restarted.DecorateMediaCategories(context.Background(), library.ID, []PlayerMediaCategory{{ID: categoryKey, Name: "电影", MediaType: "movie", ArtworkURL: "/fallback.png"}})
	if len(decorated) != 1 || decorated[0].ArtworkSource != "generated" || decorated[0].ArtworkURL != restarted.artworkURL(first.ContentHash) {
		t.Fatalf("decorated category=%+v", decorated)
	}

	restarted.categoryCandidates = func(uint, string, string) ([]artworkCandidate, error) {
		return []artworkCandidate{{key: "tmdb:/changed-poster.jpg", load: func(context.Context) ([]byte, error) {
			return nil, errors.New("upstream unavailable")
		}}}, nil
	}
	if err := restarted.reconcileCategory(context.Background(), library.ID, categoryKey, "电影", "movie"); err == nil {
		t.Fatal("changed unavailable candidate unexpectedly succeeded")
	}
	var failed models.MediaCategoryArtwork
	if err := db.First(&failed, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.ContentHash != first.ContentHash || failed.Revision != first.Revision || failed.LastErrorCode != libraryArtworkErrorGenerate {
		t.Fatalf("failed generation did not preserve active artwork: %+v", failed)
	}
	decorated = restarted.DecorateMediaCategories(context.Background(), library.ID, []PlayerMediaCategory{{ID: categoryKey, Name: "电影", MediaType: "movie", ArtworkURL: "/fallback.png"}})
	if decorated[0].ArtworkSource != "generated" || decorated[0].ArtworkURL != restarted.artworkURL(first.ContentHash) {
		t.Fatalf("failed generation hid previous artwork: %+v", decorated[0])
	}
}

func solidArtworkLoader(fill color.RGBA) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) {
		canvas := image.NewRGBA(image.Rect(0, 0, 300, 450))
		for y := 0; y < canvas.Bounds().Dy(); y++ {
			for x := 0; x < canvas.Bounds().Dx(); x++ {
				canvas.SetRGBA(x, y, fill)
			}
		}
		var output bytes.Buffer
		err := jpeg.Encode(&output, canvas, &jpeg.Options{Quality: 90})
		return output.Bytes(), err
	}
}

func TestLibraryArtworkStyleStatic3OrderAndGradientMatchReference(t *testing.T) {
	posters := make([]image.Image, 9)
	for index := range posters {
		poster := image.NewRGBA(image.Rect(0, 0, 1, 1))
		poster.SetRGBA(0, 0, color.RGBA{R: uint8(index + 1), A: 255})
		posters[index] = poster
	}
	ordered := style3PosterOrder(posters)
	wantOrder := []uint8{3, 1, 5, 4, 2, 6, 9, 8, 7}
	for index, want := range wantOrder {
		got, _, _, _ := ordered[index].At(0, 0).RGBA()
		if uint8(got>>8) != want {
			t.Fatalf("poster order at %d=%d, want %d", index, got>>8, want)
		}
	}

	gradient := style3Gradient(color.RGBA{R: 100, G: 120, B: 140, A: 255})
	if got, want := gradient.RGBAAt(0, 0), (color.RGBA{R: 65, G: 78, B: 91, A: 255}); got != want {
		t.Fatalf("style 3 left gradient=%v, want %v", got, want)
	}
	if got, want := gradient.RGBAAt(libraryArtworkWidth-1, 0), (color.RGBA{R: 145, G: 158, B: 172, A: 255}); got != want {
		t.Fatalf("style 3 right gradient=%v, want %v", got, want)
	}
}

func TestLibraryArtworkCandidatesAreIsolatedByLibraryCategoryAndMediaType(t *testing.T) {
	db, _, _, _ := newConnectionTestService(t, &fakeCloudDriver{})
	now := time.Now().UTC()
	storage := models.Storage{
		Name: "Artwork storage", NameNormalized: "artwork-storage", Type: models.StorageTypeLocal,
		RootPath: t.TempDir(), RootPathNormalized: "artwork-storage-root", Enabled: true, Capabilities: `{}`,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&storage).Error; err != nil {
		t.Fatal(err)
	}
	var profile models.MediaClassificationProfile
	if err := db.Where("code = ?", "default-v1").First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	library := models.MediaLibrary{
		Name: "Artwork library", NameNormalized: "artwork-library", StorageID: storage.ID,
		ProfileID: profile.ID, ProfileRevision: profile.Revision, RelativeRoot: "/", Enabled: true,
		Recursive: true, VideoExtensionsJSON: `[".mkv"]`, IgnorePatternsJSON: `[]`,
		Status: models.MediaLibraryStatusListening, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&library).Error; err != nil {
		t.Fatal(err)
	}

	createRecognition := func(sourceKey, title, poster, status string, updatedAt time.Time) models.MediaLibraryRecognition {
		t.Helper()
		metadataJSON, err := marshalRecognitionMetadata(MediaRecognitionResult{
			Metadata: classification.Metadata{MediaType: classification.MediaTypeMovie},
			Snapshot: tmdb.Snapshot{Version: 1, TMDBID: int64(len(sourceKey)), MediaType: "movie", Title: title, PosterPath: poster},
		})
		if err != nil {
			t.Fatal(err)
		}
		tmdbID, confidence := int64(len(sourceKey)), .99
		recognition := models.MediaLibraryRecognition{
			LibraryID: library.ID, SourceKey: sourceKey, InputFingerprint: strings.Repeat(sourceKey[:1], 64),
			ProfileID: profile.ID, ProfileRevision: profile.Revision, Status: status,
			MediaType: "movie", Title: title, TMDBID: &tmdbID, Confidence: &confidence,
			MetadataJSON: metadataJSON, LastGeneration: 1, CreatedAt: updatedAt, UpdatedAt: updatedAt,
		}
		if status == mediaRecognitionStatusUnrecognized {
			recognition.ErrorCode = mediaRecognitionLowConfidence
		}
		if err := db.Create(&recognition).Error; err != nil {
			t.Fatal(err)
		}
		return recognition
	}

	series := createRecognition("series", "Series", "/series.jpg", mediaRecognitionStatusUnrecognized, now)
	movie := createRecognition("movie", "Movie", "/movie.jpg", mediaRecognitionStatusMatched, now.Add(-time.Minute))
	entries := make([]models.MediaLibraryEntry, 0, 81)
	for index := 0; index < 80; index++ {
		recognitionID := series.ID
		entries = append(entries, models.MediaLibraryEntry{
			LibraryID: library.ID, RelativePath: fmt.Sprintf("/Series/S01E%03d.mkv", index+1),
			RecognitionID: &recognitionID, ModifiedAt: now, MediaType: "tv", Title: "Series",
			MatchStatus: mediaRecognitionStatusMatched, CategoryName: "Series", LastGeneration: 1,
			CreatedAt: now, UpdatedAt: now,
		})
	}
	movieRecognitionID := movie.ID
	entries = append(entries, models.MediaLibraryEntry{
		LibraryID: library.ID, RelativePath: "/Movies/Movie.mkv", RecognitionID: &movieRecognitionID,
		ModifiedAt: now, MediaType: "movie", Title: "Movie", MatchStatus: mediaRecognitionStatusMatched,
		CategoryName: "Movies", LastGeneration: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err := db.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}

	metadata := NewMetadataSettingsService(db, NewAuditService(db), nil, tmdb.Credential{Kind: tmdb.CredentialKindReadAccessToken, Value: "deployment-token"})
	service := NewLibraryArtworkService(db, metadata, nil, nil, zerolog.Nop())
	candidates, err := service.mediaCategoryCandidates(library.ID, "Series", "series")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].key != "tmdb:/series.jpg" {
		t.Fatalf("series candidates=%+v", candidates)
	}
	candidates, err = service.mediaCategoryCandidates(library.ID, "Movies", "movie")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].key != "tmdb:/movie.jpg" {
		t.Fatalf("movie candidates=%+v", candidates)
	}
	candidates, err = service.mediaCategoryCandidates(library.ID, "Series", "movie")
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("cross-kind candidates=%+v", candidates)
	}
}

func TestFillStyle3PosterSlotsCompletesReferenceGridForSparseCandidates(t *testing.T) {
	for _, candidateCount := range []int{1, 2, 3, 4, 8, 9} {
		t.Run(strconv.Itoa(candidateCount), func(t *testing.T) {
			candidates := make([]image.Image, candidateCount)
			for index := range candidates {
				poster := image.NewRGBA(image.Rect(0, 0, 1, 1))
				poster.SetRGBA(0, 0, color.RGBA{R: uint8(index + 1), A: 255})
				candidates[index] = poster
			}
			filled := fillStyle3PosterSlots(candidates, libraryArtworkRenderLimit)
			if len(filled) != libraryArtworkRenderLimit {
				t.Fatalf("filled slots=%d, want %d", len(filled), libraryArtworkRenderLimit)
			}
			for index, poster := range filled {
				red, _, _, _ := poster.At(0, 0).RGBA()
				if got, want := uint8(red>>8), uint8(index%candidateCount+1); got != want {
					t.Fatalf("slot %d=%d, want %d", index, got, want)
				}
			}
		})
	}
}

func TestRenderLibraryArtworkSparseCandidatesOccupyCompleteStyle3Grid(t *testing.T) {
	var expectedBrightPixels int
	for _, candidateCount := range []int{1, 2, 3, 4, 8, 9} {
		t.Run(strconv.Itoa(candidateCount), func(t *testing.T) {
			candidates := make([]image.Image, candidateCount)
			for index := range candidates {
				poster := image.NewRGBA(image.Rect(0, 0, 30, 45))
				for y := 0; y < poster.Bounds().Dy(); y++ {
					for x := 0; x < poster.Bounds().Dx(); x++ {
						poster.SetRGBA(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
					}
				}
				candidates[index] = poster
			}
			rendered := renderLibraryArtwork(candidates)
			brightPixels := 0
			for y := 0; y < rendered.Bounds().Dy(); y++ {
				for x := 0; x < rendered.Bounds().Dx(); x++ {
					pixel := rendered.RGBAAt(x, y)
					if pixel.R >= 250 && pixel.G >= 250 && pixel.B >= 250 {
						brightPixels++
					}
				}
			}
			if brightPixels < 200_000 {
				t.Fatalf("sparse style 3 foreground coverage=%d pixels", brightPixels)
			}
			if expectedBrightPixels == 0 {
				expectedBrightPixels = brightPixels
			} else if brightPixels != expectedBrightPixels {
				t.Fatalf("sparse style 3 coverage=%d, want %d", brightPixels, expectedBrightPixels)
			}
		})
	}
}

func TestLibraryArtworkGeneratorRejectsDecodedImageBombDimensions(t *testing.T) {
	service := &LibraryArtworkService{cache: make(map[string][]byte), generation: make(map[string]string)}
	oversized := func(context.Context) ([]byte, error) {
		canvas := image.NewRGBA(image.Rect(0, 0, libraryArtworkMaxDimension+1, 32))
		var output bytes.Buffer
		err := png.Encode(&output, canvas)
		return output.Bytes(), err
	}
	if _, err := service.generate(context.Background(), "Oversized", []artworkCandidate{{key: "oversized", load: oversized}}); err == nil {
		t.Fatal("oversized decoded image was accepted")
	}
}

func TestLibraryArtworkGeneratorEvictsContentAndGenerationMappingTogether(t *testing.T) {
	service := &LibraryArtworkService{
		cache:      make(map[string][]byte, libraryArtworkCacheLimit),
		generation: make(map[string]string, libraryArtworkCacheLimit),
		order:      make([]string, 0, libraryArtworkCacheLimit),
	}
	for index := 0; index < libraryArtworkCacheLimit; index++ {
		digest := fmt.Sprintf("%064x", index+1)
		service.cache[digest] = []byte{byte(index)}
		service.generation[fmt.Sprintf("generation-%d", index)] = digest
		service.order = append(service.order, digest)
	}
	evicted := service.order[0]
	if _, err := service.generate(context.Background(), "New", []artworkCandidate{{
		key: "new", load: solidArtworkLoader(color.RGBA{R: 10, G: 20, B: 30, A: 255}),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, exists := service.cache[evicted]; exists {
		t.Fatal("evicted artwork bytes remain cached")
	}
	if _, exists := service.generation["generation-0"]; exists {
		t.Fatal("generation mapping outlived its evicted artwork")
	}
	if len(service.cache) != libraryArtworkCacheLimit || len(service.order) != libraryArtworkCacheLimit {
		t.Fatalf("cache=%d order=%d", len(service.cache), len(service.order))
	}
}
