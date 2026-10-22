package services

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"testing"
)

func numericExtensionAnalysis(paths ...string) (*SiteShareAnalysis, []siteShareAnalyzedFile, []SiteSharePreviewEntry, *siteShareIdentity) {
	season := 1
	identity := &siteShareIdentity{TMDBID: 410, MediaType: "tv", Title: "Fixture Show", Season: &season}
	entries := make([]SiteSharePreviewEntry, 0, len(paths))
	for i, relative := range paths {
		entries = append(entries, SiteSharePreviewEntry{Token: fmt.Sprint(i), Path: relative, Name: path.Base(relative), Size: int64(i + 1)})
	}
	// This is a filename/context regression, not a TMDB or receiver check.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	analysis, files := (&SiteService{}).analyzeShareEntries(ctx, Actor{}, siteResultClaim{Title: "Advertisement 1-99", SearchIdentity: identity}, entries)
	return analysis, files, entries, identity
}

func TestShareNumberedLeafExtensionTrailingSeparators(t *testing.T) {
	for _, basename := range []string{"14.mkv", "14..mp4", "14...mkv", "14_.mp4", "14-.mp4", "14 .mp4", "14._- .mkv", "014..MKV", "15..mp4"} {
		t.Run(basename, func(t *testing.T) {
			relative := "Fixture Show(81)/" + basename
			analysis, files, entries, identity := numericExtensionAnalysis(relative)
			expected := 14
			if basename == "15..mp4" {
				expected = 15
			}
			if len(files) != 1 || files[0].Path != relative || !reflect.DeepEqual(files[0].Episodes, []int{expected}) {
				t.Fatalf("actual numbered leaf was not preserved: files=%+v", files)
			}
			if analysis.UnresolvedCount != 0 || len(analysis.Groups) != 1 || analysis.Groups[0].Status != mediaRecognitionStatusMatched || !reflect.DeepEqual(analysis.Groups[0].Episodes, []int{expected}) {
				t.Fatalf("numbered-leaf analysis=%+v groups=%+v", analysis, analysis.Groups)
			}
			tokens, episodes := selectAnalyzedFollowFiles(SiteSharePreviewSummary{Entries: entries, ShareAnalysis: analysis}, files, identity, 1, []int{expected})
			if !reflect.DeepEqual(tokens, []string{"0"}) || !reflect.DeepEqual(episodes, []int{expected}) {
				t.Fatalf("numbered leaf was not selected: tokens=%v episodes=%v", tokens, episodes)
			}
		})
	}
}

func TestShareNumberedLeafExtensionRejectsAmbiguousStems(t *testing.T) {
	for _, basename := range []string{"14.5..mp4", "14.0..mp4", "14-15..mp4", "14_name..mp4", "name14..mp4", "2026..mp4", "1917..mp4", "1080..mp4", "2160p..mp4", "0..mp4", "201..mp4", "999..mp4", "-14..mp4", "+14..mp4", "..mp4", "14.sample..mp4", "14..zip"} {
		t.Run(basename, func(t *testing.T) {
			analysis, files, entries, identity := numericExtensionAnalysis("Fixture Show(81)/" + basename)
			for _, file := range files {
				if len(file.Episodes) != 0 {
					t.Fatalf("ambiguous stem asserted episode facts: %+v", file)
				}
			}
			tokens, episodes := selectAnalyzedFollowFiles(SiteSharePreviewSummary{Entries: entries, ShareAnalysis: analysis}, files, identity, 1, []int{1, 14, 15, 200})
			if len(tokens) != 0 || len(episodes) != 0 {
				t.Fatalf("ambiguous stem selected: tokens=%v episodes=%v", tokens, episodes)
			}
		})
	}
}

func TestShareNumberedLeafExtensionRetainsContextFences(t *testing.T) {
	for _, relative := range []string{"14..mp4", "Other Work/14..mp4", "Other Work/Fixture Show/14..mp4", "Fixture Show/S02/14..mp4", "Fixture Show/S01/S02/14..mp4", "Fixture Show/sample/14..mp4", "Fixture Show/BDMV/14..mp4"} {
		t.Run(relative, func(t *testing.T) {
			analysis, files, entries, identity := numericExtensionAnalysis(relative)
			tokens, episodes := selectAnalyzedFollowFiles(SiteSharePreviewSummary{Entries: entries, ShareAnalysis: analysis}, files, identity, 1, []int{14})
			if len(tokens) != 0 || len(episodes) != 0 {
				t.Fatalf("unproven target context selected: tokens=%v episodes=%v groups=%+v", tokens, episodes, analysis.Groups)
			}
		})
	}
	// An advertisement does not supply the selected season for numeric leaves.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	entries := []SiteSharePreviewEntry{{Token: "a", Path: "Fixture Show/14..mp4", Name: "14..mp4", Size: 1}}
	analysis, files := (&SiteService{}).analyzeShareEntries(ctx, Actor{}, siteResultClaim{Title: "Fixture Show S01E01-E99"}, entries)
	if len(analysis.Groups) != 1 || analysis.Groups[0].Season != nil || analysis.Groups[0].Status == mediaRecognitionStatusMatched || analysis.UnresolvedCount != 1 || !reflect.DeepEqual(analysis.Groups[0].Episodes, []int{14}) {
		t.Fatalf("advertisement replaced unknown season/identity: analysis=%+v files=%+v", analysis, files)
	}
}

func TestShareNumberedLeafExtensionPreservesSparseSetAndBothFormats(t *testing.T) {
	analysis, files, entries, identity := numericExtensionAnalysis("Fixture Show(81)/10.mkv", "Fixture Show(81)/11.mkv", "Fixture Show(81)/12.mkv", "Fixture Show(81)/14..mp4", "Fixture Show(81)/15..mp4", "Fixture Show(81)/15.mkv", "Fixture Show(81)/S01E13.zip")
	if analysis.VideoCount != 6 || analysis.IgnoredCount != 1 || analysis.UnresolvedCount != 0 || len(analysis.Groups) != 1 || !reflect.DeepEqual(analysis.Groups[0].Episodes, []int{10, 11, 12, 14, 15}) {
		t.Fatalf("sparse actual analysis=%+v groups=%+v", analysis, analysis.Groups)
	}
	tokens, episodes := selectAnalyzedFollowFiles(SiteSharePreviewSummary{Entries: entries, ShareAnalysis: analysis}, files, identity, 1, []int{11, 12, 13, 14, 15})
	if !reflect.DeepEqual(episodes, []int{11, 12, 14, 15}) || !reflect.DeepEqual(tokens, []string{"1", "2", "3", "4", "5"}) {
		t.Fatalf("exact missing leaves/formats: tokens=%v episodes=%v", tokens, episodes)
	}
}
