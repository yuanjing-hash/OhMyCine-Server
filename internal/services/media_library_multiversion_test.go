package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructureRejectsLegacyLossyVersionPlanBeforeMutation(t *testing.T) {
	service, actor, library, diagnostics := prepareStructureSelectionConflicts(t, 1)
	page, err := service.StructureIssues(context.Background(), actor, library.ID, MediaLibraryStructureIssueQuery{Page: 1, PageSize: 10, Actionable: true})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.PreviewSelectionRepair(context.Background(), actor, library.ID, MediaLibraryStructureSelectionInput{Revision: diagnostics.Revision, Selections: []MediaLibraryStructureSelection{{IssueToken: page.List[0].Token, Action: StructureSelectionKeepAllVersions}}})
	if err != nil {
		t.Fatal(err)
	}
	repair, err := service.EnqueueSelectionRepair(context.Background(), actor, library.ID, preview.ConfirmationToken, RequestContext{})
	if err != nil {
		t.Fatal(err)
	}
	var record models.MediaLibraryStructureRepair
	if err := service.db.First(&record, "id = ?", repair.ID).Error; err != nil {
		t.Fatal(err)
	}
	var plan StructurePlan
	if err := json.Unmarshal([]byte(record.PlanJSON), &plan); err != nil {
		t.Fatal(err)
	}
	oldHash := sha256.Sum256([]byte(strings.Join([]string{strconv.FormatUint(library.ProfileRevision, 10), library.MovieDirectoryTemplate, library.MovieFilenameTemplate, library.TVDirectoryTemplate, library.TVFilenameTemplate, library.RelativeRoot, library.ProviderRootID}, "\x00")))
	plan.RuleFingerprint = hex.EncodeToString(oldHash[:])
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.db.Model(&record).Update("plan_json", string(data)).Error; err != nil {
		t.Fatal(err)
	}
	result := service.runRepair(context.Background(), fastScanTestRuntime{}, record.ID)
	if result.ErrorCode != CodeMediaLibraryStructureBoundaryChanged {
		t.Fatalf("legacy plan was accepted: %+v", result)
	}
}

func TestPlayerVersionLabelsDistinguishFilesWithoutLeakingPaths(t *testing.T) {
	season, episode := 2, 1
	entries := []models.MediaLibraryEntry{
		{ID: 1, RelativePath: "/private-marker/Movie.1080p.h264.DTSHD-MA.mkv"},
		{ID: 2, RelativePath: "/private-marker/Movie.1080p.h264.DTSHD-MA (1).mkv"},
		{ID: 3, RelativePath: "/private-marker/Movie.1080p.h265.AC3.mkv"},
		{ID: 4, RelativePath: "/private-marker/Show.S02E01.1080p.h265.AC3.mkv", MediaType: "tv", Season: &season, Episode: &episode},
	}
	labels := playerVersionLabels(entries)
	if labels[1] == labels[2] || labels[1] == labels[3] || !strings.Contains(labels[1], "H264 DTS-HD MA") || !strings.Contains(labels[3], "H265 AC3") {
		t.Fatalf("labels: %+v", labels)
	}
	if labels[4] != "1080p H265 AC3" {
		t.Fatalf("different episode included in movie version count: %+v", labels)
	}
	for _, label := range labels {
		if strings.Contains(label, "private-marker") || strings.Contains(label, "Movie") {
			t.Fatalf("path leaked: %q", label)
		}
	}
}

func TestPlayerCatalogDetailReturnsAllMovieVersionsWithSafeLabels(t *testing.T) {
	service, library, actor := createCatalogTestLibrary(t)
	if err := service.db.Model(&models.MediaLibrary{}).Where("id = ?", library.ID).Update("enabled", true).Error; err != nil {
		t.Fatal(err)
	}
	for i, suffix := range []string{"1080p h264 DTSHD-MA", "1080p h264 DTSHD-MA (1)", "1080p h265 AC3"} {
		entry := models.MediaLibraryEntry{LibraryID: library.ID, RelativePath: "/private-marker/Movie - " + suffix + ".mkv", ProviderID: fmt.Sprint(i), Title: "Movie", WorkKey: "movie:version-fixture", MediaType: "movie", MatchStatus: "matched"}
		if err := service.db.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
	}
	page, err := service.PlayerCatalog(actor, library.ID, MediaPageQuery{Page: 1, PageSize: 20, MediaType: "movie"})
	if err != nil || len(page.List) != 1 {
		t.Fatalf("catalog: %+v %v", page, err)
	}
	detail, err := service.PlayerCatalogDetail(context.Background(), actor, library.ID, page.List[0].ID)
	if err != nil || len(detail.Versions) != 3 {
		t.Fatalf("detail: %+v %v", detail, err)
	}
	seen := map[string]bool{}
	for _, version := range detail.Versions {
		if version.DisplayTitle != "Movie" || version.VersionName == "" || !strings.Contains(version.Title, version.VersionName) || seen[version.Title] {
			t.Fatalf("ambiguous version: %+v", version)
		}
		seen[version.Title] = true
	}
	data, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-marker") || strings.Contains(string(data), "relative_path") {
		t.Fatalf("private path leaked")
	}
}

func TestStructureDoesNotPreserveWrongEpisodeOrMalformedVersionSuffix(t *testing.T) {
	for _, source := range []string{"剧 - S02E01 - S01E01 1080p", "剧 - S02E01..", "剧 - S02E01 ", "剧 - S02E010 - 1080p", "别的剧 - S02E01 - 1080p"} {
		if structureExistingVersionName(source, "剧 - S02E01") {
			t.Errorf("accepted invalid identity: %s", source)
		}
	}
}

func TestStructurePreservesExistingMovieVersions(t *testing.T) {
	year, id := 2009, int64(20453)
	const title = "三傻大闹宝莱坞"
	const directory = "电影/外语电影/三傻大闹宝莱坞 (2009)"
	library := models.MediaLibrary{ID: 1, MovieDirectoryTemplate: "电影/{category}/{title} ({year})", MovieFilenameTemplate: "{title} ({year})"}
	basenames := []string{
		title + " (2009) - 1080p h264 DTSHD-MA 37.64 G.mkv",
		title + " (2009) 1080p h264 DTS.mkv",
		title + " (2009) 1080p h264 DTSHD-MA.mkv",
		title + " (2009) 1080p h265 AC3.mkv",
	}
	for _, sourceDir := range []string{directory, "旧目录"} {
		t.Run(sourceDir, func(t *testing.T) {
			var entries []models.MediaLibraryEntry
			var assets []models.MediaLibrarySourceAsset
			for i, basename := range basenames {
				source := path.Join(sourceDir, basename)
				entries = append(entries, models.MediaLibraryEntry{ID: uint(i + 1), RelativePath: source, ProviderID: fmt.Sprint(i + 1), MediaType: "movie", Title: title, ReleaseYear: &year, TMDBID: &id, WorkKey: "movie:tmdb:20453", CategoryName: "外语电影", MatchStatus: mediaRecognitionStatusMatched})
				assets = append(assets, models.MediaLibrarySourceAsset{RelativePath: strings.TrimSuffix(source, ".mkv") + ".zh.srt", Active: true})
			}
			plan, err := (StructurePlanner{}).Build(library, entries, assets, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.ConflictGroups) != 0 || len(plan.RecycleItems) != 0 {
				t.Fatalf("versions treated as conflicts: %+v", plan)
			}
			if sourceDir == directory {
				if plan.IssueCount != 0 || len(plan.Items) != 0 {
					t.Fatalf("valid versions need no repair: %+v", plan)
				}
			} else {
				if len(plan.Items) != 8 {
					t.Fatalf("all videos and subtitles must move together: %+v", plan)
				}
				for _, item := range plan.Items {
					if item.TargetRelative != path.Join(directory, path.Base(item.SourceRelative)) {
						t.Fatalf("version label lost: %+v", item)
					}
				}
				for i := range entries {
					entries[i].RelativePath = path.Join(directory, basenames[i])
				}
				for i := range assets {
					assets[i].RelativePath = path.Join(directory, path.Base(assets[i].RelativePath))
				}
				again, err := (StructurePlanner{}).Build(library, entries, assets, "")
				if err != nil || again.IssueCount != 0 {
					t.Fatalf("repair does not converge: %+v %v", again, err)
				}
			}
		})
	}
}

func TestStructureVersionNamesRetainEpisodeAndNumberedVersions(t *testing.T) {
	year, tmdbID := 2024, int64(1)
	library := models.MediaLibrary{MovieDirectoryTemplate: "电影/{title} ({year})", MovieFilenameTemplate: "{title} ({year})", TVDirectoryTemplate: "电视剧/{title} ({year})/Season {season:02}", TVFilenameTemplate: "{title} - S{season:02}E{episode:02}"}
	for _, season := range []int{1, 2} {
		episode := 1
		for _, suffix := range []string{" - 1080p h265 AC3", " (3)", " - 导演评论音轨"} {
			source := fmt.Sprintf("电视剧/剧 (2024)/Season %02d/剧 - S%02dE01%s.mkv", season, season, suffix)
			entry := models.MediaLibraryEntry{RelativePath: source, Title: "剧", SeriesTitle: "剧", MediaType: "tv", ReleaseYear: &year, TMDBID: &tmdbID, Season: &season, Episode: &episode}
			got, err := structureVideoTarget(library, entry)
			if err != nil || got != source {
				t.Errorf("version changed: %s => %s (%v)", source, got, err)
			}
		}
	}
	for _, source := range []string{"电影/片 (2024)/片 (2024).mkv", "电影/片 (2024)/片 (2024) (3).mkv"} {
		got, err := structureVideoTarget(library, models.MediaLibraryEntry{RelativePath: source, Title: "片", MediaType: "movie", ReleaseYear: &year})
		if err != nil || got != source {
			t.Errorf("keep-all output unstable: %s => %s %v", source, got, err)
		}
	}
}
