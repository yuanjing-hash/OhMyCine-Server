package medialibrary

import (
	"fmt"
	"testing"
	"time"
)

func TestGroupRecognitionUnitsKeepsRootMoviesSeparateAndTVSeasonTogether(t *testing.T) {
	now := time.Date(2026, 8, 20, 1, 2, 3, 0, time.UTC)
	files := []File{
		{RelativePath: "/Seven.Samurai.1954.mkv", Size: 10, ModifiedAt: now},
		{RelativePath: "/Ikiru.1952.mkv", Size: 11, ModifiedAt: now},
		{RelativePath: "/Shogun/Season 01/Shogun.S01E01.mkv", Size: 12, ModifiedAt: now},
		{RelativePath: "/Shogun/Season 01/Shogun.S01E02.mkv", Size: 13, ModifiedAt: now},
	}
	units := GroupRecognitionUnits(files)
	if len(units) != 3 {
		t.Fatalf("units=%d, want 3: %#v", len(units), units)
	}
	var tv *RecognitionUnit
	for index := range units {
		if units[index].MediaTypeHint == "tv" {
			tv = &units[index]
		}
	}
	if tv == nil || len(tv.Files) != 2 || tv.PackageName != "Shogun" {
		t.Fatalf("tv unit=%#v", tv)
	}
}

func TestGroupRecognitionUnitsUsesEverySupportedSeasonDirectoryAsTVContext(t *testing.T) {
	for _, seasonDirectory := range []string{"Season 1", "S1", "第1季", "Specials"} {
		t.Run(seasonDirectory, func(t *testing.T) {
			files := []File{
				{RelativePath: "/哆啦A梦 (2005)/" + seasonDirectory + "/哆啦A梦 01.mp4"},
				{RelativePath: "/哆啦A梦 (2005)/" + seasonDirectory + "/哆啦A梦 02.mp4"},
			}
			units := GroupRecognitionUnits(files)
			if len(units) != 1 || units[0].PackageName != "哆啦A梦 (2005)" || units[0].MediaTypeHint != "tv" {
				t.Fatalf("units=%+v", units)
			}
		})
	}
}

func TestGroupRecognitionUnitsUsesDiscOuterDirectoryAndStableProviderIDs(t *testing.T) {
	now := time.Now().UTC()
	first := GroupRecognitionUnits([]File{{RelativePath: "/Seven Samurai/BDMV/STREAM/00000.m2ts", ProviderID: "cloud-1", ProviderIDStable: true, Size: 10, ModifiedAt: now}})
	second := GroupRecognitionUnits([]File{{RelativePath: "/Renamed/BDMV/STREAM/00000.m2ts", ProviderID: "cloud-1", ProviderIDStable: true, Size: 10, ModifiedAt: now}})
	if len(first) != 1 || first[0].PackageName != "Seven Samurai" {
		t.Fatalf("first=%#v", first)
	}
	if first[0].SourceKey != second[0].SourceKey {
		t.Fatalf("provider source key changed across rename: %s != %s", first[0].SourceKey, second[0].SourceKey)
	}
}

func TestGroupRecognitionUnitsBoundsWorkEvidenceAndKeepsIdentityWhenEpisodeIsAdded(t *testing.T) {
	now := time.Date(2026, 9, 4, 1, 2, 3, 0, time.UTC)
	files := make([]File, 0, 3866)
	for episode := 1; episode <= 3866; episode++ {
		files = append(files, File{
			RelativePath:     fmt.Sprintf("/哆啦A梦 (2005)/Season 01/哆啦A梦 %04d.mp4", episode),
			ProviderID:       fmt.Sprintf("provider-%04d", episode),
			ProviderIDStable: true,
			Size:             int64(episode),
			ModifiedAt:       now,
		})
	}
	before := GroupRecognitionUnits(files[:3865])
	after := GroupRecognitionUnits(files)
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("units before=%d after=%d", len(before), len(after))
	}
	if len(after[0].Files) != 3866 {
		t.Fatalf("projection files=%d, want 3866", len(after[0].Files))
	}
	if len(after[0].EvidenceFiles) == 0 || len(after[0].EvidenceFiles) > MaxRecognitionEvidenceFiles {
		t.Fatalf("evidence files=%d", len(after[0].EvidenceFiles))
	}
	if before[0].SourceKey != after[0].SourceKey || before[0].InputFingerprint != after[0].InputFingerprint {
		t.Fatalf("work identity changed after adding an episode: before=%s/%s after=%s/%s", before[0].SourceKey, before[0].InputFingerprint, after[0].SourceKey, after[0].InputFingerprint)
	}
	if after[0].PackageName != "哆啦A梦 (2005)" || after[0].MediaTypeHint != "tv" {
		t.Fatalf("unit=%+v", after[0])
	}
}
