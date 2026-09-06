package services

import (
	"reflect"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/medialibrary"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestCatalogRecognitionGroupingReusesOnlyPurePhysicalEvidence(t *testing.T) {
	baseline := catalogScanBaseline{head: models.CatalogHead{LibraryID: 1, SourceEpoch: 1, SourceFingerprint: "source", ConfigFingerprint: "config"}}
	for i, path := range []string{"Show (2024)/Season 02/01.mkv", "Show (2024)/Season 02/02.mkv"} {
		baseline.entries = append(baseline.entries, models.MediaLibraryEntry{ID: uint(i + 1), LibraryID: 1, RelativePath: path, ProviderID: path, Size: 10, ModifiedAt: time.Unix(100, 0), MatchStatus: mediaRecognitionStatusPending})
	}
	cached := &catalogRecognitionGrouping{}
	check := func() {
		t.Helper()
		got, gfiles, gremaining := cached.pendingBatch(baseline)
		want, wfiles, wremaining := pendingCatalogRecognitionBatch(baseline)
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gfiles, wfiles) || gremaining != wremaining {
			t.Fatal("cached grouping diverged from fresh physical parse and current manual state")
		}
	}
	check()
	first := &cached.units[0]
	originalKey := first.SourceKey
	baseline.head.Revision++ // Physical compaction must not invalidate pure input.
	check()
	if first != &cached.units[0] {
		t.Fatal("head-only change reparsed all filenames")
	}
	id := uint(30)
	baseline.recognitions = []models.MediaLibraryRecognition{{ID: id, LibraryID: 1, SourceKey: "reorganization:manual-selected-file", ManualOverride: true, MediaType: "tv", Title: "Corrected"}}
	baseline.entries[0].RecognitionID = &id
	check()
	if first != &cached.units[0] || first.SourceKey != originalKey {
		t.Fatal("manual result mutated/rebuilt cached pure grouping")
	}
	baseline.entries[0].MatchStatus = mediaRecognitionStatusMatched
	check()
	if first != &cached.units[0] {
		t.Fatal("recognition progress reparsed physical input")
	}
	for _, change := range []func(){
		func() { baseline.entries[1].RelativePath = "Show (2024)/Season 03/01.mkv" },
		func() { baseline.entries[1].ProviderID = "replaced-provider" },
		func() { baseline.entries[1].Size++ },
		func() { baseline.entries[1].ModifiedAt = baseline.entries[1].ModifiedAt.Add(time.Second) },
		func() { baseline.entries[1].ID++ },
		func() { baseline.head.SourceEpoch++ },
		func() { baseline.head.SourceFingerprint = "new-source" },
		func() { baseline.head.ConfigFingerprint = "new-config" },
	} {
		previous := &cached.units[0]
		change()
		check()
		if previous == &cached.units[0] {
			t.Fatal("changed physical/source/config evidence reused stale parser result")
		}
	}
	// Preserve actual folder season; never default a bare 01 under Season03 to S1.
	for _, unit := range cached.groups(baseline) {
		for _, file := range unit.Files {
			if file.RelativePath == "Show (2024)/Season 03/01.mkv" {
				parsed := medialibrary.ParseMedia("01.mkv", file.RelativePath)
				if parsed.Season == nil || *parsed.Season != 3 || parsed.Episode == nil || *parsed.Episode != 1 {
					t.Fatal("season context changed")
				}
			}
		}
	}
}
