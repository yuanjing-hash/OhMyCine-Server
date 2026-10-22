package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func TestProviderObservationRemovedQueuedSubtreeCompletesCoverage(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{obsDir("show", "root", "Show")}
	f.reader.tree["show"] = []ObservationFact{obsDir("season", "show", "Season")}
	f.reader.tree["season"] = []ObservationFact{obsFile("episode", "season", "Episode.mkv")}
	f.round(t)
	f.advance(11 * time.Minute)
	// Control which request races with the external removal: the root is
	// observed first, and its freshly queued child disappears before its read.
	if err := observationCurrent(f.db, f.scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id <> 'root' AND is_dir = ?", true).Update("due_at", f.clock.Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.RequestFullSync(context.Background(), f.scope.ID); err != nil {
		t.Fatal(err)
	}
	f.step(t) // Root found Show and queued its subtree in this round.
	if f.reader.calls[len(f.reader.calls)-1] != "root" {
		t.Fatal("fixture did not observe root first")
	}
	delete(f.reader.tree, "show")
	delete(f.reader.tree, "season")
	f.reader.tree["root"] = nil
	f.reader.hook = func(id string, _ int64) (ObservationDirectoryPage, error, bool) {
		if id == "show" || id == "season" {
			return ObservationDirectoryPage{}, errors.New("provider directory gone"), true
		}
		return ObservationDirectoryPage{}, nil, false
	}
	// A failed child list must not remove anything. A fresh, complete parent
	// observation may establish that this branch is currently unreachable.
	f.advance(2 * time.Second)
	if _, err := f.s.ObserveNext(context.Background(), f.scope.ID, f.reader); err == nil {
		t.Fatal("missing child read unexpectedly succeeded")
	}
	if err := observationCurrent(f.db, f.scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id='root'").Update("due_at", *f.clock).Error; err != nil {
		t.Fatal(err)
	}
	f.step(t)
	for i := 0; i < 12; i++ {
		var scope models.ProviderObservationScope
		if err := f.db.First(&scope, "id=?", f.scope.ID).Error; err != nil {
			t.Fatal(err)
		}
		if scope.LastCompleteAt != nil && !scope.LastCompleteAt.Before(scope.CoverageStartedAt) {
			break
		}
		if i == 11 {
			t.Fatal("complete parent absence left an unreachable queued directory blocking coverage")
		}
		f.step(t)
	}
	if row := f.item(t, "episode"); row.DownlistedAt != nil {
		t.Fatal("one absent scope prematurely removed descendant")
	}
	f.advance(11 * time.Minute)
	f.round(t)
	for _, id := range []string{"show", "season", "episode"} {
		if row := f.item(t, id); row.DownlistedAt == nil {
			t.Fatalf("confirmed absent subtree %s was never removed: seenRound=%d missing=%d since=%v last=%v batch=%s absentRound=%d", id, row.LastSeenRound, row.MissingObservations, row.MissingSince, row.MissingLastAt, row.MissingBatchID, row.AbsentParentRound)
		}
	}
	// Restoration retains the original durable identity graph and resumes
	// its descendants instead of leaving them suppressed by parent absence.
	f.reader.hook = nil
	f.reader.tree["root"] = []ObservationFact{obsDir("show", "root", "Show")}
	f.reader.tree["show"] = []ObservationFact{obsDir("season", "show", "Season")}
	f.reader.tree["season"] = []ObservationFact{obsFile("episode", "season", "Episode.mkv")}
	f.advance(2 * time.Second)
	f.round(t)
	if row := f.item(t, "episode"); row.DownlistedAt != nil {
		t.Fatal("restored subtree stayed hidden")
	}
}

func TestProviderObservationMovedSubtreeRequeuesBeforeCoverage(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{obsDir("show", "root", "Show"), obsDir("destination", "root", "Destination")}
	f.reader.tree["show"] = []ObservationFact{obsFile("episode", "show", "Episode.mkv")}
	f.round(t)
	f.advance(11 * time.Minute)
	f.reader.tree["root"] = []ObservationFact{obsDir("destination", "root", "Destination")}
	f.reader.tree["destination"] = []ObservationFact{obsDir("show", "destination", "Show")}
	if err := observationCurrent(f.db, f.scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id <> 'root' AND is_dir = ?", true).Update("due_at", f.clock.Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.RequestFullSync(context.Background(), f.scope.ID); err != nil {
		t.Fatal(err)
	}
	f.step(t)
	if f.item(t, "show").AbsentParentRound == 0 {
		t.Fatal("old parent did not contribute complete absence evidence")
	}
	var scope models.ProviderObservationScope
	if err := f.db.First(&scope, "id=?", f.scope.ID).Error; err != nil {
		t.Fatal(err)
	}
	if scope.LastCompleteAt != nil && !scope.LastCompleteAt.Before(scope.CoverageStartedAt) {
		t.Fatal("scope finished before the possible destination was inspected")
	}
	for i := 0; i < 12; i++ {
		f.step(t)
		if err := f.db.First(&scope, "id=?", f.scope.ID).Error; err != nil {
			t.Fatal(err)
		}
		if scope.LastCompleteAt != nil && !scope.LastCompleteAt.Before(scope.CoverageStartedAt) {
			break
		}
		if i == 11 {
			t.Fatal("rediscovered branch did not complete coverage")
		}
	}
	for _, id := range []string{"show", "episode"} {
		row := f.item(t, id)
		if row.AbsentParentRound != 0 || row.DownlistedAt != nil || row.MissingSince != nil {
			t.Fatalf("moved identity %s remained missing", id)
		}
	}
	if row := f.item(t, "episode"); row.RelativePath != "/Destination/Show/Episode.mkv" {
		t.Fatalf("moved subtree did not refresh descendant path: %s", row.RelativePath)
	}
}
