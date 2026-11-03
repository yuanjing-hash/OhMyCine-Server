package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
)

type recognitionLookupFake struct {
	searchTitle string
	directID    int64
}

type rankedRecognitionLookupFake struct {
	selectedID  int64
	items       []tmdb.Candidate
	searches    []string
	searchItems map[string][]tmdb.Candidate
}

type enrichedRecognitionLookupFake struct {
	rankedRecognitionLookupFake
	enriched        map[int64]tmdb.Candidate
	enrichmentCalls int
}

type embyHintLookupFake struct {
	calls []string
}

type titleSensitiveRecognitionLookupFake struct {
	searches []string
	selected int64
}

type nyaaRecognitionLookupFake struct {
	searches []string
}

type cancelingCandidateLookupFake struct {
	calls int
}

type productionEdgeRecognitionLookupFake struct {
	searches []string
}

func (f *productionEdgeRecognitionLookupFake) Search(context.Context, string, string, *int, string, string) (tmdb.Match, error) {
	return tmdb.Match{}, &tmdb.ClientError{Code: tmdb.ErrorNoMatch}
}

func (f *productionEdgeRecognitionLookupFake) SearchCandidates(_ context.Context, mediaType, title string, year *int, language, _ string, _ int) ([]tmdb.Candidate, error) {
	yearText := ""
	if year != nil {
		yearText = fmt.Sprint(*year)
	}
	f.searches = append(f.searches, language+":"+mediaType+":"+title+":"+yearText)
	switch {
	case title == "迪迦·奥特曼" && mediaType == "movie":
		return []tmdb.Candidate{
			{ID: 113094, Title: "迪迦奥特曼 剧场版：最终圣战", MediaType: "movie", Popularity: 3.46},
			{ID: 318718, Title: "迪迦奥特曼·戴拿奥特曼&盖亚奥特曼 剧场版：超时空大决战", MediaType: "movie", Popularity: 2.53},
		}, nil
	case title == "迪迦·奥特曼" && mediaType == "tv":
		return []tmdb.Candidate{{ID: 2253, Title: "迪迦奥特曼", OriginalTitle: "ウルトラマンティガ", MediaType: "tv", Popularity: 23.74}}, nil
	case title == "The Final Odyssey" && mediaType == "movie":
		return []tmdb.Candidate{
			{ID: 113094, Title: "迪迦奥特曼 剧场版：最终圣战", OriginalTitle: "ウルトラマンティガ THE FINAL ODYSSEY", MediaType: "movie", Popularity: 3.46},
			{ID: 1559261, Title: "银河交响曲：终极奥德赛", OriginalTitle: "Galaxymphony - The Final Odyssey", MediaType: "movie", Popularity: .95},
		}, nil
	case strings.EqualFold(title, "tiga") && language == "en-US" && mediaType == "movie":
		return []tmdb.Candidate{{ID: 1377374, Title: "Tiga", MediaType: "movie", ReleaseYear: intPointerTest(1996)}}, nil
	case strings.EqualFold(title, "tiga") && language == "en-US" && mediaType == "tv":
		return []tmdb.Candidate{
			{ID: 2253, Title: "Ultraman Tiga", OriginalTitle: "ウルトラマンティガ", MediaType: "tv", ReleaseYear: intPointerTest(1996), Popularity: 23.74},
			{ID: 123417, Title: "Ultraman Trigger: New Generation Tiga", MediaType: "tv", ReleaseYear: intPointerTest(2021), Popularity: 8.87},
		}, nil
	default:
		return nil, &tmdb.ClientError{Code: tmdb.ErrorNoMatch}
	}
}

func (f *productionEdgeRecognitionLookupFake) EnrichCandidates(_ context.Context, candidates []tmdb.Candidate, _ string, limit int) ([]tmdb.Candidate, error) {
	result := append([]tmdb.Candidate(nil), candidates...)
	if len(result) > limit {
		result = result[:limit]
	}
	for index := range result {
		switch result[index].ID {
		case 113094:
			result[index].AlternativeTitles = []string{"Ultraman Tiga The Final Odyssey"}
			result[index].Translations = []string{"Ultraman Tiga: The Final Odyssey"}
		case 2253:
			result[index].AlternativeTitles = []string{"Ultraman Tiga"}
		}
	}
	return result, nil
}

func (f *productionEdgeRecognitionLookupFake) GetByID(_ context.Context, mediaType string, id int64, _ string) (tmdb.Match, error) {
	switch id {
	case 2253:
		return tmdb.Match{ID: id, Title: "迪迦奥特曼", MediaType: mediaType, ReleaseYear: intPointerTest(1996), Snapshot: tmdb.Snapshot{TMDBID: id, Title: "迪迦奥特曼", MediaType: mediaType}}, nil
	case 113094:
		return tmdb.Match{ID: id, Title: "迪迦奥特曼 剧场版：最终圣战", MediaType: mediaType, ReleaseYear: intPointerTest(2000), Snapshot: tmdb.Snapshot{TMDBID: id, Title: "迪迦奥特曼 剧场版：最终圣战", MediaType: mediaType}}, nil
	default:
		return tmdb.Match{}, &tmdb.ClientError{Code: tmdb.ErrorNoMatch}
	}
}

func (f *cancelingCandidateLookupFake) Search(context.Context, string, string, *int, string, string) (tmdb.Match, error) {
	return tmdb.Match{}, &tmdb.ClientError{Code: tmdb.ErrorNoMatch}
}

func (f *cancelingCandidateLookupFake) SearchCandidates(context.Context, string, string, *int, string, string, int) ([]tmdb.Candidate, error) {
	f.calls++
	if f.calls == 1 {
		return []tmdb.Candidate{{ID: 1, Title: "Example", MediaType: "movie"}}, nil
	}
	return nil, context.Canceled
}

func (f *cancelingCandidateLookupFake) GetByID(context.Context, string, int64, string) (tmdb.Match, error) {
	return tmdb.Match{}, errors.New("GetByID must not run after cancellation")
}

func (f *nyaaRecognitionLookupFake) Search(context.Context, string, string, *int, string, string) (tmdb.Match, error) {
	return tmdb.Match{}, &tmdb.ClientError{Code: tmdb.ErrorNoMatch}
}

func (f *nyaaRecognitionLookupFake) SearchCandidates(_ context.Context, mediaType, title string, _ *int, _, _ string, _ int) ([]tmdb.Candidate, error) {
	f.searches = append(f.searches, mediaType+":"+title)
	if mediaType == "tv" && title == "迪迦奥特曼" {
		return []tmdb.Candidate{{ID: 10820, Title: "迪迦奥特曼", OriginalTitle: "ウルトラマンティガ", MediaType: "tv", SeasonCount: 1, Popularity: 100}}, nil
	}
	return nil, &tmdb.ClientError{Code: tmdb.ErrorNoMatch}
}

func (f *nyaaRecognitionLookupFake) GetByID(_ context.Context, mediaType string, id int64, _ string) (tmdb.Match, error) {
	return tmdb.Match{ID: id, Title: "迪迦奥特曼", MediaType: mediaType, Confidence: 1, Snapshot: tmdb.Snapshot{TMDBID: id, Title: "迪迦奥特曼", MediaType: mediaType}}, nil
}

func (f *titleSensitiveRecognitionLookupFake) Search(context.Context, string, string, *int, string, string) (tmdb.Match, error) {
	return tmdb.Match{}, &tmdb.ClientError{Code: tmdb.ErrorNoMatch}
}

func (f *titleSensitiveRecognitionLookupFake) SearchCandidates(_ context.Context, mediaType, title string, _ *int, _, _ string, _ int) ([]tmdb.Candidate, error) {
	f.searches = append(f.searches, mediaType+":"+title)
	if mediaType != "tv" || title != "斗罗大陆" {
		return nil, &tmdb.ClientError{Code: tmdb.ErrorNoMatch}
	}
	return []tmdb.Candidate{{ID: 95557, Title: "斗罗大陆", MediaType: "tv", SeasonCount: 1, Popularity: 100}}, nil
}

func (f *titleSensitiveRecognitionLookupFake) GetByID(_ context.Context, mediaType string, id int64, _ string) (tmdb.Match, error) {
	f.selected = id
	return tmdb.Match{ID: id, Title: "斗罗大陆", MediaType: mediaType, Confidence: 1}, nil
}

func (f *embyHintLookupFake) Search(context.Context, string, string, *int, string, string) (tmdb.Match, error) {
	return tmdb.Match{}, &tmdb.ClientError{Code: tmdb.ErrorNoMatch}
}

func (f *embyHintLookupFake) GetByID(_ context.Context, mediaType string, id int64, _ string) (tmdb.Match, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s:%d", mediaType, id))
	if mediaType != "movie" {
		return tmdb.Match{}, &tmdb.ClientError{Code: tmdb.ErrorRequestFailed}
	}
	year := 1954
	return tmdb.Match{ID: id, Title: "Seven Samurai", MediaType: mediaType, ReleaseYear: &year, Confidence: 1, Snapshot: tmdb.Snapshot{OriginalTitle: "七人の侍"}}, nil
}

func (f *rankedRecognitionLookupFake) Search(context.Context, string, string, *int, string, string) (tmdb.Match, error) {
	return tmdb.Match{}, &tmdb.ClientError{Code: tmdb.ErrorNoMatch}
}

func (f *rankedRecognitionLookupFake) SearchCandidates(_ context.Context, mediaType, title string, _ *int, _, _ string, _ int) ([]tmdb.Candidate, error) {
	key := mediaType + ":" + title
	f.searches = append(f.searches, key)
	if f.searchItems != nil {
		return append([]tmdb.Candidate(nil), f.searchItems[key]...), nil
	}
	return append([]tmdb.Candidate(nil), f.items...), nil
}

func (f *rankedRecognitionLookupFake) GetByID(_ context.Context, mediaType string, id int64, _ string) (tmdb.Match, error) {
	f.selectedID = id
	year := 2007
	return tmdb.Match{ID: id, Title: "大明王朝1566", MediaType: mediaType, ReleaseYear: &year, Confidence: 1}, nil
}

func (f *enrichedRecognitionLookupFake) EnrichCandidates(_ context.Context, candidates []tmdb.Candidate, _ string, limit int) ([]tmdb.Candidate, error) {
	f.enrichmentCalls++
	result := append([]tmdb.Candidate(nil), candidates...)
	for index := range result {
		if replacement, exists := f.enriched[result[index].ID]; exists {
			result[index] = replacement
		}
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (f *recognitionLookupFake) Search(_ context.Context, mediaType, title string, year *int, _, _ string) (tmdb.Match, error) {
	f.searchTitle = title
	return tmdb.Match{ID: 346, Title: "七武士", MediaType: mediaType, ReleaseYear: year, Confidence: .98, Snapshot: tmdb.Snapshot{Version: 1, TMDBID: 346, MediaType: mediaType, Title: "七武士", PosterPath: "/poster.jpg"}}, nil
}

func TestRecognitionMetadataEnvelopePersistsSnapshotAndReadsLegacyClassification(t *testing.T) {
	result := MediaRecognitionResult{Metadata: classification.Metadata{MediaType: classification.MediaTypeMovie, OriginalLanguage: "ja"}, Snapshot: tmdb.Snapshot{Version: 1, TMDBID: 346, MediaType: "movie", Title: "七武士", PosterPath: "/poster.jpg"}}
	raw, err := marshalRecognitionMetadata(result)
	if err != nil {
		t.Fatal(err)
	}
	metadata, snapshot, err := decodeRecognitionMetadata(raw)
	if err != nil || metadata.OriginalLanguage != "ja" || snapshot.TMDBID != 346 || snapshot.PosterPath != "/poster.jpg" {
		t.Fatalf("metadata=%+v snapshot=%+v err=%v", metadata, snapshot, err)
	}
	if !strings.Contains(raw, `"engine_version":"`+mediarecognition.EngineVersion+`"`) {
		t.Fatalf("recognition engine version missing from durable envelope: %s", raw)
	}
	legacy, emptySnapshot, err := decodeRecognitionMetadata(`{"MediaType":"movie","OriginalLanguage":"ja"}`)
	if err != nil || legacy.OriginalLanguage != "ja" || emptySnapshot.TMDBID != 0 {
		t.Fatalf("legacy=%+v snapshot=%+v err=%v", legacy, emptySnapshot, err)
	}
}

func (f *recognitionLookupFake) GetByID(_ context.Context, mediaType string, id int64, _ string) (tmdb.Match, error) {
	f.directID = id
	return tmdb.Match{ID: id, Title: "精灵幻想记", MediaType: mediaType, Confidence: 1}, nil
}

func TestRecognizeMediaUsesFullReleaseNameAndValidatesDirectHint(t *testing.T) {
	lookup := &recognitionLookupFake{}
	result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
		PackageName:      "Seven.Samurai.1954.1080p.BluRay.x264",
		Files:            []recognitionSourceFile{{RelativePath: "Seven.Samurai.1954.1080p.BluRay.x264.mkv", Size: 10}},
		BuiltinPackCodes: mediarecognition.DefaultPackCodes(),
		Classification:   classification.EmptyRules(),
		Language:         "zh-CN",
		Region:           "CN",
	})
	if result.Status != mediaRecognitionStatusMatched || lookup.searchTitle != "Seven Samurai" || result.TMDBID == nil || *result.TMDBID != 346 {
		t.Fatalf("result=%+v search_title=%q", result, lookup.searchTitle)
	}

	direct := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
		PackageName:      "ANi 精靈幻想記 2 01",
		Files:            []recognitionSourceFile{{RelativePath: "ANi 精靈幻想記 2 01.mkv", Size: 10}},
		BuiltinPackCodes: mediarecognition.DefaultPackCodes(),
		Classification:   classification.EmptyRules(),
		Language:         "zh-CN",
	})
	if direct.Status != mediaRecognitionStatusMatched || lookup.directID != 113808 || direct.SeasonHint == nil || *direct.SeasonHint != 2 {
		t.Fatalf("direct=%+v id=%d", direct, lookup.directID)
	}
}

func TestRecognizeMediaUsesEmbyTMDBHintAndRejectsUnsafeAbsoluteFacts(t *testing.T) {
	lookup := &embyHintLookupFake{}
	result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
		PackageName:      "Seven Samurai (1954) {tmdb-346}",
		Files:            []recognitionSourceFile{{RelativePath: "Seven Samurai (1954)/Seven Samurai.mkv", Size: 10}},
		SourceKind:       mediarecognition.SourceLibraryScan,
		MediaTypeHint:    "movie",
		BuiltinPackCodes: mediarecognition.DefaultPackCodes(),
		Classification:   classification.DefaultRules(),
		Language:         "en-US",
	})
	if result.Status != mediaRecognitionStatusMatched || result.TMDBID == nil || *result.TMDBID != 346 || len(lookup.calls) != 2 {
		t.Fatalf("result=%+v calls=%v", result, lookup.calls)
	}
	unsafe := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
		PackageName:      "Seven Samurai",
		Files:            []recognitionSourceFile{{RelativePath: "/private/media/Seven Samurai.mkv", Size: 10}},
		BuiltinPackCodes: mediarecognition.DefaultPackCodes(),
		Classification:   classification.DefaultRules(),
	})
	if unsafe.ErrorCode != tmdb.ErrorInvalidRequest {
		t.Fatalf("absolute fact was sanitized into recognizer: %+v", unsafe)
	}
}

func TestRecognizeMediaRanksAllCandidatesInsteadOfTakingFirstResult(t *testing.T) {
	lookup := &rankedRecognitionLookupFake{items: []tmdb.Candidate{
		{ID: 9, Title: "Ming Dynasty", MediaType: "tv", Confidence: .82},
		{ID: 100, Title: "大明王朝1566", OriginalTitle: "Ming Dynasty in 1566", MediaType: "tv", Confidence: .98},
	}}
	files := make([]recognitionSourceFile, 0, 49)
	for episode := 1; episode <= 49; episode++ {
		files = append(files, recognitionSourceFile{RelativePath: fmt.Sprintf("Ming Dynasty in 1566/Ming.Dynasty.in.1566.S01E%02d.mkv", episode), Size: 1 << 30})
	}
	result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
		PackageName:      "Ming Dynasty in 1566 HQ -BlackTV",
		Files:            files,
		BuiltinPackCodes: mediarecognition.DefaultPackCodes(),
		Classification:   classification.DefaultRules(),
		Language:         "zh-CN",
		Region:           "CN",
	})
	if result.Status != mediaRecognitionStatusMatched || result.TMDBID == nil || *result.TMDBID != 100 || lookup.selectedID != 100 || result.MediaType != "tv" {
		t.Fatalf("result=%+v selected=%d", result, lookup.selectedID)
	}
}

func TestRecognizeMediaMatchesChineseEpisodeNamesFromLibraryScan(t *testing.T) {
	lookup := &titleSensitiveRecognitionLookupFake{}
	result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
		PackageName: "斗罗大陆 - - 第2集",
		Files: []recognitionSourceFile{
			{RelativePath: "/斗罗大陆/斗罗大陆 - - 第1集.mp4", Size: 10},
			{RelativePath: "/斗罗大陆/斗罗大陆 - - 第2集.mp4", Size: 20},
		},
		SourceKind:       mediarecognition.SourceLibraryScan,
		MediaTypeHint:    "tv",
		BuiltinPackCodes: mediarecognition.DefaultPackCodes(),
		Classification:   classification.DefaultRules(),
		Language:         "zh-CN",
		Region:           "CN",
	})
	if result.Status != mediaRecognitionStatusMatched || result.Title != "斗罗大陆" || result.MediaType != "tv" || result.TMDBID == nil || *result.TMDBID != 95557 || lookup.selected != 95557 {
		t.Fatalf("result=%+v searches=%v selected=%d", result, lookup.searches, lookup.selected)
	}
}

func TestRecognizeMediaUsesBuiltinPacksAndDomainParserForNyaaCompleteSeries(t *testing.T) {
	lookup := &nyaaRecognitionLookupFake{}
	result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
		PackageName:      "[DBD-Raws][迪迦奥特曼/Ultraman Tiga/ウルトラマンティガ][01-52TV全集+剧场+OV+特典][1080P][BDRip][HEVC-10bit][简体字幕外挂][FLAC][MKV]",
		Files:            []recognitionSourceFile{{RelativePath: "迪迦奥特曼/Ultraman.Tiga.EP01.mkv", Size: 10}},
		BuiltinPackCodes: mediarecognition.DefaultPackCodes(),
		Classification:   classification.DefaultRules(),
		Language:         "zh-CN",
		Region:           "CN",
	})
	if result.Status != mediaRecognitionStatusMatched || result.MediaType != "tv" || result.Title != "迪迦奥特曼" || result.TMDBID == nil || *result.TMDBID != 10820 {
		t.Fatalf("result=%+v searches=%v", result, lookup.searches)
	}
	found := false
	for _, search := range lookup.searches {
		if search == "tv:迪迦奥特曼" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("clean multilingual query missing: %v", lookup.searches)
	}
}

func TestRecognizeMediaHandlesRealTMDBFranchiseOrderingAndBoundedTypoFallback(t *testing.T) {
	tests := []struct {
		name         string
		release      string
		expectedID   int64
		expectedType string
	}{
		{name: "punctuated chinese series", release: "迪迦·奥特曼 1080p", expectedID: 2253, expectedType: "tv"},
		{name: "standalone franchise movie subtitle", release: "The Final Odyssey 1080p WEB-DL H264 AAC-Side", expectedID: 113094, expectedType: "movie"},
		{name: "one missing latin letter", release: "ULRAMAN+TIGA 1996 BluRay X264 1080p", expectedID: 2253, expectedType: "tv"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookup := &productionEdgeRecognitionLookupFake{}
			result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{PackageName: test.release, SourceKind: mediarecognition.SourceDownload, BuiltinPackCodes: mediarecognition.DefaultPackCodes(), Classification: classification.DefaultRules(), Language: "zh-CN", Region: "CN"})
			if result.Status != mediaRecognitionStatusMatched || result.TMDBID == nil || *result.TMDBID != test.expectedID || result.MediaType != test.expectedType {
				t.Fatalf("result=%+v searches=%v", result, lookup.searches)
			}
			if len(lookup.searches) > mediaRecognitionMaxQueries {
				t.Fatalf("request budget exceeded: %v", lookup.searches)
			}
			if strings.Contains(test.release, "ULRAMAN") && !containsTestString(lookup.searches, "en-US:tv:tiga:1996") {
				t.Fatalf("bounded English token fallback missing: %v", lookup.searches)
			}
		})
	}
}

func TestRecognizeMediaLongRunningAnimeUsesStructureAndFullSubtitleSafely(t *testing.T) {
	tests := []struct {
		name          string
		release       string
		items         []tmdb.Candidate
		expectedID    int64
		expectedType  string
		expectedError string
	}{
		{
			name:    "exact series title beats same-title movie through TV structure",
			release: "[银色子弹字幕组][名侦探柯南][第1210集 被诅咒的邻居][WEBRIP][简日双语MP4][1080P]",
			items: []tmdb.Candidate{
				{ID: 30983, Title: "名侦探柯南", OriginalTitle: "名探偵コナン", MediaType: "tv", SeasonCount: 1, Popularity: 150},
				{ID: 90001, Title: "名侦探柯南", MediaType: "movie", Popularity: 300},
				{ID: 917496, Title: "名侦探柯南：唐红的恋歌", MediaType: "movie", Popularity: 50},
			},
			expectedID: 30983, expectedType: "tv",
		},
		{
			name:    "franchise movie number keeps the distinctive bilingual subtitle",
			release: "[银色子弹字幕组&VCB-Studio] 名侦探柯南M21 唐红的恋歌 / Detective Conan M21: The Crimson Love Letter 10-bit 1080p HEVC BDRip [MOVIE Fin]",
			items: []tmdb.Candidate{
				{ID: 917496, Title: "名侦探柯南：唐红的恋歌", AlternativeTitles: []string{"Detective Conan: The Crimson Love Letter"}, MediaType: "movie"},
				{ID: 30983, Title: "名侦探柯南", OriginalTitle: "名探偵コナン", MediaType: "tv", SeasonCount: 1},
			},
			expectedID: 917496, expectedType: "movie",
		},
		{
			name:    "untyped exact cross-type identity selects stable provisional winner",
			release: "Detective Conan The Scarlet School Trip 2019 1080p BluRay HEVC FLAC 2.0 2Audios-ADE",
			items: []tmdb.Candidate{
				{ID: 1, Title: "Detective Conan The Scarlet School Trip", MediaType: "movie"},
				{ID: 2, Title: "Detective Conan The Scarlet School Trip", MediaType: "tv"},
			},
			expectedID: 1, expectedType: "movie",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lookup := &rankedRecognitionLookupFake{items: test.items}
			result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
				PackageName:      test.release,
				SourceKind:       mediarecognition.SourceDownload,
				BuiltinPackCodes: mediarecognition.DefaultPackCodes(),
				Classification:   classification.DefaultRules(),
				Language:         "zh-CN",
				Region:           "CN",
			})
			if test.expectedError != "" {
				if result.Status != mediaRecognitionStatusUnrecognized || result.ErrorCode != test.expectedError || lookup.selectedID != 0 {
					t.Fatalf("result=%+v selected=%d searches=%v", result, lookup.selectedID, lookup.searches)
				}
				return
			}
			if result.Status != mediaRecognitionStatusMatched || result.TMDBID == nil || *result.TMDBID != test.expectedID || result.MediaType != test.expectedType || lookup.selectedID != test.expectedID {
				t.Fatalf("result=%+v selected=%d searches=%v", result, lookup.selectedID, lookup.searches)
			}
		})
	}
}

func TestRecognizeMediaAuxiliaryRuleCannotPromoteDirectIdentity(t *testing.T) {
	processor, err := mediarecognition.CompileWordProcessor([]string{
		`Poison => Safe Title {[tmdbid=999;type=movie]}`,
	}, mediarecognition.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	lookup := &rankedRecognitionLookupFake{items: []tmdb.Candidate{{ID: 1, Title: "Safe Title", MediaType: "movie"}}}
	result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
		PackageName:      "Safe Title 2024",
		AuxiliaryNames:   []string{"Poison"},
		BuiltinProcessor: processor,
		Classification:   classification.DefaultRules(),
		Language:         "zh-CN",
		Region:           "CN",
	})
	if result.Status != mediaRecognitionStatusMatched || lookup.selectedID != 1 {
		t.Fatalf("untrusted auxiliary direct hint escaped into lookup: result=%+v selected=%d searches=%v", result, lookup.selectedID, lookup.searches)
	}
}

func TestRecognizeMediaBridgesPinyinThroughAuthoritativeCrossTypeTitle(t *testing.T) {
	bridgeMovie := tmdb.Candidate{ID: 541781, Title: "爱情公寓", OriginalTitle: "爱情公寓", MediaType: "movie", ReleaseYear: intPointerTest(2018)}
	series := tmdb.Candidate{ID: 68809, Title: "爱情公寓", OriginalTitle: "爱情公寓", MediaType: "tv", ReleaseYear: intPointerTest(2009)}
	lookup := &enrichedRecognitionLookupFake{
		rankedRecognitionLookupFake: rankedRecognitionLookupFake{searchItems: map[string][]tmdb.Candidate{
			"movie:Ai qing gong yu": {bridgeMovie},
			"tv:爱情公寓":               {series},
		}},
		enriched: map[int64]tmdb.Candidate{
			541781: {ID: 541781, Title: "爱情公寓", MediaType: "movie", AlternativeTitles: []string{"Ai qing gong yu"}, ReleaseYear: intPointerTest(2018)},
			68809:  {ID: 68809, Title: "爱情公寓", MediaType: "tv", AlternativeTitles: []string{"Ai qing gong yu", "iPartment"}, ReleaseYear: intPointerTest(2009), SeasonCount: 5, SeasonYears: map[int]int{3: 2012}},
		},
	}
	result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{PackageName: "Ai qing gong yu 2012 S03 2160p WEB-DL H.265 AAC-ZmWeb", SourceKind: mediarecognition.SourceDownload, BuiltinPackCodes: mediarecognition.DefaultPackCodes(), Classification: classification.DefaultRules(), Language: "zh-CN", Region: "CN"})
	if result.Status != mediaRecognitionStatusMatched || result.MediaType != "tv" || result.TMDBID == nil || *result.TMDBID != 68809 {
		t.Fatalf("result=%+v searches=%v", result, lookup.searches)
	}
	if !containsTestString(lookup.searches, "movie:Ai qing gong yu") || !containsTestString(lookup.searches, "tv:爱情公寓") || len(lookup.searches) > mediaRecognitionMaxQueries {
		t.Fatalf("bounded alias bridge searches=%v", lookup.searches)
	}
}

func TestDomainCandidateRecallDoesNotAcceptPartialResultsAfterCancellation(t *testing.T) {
	parsed, err := mediarecognition.Parse(mediarecognition.InputFacts{PackageName: "Example S01", SourceKind: mediarecognition.SourceDownload})
	if err != nil {
		t.Fatal(err)
	}
	lookup := &cancelingCandidateLookupFake{}
	if _, _, err := recognizeFromDomainCandidates(context.Background(), lookup, lookup, parsed, "en-US", "US"); !errors.Is(err, context.Canceled) {
		t.Fatalf("partial candidate result swallowed cancellation: %v", err)
	}
}

func containsTestString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func intPointerTest(value int) *int { return &value }

func TestDomainRecognitionSearchQueriesPrioritizeCanonicalVariantsAcrossSources(t *testing.T) {
	parsed := mediarecognition.ParsedFacts{Queries: []mediarecognition.QueryVariant{
		{Title: "Noisy Canonical", SuggestedType: mediarecognition.MediaTypeTV, Source: "filename", Reason: "canonical"},
		{Title: "Noisy without group", SuggestedType: mediarecognition.MediaTypeTV, Source: "filename", Reason: "without_release_group"},
		{Title: "Noisy raw", SuggestedType: mediarecognition.MediaTypeTV, Source: "filename", Reason: "raw"},
		{Title: "Clean Parent", SuggestedType: mediarecognition.MediaTypeTV, Source: "parent", Reason: "canonical"},
	}}
	queries := domainRecognitionSearchQueries(parsed)
	found := false
	for _, query := range queries {
		if query.Title == "Clean Parent" && query.MediaType == "tv" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("clean canonical source was excluded from bounded query budget: %+v", queries)
	}
}

func TestRecognizeMediaReranksBoundedEnrichedAliases(t *testing.T) {
	year2011, year1990 := 2011, 1990
	lookup := &enrichedRecognitionLookupFake{
		rankedRecognitionLookupFake: rankedRecognitionLookupFake{items: []tmdb.Candidate{
			{ID: 1, Title: "後宮甄嬛傳", MediaType: "tv", ReleaseYear: &year1990},
			{ID: 2, Title: "Empresses in the Palace", MediaType: "tv", ReleaseYear: &year2011},
		}},
		enriched: map[int64]tmdb.Candidate{
			1: {ID: 1, Title: "後宮甄嬛傳", MediaType: "tv", ReleaseYear: &year1990, SeasonCount: 1, SeasonYears: map[int]int{1: 1990}},
			2: {ID: 2, Title: "Empresses in the Palace", MediaType: "tv", ReleaseYear: &year2011, Translations: []string{"後宮甄嬛傳", "后宫甄嬛传"}, SeasonCount: 1, SeasonYears: map[int]int{1: 2011}},
		},
	}
	result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
		PackageName:      "後宮甄嬛傳.2011.S01E01.1080p",
		Files:            []recognitionSourceFile{{RelativePath: "後宮甄嬛傳/Season 01/後宮甄嬛傳.S01E01.mkv", Size: 10}},
		BuiltinPackCodes: mediarecognition.DefaultPackCodes(),
		Classification:   classification.DefaultRules(),
		Language:         "zh-CN",
		Region:           "CN",
	})
	if result.Status != mediaRecognitionStatusMatched || lookup.selectedID != 2 || lookup.enrichmentCalls != 1 {
		t.Fatalf("result=%+v selected=%d enrich_calls=%d", result, lookup.selectedID, lookup.enrichmentCalls)
	}
}

func TestRecognizeMediaUsesExtremeFloorAndProvisionalConflictWinner(t *testing.T) {
	year2005 := 2005
	files := []recognitionSourceFile{{RelativePath: "The Office/Season 01/The.Office.S01E01.mkv", Size: 10}}
	request := MediaRecognitionRequest{PackageName: "The Office.2005.S01E01", Files: files, BuiltinPackCodes: mediarecognition.DefaultPackCodes(), Classification: classification.DefaultRules(), Language: "en-US", Region: "US"}
	for _, test := range []struct {
		name     string
		items    []tmdb.Candidate
		code     string
		selected int64
	}{
		{name: "no match", items: nil, code: tmdb.ErrorNoMatch},
		{name: "extreme low confidence", items: []tmdb.Candidate{{ID: 10, Title: "Entirely Unrelated", MediaType: "tv", ReleaseYear: &year2005}}, code: tmdb.ErrorNoMatch},
		{name: "candidate conflict", items: []tmdb.Candidate{{ID: 11, Title: "The Office", MediaType: "tv", ReleaseYear: &year2005}, {ID: 12, Title: "The Office", MediaType: "tv", ReleaseYear: &year2005}}, selected: 11},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookup := &rankedRecognitionLookupFake{items: test.items}
			result := recognizeMedia(context.Background(), lookup, request)
			if test.selected != 0 {
				if result.Status != mediaRecognitionStatusMatched || result.ErrorCode != "" || lookup.selectedID != test.selected || result.IdentityStatus != mediaIdentityStatusProvisional {
					t.Fatalf("result=%+v selected=%d", result, lookup.selectedID)
				}
			} else if result.Status != mediaRecognitionStatusUnrecognized || result.ErrorCode != test.code || lookup.selectedID != 0 {
				t.Fatalf("result=%+v selected=%d", result, lookup.selectedID)
			}
		})
	}
}

func TestRecognizeMediaDoesNotBindProvisionalCandidateIntoLibraryCatalog(t *testing.T) {
	year := 2018
	lookup := &rankedRecognitionLookupFake{items: []tmdb.Candidate{
		{ID: 1, Title: "蜡笔小新：功夫小子之拉面大作战2", MediaType: "movie", ReleaseYear: &year, Popularity: 1_000},
	}}
	result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
		PackageName: "蜡笔小新：功夫小子之拉面大作战 (2018)",
		Files: []recognitionSourceFile{{
			RelativePath: "/电影/动画电影/蜡笔小新：功夫小子之拉面大作战 (2018)/蜡笔小新：功夫小子之拉面大作战.mp4",
			Size:         1 << 30,
		}},
		SourceKind:       mediarecognition.SourceLibraryScan,
		MediaTypeHint:    "movie",
		BuiltinPackCodes: mediarecognition.DefaultPackCodes(),
		Classification:   classification.DefaultRules(),
		Language:         "zh-CN",
		Region:           "CN",
	})
	if result.Status != mediaRecognitionStatusUnrecognized || result.ErrorCode != mediaRecognitionLowConfidence || result.TMDBID != nil || lookup.selectedID == 0 {
		t.Fatalf("provisional library identity was bound: result=%+v selected=%d", result, lookup.selectedID)
	}
	if result.Title != "蜡笔小新:功夫小子之拉面大作战" || result.IdentityStatus != mediaIdentityStatusProvisional || result.DecisionReason != mediarecognition.ReasonLowConfidence {
		t.Fatalf("manual recovery facts were not preserved: result=%+v", result)
	}
	for _, search := range lookup.searches {
		if strings.Contains(search, ":电影") || strings.Contains(search, ":动画电影") {
			t.Fatalf("library ancestor entered TMDB search: %v", lookup.searches)
		}
	}
}

func TestLibraryRecognitionSearchesOnlyGroupedWorkTitleAcrossNestedLayouts(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		packageName string
		workTitle   string
		forbidden   []string
	}{
		{name: "season", path: "/电视剧/华语剧/白日提灯 (2025)/Season 01/白日提灯.S01E23.mkv", packageName: "白日提灯 (2025)", workTitle: "白日提灯", forbidden: []string{"电视剧", "华语剧", "Season 01", "白日提灯 S01E23"}},
		{name: "bdmv", path: "/电影/经典电影/七武士 (1954)/BDMV/STREAM/00000.m2ts", packageName: "七武士 (1954)", workTitle: "七武士", forbidden: []string{"电影", "经典电影", "BDMV", "STREAM"}},
		{name: "video ts", path: "/电影/华语电影/英雄 (2002)/VIDEO_TS/VTS_01_1.VOB", packageName: "英雄 (2002)", workTitle: "英雄", forbidden: []string{"电影", "华语电影", "VIDEO_TS", "VTS 01 1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			units := medialibrary.GroupRecognitionUnits([]medialibrary.File{{RelativePath: test.path, Size: 1 << 30}})
			if len(units) != 1 || units[0].PackageName != test.packageName {
				t.Fatalf("library grouping did not select the work directory: units=%+v", units)
			}
			unit := units[0]
			files := make([]recognitionSourceFile, 0, len(unit.EvidenceFiles))
			for _, file := range unit.EvidenceFiles {
				files = append(files, recognitionSourceFile{RelativePath: file.RelativePath, Size: file.Size})
			}
			lookup := &rankedRecognitionLookupFake{}
			result := recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{
				PackageName: unit.PackageName, Files: files,
				SourceKind: mediarecognition.SourceLibraryScan, MediaTypeHint: unit.MediaTypeHint,
				BuiltinPackCodes: mediarecognition.DefaultPackCodes(), Classification: classification.DefaultRules(), Language: "zh-CN", Region: "CN",
			})
			if result.Status != mediaRecognitionStatusUnrecognized {
				t.Fatalf("unexpected result=%+v", result)
			}
			foundWork := false
			for _, search := range lookup.searches {
				parts := strings.SplitN(search, ":", 2)
				title := ""
				if len(parts) == 2 {
					title = parts[1]
				}
				if title == test.workTitle {
					foundWork = true
				}
				for _, forbidden := range test.forbidden {
					if title == forbidden {
						t.Fatalf("non-work surface %q entered TMDB search: %v", forbidden, lookup.searches)
					}
				}
			}
			if !foundWork {
				t.Fatalf("work title missing from TMDB searches: %v", lookup.searches)
			}
		})
	}
}

func TestDomainRecognitionSearchQueriesReserveBoundedYearFallbacks(t *testing.T) {
	year := 2007
	parsed := mediarecognition.ParsedFacts{Queries: []mediarecognition.QueryVariant{
		{Title: "Ming Dynasty in 1566", Year: &year, SuggestedType: mediarecognition.MediaTypeTV},
		{Title: "大明王朝1566", Year: &year, SuggestedType: mediarecognition.MediaTypeTV},
		{Title: "Ming Dynasty", Year: &year, SuggestedType: mediarecognition.MediaTypeTV},
		{Title: "unused", Year: &year, SuggestedType: mediarecognition.MediaTypeTV},
	}}
	queries := domainRecognitionSearchQueries(parsed)
	if len(queries) > mediaRecognitionMaxQueries {
		t.Fatalf("queries=%d", len(queries))
	}
	want := map[string]bool{"tv:2007": false, "movie:2007": false, "tv:2006": false, "tv:2008": false, "tv:none": false}
	for _, query := range queries {
		if query.Title != "Ming Dynasty in 1566" {
			continue
		}
		yearKey := "none"
		if query.Year != nil {
			yearKey = fmt.Sprint(*query.Year)
		}
		key := query.MediaType + ":" + yearKey
		if _, exists := want[key]; exists {
			want[key] = true
		}
	}
	for key, found := range want {
		if !found {
			t.Fatalf("missing fallback %s in %+v", key, queries)
		}
	}
}

func TestParseBuiltinPackCodesDistinguishesLegacyDefaultAndExplicitEmpty(t *testing.T) {
	legacy, err := parseBuiltinPackCodes("")
	if err != nil || len(legacy) != 2 {
		t.Fatalf("legacy=%v err=%v", legacy, err)
	}
	disabled, err := parseBuiltinPackCodes("[]")
	if err != nil || len(disabled) != 0 {
		t.Fatalf("disabled=%v err=%v", disabled, err)
	}
}
