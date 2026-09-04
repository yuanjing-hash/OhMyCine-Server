package medialibrary

import "testing"

func TestParseMediaGroupsPlayerCompatibleEpisodeLayouts(t *testing.T) {
	tests := []struct {
		name, path, title string
		season, episode   int
	}{
		{"Lycoris.Recoil.S01E01.mkv", "/Lycoris Recoil/Season 01/Lycoris.Recoil.S01E01.mkv", "Lycoris Recoil", 1, 1},
		{"EP02.mkv", "/Lycoris Recoil/S01/EP02.mkv", "Lycoris Recoil", 1, 2},
		{"Lycoris.Recoil.2x03.mkv", "/Lycoris Recoil/Lycoris.Recoil.2x03.mkv", "Lycoris Recoil", 2, 3},
		{"第十二话.mkv", "/动画/示例动画/第2季/第十二话.mkv", "示例动画", 2, 12},
		{"名侦探柯南.S01E1210.mkv", "/名侦探柯南/Season 01/名侦探柯南.S01E1210.mkv", "名侦探柯南", 1, 1210},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			parsed := ParseMedia(test.name, test.path)
			if parsed.MediaType != "tv" || parsed.SeriesTitle != test.title || parsed.Title != test.title || parsed.Season == nil || *parsed.Season != test.season || parsed.Episode == nil || *parsed.Episode != test.episode {
				t.Fatalf("parsed=%+v", parsed)
			}
		})
	}
}

func TestParseMediaAcceptsSxxExxAdjacentToChineseText(t *testing.T) {
	name := "舌尖上的中国S02E02更多资源-XH1080.com.mp4"
	parsed := ParseMedia(name, "/电视剧/纪录片/舌尖上的中国/Season 2/"+name)
	if parsed.MediaType != "tv" || parsed.Title != "舌尖上的中国" || parsed.Season == nil || *parsed.Season != 2 || parsed.Episode == nil || *parsed.Episode != 2 {
		t.Fatalf("parsed=%+v", parsed)
	}
}

func TestParseMediaDoesNotTreatSxxExxEmbeddedInASCIIWordAsMarker(t *testing.T) {
	name := "HOUSE01E02B.mkv"
	parsed := ParseMedia(name, "/"+name)
	if parsed.MediaType != "movie" || parsed.Season != nil || parsed.Episode != nil {
		t.Fatalf("parsed=%+v", parsed)
	}
}

func TestParseMediaUsesSeasonDirectoryForTrailingBareEpisode(t *testing.T) {
	parsed := ParseMedia("哆啦A梦 01.mp4", "/哆啦A梦 (2005)/Season 1/哆啦A梦 01.mp4")
	if parsed.MediaType != "tv" || parsed.Title != "哆啦A梦" || parsed.SeriesTitle != "哆啦A梦" || parsed.Year == nil || *parsed.Year != 2005 || parsed.Season == nil || *parsed.Season != 1 || parsed.Episode == nil || *parsed.Episode != 1 {
		t.Fatalf("parsed=%+v", parsed)
	}

	special := ParseMedia("哆啦A梦 01.mp4", "/哆啦A梦 (2005)/Specials/哆啦A梦 01.mp4")
	if special.MediaType != "tv" || special.Season == nil || *special.Season != 0 || special.Episode == nil || *special.Episode != 1 {
		t.Fatalf("special=%+v", special)
	}

	doubleDot := ParseMedia("知否知否应是绿肥红瘦 02..mp4", "/电视剧/国产剧/知否知否应是绿肥红瘦 (2018)/Season 1/知否知否应是绿肥红瘦 02..mp4")
	if doubleDot.MediaType != "tv" || doubleDot.Title != "知否知否应是绿肥红瘦" || doubleDot.Year == nil || *doubleDot.Year != 2018 || doubleDot.Season == nil || *doubleDot.Season != 1 || doubleDot.Episode == nil || *doubleDot.Episode != 2 {
		t.Fatalf("double dot=%+v", doubleDot)
	}

	for _, test := range []struct {
		folder  string
		season  int
		episode int
	}{
		{folder: "Season 1", season: 1, episode: 1},
		{folder: "Season 02", season: 2, episode: 1},
		{folder: "第3季", season: 3, episode: 1},
		{folder: "Specials", season: 0, episode: 1},
	} {
		t.Run(test.folder+" keeps directory season", func(t *testing.T) {
			parsed := ParseMedia("示例剧 01..mp4", "/示例剧/"+test.folder+"/示例剧 01..mp4")
			if parsed.Season == nil || *parsed.Season != test.season || parsed.Episode == nil || *parsed.Episode != test.episode {
				t.Fatalf("parsed=%+v want=S%02dE%02d", parsed, test.season, test.episode)
			}
		})
	}
}

func TestParseMediaDoesNotGuessBareEpisodeWithoutSeasonContext(t *testing.T) {
	for _, name := range []string{"作品 2005.mp4", "作品 1080p.mp4", "作品 10bit.mp4"} {
		t.Run(name, func(t *testing.T) {
			parsed := ParseMedia(name, "/"+name)
			if parsed.MediaType != "movie" || parsed.Episode != nil || parsed.Season != nil {
				t.Fatalf("parsed=%+v", parsed)
			}
		})
	}

	for _, name := range []string{"作品 2005.mp4", "作品 2005..mp4", "作品 1080p..mp4", "作品 10bit..mp4"} {
		t.Run("season context "+name, func(t *testing.T) {
			parsed := ParseMedia(name, "/作品/Season 1/"+name)
			if parsed.Episode != nil {
				t.Fatalf("technical number was guessed as episode: %+v", parsed)
			}
		})
	}
}

func TestParseMediaUsesTitleYearFolderForMovie(t *testing.T) {
	parsed := ParseMedia("Movie.2024.2160p.WEB-DL.mkv", "/电影/Movie (2024)/Movie.2024.2160p.WEB-DL.mkv")
	if parsed.MediaType != "movie" || parsed.Title != "Movie" || parsed.SeriesTitle != "" || parsed.Season != nil || parsed.Episode != nil {
		t.Fatalf("parsed=%+v", parsed)
	}
}

func TestParseTrailingBracketEpisodeDefaultsToSeasonOne(t *testing.T) {
	name := `[jibaketa合成&音频压制][ViuTV粤语]超人 / 超人力霸王奥米加 / 奥美迦奥特曼 / Ultraman Omega - 09 [粤语+无字幕] (WEB 1920x1080 AVC AAC YUE).mkv`
	parsed := ParseMedia(name, "/"+name)
	if parsed.MediaType != "tv" || parsed.Season == nil || *parsed.Season != 1 || parsed.Episode == nil || *parsed.Episode != 9 {
		t.Fatalf("parsed=%+v", parsed)
	}

	negative := ParseMedia("Catch-22 (2019).mkv", "/Catch-22 (2019).mkv")
	if negative.MediaType != "movie" || negative.Episode != nil {
		t.Fatalf("negative=%+v", negative)
	}
}

func TestWorkKeyGroupsSeriesButKeepsMoviesByIdentity(t *testing.T) {
	first := ParseMedia("Lycoris.Recoil.S01E01.mkv", "/Lycoris Recoil/Season 01/Lycoris.Recoil.S01E01.mkv")
	second := ParseMedia("Lycoris.Recoil.S02E01.mkv", "/Lycoris Recoil/Season 02/Lycoris.Recoil.S02E01.mkv")
	if WorkKey(first, "episode-1") != WorkKey(second, "episode-2") {
		t.Fatal("same series produced different work keys")
	}
	movie := ParseMedia("Movie.mkv", "/Movie.mkv")
	if WorkKey(movie, "movie-1") == WorkKey(movie, "movie-2") {
		t.Fatal("different movie files produced the same work key")
	}
}
