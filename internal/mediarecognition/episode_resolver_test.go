package mediarecognition

import (
	"fmt"
	"testing"
)

func TestResolvePackageEpisodesSupportsBracketedAnimeCollection(t *testing.T) {
	files := make([]FileFact, 0, 10)
	for episode := 1; episode <= 10; episode++ {
		files = append(files, FileFact{RelativePath: fmt.Sprintf("[Lilith-Raws] Megami-ryou no Ryoubo-kun. [%02dv2][Baha][WEB-DL][1080p][AVC AAC][BIG5][MP4].mp4", episode), Size: 400 << 20})
	}
	resolved := ResolvePackageEpisodes(files, MediaTypeTV)
	if !resolved.Complete || resolved.VideoCount != 10 || resolved.ResolvedCount != 10 {
		t.Fatalf("resolved=%+v", resolved)
	}
	for index, fact := range resolved.Files {
		if fact.Season == nil || *fact.Season != 1 || fact.Episode == nil || *fact.Episode != index+1 {
			t.Fatalf("fact[%d]=%+v", index, fact)
		}
	}
}

func TestResolvePackageEpisodesRejectsTechnicalNumbersAndDuplicates(t *testing.T) {
	for _, files := range [][]FileFact{
		{{RelativePath: "Example [1080p][10bit][1920X1080][2024].mkv", Size: 400 << 20}},
		{{RelativePath: "Show [01][1080p].mkv", Size: 400 << 20}, {RelativePath: "Show copy [01][1080p].mkv", Size: 400 << 20}},
	} {
		if resolved := ResolvePackageEpisodes(files, MediaTypeTV); resolved.Complete {
			t.Fatalf("unsafe package resolved=%+v", resolved)
		}
	}
}

func TestResolvePackageEpisodesStandaloneE(t *testing.T) {
	files := make([]FileFact, 0, 12)
	for episode := 1; episode <= 12; episode++ {
		files = append(files, FileFact{RelativePath: fmt.Sprintf("沉默的真相.The.Long.Night.2020.E%02d.4K.WEB-DL.H265.HDR10.AAC.mp4", episode), Size: 1 << 30})
	}
	resolved := ResolvePackageEpisodes(files, MediaTypeTV)
	if !resolved.Complete || resolved.ResolvedCount != 12 {
		t.Fatalf("resolved=%+v", resolved)
	}
	for i, fact := range resolved.Files {
		if fact.Season == nil || *fact.Season != 1 || fact.Episode == nil || *fact.Episode != i+1 {
			t.Fatalf("fact[%d]=%+v", i, fact)
		}
	}
	for _, name := range []string{"CODE01.1080p.mp4", "Show.HEVC.2020.mp4", "Show.E01bit.mp4", "Show.E2020.mp4"} {
		if result := ResolvePackageEpisodes([]FileFact{{RelativePath: name}}, MediaTypeTV); result.ResolvedCount != 0 {
			t.Fatalf("technical/title token recognized: %s: %+v", name, result)
		}
	}
}
