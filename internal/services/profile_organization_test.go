package services

import (
	"testing"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func TestTypeFirstSnapshotsSeparateMovieAndTVWithSameCategory(t *testing.T) {
	year, season, episode := 2026, 1, 2
	manifest := downloadpkg.Manifest{Complete: true, Files: []downloadpkg.File{{RelativePath: "Example.S01E02.mkv", Size: minimumAutomaticTransferVideoBytes}}}
	tests := []struct {
		name string
		task models.DownloadTask
		want string
	}{
		{
			name: "movie",
			task: models.DownloadTask{ScrapeMediaType: "movie", ScrapeTitle: "Example", ScrapeCategory: "动画", ScrapeYear: &year, MovieDirectoryTemplate: defaultMovieDirectoryTemplate, MovieFilenameTemplate: defaultMovieFilenameTemplate},
			want: "电影/动画/Example (2026)/Example (2026).mkv",
		},
		{
			name: "tv",
			task: models.DownloadTask{ScrapeMediaType: "tv", ScrapeTitle: "Example", ScrapeCategory: "动画", ScrapeYear: &year, ScrapeSeason: &season, ScrapeEpisode: &episode, TVDirectoryTemplate: defaultTVDirectoryTemplate, TVFilenameTemplate: defaultTVFilenameTemplate},
			want: "电视剧/动画/Example (2026)/Season 01/Example - S01E02.mkv",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			targets, err := buildTransferTargets(test.task, manifest)
			if err != nil {
				t.Fatal(err)
			}
			if len(targets) != 1 || targets[0].Relative != test.want {
				t.Fatalf("targets=%+v want=%q", targets, test.want)
			}
		})
	}
}
