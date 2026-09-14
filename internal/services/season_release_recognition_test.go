package services

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/classification"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata/tmdb"
	"gorm.io/gorm"
)

type seasonReleaseLookup struct {
	rankedRecognitionLookupFake
	queries []string
}

func (f *seasonReleaseLookup) SearchCandidates(_ context.Context, kind, title string, year *int, _, _ string, _ int) ([]tmdb.Candidate, error) {
	y := ""
	if year != nil {
		y = fmt.Sprint(*year)
	}
	f.queries = append(f.queries, kind+":"+title+":"+y)
	return nil, nil
}

func TestLibrarySeasonReleaseQueriesUseSeriesPremiereNotSeasonYear(t *testing.T) {
	var files []medialibrary.File
	for _, season := range []int{0, 1, 4, 7, 9} {
		files = append(files, medialibrary.File{RelativePath: fmt.Sprintf("/电视剧/欧美剧/24小时 (2001)/24小时.S%02d.%d.1080p/24小时.S%02dE01.%d.mkv", season, 2001+season, season, 2001+season), Size: 1 << 30})
	}
	units := medialibrary.GroupRecognitionUnits(files)
	if len(units) != 1 {
		t.Fatalf("groups=%+v", units)
	}
	var evidence []recognitionSourceFile
	for _, file := range units[0].EvidenceFiles {
		evidence = append(evidence, recognitionSourceFile{RelativePath: file.RelativePath, Size: file.Size})
	}
	lookup := &seasonReleaseLookup{}
	recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{PackageName: units[0].PackageName, Files: evidence, SourceKind: mediarecognition.SourceLibraryScan, MediaTypeHint: "tv", BuiltinPackCodes: mediarecognition.DefaultPackCodes(), Classification: classification.DefaultRules(), Language: "zh-CN", Region: "CN"})
	if len(lookup.queries) == 0 {
		t.Fatal("no work query")
	}
	for _, query := range lookup.queries {
		parts := strings.Split(query, ":")
		// Existing bounded type/title and +/-1 premiere-year fallback remains
		// unchanged; neither seasonal air years nor release/ancestor names enter.
		if len(parts) != 3 || (parts[2] != "" && parts[2] != "2000" && parts[2] != "2001" && parts[2] != "2002") || strings.ContainsAny(parts[1], ".") || strings.Contains(parts[1], "欧美剧") || strings.Contains(parts[1], "S0") || strings.Contains(parts[1], "1080p") {
			t.Fatalf("season/ancestor polluted work query: %s (%v)", query, lookup.queries)
		}
	}
	if lookup.queries[0] != "tv:24小时:2001" {
		t.Fatalf("primary identity=%v", lookup.queries)
	}
}

func TestLibraryNumericTitleSeasonReleaseUsesExplicitPremiereYear(t *testing.T) {
	for _, tc := range []struct {
		outer, seasonDirectory, file, wantQuery string
	}{
		{"1923 (2022)", "1923.S02.2025.1080p", "1923.S02E01.mkv", "tv:1923:2022"},
		{"1883 (2021)", "1883.S01.2021.1080p", "1883.S01E01.mkv", "tv:1883:2021"},
	} {
		t.Run(tc.outer, func(t *testing.T) {
			files := []medialibrary.File{{RelativePath: "/电视剧/" + tc.outer + "/" + tc.seasonDirectory + "/" + tc.file, Size: 1 << 30}}
			units := medialibrary.GroupRecognitionUnits(files)
			if len(units) != 1 || units[0].PackageName != tc.outer {
				t.Fatalf("groups=%+v", units)
			}
			evidence := []recognitionSourceFile{{RelativePath: files[0].RelativePath, Size: files[0].Size}}
			lookup := &seasonReleaseLookup{}
			recognizeMedia(context.Background(), lookup, MediaRecognitionRequest{PackageName: units[0].PackageName, Files: evidence, SourceKind: mediarecognition.SourceLibraryScan, MediaTypeHint: "tv", BuiltinPackCodes: mediarecognition.DefaultPackCodes(), Classification: classification.DefaultRules(), Language: "zh-CN", Region: "CN"})
			if len(lookup.queries) == 0 || lookup.queries[0] != tc.wantQuery {
				t.Fatalf("queries=%v, want first %q", lookup.queries, tc.wantQuery)
			}
		})
	}
}

func TestCatalogRescanRegroupsSeasonReleasesPreservingManualMembership(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint("manual=", manual), func(t *testing.T) {
			store, library, original, entries := catalogFixture(t)
			records := []models.MediaLibraryRecognition{original, original}
			records[1].ID, records[1].SourceKey = 0, "legacy-season-7"
			records[1].ManualOverride = manual
			records[1].Title = "User selected title"
			if err := store.writeDB.Create(&records[1]).Error; err != nil {
				t.Fatal(err)
			}
			candidate, token := catalogCandidate(t, store, library, "base", 0)
			var requests []CatalogIdentityRequest
			batch := CatalogFactBatch{}
			var files []medialibrary.File
			for i, season := range []int{4, 7} {
				record := &records[i]
				entry := &entries[i]
				entry.RelativePath = fmt.Sprintf("电视剧/欧美剧/24小时 (2001)/24小时.S%02d.%d.1080p/24小时.S%02dE01.mkv", season, 2001+season, season)
				entry.RecognitionID, entry.Season, entry.Title, entry.SeriesTitle = &record.ID, intPointerTest(season), record.Title, record.Title
				if err := store.writeDB.Save(entry).Error; err != nil {
					t.Fatal(err)
				}
				requests = append(requests, CatalogIdentityRequest{Kind: "recognition", SourceKey: record.SourceKey, ExistingID: record.ID}, CatalogIdentityRequest{Kind: "entry", SourceKey: entry.RelativePath, ExistingID: entry.ID})
				batch.Recognitions = append(batch.Recognitions, CatalogRecognitionFromLegacy(*record))
				batch.Entries = append(batch.Entries, CatalogEntryFromLegacy(*entry, record))
				files = append(files, scanFile(*entry))
			}
			if _, err := store.ResolveIdentities(context.Background(), candidate.ID, token, requests); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendBatch(context.Background(), candidate.ID, token, batch); err != nil {
				t.Fatal(err)
			}
			catalogPublish(t, store, candidate, token)
			service := NewMediaLibraryService(store.writeDB, NewAuditService(store.writeDB), zerolog.Nop())
			service.SetCatalogSnapshotStore(store)
			service.SetCatalogScanCommit(func(*gorm.DB, CatalogScanPublication) error { return nil })
			var storage models.Storage
			var profile models.MediaClassificationProfile
			if err := store.writeDB.First(&storage, library.StorageID).Error; err != nil {
				t.Fatal(err)
			}
			if err := store.writeDB.First(&profile, library.ProfileID).Error; err != nil {
				t.Fatal(err)
			}
			library, run := catalogScanRun(t, service, library.ID, storage, profile)
			ready, err := service.publishCatalogScan(context.Background(), library, storage, profile, run, medialibrary.Result{Files: files}, true, service.catalogScanCommit)
			if err != nil {
				t.Fatal(err)
			}
			if ready.RecognitionTotal != 1 {
				t.Fatalf("one automatic work should need recognition: %+v", ready)
			}
			job := catalogRecognitionJob(t, service)
			if err := service.completeFastMediaLibraryRecognition(context.Background(), fastScanTestRuntime{}, mediaLibraryRecognitionJobPayload{LibraryID: library.ID, ScanRunID: ready.ID, Generation: ready.Generation}, job); err != nil {
				t.Fatal(err)
			}
			rows := catalogReadEntries(t, store, library.ID, "")
			if len(rows) != 2 || rows[0].RecognitionID == nil || rows[1].RecognitionID == nil {
				t.Fatalf("lost files: %+v", rows)
			}
			for i, row := range rows {
				if row.ID != entries[i].ID || row.Season == nil || *row.Season != []int{4, 7}[i] || row.Episode == nil || *row.Episode != 1 {
					t.Fatalf("lost physical/episode identity: %+v", row)
				}
			}
			if !manual && *rows[0].RecognitionID != *rows[1].RecognitionID {
				t.Fatalf("automatic per-season split survived: %+v", rows)
			}
			if manual && (*rows[1].RecognitionID != records[1].ID || rows[1].Title != records[1].Title || *rows[0].RecognitionID == records[1].ID) {
				t.Fatalf("manual scope changed: %+v", rows)
			}
			var recognitions []models.MediaLibraryRecognition
			if err := store.Read(context.Background(), []uint{library.ID}, func(reader *CatalogReader) error {
				return reader.Recognitions().Order("id").Find(&recognitions).Error
			}); err != nil {
				t.Fatal(err)
			}
			wantRecognitions := 1
			if manual {
				wantRecognitions = 2
			}
			if len(recognitions) != wantRecognitions {
				t.Fatalf("orphan season recognitions did not converge: %+v", recognitions)
			}
			manualCount := 0
			for _, recognition := range recognitions {
				if recognition.ManualOverride {
					manualCount++
				}
			}
			if (!manual && manualCount != 0) || (manual && manualCount != 1) {
				t.Fatalf("manual recognition scope changed: %+v", recognitions)
			}
			if !strings.Contains(rows[0].RelativePath, "S04") {
				t.Fatal("physical source moved")
			}
		})
	}
}

func TestManualGroupingDistinguishesExistingPendingFromNewEpisode(t *testing.T) {
	id := uint(11)
	paths := []string{"Example Series/Season 01/Example.S01E01.mkv", "Example Series/Season 01/Example.S01E02.mkv", "Example Series/Season 01/Example.S01E03.mkv"}
	files := []medialibrary.File{{RelativePath: paths[0], ProviderID: "manual"}, {RelativePath: paths[1], ProviderID: "pending"}, {RelativePath: paths[2], ProviderID: "new"}}
	entries := []models.MediaLibraryEntry{{ID: 1, RelativePath: paths[0], ProviderID: "manual", RecognitionID: &id}, {ID: 2, RelativePath: paths[1], ProviderID: "pending", MatchStatus: mediaRecognitionStatusPending}}
	records := []models.MediaLibraryRecognition{{ID: id, SourceKey: "manual-work", ManualOverride: true, MediaType: "tv"}}
	units := stabilizeRecognitionUnits(medialibrary.GroupRecognitionUnits(files), entries, records)
	seen := map[string]string{}
	for _, unit := range units {
		for _, file := range unit.Files {
			seen[file.ProviderID] = unit.SourceKey
		}
	}
	if len(units) != 2 || seen["manual"] != "manual-work" || seen["new"] != "manual-work" || seen["pending"] == "manual-work" || seen["pending"] == "" {
		t.Fatalf("manual membership=%v", seen)
	}
}
