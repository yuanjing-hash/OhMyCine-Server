package services

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/database"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

type observationTestPublication struct {
	ID         uint `gorm:"primaryKey"`
	BatchID    string
	Kind       string
	ResourceID string
	Path       string
}
type observationTestPublisher struct {
	db     *gorm.DB
	before func()
	fail   bool
	calls  int
}

func (p *observationTestPublisher) PublishObservation(_ context.Context, input ObservationPublication, ack func(*gorm.DB) error) error {
	p.calls++
	if p.before != nil {
		p.before()
	}
	return p.db.Transaction(func(tx *gorm.DB) error {
		for _, c := range input.Changes {
			if err := tx.Create(&observationTestPublication{BatchID: input.BatchID, Kind: c.Kind, ResourceID: c.Fact.ID, Path: c.RelativePath}).Error; err != nil {
				return err
			}
		}
		if p.fail {
			return errors.New("publication fixture failure")
		}
		return ack(tx)
	})
}

type observationTestReader struct {
	tree     map[string][]ObservationFact
	pageSize int
	calls    []string
	offsets  []int64
	hook     func(string, int64) (ObservationDirectoryPage, error, bool)
}

func (r *observationTestReader) ObserveDirectory(_ context.Context, _ models.ProviderObservationScope, id string, offset int64, limit int) (ObservationDirectoryPage, error) {
	r.calls = append(r.calls, id)
	r.offsets = append(r.offsets, offset)
	if r.hook != nil {
		if p, e, ok := r.hook(id, offset); ok {
			return p, e
		}
	}
	items := r.tree[id]
	total := int64(len(items))
	if offset > total {
		return ObservationDirectoryPage{}, errors.New("offset past end")
	}
	if r.pageSize > 0 {
		limit = r.pageSize
	}
	end := min(int64(limit)+offset, total)
	return ObservationDirectoryPage{Items: items[offset:end], Offset: offset, HasMore: end < total, Total: &total}, nil
}

type observationFixture struct {
	db        *gorm.DB
	s         *ProviderObservationService
	scope     models.ProviderObservationScope
	clock     *time.Time
	reader    *observationTestReader
	publisher *observationTestPublisher
}

func newObservationFixture(t *testing.T) observationFixture {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "observation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	t.Cleanup(func() { _ = sql.Close() })
	if err = db.AutoMigrate(&models.ProviderObservationScope{}, &models.ProviderObservationBatch{}, &models.ProviderObservationItem{}, &models.ProviderObservationAccountBudget{}, &models.QuarkOperationReceipt{}, &models.ProviderObservationIntakeClaim{}, &models.MediaReorganizationTask{}, &models.MediaLibraryStructureRepair{}, &observationTestPublication{}); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	s := NewProviderObservationService(db)
	s.now = func() time.Time { return clock }
	s.SetFenceValidator(func(*gorm.DB, models.ProviderObservationScope) error { return nil })
	scope, err := s.EnsureScope(context.Background(), ObservationScopeInput{LibraryID: 1, ConnectionID: 1, Provider: "quark", AccountKey: "account-1", Kind: "library", RootID: "root", RootPath: "/media", Fence: "source/profile-1"})
	if err != nil {
		t.Fatal(err)
	}
	reader := &observationTestReader{tree: map[string][]ObservationFact{}}
	publisher := &observationTestPublisher{db: db}
	s.SetPublisher(publisher)
	s.SetReaderResolver(func(context.Context, models.ProviderObservationScope) (ObservationDirectoryReader, error) {
		return reader, nil
	})
	return observationFixture{db, s, scope, &clock, reader, publisher}
}
func (f observationFixture) advance(d time.Duration) { *f.clock = f.clock.Add(d) }
func (f observationFixture) step(t *testing.T) {
	t.Helper()
	f.advance(2 * time.Second)
	if _, err := f.s.ObserveNext(context.Background(), f.scope.ID, f.reader); err != nil {
		t.Fatal(err)
	}
}
func (f observationFixture) round(t *testing.T) {
	t.Helper()
	if err := f.s.RequestFullSync(context.Background(), f.scope.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		f.step(t)
		var scope models.ProviderObservationScope
		if err := f.db.First(&scope, "id = ?", f.scope.ID).Error; err != nil {
			t.Fatal(err)
		}
		if scope.LastCompleteAt != nil && !scope.LastCompleteAt.Before(scope.CoverageStartedAt) {
			return
		}
	}
	t.Fatal("coverage did not complete")
}
func (f observationFixture) item(t *testing.T, id string) models.ProviderObservationItem {
	t.Helper()
	var row models.ProviderObservationItem
	if err := observationCurrent(f.db, f.scope.ID).First(&row, "resource_id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	return row
}
func (f observationFixture) publish(t *testing.T) {
	t.Helper()
	if _, err := f.s.PublishPending(context.Background(), f.scope.ID, 100); err != nil {
		t.Fatal(err)
	}
}
func obsFile(id, parent, name string) ObservationFact {
	return ObservationFact{ID: id, ParentID: parent, Name: name, Size: 123}
}
func obsDir(id, parent, name string) ObservationFact {
	return ObservationFact{ID: id, ParentID: parent, Name: name, IsDir: true}
}

func TestProviderObservationPartialRestartAndNoOp(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.pageSize = 1
	f.reader.tree["root"] = []ObservationFact{obsFile("a", "root", "A.mkv"), obsFile("b", "root", "B.mkv")}
	f.step(t)
	f.publish(t)
	if f.publisher.calls != 1 {
		t.Fatal("first partial positive fact was not publishable")
	}
	status, err := f.s.Status(context.Background(), f.scope.ID)
	if err != nil || status.LastCompleteAt != nil || status.PendingDirectories != 1 {
		t.Fatalf("partial reported complete: %+v %v", status, err)
	}
	restarted := NewProviderObservationService(f.db)
	restarted.now = f.s.now
	restarted.SetFenceValidator(f.s.validate)
	restarted.SetPublisher(f.publisher)
	f.s = restarted
	f.step(t)
	if f.reader.offsets[1] != 0 {
		t.Fatalf("restart continued offset %d", f.reader.offsets[1])
	}
	f.step(t)
	f.publish(t)
	if f.reader.offsets[2] != 1 {
		t.Fatalf("same-runtime page continuation lost: %v", f.reader.offsets)
	}
	var old int64
	if err = f.db.Model(&models.ProviderObservationBatch{}).Where("state = 'interrupted'").Count(&old).Error; err != nil || old != 1 {
		t.Fatalf("interrupted evidence=%d %v", old, err)
	}
	calls := f.publisher.calls
	f.advance(time.Minute)
	f.round(t)
	f.publish(t)
	if f.publisher.calls != calls {
		t.Fatalf("unchanged observation rewrote catalog: %d -> %d", calls, f.publisher.calls)
	}
}

func TestProviderObservationPagingAnomaliesNeverRemove(t *testing.T) {
	for _, kind := range []string{"duplicate", "total_changed", "empty_nonterminal", "partial", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			f := newObservationFixture(t)
			f.reader.tree["root"] = []ObservationFact{obsFile("old", "root", "Old.mkv")}
			f.round(t)
			f.publish(t)
			if err := f.s.RequestFullSync(context.Background(), f.scope.ID); err != nil {
				t.Fatal(err)
			}
			f.reader.hook = func(_ string, offset int64) (ObservationDirectoryPage, error, bool) {
				total := int64(2)
				if offset == 0 {
					return ObservationDirectoryPage{Items: []ObservationFact{obsFile("new", "root", "New.mkv")}, HasMore: true, Total: &total}, nil, true
				}
				switch kind {
				case "duplicate":
					return ObservationDirectoryPage{Items: []ObservationFact{obsFile("new", "root", "New.mkv")}, Offset: 1, Total: &total}, nil, true
				case "total_changed":
					total = 3
					return ObservationDirectoryPage{Offset: 1, Total: &total}, nil, true
				case "empty_nonterminal":
					return ObservationDirectoryPage{Offset: 1, HasMore: true, Total: &total}, nil, true
				case "partial":
					return ObservationDirectoryPage{Items: []ObservationFact{obsFile("x", "root", "X.mkv")}, Offset: 1, Total: &total, Partial: true}, nil, true
				default:
					return ObservationDirectoryPage{}, errors.New("timeout"), true
				}
			}
			f.step(t)
			f.advance(2 * time.Second)
			_, err := f.s.ObserveNext(context.Background(), f.scope.ID, f.reader)
			if kind != "timeout" && err != nil {
				t.Fatal(err)
			}
			if row := f.item(t, "old"); row.MissingSince != nil || row.DownlistedAt != nil {
				t.Fatalf("incomplete read became absence: %+v", row)
			}
			status, err := f.s.Status(context.Background(), f.scope.ID)
			if err != nil || status.PendingDirectories != 1 {
				t.Fatalf("bad coverage %+v %v", status, err)
			}
		})
	}
}

func TestProviderObservationMoveMissingRestoreAndLatePublication(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{obsDir("dir-a", "root", "A"), obsDir("dir-b", "root", "B")}
	f.reader.tree["dir-a"] = []ObservationFact{obsFile("file", "dir-a", "Episode.mkv")}
	f.round(t)
	f.publish(t)
	f.reader.tree["dir-a"] = nil
	f.reader.tree["dir-b"] = []ObservationFact{obsFile("file", "dir-b", "Renamed.mkv")}
	f.advance(11 * time.Minute)
	f.round(t)
	f.publish(t)
	row := f.item(t, "file")
	if row.RelativePath != "/B/Renamed.mkv" || row.MissingSince != nil || row.DownlistedAt != nil {
		t.Fatalf("move misclassified: %+v", row)
	}
	f.reader.tree["dir-b"] = nil
	f.advance(11 * time.Minute)
	f.round(t)
	row = f.item(t, "file")
	if row.MissingObservations != 1 || row.DownlistedAt != nil {
		t.Fatalf("first absence %+v", row)
	}
	f.advance(9 * time.Minute)
	f.round(t)
	row = f.item(t, "file")
	if row.DownlistedAt != nil {
		t.Fatal("under ten minutes downlisted")
	}
	f.advance(2 * time.Minute)
	f.round(t)
	row = f.item(t, "file")
	if row.DownlistedAt == nil || row.RecoveryUntil == nil || row.RecoveryUntil.Sub(*row.DownlistedAt) < 30*24*time.Hour {
		t.Fatalf("qualified missing not preserved: %+v", row)
	}
	// Restore before old deletion publication. Old cleanup/candidate cannot win.
	f.reader.tree["dir-b"] = []ObservationFact{obsFile("file", "dir-b", "Renamed.mkv")}
	f.advance(time.Minute)
	f.round(t)
	f.publish(t)
	row = f.item(t, "file")
	if row.DownlistedAt != nil || row.MissingSince != nil {
		t.Fatal("restoration did not recover original identity")
	}
	var deletes int64
	if err := f.db.Model(&observationTestPublication{}).Where("kind = 'observation_missing'").Count(&deletes).Error; err != nil || deletes != 0 {
		t.Fatalf("late removal published after recovery=%d %v", deletes, err)
	}
}

func TestProviderObservationPendingWriteBlocksMissingAndFailedPublicationRetries(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{obsFile("a", "root", "A.mkv")}
	f.round(t)
	f.publisher.fail = true
	if _, err := f.s.PublishPending(context.Background(), f.scope.ID, 10); err == nil {
		t.Fatal("expected transaction failure")
	}
	var count int64
	_ = f.db.Model(&observationTestPublication{}).Count(&count)
	if count != 0 {
		t.Fatal("failed transaction committed publication")
	}
	f.publisher.fail = false
	f.publish(t)
	receipt := models.QuarkOperationReceipt{ID: "write", OperationKey: "write", ConnectionID: 1, State: "unknown"}
	if err := f.db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	f.reader.tree["root"] = nil
	f.advance(11 * time.Minute)
	f.round(t)
	f.advance(11 * time.Minute)
	f.round(t)
	if f.item(t, "a").MissingSince != nil {
		t.Fatal("unknown write allowed missing inference")
	}
	if err := f.db.Model(&receipt).Update("state", "confirmed").Error; err != nil {
		t.Fatal(err)
	}
	f.advance(time.Minute)
	f.round(t)
	if f.item(t, "a").MissingObservations != 1 {
		t.Fatal("blocked observations incorrectly counted")
	}
}

func TestProviderObservationDirectoryRenameRepairsDescendantsLocally(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{obsDir("dir", "root", "Old")}
	f.reader.tree["dir"] = []ObservationFact{obsFile("file", "dir", "Movie.mkv")}
	f.round(t)
	f.publish(t)
	f.reader.tree["root"][0].Name = "New"
	f.advance(16 * time.Minute)
	f.step(t)
	if f.item(t, "dir").Name != "New" {
		f.step(t)
	}
	requests := len(f.reader.calls)
	n, err := f.s.RepairPaths(context.Background(), f.scope.ID, 250)
	if err != nil || n != 1 {
		t.Fatalf("repair=%d %v", n, err)
	}
	if len(f.reader.calls) != requests || f.item(t, "file").RelativePath != "/New/Movie.mkv" {
		t.Fatal("local path repair performed remote reads or lost descendants")
	}
	f.publish(t)
}

func TestProviderObservationPublicationFenceRollsBackAndDisabledScopeStops(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{obsFile("a", "root", "A.mkv")}
	f.round(t)
	f.publisher.before = func() {
		if err := f.db.Model(&models.ProviderObservationScope{}).Where("id = ?", f.scope.ID).Update("fence", "changed").Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.PublishPending(context.Background(), f.scope.ID, 10); !errors.Is(err, ErrObservationFence) {
		t.Fatalf("fence not checked in commit: %v", err)
	}
	var count int64
	_ = f.db.Model(&observationTestPublication{}).Count(&count)
	if count != 0 {
		t.Fatal("stale catalog committed")
	}
}

func TestProviderObservationIntakeRequiresStableCompleteQuietAndClaimsOnce(t *testing.T) {
	f := newObservationFixture(t)
	scope, err := f.s.EnsureScope(context.Background(), ObservationScopeInput{LibraryID: 1, ConnectionID: 1, Provider: "quark", AccountKey: "account-1", Kind: "intake", RootID: "inbox", RootPath: "/inbox", Fence: "intake-v1", AutoIngest: true})
	if err != nil {
		t.Fatal(err)
	}
	f.scope = scope
	f.reader.tree["inbox"] = []ObservationFact{obsFile("a", "inbox", "A.mkv"), obsFile("own", "inbox", "omc-task")}
	f.round(t)
	calls := 0
	enqueue := func(*gorm.DB, models.ProviderObservationItem) (string, error) { calls++; return "task-1", nil }
	if _, err = f.s.ClaimIntake(context.Background(), scope.ID, "a", enqueue); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("one observation adopted: %v", err)
	}
	f.advance(61 * time.Second)
	f.round(t)
	for i := 0; i < 2; i++ {
		id, err := f.s.ClaimIntake(context.Background(), scope.ID, "a", enqueue)
		if err != nil || id != "task-1" {
			t.Fatalf("claim: %s %v", id, err)
		}
	}
	if calls != 1 {
		t.Fatalf("duplicate intake task %d", calls)
	}
	if _, err = f.s.ClaimIntake(context.Background(), scope.ID, "own", enqueue); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("own task adopted %v", err)
	}
	_, err = f.s.EnsureScope(context.Background(), ObservationScopeInput{LibraryID: 2, ConnectionID: 1, Provider: "quark", AccountKey: "account-1", Kind: "library", RootID: "bad", RootPath: "/inbox/final", Fence: "bad"})
	if err == nil {
		t.Fatal("overlapping intake/final roots accepted")
	}
}

func TestProviderObservationSchedulingFairnessBudgetAndCooldown(t *testing.T) {
	f := newObservationFixture(t)
	f.reader.tree["root"] = []ObservationFact{obsDir("cold", "root", "Cold")}
	f.step(t)
	// Root is due again and cold has never been covered. Both classes have work.
	if err := observationCurrent(f.db, f.scope.ID).Model(&models.ProviderObservationItem{}).Where("resource_id = ?", "root").Update("due_at", *f.clock).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&models.ProviderObservationAccountBudget{}).Where("account_key = ?", f.scope.AccountKey).Updates(map[string]any{"hot_requests": 300, "cold_requests": 0}).Error; err != nil {
		t.Fatal(err)
	}
	f.step(t)
	if f.reader.calls[len(f.reader.calls)-1] != "cold" {
		t.Fatalf("cold starved: %v", f.reader.calls)
	}
	if err := f.s.Cooldown(context.Background(), f.scope.AccountKey, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	f.advance(2 * time.Second)
	if _, err := f.s.ObserveNext(context.Background(), f.scope.ID, f.reader); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("cooldown bypassed: %v", err)
	}
	status, err := f.s.Status(context.Background(), f.scope.ID)
	if err != nil || status.CooldownUntil == nil {
		t.Fatalf("cooldown hidden: %+v %v", status, err)
	}
	f.advance(6 * time.Minute)
	if err = f.db.Model(&models.ProviderObservationAccountBudget{}).Where("account_key = ?", f.scope.AccountKey).Updates(map[string]any{"hot_requests": 300, "cold_requests": 300}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = f.s.ObserveNext(context.Background(), f.scope.ID, f.reader); !errors.Is(err, ErrObservationDeferred) {
		t.Fatalf("hourly budget exceeded: %v", err)
	}
}

func TestProviderObservationLargeConfirmationResumesWithoutProviderReplay(t *testing.T) {
	f := newObservationFixture(t)
	for i := 0; i < 601; i++ {
		f.reader.tree["root"] = append(f.reader.tree["root"], obsFile(fmt.Sprintf("id-%04d", i), "root", fmt.Sprintf("File%04d.mkv", i)))
	}
	f.round(t)
	f.reader.tree["root"] = nil
	f.advance(11 * time.Minute)
	f.round(t)
	if item := f.item(t, "id-0600"); item.DownlistedAt != nil || item.MissingObservations != 1 {
		t.Fatalf("first missing=%+v", item)
	}
	f.advance(11 * time.Minute)
	if err := f.s.RequestFullSync(context.Background(), f.scope.ID); err != nil {
		t.Fatal(err)
	}
	f.step(t)
	var pending int64
	if err := f.db.Model(&models.ProviderObservationBatch{}).Where("state = 'confirming'").Count(&pending).Error; err != nil || pending != 1 {
		t.Fatalf("unbounded confirmation %d %v", pending, err)
	}
	before := len(f.reader.calls)
	restarted := NewProviderObservationService(f.db)
	restarted.now = f.s.now
	restarted.SetFenceValidator(f.s.validate)
	restarted.SetPublisher(f.publisher)
	f.s = restarted
	f.round(t)
	if len(f.reader.calls) != before {
		t.Fatal("completed provider pagination was replayed during local confirmation")
	}
	if item := f.item(t, "id-0600"); item.DownlistedAt == nil || item.MissingObservations != 2 {
		t.Fatalf("second missing=%+v", item)
	}
	status, err := f.s.Status(context.Background(), f.scope.ID)
	if err != nil || status.PendingConfirmations != 0 || status.PendingPublications == 0 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
