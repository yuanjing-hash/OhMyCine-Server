package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
)

func observationHotSet(t *testing.T, f observationFixture, id string, values map[string]any) {
	t.Helper()
	if err := observationCurrent(f.db, f.scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id = ?", id).Updates(values).Error; err != nil {
		t.Fatal(err)
	}
}

func observationHotCoolFixture(t *testing.T, f observationFixture) {
	t.Helper()
	f.advance(31 * time.Minute)
	if err := observationCurrent(f.db, f.scope.ID).Model(&models.ProviderObservationItem{}).Where("is_dir = ?", true).
		Updates(map[string]any{"hot_until": nil, "due_at": f.clock.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
}

func observationHotRequire(t *testing.T, f observationFixture, id string, changedAt, due time.Time) models.ProviderObservationItem {
	t.Helper()
	row := f.item(t, id)
	if row.HotUntil == nil || !row.HotUntil.Equal(changedAt.Add(30*time.Minute)) || !row.DueAt.Equal(due) {
		t.Fatalf("%s: hot_until=%v due=%v; want hot=%v due=%v", id, row.HotUntil, row.DueAt, changedAt.Add(30*time.Minute), due)
	}
	return row
}

func TestProviderObservationChangedFilesHeatParent(t *testing.T) {
	for _, parent := range []string{"root", "season"} {
		for _, change := range []string{"new", "updated", "renamed", "restored"} {
			t.Run(parent+"/"+change, func(t *testing.T) {
				f := newObservationFixture(t)
				if parent != "root" {
					f.reader.tree["root"] = []ObservationFact{obsDir(parent, "root", "Season")}
				}
				f.reader.tree[parent] = []ObservationFact{obsFile("episode", parent, "Episode.mkv")}
				f.round(t)
				observationHotCoolFixture(t, f)
				switch change {
				case "new":
					f.reader.tree[parent] = append(f.reader.tree[parent], obsFile("next", parent, "Next.mkv"))
				case "updated":
					f.reader.tree[parent][0].Size++
				case "renamed":
					f.reader.tree[parent][0].Name = "Renamed.mkv"
				case "restored":
					observationHotSet(t, f, "episode", map[string]any{"downlisted_at": *f.clock})
				}
				before := f.item(t, parent)
				observationHotSet(t, f, parent, map[string]any{"due_at": *f.clock})
				f.step(t)
				changedAt := *f.clock
				row := observationHotRequire(t, f, parent, changedAt, changedAt.Add(2*time.Minute))
				if row.ActiveBatchID != "" || row.QueueRound != before.QueueRound || row.CompletedRound != before.CompletedRound {
					t.Fatal("heating changed coverage state or failed to finish the page")
				}
				// Check eligibility through the real scheduler, not only stored timestamps.
				*f.clock = row.DueAt.Add(-time.Millisecond)
				if _, err := f.s.nextDirectory(context.Background(), f.scope, *f.clock); !errors.Is(err, ErrObservationDeferred) {
					t.Fatalf("directory eligible too early: %v", err)
				}
				*f.clock = row.DueAt
				next, err := f.s.nextDirectory(context.Background(), f.scope, *f.clock)
				if err != nil || next.ResourceID != parent || !observationIsHot(f.scope, next, *f.clock) {
					t.Fatalf("changed parent not scheduled hot after two minutes: %s %v", next.ResourceID, err)
				}
			})
		}
	}
}

func TestProviderObservationHotParentNoOpAndExpiry(t *testing.T) {
	for _, parent := range []string{"root", "season"} {
		t.Run(parent, func(t *testing.T) {
			f := newObservationFixture(t)
			if parent != "root" {
				f.reader.tree["root"] = []ObservationFact{obsDir(parent, "root", "Season")}
			}
			f.reader.tree[parent] = []ObservationFact{obsFile("episode", parent, "Episode.mkv")}
			f.round(t)
			observationHotCoolFixture(t, f)
			observationHotSet(t, f, parent, map[string]any{"due_at": *f.clock})
			f.reader.tree[parent][0].Size++
			f.step(t)
			changedAt := *f.clock
			*f.clock = changedAt.Add(2 * time.Minute)
			if _, err := f.s.ObserveNext(context.Background(), f.scope.ID, f.reader); err != nil {
				t.Fatal(err)
			}
			observationHotRequire(t, f, parent, changedAt, f.clock.Add(2*time.Minute))
			*f.clock = changedAt.Add(30*time.Minute + time.Second)
			if _, err := f.s.ObserveNext(context.Background(), f.scope.ID, f.reader); err != nil {
				t.Fatal(err)
			}
			interval := 24 * time.Hour
			if parent == "root" {
				interval = 15 * time.Minute
			}
			observationHotRequire(t, f, parent, changedAt, f.clock.Add(interval))
			if _, err := f.s.nextDirectory(context.Background(), f.scope, *f.clock); !errors.Is(err, ErrObservationDeferred) {
				t.Fatalf("completed directory retained an expired due time and spins: %v", err)
			}
		})
	}
}

func TestProviderObservationMoveHeatsParentsPreservesEarlierPage(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{obsDir("old", "root", "Old"), obsDir("new", "root", "New")}
	f.reader.tree["old"] = []ObservationFact{obsFile("episode", "old", "Episode.mkv"), obsFile("other", "old", "Other.mkv")}
	f.round(t)
	observationHotCoolFixture(t, f)
	f.reader.pageSize = 1
	observationHotSet(t, f, "old", map[string]any{"due_at": *f.clock})
	f.step(t) // Old parent page zero; leave page one and its queue intact.
	old := f.item(t, "old")
	if old.ActiveBatchID == "" {
		t.Fatal("fixture did not start a paginated traversal")
	}
	due := f.clock.Add(5 * time.Second)
	observationHotSet(t, f, "old", map[string]any{"due_at": due})
	observationHotSet(t, f, "new", map[string]any{"due_at": *f.clock})
	f.reader.tree["new"] = []ObservationFact{obsFile("episode", "new", "Episode.mkv")}
	f.step(t)
	changedAt := *f.clock
	currentOld := observationHotRequire(t, f, "old", changedAt, due)
	observationHotRequire(t, f, "new", changedAt, changedAt.Add(2*time.Minute))
	if currentOld.ActiveBatchID != old.ActiveBatchID || currentOld.QueueRound != old.QueueRound || currentOld.CompletedRound != old.CompletedRound {
		t.Fatal("move replaced the old parent's active page or coverage queue")
	}
	var batch models.ProviderObservationBatch
	if err := f.db.First(&batch, "id = ?", old.ActiveBatchID).Error; err != nil || batch.Offset != 1 || batch.State != "reading" {
		t.Fatalf("old parent cursor lost: %+v %v", batch, err)
	}
	*f.clock = due
	if _, err := f.s.ObserveNext(context.Background(), f.scope.ID, f.reader); err != nil {
		t.Fatal(err)
	}
	if f.reader.calls[len(f.reader.calls)-1] != "old" || f.reader.offsets[len(f.reader.offsets)-1] != 1 {
		t.Fatalf("scheduler did not resume the preserved page: %v %v", f.reader.calls, f.reader.offsets)
	}
	if item := f.item(t, "episode"); item.ParentID != "new" {
		t.Fatal("finishing the old snapshot moved the identity back")
	}
	observationHotRequire(t, f, "old", changedAt, f.clock.Add(2*time.Minute))
}

func TestProviderObservationMoveAdvancesColdParentCheck(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{obsDir("old", "root", "Old"), obsDir("new", "root", "New")}
	f.reader.tree["old"] = []ObservationFact{obsFile("episode", "old", "Episode.mkv")}
	f.round(t)
	observationHotCoolFixture(t, f)
	observationHotSet(t, f, "new", map[string]any{"due_at": *f.clock})
	f.reader.tree["old"] = nil
	f.reader.tree["new"] = []ObservationFact{obsFile("episode", "new", "Episode.mkv")}
	f.step(t)
	changedAt := *f.clock
	observationHotRequire(t, f, "old", changedAt, changedAt.Add(2*time.Minute))
	observationHotRequire(t, f, "new", changedAt, changedAt.Add(2*time.Minute))
	*f.clock = changedAt.Add(2 * time.Minute)
	if _, err := f.s.ObserveNext(context.Background(), f.scope.ID, f.reader); err != nil {
		t.Fatal(err)
	}
	f.step(t) // Both parents are due; the scheduler checks each in turn.
	calls := f.reader.calls[len(f.reader.calls)-2:]
	if calls[0] != "new" || calls[1] != "old" {
		t.Fatalf("move left an old parent cold: %v", calls)
	}
	observationHotRequire(t, f, "old", changedAt, f.clock.Add(2*time.Minute))
}

func TestProviderObservationExcludedChangesDoNotHeatParent(t *testing.T) {
	for _, kind := range []string{"reserved_name", "excluded_identity"} {
		t.Run(kind, func(t *testing.T) {
			f := newObservationFixture(t)
			f.reader.tree["root"] = []ObservationFact{obsFile("fixture", "root", ".omc-quark-verify-fixture")}
			f.round(t)
			observationHotCoolFixture(t, f)
			if kind == "reserved_name" {
				f.reader.tree["root"] = append(f.reader.tree["root"], obsFile("new-fixture", "root", ".omc-verify-new"))
			} else {
				f.reader.tree["root"][0].Name = "OrdinaryName.mkv"
				f.reader.tree["root"][0].Size++
			}
			observationHotSet(t, f, "root", map[string]any{"due_at": *f.clock})
			f.step(t)
			root := f.item(t, "root")
			if root.HotUntil != nil || !root.DueAt.Equal(f.clock.Add(15*time.Minute)) {
				t.Fatalf("excluded change disturbed parent cadence: hot=%v due=%v", root.HotUntil, root.DueAt)
			}
			*f.clock = f.clock.Add(2 * time.Minute)
			if _, err := f.s.nextDirectory(context.Background(), f.scope, *f.clock); !errors.Is(err, ErrObservationDeferred) {
				t.Fatalf("excluded change scheduled a hot check: %v", err)
			}
		})
	}
}

func TestProviderObservationPartialPageHeatsWithoutDelayingContinuation(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.pageSize = 1
	f.reader.tree["root"] = []ObservationFact{obsFile("first", "root", "First.mkv"), obsFile("last", "root", "Last.mkv")}
	due := f.item(t, "root").DueAt
	f.step(t)
	firstAt := *f.clock
	first := observationHotRequire(t, f, "root", firstAt, due)
	if first.ActiveBatchID == "" || first.CompletedRound != 0 {
		t.Fatal("partial page incorrectly completed the directory")
	}
	f.step(t)
	last := observationHotRequire(t, f, "root", *f.clock, f.clock.Add(2*time.Minute))
	if last.ActiveBatchID != "" || f.reader.offsets[len(f.reader.offsets)-1] != 1 {
		t.Fatal("hot scheduling reset or delayed page continuation")
	}
}

func TestProviderObservationCompletionPreservesEarlierSupplementaryCheck(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{obsFile("episode", "root", "Episode.mkv")}
	f.round(t)
	observationHotCoolFixture(t, f)
	receipt := models.QuarkOperationReceipt{ID: "operation", OperationKey: "operation", ConnectionID: f.scope.ConnectionID, ScopeID: f.scope.ID, State: "confirmed"}
	if err := f.db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	f.reader.tree["root"][0].Size++
	if _, err := f.s.RecordVerifiedOperation(context.Background(), receipt.ID, f.scope.ID, "root", f.reader.tree["root"]); err != nil {
		t.Fatal(err)
	}
	changedAt := *f.clock
	observationHotRequire(t, f, "root", changedAt, changedAt.Add(5*time.Second))
	*f.clock = changedAt.Add(5 * time.Second)
	// A new supplementary deadline is scheduled while the current page is in
	// flight. Completion must retain that future deadline and the new hot state.
	var earlier time.Time
	f.reader.hook = func(_ string, _ int64) (ObservationDirectoryPage, error, bool) {
		earlier = f.clock.Add(5 * time.Second)
		observationHotSet(t, f, "root", map[string]any{"due_at": earlier})
		return ObservationDirectoryPage{}, nil, false
	}
	if _, err := f.s.ObserveNext(context.Background(), f.scope.ID, f.reader); err != nil {
		t.Fatal(err)
	}
	observationHotRequire(t, f, "root", changedAt, earlier)
	*f.clock = earlier
	next, err := f.s.nextDirectory(context.Background(), f.scope, *f.clock)
	if err != nil || next.ResourceID != "root" {
		t.Fatalf("earlier supplementary check was postponed: %s %v", next.ResourceID, err)
	}
}
