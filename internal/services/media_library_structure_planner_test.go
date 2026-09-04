package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestStructurePlannerLargeFixtureUses128WorkersWithinBudget(t *testing.T) {
	library := models.MediaLibrary{ID: 1, BaselineGeneration: 4, ProfileRevision: 1, MovieDirectoryTemplate: "电影/{category}/{title} ({year})", MovieFilenameTemplate: "{title} ({year})", TVDirectoryTemplate: "电视剧/{category}/{title} ({year})/Season {season:02}", TVFilenameTemplate: "{title} - S{season:02}E{episode:02}"}
	entries := make([]models.MediaLibraryEntry, 0, 12171)
	assets := make([]models.MediaLibrarySourceAsset, 0, 12171)
	year := 2026
	for index := 0; index < 12171; index++ {
		title := fmt.Sprintf("影片 %05d", index)
		directory := fmt.Sprintf("待整理/%05d", index)
		tmdbID := int64(index + 1)
		entries = append(entries, models.MediaLibraryEntry{RelativePath: "/" + directory + "/" + title + ".mkv", ProviderID: fmt.Sprintf("video-%d", index), MediaType: "movie", Title: title, WorkKey: fmt.Sprintf("movie:tmdb:%d", tmdbID), MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, CategoryName: "剧情"})
		assets = append(assets, models.MediaLibrarySourceAsset{RelativePath: "/" + directory + "/" + title + ".zh-CN.srt", ProviderID: fmt.Sprintf("sidecar-%d", index), Name: title + ".zh-CN.srt", Active: true})
	}
	startedWorkers, activeWorkers, maxActive := 0, 0, 0
	var mu sync.Mutex
	planner := StructurePlanner{observe: func(event string) {
		mu.Lock()
		defer mu.Unlock()
		switch event {
		case "worker_started":
			startedWorkers++
		case "task_started":
			activeWorkers++
			if activeWorkers > maxActive {
				maxActive = activeWorkers
			}
		case "task_finished":
			activeWorkers--
		}
	}}
	started := time.Now()
	plan, err := planner.BuildContext(context.Background(), library, entries, assets, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 10*time.Second {
		t.Fatalf("large structure planning took %s", elapsed)
	}
	if startedWorkers != StructurePlanningWorkers || maxActive <= 1 {
		t.Fatalf("workers started=%d max_active=%d", startedWorkers, maxActive)
	}
	if plan.CheckedItems != 24342 || len(plan.Items) != 24342 || plan.IssueCount != 24342 {
		t.Fatalf("checked=%d items=%d issues=%d", plan.CheckedItems, len(plan.Items), plan.IssueCount)
	}
}

func TestStructurePlannerIsolatesDataIssuesAndEntireConflictGroups(t *testing.T) {
	year, tmdbID := 2020, int64(100)
	library := models.MediaLibrary{ID: 1, BaselineGeneration: 2, ProfileRevision: 1, MovieDirectoryTemplate: "电影/{category}/{title} ({year})", MovieFilenameTemplate: "{title} ({year})", TVDirectoryTemplate: "电视剧/{category}/{title} ({year})/Season {season:02}", TVFilenameTemplate: "{title} - S{season:02}E{episode:02}"}
	entries := []models.MediaLibraryEntry{
		{RelativePath: "/old/a.mkv", ProviderID: "a", MediaType: "movie", Title: "同名", WorkKey: "movie:a", MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, CategoryName: "剧情"},
		{RelativePath: "/old/b.mkv", ProviderID: "b", MediaType: "movie", Title: "同名", WorkKey: "movie:b", MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, CategoryName: "剧情"},
		{RelativePath: "/old/Film.mkv", ProviderID: "film", MediaType: "movie", Title: "影片", WorkKey: "movie:film", MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, ReleaseYear: &year, CategoryName: "剧情"},
		{RelativePath: "/old/show.mkv", ProviderID: "show", MediaType: "tv", Title: "剧集", WorkKey: "series:show", MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, CategoryName: "剧情"},
		{RelativePath: "/../../escape.mkv", ProviderID: "bad", MediaType: "movie", Title: "越界", WorkKey: "movie:bad", MatchStatus: mediaRecognitionStatusMatched, TMDBID: &tmdbID, CategoryName: "剧情"},
	}
	assets := []models.MediaLibrarySourceAsset{
		{RelativePath: "/old/Film.zh.srt", ProviderID: "sidecar-a", Name: "Film.zh.srt", Active: true},
		{RelativePath: "/old/film.zh.srt", ProviderID: "sidecar-b", Name: "film.zh.srt", Active: true},
	}
	plan, err := (StructurePlanner{}).Build(library, entries, assets, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Classifications.DuplicateTarget != 2 || plan.Classifications.SidecarConflict != 2 || plan.Classifications.MissingEpisode != 1 || plan.Classifications.InvalidPath != 1 {
		t.Fatalf("classifications=%+v issues=%+v", plan.Classifications, plan.Issues)
	}
	for _, code := range []string{"duplicate_target", "sidecar_target_conflict"} {
		found := false
		for _, issue := range plan.Issues {
			if issue.Code != code {
				continue
			}
			found = true
			if issue.ConflictSourceCount != 2 || len(issue.ConflictSources) != 2 {
				t.Fatalf("%s conflict group is not inspectable: %+v", code, issue)
			}
		}
		if !found {
			t.Fatalf("missing %s issue", code)
		}
	}
	for _, item := range plan.Items {
		if strings.Contains(item.SourceRelative, "a.mkv") || strings.Contains(item.SourceRelative, "b.mkv") || strings.HasSuffix(strings.ToLower(item.SourceRelative), ".zh.srt") {
			t.Fatalf("conflicting action escaped isolation: %+v", item)
		}
	}
}

func TestStructureConflictSourceSummaryIsBounded(t *testing.T) {
	candidates := make([]structurePlanCandidate, maxStructureConflictSourceSamples+5)
	members := make([]int, len(candidates))
	for index := range candidates {
		candidates[index] = structurePlanCandidate{source: fmt.Sprintf("冲突/%02d/poster.jpg", index)}
		members[index] = index
	}
	sources := boundedStructureConflictSources(candidates, members)
	if len(sources) != maxStructureConflictSourceSamples {
		t.Fatalf("sources=%d want=%d", len(sources), maxStructureConflictSourceSamples)
	}
}

func TestStructureConflictSourceSummaryRejectsUnsafePaths(t *testing.T) {
	sources := sanitizeStructureConflictSources([]string{"安全/海报.jpg", "../越界.jpg", `C:\\私有\\海报.jpg`})
	if len(sources) != 1 || sources[0] != "安全/海报.jpg" {
		t.Fatalf("unsafe conflict sources escaped sanitization: %+v", sources)
	}
}

func TestStructureTargetConflictClassifiesRecognitionCatalogAndPhysicalCauses(t *testing.T) {
	tests := []struct {
		name       string
		candidates []structurePlanCandidate
		want       string
	}{
		{
			name: "unrelated Chinese works incorrectly bound to one TMDB identity",
			candidates: []structurePlanCandidate{
				{kind: "video", source: "电影/动画电影/吉卜力工作室特别短片合辑/吉卜力工作室特别短片合辑.mp4", title: "电影人", providerID: "ghibli", recognitionID: 11},
				{kind: "video", source: "电影/动画电影/蜡笔小新：爆睡！梦世界大作战/蜡笔小新：爆睡！梦世界大作战.mp4", title: "电影人", providerID: "crayon-dream", recognitionID: 12},
			},
			want: "recognition_suspect_conflict",
		},
		{
			name: "shared franchise prefix cannot hide a different subtitle",
			candidates: []structurePlanCandidate{
				{kind: "video", source: "电影/动画电影/蜡笔小新：功夫小子之拉面大作战/蜡笔小新：功夫小子之拉面大作战.mp4", title: "蜡笔小新：呼风唤雨！夕阳下的春日部男孩", providerID: "kung-fu", recognitionID: 21},
				{kind: "video", source: "电影/动画电影/蜡笔小新：呼风唤雨！夕阳下的春日部男孩/蜡笔小新：呼风唤雨！夕阳下的春日部男孩.mp4", title: "蜡笔小新：呼风唤雨！夕阳下的春日部男孩", providerID: "sunset", recognitionID: 22},
			},
			want: "recognition_suspect_conflict",
		},
		{
			name: "same provider fact duplicated in catalog",
			candidates: []structurePlanCandidate{
				{kind: "video", source: "电视剧/白日提灯/Season 1/白日提灯 S01E23.mp4", title: "白日提灯", providerID: "episode-23", recognitionID: 31},
				{kind: "video", source: "电视剧/白日提灯/Season 01/白日提灯 - S01E23.mp4", title: "白日提灯", providerID: "episode-23", recognitionID: 31},
			},
			want: "catalog_duplicate_conflict",
		},
		{
			name: "two real physical versions",
			candidates: []structurePlanCandidate{
				{kind: "video", source: "电影/白日提灯/白日提灯.mkv", title: "白日提灯", providerID: "version-a", recognitionID: 41},
				{kind: "video", source: "电影/白日提灯/白日提灯.mp4", title: "白日提灯", providerID: "version-b", recognitionID: 42},
			},
			want: "duplicate_target",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			members := make([]int, len(test.candidates))
			for index := range members {
				members[index] = index
			}
			if got := structureTargetConflictCode(test.candidates, members); got != test.want {
				t.Fatalf("conflict code=%q want=%q", got, test.want)
			}
		})
	}
}

func TestStructurePlannerCancellationAboveBoundedBufferDoesNotDeadlock(t *testing.T) {
	entries := make([]models.MediaLibraryEntry, 4096)
	for index := range entries {
		entries[index] = models.MediaLibraryEntry{RelativePath: fmt.Sprintf("/bulk/%05d.mkv", index), MatchStatus: mediaRecognitionStatusUnrecognized}
	}
	for iteration := 0; iteration < 25; iteration++ {
		ctx, cancel := context.WithCancel(context.Background())
		var once sync.Once
		planner := StructurePlanner{observe: func(event string) {
			if event == "task_started" {
				once.Do(cancel)
			}
		}}
		done := make(chan error, 1)
		go func() {
			_, err := planner.BuildContext(ctx, models.MediaLibrary{}, entries, nil, "", nil)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("iteration=%d err=%v", iteration, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("planner deadlocked after cancellation at iteration %d", iteration)
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

func TestStructurePlannerDoesNotTreatPendingRecognitionAsUnrecognized(t *testing.T) {
	library := models.MediaLibrary{ID: 1, ProfileRevision: 1}
	entries := []models.MediaLibraryEntry{
		{RelativePath: "/pending/等待后台识别.mkv", MediaType: "movie", Title: "等待后台识别", WorkKey: "file:pending", MatchStatus: mediaRecognitionStatusPending},
		{RelativePath: "/legacy/等待后台识别.mkv", MediaType: "movie", Title: "等待后台识别", WorkKey: "file:legacy"},
		{RelativePath: "/failed/确实没有匹配.mkv", MediaType: "movie", Title: "确实没有匹配", WorkKey: "file:failed", MatchStatus: mediaRecognitionStatusUnrecognized},
	}
	plan, err := (StructurePlanner{}).Build(library, entries, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.CheckedItems != len(entries) || plan.Unrecognized != 1 || plan.Classifications.Unrecognized != 1 || plan.IssueCount != 1 {
		t.Fatalf("pending recognition leaked into actionable diagnostics: %+v", plan)
	}
	if len(plan.Issues) != 1 || plan.Issues[0].Title != "确实没有匹配" || plan.Issues[0].Code != "media_unrecognized" {
		t.Fatalf("unexpected issue sample: %+v", plan.Issues)
	}
}

func TestStructureIssueSamplesStayBoundedAndRepresentEveryPresentClass(t *testing.T) {
	plan := StructurePlan{}
	codes := []string{"missing_season_episode", "path_mismatch", "media_unrecognized", "invalid_path", "template_unavailable", "recognition_suspect_conflict", "catalog_duplicate_conflict", "duplicate_target", "sidecar_target_conflict"}
	for _, code := range codes {
		for index := 0; index < 200; index++ {
			plan.addIssue(StructureIssue{Code: code, Kind: "video", CurrentPath: fmt.Sprintf("%s/%03d.mkv", code, index), Repairable: code == "path_mismatch"})
		}
	}
	if len(plan.Issues) != maxStructureIssueSamples {
		t.Fatalf("sample count=%d want=%d", len(plan.Issues), maxStructureIssueSamples)
	}
	seen := make(map[string]int, len(codes))
	for _, issue := range plan.Issues {
		seen[issue.Code]++
	}
	for _, code := range codes {
		if seen[code] == 0 {
			t.Fatalf("issue class %q was hidden by earlier high-cardinality samples: %+v", code, seen)
		}
	}
}
