package medialibrary

import (
	"fmt"
	"path"
	"testing"
)

func TestGroupRichSeasonReleasesUseStableOuterWork(t *testing.T) {
	var files []File
	var key, fingerprint string
	for _, season := range []int{0, 1, 4, 7, 9} {
		for episode := 1; episode <= 2; episode++ {
			relative := fmt.Sprintf("/电视剧/欧美剧/24小时 (2001)/24小时.S%02d.%d.1080p.BluRay.x265/24小时.S%02dE%02d.1080p.mkv", season, 2001+season, season, episode)
			files = append(files, File{RelativePath: relative, ProviderID: relative, ProviderIDStable: true})
			parsed := ParseMedia(path.Base(relative), relative)
			if parsed.Season == nil || *parsed.Season != season || parsed.Episode == nil || *parsed.Episode != episode {
				t.Fatalf("per-file identity changed: %+v", parsed)
			}
		}
		units := GroupRecognitionUnits(files)
		if len(units) != 1 || units[0].PackageName != "24小时 (2001)" || units[0].MediaTypeHint != "tv" || len(units[0].Files) != len(files) {
			t.Fatalf("split season release: %+v", units)
		}
		if key != "" && (key != units[0].SourceKey || fingerprint != units[0].InputFingerprint) {
			t.Fatal("later season membership changed work identity")
		}
		key, fingerprint = units[0].SourceKey, units[0].InputFingerprint
	}
	for index := range files {
		files[index].ProviderIDStable = false
	}
	local := GroupRecognitionUnits(files)
	if local[0].SourceKey != key || local[0].InputFingerprint != fingerprint {
		t.Fatal("grouping depends on provider")
	}
}

func TestGroupSeasonReleaseRequiresCorroboration(t *testing.T) {
	for _, tc := range []struct {
		directory, file string
		climb           bool
	}{
		{"Example.Series.S04.2005.1080p", "Example.Series.S04E01.mkv", true},
		{"Example Series Season 4 2005 1080p", "Example.Series.S04E01.mkv", true},
		{"Example Series 第四季 2005 1080p", "Example.Series.S04E01.mkv", true},
		{"Example.Series.S04.2005.1080p", "Other.S01E01.mkv", false},
		{"Example.Series.S04.2005.1080p", "Example.Series.S05E01.mkv", false},
		{"Behind.S04.2005.1080p", "Behind.S04E01.mkv", false},
		{"Example.Series.S04E01.2005", "Example.Series.S04E01.mkv", false},
		{"Example.Series.S04.E01.2005", "Example.Series.S04E01.mkv", false},
		{"Example.Series.S04.S05.2005", "Example.Series.S04E01.mkv", false},
		{"Example.Series.S04-05.2005", "Example.Series.S04E01.mkv", false},
		{"Example.Series.AS04.2005", "Example.Series.S04E01.mkv", false},
		{"Example.Series.S04A.2005", "Example.Series.S04E01.mkv", false},
		{"Example.Series.S-4.2005", "Example.Series.S04E01.mkv", false},
		{"Example.Series.2004.1080p", "Example.Series.S04E01.mkv", false},
		{"Example.Series.S04.2005", "Example.Series.2005.mkv", false},
	} {
		t.Run(tc.directory+"/"+tc.file, func(t *testing.T) {
			units := GroupRecognitionUnits([]File{{RelativePath: "/电视剧/Example Series (2001)/" + tc.directory + "/" + tc.file}})
			want := tc.directory
			if tc.climb {
				want = "Example Series (2001)"
			}
			if len(units) != 1 || units[0].PackageName != want {
				t.Fatalf("units=%+v want=%s", units, want)
			}
		})
	}
	units := GroupRecognitionUnits([]File{
		{RelativePath: "/电视剧/欧美剧/Alpha.S04.2005/Alpha.S04E01.mkv"},
		{RelativePath: "/电视剧/欧美剧/Beta.S04.2005/Beta.S04E01.mkv"},
		{RelativePath: "/电视剧/Alpha (2001)/Alpha.S04.2005/Alpha.S04E01.mkv"},
		{RelativePath: "/电视剧/Beta (2001)/Beta.S04.2005/Beta.S04E01.mkv"},
	})
	if len(units) != 4 {
		t.Fatalf("neighboring works merged: %+v", units)
	}
	parsed := ParseMedia("Series.S04E01.mkv", "/Series.S04E01.mkv")
	if isWorkSeasonReleaseDirectory("TV.S04.2005", "TV", parsed) {
		t.Fatal("reserved ancestor used as work")
	}
}

func TestGroupSeasonReleaseSupportsGenericTitleAndSeparatorForms(t *testing.T) {
	for _, tc := range []struct {
		name, outer, directory, file string
	}{
		{"short season", "Harbor Watch (2018)", "Harbor.Watch.S1.2018.1080p", "Harbor.Watch.S01E01.mkv"},
		{"zero-padded season", "Harbor Watch (2018)", "Harbor_Watch_S01_2018_1080p", "Harbor.Watch.S01E01.mkv"},
		{"season word spaces", "Harbor Watch (2018)", "Harbor Watch Season 2 2019 1080p", "Harbor.Watch.S02E01.mkv"},
		{"season word dots", "Harbor Watch (2018)", "Harbor.Watch.Season.2.2019.1080p", "Harbor.Watch.S02E01.mkv"},
		{"season word underscores", "Harbor Watch (2018)", "Harbor_Watch_Season_2_2019_1080p", "Harbor.Watch.S02E01.mkv"},
		{"special season zero", "Harbor Watch (2018)", "Harbor.Watch.S00.2018.1080p", "Harbor.Watch.S00E01.mkv"},
		{"title contains digits", "Station 19 (2018)", "Station.19.S03.2020.1080p", "Station.19.S03E01.mkv"},
		{"chinese season", "山海纪 (2018)", "山海纪 第三季 2020 1080p", "山海纪S03E01.mkv"},
		{"japanese title", "深夜食堂 (2009)", "深夜食堂.S02.2011.1080p", "深夜食堂S02E01.mkv"},
		{"korean title", "비밀의 숲 (2017)", "비밀의_숲_S02_2020_1080p", "비밀의 숲 S02E01.mkv"},
		{"numeric title 1923", "1923 (2022)", "1923.S02.2025.1080p", "1923.S02E01.mkv"},
		{"numeric title 1883", "1883 (2021)", "1883.S01.2021.1080p", "1883.S01E01.mkv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			units := GroupRecognitionUnits([]File{{RelativePath: "/电视剧/分类/" + tc.outer + "/" + tc.directory + "/" + tc.file}})
			if len(units) != 1 || units[0].PackageName != tc.outer || units[0].MediaTypeHint != "tv" {
				t.Fatalf("units=%+v, want outer work %q", units, tc.outer)
			}
		})
	}
}

func TestGroupNumericSeasonReleaseRequiresIndependentPremiereYear(t *testing.T) {
	for _, tc := range []struct {
		name, outer, directory string
	}{
		{"bare numeric category", "1923", "1923.S02.2025.1080p"},
		{"different numeric title", "1883 (2021)", "1923.S02.2025.1080p"},
		{"unbounded suffix", "1923 (2022) Collection", "1923.S02.2025.1080p"},
		{"numeric directory episode marker", "1923 (2022)", "1923.S02E01.2025.1080p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := "/电视剧/" + tc.outer + "/" + tc.directory + "/1923.S02E01.mkv"
			units := GroupRecognitionUnits([]File{{RelativePath: file}})
			if len(units) != 1 || units[0].PackageName != tc.directory {
				t.Fatalf("units=%+v, numeric ancestor must fail closed", units)
			}
		})
	}
}

func TestGroupSeasonReleasePreservesEstablishedNonRichShapes(t *testing.T) {
	files := []File{
		{RelativePath: "/Root.Show.S01E01.mkv"},
		{RelativePath: "/Standard Show (2020)/Season 02/Standard.Show.S02E01.mkv"},
		{RelativePath: "/Film.S04.Release/Film.2020.mkv"},
		{RelativePath: "/Disc Film/BDMV/STREAM/00001.m2ts"},
		{RelativePath: "/DVD Film/VIDEO_TS/VTS_01_1.VOB"},
	}
	units := GroupRecognitionUnits(files)
	packages := map[string]bool{}
	for _, unit := range units {
		packages[unit.PackageName] = true
	}
	for _, expected := range []string{"Root Show", "Standard Show (2020)", "Film.S04.Release", "Disc Film", "DVD Film"} {
		if !packages[expected] {
			t.Fatalf("missing unchanged package %q: %+v", expected, units)
		}
	}
}
