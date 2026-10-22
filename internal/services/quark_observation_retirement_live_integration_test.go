//go:build quark_live

package services

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"gorm.io/gorm"
)

// Real production services, a disposable SQLite DB, and a synthetic provider.
// These independent tests never backdate missing evidence or change a clock.
func retirementIndependentFixture(t *testing.T) (QuarkObservationRetirementLiveInput, *quarkObservationMutationFake) {
	t.Helper()
	in, d := quarkObservationRetirementFixture(t)
	in.StartedAt = time.Now().UTC()
	in.Deadline = in.StartedAt.Add(25 * time.Minute)
	return in, d
}

func retirementIndependentDB(t *testing.T, in QuarkObservationRetirementLiveInput, read func(*quarkObservationLiveRuntime)) {
	t.Helper()
	r, err := quarkObservationLiveOpen(context.Background(), in.Base, false)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	read(r)
}

func TestQuarkObservationRetirementIndependentRealWaitResume(t *testing.T) {
	in, d := retirementIndependentFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actor := in.Mutate
	calls := map[string]int{}
	in.Mutate = func(ctx context.Context, stage string) error { calls[stage]++; return actor(ctx, stage) }
	in.Progress = func(p QuarkObservationRetirementProgress) {
		if p.Stage == "wait_confirm" && p.WaitSeconds > 0 {
			if p.WaitSeconds < 600 || p.WaitSeconds > 605 {
				t.Fatal("first wait did not use the actual newly persisted missing time")
			}
			cancel()
		}
	}
	report, err := RunQuarkObservationRetirementLiveValidation(ctx, in)
	if err == nil || report.Stage != "wait_confirm" || report.Completed || report.TwoCompleteCoverages || report.RevokedBeforeArtifacts || calls["move_out"] != 1 || calls["move_back"] != 0 {
		t.Fatalf("first observation skipped real wait: %+v %v", report, err)
	}
	var beforeCP quarkObservationRetirementCheckpoint
	var beforeItem models.ProviderObservationItem
	var beforeScope models.ProviderObservationScope
	var beforeHead models.CatalogHead
	var beforePrior quarkObservationMutationCheckpoint
	retirementIndependentDB(t, in, func(r *quarkObservationLiveRuntime) {
		if e := r.db.First(&beforeCP, 1).Error; e != nil {
			t.Fatal(e)
		}
		var state quarkRetirementState
		if json.Unmarshal([]byte(beforeCP.StateJSON), &state) != nil || beforeCP.Stage != "wait_confirm" || state.UserDigest == "" || !state.Authority.Manual || state.Authority.TMDBID != 42 || state.Authority.Title != quarkRetirementTitle {
			t.Fatal("missing real manual/user baseline")
		}
		var e error
		beforeItem, e = r.retirementItem(context.Background(), in)
		if e != nil || beforeItem.MissingLastAt == nil || beforeItem.MissingSince == nil || beforeItem.MissingLastAt.Before(in.StartedAt) || beforeItem.MissingObservations != 1 || beforeItem.DownlistedAt != nil || !beforeItem.MissingLastAt.Equal(state.FirstMissingAt) {
			t.Fatal("first absence did not retain a real candidate")
		}
		beforeScope, e = r.retirementScope(context.Background())
		if e != nil || beforeScope.LastCompleteAt == nil || beforeScope.LastCompleteAt.Before(beforeScope.CoverageStartedAt) {
			t.Fatal("first coverage incomplete")
		}
		if e = r.retirementUserVisible(context.Background(), state, true); e != nil {
			t.Fatal(e)
		}
		if e = r.db.First(&beforeHead).Error; e != nil {
			t.Fatal(e)
		}
		if e = r.db.First(&beforePrior, 1).Error; e != nil {
			t.Fatal(e)
		}
	})
	beforeReads := d.reads.Load()
	resume, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	in.Progress = nil
	in.Mutate = func(context.Context, string) error { t.Fatal("waiting restart invoked actor"); return nil }
	report, err = RunQuarkObservationRetirementLiveValidation(resume, in)
	if err == nil || report.Stage != "wait_confirm" || report.Requests != 0 || d.reads.Load() != beforeReads || report.Completed || report.MissingIntervalSatisfied {
		t.Fatalf("restart did not preserve real remaining wait: %+v %v", report, err)
	}
	retirementIndependentDB(t, in, func(r *quarkObservationLiveRuntime) {
		var cp quarkObservationRetirementCheckpoint
		var head models.CatalogHead
		var prior quarkObservationMutationCheckpoint
		if e := r.db.First(&cp, 1).Error; e != nil {
			t.Fatal(e)
		}
		item, e := r.retirementItem(context.Background(), in)
		if e != nil || !reflect.DeepEqual(item, beforeItem) {
			t.Fatal("wait restart rewrote observed missing facts")
		}
		scope, e := r.retirementScope(context.Background())
		if e != nil || !reflect.DeepEqual(scope, beforeScope) {
			t.Fatal("wait restart advanced coverage or queue schedule")
		}
		if e = r.db.First(&head).Error; e != nil || !reflect.DeepEqual(head, beforeHead) || !reflect.DeepEqual(cp, beforeCP) {
			t.Fatal("wait restart changed catalog or checkpoint")
		}
		if e = r.db.First(&prior, 1).Error; e != nil || !reflect.DeepEqual(prior, beforePrior) {
			t.Fatal("retirement modified completed predecessor")
		}
	})
}

func TestQuarkObservationRetirementIndependentActorIsNotAbsence(t *testing.T) {
	in, d := retirementIndependentFixture(t)
	calls := 0
	in.Mutate = func(context.Context, string) error { calls++; return nil }
	report, err := RunQuarkObservationRetirementLiveValidation(context.Background(), in)
	if err == nil || report.Stage != "first_observe" || report.Completed || report.TwoCompleteCoverages || report.OwnedSTRMRemoved || calls != 1 {
		t.Fatalf("actor acknowledgement masqueraded as observed absence: %+v %v", report, err)
	}
	if d.sample.ParentID != d.directory.ID {
		t.Fatal("test actor changed provider")
	}
	retirementIndependentDB(t, in, func(r *quarkObservationLiveRuntime) {
		item, e := r.retirementItem(context.Background(), in)
		if e != nil || item.MissingSince != nil || item.DownlistedAt != nil || item.MissingObservations != 0 {
			t.Fatal("callback inserted missing facts")
		}
		var cp quarkObservationRetirementCheckpoint
		if e = r.db.First(&cp, 1).Error; e != nil || cp.Stage != "first_observe" {
			t.Fatal("unobserved absence advanced checkpoint")
		}
		var state quarkRetirementState
		if json.Unmarshal([]byte(cp.StateJSON), &state) != nil || !state.FirstMissingAt.IsZero() {
			t.Fatal("unobserved absence invented a wait timestamp")
		}
		current, e := r.mutationSnapshot(context.Background(), in.Base, retirementRelative(in))
		if e != nil || mutationDigest(current) != mutationDigest(state.Baseline) {
			t.Fatal("unchanged provider changed catalog or artifact")
		}
	})
}

func TestQuarkObservationRetirementIndependentManualCheckpointRecovery(t *testing.T) {
	in, d := retirementIndependentFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	in.Progress = func(p QuarkObservationRetirementProgress) {
		if p.Stage == "manual_match" {
			cancel()
		}
	}
	if _, err := RunQuarkObservationRetirementLiveValidation(ctx, in); err == nil {
		t.Fatal("initial manual boundary did not stop")
	}
	in.Mutate = func(context.Context, string) error { t.Fatal("manual boundary reached cloud actor"); return nil }
	in.Progress = nil
	var originalHead models.CatalogHead
	retirementIndependentDB(t, in, func(r *quarkObservationLiveRuntime) {
		if err := r.db.First(&originalHead).Error; err != nil {
			t.Fatal(err)
		}
		if err := r.db.Exec("CREATE TRIGGER independent_retirement_match_checkpoint BEFORE UPDATE OF stage ON quark_live_retirement_checkpoint WHEN NEW.stage='manual_edit' BEGIN SELECT RAISE(ABORT,'synthetic checkpoint interruption'); END").Error; err != nil {
			t.Fatal(err)
		}
	})
	reads := d.reads.Load()
	report, err := RunQuarkObservationRetirementLiveValidation(context.Background(), in)
	if err == nil || report.Stage != "manual_match" || d.reads.Load() != reads {
		t.Fatalf("manual checkpoint boundary not reached: %+v %v", report, err)
	}
	retirementIndependentDB(t, in, func(r *quarkObservationLiveRuntime) {
		var head models.CatalogHead
		if e := r.db.First(&head).Error; e != nil || head.Revision != originalHead.Revision+1 {
			t.Fatal("manual match did not publish exactly once")
		}
		a, e := r.retirementAuthority(context.Background(), in)
		if e != nil || !a.Manual || a.TMDBID != 42 {
			t.Fatal("manual write not persisted before checkpoint failure")
		}
		if e = r.db.Exec("DROP TRIGGER independent_retirement_match_checkpoint").Error; e != nil {
			t.Fatal(e)
		}
	})
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	in.Progress = func(p QuarkObservationRetirementProgress) {
		if p.Stage == "manual_artifacts" {
			cancel()
		}
	}
	report, err = RunQuarkObservationRetirementLiveValidation(ctx, in)
	if err == nil || report.Stage != "manual_artifacts" || d.reads.Load() != reads {
		t.Fatalf("manual recovery reached an unexpected stage: %+v %v", report, err)
	}
	retirementIndependentDB(t, in, func(r *quarkObservationLiveRuntime) {
		var head models.CatalogHead
		if e := r.db.First(&head).Error; e != nil || head.Revision != originalHead.Revision+2 {
			t.Fatal("recovery repeated manual match publication")
		}
		a, e := r.retirementAuthority(context.Background(), in)
		if e != nil || !a.Manual || a.TMDBID != 42 || a.Title != quarkRetirementTitle {
			t.Fatal("manual edit not preserved")
		}
	})
}

func TestQuarkObservationRetirementIndependentPreconditions(t *testing.T) {
	in, d := retirementIndependentFixture(t)
	in.Mutate = func(context.Context, string) error { t.Fatal("invalid predecessor reached actor"); return nil }
	before := d.reads.Load()
	for _, mode := range []string{"expired", "wrong_parent", "unfinished_predecessor"} {
		t.Run(mode, func(t *testing.T) {
			candidate := in
			if mode == "expired" {
				candidate.StartedAt = time.Now().UTC().Add(-26 * time.Minute)
				candidate.Deadline = candidate.StartedAt.Add(25 * time.Minute)
			} else if mode == "wrong_parent" {
				candidate.Predecessor.DirectoryTVID = "foreign-directory"
			} else {
				retirementIndependentDB(t, in, func(r *quarkObservationLiveRuntime) {
					if err := r.db.Model(&quarkObservationMutationCheckpoint{}).Where("id = 1").Update("stage", "noop").Error; err != nil {
						t.Fatal(err)
					}
				})
			}
			report, err := RunQuarkObservationRetirementLiveValidation(context.Background(), candidate)
			if err == nil || report.Completed || d.reads.Load() != before {
				t.Fatalf("invalid original contract was accepted: %s %+v", mode, report)
			}
		})
	}
	retirementIndependentDB(t, in, func(r *quarkObservationLiveRuntime) {
		if r.db.Migrator().HasTable(&quarkObservationRetirementCheckpoint{}) {
			var count int64
			if e := r.db.Model(&quarkObservationRetirementCheckpoint{}).Count(&count).Error; e != nil && e != gorm.ErrRecordNotFound || count != 0 {
				t.Fatal("failed predecessor initialized retirement")
			}
		}
	})
}
