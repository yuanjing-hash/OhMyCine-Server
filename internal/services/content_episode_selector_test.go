package services

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
)

func episodePreview(t *testing.T, paths ...string) siteContentPreview {
	t.Helper()
	s := NewSiteService(nil, nil, nil, nil, zerolog.Nop())
	claim := siteResultClaim{Title: "Advertisement E01-E99", SearchIdentity: analysisIdentity()}
	entries := analysisEntries(paths...)
	analysis, files := s.analyzeShareEntries(context.Background(), Actor{}, claim, entries)
	result := siteContentPreview{Entries: map[string]SiteSharePreviewEntry{}, AnalysisFiles: files, Summary: ContentPreviewSummary{Complete: true, Analysis: analysis}}
	byToken := map[string]siteShareAnalyzedFile{}
	for _, file := range files {
		byToken[file.Token] = file
	}
	for _, entry := range entries {
		result.Entries[entry.Token] = entry
		result.Summary.Files = append(result.Summary.Files, contentFileProjection(entry, claim, analysis, byToken))
	}
	return result
}
func TestContentEpisodeSelectorUsesActualSparseLeavesAndSubtitleEvidence(t *testing.T) {
	preview := episodePreview(t, "Fixture Show [01-52]/Season 01/01.mkv", "Fixture Show [01-52]/Season 01/13.mkv", "Fixture Show [01-52]/Season 01/13.zh-Hans.srt", "Fixture Show [01-52]/Season 01/Fixture.Show.S01E15.1080p.CHS.mkv", "Fixture Show [01-52]/Season 01/16.mkv", "Other Work/Season 01/14.mkv", "Fixture Show/Season 02/14.mkv", "Fixture Show/Season 01/S01E14-E16.mkv", "Fixture Show/Season 01/S01E14.zip")
	prefer := selectMissingContent(preview, analysisIdentity(), 1, []int{13, 15}, SubtitlePolicy{Mode: "prefer", Languages: []string{"zh-Hans", "zh-Hant"}})
	if !reflect.DeepEqual(prefer.Episodes, []int{13, 15}) || !reflect.DeepEqual(prefer.EntryTokens, []string{"1", "2", "3"}) || prefer.UnconfirmedSubtitles != 1 {
		t.Fatalf("prefer=%+v", prefer)
	}
	required := selectMissingContent(preview, analysisIdentity(), 1, []int{13, 15}, SubtitlePolicy{Mode: "require", Languages: []string{"zh-Hans"}})
	if !reflect.DeepEqual(required.Episodes, []int{13}) || !reflect.DeepEqual(required.EntryTokens, []string{"1", "2"}) {
		t.Fatalf("release claim became confirmed subtitle: %+v", required)
	}
	empty := selectMissingContent(preview, analysisIdentity(), 1, []int{14}, SubtitlePolicy{Mode: "ignore"})
	if len(empty.Episodes) != 0 {
		t.Fatalf("foreign/year/archive/range escaped missing set: %+v", empty)
	}
}
func TestContentEpisodeSelectorPrefersConfirmedLanguageAlternative(t *testing.T) {
	// Distinct actual encode stems pair the subtitle with one alternative.
	preview := episodePreview(t, "Fixture Show/Season 01/Fixture.Show.S01E13.1080p.mkv", "Fixture Show/Season 01/Fixture.Show.S01E13.2160p.mkv", "Fixture Show/Season 01/Fixture.Show.S01E13.2160p.zh-Hans.ass")
	got := selectMissingContent(preview, analysisIdentity(), 1, []int{13}, SubtitlePolicy{Mode: "prefer", Languages: []string{"zh-Hans", "zh-Hant"}})
	if !reflect.DeepEqual(got.EntryTokens, []string{"1", "2"}) || got.UnconfirmedSubtitles != 0 {
		t.Fatalf("alternative=%+v", got)
	}
}

func TestContentEpisodeSelectorNeverDuplicatesCoordinatesAcrossPhysicalLeaves(t *testing.T) {
	preview := episodePreview(t, "Fixture Show/Season 01/Fixture.Show.S01E13.1080p.mkv", "Fixture Show/Season 01/Fixture.Show.S01E13.1080p.zh-Hans.ass", "Fixture Show/Season 01/Fixture.Show.S01E13-E14.2160p.mkv")
	got := selectMissingContent(preview, analysisIdentity(), 1, []int{13, 14}, SubtitlePolicy{Mode: "prefer", Languages: []string{"zh-Hans"}})
	if !reflect.DeepEqual(got.EntryTokens, []string{"0", "1"}) || !reflect.DeepEqual(got.Episodes, []int{13}) {
		t.Fatalf("overlapping alternative was selected or unselected E14 claimed: %+v", got)
	}
}
func TestContentPreviewRetainedBytesProtectSelectionsAndRotateSmallPages(t *testing.T) {
	s := &SiteService{now: time.Now}
	state := s.contents()
	expiry := time.Now().Add(time.Minute)
	pinned := siteContentPreview{ActorID: 1, ExpiresAt: expiry, Read: inspectedContent{Manifest: &resourcecontent.Manifest{MetainfoBytes: make([]byte, 15<<20)}}, Summary: ContentPreviewSummary{Token: "pinned", Complete: true}}
	if err := s.storeContentPreviewLocked(state, "pinned", pinned); err != nil {
		t.Fatal(err)
	}
	state.selections["selection-pinned"] = siteContentSelection{ActorID: 1, PreviewToken: "pinned", ExpiresAt: expiry}
	oversized := pinned
	oversized.Read.Manifest = &resourcecontent.Manifest{MetainfoBytes: make([]byte, 18<<20)}
	if err := s.storeContentPreviewLocked(state, "too-large-together", oversized); err == nil {
		t.Fatal("actor retained byte cap ignored")
	}
	if _, ok := state.previews["pinned"]; !ok {
		t.Fatal("valid selection preview evicted")
	}
	for n := 0; n < 100; n++ {
		key := strings.Repeat("x", n+1)
		if err := s.storeContentPreviewLocked(state, key, siteContentPreview{ActorID: 1, ExpiresAt: expiry, Summary: ContentPreviewSummary{Token: key, Complete: true}}); err != nil {
			t.Fatalf("small resource %d permanently failed: %v", n, err)
		}
	}
	if count, _ := contentActorPreviewSize(state.previews, 1); count > contentActorPreviewLimit {
		t.Fatal("count cap ignored")
	}
	state.previews["pinned"] = siteContentPreview{ActorID: 1, ExpiresAt: time.Now().Add(-time.Second)}
	state.selections["selection-pinned"] = siteContentSelection{ActorID: 1, PreviewToken: "pinned", ExpiresAt: time.Now().Add(-time.Second)}
	if err := s.storeContentPreviewLocked(state, "after-expiry", oversized); err != nil {
		t.Fatalf("expired retained bytes never released: %v", err)
	}
	if _, ok := state.previews["pinned"]; ok {
		t.Fatal("expired preview retained")
	}
}
