package services

import (
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestStructurePlannerBuildsMovieAndTVTargetsWithSidecars(t *testing.T) {
	season, episode, year, tmdbID := 2, 3, 2020, int64(100)
	library := models.MediaLibrary{ID: 7, BaselineGeneration: 9, ProfileRevision: 3, RelativeRoot: "/", MovieDirectoryTemplate: "电影/{category}/{title} ({year})", MovieFilenameTemplate: "{title} ({year})", TVDirectoryTemplate: "电视剧/{category}/{title} ({year})/Season {season:02}", TVFilenameTemplate: "{title} - S{season:02}E{episode:02}"}
	entries := []models.MediaLibraryEntry{
		{LibraryID: 7, RelativePath: "/旧电影/七武士.mkv", ProviderID: "movie", Size: 10, MediaType: "movie", Title: "七武士", WorkKey: "movie:tmdb:346", MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, CategoryName: "剧情"},
		{LibraryID: 7, RelativePath: "/斗罗大陆/斗罗大陆.S02E03.mkv", ProviderID: "episode", Size: 20, MediaType: "tv", Title: "斗罗大陆", SeriesTitle: "斗罗大陆", WorkKey: "series:tmdb:100", Season: &season, Episode: &episode, MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, CategoryName: "动画"},
	}
	assets := []models.MediaLibrarySourceAsset{
		{LibraryID: 7, RelativePath: "/斗罗大陆/斗罗大陆.S02E03.zh-CN.srt", ProviderID: "subtitle", ParentProviderID: "old-dir", Size: 2, Active: true},
		{LibraryID: 7, RelativePath: "/斗罗大陆/poster.jpg", ProviderID: "poster", ParentProviderID: "old-dir", Size: 3, Active: true},
	}
	plan, err := (StructurePlanner{}).Build(library, entries, assets, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Generation != 9 || plan.RuleFingerprint == "" || plan.IssueCount != 4 || len(plan.Items) != 4 {
		t.Fatalf("plan=%+v", plan)
	}
	want := map[string]string{
		"旧电影/七武士.mkv":                "电影/剧情/七武士 (2020)/七武士 (2020).mkv",
		"斗罗大陆/斗罗大陆.S02E03.mkv":       "电视剧/动画/斗罗大陆 (2020)/Season 02/斗罗大陆 - S02E03.mkv",
		"斗罗大陆/斗罗大陆.S02E03.zh-CN.srt": "电视剧/动画/斗罗大陆 (2020)/Season 02/斗罗大陆 - S02E03.zh-CN.srt",
		"斗罗大陆/poster.jpg":            "电视剧/动画/斗罗大陆 (2020)/poster.jpg",
	}
	for _, item := range plan.Items {
		if target := want[item.SourceRelative]; item.TargetRelative != target {
			t.Fatalf("%s target=%q want=%q", item.SourceRelative, item.TargetRelative, target)
		}
	}
}

func TestStructurePlannerScopesWorkAndReportsUnrecognized(t *testing.T) {
	season, episode, tmdbID := 1, 1, int64(100)
	library := models.MediaLibrary{ID: 1, ProfileRevision: 1, MovieDirectoryTemplate: "电影/{category}/{title} ({year})", MovieFilenameTemplate: "{title} ({year})", TVDirectoryTemplate: "电视剧/{category}/{title} ({year})/Season {season:02}", TVFilenameTemplate: "{title} - S{season:02}E{episode:02}"}
	entries := []models.MediaLibraryEntry{
		{RelativePath: "/bad/file.mkv", MediaType: "movie", Title: "未知", WorkKey: "file:bad", MatchStatus: mediaRecognitionStatusUnrecognized},
		{RelativePath: "/old/show.mkv", ProviderID: "show", MediaType: "tv", Title: "剧", SeriesTitle: "剧", WorkKey: "series:tmdb:100", Season: &season, Episode: &episode, MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, CategoryName: "其他"},
	}
	full, err := (StructurePlanner{}).Build(library, entries, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if full.Unrecognized != 1 || full.IssueCount != 2 || len(full.Items) != 1 {
		t.Fatalf("full=%+v", full)
	}
	scoped, err := (StructurePlanner{}).Build(library, entries, nil, "series:tmdb:100")
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Unrecognized != 0 || scoped.IssueCount != 1 || len(scoped.Items) != 1 {
		t.Fatalf("scoped=%+v", scoped)
	}
}
