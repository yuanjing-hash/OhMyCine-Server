package mediarecognition

import "testing"

func TestDecoratedShareTitlesKeepIdentityAndEpisodeFacts(t *testing.T) {
	for _, tt := range []struct {
		raw, title  string
		first, last int
	}{
		{"🎬 嫁金钗 (2026) 1-14", "嫁金钗", 1, 14},
		{"📺 嫁金钗 (2026) S01E11 ✨4K WEB-DL AAC", "嫁金钗", 11, 11},
		{"🎬 嫁金钗 (2026) 1-10", "嫁金钗", 1, 10},
		{"🌟 Other Story (2025) S02E03 🎞️ 1080p WEB-DL H.264 AAC 2.0", "Other Story", 3, 3},
		{"✨ 长安二十四时（2024） S01E09 2160p", "长安二十四时", 9, 9},
	} {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := Parse(InputFacts{PackageName: tt.raw, SourceKind: SourceDownload})
			if err != nil || got.CanonicalTitle != tt.title || got.Episodes.EpisodeMin == nil || *got.Episodes.EpisodeMin != tt.first || got.Episodes.EpisodeMax == nil || *got.Episodes.EpisodeMax != tt.last || (got.Year == nil && got.SeasonYear == nil) {
				t.Fatalf("parse=%+v err=%v", got, err)
			}
		})
	}
}

func TestDecoratedTitleCleanupPreservesMeaningfulPunctuation(t *testing.T) {
	for _, title := range []string{"Love (Actually)", "1917", "3 Body Problem", "WALL-E", "Amélie"} {
		got, err := Parse(InputFacts{PackageName: title, SourceKind: SourceDownload})
		if err != nil || got.CanonicalTitle != title {
			t.Fatalf("title=%q got=%q err=%v", title, got.CanonicalTitle, err)
		}
	}
}
